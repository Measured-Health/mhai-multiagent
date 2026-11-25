-- Deactivating and reactivating a hospital from the platform app (POST
-- /platform/hospitals/{id}/deactivate and /reactivate; mhai-web's
-- Deactivate and Reactivate buttons). Until now only a manual UPDATE could
-- deactivate a hospital (00010 left the CLI without a deactivate command on
-- purpose: it belongs to the platform).
--
-- hospital_deactivations keeps who deactivated a hospital, when, why (one of
-- mhai-web's four reasons, a note required for "other"), the status it had
-- (active or setup_incomplete, restored on reactivation) and, once
-- reactivated, who did it, when and why. A hospital has at most one open
-- record (not yet reactivated); closed ones are kept.
--
-- mhai_platform gains:
--   - hospitals: UPDATE (status). A BEFORE trigger holds its status writes
--     to the two moves: active or setup_incomplete -> deactivated, and
--     deactivated -> the open record's from_status. It can never activate a
--     hospital in set-up, start or finish a removal, or remove one.
--     00009/00012's triggers still cancel the hospital's queued and running
--     questions when it leaves active (they run as their definer).
--   - hospital_deactivations: SELECT; INSERT of an open record for a
--     hospital now deactivated, by a platform account; UPDATE closing the
--     open record, by a platform account, once the hospital has its
--     from_status back.
--   - sessions: UPDATE (revoked_at, already granted by 00037) of the open
--     sessions of a deactivated hospital's accounts, so its staff are
--     signed out in the deactivating transaction.
--   - audit_log: hospital.deactivated and hospital.reactivated with an
--     actor and no hospital (the hospital is in the payload, as
--     hospital.updated).

-- +goose Up

CREATE TABLE hospital_deactivations (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id         uuid NOT NULL REFERENCES hospitals (id),
    from_status         text NOT NULL CHECK (from_status IN ('active', 'setup_incomplete')),
    reason              text NOT NULL CHECK (reason IN ('contract_ended', 'security_concern', 'hospital_request', 'other')),
    note                text CHECK (note IS NULL OR char_length(note) BETWEEN 1 AND 500),
    deactivated_by      uuid NOT NULL REFERENCES users (id),
    deactivated_at      timestamptz NOT NULL DEFAULT now(),
    reactivated_by      uuid REFERENCES users (id),
    reactivated_at      timestamptz,
    reactivation_reason text CHECK (reactivation_reason IS NULL OR char_length(reactivation_reason) BETWEEN 1 AND 500),
    CHECK (reason <> 'other' OR note IS NOT NULL),
    CHECK ((reactivated_at IS NULL) = (reactivated_by IS NULL)
           AND (reactivated_at IS NULL) = (reactivation_reason IS NULL))
);
CREATE UNIQUE INDEX hospital_deactivations_open ON hospital_deactivations (hospital_id) WHERE reactivated_at IS NULL;
ALTER TABLE hospital_deactivations OWNER TO mhai_migrate;
ALTER TABLE hospital_deactivations ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

GRANT SELECT ON hospital_deactivations TO mhai_platform;
GRANT INSERT (hospital_id, from_status, reason, note, deactivated_by) ON hospital_deactivations TO mhai_platform;
GRANT UPDATE (reactivated_by, reactivated_at, reactivation_reason) ON hospital_deactivations TO mhai_platform;
CREATE POLICY platform_read ON hospital_deactivations FOR SELECT TO mhai_platform USING (true);
CREATE POLICY platform_deactivate ON hospital_deactivations FOR INSERT TO mhai_platform
    WITH CHECK (reactivated_at IS NULL
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.id = hospital_id AND h.status = 'deactivated')
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = deactivated_by AND u.hospital_id IS NULL));
CREATE POLICY platform_reactivate ON hospital_deactivations FOR UPDATE TO mhai_platform
    USING (reactivated_at IS NULL)
    WITH CHECK (reactivated_at IS NOT NULL
                AND EXISTS (SELECT 1 FROM hospitals h WHERE h.id = hospital_id AND h.status = from_status)
                AND EXISTS (SELECT 1 FROM users u WHERE u.id = reactivated_by AND u.hospital_id IS NULL));

-- +goose StatementBegin
CREATE FUNCTION mhai_platform_status_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF OLD.status IN ('active', 'setup_incomplete') AND NEW.status = 'deactivated' THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'deactivated' AND EXISTS (
            SELECT 1 FROM public.hospital_deactivations d
            WHERE d.hospital_id = NEW.id AND d.reactivated_at IS NULL AND d.from_status = NEW.status) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'hospital status change refused for the platform role' USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_platform_status_guard() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_platform_status_guard() FROM PUBLIC;
CREATE TRIGGER hospitals_platform_status BEFORE UPDATE OF status ON hospitals
    FOR EACH ROW WHEN (current_user = 'mhai_platform')
    EXECUTE FUNCTION mhai_platform_status_guard();
GRANT UPDATE (status) ON hospitals TO mhai_platform;

CREATE POLICY platform_hospital_deactivate_revoke ON sessions FOR UPDATE TO mhai_platform
    USING (revoked_at IS NULL AND EXISTS (SELECT 1 FROM users u JOIN hospitals h ON h.id = u.hospital_id
                                          WHERE u.id = user_id AND h.status = 'deactivated'))
    WITH CHECK (revoked_at IS NOT NULL);

CREATE POLICY platform_hospital_status_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('hospital.deactivated', 'hospital.reactivated') AND hospital_id IS NULL AND actor_id IS NOT NULL);

-- +goose Down

DROP POLICY platform_hospital_status_append ON audit_log;
DROP POLICY platform_hospital_deactivate_revoke ON sessions;
REVOKE UPDATE (status) ON hospitals FROM mhai_platform;
DROP TRIGGER hospitals_platform_status ON hospitals;
DROP FUNCTION mhai_platform_status_guard();
DROP TABLE hospital_deactivations;
