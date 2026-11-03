-- The guardrail statistics' counts (Plan 4 spec §6.3, Task 13).
--
-- 00014 fixed mhai_guardrail_counts(p_from, p_to)'s signature and grant
-- with a body that refuses; this migration writes the body. It is SECURITY
-- DEFINER, owned by mhai_guardrail_reader (below), with a fixed
-- search_path, executable by mhai_platform only, and returns aggregates
-- only: per active hospital, over [p_from, p_to), the answers (decisions),
-- and of those PASS, REGENERATE and the rest (ESCALATE, and any other or
-- missing decision, counted as ESCALATE as the worker fails closed), and
-- the degraded ones. Every active hospital has a row, zeros included. Never
-- a decision's text, a question, a user, a patient or a row id. The
-- small-group rules are applied in Go (internal/smallgroup) before the
-- counts are encoded; raw counts never leave core.
--
-- The window is at most 90 days, starts no earlier than 91 days ago and
-- ends no later than a day from now (22023 otherwise), so the scan is
-- bounded whatever the caller passes.
--
-- Who reads: mhai_guardrail_reader, a role of its own like 00015's
-- mhai_health_reader and 00016's mhai_jobs_reader (NOLOGIN, NOINHERIT,
-- member of nothing, with no member), not mhai_migrate. It holds SELECT on
-- the columns the body aggregates only (hospital_id, hga_decision,
-- degraded and created_at of clinical_decisions; id and status of
-- hospitals), through one policy per table scoped to the rows the body
-- needs: decisions of the last 91 days, active hospitals. The role is
-- cluster-wide, so it is created only if missing and kept on Down, with
-- every grant revoked.
--
-- An index on clinical_decisions (created_at, hospital_id) keeps the scan
-- to the window.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_guardrail_reader') THEN
        CREATE ROLE mhai_guardrail_reader NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
    ALTER ROLE mhai_guardrail_reader NOLOGIN NOINHERIT NOCREATEDB NOCREATEROLE;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_guardrail_reader'
               AND (rolsuper OR rolbypassrls OR rolreplication)) THEN
        ALTER ROLE mhai_guardrail_reader NOSUPERUSER NOBYPASSRLS NOREPLICATION;
    END IF;
    -- Nobody may already act as it: its grants open clinical rows.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
               WHERE r.rolname = 'mhai_guardrail_reader') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00017: mhai_guardrail_reader already has members; remove them first';
    END IF;
    IF NOT pg_has_role(current_user, 'mhai_guardrail_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_guardrail_reader TO %I', current_user);
        PERFORM set_config('mhai.guardrail_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_guardrail_reader;
GRANT SELECT (hospital_id, hga_decision, degraded, created_at) ON clinical_decisions TO mhai_guardrail_reader;
GRANT SELECT (id, status) ON hospitals TO mhai_guardrail_reader;

CREATE POLICY guardrail_read ON clinical_decisions FOR SELECT TO mhai_guardrail_reader
    USING (created_at >= now() - interval '91 days');
CREATE POLICY guardrail_read ON hospitals FOR SELECT TO mhai_guardrail_reader
    USING (status = 'active');

CREATE INDEX clinical_decisions_created_idx ON clinical_decisions (created_at, hospital_id);

DROP FUNCTION mhai_guardrail_counts(timestamptz, timestamptz);

-- +goose StatementBegin
CREATE FUNCTION mhai_guardrail_counts(p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (hospital_id uuid, answers bigint, pass bigint, regenerate bigint, escalate bigint, degraded bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_from IS NULL OR p_to IS NULL OR p_from >= p_to
       OR p_to - p_from > interval '90 days'
       OR p_from < now() - interval '91 days'
       OR p_to > now() + interval '1 day' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_guardrail_counts: the window is at most 90 days within the last 91';
    END IF;
    -- Every output column is also a variable here: every column reference
    -- below is qualified.
    RETURN QUERY
    SELECT h.id,
           count(d.hospital_id),
           count(d.hospital_id) FILTER (WHERE d.hga_decision = 'PASS'),
           count(d.hospital_id) FILTER (WHERE d.hga_decision = 'REGENERATE'),
           count(d.hospital_id) FILTER (WHERE d.hga_decision IS NULL OR d.hga_decision NOT IN ('PASS', 'REGENERATE')),
           count(d.hospital_id) FILTER (WHERE d.degraded IS TRUE)
    FROM public.hospitals h
    LEFT JOIN public.clinical_decisions d
           ON d.hospital_id = h.id AND d.created_at >= p_from AND d.created_at < p_to
    WHERE h.status = 'active'
    GROUP BY h.id
    ORDER BY h.id;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) OWNER TO mhai_guardrail_reader;
REVOKE ALL ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) TO mhai_platform;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.guardrail_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_guardrail_reader FROM %I', current_user);
        PERFORM set_config('mhai.guardrail_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_guardrail_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_guardrail_reader TO %I', current_user);
        PERFORM set_config('mhai.guardrail_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION mhai_guardrail_counts(timestamptz, timestamptz);

-- 00014's stub, as it was.
-- +goose StatementBegin
CREATE FUNCTION mhai_guardrail_counts(p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (hospital_id uuid, answers bigint, pass bigint, regenerate bigint, escalate bigint, degraded bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = 'feature_not_supported', MESSAGE = 'mhai_guardrail_counts: not built yet';
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) TO mhai_platform;

DROP INDEX clinical_decisions_created_idx;

DROP POLICY guardrail_read ON hospitals;
DROP POLICY guardrail_read ON clinical_decisions;
REVOKE ALL ON clinical_decisions, hospitals FROM mhai_guardrail_reader;
REVOKE USAGE ON SCHEMA public FROM mhai_guardrail_reader;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.guardrail_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_guardrail_reader FROM %I', current_user);
        PERFORM set_config('mhai.guardrail_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd
