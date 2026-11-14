-- Patient Analysis, EMR facts (analysis backend design §4.1, §4.4; plan
-- Tasks 27-28, PR 10, departures 78-90). Numbered 00046: Documents PR 3
-- holds 00045.
--
-- Nothing here turns anything on. Ingest still refuses a hospital whose
-- switch is off or carries demo references, and core reads the source only
-- with ANALYSIS_INGEST=true (false unless set).
--
-- What changes:
--   - mhai_analysis_ingest_source is replaced (dropped and made again: its
--     rows gain the finding and alert columns). It now enters the window's
--     dictionary entries first (sources from emr_connections, locations
--     from emr.units, conditions from the decisions' ICD-10 codes, named by
--     the code), then returns, after the watermark and within the last 25
--     months up to the hospital's day:
--       consultations whose question names an active patient of the
--       hospital (age band and sex from the patient index, never from
--       patient_data_json; the 00021 confidence mapping, H2's to confirm);
--       findings, not shadow, open or historical, not an amendment of
--       another, when created (flag from the source flag, else the value
--       against its range, else abnormal; setting and location from the
--       observation's encounter);
--       alerts settled by a person (handled or dismissed, never withdrawn),
--       when settled, on the day raised, with the minutes to the response.
--   - mhai_analysis_patient_changes(hospital, until): the merges and the
--     done exclusion purges (with the patients merged into an excluded
--     one) after the hospital's changes_until; nothing for a hospital that
--     holds no sealed reference. A purge held by a legal hold isn't done,
--     so it isn't listed until the hold is lifted and the purge runs.
--   - mhai_analysis_apply_changes(hospital, until, {"changes": [...]}):
--     forget (an exclusion) deletes the subject's facts and first days in
--     unfrozen blocks, clears ref_enc and replaces ref_hmac with random
--     bytes; merge moves the merged subject's unfrozen facts to the
--     survivor (made when missing), gives the survivor the merged subject's
--     first days (late where frozen) and drops the merged subject's
--     unfrozen first days. Nothing in a frozen block, no period, pid, list
--     or stored body is touched.
--   - mhai_analysis_due also lists a hospital that holds a sealed
--     reference, so an exclusion reaches a subject whose facts aged out.
--   - analysis_watermarks.changes_until.
--
-- Roles: mhai_analysis_writer gains column reads only (never a name, an
-- MRN, a note, a payload, a cause or a free-text value) on the EMR tables
-- below and clinical_queries.patient_id, with a SELECT policy per table
-- (its functions filter on the hospital they are given), and UPDATE of
-- ref_hmac and ref_enc (the forget) and changes_until. The two new
-- functions are the job's only (EXECUTE to mhai_analysis_job), definers
-- owned by the writer, search_path pinned, every relation qualified.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_analysis_writer', 'MEMBER') THEN
        GRANT mhai_analysis_writer TO CURRENT_USER;
        PERFORM set_config('mhai.mhai_analysis_writer_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE analysis_watermarks ADD COLUMN changes_until timestamptz;
GRANT UPDATE (changes_until) ON analysis_watermarks TO mhai_analysis_writer;
GRANT UPDATE (ref_hmac, ref_enc) ON analysis_subjects TO mhai_analysis_writer;

-- What the source reads: codes, ids and times only.
GRANT USAGE ON SCHEMA emr TO mhai_analysis_writer;
GRANT SELECT (patient_id) ON clinical_queries TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, birth_date, sex, status, merged_into) ON emr.patients TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, patient_id, connection_id, primary_observation_id, effective_at, effective_date, status, shadow,
              supersedes, created_at) ON emr.findings TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, encounter_id, source_flag, value_si, ref_low, ref_high) ON emr.observations TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, class, unit_id) ON emr.encounters TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, label, kind) ON emr.units TO mhai_analysis_writer;
GRANT SELECT (hospital_id, kept_patient, merged_patient, merged_at) ON emr.merges TO mhai_analysis_writer;
GRANT SELECT (hospital_id, patient_id, state, done_at) ON emr.purge_requests TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, source_label) ON emr_connections TO mhai_analysis_writer;
GRANT SELECT (id, hospital_id, finding_id, patient_id, connection_id, severity, status, withdrawn, handled_at, dismiss_reason,
              created_at) ON alerts TO mhai_analysis_writer;
CREATE POLICY analysis_writer_emr ON emr.patients FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.findings FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.observations FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.encounters FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.units FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.merges FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr.purge_requests FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON emr_connections FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_emr ON alerts FOR SELECT TO mhai_analysis_writer USING (true);

-- mhai_analysis_age_band(birth, day): the band of the age on day; null
-- without a birth date. Internal (no EXECUTE for anyone but its owner).
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_age_band(p_birth date, p_day date) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE WHEN p_birth IS NULL OR p_birth > p_day THEN NULL
                WHEN extract(year FROM age(p_day, p_birth)) < 18 THEN '0-17'
                WHEN extract(year FROM age(p_day, p_birth)) < 40 THEN '18-39'
                WHEN extract(year FROM age(p_day, p_birth)) < 65 THEN '40-64'
                WHEN extract(year FROM age(p_day, p_birth)) < 80 THEN '65-79'
                ELSE '80+' END
$$;
-- +goose StatementEnd

DROP FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz);

-- mhai_analysis_ingest_source(hospital, after, until): see the header.
-- Every output column is also a variable here: every column reference is
-- qualified.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_ingest_source(p_hospital uuid, p_after timestamptz, p_until timestamptz)
    RETURNS TABLE (ref text, day date, kind text, condition_id uuid, age_band text, sex text, setting text,
                   location_id uuid, specialty text, source_id uuid, urgency text, confidence text,
                   rating text, correction text, flag text, severity text, ack_minutes integer, dismiss_reason text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_tz    text;
    v_today date;
    v_min   date;
BEGIN
    IF p_hospital IS NULL OR p_until IS NULL OR p_until > now() OR p_after > p_until THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a window that has passed';
    END IF;
    IF public.mhai_analysis_position(p_hospital) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the switch is off for this hospital';
    END IF;
    IF public.mhai_analysis_demo(p_hospital) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a demo switch never ingests consultations';
    END IF;
    v_tz := public.mhai_analysis_zone(p_hospital);
    v_today := (p_until AT TIME ZONE v_tz)::date;
    v_min := (v_today - interval '25 months')::date;

    -- The dictionaries: configuration, never patient data. A label the
    -- dictionary's check would refuse is left out.
    INSERT INTO public.analysis_sources (hospital_id, id, label)
    SELECT c.hospital_id, c.id, c.source_label
    FROM public.emr_connections c
    WHERE c.hospital_id = p_hospital
      AND c.source_label ~ '^[^[:cntrl:]]{1,80}$'
      AND c.source_label !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'
    ON CONFLICT (hospital_id, id) DO NOTHING;
    INSERT INTO public.analysis_locations (hospital_id, id, label, setting)
    SELECT u.hospital_id, u.id, u.label,
           CASE u.kind WHEN 'ward' THEN 'admitted' WHEN 'clinic' THEN 'outpatient' WHEN 'emergency' THEN 'emergency' END
    FROM emr.units u
    WHERE u.hospital_id = p_hospital
      AND u.label ~ '^[^[:cntrl:]]{1,80}$'
      AND u.label !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'
    ON CONFLICT (hospital_id, id) DO NOTHING;
    INSERT INTO public.analysis_conditions (hospital_id, icd10, name)
    SELECT DISTINCT p_hospital, upper(btrim(d.icd10_code)), upper(btrim(d.icd10_code))
    FROM public.clinical_decisions d
    JOIN public.clinical_queries q ON q.id = d.query_id AND q.hospital_id = d.hospital_id
    WHERE d.hospital_id = p_hospital AND q.patient_id IS NOT NULL
      AND (p_after IS NULL OR d.created_at > p_after) AND d.created_at <= p_until
      AND upper(btrim(d.icd10_code)) ~ '^[A-Z][0-9]{2}(\.[0-9A-Z]{1,4})?$'
    ON CONFLICT (hospital_id, icd10) WHERE icd10 IS NOT NULL DO NOTHING;

    RETURN QUERY
    -- Consultations that name an active patient of the hospital.
    SELECT q.patient_id::text, x.dd, 'consultation'::text, c.id, public.mhai_analysis_age_band(p.birth_date, x.dd),
           CASE WHEN p.sex IN ('female', 'male', 'other') THEN p.sex ELSE 'not_recorded' END,
           NULL::text, NULL::uuid, NULL::text, NULL::uuid,
           CASE WHEN q.urgency IN ('routine', 'urgent', 'emergency') THEN q.urgency::text END,
           CASE WHEN d.degraded IS TRUE THEN 'failed'
                WHEN d.hga_decision = 'PASS' AND d.trust_score >= 0.8 THEN 'high'
                WHEN d.hga_decision = 'PASS' THEN 'caution'
                ELSE 'review' END,
           NULL::text, NULL::text, NULL::text, NULL::text, NULL::integer, NULL::text
    FROM public.clinical_decisions d
    JOIN public.clinical_queries q ON q.id = d.query_id AND q.hospital_id = d.hospital_id
    JOIN emr.patients p ON p.hospital_id = q.hospital_id AND p.id = q.patient_id AND p.status = 'active'
    LEFT JOIN public.analysis_conditions c ON c.hospital_id = d.hospital_id AND c.icd10 = upper(btrim(d.icd10_code))
    CROSS JOIN LATERAL (SELECT (d.created_at AT TIME ZONE v_tz)::date AS dd) x
    WHERE d.hospital_id = p_hospital
      AND (p_after IS NULL OR d.created_at > p_after) AND d.created_at <= p_until
      AND x.dd BETWEEN v_min AND v_today
    UNION ALL
    -- Findings.
    SELECT f.patient_id::text, x.dd, 'finding'::text, NULL::uuid, public.mhai_analysis_age_band(p.birth_date, x.dd),
           CASE WHEN p.sex IN ('female', 'male', 'other') THEN p.sex ELSE 'not_recorded' END,
           CASE e.class WHEN 'inpatient' THEN 'admitted' WHEN 'outpatient' THEN 'outpatient' WHEN 'emergency' THEN 'emergency' END,
           l.id, NULL::text, s.id, NULL::text, NULL::text, NULL::text, NULL::text,
           CASE WHEN o.source_flag IN ('L', 'LL') THEN 'low'
                WHEN o.source_flag IN ('H', 'HH') THEN 'high'
                WHEN o.source_flag IN ('A', 'AA') THEN 'abnormal'
                WHEN o.value_si < o.ref_low THEN 'low'
                WHEN o.value_si > o.ref_high THEN 'high'
                ELSE 'abnormal' END,
           NULL::text, NULL::integer, NULL::text
    FROM emr.findings f
    JOIN emr.patients p ON p.hospital_id = f.hospital_id AND p.id = f.patient_id AND p.status = 'active'
    JOIN emr.observations o ON o.hospital_id = f.hospital_id AND o.id = f.primary_observation_id
    LEFT JOIN emr.encounters e ON e.hospital_id = o.hospital_id AND e.id = o.encounter_id
    LEFT JOIN public.analysis_locations l ON l.hospital_id = f.hospital_id AND l.id = e.unit_id
    LEFT JOIN public.analysis_sources s ON s.hospital_id = f.hospital_id AND s.id = f.connection_id
    CROSS JOIN LATERAL (SELECT COALESCE((f.effective_at AT TIME ZONE v_tz)::date, f.effective_date) AS dd) x
    WHERE f.hospital_id = p_hospital
      AND (p_after IS NULL OR f.created_at > p_after) AND f.created_at <= p_until
      AND NOT f.shadow AND f.status IN ('open', 'historical') AND f.supersedes IS NULL
      AND x.dd BETWEEN v_min AND v_today
    UNION ALL
    -- Alerts settled by a person.
    SELECT a.patient_id::text, x.dd, 'alert'::text, NULL::uuid, public.mhai_analysis_age_band(p.birth_date, x.dd),
           CASE WHEN p.sex IN ('female', 'male', 'other') THEN p.sex ELSE 'not_recorded' END,
           CASE e.class WHEN 'inpatient' THEN 'admitted' WHEN 'outpatient' THEN 'outpatient' WHEN 'emergency' THEN 'emergency' END,
           l.id, NULL::text, s.id, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text,
           a.severity,
           LEAST(GREATEST(floor(extract(epoch FROM a.handled_at - a.created_at) / 60), 0), 525600)::integer,
           CASE WHEN a.status = 'dismissed' THEN a.dismiss_reason END
    FROM public.alerts a
    JOIN emr.patients p ON p.hospital_id = a.hospital_id AND p.id = a.patient_id AND p.status = 'active'
    LEFT JOIN emr.findings f ON f.hospital_id = a.hospital_id AND f.id = a.finding_id
    LEFT JOIN emr.observations o ON o.hospital_id = f.hospital_id AND o.id = f.primary_observation_id
    LEFT JOIN emr.encounters e ON e.hospital_id = o.hospital_id AND e.id = o.encounter_id
    LEFT JOIN public.analysis_locations l ON l.hospital_id = a.hospital_id AND l.id = e.unit_id
    LEFT JOIN public.analysis_sources s ON s.hospital_id = a.hospital_id AND s.id = a.connection_id
    CROSS JOIN LATERAL (SELECT (a.created_at AT TIME ZONE v_tz)::date AS dd) x
    WHERE a.hospital_id = p_hospital
      AND a.status IN ('handled', 'dismissed') AND a.withdrawn IS NULL AND a.handled_at IS NOT NULL
      AND (p_after IS NULL OR a.handled_at > p_after) AND a.handled_at <= p_until
      AND x.dd BETWEEN v_min AND v_today;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_patient_changes(hospital, until): see the header. Ordered
-- by time, a merge before an exclusion at the same time.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_patient_changes(p_hospital uuid, p_until timestamptz)
    RETURNS TABLE (kind text, ref text, survivor text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_after timestamptz;
BEGIN
    IF p_hospital IS NULL OR p_until IS NULL OR p_until > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    SELECT w.changes_until INTO v_after FROM public.analysis_watermarks w WHERE w.hospital_id = p_hospital;
    IF v_after > p_until THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the watermark never moves back';
    END IF;
    -- No sealed reference, nothing to change: no patient id leaves.
    IF NOT EXISTS (SELECT 1 FROM public.analysis_subjects s WHERE s.hospital_id = p_hospital AND s.ref_enc IS NOT NULL) THEN
        RETURN;
    END IF;
    RETURN QUERY
    WITH RECURSIVE gone (pid, t) AS (
        SELECT r.patient_id, r.done_at
        FROM emr.purge_requests r
        WHERE r.hospital_id = p_hospital AND r.state = 'done'
          AND (v_after IS NULL OR r.done_at > v_after) AND r.done_at <= p_until
        UNION
        SELECT m.id, g.t
        FROM emr.patients m JOIN gone g ON m.hospital_id = p_hospital AND m.merged_into = g.pid
    )
    SELECT y.k, y.r, y.s FROM (
        SELECT 'merged'::text AS k, mg.merged_patient::text AS r, mg.kept_patient::text AS s, mg.merged_at AS t, 0 AS o
        FROM emr.merges mg
        WHERE mg.hospital_id = p_hospital
          AND (v_after IS NULL OR mg.merged_at > v_after) AND mg.merged_at <= p_until
        UNION ALL
        SELECT 'excluded'::text, g.pid::text, NULL::text, g.t, 1 FROM gone g
    ) y
    ORDER BY y.t, y.o, y.r;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_apply_changes(hospital, until, changes): see the header.
-- Every change is checked before any is applied; the freeze lock keeps a
-- block from freezing while facts move. Returns the subjects changed.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_apply_changes(p_hospital uuid, p_until timestamptz, p_changes jsonb) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_c     jsonb;
    v_kind  text;
    v_mark  timestamptz;
    v_today date;
    v_from  bytea;
    v_m     uuid;
    v_k     uuid;
    v_n     integer := 0;
BEGIN
    IF p_hospital IS NULL OR p_until IS NULL OR p_until > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    IF jsonb_typeof(p_changes) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(p_changes) k WHERE k <> 'changes')
       OR jsonb_typeof(p_changes -> 'changes') IS DISTINCT FROM 'array' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: changes must be {"changes": [...]}';
    END IF;
    FOR v_c IN SELECT e.value FROM jsonb_array_elements(p_changes -> 'changes') e LOOP
        v_kind := CASE WHEN jsonb_typeof(v_c) = 'object' THEN v_c ->> 'kind' END;
        IF v_kind IS NULL OR v_kind NOT IN ('forget', 'merge') THEN
            RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a change is malformed';
        END IF;
        IF (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(v_c) k)
              <> (CASE v_kind WHEN 'forget' THEN ARRAY['kind', 'ref_hmac']
                              ELSE ARRAY['kind', 'ref_hmac', 'to_enc', 'to_hmac'] END)
           OR jsonb_typeof(v_c -> 'ref_hmac') IS DISTINCT FROM 'string'
           OR (v_c ->> 'ref_hmac') !~ '^[0-9a-f]{64}$'
           OR (v_kind = 'merge' AND (
                   jsonb_typeof(v_c -> 'to_hmac') IS DISTINCT FROM 'string'
                   OR (v_c ->> 'to_hmac') !~ '^[0-9a-f]{64}$'
                   OR v_c ->> 'to_hmac' = v_c ->> 'ref_hmac'
                   OR jsonb_typeof(v_c -> 'to_enc') IS DISTINCT FROM 'string'
                   OR (v_c ->> 'to_enc') !~ '^[A-Za-z0-9+/]+={0,2}$'
                   OR length(v_c ->> 'to_enc') NOT BETWEEN 40 AND 1400)) THEN
            RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a change is malformed';
        END IF;
    END LOOP;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.freeze'), hashtext(p_hospital::text));
    SELECT w.changes_until INTO v_mark FROM public.analysis_watermarks w WHERE w.hospital_id = p_hospital;
    IF v_mark > p_until THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the watermark never moves back';
    END IF;
    v_today := (p_until AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date;

    FOR v_c IN SELECT e.value FROM jsonb_array_elements(p_changes -> 'changes') e LOOP
        v_from := decode(v_c ->> 'ref_hmac', 'hex');
        v_m := NULL;
        SELECT s.subject_id INTO v_m FROM public.analysis_subjects s
        WHERE s.hospital_id = p_hospital AND s.ref_hmac = v_from FOR UPDATE;
        CONTINUE WHEN v_m IS NULL;
        IF v_c ->> 'kind' = 'merge' THEN
            -- Nothing to carry over: no survivor subject is made.
            CONTINUE WHEN NOT EXISTS (SELECT 1 FROM public.analysis_first_seen m
                                      WHERE m.hospital_id = p_hospital AND m.subject_id = v_m)
                     AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f
                                     WHERE f.hospital_id = p_hospital AND f.subject_id = v_m
                                       AND NOT public.mhai_analysis_block_frozen(p_hospital, f.day));
            v_k := NULL;
            INSERT INTO public.analysis_subjects (hospital_id, ref_hmac, ref_enc, created_day)
                VALUES (p_hospital, decode(v_c ->> 'to_hmac', 'hex'), decode(v_c ->> 'to_enc', 'base64'), v_today)
                ON CONFLICT (hospital_id, ref_hmac) DO NOTHING
                RETURNING subject_id INTO v_k;
            IF v_k IS NULL THEN
                SELECT s.subject_id INTO v_k FROM public.analysis_subjects s
                WHERE s.hospital_id = p_hospital AND s.ref_hmac = decode(v_c ->> 'to_hmac', 'hex');
            END IF;
            -- The survivor's first days: the merged subject's earlier ones,
            -- late where their block is frozen; a frozen first day never
            -- moves.
            INSERT INTO public.analysis_first_seen AS fs (hospital_id, subject_id, condition_id, first_day, first_late)
            SELECT p_hospital, v_k, m.condition_id, m.first_day,
                   m.first_late OR public.mhai_analysis_block_frozen(p_hospital, m.first_day)
            FROM public.analysis_first_seen m
            WHERE m.hospital_id = p_hospital AND m.subject_id = v_m
            ON CONFLICT (hospital_id, subject_id, condition_id) DO UPDATE
            SET first_day = EXCLUDED.first_day, first_late = EXCLUDED.first_late
            WHERE EXCLUDED.first_day < fs.first_day
              AND NOT public.mhai_analysis_block_frozen(fs.hospital_id, fs.first_day);
            -- The unfrozen facts move (the insert sets the survivor's
            -- attributes if it has none yet).
            WITH moved AS (
                DELETE FROM public.analysis_facts f
                WHERE f.hospital_id = p_hospital AND f.subject_id = v_m
                  AND NOT public.mhai_analysis_block_frozen(p_hospital, f.day)
                RETURNING f.*
            )
            INSERT INTO public.analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting,
                location_id, specialty, source_id, urgency, confidence, rating, correction, flag, severity, ack_minutes,
                dismiss_reason, ingested_at, period_late)
            SELECT mv.hospital_id, v_k, mv.day, mv.kind, mv.condition_id, mv.age_band, mv.sex, mv.setting, mv.location_id,
                   mv.specialty, mv.source_id, mv.urgency, mv.confidence, mv.rating, mv.correction, mv.flag, mv.severity,
                   mv.ack_minutes, mv.dismiss_reason, mv.ingested_at, mv.period_late
            FROM moved mv ORDER BY mv.id;
        ELSE
            DELETE FROM public.analysis_facts f
            WHERE f.hospital_id = p_hospital AND f.subject_id = v_m
              AND NOT public.mhai_analysis_block_frozen(p_hospital, f.day);
            UPDATE public.analysis_subjects s
            SET ref_enc = NULL, ref_hmac = sha256(convert_to(gen_random_uuid()::text || gen_random_uuid()::text, 'UTF8'))
            WHERE s.hospital_id = p_hospital AND s.subject_id = v_m;
        END IF;
        DELETE FROM public.analysis_first_seen fs
        WHERE fs.hospital_id = p_hospital AND fs.subject_id = v_m
          AND NOT public.mhai_analysis_block_frozen(p_hospital, fs.first_day);
        v_n := v_n + 1;
    END LOOP;
    INSERT INTO public.analysis_watermarks AS w (hospital_id, changes_until) VALUES (p_hospital, p_until)
        ON CONFLICT (hospital_id) DO UPDATE SET changes_until = EXCLUDED.changes_until;
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_due, replaced: 00021's, plus any hospital that holds a
-- sealed reference.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_due(p_now timestamptz)
    RETURNS TABLE (hospital_id uuid, analysis_on boolean, demo boolean, ingested_until timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    RETURN QUERY
    SELECT h.id, public.mhai_analysis_position(h.id) = 'on', public.mhai_analysis_demo(h.id), w.ingested_until
    FROM public.hospitals h
    LEFT JOIN public.analysis_watermarks w ON w.hospital_id = h.id
    WHERE (public.mhai_analysis_position(h.id) = 'on'
           OR EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = h.id)
           OR EXISTS (SELECT 1 FROM public.analysis_subjects s WHERE s.hospital_id = h.id AND s.ref_enc IS NOT NULL))
      AND (p_now AT TIME ZONE h.time_zone) >= (p_now AT TIME ZONE h.time_zone)::date + interval '30 minutes'
      AND (w.last_run_day IS NULL OR w.last_run_day < (p_now AT TIME ZONE h.time_zone)::date)
    ORDER BY h.id;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_analysis_age_band(date, date) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_patient_changes(uuid, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_apply_changes(uuid, timestamptz, jsonb) OWNER TO mhai_analysis_writer;
REVOKE ALL ON FUNCTION mhai_analysis_age_band(date, date), mhai_analysis_ingest_source(uuid, timestamptz, timestamptz),
    mhai_analysis_patient_changes(uuid, timestamptz), mhai_analysis_apply_changes(uuid, timestamptz, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz),
    mhai_analysis_patient_changes(uuid, timestamptz), mhai_analysis_apply_changes(uuid, timestamptz, jsonb)
    TO mhai_analysis_job;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.mhai_analysis_writer_granted', true) = 'yes' THEN
        REVOKE mhai_analysis_writer FROM CURRENT_USER;
        PERFORM set_config('mhai.mhai_analysis_writer_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'mhai_analysis_writer', 'MEMBER') THEN
        GRANT mhai_analysis_writer TO CURRENT_USER;
        PERFORM set_config('mhai.mhai_analysis_writer_granted', 'yes', false);
    END IF;
END
$$;
-- +goose StatementEnd

DROP FUNCTION mhai_analysis_apply_changes(uuid, timestamptz, jsonb);
DROP FUNCTION mhai_analysis_patient_changes(uuid, timestamptz);
DROP FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz);
DROP FUNCTION mhai_analysis_age_band(date, date);

-- 00021's source, as it was.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_ingest_source(p_hospital uuid, p_after timestamptz, p_until timestamptz)
    RETURNS TABLE (ref text, day date, kind text, condition_id uuid, age_band text, sex text, setting text,
                   location_id uuid, specialty text, source_id uuid, urgency text, confidence text,
                   rating text, correction text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_tz text;
BEGIN
    IF p_hospital IS NULL OR p_until IS NULL OR p_until > now() OR p_after > p_until THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a window that has passed';
    END IF;
    IF public.mhai_analysis_position(p_hospital) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the switch is off for this hospital';
    END IF;
    IF public.mhai_analysis_demo(p_hospital) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a demo switch never ingests consultations';
    END IF;
    v_tz := public.mhai_analysis_zone(p_hospital);
    -- Every output column is also a variable here: every column reference
    -- below is qualified.
    RETURN QUERY
    SELECT NULL::text, (d.created_at AT TIME ZONE v_tz)::date, 'consultation'::text, NULL::uuid, NULL::text,
           'not_recorded'::text, NULL::text, NULL::uuid, NULL::text, NULL::uuid,
           CASE WHEN q.urgency IN ('routine', 'urgent', 'emergency') THEN q.urgency::text END,
           CASE WHEN d.degraded IS TRUE THEN 'failed'
                WHEN d.hga_decision = 'PASS' AND d.trust_score >= 0.8 THEN 'high'
                WHEN d.hga_decision = 'PASS' THEN 'caution'
                ELSE 'review' END,
           NULL::text, NULL::text
    FROM public.clinical_decisions d
    JOIN public.clinical_queries q ON q.id = d.query_id AND q.hospital_id = d.hospital_id
    WHERE d.hospital_id = p_hospital
      AND (p_after IS NULL OR d.created_at > p_after) AND d.created_at <= p_until
      AND false; -- no consultation carries a patient reference yet (PR 10)
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz) OWNER TO mhai_analysis_writer;
REVOKE ALL ON FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz) TO mhai_analysis_job;

-- 00021's due, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_due(p_now timestamptz)
    RETURNS TABLE (hospital_id uuid, analysis_on boolean, demo boolean, ingested_until timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    RETURN QUERY
    SELECT h.id, public.mhai_analysis_position(h.id) = 'on', public.mhai_analysis_demo(h.id), w.ingested_until
    FROM public.hospitals h
    LEFT JOIN public.analysis_watermarks w ON w.hospital_id = h.id
    WHERE (public.mhai_analysis_position(h.id) = 'on'
           OR EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = h.id))
      AND (p_now AT TIME ZONE h.time_zone) >= (p_now AT TIME ZONE h.time_zone)::date + interval '30 minutes'
      AND (w.last_run_day IS NULL OR w.last_run_day < (p_now AT TIME ZONE h.time_zone)::date)
    ORDER BY h.id;
END
$$;
-- +goose StatementEnd

DROP POLICY analysis_writer_emr ON emr.patients;
DROP POLICY analysis_writer_emr ON emr.findings;
DROP POLICY analysis_writer_emr ON emr.observations;
DROP POLICY analysis_writer_emr ON emr.encounters;
DROP POLICY analysis_writer_emr ON emr.units;
DROP POLICY analysis_writer_emr ON emr.merges;
DROP POLICY analysis_writer_emr ON emr.purge_requests;
DROP POLICY analysis_writer_emr ON emr_connections;
DROP POLICY analysis_writer_emr ON alerts;
REVOKE SELECT ON emr.patients, emr.findings, emr.observations, emr.encounters, emr.units, emr.merges, emr.purge_requests,
    emr_connections, alerts FROM mhai_analysis_writer;
REVOKE SELECT (patient_id) ON clinical_queries FROM mhai_analysis_writer;
REVOKE USAGE ON SCHEMA emr FROM mhai_analysis_writer;
REVOKE UPDATE (ref_hmac, ref_enc) ON analysis_subjects FROM mhai_analysis_writer;
ALTER TABLE analysis_watermarks DROP COLUMN changes_until;

-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('mhai.mhai_analysis_writer_granted', true) = 'yes' THEN
        REVOKE mhai_analysis_writer FROM CURRENT_USER;
        PERFORM set_config('mhai.mhai_analysis_writer_granted', '', false);
    END IF;
END
$$;
-- +goose StatementEnd
