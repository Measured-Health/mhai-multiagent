-- Security notice emails (product owner, 24 Nov 2026). Numbered 00058:
-- main had 00056, 00051 is held by the open documents PR 5 (#230) and
-- 00057 by the two-step replace (#277).
--
-- A person is told, at their own address, when their account's security
-- changes: their password changed (by them) or set with a reset link, two-step
-- turned on, two-step moved to a new authenticator app, a backup code used,
-- new backup codes made. (A hospital admin's two-step reset already sends
-- mfa_reset.) One outbox kind per event, so the template, the delivery and
-- the "was I told?" answer stay keyed by kind as every other message is.
--
-- The auth routes run as mhai_auth, which has no hospital set and no grant
-- on the outbox. They record a notice only through one definer:
--   mhai_security_notice(user, kind, channel)
-- EXECUTE for mhai_auth only. It takes a security kind only, finds the
-- recipient's hospital itself (never from the caller), records nothing for
-- a platform account (no hospital: the outbox is per hospital), an inactive
-- or removed one, and writes the row in the caller's transaction, so a
-- change that rolls back leaves no notice. At most one notice per kind,
-- account and calendar minute (the outbox's dedupe key, unique per
-- hospital); a second in the same minute is dropped. It returns the
-- recipient's hospital when a row was written, else null. The row holds ids
-- and the kind only, as every outbox row.
--
-- notify_claim gains created_at: the time the change happened, which the
-- notice states. Its other columns and checks are unchanged.

-- +goose Up

-- The kinds, on the outbox and on deliveries.
ALTER TABLE message_outbox DROP CONSTRAINT account_messages_kind_check;
ALTER TABLE message_outbox ADD CONSTRAINT message_outbox_kind_check CHECK (kind IN ('invite', 'password_reset', 'mfa_reset',
    'mfa_reminder', 'review_reminder', 'review_assigned', 'security_password_changed', 'security_password_reset',
    'security_mfa_enabled', 'security_mfa_replaced', 'security_backup_code_used', 'security_backup_codes_made'));
ALTER TABLE message_deliveries DROP CONSTRAINT message_deliveries_kind_check;
ALTER TABLE message_deliveries ADD CONSTRAINT message_deliveries_kind_check CHECK (kind IN ('invite', 'password_reset', 'mfa_reset',
    'mfa_reminder', 'review_reminder', 'review_assigned', 'security_password_changed', 'security_password_reset',
    'security_mfa_enabled', 'security_mfa_replaced', 'security_backup_code_used', 'security_backup_codes_made'));

-- The definer's reach: insert a security notice, nothing else.
CREATE POLICY owner_security_notice ON message_outbox FOR INSERT TO mhai_migrate
    WITH CHECK (kind LIKE 'security\_%' AND recipient_id = created_by AND subject_id IS NULL);

-- +goose StatementBegin
CREATE FUNCTION mhai_security_notice(p_user uuid, p_kind text, p_channel text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid;
BEGIN
    IF p_kind IS NULL OR p_kind NOT IN ('security_password_changed', 'security_password_reset', 'security_mfa_enabled',
                                        'security_mfa_replaced', 'security_backup_code_used', 'security_backup_codes_made') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'mhai_security_notice: not a security notice kind';
    END IF;
    IF p_channel IS NULL OR p_channel NOT IN ('log', 'email') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'mhai_security_notice: unknown channel';
    END IF;
    SELECT u.hospital_id INTO h FROM public.users u
    WHERE u.id = p_user AND u.is_active AND u.removed_at IS NULL AND u.hospital_id IS NOT NULL;
    IF h IS NULL THEN
        RETURN NULL;
    END IF;
    INSERT INTO public.message_outbox (hospital_id, recipient_id, kind, channel, created_by, dedupe_key)
    VALUES (h, p_user, p_kind, p_channel, p_user,
            p_kind || ':' || p_user::text || ':' || floor(extract(epoch FROM now()) / 60)::bigint::text)
    ON CONFLICT ON CONSTRAINT message_outbox_dedupe_key DO NOTHING;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    RETURN h;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_security_notice(uuid, text, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_security_notice(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_security_notice(uuid, text, text) TO mhai_auth;

-- notify_claim, as 00053's, plus the outbox row's created_at.
DROP FUNCTION notify_claim(uuid, interval);
-- +goose StatementBegin
CREATE FUNCTION notify_claim(p_message_id uuid, p_lease interval)
    RETURNS TABLE (outcome text, lease_id uuid, kind text, recipient_id uuid, subject_id uuid, channel text,
                   attempts integer, first_attempt_at timestamptz, next_attempt_at timestamptz, address text,
                   created_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h   uuid := public.mhai_current_hospital();
    lid uuid := gen_random_uuid();
    m   public.message_outbox%ROWTYPE;
    d   public.message_deliveries%ROWTYPE;
    ok  boolean;
    em  text;
BEGIN
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege', MESSAGE = 'notify_claim: no hospital';
    END IF;
    IF p_lease IS NULL OR p_lease < interval '10 seconds' OR p_lease > interval '10 minutes' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'notify_claim: the lease is 10 s to 10 min';
    END IF;
    SELECT o.* INTO m FROM public.message_outbox o WHERE o.id = p_message_id AND o.hospital_id = h;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::uuid, NULL::text, NULL::uuid, NULL::uuid, NULL::text,
            NULL::integer, NULL::timestamptz, NULL::timestamptz, NULL::text, NULL::timestamptz;
        RETURN;
    END IF;
    -- Reserve: make the delivery, or take its lease if it is free and due.
    INSERT INTO public.message_deliveries AS x (message_id, hospital_id, recipient_id, kind, channel, lease_until, lease_id)
    VALUES (m.id, m.hospital_id, m.recipient_id, m.kind, m.channel, now() + p_lease, lid)
    ON CONFLICT ON CONSTRAINT message_deliveries_message_id_channel_key DO UPDATE
        SET lease_until = now() + p_lease, lease_id = lid, updated_at = now()
        WHERE x.status = 'claimed' AND (x.lease_until IS NULL OR x.lease_until <= now())
          AND (x.next_attempt_at IS NULL OR x.next_attempt_at <= now())
    RETURNING x.* INTO d;
    IF NOT FOUND THEN
        SELECT x.* INTO d FROM public.message_deliveries x WHERE x.message_id = m.id AND x.channel = m.channel;
        RETURN QUERY SELECT
            CASE WHEN d.status <> 'claimed' THEN 'done'
                 WHEN d.lease_until IS NOT NULL AND d.lease_until > now() THEN 'busy'
                 ELSE 'held' END,
            NULL::uuid, m.kind, m.recipient_id, m.subject_id, m.channel, d.attempts, d.first_attempt_at,
            greatest(d.next_attempt_at, d.lease_until), NULL::text, m.created_at;
        RETURN;
    END IF;
    -- Then check: the recipient now, not when the message was due.
    SELECT u.is_active AND u.removed_at IS NULL AND u.hospital_id = m.hospital_id, u.email INTO ok, em
    FROM public.users u WHERE u.id = m.recipient_id;
    RETURN QUERY SELECT CASE WHEN coalesce(ok, false) THEN 'claimed' ELSE 'inactive' END,
        lid, m.kind, m.recipient_id, m.subject_id, m.channel, d.attempts, d.first_attempt_at, d.next_attempt_at,
        CASE WHEN coalesce(ok, false) AND m.channel = 'email' THEN em END, m.created_at;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION notify_claim(uuid, interval) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION notify_claim(uuid, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notify_claim(uuid, interval) TO mhai_notify;

-- +goose Down
DROP FUNCTION notify_claim(uuid, interval);
-- +goose StatementBegin
CREATE FUNCTION notify_claim(p_message_id uuid, p_lease interval)
    RETURNS TABLE (outcome text, lease_id uuid, kind text, recipient_id uuid, subject_id uuid, channel text,
                   attempts integer, first_attempt_at timestamptz, next_attempt_at timestamptz, address text)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h   uuid := public.mhai_current_hospital();
    lid uuid := gen_random_uuid();
    m   public.message_outbox%ROWTYPE;
    d   public.message_deliveries%ROWTYPE;
    ok  boolean;
    em  text;
BEGIN
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege', MESSAGE = 'notify_claim: no hospital';
    END IF;
    IF p_lease IS NULL OR p_lease < interval '10 seconds' OR p_lease > interval '10 minutes' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'notify_claim: the lease is 10 s to 10 min';
    END IF;
    SELECT o.* INTO m FROM public.message_outbox o WHERE o.id = p_message_id AND o.hospital_id = h;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::uuid, NULL::text, NULL::uuid, NULL::uuid, NULL::text,
            NULL::integer, NULL::timestamptz, NULL::timestamptz, NULL::text;
        RETURN;
    END IF;
    INSERT INTO public.message_deliveries AS x (message_id, hospital_id, recipient_id, kind, channel, lease_until, lease_id)
    VALUES (m.id, m.hospital_id, m.recipient_id, m.kind, m.channel, now() + p_lease, lid)
    ON CONFLICT ON CONSTRAINT message_deliveries_message_id_channel_key DO UPDATE
        SET lease_until = now() + p_lease, lease_id = lid, updated_at = now()
        WHERE x.status = 'claimed' AND (x.lease_until IS NULL OR x.lease_until <= now())
          AND (x.next_attempt_at IS NULL OR x.next_attempt_at <= now())
    RETURNING x.* INTO d;
    IF NOT FOUND THEN
        SELECT x.* INTO d FROM public.message_deliveries x WHERE x.message_id = m.id AND x.channel = m.channel;
        RETURN QUERY SELECT
            CASE WHEN d.status <> 'claimed' THEN 'done'
                 WHEN d.lease_until IS NOT NULL AND d.lease_until > now() THEN 'busy'
                 ELSE 'held' END,
            NULL::uuid, m.kind, m.recipient_id, m.subject_id, m.channel, d.attempts, d.first_attempt_at,
            greatest(d.next_attempt_at, d.lease_until), NULL::text;
        RETURN;
    END IF;
    SELECT u.is_active AND u.removed_at IS NULL AND u.hospital_id = m.hospital_id, u.email INTO ok, em
    FROM public.users u WHERE u.id = m.recipient_id;
    RETURN QUERY SELECT CASE WHEN coalesce(ok, false) THEN 'claimed' ELSE 'inactive' END,
        lid, m.kind, m.recipient_id, m.subject_id, m.channel, d.attempts, d.first_attempt_at, d.next_attempt_at,
        CASE WHEN coalesce(ok, false) AND m.channel = 'email' THEN em END;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION notify_claim(uuid, interval) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION notify_claim(uuid, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notify_claim(uuid, interval) TO mhai_notify;

DROP FUNCTION mhai_security_notice(uuid, text, text);
DROP POLICY owner_security_notice ON message_outbox;
-- Rows of a kind the old checks don't know can't stay.
DELETE FROM message_deliveries WHERE kind LIKE 'security\_%';
DELETE FROM message_outbox WHERE kind LIKE 'security\_%';
ALTER TABLE message_deliveries DROP CONSTRAINT message_deliveries_kind_check;
ALTER TABLE message_deliveries ADD CONSTRAINT message_deliveries_kind_check CHECK (kind IN ('invite', 'password_reset',
    'mfa_reset', 'mfa_reminder', 'review_reminder', 'review_assigned'));
ALTER TABLE message_outbox DROP CONSTRAINT message_outbox_kind_check;
ALTER TABLE message_outbox ADD CONSTRAINT account_messages_kind_check CHECK (kind IN ('invite', 'password_reset',
    'mfa_reset', 'mfa_reminder', 'review_reminder', 'review_assigned'));
