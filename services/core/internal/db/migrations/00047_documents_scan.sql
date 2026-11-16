-- Documents service, the scan (documents design §5, §7.2; §13 PR 4; ADR
-- 0016). Numbered 00047: main had 00046 (documents PR 3) when this was
-- pushed.
--
-- The documents service writes only through these definers (SECURITY
-- DEFINER, owner mhai_migrate, EXECUTE for mhai_documents only), each in
-- the hospital the caller set (dbx.WithHospital: the subject's <hid>, which
-- the row must match):
--   documents_scan_claim       uploaded -> scanning, a 10-minute lease
--                              (§5); an expired lease is taken over; a
--                              claim on a row scanning for 6 hours or more
--                              ends it failed scan_unavailable (§5.3)
--   documents_scan_renew       the lease holder extends it, before storing
--   documents_scan_release     the lease holder gives it up (the scanner or
--                              the sandbox is unavailable: the row stays
--                              scanning, never ready, §5.3)
--   documents_scan_ready       the lease holder's clean scan: the original's
--                              and pages' object rows with their wrapped
--                              keys, pages, sha256 and ready, in one
--                              transaction (§5 step 6)
--   documents_scan_quarantine  the lease holder's refusal, with its closed
--                              reason (§4.4)
-- Every write changes version. Nothing else of the row can be written.
--
-- mhai_documents_sweep (NOLOGIN, the documents service's login holds it
-- WITH INHERIT FALSE): the sweeper's read across hospitals of the ids of
-- rows a lost message left behind (uploaded, or scanning with an expired
-- lease). Ids, status and times only; the scan itself still runs under the
-- row's hospital.

-- +goose Up

ALTER TABLE documents
    ADD COLUMN scan_lease_id    uuid,
    ADD COLUMN scan_lease_until timestamptz,
    ADD COLUMN scan_started_at  timestamptz,
    -- A lease belongs to a scanning row only, and has an end.
    ADD CONSTRAINT documents_scan_lease CHECK ((scan_lease_id IS NULL OR status = 'scanning')
                                               AND (scan_lease_id IS NULL) = (scan_lease_until IS NULL)),
    ADD CONSTRAINT documents_scanning CHECK (status <> 'scanning' OR scan_started_at IS NOT NULL);
CREATE INDEX documents_scan_due ON documents (status, received_at) WHERE status IN ('uploaded', 'scanning');

-- The definers act as their owner; FORCE ROW LEVEL SECURITY applies to it
-- too, so it gets exactly the caller's hospital and nothing more.
CREATE POLICY scan_definer_read ON documents FOR SELECT TO mhai_migrate
    USING (hospital_id = mhai_current_hospital());
CREATE POLICY scan_definer_update ON documents FOR UPDATE TO mhai_migrate
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (hospital_id = mhai_current_hospital());
CREATE POLICY scan_definer_insert ON document_objects FOR INSERT TO mhai_migrate
    WITH CHECK (hospital_id = mhai_current_hospital());

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_documents_sweep') THEN
        CREATE ROLE mhai_documents_sweep NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
GRANT mhai_documents_sweep TO mhai_documents_app WITH INHERIT FALSE;
GRANT USAGE ON SCHEMA public TO mhai_documents_sweep;
GRANT SELECT (id, hospital_id, status, received_at, scan_lease_until) ON documents TO mhai_documents_sweep;
CREATE POLICY sweep_due ON documents FOR SELECT TO mhai_documents_sweep USING (status IN ('uploaded', 'scanning'));

-- +goose StatementBegin
CREATE FUNCTION documents_scan_claim(p_document uuid, p_lease uuid)
    RETURNS TABLE (outcome text, purpose text, mime_type text, size_bytes bigint, sha256 bytea)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
BEGIN
    IF h IS NULL OR p_document IS NULL OR p_lease IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_scan_claim: no hospital, document or lease';
    END IF;
    -- §5.3: six hours without a clean scan ends it.
    UPDATE public.documents d
       SET status = 'failed', reason = 'scan_unavailable', scan_lease_id = NULL, scan_lease_until = NULL,
           scanned_at = now(), version = d.version + 1
     WHERE d.hospital_id = h AND d.id = p_document AND d.status = 'scanning' AND d.scan_lease_until <= now()
       AND d.scan_started_at <= now() - interval '6 hours';
    IF FOUND THEN
        RETURN QUERY SELECT 'failed'::text, NULL::text, NULL::text, NULL::bigint, NULL::bytea;
        RETURN;
    END IF;
    RETURN QUERY
    UPDATE public.documents d
       SET status = 'scanning', scan_lease_id = p_lease, scan_lease_until = now() + interval '10 minutes',
           scan_started_at = coalesce(d.scan_started_at, now()), version = d.version + 1
     WHERE d.hospital_id = h AND d.id = p_document
       AND (d.status = 'uploaded' OR (d.status = 'scanning' AND d.scan_lease_until <= now()))
    RETURNING 'claimed'::text, d.purpose, d.mime_type, d.size_bytes, d.sha256;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_claimable'::text, NULL::text, NULL::text, NULL::bigint, NULL::bytea;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_scan_renew(p_document uuid, p_lease uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    UPDATE public.documents d
       SET scan_lease_until = now() + interval '10 minutes', version = d.version + 1
     WHERE d.hospital_id = public.mhai_current_hospital() AND d.id = p_document AND d.status = 'scanning'
       AND d.scan_lease_id = p_lease AND d.scan_lease_until > now();
    RETURN FOUND;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_scan_release(p_document uuid, p_lease uuid) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    UPDATE public.documents d
       SET scan_lease_until = least(d.scan_lease_until, now()), version = d.version + 1
     WHERE d.hospital_id = public.mhai_current_hospital() AND d.id = p_document AND d.status = 'scanning'
       AND d.scan_lease_id = p_lease;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_scan_quarantine(p_document uuid, p_lease uuid, p_reason text, p_sha256 bytea) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_reason IS NULL OR p_reason NOT IN ('unsupported_type', 'too_large', 'too_many_pages', 'too_many_pixels',
                                            'password_protected', 'malware', 'unreadable', 'checksum_mismatch') THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_scan_quarantine: not a quarantine reason';
    END IF;
    -- The row keeps its reason, size and SHA-256 (§5): an archive row keeps
    -- the listing's (the one the bytes didn't match).
    UPDATE public.documents d
       SET status = 'quarantined', reason = p_reason, sha256 = coalesce(d.sha256, p_sha256),
           scan_lease_id = NULL, scan_lease_until = NULL, scanned_at = now(), version = d.version + 1
     WHERE d.hospital_id = public.mhai_current_hospital() AND d.id = p_document AND d.status = 'scanning'
       AND d.scan_lease_id = p_lease;
    RETURN FOUND;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_scan_ready(p_document uuid, p_lease uuid, p_sha256 bytea, p_pages integer,
                                     p_kinds text[], p_ns integer[], p_wrapped bytea[], p_kek_versions integer[],
                                     p_sha256s bytea[], p_sizes bigint[]) RETURNS boolean
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    k integer := coalesce(array_length(p_kinds, 1), 0);
BEGIN
    IF h IS NULL OR p_sha256 IS NULL OR p_pages IS NULL OR p_pages < 1 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_scan_ready: invalid arguments';
    END IF;
    -- One original and exactly the pages 1..p_pages, one row each.
    IF k <> p_pages + 1 OR coalesce(array_length(p_ns, 1), 0) <> k OR coalesce(array_length(p_wrapped, 1), 0) <> k
       OR coalesce(array_length(p_kek_versions, 1), 0) <> k OR coalesce(array_length(p_sha256s, 1), 0) <> k
       OR coalesce(array_length(p_sizes, 1), 0) <> k
       OR (SELECT count(*) FROM unnest(p_kinds, p_ns) AS o(kind, n) WHERE o.kind = 'original' AND o.n = 0) <> 1
       OR (SELECT count(DISTINCT o.n) FROM unnest(p_kinds, p_ns) AS o(kind, n)
            WHERE o.kind = 'page' AND o.n BETWEEN 1 AND p_pages) <> p_pages THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_scan_ready: objects do not match the pages';
    END IF;
    -- An archive row's SHA-256 is the listing's: the stored bytes must be
    -- those (the scan checked it before reading further, §5 step 2).
    UPDATE public.documents d
       SET status = 'ready', pages = p_pages, sha256 = p_sha256, scan_lease_id = NULL, scan_lease_until = NULL,
           scanned_at = now(), version = d.version + 1
     WHERE d.hospital_id = h AND d.id = p_document AND d.status = 'scanning' AND d.scan_lease_id = p_lease
       AND (d.sha256 IS NULL OR d.sha256 = p_sha256);
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    INSERT INTO public.document_objects (hospital_id, document_id, kind, n, object_key, wrapped_dek, kek_version,
                                         sha256, size_bytes)
    SELECT h, p_document, o.kind, o.n,
           CASE o.kind WHEN 'original' THEN 'originals/' || h::text || '/' || p_document::text
                       ELSE 'pages/' || h::text || '/' || p_document::text || '/' || o.n::text END,
           o.wrapped, o.kek, o.sha, o.size
      FROM unnest(p_kinds, p_ns, p_wrapped, p_kek_versions, p_sha256s, p_sizes) AS o(kind, n, wrapped, kek, sha, size);
    RETURN true;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION documents_scan_claim(uuid, uuid) OWNER TO mhai_migrate;
ALTER FUNCTION documents_scan_renew(uuid, uuid) OWNER TO mhai_migrate;
ALTER FUNCTION documents_scan_release(uuid, uuid) OWNER TO mhai_migrate;
ALTER FUNCTION documents_scan_quarantine(uuid, uuid, text, bytea) OWNER TO mhai_migrate;
ALTER FUNCTION documents_scan_ready(uuid, uuid, bytea, integer, text[], integer[], bytea[], integer[], bytea[], bigint[])
    OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION documents_scan_claim(uuid, uuid), documents_scan_renew(uuid, uuid),
    documents_scan_release(uuid, uuid), documents_scan_quarantine(uuid, uuid, text, bytea),
    documents_scan_ready(uuid, uuid, bytea, integer, text[], integer[], bytea[], integer[], bytea[], bigint[])
    FROM PUBLIC;
GRANT EXECUTE ON FUNCTION documents_scan_claim(uuid, uuid), documents_scan_renew(uuid, uuid),
    documents_scan_release(uuid, uuid), documents_scan_quarantine(uuid, uuid, text, bytea),
    documents_scan_ready(uuid, uuid, bytea, integer, text[], integer[], bytea[], integer[], bytea[], bigint[])
    TO mhai_documents;

-- +goose Down

DROP FUNCTION documents_scan_ready(uuid, uuid, bytea, integer, text[], integer[], bytea[], integer[], bytea[], bigint[]);
DROP FUNCTION documents_scan_quarantine(uuid, uuid, text, bytea);
DROP FUNCTION documents_scan_release(uuid, uuid);
DROP FUNCTION documents_scan_renew(uuid, uuid);
DROP FUNCTION documents_scan_claim(uuid, uuid);
DROP POLICY sweep_due ON documents;
REVOKE ALL ON documents FROM mhai_documents_sweep;
REVOKE USAGE ON SCHEMA public FROM mhai_documents_sweep;
REVOKE mhai_documents_sweep FROM mhai_documents_app;
DROP POLICY scan_definer_insert ON document_objects;
DROP POLICY scan_definer_update ON documents;
DROP POLICY scan_definer_read ON documents;
DROP INDEX documents_scan_due;
ALTER TABLE documents DROP CONSTRAINT documents_scanning, DROP CONSTRAINT documents_scan_lease,
    DROP COLUMN scan_started_at, DROP COLUMN scan_lease_until, DROP COLUMN scan_lease_id;
