-- Clearance letters, hospital side, and the named-access switch (analysis
-- backend design §7, §8.1, §8.10, §10; plan Tasks 18-20, departures
-- 29-40; ADR 0015). NUMBERED 00034 (coordinator): EMR PR 9 takes 00032
-- and EMR PR 13 takes 00033.
--
-- Nothing here turns anything on: with no switch row the named-access
-- position is 'off', and every letter function refuses a hospital whose
-- analysis switch isn't on (not_found) or whose named-access position is
-- 'off' (named_access_off). Only the provisioning CLI's demo raise
-- (mhai_named_access_demo_raise) writes 'letters_only', and only for a
-- scope whose analysis switch carries demo references.
--
-- What it adds:
--   - mhai_clearance_rules (NOLOGIN, NOINHERIT, no members): owns the
--     triggers and functions. Its only reads: users' id, hospital, active
--     flag, email and display name; user_roles; user_mfa's enabled flag;
--     hospitals' id, status, time zone and demo flag; analysis_switch; the
--     clearance audit entries. Its only writes: the clearance tables and
--     named_access_switch.
--   - named_access_switch: off, letters_only or on, globally (NULL) and per
--     hospital; the effective position is the lower of the two
--     (mhai_named_access_position). A hospital row may not be above the
--     global row (CHECK through mhai_named_access_within_global).
--   - clearances, clearance_covered (people by user id, never by email)
--     and clearance_events (the history). The rules are triggers, for
--     every writer except a superuser acting as itself (test fixtures; the
--     core service refuses a superuser login): the statuses and who may
--     move them, the 12-month cap, narrowing only and never to nobody,
--     version up by one on every change, and the platform side's actor,
--     same_side and same_person checks (T15).
--   - Every change writes an event, and a deferred constraint trigger
--     refuses, at commit, an event without its clearance.<kind> audit
--     entry (as 00028 and 00030 do for lists and analyst changes).
--   - The gateway reads the three tables for its own hospital (RLS) and
--     writes them only through the functions. mhai_platform gets nothing
--     here: its verify, reject and revoke functions are Plan 11b's.
--   - provision_named_access_append: the CLI's named_access.changed
--     entries. platform_append is not touched.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_clearance_rules') THEN
        CREATE ROLE mhai_clearance_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    ALTER ROLE mhai_clearance_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    -- Nobody may already act as it: it writes letters and the switch.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = 'mhai_clearance_rules') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00034: mhai_clearance_rules already has members; remove them first';
    END IF;
    -- The runner acts as the owners while it hands functions over and
    -- grants on the analysis position (00021's reader owns it).
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_clearance_rules;

------------------------------------------------------------------------
-- The named-access switch (design §7).
------------------------------------------------------------------------
CREATE TABLE named_access_switch (
    hospital_id      uuid REFERENCES hospitals (id),
    position         text NOT NULL DEFAULT 'off' CHECK (position IN ('off', 'letters_only', 'on')),
    signoff_clinical text CHECK (signoff_clinical ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    signoff_privacy  text CHECK (signoff_privacy ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    signoff_legal    text CHECK (signoff_legal ~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'),
    since            timestamptz NOT NULL DEFAULT now(),
    changed_by       uuid,
    ever_signed_off  boolean NOT NULL DEFAULT false,
    CONSTRAINT named_access_switch_one UNIQUE NULLS NOT DISTINCT (hospital_id),
    CONSTRAINT named_access_switch_signed CHECK (position = 'off'
        OR (signoff_clinical IS NOT NULL AND signoff_privacy IS NOT NULL AND signoff_legal IS NOT NULL)),
    CONSTRAINT named_access_switch_one_kind CHECK (
        (COALESCE(signoff_clinical, '') = 'DEMO-SYNTHETIC') = (COALESCE(signoff_privacy, '') = 'DEMO-SYNTHETIC')
        AND (COALESCE(signoff_clinical, '') = 'DEMO-SYNTHETIC') = (COALESCE(signoff_legal, '') = 'DEMO-SYNTHETIC'))
);
ALTER TABLE named_access_switch OWNER TO mhai_migrate;
ALTER TABLE named_access_switch ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON named_access_switch FROM PUBLIC;

-- The rank of a position: off 0, letters_only 1, on 2.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_rank(p_position text) RETURNS integer
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE p_position WHEN 'on' THEN 2 WHEN 'letters_only' THEN 1 ELSE 0 END
$$;
-- +goose StatementEnd

-- A hospital's row is never above the global row (409 above_global in
-- Plan 11b's flow); the global row and 'off' always pass.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_within_global(p_hospital uuid, p_position text) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT p_hospital IS NULL OR public.mhai_named_access_rank(p_position) <=
        COALESCE((SELECT public.mhai_named_access_rank(g.position) FROM public.named_access_switch g WHERE g.hospital_id IS NULL), 0)
$$;
-- +goose StatementEnd

-- The guard: rows never move, ever_signed_off is never cleared, demo
-- references only on a demo hospital's row or the global one and never on
-- a row that was signed off.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_switch_guard() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.hospital_id IS DISTINCT FROM OLD.hospital_id THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'named_access_switch: a switch row never moves to another hospital';
        END IF;
        IF OLD.ever_signed_off AND NOT NEW.ever_signed_off THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'named_access_switch: ever_signed_off is never cleared';
        END IF;
    END IF;
    IF NEW.signoff_clinical ~ '^SO-' OR NEW.signoff_privacy ~ '^SO-' OR NEW.signoff_legal ~ '^SO-' THEN
        NEW.ever_signed_off := true;
    END IF;
    IF 'DEMO-SYNTHETIC' IN (COALESCE(NEW.signoff_clinical, ''), COALESCE(NEW.signoff_privacy, ''), COALESCE(NEW.signoff_legal, '')) THEN
        IF NEW.ever_signed_off THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'named_access_switch: a switch that was ever signed off never takes demo references';
        END IF;
        IF NEW.hospital_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.hospitals h
                                                       WHERE h.id = NEW.hospital_id AND h.is_demo) THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'named_access_switch: only a hospital created as the demo takes demo references';
        END IF;
        -- A demo global row never stands over a signed-off hospital row.
        IF NEW.hospital_id IS NULL AND EXISTS (SELECT 1 FROM public.named_access_switch h
                WHERE h.hospital_id IS NOT NULL AND h.position <> 'off' AND h.signoff_clinical ~ '^SO-') THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'named_access_switch: a signed-off hospital is on; the global row takes no demo references';
        END IF;
    END IF;
    -- A signed-off hospital row is never raised under a demo global row.
    IF NEW.hospital_id IS NOT NULL AND NEW.position <> 'off' AND NEW.signoff_clinical ~ '^SO-'
       AND EXISTS (SELECT 1 FROM public.named_access_switch g WHERE g.hospital_id IS NULL AND g.signoff_clinical = 'DEMO-SYNTHETIC') THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'named_access_switch: a signed-off hospital is never raised under a demo global row';
    END IF;
    NEW.since := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER named_access_switch_guard BEFORE INSERT OR UPDATE ON named_access_switch
    FOR EACH ROW EXECUTE FUNCTION mhai_named_access_switch_guard();
ALTER TABLE named_access_switch ADD CONSTRAINT named_access_switch_within_global
    CHECK (public.mhai_named_access_within_global(hospital_id, position));

-- mhai_named_access_position(hospital): the lower of the global and the
-- hospital's rows, 'off' when either is missing. A caller acting as
-- mhai_gateway, or with a hospital set, learns only its own hospital's.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_position(p_hospital uuid) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE
        WHEN p_hospital IS NULL THEN 'off'
        WHEN (pg_catalog.current_setting('role') = 'mhai_gateway' OR public.mhai_current_hospital() IS NOT NULL)
             AND p_hospital IS DISTINCT FROM public.mhai_current_hospital() THEN 'off'
        ELSE (ARRAY['off', 'letters_only', 'on'])[1 + LEAST(
            COALESCE((SELECT public.mhai_named_access_rank(g.position) FROM public.named_access_switch g WHERE g.hospital_id IS NULL), 0),
            COALESCE((SELECT public.mhai_named_access_rank(s.position) FROM public.named_access_switch s WHERE s.hospital_id = p_hospital), 0))]
        END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Letters (design §8.1).
------------------------------------------------------------------------
CREATE TABLE clearances (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id    uuid NOT NULL REFERENCES hospitals (id),
    status         text NOT NULL DEFAULT 'checking'
                   CHECK (status IN ('checking', 'refused', 'waiting', 'active', 'rejected', 'ended', 'revoked')),
    refused_reason text CHECK (refused_reason IN ('file_type', 'file_too_large', 'too_many_pages', 'password_protected',
                                                  'unreadable', 'failed_safety_check')),
    signed_by      text NOT NULL CHECK (signed_by ~ '^[^[:cntrl:]]{1,120}$' AND signed_by = btrim(signed_by)),
    signer_title   text NOT NULL CHECK (signer_title ~ '^[^[:cntrl:]]{1,120}$' AND signer_title = btrim(signer_title)),
    letter_date    date NOT NULL,
    end_date       date NOT NULL,
    renews         uuid,
    uploaded_by    uuid NOT NULL,
    uploaded_at    timestamptz NOT NULL DEFAULT now(),
    verified_by    uuid,
    verified_at    timestamptz,
    rejected_by    uuid,
    rejected_at    timestamptz,
    reject_reason  text CHECK (reject_reason IN ('wrong_people', 'end_date_mismatch', 'not_leadership', 'unreadable', 'other')),
    reject_note    text CHECK (char_length(reject_note) BETWEEN 1 AND 500 AND reject_note !~ '[[:cntrl:]]'),
    revoked_by     uuid,
    revoked_at     timestamptz,
    revoked_side   text CHECK (revoked_side IN ('hospital', 'platform')),
    revoke_reason  text CHECK (revoke_reason IN ('no_longer_needed', 'person_left', 'uploaded_in_error', 'security_concern', 'other')),
    revoke_note    text CHECK (char_length(revoke_note) BETWEEN 1 AND 500 AND revoke_note !~ '[[:cntrl:]]'),
    ended_at       timestamptz,
    pages          integer NOT NULL DEFAULT 0 CHECK (pages BETWEEN 0 AND 20),
    sha256         bytea CHECK (octet_length(sha256) = 32),
    demo           boolean NOT NULL DEFAULT false,
    version        integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, renews) REFERENCES clearances (hospital_id, id),
    CONSTRAINT clearances_dates CHECK (end_date > letter_date),
    CONSTRAINT clearances_refused CHECK ((status = 'refused') = (refused_reason IS NOT NULL)),
    CONSTRAINT clearances_pages CHECK ((status IN ('checking', 'refused')) = (pages = 0)
                                       AND (status IN ('checking', 'refused')) = (sha256 IS NULL)),
    CONSTRAINT clearances_verified CHECK ((status IN ('active', 'ended')) <= (verified_by IS NOT NULL AND verified_at IS NOT NULL)),
    CONSTRAINT clearances_rejected CHECK ((status = 'rejected') = (rejected_at IS NOT NULL AND reject_reason IS NOT NULL)),
    CONSTRAINT clearances_revoked CHECK ((status = 'revoked') = (revoked_at IS NOT NULL AND revoke_reason IS NOT NULL
                                                                 AND revoked_side IS NOT NULL)),
    CONSTRAINT clearances_ended CHECK ((status = 'ended') = (ended_at IS NOT NULL))
);
CREATE INDEX clearances_hospital_idx ON clearances (hospital_id, uploaded_at DESC);
CREATE INDEX clearances_active_idx ON clearances (hospital_id, end_date) WHERE status = 'active';

CREATE TABLE clearance_covered (
    hospital_id    uuid NOT NULL,
    clearance_id   uuid NOT NULL,
    user_id        uuid NOT NULL,
    side           text NOT NULL CHECK (side IN ('hospital', 'platform')),
    removed_at     timestamptz,
    removed_by     uuid,
    removed_reason text CHECK (removed_reason IN ('person_left', 'no_longer_needed', 'uploaded_in_error', 'other')),
    removed_note   text CHECK (char_length(removed_note) BETWEEN 1 AND 500 AND removed_note !~ '[[:cntrl:]]'),
    PRIMARY KEY (clearance_id, user_id),
    FOREIGN KEY (hospital_id, clearance_id) REFERENCES clearances (hospital_id, id),
    CONSTRAINT clearance_covered_removed CHECK ((removed_at IS NULL) = (removed_reason IS NULL)
                                                AND (removed_at IS NOT NULL OR removed_note IS NULL))
);
CREATE INDEX clearance_covered_user_idx ON clearance_covered (user_id) WHERE removed_at IS NULL;

CREATE TABLE clearance_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq          bigint GENERATED ALWAYS AS IDENTITY,
    hospital_id  uuid NOT NULL,
    clearance_id uuid NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('uploaded', 'refused', 'ready', 'verified', 'rejected', 'narrowed',
                                               'revoked', 'ended', 'reminded')),
    at           timestamptz NOT NULL DEFAULT now(),
    by_user      uuid,
    side         text CHECK (side IN ('hospital', 'platform')),
    reason       text CHECK (reason ~ '^[a-z_]{1,40}$'),
    note         text CHECK (char_length(note) BETWEEN 1 AND 500 AND note !~ '[[:cntrl:]]'),
    person       uuid,
    days         integer CHECK (days IN (30, 7, 1)),
    xact         xid8 NOT NULL DEFAULT pg_current_xact_id(),
    FOREIGN KEY (hospital_id, clearance_id) REFERENCES clearances (hospital_id, id),
    CONSTRAINT clearance_events_person CHECK ((kind = 'narrowed') = (person IS NOT NULL)),
    CONSTRAINT clearance_events_days CHECK ((kind = 'reminded') = (days IS NOT NULL))
);
CREATE INDEX clearance_events_letter_idx ON clearance_events (hospital_id, clearance_id, seq);
CREATE INDEX clearance_events_xact_idx ON clearance_events (xact);
CREATE UNIQUE INDEX clearance_events_reminded_idx ON clearance_events (clearance_id, days) WHERE kind = 'reminded';

-- The upload's eligibility checks, per person and hospital day (security
-- review: the check is not a lookup of who is a platform admin). Each check
-- reserves a slot first, in its own committed transaction
-- (mhai_clearance_reserve_check, one row lock: parallel checks serialize);
-- a check that wasn't an eligibility refusal gives its slot back. Past 5 a
-- day the reservation, and the check itself, are refused.
CREATE TABLE clearance_check_refusals (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    day         date NOT NULL,
    refused     integer NOT NULL CHECK (refused >= 0),
    PRIMARY KEY (hospital_id, user_id, day)
);
ALTER TABLE clearance_check_refusals OWNER TO mhai_migrate;
ALTER TABLE clearance_check_refusals ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON clearance_check_refusals FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON clearance_check_refusals TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON clearance_check_refusals TO mhai_clearance_rules USING (true) WITH CHECK (true);

ALTER TABLE clearances OWNER TO mhai_migrate;
ALTER TABLE clearance_covered OWNER TO mhai_migrate;
ALTER TABLE clearance_events OWNER TO mhai_migrate;
ALTER TABLE clearances ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE clearance_covered ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE clearance_events ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON clearances, clearance_covered, clearance_events FROM PUBLIC;

-- The entry an event names, found at once (the commit check).
CREATE INDEX audit_log_clearance_event_idx ON audit_log ((payload ->> 'event_id'))
    WHERE action IN ('clearance.uploaded', 'clearance.refused', 'clearance.ready', 'clearance.verified',
                     'clearance.rejected', 'clearance.narrowed', 'clearance.revoked', 'clearance.ended',
                     'clearance.reminder_sent');

-- The gateway reads its own hospital's letters; it writes none directly.
GRANT SELECT ON clearances, clearance_covered, clearance_events TO mhai_gateway;
CREATE POLICY gateway_read ON clearances FOR SELECT TO mhai_gateway USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY gateway_read ON clearance_covered FOR SELECT TO mhai_gateway USING (hospital_id = public.mhai_current_hospital());
CREATE POLICY gateway_read ON clearance_events FOR SELECT TO mhai_gateway USING (hospital_id = public.mhai_current_hospital());

-- The rules' owner: what the rules read and write, and nothing else.
GRANT SELECT, INSERT, UPDATE ON clearances, clearance_covered TO mhai_clearance_rules;
GRANT SELECT, INSERT ON clearance_events TO mhai_clearance_rules;
GRANT SELECT, INSERT, UPDATE ON named_access_switch TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON clearances TO mhai_clearance_rules USING (true) WITH CHECK (true);
CREATE POLICY clearance_rules ON clearance_covered TO mhai_clearance_rules USING (true) WITH CHECK (true);
CREATE POLICY clearance_rules ON clearance_events TO mhai_clearance_rules USING (true) WITH CHECK (true);
CREATE POLICY clearance_rules ON named_access_switch TO mhai_clearance_rules USING (true) WITH CHECK (true);
GRANT SELECT (id, hospital_id, is_active, email, display_name) ON users TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON users FOR SELECT TO mhai_clearance_rules USING (true);
GRANT SELECT ON user_roles TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON user_roles FOR SELECT TO mhai_clearance_rules USING (true);
GRANT SELECT (user_id, enabled) ON user_mfa TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON user_mfa FOR SELECT TO mhai_clearance_rules USING (true);
GRANT SELECT (id, status, time_zone, is_demo) ON hospitals TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON hospitals FOR SELECT TO mhai_clearance_rules USING (true);
GRANT SELECT ON analysis_switch TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON analysis_switch FOR SELECT TO mhai_clearance_rules USING (true);
GRANT SELECT (id, hospital_id, actor_id, action, entity, entity_id, payload) ON audit_log TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON audit_log FOR SELECT TO mhai_clearance_rules
    USING (action IN ('clearance.uploaded', 'clearance.refused', 'clearance.ready', 'clearance.verified',
                      'clearance.rejected', 'clearance.narrowed', 'clearance.revoked', 'clearance.ended',
                      'clearance.reminder_sent'));
GRANT EXECUTE ON FUNCTION mhai_analysis_position(uuid) TO mhai_clearance_rules;

-- The switch's writers here: the CLI's lowering (position 'off' only) and
-- the demo raise (below). The platform's two-person raise, with its grants
-- and its audit check, is Plan 11b's (security review: no write path that
-- nothing audits).
GRANT SELECT ON named_access_switch TO mhai_provision;
GRANT UPDATE (position) ON named_access_switch TO mhai_provision;
CREATE POLICY provision_read ON named_access_switch FOR SELECT TO mhai_provision USING (true);
CREATE POLICY provision_switch ON named_access_switch FOR UPDATE TO mhai_provision USING (true) WITH CHECK (position = 'off');

------------------------------------------------------------------------
-- Helpers (no EXECUTE for anyone but their owner).
------------------------------------------------------------------------

-- A superuser acting as itself: exempt (see the header).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_exempt() RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT pg_catalog.current_setting('role') = 'none'
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname = session_user AND r.rolsuper)
$$;
-- +goose StatementEnd

-- Who is writing: inside a definer function current_user is the owner,
-- but the role setting is still the caller's SET ROLE.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_actor() RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE pg_catalog.current_setting('role')
        WHEN 'mhai_gateway' THEN 'hospital'
        WHEN 'mhai_platform' THEN 'platform'
        ELSE 'other' END
$$;
-- +goose StatementEnd

-- The hospital's day at p_at, in its time zone.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_day(p_hospital uuid, p_at timestamptz) RETURNS date
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_tz text;
BEGIN
    SELECT h.time_zone INTO v_tz FROM public.hospitals h WHERE h.id = p_hospital;
    IF v_tz IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    RETURN (p_at AT TIME ZONE v_tz)::date;
END
$$;
-- +goose StatementEnd

-- An active hospital admin of p_hospital with two-step on.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_is_admin(p_user uuid, p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT p_user IS NOT NULL AND p_hospital IS NOT NULL AND EXISTS (
        SELECT 1 FROM public.users u
        JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'hospital_admin'
        JOIN public.user_mfa m ON m.user_id = u.id AND m.enabled
        WHERE u.id = p_user AND u.hospital_id = p_hospital AND u.is_active)
$$;
-- +goose StatementEnd

-- A person a letter may cover at p_hospital: active, a hospital admin or
-- an analyst, two-step on.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_staff_ok(p_user uuid, p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.users u JOIN public.user_mfa m ON m.user_id = u.id AND m.enabled
        WHERE u.id = p_user AND u.hospital_id = p_hospital AND u.is_active
          AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role IN ('hospital_admin', 'analyst')))
$$;
-- +goose StatementEnd

-- An active platform admin with two-step on.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_platform_ok(p_user uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.users u
        JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'platform_admin'
        JOIN public.user_mfa m ON m.user_id = u.id AND m.enabled
        WHERE u.id = p_user AND u.hospital_id IS NULL AND u.is_active)
$$;
-- +goose StatementEnd

-- same_side (design §8.10; plan departure 32): the platform admin's email
-- domain is the domain of an account of the hospital, active or not. No
-- hospital has a list of allowed domains yet; a shared domain refuses
-- (fails closed). Plan 11b may add a domain list and account links here.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_same_side(p_user uuid, p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.users v, public.users h
        WHERE v.id = p_user AND h.hospital_id = p_hospital
          AND pg_catalog.lower(pg_catalog.split_part(v.email::text, '@', 2))
              = pg_catalog.lower(pg_catalog.split_part(h.email::text, '@', 2)))
$$;
-- +goose StatementEnd

-- The platform side's checks on a verify or reject (T15): an
-- active platform admin with two-step (forbidden), not covered by the
-- letter, removed or not (same_person), not linked to the hospital
-- (same_side).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_check_platform(p_letter uuid, p_hospital uuid, p_user uuid) RETURNS void
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT public.mhai_clearance_platform_ok(p_user) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    IF EXISTS (SELECT 1 FROM public.clearance_covered k WHERE k.clearance_id = p_letter AND k.user_id = p_user) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'same_person';
    END IF;
    IF public.mhai_clearance_same_side(p_user, p_hospital) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'same_side';
    END IF;
END
$$;
-- +goose StatementEnd

-- Letters here are demo letters: the hospital was created as the demo and
-- its analysis switch carries demo references.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_demo_hospital(p_hospital uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.hospitals h WHERE h.id = p_hospital AND h.is_demo)
       AND EXISTS (SELECT 1 FROM public.analysis_switch s WHERE s.hospital_id = p_hospital
                     AND s.signoff_clinical = 'DEMO-SYNTHETIC')
$$;
-- +goose StatementEnd

-- The two gates: analysis on (not_found), named access at letters_only
-- or on (named_access_off).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_gate_for(p_hospital uuid) RETURNS void
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_hospital IS NULL OR public.mhai_analysis_position(p_hospital) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    IF public.mhai_named_access_position(p_hospital) = 'off' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'named_access_off';
    END IF;
END
$$;
-- +goose StatementEnd

-- The audit action of an event kind.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_action(p_kind text) RETURNS text
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE p_kind WHEN 'reminded' THEN 'clearance.reminder_sent' ELSE 'clearance.' || p_kind END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The rules on clearances.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_actor text;
    v_user  uuid;
    v_today date;
    v_group text;
BEGIN
    IF public.mhai_clearance_exempt() THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'a letter is never deleted';
    END IF;
    v_actor := public.mhai_clearance_actor();
    v_user := public.mhai_current_user();

    IF TG_OP = 'INSERT' THEN
        IF v_actor <> 'hospital' OR NEW.hospital_id IS DISTINCT FROM public.mhai_current_hospital() THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
        PERFORM public.mhai_clearance_gate_for(NEW.hospital_id);
        IF NEW.uploaded_by IS DISTINCT FROM v_user OR NOT public.mhai_clearance_is_admin(v_user, NEW.hospital_id) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
        IF NEW.status <> 'checking' OR NEW.pages <> 0 OR NEW.sha256 IS NOT NULL OR NEW.refused_reason IS NOT NULL
           OR NEW.verified_by IS NOT NULL OR NEW.verified_at IS NOT NULL
           OR NEW.rejected_by IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.reject_reason IS NOT NULL OR NEW.reject_note IS NOT NULL
           OR NEW.revoked_by IS NOT NULL OR NEW.revoked_at IS NOT NULL OR NEW.revoked_side IS NOT NULL
           OR NEW.revoke_reason IS NOT NULL OR NEW.revoke_note IS NOT NULL OR NEW.ended_at IS NOT NULL THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'a letter starts as checking';
        END IF;
        v_today := public.mhai_clearance_day(NEW.hospital_id, now());
        IF NEW.end_date <= v_today OR NEW.end_date > (v_today + interval '12 months')::date THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'end_date_too_late';
        END IF;
        IF NEW.letter_date > v_today OR NEW.letter_date < (v_today - interval '12 months')::date THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
        END IF;
        IF NEW.renews IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.clearances c
                                                  WHERE c.id = NEW.renews AND c.hospital_id = NEW.hospital_id) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
        END IF;
        NEW.version := 1;
        NEW.uploaded_at := now();
        NEW.demo := public.mhai_clearance_demo_hospital(NEW.hospital_id);
        RETURN NEW;
    END IF;

    -- UPDATE: the letter's facts never change.
    IF (NEW.id, NEW.hospital_id, NEW.signed_by, NEW.signer_title, NEW.letter_date, NEW.end_date, NEW.renews,
        NEW.uploaded_by, NEW.uploaded_at, NEW.demo)
       IS DISTINCT FROM (OLD.id, OLD.hospital_id, OLD.signed_by, OLD.signer_title, OLD.letter_date, OLD.end_date, OLD.renews,
        OLD.uploaded_by, OLD.uploaded_at, OLD.demo) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'a letter''s facts never change';
    END IF;
    NEW.version := OLD.version + 1;
    v_today := public.mhai_clearance_day(OLD.hospital_id, now());

    IF NEW.status = OLD.status THEN
        -- Narrowing: nothing but the version changes, by a hospital admin
        -- of the hospital, while the letter can still cover anyone.
        v_group := 'none';
        IF OLD.status NOT IN ('checking', 'waiting', 'active') THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
        END IF;
        IF v_actor <> 'hospital' OR OLD.hospital_id IS DISTINCT FROM public.mhai_current_hospital()
           OR NOT public.mhai_clearance_is_admin(v_user, OLD.hospital_id) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
    ELSIF OLD.status = 'checking' AND NEW.status IN ('waiting', 'refused') THEN
        -- The documents outcome: only through its function, which marks
        -- the letter it acts on.
        v_group := 'outcome';
        IF v_actor <> 'hospital' OR pg_catalog.current_setting('mhai.clearance_outcome', true) IS DISTINCT FROM OLD.id::text THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
    ELSIF OLD.status = 'waiting' AND NEW.status IN ('active', 'rejected') THEN
        v_group := CASE NEW.status WHEN 'active' THEN 'verify' ELSE 'reject' END;
        IF v_actor <> 'platform' THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
        PERFORM public.mhai_clearance_check_platform(OLD.id, OLD.hospital_id, v_user);
        IF NEW.status = 'active' AND OLD.end_date < v_today THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'the letter has ended';
        END IF;
    ELSIF OLD.status IN ('waiting', 'active') AND NEW.status = 'revoked' THEN
        v_group := 'revoke';
        IF v_actor = 'hospital' AND OLD.hospital_id = public.mhai_current_hospital()
           AND public.mhai_clearance_is_admin(v_user, OLD.hospital_id) THEN
            NEW.revoked_side := 'hospital';
        ELSIF v_actor = 'platform' THEN
            -- Revoking only takes access away: any active platform admin
            -- with two-step, linked or covered or not (security review).
            IF NOT public.mhai_clearance_platform_ok(v_user) THEN
                RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
            END IF;
            NEW.revoked_side := 'platform';
        ELSE
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
    ELSIF OLD.status = 'active' AND NEW.status = 'ended' THEN
        -- Anyone may record what the date already decided.
        v_group := 'end';
        IF OLD.end_date >= v_today THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'the letter has not ended';
        END IF;
    ELSE
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'not a transition a letter makes';
    END IF;

    -- Only the transition's own columns change.
    IF v_group <> 'outcome' AND (NEW.refused_reason, NEW.pages, NEW.sha256)
       IS DISTINCT FROM (OLD.refused_reason, OLD.pages, OLD.sha256) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    IF v_group <> 'verify' AND (NEW.verified_by, NEW.verified_at) IS DISTINCT FROM (OLD.verified_by, OLD.verified_at) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    IF v_group <> 'reject' AND (NEW.rejected_by, NEW.rejected_at, NEW.reject_reason, NEW.reject_note)
       IS DISTINCT FROM (OLD.rejected_by, OLD.rejected_at, OLD.reject_reason, OLD.reject_note) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    IF v_group <> 'revoke' AND (NEW.revoked_by, NEW.revoked_at, NEW.revoked_side, NEW.revoke_reason, NEW.revoke_note)
       IS DISTINCT FROM (OLD.revoked_by, OLD.revoked_at, OLD.revoked_side, OLD.revoke_reason, OLD.revoke_note) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    IF v_group <> 'end' AND NEW.ended_at IS DISTINCT FROM OLD.ended_at THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    -- Who and when are the server's.
    CASE v_group
        WHEN 'verify' THEN NEW.verified_by := v_user; NEW.verified_at := now();
        WHEN 'reject' THEN NEW.rejected_by := v_user; NEW.rejected_at := now();
        WHEN 'revoke' THEN NEW.revoked_by := v_user; NEW.revoked_at := now();
        WHEN 'end' THEN NEW.ended_at := now();
        ELSE NULL;
    END CASE;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- After a letter is inserted or changes status: its event.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_after() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_clearance_exempt() THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO public.clearance_events (hospital_id, clearance_id, kind, by_user, side)
        VALUES (NEW.hospital_id, NEW.id, 'uploaded', NEW.uploaded_by, 'hospital');
        RETURN NULL;
    END IF;
    IF NEW.status = OLD.status THEN
        RETURN NULL;
    END IF;
    INSERT INTO public.clearance_events (hospital_id, clearance_id, kind, by_user, side, reason, note)
    VALUES (NEW.hospital_id, NEW.id,
        CASE NEW.status WHEN 'waiting' THEN 'ready' WHEN 'refused' THEN 'refused' WHEN 'active' THEN 'verified'
            WHEN 'rejected' THEN 'rejected' WHEN 'revoked' THEN 'revoked' WHEN 'ended' THEN 'ended' END,
        CASE NEW.status WHEN 'active' THEN NEW.verified_by WHEN 'rejected' THEN NEW.rejected_by
            WHEN 'revoked' THEN NEW.revoked_by END,
        CASE NEW.status WHEN 'active' THEN 'platform' WHEN 'rejected' THEN 'platform' WHEN 'revoked' THEN NEW.revoked_side END,
        CASE NEW.status WHEN 'refused' THEN NEW.refused_reason WHEN 'rejected' THEN NEW.reject_reason
            WHEN 'revoked' THEN NEW.revoke_reason END,
        CASE NEW.status WHEN 'rejected' THEN NEW.reject_note WHEN 'revoked' THEN NEW.revoke_note END);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The rules on the people covered.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_covered_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_status text;
    v_user   uuid;
BEGIN
    IF public.mhai_clearance_exempt() THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'a person is removed, never deleted';
    END IF;
    v_user := public.mhai_current_user();
    IF TG_OP = 'INSERT' THEN
        IF public.mhai_clearance_actor() <> 'hospital' OR NEW.hospital_id IS DISTINCT FROM public.mhai_current_hospital() THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
        -- Only with the letter, in its own transaction.
        SELECT c.status INTO v_status FROM public.clearances c
        WHERE c.id = NEW.clearance_id AND c.hospital_id = NEW.hospital_id;
        IF v_status IS DISTINCT FROM 'checking' OR NOT EXISTS (
                SELECT 1 FROM public.clearance_events e WHERE e.clearance_id = NEW.clearance_id AND e.kind = 'uploaded'
                  AND e.xact = pg_catalog.pg_current_xact_id()) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'people are named only when the letter is uploaded';
        END IF;
        IF NEW.removed_at IS NOT NULL OR NEW.removed_by IS NOT NULL OR NEW.removed_reason IS NOT NULL OR NEW.removed_note IS NOT NULL THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
        END IF;
        IF NEW.side = 'hospital' AND NOT public.mhai_clearance_staff_ok(NEW.user_id, NEW.hospital_id) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible';
        END IF;
        IF NEW.side = 'platform' AND NOT public.mhai_clearance_platform_ok(NEW.user_id) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible_platform';
        END IF;
        IF (SELECT count(*) FROM public.clearance_covered k WHERE k.clearance_id = NEW.clearance_id) >= 20 THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid', DETAIL = 'at most 20 people';
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE: a removal only.
    IF (NEW.hospital_id, NEW.clearance_id, NEW.user_id, NEW.side) IS DISTINCT FROM (OLD.hospital_id, OLD.clearance_id, OLD.user_id, OLD.side)
       OR OLD.removed_at IS NOT NULL OR NEW.removed_reason IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'people are only ever removed';
    END IF;
    IF public.mhai_clearance_actor() <> 'hospital' OR OLD.hospital_id IS DISTINCT FROM public.mhai_current_hospital()
       OR NOT public.mhai_clearance_is_admin(v_user, OLD.hospital_id) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    -- One narrowing of a letter at a time; fresh statements after the lock.
    SELECT c.status INTO v_status FROM public.clearances c WHERE c.id = OLD.clearance_id FOR UPDATE;
    IF v_status NOT IN ('checking', 'waiting', 'active') THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.clearance_covered k
                   WHERE k.clearance_id = OLD.clearance_id AND k.user_id <> OLD.user_id AND k.removed_at IS NULL) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'would_empty';
    END IF;
    NEW.removed_at := now();
    NEW.removed_by := v_user;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_covered_after() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_clearance_exempt() OR OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL THEN
        RETURN NULL;
    END IF;
    INSERT INTO public.clearance_events (hospital_id, clearance_id, kind, by_user, side, reason, note, person)
    VALUES (NEW.hospital_id, NEW.clearance_id, 'narrowed', NEW.removed_by, 'hospital', NEW.removed_reason,
            NEW.removed_note, NEW.user_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- The commit check: an event is kept only with its audit entry.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_event_audited() RETURNS trigger
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- A superuser acting as itself (test fixtures) is exempt, as it is
    -- from every rule here.
    IF public.mhai_clearance_exempt() THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.action IN ('clearance.uploaded', 'clearance.refused', 'clearance.ready', 'clearance.verified',
                                      'clearance.rejected', 'clearance.narrowed', 'clearance.revoked', 'clearance.ended',
                                      'clearance.reminder_sent')
                     AND a.payload ->> 'event_id' = NEW.id::text
                     AND a.action = public.mhai_clearance_action(NEW.kind)
                     AND a.entity = 'clearance' AND a.entity_id = NEW.clearance_id::text
                     AND a.hospital_id = NEW.hospital_id
                     AND a.actor_id IS NOT DISTINCT FROM NEW.by_user) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'clearance_events: a letter change without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Nobody writes an event by hand: the triggers and the nightly function do.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_event_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_clearance_exempt() THEN
        IF TG_OP = 'INSERT' THEN
            RETURN NEW;
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'an event never changes';
    END IF;
    NEW.at := now();
    NEW.xact := pg_catalog.pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER clearances_rules BEFORE INSERT OR UPDATE OR DELETE ON clearances
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_rules();
CREATE TRIGGER clearances_after AFTER INSERT OR UPDATE ON clearances
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_after();
CREATE TRIGGER clearance_covered_rules BEFORE INSERT OR UPDATE OR DELETE ON clearance_covered
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_covered_rules();
CREATE TRIGGER clearance_covered_after AFTER UPDATE ON clearance_covered
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_covered_after();
CREATE TRIGGER clearance_events_rules BEFORE INSERT OR UPDATE OR DELETE ON clearance_events
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_event_rules();
CREATE CONSTRAINT TRIGGER clearance_events_audited AFTER INSERT ON clearance_events
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_clearance_event_audited();

------------------------------------------------------------------------
-- The gateway's functions. Each takes the hospital from
-- mhai_current_hospital() and the person from mhai_current_user().
------------------------------------------------------------------------

-- mhai_clearance_gate(): the two gates, then the caller still an active
-- hospital admin with two-step (forbidden).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_gate() RETURNS void
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_clearance_gate_for(public.mhai_current_hospital());
    IF NOT public.mhai_clearance_is_admin(public.mhai_current_user(), public.mhai_current_hospital()) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_today(): the hospital's day, the last end date a letter
-- may have, the hospital's zone and whether its letters are demo letters.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_today(OUT today date, OUT max_end_date date, OUT time_zone text, OUT demo boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_current_hospital();
BEGIN
    SELECT h.time_zone INTO time_zone FROM public.hospitals h WHERE h.id = v_h;
    IF time_zone IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    today := (now() AT TIME ZONE time_zone)::date;
    max_end_date := (today + interval '12 months')::date;
    demo := public.mhai_clearance_demo_hospital(v_h);
END
$$;
-- +goose StatementEnd

-- mhai_clearance_check(...): the upload's refusals before anything is
-- stored (design §8.1): the dates (end_date_too_late, invalid), then the
-- people who aren't eligible, the platform admins' ids for the emails in
-- order, and whether renews names a letter of this hospital.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_check(p_letter_date date, p_end_date date, p_renews uuid, p_staff uuid[], p_emails citext[],
    OUT bad_staff uuid[], OUT bad_emails text[], OUT platform_ids uuid[], OUT renews_ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_current_hospital();
    v_today date;
    v_e     public.citext;
    v_id    uuid;
BEGIN
    PERFORM public.mhai_clearance_gate();
    -- No letter is stored outside the demo until the documents service
    -- exists, and nothing is looked up for one either (security review).
    IF NOT public.mhai_clearance_demo_hospital(v_h) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'unavailable';
    END IF;
    v_today := public.mhai_clearance_day(v_h, now());
    -- Reserve, then check: a slot taken today, within the limit, or no check.
    IF COALESCE((SELECT r.refused FROM public.clearance_check_refusals r WHERE r.hospital_id = v_h
                   AND r.user_id = public.mhai_current_user() AND r.day = v_today), 0) NOT BETWEEN 1 AND 5 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'rate_limited';
    END IF;
    IF p_end_date IS NULL OR p_end_date <= v_today OR p_end_date > (v_today + interval '12 months')::date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'end_date_too_late';
    END IF;
    IF p_letter_date IS NULL OR p_letter_date > v_today OR p_letter_date < (v_today - interval '12 months')::date
       OR p_letter_date >= p_end_date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF COALESCE(cardinality(p_staff), 0) + COALESCE(cardinality(p_emails), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    SELECT COALESCE(array_agg(s ORDER BY o), '{}') INTO bad_staff
    FROM unnest(COALESCE(p_staff, '{}')) WITH ORDINALITY AS t(s, o)
    WHERE NOT public.mhai_clearance_staff_ok(s, v_h);
    bad_emails := '{}';
    platform_ids := '{}';
    FOREACH v_e IN ARRAY COALESCE(p_emails, '{}') LOOP
        SELECT u.id INTO v_id FROM public.users u WHERE u.email OPERATOR(public.=) v_e AND u.hospital_id IS NULL;
        IF v_id IS NULL OR NOT public.mhai_clearance_platform_ok(v_id) THEN
            bad_emails := bad_emails || v_e::text;
        ELSE
            platform_ids := platform_ids || v_id;
        END IF;
        v_id := NULL;
    END LOOP;
    renews_ok := p_renews IS NULL OR EXISTS (SELECT 1 FROM public.clearances c WHERE c.id = p_renews AND c.hospital_id = v_h);
END
$$;
-- +goose StatementEnd

-- mhai_clearance_create(...): a new letter, checking, and its people. The
-- triggers hold every rule; the caller appends the pending entries.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_create(p_signed_by text, p_signer_title text, p_letter_date date, p_end_date date,
    p_renews uuid, p_staff uuid[], p_platform uuid[]) RETURNS uuid
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h  uuid := public.mhai_current_hospital();
    v_id uuid;
BEGIN
    PERFORM public.mhai_clearance_gate();
    IF COALESCE(cardinality(p_staff), 0) + COALESCE(cardinality(p_platform), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    INSERT INTO public.clearances (hospital_id, signed_by, signer_title, letter_date, end_date, renews, uploaded_by)
    VALUES (v_h, p_signed_by, p_signer_title, p_letter_date, p_end_date, p_renews, public.mhai_current_user())
    RETURNING id INTO v_id;
    INSERT INTO public.clearance_covered (hospital_id, clearance_id, user_id, side)
    SELECT v_h, v_id, s, 'hospital' FROM unnest(COALESCE(p_staff, '{}')) s;
    INSERT INTO public.clearance_covered (hospital_id, clearance_id, user_id, side)
    SELECT v_h, v_id, s, 'platform' FROM unnest(COALESCE(p_platform, '{}')) s;
    RETURN v_id;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_demo_ready(id, pages, sha256): the demo's documents
-- outcome (fixture pages). Refused for a letter that isn't a demo letter:
-- a live letter waits for the documents service.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_demo_ready(p_id uuid, p_pages integer, p_sha256 bytea) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_clearance_gate();
    IF NOT EXISTS (SELECT 1 FROM public.clearances c WHERE c.id = p_id AND c.hospital_id = public.mhai_current_hospital()
                     AND c.demo AND c.status = 'checking') THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'unavailable';
    END IF;
    PERFORM pg_catalog.set_config('mhai.clearance_outcome', p_id::text, true);
    UPDATE public.clearances SET status = 'waiting', pages = p_pages, sha256 = p_sha256 WHERE id = p_id;
    PERFORM pg_catalog.set_config('mhai.clearance_outcome', '', true);
END
$$;
-- +goose StatementEnd

-- The caller's letter, locked, at the version the caller saw (any version
-- when p_version is null).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_lock(p_id uuid, p_version integer) RETURNS text
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_status  text;
    v_version integer;
BEGIN
    SELECT c.status, c.version INTO v_status, v_version FROM public.clearances c
    WHERE c.id = p_id AND c.hospital_id = public.mhai_current_hospital() FOR UPDATE;
    IF v_status IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    IF p_version IS NOT NULL AND p_version <> v_version THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    RETURN v_status;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_revoke(id, version, reason, note): the hospital side's
-- revoke of a waiting or active letter; it takes effect at once.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_revoke(p_id uuid, p_version integer, p_reason text, p_note text) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_clearance_gate();
    IF public.mhai_clearance_lock(p_id, p_version) NOT IN ('waiting', 'active') THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    UPDATE public.clearances SET status = 'revoked', revoke_reason = p_reason, revoke_note = p_note WHERE id = p_id;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_narrow(id, version, remove, reason, note): removes people
-- still covered, never the last (would_empty); version required.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_narrow(p_id uuid, p_version integer, p_remove uuid[], p_reason text, p_note text) RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_n integer;
BEGIN
    PERFORM public.mhai_clearance_gate();
    IF p_version IS NULL OR COALESCE(cardinality(p_remove), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF public.mhai_clearance_lock(p_id, p_version) NOT IN ('checking', 'waiting', 'active') THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    SELECT count(*) INTO v_n FROM public.clearance_covered k
    WHERE k.clearance_id = p_id AND k.removed_at IS NULL AND k.user_id = ANY (p_remove);
    IF v_n <> (SELECT count(DISTINCT r) FROM unnest(p_remove) r) OR v_n <> cardinality(p_remove) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid', DETAIL = 'remove only people the letter still covers';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.clearance_covered k
                   WHERE k.clearance_id = p_id AND k.removed_at IS NULL AND NOT (k.user_id = ANY (p_remove))) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'would_empty';
    END IF;
    UPDATE public.clearance_covered k SET removed_reason = p_reason, removed_note = p_note,
        removed_at = now(), removed_by = public.mhai_current_user()
    WHERE k.clearance_id = p_id AND k.user_id = ANY (p_remove);
    UPDATE public.clearances SET version = version WHERE id = p_id;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_reserve_check(): takes one of the caller's eligibility
-- check slots for the hospital's day and returns the day's count (call it
-- in its own transaction and commit; past 5, refuse). The upsert's row
-- lock serializes parallel reservations. mhai_clearance_release_check()
-- gives one back (a check that wasn't an eligibility refusal).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_reserve_check() RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_current_hospital();
    v_u uuid := public.mhai_current_user();
    v_n integer;
BEGIN
    IF v_h IS NULL OR v_u IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    INSERT INTO public.clearance_check_refusals AS r (hospital_id, user_id, day, refused)
    VALUES (v_h, v_u, public.mhai_clearance_day(v_h, now()), 1)
    ON CONFLICT (hospital_id, user_id, day) DO UPDATE SET refused = r.refused + 1
    RETURNING r.refused INTO v_n;
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_release_check() RETURNS void
    LANGUAGE sql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    UPDATE public.clearance_check_refusals r SET refused = r.refused - 1
    WHERE r.hospital_id = public.mhai_current_hospital() AND r.user_id = public.mhai_current_user()
      AND r.day = public.mhai_clearance_day(public.mhai_current_hospital(), now()) AND r.refused > 0
$$;
-- +goose StatementEnd

-- mhai_clearance_names(users): display names, for this hospital's people
-- and the platform admins Measured Health has confirmed on its letters (a
-- verified letter names them) or who acted on them. A platform admin
-- named only on an unverified letter is not shown (security review: the
-- upload is not a lookup of platform accounts).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_names(p_users uuid[]) RETURNS TABLE (id uuid, display_name text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_current_hospital();
BEGIN
    PERFORM public.mhai_clearance_gate();
    RETURN QUERY
    SELECT u.id, u.display_name FROM public.users u
    WHERE u.id = ANY (p_users)
      AND (u.hospital_id = v_h
           OR (u.hospital_id IS NULL AND (
                EXISTS (SELECT 1 FROM public.clearance_covered k JOIN public.clearances c ON c.id = k.clearance_id
                        WHERE k.hospital_id = v_h AND k.user_id = u.id AND c.verified_at IS NOT NULL)
                OR EXISTS (SELECT 1 FROM public.clearance_events e WHERE e.hospital_id = v_h AND e.by_user = u.id
                             AND e.side = 'platform'))));
END
$$;
-- +goose StatementEnd

-- mhai_clearance_pending(): this transaction's events with no audit entry
-- yet, in order, for the request's hospital. The caller appends one entry
-- per row before commit.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_pending()
    RETURNS TABLE (event_id uuid, hospital_id uuid, clearance_id uuid, kind text, by_user uuid, side text, reason text,
                   person uuid, days integer, pages integer)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT e.id, e.hospital_id, e.clearance_id, e.kind, e.by_user, e.side, e.reason, e.person, e.days, c.pages
    FROM public.clearance_events e JOIN public.clearances c ON c.id = e.clearance_id
    WHERE e.xact = pg_catalog.pg_current_xact_id()
      AND e.hospital_id = public.mhai_current_hospital()
      AND NOT EXISTS (SELECT 1 FROM public.audit_log a
                      WHERE a.action IN ('clearance.uploaded', 'clearance.refused', 'clearance.ready', 'clearance.verified',
                                         'clearance.rejected', 'clearance.narrowed', 'clearance.revoked', 'clearance.ended',
                                         'clearance.reminder_sent')
                        AND a.payload ->> 'event_id' = e.id::text)
    ORDER BY e.seq
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Ending and reminders (design §8.1, §11 step 3).
------------------------------------------------------------------------

-- mhai_clearance_due(now): the hospitals with an active letter to end, or
-- a reminder to record, at p_now (the nightly run, as mhai_analysis_job).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_due(p_now timestamptz) RETURNS SETOF uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'clearance: not a time that has passed';
    END IF;
    RETURN QUERY
    SELECT DISTINCT c.hospital_id FROM public.clearances c JOIN public.hospitals h ON h.id = c.hospital_id
    WHERE c.status = 'active'
      AND (c.end_date < (p_now AT TIME ZONE h.time_zone)::date
           OR (c.end_date - (p_now AT TIME ZONE h.time_zone)::date <= 30
               AND NOT EXISTS (SELECT 1 FROM public.clearance_events e WHERE e.clearance_id = c.id AND e.kind = 'reminded'
                                 AND e.days = CASE WHEN c.end_date - (p_now AT TIME ZONE h.time_zone)::date <= 1 THEN 1
                                                   WHEN c.end_date - (p_now AT TIME ZONE h.time_zone)::date <= 7 THEN 7
                                                   ELSE 30 END)))
    ORDER BY 1;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_nightly(now): for the request's hospital, ends every
-- active letter whose last day has passed there, and records the
-- reminder due for each other active letter: the smallest of 30, 7 and 1
-- days that applies, once per letter and number. Returns the events
-- written; the caller appends their entries.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_nightly(p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_current_hospital();
    v_today date;
    v_ended integer;
    v_rem   integer;
BEGIN
    IF v_h IS NULL OR p_now IS NULL OR p_now > now() OR p_now < now() - interval '1 hour' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'clearance: no hospital, or not the last hour';
    END IF;
    v_today := public.mhai_clearance_day(v_h, p_now);
    UPDATE public.clearances c SET status = 'ended'
    WHERE c.hospital_id = v_h AND c.status = 'active' AND c.end_date < v_today;
    GET DIAGNOSTICS v_ended = ROW_COUNT;
    INSERT INTO public.clearance_events (hospital_id, clearance_id, kind, days)
    SELECT c.hospital_id, c.id, 'reminded',
           CASE WHEN c.end_date - v_today <= 1 THEN 1 WHEN c.end_date - v_today <= 7 THEN 7 ELSE 30 END
    FROM public.clearances c
    WHERE c.hospital_id = v_h AND c.status = 'active' AND c.end_date >= v_today AND c.end_date - v_today <= 30
    ON CONFLICT (clearance_id, days) WHERE kind = 'reminded' DO NOTHING;
    GET DIAGNOSTICS v_rem = ROW_COUNT;
    RETURN v_ended + v_rem;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The CLI's demo raise (plan departure 39).
------------------------------------------------------------------------

-- mhai_named_access_demo_raise(hospital or NULL): letters_only with demo
-- references, for a scope whose analysis switch carries demo references
-- (22023 otherwise) and whose row was never signed off (23514). False
-- when it already is.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_demo_raise(p_hospital uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.analysis_switch s WHERE s.hospital_id IS NOT DISTINCT FROM p_hospital
                     AND s.signoff_clinical = 'DEMO-SYNTHETIC')
       OR (p_hospital IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.hospitals h WHERE h.id = p_hospital AND h.is_demo)) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'named_access: only a scope under a demo analysis switch takes demo references';
    END IF;
    IF EXISTS (SELECT 1 FROM public.named_access_switch n WHERE n.hospital_id IS NOT DISTINCT FROM p_hospital
                 AND n.position = 'letters_only' AND n.signoff_clinical = 'DEMO-SYNTHETIC') THEN
        RETURN false;
    END IF;
    INSERT INTO public.named_access_switch AS n (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
    VALUES (p_hospital, 'letters_only', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')
    ON CONFLICT ON CONSTRAINT named_access_switch_one DO UPDATE
        SET position = 'letters_only', signoff_clinical = 'DEMO-SYNTHETIC', signoff_privacy = 'DEMO-SYNTHETIC',
            signoff_legal = 'DEMO-SYNTHETIC', changed_by = NULL;
    RETURN true;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_named_access_rank(text) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_within_global(uuid, text) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_switch_guard() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_position(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_demo_raise(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_exempt() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_actor() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_day(uuid, timestamptz) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_is_admin(uuid, uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_staff_ok(uuid, uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_platform_ok(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_same_side(uuid, uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_check_platform(uuid, uuid, uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_demo_hospital(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_gate_for(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_action(text) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_rules() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_after() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_covered_rules() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_covered_after() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_event_audited() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_event_rules() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_gate() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_today() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_check(date, date, uuid, uuid[], citext[]) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_create(text, text, date, date, uuid, uuid[], uuid[]) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_demo_ready(uuid, integer, bytea) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_lock(uuid, integer) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_revoke(uuid, integer, text, text) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_narrow(uuid, integer, uuid[], text, text) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_names(uuid[]) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_reserve_check() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_release_check() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_pending() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_due(timestamptz) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_nightly(timestamptz) OWNER TO mhai_clearance_rules;
REVOKE ALL ON FUNCTION mhai_named_access_rank(text), mhai_named_access_within_global(uuid, text),
    mhai_named_access_switch_guard(), mhai_named_access_position(uuid), mhai_named_access_demo_raise(uuid),
    mhai_clearance_exempt(), mhai_clearance_actor(), mhai_clearance_day(uuid, timestamptz),
    mhai_clearance_is_admin(uuid, uuid), mhai_clearance_staff_ok(uuid, uuid), mhai_clearance_platform_ok(uuid),
    mhai_clearance_same_side(uuid, uuid), mhai_clearance_check_platform(uuid, uuid, uuid),
    mhai_clearance_demo_hospital(uuid), mhai_clearance_gate_for(uuid), mhai_clearance_action(text),
    mhai_clearance_rules(), mhai_clearance_after(), mhai_clearance_covered_rules(), mhai_clearance_covered_after(),
    mhai_clearance_event_audited(), mhai_clearance_event_rules(), mhai_clearance_gate(), mhai_clearance_today(),
    mhai_clearance_check(date, date, uuid, uuid[], citext[]), mhai_clearance_create(text, text, date, date, uuid, uuid[], uuid[]),
    mhai_clearance_demo_ready(uuid, integer, bytea), mhai_clearance_lock(uuid, integer),
    mhai_clearance_revoke(uuid, integer, text, text), mhai_clearance_narrow(uuid, integer, uuid[], text, text),
    mhai_clearance_names(uuid[]), mhai_clearance_reserve_check(), mhai_clearance_release_check(), mhai_clearance_pending(), mhai_clearance_due(timestamptz),
    mhai_clearance_nightly(timestamptz) FROM PUBLIC;
-- The CHECK constraint calls the rank and the bound as whoever writes the
-- switch (here the provisioning CLI's lowering); the bound is a definer
-- function, the rank immutable.
GRANT EXECUTE ON FUNCTION mhai_named_access_rank(text), mhai_named_access_within_global(uuid, text)
    TO mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_named_access_position(uuid) TO mhai_gateway, mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_named_access_demo_raise(uuid) TO mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_clearance_gate(), mhai_clearance_today(),
    mhai_clearance_check(date, date, uuid, uuid[], citext[]), mhai_clearance_create(text, text, date, date, uuid, uuid[], uuid[]),
    mhai_clearance_demo_ready(uuid, integer, bytea), mhai_clearance_revoke(uuid, integer, text, text),
    mhai_clearance_narrow(uuid, integer, uuid[], text, text), mhai_clearance_names(uuid[]), mhai_clearance_pending(),
    mhai_clearance_reserve_check(), mhai_clearance_release_check(), mhai_clearance_nightly(timestamptz) TO mhai_gateway;
GRANT EXECUTE ON FUNCTION mhai_clearance_due(timestamptz) TO mhai_analysis_job;

-- The CLI's named_access.changed entries (no actor, as its other entries).
CREATE POLICY provision_named_access_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action = 'named_access.changed' AND actor_id IS NULL);

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- The letters, their people, events and the switch go; the audit chain
-- keeps every entry.

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

DROP POLICY provision_named_access_append ON audit_log;

DROP TABLE clearance_check_refusals;
DROP TABLE clearance_events;
DROP TABLE clearance_covered;
DROP TABLE clearances;
DROP TABLE named_access_switch;
DROP INDEX audit_log_clearance_event_idx;

DROP FUNCTION mhai_named_access_demo_raise(uuid);
DROP FUNCTION mhai_clearance_nightly(timestamptz);
DROP FUNCTION mhai_clearance_due(timestamptz);
DROP FUNCTION mhai_clearance_pending();
DROP FUNCTION mhai_clearance_release_check();
DROP FUNCTION mhai_clearance_reserve_check();
DROP FUNCTION mhai_clearance_names(uuid[]);
DROP FUNCTION mhai_clearance_narrow(uuid, integer, uuid[], text, text);
DROP FUNCTION mhai_clearance_revoke(uuid, integer, text, text);
DROP FUNCTION mhai_clearance_lock(uuid, integer);
DROP FUNCTION mhai_clearance_demo_ready(uuid, integer, bytea);
DROP FUNCTION mhai_clearance_create(text, text, date, date, uuid, uuid[], uuid[]);
DROP FUNCTION mhai_clearance_check(date, date, uuid, uuid[], citext[]);
DROP FUNCTION mhai_clearance_today();
DROP FUNCTION mhai_clearance_gate();
DROP FUNCTION mhai_clearance_event_rules();
DROP FUNCTION mhai_clearance_event_audited();
DROP FUNCTION mhai_clearance_covered_after();
DROP FUNCTION mhai_clearance_covered_rules();
DROP FUNCTION mhai_clearance_after();
DROP FUNCTION mhai_clearance_rules();
DROP FUNCTION mhai_clearance_action(text);
DROP FUNCTION mhai_clearance_gate_for(uuid);
DROP FUNCTION mhai_clearance_demo_hospital(uuid);
DROP FUNCTION mhai_clearance_check_platform(uuid, uuid, uuid);
DROP FUNCTION mhai_clearance_same_side(uuid, uuid);
DROP FUNCTION mhai_clearance_platform_ok(uuid);
DROP FUNCTION mhai_clearance_staff_ok(uuid, uuid);
DROP FUNCTION mhai_clearance_is_admin(uuid, uuid);
DROP FUNCTION mhai_clearance_day(uuid, timestamptz);
DROP FUNCTION mhai_clearance_actor();
DROP FUNCTION mhai_clearance_exempt();
DROP FUNCTION mhai_named_access_position(uuid);
DROP FUNCTION mhai_named_access_switch_guard();
DROP FUNCTION mhai_named_access_within_global(uuid, text);
DROP FUNCTION mhai_named_access_rank(text);

DROP POLICY clearance_rules ON audit_log;
DROP POLICY clearance_rules ON analysis_switch;
DROP POLICY clearance_rules ON hospitals;
DROP POLICY clearance_rules ON user_mfa;
DROP POLICY clearance_rules ON user_roles;
DROP POLICY clearance_rules ON users;
REVOKE ALL ON audit_log, analysis_switch, hospitals, user_mfa, user_roles, users FROM mhai_clearance_rules;
REVOKE ALL ON FUNCTION mhai_analysis_position(uuid) FROM mhai_clearance_rules;
REVOKE USAGE ON SCHEMA public FROM mhai_clearance_rules;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
