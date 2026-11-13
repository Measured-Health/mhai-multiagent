-- EMR integration, findings (EMR design §11.3, §11.4, §12.4, §15.3, §15.4;
-- plan Tasks 26, 28, 29 and 30). Numbered 00027: 00024 is Gemini's, 00025
-- Analysis PR 4's and 00026 EMR PR 6's (emr.sync_state,
-- raw_records.backfill), which this file needs: it merges after them.
--
-- emr.findings: one row per rule hit on one primary observation, written by
-- the emr service's rules consumer. dedup_key (rule set version, rule,
-- observation) is unique, so a redelivered event makes no second finding
-- (A19). A finding is historical (stored, never an alert) when its value
-- is older than its category's freshness window, has no time (a date-only
-- value), or came from a first sync's backfill (A18); shadow when the rule
-- set isn't active for the hospital (K4). An amended result's new finding
-- names the one it replaces (supersedes); a result corrected to normal, or
-- retracted, withdraws its finding (A30). announced is the last status the
-- emr service published on emr.v1.<hid>.finding, so an unannounced change
-- is published again after a failure. A finding goes when its observation
-- does (the exclusion purge, design §10.6); the merge moves it with the
-- patient.
--
-- emr.breaker_trips: each time the per-source breaker paused a source
-- (design §12.4, A32); its id is the breaker_tripped signal's ref_id.
-- emr_pause_connection(id): the only way the emr service changes a
-- connection: it sets paused on an enabled connection of its own hospital.
--
-- emr_rule_set_activation: a hospital's activated rule set, version and
-- hash, written only by a platform admin (step-up, audited
-- emr.rule_set_switched) or removed to go back to shadow. With no row, or a
-- row naming another version or hash, every finding is shadow.
--
-- jobs.type gains emr_normalise and emr_rules (plan departure 12): EMR dead
-- letters reach the Failed jobs view as jobs rows (written by PR 8b).
--
-- emr_platform_health(): a paused source is errors at once (A42), not only
-- after three missed polls. emr_sources_status(): the caller's hospital's
-- switched-on sources, each current or behind (stale or paused, design
-- §15.3, §15.4), for core's sources_status (PR 8a).
--
-- Roles:
--   mhai_emr       its hospital's findings: SELECT, INSERT, and UPDATE of
--                  the status columns only; never DELETE. Its breaker trips:
--                  SELECT, INSERT. Its hospital's activation: SELECT.
--                  EXECUTE on emr_pause_connection().
--   mhai_emr_core  core's finding consumer (PR 8b): its hospital's findings
--                  and the evidence columns of observations, medications
--                  and patients, read only.
--   mhai_platform  the activation table (the platform route).
--   mhai_gateway   EXECUTE on emr_sources_status().

-- +goose Up

------------------------------------------------------------------------
-- Findings.
------------------------------------------------------------------------
CREATE TABLE emr.findings (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id            uuid NOT NULL,
    patient_id             uuid NOT NULL,
    connection_id          uuid,
    rule_set_version       text NOT NULL CHECK (rule_set_version ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    rule_set_sha256        bytea NOT NULL CHECK (octet_length(rule_set_sha256) = 32),
    rule_id                text NOT NULL CHECK (rule_id ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    kind                   text NOT NULL CHECK (kind IN ('threshold', 'source_flag', 'reference_range', 'delta', 'trend',
                                                         'score', 'combination')),
    category               text NOT NULL CHECK (category IN ('blood_chemistry', 'blood_count', 'kidney_function',
                                                             'blood_gases_lactate', 'heart_markers', 'blood_sugar',
                                                             'vital_signs', 'infection_markers')),
    severity               text NOT NULL CHECK (severity IN ('critical', 'urgent', 'info')),
    primary_observation_id uuid NOT NULL,
    observation_ids        uuid[] NOT NULL DEFAULT '{}' CHECK (cardinality(observation_ids) <= 20),
    medication_ids         uuid[] NOT NULL DEFAULT '{}' CHECK (cardinality(medication_ids) <= 20),
    causes                 jsonb NOT NULL DEFAULT '[]'::jsonb
                           CHECK (jsonb_typeof(causes) = 'array' AND jsonb_array_length(causes) <= 3
                                  AND octet_length(causes::text) <= 4096),
    score                  smallint CHECK (score BETWEEN 0 AND 30),
    scale                  smallint CHECK (scale IN (1, 2)),
    effective_at           timestamptz,
    effective_date         date,
    received_at            timestamptz NOT NULL,
    status                 text NOT NULL CHECK (status IN ('open', 'historical', 'superseded', 'withdrawn')),
    withdrawn_reason       text CHECK (withdrawn_reason IN ('source_amended', 'source_retracted')),
    shadow                 boolean NOT NULL,
    preliminary            boolean NOT NULL,
    arrived_late           boolean NOT NULL,
    standardised           boolean NOT NULL,
    supersedes             uuid,
    superseded_by          uuid,
    announced              text CHECK (announced IN ('open', 'superseded', 'withdrawn')),
    dedup_key              text NOT NULL CHECK (char_length(dedup_key) BETWEEN 1 AND 200),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT findings_hospital_id UNIQUE (hospital_id, id),
    -- A19: one finding per rule set version, rule and observation.
    CONSTRAINT findings_dedup UNIQUE (hospital_id, dedup_key),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, primary_observation_id) REFERENCES emr.observations (hospital_id, id) ON DELETE CASCADE,
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT findings_one_time CHECK ((effective_at IS NULL) <> (effective_date IS NULL)),
    -- No hour is invented for a date-only value: it is never fresh.
    CONSTRAINT findings_date_only_historical CHECK (effective_date IS NULL OR status = 'historical'),
    CONSTRAINT findings_withdrawn CHECK ((status = 'withdrawn') = (withdrawn_reason IS NOT NULL)),
    CONSTRAINT findings_superseded CHECK ((status = 'superseded') = (superseded_by IS NOT NULL)),
    CONSTRAINT findings_not_self CHECK (supersedes IS DISTINCT FROM id AND superseded_by IS DISTINCT FROM id),
    CONSTRAINT findings_score CHECK ((score IS NULL) = (scale IS NULL) AND (score IS NULL) = (kind <> 'score'))
);
ALTER TABLE emr.findings OWNER TO mhai_migrate;
ALTER TABLE emr.findings ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX findings_observation ON emr.findings (hospital_id, primary_observation_id);
CREATE INDEX findings_patient ON emr.findings (hospital_id, patient_id);
CREATE INDEX findings_breaker ON emr.findings (hospital_id, connection_id, created_at)
    WHERE severity = 'critical' AND status <> 'historical';
CREATE INDEX findings_unannounced ON emr.findings (hospital_id, created_at)
    WHERE status <> 'historical' AND announced IS DISTINCT FROM status;

GRANT SELECT ON emr.findings TO mhai_emr;
GRANT INSERT (hospital_id, patient_id, connection_id, rule_set_version, rule_set_sha256, rule_id, kind, category, severity,
              primary_observation_id, observation_ids, medication_ids, causes, score, scale, effective_at, effective_date,
              received_at, status, shadow, preliminary, arrived_late, standardised, supersedes, dedup_key)
    ON emr.findings TO mhai_emr;
GRANT UPDATE (status, withdrawn_reason, superseded_by, announced, updated_at) ON emr.findings TO mhai_emr;
CREATE POLICY emr_own ON emr.findings TO mhai_emr
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
-- The merge and exclusion definers run as the owner, bound to the caller's
-- hospital (as 00023's tables).
CREATE POLICY owner_scoped ON emr.findings TO mhai_migrate
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());

-- Core's finding consumer: the finding and its evidence, its hospital only.
GRANT USAGE ON SCHEMA emr TO mhai_emr_core;
GRANT SELECT (id, hospital_id, patient_id, connection_id, rule_set_version, rule_id, kind, category, severity,
              primary_observation_id, observation_ids, medication_ids, causes, score, scale, effective_at, effective_date,
              received_at, status, withdrawn_reason, shadow, preliminary, arrived_late, standardised, supersedes,
              superseded_by, created_at, updated_at)
    ON emr.findings TO mhai_emr_core;
GRANT SELECT (id, hospital_id, patient_id, encounter_id, category, loinc, local_display, value_num, comparator, value_text,
              unit_ucum, unit_source, value_si, ref_low, ref_high, ref_text, source_flag, status, effective_at,
              effective_date, issued_at, standardised, connection_id, superseded_by) ON emr.observations TO mhai_emr_core;
GRANT SELECT (id, hospital_id, patient_id, kind, atc, local_display, dose_value, dose_unit_ucum, route, frequency_text,
              status, start_at, end_at, standardised, connection_id, superseded_by) ON emr.medications TO mhai_emr_core;
GRANT SELECT (id, hospital_id, display_name, mrn, birth_date, sex, status, merged_into) ON emr.patients TO mhai_emr_core;
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['findings', 'observations', 'medications', 'patients'] LOOP
        EXECUTE format('CREATE POLICY core_read ON emr.%I FOR SELECT TO mhai_emr_core
                        USING (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The per-source breaker.
------------------------------------------------------------------------
CREATE TABLE emr.breaker_trips (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id   uuid NOT NULL,
    connection_id uuid NOT NULL,
    severity      text NOT NULL CHECK (severity IN ('critical', 'urgent', 'info')),
    window_start  timestamptz NOT NULL,
    window_count  integer NOT NULL CHECK (window_count >= 0),
    baseline      numeric NOT NULL CHECK (baseline >= 0),
    tripped_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.breaker_trips OWNER TO mhai_migrate;
ALTER TABLE emr.breaker_trips ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT ON emr.breaker_trips TO mhai_emr;
CREATE POLICY emr_own ON emr.breaker_trips TO mhai_emr
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());

-- The definer below updates as the owner, bound to the caller's hospital.
CREATE POLICY owner_pause ON emr_connections FOR UPDATE TO mhai_migrate
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

-- Design §12.4: pause an enabled source of the caller's hospital. Returns
-- whether this call paused it (false when already paused, off, or not the
-- caller's). Resuming is a platform admin's (plan Task 37).
-- +goose StatementBegin
CREATE FUNCTION emr_pause_connection(cid uuid) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    UPDATE public.emr_connections c SET paused = true, updated_at = now()
    WHERE c.id = cid AND c.hospital_id = public.mhai_current_hospital() AND c.enabled AND NOT c.paused;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_pause_connection(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_pause_connection(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_pause_connection(uuid) TO mhai_emr;

------------------------------------------------------------------------
-- A hospital's activated rule set.
------------------------------------------------------------------------
CREATE TABLE emr_rule_set_activation (
    hospital_id  uuid PRIMARY KEY REFERENCES hospitals (id),
    version      text NOT NULL CHECK (version ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    sha256       bytea NOT NULL CHECK (octet_length(sha256) = 32),
    activated_by uuid,
    activated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE emr_rule_set_activation OWNER TO mhai_migrate;
ALTER TABLE emr_rule_set_activation ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, DELETE ON emr_rule_set_activation TO mhai_platform;
GRANT UPDATE (version, sha256, activated_by, activated_at) ON emr_rule_set_activation TO mhai_platform;
CREATE POLICY platform_all ON emr_rule_set_activation TO mhai_platform USING (true) WITH CHECK (true);
GRANT SELECT (hospital_id, version, sha256) ON emr_rule_set_activation TO mhai_emr;
CREATE POLICY emr_own ON emr_rule_set_activation FOR SELECT TO mhai_emr USING (hospital_id = mhai_current_hospital());

-- The platform admin's switch is audited (step-up, no hospital on the row,
-- as every platform action).
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed', 'analytics.followed_up', 'analytics.follow_up_edited',
                           'analytics.follow_up_undone', 'emr.rule_set_switched')
                AND hospital_id IS NULL);

------------------------------------------------------------------------
-- EMR dead letters in the Failed jobs view (plan departure 12).
------------------------------------------------------------------------
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check,
    ADD CONSTRAINT jobs_type_check CHECK (type IN ('approve_correction', 'emr_normalise', 'emr_rules'));

------------------------------------------------------------------------
-- Sources: paused is behind at once.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
    RETURNS TABLE (connection_id uuid, health text, last_sync_at timestamptz, last_sync_ok boolean,
                   last_sync_records integer, errors_24h bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id,
           CASE WHEN c.enabled AND c.paused THEN 'errors'
                WHEN l.at IS NULL THEN 'never'
                WHEN EXISTS (SELECT 1 FROM (SELECT r.ok FROM emr.sync_runs r
                                            WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                                            ORDER BY r.finished_at DESC LIMIT 3) t
                             WHERE t.ok IS NOT TRUE) THEN 'errors'
                WHEN c.enabled AND NOT EXISTS (SELECT 1 FROM emr.sync_runs r
                                               WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL
                                                 AND r.finished_at >= now() - make_interval(secs => 3 * c.poll_seconds)) THEN 'errors'
                ELSE 'ok' END,
           l.at, l.ok, l.records,
           (SELECT count(*) FROM emr.connection_errors e
            WHERE e.connection_id = c.id AND e.at > now() - interval '24 hours')
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT r.finished_at AS at, r.ok, r.records FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                       ORDER BY r.finished_at DESC LIMIT 1) l ON true
$$;
-- +goose StatementEnd

-- Design §15.3, §15.4: the caller's hospital's switched-on sources, each
-- current or behind. Behind: paused by the breaker, or no successful
-- finished poll for three poll intervals. last_update_at is the last
-- successful poll, not the newest record. Labels and ids only.
-- +goose StatementBegin
CREATE FUNCTION emr_sources_status()
    RETURNS TABLE (connection_id uuid, source_label text, status text, last_update_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.source_label,
           CASE WHEN c.paused THEN 'behind'
                WHEN l.at IS NULL OR l.at < now() - make_interval(secs => 3 * c.poll_seconds) THEN 'behind'
                ELSE 'current' END,
           l.at
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT max(r.finished_at) AS at FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL) l ON true
    WHERE c.hospital_id = public.mhai_current_hospital() AND c.enabled
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_sources_status() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_sources_status() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_sources_status() TO mhai_gateway;

------------------------------------------------------------------------
-- The merge moves the merged patient's findings too.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_merge_patients(match_id uuid, keep text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid      uuid := public.mhai_current_hospital();
    m        record;
    kept     uuid;
    gone     uuid;
    n        integer;
    v_merge  uuid := gen_random_uuid();
BEGIN
    IF hid IS NULL OR keep IS NULL OR keep NOT IN ('a', 'b') THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    SELECT x.id, x.patient_a, x.patient_b INTO m FROM emr.possible_matches x
    WHERE x.id = match_id AND x.hospital_id = hid AND x.decision IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    IF keep = 'a' THEN kept := m.patient_a; gone := m.patient_b;
    ELSE kept := m.patient_b; gone := m.patient_a; END IF;
    PERFORM 1 FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid ORDER BY p.id FOR UPDATE;
    SELECT count(*) INTO n FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid AND p.status = 'active';
    IF n <> 2 THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    INSERT INTO emr.merges (id, hospital_id, match_id, kept_patient, merged_patient, merged_by)
    VALUES (v_merge, hid, m.id, kept, gone, NULLIF(current_setting('mhai.user_id', true), '')::uuid);
    UPDATE emr.patient_links l SET patient_id = kept, merge_id = v_merge
        WHERE l.hospital_id = hid AND l.patient_id = gone;
    UPDATE emr.encounters e SET patient_id = kept, merge_id = v_merge
        WHERE e.hospital_id = hid AND e.patient_id = gone;
    UPDATE emr.observations o SET patient_id = kept, merge_id = v_merge
        WHERE o.hospital_id = hid AND o.patient_id = gone;
    UPDATE emr.medications d SET patient_id = kept, merge_id = v_merge
        WHERE d.hospital_id = hid AND d.patient_id = gone;
    UPDATE emr.findings f SET patient_id = kept
        WHERE f.hospital_id = hid AND f.patient_id = gone;
    UPDATE emr.patients p SET status = 'merged_into', merged_into = kept, updated_at = now()
        WHERE p.hospital_id = hid AND p.id = gone;
    UPDATE emr.possible_matches x SET decision = 'confirmed', decided_at = now()
        WHERE x.hospital_id = hid AND x.id = m.id;
    RETURN v_merge;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_merge_patients(match_id uuid, keep text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid      uuid := public.mhai_current_hospital();
    m        record;
    kept     uuid;
    gone     uuid;
    n        integer;
    v_merge  uuid := gen_random_uuid();
BEGIN
    IF hid IS NULL OR keep IS NULL OR keep NOT IN ('a', 'b') THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    SELECT x.id, x.patient_a, x.patient_b INTO m FROM emr.possible_matches x
    WHERE x.id = match_id AND x.hospital_id = hid AND x.decision IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    IF keep = 'a' THEN kept := m.patient_a; gone := m.patient_b;
    ELSE kept := m.patient_b; gone := m.patient_a; END IF;
    PERFORM 1 FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid ORDER BY p.id FOR UPDATE;
    SELECT count(*) INTO n FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid AND p.status = 'active';
    IF n <> 2 THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    INSERT INTO emr.merges (id, hospital_id, match_id, kept_patient, merged_patient, merged_by)
    VALUES (v_merge, hid, m.id, kept, gone, NULLIF(current_setting('mhai.user_id', true), '')::uuid);
    UPDATE emr.patient_links l SET patient_id = kept, merge_id = v_merge
        WHERE l.hospital_id = hid AND l.patient_id = gone;
    UPDATE emr.encounters e SET patient_id = kept, merge_id = v_merge
        WHERE e.hospital_id = hid AND e.patient_id = gone;
    UPDATE emr.observations o SET patient_id = kept, merge_id = v_merge
        WHERE o.hospital_id = hid AND o.patient_id = gone;
    UPDATE emr.medications d SET patient_id = kept, merge_id = v_merge
        WHERE d.hospital_id = hid AND d.patient_id = gone;
    UPDATE emr.patients p SET status = 'merged_into', merged_into = kept, updated_at = now()
        WHERE p.hospital_id = hid AND p.id = gone;
    UPDATE emr.possible_matches x SET decision = 'confirmed', decided_at = now()
        WHERE x.hospital_id = hid AND x.id = m.id;
    RETURN v_merge;
END
$$;
-- +goose StatementEnd

DROP FUNCTION emr_sources_status();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
    RETURNS TABLE (connection_id uuid, health text, last_sync_at timestamptz, last_sync_ok boolean,
                   last_sync_records integer, errors_24h bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id,
           CASE WHEN l.at IS NULL THEN 'never'
                WHEN EXISTS (SELECT 1 FROM (SELECT r.ok FROM emr.sync_runs r
                                            WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                                            ORDER BY r.finished_at DESC LIMIT 3) t
                             WHERE t.ok IS NOT TRUE) THEN 'errors'
                WHEN c.enabled AND NOT EXISTS (SELECT 1 FROM emr.sync_runs r
                                               WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL
                                                 AND r.finished_at >= now() - make_interval(secs => 3 * c.poll_seconds)) THEN 'errors'
                ELSE 'ok' END,
           l.at, l.ok, l.records,
           (SELECT count(*) FROM emr.connection_errors e
            WHERE e.connection_id = c.id AND e.at > now() - interval '24 hours')
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT r.finished_at AS at, r.ok, r.records FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                       ORDER BY r.finished_at DESC LIMIT 1) l ON true
$$;
-- +goose StatementEnd

ALTER TABLE jobs DROP CONSTRAINT jobs_type_check,
    ADD CONSTRAINT jobs_type_check CHECK (type IN ('approve_correction'));

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

DROP TABLE emr_rule_set_activation;
DROP FUNCTION emr_pause_connection(uuid);
DROP POLICY owner_pause ON emr_connections;
DROP TABLE emr.breaker_trips;
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['observations', 'medications', 'patients'] LOOP
        EXECUTE format('DROP POLICY core_read ON emr.%I', t);
    END LOOP;
END
$$;
-- +goose StatementEnd
REVOKE SELECT ON emr.observations, emr.medications, emr.patients FROM mhai_emr_core;
-- The table takes its policies, indexes and grants with it.
DROP TABLE emr.findings;
REVOKE USAGE ON SCHEMA emr FROM mhai_emr_core;
