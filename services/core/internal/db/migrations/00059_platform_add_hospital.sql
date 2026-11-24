-- Adding a hospital from the platform app (POST /platform/hospitals;
-- mhai-web's "Add hospital" form, product owner 24 Nov 2026). Until now a
-- hospital was made only by the provisioning CLI (core hospitals create, as
-- mhai_provision). Numbered 00059: main has 00058 (security notices, #279) and
-- 00051 is held by the open documents PR (#230).
--
-- The hospital keeps what the form sends and the CLI never asked for:
--   - hospitals.country: an ISO 3166-1 alpha-2 code (NULL for a hospital
--     the CLI made; the page shows "").
--   - hospitals.email_domains: lower-case domains, at most 20 ('{}' for a
--     CLI hospital).
--   - hospitals.first_admin_id: the hospital admin invited with it. The
--     foreign key is deferred: the hospital row comes first (the user's
--     hospital_id needs it) and names its first admin, whose row follows
--     in the same transaction.
--
-- mhai_platform (the platform app's role) gains exactly the writes of that
-- one transaction, each held by its policy to a hospital the transaction
-- itself has just made (mhai_hospital_added_now: the hospital row's xmin
-- is this transaction's, as 00044's mhai_export_entry_mine, and it is
-- still setup_incomplete):
--   - hospitals: INSERT of id, name, short_code, time_zone, country,
--     email_domains and first_admin_id; never status (setup_incomplete),
--     is_demo (false) or created_at. A first admin is required.
--   - hospital_settings: INSERT of hospital_id and
--     require_mfa_for_clinicians, which must be true (the live site's
--     onboarding passes --require-mfa-for-clinicians to the CLI; the
--     hospital admin can change it later). Every other setting is at its
--     default (allow_trust_device off).
--   - users: INSERT of id, hospital_id, email, display_name and
--     invited_at; never a password hash, a single sign-on subject or a
--     platform account (hospital_id NULL). Only the row the new hospital
--     names as its first admin, and only as invited.
--   - user_roles: hospital_admin for that first admin only (00037's
--     platform_team_add keeps platform roles on platform accounts).
--   - account_tokens: INSERT of an invite for that first admin only,
--     created by a platform account; SELECT of the invite rows' columns
--     but never token_hash, so a hospital page can show its first
--     admin's invite and leave invitees out of its admins.
--   - message_outbox: INSERT of the invite's row for that first admin
--     only (kind invite, no subject).
--   - audit_log: hospital.created and staff.invited for that hospital,
--     with an actor.
-- Nothing is granted on an existing hospital: a platform admin can add a
-- hospital, not people to one.

-- +goose Up

ALTER TABLE hospitals
    ADD COLUMN country text CHECK (country ~ '^[A-Z]{2}$'),
    ADD COLUMN email_domains text[] NOT NULL DEFAULT '{}'
        CHECK (cardinality(email_domains) <= 20 AND array_position(email_domains, NULL) IS NULL),
    ADD COLUMN first_admin_id uuid REFERENCES users (id) DEFERRABLE INITIALLY DEFERRED;

-- +goose StatementBegin
CREATE FUNCTION mhai_hospital_added_now(p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.hospitals h
                   WHERE h.id = p_hospital AND h.status = 'setup_incomplete'
                     AND h.xmin::text::bigint = (pg_catalog.pg_current_xact_id()::text::bigint % 4294967296))
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_hospital_added_now(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_hospital_added_now(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_hospital_added_now(uuid) TO mhai_platform;

GRANT INSERT (id, name, short_code, time_zone, country, email_domains, first_admin_id) ON hospitals TO mhai_platform;
CREATE POLICY platform_add ON hospitals FOR INSERT TO mhai_platform
    WITH CHECK (status = 'setup_incomplete' AND NOT is_demo AND country IS NOT NULL AND first_admin_id IS NOT NULL);

GRANT INSERT (hospital_id, require_mfa_for_clinicians) ON hospital_settings TO mhai_platform;
CREATE POLICY platform_add ON hospital_settings FOR INSERT TO mhai_platform
    WITH CHECK (require_mfa_for_clinicians AND mhai_hospital_added_now(hospital_id));

GRANT INSERT (id, hospital_id, email, display_name, invited_at) ON users TO mhai_platform;
CREATE POLICY platform_first_admin ON users FOR INSERT TO mhai_platform
    WITH CHECK (hospital_id IS NOT NULL AND invited_at IS NOT NULL AND mhai_hospital_added_now(hospital_id)
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.id = hospital_id AND h.first_admin_id = users.id));

CREATE POLICY platform_first_admin ON user_roles FOR INSERT TO mhai_platform
    WITH CHECK (role = 'hospital_admin'
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.first_admin_id = user_id AND mhai_hospital_added_now(h.id)));

GRANT INSERT (hospital_id, user_id, purpose, token_hash, created_by, expires_at) ON account_tokens TO mhai_platform;
GRANT SELECT (id, hospital_id, user_id, purpose, created_at, expires_at, used_at, revoked_at) ON account_tokens TO mhai_platform;
CREATE POLICY platform_first_admin ON account_tokens FOR INSERT TO mhai_platform
    WITH CHECK (purpose = 'invite' AND mhai_hospital_added_now(hospital_id)
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.id = hospital_id AND h.first_admin_id = user_id)
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = created_by AND u.hospital_id IS NULL));
CREATE POLICY platform_invite_read ON account_tokens FOR SELECT TO mhai_platform USING (purpose = 'invite');

GRANT INSERT (hospital_id, recipient_id, kind, subject_id, channel, created_by) ON message_outbox TO mhai_platform;
CREATE POLICY platform_first_admin ON message_outbox FOR INSERT TO mhai_platform
    WITH CHECK (kind = 'invite' AND subject_id IS NULL AND mhai_hospital_added_now(hospital_id)
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.id = hospital_id AND h.first_admin_id = recipient_id));

CREATE POLICY platform_hospital_added_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('hospital.created', 'staff.invited') AND actor_id IS NOT NULL
                AND hospital_id IS NOT NULL AND mhai_hospital_added_now(hospital_id));

-- +goose Down

DROP POLICY platform_hospital_added_append ON audit_log;
DROP POLICY platform_first_admin ON message_outbox;
REVOKE INSERT (hospital_id, recipient_id, kind, subject_id, channel, created_by) ON message_outbox FROM mhai_platform;
DROP POLICY platform_invite_read ON account_tokens;
DROP POLICY platform_first_admin ON account_tokens;
REVOKE SELECT (id, hospital_id, user_id, purpose, created_at, expires_at, used_at, revoked_at) ON account_tokens FROM mhai_platform;
REVOKE INSERT (hospital_id, user_id, purpose, token_hash, created_by, expires_at) ON account_tokens FROM mhai_platform;
DROP POLICY platform_first_admin ON user_roles;
DROP POLICY platform_first_admin ON users;
REVOKE INSERT (id, hospital_id, email, display_name, invited_at) ON users FROM mhai_platform;
DROP POLICY platform_add ON hospital_settings;
REVOKE INSERT (hospital_id, require_mfa_for_clinicians) ON hospital_settings FROM mhai_platform;
DROP POLICY platform_add ON hospitals;
REVOKE INSERT (id, name, short_code, time_zone, country, email_domains, first_admin_id) ON hospitals FROM mhai_platform;
DROP FUNCTION mhai_hospital_added_now(uuid);

ALTER TABLE hospitals
    DROP COLUMN first_admin_id,
    DROP COLUMN email_domains,
    DROP COLUMN country;
