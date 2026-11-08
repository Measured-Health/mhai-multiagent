-- EMR integration, connections (EMR design §3.3, §6.1, §6.3, §6.4, §18 item
-- 1; plan Task 6). Numbered 00020 after Plan 7c-1's 00019 (ruling M1); later
-- migrations take the numbers after it.
--
-- Roles (cluster-wide: created if missing, never dropped; Down takes back
-- every grant and membership this file made):
--   mhai_emr        NOLOGIN: the emr service inside dbx.WithHospital.
--   mhai_emr_app    NOLOGIN NOINHERIT: the emr service's login (emr_app,
--                   made by core migrate up through db.EnsureLogin) is a
--                   member of it only. It holds mhai_emr and mhai_emr_sched
--                   WITH INHERIT FALSE, so the login holds nothing until SET
--                   ROLE, as intelligence_app (00008).
--   mhai_emr_sched  NOLOGIN: the scheduler's read of enabled connections'
--                   ids across hospitals (as mhai_jobs for the sweeper).
--   mhai_emr_core   NOLOGIN: core's finding consumer (grants in a later
--                   migration); core's login reaches it by SET ROLE only.
--   mhai_emr_admin  NOLOGIN: operator functions (later migrations).
--
-- emr_connections (public): configuration only. No column holds a secret,
-- and settings may not name one (a CHECK on the key names). The platform
-- (mhai_platform) reads and writes it; the emr service reads its own
-- hospital's rows under RLS; core's clinical role holds nothing on it.
-- credentials_origin is the set of scheme://host:port the credentials were
-- entered for (§6.3), sorted. While a connection is on, its origin can't
-- change: core wipes the credentials and switches it off in the same
-- request (a trigger refuses anything else).
--
-- emr.connection_ranges: the declared private ranges of every connection,
-- kept equal to emr_connections.private_cidrs by a definer trigger, with an
-- exclusion constraint so two hospitals' ranges never overlap (A37). Each
-- range is a private IPv4 network (RFC 1918) of at most a /24.
--
-- emr.credentials: metadata only (field, set_at, secret_version, set_by,
-- origin): never the value, a hash of it, or its length (§6.1). The values
-- live in the emr service's secret store. emr_credentials_meta() answers
-- the platform the fields set for the connection's current origin only.
--
-- emr.sync_runs, emr.connection_errors: codes and counts (closed codes).
-- The platform reads them through emr_platform_health() and
-- emr_platform_errors(), definer functions that return codes and counts.
--
-- Every table: ENABLE and FORCE row-level security, owner mhai_migrate.
-- Every definer function: owner mhai_migrate, search_path pinned to
-- pg_catalog, pg_temp, every relation schema-qualified, no PUBLIC execute.
--
-- The platform's audit append policy (00018's list) gains the connection
-- actions and emr.source_resumed, with its condition kept.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_emr', 'mhai_emr_sched', 'mhai_emr_core', 'mhai_emr_admin'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION', r);
        END IF;
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_emr_app') THEN
        CREATE ROLE mhai_emr_app NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER ROLE mhai_emr_app NOINHERIT;
GRANT mhai_emr, mhai_emr_sched TO mhai_emr_app WITH INHERIT FALSE;
GRANT mhai_emr_core TO mhai_app WITH INHERIT FALSE;

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE SCHEMA emr AUTHORIZATION mhai_migrate;
REVOKE ALL ON SCHEMA emr FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO mhai_emr, mhai_emr_sched;
GRANT USAGE ON SCHEMA emr TO mhai_emr;

------------------------------------------------------------------------
-- The connections.
------------------------------------------------------------------------
CREATE TABLE emr_connections (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id        uuid NOT NULL REFERENCES hospitals (id),
    type               text NOT NULL CHECK (type IN ('fhir_r4', 'document_archive', 'file_export', 'stub')),
    source_label       text NOT NULL CHECK (char_length(source_label) BETWEEN 1 AND 40),
    base_url           text NOT NULL CHECK (char_length(base_url) BETWEEN 1 AND 500),
    settings           jsonb NOT NULL DEFAULT '{}'::jsonb,
    credentials_origin text[] NOT NULL DEFAULT '{}',
    enabled            boolean NOT NULL DEFAULT false,
    paused             boolean NOT NULL DEFAULT false,
    private_network    boolean NOT NULL DEFAULT false,
    private_cidrs      cidr[] NOT NULL DEFAULT '{}',
    poll_seconds       integer NOT NULL DEFAULT 300 CHECK (poll_seconds >= 60 AND poll_seconds <= 86400),
    last_test_at       timestamptz,
    last_test_ok       boolean,
    last_test_code     text CHECK (last_test_code IN ('ok', 'unreachable', 'sign_in_refused', 'timed_out',
                                                      'unreadable_data', 'unmapped_codes', 'too_many_requests')),
    version            integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT emr_connections_id_hospital UNIQUE (id, hospital_id),
    CONSTRAINT emr_connections_settings_object CHECK (jsonb_typeof(settings) = 'object'),
    CONSTRAINT emr_connections_no_secret_settings CHECK (NOT (settings ?| ARRAY['api_key', 'private_key', 'shared_key',
        'access_key', 'password', 'secret', 'token', 'client_secret'])),
    CONSTRAINT emr_connections_ranges_private CHECK (cardinality(private_cidrs) = 0 OR private_network),
    CONSTRAINT emr_connections_test CHECK ((last_test_at IS NULL) = (last_test_ok IS NULL)
                                           AND (last_test_at IS NULL) = (last_test_code IS NULL)),
    CONSTRAINT emr_connections_origin_short CHECK (cardinality(credentials_origin) <= 4)
);
ALTER TABLE emr_connections OWNER TO mhai_migrate;
ALTER TABLE emr_connections ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX emr_connections_hospital ON emr_connections (hospital_id);
-- The label is how the hospital tells its sources apart (design §16.1), so
-- one hospital has each label once, in any case: a create retried without
-- its Idempotency-Key can't add a second connection (ruling E1).
CREATE UNIQUE INDEX emr_connections_label ON emr_connections (hospital_id, lower(source_label));

-- The platform app: every hospital's connections; configuration columns
-- only on UPDATE (never the id, the hospital or created_at).
GRANT SELECT, INSERT ON emr_connections TO mhai_platform;
GRANT UPDATE (type, source_label, base_url, settings, credentials_origin, enabled, paused, private_network,
              private_cidrs, poll_seconds, last_test_at, last_test_ok, last_test_code, version, updated_by, updated_at)
    ON emr_connections TO mhai_platform;
CREATE POLICY platform_all ON emr_connections TO mhai_platform USING (true) WITH CHECK (true);

-- The emr service: its hospital's connections, read only, the columns it
-- needs to test and fetch.
GRANT SELECT (id, hospital_id, type, base_url, settings, enabled, paused, version, poll_seconds,
              credentials_origin, private_cidrs) ON emr_connections TO mhai_emr;
CREATE POLICY emr_own ON emr_connections FOR SELECT TO mhai_emr USING (hospital_id = mhai_current_hospital());

-- The scheduler: which enabled connections to poll, across hospitals.
GRANT SELECT (id, hospital_id, enabled, paused, poll_seconds) ON emr_connections TO mhai_emr_sched;
CREATE POLICY sched_enabled ON emr_connections FOR SELECT TO mhai_emr_sched USING (enabled);

-- The definer functions below read as the owner.
CREATE POLICY owner_read ON emr_connections FOR SELECT TO mhai_migrate USING (true);

-- The emr service reads its own hospital's id and status.
GRANT SELECT (id, status) ON hospitals TO mhai_emr;
CREATE POLICY emr_read ON hospitals FOR SELECT TO mhai_emr USING (id = mhai_current_hospital());

-- An enabled connection's origin never changes: the change switches it off
-- (core wipes the credentials first, design §6.3).
-- +goose StatementBegin
CREATE FUNCTION emr_origin_guard() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.credentials_origin IS DISTINCT FROM OLD.credentials_origin AND NEW.enabled THEN
        RAISE EXCEPTION USING ERRCODE = 'raise_exception',
            MESSAGE = 'emr_connections: switch the connection off to change its origin';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_origin_guard() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_origin_guard() FROM PUBLIC;
CREATE TRIGGER emr_connections_origin_guard BEFORE UPDATE ON emr_connections
    FOR EACH ROW EXECUTE FUNCTION emr_origin_guard();

------------------------------------------------------------------------
-- Declared private ranges, never overlapping across hospitals (A37).
------------------------------------------------------------------------
CREATE TABLE emr.connection_ranges (
    connection_id uuid NOT NULL,
    hospital_id   uuid NOT NULL,
    cidr          cidr NOT NULL,
    PRIMARY KEY (connection_id, cidr),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id) ON DELETE CASCADE,
    CONSTRAINT connection_ranges_private CHECK (family(cidr) = 4 AND masklen(cidr) >= 24
        AND (cidr <<= '10.0.0.0/8'::cidr OR cidr <<= '172.16.0.0/12'::cidr OR cidr <<= '192.168.0.0/16'::cidr)),
    CONSTRAINT connection_ranges_no_overlap EXCLUDE USING gist (cidr inet_ops WITH &&, hospital_id WITH <>)
);
ALTER TABLE emr.connection_ranges OWNER TO mhai_migrate;
ALTER TABLE emr.connection_ranges ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON emr.connection_ranges TO mhai_migrate USING (true) WITH CHECK (true);

-- +goose StatementBegin
CREATE FUNCTION emr_sync_ranges() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    DELETE FROM emr.connection_ranges r WHERE r.connection_id = NEW.id;
    INSERT INTO emr.connection_ranges (connection_id, hospital_id, cidr)
    SELECT DISTINCT NEW.id, NEW.hospital_id, c FROM unnest(NEW.private_cidrs) AS c;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_sync_ranges() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_sync_ranges() FROM PUBLIC;
CREATE TRIGGER emr_connections_ranges AFTER INSERT OR UPDATE OF private_cidrs, hospital_id ON emr_connections
    FOR EACH ROW EXECUTE FUNCTION emr_sync_ranges();

------------------------------------------------------------------------
-- Credential metadata: never a value.
------------------------------------------------------------------------
CREATE TABLE emr.credentials (
    connection_id  uuid NOT NULL,
    hospital_id    uuid NOT NULL,
    field          text NOT NULL CHECK (field ~ '^[a-z_]{1,40}$'),
    set_at         timestamptz NOT NULL,
    secret_version text NOT NULL CHECK (secret_version ~ '^[A-Za-z0-9._-]{1,64}$'),
    set_by         uuid,
    origin         text[] NOT NULL CHECK (cardinality(origin) BETWEEN 1 AND 4),
    PRIMARY KEY (connection_id, field),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.credentials OWNER TO mhai_migrate;
ALTER TABLE emr.credentials ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON emr.credentials TO mhai_emr;
CREATE POLICY emr_own ON emr.credentials TO mhai_emr
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY owner_read ON emr.credentials FOR SELECT TO mhai_migrate USING (true);

------------------------------------------------------------------------
-- Sync runs and connection errors: codes and counts.
------------------------------------------------------------------------
CREATE TABLE emr.sync_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id   uuid NOT NULL,
    connection_id uuid NOT NULL,
    started_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    ok            boolean,
    records       integer NOT NULL DEFAULT 0 CHECK (records >= 0),
    outcome       text CHECK (outcome IN ('ok', 'unreachable', 'sign_in_refused', 'timed_out', 'unreadable_data',
                                          'unmapped_codes', 'too_many_requests')),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.sync_runs OWNER TO mhai_migrate;
ALTER TABLE emr.sync_runs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX sync_runs_connection ON emr.sync_runs (connection_id, started_at DESC);
GRANT SELECT, INSERT ON emr.sync_runs TO mhai_emr;
GRANT UPDATE (finished_at, ok, records, outcome) ON emr.sync_runs TO mhai_emr;
CREATE POLICY emr_own ON emr.sync_runs TO mhai_emr
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY owner_read ON emr.sync_runs FOR SELECT TO mhai_migrate USING (true);

CREATE TABLE emr.connection_errors (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    hospital_id   uuid NOT NULL,
    connection_id uuid NOT NULL,
    at            timestamptz NOT NULL DEFAULT now(),
    code          text NOT NULL CHECK (code IN ('unreachable', 'sign_in_refused', 'timed_out', 'unreadable_data',
                                                'unmapped_codes', 'too_many_requests')),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.connection_errors OWNER TO mhai_migrate;
ALTER TABLE emr.connection_errors ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
CREATE INDEX connection_errors_connection ON emr.connection_errors (connection_id, at DESC);
GRANT INSERT ON emr.connection_errors TO mhai_emr;
CREATE POLICY emr_append ON emr.connection_errors FOR INSERT TO mhai_emr
    WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY owner_read ON emr.connection_errors FOR SELECT TO mhai_migrate USING (true);

------------------------------------------------------------------------
-- The platform's reads: codes, counts and set times only.
------------------------------------------------------------------------

-- One row per connection: its health (never: no finished run; errors: any
-- of the last three finished runs failed; ok otherwise), its last finished
-- run, and its errors in the last 24 hours.
-- +goose StatementBegin
CREATE FUNCTION emr_platform_health()
    RETURNS TABLE (connection_id uuid, health text, last_sync_at timestamptz, last_sync_ok boolean,
                   last_sync_records integer, errors_24h bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id,
           CASE WHEN l.at IS NULL THEN 'never'
                WHEN EXISTS (SELECT 1 FROM (SELECT r.ok FROM emr.sync_runs r
                                            WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                                            ORDER BY r.finished_at DESC LIMIT 3) t
                             WHERE t.ok IS NOT TRUE) THEN 'errors'
                ELSE 'ok' END,
           l.at, l.ok, l.records,
           (SELECT count(*) FROM emr.connection_errors e
            WHERE e.connection_id = c.id AND e.at > now() - interval '24 hours')
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT r.finished_at AS at, r.ok, r.records FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                       ORDER BY r.finished_at DESC LIMIT 1) l ON true
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_platform_health() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_platform_health() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_platform_health() TO mhai_platform;

-- A connection's errors in the last 24 hours, by code: the latest time and
-- the count, newest first, at most 5.
-- +goose StatementBegin
CREATE FUNCTION emr_platform_errors(p_connection uuid) RETURNS TABLE (at timestamptz, code text, count bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT max(e.at), e.code, count(*) FROM emr.connection_errors e
    WHERE e.connection_id = p_connection AND e.at > now() - interval '24 hours'
    GROUP BY e.code ORDER BY max(e.at) DESC LIMIT 5
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_platform_errors(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_platform_errors(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_platform_errors(uuid) TO mhai_platform;

-- The fields set for a connection's current origin, and when. A row set for
-- another origin reads as not set (design §6.3).
-- +goose StatementBegin
CREATE FUNCTION emr_credentials_meta(p_connection uuid) RETURNS TABLE (field text, set_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT k.field, k.set_at FROM emr.credentials k
    JOIN public.emr_connections c ON c.id = k.connection_id AND c.hospital_id = k.hospital_id
    WHERE k.connection_id = p_connection
      AND (SELECT array_agg(o ORDER BY o) FROM unnest(k.origin) o)
          = (SELECT array_agg(o ORDER BY o) FROM unnest(c.credentials_origin) o)
    ORDER BY k.field
$$;
-- +goose StatementEnd
ALTER FUNCTION emr_credentials_meta(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION emr_credentials_meta(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION emr_credentials_meta(uuid) TO mhai_platform;

------------------------------------------------------------------------
-- The platform's audit appends: the connection actions, conditions kept.
------------------------------------------------------------------------
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed')
                AND hospital_id IS NULL);

-- +goose Down

-- 00018's append, as it was.
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed')
                AND hospital_id IS NULL);

DROP FUNCTION emr_credentials_meta(uuid);
DROP FUNCTION emr_platform_errors(uuid);
DROP FUNCTION emr_platform_health();
DROP TABLE emr.connection_errors;
DROP TABLE emr.sync_runs;
DROP TABLE emr.credentials;
DROP TRIGGER emr_connections_ranges ON emr_connections;
DROP FUNCTION emr_sync_ranges();
DROP TABLE emr.connection_ranges;
DROP TRIGGER emr_connections_origin_guard ON emr_connections;
DROP FUNCTION emr_origin_guard();
DROP POLICY emr_read ON hospitals;
REVOKE SELECT (id, status) ON hospitals FROM mhai_emr;
-- The table takes its policies and grants with it.
DROP TABLE emr_connections;
DROP SCHEMA emr;
REVOKE USAGE ON SCHEMA public FROM mhai_emr, mhai_emr_sched;
REVOKE mhai_emr_core FROM mhai_app;
REVOKE mhai_emr, mhai_emr_sched FROM mhai_emr_app;
-- btree_gist stays: an extension may be used by objects outside this file.
