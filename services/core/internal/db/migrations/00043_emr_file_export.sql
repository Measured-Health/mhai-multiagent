-- EMR PR 11, the scheduled file export (EMR design §5.4, §15.4; plan Task
-- 44). NUMBERED 00043: 00040 and 00041 are the reference library's and
-- 00042 Patient Analysis PR 8's. goose refuses a lower version once a
-- higher one is applied, so this file merges after all three.
--
-- emr.sync_state.last_file_at: when the scheduler last loaded a new file
-- from a file_export connection (the emr service's clock, never the file's
-- own times). The emr role writes it with the rest of the state.
--
-- emr_platform_health() and emr_sources_status() (00027) are replaced: a
-- file_export connection that is on is also errors / behind when no new
-- file has been loaded for longer than the hospital's declared export
-- interval plus 50% (settings.expected_every_minutes, required for the
-- type), or ever, or when the interval isn't a whole number of minutes
-- from 5 to 10080 (the connector's range). A quiet export is a fault on
-- the hospital's side, and a source we can't judge never reads as current.
-- The platform also sees a file export as errors for 24 hours after a run
-- that kept rows as dead letters or set files aside (an unreadable_data
-- error row; the run itself is ok, so the data stays current for
-- clinicians). Other types are unchanged.

-- +goose Up

ALTER TABLE emr.sync_state ADD COLUMN last_file_at timestamptz;
GRANT UPDATE (last_file_at) ON emr.sync_state TO mhai_emr;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
    RETURNS TABLE (connection_id uuid, health text, last_sync_at timestamptz, last_sync_ok boolean,
                   last_sync_records integer, errors_24h bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id,
           CASE WHEN c.enabled AND c.paused THEN 'errors'
                WHEN l.at IS NULL THEN 'never'
                WHEN EXISTS (SELECT 1 FROM (SELECT r.ok FROM emr.sync_runs r
                                            WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                                            ORDER BY r.finished_at DESC LIMIT 3) t
                             WHERE t.ok IS NOT TRUE) THEN 'errors'
                WHEN c.enabled AND NOT EXISTS (SELECT 1 FROM emr.sync_runs r
                                               WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL
                                                 AND r.finished_at >= now() - make_interval(secs => 3 * c.poll_seconds)) THEN 'errors'
                WHEN c.enabled AND c.type = 'file_export'
                     AND (s.last_file_at IS NULL
                          OR CASE WHEN c.settings ->> 'expected_every_minutes' ~ '^[1-9][0-9]{0,5}$'
                                  THEN CASE WHEN (c.settings ->> 'expected_every_minutes')::integer BETWEEN 5 AND 10080
                                            THEN s.last_file_at < now() - make_interval(secs => (c.settings ->> 'expected_every_minutes')::integer * 90)
                                            ELSE true END
                                  ELSE true END) THEN 'errors'
                WHEN c.enabled AND c.type = 'file_export'
                     AND EXISTS (SELECT 1 FROM emr.connection_errors e
                                 WHERE e.connection_id = c.id AND e.code = 'unreadable_data'
                                   AND e.at > now() - interval '24 hours') THEN 'errors'
                ELSE 'ok' END,
           l.at, l.ok, l.records,
           (SELECT count(*) FROM emr.connection_errors e
            WHERE e.connection_id = c.id AND e.at > now() - interval '24 hours')
    FROM public.emr_connections c
    LEFT JOIN emr.sync_state s ON s.connection_id = c.id
    LEFT JOIN LATERAL (SELECT r.finished_at AS at, r.ok, r.records FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.finished_at IS NOT NULL
                       ORDER BY r.finished_at DESC LIMIT 1) l ON true
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_sources_status()
    RETURNS TABLE (connection_id uuid, source_label text, status text, last_update_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.source_label,
           CASE WHEN c.paused THEN 'behind'
                WHEN l.at IS NULL OR l.at < now() - make_interval(secs => 3 * c.poll_seconds) THEN 'behind'
                WHEN c.type = 'file_export'
                     AND (s.last_file_at IS NULL
                          OR CASE WHEN c.settings ->> 'expected_every_minutes' ~ '^[1-9][0-9]{0,5}$'
                                  THEN CASE WHEN (c.settings ->> 'expected_every_minutes')::integer BETWEEN 5 AND 10080
                                            THEN s.last_file_at < now() - make_interval(secs => (c.settings ->> 'expected_every_minutes')::integer * 90)
                                            ELSE true END
                                  ELSE true END) THEN 'behind'
                ELSE 'current' END,
           l.at
    FROM public.emr_connections c
    LEFT JOIN emr.sync_state s ON s.connection_id = c.id
    LEFT JOIN LATERAL (SELECT max(r.finished_at) AS at FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL) l ON true
    WHERE c.hospital_id = public.mhai_current_hospital() AND c.enabled
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_sources_status()
    RETURNS TABLE (connection_id uuid, source_label text, status text, last_update_at timestamptz)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.source_label,
           CASE WHEN c.paused THEN 'behind'
                WHEN l.at IS NULL OR l.at < now() - make_interval(secs => 3 * c.poll_seconds) THEN 'behind'
                ELSE 'current' END,
           l.at
    FROM public.emr_connections c
    LEFT JOIN LATERAL (SELECT max(r.finished_at) AS at FROM emr.sync_runs r
                       WHERE r.connection_id = c.id AND r.ok AND r.finished_at IS NOT NULL) l ON true
    WHERE c.hospital_id = public.mhai_current_hospital() AND c.enabled
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION emr_platform_health()
    RETURNS TABLE (connection_id uuid, health text, last_sync_at timestamptz, last_sync_ok boolean,
                   last_sync_records integer, errors_24h bigint)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id,
           CASE WHEN c.enabled AND c.paused THEN 'errors'
                WHEN l.at IS NULL THEN 'never'
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

REVOKE UPDATE (last_file_at) ON emr.sync_state FROM mhai_emr;
ALTER TABLE emr.sync_state DROP COLUMN last_file_at;
