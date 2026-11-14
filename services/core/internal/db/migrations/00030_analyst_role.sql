-- Patient Analysis, the analyst grant (analysis backend design §6.1-§6.3,
-- §10; plan Tasks 15-16, "migration D"; ADR 0015). NUMBERED 00030
-- (coordinator): EMR PR 8a takes 00029.
--
-- Analysis stays OFF: nothing here raises a switch, and an analyst can only
-- be granted in a hospital whose analysis switch is on.
--
-- What it adds:
--   - mhai_analyst_rules (NOLOGIN, NOINHERIT, no members): owns the
--     trigger functions and the grant functions. Its only reads: users'
--     hospital, active flag and email (the CLI's lookup), user_mfa's
--     enabled flag, user_roles, the analyst audit entries and its own
--     table; its only writes: the analyst rows of user_roles and
--     analyst_changes.
--   - The rules (§6.1), in triggers on user_roles, for every writer:
--     inserting 'analyst' needs the grantor to be an active hospital admin
--     of the person's hospital who isn't the person (the token's user,
--     mhai_current_user(), or for the provisioning CLI the --by admin in
--     mhai.operator_id, read only when acting as mhai_provision), the
--     switch on, a 'clinician' row, the person active with two-step on,
--     and fewer than 10 analysts in the hospital. Refusals raise P0001
--     with message forbidden, not_found, analysis_off, self_change,
--     not_eligible or analyst_cap. An UPDATE that makes, moves or changes
--     an analyst row is refused (grant and remove instead). Removing
--     'clinician' (DELETE, or an UPDATE away from it) removes 'analyst'.
--     Grants and removals of one hospital are serialised by a transaction
--     advisory lock, so the cap and the clinician rule hold under
--     concurrency.
--   - analyst_changes: one row per grant or removal (who, by whom, how,
--     why), written by the triggers. A deferred constraint trigger refuses,
--     at commit, a change without its analyst.granted or analyst.removed
--     audit entry (entity user, the change's id in the payload, by its
--     grantor, in its hospital): no grant or removal is kept unaudited,
--     however the row was written (plan departure 25).
--   - mhai_grant_analyst(user) and mhai_remove_analyst(user): the route's
--     and the CLI's one path (true when something changed);
--     mhai_analyst_pending(): the changes of this transaction still to be
--     audited (the caller appends them); mhai_analyst_cli_users(person
--     email, admin email): the CLI's lookup, which refuses an admin who
--     isn't an active hospital admin of the person's hospital.
--   - provision_analyst_append: the CLI's analyst entries, by the --by
--     admin. platform_append is not touched.
--
-- A superuser session acting as itself (no SET ROLE) is exempt from the
-- rules and the audit check (plan departure 25): a superuser can disable
-- any trigger anyway, and the core service refuses a superuser login
-- (db.CheckLoginRole). Every app role, the provisioning role and the
-- migration owner are held to them.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_analyst_rules') THEN
        CREATE ROLE mhai_analyst_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    ALTER ROLE mhai_analyst_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    -- Nobody may already act as it: it writes roles.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = 'mhai_analyst_rules') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00030: mhai_analyst_rules already has members; remove them first';
    END IF;
    -- The runner acts as the owners while it hands functions over and
    -- grants on the position function (00021's reader owns it).
    FOREACH r IN ARRAY ARRAY['mhai_analyst_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_analyst_rules;

-- No analyst row from before the rules may break the clinician rule (an
-- account given analyst at creation, which core users create allowed
-- until now): the migration refuses, and names the count only. Remove the
-- role from those accounts, or give them clinician, and run it again.
-- (security review). The grant-time rules (two-step, switch, cap) are
-- checked at each grant; the patient-level routes re-check two-step.
-- +goose StatementBegin
DO $$
DECLARE
    n integer;
BEGIN
    SELECT count(*) INTO n FROM user_roles a
    WHERE a.role = 'analyst'
      AND NOT EXISTS (SELECT 1 FROM user_roles c WHERE c.user_id = a.user_id AND c.role = 'clinician');
    IF n > 0 THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = format('00030: %s analyst account(s) without the clinician role; remove analyst from them or give them clinician first', n);
    END IF;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The record of grants and removals.
------------------------------------------------------------------------
CREATE TABLE analyst_changes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq         bigint GENERATED ALWAYS AS IDENTITY,
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    change      text NOT NULL CHECK (change IN ('granted', 'removed')),
    reason      text NOT NULL CHECK (reason IN ('granted', 'removed', 'clinician_removed')),
    via         text NOT NULL CHECK (via IN ('app', 'cli')),
    by_user     uuid,
    xact        xid8 NOT NULL DEFAULT pg_current_xact_id(),
    at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT analyst_changes_reason CHECK ((change = 'granted') = (reason = 'granted'))
);
CREATE INDEX analyst_changes_xact_idx ON analyst_changes (xact);
ALTER TABLE analyst_changes OWNER TO mhai_migrate;
ALTER TABLE analyst_changes ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analyst_changes FROM PUBLIC;

-- The entry a change names, found at once (the commit check).
CREATE INDEX audit_log_analyst_change_idx ON audit_log ((payload ->> 'change_id'))
    WHERE action IN ('analyst.granted', 'analyst.removed');

-- The rules' owner: what the rules read and write, and nothing else.
GRANT SELECT, INSERT ON analyst_changes TO mhai_analyst_rules;
CREATE POLICY analyst_rules ON analyst_changes TO mhai_analyst_rules USING (true) WITH CHECK (true);
-- email for the CLI's lookup only (mhai_analyst_cli_users).
GRANT SELECT (id, hospital_id, is_active, email) ON users TO mhai_analyst_rules;
CREATE POLICY analyst_rules ON users FOR SELECT TO mhai_analyst_rules USING (true);
GRANT SELECT (user_id, enabled) ON user_mfa TO mhai_analyst_rules;
CREATE POLICY analyst_rules ON user_mfa FOR SELECT TO mhai_analyst_rules USING (true);
GRANT SELECT, INSERT, DELETE ON user_roles TO mhai_analyst_rules;
CREATE POLICY analyst_rules_read ON user_roles FOR SELECT TO mhai_analyst_rules USING (true);
CREATE POLICY analyst_rules_insert ON user_roles FOR INSERT TO mhai_analyst_rules WITH CHECK (role = 'analyst');
CREATE POLICY analyst_rules_delete ON user_roles FOR DELETE TO mhai_analyst_rules USING (role = 'analyst');
GRANT SELECT (id, hospital_id, actor_id, action, entity, entity_id, payload) ON audit_log TO mhai_analyst_rules;
CREATE POLICY analyst_rules ON audit_log FOR SELECT TO mhai_analyst_rules
    USING (action IN ('analyst.granted', 'analyst.removed'));
GRANT EXECUTE ON FUNCTION mhai_analysis_position(uuid) TO mhai_analyst_rules;

------------------------------------------------------------------------
-- Helpers (no EXECUTE for anyone but their owner).
------------------------------------------------------------------------

-- A superuser acting as itself: exempt (see the header).
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_exempt() RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT pg_catalog.current_setting('role') = 'none'
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname = session_user AND r.rolsuper)
$$;
-- +goose StatementEnd

-- The writer's grantor and channel. Inside a definer function current_user
-- is the owner, but the role setting is still the caller's SET ROLE. As
-- mhai_gateway (db.WithHospitalUser) the grantor is the token's user; as
-- mhai_provision (the CLI's SET LOCAL ROLE) it is mhai.operator_id, the
-- --by admin mhai_analyst_cli_users checked. Any other session, such as a
-- provisioning login that inherits the role without SET ROLE, has no
-- grantor, so it can't pose as either (security review).
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_actor(OUT by_user uuid, OUT via text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF pg_catalog.current_setting('role') = 'mhai_provision' THEN
        by_user := NULLIF(pg_catalog.current_setting('mhai.operator_id', true), '')::uuid;
        via := 'cli';
    ELSIF pg_catalog.current_setting('role') = 'mhai_gateway' THEN
        by_user := public.mhai_current_user();
        via := 'app';
    ELSE
        by_user := NULL;
        via := 'app';
    END IF;
END
$$;
-- +goose StatementEnd

-- One hospital's grants and removals, one at a time (the cap and the
-- clinician rule under concurrency).
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_lock(p_hospital uuid) RETURNS void
    LANGUAGE sql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('mhai_analyst:' || p_hospital::text, 0))
$$;
-- +goose StatementEnd

-- The grantor may change a person's analyst role: an active hospital admin
-- of the person's hospital. Raises forbidden otherwise.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_check_grantor(p_by uuid, p_hospital uuid) RETURNS void
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_by IS NULL OR p_hospital IS NULL OR NOT EXISTS (
            SELECT 1 FROM public.users g JOIN public.user_roles r ON r.user_id = g.id AND r.role = 'hospital_admin'
            WHERE g.id = p_by AND g.hospital_id = p_hospital AND g.is_active) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
END
$$;
-- +goose StatementEnd

-- Records a change of p_user's analyst role (none for an exempt writer).
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_record(p_user uuid, p_change text, p_reason text) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid;
    v_by  uuid;
    v_via text;
BEGIN
    IF public.mhai_analyst_exempt() THEN
        RETURN;
    END IF;
    SELECT u.hospital_id INTO v_h FROM public.users u WHERE u.id = p_user;
    -- An account being deleted with its roles: its hospital is the request's.
    v_h := COALESCE(v_h, public.mhai_current_hospital());
    IF v_h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    SELECT a.by_user, a.via INTO v_by, v_via FROM public.mhai_analyst_actor() a;
    INSERT INTO public.analyst_changes (hospital_id, user_id, change, reason, via, by_user)
    VALUES (v_h, p_user, p_change, p_reason, v_via, v_by);
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The rules on user_roles.
------------------------------------------------------------------------

-- Before a row is inserted or updated: §6.1 for a new analyst row; no
-- UPDATE ever makes, moves or changes one.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid;
    v_by  uuid;
    v_n   integer;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF (OLD.role = 'analyst' OR NEW.role = 'analyst')
           AND (OLD.role IS DISTINCT FROM NEW.role OR OLD.user_id IS DISTINCT FROM NEW.user_id) THEN
            IF public.mhai_analyst_exempt() THEN
                RETURN NEW;
            END IF;
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible',
                DETAIL = 'an analyst role is granted and removed, never updated';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.role <> 'analyst' OR public.mhai_analyst_exempt() THEN
        RETURN NEW;
    END IF;
    IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible',
            DETAIL = 'grant an analyst in a READ COMMITTED transaction';
    END IF;
    SELECT u.hospital_id INTO v_h FROM public.users u WHERE u.id = NEW.user_id;
    IF v_h IS NULL OR (public.mhai_current_hospital() IS NOT NULL AND v_h <> public.mhai_current_hospital()) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    SELECT a.by_user INTO v_by FROM public.mhai_analyst_actor() a;
    PERFORM public.mhai_analyst_check_grantor(v_by, v_h);
    PERFORM public.mhai_analyst_lock(v_h);
    -- Fresh statements after the lock: under READ COMMITTED each sees what
    -- the previous holder committed.
    IF public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'analysis_off';
    END IF;
    IF v_by = NEW.user_id THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'self_change';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = NEW.user_id AND r.role = 'clinician')
       OR NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id = NEW.user_id AND u.is_active)
       OR NOT EXISTS (SELECT 1 FROM public.user_mfa m WHERE m.user_id = NEW.user_id AND m.enabled) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible';
    END IF;
    SELECT count(*) INTO v_n FROM public.user_roles r JOIN public.users u ON u.id = r.user_id
    WHERE r.role = 'analyst' AND u.hospital_id = v_h;
    IF v_n >= 10 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'analyst_cap';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- After an analyst row is inserted or deleted: the record. After a
-- clinician row is deleted or updated away: the analyst row goes too (its
-- own delete is then recorded, reason clinician_removed).
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_after() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.role = 'analyst' THEN
            PERFORM public.mhai_analyst_record(NEW.user_id, 'granted', 'granted');
        END IF;
        RETURN NULL;
    END IF;
    IF TG_OP = 'DELETE' AND OLD.role = 'analyst' THEN
        PERFORM public.mhai_analyst_record(OLD.user_id, 'removed',
            CASE WHEN EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'clinician')
                 THEN 'removed' ELSE 'clinician_removed' END);
        RETURN NULL;
    END IF;
    IF OLD.role = 'clinician' AND (TG_OP = 'DELETE' OR NEW.role <> 'clinician' OR NEW.user_id <> OLD.user_id)
       AND NOT EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'clinician') THEN
        SELECT u.hospital_id INTO v_h FROM public.users u WHERE u.id = OLD.user_id;
        IF v_h IS NOT NULL THEN
            PERFORM public.mhai_analyst_lock(v_h);
        END IF;
        DELETE FROM public.user_roles r WHERE r.user_id = OLD.user_id AND r.role = 'analyst';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- The commit check: a change is kept only with its audit entry.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_change_audited() RETURNS trigger
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.action IN ('analyst.granted', 'analyst.removed')
                     AND a.payload ->> 'change_id' = NEW.id::text
                     AND a.action = 'analyst.' || NEW.change
                     AND a.entity = 'user' AND a.entity_id = NEW.user_id::text
                     AND a.hospital_id = NEW.hospital_id
                     AND a.actor_id IS NOT DISTINCT FROM NEW.by_user) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'analyst_changes: a grant or removal without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER user_roles_analyst BEFORE INSERT OR UPDATE ON user_roles
    FOR EACH ROW EXECUTE FUNCTION mhai_analyst_rules();
CREATE TRIGGER user_roles_analyst_after AFTER INSERT OR UPDATE OR DELETE ON user_roles
    FOR EACH ROW EXECUTE FUNCTION mhai_analyst_after();
CREATE CONSTRAINT TRIGGER analyst_changes_audited AFTER INSERT ON analyst_changes
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_analyst_change_audited();

------------------------------------------------------------------------
-- The request functions.
------------------------------------------------------------------------

-- The person, for a grant or a removal: in the request's hospital (the
-- gateway), else not_found.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_person(p_user uuid) RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid;
BEGIN
    SELECT u.hospital_id INTO v_h FROM public.users u WHERE u.id = p_user;
    IF v_h IS NULL
       OR (pg_catalog.current_setting('role') <> 'mhai_provision' AND v_h IS DISTINCT FROM public.mhai_current_hospital()) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    RETURN v_h;
END
$$;
-- +goose StatementEnd

-- mhai_grant_analyst(user): adds 'analyst' under the rules; false when the
-- person already holds it. The caller appends the audit entry
-- (mhai_analyst_pending) in the same transaction.
-- +goose StatementBegin
CREATE FUNCTION mhai_grant_analyst(p_user uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid;
BEGIN
    v_h := public.mhai_analyst_person(p_user);
    -- The grantor first: nobody else learns whether the person holds it.
    PERFORM public.mhai_analyst_check_grantor((SELECT a.by_user FROM public.mhai_analyst_actor() a), v_h);
    IF EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = p_user AND r.role = 'analyst') THEN
        RETURN false;
    END IF;
    INSERT INTO public.user_roles (user_id, role) VALUES (p_user, 'analyst') ON CONFLICT DO NOTHING;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd

-- mhai_remove_analyst(user): removes 'analyst', by an active hospital admin
-- of the person's hospital (any, the person included: removing only ever
-- takes access away); false when the person doesn't hold it.
-- +goose StatementBegin
CREATE FUNCTION mhai_remove_analyst(p_user uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h  uuid;
    v_by uuid;
BEGIN
    v_h := public.mhai_analyst_person(p_user);
    SELECT a.by_user INTO v_by FROM public.mhai_analyst_actor() a;
    PERFORM public.mhai_analyst_check_grantor(v_by, v_h);
    PERFORM public.mhai_analyst_lock(v_h);
    DELETE FROM public.user_roles r WHERE r.user_id = p_user AND r.role = 'analyst';
    RETURN FOUND;
END
$$;
-- +goose StatementEnd

-- mhai_analyst_pending(): this transaction's changes with no audit entry
-- yet, in order, for the request's hospital (the gateway) or any (the
-- CLI). The caller appends one entry per row before commit.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_pending()
    RETURNS TABLE (change_id uuid, hospital_id uuid, user_id uuid, change text, reason text, via text, by_user uuid)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.hospital_id, c.user_id, c.change, c.reason, c.via, c.by_user
    FROM public.analyst_changes c
    WHERE c.xact = pg_catalog.pg_current_xact_id()
      AND (pg_catalog.current_setting('role') = 'mhai_provision' OR c.hospital_id = public.mhai_current_hospital())
      AND NOT EXISTS (SELECT 1 FROM public.audit_log a
                      WHERE a.action IN ('analyst.granted', 'analyst.removed') AND a.payload ->> 'change_id' = c.id::text)
    ORDER BY c.seq
$$;
-- +goose StatementEnd

-- mhai_analyst_cli_users(person email, admin email): the CLI's lookup. The
-- person's id and hospital and the admin's id, only when the admin is an
-- active hospital admin of the person's hospital (forbidden otherwise;
-- not_found for an unknown person). Never a general email lookup.
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_cli_users(p_person citext, p_by citext,
    OUT person uuid, OUT hospital_id uuid, OUT by_user uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
    AS $$
BEGIN
    SELECT u.id, u.hospital_id INTO person, hospital_id FROM public.users u WHERE u.email = p_person;
    IF person IS NULL OR hospital_id IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    SELECT g.id INTO by_user FROM public.users g WHERE g.email = p_by;
    PERFORM public.mhai_analyst_check_grantor(by_user, hospital_id);
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_analyst_exempt() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_actor() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_lock(uuid) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_check_grantor(uuid, uuid) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_record(uuid, text, text) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_rules() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_after() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_change_audited() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_person(uuid) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_grant_analyst(uuid) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_remove_analyst(uuid) OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_pending() OWNER TO mhai_analyst_rules;
ALTER FUNCTION mhai_analyst_cli_users(citext, citext) OWNER TO mhai_analyst_rules;
REVOKE ALL ON FUNCTION mhai_analyst_exempt(), mhai_analyst_actor(), mhai_analyst_lock(uuid),
    mhai_analyst_check_grantor(uuid, uuid), mhai_analyst_record(uuid, text, text), mhai_analyst_rules(),
    mhai_analyst_after(), mhai_analyst_change_audited(), mhai_analyst_person(uuid), mhai_grant_analyst(uuid),
    mhai_remove_analyst(uuid), mhai_analyst_pending(), mhai_analyst_cli_users(citext, citext) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_grant_analyst(uuid), mhai_remove_analyst(uuid), mhai_analyst_pending()
    TO mhai_gateway, mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_analyst_cli_users(citext, citext) TO mhai_provision;

------------------------------------------------------------------------
-- The CLI's analyst entries: only for a change this transaction made, by
-- the --by admin, in the person's hospital (security review: the
-- provisioning role can't write an entry with no change behind it).
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_analyst_change_open(p_change text, p_user text, p_hospital uuid, p_by uuid, p_action text)
    RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.analyst_changes c
                   WHERE c.id::text = p_change AND c.user_id::text = p_user AND c.hospital_id = p_hospital
                     AND c.by_user = p_by AND 'analyst.' || c.change = p_action AND c.via = 'cli'
                     AND c.xact = pg_catalog.pg_current_xact_id())
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_analyst_change_open(text, text, uuid, uuid, text) OWNER TO mhai_analyst_rules;
REVOKE ALL ON FUNCTION mhai_analyst_change_open(text, text, uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analyst_change_open(text, text, uuid, uuid, text) TO mhai_provision;
CREATE POLICY provision_analyst_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('analyst.granted', 'analyst.removed') AND entity = 'user'
                AND hospital_id IS NOT NULL AND actor_id IS NOT NULL
                AND actor_id = NULLIF(current_setting('mhai.operator_id', true), '')::uuid
                AND payload ->> 'via' = 'cli'
                AND public.mhai_analyst_change_open(payload ->> 'change_id', entity_id, hospital_id, actor_id, action));

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analyst_rules', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Analyst rows stay as they are, with no rules over them until 00030 runs
-- again; the change record goes, and the audit chain keeps every entry.

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analyst_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

DROP POLICY provision_analyst_append ON audit_log;
DROP FUNCTION mhai_analyst_change_open(text, text, uuid, uuid, text);

DROP TRIGGER analyst_changes_audited ON analyst_changes;
DROP TRIGGER user_roles_analyst_after ON user_roles;
DROP TRIGGER user_roles_analyst ON user_roles;
DROP FUNCTION mhai_analyst_cli_users(citext, citext);
DROP FUNCTION mhai_analyst_pending();
DROP FUNCTION mhai_remove_analyst(uuid);
DROP FUNCTION mhai_grant_analyst(uuid);
DROP FUNCTION mhai_analyst_person(uuid);
DROP FUNCTION mhai_analyst_change_audited();
DROP FUNCTION mhai_analyst_after();
DROP FUNCTION mhai_analyst_rules();
DROP FUNCTION mhai_analyst_record(uuid, text, text);
DROP FUNCTION mhai_analyst_check_grantor(uuid, uuid);
DROP FUNCTION mhai_analyst_lock(uuid);
DROP FUNCTION mhai_analyst_actor();
DROP FUNCTION mhai_analyst_exempt();

DROP POLICY analyst_rules ON audit_log;
DROP POLICY analyst_rules_delete ON user_roles;
DROP POLICY analyst_rules_insert ON user_roles;
DROP POLICY analyst_rules_read ON user_roles;
DROP POLICY analyst_rules ON user_mfa;
DROP POLICY analyst_rules ON users;
REVOKE ALL ON audit_log, user_roles, user_mfa, users FROM mhai_analyst_rules;
REVOKE ALL ON FUNCTION mhai_analysis_position(uuid) FROM mhai_analyst_rules;
DROP INDEX audit_log_analyst_change_idx;

-- The table takes its policy, grants and index with it.
DROP TABLE analyst_changes;

REVOKE USAGE ON SCHEMA public FROM mhai_analyst_rules;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_analyst_rules', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
