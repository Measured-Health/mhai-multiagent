-- Patient Analysis, stored bodies (analysis backend design §5.4, §9-§11;
-- plan Task 9, "migration B"; ADR 0015). NUMBERED 00024 (coordinator):
-- 00023 is EMR PR 5's.
--
-- Analysis stays OFF: nothing here raises a switch. Every function below
-- refuses a hospital whose position isn't 'on', and takes the hospital
-- from mhai_current_hospital() (the request's, set by authz.Tx), never from
-- an argument.
--
-- What it adds:
--   - analysis_subject_attrs: a subject's attributes (age band, sex,
--     location and its setting, specialty, source), fixed by its first
--     fact and never changed. The disclosure engine's equations need one
--     value of each per patient, the same at every level (a context is the
--     sum of its sub-contexts; a month the sum of its blocks), so a
--     subject's attributes don't move between periods. Written by a trigger
--     on analysis_facts, as the inserting role (the writer).
--   - analysis_hospital_start: the quarter a hospital's history starts
--     (the first fact in a frozen block), fixed once: in that quarter's
--     first periods every case is new (Context.First), which the attacker
--     knows.
--   - analysis_snapshots: one guarded body per context (period and
--     filters), insert only, served as stored (design §11, I1). Each holds
--     the engine's record of the body (for the period's guard, ruling C10)
--     and the five views as sent; or the period's filter lists.
--   - analysis_cold_budget and analysis_cold_contexts: at most a cap of
--     distinct cold contexts per person and period (security review).
--   - analysis_view_log and analysis_unusual: analysis.viewed once per
--     session, filter set and period; analysis.unusual_querying once per
--     person and hospital day (D7).
--   - the request functions, EXECUTE to mhai_gateway only, owned by
--     mhai_analysis_reader: period, periods, lock, cells, filters,
--     snapshot_get, snapshot_put, records, viewed.
--   - mhai_analysis_sweep replaced (same signature, owner and grants): it
--     also sweeps bodies, the read log and the flags.
--
-- The lock (PR 3 review, condition 1): mhai_analysis_lock(period) takes a
-- transaction advisory lock on (hospital, period), so the guards of every
-- context of one period run one at a time, whatever the filters.
--
-- Plan departures: pairs are counted in cells rather than materialised at
-- freeze, and cells returns all five views at once (one call, no
-- REPEATABLE READ needed: a frozen period's facts never change). Both are
-- on the "Before scale" list in TODO.md.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- A subject's attributes, fixed by its first fact.
------------------------------------------------------------------------
CREATE TABLE analysis_subject_attrs (
    hospital_id uuid NOT NULL,
    subject_id  uuid NOT NULL PRIMARY KEY,
    age_band    text CHECK (age_band IN ('0-17', '18-39', '40-64', '65-79', '80+')),
    sex         text NOT NULL CHECK (sex IN ('female', 'male', 'other', 'not_recorded')),
    location_id uuid,
    setting     text CHECK (setting IN ('admitted', 'outpatient', 'emergency')),
    specialty   text CHECK (specialty ~ '^[a-z][a-z_]{1,39}$'),
    source_id   uuid,
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id) ON DELETE CASCADE
);

-- A location's setting is its own (the dictionary's, never changed); a
-- fact with no location keeps its setting.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_subject_attrs_put() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    INSERT INTO public.analysis_subject_attrs (hospital_id, subject_id, age_band, sex, location_id, setting, specialty, source_id)
    VALUES (NEW.hospital_id, NEW.subject_id, NEW.age_band, NEW.sex, NEW.location_id,
            CASE WHEN NEW.location_id IS NULL THEN NEW.setting
                 ELSE (SELECT l.setting FROM public.analysis_locations l
                       WHERE l.hospital_id = NEW.hospital_id AND l.id = NEW.location_id) END,
            NEW.specialty, NEW.source_id)
    ON CONFLICT (subject_id) DO NOTHING;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_analysis_subject_attrs_put() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_analysis_subject_attrs_put() FROM PUBLIC;
CREATE TRIGGER analysis_facts_subject_attrs AFTER INSERT ON analysis_facts
    FOR EACH ROW EXECUTE FUNCTION mhai_analysis_subject_attrs_put();

-- Subjects already stored (the demo's seed): their first fact.
INSERT INTO analysis_subject_attrs (hospital_id, subject_id, age_band, sex, location_id, setting, specialty, source_id)
SELECT DISTINCT ON (f.subject_id) f.hospital_id, f.subject_id, f.age_band, f.sex, f.location_id,
       CASE WHEN f.location_id IS NULL THEN f.setting ELSE l.setting END, f.specialty, f.source_id
FROM analysis_facts f
LEFT JOIN analysis_locations l ON l.hospital_id = f.hospital_id AND l.id = f.location_id
ORDER BY f.subject_id, f.id;

------------------------------------------------------------------------
-- Where a hospital's history starts, fixed once.
------------------------------------------------------------------------
CREATE TABLE analysis_hospital_start (
    hospital_id uuid PRIMARY KEY REFERENCES hospitals (id),
    start       date NOT NULL CHECK (start = date_trunc('quarter', start)::date)
);

------------------------------------------------------------------------
-- Stored bodies (design §4.3, §11).
------------------------------------------------------------------------
CREATE TABLE analysis_snapshots (
    hospital_id uuid NOT NULL,
    period_id   uuid NOT NULL,
    query_hash  bytea NOT NULL CHECK (octet_length(query_hash) = 32),
    seq         bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    body        jsonb NOT NULL CHECK (octet_length(body::text) <= 4194304),
    stored_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hospital_id, period_id, query_hash),
    FOREIGN KEY (hospital_id, period_id) REFERENCES analysis_periods (hospital_id, id)
);

------------------------------------------------------------------------
-- Reads, for analysis.viewed once per session, filter set and period, and
-- the day's distinct filter sets (D7).
------------------------------------------------------------------------
CREATE TABLE analysis_view_log (
    hospital_id  uuid NOT NULL REFERENCES hospitals (id),
    user_id      uuid NOT NULL,
    session_id   text NOT NULL CHECK (session_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    period_id    uuid NOT NULL,
    query_hash   bytea NOT NULL CHECK (octet_length(query_hash) = 32),
    filters_hash bytea NOT NULL CHECK (octet_length(filters_hash) = 32),
    day          date NOT NULL,
    at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hospital_id, session_id, period_id, query_hash)
);
CREATE INDEX analysis_view_log_day_idx ON analysis_view_log (hospital_id, user_id, day);
CREATE TABLE analysis_unusual (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    day         date NOT NULL,
    PRIMARY KEY (hospital_id, user_id, day)
);

------------------------------------------------------------------------
-- Cold contexts per person and period (security review): a person may
-- have at most a cap of distinct contexts computed cold in one period,
-- reserved before anything is computed (reserve, then check: the counter
-- row is locked FOR UPDATE).
------------------------------------------------------------------------
CREATE TABLE analysis_cold_budget (
    hospital_id uuid NOT NULL,
    user_id     uuid NOT NULL,
    period_id   uuid NOT NULL,
    used        integer NOT NULL DEFAULT 0 CHECK (used >= 0),
    PRIMARY KEY (hospital_id, user_id, period_id),
    FOREIGN KEY (hospital_id, period_id) REFERENCES analysis_periods (hospital_id, id)
);
CREATE TABLE analysis_cold_contexts (
    hospital_id uuid NOT NULL,
    user_id     uuid NOT NULL,
    period_id   uuid NOT NULL,
    query_hash  bytea NOT NULL CHECK (octet_length(query_hash) = 32),
    PRIMARY KEY (hospital_id, user_id, period_id, query_hash),
    FOREIGN KEY (hospital_id, user_id, period_id) REFERENCES analysis_cold_budget (hospital_id, user_id, period_id)
);

ALTER TABLE analysis_subject_attrs OWNER TO mhai_migrate;
ALTER TABLE analysis_hospital_start OWNER TO mhai_migrate;
ALTER TABLE analysis_snapshots OWNER TO mhai_migrate;
ALTER TABLE analysis_view_log OWNER TO mhai_migrate;
ALTER TABLE analysis_unusual OWNER TO mhai_migrate;
ALTER TABLE analysis_cold_budget OWNER TO mhai_migrate;
ALTER TABLE analysis_cold_contexts OWNER TO mhai_migrate;
ALTER TABLE analysis_subject_attrs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_hospital_start ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_snapshots ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_view_log ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_unusual ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_cold_budget ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_cold_contexts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analysis_subject_attrs, analysis_hospital_start, analysis_snapshots, analysis_view_log, analysis_unusual,
    analysis_cold_budget, analysis_cold_contexts FROM PUBLIC;

-- The writer: attributes through the trigger; the sweep's deletes.
GRANT SELECT, INSERT ON analysis_subject_attrs TO mhai_analysis_writer;
GRANT SELECT, DELETE ON analysis_snapshots, analysis_view_log, analysis_unusual, analysis_cold_budget, analysis_cold_contexts
    TO mhai_analysis_writer;
CREATE POLICY analysis_writer ON analysis_cold_budget TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_cold_contexts TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_subject_attrs TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_snapshots TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_view_log TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_unusual TO mhai_analysis_writer USING (true);

-- The reader, inside its functions only, and only the request's hospital
-- (row-level security is the second wall: a function that forgot its
-- hospital filter still sees one hospital). Insert only where it writes.
GRANT SELECT ON analysis_facts, analysis_subject_attrs, analysis_first_seen, analysis_periods,
    analysis_conditions, analysis_locations, analysis_sources TO mhai_analysis_reader;
GRANT SELECT, INSERT ON analysis_hospital_start, analysis_snapshots, analysis_view_log, analysis_unusual,
    analysis_cold_budget, analysis_cold_contexts TO mhai_analysis_reader;
GRANT UPDATE (used) ON analysis_cold_budget TO mhai_analysis_reader;
GRANT SELECT (id, time_zone) ON hospitals TO mhai_analysis_reader;
CREATE POLICY analysis_reader ON analysis_facts FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_subject_attrs FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_first_seen FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_periods FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_conditions FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_locations FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_sources FOR SELECT TO mhai_analysis_reader USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON hospitals FOR SELECT TO mhai_analysis_reader USING (id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_hospital_start TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_snapshots TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_view_log TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_unusual TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_cold_budget TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_cold_contexts TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());

------------------------------------------------------------------------
-- Internal helpers (no EXECUTE for anyone but their owner, the reader).
------------------------------------------------------------------------

-- mhai_analysis_hospital(): the request's hospital, refused (22023) when
-- there is none or its analysis is off.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_hospital() RETURNS uuid
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_current_hospital();
BEGIN
    IF v_h IS NULL OR public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: off for this hospital';
    END IF;
    RETURN v_h;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_frozen(period): the period's level and days, when it is a
-- frozen period of the request's hospital; refused (22023) otherwise.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_frozen(p_period uuid, OUT level text, OUT start date, OUT "end" date)
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    SELECT p.level, p.start, p."end" INTO level, start, "end"
    FROM public.analysis_periods p WHERE p.id = p_period AND p.hospital_id = public.mhai_analysis_hospital();
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a frozen period of this hospital';
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_small_shown(json): whether any {value, suppressed} in it
-- shows a whole number from 1 to 4. Shares are written with four decimals
-- and medians are minutes, not counts: they are skipped.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_small_shown(p jsonb) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    k text;
    v jsonb;
BEGIN
    IF jsonb_typeof(p) = 'object' THEN
        IF p ? 'value' AND p ? 'suppressed' AND jsonb_typeof(p -> 'value') = 'number'
           AND (p ->> 'value') !~ '[.eE]' AND (p ->> 'value')::numeric BETWEEN 1 AND 4 THEN
            RETURN true;
        END IF;
        FOR k, v IN SELECT e.key, e.value FROM jsonb_each(p) e LOOP
            IF k NOT IN ('median_ack_minutes', 'critical_median_ack_minutes', 'MedianAck', 'CriticalMedianAck')
               AND public.mhai_analysis_small_shown(v) THEN
                RETURN true;
            END IF;
        END LOOP;
    ELSIF jsonb_typeof(p) = 'array' THEN
        FOR v IN SELECT e.value FROM jsonb_array_elements(p) e LOOP
            IF public.mhai_analysis_small_shown(v) THEN
                RETURN true;
            END IF;
        END LOOP;
    END IF;
    RETURN false;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The request functions (EXECUTE to mhai_gateway only).
------------------------------------------------------------------------

-- mhai_analysis_period(level, start, now): the period of that level
-- starting that day: 'frozen' with its id; 'pending' when it has ended
-- (hospital days) but isn't frozen yet (503 not_ready); 'none' when it
-- isn't a period, hasn't ended, or starts more than 24 months before the
-- hospital day of now (422 invalid_range). now defaults to the database's
-- and may not be later (tests pin it; the routes pass none).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_period(p_level text, p_start date, p_now timestamptz)
    RETURNS TABLE (period_id uuid, state text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_analysis_hospital();
    v_now   timestamptz := COALESCE(p_now, now());
    v_today date;
    v_end   date;
    v_id    uuid;
BEGIN
    IF v_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_today FROM public.hospitals h WHERE h.id = v_h;
    v_end := CASE p_level
        WHEN 'week' THEN CASE WHEN extract(day FROM p_start) = 22
                              THEN (date_trunc('month', p_start) + interval '1 month')::date - 1 ELSE p_start + 6 END
        WHEN 'month' THEN (p_start + interval '1 month')::date - 1
        WHEN 'quarter' THEN (p_start + interval '3 months')::date - 1 END;
    IF v_end IS NULL OR NOT public.mhai_reporting_period_ok(p_level, p_start, v_end + 1)
       OR v_end >= v_today OR p_start < (v_today - interval '24 months')::date THEN
        RETURN QUERY SELECT NULL::uuid, 'none'::text;
        RETURN;
    END IF;
    SELECT p.id INTO v_id FROM public.analysis_periods p
    WHERE p.hospital_id = v_h AND p.level = p_level AND p.start = p_start;
    IF v_id IS NULL THEN
        RETURN QUERY SELECT NULL::uuid, 'pending'::text;
    ELSE
        RETURN QUERY SELECT v_id, 'frozen'::text;
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_periods(now): the answerable frozen periods, for the
-- picker: within 24 months of the hospital day of now.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_periods(p_now timestamptz)
    RETURNS TABLE (period_id uuid, level text, start date)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_analysis_hospital();
    v_now   timestamptz := COALESCE(p_now, now());
    v_today date;
BEGIN
    IF v_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_today FROM public.hospitals h WHERE h.id = v_h;
    RETURN QUERY SELECT p.id, p.level, p.start FROM public.analysis_periods p
    WHERE p.hospital_id = v_h AND p."end" < v_today AND p.start >= (v_today - interval '24 months')::date
    ORDER BY p.level, p.start;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_lock(period): one guard per hospital and period at a time
-- (ruling C10's period-wide guard; PR 3 review, condition 1), held to the
-- end of the transaction.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_lock(p_period uuid) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_analysis_frozen(p_period);
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.body'),
                                  hashtext(public.mhai_current_hospital()::text || '/' || p_period::text));
END
$$;
-- +goose StatementEnd

-- mhai_analysis_cells(period, filters): one context's raw values, every
-- view, as the disclosure engine's definitions count them (layout.go;
-- disclose's reference engine, rawOf): facts of the frozen period that
-- aren't late, of subjects whose fixed attributes meet the filters. A
-- block counts for a subject under a condition filter when the subject
-- had the condition in it; a case is a subject, condition and block; it is
-- new when the case's first day is in that block and wasn't late. The raw
-- values exist only in this result and in the engine's memory, for one
-- computation. Refuses (22023) an unfrozen or foreign period, an unknown
-- filter, more than two, or a value that isn't a code or an id.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_cells(p_period uuid, p_filters jsonb) RETURNS jsonb
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h      uuid := public.mhai_analysis_hospital();
    v_per    record;
    v_cond   uuid;
    v_age    text;
    v_sex    text;
    v_loc    uuid;
    v_set    text;
    v_spec   text;
    v_src    uuid;
    v_hstart date;
    v_out    jsonb;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    IF jsonb_typeof(p_filters) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_each(p_filters) e
                  WHERE e.key NOT IN ('condition', 'age_band', 'sex', 'location', 'setting', 'specialty', 'source')
                     OR jsonb_typeof(e.value) <> 'string')
       OR (SELECT count(*) FROM jsonb_object_keys(p_filters)) > 2
       OR (p_filters ->> 'condition') !~ '^[0-9a-f-]{36}$'
       OR (p_filters ->> 'location') !~ '^[0-9a-f-]{36}$'
       OR (p_filters ->> 'source') !~ '^[0-9a-f-]{36}$'
       OR (p_filters ->> 'age_band') NOT IN ('0-17', '18-39', '40-64', '65-79', '80+')
       OR (p_filters ->> 'sex') NOT IN ('female', 'male', 'other', 'not_recorded')
       OR (p_filters ->> 'setting') NOT IN ('admitted', 'outpatient', 'emergency')
       OR (p_filters ->> 'specialty') !~ '^[a-z][a-z_]{1,39}$' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a filter set';
    END IF;
    v_cond := (p_filters ->> 'condition')::uuid;
    v_age := p_filters ->> 'age_band';
    v_sex := p_filters ->> 'sex';
    v_loc := (p_filters ->> 'location')::uuid;
    v_set := p_filters ->> 'setting';
    v_spec := p_filters ->> 'specialty';
    v_src := (p_filters ->> 'source')::uuid;

    -- The history's start: fixed by the first fact in a frozen block (no
    -- fact can arrive before one that isn't late).
    INSERT INTO public.analysis_hospital_start (hospital_id, start)
    SELECT v_h, date_trunc('quarter', min(f.day))::date
    FROM public.analysis_facts f
    JOIN public.analysis_periods w ON w.hospital_id = f.hospital_id AND w.level = 'week'
                                  AND f.day BETWEEN w.start AND w."end"
    WHERE f.hospital_id = v_h
    HAVING min(f.day) IS NOT NULL
    ON CONFLICT (hospital_id) DO NOTHING;
    SELECT s.start INTO v_hstart FROM public.analysis_hospital_start s WHERE s.hospital_id = v_h;

    WITH f AS (
        SELECT x.subject_id AS s, x.kind, x.condition_id AS c, x.flag, x.severity, x.ack_minutes, x.dismiss_reason,
               x.confidence, x.rating, x.correction,
               make_date(extract(year FROM x.day)::int, extract(month FROM x.day)::int,
                         CASE WHEN extract(day FROM x.day) <= 7 THEN 1 WHEN extract(day FROM x.day) <= 14 THEN 8
                              WHEN extract(day FROM x.day) <= 21 THEN 15 ELSE 22 END) AS b
        FROM public.analysis_facts x
        JOIN public.analysis_subject_attrs a ON a.hospital_id = x.hospital_id AND a.subject_id = x.subject_id
        WHERE x.hospital_id = v_h AND x.day BETWEEN v_per.start AND v_per."end" AND NOT x.period_late
          AND (v_age IS NULL OR a.age_band = v_age) AND (v_sex IS NULL OR a.sex = v_sex)
          AND (v_loc IS NULL OR a.location_id = v_loc) AND (v_set IS NULL OR a.setting = v_set)
          AND (v_spec IS NULL OR a.specialty = v_spec) AND (v_src IS NULL OR a.source_id = v_src)
    ),
    u AS (SELECT DISTINCT f.s, f.b FROM f WHERE v_cond IS NULL OR f.c = v_cond),
    uf AS (SELECT f.* FROM f JOIN u ON u.s = f.s AND u.b = f.b),
    ds AS (SELECT DISTINCT u.s FROM u),
    da AS (SELECT a.* FROM ds JOIN public.analysis_subject_attrs a ON a.hospital_id = v_h AND a.subject_id = ds.s),
    dc AS (SELECT DISTINCT f.s, f.c FROM f JOIN ds ON ds.s = f.s WHERE f.c IS NOT NULL),
    cs AS (
        SELECT k.s, k.b, k.c,
               (fs.first_day BETWEEN k.b AND CASE WHEN extract(day FROM k.b) = 22
                    THEN (date_trunc('month', k.b) + interval '1 month')::date - 1 ELSE k.b + 6 END
                AND NOT fs.first_late) IS TRUE AS is_new
        FROM (SELECT DISTINCT uf.s, uf.b, uf.c FROM uf WHERE uf.c IS NOT NULL) k
        LEFT JOIN public.analysis_first_seen fs ON fs.hospital_id = v_h AND fs.subject_id = k.s AND fs.condition_id = k.c
    ),
    al AS (SELECT * FROM uf WHERE uf.kind = 'alert' AND uf.severity IS NOT NULL),
    qc AS (SELECT * FROM uf WHERE uf.kind = 'consultation' AND uf.c IS NOT NULL AND uf.confidence IS NOT NULL)
    SELECT jsonb_build_object(
        'first', v_hstart IS NOT NULL AND v_hstart = v_per.start,
        'patients', (SELECT count(*) FROM ds),
        'cases', (SELECT count(*) FROM cs),
        'new', (SELECT count(*) FROM cs WHERE cs.is_new),
        'by_condition', COALESCE((SELECT jsonb_agg(jsonb_build_object('c', x.c, 'cases', x.n, 'new', x.nw) ORDER BY x.c)
            FROM (SELECT cs.c::text AS c, count(*) AS n, count(*) FILTER (WHERE cs.is_new) AS nw FROM cs GROUP BY cs.c) x), '[]'),
        'blocks', COALESCE((SELECT jsonb_agg(jsonb_build_object('start', x.b, 'cases', x.n, 'new', x.nw) ORDER BY x.b)
            FROM (SELECT cs.b, count(*) AS n, count(*) FILTER (WHERE cs.is_new) AS nw FROM cs GROUP BY cs.b) x), '[]'),
        'by_age', COALESCE((SELECT jsonb_agg(jsonb_build_object('k', x.k, 'n', x.n) ORDER BY x.k)
            FROM (SELECT COALESCE(da.age_band, '') AS k, count(*) AS n FROM da GROUP BY 1) x), '[]'),
        'by_sex', COALESCE((SELECT jsonb_agg(jsonb_build_object('k', x.k, 'n', x.n) ORDER BY x.k)
            FROM (SELECT da.sex AS k, count(*) AS n FROM da GROUP BY 1) x), '[]'),
        'by_setting', COALESCE((SELECT jsonb_agg(jsonb_build_object('k', x.k, 'n', x.n) ORDER BY x.k)
            FROM (SELECT COALESCE(da.setting, '') AS k, count(*) AS n FROM da GROUP BY 1) x), '[]'),
        'by_location', COALESCE((SELECT jsonb_agg(jsonb_build_object('k', x.k, 'setting', x.st, 'n', x.n) ORDER BY x.k)
            FROM (SELECT COALESCE(da.location_id::text, '') AS k,
                         CASE WHEN da.location_id IS NULL THEN '' ELSE COALESCE(min(da.setting), '') END AS st,
                         count(*) AS n FROM da GROUP BY da.location_id) x), '[]'),
        'by_specialty', COALESCE((SELECT jsonb_agg(jsonb_build_object('k', x.k, 'n', x.n) ORDER BY x.k)
            FROM (SELECT COALESCE(da.specialty, '') AS k, count(*) AS n FROM da GROUP BY 1) x), '[]'),
        'sources', COALESCE((SELECT jsonb_agg(x.k ORDER BY x.k)
            FROM (SELECT DISTINCT COALESCE(da.source_id::text, '') AS k FROM da) x), '[]'),
        'pairs', COALESCE((SELECT jsonb_agg(jsonb_build_object('a', x.a, 'b', x.b, 'a_patients', x.na, 'patients', x.nb) ORDER BY x.a, x.b)
            FROM (SELECT p.c::text AS a, q.c::text AS b,
                         (SELECT count(*) FROM dc z WHERE z.c = p.c) AS na, count(*) AS nb
                  FROM dc p JOIN dc q ON q.s = p.s AND q.c::text > p.c::text
                  GROUP BY p.c, q.c) x), '[]'),
        'findings', COALESCE((SELECT jsonb_agg(jsonb_build_object('name', x.c, 'flag', x.flag, 'patients', x.np, 'results', x.nr) ORDER BY x.c, x.flag)
            FROM (SELECT COALESCE(fd.c::text, '') AS c, COALESCE(fd.flag, '') AS flag, count(DISTINCT fd.s) AS np,
                         (SELECT count(*) FROM uf r WHERE r.kind = 'finding' AND r.c IS NOT DISTINCT FROM fd.c
                                                    AND r.flag IS NOT DISTINCT FROM fd.flag) AS nr
                  FROM f fd JOIN ds ON ds.s = fd.s WHERE fd.kind = 'finding' GROUP BY fd.c, fd.flag) x), '[]'),
        'alerts', jsonb_build_object(
            'total', (SELECT count(*) FROM al),
            'critical', (SELECT count(*) FROM al WHERE al.severity = 'critical'),
            'urgent', (SELECT count(*) FROM al WHERE al.severity = 'urgent'),
            'info', (SELECT count(*) FROM al WHERE al.severity = 'info'),
            'dismissed', (SELECT count(*) FROM al WHERE al.dismiss_reason IS NOT NULL),
            'reasons', COALESCE((SELECT jsonb_agg(jsonb_build_object('reason', x.r, 'count', x.n) ORDER BY x.r)
                FROM (SELECT al.dismiss_reason AS r, count(*) AS n FROM al WHERE al.dismiss_reason IS NOT NULL GROUP BY 1) x), '[]'),
            'ack', COALESCE((SELECT jsonb_agg(al.ack_minutes ORDER BY al.ack_minutes) FROM al WHERE al.ack_minutes IS NOT NULL), '[]'),
            'critical_ack', COALESCE((SELECT jsonb_agg(al.ack_minutes ORDER BY al.ack_minutes) FROM al
                                      WHERE al.ack_minutes IS NOT NULL AND al.severity = 'critical'), '[]')),
        'quality', COALESCE((SELECT jsonb_agg(jsonb_build_object('c', x.c, 'n', x.n, 'high', x.hi, 'caution', x.ca, 'review', x.re,
                'failed', x.fa, 'rated', x.ra, 'agreed', x.ag, 'corrections', x.co, 'approved', x.ap, 'rejected', x.rj, 'waiting', x.wa) ORDER BY x.c)
            FROM (SELECT qc.c::text AS c, count(*) AS n,
                         count(*) FILTER (WHERE qc.confidence = 'high') AS hi, count(*) FILTER (WHERE qc.confidence = 'caution') AS ca,
                         count(*) FILTER (WHERE qc.confidence = 'review') AS re, count(*) FILTER (WHERE qc.confidence = 'failed') AS fa,
                         count(*) FILTER (WHERE qc.rating IS NOT NULL) AS ra, count(*) FILTER (WHERE qc.rating = 'agreed') AS ag,
                         count(*) FILTER (WHERE qc.correction IS NOT NULL) AS co, count(*) FILTER (WHERE qc.correction = 'approved') AS ap,
                         count(*) FILTER (WHERE qc.correction = 'rejected') AS rj, count(*) FILTER (WHERE qc.correction = 'waiting') AS wa
                  FROM qc GROUP BY qc.c) x), '[]')
    ) INTO v_out;
    RETURN v_out;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_filters(period): the period's filter lists (design §9,
-- M4): conditions with 5 or more patients in it, plus every "Not
-- standardised" entry; specialties with 5 or more; the hospital's
-- locations and sources. Stored once per period, so a frozen period's
-- list never changes (P6).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_filters(p_period uuid) RETURNS jsonb
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_per record;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    RETURN jsonb_build_object(
        'conditions', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', c.id, 'name', c.name, 'icd10', c.icd10)
                                                ORDER BY c.icd10 NULLS LAST, c.id)
            FROM public.analysis_conditions c
            WHERE c.hospital_id = v_h
              AND (c.icd10 IS NULL OR (SELECT count(DISTINCT f.subject_id) FROM public.analysis_facts f
                                       WHERE f.hospital_id = v_h AND f.condition_id = c.id AND NOT f.period_late
                                         AND f.day BETWEEN v_per.start AND v_per."end") >= 5)), '[]'),
        'locations', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', l.id, 'label', l.label, 'setting', l.setting) ORDER BY l.label, l.id)
            FROM public.analysis_locations l WHERE l.hospital_id = v_h), '[]'),
        'specialties', COALESCE((SELECT jsonb_agg(x.specialty ORDER BY x.specialty)
            FROM (SELECT a.specialty FROM public.analysis_subject_attrs a
                  WHERE a.hospital_id = v_h AND a.specialty IS NOT NULL
                    AND EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = v_h AND f.subject_id = a.subject_id
                                AND NOT f.period_late AND f.day BETWEEN v_per.start AND v_per."end")
                  GROUP BY a.specialty HAVING count(*) >= 5) x), '[]'),
        'sources', COALESCE((SELECT jsonb_agg(jsonb_build_object('id', s.id, 'label', s.label) ORDER BY s.label, s.id)
            FROM public.analysis_sources s WHERE s.hospital_id = v_h), '[]'));
END
$$;
-- +goose StatementEnd

-- mhai_analysis_snapshot_get(period, hash): a stored body and when.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_snapshot_get(p_period uuid, p_hash bytea)
    RETURNS TABLE (seq bigint, body jsonb, stored_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_analysis_frozen(p_period);
    RETURN QUERY SELECT s.seq, s.body, s.stored_at FROM public.analysis_snapshots s
    WHERE s.hospital_id = public.mhai_current_hospital() AND s.period_id = p_period AND s.query_hash = p_hash;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_snapshot_put(period, hash, body): stores a body once
-- (never overwritten: null when one is stored already). Refuses (22023)
-- an unfrozen or foreign period, a body over 4 MiB, or one showing a count
-- of 1 to 4 anywhere (N7); and (54000) the 20,001st body of the period
-- (design §11: 503, never served unstored). Run under the period's lock.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_snapshot_put(p_period uuid, p_hash bytea, p_body jsonb) RETURNS bigint
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_seq bigint;
BEGIN
    PERFORM public.mhai_analysis_frozen(p_period);
    IF p_hash IS NULL OR octet_length(p_hash) <> 32 OR jsonb_typeof(p_body) IS DISTINCT FROM 'object'
       OR octet_length(p_body::text) > 4194304 OR public.mhai_analysis_small_shown(p_body) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a body that may be stored';
    END IF;
    IF EXISTS (SELECT 1 FROM public.analysis_snapshots s
               WHERE s.hospital_id = v_h AND s.period_id = p_period AND s.query_hash = p_hash) THEN
        RETURN NULL;
    END IF;
    IF (SELECT count(*) FROM public.analysis_snapshots s WHERE s.hospital_id = v_h AND s.period_id = p_period) >= 20000 THEN
        RAISE EXCEPTION USING ERRCODE = 'program_limit_exceeded', MESSAGE = 'analysis: the period holds its most bodies';
    END IF;
    INSERT INTO public.analysis_snapshots (hospital_id, period_id, query_hash, body)
    VALUES (v_h, p_period, p_hash, p_body)
    ON CONFLICT (hospital_id, period_id, query_hash) DO NOTHING
    RETURNING analysis_snapshots.seq INTO v_seq;
    RETURN v_seq;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_records(period, have): the engine's record of every body
-- stored for the period and inside it (its blocks and months), except
-- those in have: what the period's guard knows (ruling C10).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_records(p_period uuid, p_have bigint[])
    RETURNS TABLE (seq bigint, record jsonb)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_per record;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    RETURN QUERY SELECT s.seq, s.body -> 'record' FROM public.analysis_snapshots s
    JOIN public.analysis_periods p ON p.hospital_id = s.hospital_id AND p.id = s.period_id
    WHERE s.hospital_id = public.mhai_current_hospital() AND p.start >= v_per.start AND p."end" <= v_per."end"
      AND s.body ? 'record' AND NOT (s.seq = ANY (COALESCE(p_have, '{}')))
    ORDER BY s.seq;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_reserve_cold(user, period, hash, cap): reserves one cold
-- context of the period for the person before anything is computed
-- (security review): true when the context is stored already, reserved
-- before by them, or newly reserved within cap; false past the cap. The
-- person's counter row is locked FOR UPDATE, so parallel requests queue
-- on it (reserve, then check). The user is the verified token's, passed
-- by core.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_reserve_cold(p_user uuid, p_period uuid, p_hash bytea, p_cap integer) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h    uuid := public.mhai_analysis_hospital();
    v_used integer;
BEGIN
    PERFORM public.mhai_analysis_frozen(p_period);
    IF p_user IS NULL OR p_hash IS NULL OR octet_length(p_hash) <> 32 OR p_cap IS NULL OR p_cap < 1 OR p_cap > 10000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a reservation';
    END IF;
    IF EXISTS (SELECT 1 FROM public.analysis_snapshots s
               WHERE s.hospital_id = v_h AND s.period_id = p_period AND s.query_hash = p_hash) THEN
        RETURN true;
    END IF;
    INSERT INTO public.analysis_cold_budget (hospital_id, user_id, period_id) VALUES (v_h, p_user, p_period)
        ON CONFLICT (hospital_id, user_id, period_id) DO NOTHING;
    SELECT b.used INTO v_used FROM public.analysis_cold_budget b
    WHERE b.hospital_id = v_h AND b.user_id = p_user AND b.period_id = p_period FOR UPDATE;
    IF EXISTS (SELECT 1 FROM public.analysis_cold_contexts c WHERE c.hospital_id = v_h AND c.user_id = p_user
               AND c.period_id = p_period AND c.query_hash = p_hash) THEN
        RETURN true;
    END IF;
    IF v_used >= p_cap THEN
        RETURN false;
    END IF;
    INSERT INTO public.analysis_cold_contexts (hospital_id, user_id, period_id, query_hash) VALUES (v_h, p_user, p_period, p_hash);
    UPDATE public.analysis_cold_budget b SET used = b.used + 1
    WHERE b.hospital_id = v_h AND b.user_id = p_user AND b.period_id = p_period;
    RETURN true;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_viewed(user, session, period, query, filters, now): logs a
-- read; first is whether it is the first of this session, filter set and
-- period (analysis.viewed is written then), and unusual whether it is the
-- read that takes the person past 100 distinct filter sets in the hospital
-- day (analysis.unusual_querying, once a day). The user and session are
-- the verified token's, passed by core.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_viewed(p_user uuid, p_session text, p_period uuid, p_query bytea, p_filters bytea,
                                     p_now timestamptz)
    RETURNS TABLE (first boolean, unusual boolean)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_analysis_hospital();
    v_now   timestamptz := COALESCE(p_now, now());
    v_day   date;
    v_first boolean;
    v_n     integer;
    v_flag  boolean := false;
BEGIN
    IF p_user IS NULL OR v_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: no reader';
    END IF;
    PERFORM public.mhai_analysis_frozen(p_period);
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_view_log (hospital_id, user_id, session_id, period_id, query_hash, filters_hash, day, at)
    VALUES (v_h, p_user, p_session, p_period, p_query, p_filters, v_day, v_now)
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    v_first := v_n = 1;
    IF v_first THEN
        SELECT count(DISTINCT l.filters_hash) INTO v_n FROM public.analysis_view_log l
        WHERE l.hospital_id = v_h AND l.user_id = p_user AND l.day = v_day;
        IF v_n > 100 THEN
            INSERT INTO public.analysis_unusual (hospital_id, user_id, day) VALUES (v_h, p_user, v_day)
            ON CONFLICT DO NOTHING;
            GET DIAGNOSTICS v_n = ROW_COUNT;
            v_flag := v_n = 1;
        END IF;
    END IF;
    RETURN QUERY SELECT v_first, v_flag;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_sweep, replaced: 00021's retention, plus stored bodies of
-- periods that ended 25 months ago, and the read log and flags after two
-- hospital days.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_cut date;
    v_n   integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    v_cut := ((p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date - interval '25 months')::date;
    DELETE FROM public.analysis_facts f WHERE f.hospital_id = p_hospital AND f.day < v_cut;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    -- 00024: stored bodies live as long as their period's facts (25
    -- months); the read log and the day's flags for two hospital days.
    DELETE FROM public.analysis_cold_contexts c
    WHERE c.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = c.hospital_id AND p.id = c.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_cold_budget b
    WHERE b.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = b.hospital_id AND p.id = b.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_snapshots s
    WHERE s.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = s.hospital_id AND p.id = s.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_view_log l WHERE l.hospital_id = p_hospital AND l.day < (v_cut + interval '25 months')::date - 2;
    DELETE FROM public.analysis_unusual u WHERE u.hospital_id = p_hospital AND u.day < (v_cut + interval '25 months')::date - 2;
    DELETE FROM public.analysis_first_seen fs WHERE fs.hospital_id = p_hospital AND fs.first_day < v_cut;
    DELETE FROM public.analysis_subjects s
    WHERE s.hospital_id = p_hospital AND s.created_day < v_cut
      AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = s.hospital_id AND f.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_first_seen fs WHERE fs.hospital_id = s.hospital_id AND fs.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_pseudonyms p WHERE p.hospital_id = s.hospital_id AND p.subject_id = s.subject_id);
    RETURN v_n;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_analysis_hospital() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_frozen(uuid) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_small_shown(jsonb) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_period(text, date, timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_periods(timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_lock(uuid) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_cells(uuid, jsonb) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_filters(uuid) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_snapshot_get(uuid, bytea) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_snapshot_put(uuid, bytea, jsonb) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_records(uuid, bigint[]) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_viewed(uuid, text, uuid, bytea, bytea, timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_reserve_cold(uuid, uuid, bytea, integer) OWNER TO mhai_analysis_reader;
REVOKE ALL ON FUNCTION mhai_analysis_hospital(), mhai_analysis_frozen(uuid), mhai_analysis_small_shown(jsonb),
    mhai_analysis_period(text, date, timestamptz), mhai_analysis_periods(timestamptz), mhai_analysis_lock(uuid),
    mhai_analysis_cells(uuid, jsonb), mhai_analysis_filters(uuid), mhai_analysis_snapshot_get(uuid, bytea),
    mhai_analysis_snapshot_put(uuid, bytea, jsonb), mhai_analysis_records(uuid, bigint[]),
    mhai_analysis_viewed(uuid, text, uuid, bytea, bytea, timestamptz), mhai_analysis_reserve_cold(uuid, uuid, bytea, integer) FROM PUBLIC;
-- The definer functions reach the switch and the period check as the reader.
GRANT EXECUTE ON FUNCTION mhai_reporting_period_ok(text, date, date) TO mhai_analysis_reader;
GRANT EXECUTE ON FUNCTION mhai_analysis_period(text, date, timestamptz), mhai_analysis_periods(timestamptz),
    mhai_analysis_lock(uuid), mhai_analysis_cells(uuid, jsonb), mhai_analysis_filters(uuid),
    mhai_analysis_snapshot_get(uuid, bytea), mhai_analysis_snapshot_put(uuid, bytea, jsonb),
    mhai_analysis_records(uuid, bigint[]), mhai_analysis_viewed(uuid, text, uuid, bytea, bytea, timestamptz),
    mhai_analysis_reserve_cold(uuid, uuid, bytea, integer) TO mhai_gateway;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- 00021's sweep, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_cut date;
    v_n   integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    v_cut := ((p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date - interval '25 months')::date;
    DELETE FROM public.analysis_facts f WHERE f.hospital_id = p_hospital AND f.day < v_cut;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    DELETE FROM public.analysis_first_seen fs WHERE fs.hospital_id = p_hospital AND fs.first_day < v_cut;
    DELETE FROM public.analysis_subjects s
    WHERE s.hospital_id = p_hospital AND s.created_day < v_cut
      AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = s.hospital_id AND f.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_first_seen fs WHERE fs.hospital_id = s.hospital_id AND fs.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_pseudonyms p WHERE p.hospital_id = s.hospital_id AND p.subject_id = s.subject_id);
    RETURN v_n;
END
$$;
-- +goose StatementEnd

DROP FUNCTION mhai_analysis_reserve_cold(uuid, uuid, bytea, integer);
DROP FUNCTION mhai_analysis_viewed(uuid, text, uuid, bytea, bytea, timestamptz);
DROP FUNCTION mhai_analysis_records(uuid, bigint[]);
DROP FUNCTION mhai_analysis_snapshot_put(uuid, bytea, jsonb);
DROP FUNCTION mhai_analysis_snapshot_get(uuid, bytea);
DROP FUNCTION mhai_analysis_filters(uuid);
DROP FUNCTION mhai_analysis_cells(uuid, jsonb);
DROP FUNCTION mhai_analysis_lock(uuid);
DROP FUNCTION mhai_analysis_periods(timestamptz);
DROP FUNCTION mhai_analysis_period(text, date, timestamptz);
DROP FUNCTION mhai_analysis_small_shown(jsonb);
DROP FUNCTION mhai_analysis_frozen(uuid);
DROP FUNCTION mhai_analysis_hospital();
REVOKE EXECUTE ON FUNCTION mhai_reporting_period_ok(text, date, date) FROM mhai_analysis_reader;

DROP POLICY analysis_reader ON hospitals;
REVOKE ALL ON hospitals FROM mhai_analysis_reader;
DROP POLICY analysis_reader ON analysis_facts;
DROP POLICY analysis_reader ON analysis_first_seen;
DROP POLICY analysis_reader ON analysis_periods;
DROP POLICY analysis_reader ON analysis_conditions;
DROP POLICY analysis_reader ON analysis_locations;
DROP POLICY analysis_reader ON analysis_sources;
REVOKE ALL ON analysis_facts, analysis_first_seen, analysis_periods, analysis_conditions, analysis_locations,
    analysis_sources FROM mhai_analysis_reader;

DROP TABLE analysis_cold_contexts;
DROP TABLE analysis_cold_budget;
DROP TABLE analysis_unusual;
DROP TABLE analysis_view_log;
DROP TABLE analysis_snapshots;
DROP TABLE analysis_hospital_start;
DROP TRIGGER analysis_facts_subject_attrs ON analysis_facts;
DROP FUNCTION mhai_analysis_subject_attrs_put();
DROP TABLE analysis_subject_attrs;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
