-- The per-hospital learning buffer (Plan 4 spec §4.8, §4.9).
--
-- A row here is an approved correction waiting for the weight-update job to
-- learn from it. Only a correction that two people have been through may
-- enter, and the database holds that line, whoever the writer is:
--   - the FK (correction_id, hospital_id) ties a row to a correction of its
--     own hospital; correction_id is UNIQUE, so a correction enters once;
--   - mhai_worker, the only role with a grant, inserts under hospital_rows
--     and the RESTRICTIVE buffer_needs_approval: the correction must be
--     approved, with a decider who is not its submitter. The policy reads
--     feedback_events through the worker's seven-column SELECT (00011) and
--     its hospital_rows, so another hospital's correction is invisible to it.
--   - the insert grant is column-wise: id, cleared_at and reset_id are never
--     the worker's to set.
-- Nobody else is granted anything. The platform sees only a count, through
-- mhai_feedback_buffer_waiting(); the owner reads it under FORCE through
-- one policy scoped to rows not yet cleared.

-- +goose Up

-- No role of Plan 4 writes the knowledge base: the gateway's 00003 write
-- grants on kb_chunks go, as the worker's did in 00008. Nothing in core
-- writes it as the gateway.
REVOKE INSERT, UPDATE, DELETE ON kb_chunks FROM mhai_gateway;
REVOKE USAGE ON SEQUENCE kb_chunks_id_seq FROM mhai_gateway;

-- The target of the same-hospital FK below.
ALTER TABLE feedback_events ADD CONSTRAINT feedback_events_id_hospital_key UNIQUE (id, hospital_id);

CREATE TABLE feedback_buffer (
    id               bigserial PRIMARY KEY,
    hospital_id      uuid NOT NULL REFERENCES hospitals (id),
    correction_id    uuid NOT NULL UNIQUE,
    nli_score        real CHECK (nli_score BETWEEN 0 AND 1),
    coherence_score  real CHECK (coherence_score BETWEEN 0 AND 1),
    numerical_score  real CHECK (numerical_score BETWEEN 0 AND 1),
    se_score         real CHECK (se_score BETWEEN 0 AND 1),
    system_decision  text NOT NULL CHECK (system_decision IN ('PASS', 'REGENERATE', 'ESCALATE')),
    model_versions   jsonb CHECK (model_versions IS NULL
                                  OR (jsonb_typeof(model_versions) = 'object' AND octet_length(model_versions::text) <= 4096)),
    created_at       timestamptz NOT NULL DEFAULT now(),
    cleared_at       timestamptz,
    reset_id         uuid,
    FOREIGN KEY (correction_id, hospital_id) REFERENCES feedback_events (id, hospital_id)
);
CREATE INDEX feedback_buffer_hospital_waiting_idx ON feedback_buffer (hospital_id) WHERE cleared_at IS NULL;
ALTER TABLE feedback_buffer OWNER TO mhai_migrate;
ALTER TABLE feedback_buffer ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- mhai_worker: insert the learning facts of its own hospital, nothing else.
GRANT INSERT (hospital_id, correction_id, nli_score, coherence_score, numerical_score, se_score, system_decision, model_versions)
    ON feedback_buffer TO mhai_worker;
GRANT USAGE ON SEQUENCE feedback_buffer_id_seq TO mhai_worker;
CREATE POLICY hospital_rows ON feedback_buffer FOR INSERT TO mhai_worker
    WITH CHECK (hospital_id = mhai_current_hospital());
-- C8: approved by someone other than the submitter. Names are qualified:
-- inside the subquery a bare hospital_id would be f's own.
CREATE POLICY buffer_needs_approval ON feedback_buffer AS RESTRICTIVE FOR INSERT TO mhai_worker
    WITH CHECK (EXISTS (SELECT 1 FROM feedback_events f
                        WHERE f.id = feedback_buffer.correction_id
                          AND f.hospital_id = feedback_buffer.hospital_id
                          AND f.status = 'approved'
                          AND f.decided_by IS NOT NULL
                          AND f.decided_by <> f.submitted_by));

-- The owner's one read: the waiting rows, for the count below.
CREATE POLICY buffer_waiting_read ON feedback_buffer FOR SELECT TO mhai_migrate
    USING (cleared_at IS NULL);

-- +goose StatementBegin
CREATE FUNCTION mhai_feedback_buffer_waiting() RETURNS bigint
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$ SELECT count(*) FROM public.feedback_buffer WHERE cleared_at IS NULL $$;
-- +goose StatementEnd
ALTER FUNCTION mhai_feedback_buffer_waiting() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_feedback_buffer_waiting() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_feedback_buffer_waiting() TO mhai_platform;

-- +goose Down
GRANT INSERT, UPDATE, DELETE ON kb_chunks TO mhai_gateway;
GRANT USAGE ON SEQUENCE kb_chunks_id_seq TO mhai_gateway;
DROP FUNCTION mhai_feedback_buffer_waiting();
-- The table takes its policies, grants, index and sequence with it.
DROP TABLE feedback_buffer;
ALTER TABLE feedback_events DROP CONSTRAINT feedback_events_id_hospital_key;
