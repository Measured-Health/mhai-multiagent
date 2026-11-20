-- The notifications outbox and deliveries (notifications design §6.2,
-- §6.3, §9; §15 PR 3; ADR 0018). Numbered 00053: main had 00050, 00051 is
-- held by the open documents PR 5 (#230) and 00052 is the notifications
-- role (PR 2).
--
-- message_outbox: #228's account_messages, renamed and widened. A row
-- still records only that a message is due (who, what kind, about which
-- correction, by whom, through which channel), written in the action's own
-- transaction by the gateway: never a token, a link, an address or any
-- text. New: channel may be email (no sender writes it until PR 4/5);
-- dedupe_key (ids and a kind only, unique per hospital); published_at (set
-- by core's drainer once the due message is on NOTIFY). account_messages
-- stays as a security-invoker view for one release, so code and tests
-- written against the old name keep working under the same RLS.
--
-- The drainer (core, after commit) reaches the outbox only through three
-- definers, as mhai_notify_outbox, a NOLOGIN role core's login reaches by
-- SET LOCAL ROLE only (WITH INHERIT FALSE, as mhai_cloud_reaper):
--   mhai_outbox_due(limit)        ids and hospitals of rows not yet
--                                 published, at most 24 hours old, and
--                                 never a link kind (invite and
--                                 password_reset go by request/reply with
--                                 their link, design §4.4; their rows are
--                                 records only);
--   mhai_outbox_published(ids)    marks them published;
--   mhai_outbox_retention()       deletes rows older than 90 days.
--
-- message_deliveries (the notifications service's): one row per message
-- and channel (UNIQUE), ids and closed codes only. A row is made only by
-- notify_claim (reserve, then check), never inserted directly; the service
-- updates its own hospital's claimed rows (column grants), and a row that
-- is sent, failed or suppressed never changes again (trigger). It keeps
-- kind and recipient_id (ids) itself and has no foreign key to the outbox,
-- so it can outlive the outbox row (1 year against 90 days) and still
-- answer "was I told?". The gateway reads its hospital's status columns.
--
-- message_send_counts: the service's rate counters (§9.4), one row per
-- hospital and hour (scope hospital, key = the hospital) or recipient and
-- hour (scope recipient), reserved by one upsert that serialises.
--
-- notify_claim(message_id, lease), EXECUTE for mhai_notify only, runs under
-- the hospital the due subject names (dbx.WithHospital): a message of
-- another hospital is not_found (NT9). It returns the recipient's address
-- only for the email channel and an active user, so the log channel never
-- sees one. notify_retention() deletes deliveries older than a year and
-- counters older than two days.
--
-- Every definer follows 00047: owner mhai_migrate, search_path pinned,
-- relations written public.<name>, no PUBLIC execute, EXECUTE for its one
-- role. Under FORCE the owner reaches the tables through owner policies.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_notify_outbox') THEN
        CREATE ROLE mhai_notify_outbox NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
GRANT mhai_notify_outbox TO mhai_app WITH INHERIT FALSE;

------------------------------------------------------------------------
-- The outbox.
------------------------------------------------------------------------
ALTER TABLE account_messages RENAME TO message_outbox;
ALTER INDEX account_messages_pkey RENAME TO message_outbox_pkey;
ALTER INDEX account_messages_recipient_idx RENAME TO message_outbox_recipient_idx;
ALTER INDEX account_messages_subject_idx RENAME TO message_outbox_subject_idx;
ALTER TABLE message_outbox DROP CONSTRAINT account_messages_channel_check;
ALTER TABLE message_outbox ADD CONSTRAINT message_outbox_channel_check CHECK (channel IN ('log', 'email'));
ALTER TABLE message_outbox ADD COLUMN dedupe_key text, ADD COLUMN published_at timestamptz;
-- Ids and a kind only (for example review_reminder:<correction>:<hour>),
-- never text: lower-case names, uuids and digits.
ALTER TABLE message_outbox ADD CONSTRAINT message_outbox_dedupe_key_shape
    CHECK (dedupe_key IS NULL OR dedupe_key ~ '^[a-z_]{1,32}(:[0-9a-f-]{1,36}){1,3}$');
-- Unique per hospital, so one hospital's key can never collide with (and
-- reveal) another's.
ALTER TABLE message_outbox ADD CONSTRAINT message_outbox_dedupe_key UNIQUE (hospital_id, dedupe_key);
CREATE INDEX message_outbox_unpublished_idx ON message_outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX message_outbox_created_idx ON message_outbox (created_at);
-- The gateway's grants (SELECT, INSERT) and its policy moved with the
-- table; published_at is never the gateway's to write.

-- The old name, for one release: the same rows under the caller's RLS.
CREATE VIEW account_messages WITH (security_invoker = true) AS
    SELECT id, hospital_id, recipient_id, kind, subject_id, channel, created_by, created_at FROM message_outbox;
ALTER VIEW account_messages OWNER TO mhai_migrate;
GRANT SELECT, INSERT ON account_messages TO mhai_gateway;

-- The definers' reach: read, mark published, delete; never insert.
CREATE POLICY owner_drain_read ON message_outbox FOR SELECT TO mhai_migrate USING (true);
CREATE POLICY owner_drain_mark ON message_outbox FOR UPDATE TO mhai_migrate USING (true) WITH CHECK (true);
CREATE POLICY owner_retention ON message_outbox FOR DELETE TO mhai_migrate USING (true);

-- +goose StatementBegin
CREATE FUNCTION mhai_outbox_due(p_limit integer) RETURNS TABLE (id uuid, hospital_id uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT o.id, o.hospital_id FROM public.message_outbox o
    WHERE o.published_at IS NULL AND o.created_at > now() - interval '24 hours'
      AND o.kind NOT IN ('invite', 'password_reset')
    ORDER BY o.created_at
    LIMIT least(greatest(coalesce(p_limit, 0), 0), 500)
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_outbox_due(integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_outbox_due(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_outbox_due(integer) TO mhai_notify_outbox;

-- +goose StatementBegin
CREATE FUNCTION mhai_outbox_published(p_ids uuid[]) RETURNS integer
    LANGUAGE sql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH m AS (
        UPDATE public.message_outbox o SET published_at = now()
        WHERE o.id = ANY (p_ids) AND o.published_at IS NULL
          AND cardinality(p_ids) <= 500
        RETURNING 1)
    SELECT count(*)::integer FROM m
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_outbox_published(uuid[]) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_outbox_published(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_outbox_published(uuid[]) TO mhai_notify_outbox;

-- +goose StatementBegin
CREATE FUNCTION mhai_outbox_retention() RETURNS integer
    LANGUAGE sql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH d AS (DELETE FROM public.message_outbox o WHERE o.created_at < now() - interval '90 days' RETURNING 1)
    SELECT count(*)::integer FROM d
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_outbox_retention() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_outbox_retention() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_outbox_retention() TO mhai_notify_outbox;

------------------------------------------------------------------------
-- Deliveries.
------------------------------------------------------------------------
CREATE TABLE message_deliveries (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id        uuid NOT NULL,
    hospital_id       uuid NOT NULL REFERENCES hospitals (id),
    recipient_id      uuid NOT NULL REFERENCES users (id),
    kind              text NOT NULL CHECK (kind IN ('invite', 'password_reset', 'mfa_reset', 'mfa_reminder',
                                                     'review_reminder', 'review_assigned')),
    channel           text NOT NULL CHECK (channel IN ('log', 'email')),
    status            text NOT NULL DEFAULT 'claimed' CHECK (status IN ('claimed', 'sent', 'failed', 'suppressed')),
    attempts          integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1000),
    next_attempt_at   timestamptz,
    lease_until       timestamptz,
    lease_id          uuid,
    outcome_code      text CHECK (outcome_code IN ('accepted', 'rejected_address', 'rate_limited', 'provider_down',
                                                   'unsubscribed', 'inactive_user', 'quiet_hours_held', 'retrying',
                                                   'channel_off', 'breaker_open')),
    -- SHA-256 of the provider's message id (PR 4/10), never the id itself.
    provider_ref_hash bytea CHECK (provider_ref_hash IS NULL OR length(provider_ref_hash) = 32),
    first_attempt_at  timestamptz,
    sent_at           timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, channel),
    CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
    CHECK (status = 'claimed' OR outcome_code IS NOT NULL)
);
CREATE INDEX message_deliveries_hospital_idx ON message_deliveries (hospital_id, recipient_id, created_at);
CREATE INDEX message_deliveries_created_idx ON message_deliveries (created_at);
ALTER TABLE message_deliveries OWNER TO mhai_migrate;
ALTER TABLE message_deliveries ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- The service: its hospital's rows, the outcome columns only; never an
-- insert (notify_claim makes rows) or a delete (notify_retention).
GRANT SELECT, UPDATE (status, attempts, next_attempt_at, lease_until, lease_id, outcome_code, provider_ref_hash,
    first_attempt_at, sent_at, updated_at) ON message_deliveries TO mhai_notify;
CREATE POLICY notify_hospital ON message_deliveries TO mhai_notify
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
-- The gateway: its hospital's outcomes, for the staff screen (PR 5).
GRANT SELECT (message_id, hospital_id, recipient_id, kind, channel, status, outcome_code, sent_at, created_at)
    ON message_deliveries TO mhai_gateway;
CREATE POLICY gateway_hospital ON message_deliveries FOR SELECT TO mhai_gateway
    USING (hospital_id = mhai_current_hospital());
-- The definers (owner): claim, re-lease and retention.
CREATE POLICY owner_claim ON message_deliveries TO mhai_migrate USING (true) WITH CHECK (true);

-- A finished delivery never changes; ids never change.
-- +goose StatementBegin
CREATE FUNCTION mhai_delivery_final_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF OLD.status <> 'claimed' THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'message_deliveries: a finished delivery does not change';
    END IF;
    IF NEW.message_id IS DISTINCT FROM OLD.message_id OR NEW.hospital_id IS DISTINCT FROM OLD.hospital_id
       OR NEW.recipient_id IS DISTINCT FROM OLD.recipient_id OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.channel IS DISTINCT FROM OLD.channel OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'message_deliveries: ids do not change';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_delivery_final_check() OWNER TO mhai_migrate;
CREATE TRIGGER message_deliveries_final BEFORE UPDATE ON message_deliveries
    FOR EACH ROW EXECUTE FUNCTION mhai_delivery_final_check();

------------------------------------------------------------------------
-- Rate counters.
------------------------------------------------------------------------
CREATE TABLE message_send_counts (
    hospital_id  uuid NOT NULL REFERENCES hospitals (id),
    scope        text NOT NULL CHECK (scope IN ('hospital', 'recipient')),
    key          uuid NOT NULL,
    window_start timestamptz NOT NULL CHECK (date_trunc('hour', window_start AT TIME ZONE 'UTC') = window_start AT TIME ZONE 'UTC'),
    n            integer NOT NULL CHECK (n >= 0),
    PRIMARY KEY (hospital_id, scope, key, window_start),
    CHECK ((scope = 'hospital') = (key = hospital_id))
);
ALTER TABLE message_send_counts OWNER TO mhai_migrate;
ALTER TABLE message_send_counts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE (n) ON message_send_counts TO mhai_notify;
CREATE POLICY notify_hospital ON message_send_counts TO mhai_notify
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY owner_retention ON message_send_counts TO mhai_migrate USING (true) WITH CHECK (true);

------------------------------------------------------------------------
-- The claim (reserve, then check) and retention.
------------------------------------------------------------------------
-- outcome: claimed (this caller holds the lease until lease_until),
-- inactive (claimed, but the recipient is deactivated, removed or of
-- another hospital: suppress it), busy (another holds the lease), held
-- (waiting until next_attempt_at), done (already sent, failed or
-- suppressed), not_found (no such message in the current hospital).
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
            greatest(d.next_attempt_at, d.lease_until), NULL::text;
        RETURN;
    END IF;
    -- Then check: the recipient now, not when the message was due.
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

-- +goose StatementBegin
CREATE FUNCTION notify_retention() RETURNS integer
    LANGUAGE sql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH d AS (DELETE FROM public.message_deliveries x WHERE x.created_at < now() - interval '1 year' RETURNING 1),
         c AS (DELETE FROM public.message_send_counts s WHERE s.window_start < now() - interval '2 days' RETURNING 1)
    SELECT ((SELECT count(*) FROM d) + (SELECT count(*) FROM c))::integer
$$;
-- +goose StatementEnd
ALTER FUNCTION notify_retention() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION notify_retention() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notify_retention() TO mhai_notify;

-- +goose Down
DROP FUNCTION notify_retention();
DROP FUNCTION notify_claim(uuid, interval);
DROP TABLE message_send_counts;
DROP TABLE message_deliveries;
DROP FUNCTION mhai_delivery_final_check();
DROP FUNCTION mhai_outbox_retention();
DROP FUNCTION mhai_outbox_published(uuid[]);
DROP FUNCTION mhai_outbox_due(integer);
-- A row on a channel the old table doesn't know can't stay.
DELETE FROM message_outbox WHERE channel <> 'log';
DROP POLICY owner_retention ON message_outbox;
DROP POLICY owner_drain_mark ON message_outbox;
DROP POLICY owner_drain_read ON message_outbox;
DROP VIEW account_messages;
DROP INDEX message_outbox_created_idx;
DROP INDEX message_outbox_unpublished_idx;
ALTER TABLE message_outbox DROP CONSTRAINT message_outbox_dedupe_key;
ALTER TABLE message_outbox DROP CONSTRAINT message_outbox_dedupe_key_shape;
ALTER TABLE message_outbox DROP COLUMN published_at, DROP COLUMN dedupe_key;
ALTER TABLE message_outbox DROP CONSTRAINT message_outbox_channel_check;
ALTER TABLE message_outbox ADD CONSTRAINT account_messages_channel_check CHECK (channel IN ('log'));
ALTER INDEX message_outbox_subject_idx RENAME TO account_messages_subject_idx;
ALTER INDEX message_outbox_recipient_idx RENAME TO account_messages_recipient_idx;
ALTER INDEX message_outbox_pkey RENAME TO account_messages_pkey;
ALTER TABLE message_outbox RENAME TO account_messages;
REVOKE mhai_notify_outbox FROM mhai_app;
