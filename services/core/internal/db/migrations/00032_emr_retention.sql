-- EMR integration, retention, the exclusion purge and legal holds (EMR
-- design §10.6, §14.2; plan Tasks 39-40, PR 9). Numbered 00032: 00031 is
-- EMR PR 8b's. Nothing here runs by itself: core's maintenance loop calls
-- the functions below (CORE_EMR_ENABLED, off by default; the retention
-- sweep also needs CORE_EMR_RETENTION_SWEEP), and the operator CLI calls
-- the hold and removal functions.
--
-- legal_holds (plan departure 10): a minimal hold per hospital, placed and
-- lifted only by the provisioning login (core hospitals legal-hold), never
-- deleted. A hospital with an open hold is on hold
-- (mhai_hospital_on_hold, the one reader), and then nothing of its EMR
-- data is deleted or cleared: no purge, no retention, no removal.
--
-- emr.purge_requests (design §10.6): one durable row per exclusion. When
-- the hospital isn't on hold the exclusion purges in its own transaction
-- (product owner, 11 Nov) and the row is done at once. Under a hold the
-- row is held: the patient is excluded (no processing, no alert), its open
-- alerts are withdrawn, and the alerts' patient columns move into
-- emr.purge_held_alerts so no alert names the patient while nothing is
-- deleted. When the hold is lifted the loop moves the row back to pending
-- (due in 24 hours) and purges it on its next run; a held row older than
-- 90 days is listed to the operators.
--
-- The purge (emr_purge_patient_data, internal): the patient's findings
-- (and what core's consumer recorded about them), canonical rows, the raw
-- records behind them and every raw record naming one of its source ids,
-- its possible matches; the alerts keep codes and times for Usage but
-- lose the patient, the finding and the dismissal note; the patient row
-- keeps its id and links only (so later records are recognised and
-- dropped). The patients merged into it are cleared the same way.
--
-- emr_retention_sweep(cap) (design §14.2): per hospital, capped per step:
-- raw payloads after 7 days (once normalised or rejected), canonical
-- observations, medications and encounters 13 months after their time,
-- findings that never became alerts after 90 days, alerts' patient
-- columns after 13 months, alerts after 24 months, what core's consumer
-- recorded after 90 days, and patients nothing references for 13 months.
-- Excluded patients stay (their links drop later records).
--
-- emr_remove_hospital_data(hid) (plan departure 11): the hook the removal
-- flow will call; the provisioning CLI calls it now. Refused unless the
-- hospital is removal_scheduled, not on hold, and holds no credential
-- metadata (the emr service deletes a field's row only after it deleted
-- the secret, so no row means no secret).
--
-- Roles:
--   mhai_emr_maint  NOLOGIN, new: core's maintenance loop (core's login
--                   reaches it by SET ROLE only). EXECUTE on the loop's
--                   functions and its audit append; nothing else.
--   mhai_emr_admin  the operator's exclusion (core emr exclude, through a
--                   login made by core migrate up, EMR_ADMIN_DB_USER): its
--                   hospital's patients' id, MRN and status, the hospitals'
--                   ids and codes, and its audit append.
--   mhai_provision  the legal holds, emr_remove_hospital_data() and their
--                   audit rows.
--
-- Every table: ENABLE and FORCE row-level security, owner mhai_migrate.
-- Every definer: owner mhai_migrate, search_path pinned, every relation
-- schema-qualified, no PUBLIC execute, and bound to the hospital it was
-- called for (the owner's write policies are scoped to it).

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_emr_maint') THEN
        CREATE ROLE mhai_emr_maint NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
GRANT mhai_emr_maint TO mhai_app WITH INHERIT FALSE;

------------------------------------------------------------------------
-- Legal holds.
------------------------------------------------------------------------
CREATE TABLE legal_holds (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    -- A document reference, never a name: AGR-2026-0042, LH-2026-17.
    reference   text NOT NULL CHECK (reference ~ '^[A-Z]{2,8}-[0-9]{4}-[0-9]{2,8}$'),
    placed_at   timestamptz NOT NULL DEFAULT now(),
    lifted_at   timestamptz,
    CONSTRAINT legal_holds_lifted CHECK (lifted_at IS NULL OR lifted_at >= placed_at)
);
ALTER TABLE legal_holds OWNER TO mhai_migrate;
ALTER TABLE legal_holds ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE UNIQUE INDEX legal_holds_open ON legal_holds (hospital_id, reference) WHERE lifted_at IS NULL;

-- The provisioning login places and lifts; a lifted hold never changes
-- again, and nothing deletes one.
GRANT SELECT (id, hospital_id, reference, placed_at, lifted_at), INSERT (hospital_id, reference), UPDATE (lifted_at)
    ON legal_holds TO mhai_provision;
CREATE POLICY provision_read ON legal_holds FOR SELECT TO mhai_provision USING (true);
CREATE POLICY provision_place ON legal_holds FOR INSERT TO mhai_provision WITH CHECK (lifted_at IS NULL);
CREATE POLICY provision_lift ON legal_holds FOR UPDATE TO mhai_provision
    USING (lifted_at IS NULL) WITH CHECK (lifted_at IS NOT NULL);
-- The definers read every hold (mhai_hospital_on_hold).
CREATE POLICY owner_read ON legal_holds FOR SELECT TO mhai_migrate USING (true);

-- Whether hospital hid has an open legal hold. Internal: the definers
-- below call it; no role may.
-- +goose StatementBegin
CREATE FUNCTION mhai_hospital_on_hold(hid uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT hid IS NULL OR EXISTS (SELECT 1 FROM public.legal_holds h WHERE h.hospital_id = hid AND h.lifted_at IS NULL)
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_hospital_on_hold(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_hospital_on_hold(uuid) FROM PUBLIC;

-- A hold and a deletion never interleave: every deleting definer takes the
-- hospital's hold lock shared before it reads the hold (emr_hold_check),
-- and placing or lifting a hold takes it exclusive (the trigger below). A
-- hold placed while a purge runs waits for the purge to commit; a purge
-- that starts after a hold was placed waits for it, then sees it.
-- +goose StatementBegin
CREATE FUNCTION emr_hold_check(hid uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF hid IS NULL THEN
        RETURN true;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(hashtextextended('emr-hold:' || hid::text, 0));
    -- A new statement after the lock: under READ COMMITTED it sees a hold
    -- committed by the lock's last holder.
    RETURN public.mhai_hospital_on_hold(hid);
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_hold_check(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_hold_check(uuid) FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION mhai_legal_hold_lock() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('emr-hold:' || NEW.hospital_id::text, 0));
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_legal_hold_lock() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_legal_hold_lock() FROM PUBLIC;
CREATE TRIGGER legal_holds_lock BEFORE INSERT OR UPDATE ON legal_holds
    FOR EACH ROW EXECUTE FUNCTION mhai_legal_hold_lock();

------------------------------------------------------------------------
-- Purge requests.
------------------------------------------------------------------------
CREATE TABLE emr.purge_requests (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id  uuid NOT NULL REFERENCES public.hospitals (id),
    patient_id   uuid NOT NULL,
    kind         text NOT NULL DEFAULT 'exclusion' CHECK (kind IN ('exclusion')),
    requested_at timestamptz NOT NULL DEFAULT now(),
    due_at       timestamptz NOT NULL,
    state        text NOT NULL CHECK (state IN ('pending', 'held', 'done')),
    held_since   timestamptz,
    done_at      timestamptz,
    CONSTRAINT purge_requests_hospital_id UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    CONSTRAINT purge_requests_held CHECK (state <> 'held' OR held_since IS NOT NULL),
    CONSTRAINT purge_requests_done CHECK ((state = 'done') = (done_at IS NOT NULL))
);
ALTER TABLE emr.purge_requests OWNER TO mhai_migrate;
ALTER TABLE emr.purge_requests ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- One open request per patient.
CREATE UNIQUE INDEX purge_requests_open ON emr.purge_requests (hospital_id, patient_id) WHERE state <> 'done';
CREATE INDEX purge_requests_due ON emr.purge_requests (hospital_id, due_at) WHERE state = 'pending';

-- An alert's patient columns while its patient's purge is held: moved, not
-- deleted, so the alert names no excluded patient and nothing is lost
-- under the hold. The purge deletes them.
CREATE TABLE emr.purge_held_alerts (
    request_id   uuid NOT NULL,
    hospital_id  uuid NOT NULL,
    alert_id     uuid NOT NULL,
    patient_id   uuid,
    finding_id   uuid,
    dismiss_note text,
    amended      jsonb,
    PRIMARY KEY (request_id, alert_id),
    FOREIGN KEY (hospital_id, request_id) REFERENCES emr.purge_requests (hospital_id, id),
    FOREIGN KEY (hospital_id, alert_id) REFERENCES public.alerts (hospital_id, id) ON DELETE CASCADE
);
ALTER TABLE emr.purge_held_alerts OWNER TO mhai_migrate;
ALTER TABLE emr.purge_held_alerts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

-- No application role holds anything on either table: the definers below
-- (as the owner) are the only way in. They read the requests across
-- hospitals (which hospitals have work) and write only the hospital they
-- were called for.
CREATE POLICY owner_read ON emr.purge_requests FOR SELECT TO mhai_migrate USING (true);
CREATE POLICY owner_scoped ON emr.purge_requests TO mhai_migrate
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY owner_scoped ON emr.purge_held_alerts TO mhai_migrate
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());

-- The owner's writes the purge, the sweep and the removal need on tables
-- that had no owner policy for them, scoped to the hospital set.
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['public.emr_consumed', 'public.emr_connections', 'emr.sync_runs', 'emr.connection_errors',
                             'emr.sync_state', 'emr.breaker_trips', 'public.emr_rule_set_activation',
                             'public.alert_settings', 'public.alert_units', 'public.alert_levels',
                             'public.alert_recipients', 'public.user_notification_settings'] LOOP
        EXECUTE format('CREATE POLICY owner_maint ON %s TO mhai_migrate
                        USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- The sweep's reads by age.
CREATE INDEX raw_records_payload_age ON emr.raw_records (hospital_id, received_at) WHERE payload IS NOT NULL;
CREATE INDEX alerts_finding ON alerts (hospital_id, finding_id) WHERE finding_id IS NOT NULL;
CREATE INDEX findings_age ON emr.findings (hospital_id, created_at);

------------------------------------------------------------------------
-- The purge.
------------------------------------------------------------------------

-- Patient pid of hospital hid and every patient merged into it, at any
-- depth. Internal.
-- +goose StatementBegin
CREATE FUNCTION emr_purge_ids(hid uuid, pid uuid) RETURNS uuid[]
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH RECURSIVE s(id) AS (
        SELECT pid
        UNION
        SELECT p.id FROM emr.patients p JOIN s ON p.merged_into = s.id AND p.hospital_id = hid
    )
    SELECT coalesce(array_agg(s.id), '{}') FROM s
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_purge_ids(uuid, uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_purge_ids(uuid, uuid) FROM PUBLIC;

-- Take the emr service's link locks of patients ids (sorted, the order it
-- takes them in: advisory, then the patient row), so a normaliser or sink
-- transaction on them either finishes first or waits and then sees the
-- patient excluded. Internal.
-- +goose StatementBegin
CREATE FUNCTION emr_purge_lock(hid uuid, ids uuid[]) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('emr-link:' || k.connection_id::text || ':' || k.source_patient_id, 0))
    FROM (SELECT l.connection_id, l.source_patient_id FROM emr.patient_links l
          WHERE l.hospital_id = hid AND l.patient_id = ANY (ids) ORDER BY 1, 2) k;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_purge_lock(uuid, uuid[]) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_purge_lock(uuid, uuid[]) FROM PUBLIC;

-- Delete what is held for patients ids of hospital hid (the hospital set
-- for the transaction), for purge request req, and return the counts.
-- The caller has checked the hold and locked the patient. Internal.
-- +goose StatementBegin
CREATE FUNCTION emr_purge_patient_data(hid uuid, ids uuid[], req uuid) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    raws  uuid[];
    fids  uuid[];
    n_o   bigint;
    n_m   bigint;
    n_e   bigint;
    n_r   bigint;
    n_x   bigint;
    n_f   bigint;
    n_a   bigint;
    n_h   bigint;
BEGIN
    IF hid IS NULL OR hid IS DISTINCT FROM public.mhai_current_hospital() OR public.emr_hold_check(hid) THEN
        RAISE EXCEPTION USING ERRCODE = 'object_not_in_prerequisite_state', MESSAGE = 'emr purge: refused';
    END IF;

    -- Alerts keep codes and times; they lose the patient, the finding and
    -- the note (and a held copy of them goes too).
    UPDATE public.alerts a SET patient_id = NULL, finding_id = NULL, dismiss_note = NULL, amended = NULL, updated_at = now()
    WHERE a.hospital_id = hid AND a.patient_id = ANY (ids);
    GET DIAGNOSTICS n_a = ROW_COUNT;
    DELETE FROM emr.purge_held_alerts h WHERE h.hospital_id = hid AND h.request_id = req;
    GET DIAGNOSTICS n_h = ROW_COUNT;

    -- Findings: the patient's, and any standing on its observations.
    WITH f AS (
        DELETE FROM emr.findings f
        WHERE f.hospital_id = hid
          AND (f.patient_id = ANY (ids)
               OR f.primary_observation_id IN (SELECT o.id FROM emr.observations o
                                               WHERE o.hospital_id = hid AND o.patient_id = ANY (ids)))
        RETURNING f.id
    )
    SELECT coalesce(array_agg(f.id), '{}') INTO fids FROM f;
    n_f := cardinality(fids);
    DELETE FROM public.emr_consumed c WHERE c.hospital_id = hid AND c.kind = 'finding' AND c.ref_id = ANY (fids);

    SELECT coalesce(array_agg(r.raw_id), '{}') INTO raws FROM (
        SELECT o.raw_id FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids)
        UNION ALL SELECT d.raw_id FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids)
        UNION ALL SELECT e.raw_id FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids)
        -- The patient's own record, linked or not (a document patient).
        UNION ALL SELECT p.raw_id FROM emr.patients p WHERE p.hospital_id = hid AND p.id = ANY (ids) AND p.raw_id IS NOT NULL) r;

    DELETE FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids);
    GET DIAGNOSTICS n_o = ROW_COUNT;
    DELETE FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids);
    GET DIAGNOSTICS n_m = ROW_COUNT;
    DELETE FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids);
    GET DIAGNOSTICS n_e = ROW_COUNT;
    DELETE FROM emr.possible_matches x WHERE x.hospital_id = hid AND (x.patient_a = ANY (ids) OR x.patient_b = ANY (ids))
        AND NOT EXISTS (SELECT 1 FROM emr.merges g WHERE g.hospital_id = hid AND g.match_id = x.id);
    GET DIAGNOSTICS n_x = ROW_COUNT;
    -- The raw records behind those rows, the patient's own records, and
    -- every other record naming one of its source ids, unless a canonical
    -- row of another patient still stands on it.
    DELETE FROM emr.raw_records r
    WHERE r.hospital_id = hid
      AND (r.id = ANY (raws)
           OR EXISTS (SELECT 1 FROM emr.patient_links l
                      WHERE l.hospital_id = hid AND l.patient_id = ANY (ids) AND l.connection_id = r.connection_id
                        AND ((r.resource_kind = 'patient' AND r.source_id = l.source_patient_id)
                             OR r.payload ->> 'patient' = l.source_patient_id)))
      AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.raw_id = r.id);
    GET DIAGNOSTICS n_r = ROW_COUNT;

    -- The patient index keeps the ids and the links only.
    UPDATE emr.patients p SET display_name = NULL, mrn = NULL, birth_date = NULL, sex = 'unknown', raw_id = NULL,
                              updated_at = now()
    WHERE p.hospital_id = hid AND p.id = ANY (ids)
      AND (p.display_name IS NOT NULL OR p.mrn IS NOT NULL OR p.birth_date IS NOT NULL OR p.sex <> 'unknown'
           OR p.raw_id IS NOT NULL);

    RETURN jsonb_build_object('observations', n_o, 'medications', n_m, 'encounters', n_e, 'raw_records', n_r,
                              'matches', n_x, 'findings', n_f, 'alerts', n_a + n_h);
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_purge_patient_data(uuid, uuid[], uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_purge_patient_data(uuid, uuid[], uuid) FROM PUBLIC;

-- Under a hold: move the alerts' patient columns of patients ids into
-- emr.purge_held_alerts for request req. Internal.
-- +goose StatementBegin
CREATE FUNCTION emr_purge_hold_alerts(hid uuid, ids uuid[], req uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    INSERT INTO emr.purge_held_alerts (request_id, hospital_id, alert_id, patient_id, finding_id, dismiss_note, amended)
    SELECT req, hid, a.id, a.patient_id, a.finding_id, a.dismiss_note, a.amended FROM public.alerts a
    WHERE a.hospital_id = hid AND a.patient_id = ANY (ids)
    ON CONFLICT (request_id, alert_id) DO NOTHING;
    UPDATE public.alerts a SET patient_id = NULL, finding_id = NULL, dismiss_note = NULL, amended = NULL, updated_at = now()
    WHERE a.hospital_id = hid AND a.patient_id = ANY (ids);
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_purge_hold_alerts(uuid, uuid[], uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_purge_hold_alerts(uuid, uuid[], uuid) FROM PUBLIC;

------------------------------------------------------------------------
-- The exclusion, now with its durable purge request and the hold.
------------------------------------------------------------------------
DROP FUNCTION emr_exclude_patient(uuid, text);
-- Design §10.6: exclude a patient of the caller's hospital. At once: the
-- patient is excluded and its open alerts (and those of the patients
-- merged into it) are withdrawn; alert_ids are those, for core's
-- alert.withdrawn rows. Then the purge request: purged in this
-- transaction (held false, the counts, purge_by now; alerts_cleared the
-- alerts that lost the patient), or, under a legal
-- hold, held (held true, the counts 0, purge_by the request's due time;
-- nothing is deleted and the alerts' patient columns move aside). A
-- patient not active or excluded in the caller's hospital raises
-- no_data_found (core answers 404); excluding again reuses the open
-- request.
-- +goose StatementBegin
CREATE FUNCTION emr_exclude_patient(pid uuid, reason text)
    RETURNS TABLE (observations bigint, medications bigint, encounters bigint, raw_records bigint, matches bigint,
                   alert_ids uuid[], findings bigint, request_id uuid, held boolean, purge_by timestamptz,
                   alerts_cleared bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid  uuid := public.mhai_current_hospital();
    ids  uuid[];
    wd   uuid[];
    req  uuid;
    due  timestamptz;
    c    jsonb;
BEGIN
    IF reason IS NULL OR reason NOT IN ('objection', 'hospital_policy', 'other') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'emr_exclude_patient: reason';
    END IF;
    IF hid IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_exclude_patient: no such patient';
    END IF;
    -- Links move with a merge, so the patient's own links cover the
    -- patients merged into it.
    PERFORM public.emr_purge_lock(hid, ARRAY[pid]);
    PERFORM 1 FROM emr.patients p WHERE p.id = pid AND p.hospital_id = hid AND p.status IN ('active', 'excluded')
        FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_exclude_patient: no such patient';
    END IF;
    ids := public.emr_purge_ids(hid, pid);

    UPDATE emr.patients p SET status = 'excluded', excluded_at = coalesce(p.excluded_at, now()),
                              exclude_reason = coalesce(p.exclude_reason, emr_exclude_patient.reason), updated_at = now()
        WHERE p.hospital_id = hid AND p.id = pid;

    WITH w AS (
        UPDATE public.alerts a SET status = 'dismissed', withdrawn = 'excluded', dismiss_reason = 'other',
                                   handled_by = NULL, handled_at = now(), updated_at = now()
        WHERE a.hospital_id = hid AND a.patient_id = ANY (ids) AND a.status = 'new'
        RETURNING a.id
    )
    SELECT coalesce(array_agg(w.id ORDER BY w.id), '{}') INTO wd FROM w;

    SELECT r.id, r.due_at INTO req, due FROM emr.purge_requests r
    WHERE r.hospital_id = hid AND r.patient_id = pid AND r.state <> 'done'
    FOR UPDATE;
    IF NOT FOUND THEN
        INSERT INTO emr.purge_requests AS q (hospital_id, patient_id, due_at, state)
        VALUES (hid, pid, now() + interval '24 hours', 'pending')
        RETURNING q.id, q.due_at INTO req, due;
    END IF;

    IF public.emr_hold_check(hid) THEN
        UPDATE emr.purge_requests r SET state = 'held', held_since = coalesce(r.held_since, now())
        WHERE r.hospital_id = hid AND r.id = req;
        PERFORM public.emr_purge_hold_alerts(hid, ids, req);
        RETURN QUERY SELECT 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, wd, 0::bigint, req, true, due, 0::bigint;
        RETURN;
    END IF;

    c := public.emr_purge_patient_data(hid, ids, req);
    UPDATE emr.purge_requests r SET state = 'done', done_at = now() WHERE r.hospital_id = hid AND r.id = req;
    RETURN QUERY SELECT (c ->> 'observations')::bigint, (c ->> 'medications')::bigint, (c ->> 'encounters')::bigint,
                        (c ->> 'raw_records')::bigint, (c ->> 'matches')::bigint, wd, (c ->> 'findings')::bigint,
                        req, false, now(), (c ->> 'alerts')::bigint;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_exclude_patient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_exclude_patient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_exclude_patient(uuid, text) TO mhai_gateway, mhai_emr_admin;

------------------------------------------------------------------------
-- Core's maintenance loop (mhai_emr_maint).
------------------------------------------------------------------------

-- The hospitals with EMR data or a purge request: the loop visits each in
-- a transaction of its own, with the hospital set.
-- +goose StatementBegin
CREATE FUNCTION emr_maint_hospitals() RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.hospital_id FROM public.emr_connections c
    UNION
    SELECT r.hospital_id FROM emr.purge_requests r WHERE r.state <> 'done'
$$;
-- +goose StatementEnd

-- The caller's hospital's held requests go back to pending, due in 24
-- hours, once the hospital has no open hold. Returns their ids.
-- +goose StatementBegin
CREATE FUNCTION emr_release_holds() RETURNS uuid[]
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid uuid := public.mhai_current_hospital();
    res uuid[];
BEGIN
    IF hid IS NULL OR public.emr_hold_check(hid) THEN
        RETURN '{}';
    END IF;
    WITH r AS (
        UPDATE emr.purge_requests r SET state = 'pending', due_at = now() + interval '24 hours'
        WHERE r.hospital_id = hid AND r.state = 'held'
        RETURNING r.id
    )
    SELECT coalesce(array_agg(r.id ORDER BY r.id), '{}') INTO res FROM r;
    RETURN res;
END
$$;
-- +goose StatementEnd

-- The caller's hospital's pending requests, soonest due first, at most n
-- (1 to 1000).
-- +goose StatementBegin
CREATE FUNCTION emr_pending_purges(n integer) RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT r.id FROM emr.purge_requests r
    WHERE r.hospital_id = public.mhai_current_hospital() AND r.state = 'pending'
    ORDER BY r.due_at, r.id
    LIMIT least(greatest(coalesce(n, 0), 0), 1000)
$$;
-- +goose StatementEnd

-- Purge request rid of the caller's hospital: a pending request is purged
-- ({state: done, patient_id, the counts}) or, when the hospital is on hold
-- now, held ({state: held, patient_id}); any other request is left as it is
-- ({state}). Not the caller's hospital's: no_data_found.
-- +goose StatementBegin
CREATE FUNCTION emr_purge_patient(rid uuid) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid uuid := public.mhai_current_hospital();
    r   record;
    ids uuid[];
    c   jsonb;
BEGIN
    SELECT x.id, x.patient_id, x.state INTO r FROM emr.purge_requests x
    WHERE x.id = rid AND x.hospital_id = hid;
    IF hid IS NULL OR NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_purge_patient: no such request';
    END IF;
    -- The exclusion's lock order: link locks, the patient row, then the
    -- request.
    PERFORM public.emr_purge_lock(hid, ARRAY[r.patient_id]);
    PERFORM 1 FROM emr.patients p WHERE p.id = r.patient_id AND p.hospital_id = hid FOR UPDATE;
    SELECT x.id, x.patient_id, x.state INTO r FROM emr.purge_requests x
    WHERE x.id = rid AND x.hospital_id = hid FOR UPDATE;
    IF r.state <> 'pending' THEN
        RETURN jsonb_build_object('state', r.state, 'patient_id', r.patient_id);
    END IF;
    ids := public.emr_purge_ids(hid, r.patient_id);
    IF public.emr_hold_check(hid) THEN
        UPDATE emr.purge_requests x SET state = 'held', held_since = now() WHERE x.hospital_id = hid AND x.id = rid;
        PERFORM public.emr_purge_hold_alerts(hid, ids, rid);
        RETURN jsonb_build_object('state', 'held', 'patient_id', r.patient_id);
    END IF;
    c := public.emr_purge_patient_data(hid, ids, rid);
    UPDATE emr.purge_requests x SET state = 'done', done_at = now() WHERE x.hospital_id = hid AND x.id = rid;
    RETURN c || jsonb_build_object('state', 'done', 'patient_id', r.patient_id);
END
$$;
-- +goose StatementEnd

-- The caller's hospital's requests held for more than 90 days (design
-- §10.6: listed so they aren't forgotten).
-- +goose StatementBegin
CREATE FUNCTION emr_held_overdue() RETURNS TABLE (request_id uuid, held_since timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT r.id, r.held_since FROM emr.purge_requests r
    WHERE r.hospital_id = public.mhai_current_hospital() AND r.state = 'held'
      AND r.held_since < now() - interval '90 days'
    ORDER BY r.held_since, r.id
    LIMIT 500
$$;
-- +goose StatementEnd

-- Design §14.2: one capped pass of the retention rules over the caller's
-- hospital (cap rows per step, 1 to 100000). A hospital on hold is left
-- alone ({held: true}). Returns the counts and more: whether any step met
-- its cap (call again).
-- +goose StatementBegin
CREATE FUNCTION emr_retention_sweep(cap integer) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid   uuid := public.mhai_current_hospital();
    lim   integer := least(greatest(coalesce(cap, 0), 1), 100000);
    raws  uuid[];
    pids  uuid[];
    n     bigint;
    n_pay bigint;
    n_f   bigint;
    n_o   bigint;
    n_m   bigint;
    n_e   bigint;
    n_r   bigint;
    n_ap  bigint;
    n_a   bigint;
    n_c   bigint;
    n_p   bigint;
BEGIN
    IF hid IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_retention_sweep: no hospital';
    END IF;
    IF public.emr_hold_check(hid) THEN
        RETURN jsonb_build_object('held', true);
    END IF;

    -- Raw payloads: cleared after 7 days once normalised or rejected (the
    -- hash and the provenance stay).
    UPDATE emr.raw_records r SET payload = NULL
    WHERE r.hospital_id = hid AND r.id IN (
        SELECT x.id FROM emr.raw_records x
        WHERE x.hospital_id = hid AND x.payload IS NOT NULL AND x.status <> 'pending'
          AND x.received_at < now() - interval '7 days'
        ORDER BY x.received_at LIMIT lim);
    GET DIAGNOSTICS n_pay = ROW_COUNT;

    -- Findings that never became alerts: 90 days.
    WITH f AS (
        DELETE FROM emr.findings f
        WHERE f.hospital_id = hid AND f.id IN (
            SELECT x.id FROM emr.findings x
            WHERE x.hospital_id = hid AND x.created_at < now() - interval '90 days'
              AND NOT EXISTS (SELECT 1 FROM public.alerts a WHERE a.hospital_id = hid AND a.finding_id = x.id)
            ORDER BY x.created_at LIMIT lim)
        RETURNING f.id
    )
    SELECT count(*) INTO n_f FROM f;

    -- Canonical rows: 13 months after their time, with the raw records
    -- behind them (a finding on an observation goes with it).
    WITH o AS (
        DELETE FROM emr.observations o
        WHERE o.hospital_id = hid AND o.id IN (
            SELECT x.id FROM emr.observations x
            WHERE x.hospital_id = hid
              AND coalesce(x.effective_at, x.effective_date::timestamptz, x.received_at) < now() - interval '13 months'
            LIMIT lim)
        RETURNING o.raw_id
    )
    SELECT coalesce(array_agg(o.raw_id), '{}') INTO raws FROM o;
    n_o := cardinality(raws);
    WITH d AS (
        DELETE FROM emr.medications d
        WHERE d.hospital_id = hid AND d.id IN (
            SELECT x.id FROM emr.medications x
            WHERE x.hospital_id = hid AND coalesce(x.end_at, x.start_at, x.received_at) < now() - interval '13 months'
            LIMIT lim)
        RETURNING d.raw_id
    )
    SELECT raws || coalesce(array_agg(d.raw_id), '{}'), count(*) INTO raws, n_m FROM d;
    WITH e AS (
        DELETE FROM emr.encounters e
        WHERE e.hospital_id = hid AND e.id IN (
            SELECT x.id FROM emr.encounters x
            WHERE x.hospital_id = hid
              AND coalesce(x.period_end, x.period_start, x.received_at) < now() - interval '13 months'
            LIMIT lim)
        RETURNING e.raw_id
    )
    SELECT raws || coalesce(array_agg(e.raw_id), '{}'), count(*) INTO raws, n_e FROM e;
    -- Raw records: those behind the rows above, and any other older than
    -- 13 months that no canonical row stands on.
    DELETE FROM emr.raw_records r
    WHERE r.hospital_id = hid AND r.id = ANY (raws)
      AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.raw_id = r.id);
    GET DIAGNOSTICS n_r = ROW_COUNT;
    DELETE FROM emr.raw_records r
    WHERE r.hospital_id = hid
      AND r.id IN (SELECT x.id FROM emr.raw_records x
                   WHERE x.hospital_id = hid AND x.received_at < now() - interval '13 months'
                     AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.raw_id = x.id)
                     AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.raw_id = x.id)
                     AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.raw_id = x.id)
                   ORDER BY x.received_at LIMIT lim);
    GET DIAGNOSTICS n = ROW_COUNT;
    n_r := n_r + n;

    -- Alerts: the patient, the finding and the note go after 13 months;
    -- the alert, with its deliveries, after 24.
    UPDATE public.alerts a SET patient_id = NULL, finding_id = NULL, dismiss_note = NULL, amended = NULL, updated_at = now()
    WHERE a.hospital_id = hid AND a.id IN (
        SELECT x.id FROM public.alerts x
        WHERE x.hospital_id = hid AND x.created_at < now() - interval '13 months'
          AND (x.patient_id IS NOT NULL OR x.finding_id IS NOT NULL OR x.dismiss_note IS NOT NULL OR x.amended IS NOT NULL)
        LIMIT lim);
    GET DIAGNOSTICS n_ap = ROW_COUNT;
    DELETE FROM public.alerts a
    WHERE a.hospital_id = hid AND a.id IN (
        SELECT x.id FROM public.alerts x WHERE x.hospital_id = hid AND x.created_at < now() - interval '24 months' LIMIT lim);
    GET DIAGNOSTICS n_a = ROW_COUNT;

    -- What core's consumer recorded: 90 days, once its finding is gone.
    DELETE FROM public.emr_consumed c
    WHERE c.hospital_id = hid AND (c.hospital_id, c.kind, c.ref_id, c.status) IN (
        SELECT x.hospital_id, x.kind, x.ref_id, x.status FROM public.emr_consumed x
        WHERE x.hospital_id = hid AND x.created_at < now() - interval '90 days'
          AND (x.kind = 'signal'
               OR NOT EXISTS (SELECT 1 FROM emr.findings f WHERE f.hospital_id = hid AND f.id = x.ref_id))
        LIMIT lim);
    GET DIAGNOSTICS n_c = ROW_COUNT;

    -- Patients nothing references and nothing changed for 13 months (never
    -- an excluded one: its links drop later records; never one with a
    -- purge request). A patient merged into another that stays keeps it.
    SELECT coalesce(array_agg(p.id), '{}') INTO pids FROM (
        SELECT p.id FROM emr.patients p
        WHERE p.hospital_id = hid AND p.status IN ('active', 'merged_into')
          AND p.updated_at < now() - interval '13 months' AND p.created_at < now() - interval '13 months'
          AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM emr.findings f WHERE f.hospital_id = hid AND f.patient_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM public.alerts a WHERE a.hospital_id = hid AND a.patient_id = p.id)
          AND NOT EXISTS (SELECT 1 FROM emr.purge_requests q WHERE q.hospital_id = hid AND q.patient_id = p.id)
        LIMIT lim) p;
    LOOP
        -- A candidate another patient (not a candidate) was merged into
        -- stays, until nothing that stays points at a candidate.
        SELECT coalesce(array_agg(x), '{}') INTO pids FROM unnest(pids) x
        WHERE NOT EXISTS (SELECT 1 FROM emr.patients c
                          WHERE c.hospital_id = hid AND c.merged_into = x AND NOT (c.id = ANY (pids)));
        EXIT WHEN NOT EXISTS (SELECT 1 FROM unnest(pids) x
                              WHERE EXISTS (SELECT 1 FROM emr.patients c
                                            WHERE c.hospital_id = hid AND c.merged_into = x AND NOT (c.id = ANY (pids))));
    END LOOP;
    DELETE FROM emr.merges g WHERE g.hospital_id = hid AND (g.kept_patient = ANY (pids) OR g.merged_patient = ANY (pids));
    DELETE FROM emr.merges g WHERE g.hospital_id = hid AND g.match_id IN (
        SELECT x.id FROM emr.possible_matches x WHERE x.hospital_id = hid AND (x.patient_a = ANY (pids) OR x.patient_b = ANY (pids)));
    DELETE FROM emr.possible_matches x WHERE x.hospital_id = hid AND (x.patient_a = ANY (pids) OR x.patient_b = ANY (pids));
    DELETE FROM emr.patient_links l WHERE l.hospital_id = hid AND l.patient_id = ANY (pids);
    DELETE FROM emr.patients p WHERE p.hospital_id = hid AND p.id = ANY (pids);
    GET DIAGNOSTICS n_p = ROW_COUNT;

    RETURN jsonb_build_object('raw_payloads', n_pay, 'findings', n_f, 'observations', n_o, 'medications', n_m,
                              'encounters', n_e, 'raw_records', n_r, 'alert_patients', n_ap, 'alerts', n_a,
                              'consumed', n_c, 'patients', n_p,
                              'more', greatest(n_pay, n_f, n_o, n_m, n_e, n, n_ap, n_a, n_c, n_p) >= lim);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['emr_maint_hospitals()', 'emr_release_holds()', 'emr_pending_purges(integer)',
                             'emr_purge_patient(uuid)', 'emr_held_overdue()', 'emr_retention_sweep(integer)'] LOOP
        EXECUTE format('ALTER FUNCTION public.%s OWNER TO mhai_migrate', f);
        EXECUTE format('REVOKE ALL ON FUNCTION public.%s FROM PUBLIC', f);
        EXECUTE format('GRANT EXECUTE ON FUNCTION public.%s TO mhai_emr_maint', f);
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Hospital removal (plan departure 11).
------------------------------------------------------------------------

-- Delete every EMR row of hospital hid and its alert tables. Refused
-- (object_not_in_prerequisite_state) unless the hospital is
-- removal_scheduled, has no open legal hold, and holds no credential
-- metadata (every connection's credentials wiped first). Returns the
-- counts. The provisioning login only.
-- +goose StatementBegin
CREATE FUNCTION emr_remove_hospital_data(hid uuid) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    prev   text := current_setting('mhai.hospital_id', true);
    hstate text;
    res    jsonb := '{}';
    n      bigint;
    t      text;
BEGIN
    IF hid IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_remove_hospital_data: no such hospital';
    END IF;
    -- The owner's policies are scoped to the hospital set: this one, for
    -- the call, then put back.
    PERFORM set_config('mhai.hospital_id', hid::text, true);
    SELECT h.status INTO hstate FROM public.hospitals h WHERE h.id = hid;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_remove_hospital_data: no such hospital';
    END IF;
    IF hstate <> 'removal_scheduled' THEN
        RAISE EXCEPTION USING ERRCODE = 'object_not_in_prerequisite_state',
            MESSAGE = 'emr_remove_hospital_data: the hospital is not removal_scheduled';
    END IF;
    IF public.emr_hold_check(hid) THEN
        RAISE EXCEPTION USING ERRCODE = 'object_not_in_prerequisite_state',
            MESSAGE = 'emr_remove_hospital_data: the hospital is on legal hold';
    END IF;
    IF EXISTS (SELECT 1 FROM emr.credentials k WHERE k.hospital_id = hid) THEN
        RAISE EXCEPTION USING ERRCODE = 'object_not_in_prerequisite_state',
            MESSAGE = 'emr_remove_hospital_data: credentials not wiped';
    END IF;
    -- Children before parents.
    FOREACH t IN ARRAY ARRAY['emr.purge_held_alerts', 'public.alert_deliveries', 'public.alerts',
                             'public.alert_recipients', 'public.alert_levels', 'public.alert_units',
                             'public.alert_settings', 'public.user_notification_settings', 'public.emr_consumed',
                             'emr.findings', 'emr.breaker_trips', 'emr.observations', 'emr.medications',
                             'emr.encounters', 'emr.units', 'emr.merges', 'emr.possible_matches',
                             'emr.purge_requests', 'emr.patient_links', 'emr.patients', 'emr.raw_records',
                             'emr.unmapped_codes', 'emr.sync_state', 'emr.sync_runs', 'emr.connection_errors',
                             'public.emr_rule_set_activation', 'public.emr_connections'] LOOP
        EXECUTE format('DELETE FROM %s WHERE hospital_id = $1', t) USING hid;
        GET DIAGNOSTICS n = ROW_COUNT;
        res := res || jsonb_build_object(replace(replace(t, 'public.', ''), 'emr.', ''), n);
    END LOOP;
    PERFORM set_config('mhai.hospital_id', coalesce(prev, ''), true);
    RETURN res;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_remove_hospital_data(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_remove_hospital_data(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_remove_hospital_data(uuid) TO mhai_provision;
-- The removal reads its hospital's status and credential rows as the
-- owner (00031's owner_alert_read and 00020's owner_read).

------------------------------------------------------------------------
-- The operator's exclusion (mhai_emr_admin).
------------------------------------------------------------------------
-- The operator finds a patient by MRN through a definer that answers at
-- most two ids of the hospital set (two: ambiguous), never by reading the
-- MRN column itself.
-- +goose StatementBegin
CREATE FUNCTION emr_patients_by_mrn(p_mrn text) RETURNS SETOF uuid
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT p.id FROM emr.patients p
    WHERE p.hospital_id = public.mhai_current_hospital() AND p.mrn = p_mrn AND p.status = 'active'
      AND p_mrn IS NOT NULL AND char_length(p_mrn) BETWEEN 1 AND 64
    ORDER BY p.id LIMIT 2
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_patients_by_mrn(text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_patients_by_mrn(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_patients_by_mrn(text) TO mhai_emr_admin;
GRANT SELECT (id, short_code) ON hospitals TO mhai_emr_admin;
CREATE POLICY emr_admin_read ON hospitals FOR SELECT TO mhai_emr_admin USING (true);

------------------------------------------------------------------------
-- Audit appends: hospital set, no actor, their actions only.
------------------------------------------------------------------------
GRANT INSERT ON audit_log TO mhai_emr_maint, mhai_emr_admin;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_emr_maint, mhai_emr_admin;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_emr_maint, mhai_emr_admin;
CREATE POLICY emr_maint_append ON audit_log FOR INSERT TO mhai_emr_maint
    WITH CHECK (hospital_id = mhai_current_hospital() AND actor_id IS NULL
                AND action IN ('emr.exclusion_purged', 'emr.exclusion_purge_held', 'emr.exclusion_purge_released',
                               'emr.retention_swept'));
CREATE POLICY emr_admin_append ON audit_log FOR INSERT TO mhai_emr_admin
    WITH CHECK (hospital_id = mhai_current_hospital() AND actor_id IS NULL
                AND action IN ('emr.patient_excluded', 'alert.withdrawn', 'emr.exclusion_purged',
                               'emr.exclusion_purge_held'));
CREATE POLICY provision_emr_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (hospital_id IS NOT NULL AND actor_id IS NULL AND payload ->> 'via' = 'cli'
                AND action IN ('legal_hold.placed', 'legal_hold.lifted', 'emr.hospital_data_removed'));

-- +goose Down

-- A purge still owed (pending or held) would be lost with its table:
-- refused, so the operator settles it first.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM emr.purge_requests WHERE state <> 'done') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_not_in_prerequisite_state',
            MESSAGE = '00032 down: purge requests are pending or held; purge them first';
    END IF;
END
$$;
-- +goose StatementEnd

DROP POLICY provision_emr_append ON audit_log;
DROP POLICY emr_admin_append ON audit_log;
DROP POLICY emr_maint_append ON audit_log;
REVOKE EXECUTE ON FUNCTION audit_chain_head() FROM mhai_emr_maint, mhai_emr_admin;
REVOKE USAGE ON SEQUENCE audit_log_id_seq FROM mhai_emr_maint, mhai_emr_admin;
REVOKE INSERT ON audit_log FROM mhai_emr_maint, mhai_emr_admin;

DROP POLICY emr_admin_read ON hospitals;
REVOKE SELECT (id, short_code) ON hospitals FROM mhai_emr_admin;
DROP FUNCTION emr_patients_by_mrn(text);

DROP FUNCTION emr_remove_hospital_data(uuid);
DROP FUNCTION emr_retention_sweep(integer);
DROP FUNCTION emr_held_overdue();
DROP FUNCTION emr_purge_patient(uuid);
DROP FUNCTION emr_pending_purges(integer);
DROP FUNCTION emr_release_holds();
DROP FUNCTION emr_maint_hospitals();

-- 00029's exclusion, as it was (it purges at once, whatever the hold).
DROP FUNCTION emr_exclude_patient(uuid, text);
-- +goose StatementBegin
CREATE FUNCTION emr_exclude_patient(pid uuid, reason text)
    RETURNS TABLE (observations bigint, medications bigint, encounters bigint, raw_records bigint, matches bigint,
                   alert_ids uuid[])
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid  uuid := public.mhai_current_hospital();
    ids  uuid[];
    raws uuid[];
    wd   uuid[];
    n_o  bigint;
    n_m  bigint;
    n_e  bigint;
    n_r  bigint;
    n_x  bigint;
BEGIN
    IF reason IS NULL OR reason NOT IN ('objection', 'hospital_policy', 'other') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'emr_exclude_patient: reason';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('emr-link:' || k.connection_id::text || ':' || k.source_patient_id, 0))
    FROM (SELECT l.connection_id, l.source_patient_id FROM emr.patient_links l
          WHERE l.hospital_id = hid AND l.patient_id = pid ORDER BY 1, 2) k;
    PERFORM 1 FROM emr.patients p WHERE p.id = pid AND p.hospital_id = hid AND p.status IN ('active', 'excluded')
        FOR UPDATE;
    IF hid IS NULL OR NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_exclude_patient: no such patient';
    END IF;
    WITH RECURSIVE s(id) AS (
        SELECT pid
        UNION
        SELECT p.id FROM emr.patients p JOIN s ON p.merged_into = s.id AND p.hospital_id = hid
    )
    SELECT array_agg(s.id) INTO ids FROM s;

    UPDATE emr.patients p SET status = 'excluded', excluded_at = coalesce(p.excluded_at, now()),
                              exclude_reason = coalesce(p.exclude_reason, emr_exclude_patient.reason), updated_at = now()
        WHERE p.hospital_id = hid AND p.id = pid;
    UPDATE emr.patients p SET display_name = NULL, mrn = NULL, birth_date = NULL, sex = 'unknown', raw_id = NULL,
                              updated_at = now()
        WHERE p.hospital_id = hid AND p.id = ANY (ids);

    WITH w AS (
        UPDATE public.alerts a SET status = 'dismissed', withdrawn = 'excluded', dismiss_reason = 'other',
                                   handled_by = NULL, handled_at = now(), updated_at = now()
        WHERE a.hospital_id = hid AND a.patient_id = ANY (ids) AND a.status = 'new'
        RETURNING a.id
    )
    SELECT coalesce(array_agg(w.id ORDER BY w.id), '{}') INTO wd FROM w;
    UPDATE public.alerts a SET patient_id = NULL, finding_id = NULL, dismiss_note = NULL, updated_at = now()
        WHERE a.hospital_id = hid AND a.patient_id = ANY (ids);

    SELECT coalesce(array_agg(r.raw_id), '{}') INTO raws FROM (
        SELECT o.raw_id FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids)
        UNION ALL SELECT d.raw_id FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids)
        UNION ALL SELECT e.raw_id FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids)) r;

    DELETE FROM emr.observations o WHERE o.hospital_id = hid AND o.patient_id = ANY (ids);
    GET DIAGNOSTICS n_o = ROW_COUNT;
    DELETE FROM emr.medications d WHERE d.hospital_id = hid AND d.patient_id = ANY (ids);
    GET DIAGNOSTICS n_m = ROW_COUNT;
    DELETE FROM emr.encounters e WHERE e.hospital_id = hid AND e.patient_id = ANY (ids);
    GET DIAGNOSTICS n_e = ROW_COUNT;
    DELETE FROM emr.possible_matches x WHERE x.hospital_id = hid AND (x.patient_a = ANY (ids) OR x.patient_b = ANY (ids))
        AND NOT EXISTS (SELECT 1 FROM emr.merges g WHERE g.hospital_id = hid AND g.match_id = x.id);
    GET DIAGNOSTICS n_x = ROW_COUNT;
    DELETE FROM emr.raw_records r
    WHERE r.hospital_id = hid
      AND (r.id = ANY (raws)
           OR EXISTS (SELECT 1 FROM emr.patient_links l
                      WHERE l.hospital_id = hid AND l.patient_id = ANY (ids) AND l.connection_id = r.connection_id
                        AND ((r.resource_kind = 'patient' AND r.source_id = l.source_patient_id)
                             OR r.payload ->> 'patient' = l.source_patient_id)))
      AND NOT EXISTS (SELECT 1 FROM emr.observations o WHERE o.hospital_id = hid AND o.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.medications d WHERE d.hospital_id = hid AND d.raw_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM emr.encounters e WHERE e.hospital_id = hid AND e.raw_id = r.id);
    GET DIAGNOSTICS n_r = ROW_COUNT;
    RETURN QUERY SELECT n_o, n_m, n_e, n_r, n_x, wd;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_exclude_patient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_exclude_patient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_exclude_patient(uuid, text) TO mhai_gateway, mhai_emr_admin;

DROP FUNCTION emr_purge_hold_alerts(uuid, uuid[], uuid);
DROP FUNCTION emr_purge_patient_data(uuid, uuid[], uuid);
DROP FUNCTION emr_purge_lock(uuid, uuid[]);
DROP FUNCTION emr_purge_ids(uuid, uuid);

DROP INDEX emr.findings_age;
DROP INDEX alerts_finding;
DROP INDEX emr.raw_records_payload_age;

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['public.emr_consumed', 'public.emr_connections', 'emr.sync_runs', 'emr.connection_errors',
                             'emr.sync_state', 'emr.breaker_trips', 'public.emr_rule_set_activation',
                             'public.alert_settings', 'public.alert_units', 'public.alert_levels',
                             'public.alert_recipients', 'public.user_notification_settings'] LOOP
        EXECUTE format('DROP POLICY owner_maint ON %s', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- The tables take their policies, indexes and grants with them.
DROP TABLE emr.purge_held_alerts;
DROP TABLE emr.purge_requests;
DROP TRIGGER legal_holds_lock ON legal_holds;
DROP FUNCTION mhai_legal_hold_lock();
DROP FUNCTION emr_hold_check(uuid);
DROP FUNCTION mhai_hospital_on_hold(uuid);
DROP TABLE legal_holds;

REVOKE mhai_emr_maint FROM mhai_app;
