-- The notifications service's database role (notifications design §6,
-- §16.4; §15 PR 2; ADR 0018). Numbered 00052: main had 00050 when this was
-- pushed, and 00051 is held by the open documents PR 5 (#230).
--
-- Roles (cluster-wide: created if missing, never dropped; Down takes back
-- every grant and membership this file made):
--   mhai_notify      NOLOGIN: the notifications service inside
--                    dbx.WithHospital. It holds nothing in this migration
--                    but USAGE on the schema: no clinical table, no users,
--                    sessions or audit_log. Its own tables (deliveries, the
--                    send counters) and its claim come with PR 3, each with
--                    its grant, so no role holds a grant no code uses.
--   mhai_notify_app  NOLOGIN NOINHERIT: the service's login (notify_app,
--                    made by core migrate up through db.EnsureLogin) is a
--                    member of it only. It holds mhai_notify WITH INHERIT
--                    FALSE, so the login holds nothing until SET ROLE (as
--                    mhai_documents_app and mhai_emr_app).
--
-- Additive: no table changes shape.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_notify') THEN
        CREATE ROLE mhai_notify NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_notify_app') THEN
        CREATE ROLE mhai_notify_app NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER ROLE mhai_notify_app NOINHERIT;
GRANT mhai_notify TO mhai_notify_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_notify;

-- +goose Down
REVOKE USAGE ON SCHEMA public FROM mhai_notify;
REVOKE mhai_notify FROM mhai_notify_app;
