-- Platform analytics on calendar periods (Plan 7c spec §4, §6.2, §8, §9;
-- rulings R41a, R41a-2, R41a-3).
--
-- Platform statistics show only finished, nested calendar periods (UTC):
-- month-aligned weeks (days 1–7, 8–14, 15–21, 22 to the month's end),
-- months and quarters. mhai_reporting_period_ok(kind, from, to) is the SQL
-- twin of internal/analytics/calendar ([from, to), to exclusive); every
-- window below goes through it, so a rolling window can't be counted even
-- by SQL run as the scheduler's role.
--
-- Roles:
--   - mhai_analytics_reader (NOLOGIN, NOINHERIT, member of nothing, with no
--     member, as 00017's mhai_guardrail_reader): owns the four functions and
--     holds column SELECT on exactly the clinical columns they aggregate, rows
--     of the last 120 days, plus SELECT and INSERT on the frozen counts. It
--     owns no table.
--   - mhai_analytics_job: the analytics scheduler's role, reached by SET ROLE
--     from the core login (mhai_app WITH INHERIT FALSE), as mhai_jobs is
--     (00007): the only role with EXECUTE on the four functions, and the only
--     one that inserts snapshots. Core gives it to no route (db.WithAnalyticsJob,
--     internal/analytics only).
--   Both are cluster-wide: created if missing and kept on Down, with every
--   grant revoked.
--
-- Tables (owner mhai_migrate, RLS enabled and forced, nothing for PUBLIC):
--   - analytics_epoch: one row, the release (R41a-3). Only periods that start
--     at or after it are eligible; a month or quarter is eligible only if all
--     its children are, which for nested periods is the same test on its
--     first day.
--   - analytics_frozen_periods, analytics_period_counts: each finished
--     period's raw per-hospital counts, frozen once (§6.2). No platform role
--     may read them; the reader inserts and reads them inside the functions.
--   - analytics_snapshots: one guarded snapshot per finished period; the
--     routes (mhai_platform) read it, the scheduler inserts it once.
--
-- mhai_guardrail_counts (00017) returns unguarded counts for any window up
-- to 90 days, and the deprecated rolling guardrail values are now served
-- fully hidden: EXECUTE is revoked from mhai_platform (§9, A30). The stored
-- rolling snapshots are emptied (R41a-3); Down can't bring them back (they
-- were a cache of guarded figures).

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analytics_reader') THEN
        CREATE ROLE mhai_analytics_reader NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
    ALTER ROLE mhai_analytics_reader NOLOGIN NOINHERIT NOCREATEDB NOCREATEROLE;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analytics_reader'
               AND (rolsuper OR rolbypassrls OR rolreplication)) THEN
        ALTER ROLE mhai_analytics_reader NOSUPERUSER NOBYPASSRLS NOREPLICATION;
    END IF;
    -- Nobody may already act as it: its grants open clinical rows.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
               WHERE r.rolname = 'mhai_analytics_reader') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00019: mhai_analytics_reader already has members; remove them first';
    END IF;
    IF NOT pg_has_role(current_user, 'mhai_analytics_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_analytics_reader TO %I', current_user);
        PERFORM set_config('mhai.analytics_reader_granted', 'yes', false);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analytics_job') THEN
        CREATE ROLE mhai_analytics_job NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT mhai_analytics_job TO mhai_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_analytics_job, mhai_analytics_reader;

-- The calendar's SQL twin: [p_from, p_to) is a nested period of p_kind.
CREATE FUNCTION mhai_reporting_period_ok(p_kind text, p_from date, p_to date) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT COALESCE(CASE p_kind
        WHEN 'week' THEN extract(day FROM p_from) IN (1, 8, 15, 22)
             AND p_to = CASE WHEN extract(day FROM p_from) = 22
                             THEN (date_trunc('month', p_from) + interval '1 month')::date
                             ELSE p_from + 7 END
        WHEN 'month' THEN extract(day FROM p_from) = 1 AND p_to = (p_from + interval '1 month')::date
        WHEN 'quarter' THEN extract(day FROM p_from) = 1 AND extract(month FROM p_from) IN (1, 4, 7, 10)
             AND p_to = (p_from + interval '3 months')::date
        ELSE false END, false)
$$;
ALTER FUNCTION mhai_reporting_period_ok(text, date, date) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_reporting_period_ok(text, date, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_reporting_period_ok(text, date, date) TO mhai_platform, mhai_analytics_job, mhai_analytics_reader;

-- The release epoch (R41a-3): one row, set now.
CREATE TABLE analytics_epoch (
    one         boolean     PRIMARY KEY DEFAULT true CHECK (one),
    released_at timestamptz NOT NULL
);
INSERT INTO analytics_epoch (released_at) VALUES (now());
ALTER TABLE analytics_epoch OWNER TO mhai_migrate;
ALTER TABLE analytics_epoch ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_epoch FROM PUBLIC;
GRANT SELECT ON analytics_epoch TO mhai_platform, mhai_analytics_job, mhai_analytics_reader;
CREATE POLICY analytics_epoch_read ON analytics_epoch FOR SELECT TO mhai_platform, mhai_analytics_job, mhai_analytics_reader USING (true);

-- A period is eligible when it starts at or after the release: a week then
-- closes after it, and a parent's children all do (its first week starts
-- on its first day).
CREATE FUNCTION mhai_reporting_eligible(p_from date) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT COALESCE((p_from::timestamp AT TIME ZONE 'UTC') >= (SELECT e.released_at FROM public.analytics_epoch e), false)
$$;
ALTER FUNCTION mhai_reporting_eligible(date) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_reporting_eligible(date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_reporting_eligible(date) TO mhai_platform, mhai_analytics_job, mhai_analytics_reader;

-- Frozen counts (§6.2): the marker first, once per period.
CREATE TABLE analytics_frozen_periods (
    kind        text        NOT NULL,
    period_from date        NOT NULL,
    period_to   date        NOT NULL,
    frozen_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, period_from),
    UNIQUE (kind, period_from, frozen_at),
    CHECK (mhai_reporting_period_ok(kind, period_from, period_to))
);
CREATE TABLE analytics_period_counts (
    kind                  text   NOT NULL,
    period_from           date   NOT NULL,
    frozen_at             timestamptz NOT NULL,
    hospital_id           uuid   NOT NULL REFERENCES hospitals (id),
    q_pass                bigint NOT NULL CHECK (q_pass >= 0),
    q_pass_degraded       bigint NOT NULL CHECK (q_pass_degraded >= 0),
    q_regenerate          bigint NOT NULL CHECK (q_regenerate >= 0),
    q_regenerate_degraded bigint NOT NULL CHECK (q_regenerate_degraded >= 0),
    q_escalate            bigint NOT NULL CHECK (q_escalate >= 0),
    q_escalate_degraded   bigint NOT NULL CHECK (q_escalate_degraded >= 0),
    q_unanswered          bigint NOT NULL CHECK (q_unanswered >= 0),
    r_agreed              bigint NOT NULL CHECK (r_agreed >= 0),
    r_corrected           bigint NOT NULL CHECK (r_corrected >= 0),
    active_clinicians     bigint NOT NULL CHECK (active_clinicians >= 0),
    reviews_decided       bigint NOT NULL CHECK (reviews_decided >= 0),
    median_review_hours   double precision CHECK (median_review_hours >= 0),
    PRIMARY KEY (kind, period_from, hospital_id),
    FOREIGN KEY (kind, period_from, frozen_at) REFERENCES analytics_frozen_periods (kind, period_from, frozen_at)
);
ALTER TABLE analytics_frozen_periods OWNER TO mhai_migrate;
ALTER TABLE analytics_period_counts OWNER TO mhai_migrate;
ALTER TABLE analytics_frozen_periods ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analytics_period_counts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_frozen_periods, analytics_period_counts FROM PUBLIC;
GRANT SELECT, INSERT ON analytics_frozen_periods, analytics_period_counts TO mhai_analytics_reader;
CREATE POLICY analytics_reader ON analytics_frozen_periods TO mhai_analytics_reader USING (true) WITH CHECK (true);
CREATE POLICY analytics_reader ON analytics_period_counts TO mhai_analytics_reader USING (true) WITH CHECK (true);

-- The stored snapshots (§8.1): guarded output only.
CREATE TABLE analytics_snapshots (
    kind        text        NOT NULL,
    period_from date        NOT NULL,
    period_to   date        NOT NULL,
    state       text        NOT NULL CHECK (state IN ('ok', 'unavailable')),
    body        jsonb,
    computed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, period_from),
    CHECK (mhai_reporting_period_ok(kind, period_from, period_to)),
    CHECK ((body IS NULL) = (state = 'unavailable')),
    CHECK (body IS NULL OR (jsonb_typeof(body) = 'object' AND octet_length(body::text) <= 4194304))
);
ALTER TABLE analytics_snapshots OWNER TO mhai_migrate;
ALTER TABLE analytics_snapshots ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_snapshots FROM PUBLIC;
GRANT SELECT ON analytics_snapshots TO mhai_platform;
GRANT SELECT, INSERT ON analytics_snapshots TO mhai_analytics_job;
CREATE POLICY analytics_platform_read ON analytics_snapshots FOR SELECT TO mhai_platform USING (true);
CREATE POLICY analytics_job_read ON analytics_snapshots FOR SELECT TO mhai_analytics_job USING (true);
-- A finished period (its end plus the 15-minute grace) that is eligible.
CREATE POLICY analytics_job_insert ON analytics_snapshots FOR INSERT TO mhai_analytics_job
    WITH CHECK ((period_to::timestamp AT TIME ZONE 'UTC') + interval '15 minutes' <= now()
                AND mhai_reporting_eligible(period_from));

-- The reader's columns and rows (§8.2).
GRANT SELECT (id, hospital_id, created_by, created_at) ON clinical_queries TO mhai_analytics_reader;
GRANT SELECT (query_id, hospital_id, hga_decision, degraded, created_at) ON clinical_decisions TO mhai_analytics_reader;
GRANT SELECT (hospital_id, action, status, created_at, decided_at) ON feedback_events TO mhai_analytics_reader;
GRANT SELECT (id, name, status, created_at) ON hospitals TO mhai_analytics_reader;
GRANT SELECT (id, hospital_id, is_active) ON users TO mhai_analytics_reader;
GRANT SELECT (user_id, role) ON user_roles TO mhai_analytics_reader;
CREATE POLICY analytics_read ON clinical_queries FOR SELECT TO mhai_analytics_reader
    USING (created_at >= now() - interval '120 days');
CREATE POLICY analytics_read ON clinical_decisions FOR SELECT TO mhai_analytics_reader
    USING (created_at >= now() - interval '120 days');
CREATE POLICY analytics_read ON feedback_events FOR SELECT TO mhai_analytics_reader
    USING (created_at >= now() - interval '120 days' OR decided_at >= now() - interval '120 days');
CREATE POLICY analytics_read ON hospitals FOR SELECT TO mhai_analytics_reader USING (true);
CREATE POLICY analytics_read ON users FOR SELECT TO mhai_analytics_reader USING (true);
CREATE POLICY analytics_read ON user_roles FOR SELECT TO mhai_analytics_reader USING (true);

CREATE INDEX clinical_queries_created_idx ON clinical_queries (created_at, hospital_id);
CREATE INDEX feedback_events_created_idx ON feedback_events (created_at, hospital_id);
CREATE INDEX feedback_events_decided_idx ON feedback_events (decided_at, hospital_id);

-- mhai_analytics_freeze(kind, from): freezes one finished, eligible period,
-- once. A week from the clinical tables (questions by their created_at,
-- each with its decision if one exists now); a month or a quarter as the sum
-- of its frozen children. Distinct clinicians and review times over the
-- period's own days. Every hospital active now or with rows in the period
-- (a parent: in any child). Returns nothing.
-- +goose StatementBegin
CREATE FUNCTION mhai_analytics_freeze(p_kind text, p_from date) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_to       date;
    v_children date[];
    v_child    text;
    v_at       timestamptz;
    v_lo       timestamptz;
    v_hi       timestamptz;
BEGIN
    IF p_kind IS NULL OR p_from IS NULL OR p_kind NOT IN ('week', 'month', 'quarter') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_analytics_freeze: not a reporting period';
    END IF;
    v_to := CASE p_kind
        WHEN 'week' THEN CASE WHEN extract(day FROM p_from) = 22
                              THEN (date_trunc('month', p_from) + interval '1 month')::date ELSE p_from + 7 END
        WHEN 'month' THEN (p_from + interval '1 month')::date
        ELSE (p_from + interval '3 months')::date END;
    v_lo := p_from::timestamp AT TIME ZONE 'UTC';
    v_hi := v_to::timestamp AT TIME ZONE 'UTC';
    IF NOT public.mhai_reporting_period_ok(p_kind, p_from, v_to)
       OR v_hi + interval '15 minutes' > now()
       OR NOT public.mhai_reporting_eligible(p_from)
       OR v_lo < now() - interval '118 days' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_analytics_freeze: not a finished, eligible reporting period within reach';
    END IF;
    IF p_kind = 'month' THEN
        v_children := ARRAY[p_from, p_from + 7, p_from + 14, p_from + 21];
        v_child := 'week';
    ELSIF p_kind = 'quarter' THEN
        v_children := ARRAY[p_from, (p_from + interval '1 month')::date, (p_from + interval '2 months')::date];
        v_child := 'month';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analytics.freeze'), hashtext(p_kind || ' ' || p_from::text));
    IF v_children IS NOT NULL AND (SELECT count(*) FROM public.analytics_frozen_periods f
            WHERE f.kind = v_child AND f.period_from = ANY (v_children)) <> cardinality(v_children) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_analytics_freeze: a child period is not frozen';
    END IF;
    INSERT INTO public.analytics_frozen_periods (kind, period_from, period_to)
        VALUES (p_kind, p_from, v_to) ON CONFLICT DO NOTHING
        RETURNING frozen_at INTO v_at;
    IF v_at IS NULL THEN
        RETURN; -- frozen already: never recounted
    END IF;

    IF p_kind = 'week' THEN
        INSERT INTO public.analytics_period_counts (kind, period_from, frozen_at, hospital_id,
            q_pass, q_pass_degraded, q_regenerate, q_regenerate_degraded, q_escalate, q_escalate_degraded,
            q_unanswered, r_agreed, r_corrected, active_clinicians, reviews_decided, median_review_hours)
        WITH q AS (
            SELECT cq.hospital_id, cq.created_by, d.query_id IS NOT NULL AS answered,
                   COALESCE(d.hga_decision, '') AS hga, d.degraded IS TRUE AS deg
            FROM public.clinical_queries cq
            LEFT JOIN public.clinical_decisions d ON d.query_id = cq.id AND d.hospital_id = cq.hospital_id
            WHERE cq.created_at >= v_lo AND cq.created_at < v_hi
        ), qc AS (
            SELECT hospital_id,
                   count(*) FILTER (WHERE answered AND hga = 'PASS' AND NOT deg) AS p,
                   count(*) FILTER (WHERE answered AND hga = 'PASS' AND deg) AS pd,
                   count(*) FILTER (WHERE answered AND hga = 'REGENERATE' AND NOT deg) AS g,
                   count(*) FILTER (WHERE answered AND hga = 'REGENERATE' AND deg) AS gd,
                   count(*) FILTER (WHERE answered AND hga NOT IN ('PASS', 'REGENERATE') AND NOT deg) AS e,
                   count(*) FILTER (WHERE answered AND hga NOT IN ('PASS', 'REGENERATE') AND deg) AS ed,
                   count(*) FILTER (WHERE NOT answered) AS u,
                   count(DISTINCT created_by) AS clinicians
            FROM q GROUP BY hospital_id
        ), rc AS (
            SELECT f.hospital_id,
                   count(*) FILTER (WHERE f.action = 'validate') AS agreed,
                   count(*) FILTER (WHERE f.action = 'correct') AS corrected
            FROM public.feedback_events f
            WHERE f.created_at >= v_lo AND f.created_at < v_hi AND f.status <> 'withdrawn'
            GROUP BY f.hospital_id
        ), dc AS (
            SELECT f.hospital_id, count(*) AS decided,
                   percentile_cont(0.5) WITHIN GROUP (ORDER BY (extract(epoch FROM f.decided_at - f.created_at) / 3600.0)::double precision) AS median
            FROM public.feedback_events f
            WHERE f.decided_at >= v_lo AND f.decided_at < v_hi AND f.status IN ('approved', 'rejected')
            GROUP BY f.hospital_id
        ), hs AS (
            SELECT h.id FROM public.hospitals h
            WHERE h.status = 'active' OR h.id IN (SELECT hospital_id FROM qc UNION SELECT hospital_id FROM rc
                                                  UNION SELECT hospital_id FROM dc)
        )
        SELECT 'week', p_from, v_at, hs.id,
               COALESCE(qc.p, 0), COALESCE(qc.pd, 0), COALESCE(qc.g, 0), COALESCE(qc.gd, 0),
               COALESCE(qc.e, 0), COALESCE(qc.ed, 0), COALESCE(qc.u, 0),
               COALESCE(rc.agreed, 0), COALESCE(rc.corrected, 0), COALESCE(qc.clinicians, 0),
               COALESCE(dc.decided, 0), dc.median
        FROM hs
        LEFT JOIN qc ON qc.hospital_id = hs.id
        LEFT JOIN rc ON rc.hospital_id = hs.id
        LEFT JOIN dc ON dc.hospital_id = hs.id;
    ELSE
        INSERT INTO public.analytics_period_counts (kind, period_from, frozen_at, hospital_id,
            q_pass, q_pass_degraded, q_regenerate, q_regenerate_degraded, q_escalate, q_escalate_degraded,
            q_unanswered, r_agreed, r_corrected, active_clinicians, reviews_decided, median_review_hours)
        WITH ch AS (
            SELECT c.hospital_id,
                   sum(c.q_pass) AS p, sum(c.q_pass_degraded) AS pd, sum(c.q_regenerate) AS g,
                   sum(c.q_regenerate_degraded) AS gd, sum(c.q_escalate) AS e, sum(c.q_escalate_degraded) AS ed,
                   sum(c.q_unanswered) AS u, sum(c.r_agreed) AS agreed, sum(c.r_corrected) AS corrected
            FROM public.analytics_period_counts c
            WHERE c.kind = v_child AND c.period_from = ANY (v_children)
            GROUP BY c.hospital_id
        ), cl AS (
            SELECT cq.hospital_id, count(DISTINCT cq.created_by) AS clinicians
            FROM public.clinical_queries cq
            WHERE cq.created_at >= v_lo AND cq.created_at < v_hi
            GROUP BY cq.hospital_id
        ), dc AS (
            SELECT f.hospital_id, count(*) AS decided,
                   percentile_cont(0.5) WITHIN GROUP (ORDER BY (extract(epoch FROM f.decided_at - f.created_at) / 3600.0)::double precision) AS median
            FROM public.feedback_events f
            WHERE f.decided_at >= v_lo AND f.decided_at < v_hi AND f.status IN ('approved', 'rejected')
            GROUP BY f.hospital_id
        ), hs AS (
            SELECT h.id FROM public.hospitals h
            WHERE h.status = 'active' OR h.id IN (SELECT hospital_id FROM ch UNION SELECT hospital_id FROM cl
                                                  UNION SELECT hospital_id FROM dc)
        )
        SELECT p_kind, p_from, v_at, hs.id,
               COALESCE(ch.p, 0), COALESCE(ch.pd, 0), COALESCE(ch.g, 0), COALESCE(ch.gd, 0),
               COALESCE(ch.e, 0), COALESCE(ch.ed, 0), COALESCE(ch.u, 0),
               COALESCE(ch.agreed, 0), COALESCE(ch.corrected, 0), COALESCE(cl.clinicians, 0),
               COALESCE(dc.decided, 0), dc.median
        FROM hs
        LEFT JOIN ch ON ch.hospital_id = hs.id
        LEFT JOIN cl ON cl.hospital_id = hs.id
        LEFT JOIN dc ON dc.hospital_id = hs.id;
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_analytics_frozen(quarter): the frozen rows of one block, per period
-- and hospital, for the guard. Every hospital (any status).
-- +goose StatementBegin
CREATE FUNCTION mhai_analytics_frozen(p_quarter_from date)
    RETURNS TABLE (kind text, period_from date, period_to date, hospital_id uuid,
                   q_pass bigint, q_pass_degraded bigint, q_regenerate bigint, q_regenerate_degraded bigint,
                   q_escalate bigint, q_escalate_degraded bigint, q_unanswered bigint,
                   r_agreed bigint, r_corrected bigint, active_clinicians bigint, reviews_decided bigint,
                   median_review_hours double precision)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT public.mhai_reporting_period_ok('quarter', p_quarter_from, (p_quarter_from + interval '3 months')::date) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_analytics_frozen: not a quarter';
    END IF;
    -- Every output column is also a variable here: every column reference
    -- below is qualified.
    RETURN QUERY
    SELECT c.kind, c.period_from, f.period_to, c.hospital_id,
           c.q_pass, c.q_pass_degraded, c.q_regenerate, c.q_regenerate_degraded,
           c.q_escalate, c.q_escalate_degraded, c.q_unanswered,
           c.r_agreed, c.r_corrected, c.active_clinicians, c.reviews_decided, c.median_review_hours
    FROM public.analytics_period_counts c
    JOIN public.analytics_frozen_periods f ON f.kind = c.kind AND f.period_from = c.period_from
    WHERE c.period_from >= p_quarter_from AND c.period_from < (p_quarter_from + interval '3 months')::date
    ORDER BY c.kind, c.period_from, c.hospital_id;
END
$$;
-- +goose StatementEnd

-- mhai_analytics_onboarding(): per hospital, its name, status and creation
-- (the onboarding anchor of §6.1).
-- +goose StatementBegin
CREATE FUNCTION mhai_analytics_onboarding()
    RETURNS TABLE (hospital_id uuid, name text, status text, created_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT h.id, h.name, h.status, h.created_at FROM public.hospitals h ORDER BY h.id
$$;
-- +goose StatementEnd

-- mhai_analytics_staff(): per active hospital, its active accounts with a
-- hospital role (0 included).
-- +goose StatementBegin
CREATE FUNCTION mhai_analytics_staff()
    RETURNS TABLE (hospital_id uuid, staff bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT h.id, count(DISTINCT u.id) FILTER (WHERE u.id IS NOT NULL)
    FROM public.hospitals h
    LEFT JOIN public.users u ON u.hospital_id = h.id AND u.is_active
        AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id
                    AND r.role IN ('clinician', 'hospital_admin', 'analyst'))
    WHERE h.status = 'active'
    GROUP BY h.id ORDER BY h.id
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_analytics_freeze(text, date) OWNER TO mhai_analytics_reader;
ALTER FUNCTION mhai_analytics_frozen(date) OWNER TO mhai_analytics_reader;
ALTER FUNCTION mhai_analytics_onboarding() OWNER TO mhai_analytics_reader;
ALTER FUNCTION mhai_analytics_staff() OWNER TO mhai_analytics_reader;
REVOKE ALL ON FUNCTION mhai_analytics_freeze(text, date), mhai_analytics_frozen(date),
    mhai_analytics_onboarding(), mhai_analytics_staff() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analytics_freeze(text, date), mhai_analytics_frozen(date),
    mhai_analytics_onboarding(), mhai_analytics_staff() TO mhai_analytics_job;

-- §9: the rolling guardrail values are served fully hidden; the unguarded
-- counts function is no longer the platform's (A30).
REVOKE EXECUTE ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) FROM mhai_platform;
-- R41a-3: no old rolling figure is ever served beside a new period.
TRUNCATE guardrail_snapshots;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.analytics_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_analytics_reader FROM %I', current_user);
        PERFORM set_config('mhai.analytics_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- The rolling guardrail snapshots emptied by Up can't come back: they were a
-- cache of guarded figures, and 00017's route recomputed them on demand.
GRANT EXECUTE ON FUNCTION mhai_guardrail_counts(timestamptz, timestamptz) TO mhai_platform;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_analytics_reader', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_analytics_reader TO %I', current_user);
        PERFORM set_config('mhai.analytics_reader_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd
DROP FUNCTION mhai_analytics_staff();
DROP FUNCTION mhai_analytics_onboarding();
DROP FUNCTION mhai_analytics_frozen(date);
DROP FUNCTION mhai_analytics_freeze(text, date);

DROP INDEX feedback_events_decided_idx;
DROP INDEX feedback_events_created_idx;
DROP INDEX clinical_queries_created_idx;

DROP POLICY analytics_read ON user_roles;
DROP POLICY analytics_read ON users;
DROP POLICY analytics_read ON hospitals;
DROP POLICY analytics_read ON feedback_events;
DROP POLICY analytics_read ON clinical_decisions;
DROP POLICY analytics_read ON clinical_queries;
REVOKE ALL ON clinical_queries, clinical_decisions, feedback_events, hospitals, users, user_roles FROM mhai_analytics_reader;

-- The tables take their policies and grants with them.
DROP TABLE analytics_snapshots;
DROP TABLE analytics_period_counts;
DROP TABLE analytics_frozen_periods;
DROP FUNCTION mhai_reporting_eligible(date);
DROP TABLE analytics_epoch;
DROP FUNCTION mhai_reporting_period_ok(text, date, date);

REVOKE USAGE ON SCHEMA public FROM mhai_analytics_job, mhai_analytics_reader;
REVOKE mhai_analytics_job FROM mhai_app;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.analytics_reader_granted', true) = 'yes' THEN
        EXECUTE format('REVOKE mhai_analytics_reader FROM %I', current_user);
        PERFORM set_config('mhai.analytics_reader_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd
