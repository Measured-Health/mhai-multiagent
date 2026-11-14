-- Named access, hospital side (analysis backend design §8.2-§8.7, §10;
-- plan Task 21, departures 45-59; ADR 0015). NUMBERED 00042 (coordinator):
-- the clinician patient lookup (#172) took 00039, and the reference
-- library PRs take 00040 and 00041.
--
-- Nothing here turns anything on. A named list needs the hospital's
-- analysis switch on, its effective named-access position 'on' (no
-- hospital's is: only the CLI's demo raise writes 'on', for the demo
-- scope), and an active, verified letter of the hospital that covers the
-- person today (only the platform's verify, Plan 11b, makes a letter
-- active).
--
-- What it adds:
--   - mhai_named_cover(user): who is covered (design §8.2), owned by
--     mhai_clearance_rules, for the session's own person only, nothing
--     cached. It takes FOR SHARE on the covering letter and the person's
--     row in it: a revocation or narrowing that commits first is seen, and
--     one that comes later waits for the list's commit.
--   - mhai_named_access_state(): the effective position and since, for the
--     caller's hospital. mhai_named_access_position gains mhai_auth
--     (features.named_access at sign-in).
--   - analysis_views takes named lists (purpose, the letter, named_until),
--     and its commit check asks for analysis.named_viewed on one.
--   - named_view_keys (replays, design §8.5), named_view_notes (the purpose
--     note, sealed in Go with the note key) and named_view_budget (5 a
--     hospital day, reserved by one upsert in the request's transaction).
--     No app role holds a grant on them: the reader's functions write
--     them. A deferred constraint trigger refuses, at commit, a replay
--     without its analysis.named_viewed entry (departure 49).
--   - mhai_analysis_named_admit, _named_key, _open_named and
--     _named_replay (EXECUTE to mhai_gateway); _named_rows (internal).
--   - mhai_analysis_drill_flag takes 'named_day'; the sweep drops keys a
--     day after named_until and budget rows after two hospital days.
--   - mhai_named_access_demo_on(hospital or NULL): the CLI's demo raise to
--     'on' (departure 56).
--   - platform_append is not touched.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader', 'mhai_analysis_writer'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Named lists (design §4.3, §8.3).
------------------------------------------------------------------------
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_level_check;
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_purpose_check;
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_clearance_id_check;
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_named_until_check;
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_level_check CHECK (level IN ('deidentified', 'named'));
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_named CHECK (
    (level = 'deidentified' AND purpose IS NULL AND clearance_id IS NULL AND named_until IS NULL)
    OR (level = 'named' AND purpose IN ('clinical_follow_up', 'quality_improvement', 'audit', 'other')
        AND clearance_id IS NOT NULL AND named_until IS NOT NULL));
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_clearance_fk
    FOREIGN KEY (hospital_id, clearance_id) REFERENCES clearances (hospital_id, id);

CREATE TABLE named_view_keys (
    hospital_id uuid NOT NULL,
    key_hash    bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    user_id     uuid NOT NULL,
    body_mac    bytea NOT NULL CHECK (octet_length(body_mac) = 32),
    view_id     uuid NOT NULL,
    named_until timestamptz NOT NULL,
    replays     integer NOT NULL DEFAULT 0 CHECK (replays BETWEEN 0 AND 1000),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hospital_id, key_hash),
    UNIQUE (hospital_id, view_id),
    FOREIGN KEY (hospital_id, view_id) REFERENCES analysis_views (hospital_id, id)
);
CREATE INDEX named_view_keys_until_idx ON named_view_keys (hospital_id, named_until);

CREATE TABLE named_view_notes (
    hospital_id uuid NOT NULL,
    view_id     uuid NOT NULL,
    note_enc    bytea NOT NULL CHECK (octet_length(note_enc) BETWEEN 29 AND 2048),
    PRIMARY KEY (hospital_id, view_id),
    FOREIGN KEY (hospital_id, view_id) REFERENCES analysis_views (hospital_id, id)
);

CREATE TABLE named_view_budget (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    day         date NOT NULL,
    used        integer NOT NULL CHECK (used >= 0),
    -- The transaction that last reserved a slot: a named list or replay
    -- opens only in the transaction that reserved it.
    xact        xid8 NOT NULL,
    PRIMARY KEY (hospital_id, user_id, day)
);

ALTER TABLE named_view_keys OWNER TO mhai_migrate;
ALTER TABLE named_view_notes OWNER TO mhai_migrate;
ALTER TABLE named_view_budget OWNER TO mhai_migrate;
ALTER TABLE named_view_keys ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE named_view_notes ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE named_view_budget ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON named_view_keys, named_view_notes, named_view_budget FROM PUBLIC;

-- The reader, inside its functions only, for the request's hospital. A
-- key is read by anyone of the hospital (another person's key is a
-- mismatch) and written only as its own person; a note is written, never
-- read here (PR 9's export and legal functions will).
GRANT SELECT, INSERT ON named_view_keys TO mhai_analysis_reader;
GRANT UPDATE (replays) ON named_view_keys TO mhai_analysis_reader;
GRANT INSERT ON named_view_notes TO mhai_analysis_reader;
GRANT SELECT, INSERT ON named_view_budget TO mhai_analysis_reader;
GRANT UPDATE (used, xact) ON named_view_budget TO mhai_analysis_reader;
CREATE POLICY analysis_reader ON named_view_keys TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user());
CREATE POLICY analysis_reader ON named_view_notes FOR INSERT TO mhai_analysis_reader
    WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY analysis_reader ON named_view_budget TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user())
    WITH CHECK (hospital_id = public.mhai_current_hospital() AND user_id = public.mhai_current_user());
-- A named list's rows carry each subject's encrypted reference (core's key
-- opens it, for the identity call only).
GRANT SELECT (hospital_id, subject_id, ref_enc) ON analysis_subjects TO mhai_analysis_reader;
CREATE POLICY analysis_reader ON analysis_subjects FOR SELECT TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital());
-- The commit checks read the request's hospital's list entries, and a
-- replay's number.
DROP POLICY analysis_reader ON audit_log;
CREATE POLICY analysis_reader ON audit_log FOR SELECT TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND action IN ('analysis.patients_viewed', 'analysis.named_viewed'));
GRANT SELECT (payload) ON audit_log TO mhai_analysis_reader;

-- The writer: the sweep's reads and deletes.
GRANT SELECT, DELETE ON named_view_keys, named_view_budget TO mhai_analysis_writer;
CREATE POLICY analysis_writer ON named_view_keys TO mhai_analysis_writer USING (true);
CREATE POLICY analysis_writer ON named_view_budget TO mhai_analysis_writer USING (true);

-- The named-view refusal's flag, once a person and hospital day.
ALTER TABLE analysis_drill_flags DROP CONSTRAINT analysis_drill_flags_kind_check;
ALTER TABLE analysis_drill_flags ADD CONSTRAINT analysis_drill_flags_kind_check
    CHECK (kind IN ('list_minute', 'list_day', 'patient_cap', 'named_day'));

------------------------------------------------------------------------
-- Who is covered (design §8.2).
------------------------------------------------------------------------

-- mhai_named_cover(user): the session's person, at the session's
-- hospital (forbidden for anyone else; not_found while analysis is off).
-- named with the covering letter (the latest end date first) and until
-- (the earlier of the end of its last day in the hospital's zone and now
-- plus 30 minutes); otherwise reason: switched_off (the position below
-- on), role_or_account (the hospital not active, or the person not active
-- there as a hospital admin or analyst with two-step), or, from the
-- verified letter whose cover of the person stopped last,
-- removed_from_letter, revoked or ended; none when there is none.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_cover(p_user uuid)
    RETURNS TABLE (named boolean, until timestamptz, clearance_id uuid, version integer, end_date date, demo boolean,
                   reason text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h      uuid := public.mhai_current_hospital();
    v_tz     text;
    v_status text;
    v_today  date;
    v_c      record;
BEGIN
    IF v_h IS NULL OR p_user IS NULL OR p_user IS DISTINCT FROM public.mhai_current_user() THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    IF public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    SELECT h.time_zone, h.status INTO v_tz, v_status FROM public.hospitals h WHERE h.id = v_h;
    v_today := (now() AT TIME ZONE v_tz)::date;
    named := false;
    IF public.mhai_named_access_position(v_h) <> 'on' THEN
        reason := 'switched_off';
        RETURN NEXT;
        RETURN;
    END IF;
    IF v_status IS DISTINCT FROM 'active' OR NOT public.mhai_clearance_staff_ok(p_user, v_h) THEN
        reason := 'role_or_account';
        RETURN NEXT;
        RETURN;
    END IF;
    SELECT c.id, c.version, c.end_date, c.demo INTO v_c
    FROM public.clearances c
    JOIN public.clearance_covered k ON k.hospital_id = c.hospital_id AND k.clearance_id = c.id
    WHERE c.hospital_id = v_h AND c.status = 'active' AND c.verified_at IS NOT NULL AND c.end_date >= v_today
      AND k.user_id = p_user AND k.side = 'hospital' AND k.removed_at IS NULL
    ORDER BY c.end_date DESC, c.id
    LIMIT 1
    FOR SHARE OF c, k;
    IF FOUND THEN
        named := true;
        clearance_id := v_c.id;
        version := v_c.version;
        end_date := v_c.end_date;
        demo := v_c.demo;
        until := LEAST(((v_c.end_date + 1)::timestamp AT TIME ZONE v_tz), now() + interval '30 minutes');
        RETURN NEXT;
        RETURN;
    END IF;
    SELECT CASE
             WHEN k.removed_at IS NOT NULL AND k.removed_at <= LEAST(COALESCE(c.revoked_at, 'infinity'), e.stop) THEN 'removed_from_letter'
             WHEN c.revoked_at IS NOT NULL AND c.revoked_at <= e.stop THEN 'revoked'
             ELSE 'ended' END
    INTO reason
    FROM public.clearances c
    JOIN public.clearance_covered k ON k.hospital_id = c.hospital_id AND k.clearance_id = c.id
    CROSS JOIN LATERAL (SELECT ((c.end_date + 1)::timestamp AT TIME ZONE v_tz) AS stop) e
    WHERE c.hospital_id = v_h AND c.verified_at IS NOT NULL AND c.status IN ('active', 'ended', 'revoked')
      AND k.user_id = p_user AND k.side = 'hospital'
    ORDER BY LEAST(COALESCE(k.removed_at, 'infinity'), COALESCE(c.revoked_at, 'infinity'), e.stop) DESC, c.id
    LIMIT 1;
    reason := COALESCE(reason, 'none');
    RETURN NEXT;
END
$$;
-- +goose StatementEnd

-- mhai_named_access_state(): the effective named-access position for the
-- caller's hospital and the last time either of its two rows changed
-- (null with no row). not_found while analysis is off.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_state(OUT "position" text, OUT since timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_current_hospital();
BEGIN
    IF v_h IS NULL OR public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    "position" := public.mhai_named_access_position(v_h);
    SELECT max(s.since) INTO since FROM public.named_access_switch s WHERE s.hospital_id IS NULL OR s.hospital_id = v_h;
END
$$;
-- +goose StatementEnd

-- mhai_named_access_demo_on(hospital or NULL): on with demo references,
-- for a scope whose analysis switch carries demo references (22023
-- otherwise) and whose row was never signed off (23514, the switch's
-- guard). False when it already is. The CLI raises the global row first.
-- +goose StatementBegin
CREATE FUNCTION mhai_named_access_demo_on(p_hospital uuid) RETURNS boolean
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
                 AND n.position = 'on' AND n.signoff_clinical = 'DEMO-SYNTHETIC') THEN
        RETURN false;
    END IF;
    INSERT INTO public.named_access_switch AS n (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
    VALUES (p_hospital, 'on', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC', 'DEMO-SYNTHETIC')
    ON CONFLICT ON CONSTRAINT named_access_switch_one DO UPDATE
        SET position = 'on', signoff_clinical = 'DEMO-SYNTHETIC', signoff_privacy = 'DEMO-SYNTHETIC',
            signoff_legal = 'DEMO-SYNTHETIC', changed_by = NULL;
    RETURN true;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_named_cover(uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_state() OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_named_access_demo_on(uuid) OWNER TO mhai_clearance_rules;
REVOKE ALL ON FUNCTION mhai_named_cover(uuid), mhai_named_access_state(), mhai_named_access_demo_on(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_named_cover(uuid) TO mhai_gateway, mhai_analysis_reader;
GRANT EXECUTE ON FUNCTION mhai_named_access_state() TO mhai_gateway;
GRANT EXECUTE ON FUNCTION mhai_named_access_demo_on(uuid) TO mhai_provision;
GRANT EXECUTE ON FUNCTION mhai_named_access_position(uuid) TO mhai_auth;

------------------------------------------------------------------------
-- The named list (design §8.3, §8.5).
------------------------------------------------------------------------

-- The commit check, replaced: a de-identified list needs its
-- analysis.patients_viewed entry, a named one its analysis.named_viewed
-- entry, by its viewer, in its hospital.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_view_audited() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'analysis_view' AND a.entity_id = NEW.id::text
                     AND a.action = CASE NEW.level WHEN 'named' THEN 'analysis.named_viewed' ELSE 'analysis.patients_viewed' END
                     AND a.actor_id = NEW.viewer AND a.hospital_id = NEW.hospital_id) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'analysis_views: a list without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- A replay is kept only with its own analysis.named_viewed entry, naming
-- the list and the replay's number, by the key's person (departure 49).
-- +goose StatementBegin
CREATE FUNCTION mhai_named_replay_audited() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.replays IS DISTINCT FROM OLD.replays AND NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'analysis_view' AND a.entity_id = NEW.view_id::text
                     AND a.action = 'analysis.named_viewed' AND a.actor_id = NEW.user_id
                     AND a.hospital_id = NEW.hospital_id AND a.payload ->> 'replays' = NEW.replays::text) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'named_view_keys: a replay without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER named_view_keys_replay_audited AFTER UPDATE ON named_view_keys
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_named_replay_audited();

-- The cover of the session's person, or its refusal as the route's code
-- (P0001). Internal.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_covered()
    RETURNS TABLE (named boolean, until timestamptz, clearance_id uuid, version integer, end_date date, demo boolean,
                   reason text)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_c record;
BEGIN
    SELECT * INTO v_c FROM public.mhai_named_cover(public.mhai_current_user());
    IF NOT COALESCE(v_c.named, false) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = CASE v_c.reason
            WHEN 'switched_off' THEN 'named_access_off'
            WHEN 'ended' THEN 'clearance_ended'
            WHEN 'role_or_account' THEN 'forbidden'
            ELSE 'not_cleared' END;
    END IF;
    named := v_c.named;
    until := v_c.until;
    clearance_id := v_c.clearance_id;
    version := v_c.version;
    end_date := v_c.end_date;
    demo := v_c.demo;
    reason := v_c.reason;
    RETURN NEXT;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_named_rows(view, period, filters, group): the rows of the
-- session person's named list, in order, as mhai_analysis_open sends them
-- (what the list was opened on first), with each subject's encrypted
-- reference. Internal.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_rows(p_view uuid, p_period uuid, p_filters jsonb, p_group jsonb)
    RETURNS TABLE (ord integer, pid text, age_band text, sex text, setting text, conditions jsonb, findings jsonb, ref_enc bytea)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h    uuid := public.mhai_analysis_hospital();
    v_per  record;
    v_prio text[] := ARRAY[p_filters ->> 'condition', p_group ->> 'a', p_group ->> 'b'];
    v_fc   text := CASE WHEN p_group ->> 'kind' = 'finding' THEN p_group ->> 'condition' END;
    v_ff   text := CASE WHEN p_group ->> 'kind' = 'finding' THEN p_group ->> 'flag' END;
    v_rows uuid[];
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    SELECT array_agg(r.subject_id ORDER BY r.ord) INTO v_rows
    FROM public.analysis_views v JOIN public.analysis_view_rows r ON r.hospital_id = v.hospital_id AND r.view_id = v.id
    WHERE v.hospital_id = v_h AND v.id = p_view AND v.viewer = public.mhai_current_user() AND v.level = 'named'
      AND v.period_id = p_period;
    IF v_rows IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a named list of this person';
    END IF;
    RETURN QUERY
    SELECT r.n::integer, p.pid, a.age_band, a.sex, a.setting,
           COALESCE((SELECT jsonb_agg(x.c ORDER BY x.c) FROM (
                SELECT y.c FROM (
                    SELECT DISTINCT fc.condition_id::text AS c FROM public.analysis_facts fc
                    WHERE fc.hospital_id = v_h AND fc.subject_id = r.s AND fc.day BETWEEN v_per.start AND v_per."end"
                      AND NOT fc.period_late AND fc.condition_id IS NOT NULL) y
                ORDER BY COALESCE(y.c = ANY (v_prio), false) DESC, y.c LIMIT 10) x), '[]'::jsonb),
           COALESCE((SELECT jsonb_agg(jsonb_build_object('condition', x.c, 'flag', x.fl) ORDER BY x.c, x.fl) FROM (
                SELECT y.c, y.fl FROM (
                    SELECT DISTINCT COALESCE(ff.condition_id::text, '') AS c, COALESCE(ff.flag, '') AS fl FROM public.analysis_facts ff
                    WHERE ff.hospital_id = v_h AND ff.subject_id = r.s AND ff.day BETWEEN v_per.start AND v_per."end"
                      AND NOT ff.period_late AND ff.kind = 'finding') y
                ORDER BY (y.c IS NOT DISTINCT FROM v_fc AND y.fl IS NOT DISTINCT FROM v_ff) DESC, y.c, y.fl LIMIT 10) x), '[]'::jsonb),
           s.ref_enc
    FROM unnest(v_rows) WITH ORDINALITY r(s, n)
    JOIN public.analysis_subject_attrs a ON a.hospital_id = v_h AND a.subject_id = r.s
    JOIN public.analysis_subjects s ON s.hospital_id = v_h AND s.subject_id = r.s
    JOIN public.mhai_analysis_pseudonyms(p_period, v_rows) p ON p.subject_id = r.s
    ORDER BY r.n;
END
$$;
-- +goose StatementEnd

-- The hospital's day now (the database's clock). Internal.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_day(p_hospital uuid) RETURNS date
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT (now() AT TIME ZONE h.time_zone)::date FROM public.hospitals h WHERE h.id = p_hospital
$$;
-- +goose StatementEnd

-- A named list or replay opens only in the transaction that reserved its
-- slot within the day's limit (mhai_analysis_named_admit): the budget is
-- the database's, not the caller's order of calls. Internal.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_reserved() RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_analysis_hospital();
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.named_view_budget b
                   WHERE b.hospital_id = v_h AND b.user_id = public.mhai_current_user()
                     AND b.day = public.mhai_analysis_named_day(v_h)
                     AND b.xact = pg_catalog.pg_current_xact_id() AND b.used <= 5) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_reserved';
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_named_admit(limit): reserves one of the person's named
-- views for the hospital's day (the database's clock; replays count), by
-- one upsert whose row lock queues the person's parallel requests to their
-- commit, and marks the reservation with this transaction; false past
-- limit (at most 5). A refusal, or any later failure of the request, rolls
-- the slot back with the transaction.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_admit(p_limit integer) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_day date;
    v_n   integer;
BEGIN
    IF v_u IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 5 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a named admission';
    END IF;
    v_day := public.mhai_analysis_named_day(v_h);
    INSERT INTO public.named_view_budget AS b (hospital_id, user_id, day, used, xact)
    VALUES (v_h, v_u, v_day, 1, pg_catalog.pg_current_xact_id())
    ON CONFLICT (hospital_id, user_id, day) DO UPDATE SET used = b.used + 1, xact = pg_catalog.pg_current_xact_id()
    RETURNING b.used INTO v_n;
    RETURN v_n <= p_limit;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_named_key(key hash, body mac): what this Idempotency-Key
-- is at the request's hospital, under a transaction lock on the key (a
-- parallel request with it waits for this one's commit): 'new'; 'replay'
-- (the session's person, the same body, before named_until) with its
-- list; 'mismatch' (another person's, or another body); 'expired'.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_key(p_key bytea, p_body bytea, OUT state text, OUT view_id uuid)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h uuid := public.mhai_analysis_hospital();
    v_k record;
BEGIN
    IF public.mhai_current_user() IS NULL OR octet_length(p_key) IS DISTINCT FROM 32 OR octet_length(p_body) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a named key';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('mhai.analysis.named_key'), hashtext(v_h::text || '/' || encode(p_key, 'hex')));
    SELECT k.user_id, k.body_mac, k.view_id, k.named_until INTO v_k FROM public.named_view_keys k
    WHERE k.hospital_id = v_h AND k.key_hash = p_key;
    IF NOT FOUND THEN
        state := 'new';
    ELSIF v_k.user_id IS DISTINCT FROM public.mhai_current_user() OR v_k.body_mac IS DISTINCT FROM p_body THEN
        state := 'mismatch';
    ELSIF v_k.named_until <= now() THEN
        state := 'expired';
    ELSE
        state := 'replay';
        view_id := v_k.view_id;
    END IF;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_open_named(view, period, filters, group, source view,
-- drill hash, expected, cap, purpose, note ciphertext, key hash, body
-- mac): one named list, in the transaction that reserved its slot, for the session's person, under the letter that
-- covers them now (P0001 named_access_off, not_cleared, clearance_ended,
-- forbidden), as mhai_analysis_open: the group recounted and checked
-- against the shown cell (too_small), the first 200 subjects reserved
-- against the quarter (patient_cap), the list and its rows recorded with
-- the purpose, the letter and named_until, the note and the key stored.
-- Returns the rows with each subject's encrypted reference, the letter's
-- id, end date and demo flag, and named_until. The list is kept only if
-- its analysis.named_viewed entry is appended before commit.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_open_named(p_view uuid, p_period uuid, p_filters jsonb, p_group jsonb, p_source text,
                                         p_drill bytea, p_expected integer, p_cap integer, p_purpose text, p_note bytea,
                                         p_key bytea, p_body bytea)
    RETURNS TABLE (ord integer, pid text, age_band text, sex text, setting text, conditions jsonb, findings jsonb,
                   ref_enc bytea, clearance_id uuid, clearance_end date, demo boolean, named_until timestamptz,
                   replays integer)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_now timestamptz := clock_timestamp();
    v_per record;
    v_cov record;
    v_all uuid[];
    v_sub uuid[];
    v_day date;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    IF v_u IS NULL OR p_view IS NULL OR p_source NOT IN ('affected', 'co-occurring', 'findings')
       OR p_drill IS NULL OR octet_length(p_drill) <> 32 OR p_expected IS NULL OR p_cap IS NULL
       OR p_purpose IS NULL OR p_purpose NOT IN ('clinical_follow_up', 'quality_improvement', 'audit', 'other')
       OR (p_purpose = 'other' AND p_note IS NULL)
       OR octet_length(p_key) IS DISTINCT FROM 32 OR octet_length(p_body) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a named list';
    END IF;
    PERFORM public.mhai_analysis_named_reserved();
    SELECT * INTO v_cov FROM public.mhai_analysis_named_covered();
    SELECT array_agg(g ORDER BY g) INTO v_all FROM public.mhai_analysis_group(p_period, p_filters, p_group) g;
    IF COALESCE(cardinality(v_all), 0) <> p_expected OR p_expected < 5 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'too_small';
    END IF;
    v_sub := v_all[1:200];
    PERFORM public.mhai_analysis_reserve_patients(date_trunc('quarter', v_per.start)::date, v_sub, p_cap);
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_views (id, hospital_id, viewer, level, period_id, source_view, filters, drill_hash,
                                       matched, sent, purpose, clearance_id, named_until, day, at)
    VALUES (p_view, v_h, v_u, 'named', p_period, p_source, p_filters, p_drill,
            cardinality(v_all), cardinality(v_sub), p_purpose, v_cov.clearance_id, v_cov.until, v_day, v_now);
    INSERT INTO public.analysis_view_rows (hospital_id, view_id, ord, subject_id)
    SELECT v_h, p_view, r.n::integer, r.s FROM unnest(v_sub) WITH ORDINALITY r(s, n);
    IF p_note IS NOT NULL THEN
        INSERT INTO public.named_view_notes (hospital_id, view_id, note_enc) VALUES (v_h, p_view, p_note);
    END IF;
    INSERT INTO public.named_view_keys (hospital_id, key_hash, user_id, body_mac, view_id, named_until)
    VALUES (v_h, p_key, v_u, p_body, p_view, v_cov.until);
    RETURN QUERY
    SELECT x.ord, x.pid, x.age_band, x.sex, x.setting, x.conditions, x.findings, x.ref_enc,
           v_cov.clearance_id, v_cov.end_date, v_cov.demo, v_cov.until, 0
    FROM public.mhai_analysis_named_rows(p_view, p_period, p_filters, p_group) x;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_named_replay(key hash, body mac, period, filters, group,
-- drill hash, expected, cap): a replay, in the transaction that reserved
-- its slot, of the session person's named
-- list (design §8.5): the key (P0001 idempotency_mismatch, expired), the
-- cover now, the group recounted against the stored list (too_small), the
-- stored subjects reserved again (nothing new), the replay counted. The
-- stored rows, with named_until the earlier of the original and today's
-- cover, and the replay's number: its analysis.named_viewed entry must be
-- appended before commit.
-- +goose StatementBegin
CREATE FUNCTION mhai_analysis_named_replay(p_key bytea, p_body bytea, p_period uuid, p_filters jsonb, p_group jsonb,
                                           p_drill bytea, p_expected integer, p_cap integer)
    RETURNS TABLE (ord integer, pid text, age_band text, sex text, setting text, conditions jsonb, findings jsonb,
                   ref_enc bytea, clearance_id uuid, clearance_end date, demo boolean, named_until timestamptz,
                   replays integer)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_per record;
    v_k   record;
    v_v   record;
    v_cov record;
    v_all uuid[];
    v_sub uuid[];
    v_n   integer;
BEGIN
    SELECT * INTO v_per FROM public.mhai_analysis_frozen(p_period);
    IF v_u IS NULL OR p_drill IS NULL OR octet_length(p_drill) <> 32 OR p_expected IS NULL OR p_cap IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a named replay';
    END IF;
    PERFORM public.mhai_analysis_named_reserved();
    SELECT k.user_id, k.body_mac, k.view_id, k.named_until INTO v_k FROM public.named_view_keys k
    WHERE k.hospital_id = v_h AND k.key_hash = p_key FOR UPDATE;
    IF NOT FOUND OR v_k.user_id IS DISTINCT FROM v_u OR v_k.body_mac IS DISTINCT FROM p_body THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'idempotency_mismatch';
    END IF;
    IF v_k.named_until <= now() THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'expired';
    END IF;
    SELECT * INTO v_cov FROM public.mhai_analysis_named_covered();
    SELECT v.matched INTO v_v FROM public.analysis_views v
    WHERE v.hospital_id = v_h AND v.id = v_k.view_id AND v.viewer = v_u AND v.level = 'named'
      AND v.period_id = p_period AND v.drill_hash = p_drill AND v.filters = p_filters;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'idempotency_mismatch';
    END IF;
    SELECT array_agg(g ORDER BY g) INTO v_all FROM public.mhai_analysis_group(p_period, p_filters, p_group) g;
    IF COALESCE(cardinality(v_all), 0) <> p_expected OR p_expected <> v_v.matched OR p_expected < 5 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'too_small';
    END IF;
    SELECT array_agg(r.subject_id ORDER BY r.ord) INTO v_sub FROM public.analysis_view_rows r
    WHERE r.hospital_id = v_h AND r.view_id = v_k.view_id;
    PERFORM public.mhai_analysis_reserve_patients(date_trunc('quarter', v_per.start)::date, v_sub, p_cap);
    UPDATE public.named_view_keys k SET replays = k.replays + 1
    WHERE k.hospital_id = v_h AND k.key_hash = p_key
    RETURNING k.replays INTO v_n;
    RETURN QUERY
    SELECT x.ord, x.pid, x.age_band, x.sex, x.setting, x.conditions, x.findings, x.ref_enc,
           v_cov.clearance_id, v_cov.end_date, v_cov.demo, LEAST(v_k.named_until, v_cov.until), v_n
    FROM public.mhai_analysis_named_rows(v_k.view_id, p_period, p_filters, p_group) x;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_drill_flag, replaced: 00028's, with named_day.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_drill_flag(p_kind text, p_now timestamptz) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_now timestamptz := COALESCE(p_now, now());
    v_day date;
    v_n   integer;
BEGIN
    IF v_u IS NULL OR v_now > now() OR p_kind NOT IN ('list_minute', 'list_day', 'patient_cap', 'named_day') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a flag';
    END IF;
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_drill_flags (hospital_id, user_id, kind, day) VALUES (v_h, v_u, p_kind, v_day)
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n = 1;
END
$$;
-- +goose StatementEnd

-- mhai_analysis_sweep, replaced: 00028's, plus named keys a day after
-- named_until and the named budget after two hospital days. Notes stay
-- with their list.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_cut   date;
    v_today date;
    v_n     integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    v_today := (p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date;
    v_cut := (v_today - interval '25 months')::date;
    DELETE FROM public.analysis_facts f WHERE f.hospital_id = p_hospital AND f.day < v_cut;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    -- 00025: stored bodies live as long as their period's facts (25
    -- months); the read log and the day's flags for two hospital days.
    DELETE FROM public.analysis_cold_contexts c
    WHERE c.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = c.hospital_id AND p.id = c.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_cold_budget b
    WHERE b.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = b.hospital_id AND p.id = b.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_snapshots s
    WHERE s.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = s.hospital_id AND p.id = s.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_view_log l WHERE l.hospital_id = p_hospital AND l.day < v_today - 2;
    DELETE FROM public.analysis_unusual u WHERE u.hospital_id = p_hospital AND u.day < v_today - 2;
    -- 00028: the distinct-patient cap's opens and counters once no period
    -- of their quarter can be listed any more (the quarter ended before the
    -- 25-month cut; N10, departure 22): until then the cap holds for it.
    -- The drill flags after two hospital days.
    DELETE FROM public.analysis_patient_opens o
    WHERE o.hospital_id = p_hospital AND (o.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_patient_budget b
    WHERE b.hospital_id = p_hospital AND (b.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_drill_flags d WHERE d.hospital_id = p_hospital AND d.day < v_today - 2;
    -- 00042: a named list's key 24 hours after named_until (design §8.5),
    -- the named budget after two hospital days.
    DELETE FROM public.named_view_keys k WHERE k.hospital_id = p_hospital AND k.named_until < p_now - interval '24 hours';
    DELETE FROM public.named_view_budget b WHERE b.hospital_id = p_hospital AND b.day < v_today - 2;
    DELETE FROM public.analysis_first_seen fs WHERE fs.hospital_id = p_hospital AND fs.first_day < v_cut;
    DELETE FROM public.analysis_subjects s
    WHERE s.hospital_id = p_hospital AND s.created_day < v_cut
      AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = s.hospital_id AND f.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_first_seen fs WHERE fs.hospital_id = s.hospital_id AND fs.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_pseudonyms p WHERE p.hospital_id = s.hospital_id AND p.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_view_rows r WHERE r.hospital_id = s.hospital_id AND r.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_patient_opens o WHERE o.hospital_id = s.hospital_id AND o.subject_id = s.subject_id);
    RETURN v_n;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_named_replay_audited() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_covered() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_rows(uuid, uuid, jsonb, jsonb) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_day(uuid) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_reserved() OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_admit(integer) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_key(bytea, bytea) OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_open_named(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, text, bytea, bytea, bytea)
    OWNER TO mhai_analysis_reader;
ALTER FUNCTION mhai_analysis_named_replay(bytea, bytea, uuid, jsonb, jsonb, bytea, integer, integer)
    OWNER TO mhai_analysis_reader;
REVOKE ALL ON FUNCTION mhai_named_replay_audited(), mhai_analysis_named_covered(),
    mhai_analysis_named_rows(uuid, uuid, jsonb, jsonb), mhai_analysis_named_admit(integer),
    mhai_analysis_named_day(uuid), mhai_analysis_named_reserved(),
    mhai_analysis_named_key(bytea, bytea),
    mhai_analysis_open_named(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, text, bytea, bytea, bytea),
    mhai_analysis_named_replay(bytea, bytea, uuid, jsonb, jsonb, bytea, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_analysis_named_admit(integer), mhai_analysis_named_key(bytea, bytea),
    mhai_analysis_open_named(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, text, bytea, bytea, bytea),
    mhai_analysis_named_replay(bytea, bytea, uuid, jsonb, jsonb, bytea, integer, integer) TO mhai_gateway;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader', 'mhai_analysis_writer'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Named lists are audit-cited records: the Down refuses while any exists
-- (the restored check would refuse them anyway). Keys, notes and the
-- budget go; the audit chain keeps every entry.

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    IF EXISTS (SELECT 1 FROM analysis_views WHERE level = 'named') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use', MESSAGE = '00042: named lists exist; they are kept with the audit chain';
    END IF;
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader', 'mhai_analysis_writer'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- 00028's sweep, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_sweep(p_hospital uuid, p_now timestamptz) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_cut   date;
    v_today date;
    v_n     integer;
BEGIN
    IF p_hospital IS NULL OR p_now IS NULL OR p_now > now() THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a time that has passed';
    END IF;
    v_today := (p_now AT TIME ZONE public.mhai_analysis_zone(p_hospital))::date;
    v_cut := (v_today - interval '25 months')::date;
    DELETE FROM public.analysis_facts f WHERE f.hospital_id = p_hospital AND f.day < v_cut;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    -- 00025: stored bodies live as long as their period's facts (25
    -- months); the read log and the day's flags for two hospital days.
    DELETE FROM public.analysis_cold_contexts c
    WHERE c.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = c.hospital_id AND p.id = c.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_cold_budget b
    WHERE b.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = b.hospital_id AND p.id = b.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_snapshots s
    WHERE s.hospital_id = p_hospital
      AND EXISTS (SELECT 1 FROM public.analysis_periods p
                  WHERE p.hospital_id = s.hospital_id AND p.id = s.period_id AND p."end" < v_cut);
    DELETE FROM public.analysis_view_log l WHERE l.hospital_id = p_hospital AND l.day < v_today - 2;
    DELETE FROM public.analysis_unusual u WHERE u.hospital_id = p_hospital AND u.day < v_today - 2;
    -- 00028: the distinct-patient cap's opens and counters once no period
    -- of their quarter can be listed any more (the quarter ended before the
    -- 25-month cut; N10, departure 22): until then the cap holds for it.
    -- The drill flags after two hospital days.
    DELETE FROM public.analysis_patient_opens o
    WHERE o.hospital_id = p_hospital AND (o.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_patient_budget b
    WHERE b.hospital_id = p_hospital AND (b.quarter + interval '3 months')::date <= v_cut;
    DELETE FROM public.analysis_drill_flags d WHERE d.hospital_id = p_hospital AND d.day < v_today - 2;
    DELETE FROM public.analysis_first_seen fs WHERE fs.hospital_id = p_hospital AND fs.first_day < v_cut;
    DELETE FROM public.analysis_subjects s
    WHERE s.hospital_id = p_hospital AND s.created_day < v_cut
      AND NOT EXISTS (SELECT 1 FROM public.analysis_facts f WHERE f.hospital_id = s.hospital_id AND f.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_first_seen fs WHERE fs.hospital_id = s.hospital_id AND fs.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_pseudonyms p WHERE p.hospital_id = s.hospital_id AND p.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_view_rows r WHERE r.hospital_id = s.hospital_id AND r.subject_id = s.subject_id)
      AND NOT EXISTS (SELECT 1 FROM public.analysis_patient_opens o WHERE o.hospital_id = s.hospital_id AND o.subject_id = s.subject_id);
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- 00028's flag function, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_drill_flag(p_kind text, p_now timestamptz) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h   uuid := public.mhai_analysis_hospital();
    v_u   uuid := public.mhai_current_user();
    v_now timestamptz := COALESCE(p_now, now());
    v_day date;
    v_n   integer;
BEGIN
    IF v_u IS NULL OR v_now > now() OR p_kind NOT IN ('list_minute', 'list_day', 'patient_cap') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'analysis: not a flag';
    END IF;
    SELECT (v_now AT TIME ZONE h.time_zone)::date INTO v_day FROM public.hospitals h WHERE h.id = v_h;
    INSERT INTO public.analysis_drill_flags (hospital_id, user_id, kind, day) VALUES (v_h, v_u, p_kind, v_day)
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n = 1;
END
$$;
-- +goose StatementEnd

-- 00028's commit check, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_analysis_view_audited() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'analysis_view' AND a.entity_id = NEW.id::text
                     AND a.action = 'analysis.patients_viewed' AND a.actor_id = NEW.viewer
                     AND a.hospital_id = NEW.hospital_id) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'analysis_views: a list without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

DROP FUNCTION mhai_analysis_named_replay(bytea, bytea, uuid, jsonb, jsonb, bytea, integer, integer);
DROP FUNCTION mhai_analysis_open_named(uuid, uuid, jsonb, jsonb, text, bytea, integer, integer, text, bytea, bytea, bytea);
DROP FUNCTION mhai_analysis_named_key(bytea, bytea);
DROP FUNCTION mhai_analysis_named_admit(integer);
DROP FUNCTION mhai_analysis_named_reserved();
DROP FUNCTION mhai_analysis_named_day(uuid);
DROP FUNCTION mhai_analysis_named_rows(uuid, uuid, jsonb, jsonb);
DROP FUNCTION mhai_analysis_named_covered();
DROP TABLE named_view_keys;
DROP FUNCTION mhai_named_replay_audited();
DROP TABLE named_view_notes;
DROP TABLE named_view_budget;

REVOKE EXECUTE ON FUNCTION mhai_named_access_position(uuid) FROM mhai_auth;
DROP FUNCTION mhai_named_access_demo_on(uuid);
DROP FUNCTION mhai_named_access_state();
DROP FUNCTION mhai_named_cover(uuid);

DELETE FROM analysis_drill_flags WHERE kind = 'named_day';
ALTER TABLE analysis_drill_flags DROP CONSTRAINT analysis_drill_flags_kind_check;
ALTER TABLE analysis_drill_flags ADD CONSTRAINT analysis_drill_flags_kind_check
    CHECK (kind IN ('list_minute', 'list_day', 'patient_cap'));

REVOKE SELECT (payload) ON audit_log FROM mhai_analysis_reader;
DROP POLICY analysis_reader ON audit_log;
CREATE POLICY analysis_reader ON audit_log FOR SELECT TO mhai_analysis_reader
    USING (hospital_id = public.mhai_current_hospital() AND action = 'analysis.patients_viewed');
DROP POLICY analysis_reader ON analysis_subjects;
REVOKE ALL ON analysis_subjects FROM mhai_analysis_reader;

ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_clearance_fk;
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_named;
ALTER TABLE analysis_views DROP CONSTRAINT analysis_views_level_check;
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_level_check CHECK (level = 'deidentified');
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_purpose_check CHECK (purpose IS NULL);
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_clearance_id_check CHECK (clearance_id IS NULL);
ALTER TABLE analysis_views ADD CONSTRAINT analysis_views_named_until_check CHECK (named_until IS NULL);

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules', 'mhai_analysis_reader', 'mhai_analysis_writer'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
