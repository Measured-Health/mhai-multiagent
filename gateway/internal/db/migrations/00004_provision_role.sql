-- mhai_provision: the least-privilege role behind the seed CLI
-- (gateway hospitals create / gateway users create).
--
-- It can INSERT hospitals, their settings, users, user roles and the two
-- audit actions the CLI takes, and read only hospitals(id, short_code) (for
-- the short-code lookup). It cannot read users or any other table, and it has
-- no UPDATE or DELETE anywhere. It is NOLOGIN, without BYPASSRLS, and never a
-- member of mhai_app: the provisioning login is created by the operator as
--   CREATE ROLE provisioner LOGIN PASSWORD '...' IN ROLE mhai_provision;
-- and the CLI runs SET LOCAL ROLE mhai_provision. The gateway's runtime
-- login must not be a member of it (db.CheckLoginRole refuses that).
--
-- The role is cluster-wide, so it is created only if missing and kept on Down.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_provision') THEN
        CREATE ROLE mhai_provision NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_provision;

GRANT INSERT (id, name, short_code, time_zone) ON hospitals TO mhai_provision;
CREATE POLICY provision_insert ON hospitals FOR INSERT TO mhai_provision
    WITH CHECK (status = 'setup_incomplete');
-- The short-code lookup, and nothing else of hospitals.
GRANT SELECT (id, short_code) ON hospitals TO mhai_provision;
CREATE POLICY provision_read ON hospitals FOR SELECT TO mhai_provision USING (true);

GRANT INSERT (hospital_id, require_mfa_for_clinicians, allow_trust_device) ON hospital_settings TO mhai_provision;
CREATE POLICY provision_insert ON hospital_settings FOR INSERT TO mhai_provision WITH CHECK (true);

-- A provisioned account must change its password at first sign-in. The
-- caller supplies the id, so no read-back of users is needed.
GRANT INSERT (id, hospital_id, email, password_hash, display_name, must_change_password) ON users TO mhai_provision;
CREATE POLICY provision_insert ON users FOR INSERT TO mhai_provision WITH CHECK (must_change_password);

-- Every role is allowed: the CLI is the only path to platform roles. The
-- pairing triggers still apply.
GRANT INSERT ON user_roles TO mhai_provision;
CREATE POLICY provision_insert ON user_roles FOR INSERT TO mhai_provision WITH CHECK (true);

-- audit.Write: append, plus the chain head.
GRANT INSERT ON audit_log TO mhai_provision;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_provision;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_provision;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created') AND actor_id IS NULL);

-- +goose Down
DROP POLICY provision_append ON audit_log;
DROP POLICY provision_insert ON user_roles;
DROP POLICY provision_insert ON users;
DROP POLICY provision_insert ON hospital_settings;
DROP POLICY provision_read ON hospitals;
DROP POLICY provision_insert ON hospitals;
REVOKE ALL ON hospitals, hospital_settings, users, user_roles, audit_log FROM mhai_provision;
REVOKE ALL ON SEQUENCE audit_log_id_seq FROM mhai_provision;
REVOKE ALL ON FUNCTION audit_chain_head() FROM mhai_provision;
REVOKE ALL ON SCHEMA public FROM mhai_provision;
