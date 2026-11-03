-- Feedback and corrections, with the two-person rule in the database (Plan 4
-- spec §4.3, §4.4, §4.9).
--
-- feedback_events (00002, empty outside tests) is widened rather than
-- replaced, so a validation and a correction are one kind of row:
--   - query_id (NOT NULL, FK (query_id, hospital_id) to clinical_queries)
--     replaces decision_id as the link; clinician_id becomes submitted_by;
--     status, decided_by, decided_at, rejection_code, rejection_note,
--     client_created_at, request_id and assigned_to (always null in Plan 4)
--     are new; decision_id, hga_weight_suggestions_json, processed and
--     processed_at go (nothing reads them).
--   - CHECKs tie action to status, bound the free text, close the rejection
--     codes and keep decided_by apart from submitted_by.
--
-- The rule "nobody decides their own correction, and only an active
-- clinician of the same hospital decides" holds with no help from core:
--   - the CHECKs refuse decided_by = submitted_by from every writer;
--   - feedback_decider_check, a DEFERRABLE INITIALLY IMMEDIATE constraint
--     trigger (so no grant can set decided_by around it), looks the decider
--     up as it commits: active, same hospital, holds clinician. It is
--     SECURITY DEFINER, owned by mhai_migrate, search_path pinned, and reads
--     users (FOR SHARE, so a deactivation can't slip in between) and
--     user_roles through 00003's owner policies pairing_check and
--     pairing_lock, which already exist and are all it uses;
--   - feedback_transition_check: pending_review moves once, to approved,
--     rejected or withdrawn; recorded and decided rows never change; the
--     identity and text columns never change; withdrawn needs the
--     submitter and approved or rejected needs the decider to be the
--     session's user (mhai.user_id, set by db.WithHospitalUser from the
--     verified token). The setting is only a claim; the decider check
--     above is what makes the claim worth anything.
--
-- clinical_decisions gains masked_question and masked_patient_json (what a
-- reviewer may read in place of the raw consultation, spec §4.4) and
-- timings_ms (closed keys, spec §6.3). mhai_worker's table-wide INSERT on
-- clinical_decisions (00008) covers the new columns; mhai_gateway's table
-- SELECT does too.
--
-- Grants: mhai_gateway keeps SELECT and INSERT on feedback_events and loses
-- the table-wide UPDATE for UPDATE of the five decision columns only.
-- mhai_worker reads seven non-text columns and writes nothing: reason,
-- corrected_diagnosis and rejection_note are never readable by it.

-- +goose Up

-- The verified caller of this transaction, or null (db.WithHospitalUser).
CREATE FUNCTION mhai_current_user() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT nullif(current_setting('mhai.user_id', true), '')::uuid $$;
ALTER FUNCTION mhai_current_user() OWNER TO mhai_migrate;

------------------------------------------------------------------------
-- feedback_events
------------------------------------------------------------------------
DROP INDEX feedback_events_hospital_processed_idx;
ALTER TABLE feedback_events
    DROP COLUMN decision_id,
    DROP COLUMN hga_weight_suggestions_json,
    DROP COLUMN processed,
    DROP COLUMN processed_at;
ALTER TABLE feedback_events RENAME COLUMN clinician_id TO submitted_by;
ALTER TABLE feedback_events
    ALTER COLUMN submitted_by SET NOT NULL,
    ALTER COLUMN action SET NOT NULL,
    ADD COLUMN query_id          uuid NOT NULL,
    ADD COLUMN status            text NOT NULL,
    ADD COLUMN decided_by        uuid REFERENCES users (id),
    ADD COLUMN decided_at        timestamptz,
    ADD COLUMN rejection_code    text,
    ADD COLUMN rejection_note    text,
    ADD COLUMN client_created_at timestamptz,
    ADD COLUMN request_id        text,
    ADD COLUMN assigned_to       uuid REFERENCES users (id),
    ADD CONSTRAINT feedback_events_query_fk
        FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id),
    ADD CONSTRAINT feedback_action_check CHECK (action IN ('validate', 'correct')),
    ADD CONSTRAINT feedback_status_check CHECK (
        (action = 'validate' AND status = 'recorded')
        OR (action = 'correct' AND status IN ('pending_review', 'approved', 'rejected', 'withdrawn'))),
    ADD CONSTRAINT feedback_reason_check CHECK (
        (action = 'validate' AND reason IS NULL)
        OR (action = 'correct' AND char_length(reason) BETWEEN 3 AND 1000)),
    ADD CONSTRAINT feedback_diagnosis_check CHECK (
        (action = 'validate' AND corrected_diagnosis IS NULL)
        OR (action = 'correct' AND char_length(corrected_diagnosis) BETWEEN 1 AND 200)),
    ADD CONSTRAINT feedback_rejection_code_check CHECK (
        rejection_code IS NULL OR rejection_code IN ('not_supported', 'needs_rewording', 'unsafe', 'out_of_scope')),
    ADD CONSTRAINT feedback_rejection_note_check CHECK (
        rejection_note IS NULL OR char_length(rejection_note) <= 500),
    -- A rejection has a code; nothing else carries a code or a note.
    ADD CONSTRAINT feedback_rejection_shape CHECK (
        (status = 'rejected') = (rejection_code IS NOT NULL)
        AND (rejection_note IS NULL OR status = 'rejected')),
    -- C2: nobody decides their own correction.
    ADD CONSTRAINT feedback_not_self_decided CHECK (decided_by IS NULL OR decided_by <> submitted_by),
    -- Decided means a decider and a time; only a decision has a decider
    -- (a withdrawal has none).
    ADD CONSTRAINT feedback_decided_shape CHECK (
        (status IN ('approved', 'rejected')) = (decided_by IS NOT NULL AND decided_at IS NOT NULL)
        AND (decided_by IS NULL OR status IN ('approved', 'rejected')));

-- C11: one live feedback per query; a withdrawal frees the slot.
CREATE UNIQUE INDEX feedback_one_live ON feedback_events (query_id) WHERE status <> 'withdrawn';
CREATE INDEX feedback_events_hospital_status_idx ON feedback_events (hospital_id, status);
CREATE INDEX feedback_events_submitter_idx ON feedback_events (hospital_id, submitted_by);

-- The decider check. Failures carry a fixed message: no ids, no text.
-- +goose StatementBegin
CREATE FUNCTION mhai_feedback_decider_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM 1
    FROM public.users u
    WHERE u.id = NEW.decided_by
      AND u.is_active
      AND u.hospital_id = NEW.hospital_id
      AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role = 'clinician')
    FOR SHARE OF u;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: the decider must be an active clinician of the hospital';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_feedback_decider_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_feedback_decider_check() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER feedback_decider_check
    AFTER INSERT OR UPDATE OF status, decided_by ON feedback_events
    DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW WHEN (NEW.status IN ('approved', 'rejected'))
    EXECUTE FUNCTION mhai_feedback_decider_check();

-- The transition check. SECURITY DEFINER like its twin, though it reads
-- nothing but the session setting; the pinned search_path is what matters.
-- +goose StatementBegin
CREATE FUNCTION mhai_feedback_transition_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('recorded', 'pending_review') THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'feedback_events: a row starts recorded or pending_review';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.hospital_id IS DISTINCT FROM OLD.hospital_id
       OR NEW.query_id IS DISTINCT FROM OLD.query_id
       OR NEW.submitted_by IS DISTINCT FROM OLD.submitted_by
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.corrected_diagnosis IS DISTINCT FROM OLD.corrected_diagnosis
       OR NEW.client_created_at IS DISTINCT FROM OLD.client_created_at
       OR NEW.request_id IS DISTINCT FROM OLD.request_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: a submitted feedback does not change';
    END IF;
    IF OLD.status <> 'pending_review' THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: this feedback is final';
    END IF;
    IF NEW.status NOT IN ('approved', 'rejected', 'withdrawn') THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: a waiting correction is approved, rejected or withdrawn';
    END IF;
    IF NEW.status = 'withdrawn' AND NEW.submitted_by IS DISTINCT FROM public.mhai_current_user() THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: only the submitter withdraws';
    END IF;
    IF NEW.status IN ('approved', 'rejected') AND NEW.decided_by IS DISTINCT FROM public.mhai_current_user() THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: the decider must be the signed-in user';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_feedback_transition_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_feedback_transition_check() FROM PUBLIC;
CREATE TRIGGER feedback_transition_check BEFORE INSERT OR UPDATE ON feedback_events
    FOR EACH ROW EXECUTE FUNCTION mhai_feedback_transition_check();

-- Grants (spec §4.9). RLS hospital_rows (00003) already covers both roles.
REVOKE UPDATE ON feedback_events FROM mhai_gateway;
GRANT UPDATE (status, decided_by, decided_at, rejection_code, rejection_note) ON feedback_events TO mhai_gateway;
GRANT SELECT (id, hospital_id, query_id, action, status, submitted_by, decided_by) ON feedback_events TO mhai_worker;

------------------------------------------------------------------------
-- clinical_decisions
------------------------------------------------------------------------
ALTER TABLE clinical_decisions
    ADD COLUMN masked_question     text,
    ADD COLUMN masked_patient_json jsonb,
    ADD COLUMN timings_ms          jsonb,
    ADD CONSTRAINT decisions_masked_question_check CHECK (masked_question IS NULL OR char_length(masked_question) <= 8000),
    ADD CONSTRAINT decisions_masked_patient_check CHECK (
        masked_patient_json IS NULL
        OR (jsonb_typeof(masked_patient_json) = 'object' AND octet_length(masked_patient_json::text) <= 32768)),
    -- Closed keys, non-negative whole milliseconds.
    ADD CONSTRAINT decisions_timings_check CHECK (
        timings_ms IS NULL
        OR (jsonb_typeof(timings_ms) = 'object'
            AND NOT jsonb_path_exists(timings_ms,
                '$.keyvalue() ? (@.key != "privacy" && @.key != "guidelines" && @.key != "answer" && @.key != "evidence"
                                 || @.value.type() != "number" || @.value < 0 || @.value != @.value.floor())')));

-- +goose Down
ALTER TABLE clinical_decisions
    DROP CONSTRAINT decisions_timings_check,
    DROP CONSTRAINT decisions_masked_patient_check,
    DROP CONSTRAINT decisions_masked_question_check,
    DROP COLUMN timings_ms,
    DROP COLUMN masked_patient_json,
    DROP COLUMN masked_question;

-- 00003's grants back: the gateway's table-wide UPDATE, nothing for the worker.
REVOKE SELECT ON feedback_events FROM mhai_worker;
REVOKE UPDATE (status, decided_by, decided_at, rejection_code, rejection_note) ON feedback_events FROM mhai_gateway;
GRANT UPDATE ON feedback_events TO mhai_gateway;

DROP TRIGGER feedback_transition_check ON feedback_events;
DROP TRIGGER feedback_decider_check ON feedback_events;
DROP FUNCTION mhai_feedback_transition_check();
DROP FUNCTION mhai_feedback_decider_check();
DROP INDEX feedback_events_submitter_idx;
DROP INDEX feedback_events_hospital_status_idx;
DROP INDEX feedback_one_live;

ALTER TABLE feedback_events
    DROP CONSTRAINT feedback_decided_shape,
    DROP CONSTRAINT feedback_not_self_decided,
    DROP CONSTRAINT feedback_rejection_shape,
    DROP CONSTRAINT feedback_rejection_note_check,
    DROP CONSTRAINT feedback_rejection_code_check,
    DROP CONSTRAINT feedback_diagnosis_check,
    DROP CONSTRAINT feedback_reason_check,
    DROP CONSTRAINT feedback_status_check,
    DROP CONSTRAINT feedback_action_check,
    DROP CONSTRAINT feedback_events_query_fk,
    DROP COLUMN assigned_to,
    DROP COLUMN request_id,
    DROP COLUMN client_created_at,
    DROP COLUMN rejection_note,
    DROP COLUMN rejection_code,
    DROP COLUMN decided_at,
    DROP COLUMN decided_by,
    DROP COLUMN status,
    DROP COLUMN query_id,
    ALTER COLUMN action DROP NOT NULL,
    ALTER COLUMN submitted_by DROP NOT NULL;
ALTER TABLE feedback_events RENAME COLUMN submitted_by TO clinician_id;
ALTER TABLE feedback_events
    ADD COLUMN decision_id uuid,
    ADD COLUMN hga_weight_suggestions_json jsonb,
    ADD COLUMN processed boolean NOT NULL DEFAULT false,
    ADD COLUMN processed_at timestamptz,
    ADD FOREIGN KEY (decision_id, hospital_id) REFERENCES clinical_decisions (id, hospital_id);
CREATE INDEX feedback_events_hospital_processed_idx ON feedback_events (hospital_id, processed);

DROP FUNCTION mhai_current_user();
