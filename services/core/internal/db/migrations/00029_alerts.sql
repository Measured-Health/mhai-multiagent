-- EMR integration, alerts (EMR design §12, §13.2, §15.3, §16.2, §16.3,
-- §18 item 4; plan Tasks 31-34). Numbered 00029: 00028 is Analysis PR 5's.
-- Nothing here raises an alert: core's finding consumer (PR 8b) writes the
-- alerts, and with no consumer running the routes answer empty lists.
--
-- The hospital admin's settings (design §12.2): alert_settings (the queue
-- switch and the version, one row per hospital, made on the first save),
-- alert_units (a unit with no row is covered: a new unit is covered by
-- default), alert_levels (a category with no row has its default level) and
-- alert_recipients (named clinicians). A named recipient, and every
-- delivery, must be an active clinician of the alert's hospital (A15): a
-- trigger checks it, whatever writes the row.
--
-- alerts and alert_deliveries: who sees an alert is fixed when it is raised
-- (routed_queue, and one delivery row per named clinician), never
-- recomputed from later settings (Review Focus 4). One open alert per
-- patient, rule and analyte (design §12.4). The gateway may only move an
-- alert out of new, as the caller, once (first handler wins at the
-- database). alerts.finding_id and patient_id have no key to the finding:
-- an exclusion deletes the finding and clears the alert's patient columns,
-- and the alert stays for the counts.
--
-- A recipient who leaves (plan departure 8): a user deactivated or losing
-- the clinician role, or taken off the named list, has every open alert
-- delivered only to them moved to the queue in the same transaction, with
-- an alert.requeued fact (ids and a reason code, at most 500 ids a fact) in
-- intelligence_audit_outbox; core's drainer puts it on the chain. The
-- outbox takes facts with no question or job for alert.* actions only, and
-- the worker may not write them.
--
-- The merge re-points the merged patient's alerts, and the exclusion
-- withdraws the patient's open alerts (withdrawn excluded) and clears the
-- patient columns of all of them (TODO.md, PR 5 follow-ups). Both now
-- return what core audits. emr_reject_match() rejects a proposed match.
--
-- The emr service no longer announces shadow findings (TODO.md, PR 7
-- follow-ups): they are never alerts, so core's consumer never sees them;
-- the unannounced index leaves them out.
--
-- Roles:
--   mhai_gateway   its hospital's settings (read, write); alerts and its own
--                  deliveries (read; handle a new alert as the caller; mark
--                  its own read); its own notification settings; the
--                  finding and match columns an alert or a match card shows;
--                  EXECUTE on emr_hospital_sources(), emr_patient_sources(),
--                  emr_alert_links(), emr_reject_match() and
--                  alerts_requeue_removed().
--   mhai_emr_core  its hospital's alerts and deliveries (write, PR 8b), the
--                  settings (read).

-- +goose Up

------------------------------------------------------------------------
-- Settings.
------------------------------------------------------------------------
CREATE TABLE alert_settings (
    hospital_id uuid PRIMARY KEY REFERENCES hospitals (id),
    queue       boolean NOT NULL DEFAULT true,
    version     integer NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_by  uuid REFERENCES users (id),
    updated_at  timestamptz
);

CREATE TABLE alert_units (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    unit_id     uuid NOT NULL,
    covered     boolean NOT NULL,
    PRIMARY KEY (hospital_id, unit_id),
    FOREIGN KEY (hospital_id, unit_id) REFERENCES emr.units (hospital_id, id) ON DELETE CASCADE
);

CREATE TABLE alert_levels (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    category    text NOT NULL CHECK (category IN ('blood_chemistry', 'blood_count', 'kidney_function',
                                                  'blood_gases_lactate', 'heart_markers', 'blood_sugar',
                                                  'vital_signs', 'infection_markers')),
    level       text NOT NULL CHECK (level IN ('off', 'critical', 'critical_urgent', 'all')),
    PRIMARY KEY (hospital_id, category)
);

CREATE TABLE alert_recipients (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL REFERENCES users (id),
    PRIMARY KEY (hospital_id, user_id)
);

------------------------------------------------------------------------
-- Alerts.
------------------------------------------------------------------------
CREATE TABLE alerts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id      uuid NOT NULL REFERENCES hospitals (id),
    finding_id       uuid,
    patient_id       uuid,
    connection_id    uuid,
    rule_id          text NOT NULL CHECK (rule_id ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    loinc            text CHECK (loinc ~ '^[0-9]{1,7}-[0-9]$'),
    category         text NOT NULL CHECK (category IN ('blood_chemistry', 'blood_count', 'kidney_function',
                                                       'blood_gases_lactate', 'heart_markers', 'blood_sugar',
                                                       'vital_signs', 'infection_markers')),
    severity         text NOT NULL CHECK (severity IN ('critical', 'urgent', 'info')),
    status           text NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'handled', 'dismissed')),
    routed_queue     boolean NOT NULL,
    grouped          boolean NOT NULL DEFAULT false,
    preliminary      boolean NOT NULL DEFAULT false,
    arrived_late     boolean NOT NULL DEFAULT false,
    amended          jsonb CHECK (amended IS NULL OR (jsonb_typeof(amended) = 'object' AND octet_length(amended::text) <= 1024)),
    withdrawn        text CHECK (withdrawn IN ('source_amended', 'source_retracted', 'excluded')),
    handled_by       uuid REFERENCES users (id),
    handled_at       timestamptz,
    dismiss_reason   text CHECK (dismiss_reason IN ('not_relevant', 'already_known', 'wrong_patient', 'other')),
    dismiss_note     text CHECK (char_length(dismiss_note) BETWEEN 1 AND 500),
    last_notified_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT alerts_hospital_id UNIQUE (hospital_id, id),
    FOREIGN KEY (hospital_id, patient_id) REFERENCES emr.patients (hospital_id, id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id),
    CONSTRAINT alerts_handled CHECK ((status = 'new') = (handled_at IS NULL)),
    -- Handled by a person, unless the source or an exclusion withdrew it.
    CONSTRAINT alerts_handled_by CHECK (status = 'new' OR (handled_by IS NULL) = (withdrawn IS NOT NULL)),
    CONSTRAINT alerts_dismissed CHECK ((status = 'dismissed') = (dismiss_reason IS NOT NULL)),
    CONSTRAINT alerts_note CHECK (dismiss_note IS NULL OR status = 'dismissed'),
    -- Design §12.5: a withdrawn alert reads as dismissed, by nobody, other.
    CONSTRAINT alerts_withdrawn CHECK (withdrawn IS NULL OR (status = 'dismissed' AND dismiss_reason = 'other'))
);
-- Design §12.4: one open alert per patient, rule and analyte.
CREATE UNIQUE INDEX alerts_one_open ON alerts (hospital_id, patient_id, rule_id, coalesce(loinc, ''))
    WHERE status = 'new';
CREATE INDEX alerts_list ON alerts (hospital_id, created_at DESC, id DESC);
CREATE INDEX alerts_patient ON alerts (hospital_id, patient_id);

CREATE TABLE alert_deliveries (
    alert_id    uuid NOT NULL,
    hospital_id uuid NOT NULL,
    -- A27: the only recipient is a user (a clinician, checked below); there
    -- is no patient or address recipient.
    user_id     uuid NOT NULL REFERENCES users (id),
    read_at     timestamptz,
    notified_at timestamptz,
    PRIMARY KEY (alert_id, user_id),
    FOREIGN KEY (hospital_id, alert_id) REFERENCES alerts (hospital_id, id) ON DELETE CASCADE
);
CREATE INDEX alert_deliveries_user ON alert_deliveries (hospital_id, user_id);

CREATE TABLE user_notification_settings (
    user_id           uuid PRIMARY KEY REFERENCES users (id),
    hospital_id       uuid NOT NULL REFERENCES hospitals (id),
    alert_popups      boolean NOT NULL DEFAULT true,
    correction_popups boolean NOT NULL DEFAULT true,
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['alert_settings', 'alert_units', 'alert_levels', 'alert_recipients', 'alerts',
                             'alert_deliveries', 'user_notification_settings'] LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO mhai_migrate', t);
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- A15: recipients are active clinicians of the same hospital.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_alert_recipient_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.users u
                   JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'clinician'
                   WHERE u.id = NEW.user_id AND u.hospital_id = NEW.hospital_id AND u.is_active) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'alert recipient: not an active clinician of the hospital';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_alert_recipient_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_alert_recipient_check() FROM PUBLIC;
CREATE TRIGGER alert_recipients_check BEFORE INSERT OR UPDATE ON alert_recipients
    FOR EACH ROW EXECUTE FUNCTION mhai_alert_recipient_check();
CREATE TRIGGER alert_deliveries_check BEFORE INSERT OR UPDATE OF user_id, hospital_id ON alert_deliveries
    FOR EACH ROW EXECUTE FUNCTION mhai_alert_recipient_check();

------------------------------------------------------------------------
-- Grants and policies.
------------------------------------------------------------------------
GRANT SELECT (hospital_id, queue, version, updated_by, updated_at),
      INSERT (hospital_id, queue, version, updated_by, updated_at),
      UPDATE (queue, version, updated_by, updated_at) ON alert_settings TO mhai_gateway;
GRANT SELECT (hospital_id, unit_id, covered), INSERT (hospital_id, unit_id, covered), UPDATE (covered)
    ON alert_units TO mhai_gateway;
GRANT SELECT (hospital_id, category, level), INSERT (hospital_id, category, level), UPDATE (level)
    ON alert_levels TO mhai_gateway;
GRANT SELECT (hospital_id, user_id), INSERT (hospital_id, user_id), DELETE ON alert_recipients TO mhai_gateway;
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['alert_settings', 'alert_units', 'alert_levels', 'alert_recipients'] LOOP
        EXECUTE format('CREATE POLICY gateway_hospital ON public.%I TO mhai_gateway
                        USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
        EXECUTE format('GRANT SELECT ON public.%I TO mhai_emr_core', t);
        EXECUTE format('CREATE POLICY core_read ON public.%I FOR SELECT TO mhai_emr_core
                        USING (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- Alerts: the gateway reads its hospital's, and moves one out of new as
-- the caller (the restrictive policy: only from new, so the first handler
-- wins and nothing reopens).
GRANT SELECT (id, hospital_id, finding_id, patient_id, connection_id, rule_id, loinc, category, severity, status,
              routed_queue, grouped, preliminary, arrived_late, amended, withdrawn, handled_by, handled_at,
              dismiss_reason, dismiss_note, last_notified_at, created_at, updated_at) ON alerts TO mhai_gateway;
GRANT UPDATE (status, handled_by, handled_at, dismiss_reason, dismiss_note, updated_at) ON alerts TO mhai_gateway;
CREATE POLICY gateway_read ON alerts FOR SELECT TO mhai_gateway USING (hospital_id = mhai_current_hospital());
CREATE POLICY gateway_handle ON alerts FOR UPDATE TO mhai_gateway
    USING (hospital_id = mhai_current_hospital()
           AND (routed_queue OR EXISTS (SELECT 1 FROM alert_deliveries d
                                        WHERE d.hospital_id = alerts.hospital_id AND d.alert_id = alerts.id
                                          AND d.user_id = mhai_current_user())))
    WITH CHECK (hospital_id = mhai_current_hospital() AND status IN ('handled', 'dismissed')
                AND handled_by = mhai_current_user() AND withdrawn IS NULL);
CREATE POLICY gateway_only_new ON alerts AS RESTRICTIVE FOR UPDATE TO mhai_gateway USING (status = 'new') WITH CHECK (true);

GRANT SELECT (alert_id, hospital_id, user_id, read_at, notified_at) ON alert_deliveries TO mhai_gateway;
GRANT UPDATE (read_at) ON alert_deliveries TO mhai_gateway;
CREATE POLICY gateway_read ON alert_deliveries FOR SELECT TO mhai_gateway USING (hospital_id = mhai_current_hospital());
CREATE POLICY gateway_own_read_at ON alert_deliveries FOR UPDATE TO mhai_gateway
    USING (hospital_id = mhai_current_hospital() AND user_id = mhai_current_user())
    WITH CHECK (hospital_id = mhai_current_hospital() AND user_id = mhai_current_user());

GRANT SELECT (user_id, hospital_id, alert_popups, correction_popups, updated_at),
      INSERT (user_id, hospital_id, alert_popups, correction_popups, updated_at),
      UPDATE (alert_popups, correction_popups, updated_at) ON user_notification_settings TO mhai_gateway;
CREATE POLICY gateway_own ON user_notification_settings TO mhai_gateway
    USING (hospital_id = mhai_current_hospital() AND user_id = mhai_current_user())
    WITH CHECK (hospital_id = mhai_current_hospital() AND user_id = mhai_current_user());

-- Core's finding consumer (PR 8b): writes its hospital's alerts and
-- deliveries; reads the settings above and the notification switches.
GRANT SELECT (id, hospital_id, finding_id, patient_id, connection_id, rule_id, loinc, category, severity, status,
              routed_queue, grouped, preliminary, arrived_late, amended, withdrawn, handled_by, handled_at,
              dismiss_reason, last_notified_at, created_at, updated_at),
      INSERT (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc, category, severity, routed_queue,
              grouped, preliminary, arrived_late, last_notified_at),
      UPDATE (severity, status, routed_queue, grouped, preliminary, arrived_late, amended, withdrawn, handled_at,
              dismiss_reason, finding_id, last_notified_at, updated_at) ON alerts TO mhai_emr_core;
GRANT SELECT (alert_id, hospital_id, user_id, read_at, notified_at), INSERT (alert_id, hospital_id, user_id, notified_at),
      UPDATE (notified_at) ON alert_deliveries TO mhai_emr_core;
GRANT SELECT (user_id, hospital_id, alert_popups, correction_popups) ON user_notification_settings TO mhai_emr_core;
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['alerts', 'alert_deliveries'] LOOP
        EXECUTE format('CREATE POLICY core_write ON public.%I TO mhai_emr_core
                        USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
        -- The definers run as the owner, bound to the caller's hospital.
        EXECUTE format('CREATE POLICY owner_scoped ON public.%I TO mhai_migrate
                        USING (hospital_id = public.mhai_current_hospital())
                        WITH CHECK (hospital_id = public.mhai_current_hospital())', t);
    END LOOP;
END
$$;
-- +goose StatementEnd
CREATE POLICY core_read ON user_notification_settings FOR SELECT TO mhai_emr_core
    USING (hospital_id = mhai_current_hospital());

-- The finding columns an alert shows, and the match columns a match card
-- shows: the gateway, its hospital only.
GRANT SELECT (id, hospital_id, patient_id, connection_id, rule_set_version, rule_id, kind, category, severity,
              primary_observation_id, causes, effective_at, effective_date, received_at, status, withdrawn_reason,
              preliminary, arrived_late, standardised, created_at) ON emr.findings TO mhai_gateway;
CREATE POLICY gateway_read ON emr.findings FOR SELECT TO mhai_gateway USING (hospital_id = public.mhai_current_hospital());
GRANT SELECT (id, hospital_id, patient_a, patient_b, reason, proposed_at, decided_at, decision)
    ON emr.possible_matches TO mhai_gateway;
CREATE POLICY gateway_read ON emr.possible_matches FOR SELECT TO mhai_gateway
    USING (hospital_id = public.mhai_current_hospital());

-- Shadow findings are never announced (they are never alerts).
DROP INDEX emr.findings_unannounced;
CREATE INDEX findings_unannounced ON emr.findings (hospital_id, created_at)
    WHERE status <> 'historical' AND NOT shadow AND announced IS DISTINCT FROM status;

------------------------------------------------------------------------
-- A recipient leaves: their open alerts move to the queue (departure 8).
------------------------------------------------------------------------
-- The outbox takes alert facts, with no question or job; only the owner's
-- definers write them (the worker may not).
ALTER TABLE intelligence_audit_outbox
    DROP CONSTRAINT intelligence_audit_outbox_one_parent,
    ADD CONSTRAINT intelligence_audit_outbox_one_parent
        CHECK (num_nonnulls(query_id, job_id) = CASE WHEN action LIKE 'alert.%' THEN 0 ELSE 1 END),
    DROP CONSTRAINT intelligence_audit_outbox_action_check,
    ADD CONSTRAINT intelligence_audit_outbox_action_check
        CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed',
                          'alert.requeued'));
CREATE POLICY worker_no_alert_facts ON intelligence_audit_outbox AS RESTRICTIVE FOR INSERT TO mhai_worker
    WITH CHECK (action NOT LIKE 'alert.%');
DROP POLICY drain_append ON audit_log;
CREATE POLICY drain_append ON audit_log FOR INSERT TO mhai_audit_drain
    WITH CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed',
                           'alert.requeued')
                AND hospital_id IS NOT NULL);

-- mhai_alerts_requeue(hid, users, reason): every new alert of hospital hid
-- that is delivered to one of users, isn't on the queue, and has no
-- delivery to another active clinician moves to the queue; a critical one
-- is notified again (last_notified_at cleared). One alert.requeued fact per
-- 500 ids. The hospital is set for the statement (the owner's policies are
-- scoped to it) and put back after. Callers: the triggers below and
-- alerts_requeue_removed(); no role may call it directly.
-- +goose StatementBegin
CREATE FUNCTION mhai_alerts_requeue(hid uuid, uids uuid[], reason text) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    prev text := current_setting('mhai.hospital_id', true);
    ids  uuid[];
    n    integer;
    i    integer := 1;
BEGIN
    IF hid IS NULL OR uids IS NULL OR cardinality(uids) = 0
       OR reason NOT IN ('user_deactivated', 'role_removed', 'recipient_removed') THEN
        RETURN 0;
    END IF;
    PERFORM set_config('mhai.hospital_id', hid::text, true);
    WITH moved AS (
        UPDATE public.alerts a
        SET routed_queue = true,
            last_notified_at = CASE WHEN a.severity = 'critical' THEN NULL ELSE a.last_notified_at END,
            updated_at = now()
        WHERE a.hospital_id = hid AND a.status = 'new' AND NOT a.routed_queue
          AND EXISTS (SELECT 1 FROM public.alert_deliveries d
                      WHERE d.hospital_id = hid AND d.alert_id = a.id AND d.user_id = ANY (uids))
          AND NOT EXISTS (SELECT 1 FROM public.alert_deliveries d
                          JOIN public.users u ON u.id = d.user_id AND u.hospital_id = hid AND u.is_active
                          JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'clinician'
                          WHERE d.hospital_id = hid AND d.alert_id = a.id AND NOT (d.user_id = ANY (uids)))
        RETURNING a.id
    )
    SELECT coalesce(array_agg(m.id ORDER BY m.id), '{}') INTO ids FROM moved m;
    n := cardinality(ids);
    WHILE i <= n LOOP
        INSERT INTO public.intelligence_audit_outbox (hospital_id, action, entity, payload)
        VALUES (hid, 'alert.requeued', 'alert',
                jsonb_build_object('alert_ids', to_jsonb(ids[i:i + 499]::text[]), 'reason', reason));
        i := i + 500;
    END LOOP;
    PERFORM set_config('mhai.hospital_id', coalesce(prev, ''), true);
    RETURN n;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_alerts_requeue(uuid, uuid[], text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_alerts_requeue(uuid, uuid[], text) FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION mhai_alerts_requeue_user() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    PERFORM public.mhai_alerts_requeue(NEW.hospital_id, ARRAY[NEW.id], 'user_deactivated');
    RETURN NULL;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION mhai_alerts_requeue_role() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid uuid;
BEGIN
    SELECT u.hospital_id INTO hid FROM public.users u WHERE u.id = OLD.user_id;
    PERFORM public.mhai_alerts_requeue(hid, ARRAY[OLD.user_id], 'role_removed');
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_alerts_requeue_user() OWNER TO mhai_migrate;
ALTER FUNCTION mhai_alerts_requeue_role() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_alerts_requeue_user(), mhai_alerts_requeue_role() FROM PUBLIC;
CREATE TRIGGER users_requeue_alerts AFTER UPDATE OF is_active ON users
    FOR EACH ROW WHEN (OLD.is_active AND NOT NEW.is_active)
    EXECUTE FUNCTION mhai_alerts_requeue_user();
CREATE TRIGGER user_roles_requeue_alerts AFTER DELETE ON user_roles
    FOR EACH ROW WHEN (OLD.role = 'clinician')
    EXECUTE FUNCTION mhai_alerts_requeue_role();
CREATE TRIGGER user_roles_requeue_alerts_update AFTER UPDATE OF role ON user_roles
    FOR EACH ROW WHEN (OLD.role = 'clinician' AND NEW.role <> 'clinician')
    EXECUTE FUNCTION mhai_alerts_requeue_role();

-- A hospital admin takes clinicians off the named list (design §12.2): the
-- same move, for the caller's hospital, in the settings transaction; only
-- for users no longer on the list (a named recipient's alerts never move).
CREATE POLICY owner_read ON alert_recipients FOR SELECT TO mhai_migrate
    USING (hospital_id = mhai_current_hospital());
-- +goose StatementBegin
CREATE FUNCTION alerts_requeue_removed(uids uuid[]) RETURNS integer
    LANGUAGE sql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT public.mhai_alerts_requeue(public.mhai_current_hospital(),
        ARRAY(SELECT u FROM unnest(uids) u
              WHERE NOT EXISTS (SELECT 1 FROM public.alert_recipients r
                                WHERE r.hospital_id = public.mhai_current_hospital() AND r.user_id = u)),
        'recipient_removed')
$$;
-- +goose StatementEnd
ALTER FUNCTION alerts_requeue_removed(uuid[]) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION alerts_requeue_removed(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION alerts_requeue_removed(uuid[]) TO mhai_gateway;

------------------------------------------------------------------------
-- Reads the gateway can't make itself (it holds nothing on the
-- connections or the patient links).
------------------------------------------------------------------------
-- Design §16.2: every source of the caller's hospital, by label: off, or
-- connected, or problems (paused, or no successful poll in three
-- intervals). No address, type or error code.
-- +goose StatementBegin
CREATE FUNCTION emr_hospital_sources()
    RETURNS TABLE (connection_id uuid, source_label text, status text, last_update_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.source_label,
           CASE WHEN NOT c.enabled THEN 'off'
                WHEN c.paused THEN 'problems'
                WHEN l.at IS NULL OR l.at < now() - make_interval(secs => 3 * c.poll_seconds) THEN 'problems'
                ELSE 'connected' END,
           l.at
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT max(r.finished_at) AS at FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL) l ON true
    WHERE c.hospital_id = public.mhai_current_hospital()
    ORDER BY c.source_label, c.id
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_hospital_sources() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_hospital_sources() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_hospital_sources() TO mhai_gateway;

-- Design §16.2: a match card's sources (labels) and record count, for
-- patients of the caller's hospital only.
-- +goose StatementBegin
CREATE FUNCTION emr_patient_sources(pids uuid[])
    RETURNS TABLE (patient_id uuid, sources text[], records bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT p.id,
           coalesce((SELECT array_agg(DISTINCT c.source_label ORDER BY c.source_label)
                     FROM emr.patient_links l
                     JOIN public.emr_connections c ON c.id = l.connection_id AND c.hospital_id = l.hospital_id
                     WHERE l.hospital_id = p.hospital_id AND l.patient_id = p.id), '{}'),
           (SELECT count(*) FROM emr.observations o
            WHERE o.hospital_id = p.hospital_id AND o.patient_id = p.id AND o.superseded_by IS NULL)
           + (SELECT count(*) FROM emr.medications d
              WHERE d.hospital_id = p.hospital_id AND d.patient_id = p.id AND d.superseded_by IS NULL)
    FROM emr.patients p
    WHERE p.hospital_id = public.mhai_current_hospital() AND p.id = ANY (pids)
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_patient_sources(uuid[]) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_patient_sources(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_patient_sources(uuid[]) TO mhai_gateway;

-- Design §5.1, A25 (plan departure 16): what core needs to build an
-- alert's record link (gokit/recordlink, at read time): the connection's
-- template and base address and the patient's one source id on it. Only
-- for alerts the caller sees (on the queue, or delivered to them), and only
-- when the patient has exactly one id on that connection (after a merge it
-- may have two; then no link, never the wrong record).
-- +goose StatementBegin
CREATE FUNCTION emr_alert_links(aids uuid[])
    RETURNS TABLE (alert_id uuid, template text, base_url text, source_patient_id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT a.id, c.settings ->> 'record_link', c.base_url, l.source_patient_id
    FROM public.alerts a
    JOIN public.emr_connections c ON c.id = a.connection_id AND c.hospital_id = a.hospital_id
    JOIN LATERAL (SELECT min(x.source_patient_id) AS source_patient_id, count(*) AS n FROM emr.patient_links x
                  WHERE x.hospital_id = a.hospital_id AND x.connection_id = a.connection_id
                    AND x.patient_id = a.patient_id) l ON l.n = 1
    WHERE a.hospital_id = public.mhai_current_hospital() AND a.id = ANY (aids)
      AND c.settings ? 'record_link'
      AND (a.routed_queue OR EXISTS (SELECT 1 FROM public.alert_deliveries d
                                     WHERE d.hospital_id = a.hospital_id AND d.alert_id = a.id
                                       AND d.user_id = public.mhai_current_user()))
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_alert_links(uuid[]) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_alert_links(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_alert_links(uuid[]) TO mhai_gateway;

------------------------------------------------------------------------
-- Matches: reject (design §10.2), and the merge re-points alerts.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION emr_reject_match(match_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    UPDATE emr.possible_matches x SET decision = 'rejected', decided_at = now()
    WHERE x.id = match_id AND x.hospital_id = public.mhai_current_hospital() AND x.decision IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_reject_match: no such match';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_reject_match(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_reject_match(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_reject_match(uuid) TO mhai_gateway, mhai_emr_admin;

-- 00027's merge, plus: the merged patient's alerts move to the kept one,
-- except an open alert whose rule and analyte the kept patient already has
-- open (it stays on the merged patient's row, still listed: nothing is
-- lost and the one-open rule holds).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_merge_patients(match_id uuid, keep text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid      uuid := public.mhai_current_hospital();
    m        record;
    kept     uuid;
    gone     uuid;
    n        integer;
    v_merge  uuid := gen_random_uuid();
BEGIN
    IF hid IS NULL OR keep IS NULL OR keep NOT IN ('a', 'b') THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    SELECT x.id, x.patient_a, x.patient_b INTO m FROM emr.possible_matches x
    WHERE x.id = match_id AND x.hospital_id = hid AND x.decision IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    IF keep = 'a' THEN kept := m.patient_a; gone := m.patient_b;
    ELSE kept := m.patient_b; gone := m.patient_a; END IF;
    PERFORM 1 FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid ORDER BY p.id FOR UPDATE;
    SELECT count(*) INTO n FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid AND p.status = 'active';
    IF n <> 2 THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    INSERT INTO emr.merges (id, hospital_id, match_id, kept_patient, merged_patient, merged_by)
    VALUES (v_merge, hid, m.id, kept, gone, NULLIF(current_setting('mhai.user_id', true), '')::uuid);
    UPDATE emr.patient_links l SET patient_id = kept, merge_id = v_merge
        WHERE l.hospital_id = hid AND l.patient_id = gone;
    UPDATE emr.encounters e SET patient_id = kept, merge_id = v_merge
        WHERE e.hospital_id = hid AND e.patient_id = gone;
    UPDATE emr.observations o SET patient_id = kept, merge_id = v_merge
        WHERE o.hospital_id = hid AND o.patient_id = gone;
    UPDATE emr.medications d SET patient_id = kept, merge_id = v_merge
        WHERE d.hospital_id = hid AND d.patient_id = gone;
    UPDATE emr.findings f SET patient_id = kept
        WHERE f.hospital_id = hid AND f.patient_id = gone;
    UPDATE public.alerts a SET patient_id = kept, updated_at = now()
        WHERE a.hospital_id = hid AND a.patient_id = gone
          AND (a.status <> 'new' OR NOT EXISTS (SELECT 1 FROM public.alerts b
                                                WHERE b.hospital_id = hid AND b.patient_id = kept AND b.status = 'new'
                                                  AND b.rule_id = a.rule_id
                                                  AND coalesce(b.loinc, '') = coalesce(a.loinc, '')));
    UPDATE emr.patients p SET status = 'merged_into', merged_into = kept, updated_at = now()
        WHERE p.hospital_id = hid AND p.id = gone;
    UPDATE emr.possible_matches x SET decision = 'confirmed', decided_at = now()
        WHERE x.hospital_id = hid AND x.id = m.id;
    RETURN v_merge;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The exclusion withdraws the patient's open alerts at once (A34).
------------------------------------------------------------------------
DROP FUNCTION emr_exclude_patient(uuid, text);
-- 00023's exclusion, plus: every open alert of the patient (and of the
-- patients merged into it) is withdrawn (dismissed, by nobody, other,
-- withdrawn excluded), and every alert of theirs loses its patient, its
-- finding and its note; the alert keeps its codes and times for the
-- counts. alert_ids are the alerts withdrawn, for core's alert.withdrawn
-- rows.
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
    -- The emr service's link locks first (the order it takes them in:
    -- advisory, then the patient row), sorted, so a normaliser or sink
    -- transaction on this patient either finishes before the purge or
    -- waits and then sees the patient excluded. Links move with a merge,
    -- so the patient's own links cover the patients merged into it.
    PERFORM pg_advisory_xact_lock(hashtextextended('emr-link:' || k.connection_id::text || ':' || k.source_patient_id, 0))
    FROM (SELECT l.connection_id, l.source_patient_id FROM emr.patient_links l
          WHERE l.hospital_id = hid AND l.patient_id = pid ORDER BY 1, 2) k;
    PERFORM 1 FROM emr.patients p WHERE p.id = pid AND p.hospital_id = hid AND p.status IN ('active', 'excluded')
        FOR UPDATE;
    IF hid IS NULL OR NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_exclude_patient: no such patient';
    END IF;
    -- The patient and every patient merged into it, at any depth.
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
    RETURN QUERY SELECT n_o, n_m, n_e, n_r, n_x, wd;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_exclude_patient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_exclude_patient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_exclude_patient(uuid, text) TO mhai_gateway, mhai_emr_admin;

-- +goose Down

-- 00023's exclusion, as it was.
DROP FUNCTION emr_exclude_patient(uuid, text);
-- +goose StatementBegin
CREATE FUNCTION emr_exclude_patient(pid uuid, reason text)
    RETURNS TABLE (observations bigint, medications bigint, encounters bigint, raw_records bigint, matches bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid  uuid := public.mhai_current_hospital();
    ids  uuid[];
    raws uuid[];
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
    RETURN QUERY SELECT n_o, n_m, n_e, n_r, n_x;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_exclude_patient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_exclude_patient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_exclude_patient(uuid, text) TO mhai_gateway, mhai_emr_admin;

-- 00027's merge, as it was.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_merge_patients(match_id uuid, keep text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    hid      uuid := public.mhai_current_hospital();
    m        record;
    kept     uuid;
    gone     uuid;
    n        integer;
    v_merge  uuid := gen_random_uuid();
BEGIN
    IF hid IS NULL OR keep IS NULL OR keep NOT IN ('a', 'b') THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    SELECT x.id, x.patient_a, x.patient_b INTO m FROM emr.possible_matches x
    WHERE x.id = match_id AND x.hospital_id = hid AND x.decision IS NULL
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    IF keep = 'a' THEN kept := m.patient_a; gone := m.patient_b;
    ELSE kept := m.patient_b; gone := m.patient_a; END IF;
    PERFORM 1 FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid ORDER BY p.id FOR UPDATE;
    SELECT count(*) INTO n FROM emr.patients p WHERE p.id IN (kept, gone) AND p.hospital_id = hid AND p.status = 'active';
    IF n <> 2 THEN
        RAISE EXCEPTION USING ERRCODE = 'no_data_found', MESSAGE = 'emr_merge_patients: no such match';
    END IF;
    INSERT INTO emr.merges (id, hospital_id, match_id, kept_patient, merged_patient, merged_by)
    VALUES (v_merge, hid, m.id, kept, gone, NULLIF(current_setting('mhai.user_id', true), '')::uuid);
    UPDATE emr.patient_links l SET patient_id = kept, merge_id = v_merge
        WHERE l.hospital_id = hid AND l.patient_id = gone;
    UPDATE emr.encounters e SET patient_id = kept, merge_id = v_merge
        WHERE e.hospital_id = hid AND e.patient_id = gone;
    UPDATE emr.observations o SET patient_id = kept, merge_id = v_merge
        WHERE o.hospital_id = hid AND o.patient_id = gone;
    UPDATE emr.medications d SET patient_id = kept, merge_id = v_merge
        WHERE d.hospital_id = hid AND d.patient_id = gone;
    UPDATE emr.findings f SET patient_id = kept
        WHERE f.hospital_id = hid AND f.patient_id = gone;
    UPDATE emr.patients p SET status = 'merged_into', merged_into = kept, updated_at = now()
        WHERE p.hospital_id = hid AND p.id = gone;
    UPDATE emr.possible_matches x SET decision = 'confirmed', decided_at = now()
        WHERE x.hospital_id = hid AND x.id = m.id;
    RETURN v_merge;
END
$$;
-- +goose StatementEnd

DROP FUNCTION emr_reject_match(uuid);
DROP FUNCTION emr_alert_links(uuid[]);
DROP FUNCTION emr_patient_sources(uuid[]);
DROP FUNCTION emr_hospital_sources();
DROP FUNCTION alerts_requeue_removed(uuid[]);
DROP TRIGGER user_roles_requeue_alerts_update ON user_roles;
DROP TRIGGER user_roles_requeue_alerts ON user_roles;
DROP TRIGGER users_requeue_alerts ON users;
DROP FUNCTION mhai_alerts_requeue_role();
DROP FUNCTION mhai_alerts_requeue_user();
DROP FUNCTION mhai_alerts_requeue(uuid, uuid[], text);

-- 00012's drain policy and outbox checks. Alert facts not yet drained go
-- with the actions (FORCE holds the owner too, so it is lifted for the
-- delete and put back).
DROP POLICY drain_append ON audit_log;
CREATE POLICY drain_append ON audit_log FOR INSERT TO mhai_audit_drain
    WITH CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed')
                AND hospital_id IS NOT NULL);
DROP POLICY worker_no_alert_facts ON intelligence_audit_outbox;
ALTER TABLE intelligence_audit_outbox NO FORCE ROW LEVEL SECURITY;
DELETE FROM intelligence_audit_outbox WHERE action LIKE 'alert.%';
ALTER TABLE intelligence_audit_outbox FORCE ROW LEVEL SECURITY;
ALTER TABLE intelligence_audit_outbox
    DROP CONSTRAINT intelligence_audit_outbox_action_check,
    ADD CONSTRAINT intelligence_audit_outbox_action_check
        CHECK (action IN ('query.completed', 'query.failed', 'query.cancelled', 'correction.applied', 'correction.apply_failed')),
    DROP CONSTRAINT intelligence_audit_outbox_one_parent,
    ADD CONSTRAINT intelligence_audit_outbox_one_parent CHECK (num_nonnulls(query_id, job_id) = 1);

DROP INDEX emr.findings_unannounced;
CREATE INDEX findings_unannounced ON emr.findings (hospital_id, created_at)
    WHERE status <> 'historical' AND announced IS DISTINCT FROM status;

DROP POLICY gateway_read ON emr.possible_matches;
REVOKE SELECT ON emr.possible_matches FROM mhai_gateway;
DROP POLICY gateway_read ON emr.findings;
REVOKE SELECT ON emr.findings FROM mhai_gateway;

-- The tables take their policies, triggers, indexes and grants with them.
DROP TABLE user_notification_settings;
DROP TABLE alert_deliveries;
DROP TABLE alerts;
DROP TABLE alert_recipients;
DROP TABLE alert_levels;
DROP TABLE alert_units;
DROP TABLE alert_settings;
DROP FUNCTION mhai_alert_recipient_check();
