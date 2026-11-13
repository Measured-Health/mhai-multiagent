-- Patient Analysis, the de-identified drill-down and its reveal (analysis
-- backend design §4.5, §5.6, §8.8; plan Tasks 12-14, "migration C"; ADR
-- 0015). NUMBERED 00028 (coordinator): 00026 and 00027 are EMR PRs 6 and 7.
--
-- Analysis stays OFF: nothing here raises a switch. Every request function
-- refuses a hospital whose position isn't 'on' and takes the hospital from
-- mhai_current_hospital() and the person from mhai_current_user() (the
-- verified token's, set by db.WithHospitalUser), never from an argument.
--
-- What it adds:
--   - analysis_views and analysis_view_rows: one row per de-identified
--     list (who, which period, filters and group, the count, the rows
--     sent in order). Written only by mhai_analysis_open, as the viewer
--     (WITH CHECK viewer = mhai_current_user()); no role may update or
--     delete one. A deferred constraint trigger refuses, at commit, a list
--     with no analysis.patients_viewed entry naming it: no list is kept,
--     or sent, unaudited (T18).
--   - analysis_patient_budget and analysis_patient_opens: the distinct
--     patients one person opens per calendar quarter (design §5.6),
--     reserved then checked under FOR UPDATE on the person's row (T28).
--   - analysis_drill_flags: one flagged audit entry per person, kind and
--     hospital day when a list limit or the patient cap refuses.
--   - pids (design §4.5): drawn at random per hospital, period and subject
--     on the hospital side, redrawn on a collision (N9).
--   - mhai_analysis_auditor (NOLOGIN, NOINHERIT, no members): owns the one
--     platform function, mhai_audit_view_pseudonyms(audit id), the reveal
--     (design §8.8). Its only reads: the drill-down audit entries, the
--     views, their rows and the pids.
--   - platform_append: 00022's list plus audit.patients_viewed.
--   - mhai_analysis_sweep replaced (same signature, owner and grants):
--     opens and budgets once their quarter can't be listed any more, flags
--     after two hospital days; a subject a view row or an open cites is
--     kept.
--
-- Drill groups (plan departure 19): a list is opened only on a group the
-- rows describe, in a context whose filters the rows describe (age band,
-- sex, setting, condition). A location, specialty or source filter, or a
-- by_location or by_specialty row, has no drill: pids are stable within a
-- period, so two lists split by a dimension the rows don't show would tell
-- which patients are in a group the aggregate rule hides.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analysis_auditor') THEN
        CREATE ROLE mhai_analysis_auditor NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    ALTER ROLE mhai_analysis_auditor NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    -- Nobody may already act as it: its grants open the pids.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = 'mhai_analysis_auditor') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00028: mhai_analysis_auditor already has members; remove them first';
    END IF;
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader', 'mhai_analysis_auditor'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_analysis_auditor;

------------------------------------------------------------------------
-- Lists (design §4.3).
------------------------------------------------------------------------
CREATE TABLE analysis_views (
    id           uuid PRIMARY KEY,
    hospital_id  uuid NOT NULL REFERENCES hospitals (id),
    viewer       uuid NOT NULL,
    level        text NOT NULL CHECK (level = 'deidentified'),
    period_id    uuid NOT NULL,
    source_view  text NOT NULL CHECK (source_view IN ('affected', 'co-occurring', 'findings')),
    filters      jsonb NOT NULL CHECK (jsonb_typeof(filters) = 'object'),
    drill_hash   bytea NOT NULL CHECK (octet_length(drill_hash) = 32),
    matched      integer NOT NULL CHECK (matched >= 5),
    sent         integer NOT NULL CHECK (sent BETWEEN 1 AND 200 AND sent <= matched),
    -- Named access (PR 8) fills these; a de-identified list has none.
    purpose      text CHECK (purpose IS NULL),
    clearance_id uuid CHECK (clearance_id IS NULL),
    named_until  timestamptz CHECK (named_until IS NULL),
    day          date NOT NULL,
    at           timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, period_id) REFERENCES analysis_periods (hospital_id, id)
);
CREATE INDEX analysis_views_viewer_idx ON analysis_views (hospital_id, viewer, at);

CREATE TABLE analysis_view_rows (
    hospital_id uuid NOT NULL,
    view_id     uuid NOT NULL,
    ord         integer NOT NULL CHECK (ord BETWEEN 1 AND 200),
    subject_id  uuid NOT NULL,
    PRIMARY KEY (view_id, ord),
    UNIQUE (view_id, subject_id),
    FOREIGN KEY (hospital_id, view_id) REFERENCES analysis_views (hospital_id, id),
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id)
);
CREATE INDEX analysis_view_rows_subject_idx ON analysis_view_rows (hospital_id, subject_id);

------------------------------------------------------------------------
-- The distinct-patient cap, per person and calendar quarter (design §5.6).
------------------------------------------------------------------------
CREATE TABLE analysis_patient_budget (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    quarter     date NOT NULL CHECK (quarter = date_trunc('quarter', quarter)::date),
    opened      integer NOT NULL DEFAULT 0 CHECK (opened BETWEEN 0 AND 300),
    PRIMARY KEY (hospital_id, user_id, quarter)
);
CREATE TABLE analysis_patient_opens (
    hospital_id uuid NOT NULL,
    user_id     uuid NOT NULL,
    quarter     date NOT NULL,
    subject_id  uuid NOT NULL,
    PRIMARY KEY (hospital_id, user_id, quarter, subject_id),
    FOREIGN KEY (hospital_id, user_id, quarter) REFERENCES analysis_patient_budget (hospital_id, user_id, quarter),
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id)
);
CREATE INDEX analysis_patient_opens_subject_idx ON analysis_patient_opens (hospital_id, subject_id);

------------------------------------------------------------------------
-- One flagged entry per person, kind and hospital day.
------------------------------------------------------------------------
CREATE TABLE analysis_drill_flags (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('list_minute', 'list_day', 'patient_cap')),
    day         date NOT NULL,
    PRIMARY KEY (hospital_id, user_id, kind, day)
);

ALTER TABLE analysis_views OWNER TO mhai_migrate;
ALTER TABLE analysis_view_rows OWNER TO mhai_migrate;
ALTER TABLE analysis_patient_budget OWNER TO mhai_migrate;
ALTER TABLE analysis_patient_opens OWNER TO mhai_migrate;
ALTER TABLE analysis_drill_flags OWNER TO mhai_migrate;
ALTER TABLE analysis_views ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_view_rows ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_patient_budget ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_patient_opens ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_drill_flags ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analysis_views, analysis_view_rows, analysis_patient_budget, analysis_patient_opens, analysis_drill_flags FROM PUBLIC;

-- The audit entry a list names, found at once (the commit check and the
-- reveal).
CREATE INDEX audit_log_analysis_view_idx ON audit_log (entity_id) WHERE entity = 'analysis_view';

-- The reader, inside its functions only, for the request's hospital; a
-- list only as its viewer. Insert only: nothing is updated or deleted,
-- except the person's opened count.
GRANT SELECT, INSERT ON analysis_views, analysis_view_rows, analysis_patient_budget, analysis_patient_opens,
    analysis_drill_flags, analysis_pseudonyms TO mhai_analysis_reader;
GRANT UPDATE (opened) ON analysis_patient_budget TO mhai_analysis_reader;
CREATE POLICY analysis_reader ON analysis_views TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND viewer = public.mhai_current_user());
CREATE POLICY analysis_reader ON analysis_view_rows TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON analysis_patient_budget TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user());
CREATE POLICY analysis_reader ON analysis_patient_opens TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user());
CREATE POLICY analysis_reader ON analysis_drill_flags TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user());
CREATE POLICY analysis_reader ON analysis_pseudonyms TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND side = 'hospital')
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND side = 'hospital' AND scope IS NULL);
-- The commit check reads the request's own hospital's drill-down entries.
GRANT SELECT (id, hospital_id, actor_id, action, entity, entity_id) ON audit_log TO mhai_analysis_reader;
CREATE POLICY analysis_reader ON audit_log FOR SELECT TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND action = 'analysis.patients_viewed');

-- The writer: the sweep's reads and deletes.
GRANT SELECT, DELETE ON analysis_patient_budget, analysis_patient_opens, analysis_drill_flags TO mhai_analysis_writer;
GRANT SELECT ON analysis_view_rows TO mhai_analysis_writer;
CREATE POLICY analysis_writer ON analysis_patient_budget TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_patient_opens TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_drill_flags TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_view_rows FOR SELECT TO mhai_analysis_writer USING (true);

-- The auditor: what the reveal reads, and nothing else.
GRANT SELECT (id, hospital_id, action, entity, entity_id) ON audit_log TO mhai_analysis_auditor;
GRANT SELECT (id, hospital_id, period_id) ON analysis_views TO mhai_analysis_auditor;
GRANT SELECT ON analysis_view_rows TO mhai_analysis_auditor;
GRANT SELECT (hospital_id, side, scope, period_id, subject_id, pid) ON analysis_pseudonyms TO mhai_analysis_auditor;
CREATE POLICY analysis_auditor ON audit_log FOR SELECT TO mhai_analysis_auditor
    USING (action = 'analysis.patients_viewed' AND entity = 'analysis_view');
CREATE POLICY analysis_auditor ON analysis_views FOR SELECT TO mhai_analysis_auditor USING (true);
CREATE POLICY analysis_auditor ON analysis_view_rows FOR SELECT TO mhai_analysis_auditor USING (true);
CREATE POLICY analysis_auditor ON analysis_pseudonyms FOR SELECT TO mhai_analysis_auditor USING (side = 'hospital');

------------------------------------------------------------------------
-- Internal helpers (no EXECUTE for anyone but their owner).
------------------------------------------------------------------------

-- mhai_analysis_pid_draw(): 'P-' and 8 characters of RFC 4648 base32
-- (A-Z, 2-7) from 40 random bits of gen_random_uuid() (the server's strong
-- random source; a version 4 uuid's first five bytes are all random).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_pid_draw() RETURNS text
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    b   bytea := uuid_send(gen_random_uuid());
    v   bigint;
    out text := 'P-';
BEGIN
    v := (get_byte(b, 0)::bigint << 32) | (get_byte(b, 1)::bigint << 24) | (get_byte(b, 2)::bigint << 16)
         | (get_byte(b, 3)::bigint << 8) | get_byte(b, 4)::bigint;
    FOR i IN REVERSE 7..0 LOOP
        out := out || substr('ABCDEFGHIJKLMNOPQRSTUVWXYZ234567', ((v >> (5 * i)) & 31)::integer + 1, 1);
    END LOOP;
    RETURN out;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_group(period, filters, group): every subject of one
-- drillable group of a frozen period of the request's hospital, by the
-- definitions mhai_analysis_cells counts with (00025): the patients of the
-- context (facts of the period that aren't late, of subjects whose fixed
-- attributes meet the filters, in a block with the condition under a
-- condition filter), then
--   {"kind": "part", "dim": "age_band" | "sex" | "setting", "key": k}
--       (k "" the null row);
--   {"kind": "pair", "a": id, "b": id} (a < b): with both conditions in
--       the period;
--   {"kind": "finding", "condition": id | "", "flag": f | ""}: with that
--       finding in the period.
-- Only the dimensions a row shows may filter (departure 19). Refuses
-- (22023) anything else.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_group(p_period uuid, p_filters jsonb, p_group jsonb) RETURNS SETOF uuid
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h    uuid := public.mhai_analysis_hospital();
    v_per  record;
    v_cond uuid;
    v_age  text;
    v_sex  text;
    v_set  text;
    g_kind text := p_group ->> 'kind';
    g_dim  text;
    g_key  text;
    g_a    uuid;
    g_b    uuid;
    g_c    uuid;
    g_flag text;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    IF jsonb_typeof(p_filters) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_each(p_filters) e
                  WHERE e.key NOT IN ('condition', 'age_band', 'sex', 'setting') OR jsonb_typeof(e.value) <> 'string')
       OR (SELECT count(*) FROM jsonb_object_keys(p_filters)) > 2
       OR (p_filters ->> 'condition') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR (p_filters ->> 'age_band') NOT IN ('0-17', '18-39', '40-64', '65-79', '80+')
       OR (p_filters ->> 'sex') NOT IN ('female', 'male', 'other', 'not_recorded')
       OR (p_filters ->> 'setting') NOT IN ('admitted', 'outpatient', 'emergency') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a drillable filter set';
    END IF;
    v_cond := (p_filters ->> 'condition')::uuid;
    v_age := p_filters ->> 'age_band';
    v_sex := p_filters ->> 'sex';
    v_set := p_filters ->> 'setting';

    IF jsonb_typeof(p_group) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_each(p_group) e WHERE jsonb_typeof(e.value) <> 'string') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a drillable group';
    END IF;
    IF g_kind = 'part' AND (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(p_group) k) = ARRAY['dim', 'key', 'kind'] THEN
        g_dim := p_group ->> 'dim';
        g_key := p_group ->> 'key';
        IF NOT ((g_dim = 'age_band' AND g_key IN ('', '0-17', '18-39', '40-64', '65-79', '80+'))
                OR (g_dim = 'sex' AND g_key IN ('female', 'male', 'other', 'not_recorded'))
                OR (g_dim = 'setting' AND g_key IN ('', 'admitted', 'outpatient', 'emergency'))) THEN
            RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a drillable group';
        END IF;
    ELSIF g_kind = 'pair' AND (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(p_group) k) = ARRAY['a', 'b', 'kind']
          AND (p_group ->> 'a') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          AND (p_group ->> 'b') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
          AND (p_group ->> 'a') < (p_group ->> 'b') THEN
        g_a := (p_group ->> 'a')::uuid;
        g_b := (p_group ->> 'b')::uuid;
    ELSIF g_kind = 'finding' AND (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(p_group) k) = ARRAY['condition', 'flag', 'kind']
          AND ((p_group ->> 'condition') = '' OR (p_group ->> 'condition') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
          AND (p_group ->> 'flag') IN ('', 'low', 'high', 'abnormal') THEN
        g_c := NULLIF(p_group ->> 'condition', '')::uuid;
        g_flag := NULLIF(p_group ->> 'flag', '');
    ELSE
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a drillable group';
    END IF;

    RETURN QUERY
    WITH f AS (
        SELECT x.subject_id AS s, x.kind, x.condition_id AS c, x.flag,
               make_date(extract(year FROM x.day)::int, extract(month FROM x.day)::int,
                         CASE WHEN extract(day FROM x.day) <= 7 THEN 1 WHEN extract(day FROM x.day) <= 14 THEN 8
                              WHEN extract(day FROM x.day) <= 21 THEN 15 ELSE 22 END) AS b
        FROM public.analysis_facts x
        JOIN public.analysis_subject_attrs a ON a.hospital_id = x.hospital_id AND a.subject_id = x.subject_id
        WHERE x.hospital_id = v_h AND x.day BETWEEN v_per.start AND v_per."end" AND NOT x.period_late
          AND (v_age IS NULL OR a.age_band = v_age) AND (v_sex IS NULL OR a.sex = v_sex)
          AND (v_set IS NULL OR a.setting = v_set)
    ),
    u AS (SELECT DISTINCT f.s, f.b FROM f WHERE v_cond IS NULL OR f.c = v_cond),
    ds AS (SELECT DISTINCT u.s FROM u)
    SELECT ds.s FROM ds
    JOIN public.analysis_subject_attrs a ON a.hospital_id = v_h AND a.subject_id = ds.s
    WHERE CASE g_kind
        WHEN 'part' THEN CASE g_dim
            WHEN 'age_band' THEN COALESCE(a.age_band, '') = g_key
            WHEN 'sex' THEN a.sex = g_key
            ELSE COALESCE(a.setting, '') = g_key END
        WHEN 'pair' THEN EXISTS (SELECT 1 FROM f WHERE f.s = ds.s AND f.c = g_a)
                     AND EXISTS (SELECT 1 FROM f WHERE f.s = ds.s AND f.c = g_b)
        ELSE EXISTS (SELECT 1 FROM f WHERE f.s = ds.s AND f.kind = 'finding'
                     AND f.c IS NOT DISTINCT FROM g_c AND f.flag IS NOT DISTINCT FROM g_flag) END;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_reserve_patients(quarter, subjects, cap): design §5.6
-- steps 1-3 for the request's person: the counter row locked FOR UPDATE
-- (parallel requests queue on it), the subjects not yet opened this
-- quarter counted, refused (P0001 'patient_cap') past cap, otherwise
-- recorded. Returns the person's new count.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_reserve_patients(p_quarter date, p_subjects uuid[], p_cap integer) RETURNS integer
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h      uuid := public.mhai_analysis_hospital();
    v_u      uuid := public.mhai_current_user();
    v_opened integer;
    v_new    integer;
BEGIN
    IF v_u IS NULL OR p_quarter IS NULL OR p_quarter <> date_trunc('quarter', p_quarter)::date
       OR p_cap IS NULL OR p_cap < 1 OR p_cap > 300 OR p_subjects IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a reservation';
    END IF;
    INSERT INTO public.analysis_patient_budget (hospital_id, user_id, quarter) VALUES (v_h, v_u, p_quarter)
        ON CONFLICT (hospital_id, user_id, quarter) DO NOTHING;
    SELECT b.opened INTO v_opened FROM public.analysis_patient_budget b
    WHERE b.hospital_id = v_h AND b.user_id = v_u AND b.quarter = p_quarter FOR UPDATE;
    SELECT count(DISTINCT s) INTO v_new FROM unnest(p_subjects) s
    WHERE NOT EXISTS (SELECT 1 FROM public.analysis_patient_opens o
                      WHERE o.hospital_id = v_h AND o.user_id = v_u AND o.quarter = p_quarter AND o.subject_id = s);
    IF v_opened + v_new > p_cap THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'patient_cap';
    END IF;
    INSERT INTO public.analysis_patient_opens (hospital_id, user_id, quarter, subject_id)
    SELECT DISTINCT v_h, v_u, p_quarter, s FROM unnest(p_subjects) s
    ON CONFLICT DO NOTHING;
    UPDATE public.analysis_patient_budget b SET opened = b.opened + v_new
    WHERE b.hospital_id = v_h AND b.user_id = v_u AND b.quarter = p_quarter;
    RETURN v_opened + v_new;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_pseudonyms(period, subjects): each subject's hospital-side
-- pid for the period, drawn when missing. A unique violation (a pid
-- already used in the period, or the subject's pid drawn by a parallel
-- request) draws again (N9).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_pseudonyms(p_period uuid, p_subjects uuid[]) RETURNS TABLE (subject_id uuid, pid text)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_s   uuid;
    v_try integer;
BEGIN
    PERFORM public.mhai_analysis_frozen(p_period);
    FOREACH v_s IN ARRAY p_subjects LOOP
        v_try := 0;
        LOOP
            EXIT WHEN EXISTS (SELECT 1 FROM public.analysis_pseudonyms p
                              WHERE p.hospital_id = v_h AND p.side = 'hospital' AND p.scope IS NULL
                                AND p.period_id = p_period AND p.subject_id = v_s);
            v_try := v_try + 1;
            IF v_try > 20 THEN
                RAISE EXCEPTION USING ERRCODE = 'program_limit_exceeded', MESSAGE = 'analysis: no free pid';
            END IF;
            BEGIN
                INSERT INTO public.analysis_pseudonyms (hospital_id, side, scope, period_id, subject_id, pid)
                VALUES (v_h, 'hospital', NULL, p_period, v_s, public.mhai_analysis_pid_draw());
            EXCEPTION WHEN unique_violation THEN
                NULL; -- drawn again
            END;
        END LOOP;
    END LOOP;
    RETURN QUERY SELECT p.subject_id, p.pid FROM public.analysis_pseudonyms p
    WHERE p.hospital_id = v_h AND p.side = 'hospital' AND p.scope IS NULL AND p.period_id = p_period
      AND p.subject_id = ANY (p_subjects);
END
$$;
-- +goose StatementEnd

-- The commit check: a list is kept only with its analysis.patients_viewed
-- entry, by its viewer, in its hospital (T18). Deferred to the end of the
-- transaction, so the entry may be appended after the list (the audit
-- chain's lock is then held only until commit).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_view_audited() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'analysis_view' AND a.entity_id = NEW.id::text
                     AND a.action = 'analysis.patients_viewed' AND a.actor_id = NEW.viewer
                     AND a.hospital_id = NEW.hospital_id) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'analysis_views: a list without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER analysis_views_audited AFTER INSERT ON analysis_views
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_analysis_view_audited();

------------------------------------------------------------------------
-- The request functions (EXECUTE to mhai_gateway only).
------------------------------------------------------------------------

-- mhai_analysis_list_admit(per minute, per day, now): the person's lists
-- in the minute before now and in the hospital day of now (now: the wall
-- clock once the lock is held, unless pinned), under a lock
-- per person and hospital held to the end of the transaction (parallel
-- lists queue, so the count is exact): 'ok', or the limit that refuses
-- ('minute', 'day'). now defaults to the database's and may not be later.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_list_admit(p_minute integer, p_day integer, p_now timestamptz) RETURNS text
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_now timestamptz;
    v_day date;
BEGIN
    IF v_u IS NULL OR p_now > now() OR p_minute IS NULL OR p_minute < 1 OR p_minute > 100
       OR p_day IS NULL OR p_day < 1 OR p_day > 1000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a list admission';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.lists'), hashtext(v_h::text || '/' || v_u::text));
    -- The wall clock after the lock (not the transaction's start): a list
    -- committed by a request that started later but took the lock first is
    -- counted (lists are stamped the same way, mhai_analysis_open).
    v_now := COALESCE(p_now, clock_timestamp());
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    IF (SELECT count(*) FROM public.analysis_views v
        WHERE v.hospital_id = v_h AND v.viewer = v_u AND v.at > v_now - interval '1 minute'
          AND (p_now IS NULL OR v.at <= v_now)) >= p_minute THEN
        RETURN 'minute';
    END IF;
    IF (SELECT count(*) FROM public.analysis_views v
        WHERE v.hospital_id = v_h AND v.viewer = v_u AND v.day = v_day) >= p_day THEN
        RETURN 'day';
    END IF;
    RETURN 'ok';
END
$$;
-- +goose StatementEnd

-- mhai_analysis_open(view, period, filters, group, source view, drill
-- hash, expected, cap, now): one de-identified list, for the request's
-- person. The group's count must equal expected (the shown cell it was
-- opened from) and be 5 or more, otherwise P0001 'too_small'. The first
-- 200 subjects by subject id are reserved against the person's quarter
-- (P0001 'patient_cap' past cap), recorded with the list, given their pids
-- for the period, and returned in order: pid, age band, sex, setting, and
-- at most 10 condition ids and 10 findings (condition id or "", flag or
-- "") recorded within the period, those the list was opened on first, then
-- in a fixed order (so a list's membership is always on its rows). The list is kept only
-- if its audit entry is appended before commit (the deferred check).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_open(p_view uuid, p_period uuid, p_filters jsonb, p_group jsonb, p_source text,
                                   p_drill bytea, p_expected integer, p_cap integer, p_now timestamptz)
    RETURNS TABLE (ord integer, pid text, age_band text, sex text, setting text, conditions jsonb, findings jsonb)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h    uuid := public.mhai_analysis_hospital();
    v_u    uuid := public.mhai_current_user();
    v_now  timestamptz := COALESCE(p_now, clock_timestamp());
    v_per  record;
    -- What the list was opened on comes first on a row (at most 10 of
    -- each are sent): the context's condition, the pair's two, the finding.
    v_prio text[] := ARRAY[p_filters ->> 'condition', p_group ->> 'a', p_group ->> 'b'];
    v_fc   text := CASE WHEN p_group ->> 'kind' = 'finding' THEN p_group ->> 'condition' END;
    v_ff   text := CASE WHEN p_group ->> 'kind' = 'finding' THEN p_group ->> 'flag' END;
    v_all  uuid[];
    v_rows uuid[];
    v_day  date;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    IF v_u IS NULL OR p_view IS NULL OR p_now > now() OR p_source NOT IN ('affected', 'co-occurring', 'findings')
       OR p_drill IS NULL OR octet_length(p_drill) <> 32 OR p_expected IS NULL OR p_cap IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a list';
    END IF;
    SELECT array_agg(g ORDER BY g) INTO v_all FROM public.mhai_analysis_group(p_period, p_filters, p_group) g;
    IF COALESCE(cardinality(v_all), 0) <> p_expected OR p_expected < 5 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'too_small';
    END IF;
    v_rows := v_all[1:200];
    PERFORM public.mhai_analysis_reserve_patients(date_trunc('quarter', v_per.start)::date, v_rows, p_cap);
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_views (id, hospital_id, viewer, level, period_id, source_view, filters, drill_hash,
                                       matched, sent, day, at)
    VALUES (p_view, v_h, v_u, 'deidentified', p_period, p_source, p_filters, p_drill,
            cardinality(v_all), cardinality(v_rows), v_day, v_now);
    INSERT INTO public.analysis_view_rows (hospital_id, view_id, ord, subject_id)
    SELECT v_h, p_view, r.n::integer, r.s FROM unnest(v_rows) WITH ORDINALITY r(s, n);
    RETURN QUERY
    SELECT r.n::integer, p.pid, a.age_band, a.sex, a.setting,
           COALESCE((SELECT jsonb_agg(x.c ORDER BY x.c) FROM (
                SELECT y.c FROM (
                    SELECT DISTINCT fc.condition_id::text AS c FROM public.analysis_facts fc
                    WHERE fc.hospital_id = v_h AND fc.subject_id = r.s AND fc.day BETWEEN v_per.start AND v_per."end"
                      AND NOT fc.period_late AND fc.condition_id IS NOT NULL) y
                ORDER BY COALESCE(y.c = ANY (v_prio), false) DESC, y.c LIMIT 10) x), '[]'::jsonb),
           COALESCE((SELECT jsonb_agg(jsonb_build_object('condition', x.c, 'flag', x.fl) ORDER BY x.c, x.fl) FROM (
                SELECT y.c, y.fl FROM (
                    SELECT DISTINCT COALESCE(ff.condition_id::text, '') AS c, COALESCE(ff.flag, '') AS fl FROM public.analysis_facts ff
                    WHERE ff.hospital_id = v_h AND ff.subject_id = r.s AND ff.day BETWEEN v_per.start AND v_per."end"
                      AND NOT ff.period_late AND ff.kind = 'finding') y
                ORDER BY (y.c IS NOT DISTINCT FROM v_fc AND y.fl IS NOT DISTINCT FROM v_ff) DESC, y.c, y.fl LIMIT 10) x), '[]'::jsonb)
    FROM unnest(v_rows) WITH ORDINALITY r(s, n)
    JOIN public.analysis_subject_attrs a ON a.hospital_id = v_h AND a.subject_id = r.s
    JOIN public.mhai_analysis_pseudonyms(p_period, v_rows) p ON p.subject_id = r.s
    ORDER BY r.n;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_drill_flag(kind, now): records that a list limit or the
-- patient cap refused the request's person; true the first time in the
-- hospital day (the flagged audit entry is written then, once).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_drill_flag(p_kind text, p_now timestamptz) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_now timestamptz := COALESCE(p_now, now());
    v_day date;
    v_n   integer;
BEGIN
    IF v_u IS NULL OR v_now > now() OR p_kind NOT IN ('list_minute', 'list_day', 'patient_cap') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a flag';
    END IF;
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_drill_flags (hospital_id, user_id, kind, day) VALUES (v_h, v_u, p_kind, v_day)
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n = 1;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The reveal (EXECUTE to mhai_platform only).
------------------------------------------------------------------------

-- mhai_audit_view_pseudonyms(audit id): the pids a drill-down entry's list
-- sent, in order, for its period; null when the entry isn't one.
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_view_pseudonyms(p_audit_id bigint) RETURNS text[]
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_entity text;
    v_h      uuid;
    v_out    text[];
BEGIN
    SELECT a.entity_id, a.hospital_id INTO v_entity, v_h FROM public.audit_log a
    WHERE a.id = p_audit_id AND a.action = 'analysis.patients_viewed' AND a.entity = 'analysis_view';
    IF v_entity IS NULL OR v_h IS NULL
       OR v_entity !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        RETURN NULL;
    END IF;
    SELECT array_agg(p.pid ORDER BY r.ord) INTO v_out
    FROM public.analysis_views v
    JOIN public.analysis_view_rows r ON r.hospital_id = v.hospital_id AND r.view_id = v.id
    JOIN public.analysis_pseudonyms p ON p.hospital_id = v.hospital_id AND p.side = 'hospital' AND p.scope IS NULL
                                     AND p.period_id = v.period_id AND p.subject_id = r.subject_id
    WHERE v.id = v_entity::uuid AND v.hospital_id = v_h;
    RETURN v_out;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_sweep, replaced: 00025's retention, plus opens and their
-- counters once their quarter can't be listed any more, flags after two
-- hospital days; a subject a list or an open cites is kept (design §4.3: as long as
-- the audit record that cites it).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_cut   date;
    v_today date;
    v_n     integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    v_today := (p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date;
    v_cut := (v_today - interval '25 months')::date;
    DELETE FROM public.analysis_facts f WHERE f.hospital_id = p_hospital AND f.day < v_cut;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    -- 00025: stored bodies live as long as their period's facts (25
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
    DELETE FROM public.analysis_view_log l WHERE l.hospital_id = p_hospital AND l.day < v_today - 2;
    DELETE FROM public.analysis_unusual u WHERE u.hospital_id = p_hospital AND u.day < v_today - 2;
    -- 00028: the distinct-patient cap's opens and counters once no period
    -- of their quarter can be listed any more (the quarter ended before the
    -- 25-month cut; N10, departure 22): until then the cap holds for it.
    -- The drill flags after two hospital days.
    DELETE FROM public.analysis_patient_opens o
    WHERE o.hospital_id = p_hospital AND (o.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_patient_budget b
    WHERE b.hospital_id = p_hospital AND (b.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_drill_flags d WHERE d.hospital_id = p_hospital AND d.day < v_today - 2;
    DELETE FROM public.analysis_first_seen fs WHERE fs.hospital_id = p_hospital AND fs.first_day < v_cut;
    DELETE FROM public.analysis_subjects s
    WHERE s.hospital_id = p_hospital AND s.created_day < v_cut
      AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = s.hospital_id AND f.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_first_seen fs WHERE fs.hospital_id = s.hospital_id AND fs.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_pseudonyms p WHERE p.hospital_id = s.hospital_id AND p.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_view_rows r WHERE r.hospital_id = s.hospital_id AND r.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_patient_opens o WHERE o.hospital_id = s.hospital_id AND o.subject_id = s.subject_id);
    RETURN v_n;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_analysis_pid_draw() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_group(uuid, jsonb, jsonb) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_reserve_patients(date, uuid[], integer) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_pseudonyms(uuid, uuid[]) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_view_audited() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_list_admit(integer, integer, timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_open(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_drill_flag(text, timestamptz) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_audit_view_pseudonyms(bigint) OWNER TO mhai_analysis_auditor;
REVOKE ALL ON FUNCTION mhai_analysis_pid_draw(), mhai_analysis_group(uuid, jsonb, jsonb),
    mhai_analysis_reserve_patients(date, uuid[], integer), mhai_analysis_pseudonyms(uuid, uuid[]),
    mhai_analysis_view_audited(), mhai_analysis_list_admit(integer, integer, timestamptz),
    mhai_analysis_open(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, timestamptz),
    mhai_analysis_drill_flag(text, timestamptz), mhai_audit_view_pseudonyms(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_list_admit(integer, integer, timestamptz),
    mhai_analysis_open(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, timestamptz),
    mhai_analysis_drill_flag(text, timestamptz) TO mhai_gateway;
GRANT EXECUTE ON FUNCTION mhai_audit_view_pseudonyms(bigint) TO mhai_platform;

------------------------------------------------------------------------
-- The platform's audit appends: 00022's list plus the reveal.
------------------------------------------------------------------------
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed', 'analytics.followed_up', 'analytics.follow_up_edited',
                           'analytics.follow_up_undone', 'audit.patients_viewed')
                AND hospital_id IS NULL);

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader', 'mhai_analysis_auditor'] LOOP
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
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader', 'mhai_analysis_auditor'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- 00022's append, as it was.
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed', 'analytics.followed_up', 'analytics.follow_up_edited',
                           'analytics.follow_up_undone')
                AND hospital_id IS NULL);

-- 00025's sweep, as it was.
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
    -- 00025: stored bodies live as long as their period's facts (25
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

DROP FUNCTION mhai_audit_view_pseudonyms(bigint);
DROP FUNCTION mhai_analysis_drill_flag(text, timestamptz);
DROP FUNCTION mhai_analysis_open(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, timestamptz);
DROP FUNCTION mhai_analysis_list_admit(integer, integer, timestamptz);
DROP TRIGGER analysis_views_audited ON analysis_views;
DROP FUNCTION mhai_analysis_view_audited();
DROP FUNCTION mhai_analysis_pseudonyms(uuid, uuid[]);
DROP FUNCTION mhai_analysis_reserve_patients(date, uuid[], integer);
DROP FUNCTION mhai_analysis_group(uuid, jsonb, jsonb);
DROP FUNCTION mhai_analysis_pid_draw();

DROP POLICY analysis_auditor ON audit_log;
DROP POLICY analysis_reader ON audit_log;
REVOKE ALL ON audit_log FROM mhai_analysis_reader, mhai_analysis_auditor;
DROP POLICY analysis_auditor ON analysis_pseudonyms;
DROP POLICY analysis_reader ON analysis_pseudonyms;
REVOKE ALL ON analysis_pseudonyms FROM mhai_analysis_reader, mhai_analysis_auditor;
DROP INDEX audit_log_analysis_view_idx;

-- The tables take their policies, grants and indexes with them.
DROP TABLE analysis_drill_flags;
DROP TABLE analysis_patient_opens;
DROP TABLE analysis_patient_budget;
DROP TABLE analysis_view_rows;
DROP TABLE analysis_views;

REVOKE USAGE ON SCHEMA public FROM mhai_analysis_auditor;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader', 'mhai_analysis_auditor'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
