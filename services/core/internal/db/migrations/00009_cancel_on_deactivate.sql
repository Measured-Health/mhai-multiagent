-- Cancel a deactivated user's or hospital's questions in the database
-- (spec §8.3, §9.4; plan departure 11), whatever path made the change: the
-- hospital admin's route, the platform app, the CLI or a manual UPDATE.
--
-- Two AFTER UPDATE triggers, one on users (is_active true -> false) and one
-- on hospitals (status away from active), run SECURITY DEFINER functions
-- owned by mhai_migrate with search_path pinned. Each sets that user's (as
-- creator) or that hospital's queued and running questions to cancelled with
-- a cancel_reason, and adds a query.cancelled fact to
-- intelligence_audit_outbox for each, in the same transaction as the
-- deactivation; core's drainer puts the fact on the chain. A running job
-- stops at the worker's next checkpoint: its UPDATEs need status queued or
-- running (00008 worker_status), so they match no row.
--
-- FORCE ROW LEVEL SECURITY holds the owner too, so mhai_migrate gets
-- policies scoped to exactly this (F4; not USING (true), which would also
-- widen 00007's mhai_fail_expired_queries): it may see and update only
-- queued or running rows whose creator is inactive or whose hospital isn't
-- active, and may set only cancelled with one of the two reasons. It may
-- read only hospitals that aren't active. The outbox append policy is
-- 00008's owner_append. 00003's pairing_check already lets it read users.
--
-- Also:
--   - mhai_provision may append intelligence.key_rotated (core keys untrust
--     and the second request key, spec §7.5, §10.2), still with no actor.
--   - clinical_queries.retried_kid: the kid a dead-lettered job was last
--     re-minted for after an emergency rotation (intel.RunDeadLetters), so a
--     replayed letter can't re-mint it again. mhai_jobs reads and sets it.
--   - mhai_jobs's audit rows are tied to a job of the row's own hospital
--     (Plan 3 T5 review): actor none, entity query, entity_id the id of a
--     clinical_queries row whose hospital is the audit row's hospital.

--   - Final review I-3: the deactivation race. A question whose row
--     commits after the hospital's trigger ran (the submit's FOR SHARE locks
--     the user, not the hospital) is never cancelled by it. Core re-checks
--     before it publishes, and the worker may not move a row of a hospital
--     that isn't active: worker_hospital_active, RESTRICTIVE on top of
--     00008's worker_status, refuses the claim, every checkpoint and the
--     completion. For it mhai_worker reads hospitals (id, status), its own
--     hospital's row only. That is narrower than a SECURITY DEFINER helper
--     (no new privileged code running as the owner, whose FORCE RLS
--     policies on hospitals would need widening) and than FOR SHARE OF h at
--     submit (row locks need UPDATE on hospitals for mhai_gateway).

-- +goose Up

ALTER TABLE clinical_queries
    ADD COLUMN retried_kid text,
    ADD CONSTRAINT clinical_queries_retried_kid_check
        CHECK (retried_kid IS NULL OR retried_kid ~ '^[A-Za-z0-9_.:-]{1,64}$');
GRANT SELECT (retried_kid), UPDATE (retried_kid) ON clinical_queries TO mhai_jobs;

CREATE POLICY cancel_check ON hospitals FOR SELECT TO mhai_migrate
    USING (status <> 'active');

CREATE POLICY cancel_read ON clinical_queries FOR SELECT TO mhai_migrate
    USING ((status IN ('queued', 'running')
            OR (status = 'cancelled' AND cancel_reason IN ('user_deactivated', 'hospital_deactivated')))
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = clinical_queries.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = clinical_queries.hospital_id AND h.status <> 'active')));
CREATE POLICY cancel_update ON clinical_queries FOR UPDATE TO mhai_migrate
    USING (status IN ('queued', 'running')
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = clinical_queries.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = clinical_queries.hospital_id AND h.status <> 'active')))
    WITH CHECK (status = 'cancelled' AND cancel_reason IN ('user_deactivated', 'hospital_deactivated')
           AND (EXISTS (SELECT 1 FROM users u WHERE u.id = clinical_queries.created_by AND NOT u.is_active)
                OR EXISTS (SELECT 1 FROM hospitals h WHERE h.id = clinical_queries.hospital_id AND h.status <> 'active')));

-- +goose StatementBegin
CREATE FUNCTION mhai_cancel_user_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'user_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.created_by = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'user_deactivated')
    FROM gone g;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_cancel_hospital_queries() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    WITH gone AS (
        UPDATE public.clinical_queries q
        SET status = 'cancelled', cancel_reason = 'hospital_deactivated', completed_at = now(), lease_until = NULL
        WHERE q.hospital_id = NEW.id AND q.status IN ('queued', 'running')
        RETURNING q.id, q.hospital_id
    )
    INSERT INTO public.intelligence_audit_outbox (hospital_id, query_id, action, entity, entity_id, payload)
    SELECT g.hospital_id, g.id, 'query.cancelled', 'query', g.id::text,
           jsonb_build_object('query_id', g.id::text, 'reason', 'hospital_deactivated')
    FROM gone g;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cancel_user_queries() OWNER TO mhai_migrate;
ALTER FUNCTION mhai_cancel_hospital_queries() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cancel_user_queries(), mhai_cancel_hospital_queries() FROM PUBLIC;

CREATE TRIGGER users_cancel_queries AFTER UPDATE OF is_active ON users
    FOR EACH ROW WHEN (OLD.is_active AND NOT NEW.is_active)
    EXECUTE FUNCTION mhai_cancel_user_queries();
CREATE TRIGGER hospitals_cancel_queries AFTER UPDATE OF status ON hospitals
    FOR EACH ROW WHEN (OLD.status = 'active' AND NEW.status <> 'active')
    EXECUTE FUNCTION mhai_cancel_hospital_queries();

-- The key rotation audit (core keys).
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated') AND actor_id IS NULL);

-- mhai_jobs writes only about a job of the row's own hospital. RESTRICTIVE,
-- so it holds on top of 00007's jobs_append (the action list). The CASE
-- keeps a non-uuid entity_id from failing the cast: it just doesn't match.
CREATE POLICY jobs_append_own_job ON audit_log AS RESTRICTIVE FOR INSERT TO mhai_jobs
    WITH CHECK (actor_id IS NULL AND entity = 'query'
                AND EXISTS (SELECT 1 FROM clinical_queries q
                            WHERE q.id = CASE WHEN audit_log.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                              THEN audit_log.entity_id::uuid END
                              AND q.hospital_id = audit_log.hospital_id));

-- The worker moves only rows of an active hospital (final review I-3).
GRANT SELECT (id, status) ON hospitals TO mhai_worker;
CREATE POLICY worker_own ON hospitals FOR SELECT TO mhai_worker
    USING (id = mhai_current_hospital());
CREATE POLICY worker_hospital_active ON clinical_queries AS RESTRICTIVE FOR UPDATE TO mhai_worker
    USING (EXISTS (SELECT 1 FROM hospitals h WHERE h.id = clinical_queries.hospital_id AND h.status = 'active'));

-- +goose Down
DROP POLICY worker_hospital_active ON clinical_queries;
DROP POLICY worker_own ON hospitals;
REVOKE SELECT (id, status) ON hospitals FROM mhai_worker;
DROP POLICY jobs_append_own_job ON audit_log;
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created') AND actor_id IS NULL);
DROP TRIGGER hospitals_cancel_queries ON hospitals;
DROP TRIGGER users_cancel_queries ON users;
DROP FUNCTION mhai_cancel_hospital_queries();
DROP FUNCTION mhai_cancel_user_queries();
DROP POLICY cancel_update ON clinical_queries;
DROP POLICY cancel_read ON clinical_queries;
DROP POLICY cancel_check ON hospitals;
REVOKE SELECT (retried_kid), UPDATE (retried_kid) ON clinical_queries FROM mhai_jobs;
ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_retried_kid_check,
    DROP COLUMN retried_kid;
