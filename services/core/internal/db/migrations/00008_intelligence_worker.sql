-- The pipeline's database access (spec §8.2, §10.2; plan departures 2, 3).
--
-- mhai_worker_app is the role the pipeline's login (INTELLIGENCE_DB_USER,
-- default intelligence_app, made by core migrate up through db.EnsureLogin)
-- is a member of, and of nothing else. It is NOINHERIT and holds
-- mhai_worker WITH INHERIT FALSE, so the login holds no privilege at all
-- until it runs SET LOCAL ROLE mhai_worker inside with_hospital() (Python
-- boundary/db.py), and can SET ROLE to no other role. This retires the
-- superuser connection (Plan 2 review T2 M6).
--
-- mhai_worker is narrowed to what the pipeline uses: clinical_queries
-- SELECT and UPDATE of the status columns only, of rows in flight, to
-- running, completed (with a decision) or failed, attempts never going
-- down; clinical_decisions and
-- hga_evaluations SELECT, INSERT; kb_chunks and guardrail_weights SELECT
-- (no enrichment in this plan, so no kb_chunks writes); the outbox INSERT.
--
-- intelligence_audit_outbox holds the audit facts of a job, written by the
-- worker in the same transaction as the result. Core's drainer (Plan 3
-- T12) moves them onto the hash chain: DELETE ... WHERE id IN (SELECT id
-- ... ORDER BY id LIMIT n) RETURNING, then audit.Write, in one transaction.
-- Under READ COMMITTED a second drainer waits on the first's row locks and
-- then finds the rows gone, so each fact is appended once (F2). Core takes
-- the actor, hospital and request id from clinical_queries (which it
-- wrote), never from the fact.
--
-- mhai_audit_drain is the drainer's role: the outbox of every hospital
-- (SELECT, DELETE), four id columns and the status of clinical_queries (a
-- fact's action must match the job's status: final review I-1), and
-- appending the three job actions to audit_log. Nothing else. The core login reaches it
-- only by SET ROLE (mhai_app holds it WITH INHERIT FALSE).
--
-- mhai_gateway is narrowed on the pipeline tables too (final review I-4):
-- clinical_queries INSERT and UPDATE of published_at only,
-- clinical_decisions and hga_evaluations SELECT only.
--
-- Roles are cluster-wide, so they are created only if missing and kept on
-- Down; Down takes back every grant and membership this file made.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_worker_app') THEN
        CREATE ROLE mhai_worker_app NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_audit_drain') THEN
        CREATE ROLE mhai_audit_drain NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
-- Whatever a pre-existing role looked like, it holds nothing it inherits.
ALTER ROLE mhai_worker_app NOINHERIT;

GRANT mhai_worker TO mhai_worker_app WITH INHERIT FALSE;
GRANT mhai_audit_drain TO mhai_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_audit_drain;

------------------------------------------------------------------------
-- mhai_worker, narrowed. REVOKE ALL also takes any column grant away.
------------------------------------------------------------------------
REVOKE ALL ON clinical_queries, clinical_decisions, hga_evaluations, kb_chunks, guardrail_weights FROM mhai_worker;
REVOKE ALL ON SEQUENCE kb_chunks_id_seq FROM mhai_worker;
GRANT SELECT ON clinical_queries TO mhai_worker;
GRANT UPDATE (status, progress, current_stage, started_at, completed_at, error, attempts, lease_until)
    ON clinical_queries TO mhai_worker;
GRANT SELECT, INSERT ON clinical_decisions, hga_evaluations TO mhai_worker;
GRANT USAGE ON SEQUENCE hga_evaluations_id_seq TO mhai_worker;
GRANT SELECT ON kb_chunks, guardrail_weights TO mhai_worker;

-- The worker's status moves, as 00007 does for mhai_jobs. RESTRICTIVE, so it
-- holds on top of hospital_rows: the worker touches only rows still in
-- flight (queued or running), and leaves them running, completed or failed.
-- It can't re-open a cancelled, failed or completed question, cancel one
-- (core does), or put one back in the queue (a crashed worker's job is
-- redelivered by JetStream and re-claimed while running: no 'queued' is
-- needed). completed needs the decision row first, so a question is never
-- complete without an answer.
CREATE POLICY worker_status ON clinical_queries AS RESTRICTIVE FOR UPDATE TO mhai_worker
    USING (status IN ('queued', 'running'))
    WITH CHECK (status IN ('running', 'completed', 'failed')
                AND (status <> 'completed'
                     OR EXISTS (SELECT 1 FROM clinical_decisions d
                                WHERE d.query_id = clinical_queries.id
                                  AND d.hospital_id = clinical_queries.hospital_id)));

-- attempts only grows, whoever writes it: it is the delivery count the
-- audit (query.failed) and the retry limit read.
-- +goose StatementBegin
CREATE FUNCTION mhai_attempts_never_decrease() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = 'check_violation',
        MESSAGE = 'clinical_queries: attempts never decreases';
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_attempts_never_decrease() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_attempts_never_decrease() FROM PUBLIC;
CREATE TRIGGER clinical_queries_attempts BEFORE UPDATE OF attempts ON clinical_queries
    FOR EACH ROW WHEN (NEW.attempts < OLD.attempts) EXECUTE FUNCTION mhai_attempts_never_decrease();

------------------------------------------------------------------------
-- mhai_gateway on the pipeline tables (final review I-4): what core's
-- request path writes, and no more. 00003 gave it SELECT, INSERT, UPDATE
-- on clinical_queries and clinical_decisions and SELECT, INSERT on
-- hga_evaluations; results are the worker's to write. The submit inserts
-- the queued row (queries.create) and the publish sets published_at
-- (queries.publish); every other job column is the worker's, mhai_jobs's
-- or a trigger's.
------------------------------------------------------------------------
REVOKE UPDATE ON clinical_queries FROM mhai_gateway;
GRANT UPDATE (published_at) ON clinical_queries TO mhai_gateway;
REVOKE INSERT, UPDATE ON clinical_decisions FROM mhai_gateway;
REVOKE INSERT ON hga_evaluations FROM mhai_gateway;
REVOKE USAGE ON SEQUENCE hga_evaluations_id_seq FROM mhai_gateway;

------------------------------------------------------------------------
-- The outbox.
------------------------------------------------------------------------
CREATE TABLE intelligence_audit_outbox (
    -- The drainer's order and its lock target (F2).
    id          bigserial PRIMARY KEY,
    hospital_id uuid NOT NULL,
    query_id    uuid NOT NULL,
    action      text NOT NULL CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled')),
    entity      text CHECK (entity IS NULL OR entity ~ '^[a-z_]{1,32}$'),
    entity_id   text CHECK (entity_id IS NULL OR length(entity_id) <= 64),
    -- Ids and codes only (spec §10.2); audit.Write checks the keys again.
    payload     jsonb NOT NULL DEFAULT '{}'
                CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 8192),
    created_at  timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id)
);
CREATE INDEX intelligence_audit_outbox_query_idx ON intelligence_audit_outbox (query_id, hospital_id);
ALTER TABLE intelligence_audit_outbox OWNER TO mhai_migrate;
ALTER TABLE intelligence_audit_outbox ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- The worker appends its own hospital's facts and reads none back.
GRANT INSERT ON intelligence_audit_outbox TO mhai_worker;
GRANT USAGE ON SEQUENCE intelligence_audit_outbox_id_seq TO mhai_worker;
CREATE POLICY worker_append ON intelligence_audit_outbox FOR INSERT TO mhai_worker
    WITH CHECK (hospital_id = mhai_current_hospital());

-- The drainer: every hospital's facts, read and delete only.
GRANT SELECT, DELETE ON intelligence_audit_outbox TO mhai_audit_drain;
CREATE POLICY drain_read ON intelligence_audit_outbox FOR SELECT TO mhai_audit_drain USING (true);
CREATE POLICY drain_delete ON intelligence_audit_outbox FOR DELETE TO mhai_audit_drain USING (true);

-- F4: FORCE applies to the owner too. 00009's SECURITY DEFINER triggers
-- (owned by mhai_migrate, like 00003's pairing_* checks) append facts
-- here, so the owner gets an append policy and nothing more.
CREATE POLICY owner_append ON intelligence_audit_outbox FOR INSERT TO mhai_migrate WITH CHECK (true);

------------------------------------------------------------------------
-- mhai_audit_drain on the rest: the ids core needs, the job's status (to
-- check a fact against it), and the chain.
------------------------------------------------------------------------
GRANT SELECT (id, hospital_id, created_by, request_id, status) ON clinical_queries TO mhai_audit_drain;
CREATE POLICY drain_read ON clinical_queries FOR SELECT TO mhai_audit_drain USING (true);

GRANT INSERT ON audit_log TO mhai_audit_drain;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_audit_drain;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_audit_drain;
CREATE POLICY drain_append ON audit_log FOR INSERT TO mhai_audit_drain
    WITH CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled') AND hospital_id IS NOT NULL);

-- +goose Down
DROP POLICY drain_append ON audit_log;
DROP POLICY drain_read ON clinical_queries;
REVOKE ALL ON clinical_queries, audit_log FROM mhai_audit_drain;
REVOKE ALL ON SEQUENCE audit_log_id_seq FROM mhai_audit_drain;
REVOKE ALL ON FUNCTION audit_chain_head() FROM mhai_audit_drain;

-- Its policies and grants go with it.
DROP TABLE intelligence_audit_outbox;
REVOKE ALL ON SCHEMA public FROM mhai_audit_drain;

DROP TRIGGER clinical_queries_attempts ON clinical_queries;
DROP FUNCTION mhai_attempts_never_decrease();
DROP POLICY worker_status ON clinical_queries;

-- mhai_gateway back to 00003's grants.
REVOKE UPDATE (published_at) ON clinical_queries FROM mhai_gateway;
GRANT UPDATE ON clinical_queries, clinical_decisions TO mhai_gateway;
GRANT INSERT ON clinical_decisions, hga_evaluations TO mhai_gateway;
GRANT USAGE ON SEQUENCE hga_evaluations_id_seq TO mhai_gateway;

-- mhai_worker back to 00003's grants.
REVOKE ALL ON clinical_queries, clinical_decisions, hga_evaluations, kb_chunks, guardrail_weights FROM mhai_worker;
GRANT SELECT, UPDATE ON clinical_queries TO mhai_worker;
GRANT SELECT, INSERT ON clinical_decisions, hga_evaluations TO mhai_worker;
GRANT USAGE ON SEQUENCE hga_evaluations_id_seq TO mhai_worker;
GRANT SELECT, INSERT, UPDATE ON kb_chunks TO mhai_worker;
GRANT USAGE ON SEQUENCE kb_chunks_id_seq TO mhai_worker;
GRANT SELECT ON guardrail_weights TO mhai_worker;

REVOKE mhai_audit_drain FROM mhai_app;
REVOKE mhai_worker FROM mhai_worker_app;
