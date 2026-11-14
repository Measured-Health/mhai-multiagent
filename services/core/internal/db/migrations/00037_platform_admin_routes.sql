-- The platform app's team, legal holds and analytics e-mail setting
-- (mhai-web Plan 7b §9, Plan 8a, Plan 7d; platform admin design §9, §13).
-- NUMBERED 00037: 00036 is the hospitals list and page.
--
-- What it adds, all for mhai_platform (the platform app's role) and nothing
-- for any clinical role:
--
--   - The team: a platform account's activity flag (users.is_active, only
--     for accounts with no hospital), its platform role rows (user_roles,
--     platform_admin and auditor only, of accounts with no hospital), its
--     sessions' revoked_at, its two-step flag (through a definer: nothing on user_mfa)
--     and platform_member_versions (the role's version, for "changed by
--     someone else"). Core serialises every team write with a transaction
--     advisory lock and checks the last-admin and own-role rules under it.
--   - Legal holds: the platform places a hold (reason code, review date,
--     who placed it; never a name or a note) and lifts one with two
--     different platform admins (legal_hold_lifts: the request, then the
--     approval; the CHECK refuses a requester approving their own, and a
--     hold is lifted only with such an approved request). The
--     provisioning CLI (00032) keeps placing and lifting as before.
--   - platform_email_prefs: a platform admin's analytics e-mail mode.
--   - platform_admin_append: a second permissive audit policy for these
--     actions only, so it never replaces another migration's
--     platform_append list. Rows carry no hospital (as every platform
--     action); the hospital is in the payload.

-- +goose Up

------------------------------------------------------------------------
-- The team.
------------------------------------------------------------------------
CREATE TABLE platform_member_versions (
    user_id uuid PRIMARY KEY REFERENCES users (id),
    version integer NOT NULL DEFAULT 1 CHECK (version >= 1)
);
ALTER TABLE platform_member_versions OWNER TO mhai_migrate;
ALTER TABLE platform_member_versions ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE (version) ON platform_member_versions TO mhai_platform;
CREATE POLICY platform_all ON platform_member_versions TO mhai_platform
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL));

GRANT UPDATE (is_active) ON users TO mhai_platform;
-- Deactivation only: no route reactivates an account (the CLI does).
CREATE POLICY platform_team_update ON users FOR UPDATE TO mhai_platform
    USING (hospital_id IS NULL) WITH CHECK (hospital_id IS NULL AND NOT is_active);

GRANT INSERT, DELETE ON user_roles TO mhai_platform;
CREATE POLICY platform_team_add ON user_roles FOR INSERT TO mhai_platform
    WITH CHECK (role IN ('platform_admin', 'auditor')
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL));
CREATE POLICY platform_team_drop ON user_roles FOR DELETE TO mhai_platform
    USING (role IN ('platform_admin', 'auditor')
           AND EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL));

GRANT UPDATE (revoked_at) ON sessions TO mhai_platform;
-- Open sessions only: a revoked session's time is never rewritten.
CREATE POLICY platform_team_revoke ON sessions FOR UPDATE TO mhai_platform
    USING (revoked_at IS NULL AND EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL))
    WITH CHECK (revoked_at IS NOT NULL);

-- A platform account's two-step flag, through a definer: mhai_platform
-- holds nothing on user_mfa (its secrets and backup codes).
CREATE POLICY platform_mfa_owner ON user_mfa FOR SELECT TO mhai_migrate USING (true);
-- +goose StatementBegin
CREATE FUNCTION mhai_platform_mfa_enabled(uid uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT COALESCE((SELECT m.enabled FROM public.user_mfa m JOIN public.users u ON u.id = m.user_id
                     WHERE m.user_id = uid AND u.hospital_id IS NULL), false)
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_platform_mfa_enabled(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_platform_mfa_enabled(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_platform_mfa_enabled(uuid) TO mhai_platform;

------------------------------------------------------------------------
-- Legal holds.
------------------------------------------------------------------------
ALTER TABLE legal_holds
    ADD COLUMN reason    text CHECK (reason IN ('court_case', 'regulator', 'complaint', 'investigation', 'other')),
    ADD COLUMN review_on date,
    ADD COLUMN placed_by uuid REFERENCES users (id),
    ADD COLUMN version   integer NOT NULL DEFAULT 1 CHECK (version >= 1);
GRANT SELECT (reason, review_on, placed_by, version) ON legal_holds TO mhai_platform;
GRANT INSERT (hospital_id, reference, reason, review_on, placed_by), UPDATE (lifted_at, version)
    ON legal_holds TO mhai_platform;
CREATE POLICY platform_place ON legal_holds FOR INSERT TO mhai_platform
    WITH CHECK (lifted_at IS NULL AND reason IS NOT NULL AND review_on IS NOT NULL AND placed_by IS NOT NULL);

CREATE TABLE legal_hold_lifts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hold_id      uuid NOT NULL REFERENCES legal_holds (id),
    reason       text NOT NULL CHECK (reason IN ('case_closed', 'withdrawn', 'mistake', 'other')),
    -- A document reference, never a name (as a hold's reference).
    authority    text NOT NULL CHECK (authority ~ '^[A-Z]{2,8}-[0-9]{4}-[0-9]{2,8}$'),
    requested_by uuid NOT NULL REFERENCES users (id),
    requested_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    approved_by  uuid REFERENCES users (id),
    approved_at  timestamptz,
    -- Cancelled with no one: it lapsed.
    cancelled_by uuid REFERENCES users (id),
    cancelled_at timestamptz,
    CONSTRAINT legal_hold_lifts_two_people CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT legal_hold_lifts_approved CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
    CONSTRAINT legal_hold_lifts_one_end CHECK (approved_at IS NULL OR cancelled_at IS NULL),
    CONSTRAINT legal_hold_lifts_expiry CHECK (expires_at > requested_at)
);
ALTER TABLE legal_hold_lifts OWNER TO mhai_migrate;
ALTER TABLE legal_hold_lifts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON legal_hold_lifts FOR SELECT TO mhai_migrate USING (true);

-- Whether hold hid has an approved lift request of two different people.
-- +goose StatementBegin
CREATE FUNCTION mhai_hold_lift_approved(hid uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.legal_hold_lifts k
                   WHERE k.hold_id = hid AND k.approved_at IS NOT NULL AND k.approved_by <> k.requested_by)
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_hold_lift_approved(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_hold_lift_approved(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_hold_lift_approved(uuid) TO mhai_platform;

-- A lifted hold never changes again (as 00032's provision_lift), and the
-- platform lifts one only with an approved request of two different
-- people (legal_hold_lifts, above; the function reads it as its owner).
CREATE POLICY platform_lift ON legal_holds FOR UPDATE TO mhai_platform
    USING (lifted_at IS NULL)
    WITH CHECK (lifted_at IS NULL OR mhai_hold_lift_approved(id));
CREATE UNIQUE INDEX legal_hold_lifts_open ON legal_hold_lifts (hold_id) WHERE approved_at IS NULL AND cancelled_at IS NULL;
GRANT SELECT, INSERT (hold_id, reason, authority, requested_by, expires_at),
    UPDATE (approved_by, approved_at, cancelled_by, cancelled_at) ON legal_hold_lifts TO mhai_platform;
CREATE POLICY platform_all ON legal_hold_lifts TO mhai_platform USING (true) WITH CHECK (true);
-- An ended request never changes again.
CREATE POLICY platform_open_only ON legal_hold_lifts AS RESTRICTIVE FOR UPDATE TO mhai_platform
    USING (approved_at IS NULL AND cancelled_at IS NULL) WITH CHECK (true);

-- A hold's history is its audit rows: read by hold id.
CREATE INDEX audit_log_legal_hold ON audit_log ((payload ->> 'hold_id'))
    WHERE action IN ('legal_hold.placed', 'legal_hold.lift_requested', 'legal_hold.lift_cancelled', 'legal_hold.lifted');

------------------------------------------------------------------------
-- The analytics e-mail setting.
------------------------------------------------------------------------
CREATE TABLE platform_email_prefs (
    user_id uuid PRIMARY KEY REFERENCES users (id),
    mode    text NOT NULL CHECK (mode IN ('off', 'critical', 'critical_and_digest')),
    version integer NOT NULL DEFAULT 1 CHECK (version >= 1)
);
ALTER TABLE platform_email_prefs OWNER TO mhai_migrate;
ALTER TABLE platform_email_prefs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE (mode, version) ON platform_email_prefs TO mhai_platform;
CREATE POLICY platform_all ON platform_email_prefs TO mhai_platform
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id AND u.hospital_id IS NULL));

------------------------------------------------------------------------
-- The audit rows of these writes.
------------------------------------------------------------------------
CREATE POLICY platform_admin_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('legal_hold.placed', 'legal_hold.lift_requested', 'legal_hold.lift_cancelled',
                           'legal_hold.lifted', 'platform.team_role_changed', 'platform.team_member_removed',
                           'platform.analytics_email_changed')
                AND hospital_id IS NULL AND actor_id IS NOT NULL);

-- +goose Down
DROP POLICY platform_admin_append ON audit_log;

DROP TABLE platform_email_prefs;

DROP INDEX audit_log_legal_hold;
DROP POLICY platform_lift ON legal_holds;
DROP FUNCTION mhai_hold_lift_approved(uuid);
DROP TABLE legal_hold_lifts;
DROP POLICY platform_place ON legal_holds;
REVOKE INSERT (hospital_id, reference, reason, review_on, placed_by), UPDATE (lifted_at, version)
    ON legal_holds FROM mhai_platform;
REVOKE SELECT (reason, review_on, placed_by, version) ON legal_holds FROM mhai_platform;
ALTER TABLE legal_holds DROP COLUMN version, DROP COLUMN placed_by, DROP COLUMN review_on, DROP COLUMN reason;

DROP FUNCTION mhai_platform_mfa_enabled(uuid);
DROP POLICY platform_mfa_owner ON user_mfa;
DROP POLICY platform_team_revoke ON sessions;
REVOKE UPDATE (revoked_at) ON sessions FROM mhai_platform;
DROP POLICY platform_team_drop ON user_roles;
DROP POLICY platform_team_add ON user_roles;
REVOKE INSERT, DELETE ON user_roles FROM mhai_platform;
DROP POLICY platform_team_update ON users;
REVOKE UPDATE (is_active) ON users FROM mhai_platform;
DROP TABLE platform_member_versions;
