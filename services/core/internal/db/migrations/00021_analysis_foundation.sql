-- Patient Analysis, data foundation (analysis backend design §4, §7, §10,
-- §11; plan 2026-11-07-analysis-backend Task 2, "migration A"; ADR 0015).
--
-- NUMBERING IS FIXED: this is 00021. The EMR plan takes 00020 and merges
-- first; the later Plan 7c PR takes 00022 and up. Don't renumber it.
--
-- Analysis is OFF by default. With no analysis_switch row, a hospital's
-- position is 'off': ingest refuses it, and nothing here turns anything on.
-- Before any sign-off, the only writer that can raise a switch is the
-- demo-only CLI (core analysis seed-demo), with DEMO-SYNTHETIC references,
-- and a hospital under a demo switch never ingests a real consultation.
--
-- The demo (review items 1 and 2): hospitals.is_demo marks the synthetic
-- demo hospital. It is set only when a hospital is created (the
-- provisioning login's INSERT, core hospitals create --demo) and no role,
-- the owner included, can change it, with one exception: a hospital made
-- before the flag existed (the live demo's) is marked once through
-- mhai_hospital_mark_demo (core hospitals mark-demo, audited
-- hospital.marked_demo), never back, never a hospital with a real cloud
-- agreement, and never once any analysis switch has been signed off. Only a demo hospital's switch takes
-- DEMO-SYNTHETIC references, and only through mhai_analysis_demo_raise;
-- provisioning otherwise only lowers a switch and can't touch references.
-- analysis_switch.ever_signed_off is set by any SO- reference and is never
-- cleared; a row carrying it never takes demo references again.
--
-- Roles (cluster-wide: created if missing, refused if they have members,
-- kept on Down with every grant revoked, as 00017/00019):
--   - mhai_analysis_writer (NOLOGIN, NOINHERIT, no members): owns the job's
--     and the seed's functions. It holds column SELECT on exactly the
--     clinical columns ingest reads (never response_text, clinical_question,
--     citations_json, patient_data_json, medical_history, nor the free-text
--     diagnosis: ingest reads the ICD-10 code) and writes the analysis tables.
--   - mhai_analysis_reader (NOLOGIN, NOINHERIT, no members): owns the read
--     functions (here: mhai_analysis_position), with SELECT on the switch.
--   - mhai_analysis_job: the nightly run's role, reached by SET ROLE from
--     the core login (mhai_app WITH INHERIT FALSE), as 7c's
--     mhai_analytics_job: the only role with EXECUTE on the job's functions
--     (db.WithAnalysisJob, internal/analysis only; no route reaches it).
--
-- Tables (owner mhai_migrate, RLS enabled and forced, nothing for PUBLIC):
--   analysis_switch, analysis_sources, analysis_locations,
--   analysis_conditions, analysis_subjects, analysis_facts,
--   analysis_first_seen, analysis_periods, analysis_pseudonyms,
--   analysis_watermarks. Subjects, facts, first-seen, periods, pseudonyms
--   and watermarks are reached only through the definer functions; the
--   gateway may SELECT the dictionaries of its own hospital.
--
-- Definer functions: owner the writer or reader, search_path pinned to
-- pg_catalog, pg_temp, every relation and function written public.<name>
-- (a temporary table can't stand in for one), no PUBLIC execute.
--
-- The clock: the job's functions take the time they act at (p_now /
-- p_until) and refuse one later than the database's now(), so the Go side
-- injects its clock (tests pin dates) and nothing can freeze or ingest the
-- future.
--
-- Late facts (design §4.4, plan departure 8): a fact whose day falls in a
-- frozen block is stored with period_late and never counted. It may lower a
-- case's first day only while that first day is in a block that hasn't
-- frozen, recording first_late = true; a case is "new" in P when
-- first_day ∈ P AND NOT first_late. So no frozen block's "new" ever changes.
--
-- Periods: a block (7c's month-aligned week: 1-7, 8-14, 15-21, 22-end)
-- freezes at hospital-local midnight 3 days after its end; a month with its
-- last block; a quarter with its last month. Rows exist only once frozen,
-- and nobody may update or delete them.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analysis_writer', 'mhai_analysis_reader'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION', r);
        END IF;
        EXECUTE format('ALTER ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION', r);
        -- Nobody may already act as it: its grants open clinical rows.
        IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = r) THEN
            RAISE EXCEPTION USING ERRCODE = 'object_in_use',
                MESSAGE = format('00021: %s already has members; remove them first', r);
        END IF;
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analysis_job') THEN
        CREATE ROLE mhai_analysis_job NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    ALTER ROLE mhai_analysis_job NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    IF EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = 'mhai_analysis_job'::regrole
               AND m.member <> 'mhai_app'::regrole) THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00021: mhai_analysis_job has members other than mhai_app; remove them first';
    END IF;
END
$$;
-- +goose StatementEnd

GRANT mhai_analysis_job TO mhai_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_analysis_job, mhai_analysis_writer, mhai_analysis_reader;
-- The periods CHECK runs 7c's calendar twin as the inserting role.
GRANT EXECUTE ON FUNCTION mhai_reporting_period_ok(text, date, date) TO mhai_analysis_writer;

------------------------------------------------------------------------
-- The demo marker: set at creation only.
------------------------------------------------------------------------
ALTER TABLE hospitals ADD COLUMN is_demo boolean NOT NULL DEFAULT false;
GRANT INSERT (is_demo) ON hospitals TO mhai_provision;

-- +goose StatementBegin
CREATE FUNCTION mhai_hospital_demo_fixed() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- The one exception: false to true, inside mhai_hospital_mark_demo (the
    -- writer, with the hospital named in the transaction's setting).
    IF NOT OLD.is_demo AND NEW.is_demo AND current_user = 'mhai_analysis_writer'
       AND current_setting('mhai.demo_mark', true) = NEW.id::text THEN
        RETURN NEW;
    END IF;
    IF NEW.is_demo IS DISTINCT FROM OLD.is_demo THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'hospitals: is_demo is set when a hospital is created and never changes';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_hospital_demo_fixed() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_hospital_demo_fixed() FROM PUBLIC;
CREATE TRIGGER hospitals_demo_fixed BEFORE UPDATE ON hospitals
    FOR EACH ROW EXECUTE FUNCTION mhai_hospital_demo_fixed();

------------------------------------------------------------------------
-- The switch (design §7). A NULL hospital_id is the global row. The
-- effective position is the lower of the global and the hospital's; a
-- missing row is 'off'. References are DEMO-SYNTHETIC (the demo) or
-- SO-YYYY-NNNN (a sign-off), never mixed in one row, and a row that has
-- carried a real sign-off can't be given demo references.
------------------------------------------------------------------------
CREATE TABLE analysis_switch (
    hospital_id      uuid REFERENCES hospitals (id),
    position         text NOT NULL DEFAULT 'off' CHECK (position IN ('off', 'on')),
    signoff_clinical text CHECK (signoff_clinical ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    signoff_privacy  text CHECK (signoff_privacy ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    signoff_legal    text CHECK (signoff_legal ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    since            timestamptz NOT NULL DEFAULT now(),
    changed_by       uuid,
    ever_signed_off  boolean NOT NULL DEFAULT false,
    CONSTRAINT analysis_switch_one UNIQUE NULLS NOT DISTINCT (hospital_id),
    CONSTRAINT analysis_switch_signed CHECK (position = 'off'
        OR (signoff_clinical IS NOT NULL AND signoff_privacy IS NOT NULL AND signoff_legal IS NOT NULL)),
    CONSTRAINT analysis_switch_one_kind CHECK (
        (COALESCE(signoff_clinical, '') = 'DEMO-SYNTHETIC') = (COALESCE(signoff_privacy, '') = 'DEMO-SYNTHETIC')
        AND (COALESCE(signoff_clinical, '') = 'DEMO-SYNTHETIC') = (COALESCE(signoff_legal, '') = 'DEMO-SYNTHETIC'))
);
ALTER TABLE analysis_switch OWNER TO mhai_migrate;
ALTER TABLE analysis_switch ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analysis_switch FROM PUBLIC;

-- The guard runs as the writer (to read hospitals.is_demo), whoever writes.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_switch_guard() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.hospital_id IS DISTINCT FROM OLD.hospital_id THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'analysis_switch: a switch row never moves to another hospital';
        END IF;
        IF OLD.ever_signed_off AND NOT NEW.ever_signed_off THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'analysis_switch: ever_signed_off is never cleared';
        END IF;
    END IF;
    IF NEW.signoff_clinical ~ '^SO-' OR NEW.signoff_privacy ~ '^SO-' OR NEW.signoff_legal ~ '^SO-' THEN
        NEW.ever_signed_off := true;
    END IF;
    IF 'DEMO-SYNTHETIC' IN (COALESCE(NEW.signoff_clinical, ''), COALESCE(NEW.signoff_privacy, ''), COALESCE(NEW.signoff_legal, '')) THEN
        IF NEW.ever_signed_off THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'analysis_switch: a switch that was ever signed off never takes demo references';
        END IF;
        IF NEW.hospital_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.hospitals h
                                                       WHERE h.id = NEW.hospital_id AND h.is_demo) THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'analysis_switch: only a hospital created as the demo takes demo references';
        END IF;
    END IF;
    NEW.since := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_analysis_switch_guard() OWNER TO mhai_analysis_writer;
REVOKE ALL ON FUNCTION mhai_analysis_switch_guard() FROM PUBLIC;
CREATE TRIGGER analysis_switch_guard BEFORE INSERT OR UPDATE ON analysis_switch
    FOR EACH ROW EXECUTE FUNCTION mhai_analysis_switch_guard();

-- The platform (Plan 11b's flow): read; write real references or off, never
-- demo references. The provisioning CLI: read, and lower a switch (UPDATE
-- of position to 'off' only, never a reference); it raises the demo only
-- through mhai_analysis_demo_raise. Nobody but the guard writes
-- ever_signed_off, and nobody deletes a row. The gateway reads its
-- position through the function.
GRANT SELECT, INSERT ON analysis_switch TO mhai_platform;
GRANT UPDATE (position, signoff_clinical, signoff_privacy, signoff_legal, changed_by)
    ON analysis_switch TO mhai_platform;
GRANT SELECT ON analysis_switch TO mhai_provision;
GRANT UPDATE (position) ON analysis_switch TO mhai_provision;
GRANT SELECT ON analysis_switch TO mhai_analysis_reader, mhai_analysis_writer;
GRANT INSERT (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal, changed_by),
    UPDATE (position, signoff_clinical, signoff_privacy, signoff_legal, changed_by)
    ON analysis_switch TO mhai_analysis_writer;
CREATE POLICY platform_switch ON analysis_switch TO mhai_platform USING (true)
    WITH CHECK (COALESCE(signoff_clinical, '') <> 'DEMO-SYNTHETIC'
                AND COALESCE(signoff_privacy, '') <> 'DEMO-SYNTHETIC'
                AND COALESCE(signoff_legal, '') <> 'DEMO-SYNTHETIC');
CREATE POLICY provision_switch ON analysis_switch FOR UPDATE TO mhai_provision USING (true)
    WITH CHECK (position = 'off');
CREATE POLICY provision_read ON analysis_switch FOR SELECT TO mhai_provision USING (true);
CREATE POLICY analysis_read ON analysis_switch FOR SELECT TO mhai_analysis_reader, mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_insert ON analysis_switch FOR INSERT TO mhai_analysis_writer WITH CHECK (true);
CREATE POLICY analysis_writer_update ON analysis_switch FOR UPDATE TO mhai_analysis_writer USING (true) WITH CHECK (true);

------------------------------------------------------------------------
-- Dictionaries (design §4.3): per hospital, opaque ids, the hospital's own
-- labels. A condition with no ICD-10 code (and no name) is the hospital's
-- "Not standardised" entry for its source.
------------------------------------------------------------------------
CREATE TABLE analysis_sources (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    id          uuid NOT NULL DEFAULT gen_random_uuid(),
    label       text NOT NULL CHECK (label ~ '^[^[:cntrl:]]{1,80}$'
                                     AND label !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'),
    PRIMARY KEY (hospital_id, id)
);
CREATE TABLE analysis_locations (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    id          uuid NOT NULL DEFAULT gen_random_uuid(),
    label       text NOT NULL CHECK (label ~ '^[^[:cntrl:]]{1,80}$'
                                     AND label !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'),
    setting     text CHECK (setting IN ('admitted', 'outpatient', 'emergency')),
    PRIMARY KEY (hospital_id, id)
);
CREATE TABLE analysis_conditions (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    id          uuid NOT NULL DEFAULT gen_random_uuid(),
    icd10       text CHECK (icd10 ~ '^[A-Z][0-9]{2}(\.[0-9A-Z]{1,4})?$'),
    name        text CHECK (name ~ '^[^[:cntrl:]]{1,120}$'
                            AND name !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'),
    source_id   uuid,
    PRIMARY KEY (hospital_id, id),
    CONSTRAINT analysis_conditions_named CHECK ((icd10 IS NULL) = (name IS NULL)),
    FOREIGN KEY (hospital_id, source_id) REFERENCES analysis_sources (hospital_id, id)
);
CREATE UNIQUE INDEX analysis_conditions_code_idx ON analysis_conditions (hospital_id, icd10) WHERE icd10 IS NOT NULL;
CREATE UNIQUE INDEX analysis_conditions_unstandardised_idx ON analysis_conditions (hospital_id, source_id)
    NULLS NOT DISTINCT WHERE icd10 IS NULL;

------------------------------------------------------------------------
-- Subjects, facts and first-seen (design §4.3, §4.4).
------------------------------------------------------------------------
CREATE TABLE analysis_subjects (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    subject_id  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ref_hmac    bytea NOT NULL CHECK (octet_length(ref_hmac) = 32),
    ref_enc     bytea CHECK (octet_length(ref_enc) BETWEEN 29 AND 1024),
    created_day date NOT NULL,
    UNIQUE (hospital_id, ref_hmac),
    UNIQUE (hospital_id, subject_id)
);

CREATE TABLE analysis_facts (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    hospital_id    uuid NOT NULL,
    subject_id     uuid NOT NULL,
    day            date NOT NULL,
    kind           text NOT NULL CHECK (kind IN ('consultation', 'diagnosis', 'finding', 'alert')),
    condition_id   uuid,
    age_band       text CHECK (age_band IN ('0-17', '18-39', '40-64', '65-79', '80+')),
    sex            text NOT NULL CHECK (sex IN ('female', 'male', 'other', 'not_recorded')),
    setting        text CHECK (setting IN ('admitted', 'outpatient', 'emergency')),
    location_id    uuid,
    specialty      text CHECK (specialty ~ '^[a-z][a-z_]{1,39}$'),
    source_id      uuid,
    -- consultation
    urgency        text CHECK (urgency IN ('routine', 'urgent', 'emergency')),
    confidence     text CHECK (confidence IN ('high', 'caution', 'review', 'failed')),
    rating         text CHECK (rating IN ('agreed', 'not_agreed')),
    correction     text CHECK (correction IN ('waiting', 'approved', 'rejected')),
    -- finding
    flag           text CHECK (flag IN ('low', 'high', 'abnormal')),
    -- alert
    severity       text CHECK (severity IN ('critical', 'urgent', 'info')),
    ack_minutes    integer CHECK (ack_minutes BETWEEN 0 AND 525600),
    dismiss_reason text CHECK (dismiss_reason IN ('not_relevant', 'already_known', 'wrong_patient', 'other')),
    ingested_at    timestamptz NOT NULL DEFAULT now(),
    period_late    boolean NOT NULL,
    CONSTRAINT analysis_facts_consultation CHECK (kind = 'consultation'
        OR (urgency IS NULL AND confidence IS NULL AND rating IS NULL AND correction IS NULL)),
    CONSTRAINT analysis_facts_finding CHECK (kind = 'finding' OR flag IS NULL),
    CONSTRAINT analysis_facts_alert CHECK (kind = 'alert'
        OR (severity IS NULL AND ack_minutes IS NULL AND dismiss_reason IS NULL)),
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id),
    FOREIGN KEY (hospital_id, condition_id) REFERENCES analysis_conditions (hospital_id, id),
    FOREIGN KEY (hospital_id, location_id) REFERENCES analysis_locations (hospital_id, id),
    FOREIGN KEY (hospital_id, source_id) REFERENCES analysis_sources (hospital_id, id)
);
CREATE INDEX analysis_facts_day_idx ON analysis_facts (hospital_id, day);
CREATE INDEX analysis_facts_subject_idx ON analysis_facts (hospital_id, subject_id);

CREATE TABLE analysis_first_seen (
    hospital_id  uuid NOT NULL,
    subject_id   uuid NOT NULL,
    condition_id uuid NOT NULL,
    first_day    date NOT NULL,
    first_late   boolean NOT NULL,
    PRIMARY KEY (hospital_id, subject_id, condition_id),
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id),
    FOREIGN KEY (hospital_id, condition_id) REFERENCES analysis_conditions (hospital_id, id)
);

------------------------------------------------------------------------
-- Frozen periods (design §5.4 D1-D3, §11). A row exists only once frozen.
------------------------------------------------------------------------
CREATE TABLE analysis_periods (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id    uuid NOT NULL REFERENCES hospitals (id),
    level          text NOT NULL CHECK (level IN ('week', 'month', 'quarter')),
    start          date NOT NULL,
    "end"          date NOT NULL,
    frozen_at      timestamptz NOT NULL DEFAULT now(),
    engine_version text NOT NULL CHECK (engine_version ~ '^[a-z0-9][a-z0-9.-]{0,15}$'),
    UNIQUE (hospital_id, level, start),
    UNIQUE (hospital_id, id),
    CHECK (public.mhai_reporting_period_ok(level, start, "end" + 1))
);

------------------------------------------------------------------------
-- Pseudonyms (design §4.5): filled in PR 5. Only the writer's sweep reads
-- it (a subject a pid cites is kept).
------------------------------------------------------------------------
CREATE TABLE analysis_pseudonyms (
    hospital_id uuid NOT NULL,
    side        text NOT NULL CHECK (side IN ('hospital', 'platform')),
    scope       uuid,
    period_id   uuid NOT NULL,
    subject_id  uuid NOT NULL,
    pid         text NOT NULL CHECK (pid ~ '^P-[A-Z2-7]{8}$'),
    UNIQUE NULLS NOT DISTINCT (hospital_id, side, scope, period_id, subject_id),
    UNIQUE (hospital_id, side, period_id, pid),
    FOREIGN KEY (hospital_id, period_id) REFERENCES analysis_periods (hospital_id, id),
    FOREIGN KEY (hospital_id, subject_id) REFERENCES analysis_subjects (hospital_id, subject_id)
);

------------------------------------------------------------------------
-- The nightly run's bookkeeping: how far ingest has read, and the last
-- hospital day the run finished.
------------------------------------------------------------------------
CREATE TABLE analysis_watermarks (
    hospital_id    uuid PRIMARY KEY REFERENCES hospitals (id),
    ingested_until timestamptz,
    last_run_day   date
);

-- Ownership, RLS, grants.
ALTER TABLE analysis_sources OWNER TO mhai_migrate;
ALTER TABLE analysis_locations OWNER TO mhai_migrate;
ALTER TABLE analysis_conditions OWNER TO mhai_migrate;
ALTER TABLE analysis_subjects OWNER TO mhai_migrate;
ALTER TABLE analysis_facts OWNER TO mhai_migrate;
ALTER TABLE analysis_first_seen OWNER TO mhai_migrate;
ALTER TABLE analysis_periods OWNER TO mhai_migrate;
ALTER TABLE analysis_pseudonyms OWNER TO mhai_migrate;
ALTER TABLE analysis_watermarks OWNER TO mhai_migrate;
ALTER TABLE analysis_sources ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_locations ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_conditions ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_subjects ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_facts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_first_seen ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_periods ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_pseudonyms ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE analysis_watermarks ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analysis_sources, analysis_locations, analysis_conditions, analysis_subjects, analysis_facts,
    analysis_first_seen, analysis_periods, analysis_pseudonyms, analysis_watermarks FROM PUBLIC;

-- The gateway: its own hospital's dictionaries, read only.
GRANT SELECT ON analysis_sources, analysis_locations, analysis_conditions TO mhai_gateway;
CREATE POLICY gateway_read ON analysis_sources FOR SELECT TO mhai_gateway USING (hospital_id = mhai_current_hospital());
CREATE POLICY gateway_read ON analysis_locations FOR SELECT TO mhai_gateway USING (hospital_id = mhai_current_hospital());
CREATE POLICY gateway_read ON analysis_conditions FOR SELECT TO mhai_gateway USING (hospital_id = mhai_current_hospital());

-- The writer, inside its functions only (each takes the hospital as an
-- argument and filters on it): insert-only where frozen means frozen.
GRANT SELECT, INSERT ON analysis_sources, analysis_locations, analysis_conditions TO mhai_analysis_writer;
GRANT SELECT, INSERT, DELETE ON analysis_subjects, analysis_facts TO mhai_analysis_writer;
GRANT SELECT, INSERT, DELETE ON analysis_first_seen TO mhai_analysis_writer;
GRANT UPDATE (first_day, first_late) ON analysis_first_seen TO mhai_analysis_writer;
GRANT SELECT, INSERT ON analysis_periods TO mhai_analysis_writer;
GRANT SELECT ON analysis_pseudonyms TO mhai_analysis_writer; -- the sweep keeps a subject a pid cites
GRANT SELECT, INSERT ON analysis_watermarks TO mhai_analysis_writer;
GRANT UPDATE (ingested_until, last_run_day) ON analysis_watermarks TO mhai_analysis_writer;
CREATE POLICY analysis_writer ON analysis_sources TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_locations TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_conditions TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_subjects TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_facts TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_first_seen TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_periods TO mhai_analysis_writer USING (true) WITH CHECK (true);
CREATE POLICY analysis_writer ON analysis_pseudonyms FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON analysis_watermarks TO mhai_analysis_writer USING (true) WITH CHECK (true);

-- What ingest reads (design §4.1, §10): columns, never text.
GRANT SELECT (id, hospital_id, created_at, urgency) ON clinical_queries TO mhai_analysis_writer;
GRANT SELECT (query_id, hospital_id, icd10_code, hga_decision, trust_score, degraded, created_at)
    ON clinical_decisions TO mhai_analysis_writer;
GRANT SELECT (query_id, hospital_id, action, status, created_at) ON feedback_events TO mhai_analysis_writer;
GRANT SELECT (id, status, time_zone, is_demo) ON hospitals TO mhai_analysis_writer;
-- mhai_hospital_mark_demo only: is_demo, and the cloud agreement it checks.
GRANT UPDATE (is_demo) ON hospitals TO mhai_analysis_writer;
CREATE POLICY analysis_writer_mark ON hospitals FOR UPDATE TO mhai_analysis_writer USING (true) WITH CHECK (true);
GRANT SELECT (hospital_id, agreement_ref) ON hospital_model_policy TO mhai_analysis_writer;
CREATE POLICY analysis_writer_read ON hospital_model_policy FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_read ON clinical_queries FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_read ON clinical_decisions FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_read ON feedback_events FOR SELECT TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer_read ON hospitals FOR SELECT TO mhai_analysis_writer USING (true);

------------------------------------------------------------------------
-- mhai_analysis_position(hospital): the effective position, the lower of
-- the global and the hospital's rows; 'off' when either is missing. A
-- caller acting as mhai_gateway, or any caller with a hospital set, learns
-- only its own hospital's position (any other is 'off').
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_position(p_hospital uuid) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE
        WHEN p_hospital IS NULL THEN 'off'
        WHEN (pg_catalog.current_setting('role') = 'mhai_gateway' OR public.mhai_current_hospital() IS NOT NULL)
             AND p_hospital IS DISTINCT FROM public.mhai_current_hospital() THEN 'off'
        WHEN EXISTS (SELECT 1 FROM public.analysis_switch g WHERE g.hospital_id IS NULL AND g.position = 'on')
             AND EXISTS (SELECT 1 FROM public.analysis_switch s WHERE s.hospital_id = p_hospital AND s.position = 'on')
            THEN 'on'
        ELSE 'off' END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_analysis_position(uuid) OWNER TO mhai_analysis_reader;
REVOKE ALL ON FUNCTION mhai_analysis_position(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_position(uuid)
    TO mhai_gateway, mhai_auth, mhai_platform, mhai_provision, mhai_analysis_job, mhai_analysis_writer;

------------------------------------------------------------------------
-- Internal helpers of the writer's functions (no EXECUTE for anyone but
-- their owner).
------------------------------------------------------------------------

-- mhai_analysis_demo(hospital): the global or the hospital's row carries
-- demo references.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_demo(p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.analysis_switch s
                   WHERE (s.hospital_id IS NULL OR s.hospital_id = p_hospital)
                     AND 'DEMO-SYNTHETIC' IN (s.signoff_clinical, s.signoff_privacy, s.signoff_legal))
$$;
-- +goose StatementEnd

-- mhai_analysis_zone(hospital): the hospital's time zone; refuses an
-- unknown hospital.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_zone(p_hospital uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_tz text;
BEGIN
    SELECT h.time_zone INTO v_tz FROM public.hospitals h WHERE h.id = p_hospital;
    IF v_tz IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: no such hospital';
    END IF;
    RETURN v_tz;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_block_frozen(hospital, day): day lies in a frozen block.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_block_frozen(p_hospital uuid, p_day date) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.analysis_periods p
                   WHERE p.hospital_id = p_hospital AND p.level = 'week'
                     AND p.start <= p_day AND p."end" >= p_day)
$$;
-- +goose StatementEnd

-- mhai_analysis_put_facts(hospital, at, subjects, seed): the one insert
-- path of ingest and the seed. subjects is a JSON array of objects:
--   ingest: {ref_hmac: 64 hex, ref_enc: base64, facts: [...]};
--   seed:   {facts: [...]} (a random ref_hmac, no ref_enc).
-- A fact is an object with only these keys, each a code (or null), never
-- free text: day (YYYY-MM-DD, within the last 25 months, not after the
-- hospital day of p_at), kind, condition, age_band, sex, setting, location,
-- specialty, source, urgency, confidence, rating, correction, flag,
-- severity, ack_minutes, dismiss_reason. The table's CHECKs refuse an
-- unknown code. Returns the number of late facts.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_put_facts(p_hospital uuid, p_at timestamptz, p_subjects jsonb, p_seed boolean)
    RETURNS integer
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_today   date;
    v_min     date;
    v_subject jsonb;
    v_fact    jsonb;
    v_id      uuid;
    v_hmac    bytea;
    v_enc     bytea;
    v_day     date;
    v_late    boolean;
    v_cond    uuid;
    v_count   integer := 0;
    v_key     text;
    v_type    text;
BEGIN
    v_today := (p_at AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date;
    v_min := (v_today - interval '25 months')::date;
    IF jsonb_typeof(p_subjects) IS DISTINCT FROM 'array' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: subjects must be an array';
    END IF;
    FOR v_subject IN SELECT e.value FROM jsonb_array_elements(p_subjects) e LOOP
        IF jsonb_typeof(v_subject) IS DISTINCT FROM 'object'
           OR jsonb_typeof(v_subject -> 'facts') IS DISTINCT FROM 'array'
           OR EXISTS (SELECT 1 FROM jsonb_object_keys(v_subject) k
                      WHERE k NOT IN ('facts', 'ref_hmac', 'ref_enc') OR (p_seed AND k <> 'facts')) THEN
            RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a subject has an unknown key';
        END IF;
        IF p_seed THEN
            v_hmac := sha256(convert_to(gen_random_uuid()::text || gen_random_uuid()::text, 'UTF8'));
            v_enc := NULL;
        ELSE
            IF jsonb_typeof(v_subject -> 'ref_hmac') IS DISTINCT FROM 'string'
               OR (v_subject ->> 'ref_hmac') !~ '^[0-9a-f]{64}$'
               OR jsonb_typeof(v_subject -> 'ref_enc') IS DISTINCT FROM 'string'
               OR (v_subject ->> 'ref_enc') !~ '^[A-Za-z0-9+/]+={0,2}$'
               OR length(v_subject ->> 'ref_enc') NOT BETWEEN 40 AND 1400 THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a subject reference is malformed';
            END IF;
            v_hmac := decode(v_subject ->> 'ref_hmac', 'hex');
            v_enc := decode(v_subject ->> 'ref_enc', 'base64');
        END IF;
        v_id := NULL;
        INSERT INTO public.analysis_subjects (hospital_id, ref_hmac, ref_enc, created_day)
            VALUES (p_hospital, v_hmac, v_enc, v_today)
            ON CONFLICT (hospital_id, ref_hmac) DO NOTHING
            RETURNING subject_id INTO v_id;
        IF v_id IS NULL THEN
            SELECT s.subject_id INTO v_id FROM public.analysis_subjects s
            WHERE s.hospital_id = p_hospital AND s.ref_hmac = v_hmac;
        END IF;

        FOR v_fact IN SELECT e.value FROM jsonb_array_elements(v_subject -> 'facts') e LOOP
            IF jsonb_typeof(v_fact) IS DISTINCT FROM 'object' THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact must be an object';
            END IF;
            FOR v_key, v_type IN SELECT k, jsonb_typeof(v_fact -> k) FROM jsonb_object_keys(v_fact) k LOOP
                IF v_key NOT IN ('day', 'kind', 'condition', 'age_band', 'sex', 'setting', 'location', 'specialty',
                                 'source', 'urgency', 'confidence', 'rating', 'correction', 'flag', 'severity',
                                 'ack_minutes', 'dismiss_reason') THEN
                    RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact has an unknown key';
                END IF;
                IF NOT (v_type = 'null' OR (v_key = 'ack_minutes' AND v_type = 'number')
                        OR (v_key <> 'ack_minutes' AND v_type = 'string')) THEN
                    RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact value has the wrong type';
                END IF;
            END LOOP;
            IF (v_fact ->> 'day') IS NULL OR (v_fact ->> 'day') !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact day is malformed';
            END IF;
            v_day := (v_fact ->> 'day')::date;
            IF v_day < v_min OR v_day > v_today THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
                    MESSAGE = 'analysis: a fact day is outside the last 25 months';
            END IF;
            IF (v_fact ->> 'condition') IS NOT NULL AND (v_fact ->> 'condition') !~ '^[0-9a-f-]{36}$' THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact id is malformed';
            END IF;
            IF ((v_fact ->> 'location') IS NOT NULL AND (v_fact ->> 'location') !~ '^[0-9a-f-]{36}$')
               OR ((v_fact ->> 'source') IS NOT NULL AND (v_fact ->> 'source') !~ '^[0-9a-f-]{36}$')
               OR ((v_fact ->> 'ack_minutes') IS NOT NULL AND (v_fact ->> 'ack_minutes') !~ '^[0-9]{1,6}$') THEN
                RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a fact id is malformed';
            END IF;
            v_cond := (v_fact ->> 'condition')::uuid;
            v_late := public.mhai_analysis_block_frozen(p_hospital, v_day);
            INSERT INTO public.analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, setting,
                location_id, specialty, source_id, urgency, confidence, rating, correction, flag, severity,
                ack_minutes, dismiss_reason, period_late)
            VALUES (p_hospital, v_id, v_day, v_fact ->> 'kind', v_cond, v_fact ->> 'age_band',
                COALESCE(v_fact ->> 'sex', 'not_recorded'), v_fact ->> 'setting', (v_fact ->> 'location')::uuid,
                v_fact ->> 'specialty', (v_fact ->> 'source')::uuid, v_fact ->> 'urgency', v_fact ->> 'confidence',
                v_fact ->> 'rating', v_fact ->> 'correction', v_fact ->> 'flag', v_fact ->> 'severity',
                (v_fact ->> 'ack_minutes')::integer, v_fact ->> 'dismiss_reason', v_late);
            IF v_late THEN
                v_count := v_count + 1;
            END IF;
            IF v_cond IS NOT NULL THEN
                -- First seen: an earlier day replaces the first day only
                -- while that first day's block hasn't frozen, so no frozen
                -- block's "new" ever changes; a late day is recorded as late
                -- and makes the case new nowhere.
                INSERT INTO public.analysis_first_seen AS fs (hospital_id, subject_id, condition_id, first_day, first_late)
                    VALUES (p_hospital, v_id, v_cond, v_day, v_late)
                    ON CONFLICT (hospital_id, subject_id, condition_id) DO UPDATE
                    SET first_day = EXCLUDED.first_day, first_late = EXCLUDED.first_late
                    WHERE EXCLUDED.first_day < fs.first_day
                      AND NOT public.mhai_analysis_block_frozen(fs.hospital_id, fs.first_day);
            END IF;
        END LOOP;
    END LOOP;
    RETURN v_count;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The job's functions (EXECUTE to mhai_analysis_job only).
------------------------------------------------------------------------

-- mhai_analysis_ingest_source(hospital, after, until): the coded
-- consultation rows committed in (after, until], with the patient
-- reference Go turns into ref_hmac and ref_enc (plan departure 4). It
-- refuses a hospital that is off, any hospital under a demo switch (no real
-- consultation is ever ingested under one), and the future. Consultations
-- carry no patient reference yet (design §4.2), so it returns no rows until
-- PR 10 adds one.
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

-- mhai_analysis_ingest(hospital, until, facts): writes the facts Go built
-- from ingest_source ({"subjects": [...]}, see put_facts) and advances the
-- watermark to until. Refuses a hospital that is off or under a demo
-- switch, the future, and a watermark moving back. Returns the late count.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_ingest(p_hospital uuid, p_until timestamptz, p_facts jsonb) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_mark timestamptz;
    v_late integer;
BEGIN
    IF p_hospital IS NULL OR p_until IS NULL OR p_until > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    IF public.mhai_analysis_position(p_hospital) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the switch is off for this hospital';
    END IF;
    IF public.mhai_analysis_demo(p_hospital) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a demo switch never ingests consultations';
    END IF;
    IF jsonb_typeof(p_facts) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(p_facts) k WHERE k <> 'subjects') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: facts must be {"subjects": [...]}';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.freeze'), hashtext(p_hospital::text));
    SELECT w.ingested_until INTO v_mark FROM public.analysis_watermarks w WHERE w.hospital_id = p_hospital;
    IF v_mark > p_until THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the watermark never moves back';
    END IF;
    v_late := public.mhai_analysis_put_facts(p_hospital, p_until, COALESCE(p_facts -> 'subjects', '[]'::jsonb), false);
    INSERT INTO public.analysis_watermarks AS w (hospital_id, ingested_until) VALUES (p_hospital, p_until)
        ON CONFLICT (hospital_id) DO UPDATE SET ingested_until = EXCLUDED.ingested_until;
    RETURN v_late;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_freeze(hospital, now): under a per-hospital advisory lock,
-- freezes every block whose end + 1 day + 3 days (hospital-local midnight)
-- has passed by p_now, then every month whose blocks are all frozen, then
-- every quarter whose months are, from the quarter 24 months back. Each
-- period is frozen once, with engine_version from mhai.analysis.engine.
-- Returns the number frozen.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_freeze(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_tz     text;
    v_engine text;
    v_today  date;
    v_start  date;
    v_week   date;
    v_end    date;
    v_n      integer := 0;
    v_rows   integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    IF public.mhai_analysis_position(p_hospital) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the switch is off for this hospital';
    END IF;
    v_engine := current_setting('mhai.analysis.engine', true);
    IF v_engine IS NULL OR v_engine !~ '^[a-z0-9][a-z0-9.-]{0,15}$' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: no engine version set';
    END IF;
    v_tz := public.mhai_analysis_zone(p_hospital);
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.freeze'), hashtext(p_hospital::text));
    v_today := (p_now AT TIME ZONE v_tz)::date;
    v_start := date_trunc('quarter', v_today - interval '24 months')::date;

    v_week := v_start;
    LOOP
        v_end := CASE WHEN extract(day FROM v_week) = 22
                      THEN (date_trunc('month', v_week) + interval '1 month')::date - 1
                      ELSE v_week + 6 END;
        EXIT WHEN ((v_end + 4)::timestamp AT TIME ZONE v_tz) > p_now;
        INSERT INTO public.analysis_periods (hospital_id, level, start, "end", engine_version)
            VALUES (p_hospital, 'week', v_week, v_end, v_engine)
            ON CONFLICT (hospital_id, level, start) DO NOTHING;
        GET DIAGNOSTICS v_rows = ROW_COUNT;
        v_n := v_n + v_rows;
        v_week := v_end + 1;
    END LOOP;

    -- Months whose four blocks are frozen.
    INSERT INTO public.analysis_periods (hospital_id, level, start, "end", engine_version)
    SELECT p_hospital, 'month', w.start - 21, w."end", v_engine
    FROM public.analysis_periods w
    WHERE w.hospital_id = p_hospital AND w.level = 'week' AND extract(day FROM w.start) = 22
      AND (SELECT count(*) FROM public.analysis_periods c
           WHERE c.hospital_id = p_hospital AND c.level = 'week'
             AND c.start >= w.start - 21 AND c.start <= w.start) = 4
    ON CONFLICT (hospital_id, level, start) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    v_n := v_n + v_rows;

    -- Quarters whose three months are frozen.
    INSERT INTO public.analysis_periods (hospital_id, level, start, "end", engine_version)
    SELECT p_hospital, 'quarter', (m.start - interval '2 months')::date, m."end", v_engine
    FROM public.analysis_periods m
    WHERE m.hospital_id = p_hospital AND m.level = 'month' AND extract(month FROM m.start) IN (3, 6, 9, 12)
      AND (SELECT count(*) FROM public.analysis_periods c
           WHERE c.hospital_id = p_hospital AND c.level = 'month'
             AND c.start >= (m.start - interval '2 months')::date AND c.start <= m.start) = 3
    ON CONFLICT (hospital_id, level, start) DO NOTHING;
    GET DIAGNOSTICS v_rows = ROW_COUNT;
    v_n := v_n + v_rows;
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_sweep(hospital, now): retention (design §4.3). Deletes
-- facts and first-seen rows older than 25 months (hospital days), then the
-- subjects nothing cites any more that are as old. Runs whatever the
-- switch: switched-off facts age out too. Returns the facts deleted.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
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

-- mhai_analysis_due(now): the hospitals the nightly run serves now: those
-- switched on, or holding facts (for the sweep), whose local time is past
-- midnight + 30 minutes and that haven't had a run this hospital day. With
-- each: whether it is on, whether its switch is a demo, and its watermark.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_due(p_now timestamptz)
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

-- mhai_analysis_ran(hospital, now): records the hospital day of a finished
-- run.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_ran(p_hospital uuid, p_now timestamptz) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    INSERT INTO public.analysis_watermarks AS w (hospital_id, last_run_day)
        VALUES (p_hospital, (p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date)
        ON CONFLICT (hospital_id) DO UPDATE SET last_run_day = EXCLUDED.last_run_day
        WHERE w.last_run_day IS NULL OR w.last_run_day < EXCLUDED.last_run_day;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The seed (EXECUTE to mhai_provision only): the demo hospital's
-- synthetic cohort. p_seed is {conditions: [{id, icd10, name}], locations:
-- [{id, label, setting}], sources: [{id, label}], subjects: [{facts}]}.
-- Only for a hospital that is on with demo references on its own row, and
-- only once. Returns the subjects written.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_seed(p_hospital uuid, p_now timestamptz, p_seed jsonb) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_n integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.hospitals h WHERE h.id = p_hospital AND h.is_demo)
       OR public.mhai_analysis_position(p_hospital) <> 'on'
       OR NOT EXISTS (SELECT 1 FROM public.analysis_switch s
                      WHERE s.hospital_id = p_hospital AND s.signoff_clinical = 'DEMO-SYNTHETIC'
                        AND s.signoff_privacy = 'DEMO-SYNTHETIC' AND s.signoff_legal = 'DEMO-SYNTHETIC') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'analysis: the seed is only for a hospital switched on for the demo';
    END IF;
    IF jsonb_typeof(p_seed) IS DISTINCT FROM 'object'
       OR EXISTS (SELECT 1 FROM jsonb_object_keys(p_seed) k WHERE k NOT IN ('conditions', 'locations', 'sources', 'subjects')) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: the seed has an unknown key';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.freeze'), hashtext(p_hospital::text));
    IF EXISTS (SELECT 1 FROM public.analysis_subjects s WHERE s.hospital_id = p_hospital) THEN
        RAISE EXCEPTION USING ERRCODE = 'unique_violation', MESSAGE = 'analysis: this hospital is seeded already';
    END IF;
    INSERT INTO public.analysis_sources (hospital_id, id, label)
    SELECT p_hospital, (e ->> 'id')::uuid, e ->> 'label'
    FROM jsonb_array_elements(COALESCE(p_seed -> 'sources', '[]'::jsonb)) e;
    INSERT INTO public.analysis_locations (hospital_id, id, label, setting)
    SELECT p_hospital, (e ->> 'id')::uuid, e ->> 'label', e ->> 'setting'
    FROM jsonb_array_elements(COALESCE(p_seed -> 'locations', '[]'::jsonb)) e;
    INSERT INTO public.analysis_conditions (hospital_id, id, icd10, name, source_id)
    SELECT p_hospital, (e ->> 'id')::uuid, e ->> 'icd10', e ->> 'name', (e ->> 'source')::uuid
    FROM jsonb_array_elements(COALESCE(p_seed -> 'conditions', '[]'::jsonb)) e;
    PERFORM public.mhai_analysis_put_facts(p_hospital, p_now, COALESCE(p_seed -> 'subjects', '[]'::jsonb), true);
    SELECT count(*) INTO v_n FROM public.analysis_subjects s WHERE s.hospital_id = p_hospital;
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_demo_raise(hospital): switches the demo on with
-- DEMO-SYNTHETIC references (EXECUTE to mhai_provision only), for the
-- global row (NULL) or a hospital created as the demo. Refuses (22023) any
-- other hospital; the guard refuses (23514) a row that was ever signed off.
-- Returns whether the row was not on before.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_demo_raise(p_hospital uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_was text;
BEGIN
    IF p_hospital IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.hospitals h WHERE h.id = p_hospital AND h.is_demo) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'analysis: only a hospital created as the demo takes demo references';
    END IF;
    SELECT s.position INTO v_was FROM public.analysis_switch s WHERE s.hospital_id IS NOT DISTINCT FROM p_hospital FOR UPDATE;
    INSERT INTO public.analysis_switch AS s (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
        VALUES (p_hospital, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')
        ON CONFLICT (hospital_id) DO UPDATE SET position = 'on', signoff_clinical = EXCLUDED.signoff_clinical,
            signoff_privacy = EXCLUDED.signoff_privacy, signoff_legal = EXCLUDED.signoff_legal, changed_by = NULL;
    RETURN v_was IS DISTINCT FROM 'on';
END
$$;
-- +goose StatementEnd

-- mhai_hospital_mark_demo(hospital): marks a hospital made before is_demo
-- existed as the demo, once (EXECUTE to mhai_provision only). Refuses
-- (22023) an unknown or already-marked hospital, one with a real cloud
-- agreement (any reference but DEMO-SYNTHETIC), and any hospital once an
-- analysis switch has ever been signed off.
-- +goose StatementBegin
CREATE FUNCTION mhai_hospital_mark_demo(p_hospital uuid) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.hospitals h WHERE h.id = p_hospital AND NOT h.is_demo) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: no such hospital, or already the demo';
    END IF;
    IF EXISTS (SELECT 1 FROM public.hospital_model_policy p
               WHERE p.hospital_id = p_hospital AND p.agreement_ref IS DISTINCT FROM 'DEMO-SYNTHETIC' AND p.agreement_ref IS NOT NULL) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: a hospital with a real cloud agreement is not the demo';
    END IF;
    IF EXISTS (SELECT 1 FROM public.analysis_switch s WHERE s.ever_signed_off) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: an analysis switch was signed off; no hospital is marked as the demo any more';
    END IF;
    PERFORM set_config('mhai.demo_mark', p_hospital::text, true);
    UPDATE public.hospitals SET is_demo = true WHERE id = p_hospital;
    PERFORM set_config('mhai.demo_mark', '', true);
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_hospital_mark_demo(uuid) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_demo_raise(uuid) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_demo(uuid) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_zone(uuid) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_block_frozen(uuid, date) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_put_facts(uuid, timestamptz, jsonb, boolean) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_ingest(uuid, timestamptz, jsonb) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_freeze(uuid, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_sweep(uuid, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_due(timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_ran(uuid, timestamptz) OWNER TO mhai_analysis_writer;
ALTER FUNCTION mhai_analysis_seed(uuid, timestamptz, jsonb) OWNER TO mhai_analysis_writer;
REVOKE ALL ON FUNCTION mhai_analysis_demo(uuid), mhai_analysis_zone(uuid), mhai_analysis_block_frozen(uuid, date),
    mhai_analysis_put_facts(uuid, timestamptz, jsonb, boolean), mhai_analysis_ingest_source(uuid, timestamptz, timestamptz),
    mhai_analysis_ingest(uuid, timestamptz, jsonb), mhai_analysis_freeze(uuid, timestamptz),
    mhai_analysis_sweep(uuid, timestamptz), mhai_analysis_due(timestamptz), mhai_analysis_ran(uuid, timestamptz),
    mhai_analysis_seed(uuid, timestamptz, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz),
    mhai_analysis_ingest(uuid, timestamptz, jsonb), mhai_analysis_freeze(uuid, timestamptz),
    mhai_analysis_sweep(uuid, timestamptz), mhai_analysis_due(timestamptz), mhai_analysis_ran(uuid, timestamptz)
    TO mhai_analysis_job;
REVOKE ALL ON FUNCTION mhai_analysis_demo_raise(uuid), mhai_hospital_mark_demo(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_hospital_mark_demo(uuid) TO mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_analysis_seed(uuid, timestamptz, jsonb), mhai_analysis_demo_raise(uuid) TO mhai_provision;

------------------------------------------------------------------------
-- The provisioning audit: the CLI's switch changes.
------------------------------------------------------------------------
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated',
                           'hospital.model_policy_changed', 'analysis.switch_raised', 'analysis.switch_lowered',
                           'hospital.marked_demo')
                AND actor_id IS NULL);

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

-- 00018's append, as it was.
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated',
                           'hospital.model_policy_changed')
                AND actor_id IS NULL);

DROP FUNCTION mhai_hospital_mark_demo(uuid);
DROP POLICY analysis_writer_read ON hospital_model_policy;
REVOKE ALL ON hospital_model_policy FROM mhai_analysis_writer;
DROP POLICY analysis_writer_mark ON hospitals;
DROP FUNCTION mhai_analysis_demo_raise(uuid);
DROP FUNCTION mhai_analysis_seed(uuid, timestamptz, jsonb);
DROP FUNCTION mhai_analysis_ran(uuid, timestamptz);
DROP FUNCTION mhai_analysis_due(timestamptz);
DROP FUNCTION mhai_analysis_sweep(uuid, timestamptz);
DROP FUNCTION mhai_analysis_freeze(uuid, timestamptz);
DROP FUNCTION mhai_analysis_ingest(uuid, timestamptz, jsonb);
DROP FUNCTION mhai_analysis_ingest_source(uuid, timestamptz, timestamptz);
DROP FUNCTION mhai_analysis_put_facts(uuid, timestamptz, jsonb, boolean);
DROP FUNCTION mhai_analysis_block_frozen(uuid, date);
DROP FUNCTION mhai_analysis_zone(uuid);
DROP FUNCTION mhai_analysis_demo(uuid);
DROP FUNCTION mhai_analysis_position(uuid);

DROP POLICY analysis_writer_read ON hospitals;
DROP POLICY analysis_writer_read ON feedback_events;
DROP POLICY analysis_writer_read ON clinical_decisions;
DROP POLICY analysis_writer_read ON clinical_queries;
REVOKE ALL ON clinical_queries, clinical_decisions, feedback_events, hospitals FROM mhai_analysis_writer;

-- The tables take their policies, grants and indexes with them.
DROP TABLE analysis_watermarks;
DROP TABLE analysis_pseudonyms;
DROP TABLE analysis_periods;
DROP TABLE analysis_first_seen;
DROP TABLE analysis_facts;
DROP TABLE analysis_subjects;
DROP TABLE analysis_conditions;
DROP TABLE analysis_locations;
DROP TABLE analysis_sources;
DROP TABLE analysis_switch;
DROP FUNCTION mhai_analysis_switch_guard();
DROP TRIGGER hospitals_demo_fixed ON hospitals;
DROP FUNCTION mhai_hospital_demo_fixed();
ALTER TABLE hospitals DROP COLUMN is_demo;

REVOKE EXECUTE ON FUNCTION mhai_reporting_period_ok(text, date, date) FROM mhai_analysis_writer;
REVOKE USAGE ON SCHEMA public FROM mhai_analysis_job, mhai_analysis_writer, mhai_analysis_reader;
REVOKE mhai_analysis_job FROM mhai_app;

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
