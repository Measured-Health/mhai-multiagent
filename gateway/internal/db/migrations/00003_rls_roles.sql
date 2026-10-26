-- Database roles, grants and row-level security (design §7.3, §7.4).
--
-- Roles (all NOLOGIN; deployments create login users and grant them roles):
--   mhai_migrate   owns every table, view and function. Not used by the app.
--   mhai_gateway   clinical-side requests, scoped to mhai.hospital_id by RLS.
--   mhai_platform  platform endpoints: metadata across hospitals, no clinical
--                  tables, no password hashes.
--   mhai_auth      sign-in before a hospital is known: account tables only.
--   mhai_worker    Python services, scoped to mhai.hospital_id by RLS.
--   mhai_app       the gateway's login user is granted this. It holds no
--                  privileges and inherits none (NOINHERIT, INHERIT FALSE
--                  grants), so a query outside db.With* fails; it can only
--                  SET ROLE to mhai_gateway, mhai_platform or mhai_auth.
--
-- No app role owns a table or has BYPASSRLS, and every table with
-- hospital_id has RLS enabled and forced, so the policies always apply.
-- Tables created by later migrations must be handed to mhai_migrate and
-- given grants and policies the same way.
--
-- Roles are cluster-wide, so they are created only if missing and are not
-- dropped on Down.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_migrate', 'mhai_gateway', 'mhai_platform', 'mhai_auth', 'mhai_worker'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE', r);
        END IF;
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_app') THEN
        CREATE ROLE mhai_app NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
    END IF;
    -- The migration runner must be able to act as mhai_migrate to hand
    -- objects over (a no-op for a superuser).
    IF NOT pg_has_role(current_user, 'mhai_migrate', 'MEMBER') THEN
        EXECUTE format('GRANT mhai_migrate TO %I', current_user);
    END IF;
END
$$;
-- +goose StatementEnd

GRANT mhai_gateway, mhai_platform, mhai_auth TO mhai_app WITH INHERIT FALSE;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO mhai_migrate;
GRANT USAGE ON SCHEMA public TO mhai_gateway, mhai_platform, mhai_auth, mhai_worker;

-- The hospital of the current transaction, or null when none is set. An
-- empty setting (left behind on a pooled connection after SET LOCAL) is
-- also null, so a query without a hospital matches no rows. A malformed
-- value fails the cast, which fails the query.
CREATE FUNCTION mhai_current_hospital() RETURNS uuid
    LANGUAGE sql STABLE PARALLEL SAFE
    AS $$ SELECT NULLIF(current_setting('mhai.hospital_id', true), '')::uuid $$;

-- Users without a password hash, for the platform endpoints. It runs with
-- the caller's privileges, so RLS and column grants still apply.
CREATE VIEW platform_users WITH (security_invoker = true) AS
SELECT id, hospital_id, email, display_name, full_name, role_title, specialty,
       is_active, must_change_password, terms_version, terms_accepted_at,
       last_login_at, oidc_issuer, oidc_subject, created_at
FROM users;

-- The last link of the global audit chain. The gateway sees only its own
-- hospital's audit rows, but every append needs the newest hash. Returns
-- id and hash only. Callers serialise appends with pg_advisory_xact_lock.
-- +goose StatementBegin
CREATE FUNCTION audit_chain_head(OUT id bigint, OUT hash text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public
    AS $$ SELECT a.id, a.hash FROM public.audit_log a ORDER BY a.id DESC LIMIT 1 $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION audit_chain_head() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_gateway, mhai_platform, mhai_auth;

-- Hand every object to mhai_migrate. Serial sequences follow their table.
-- +goose StatementBegin
DO $$
DECLARE
    obj record;
BEGIN
    FOR obj IN
        SELECT c.relname, c.relkind FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relkind IN ('r', 'v') AND c.relname <> 'goose_db_version'
    LOOP
        IF obj.relkind = 'v' THEN
            EXECUTE format('ALTER VIEW public.%I OWNER TO mhai_migrate', obj.relname);
        ELSE
            EXECUTE format('ALTER TABLE public.%I OWNER TO mhai_migrate', obj.relname);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_current_hospital() OWNER TO mhai_migrate;
ALTER FUNCTION audit_chain_head() OWNER TO mhai_migrate;

-- Row-level security on, and forced, for every table with hospital_id, and
-- for the account tables the gateway can reach (scoped through users).
ALTER TABLE hospitals          ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE hospital_settings  ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE users              ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE user_roles         ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE user_mfa           ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions           ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens     ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE trusted_devices    ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_log          ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE kb_chunks          ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE clinical_queries   ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE clinical_decisions ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE hga_evaluations    ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE feedback_events    ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

------------------------------------------------------------------------
-- mhai_gateway: its own hospital's rows only.
------------------------------------------------------------------------
GRANT SELECT ON hospitals TO mhai_gateway;
CREATE POLICY gateway_own ON hospitals TO mhai_gateway
    USING (id = mhai_current_hospital());

GRANT SELECT, UPDATE ON hospital_settings TO mhai_gateway;
CREATE POLICY gateway_hospital ON hospital_settings TO mhai_gateway
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

GRANT SELECT, INSERT, UPDATE ON users TO mhai_gateway;
CREATE POLICY gateway_hospital ON users TO mhai_gateway
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

-- Account tables without hospital_id are scoped through users, whose own
-- policy limits the subquery to this hospital.
GRANT SELECT, INSERT, UPDATE, DELETE ON user_roles, user_mfa, sessions, trusted_devices TO mhai_gateway;
CREATE POLICY gateway_hospital ON user_roles TO mhai_gateway
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id));
CREATE POLICY gateway_hospital ON user_mfa TO mhai_gateway
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id));
CREATE POLICY gateway_hospital ON sessions TO mhai_gateway
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id));
CREATE POLICY gateway_hospital ON trusted_devices TO mhai_gateway
    USING (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id))
    WITH CHECK (EXISTS (SELECT 1 FROM users u WHERE u.id = user_id));
GRANT SELECT, INSERT, UPDATE, DELETE ON refresh_tokens TO mhai_gateway;
CREATE POLICY gateway_hospital ON refresh_tokens TO mhai_gateway
    USING (EXISTS (SELECT 1 FROM sessions s WHERE s.id = session_id))
    WITH CHECK (EXISTS (SELECT 1 FROM sessions s WHERE s.id = session_id));

-- Audit is append-only: INSERT and SELECT, never UPDATE or DELETE.
GRANT SELECT, INSERT ON audit_log TO mhai_gateway;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_gateway;
CREATE POLICY gateway_read ON audit_log FOR SELECT TO mhai_gateway
    USING (hospital_id = mhai_current_hospital());
CREATE POLICY gateway_append ON audit_log FOR INSERT TO mhai_gateway
    WITH CHECK (hospital_id = mhai_current_hospital());

GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO mhai_gateway;

GRANT SELECT, INSERT, UPDATE ON clinical_queries, clinical_decisions, feedback_events TO mhai_gateway;
GRANT SELECT, INSERT ON hga_evaluations TO mhai_gateway;
GRANT USAGE ON SEQUENCE hga_evaluations_id_seq TO mhai_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON kb_chunks TO mhai_gateway;
GRANT USAGE ON SEQUENCE kb_chunks_id_seq TO mhai_gateway;
GRANT SELECT ON guardrail_weights TO mhai_gateway;

------------------------------------------------------------------------
-- mhai_worker: the pipeline tables only, under the same hospital policy.
------------------------------------------------------------------------
GRANT SELECT, UPDATE ON clinical_queries TO mhai_worker;
GRANT SELECT, INSERT ON clinical_decisions, hga_evaluations TO mhai_worker;
GRANT USAGE ON SEQUENCE hga_evaluations_id_seq TO mhai_worker;
GRANT SELECT, INSERT, UPDATE ON kb_chunks TO mhai_worker;
GRANT USAGE ON SEQUENCE kb_chunks_id_seq TO mhai_worker;
GRANT SELECT ON guardrail_weights TO mhai_worker;

-- The clinical policies apply to both hospital-scoped roles.
CREATE POLICY hospital_rows ON clinical_queries TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY hospital_rows ON clinical_decisions TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY hospital_rows ON hga_evaluations TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY hospital_rows ON feedback_events TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY hospital_rows ON kb_chunks TO mhai_gateway, mhai_worker
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
-- Shared reference content: read-only for everyone (design §7.6).
CREATE POLICY shared_read ON kb_chunks FOR SELECT TO mhai_gateway, mhai_worker
    USING (hospital_id IS NULL);

------------------------------------------------------------------------
-- mhai_auth: account tables across hospitals, for sign-in. No clinical
-- tables.
------------------------------------------------------------------------
GRANT SELECT ON hospitals TO mhai_auth;
CREATE POLICY auth_all ON hospitals FOR SELECT TO mhai_auth USING (true);
GRANT SELECT, INSERT, UPDATE ON users, hospital_settings TO mhai_auth;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_roles, user_mfa, sessions, refresh_tokens, trusted_devices TO mhai_auth;
CREATE POLICY auth_all ON users             TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON hospital_settings TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON user_roles        TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON user_mfa          TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON sessions          TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON refresh_tokens    TO mhai_auth USING (true) WITH CHECK (true);
CREATE POLICY auth_all ON trusted_devices   TO mhai_auth USING (true) WITH CHECK (true);
-- Sign-in attempts are audited; append only.
GRANT INSERT ON audit_log TO mhai_auth;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_auth;
CREATE POLICY auth_append ON audit_log FOR INSERT TO mhai_auth WITH CHECK (true);
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO mhai_auth;

------------------------------------------------------------------------
-- mhai_platform: metadata across hospitals, no clinical content, no
-- password hashes.
------------------------------------------------------------------------
GRANT SELECT ON hospitals, user_roles, sessions, audit_log, audit_checkpoints TO mhai_platform;
GRANT SELECT (id, hospital_id, email, display_name, full_name, role_title, specialty,
              is_active, must_change_password, terms_version, terms_accepted_at,
              last_login_at, oidc_issuer, oidc_subject, created_at)
    ON users TO mhai_platform;
GRANT SELECT ON platform_users TO mhai_platform;
CREATE POLICY platform_read ON hospitals  FOR SELECT TO mhai_platform USING (true);
CREATE POLICY platform_read ON users      FOR SELECT TO mhai_platform USING (true);
CREATE POLICY platform_read ON user_roles FOR SELECT TO mhai_platform USING (true);
CREATE POLICY platform_read ON sessions   FOR SELECT TO mhai_platform USING (true);
CREATE POLICY platform_read ON audit_log  FOR SELECT TO mhai_platform USING (true);
-- Platform actions are audited; append only.
GRANT INSERT ON audit_log TO mhai_platform;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_platform;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform WITH CHECK (true);
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO mhai_platform;

------------------------------------------------------------------------
-- mhai_migrate owns audit_log, but FORCE applies to owners too: this lets
-- audit_chain_head() (SECURITY DEFINER) read the newest row.
------------------------------------------------------------------------
CREATE POLICY chain_head ON audit_log FOR SELECT TO mhai_migrate USING (true);

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
    p record;
BEGIN
    FOR p IN SELECT schemaname, tablename, policyname FROM pg_policies WHERE schemaname = 'public' LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I', p.policyname, p.schemaname, p.tablename);
    END LOOP;
END
$$;
-- +goose StatementEnd
ALTER TABLE hospitals          NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE hospital_settings  NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE users              NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE user_roles         NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE user_mfa           NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE sessions           NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens     NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE trusted_devices    NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log          NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE kb_chunks          NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE clinical_queries   NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE clinical_decisions NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE hga_evaluations    NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
ALTER TABLE feedback_events    NO FORCE ROW LEVEL SECURITY, DISABLE ROW LEVEL SECURITY;
DROP FUNCTION audit_chain_head();
DROP VIEW platform_users;
DROP FUNCTION mhai_current_hospital();
-- Revokes this database's grants to the app roles; the roles themselves
-- are cluster-wide and stay.
DROP OWNED BY mhai_gateway, mhai_platform, mhai_auth, mhai_worker, mhai_app;
REASSIGN OWNED BY mhai_migrate TO CURRENT_USER;
