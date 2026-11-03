-- core hospitals activate: the provisioning role moves a hospital from
-- setup_incomplete to active, and nothing else.
--
-- A hospital made with "core hospitals create" starts setup_incomplete
-- (00004's provision_insert), and POST /queries needs an active one. Until
-- now nothing in core could activate a hospital; an operator ran an UPDATE
-- as the owner.
--
-- mhai_provision gets the narrowest grant that does it:
--   - UPDATE (status) on hospitals, held by provision_activate to exactly
--     setup_incomplete -> active. It cannot deactivate, reactivate, or touch
--     a hospital in removal: reactivating a deactivated hospital is the
--     platform app's, with a reason and two-step (frontend §14.2), and the
--     removal states never go back to active. Since the new status is
--     always active, 00009's hospitals_cancel_queries (status away from
--     active) can never fire from here.
--   - SELECT (status), so the CLI can say why it refused, or that the
--     hospital was already active. Status is not sensitive; name and the
--     settings stay unreadable.
--   - hospital.activated on the audit chain, still with no actor.
--
-- No deactivate command: a deactivation cancels the hospital's questions
-- (00009) and, per the contract, takes a reason, signs every member of
-- staff out and is reversed only through the platform app. That is not a
-- one-column CLI change.

-- +goose Up

GRANT SELECT (status), UPDATE (status) ON hospitals TO mhai_provision;
CREATE POLICY provision_activate ON hospitals FOR UPDATE TO mhai_provision
    USING (status = 'setup_incomplete')
    WITH CHECK (status = 'active');

DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated')
                AND actor_id IS NULL);

-- +goose Down
DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated') AND actor_id IS NULL);
DROP POLICY provision_activate ON hospitals;
REVOKE SELECT (status), UPDATE (status) ON hospitals FROM mhai_provision;
