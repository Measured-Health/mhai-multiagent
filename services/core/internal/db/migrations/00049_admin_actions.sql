-- The hospital admin's account and review actions that answered 501
-- (frontend requirements §5.1, §5.4; TODO.md "Hospital admin routes:
-- gaps"). Additive only. Numbered 00049: main had 00048 (#documents scan)
-- when this was pushed.
--
-- What it adds:
--   - users.removed_at: DELETE /hospital/staff/{id}. A removed account is
--     deactivated, loses its roles, two-step, sessions and trusted devices,
--     and its email is replaced by a tombstone (so the address can be
--     invited again and no longer sits on the row); its names stay, so
--     past consultations, feedback and audit entries remain attributed.
--     The staff routes never show it again.
--   - account_messages: the outbox of messages core owes a person (an
--     invite, a reset link, a two-step notice or reminder, a review
--     reminder or assignment). A row records only that a message is due:
--     who, what kind, about which correction, by whom, through which
--     channel. Never a token, a link, an email or any text. It also bounds
--     the reminders (rate limits) and gives the oversight its reminded_at.
--   - feedback_events.assigned_to becomes writable: a hospital admin of
--     the correction's hospital assigns a waiting correction to an active
--     clinician of that hospital other than its submitter, or clears it.
--     The transition trigger enforces it whatever core does; nothing else
--     of a waiting correction changes with it, and a decision never moves
--     the assignee.

-- +goose Up
ALTER TABLE users ADD COLUMN removed_at timestamptz;
-- A removed account is never active (the staff routes rely on it).
ALTER TABLE users ADD CONSTRAINT users_removed_inactive CHECK (removed_at IS NULL OR NOT is_active);

CREATE TABLE account_messages (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id  uuid NOT NULL REFERENCES hospitals (id),
    recipient_id uuid NOT NULL REFERENCES users (id),
    kind         text NOT NULL CHECK (kind IN ('invite', 'password_reset', 'mfa_reset', 'mfa_reminder',
                                               'review_reminder', 'review_assigned')),
    -- The correction a review message is about; null for the rest.
    subject_id   uuid REFERENCES feedback_events (id),
    channel      text NOT NULL CHECK (channel IN ('log')),
    created_by   uuid NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind IN ('review_reminder', 'review_assigned')) = (subject_id IS NOT NULL))
);
CREATE INDEX account_messages_recipient_idx ON account_messages (hospital_id, recipient_id, kind, created_at);
CREATE INDEX account_messages_subject_idx ON account_messages (subject_id, kind, created_at) WHERE subject_id IS NOT NULL;
ALTER TABLE account_messages OWNER TO mhai_migrate;
ALTER TABLE account_messages ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- The gateway records and reads its own hospital's messages; nothing
-- updates or deletes one.
GRANT SELECT, INSERT ON account_messages TO mhai_gateway;
CREATE POLICY gateway_hospital ON account_messages TO mhai_gateway
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

-- The transition check, as 00011's, plus the assignment: a waiting
-- correction that stays waiting may change its assignee only, by an
-- active hospital admin of its hospital, to an active clinician of its
-- hospital other than its submitter, or to nobody. A decision or a
-- withdrawal leaves the assignee as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_feedback_transition_check() RETURNS trigger
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
    IF NEW.status = 'pending_review' THEN
        -- The assignment: nothing but the assignee changes.
        IF NEW.decided_by IS DISTINCT FROM OLD.decided_by
           OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
           OR NEW.rejection_code IS DISTINCT FROM OLD.rejection_code
           OR NEW.rejection_note IS DISTINCT FROM OLD.rejection_note THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'feedback_events: a waiting correction changes only its assignee';
        END IF;
        IF NEW.assigned_to IS NOT DISTINCT FROM OLD.assigned_to THEN
            -- A stay that changes nothing is not a transition (as 00011).
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'feedback_events: a waiting correction is approved, rejected or withdrawn';
        END IF;
        BEGIN
            PERFORM 1 FROM public.users a
            WHERE a.id = public.mhai_current_user() AND a.is_active AND a.hospital_id = NEW.hospital_id
              AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = a.id AND r.role = 'hospital_admin');
            IF NOT FOUND THEN
                RAISE EXCEPTION USING ERRCODE = 'check_violation',
                    MESSAGE = 'feedback_events: only a hospital admin of the hospital assigns a reviewer';
            END IF;
            IF NEW.assigned_to IS NOT NULL THEN
                PERFORM 1 FROM public.users u
                WHERE u.id = NEW.assigned_to AND u.is_active AND u.hospital_id = NEW.hospital_id
                  AND u.id IS DISTINCT FROM NEW.submitted_by
                  AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role = 'clinician')
                FOR SHARE OF u;
                IF NOT FOUND THEN
                    RAISE EXCEPTION USING ERRCODE = 'check_violation',
                        MESSAGE = 'feedback_events: the reviewer must be an active clinician of the hospital, not the submitter';
                END IF;
            END IF;
        END;
        RETURN NEW;
    END IF;
    IF NEW.assigned_to IS DISTINCT FROM OLD.assigned_to THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'feedback_events: a decision does not move the assignee';
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

GRANT UPDATE (assigned_to) ON feedback_events TO mhai_gateway;

-- +goose Down
REVOKE UPDATE (assigned_to) ON feedback_events FROM mhai_gateway;
UPDATE feedback_events SET assigned_to = NULL WHERE assigned_to IS NOT NULL AND status = 'pending_review';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_feedback_transition_check() RETURNS trigger
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

DROP TABLE account_messages;
ALTER TABLE users DROP CONSTRAINT users_removed_inactive;
ALTER TABLE users DROP COLUMN removed_at;
