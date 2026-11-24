-- Moving two-step to a new authenticator app (POST /auth/mfa/replace and
-- /auth/mfa/replace/confirm; product owner decision 24 Nov 2026).
--
-- A signed-in account with two-step on proves its current factor (a step-up
-- code), and core makes a new TOTP secret. It is kept beside the current
-- one, sealed exactly as secret_enc is (AES-256-GCM, bound to the user id),
-- until a code from the new app confirms it within 15 minutes; then it
-- becomes secret_enc. Until then the current secret keeps working.
--
--   - user_mfa gains pending_secret_enc (sealed) and pending_created_at,
--     both NULL or both set.
--   - mhai_auth may UPDATE the two columns (it already has INSERT and
--     column-level UPDATE on the others, 00003). No other role gains
--     anything: mhai_gateway's table-wide grants and its hospital policy
--     (00003) already cover every column, as they do secret_enc; the rule
--     roles' column grants (user_id, enabled) don't reach these.
--
-- Down revokes the grant and drops the columns (a pending move is lost;
-- the current secret is untouched).

-- +goose Up

ALTER TABLE user_mfa
    ADD COLUMN pending_secret_enc bytea,
    ADD COLUMN pending_created_at timestamptz,
    ADD CONSTRAINT user_mfa_pending_pair CHECK ((pending_secret_enc IS NULL) = (pending_created_at IS NULL));

GRANT UPDATE (pending_secret_enc, pending_created_at) ON user_mfa TO mhai_auth;

-- +goose Down

REVOKE UPDATE (pending_secret_enc, pending_created_at) ON user_mfa FROM mhai_auth;

ALTER TABLE user_mfa
    DROP CONSTRAINT user_mfa_pending_pair,
    DROP COLUMN pending_created_at,
    DROP COLUMN pending_secret_enc;
