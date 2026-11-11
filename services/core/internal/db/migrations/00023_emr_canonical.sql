-- EMR integration, the canonical model (EMR design §8, §10, §13.2, §18 item
-- 2; plan Task 16). Numbered 00023: 00021 is the Analysis foundation and
-- 00022 is Plan 7c-3's.
--
-- emr.raw_records: one row per source record version, holding only the
-- allow-listed fields (the emr service's RawSink drops the rest before the
-- insert). The unique key (design §8.1) makes a replay a no-op. A document
-- record names its document and its review and no connection (A26); the
-- document pipeline's grants and keys arrive with 00026 (plan departure 7).
--
-- The canonical tables (emr.patients, emr.patient_links, emr.units,
-- emr.encounters, emr.observations, emr.medications) carry the hospital in
-- every key: (hospital_id, id) pairs, so a row can only point at a row of
-- its own hospital (ADR 0009 rule 5). Every versioned row carries its
-- provenance (design §8.2) and is never changed by a new source version: the
-- new version is a new row and the old one gets superseded_by. superseded_by
-- has no foreign key on purpose: when an exclusion deletes the newer row,
-- the older one stays superseded, never current again.
--
-- The patient index (design §10): the MHAI patient id is a random uuid;
-- a link ties one (connection, source patient id) to one patient of the
-- same hospital. A patient may have no MRN and no birth date (a document
-- patient, §10.3). Nothing merges automatically: the emr service writes
-- emr.possible_matches rows, and only core's definer emr_merge_patients()
-- merges, for a hospital admin's confirm.
--
-- Roles:
--   mhai_emr      SELECT, INSERT and column-limited UPDATE on its hospital's
--                 rows (RLS). No DELETE anywhere, and no column that marks a
--                 patient merged or excluded.
--   mhai_gateway  under RLS, the columns an alert shows (design §3.3).
--   definers      emr_patient_in_hospital(), emr_merge_patients(),
--                 emr_exclude_patient(): owner mhai_migrate, search_path
--                 pinned, every relation schema-qualified, EXECUTE for
--                 mhai_gateway and mhai_emr_admin only, and bound to
--                 mhai_current_hospital() (the owner's policies are too).
--
-- Exclusion (design §10.6) purges at once in this version: the patient's
-- canonical rows, raw records and possible matches are deleted and its
-- patient columns cleared in the same transaction; the patient row and its
-- links stay, so future records from the source are recognised and dropped.
-- The legal-hold deferral and the durable purge request arrive with 00025
-- (PR 9); 00024 adds the alert steps to the merge and the exclusion.

-- +goose Up

GRANT USAGE ON SCHEMA emr TO mhai_gateway;

------------------------------------------------------------------------
-- Raw records.
------------------------------------------------------------------------
CREATE TABLE emr.raw_records (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id       uuid NOT NULL REFERENCES public.hospitals (id),
    source_kind       text NOT NULL CHECK (source_kind IN ('connector', 'document')),
    connection_id     uuid,
    document_id       uuid,
    review_id         uuid,
    resource_kind     text NOT NULL CHECK (resource_kind IN ('patient', 'encounter', 'location', 'observation', 'medication')),
    source_id         text NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 256),
    source_version    text NOT NULL DEFAULT '' CHECK (char_length(source_version) <= 64),
    source_updated_at timestamptz,
    received_at       timestamptz NOT NULL DEFAULT now(),
    payload           jsonb CHECK (payload IS NULL OR (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 32768)),
    payload_sha256    bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    connector_type    text NOT NULL CHECK (connector_type IN ('fhir_r4', 'document_archive', 'file_export', 'stub', 'document')),
    connector_version integer NOT NULL CHECK (connector_version >= 1),
    status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'normalised', 'rejected')),
    reject_code       text CHECK (reject_code IN ('unreadable_data', 'out_of_scope', 'no_patient', 'patient_inactive', 'excluded')),
    published_at      timestamptz,
    normalised_at     timestamptz,
    CONSTRAINT raw_records_hospital_id UNIQUE (hospital_id, id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    -- A26: a document record exists only after its review; it has no
    -- connection. A connector record has a connection and no document.
    CONSTRAINT raw_records_document_reviewed CHECK (source_kind <> 'document'
        OR (document_id IS NOT NULL AND review_id IS NOT NULL AND connection_id IS NULL)),
    CONSTRAINT raw_records_connector CHECK (source_kind <> 'connector'
        OR (connection_id IS NOT NULL AND document_id IS NULL AND review_id IS NULL)),
    CONSTRAINT raw_records_reject_code CHECK ((status = 'rejected') = (reject_code IS NOT NULL))
);
ALTER TABLE emr.raw_records OWNER TO mhai_migrate;
ALTER TABLE emr.raw_records ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- Design §8.1: a replay or a duplicate is a no-op.
CREATE UNIQUE INDEX raw_records_source ON emr.raw_records
    (hospital_id, source_kind, coalesce(connection_id, document_id), resource_kind, source_id, source_version);
CREATE INDEX raw_records_unpublished ON emr.raw_records (hospital_id, received_at) WHERE published_at IS NULL AND status = 'pending';

------------------------------------------------------------------------
-- The patient index.
------------------------------------------------------------------------
CREATE TABLE emr.patients (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid()
                  CHECK (id::text NOT IN ('reference', 'unassigned', 'unknown', 'ref')),
    hospital_id   uuid NOT NULL REFERENCES public.hospitals (id),
    display_name  text CHECK (char_length(display_name) BETWEEN 1 AND 120),
    mrn           text CHECK (char_length(mrn) BETWEEN 1 AND 64),
    birth_date    date,
    sex           text NOT NULL DEFAULT 'unknown' CHECK (sex IN ('female', 'male', 'other', 'unknown')),
    origin        text NOT NULL DEFAULT 'connector' CHECK (origin IN ('connector', 'document')),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'merged_into', 'excluded')),
    merged_into   uuid,
    excluded_at   timestamptz,
    exclude_reason text CHECK (exclude_reason IN ('objection', 'hospital_policy', 'other')),
    raw_id        uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT patients_hospital_id UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, merged_into) REFERENCES emr.patients (hospital_id, id),
    CONSTRAINT patients_merged CHECK ((status = 'merged_into') = (merged_into IS NOT NULL) AND merged_into IS DISTINCT FROM id),
    CONSTRAINT patients_excluded CHECK ((status = 'excluded') = (excluded_at IS NOT NULL)
                                       AND (excluded_at IS NULL) = (exclude_reason IS NULL))
);
ALTER TABLE emr.patients OWNER TO mhai_migrate;
ALTER TABLE emr.patients ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX patients_mrn ON emr.patients (hospital_id, mrn) WHERE mrn IS NOT NULL;

CREATE TABLE emr.patient_links (
    hospital_id       uuid NOT NULL,
    connection_id     uuid NOT NULL,
    source_patient_id text NOT NULL CHECK (char_length(source_patient_id) BETWEEN 1 AND 256),
    patient_id        uuid NOT NULL,
    merge_id          uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, source_patient_id),
    -- One hospital on both sides: the connection's and the patient's.
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id)
);
ALTER TABLE emr.patient_links OWNER TO mhai_migrate;
ALTER TABLE emr.patient_links ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX patient_links_patient ON emr.patient_links (hospital_id, patient_id);

CREATE TABLE emr.possible_matches (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id   uuid NOT NULL,
    patient_a     uuid NOT NULL,
    patient_b     uuid NOT NULL,
    reason        text NOT NULL CHECK (reason IN ('mrn_birth_sex', 'mrn_only', 'source_merge', 'document_link')),
    connection_id uuid,
    proposed_at   timestamptz NOT NULL DEFAULT now(),
    decided_at    timestamptz,
    decision      text CHECK (decision IN ('confirmed', 'rejected')),
    CONSTRAINT possible_matches_hospital_id UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, patient_a) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, patient_b) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT possible_matches_two CHECK (patient_a <> patient_b),
    CONSTRAINT possible_matches_decided CHECK ((decided_at IS NULL) = (decision IS NULL))
);
ALTER TABLE emr.possible_matches OWNER TO mhai_migrate;
ALTER TABLE emr.possible_matches ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- A pair is proposed once per reason; a rejected pair is not proposed again.
CREATE UNIQUE INDEX possible_matches_pair ON emr.possible_matches
    (hospital_id, least(patient_a, patient_b), greatest(patient_a, patient_b), reason);

CREATE TABLE emr.merges (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id    uuid NOT NULL,
    match_id       uuid NOT NULL,
    kept_patient   uuid NOT NULL,
    merged_patient uuid NOT NULL,
    merged_by      uuid,
    merged_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (hospital_id, match_id) REFERENCES emr.possible_matches (hospital_id, id),
    FOREIGN KEY (hospital_id, kept_patient) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, merged_patient) REFERENCES emr.patients (hospital_id, id)
);
ALTER TABLE emr.merges OWNER TO mhai_migrate;
ALTER TABLE emr.merges ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

------------------------------------------------------------------------
-- Locations, encounters, observations, medications.
------------------------------------------------------------------------
CREATE TABLE emr.units (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id        uuid NOT NULL,
    connection_id      uuid NOT NULL,
    source_location_id text NOT NULL CHECK (char_length(source_location_id) BETWEEN 1 AND 256),
    label              text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 60),
    kind               text NOT NULL CHECK (kind IN ('ward', 'clinic', 'emergency', 'other')),
    raw_id             uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT units_hospital_id UNIQUE (hospital_id, id),
    CONSTRAINT units_source UNIQUE (connection_id, source_location_id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.units OWNER TO mhai_migrate;
ALTER TABLE emr.units ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- The provenance columns every versioned canonical table carries (design
-- §8.2), written out in each table below.
CREATE TABLE emr.encounters (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id        uuid NOT NULL,
    patient_id         uuid NOT NULL,
    class              text NOT NULL CHECK (class IN ('inpatient', 'outpatient', 'emergency')),
    status             text NOT NULL CHECK (status IN ('planned', 'arrived', 'in-progress', 'finished', 'cancelled', 'entered-in-error')),
    period_start       timestamptz,
    period_end         timestamptz,
    unit_id            uuid,
    bed_label          text CHECK (char_length(bed_label) BETWEEN 1 AND 20),
    raw_id             uuid NOT NULL,
    source_kind        text NOT NULL CHECK (source_kind IN ('connector', 'document')),
    connection_id      uuid,
    connector_type     text NOT NULL,
    connector_version  integer NOT NULL,
    source_id          text NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 256),
    source_version     text NOT NULL CHECK (char_length(source_version) <= 64),
    source_updated_at  timestamptz,
    received_at        timestamptz NOT NULL,
    mapping_version    text NOT NULL CHECK (char_length(mapping_version) BETWEEN 1 AND 64),
    normaliser_version text NOT NULL CHECK (char_length(normaliser_version) BETWEEN 1 AND 32),
    superseded_by      uuid,
    merge_id           uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT encounters_hospital_id UNIQUE (hospital_id, id),
    CONSTRAINT encounters_raw UNIQUE (hospital_id, raw_id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, raw_id) REFERENCES emr.raw_records (hospital_id, id),
    FOREIGN KEY (hospital_id, unit_id) REFERENCES emr.units (hospital_id, id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT encounters_source CHECK ((source_kind = 'connector') = (connection_id IS NOT NULL)),
    CONSTRAINT encounters_not_self CHECK (superseded_by IS DISTINCT FROM id)
);
ALTER TABLE emr.encounters OWNER TO mhai_migrate;
ALTER TABLE emr.encounters ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX encounters_current ON emr.encounters (hospital_id, connection_id, source_id) WHERE superseded_by IS NULL;
CREATE INDEX encounters_patient ON emr.encounters (hospital_id, patient_id);

CREATE TABLE emr.observations (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id        uuid NOT NULL,
    patient_id         uuid NOT NULL,
    encounter_id       uuid,
    category           text NOT NULL CHECK (category IN ('laboratory', 'vital-signs')),
    loinc              text CHECK (loinc ~ '^[0-9]{1,7}-[0-9]$'),
    local_system       text CHECK (char_length(local_system) <= 200),
    local_code         text CHECK (char_length(local_code) <= 64),
    local_display      text CHECK (char_length(local_display) <= 200),
    value_num          numeric,
    comparator         text CHECK (comparator IN ('<', '<=', '>', '>=')),
    value_text         text CHECK (char_length(value_text) <= 120),
    unit_ucum          text CHECK (char_length(unit_ucum) <= 40),
    unit_source        text CHECK (char_length(unit_source) <= 40),
    value_si           numeric,
    ref_low            numeric,
    ref_high           numeric,
    ref_text           text CHECK (char_length(ref_text) <= 120),
    source_flag        text CHECK (source_flag IN ('L', 'H', 'LL', 'HH', 'A', 'AA', 'N')),
    status             text NOT NULL CHECK (status IN ('preliminary', 'final', 'amended', 'corrected', 'cancelled', 'entered-in-error')),
    -- A date-only source time is a date, never an invented hour (Review
    -- Focus 1): exactly one of the two is set.
    effective_at       timestamptz,
    effective_date     date,
    issued_at          timestamptz,
    standardised       boolean NOT NULL,
    raw_id             uuid NOT NULL,
    source_kind        text NOT NULL CHECK (source_kind IN ('connector', 'document')),
    connection_id      uuid,
    connector_type     text NOT NULL,
    connector_version  integer NOT NULL,
    source_id          text NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 256),
    source_version     text NOT NULL CHECK (char_length(source_version) <= 64),
    source_updated_at  timestamptz,
    received_at        timestamptz NOT NULL,
    mapping_version    text NOT NULL CHECK (char_length(mapping_version) BETWEEN 1 AND 64),
    normaliser_version text NOT NULL CHECK (char_length(normaliser_version) BETWEEN 1 AND 32),
    superseded_by      uuid,
    merge_id           uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT observations_hospital_id UNIQUE (hospital_id, id),
    CONSTRAINT observations_raw UNIQUE (hospital_id, raw_id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, raw_id) REFERENCES emr.raw_records (hospital_id, id),
    FOREIGN KEY (hospital_id, encounter_id) REFERENCES emr.encounters (hospital_id, id) ON DELETE SET NULL (encounter_id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT observations_source CHECK ((source_kind = 'connector') = (connection_id IS NOT NULL)),
    CONSTRAINT observations_one_time CHECK ((effective_at IS NULL) <> (effective_date IS NULL)),
    CONSTRAINT observations_comparator CHECK (comparator IS NULL OR (value_num IS NULL AND value_si IS NULL AND value_text IS NOT NULL)),
    CONSTRAINT observations_standardised CHECK (NOT standardised OR loinc IS NOT NULL),
    CONSTRAINT observations_not_self CHECK (superseded_by IS DISTINCT FROM id)
);
ALTER TABLE emr.observations OWNER TO mhai_migrate;
ALTER TABLE emr.observations ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX observations_current ON emr.observations (hospital_id, connection_id, source_id) WHERE superseded_by IS NULL;
CREATE INDEX observations_patient ON emr.observations (hospital_id, patient_id, loinc);

CREATE TABLE emr.medications (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id        uuid NOT NULL,
    patient_id         uuid NOT NULL,
    kind               text NOT NULL CHECK (kind IN ('request', 'statement')),
    atc                text CHECK (atc ~ '^[A-Z][0-9]{2}([A-Z]([A-Z]([0-9]{2})?)?)?$'),
    local_system       text CHECK (char_length(local_system) <= 200),
    local_code         text CHECK (char_length(local_code) <= 64),
    local_display      text CHECK (char_length(local_display) <= 200),
    dose_value         numeric,
    dose_unit_ucum     text CHECK (char_length(dose_unit_ucum) <= 40),
    route              text CHECK (char_length(route) <= 40),
    frequency_text     text CHECK (char_length(frequency_text) <= 60),
    status             text NOT NULL CHECK (status IN ('active', 'on-hold', 'stopped', 'completed', 'cancelled', 'entered-in-error')),
    start_at           timestamptz,
    end_at             timestamptz,
    standardised       boolean NOT NULL,
    raw_id             uuid NOT NULL,
    source_kind        text NOT NULL CHECK (source_kind IN ('connector', 'document')),
    connection_id      uuid,
    connector_type     text NOT NULL,
    connector_version  integer NOT NULL,
    source_id          text NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 256),
    source_version     text NOT NULL CHECK (char_length(source_version) <= 64),
    source_updated_at  timestamptz,
    received_at        timestamptz NOT NULL,
    mapping_version    text NOT NULL CHECK (char_length(mapping_version) BETWEEN 1 AND 64),
    normaliser_version text NOT NULL CHECK (char_length(normaliser_version) BETWEEN 1 AND 32),
    superseded_by      uuid,
    merge_id           uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT medications_hospital_id UNIQUE (hospital_id, id),
    CONSTRAINT medications_raw UNIQUE (hospital_id, raw_id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (hospital_id, raw_id) REFERENCES emr.raw_records (hospital_id, id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT medications_source CHECK ((source_kind = 'connector') = (connection_id IS NOT NULL)),
    CONSTRAINT medications_standardised CHECK (NOT standardised OR atc IS NOT NULL),
    CONSTRAINT medications_not_self CHECK (superseded_by IS DISTINCT FROM id)
);
ALTER TABLE emr.medications OWNER TO mhai_migrate;
ALTER TABLE emr.medications ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX medications_current ON emr.medications (hospital_id, connection_id, source_id) WHERE superseded_by IS NULL;
CREATE INDEX medications_patient ON emr.medications (hospital_id, patient_id);

-- Codes the normaliser couldn't map (design §9.1): codes, display text and
-- counts per connection; no patient data.
CREATE TABLE emr.unmapped_codes (
    hospital_id   uuid NOT NULL,
    connection_id uuid NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('observation', 'medication', 'unit')),
    local_system  text NOT NULL CHECK (char_length(local_system) <= 200),
    local_code    text NOT NULL CHECK (char_length(local_code) BETWEEN 1 AND 64),
    local_display text CHECK (char_length(local_display) <= 200),
    first_seen    timestamptz NOT NULL DEFAULT now(),
    last_seen     timestamptz NOT NULL DEFAULT now(),
    count         bigint NOT NULL DEFAULT 1 CHECK (count >= 1),
    PRIMARY KEY (connection_id, kind, local_system, local_code),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.unmapped_codes OWNER TO mhai_migrate;
ALTER TABLE emr.unmapped_codes ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

------------------------------------------------------------------------
-- Grants and policies.
------------------------------------------------------------------------

-- The emr service: its hospital's rows; never DELETE; never a column that
-- merges or excludes a patient, or decides a match.
GRANT SELECT ON emr.raw_records TO mhai_emr;
-- status and reject_code: a record of an excluded patient is stored
-- rejected, without its payload (design §10.6).
GRANT INSERT (hospital_id, source_kind, connection_id, resource_kind, source_id, source_version, source_updated_at,
              payload, payload_sha256, connector_type, connector_version, status, reject_code) ON emr.raw_records TO mhai_emr;
GRANT UPDATE (status, reject_code, payload, published_at, normalised_at) ON emr.raw_records TO mhai_emr;

GRANT SELECT ON emr.patients TO mhai_emr;
GRANT INSERT (hospital_id, display_name, mrn, birth_date, sex, raw_id) ON emr.patients TO mhai_emr;
GRANT UPDATE (display_name, mrn, birth_date, sex, raw_id, updated_at) ON emr.patients TO mhai_emr;

GRANT SELECT ON emr.patient_links TO mhai_emr;
GRANT INSERT (hospital_id, connection_id, source_patient_id, patient_id) ON emr.patient_links TO mhai_emr;

GRANT SELECT ON emr.possible_matches TO mhai_emr;
GRANT INSERT (hospital_id, patient_a, patient_b, reason, connection_id) ON emr.possible_matches TO mhai_emr;

GRANT SELECT ON emr.units TO mhai_emr;
GRANT INSERT (hospital_id, connection_id, source_location_id, label, kind, raw_id) ON emr.units TO mhai_emr;
GRANT UPDATE (label, kind, raw_id, updated_at) ON emr.units TO mhai_emr;

GRANT SELECT ON emr.encounters, emr.observations, emr.medications TO mhai_emr;
GRANT INSERT (hospital_id, patient_id, class, status, period_start, period_end, unit_id, bed_label, raw_id, source_kind,
              connection_id, connector_type, connector_version, source_id, source_version, source_updated_at, received_at,
              mapping_version, normaliser_version, superseded_by) ON emr.encounters TO mhai_emr;
GRANT INSERT (hospital_id, patient_id, encounter_id, category, loinc, local_system, local_code, local_display, value_num,
              comparator, value_text, unit_ucum, unit_source, value_si, ref_low, ref_high, ref_text, source_flag, status,
              effective_at, effective_date, issued_at, standardised, raw_id, source_kind, connection_id, connector_type,
              connector_version, source_id, source_version, source_updated_at, received_at, mapping_version,
              normaliser_version, superseded_by) ON emr.observations TO mhai_emr;
GRANT INSERT (hospital_id, patient_id, kind, atc, local_system, local_code, local_display, dose_value, dose_unit_ucum,
              route, frequency_text, status, start_at, end_at, standardised, raw_id, source_kind, connection_id,
              connector_type, connector_version, source_id, source_version, source_updated_at, received_at,
              mapping_version, normaliser_version, superseded_by) ON emr.medications TO mhai_emr;
GRANT UPDATE (superseded_by, status) ON emr.encounters, emr.observations, emr.medications TO mhai_emr;

GRANT SELECT ON emr.unmapped_codes TO mhai_emr;
GRANT INSERT (hospital_id, connection_id, kind, local_system, local_code, local_display) ON emr.unmapped_codes TO mhai_emr;
GRANT UPDATE (local_display, last_seen, count) ON emr.unmapped_codes TO mhai_emr;

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['raw_records', 'patients', 'patient_links', 'possible_matches', 'units', 'encounters',
                             'observations', 'medications', 'unmapped_codes'] LOOP
        EXECUTE format('CREATE POLICY emr_own ON emr.%I TO mhai_emr USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
        -- The definers below run as the owner, bound to the caller's hospital.
        EXECUTE format('CREATE POLICY owner_scoped ON emr.%I TO mhai_migrate USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd
CREATE POLICY owner_scoped ON emr.merges TO mhai_migrate USING (hospital_id = public.mhai_current_hospital())
    WITH CHECK (hospital_id = public.mhai_current_hospital());

-- A new patient is active and has no merge or exclusion: those columns are
-- not granted, so they take their defaults.

-- Core's clinical role: the columns an alert shows, its hospital only.
GRANT SELECT (id, hospital_id, display_name, mrn, birth_date, sex, status) ON emr.patients TO mhai_gateway;
GRANT SELECT (id, hospital_id, patient_id, encounter_id, category, loinc, local_display, value_num, comparator, value_text,
              unit_ucum, unit_source, value_si, ref_low, ref_high, ref_text, source_flag, status, effective_at,
              effective_date, issued_at, standardised, connection_id, superseded_by) ON emr.observations TO mhai_gateway;
GRANT SELECT (id, hospital_id, patient_id, kind, atc, local_display, dose_value, dose_unit_ucum, route, frequency_text,
              status, start_at, end_at, standardised, connection_id, superseded_by) ON emr.medications TO mhai_gateway;
GRANT SELECT (id, hospital_id, patient_id, class, status, period_start, period_end, unit_id, bed_label, superseded_by)
    ON emr.encounters TO mhai_gateway;
GRANT SELECT (id, hospital_id, label, kind, connection_id) ON emr.units TO mhai_gateway;
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['patients', 'observations', 'medications', 'encounters', 'units'] LOOP
        EXECUTE format('CREATE POLICY gateway_read ON emr.%I FOR SELECT TO mhai_gateway
                        USING (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Core's patient functions.
------------------------------------------------------------------------

-- Design §10.5: an MHAI patient id is accepted in a request only if it is
-- an active patient of the caller's hospital.
-- +goose StatementBegin
CREATE FUNCTION emr_patient_in_hospital(pid uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM emr.patients p
                   WHERE p.id = pid AND p.hospital_id = public.mhai_current_hospital() AND p.status = 'active')
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_patient_in_hospital(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_patient_in_hospital(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_patient_in_hospital(uuid) TO mhai_gateway, mhai_emr_admin;

-- Design §10.2: a hospital admin confirms a proposed match. keep is 'a' or
-- 'b', the patient that stays. Both patients are locked in id order and
-- re-checked active and in the caller's hospital; the match must be
-- undecided. The other patient's links and canonical rows move to the kept
-- one with a new merge id, and it becomes merged_into. Core writes
-- emr.patients_merged in the same transaction. A refusal raises
-- no_data_found (core answers 404).
-- +goose StatementBegin
CREATE FUNCTION emr_merge_patients(match_id uuid, keep text) RETURNS uuid
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
ALTER FUNCTION emr_merge_patients(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_merge_patients(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_merge_patients(uuid, text) TO mhai_gateway, mhai_emr_admin;

-- Design §10.6: exclude a patient of the caller's hospital and purge what is
-- held for it, in this transaction. The patient row stays (status excluded,
-- its name, MRN, birth date and sex cleared), with its links, so future
-- records from the source are recognised and dropped; so do the patients
-- merged into it, cleared the same way. Deleted: its canonical rows, the raw
-- records behind them and every raw record naming one of its source patient
-- ids, and its possible matches. Returns the counts (for the audit row core
-- writes, emr.patient_excluded). A patient not active or excluded in the
-- caller's hospital raises no_data_found (core answers 404).
-- +goose StatementBegin
CREATE FUNCTION emr_exclude_patient(pid uuid, reason text)
    RETURNS TABLE (observations bigint, medications bigint, encounters bigint, raw_records bigint, matches bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid  uuid := public.mhai_current_hospital();
    ids  uuid[];
    raws uuid[];
    n_o  bigint;
    n_m  bigint;
    n_e  bigint;
    n_r  bigint;
    n_x  bigint;
BEGIN
    IF reason IS NULL OR reason NOT IN ('objection', 'hospital_policy', 'other') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'emr_exclude_patient: reason';
    END IF;
    -- The emr service's link locks first (the order it takes them in:
    -- advisory, then the patient row), sorted, so a normaliser or sink
    -- transaction on this patient either finishes before the purge or
    -- waits and then sees the patient excluded. Links move with a merge,
    -- so the patient's own links cover the patients merged into it.
    PERFORM pg_advisory_xact_lock(hashtextextended('emr-link:' || k.connection_id::text || ':' || k.source_patient_id, 0))
    FROM (SELECT l.connection_id, l.source_patient_id FROM emr.patient_links l
          WHERE l.hospital_id = hid AND l.patient_id = pid ORDER BY 1, 2) k;
    PERFORM 1 FROM emr.patients p WHERE p.id = pid AND p.hospital_id = hid AND p.status IN ('active', 'excluded')
        FOR UPDATE;
    IF hid IS NULL OR NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_exclude_patient: no such patient';
    END IF;
    -- The patient and every patient merged into it, at any depth.
    WITH RECURSIVE s(id) AS (
        SELECT pid
        UNION
        SELECT p.id FROM emr.patients p JOIN s ON p.merged_into = s.id AND p.hospital_id = hid
    )
    SELECT array_agg(s.id) INTO ids FROM s;

    UPDATE emr.patients p SET status = 'excluded', excluded_at = coalesce(p.excluded_at, now()),
                              exclude_reason = coalesce(p.exclude_reason, emr_exclude_patient.reason), updated_at = now()
        WHERE p.hospital_id = hid AND p.id = pid;
    UPDATE emr.patients p SET display_name = NULL, mrn = NULL, birth_date = NULL, sex = 'unknown', raw_id = NULL,
                              updated_at = now()
        WHERE p.hospital_id = hid AND p.id = ANY (ids);

    SELECT coalesce(array_agg(r.raw_id), '{}') INTO raws FROM (
        SELECT o.raw_id FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids)
        UNION ALL SELECT d.raw_id FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids)
        UNION ALL SELECT e.raw_id FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids)) r;

    DELETE FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids);
    GET DIAGNOSTICS n_o = ROW_COUNT;
    DELETE FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids);
    GET DIAGNOSTICS n_m = ROW_COUNT;
    DELETE FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids);
    GET DIAGNOSTICS n_e = ROW_COUNT;
    DELETE FROM emr.possible_matches x WHERE x.hospital_id = hid AND (x.patient_a = ANY (ids) OR x.patient_b = ANY (ids))
        AND NOT EXISTS (SELECT 1 FROM emr.merges g WHERE g.hospital_id = hid AND g.match_id = x.id);
    GET DIAGNOSTICS n_x = ROW_COUNT;
    -- The raw records behind those rows, the patient's own records, and
    -- every other record naming one of its source ids, unless a canonical
    -- row of another patient still stands on it.
    DELETE FROM emr.raw_records r
    WHERE r.hospital_id = hid
      AND (r.id = ANY (raws)
           OR EXISTS (SELECT 1 FROM emr.patient_links l
                      WHERE l.hospital_id = hid AND l.patient_id = ANY (ids) AND l.connection_id = r.connection_id
                        AND ((r.resource_kind = 'patient' AND r.source_id = l.source_patient_id)
                             OR r.payload ->> 'patient' = l.source_patient_id)))
      AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.raw_id = r.id);
    GET DIAGNOSTICS n_r = ROW_COUNT;
    RETURN QUERY SELECT n_o, n_m, n_e, n_r, n_x;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_exclude_patient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_exclude_patient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_exclude_patient(uuid, text) TO mhai_gateway, mhai_emr_admin;

-- +goose Down

DROP FUNCTION emr_exclude_patient(uuid, text);
DROP FUNCTION emr_merge_patients(uuid, text);
DROP FUNCTION emr_patient_in_hospital(uuid);
-- The tables take their policies and grants with them.
DROP TABLE emr.unmapped_codes;
DROP TABLE emr.medications;
DROP TABLE emr.observations;
DROP TABLE emr.encounters;
DROP TABLE emr.units;
DROP TABLE emr.merges;
DROP TABLE emr.possible_matches;
DROP TABLE emr.patient_links;
DROP TABLE emr.patients;
DROP TABLE emr.raw_records;
REVOKE USAGE ON SCHEMA emr FROM mhai_gateway;
