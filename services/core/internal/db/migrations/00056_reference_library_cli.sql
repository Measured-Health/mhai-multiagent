-- The shared reference library, loaded on the live site by an operator
-- with core-cli (deploy/README.md, "Live site: the shared reference
-- library"; docs/models/reference-library.md).
--
-- Until now a library (00040) could be loaded only with the migration DSN
-- (services/intelligence/scripts/reflib_load.py), from a repository
-- checkout, without an audit entry. The live host has neither a checkout
-- nor a reason to hand the owner password to a loader. `core
-- reference-library load` runs as the provisioning login instead (SET
-- LOCAL ROLE mhai_provision, as every core-cli command), checks the
-- artefact against the pin compiled into core, and appends
-- reference_library.loaded to the audit chain in the same transaction.
--
-- mhai_provision gains, and nobody else:
--   - reference_libraries: SELECT, INSERT, UPDATE (active, chunk_count),
--     DELETE. A library's metadata (name, version, digest, licence,
--     attribution, embedder, count): no hospital, no patient.
--   - kb_chunks: INSERT and DELETE on library rows only, and SELECT of the
--     four columns those statements read. The policy holds it to rows with
--     a library and no hospital, so it can never read, write or delete a
--     hospital's own row or the seeded guidelines (rows in no library);
--     its WITH CHECK also keeps both source fields empty (00002's
--     kb_shared_has_no_patient holds as well). COPY is not used: Postgres
--     refuses COPY FROM under row-level security.
--   - USAGE on kb_chunks_id_seq (the insert's id).
--   - provision_append gains 'reference_library.loaded' (00021's list
--     otherwise unchanged; actor_id still NULL).
--
-- mhai_health_guidelines() (00015) is unchanged: it is true once any
-- shared row exists, so a loaded library turns the health page's
-- guidelines part ok on a live database that has no demo seed.
--
-- Down revokes all of it and restores 00021's provision_append exactly. It
-- leaves loaded rows in place (they are the owner's, as 00040's are).

-- +goose Up

GRANT SELECT ON reference_libraries TO mhai_provision;
GRANT INSERT (name, version, sha256, licence, attribution, embedder, chunk_count, active),
      UPDATE (active, chunk_count), DELETE
    ON reference_libraries TO mhai_provision;
CREATE POLICY provision_library ON reference_libraries TO mhai_provision USING (true) WITH CHECK (true);

GRANT SELECT (id, chunk_id, hospital_id, library_id) ON kb_chunks TO mhai_provision;
GRANT INSERT (chunk_id, hospital_id, content, chunk_type, source_patient_id, source_subject_id,
              metadata_json, library_id, embedding), DELETE
    ON kb_chunks TO mhai_provision;
GRANT USAGE ON SEQUENCE kb_chunks_id_seq TO mhai_provision;
CREATE POLICY provision_library_rows ON kb_chunks TO mhai_provision
    USING (library_id IS NOT NULL AND hospital_id IS NULL)
    WITH CHECK (library_id IS NOT NULL AND hospital_id IS NULL
                AND source_patient_id IS NULL AND source_subject_id IS NULL);

DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated',
                           'hospital.model_policy_changed', 'analysis.switch_raised', 'analysis.switch_lowered',
                           'hospital.marked_demo', 'reference_library.loaded')
                AND actor_id IS NULL);

-- +goose Down

DROP POLICY provision_append ON audit_log;
CREATE POLICY provision_append ON audit_log FOR INSERT TO mhai_provision
    WITH CHECK (action IN ('hospital.created', 'user.created', 'intelligence.key_rotated', 'hospital.activated',
                           'hospital.model_policy_changed', 'analysis.switch_raised', 'analysis.switch_lowered',
                           'hospital.marked_demo')
                AND actor_id IS NULL);

DROP POLICY provision_library_rows ON kb_chunks;
REVOKE ALL ON SEQUENCE kb_chunks_id_seq FROM mhai_provision;
REVOKE ALL ON kb_chunks FROM mhai_provision;
DROP POLICY provision_library ON reference_libraries;
REVOKE ALL ON reference_libraries FROM mhai_provision;
