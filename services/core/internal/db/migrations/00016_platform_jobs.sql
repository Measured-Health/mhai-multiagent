-- The platform's failed jobs: list, retry and cancel (Plan 4 spec §6.2,
-- Task 12).
--
-- clinical_queries (run_query) gains what the platform's retry and list
-- need:
--   - retry_count: the platform's retries of the row, as jobs has it. The
--     run_query worker caps its attempts per retry (max_attempts *
--     (retry_count + 1)), as approve_correction does, so a retried question
--     is not failed at once on a lifetime cap (attempts never goes down).
--   - failed_at: when the row last failed, stamped by a trigger whoever
--     fails it (the worker, the dead-letter consumer, the deadline expiry),
--     and kept through a retry, so the list can sort by it.
--   - query_cancellations: who cancelled a question from the platform,
--     when, and the note. A table of its own, not columns: mhai_worker and
--     mhai_gateway hold SELECT on the whole of clinical_queries (00008,
--     00003), and the note and the platform admin's id must reach neither.
--     mhai_jobs inserts (only for a row it has just cancelled); only the
--     list's reader reads it, and never the note.
--
-- jobs (approve_correction) gains queued_at, its place in its type's
-- in-flight budget (intel.QueueJob, the sweeper): created_at at insert (a
-- trigger copies it), and clock_timestamp() under the type's lock when the
-- platform retries it, so a retried job queues behind the live ones
-- instead of jumping ahead on its old created_at (which would let more
-- than the budget be published and live).
--
-- mhai_jobs, narrowed and widened:
--   - its update policies on clinical_queries (00007 jobs_update) and jobs
--     (00012 jobs_role_update) no longer reach a cancelled row: a
--     cancelled job (by the platform, or because its user or hospital was
--     deactivated) can never be re-queued. A trigger on both tables holds
--     the rest for every role but a superuser: a row enters queued only
--     from failed, with retry_count one higher. So a retry is failed →
--     queued and nothing else.
--   - it may set retry_count and deadline_at (and queued_at on jobs), and
--     reads the outbox's two parent ids: the retry waits until a failed
--     job's facts have drained (a fact stating a status the row has left
--     would be refused by the drainer and lost).
--
-- mhai_platform_jobs() gets its body, owned by mhai_jobs_reader, a role of
-- its own like 00015's mhai_health_reader (NOLOGIN, NOINHERIT, member of
-- nothing, no member), holding SELECT on the listed columns only, through
-- policies scoped to the listable rows. Its signature changes: p_not_errors
-- (codes to leave out) is added, so the "other" kind can be asked for as
-- "every code that isn't another kind's" (Go owns the code-to-kind table).
-- It returns the contract's columns only: never the subject, the creator,
-- an error message or the cancel note. mhai_platform runs it; nobody else.

-- +goose Up

------------------------------------------------------------------------
-- clinical_queries: retry_count, failed_at
------------------------------------------------------------------------
ALTER TABLE clinical_queries
    ADD COLUMN retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    ADD COLUMN failed_at   timestamptz;
-- FORCE holds the owner too: lifted for the backfill and put back.
ALTER TABLE clinical_queries NO FORCE ROW LEVEL SECURITY;
UPDATE clinical_queries SET failed_at = completed_at WHERE status = 'failed' AND failed_at IS NULL;
ALTER TABLE clinical_queries FORCE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE FUNCTION mhai_stamp_failed_at() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.failed_at := COALESCE(NEW.failed_at, NEW.completed_at, now());
    ELSE
        NEW.failed_at := now();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_stamp_failed_at() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_stamp_failed_at() FROM PUBLIC;
CREATE TRIGGER clinical_queries_failed_at BEFORE UPDATE OF status ON clinical_queries
    FOR EACH ROW WHEN (NEW.status = 'failed' AND OLD.status IS DISTINCT FROM 'failed')
    EXECUTE FUNCTION mhai_stamp_failed_at();
CREATE TRIGGER clinical_queries_failed_at_insert BEFORE INSERT ON clinical_queries
    FOR EACH ROW WHEN (NEW.status = 'failed') EXECUTE FUNCTION mhai_stamp_failed_at();

------------------------------------------------------------------------
-- jobs: queued_at
------------------------------------------------------------------------
ALTER TABLE jobs ADD COLUMN queued_at timestamptz;
ALTER TABLE jobs NO FORCE ROW LEVEL SECURITY;
UPDATE jobs SET queued_at = created_at;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE FUNCTION mhai_job_queued_at() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    NEW.queued_at := COALESCE(NEW.queued_at, NEW.created_at);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_job_queued_at() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_job_queued_at() FROM PUBLIC;
CREATE TRIGGER jobs_queued_at BEFORE INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION mhai_job_queued_at();
ALTER TABLE jobs ALTER COLUMN queued_at SET NOT NULL;
CREATE INDEX jobs_queue_order_idx ON jobs (hospital_id, type, queued_at, id) WHERE status IN ('queued', 'running');

-- A job is born in its creation-time place: the gateway can't put a new
-- job ahead of the queue.
DROP POLICY gateway_new_job ON jobs;
CREATE POLICY gateway_new_job ON jobs AS RESTRICTIVE FOR INSERT TO mhai_gateway
    WITH CHECK (status = 'queued' AND attempts = 0 AND retry_count = 0 AND cancelled_by IS NULL
                AND cancel_note IS NULL AND cancel_reason IS NULL AND error IS NULL
                AND lease_until IS NULL AND published_at IS NULL
                AND queued_at = created_at
                AND created_by = mhai_current_user());

------------------------------------------------------------------------
-- A retry is failed → queued, nothing else (both tables)
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_job_requeue_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- A superuser (an operator's repair, a test's setup) is not held.
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname = current_user AND r.rolsuper) THEN
        RETURN NEW;
    END IF;
    IF OLD.status IS DISTINCT FROM 'failed' OR NEW.retry_count IS DISTINCT FROM OLD.retry_count + 1 THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'jobs: a row is re-queued only from failed, as a retry';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_job_requeue_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_job_requeue_check() FROM PUBLIC;
CREATE TRIGGER clinical_queries_requeue BEFORE UPDATE OF status ON clinical_queries
    FOR EACH ROW WHEN (NEW.status = 'queued' AND OLD.status IS DISTINCT FROM 'queued')
    EXECUTE FUNCTION mhai_job_requeue_check();
CREATE TRIGGER jobs_requeue BEFORE UPDATE OF status ON jobs
    FOR EACH ROW WHEN (NEW.status = 'queued' AND OLD.status IS DISTINCT FROM 'queued')
    EXECUTE FUNCTION mhai_job_requeue_check();

-- mhai_jobs never touches a cancelled row again (it may still cancel a
-- queued or failed one).
DROP POLICY jobs_update ON clinical_queries;
CREATE POLICY jobs_update ON clinical_queries FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));
DROP POLICY jobs_role_update ON jobs;
CREATE POLICY jobs_role_update ON jobs FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));

------------------------------------------------------------------------
-- mhai_jobs: the retry's columns, and the outbox's parent ids
------------------------------------------------------------------------
GRANT SELECT (retry_count), UPDATE (retry_count, deadline_at) ON clinical_queries TO mhai_jobs;
GRANT SELECT (queued_at), UPDATE (deadline_at, queued_at) ON jobs TO mhai_jobs;
GRANT SELECT (query_id, job_id) ON intelligence_audit_outbox TO mhai_jobs;
CREATE POLICY jobs_facts_read ON intelligence_audit_outbox FOR SELECT TO mhai_jobs USING (true);

------------------------------------------------------------------------
-- query_cancellations
------------------------------------------------------------------------
CREATE TABLE query_cancellations (
    query_id     uuid PRIMARY KEY,
    hospital_id  uuid NOT NULL,
    cancelled_by uuid NOT NULL REFERENCES users (id),
    cancelled_at timestamptz NOT NULL DEFAULT now(),
    cancel_note  text CHECK (cancel_note IS NULL OR char_length(cancel_note) BETWEEN 1 AND 500),
    FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id) ON DELETE CASCADE
);
ALTER TABLE query_cancellations OWNER TO mhai_migrate;
ALTER TABLE query_cancellations ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT INSERT ON query_cancellations TO mhai_jobs;
-- Only for a question of that hospital already cancelled (in the same
-- transaction, by the platform's cancel).
CREATE POLICY jobs_cancel_record ON query_cancellations FOR INSERT TO mhai_jobs
    WITH CHECK (EXISTS (SELECT 1 FROM clinical_queries q
                        WHERE q.id = query_cancellations.query_id
                          AND q.hospital_id = query_cancellations.hospital_id
                          AND q.status = 'cancelled'));

------------------------------------------------------------------------
-- mhai_platform_jobs(), owned by mhai_jobs_reader
------------------------------------------------------------------------
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_jobs_reader') THEN
        CREATE ROLE mhai_jobs_reader NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
    ALTER ROLE mhai_jobs_reader NOLOGIN NOINHERIT NOCREATEDB NOCREATEROLE;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_jobs_reader'
               AND (rolsuper OR rolbypassrls OR rolreplication)) THEN
        ALTER ROLE mhai_jobs_reader NOSUPERUSER NOBYPASSRLS NOREPLICATION;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
               WHERE r.rolname = 'mhai_jobs_reader') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00016: mhai_jobs_reader already has members; remove them first';
    END IF;
    IF NOT pg_has_role(current_user, 'mhai_jobs_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_jobs_reader TO %I', current_user);
        PERFORM set_config('mhai.jobs_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_jobs_reader;
GRANT SELECT (id, hospital_id, status, error, attempts, retry_count, created_at, completed_at, failed_at)
    ON clinical_queries TO mhai_jobs_reader;
GRANT SELECT (id, hospital_id, type, status, error, attempts, retry_count, created_at, completed_at, failed_at,
              cancelled_by, cancelled_at)
    ON jobs TO mhai_jobs_reader;
GRANT SELECT (query_id, cancelled_by, cancelled_at) ON query_cancellations TO mhai_jobs_reader;
-- The listable rows only: failed, cancelled, or retried by the platform.
CREATE POLICY jobs_list_read ON clinical_queries FOR SELECT TO mhai_jobs_reader
    USING (status IN ('failed', 'cancelled') OR retry_count > 0);
CREATE POLICY jobs_list_read ON jobs FOR SELECT TO mhai_jobs_reader
    USING (status IN ('failed', 'cancelled') OR retry_count > 0);
CREATE POLICY jobs_list_read ON query_cancellations FOR SELECT TO mhai_jobs_reader USING (true);

DROP FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer);

-- status is the contract's: cancelled, failed, retrying (queued or running
-- after a retry) or retried (completed after a retry); p_status is one of
-- them or 'all'. Retried and cancelled jobs are listed for 30 days, failed
-- ones until they leave failed. failed_at is the last failure (or, for a
-- job cancelled before it ever failed, when it was cancelled). Newest
-- first, keyset after (p_after_failed_at, p_after_id); the limit is 1-100.
-- +goose StatementBegin
CREATE FUNCTION mhai_platform_jobs(p_status text, p_type text, p_hospital uuid, p_errors text[], p_not_errors text[],
                                   p_after_failed_at timestamptz, p_after_id uuid, p_limit integer)
    RETURNS TABLE (id uuid, type text, hospital_id uuid, created_at timestamptz, failed_at timestamptz,
                   attempts integer, error text, status text, retry_count integer,
                   cancelled_by uuid, cancelled_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('failed', 'retrying', 'retried', 'cancelled', 'all') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'mhai_platform_jobs: unknown status';
    END IF;
    IF (p_after_failed_at IS NULL) <> (p_after_id IS NULL) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'mhai_platform_jobs: half a cursor';
    END IF;
    -- Every output column is also a variable here: every column reference
    -- below is qualified.
    RETURN QUERY
    WITH a AS (
        SELECT cq.id AS jid, 'run_query'::text AS jtype, cq.hospital_id AS hid, cq.created_at AS cat,
               COALESCE(cq.failed_at, c.cancelled_at, cq.completed_at, cq.created_at) AS fat,
               cq.attempts AS att, cq.error::text AS err,
               CASE WHEN cq.status = 'cancelled' THEN 'cancelled'
                    WHEN cq.status = 'failed' THEN 'failed'
                    WHEN cq.status IN ('queued', 'running') AND cq.retry_count > 0 THEN 'retrying'
                    WHEN cq.status = 'completed' AND cq.retry_count > 0 THEN 'retried' END AS st,
               cq.retry_count AS rc, c.cancelled_by AS cby,
               CASE WHEN cq.status = 'cancelled' THEN COALESCE(c.cancelled_at, cq.completed_at) END AS cnat
        FROM public.clinical_queries cq
        LEFT JOIN public.query_cancellations c ON c.query_id = cq.id
        WHERE (p_type IS NULL OR p_type = 'run_query')
          AND (cq.status IN ('failed', 'cancelled') OR cq.retry_count > 0)
        UNION ALL
        SELECT j.id, j.type, j.hospital_id, j.created_at,
               COALESCE(j.failed_at, j.cancelled_at, j.completed_at, j.created_at),
               j.attempts, j.error,
               CASE WHEN j.status = 'cancelled' THEN 'cancelled'
                    WHEN j.status = 'failed' THEN 'failed'
                    WHEN j.status IN ('queued', 'running') AND j.retry_count > 0 THEN 'retrying'
                    WHEN j.status = 'completed' AND j.retry_count > 0 THEN 'retried' END,
               j.retry_count, j.cancelled_by,
               CASE WHEN j.status = 'cancelled' THEN COALESCE(j.cancelled_at, j.completed_at) END
        FROM public.jobs j
        WHERE (p_type IS NULL OR j.type = p_type)
          AND (j.status IN ('failed', 'cancelled') OR j.retry_count > 0)
    )
    SELECT a.jid, a.jtype, a.hid, a.cat, a.fat, a.att, a.err, a.st, a.rc, a.cby, a.cnat
    FROM a
    WHERE a.st IS NOT NULL
      AND (p_status = 'all' OR a.st = p_status)
      AND (a.st = 'failed' OR a.fat >= now() - interval '30 days')
      AND (p_hospital IS NULL OR a.hid = p_hospital)
      AND (p_errors IS NULL OR COALESCE(a.err, '') = ANY (p_errors))
      AND (p_not_errors IS NULL OR NOT (COALESCE(a.err, '') = ANY (p_not_errors)))
      AND (p_after_failed_at IS NULL OR (a.fat, a.jid) < (p_after_failed_at, p_after_id))
    ORDER BY a.fat DESC, a.jid DESC
    LIMIT GREATEST(LEAST(COALESCE(p_limit, 50), 100), 1);
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_platform_jobs(text, text, uuid, text[], text[], timestamptz, uuid, integer) OWNER TO mhai_jobs_reader;
REVOKE ALL ON FUNCTION mhai_platform_jobs(text, text, uuid, text[], text[], timestamptz, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_platform_jobs(text, text, uuid, text[], text[], timestamptz, uuid, integer) TO mhai_platform;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.jobs_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_jobs_reader FROM %I', current_user);
        PERFORM set_config('mhai.jobs_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_jobs_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_jobs_reader TO %I', current_user);
        PERFORM set_config('mhai.jobs_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION mhai_platform_jobs(text, text, uuid, text[], text[], timestamptz, uuid, integer);

-- 00014's stub, as it was.
-- +goose StatementBegin
CREATE FUNCTION mhai_platform_jobs(p_status text, p_type text, p_hospital uuid, p_errors text[],
                                   p_after_failed_at timestamptz, p_after_id uuid, p_limit integer)
    RETURNS TABLE (id uuid, type text, hospital_id uuid, created_at timestamptz, failed_at timestamptz,
                   attempts integer, error text, status text, retry_count integer,
                   cancelled_by uuid, cancelled_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = 'feature_not_supported', MESSAGE = 'mhai_platform_jobs: not built yet';
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer) TO mhai_platform;

DROP POLICY jobs_list_read ON query_cancellations;
DROP POLICY jobs_list_read ON jobs;
DROP POLICY jobs_list_read ON clinical_queries;
REVOKE ALL ON clinical_queries, jobs FROM mhai_jobs_reader;
REVOKE USAGE ON SCHEMA public FROM mhai_jobs_reader;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.jobs_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_jobs_reader FROM %I', current_user);
        PERFORM set_config('mhai.jobs_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- The table takes its policies and grants with it.
DROP TABLE query_cancellations;

DROP POLICY jobs_facts_read ON intelligence_audit_outbox;
REVOKE SELECT (query_id, job_id) ON intelligence_audit_outbox FROM mhai_jobs;
REVOKE SELECT (queued_at), UPDATE (deadline_at, queued_at) ON jobs FROM mhai_jobs;
REVOKE SELECT (retry_count), UPDATE (retry_count, deadline_at) ON clinical_queries FROM mhai_jobs;

-- 00007's and 00012's update policies, as they were.
DROP POLICY jobs_role_update ON jobs;
CREATE POLICY jobs_role_update ON jobs FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed', 'cancelled')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));
DROP POLICY jobs_update ON clinical_queries;
CREATE POLICY jobs_update ON clinical_queries FOR UPDATE TO mhai_jobs
    USING (status IN ('queued', 'failed', 'cancelled')) WITH CHECK (status IN ('queued', 'failed', 'cancelled'));

DROP TRIGGER jobs_requeue ON jobs;
DROP TRIGGER clinical_queries_requeue ON clinical_queries;
DROP FUNCTION mhai_job_requeue_check();

DROP POLICY gateway_new_job ON jobs;
CREATE POLICY gateway_new_job ON jobs AS RESTRICTIVE FOR INSERT TO mhai_gateway
    WITH CHECK (status = 'queued' AND attempts = 0 AND retry_count = 0 AND cancelled_by IS NULL
                AND cancel_note IS NULL AND cancel_reason IS NULL AND error IS NULL
                AND lease_until IS NULL AND published_at IS NULL
                AND created_by = mhai_current_user());
DROP INDEX jobs_queue_order_idx;
DROP TRIGGER jobs_queued_at ON jobs;
DROP FUNCTION mhai_job_queued_at();
ALTER TABLE jobs DROP COLUMN queued_at;

DROP TRIGGER clinical_queries_failed_at_insert ON clinical_queries;
DROP TRIGGER clinical_queries_failed_at ON clinical_queries;
DROP FUNCTION mhai_stamp_failed_at();
ALTER TABLE clinical_queries DROP COLUMN failed_at, DROP COLUMN retry_count;
