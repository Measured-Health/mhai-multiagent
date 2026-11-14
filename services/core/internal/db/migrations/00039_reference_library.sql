-- A persistent shared reference library in kb_chunks (P0: the pilot's
-- guideline gap). NUMBERED 00039: 00038 is Patient Analysis PR 7's.
--
-- Until now the worker read kb_chunks ORDER BY hospital_id NULLS FIRST
-- LIMIT 5000, so more than 5,000 shared rows would crowd a hospital's own
-- chunks out of every question. A large library is loaded once per pinned
-- version instead, and the worker never reads all of it: the database
-- pre-filters it to the rows that match the question (full-text search on
-- a stored tsvector, a GIN index on shared rows only), and a hospital's own
-- rows are read by a query of their own, with their own bound
-- (intelligence/boundary/kb_repo.py).
--
-- reference_libraries: one row per loaded version of a library (name,
-- version, the artefact's SHA-256, licence, attribution, the embedder its
-- vectors were made with, the row count). At most one version of a name is
-- active; loading a version retires the one before it in the same
-- transaction (intelligence/knowledge_base/reference_library.py). No
-- hospital and no patient: readable by the worker and the gateway, written
-- by nobody but the migration owner (the operator's load command runs with
-- the migration DSN).
--
-- kb_chunks gains:
--   library_id  the library version a shared row belongs to. A library row
--               is shared (no hospital), so kb_shared_has_no_patient keeps
--               every patient field off it; its id is in the reference
--               namespace KB#nnnnnnn (seven digits, no leading zero), which
--               no hospital row may use for a library's citations to stay
--               unambiguous (kb_repo drops a hospital row that tries).
--   search_tsv  to_tsvector('english', content), generated and stored.
--   embedding   the library's vector for the row (its embedder's), so a
--               job need not encode it again. Library rows only.
--
-- Roles: mhai_worker and mhai_gateway get SELECT on reference_libraries;
-- kb_chunks' grants and policies are unchanged (shared_read already shows
-- shared rows to both, and neither may write one).

-- +goose Up

CREATE TABLE reference_libraries (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        varchar(64) NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    version     varchar(32) NOT NULL CHECK (version ~ '^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$'),
    sha256      char(64)    NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    licence     text        NOT NULL CHECK (length(licence) BETWEEN 1 AND 512),
    attribution text        NOT NULL CHECK (length(attribution) BETWEEN 1 AND 2048),
    embedder    varchar(200) NOT NULL CHECK (length(embedder) >= 1),
    chunk_count integer     NOT NULL CHECK (chunk_count >= 0),
    active      boolean     NOT NULL DEFAULT false,
    loaded_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);
CREATE UNIQUE INDEX reference_libraries_one_active ON reference_libraries (name) WHERE active;
ALTER TABLE reference_libraries OWNER TO mhai_migrate;
ALTER TABLE reference_libraries ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
GRANT SELECT ON reference_libraries TO mhai_worker, mhai_gateway;
-- Platform-wide reference metadata: no hospital, no patient.
CREATE POLICY shared_read ON reference_libraries FOR SELECT TO mhai_worker, mhai_gateway USING (true);

ALTER TABLE kb_chunks
    ADD COLUMN library_id bigint REFERENCES reference_libraries (id),
    ADD COLUMN search_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english'::regconfig, content)) STORED,
    ADD COLUMN embedding real[],
    ADD CONSTRAINT kb_library_rows_are_shared CHECK (library_id IS NULL OR hospital_id IS NULL),
    ADD CONSTRAINT kb_library_chunk_id CHECK (library_id IS NULL OR chunk_id ~ '^KB#[1-9][0-9]{6}$'),
    ADD CONSTRAINT kb_embedding_is_library CHECK (
        embedding IS NULL OR (library_id IS NOT NULL AND array_ndims(embedding) = 1
                              AND cardinality(embedding) BETWEEN 1 AND 4096));
CREATE INDEX kb_chunks_shared_search ON kb_chunks USING gin (search_tsv) WHERE hospital_id IS NULL;
CREATE INDEX kb_chunks_library ON kb_chunks (library_id) WHERE library_id IS NOT NULL;
-- A hospital's own rows, in id order, for their own bounded read.
CREATE INDEX kb_chunks_hospital_rows ON kb_chunks (hospital_id, id) WHERE hospital_id IS NOT NULL;

-- +goose Down

DROP INDEX kb_chunks_hospital_rows;
DROP INDEX kb_chunks_library;
DROP INDEX kb_chunks_shared_search;
ALTER TABLE kb_chunks
    DROP CONSTRAINT kb_embedding_is_library,
    DROP CONSTRAINT kb_library_chunk_id,
    DROP CONSTRAINT kb_library_rows_are_shared,
    DROP COLUMN embedding,
    DROP COLUMN search_tsv,
    DROP COLUMN library_id;
DROP TABLE reference_libraries;
