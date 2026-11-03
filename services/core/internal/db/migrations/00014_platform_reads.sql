-- The platform's reads (Plan 4 spec §5.4, §5.5, §6.1-§6.3, §10.3).
--
-- audit_verify_runs holds the chain checks: global (no hospital), read and
-- written by mhai_platform only. One run may be running at a time (a
-- partial unique index), and the row's own CHECKs hold what each state
-- means whoever writes it: an intact run carries every proof field, a
-- broken one its first broken entry, an error one its code, and only a
-- broken run carries an outcome note (at most 1000 characters, with its
-- author and time). The note is free text: it lives on this row only, never
-- in the audit chain or a log.
-- The platform inserts a run only as running, and a trigger fixes a
-- finished run but for its outcome and version.
--
-- mhai_platform's append to audit_log (00003: WITH CHECK (true)) narrows to
-- spec §5.5's eight actions, with no hospital: a platform account has none.
-- Its INSERT, the sequence and audit_chain_head() are 00003's grants, kept;
-- audit.Write's advisory lock and chain head are the same path the other
-- roles take, so the chain stays one chain.
--
-- mhai_platform holds no grant on any clinical table (C17). It reaches
-- clinical data only through four SECURITY DEFINER functions owned by
-- mhai_migrate, which return aggregates or the failed-jobs list's columns
-- only. This migration fixes their signatures and grants; each body is a
-- refusal until Plan 4 Tasks 11-13 write it (a function with no body yet
-- answers nothing rather than something wrong). The health window is
-- checked already: 1 hour or 24 hours, nothing else.

-- +goose Up

CREATE TABLE audit_verify_runs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    -- The person who started it; null for the daily schedule.
    by               uuid REFERENCES users (id),
    state            text NOT NULL DEFAULT 'running'
                     CHECK (state IN ('running', 'intact', 'broken', 'error')),
    entries_checked  bigint CHECK (entries_checked IS NULL OR entries_checked >= 0),
    from_first       boolean NOT NULL DEFAULT false,
    -- Always null until signed checkpoints exist (spec §5.4).
    from_checkpoint  bigint REFERENCES audit_checkpoints (id),
    to_seq           bigint CHECK (to_seq IS NULL OR to_seq > 0),
    to_at            timestamptz,
    first_broken_seq bigint CHECK (first_broken_seq IS NULL OR first_broken_seq > 0),
    first_broken_id  text CHECK (first_broken_id IS NULL OR first_broken_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    first_broken_at  timestamptz,
    error_code       text CHECK (error_code IS NULL OR error_code IN ('timed_out', 'unreachable', 'unexpected_reply')),
    outcome_note     text CHECK (outcome_note IS NULL OR char_length(outcome_note) BETWEEN 1 AND 1000),
    outcome_by       uuid REFERENCES users (id),
    outcome_at       timestamptz,
    version          integer NOT NULL DEFAULT 1 CHECK (version > 0),
    -- Running exactly while unfinished.
    CONSTRAINT audit_verify_runs_finished CHECK ((state = 'running') = (finished_at IS NULL)),
    -- intact only when the whole chain was walked (C24).
    -- Every clause is NULL-safe: a NULL inside the OR would pass the CHECK.
    CONSTRAINT audit_verify_runs_intact_proof CHECK (state <> 'intact'
        OR (entries_checked IS NOT NULL AND entries_checked > 0
            AND to_seq IS NOT NULL AND to_at IS NOT NULL AND from_first)),
    -- The first broken entry: all three fields together, and only on a broken run.
    CONSTRAINT audit_verify_runs_broken_entry CHECK (
        num_nonnulls(first_broken_seq, first_broken_id, first_broken_at) IN (0, 3)
        AND (state = 'broken') = (first_broken_seq IS NOT NULL)),
    CONSTRAINT audit_verify_runs_error_code CHECK ((state = 'error') = (error_code IS NOT NULL)),
    -- The outcome: note, author and time together, on a broken run only.
    CONSTRAINT audit_verify_runs_outcome CHECK (
        num_nonnulls(outcome_note, outcome_by, outcome_at) IN (0, 3)
        AND (outcome_note IS NULL OR state = 'broken'))
);
-- One run at a time: POST /verify returns the running one (spec §5.4, C25).
CREATE UNIQUE INDEX audit_verify_one_running ON audit_verify_runs ((true)) WHERE state = 'running';
CREATE INDEX audit_verify_runs_started_idx ON audit_verify_runs (started_at DESC, id DESC);
ALTER TABLE audit_verify_runs OWNER TO mhai_migrate;
ALTER TABLE audit_verify_runs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- mhai_platform: start runs, read them, and finish them or add the outcome.
-- Never delete one, or change who or when started it.
GRANT SELECT, INSERT ON audit_verify_runs TO mhai_platform;
GRANT UPDATE (finished_at, state, entries_checked, from_first, from_checkpoint, to_seq, to_at,
              first_broken_seq, first_broken_id, first_broken_at, error_code,
              outcome_note, outcome_by, outcome_at, version)
    ON audit_verify_runs TO mhai_platform;
CREATE POLICY platform_runs ON audit_verify_runs TO mhai_platform USING (true) WITH CHECK (true);
-- A run is born running, with no outcome: the platform can't insert a
-- finished (say, intact) run, only finish one it started.
CREATE POLICY platform_runs_start ON audit_verify_runs AS RESTRICTIVE FOR INSERT TO mhai_platform
    WITH CHECK (state = 'running' AND outcome_note IS NULL);

-- Only a running run may finish; a finished run is fixed, but for its
-- outcome (note, author, time) and version. Whoever writes, owner included.
-- +goose StatementBegin
CREATE FUNCTION mhai_verify_run_transition() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- "by" is quoted: plpgsql reads a bare BY as a keyword.
    IF OLD.state <> 'running'
       AND (NEW.id, NEW.started_at, NEW.finished_at, NEW."by", NEW.state, NEW.entries_checked, NEW.from_first,
            NEW.from_checkpoint, NEW.to_seq, NEW.to_at, NEW.first_broken_seq, NEW.first_broken_id,
            NEW.first_broken_at, NEW.error_code)
           IS DISTINCT FROM
           (OLD.id, OLD.started_at, OLD.finished_at, OLD."by", OLD.state, OLD.entries_checked, OLD.from_first,
            OLD.from_checkpoint, OLD.to_seq, OLD.to_at, OLD.first_broken_seq, OLD.first_broken_id,
            OLD.first_broken_at, OLD.error_code) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'audit_verify_runs: a finished run changes only its outcome';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_verify_run_transition() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_verify_run_transition() FROM PUBLIC;
CREATE TRIGGER audit_verify_runs_transition BEFORE UPDATE ON audit_verify_runs
    FOR EACH ROW EXECUTE FUNCTION mhai_verify_run_transition();

------------------------------------------------------------------------
-- mhai_platform's audit append: spec §5.5's eight actions, no hospital.
------------------------------------------------------------------------
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled')
                AND hospital_id IS NULL);

------------------------------------------------------------------------
-- The four platform functions. Owned by mhai_migrate (under FORCE, the
-- owner reads a clinical table only through a policy a later body adds),
-- a fixed search_path, executable by mhai_platform only.
------------------------------------------------------------------------

-- p50 and p95 of the question time ('overall') and of each step
-- (privacy, guidelines, answer, evidence), over the last hour or 24 hours.
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

-- Failed jobs (run_query and jobs rows) by type and error code: failed now,
-- and failed in the last hour and 24 hours. Go maps codes to kinds.
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

-- The failed-jobs list (spec §6.2): the contract's columns only, never the
-- subject, the creator, an error message or the cancel note. Newest failure
-- first, keyset after (failed_at, id), the limit clamped in the body.
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

-- HGA decision counts per active hospital over [p_from, p_to) (spec §6.3).
-- Raw counts: the small-group rules are applied in Go before encoding.
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

ALTER FUNCTION mhai_health_timings(interval) OWNER TO mhai_migrate;
ALTER FUNCTION mhai_health_failures() OWNER TO mhai_migrate;
ALTER FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer) OWNER TO mhai_migrate;
ALTER FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_health_timings(interval), mhai_health_failures(),
    mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer),
    mhai_guardrail_counts(timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_health_timings(interval), mhai_health_failures(),
    mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer),
    mhai_guardrail_counts(timestamptz, timestamptz) TO mhai_platform;

-- +goose Down
DROP FUNCTION mhai_guardrail_counts(timestamptz, timestamptz);
DROP FUNCTION mhai_platform_jobs(text, text, uuid, text[], timestamptz, uuid, integer);
DROP FUNCTION mhai_health_failures();
DROP FUNCTION mhai_health_timings(interval);

-- 00003's append, as it was.
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform WITH CHECK (true);

-- The table takes its policies, grants, indexes and trigger with it.
DROP TABLE audit_verify_runs;
DROP FUNCTION mhai_verify_run_transition();
