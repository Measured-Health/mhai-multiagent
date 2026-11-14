-- The reference sources an answer cites: title, link and source credit
-- (reference library, PR 3). NUMBERED 00040: 00039 is the reference library.
--
-- clinical_decisions.cited_sources_json: what the pipeline wrote when it
-- answered (intelligence/boundary/cited_sources.py), so an answer keeps its
-- sources after a library version is retired and its ids reused. Shared
-- reference rows only (no hospital, no patient): an array of at most 50
-- objects {chunk_id, title, url, source, attribution}, or null for none.
-- Core checks every entry again before it shows one (queries/handlers.go).
--
-- Roles: none changed. mhai_worker already inserts and mhai_gateway reads
-- whole clinical_decisions rows under hospital_rows.

-- +goose Up

ALTER TABLE clinical_decisions
    ADD COLUMN cited_sources_json jsonb,
    ADD CONSTRAINT clinical_decisions_cited_sources CHECK (
        cited_sources_json IS NULL
        OR (jsonb_typeof(cited_sources_json) = 'array'
            AND jsonb_array_length(cited_sources_json) <= 50
            AND pg_column_size(cited_sources_json) <= 65536));

-- +goose Down

ALTER TABLE clinical_decisions DROP CONSTRAINT clinical_decisions_cited_sources, DROP COLUMN cited_sources_json;
