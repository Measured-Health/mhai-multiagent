-- Cloud model providers (design 2026-11-03 §5, §10; plan 2026-11-06 Task 1):
-- the per-hospital model policy, the route on every question, and the
-- monthly token ledger.
--
-- hospital_model_policy: one row per hospital; no row, or a NULL provider,
-- means local. A provider needs the reference of the recorded agreement
-- check, and only a provider has one (switching off clears it). The
-- reference is an agreement code, AGR-YYYY-NNNN or DPA-YYYY-NNNN (a fixed
-- prefix, a four-digit year, 2 to 8 digits), or the literal DEMO-SYNTHETIC:
-- letters appear only in the fixed prefixes, so it can't hold a word, a
-- person's name or a patient pseudonym, which would stay on the hash-chained
-- audit log for good (Task 2 review I1). The pseudonym shape is refused
-- again on its own, as mhai-web's PSEUDONYM_IN. It is platform-only: mhai_platform (the platform app) and
-- mhai_provision (the CLI twin) read and write it; mhai_gateway and
-- mhai_worker hold nothing on it (plan departures 3 and 14).
--
-- clinical_queries.model_route is set by a SECURITY DEFINER BEFORE INSERT
-- trigger from the policy, overwriting whatever the insert supplied. No
-- application role may update it (the gateway keeps 00008's
-- UPDATE (published_at) only).
--
-- mhai_effective_model_route: the worker's claim re-checks the route here.
-- It stays cloud only while the caller's own hospital is active and its
-- policy still names that provider; anything else runs local (design §5.2,
-- A14).
--
-- cloud_usage and cloud_reservations: the ledger. No application role holds
-- a grant on either. The worker reserves and settles only through two
-- definer functions bound to mhai_current_hospital() (plan departure 2):
-- the reservation names its provider and is one atomic UPDATE under the
-- budget of a policy enabled for that provider now, for an active hospital
-- only (review ruling C1, M3): switching or turning the policy off stops
-- attempts still in flight to the old provider. A settle consumes its
-- reservation row, so it happens once, for its own hospital.
--
-- Hospital status: under FORCE the owner sees hospitals only through
-- 00009's cancel_check (status <> 'active'), so "active" is written as "not
-- a row the owner sees as inactive". This reads no more than 00009 already
-- lets the owner read; TestReserveAndRouteRefuseInactiveHospital pins it.
-- The platform reads a hospital's month through mhai_cloud_usage.
--
-- Every definer function follows 00003/00007/00009: owner mhai_migrate,
-- search_path pinned to pg_catalog, pg_temp, every relation and function
-- written public.<name> (so a temporary table can't stand in for one),
-- no PUBLIC execute, EXECUTE for its one role. Under FORCE the owner reads
-- and writes these tables through its own policies.
--
-- The audit append policies of mhai_platform (00014) and mhai_provision
-- (00010) gain hospital.model_policy_changed, with their conditions kept.
--
-- Additive: nothing existing changes meaning. With no policy rows every
-- question is local, as before.

-- +goose Up

------------------------------------------------------------------------
-- The policy.
------------------------------------------------------------------------
CREATE TABLE hospital_model_policy (
    hospital_id          uuid PRIMARY KEY REFERENCES hospitals (id),
    cloud_provider       text CHECK (cloud_provider IN ('openai', 'anthropic', 'xai')),
    agreement_ref        text CHECK (agreement_ref ~ '^(DEMO-SYNTHETIC|(AGR|DPA)-[0-9]{4}-[0-9]{2,8})$'
                                     AND agreement_ref !~ '(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)'),
    monthly_token_budget bigint NOT NULL DEFAULT 2000000 CHECK (monthly_token_budget > 0),
    updated_by           uuid,
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT hospital_model_policy_agreement CHECK ((cloud_provider IS NULL) = (agreement_ref IS NULL))
);
ALTER TABLE hospital_model_policy OWNER TO mhai_migrate;
ALTER TABLE hospital_model_policy ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- The platform app and the CLI: read, create and change a policy, never
-- delete one or move it to another hospital.
GRANT SELECT, INSERT ON hospital_model_policy TO mhai_platform, mhai_provision;
GRANT UPDATE (cloud_provider, agreement_ref, monthly_token_budget, updated_by, updated_at)
    ON hospital_model_policy TO mhai_platform, mhai_provision;
CREATE POLICY platform_policy ON hospital_model_policy TO mhai_platform USING (true) WITH CHECK (true);
CREATE POLICY provision_policy ON hospital_model_policy TO mhai_provision USING (true) WITH CHECK (true);
-- The definer functions below read it as the owner.
CREATE POLICY owner_read ON hospital_model_policy FOR SELECT TO mhai_migrate USING (true);

------------------------------------------------------------------------
-- The route on every question.
------------------------------------------------------------------------
ALTER TABLE clinical_queries
    ADD COLUMN model_route text NOT NULL DEFAULT 'local',
    ADD CONSTRAINT clinical_queries_model_route_check
        CHECK (model_route IN ('local', 'cloud:openai', 'cloud:anthropic', 'cloud:xai'));

-- +goose StatementBegin
CREATE FUNCTION mhai_set_model_route() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- Whatever the insert named is ignored.
    NEW.model_route := coalesce((SELECT 'cloud:' || p.cloud_provider
                                 FROM public.hospital_model_policy p
                                 WHERE p.hospital_id = NEW.hospital_id), 'local');
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_set_model_route() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_set_model_route() FROM PUBLIC;
CREATE TRIGGER clinical_queries_model_route BEFORE INSERT ON clinical_queries
    FOR EACH ROW EXECUTE FUNCTION mhai_set_model_route();

-- The route a claimed question runs on: p_route only while the caller's own
-- hospital's policy still names that provider; otherwise local.
-- +goose StatementBegin
CREATE FUNCTION mhai_effective_model_route(p_hospital uuid, p_route text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE WHEN p_route IS DISTINCT FROM 'local'
                 AND p_hospital = public.mhai_current_hospital()
                 AND NOT EXISTS (SELECT 1 FROM public.hospitals hs
                                 WHERE hs.id = p_hospital AND hs.status <> 'active')
                 AND EXISTS (SELECT 1 FROM public.hospital_model_policy p
                             WHERE p.hospital_id = p_hospital
                               AND 'cloud:' || p.cloud_provider = p_route)
                THEN p_route
                ELSE 'local' END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_effective_model_route(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_effective_model_route(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_effective_model_route(uuid, text) TO mhai_worker;

------------------------------------------------------------------------
-- The ledger.
------------------------------------------------------------------------
CREATE TABLE cloud_usage (
    hospital_id uuid REFERENCES hospitals (id),
    month       date,
    used        bigint NOT NULL DEFAULT 0 CHECK (used >= 0),
    reserved    bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    PRIMARY KEY (hospital_id, month)
);
ALTER TABLE cloud_usage OWNER TO mhai_migrate;
ALTER TABLE cloud_usage ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON cloud_usage FOR SELECT TO mhai_migrate USING (true);
CREATE POLICY owner_insert ON cloud_usage FOR INSERT TO mhai_migrate WITH CHECK (true);
CREATE POLICY owner_update ON cloud_usage FOR UPDATE TO mhai_migrate USING (true) WITH CHECK (true);

CREATE TABLE cloud_reservations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    month       date NOT NULL,
    tokens      bigint NOT NULL CHECK (tokens > 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE cloud_reservations OWNER TO mhai_migrate;
ALTER TABLE cloud_reservations ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_read ON cloud_reservations FOR SELECT TO mhai_migrate USING (true);
CREATE POLICY owner_insert ON cloud_reservations FOR INSERT TO mhai_migrate WITH CHECK (true);
CREATE POLICY owner_delete ON cloud_reservations FOR DELETE TO mhai_migrate USING (true);

-- Reserve p_tokens of this month's budget for the caller's hospital, for a
-- send to p_provider, in one atomic UPDATE under the budget. No row: over
-- budget, the policy isn't enabled for p_provider right now, or the hospital
-- isn't active. The month is the database's, in UTC.
-- +goose StatementBegin
CREATE FUNCTION mhai_cloud_reserve(p_provider text, p_tokens bigint) RETURNS TABLE (reservation uuid, percent integer)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    m date := (date_trunc('month', now() AT TIME ZONE 'UTC'))::date;
    v_total bigint;
    v_budget bigint;
    v_id uuid;
BEGIN
    IF p_provider IS NULL OR p_provider NOT IN ('openai', 'anthropic', 'xai') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: unknown provider';
    END IF;
    IF p_tokens IS NULL OR p_tokens <= 0 OR p_tokens > 100000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: tokens are 1 to 100000';
    END IF;
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege',
            MESSAGE = 'mhai_cloud_reserve: no hospital';
    END IF;
    IF EXISTS (SELECT 1 FROM public.hospitals hs WHERE hs.id = h AND hs.status <> 'active') THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_usage (hospital_id, month) VALUES (h, m) ON CONFLICT DO NOTHING;
    UPDATE public.cloud_usage u
    SET reserved = u.reserved + p_tokens
    WHERE u.hospital_id = h AND u.month = m
      AND u.used + u.reserved + p_tokens <= (SELECT p.monthly_token_budget FROM public.hospital_model_policy p
                                             WHERE p.hospital_id = h AND p.cloud_provider = p_provider)
    RETURNING u.used + u.reserved,
              (SELECT p.monthly_token_budget FROM public.hospital_model_policy p WHERE p.hospital_id = h)
    INTO v_total, v_budget;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_reservations (hospital_id, month, tokens) VALUES (h, m, p_tokens)
    RETURNING id INTO v_id;
    reservation := v_id;
    percent := (v_total * 100 / v_budget)::integer;
    RETURN NEXT;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cloud_reserve(text, bigint) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cloud_reserve(text, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_cloud_reserve(text, bigint) TO mhai_worker;

-- Settle a reservation of the caller's hospital, once: its row is consumed,
-- reserved drops by its tokens and used grows by the reported use, at most
-- the tokens reserved. A NULL p_used (reply unknown) counts as all of them.
-- Another hospital's id, or one already settled, changes nothing.
-- +goose StatementBegin
CREATE FUNCTION mhai_cloud_settle(p_reservation uuid, p_used bigint) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_month date;
    v_tokens bigint;
BEGIN
    DELETE FROM public.cloud_reservations r
    WHERE r.id = p_reservation AND r.hospital_id = public.mhai_current_hospital()
    RETURNING r.month, r.tokens INTO v_month, v_tokens;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    UPDATE public.cloud_usage u
    SET reserved = u.reserved - v_tokens,
        used = u.used + least(greatest(coalesce(p_used, v_tokens), 0), v_tokens)
    WHERE u.hospital_id = public.mhai_current_hospital() AND u.month = v_month;
    RETURN true;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cloud_settle(uuid, bigint) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cloud_settle(uuid, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_cloud_settle(uuid, bigint) TO mhai_worker;

-- This month's usage of one hospital, for the platform app: one row, zeros
-- when nothing was used.
-- +goose StatementBegin
CREATE FUNCTION mhai_cloud_usage(p_hospital uuid) RETURNS TABLE (month date, used bigint, reserved bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT m.month, coalesce(u.used, 0), coalesce(u.reserved, 0)
    FROM (SELECT (date_trunc('month', now() AT TIME ZONE 'UTC'))::date AS month) m
    LEFT JOIN public.cloud_usage u ON u.hospital_id = p_hospital AND u.month = m.month
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cloud_usage(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cloud_usage(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_cloud_usage(uuid) TO mhai_platform;

------------------------------------------------------------------------
-- The audit appends: hospital.model_policy_changed, conditions kept.
------------------------------------------------------------------------
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed')
                AND hospital_id IS NULL);

DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated',
                           'hospital.model_policy_changed')
                AND actor_id IS NULL);

-- +goose Down

-- 00010's and 00014's appends, as they were.
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated')
                AND actor_id IS NULL);
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled')
                AND hospital_id IS NULL);

DROP FUNCTION mhai_cloud_usage(uuid);
DROP FUNCTION mhai_cloud_settle(uuid, bigint);
DROP FUNCTION mhai_cloud_reserve(text, bigint);
DROP TABLE cloud_reservations;
DROP TABLE cloud_usage;

DROP FUNCTION mhai_effective_model_route(uuid, text);
DROP TRIGGER clinical_queries_model_route ON clinical_queries;
DROP FUNCTION mhai_set_model_route();
ALTER TABLE clinical_queries DROP CONSTRAINT clinical_queries_model_route_check, DROP COLUMN model_route;

-- The table takes its policies and grants with it.
DROP TABLE hospital_model_policy;
