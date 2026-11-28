-- Platform settings, the platform notifications' seen state and platform
-- team invites (mhai-web Plan 8c, Plan 7d, Plan 7b; platform operations
-- design §7, §9; frontend requirements §14.5, §14.7, §14.8).
-- NUMBERED 00062: 00061 is the HL7 v2 file connector.
--
-- What it adds, all for mhai_platform (the platform app's role) unless
-- said otherwise, and nothing for any clinical role:
--
--   - Retention: one row per record type with its period, the least the
--     server allows and its version. The periods are today's placeholders
--     (6 years for records and documents, 1 year for waiting corrections,
--     6 years for the audit log, which can't go lower); nothing here
--     shortens one by itself. A shorter period is a request
--     (platform_setting_requests) a DIFFERENT platform admin approves: the
--     CHECK refuses an approval by the requester or after the request
--     lapsed, and a trigger refuses a shorter period unless a request of two
--     different people for exactly that type and period was approved in the
--     same transaction. No job deletes anything by these periods yet: the
--     settings record the policy (the only sweep core has is the EMR
--     retention sweep, with its own periods).
--   - The clean-up pause: one row (paused, by, at, version). Pausing only
--     ever keeps more.
--   - platform_setting_log: each settings write's reason, by whom and
--     when (the audit rows carry ids and codes only, never the text).
--   - The learning-data reset: mhai_feedback_buffer_reset marks every
--     waiting feedback_buffer row cleared (cleared_at, reset_id; never a
--     DELETE) and records the reset (platform_feedback_resets).
--   - mhai_retention_preview: how many rows a period would cover, counts
--     only, leaving out every hospital with an open legal hold. It reads
--     each hospital's rows under that hospital (the owner's read policies
--     below are keyed to mhai.hospital_id, as documents' scan definers
--     are), and the function's own SET puts mhai.hospital_id back when it
--     returns.
--   - platform_notifications_seen: which notification each platform
--     member has seen.
--   - Team invites: a platform account invited with no password (users,
--     no hospital; only with its own platform.team_invited audit entry in
--     the same transaction, checked at commit), its platform role, and an invite link with no hospital
--     (account_tokens.hospital_id may now be null, for an invite only);
--     notify_platform_link_recipient answers the notifications service the
--     address of such an invite's recipient (an active platform account
--     with a live invite made in the last 2 minutes), and nothing else.
--   - platform_settings_append: the audit rows of these writes.

-- +goose Up

------------------------------------------------------------------------
-- Retention.
------------------------------------------------------------------------

CREATE TABLE platform_retention (
    type         text PRIMARY KEY CHECK (type IN ('records', 'documents', 'pending_corrections', 'audit_log')),
    days         integer NOT NULL,
    minimum_days integer NOT NULL CHECK (minimum_days >= 1),
    version      integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    changed_by   uuid REFERENCES users (id),
    changed_at   timestamptz,
    CONSTRAINT platform_retention_minimum CHECK (days >= minimum_days AND days <= 36500),
    -- The audit log is never kept under 6 years.
    CONSTRAINT platform_retention_audit_log CHECK (type <> 'audit_log' OR minimum_days >= 2190),
    CONSTRAINT platform_retention_changed CHECK ((changed_by IS NULL) = (changed_at IS NULL))
);
INSERT INTO platform_retention (type, days, minimum_days) VALUES
    ('records', 2190, 365),
    ('documents', 2190, 365),
    ('pending_corrections', 365, 30),
    ('audit_log', 2190, 2190);
ALTER TABLE platform_retention OWNER TO mhai_migrate;
ALTER TABLE platform_retention ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- Never the type or the minimum; no insert or delete.
GRANT SELECT, UPDATE (days, version, changed_by, changed_at) ON platform_retention TO mhai_platform;
CREATE POLICY platform_all ON platform_retention TO mhai_platform
    USING (true)
    WITH CHECK (changed_by IS NULL OR EXISTS (SELECT 1 FROM users u WHERE u.id = changed_by AND u.hospital_id IS NULL));

CREATE TABLE platform_setting_requests (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Triage can't be switched on (core has no triage), so a shorter
    -- retention period is the only request there is.
    kind         text NOT NULL CHECK (kind = 'retention_shorten'),
    type         text NOT NULL REFERENCES platform_retention (type),
    from_days    integer NOT NULL,
    to_days      integer NOT NULL CHECK (to_days >= 1),
    reason       text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    requested_by uuid NOT NULL REFERENCES users (id),
    requested_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    approved_by  uuid REFERENCES users (id),
    approved_at  timestamptz,
    ended_by     uuid REFERENCES users (id),
    ended_at     timestamptz,
    end_reason   text CHECK (end_reason IN ('cancelled', 'lapsed', 'superseded', 'requester_left')),
    CONSTRAINT platform_setting_requests_shorter CHECK (to_days < from_days),
    CONSTRAINT platform_setting_requests_two_people CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT platform_setting_requests_approved CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
    CONSTRAINT platform_setting_requests_in_time CHECK (approved_at IS NULL OR approved_at < expires_at),
    CONSTRAINT platform_setting_requests_one_end CHECK (approved_at IS NULL OR ended_at IS NULL),
    CONSTRAINT platform_setting_requests_ended CHECK ((ended_at IS NULL) = (end_reason IS NULL)),
    CONSTRAINT platform_setting_requests_expiry CHECK (expires_at > requested_at AND expires_at <= requested_at + interval '7 days')
);
-- One open request per type.
CREATE UNIQUE INDEX platform_setting_requests_open ON platform_setting_requests (type)
    WHERE approved_at IS NULL AND ended_at IS NULL;
ALTER TABLE platform_setting_requests OWNER TO mhai_migrate;
ALTER TABLE platform_setting_requests ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT (kind, type, from_days, to_days, reason, requested_by, expires_at),
    UPDATE (approved_by, approved_at, ended_by, ended_at, end_reason) ON platform_setting_requests TO mhai_platform;
CREATE POLICY platform_all ON platform_setting_requests TO mhai_platform
    USING (true)
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = requested_by AND u.hospital_id IS NULL)
                AND (approved_by IS NULL OR EXISTS (SELECT 1 FROM users u WHERE u.id = approved_by AND u.hospital_id IS NULL)));
-- An ended or approved request never changes again.
CREATE POLICY platform_open_only ON platform_setting_requests AS RESTRICTIVE FOR UPDATE TO mhai_platform
    USING (approved_at IS NULL AND ended_at IS NULL) WITH CHECK (true);

-- A shorter period only with a request of two different people for that
-- type and period, approved in this transaction. A longer one, or the
-- same, needs nothing: keeping more is safe.
-- +goose StatementBegin
CREATE FUNCTION mhai_retention_shorten_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.type IS DISTINCT FROM OLD.type OR NEW.minimum_days IS DISTINCT FROM OLD.minimum_days THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'platform_retention: the type and minimum do not change';
    END IF;
    IF NEW.days < OLD.days AND NOT EXISTS (
        SELECT 1 FROM public.platform_setting_requests q
        WHERE q.type = NEW.type AND q.from_days = OLD.days AND q.to_days = NEW.days
          AND q.approved_at IS NOT NULL AND q.approved_by <> q.requested_by
          AND q.xmin::text::bigint = (pg_catalog.pg_current_xact_id()::text::bigint % 4294967296)) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'platform_retention: a shorter period needs a request approved by a different platform admin';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_retention_shorten_check() OWNER TO mhai_migrate;
CREATE TRIGGER platform_retention_shorten BEFORE UPDATE ON platform_retention
    FOR EACH ROW EXECUTE FUNCTION mhai_retention_shorten_check();

CREATE TABLE platform_retention_pause (
    one       boolean PRIMARY KEY DEFAULT true CHECK (one),
    paused    boolean NOT NULL DEFAULT false,
    paused_by uuid REFERENCES users (id),
    paused_at timestamptz,
    version   integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    CONSTRAINT platform_retention_pause_who CHECK (paused = (paused_by IS NOT NULL) AND paused = (paused_at IS NOT NULL))
);
INSERT INTO platform_retention_pause DEFAULT VALUES;
ALTER TABLE platform_retention_pause OWNER TO mhai_migrate;
ALTER TABLE platform_retention_pause ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, UPDATE (paused, paused_by, paused_at, version) ON platform_retention_pause TO mhai_platform;
CREATE POLICY platform_all ON platform_retention_pause TO mhai_platform
    USING (true)
    WITH CHECK (paused_by IS NULL OR EXISTS (SELECT 1 FROM users u WHERE u.id = paused_by AND u.hospital_id IS NULL));

-- Each settings write's reason (append only).
CREATE TABLE platform_setting_log (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action text NOT NULL CHECK (action IN ('retention_changed', 'setting_change_requested', 'clean_up_paused',
                                           'clean_up_resumed', 'triage_switched_off')),
    by     uuid NOT NULL REFERENCES users (id),
    at     timestamptz NOT NULL DEFAULT now(),
    reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500)
);
ALTER TABLE platform_setting_log OWNER TO mhai_migrate;
ALTER TABLE platform_setting_log ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT (action, by, reason) ON platform_setting_log TO mhai_platform;
CREATE POLICY platform_all ON platform_setting_log TO mhai_platform
    USING (true) WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = by AND u.hospital_id IS NULL));

-- How many rows of a type a period of p_days would cover: older than it,
-- outside every hospital with an open legal hold. Counts only. Each
-- hospital is counted under its own mhai.hospital_id (the owner's read
-- policies below are keyed to it); the SET clause restores the caller's
-- value when the function returns.
CREATE POLICY retention_preview_read ON clinical_decisions FOR SELECT TO mhai_migrate
    USING (hospital_id = mhai_current_hospital());
CREATE POLICY retention_preview_read ON feedback_events FOR SELECT TO mhai_migrate
    USING (hospital_id = mhai_current_hospital());
-- +goose StatementBegin
CREATE FUNCTION mhai_retention_preview(p_type text, p_days integer) RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    SET mhai.hospital_id = ''
    AS $$
DECLARE
    h      uuid;
    n      bigint := 0;
    before timestamptz;
BEGIN
    IF p_days IS NULL OR p_days < 1 OR p_days > 36500
       OR p_type NOT IN ('records', 'documents', 'pending_corrections', 'audit_log') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'mhai_retention_preview: bad type or period';
    END IF;
    before := now() - make_interval(days => p_days);
    IF p_type = 'audit_log' THEN
        RETURN (SELECT count(*) FROM public.audit_log a WHERE a.ts < before);
    END IF;
    FOR h IN SELECT x.id FROM public.hospitals x
             WHERE NOT EXISTS (SELECT 1 FROM public.legal_holds l WHERE l.hospital_id = x.id AND l.lifted_at IS NULL)
    LOOP
        PERFORM pg_catalog.set_config('mhai.hospital_id', h::text, true);
        IF p_type = 'records' THEN
            n := n + (SELECT count(*) FROM public.clinical_decisions c WHERE c.hospital_id = h AND c.created_at < before);
        ELSIF p_type = 'documents' THEN
            n := n + (SELECT count(*) FROM public.documents d
                      WHERE d.hospital_id = h AND d.status <> 'purged' AND d.received_at < before);
        ELSE
            n := n + (SELECT count(*) FROM public.feedback_events f
                      WHERE f.hospital_id = h AND f.status = 'pending_review' AND f.created_at < before);
        END IF;
    END LOOP;
    RETURN n;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_retention_preview(text, integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_retention_preview(text, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_retention_preview(text, integer) TO mhai_platform;

------------------------------------------------------------------------
-- The learning-data reset.
------------------------------------------------------------------------

CREATE TABLE platform_feedback_resets (
    id      uuid PRIMARY KEY,
    by      uuid NOT NULL REFERENCES users (id),
    at      timestamptz NOT NULL DEFAULT now(),
    reason  text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    cleared integer NOT NULL CHECK (cleared >= 0)
);
ALTER TABLE platform_feedback_resets OWNER TO mhai_migrate;
ALTER TABLE platform_feedback_resets ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT ON platform_feedback_resets TO mhai_platform;
CREATE POLICY platform_read ON platform_feedback_resets FOR SELECT TO mhai_platform USING (true);
CREATE POLICY owner_write ON platform_feedback_resets FOR INSERT TO mhai_migrate WITH CHECK (true);
-- The owner clears waiting rows only, and only by marking them.
CREATE POLICY owner_reset ON feedback_buffer FOR UPDATE TO mhai_migrate
    USING (cleared_at IS NULL) WITH CHECK (cleared_at IS NOT NULL AND reset_id IS NOT NULL);
-- An UPDATE's new row must also pass the owner's read policies: the rows a
-- reset cleared (scores and ids, no text).
CREATE POLICY owner_reset_read ON feedback_buffer FOR SELECT TO mhai_migrate
    USING (cleared_at IS NOT NULL AND reset_id IS NOT NULL);

-- Marks every waiting row cleared and records the reset, by p_by (an
-- active platform admin); returns the reset's id and how many it cleared.
-- +goose StatementBegin
CREATE FUNCTION mhai_feedback_buffer_reset(p_by uuid, p_reason text) RETURNS TABLE (reset_id uuid, cleared integer)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    rid uuid := gen_random_uuid();
    n   integer;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id = p_by AND u.hospital_id IS NULL AND u.is_active
                   AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role = 'platform_admin')) THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege', MESSAGE = 'mhai_feedback_buffer_reset: not a platform admin';
    END IF;
    UPDATE public.feedback_buffer b SET cleared_at = now(), reset_id = rid WHERE b.cleared_at IS NULL;
    GET DIAGNOSTICS n = ROW_COUNT;
    INSERT INTO public.platform_feedback_resets (id, by, reason, cleared) VALUES (rid, p_by, p_reason, n);
    RETURN QUERY SELECT rid, n;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_feedback_buffer_reset(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_feedback_buffer_reset(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_feedback_buffer_reset(uuid, text) TO mhai_platform;

------------------------------------------------------------------------
-- Notifications: who has seen what.
------------------------------------------------------------------------

CREATE TABLE platform_notifications_seen (
    user_id         uuid NOT NULL REFERENCES users (id),
    notification_id text NOT NULL CHECK (notification_id ~ '^ntf_(audit_)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    seen_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, notification_id)
);
ALTER TABLE platform_notifications_seen OWNER TO mhai_migrate;
ALTER TABLE platform_notifications_seen ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT (user_id, notification_id) ON platform_notifications_seen TO mhai_platform;
CREATE POLICY platform_all ON platform_notifications_seen TO mhai_platform
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL));

------------------------------------------------------------------------
-- Team invites.
------------------------------------------------------------------------

-- A platform account's invite has no hospital; a reset link always has one.
ALTER TABLE account_tokens ALTER COLUMN hospital_id DROP NOT NULL;
ALTER TABLE account_tokens ADD CONSTRAINT account_tokens_platform_invite CHECK (hospital_id IS NOT NULL OR purpose = 'invite');

-- The invited account: no hospital, no password, invited now. The team
-- list reads invited_at (an invited member is pending until they sign in).
GRANT SELECT (invited_at) ON users TO mhai_platform;
CREATE POLICY platform_team_invite ON users FOR INSERT TO mhai_platform
    WITH CHECK (hospital_id IS NULL AND invited_at IS NOT NULL);
-- An invited platform account exists only with its own audit entry,
-- written in the same transaction (its xmin) by a person: checked at
-- commit, so a stray insert with no platform.team_invited entry for that
-- account is rolled back.
-- +goose StatementBegin
CREATE FUNCTION mhai_platform_invite_audited() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.action = 'platform.team_invited' AND a.entity = 'user' AND a.entity_id = NEW.id::text
                     AND a.actor_id IS NOT NULL AND a.hospital_id IS NULL
                     AND a.xmin::text::bigint = (pg_catalog.pg_current_xact_id()::text::bigint % 4294967296)) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'users: an invited platform account needs its platform.team_invited entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_platform_invite_audited() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_platform_invite_audited() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER users_platform_invite_audited AFTER INSERT ON users
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.hospital_id IS NULL AND NEW.invited_at IS NOT NULL)
    EXECUTE FUNCTION mhai_platform_invite_audited();
-- Its link: an invite with no hospital, for an invited platform account
-- that has never signed in, made by a platform account.
CREATE POLICY platform_team_invite ON account_tokens FOR INSERT TO mhai_platform
    WITH CHECK (purpose = 'invite' AND hospital_id IS NULL
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL AND u.invited_at IS NOT NULL
                            AND u.last_login_at IS NULL)
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = created_by AND u.hospital_id IS NULL));

-- The address of a platform invite's recipient, for the notifications
-- service's send_platform_link: an active platform account with a live
-- invite made in the last 2 minutes (a stale or replayed request names
-- nobody). No hospital is set, so nothing hospital-scoped is read.
-- +goose StatementBegin
CREATE FUNCTION notify_platform_link_recipient(p_user uuid, p_purpose text) RETURNS TABLE (address text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT u.email::text
    FROM public.users u
    WHERE u.id = p_user AND u.hospital_id IS NULL AND u.is_active AND u.removed_at IS NULL
      AND p_purpose = 'invite'
      AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role IN ('platform_admin', 'auditor'))
      AND EXISTS (SELECT 1 FROM public.account_tokens t
                  WHERE t.user_id = u.id AND t.hospital_id IS NULL AND t.purpose = 'invite'
                    AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > now()
                    AND t.created_at > now() - interval '2 minutes')
$$;
-- +goose StatementEnd
ALTER FUNCTION notify_platform_link_recipient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION notify_platform_link_recipient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notify_platform_link_recipient(uuid, text) TO mhai_notify;

------------------------------------------------------------------------
-- The audit rows of these writes.
------------------------------------------------------------------------

CREATE POLICY platform_settings_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('platform.retention_changed', 'platform.setting_change_requested',
                           'platform.setting_change_approved', 'platform.setting_change_cancelled',
                           'platform.setting_change_lapsed', 'platform.clean_up_paused', 'platform.clean_up_resumed',
                           'platform.feedback_buffer_cleared', 'platform.team_invited')
                AND hospital_id IS NULL AND actor_id IS NOT NULL);

-- +goose Down
DROP POLICY platform_settings_append ON audit_log;
DROP FUNCTION notify_platform_link_recipient(uuid, text);
DROP POLICY platform_team_invite ON account_tokens;
DROP TRIGGER users_platform_invite_audited ON users;
DROP FUNCTION mhai_platform_invite_audited();
DROP POLICY platform_team_invite ON users;
REVOKE SELECT (invited_at) ON users FROM mhai_platform;
-- Platform invites go first: a reset link always had a hospital.
DELETE FROM account_tokens WHERE hospital_id IS NULL;
ALTER TABLE account_tokens DROP CONSTRAINT account_tokens_platform_invite;
ALTER TABLE account_tokens ALTER COLUMN hospital_id SET NOT NULL;
DROP TABLE platform_notifications_seen;
DROP FUNCTION mhai_feedback_buffer_reset(uuid, text);
DROP POLICY owner_reset_read ON feedback_buffer;
DROP POLICY owner_reset ON feedback_buffer;
DROP TABLE platform_feedback_resets;
DROP FUNCTION mhai_retention_preview(text, integer);
DROP POLICY retention_preview_read ON feedback_events;
DROP POLICY retention_preview_read ON clinical_decisions;
DROP TABLE platform_setting_log;
DROP TABLE platform_retention_pause;
DROP TRIGGER platform_retention_shorten ON platform_retention;
DROP FUNCTION mhai_retention_shorten_check();
DROP TABLE platform_setting_requests;
DROP TABLE platform_retention;
