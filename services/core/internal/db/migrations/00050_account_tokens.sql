-- Invites and password resets by a hospital admin (frontend requirements
-- §2.5, §5.1; TODO.md "Hospital admin routes: gaps"). Additive only.
-- Numbered 00050: it follows 00049 (the admin actions), which main did
-- not have yet when this was pushed.
--
-- What it adds:
--   - users.invited_at: an account a hospital admin invited. It has no
--     password until the person redeems the invite's link and chooses
--     one (the staff routes show it as `invited` until then).
--   - account_tokens: the single-use links. Only the token's SHA-256 is
--     stored (32 random bytes, so an unsalted hash is enough, as refresh
--     tokens); a token is live until it is used, replaced (revoked) or
--     expires (7 days for an invite, 30 minutes for a reset), and a person
--     has at most one live token per purpose. Nothing else is stored: no
--     link, no email.
--
-- Who touches account_tokens:
--   - mhai_gateway (the hospital admin's routes): its own hospital's rows;
--     inserts a token, revokes one (revoked_at), and reads them.
--   - mhai_auth (POST /auth/password/reset, public: the person isn't
--     signed in, so no hospital is known before the token is): reads by
--     hash across hospitals and marks a token used or revoked; nothing
--     else.

-- +goose Up
ALTER TABLE users ADD COLUMN invited_at timestamptz;

CREATE TABLE account_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL REFERENCES users (id),
    purpose     text NOT NULL CHECK (purpose IN ('invite', 'password_reset')),
    token_hash  text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    revoked_at  timestamptz,
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '7 days'),
    CHECK (used_at IS NULL OR revoked_at IS NULL)
);
-- At most one live token per person and purpose.
CREATE UNIQUE INDEX account_tokens_live ON account_tokens (user_id, purpose) WHERE used_at IS NULL AND revoked_at IS NULL;
CREATE INDEX account_tokens_hospital_idx ON account_tokens (hospital_id, created_at);
ALTER TABLE account_tokens OWNER TO mhai_migrate;
ALTER TABLE account_tokens ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

GRANT SELECT, INSERT ON account_tokens TO mhai_gateway;
GRANT UPDATE (revoked_at) ON account_tokens TO mhai_gateway;
CREATE POLICY gateway_hospital ON account_tokens TO mhai_gateway
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

GRANT SELECT (id, hospital_id, user_id, purpose, token_hash, expires_at, used_at, revoked_at) ON account_tokens TO mhai_auth;
GRANT UPDATE (used_at, revoked_at) ON account_tokens TO mhai_auth;
CREATE POLICY auth_redeem ON account_tokens FOR SELECT TO mhai_auth USING (true);
CREATE POLICY auth_redeem_update ON account_tokens FOR UPDATE TO mhai_auth USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE account_tokens;
ALTER TABLE users DROP COLUMN invited_at;
