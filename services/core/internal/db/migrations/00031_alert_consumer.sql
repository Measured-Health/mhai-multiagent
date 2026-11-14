-- EMR integration, core's finding consumer (EMR design §11.4, §12.1, §12.4,
-- §12.5, §12.6, §15; plan Tasks 35-37, PR 8b). Numbered 00031: 00030 is
-- Analysis PR 6's. Nothing here raises an alert on its own: core's
-- consumer (CORE_EMR_ENABLED, off by default) writes the alerts, and with
-- no rule set activated for a hospital every finding is shadow and never
-- reaches it.
--
-- emr_consumed: what core's consumer has acted on, one row per finding and
-- status (or per signal), with the outcome code. A redelivered or
-- replayed message finds its row and changes nothing (A19), whichever
-- core copy takes it. Ids and codes only.
--
-- Alerts: a handled alert whose result the source withdraws gets the mark
-- (withdrawn) without reopening (design §11.4: "a handled alert is not
-- reopened, but gets the same amended or withdrawn mark"); 00029's checks
-- allowed withdrawn only on an alert dismissed by nobody. A new alert or
-- a new patient on an alert must name an active patient, checked under a
-- share lock on the patient row, so an alert can't be raised for a
-- patient an exclusion or a merge took away while the consumer was
-- reading (the consumer retries and reads the finding again).
--
-- alert_route_facts(pid): what routing needs that core's consumer role
-- can't read itself: whether the hospital is active with a connection
-- switched on, the patient's current unit, and the named recipients who
-- are still active clinicians of the hospital. alert_source_known(cid):
-- whether a connection a signal names is the hospital's.
--
-- Roles:
--   mhai_emr_core  emr_consumed (read, insert); appends its hospital's
--                  alert.raised, alert.updated, alert.not_raised,
--                  alert.withdrawn and emr.source_paused rows to the
--                  audit chain, with no actor; EXECUTE on the two
--                  functions above.

-- +goose Up

------------------------------------------------------------------------
-- What the consumer has acted on.
------------------------------------------------------------------------
CREATE TABLE emr_consumed (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    kind        text NOT NULL CHECK (kind IN ('finding', 'signal')),
    ref_id      uuid NOT NULL,
    status      text NOT NULL CHECK (status ~ '^[a-z_]{1,32}$'),
    outcome     text NOT NULL CHECK (outcome IN ('raised', 'updated', 'amended', 'withdrawn', 'not_raised', 'no_change',
                                                 'recorded')),
    reason      text CHECK (reason ~ '^[a-z_]{1,32}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hospital_id, kind, ref_id, status)
);
ALTER TABLE emr_consumed OWNER TO mhai_migrate;
ALTER TABLE emr_consumed ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT (hospital_id, kind, ref_id, status, outcome, reason, created_at),
      INSERT (hospital_id, kind, ref_id, status, outcome, reason) ON emr_consumed TO mhai_emr_core;
CREATE POLICY core_own ON emr_consumed TO mhai_emr_core
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

------------------------------------------------------------------------
-- A handled alert keeps the source's withdrawn mark.
------------------------------------------------------------------------
ALTER TABLE alerts
    DROP CONSTRAINT alerts_handled_by,
    -- Handled by a person, unless the source or an exclusion withdrew it.
    ADD CONSTRAINT alerts_handled_by CHECK (status = 'new' OR handled_by IS NOT NULL OR withdrawn IS NOT NULL),
    DROP CONSTRAINT alerts_withdrawn,
    -- Design §12.5: withdrawn while new reads as dismissed, by nobody,
    -- other; a handled alert keeps who handled it and gets the mark.
    ADD CONSTRAINT alerts_withdrawn CHECK (withdrawn IS NULL
                                           OR (status <> 'new'
                                               AND (handled_by IS NOT NULL
                                                    OR (status = 'dismissed' AND dismiss_reason = 'other'))));

------------------------------------------------------------------------
-- An alert names an active patient.
------------------------------------------------------------------------
-- The share lock waits for an exclusion or a merge holding the patient
-- row, then reads the row as committed: an alert for a patient excluded
-- or merged away meanwhile is refused (the consumer retries).
-- +goose StatementBegin
CREATE FUNCTION mhai_alert_patient_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    prev text := current_setting('mhai.hospital_id', true);
    ok   boolean;
BEGIN
    IF NEW.patient_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- The owner's policy on the patients is scoped to the hospital set:
    -- the alert's own, for this statement, then put back.
    PERFORM set_config('mhai.hospital_id', NEW.hospital_id::text, true);
    PERFORM 1 FROM emr.patients p
    WHERE p.hospital_id = NEW.hospital_id AND p.id = NEW.patient_id AND p.status = 'active'
    FOR SHARE;
    ok := FOUND;
    PERFORM set_config('mhai.hospital_id', coalesce(prev, ''), true);
    IF NOT ok THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'alert patient: not an active patient of the hospital';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_alert_patient_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_alert_patient_check() FROM PUBLIC;
CREATE TRIGGER alerts_patient_check BEFORE INSERT OR UPDATE OF patient_id ON alerts
    FOR EACH ROW EXECUTE FUNCTION mhai_alert_patient_check();

------------------------------------------------------------------------
-- What routing reads (definers, the caller's hospital only).
------------------------------------------------------------------------
-- The definer reads the hospital row as the owner, its own hospital only.
CREATE POLICY owner_alert_read ON hospitals FOR SELECT TO mhai_migrate USING (id = mhai_current_hospital());

-- Design §12.1 step 2 and §12.2: hospital is ok (active, with a connection
-- switched on), hospital_inactive or no_connection; unit_id the patient's
-- current encounter's unit (null: not recorded, covered); recipients the
-- named clinicians who are still active clinicians of the hospital,
-- sorted.
-- +goose StatementBegin
CREATE FUNCTION alert_route_facts(pid uuid)
    RETURNS TABLE (hospital text, unit_id uuid, recipients uuid[])
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM public.hospitals h
                                 WHERE h.id = public.mhai_current_hospital() AND h.status = 'active') THEN 'hospital_inactive'
                WHEN NOT EXISTS (SELECT 1 FROM public.emr_connections c
                                 WHERE c.hospital_id = public.mhai_current_hospital() AND c.enabled) THEN 'no_connection'
                ELSE 'ok' END,
           (SELECT e.unit_id FROM emr.encounters e
            WHERE e.hospital_id = public.mhai_current_hospital() AND e.patient_id = pid AND e.superseded_by IS NULL
              AND e.status IN ('arrived', 'in-progress')
            ORDER BY e.period_start DESC NULLS LAST, e.id LIMIT 1),
           coalesce((SELECT array_agg(r.user_id ORDER BY r.user_id) FROM public.alert_recipients r
                     JOIN public.users u ON u.id = r.user_id AND u.hospital_id = r.hospital_id AND u.is_active
                     WHERE r.hospital_id = public.mhai_current_hospital()
                       AND EXISTS (SELECT 1 FROM public.user_roles x WHERE x.user_id = u.id AND x.role = 'clinician')),
                    '{}')
    WHERE public.mhai_current_hospital() IS NOT NULL
$$;
-- +goose StatementEnd
ALTER FUNCTION alert_route_facts(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION alert_route_facts(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION alert_route_facts(uuid) TO mhai_emr_core;

-- Whether connection cid is the caller's hospital's.
-- +goose StatementBegin
CREATE FUNCTION alert_source_known(cid uuid) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT EXISTS (SELECT 1 FROM public.emr_connections c
                   WHERE c.id = cid AND c.hospital_id = public.mhai_current_hospital())
$$;
-- +goose StatementEnd
ALTER FUNCTION alert_source_known(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION alert_source_known(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION alert_source_known(uuid) TO mhai_emr_core;

------------------------------------------------------------------------
-- The consumer's audit rows: its hospital's, no actor, its actions only.
------------------------------------------------------------------------
GRANT INSERT ON audit_log TO mhai_emr_core;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_emr_core;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_emr_core;
CREATE POLICY emr_core_append ON audit_log FOR INSERT TO mhai_emr_core
    WITH CHECK (hospital_id = mhai_current_hospital() AND actor_id IS NULL
                AND action IN ('alert.raised', 'alert.updated', 'alert.not_raised', 'alert.withdrawn', 'emr.source_paused'));

-- +goose Down

DROP POLICY emr_core_append ON audit_log;
REVOKE EXECUTE ON FUNCTION audit_chain_head() FROM mhai_emr_core;
REVOKE USAGE ON SEQUENCE audit_log_id_seq FROM mhai_emr_core;
REVOKE INSERT ON audit_log FROM mhai_emr_core;

DROP FUNCTION alert_source_known(uuid);
DROP FUNCTION alert_route_facts(uuid);
DROP POLICY owner_alert_read ON hospitals;

DROP TRIGGER alerts_patient_check ON alerts;
DROP FUNCTION mhai_alert_patient_check();

-- 00029's checks. A handled alert's withdrawn mark goes (FORCE holds the
-- owner too, so it is lifted for the update and put back).
ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
UPDATE alerts SET withdrawn = NULL WHERE withdrawn IS NOT NULL AND handled_by IS NOT NULL;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE alerts
    DROP CONSTRAINT alerts_withdrawn,
    ADD CONSTRAINT alerts_withdrawn CHECK (withdrawn IS NULL OR (status = 'dismissed' AND dismiss_reason = 'other')),
    DROP CONSTRAINT alerts_handled_by,
    ADD CONSTRAINT alerts_handled_by CHECK (status = 'new' OR (handled_by IS NULL) = (withdrawn IS NOT NULL));

-- The table takes its policy and grants with it.
DROP TABLE emr_consumed;
