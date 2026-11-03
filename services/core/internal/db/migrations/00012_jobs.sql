-- The generic jobs table (Plan 4 spec §6.5).
--
-- run_query keeps its job columns on clinical_queries (00007). Every new job
-- type gets a row in jobs instead; Plan 4 allows approve_correction only,
-- and the documents plan adds ingest_document and the rest by widening the
-- type CHECK. The columns, the roles and the policies are 00007's, 00008's
-- and 00009's for clinical_queries, copied:
--   - mhai_gateway: SELECT, INSERT, UPDATE (published_at), under
--     hospital_rows (like 00003).
--   - mhai_worker: SELECT and UPDATE of its status columns, under
--     hospital_rows, worker_status (RESTRICTIVE: only queued or running
--     rows, only to running, completed or failed) and worker_hospital_active
--     (RESTRICTIVE: only an active hospital's rows). attempts never goes
--     down (trigger).
--   - mhai_jobs: the job columns, and the platform's cancel and retry
--     columns; only rows no worker holds (queued, failed, cancelled), before
--     and after; the expiry function mhai_fail_expired_jobs(); and one more
--     audit action, job.cancelled (job.retried is 00007's).
--   - mhai_migrate (the owner, under FORCE): the expiry and cancel-on-
--     deactivate policies, scoped as 00007's and 00009's.
--
-- 00009's two triggers also cancel the user's or hospital's queued and
-- running jobs. No outbox fact is added for them: the outbox has no
-- job-cancel action in Plan 4 (the cancelled row says why).
--
-- The outbox is widened: query_id becomes nullable, job_id (with the
-- same-hospital FK) is added, and exactly one of the two is set. Its action
-- CHECK adds correction.applied and correction.apply_failed. The drainer's
-- role reads the seven job columns it needs and appends the two actions.
--
-- 00009's jobs_append_own_job ties mhai_jobs's audit rows to a job of the
-- row's own hospital; it now accepts a jobs row too (entity 'job').

-- +goose Up

CREATE TABLE jobs (
    id            uuid PRIMARY KEY,
    hospital_id   uuid REFERENCES hospitals (id),
    type          text NOT NULL CHECK (type IN ('approve_correction')),
    subject_id    uuid NOT NULL,
    created_by    uuid REFERENCES users (id),
    request_id    text,
    status        text NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    attempts      integer NOT NULL DEFAULT 0,
    lease_until   timestamptz,
    published_at  timestamptz,
    deadline_at   timestamptz,
    error         text CHECK (error IS NULL OR error ~ '^[a-z_]{1,32}$'),
    cancel_reason text CHECK (cancel_reason IS NULL OR cancel_reason ~ '^[a-z_]{1,32}$'),
    cancelled_by  uuid REFERENCES users (id),
    cancelled_at  timestamptz,
    cancel_note   text CHECK (cancel_note IS NULL OR length(cancel_note) <= 500),
    retry_count   integer NOT NULL DEFAULT 0,
    failed_at     timestamptz,
    completed_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, hospital_id)
);
-- One live job per subject, within a hospital: a subject id of another
-- hospital can neither collide with nor block this one's.
CREATE UNIQUE INDEX jobs_one_live ON jobs (hospital_id, type, subject_id) WHERE status IN ('queued', 'running');
-- The publish sweeper: queued rows with no published_at.
CREATE INDEX jobs_status_published_idx ON jobs (status, published_at);
ALTER TABLE jobs OWNER TO mhai_migrate;
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

------------------------------------------------------------------------
-- mhai_gateway and mhai_worker (00003, 00008, 00009)
------------------------------------------------------------------------
-- cancel_note (free text from the platform) and cancelled_by are read by
-- neither role: the platform reads them through its own functions (spec 6.2).
GRANT INSERT ON jobs TO mhai_gateway;
GRANT SELECT (id, hospital_id, type, subject_id, created_by, request_id, status, attempts, lease_until, published_at,
              deadline_at, error, cancel_reason, cancelled_at, retry_count, failed_at, completed_at, created_at)
    ON jobs TO mhai_gateway, mhai_worker;
GRANT UPDATE (published_at) ON jobs TO mhai_gateway;
GRANT UPDATE (status, attempts, lease_until, error, completed_at, failed_at) ON jobs TO mhai_worker;

CREATE POLICY hospital_rows ON jobs TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
-- A job is born queued and untouched: core's insert sets no state.
CREATE POLICY gateway_new_job ON jobs AS RESTRICTIVE FOR INSERT TO mhai_gateway
    WITH CHECK (status = 'queued' AND attempts = 0 AND retry_count = 0 AND cancelled_by IS NULL
                AND cancel_note IS NULL AND cancel_reason IS NULL AND error IS NULL);
CREATE POLICY worker_status ON jobs AS RESTRICTIVE FOR UPDATE TO mhai_worker
    USING (status IN ('queued', 'running'))
    WITH CHECK (status IN ('running', 'completed', 'failed'));
CREATE POLICY worker_hospital_active ON jobs AS RESTRICTIVE FOR UPDATE TO mhai_worker
    USING (EXISTS (SELECT 1 FROM hospitals h WHERE h.id = jobs.hospital_id AND h.status = 'active'));

-- attempts only grows, whoever writes it.
-- +goose StatementBegin
CREATE FUNCTION mhai_job_attempts_never_decrease() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = 'check_violation',
        MESSAGE = 'jobs: attempts never decreases';
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_job_attempts_never_decrease() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_job_attempts_never_decrease() FROM PUBLIC;
CREATE TRIGGER jobs_attempts BEFORE UPDATE OF attempts ON jobs
    FOR EACH ROW WHEN (NEW.attempts < OLD.attempts) EXECUTE FUNCTION mhai_job_attempts_never_decrease();

-- The subject of an approve_correction job is a correction of the job's own
-- hospital. No generic FK (jobs is generic). It runs as the inserting role,
-- not SECURITY DEFINER: the gateway reads feedback_events under
-- hospital_rows, which is exactly the check wanted (another hospital's
-- correction is invisible, so it is refused with a message that says nothing
-- about whether it exists). Owner and superuser inserts see every row.
-- +goose StatementBegin
CREATE FUNCTION mhai_job_subject_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.type = 'approve_correction' AND NOT EXISTS (
        SELECT 1 FROM public.feedback_events f WHERE f.id = NEW.subject_id AND f.hospital_id = NEW.hospital_id) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'jobs: subject is not a correction of this hospital';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_job_subject_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_job_subject_check() FROM PUBLIC;
CREATE TRIGGER jobs_subject BEFORE INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION mhai_job_subject_check();

------------------------------------------------------------------------
-- mhai_jobs (00007)
------------------------------------------------------------------------
GRANT SELECT (id, hospital_id, type, subject_id, created_by, request_id, status, attempts, published_at,
              deadline_at, error, cancel_reason, retry_count, created_at)
    ON jobs TO mhai_jobs;
GRANT UPDATE (status, error, published_at, cancel_reason, completed_at, failed_at, retry_count, cancelled_by, cancelled_at, cancel_note)
    ON jobs TO mhai_jobs;
CREATE POLICY jobs_role_read ON jobs FOR SELECT TO mhai_jobs USING (true);
CREATE POLICY jobs_role_update ON jobs FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed', 'cancelled')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));

-- job.retried is in 00007's list already; job.cancelled is new.
DROP POLICY jobs_append ON audit_log;
CREATE POLICY jobs_append ON audit_log FOR INSERT TO mhai_jobs
    WITH CHECK (action IN ('query.failed', 'query.cancelled', 'job.dead_lettered', 'job.retried', 'job.cancelled', 'intelligence.refused'));

-- Tied to a job of the row's own hospital: a clinical_queries row (entity
-- query) or a jobs row (entity job).
DROP POLICY jobs_append_own_job ON audit_log;
CREATE POLICY jobs_append_own_job ON audit_log AS RESTRICTIVE FOR INSERT TO mhai_jobs
    WITH CHECK (actor_id IS NULL AND entity IN ('query', 'job')
                AND audit_log.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                AND ((entity = 'query'
                      AND EXISTS (SELECT 1 FROM clinical_queries q
                                  WHERE q.id = CASE WHEN audit_log.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                                    THEN audit_log.entity_id::uuid END
                                    AND q.hospital_id = audit_log.hospital_id))
                     OR (entity = 'job'
                         AND EXISTS (SELECT 1 FROM jobs j
                                     WHERE j.id = CASE WHEN audit_log.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                                       THEN audit_log.entity_id::uuid END
                                       AND j.hospital_id = audit_log.hospital_id))));

------------------------------------------------------------------------
-- Deadline expiry (00007's twin)
------------------------------------------------------------------------
CREATE POLICY job_expire_read ON jobs FOR SELECT TO mhai_migrate
    USING ((status IN ('queued', 'running') AND COALESCE(deadline_at, created_at + interval '10 minutes') <= now())
           OR (status = 'failed' AND error = 'deadline_exceeded'));
CREATE POLICY job_expire_update ON jobs FOR UPDATE TO mhai_migrate
    USING (status IN ('queued', 'running') AND COALESCE(deadline_at, created_at + interval '10 minutes') <= now())
    WITH CHECK (status = 'failed' AND error = 'deadline_exceeded');

-- +goose StatementBegin
CREATE FUNCTION mhai_fail_expired_jobs(batch integer)
    RETURNS TABLE (id uuid, hospital_id uuid, type text, request_id text, attempts integer)
    LANGUAGE sql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH doomed AS (
        SELECT j.id, j.hospital_id, j.type, j.request_id, j.attempts FROM public.jobs j
        WHERE j.status IN ('queued', 'running')
          AND COALESCE(j.deadline_at, j.created_at + interval '10 minutes') <= now()
        ORDER BY j.created_at
        LIMIT GREATEST(LEAST(batch, 1000), 1)
        FOR UPDATE SKIP LOCKED
    ), gone AS (
        UPDATE public.jobs j
        SET status = 'failed', error = 'deadline_exceeded', failed_at = now(), completed_at = now()
        FROM doomed d
        WHERE j.id = d.id AND j.status IN ('queued', 'running')
        RETURNING d.id, d.hospital_id, d.type, d.request_id, d.attempts
    )
    SELECT g.id, g.hospital_id, g.type, g.request_id, g.attempts FROM gone g
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_fail_expired_jobs(integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_fail_expired_jobs(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_fail_expired_jobs(integer) TO mhai_jobs;

------------------------------------------------------------------------
-- Cancel on deactivation (00009's triggers, widened)
------------------------------------------------------------------------
CREATE POLICY cancel_read ON jobs FOR SELECT TO mhai_migrate
    USING ((status IN ('queued', 'running')
            OR (status = 'cancelled' AND cancel_reason IN ('user_deactivated', 'hospital_deactivated')))
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = jobs.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = jobs.hospital_id AND h.status <> 'active')));
CREATE POLICY cancel_update ON jobs FOR UPDATE TO mhai_migrate
    USING (status IN ('queued', 'running')
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = jobs.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = jobs.hospital_id AND h.status <> 'active')))
    WITH CHECK (status = 'cancelled' AND cancel_reason IN ('user_deactivated', 'hospital_deactivated')
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = jobs.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = jobs.hospital_id AND h.status <> 'active')));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cancel_user_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'user_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.created_by = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'user_deactivated')
    FROM gone g;
    UPDATE public.jobs j
    SET status = 'cancelled', cancel_reason = 'user_deactivated', completed_at = now(), lease_until = NULL
    WHERE j.created_by = NEW.id AND j.status IN ('queued', 'running');
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cancel_hospital_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'hospital_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.hospital_id = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'hospital_deactivated')
    FROM gone g;
    UPDATE public.jobs j
    SET status = 'cancelled', cancel_reason = 'hospital_deactivated', completed_at = now(), lease_until = NULL
    WHERE j.hospital_id = NEW.id AND j.status IN ('queued', 'running');
    RETURN NULL;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The outbox, widened
------------------------------------------------------------------------
ALTER TABLE intelligence_audit_outbox
    ALTER COLUMN query_id DROP NOT NULL,
    ADD COLUMN job_id uuid,
    ADD FOREIGN KEY (job_id, hospital_id) REFERENCES jobs (id, hospital_id),
    ADD CONSTRAINT intelligence_audit_outbox_parent_matches_action
        CHECK ((query_id IS NOT NULL) = (action LIKE 'query.%')),
    ADD CONSTRAINT intelligence_audit_outbox_one_parent CHECK (num_nonnulls(query_id, job_id) = 1),
    DROP CONSTRAINT intelligence_audit_outbox_action_check,
    ADD CONSTRAINT intelligence_audit_outbox_action_check
        CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed'));
CREATE INDEX intelligence_audit_outbox_job_idx ON intelligence_audit_outbox (job_id, hospital_id);

-- The drainer: the ids and status of a job (to check a fact against it), its
-- subject (a correction fact must name the job's own correction), and the
-- two new actions. Never the cancel note or who cancelled.
GRANT SELECT (id, hospital_id, created_by, request_id, status, type, subject_id) ON jobs TO mhai_audit_drain;
CREATE POLICY drain_read ON jobs FOR SELECT TO mhai_audit_drain USING (true);
DROP POLICY drain_append ON audit_log;
CREATE POLICY drain_append ON audit_log FOR INSERT TO mhai_audit_drain
    WITH CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed')
                AND hospital_id IS NOT NULL);

-- +goose Down
DROP POLICY drain_append ON audit_log;
CREATE POLICY drain_append ON audit_log FOR INSERT TO mhai_audit_drain
    WITH CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled') AND hospital_id IS NOT NULL);

-- Facts about jobs go with the jobs. FORCE holds the owner too, so it is
-- lifted for the delete and put back.
ALTER TABLE intelligence_audit_outbox NO FORCE ROW LEVEL SECURITY;
DELETE FROM intelligence_audit_outbox WHERE job_id IS NOT NULL;
ALTER TABLE intelligence_audit_outbox FORCE ROW LEVEL SECURITY;
DROP INDEX intelligence_audit_outbox_job_idx;
ALTER TABLE intelligence_audit_outbox
    DROP CONSTRAINT intelligence_audit_outbox_action_check,
    ADD CONSTRAINT intelligence_audit_outbox_action_check
        CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled')),
    DROP CONSTRAINT intelligence_audit_outbox_one_parent,
    DROP CONSTRAINT intelligence_audit_outbox_parent_matches_action,
    DROP COLUMN job_id,
    ALTER COLUMN query_id SET NOT NULL;

-- 00009's functions as they were.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cancel_user_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'user_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.created_by = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'user_deactivated')
    FROM gone g;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cancel_hospital_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'hospital_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.hospital_id = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'hospital_deactivated')
    FROM gone g;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

DROP POLICY jobs_append_own_job ON audit_log;
CREATE POLICY jobs_append_own_job ON audit_log AS RESTRICTIVE FOR INSERT TO mhai_jobs
    WITH CHECK (actor_id IS NULL AND entity = 'query'
                AND EXISTS (SELECT 1 FROM clinical_queries q
                            WHERE q.id = CASE WHEN audit_log.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                              THEN audit_log.entity_id::uuid END
                              AND q.hospital_id = audit_log.hospital_id));
DROP POLICY jobs_append ON audit_log;
CREATE POLICY jobs_append ON audit_log FOR INSERT TO mhai_jobs
    WITH CHECK (action IN ('query.failed', 'query.cancelled', 'job.dead_lettered', 'job.retried', 'intelligence.refused'));

-- The table takes its policies, grants, indexes and trigger with it.
DROP TABLE jobs;
DROP FUNCTION mhai_fail_expired_jobs(integer);
DROP FUNCTION mhai_job_attempts_never_decrease();
DROP FUNCTION mhai_job_subject_check();
