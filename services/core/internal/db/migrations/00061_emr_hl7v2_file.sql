-- HL7 v2 message files (design docs/designs/2026-11-27-hl7v2-file-
-- connector-and-bridge.md §6): the hl7v2_file connector type. NUMBERED
-- 00061: the next free number on main when the PR opened; merge main and
-- renumber if a parallel change took it first (goose refuses a lower
-- version once a higher one is applied).
--
-- emr_connections.type and emr.raw_records.connector_type accept
-- hl7v2_file. emr_platform_health() and emr_sources_status() (00043) are
-- replaced so a switched-on hl7v2_file connection is judged by the
-- file_export rule: errors / behind with no new file for longer than the
-- declared interval plus 50%, or ever, or with an interval out of range,
-- and errors for 24 hours after a run that set files aside or kept dead
-- letters. No new table, column or grant.

-- +goose Up

ALTER TABLE emr_connections DROP CONSTRAINT emr_connections_type_check,
    ADD CONSTRAINT emr_connections_type_check
    CHECK (type IN ('fhir_r4', 'document_archive', 'file_export', 'hl7v2_file', 'stub'));
ALTER TABLE emr.raw_records DROP CONSTRAINT raw_records_connector_type_check,
    ADD CONSTRAINT raw_records_connector_type_check
    CHECK (connector_type IN ('fhir_r4', 'document_archive', 'file_export', 'hl7v2_file', 'stub', 'document'));

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
                WHEN c.enabled AND c.type IN ('file_export', 'hl7v2_file')
                     AND (s.last_file_at IS NULL
                          OR CASE WHEN c.settings ->> 'expected_every_minutes' ~ '^[1-9][0-9]{0,5}$'
                                  THEN CASE WHEN (c.settings ->> 'expected_every_minutes')::integer BETWEEN 5 AND 10080
                                            THEN s.last_file_at < now() - make_interval(secs => (c.settings ->> 'expected_every_minutes')::integer * 90)
                                            ELSE true END
                                  ELSE true END) THEN 'errors'
                WHEN c.enabled AND c.type IN ('file_export', 'hl7v2_file')
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
                WHEN c.type IN ('file_export', 'hl7v2_file')
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

-- Refused (the CHECKs fail) while an hl7v2_file connection or raw record
-- exists: remove them first.
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

ALTER TABLE emr.raw_records DROP CONSTRAINT raw_records_connector_type_check,
    ADD CONSTRAINT raw_records_connector_type_check
    CHECK (connector_type IN ('fhir_r4', 'document_archive', 'file_export', 'stub', 'document'));
ALTER TABLE emr_connections DROP CONSTRAINT emr_connections_type_check,
    ADD CONSTRAINT emr_connections_type_check
    CHECK (type IN ('fhir_r4', 'document_archive', 'file_export', 'stub'));
