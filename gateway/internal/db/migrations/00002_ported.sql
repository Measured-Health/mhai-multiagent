-- The model-pipeline tables, ported from the former init-db/init.sql.
-- Every hospital-owned table carries hospital_id; child tables reference
-- their parent by (id, hospital_id), so a row can't point across hospitals.
-- users and audit_log from init.sql are replaced by 00001_core.

-- +goose Up

-- hospital_id null = shared reference content (design §7.6): readable by
-- every hospital, writable by none, and never patient data.
CREATE TABLE kb_chunks (
    id                bigserial PRIMARY KEY,
    chunk_id          varchar(16) NOT NULL,
    hospital_id       uuid REFERENCES hospitals (id),
    content           text NOT NULL,
    chunk_type        varchar(24) NOT NULL,
    source_patient_id varchar(24),
    source_subject_id varchar(24),
    metadata_json     jsonb,
    embedding_ref     varchar(512),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (hospital_id, chunk_id),
    CONSTRAINT kb_shared_has_no_patient CHECK (
        hospital_id IS NOT NULL OR (source_patient_id IS NULL AND source_subject_id IS NULL))
);
CREATE INDEX kb_chunks_type_idx ON kb_chunks (chunk_type);
CREATE INDEX kb_chunks_source_idx ON kb_chunks (hospital_id, source_patient_id);

CREATE TABLE clinical_queries (
    id                uuid PRIMARY KEY,
    hospital_id       uuid NOT NULL REFERENCES hospitals (id),
    patient_data_json jsonb,
    clinical_question text NOT NULL,
    urgency           varchar(16) NOT NULL,
    category          varchar(32),
    status            varchar(16) NOT NULL DEFAULT 'queued',
    progress          real NOT NULL DEFAULT 0.0,
    current_stage     varchar(64),
    started_at        timestamptz,
    completed_at      timestamptz,
    error             text,
    created_by        uuid REFERENCES users (id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, hospital_id)
);
CREATE INDEX clinical_queries_hospital_status_idx ON clinical_queries (hospital_id, status);

CREATE TABLE clinical_decisions (
    id                    uuid PRIMARY KEY,
    hospital_id           uuid NOT NULL REFERENCES hospitals (id),
    query_id              uuid UNIQUE,
    diagnosis             text,
    icd10_code            varchar(16),
    response_text         text,
    citations_json        jsonb,
    trust_score           real,
    hga_decision          varchar(16),
    guardrail_report_json jsonb,
    retrieval_ms          real,
    generation_ms         real,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, hospital_id),
    FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id)
);
CREATE INDEX clinical_decisions_hospital_hga_idx ON clinical_decisions (hospital_id, hga_decision);

CREATE TABLE hga_evaluations (
    id                    bigserial PRIMARY KEY,
    hospital_id           uuid NOT NULL REFERENCES hospitals (id),
    query_id              uuid,
    target_agent          varchar(24),
    trust                 real,
    decision              varchar(16),
    nli_score             real,
    coherence_score       real,
    numerical_score       real,
    se_score              real,
    flag_penalty          real,
    weights_snapshot_json jsonb,
    cascade_layers_json   jsonb,
    ts                    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id)
);
CREATE INDEX hga_evaluations_hospital_decision_idx ON hga_evaluations (hospital_id, decision);

-- Global: the trust weights are model configuration, not hospital data.
CREATE TABLE guardrail_weights (
    id         bigserial PRIMARY KEY,
    alpha      real,
    beta       real,
    gamma      real,
    delta      real,
    source     varchar(16),
    changed_by varchar(64),
    changed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feedback_events (
    id                          uuid PRIMARY KEY,
    hospital_id                 uuid NOT NULL REFERENCES hospitals (id),
    decision_id                 uuid,
    clinician_id                uuid REFERENCES users (id),
    action                      varchar(16),
    reason                      text,
    corrected_diagnosis         text,
    hga_weight_suggestions_json jsonb,
    processed                   boolean NOT NULL DEFAULT false,
    processed_at                timestamptz,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (decision_id, hospital_id) REFERENCES clinical_decisions (id, hospital_id)
);
CREATE INDEX feedback_events_hospital_processed_idx ON feedback_events (hospital_id, processed);

-- Initial trust weights (from init.sql).
INSERT INTO guardrail_weights (alpha, beta, gamma, delta, source)
VALUES (0.35, 0.25, 0.20, 0.20, 'init');

-- +goose Down
DROP TABLE feedback_events;
DROP TABLE guardrail_weights;
DROP TABLE hga_evaluations;
DROP TABLE clinical_decisions;
DROP TABLE clinical_queries;
DROP TABLE kb_chunks;
