-- Jobs (spec §9.2, plan departures 1, 3 and 8).
--
-- clinical_queries gains the columns a job needs: the request id the audit
-- drainer uses, the worker's claim (attempts, lease_until), the publish
-- sweeper's marker (published_at), the deadline and a cancel reason. The
-- status set is closed, and error holds a code only, never text (spec
-- §10.1: no PHI or internals can leak through it).
--
-- clinical_decisions gains what the frontend contract's result needs:
-- verified, degraded, failed_agents, model_versions and the trust band.
--
-- mhai_jobs is the role behind core's background loops (the publish
-- sweeper; later the dead-letter consumer). They read and update job rows
-- of every hospital, which mhai_gateway can't. It sees the job columns only
-- (never the question or the patient data), can fail, cancel or re-queue a
-- job no worker holds (never touch a running or completed one, or mark one
-- running or completed: only the worker does), can fail expired rows
-- through mhai_fail_expired_queries(), and can append only its own audit
-- actions. No other table. NOLOGIN, no BYPASSRLS;
-- the core login reaches it only by SET ROLE inside db.WithJobs (mhai_app
-- holds it WITH INHERIT FALSE). The role is cluster-wide, so it is created
-- only if missing and kept on Down.

-- +goose Up

ALTER TABLE clinical_queries
    ADD COLUMN request_id    text,
    ADD COLUMN attempts      integer NOT NULL DEFAULT 0,
    ADD COLUMN lease_until   timestamptz,
    ADD COLUMN published_at  timestamptz,
    ADD COLUMN deadline_at   timestamptz,
    ADD COLUMN cancel_reason text,
    ADD CONSTRAINT clinical_queries_status_check
        CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    ADD CONSTRAINT clinical_queries_error_check
        CHECK (error IS NULL OR error ~ '^[a-z_]{1,32}$'),
    ADD CONSTRAINT clinical_queries_cancel_reason_check
        CHECK (cancel_reason IS NULL OR cancel_reason ~ '^[a-z_]{1,32}$');
-- The publish sweeper: queued rows with no published_at.
CREATE INDEX clinical_queries_status_published_idx ON clinical_queries (status, published_at);

ALTER TABLE clinical_decisions
    ADD COLUMN verified       boolean,
    ADD COLUMN degraded       boolean,
    ADD COLUMN failed_agents  text[],
    ADD COLUMN model_versions jsonb,
    ADD COLUMN trust_band     text,
    ADD CONSTRAINT clinical_decisions_trust_band_check
        CHECK (trust_band IS NULL OR trust_band IN ('low', 'mid', 'high'));

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_jobs') THEN
        CREATE ROLE mhai_jobs NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT mhai_jobs TO mhai_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_jobs;

-- The job columns only: no clinical_question, no patient_data_json.
GRANT SELECT (id, hospital_id, created_by, request_id, status, attempts, published_at, deadline_at, created_at)
    ON clinical_queries TO mhai_jobs;
GRANT UPDATE (status, error, published_at, cancel_reason, completed_at)
    ON clinical_queries TO mhai_jobs;
CREATE POLICY jobs_read ON clinical_queries FOR SELECT TO mhai_jobs USING (true);
-- mhai_jobs touches only rows no worker holds: queued, failed or cancelled,
-- and leaves them queued, failed or cancelled. It can't touch a running row
-- (so it can't re-queue a job a worker is running) or a completed one (so a
-- finished question is never re-run). The one way it ends a running row is
-- mhai_fail_expired_queries() below, and only past the row's deadline.
CREATE POLICY jobs_update ON clinical_queries FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed', 'cancelled')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));

-- Past its deadline a queued or running question is failed with
-- deadline_exceeded, whether or not it was ever published, so a worker that
-- died on its last delivery or a published message that was lost can't hold
-- an in-flight slot for ever. A SECURITY DEFINER function owned by
-- mhai_migrate does it, like mhai_sweep_idempotency (00006): its policies
-- let it see and change only expired queued or running rows, and set only
-- failed / deadline_exceeded. Only mhai_jobs may run it. It returns the ids
-- the caller audits (query.failed) in the same transaction. The 10 minutes
-- is pipeline.run_query's timeout, the deadline of a row without one
-- (authz.Operation; a Go test holds the two together).
CREATE POLICY expire_read ON clinical_queries FOR SELECT TO mhai_migrate
    USING ((status IN ('queued', 'running') AND COALESCE(deadline_at, created_at + interval '10 minutes') <= now())
           OR (status = 'failed' AND error = 'deadline_exceeded'));
CREATE POLICY expire_update ON clinical_queries FOR UPDATE TO mhai_migrate
    USING (status IN ('queued', 'running') AND COALESCE(deadline_at, created_at + interval '10 minutes') <= now())
    WITH CHECK (status = 'failed' AND error = 'deadline_exceeded');

-- Fails at most batch expired rows (1 to 1000) and returns them.
-- +goose StatementBegin
CREATE FUNCTION mhai_fail_expired_queries(batch integer)
    RETURNS TABLE (id uuid, hospital_id uuid, request_id text, attempts integer)
    LANGUAGE sql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH doomed AS (
        SELECT q.id, q.hospital_id, q.request_id, q.attempts FROM public.clinical_queries q
        WHERE q.status IN ('queued', 'running')
          AND COALESCE(q.deadline_at, q.created_at + interval '10 minutes') <= now()
        ORDER BY q.created_at
        LIMIT GREATEST(LEAST(batch, 1000), 1)
        FOR UPDATE SKIP LOCKED
    ), gone AS (
        UPDATE public.clinical_queries q
        SET status = 'failed', error = 'deadline_exceeded', completed_at = now()
        FROM doomed d
        WHERE q.id = d.id AND q.status IN ('queued', 'running')
        RETURNING d.id, d.hospital_id, d.request_id, d.attempts
    )
    SELECT g.id, g.hospital_id, g.request_id, g.attempts FROM gone g
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_fail_expired_queries(integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_fail_expired_queries(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_fail_expired_queries(integer) TO mhai_jobs;

-- audit.Write: append its own actions, plus the chain head.
GRANT INSERT ON audit_log TO mhai_jobs;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_jobs;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_jobs;
CREATE POLICY jobs_append ON audit_log FOR INSERT TO mhai_jobs
    WITH CHECK (action IN ('query.failed', 'query.cancelled', 'job.dead_lettered', 'job.retried', 'intelligence.refused'));

-- +goose Down
DROP FUNCTION mhai_fail_expired_queries(integer);
DROP POLICY expire_update ON clinical_queries;
DROP POLICY expire_read ON clinical_queries;
DROP POLICY jobs_append ON audit_log;
DROP POLICY jobs_update ON clinical_queries;
DROP POLICY jobs_read ON clinical_queries;
REVOKE ALL ON clinical_queries, audit_log FROM mhai_jobs;
REVOKE ALL ON SEQUENCE audit_log_id_seq FROM mhai_jobs;
REVOKE ALL ON FUNCTION audit_chain_head() FROM mhai_jobs;
REVOKE ALL ON SCHEMA public FROM mhai_jobs;

ALTER TABLE clinical_decisions
    DROP CONSTRAINT clinical_decisions_trust_band_check,
    DROP COLUMN trust_band,
    DROP COLUMN model_versions,
    DROP COLUMN failed_agents,
    DROP COLUMN degraded,
    DROP COLUMN verified;

DROP INDEX clinical_queries_status_published_idx;
ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_cancel_reason_check,
    DROP CONSTRAINT clinical_queries_error_check,
    DROP CONSTRAINT clinical_queries_status_check,
    DROP COLUMN cancel_reason,
    DROP COLUMN deadline_at,
    DROP COLUMN published_at,
    DROP COLUMN lease_until,
    DROP COLUMN attempts,
    DROP COLUMN request_id;
