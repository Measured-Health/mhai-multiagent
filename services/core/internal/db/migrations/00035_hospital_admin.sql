-- The hospital admin routes (frontend requirements §5; the staff-endpoints
-- work, which takes Analysis plan Task 17). Additive only.
--
-- What it adds:
--   - users.staff_version: the version a hospital admin's staff edit is
--     made at (409 conflict when stale). Only the staff routes move it;
--     sign-in and the other writers leave it alone.
--   - hospital_settings.version, updated_by, updated_at: the version a
--     settings edit is made at, and who made the last one.
--
-- No grant changes: mhai_gateway already holds table-level SELECT and
-- UPDATE on both tables under its hospital policies (00003), and the
-- column grants of mhai_auth, mhai_platform, mhai_provision and the
-- readers name their columns, so they gain nothing.

-- +goose Up
ALTER TABLE users
    ADD COLUMN staff_version integer NOT NULL DEFAULT 1 CHECK (staff_version > 0);

ALTER TABLE hospital_settings
    ADD COLUMN version    integer NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN updated_by uuid REFERENCES users (id),
    ADD COLUMN updated_at timestamptz;

-- +goose Down
ALTER TABLE hospital_settings
    DROP COLUMN updated_at,
    DROP COLUMN updated_by,
    DROP COLUMN version;

ALTER TABLE users
    DROP COLUMN staff_version;
