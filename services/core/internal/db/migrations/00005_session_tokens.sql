-- Per-session state for token rotation and revocation (Task 6).
--
-- access_jti / access_expires_at: the session's latest access token, so a
-- logout or a refresh-token reuse can put it on the denylist.
-- mfa / amr: how the session was authenticated, carried into every access
-- token minted by refresh.
-- mhai_auth and mhai_gateway already hold table-level privileges on
-- sessions, which cover the new columns.

-- +goose Up
ALTER TABLE sessions
    ADD COLUMN access_jti        uuid,
    ADD COLUMN access_expires_at timestamptz,
    ADD COLUMN mfa               boolean NOT NULL DEFAULT false,
    ADD COLUMN amr               text[]  NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE sessions
    DROP COLUMN amr,
    DROP COLUMN mfa,
    DROP COLUMN access_expires_at,
    DROP COLUMN access_jti;
