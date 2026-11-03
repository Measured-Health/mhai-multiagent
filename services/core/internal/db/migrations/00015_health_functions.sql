-- The health reading's database figures (Plan 4 spec §6.1, Task 11).
--
-- 00014 fixed the health functions' signatures and grants with bodies that
-- refuse; this migration writes the bodies. Each is SECURITY DEFINER, owned
-- by mhai_health_reader (below), with a fixed search_path, executable by
-- mhai_platform only, and returns aggregates only: counts, percentiles, a job type and a
-- stored error code (^[a-z_]{1,32}$, 00007/00012). Never a hospital, a
-- user, a patient, a row id or free text.
--
--   - mhai_health_timings(p_window text): p50 and p95 of the question time
--     (step 'overall': completed_at - created_at of completed questions)
--     and of each step of clinical_decisions.timings_ms, over the last hour
--     or 24 hours. Always five rows, in a fixed order; a step with no
--     samples has null percentiles and 0 samples. The window is the text
--     '1 hour' or '24 hours' exactly. 00014 took an interval, which can't
--     tell '60 minutes' or '1 day' from the two allowed values (interval
--     equality normalises them), so the function is replaced, with a text
--     parameter compared as text.
--   - mhai_health_failures(): failed run_query questions and failed jobs,
--     by job type and error code: failed now, and of those, failed in the
--     last hour and 24 hours. Go maps each code to its kind (intel.ErrorKind).
--   - mhai_health_guidelines() (new): whether any shared (hospital-less)
--     kb_chunks row exists. The reading's guidelines part needs this and
--     mhai_platform has no grant on kb_chunks; shared rows are reference
--     content, never patient data (00002's kb_shared_has_no_patient).
--
-- Who reads: mhai_health_reader, a role of its own (NOLOGIN, NOINHERIT,
-- member of nothing, with no member), not mhai_migrate. It holds SELECT on
-- the columns the bodies aggregate only (never clinical_question, patient
-- data, a decision's text or the masked consultation), and one policy per
-- table, scoped to the rows the bodies need: failed and recently completed
-- questions, their recent decisions, failed jobs, shared chunks. So no
-- other function mhai_migrate owns, and no migration login, sees a row
-- through these policies, and the functions can't read a column they don't
-- aggregate. The role is cluster-wide, so it is created only if missing and
-- kept on Down (as 00007's mhai_jobs), with every grant revoked.
--
-- The migration runner briefly becomes a member (a no-op for a superuser)
-- to hand the functions over, and leaves again.
--
-- Partial indexes on failed rows keep the failures scan to failed rows.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_health_reader') THEN
        CREATE ROLE mhai_health_reader NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
    -- Whatever a pre-existing role looked like, it is this one now. The
    -- superuser, BYPASSRLS and REPLICATION attributes need a superuser to
    -- change, so they are cleared only when set (a non-superuser runner
    -- then fails here, which is the point).
    ALTER ROLE mhai_health_reader NOLOGIN NOINHERIT NOCREATEDB NOCREATEROLE;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_health_reader'
               AND (rolsuper OR rolbypassrls OR rolreplication)) THEN
        ALTER ROLE mhai_health_reader NOSUPERUSER NOBYPASSRLS NOREPLICATION;
    END IF;
    -- Nobody may already act as it: its grants open clinical rows.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
               WHERE r.rolname = 'mhai_health_reader') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00015: mhai_health_reader already has members; remove them first';
    END IF;
    IF NOT pg_has_role(current_user, 'mhai_health_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_health_reader TO %I', current_user);
        PERFORM set_config('mhai.health_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_health_reader;
GRANT SELECT (id, hospital_id, status, error, created_at, completed_at) ON clinical_queries TO mhai_health_reader;
GRANT SELECT (query_id, hospital_id, timings_ms, created_at) ON clinical_decisions TO mhai_health_reader;
GRANT SELECT (type, status, error, failed_at) ON jobs TO mhai_health_reader;
GRANT SELECT (hospital_id) ON kb_chunks TO mhai_health_reader;

CREATE POLICY health_read ON clinical_queries FOR SELECT TO mhai_health_reader
    USING (status = 'failed' OR (status = 'completed' AND completed_at >= now() - interval '24 hours'));
-- A decision is written in its question's completion transaction; the
-- hour of slack keeps the policy from cutting a sample the body counts.
CREATE POLICY health_read ON clinical_decisions FOR SELECT TO mhai_health_reader
    USING (timings_ms IS NOT NULL AND created_at >= now() - interval '25 hours');
CREATE POLICY health_read ON jobs FOR SELECT TO mhai_health_reader
    USING (status = 'failed');
CREATE POLICY health_read ON kb_chunks FOR SELECT TO mhai_health_reader
    USING (hospital_id IS NULL);

CREATE INDEX clinical_queries_failed_idx ON clinical_queries (completed_at) WHERE status = 'failed';
CREATE INDEX jobs_failed_idx ON jobs (failed_at) WHERE status = 'failed';

DROP FUNCTION mhai_health_timings(interval);

-- +goose StatementBegin
CREATE FUNCTION mhai_health_timings(p_window text)
    RETURNS TABLE (step text, p50_ms integer, p95_ms integer, samples bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_since timestamptz;
BEGIN
    -- As text, so an equal interval written another way is refused too.
    IF p_window IS NULL OR p_window NOT IN ('1 hour', '24 hours') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_health_timings: the window is 1 hour or 24 hours';
    END IF;
    v_since := now() - CASE p_window WHEN '1 hour' THEN interval '1 hour' ELSE interval '24 hours' END;
    -- Every output column is also a variable here: every column reference
    -- below is qualified.
    RETURN QUERY
    WITH q AS (
        SELECT cq.id AS qid, cq.hospital_id AS hid,
               EXTRACT(EPOCH FROM (cq.completed_at - cq.created_at))::float8 * 1000 AS ms
        FROM public.clinical_queries cq
        WHERE cq.status = 'completed' AND cq.completed_at >= v_since AND cq.completed_at >= cq.created_at
    ), s AS (
        SELECT 'overall'::text AS k, q.ms AS ms FROM q
        UNION ALL
        SELECT t.key, (t.value #>> '{}')::float8
        FROM q
        JOIN public.clinical_decisions d ON d.query_id = q.qid AND d.hospital_id = q.hid
        CROSS JOIN LATERAL jsonb_each(d.timings_ms) AS t(key, value)
        WHERE d.timings_ms IS NOT NULL AND jsonb_typeof(t.value) = 'number'
    ), ks (k, ord) AS (
        VALUES ('overall'::text, 0), ('privacy', 1), ('guidelines', 2), ('answer', 3), ('evidence', 4)
    )
    -- LEAST ignores a null, so the clamp applies only with samples.
    SELECT ks.k,
           CASE WHEN count(s.ms) > 0
                THEN LEAST(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY s.ms)), 2147483647)::integer END,
           CASE WHEN count(s.ms) > 0
                THEN LEAST(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY s.ms)), 2147483647)::integer END,
           count(s.ms)
    FROM ks LEFT JOIN s ON s.k = ks.k
    GROUP BY ks.k, ks.ord
    ORDER BY ks.ord;
END
$$;
-- +goose StatementEnd

DROP FUNCTION mhai_health_failures();

-- +goose StatementBegin
CREATE FUNCTION mhai_health_failures()
    RETURNS TABLE (job_type text, error text, failed_now bigint, failed_last_hour bigint, failed_last_24h bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- A failed question's failure time is its completed_at (the worker,
    -- the dead-letter consumer and the deadline expiry all set it); a
    -- job's is failed_at.
    RETURN QUERY
    WITH f AS (
        SELECT 'run_query'::text AS t, cq.error AS e, cq.completed_at AS fat
        FROM public.clinical_queries cq WHERE cq.status = 'failed'
        UNION ALL
        SELECT j.type, j.error, j.failed_at
        FROM public.jobs j WHERE j.status = 'failed'
    )
    SELECT f.t, f.e, count(*),
           count(*) FILTER (WHERE f.fat >= now() - interval '1 hour'),
           count(*) FILTER (WHERE f.fat >= now() - interval '24 hours')
    FROM f
    GROUP BY f.t, f.e
    ORDER BY f.t, f.e;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_health_guidelines()
    RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT EXISTS (SELECT 1 FROM public.kb_chunks k WHERE k.hospital_id IS NULL) $$;
-- +goose StatementEnd

ALTER FUNCTION mhai_health_timings(text) OWNER TO mhai_health_reader;
ALTER FUNCTION mhai_health_failures() OWNER TO mhai_health_reader;
ALTER FUNCTION mhai_health_guidelines() OWNER TO mhai_health_reader;
REVOKE ALL ON FUNCTION mhai_health_timings(text), mhai_health_failures(), mhai_health_guidelines() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_health_timings(text), mhai_health_failures(), mhai_health_guidelines() TO mhai_platform;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.health_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_health_reader FROM %I', current_user);
        PERFORM set_config('mhai.health_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_health_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_health_reader TO %I', current_user);
        PERFORM set_config('mhai.health_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION mhai_health_guidelines();
DROP FUNCTION mhai_health_timings(text);
DROP FUNCTION mhai_health_failures();

-- 00014's stubs, as they were.
-- +goose StatementBegin
CREATE FUNCTION mhai_health_timings(time_window interval)
    RETURNS TABLE (step text, p50_ms integer, p95_ms integer, samples bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF time_window IS NULL OR time_window NOT IN (interval '1 hour', interval '24 hours') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_health_timings: the window is 1 hour or 24 hours';
    END IF;
    RAISE EXCEPTION USING ERRCODE = 'feature_not_supported', MESSAGE = 'mhai_health_timings: not built yet';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_health_failures()
    RETURNS TABLE (job_type text, error text, failed_now bigint, failed_last_hour bigint, failed_last_24h bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = 'feature_not_supported', MESSAGE = 'mhai_health_failures: not built yet';
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_health_timings(interval) OWNER TO mhai_migrate;
ALTER FUNCTION mhai_health_failures() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_health_timings(interval), mhai_health_failures() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_health_timings(interval), mhai_health_failures() TO mhai_platform;

DROP INDEX jobs_failed_idx;
DROP INDEX clinical_queries_failed_idx;

DROP POLICY health_read ON kb_chunks;
DROP POLICY health_read ON jobs;
DROP POLICY health_read ON clinical_decisions;
DROP POLICY health_read ON clinical_queries;
REVOKE ALL ON kb_chunks, jobs, clinical_decisions, clinical_queries FROM mhai_health_reader;
REVOKE USAGE ON SCHEMA public FROM mhai_health_reader;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.health_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_health_reader FROM %I', current_user);
        PERFORM set_config('mhai.health_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd
