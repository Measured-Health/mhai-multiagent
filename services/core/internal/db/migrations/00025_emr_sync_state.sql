-- EMR integration, the scheduler's state (EMR design §5.1, §7.1, §15.2,
-- §15.4; plan Tasks 23-24). Numbered 00025: 00024 is held by Analysis PR 4,
-- which may merge first or after. goose refuses a lower version once a
-- higher one is applied, so this file merges after Analysis PR 4 or is
-- renumbered (the PR says so).
--
-- emr.sync_state: one row per connection the scheduler has seen: the
-- connector's cursor (opaque to the database, small), live_since (null
-- while the first sync is a backfill, design §7.1), next_due_at, the
-- consecutive failure counts (any failure, and sign-in refusals: three in a
-- row stop polling until a test passes, §15.2), and the hospital-local day
-- of the last daily reconciliation (§5.1).
--
-- emr.raw_records.backfill: set on records a first sync stored, so their
-- findings are historical and never alert (A18; the rules arrive in PR 7).
--
-- Roles:
--   mhai_emr        its hospital's sync state (RLS): SELECT, INSERT, and
--                   UPDATE of the state columns only; never DELETE, never
--                   the key columns. INSERT of raw_records.backfill, never
--                   UPDATE of it. SELECT of hospitals.time_zone (the
--                   reconciliation hour is 02:00 hospital time).
--   mhai_emr_sched  across hospitals, the ids, due times and sign-in count
--                   (which connection to poll); never the cursor.
--
-- emr_platform_health() (00020) is replaced: a connection that is on and
-- has had no successful finished run for three poll intervals is 'errors'
-- (§15.4: a quiet FHIR source still polls ok, so silence is a fault). A
-- stale source never looks fresh.

-- +goose Up

GRANT USAGE ON SCHEMA emr TO mhai_emr_sched;

CREATE TABLE emr.sync_state (
    connection_id    uuid PRIMARY KEY,
    hospital_id      uuid NOT NULL,
    cursor           jsonb CHECK (cursor IS NULL OR (jsonb_typeof(cursor) = 'object' AND octet_length(cursor::text) <= 2048)),
    live_since       timestamptz,
    next_due_at      timestamptz NOT NULL DEFAULT now(),
    failures         integer NOT NULL DEFAULT 0 CHECK (failures BETWEEN 0 AND 1000000),
    sign_in_failures integer NOT NULL DEFAULT 0 CHECK (sign_in_failures BETWEEN 0 AND 1000000),
    reconciled_on    date,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES public.emr_connections (id, hospital_id)
);
ALTER TABLE emr.sync_state OWNER TO mhai_migrate;
ALTER TABLE emr.sync_state ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON emr.sync_state FROM PUBLIC;
CREATE INDEX sync_state_due ON emr.sync_state (next_due_at);

GRANT SELECT, INSERT ON emr.sync_state TO mhai_emr;
GRANT UPDATE (cursor, live_since, next_due_at, failures, sign_in_failures, reconciled_on, updated_at)
    ON emr.sync_state TO mhai_emr;
CREATE POLICY emr_own ON emr.sync_state TO mhai_emr
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());

GRANT SELECT (connection_id, hospital_id, next_due_at, sign_in_failures) ON emr.sync_state TO mhai_emr_sched;
CREATE POLICY sched_read ON emr.sync_state FOR SELECT TO mhai_emr_sched USING (true);

CREATE POLICY owner_read ON emr.sync_state FOR SELECT TO mhai_migrate USING (true);

ALTER TABLE emr.raw_records ADD COLUMN backfill boolean NOT NULL DEFAULT false;
GRANT INSERT (backfill) ON emr.raw_records TO mhai_emr;

GRANT SELECT (time_zone) ON hospitals TO mhai_emr;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
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
                WHEN c.enabled AND NOT EXISTS (SELECT 1 FROM emr.sync_runs r
                                               WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL
                                                 AND r.finished_at >= now() - make_interval(secs => 3 * c.poll_seconds)) THEN 'errors'
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

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
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

REVOKE SELECT (time_zone) ON hospitals FROM mhai_emr;
REVOKE INSERT (backfill) ON emr.raw_records FROM mhai_emr;
ALTER TABLE emr.raw_records DROP COLUMN backfill;
-- The table takes its policies, index and grants with it.
DROP TABLE emr.sync_state;
REVOKE USAGE ON SCHEMA emr FROM mhai_emr_sched;
