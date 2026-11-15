-- Documents service, storage and keys (documents design §6, §7.1, §7.2;
-- §13 PR 3; ADR 0016). Numbered 00046: main had 00045 (Analysis PR 10)
-- when this was pushed (design §17 departure 8).
--
-- Roles (cluster-wide: created if missing, never dropped; Down takes back
-- every grant and membership this file made):
--   mhai_documents      NOLOGIN: the documents service inside
--                       dbx.WithHospital. Reads only (this PR): its
--                       hospital's documents rows (never source_name, the
--                       uploader or the patient hint) and document_objects.
--                       Its writes come as documents_scan_* definers with
--                       the scan (PR 4).
--   mhai_documents_app  NOLOGIN NOINHERIT: the documents service's login
--                       (documents_app, made by core migrate up through
--                       db.EnsureLogin) is a member of it only. It holds
--                       mhai_documents WITH INHERIT FALSE, so the login
--                       holds nothing until SET ROLE (as mhai_emr_app).
-- The other roles of design §7.2 gain their grants with the PRs that use
-- them (gateway: PR 5; worker: PRs 6-7; emr: PR 10; mhai_documents_maint:
-- PR 9), so no role holds a grant no code uses (design §17, PR 3).
--
-- documents: one row per document, fixed purpose; the hospital in every
-- key. The purpose fixes who started it (clinical and clearance: a user;
-- archive: the emr service's connector), its types, its size limit and its
-- page limit (§4.1). quarantined and failed carry a closed reason (§4.4).
-- source_name (an archive file name) has no column grant to any role: it
-- is read only through a definer (PR 6).
--
-- document_objects: one row per stored, encrypted object (§6.2): the
-- wrapped data key and its KEK version live here, never in the object's
-- metadata, so deleting the row (or its document: ON DELETE CASCADE) makes
-- the object and every older bucket version of it unreadable at once. The
-- object key is derived from the row's ids by a CHECK, so it holds ids
-- only (never a name) and always names the row's own hospital.
--
-- Every table: ENABLE and FORCE row-level security, owner mhai_migrate.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_documents') THEN
        CREATE ROLE mhai_documents NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_documents_app') THEN
        CREATE ROLE mhai_documents_app NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER ROLE mhai_documents_app NOINHERIT;
GRANT mhai_documents TO mhai_documents_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_documents;

------------------------------------------------------------------------
-- Documents.
------------------------------------------------------------------------
CREATE TABLE documents (
    id               uuid NOT NULL DEFAULT gen_random_uuid(),
    hospital_id      uuid NOT NULL REFERENCES hospitals (id),
    purpose          text NOT NULL CHECK (purpose IN ('clinical', 'archive', 'clearance')),
    status           text NOT NULL DEFAULT 'uploading' CHECK (status IN ('uploading', 'uploaded', 'scanning', 'ready',
                         'quarantined', 'failed', 'queued', 'extracting', 'in_review', 'finalising', 'completed',
                         'rejected', 'cancelled', 'purged')),
    reason           text,
    patient_hint_id  uuid,
    uploaded_by      uuid,
    via              text NOT NULL CHECK (via IN ('user', 'emr')),
    connection_id    uuid,
    archive_file_id  text CHECK (char_length(archive_file_id) BETWEEN 1 AND 256),
    archive_revision text CHECK (char_length(archive_revision) BETWEEN 1 AND 64),
    source_name      text CHECK (char_length(source_name) BETWEEN 1 AND 512),
    source_removed   boolean NOT NULL DEFAULT false,
    mime_type        text NOT NULL CHECK (mime_type IN ('application/pdf', 'image/png', 'image/jpeg', 'image/tiff')),
    -- The declared size (the POST policy's upper bound comes from the
    -- purpose, §4.1); the scan reads the stored object's real size.
    size_bytes       bigint NOT NULL CHECK (size_bytes >= 1),
    sha256           bytea CHECK (octet_length(sha256) = 32),
    pages            integer CHECK (pages BETWEEN 1 AND 50),
    received_at      timestamptz NOT NULL DEFAULT now(),
    scanned_at       timestamptz,
    completed_at     timestamptz,
    purge_state      text CHECK (purge_state IN ('purge_pending', 'held')),
    version          integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    PRIMARY KEY (hospital_id, id),
    CONSTRAINT documents_id UNIQUE (id),
    FOREIGN KEY (connection_id, hospital_id) REFERENCES emr_connections (id, hospital_id),
    FOREIGN KEY (hospital_id, patient_hint_id) REFERENCES emr.patients (hospital_id, id),
    -- §4.1: archive documents come from the emr service's connector only,
    -- with the connection, the archive's file id and revision; the others
    -- from a signed-in user, with no archive columns and no file name.
    CONSTRAINT documents_via CHECK ((purpose = 'archive') = (via = 'emr')),
    CONSTRAINT documents_archive CHECK (via <> 'emr' OR (connection_id IS NOT NULL AND archive_file_id IS NOT NULL
                                                         AND archive_revision IS NOT NULL AND uploaded_by IS NULL)),
    CONSTRAINT documents_user CHECK (via <> 'user' OR (uploaded_by IS NOT NULL AND connection_id IS NULL
                                                       AND archive_file_id IS NULL AND archive_revision IS NULL
                                                       AND source_name IS NULL AND NOT source_removed)),
    -- §4.1: types, sizes and pages per purpose. A clearance letter is a
    -- PDF, PNG or JPEG of at most 10 MB and 20 pages, and names no patient.
    CONSTRAINT documents_limits CHECK (CASE purpose
        WHEN 'clearance' THEN mime_type <> 'image/tiff' AND size_bytes <= 10485760 AND coalesce(pages, 1) <= 20
                              AND patient_hint_id IS NULL
        ELSE size_bytes <= 26214400 END),
    -- §4.4: quarantined and failed always carry a reason from their closed
    -- list; cancelled may carry source_removed (§6.5); nothing else has one.
    CONSTRAINT documents_reason CHECK (CASE status
        WHEN 'quarantined' THEN reason IS NOT NULL AND reason IN ('unsupported_type', 'too_large', 'too_many_pages',
                                'too_many_pixels', 'password_protected', 'malware', 'unreadable', 'checksum_mismatch')
        WHEN 'failed' THEN reason IS NOT NULL AND reason IN ('scan_unavailable', 'no_text', 'no_patient_identifiers',
                           'multiple_patients', 'unreadable')
        WHEN 'cancelled' THEN reason IS NULL OR reason = 'source_removed'
        ELSE reason IS NULL END)
);
ALTER TABLE documents OWNER TO mhai_migrate;
ALTER TABLE documents ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
-- §4.3: an archive file and revision is registered once per connection.
CREATE UNIQUE INDEX documents_archive_file ON documents (hospital_id, connection_id, archive_file_id, archive_revision)
    WHERE via = 'emr';
CREATE INDEX documents_status ON documents (hospital_id, status, received_at);

-- The documents service: its hospital's rows, the columns it needs to
-- sign an upload and (PR 4) to scan. Never source_name, uploaded_by or
-- patient_hint_id.
GRANT SELECT (id, hospital_id, purpose, status, reason, via, mime_type, size_bytes, sha256, pages, received_at,
              scanned_at, version) ON documents TO mhai_documents;
CREATE POLICY documents_own ON documents FOR SELECT TO mhai_documents
    USING (hospital_id = mhai_current_hospital());

------------------------------------------------------------------------
-- Stored objects and their wrapped data keys.
------------------------------------------------------------------------
CREATE TABLE document_objects (
    hospital_id uuid NOT NULL,
    document_id uuid NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('original', 'page')),
    n           integer NOT NULL,
    object_key  text NOT NULL,
    -- The data key wrapped under the hospital's KEK (the file provider's
    -- AES-256-GCM: 60 bytes; a KMS ciphertext is longer).
    wrapped_dek bytea NOT NULL CHECK (octet_length(wrapped_dek) BETWEEN 40 AND 1024),
    kek_version integer NOT NULL CHECK (kek_version >= 1),
    -- The plaintext's SHA-256 and size.
    sha256      bytea NOT NULL CHECK (octet_length(sha256) = 32),
    size_bytes  bigint NOT NULL CHECK (size_bytes >= 1),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (hospital_id, document_id, kind, n),
    CONSTRAINT document_objects_key_unique UNIQUE (object_key),
    FOREIGN KEY (hospital_id, document_id) REFERENCES documents (hospital_id, id) ON DELETE CASCADE,
    CONSTRAINT document_objects_n CHECK ((kind = 'original' AND n = 0) OR (kind = 'page' AND n BETWEEN 1 AND 50)),
    -- §6.1: object keys hold ids only, under the row's own hospital.
    CONSTRAINT document_objects_key CHECK (object_key = CASE kind
        WHEN 'original' THEN 'originals/' || hospital_id::text || '/' || document_id::text
        ELSE 'pages/' || hospital_id::text || '/' || document_id::text || '/' || n::text END)
);
ALTER TABLE document_objects OWNER TO mhai_migrate;
ALTER TABLE document_objects ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;

GRANT SELECT ON document_objects TO mhai_documents;
CREATE POLICY objects_own ON document_objects FOR SELECT TO mhai_documents
    USING (hospital_id = mhai_current_hospital());

-- +goose Down

-- The tables take their policies, grants and indexes with them.
DROP TABLE document_objects;
DROP TABLE documents;
REVOKE USAGE ON SCHEMA public FROM mhai_documents;
REVOKE mhai_documents FROM mhai_documents_app;
