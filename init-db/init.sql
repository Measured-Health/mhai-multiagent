-- MHAI Database Bootstrap
-- Mounted into postgres container at /docker-entrypoint-initdb.d/

CREATE TABLE IF NOT EXISTS kb_chunks (
    id SERIAL PRIMARY KEY,
    chunk_id VARCHAR(16) UNIQUE NOT NULL,
    content TEXT NOT NULL,
    chunk_type VARCHAR(24) NOT NULL,
    source_patient_id VARCHAR(24),
    source_subject_id VARCHAR(24),
    metadata_json JSONB,
    embedding_ref VARCHAR(512),
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_kb_chunk_type ON kb_chunks(chunk_type);
CREATE INDEX idx_kb_source ON kb_chunks(source_patient_id);

CREATE TABLE IF NOT EXISTS clinical_queries (
    id UUID PRIMARY KEY,
    patient_data_json JSONB,
    clinical_question TEXT NOT NULL,
    urgency VARCHAR(16) NOT NULL,
    category VARCHAR(32),
    status VARCHAR(16) DEFAULT 'queued',
    progress REAL DEFAULT 0.0,
    current_stage VARCHAR(64),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    error TEXT,
    created_by VARCHAR(64),
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_query_status ON clinical_queries(status);

CREATE TABLE IF NOT EXISTS clinical_decisions (
    id UUID PRIMARY KEY,
    query_id UUID UNIQUE REFERENCES clinical_queries(id),
    diagnosis TEXT,
    icd10_code VARCHAR(16),
    response_text TEXT,
    citations_json JSONB,
    trust_score REAL,
    hga_decision VARCHAR(16),
    guardrail_report_json JSONB,
    retrieval_ms REAL,
    generation_ms REAL,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_decision_hga ON clinical_decisions(hga_decision);

CREATE TABLE IF NOT EXISTS hga_evaluations (
    id SERIAL PRIMARY KEY,
    query_id UUID,
    target_agent VARCHAR(24),
    trust REAL,
    decision VARCHAR(16),
    nli_score REAL,
    coherence_score REAL,
    numerical_score REAL,
    se_score REAL,
    flag_penalty REAL,
    weights_snapshot_json JSONB,
    cascade_layers_json JSONB,
    ts TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_hga_decision ON hga_evaluations(decision);

CREATE TABLE IF NOT EXISTS guardrail_weights (
    id SERIAL PRIMARY KEY,
    alpha REAL, beta REAL, gamma REAL, delta REAL,
    source VARCHAR(16),
    changed_by VARCHAR(64),
    changed_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS feedback_events (
    id UUID PRIMARY KEY,
    decision_id UUID REFERENCES clinical_decisions(id),
    clinician_id VARCHAR(64),
    action VARCHAR(16),
    reason TEXT,
    corrected_diagnosis TEXT,
    hga_weight_suggestions_json JSONB,
    processed BOOLEAN DEFAULT FALSE,
    processed_at TIMESTAMP,
    created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_feedback_processed ON feedback_events(processed);

CREATE TABLE IF NOT EXISTS audit_log (
    id SERIAL PRIMARY KEY,
    actor VARCHAR(64),
    action VARCHAR(64),
    entity VARCHAR(64),
    entity_id VARCHAR(64),
    payload_json JSONB,
    prev_hash VARCHAR(64),
    hash VARCHAR(64),
    ts TIMESTAMP DEFAULT NOW()
);
CREATE INDEX idx_audit_hash ON audit_log(hash);

CREATE TABLE IF NOT EXISTS users (
    user_id SERIAL PRIMARY KEY,
    username VARCHAR(64) UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    role VARCHAR(16) DEFAULT 'clinician',
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP DEFAULT NOW(),
    last_login TIMESTAMP
);

-- Seed initial admin user (password: changeme — must rotate in prod)
INSERT INTO users (username, password_hash, role) VALUES
    ('admin', '$2b$12$placeholder_hash_change_in_prod', 'admin')
ON CONFLICT (username) DO NOTHING;

-- Seed initial trust weights
INSERT INTO guardrail_weights (alpha, beta, gamma, delta, source) VALUES
    (0.35, 0.25, 0.20, 0.20, 'init');
