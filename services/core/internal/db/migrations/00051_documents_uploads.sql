-- Documents service PR 5: uploads and letters live (documents design §4.2,
-- §7.2, §8, §10, §11; §13 PR 5; ADR 0016). Numbered 00051: main had 00048
-- (documents PR 4) when this was pushed, and open PRs #228 (hospital admin
-- actions) and #229 (invites) take 00049 and 00050.
--
-- Core (mhai_gateway) gains its part of design §7.2: it reads its
-- hospital's documents rows (never source_name; RLS) and writes them only
-- through these definers (owner mhai_migrate, EXECUTE for mhai_gateway
-- only), each under the caller's hospital and user (authz.TxUser):
--   documents_upload_start    a new row, uploading, for the caller, after
--                             the per-user hourly limit (30) and the
--                             hospital's queue cap (20 queued), under an
--                             advisory lock (reserve, then check)
--   documents_mark_uploaded   the uploader's uploading -> uploaded; again
--                             it changes nothing; a row left uploading 30
--                             minutes is cancelled instead (§4.2)
--   documents_cancel          the uploader's cancel at the version seen,
--                             before review (uploading to queued)
--   documents_scan_audited    core_documents' once-only mark that the
--                             scan's audit entry is written (§17 departure
--                             24): returns the row the entry is made from
--
-- Clearance letters go live (analysis design §8.1, documents design §10):
--   clearances.document_id    the letter's documents row (purpose
--                             clearance), fixed at creation
--   mhai_clearance_check_live the upload's checks without the demo-only
--                             refusal (mhai_clearance_check keeps it)
--   mhai_clearance_create_live a checking letter tied to the caller's own
--                             new clearance document
--   mhai_clearance_scanned    the scan's outcome, read from the documents
--                             row (never from the message): waiting with
--                             the pages and SHA-256, or refused
-- The rules owner (mhai_clearance_rules) reads the documents rows it ties
-- letters to, under the caller's hospital, and nothing else of them.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    -- The runner acts as the rules' owner while it hands functions over.
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- Core reads its hospital's documents rows.
------------------------------------------------------------------------
ALTER TABLE documents ADD COLUMN scan_audited boolean NOT NULL DEFAULT false;
CREATE INDEX documents_uploader ON documents (hospital_id, uploaded_by, received_at) WHERE via = 'user';

GRANT SELECT (id, hospital_id, purpose, status, reason, patient_hint_id, uploaded_by, via, mime_type, size_bytes,
              pages, received_at, scanned_at, completed_at, version) ON documents TO mhai_gateway;
CREATE POLICY gateway_read ON documents FOR SELECT TO mhai_gateway
    USING (hospital_id = mhai_current_hospital());

-- The definers below insert as their owner (FORCE RLS: the caller's
-- hospital only; 00048 gave it SELECT and UPDATE the same way).
CREATE POLICY upload_definer_insert ON documents FOR INSERT TO mhai_migrate
    WITH CHECK (hospital_id = mhai_current_hospital());

-- +goose StatementBegin
CREATE FUNCTION documents_upload_start(p_purpose text, p_mime text, p_size bigint, p_patient uuid)
    RETURNS TABLE (outcome text, document_id uuid)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h    uuid := public.mhai_current_hospital();
    u    uuid := public.mhai_current_user();
    v_id uuid;
BEGIN
    IF h IS NULL OR u IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_upload_start: no hospital or user';
    END IF;
    -- §4.1: the browser's purposes, their types and sizes.
    IF p_purpose IS NULL OR p_purpose NOT IN ('clinical', 'clearance') OR p_mime IS NULL OR p_size IS NULL OR p_size < 1
       OR (p_purpose = 'clinical' AND (p_mime NOT IN ('application/pdf', 'image/png', 'image/jpeg', 'image/tiff')
                                       OR p_size > 26214400))
       OR (p_purpose = 'clearance' AND (p_mime NOT IN ('application/pdf', 'image/png', 'image/jpeg')
                                        OR p_size > 10485760 OR p_patient IS NOT NULL)) THEN
        RETURN QUERY SELECT 'invalid'::text, NULL::uuid;
        RETURN;
    END IF;
    -- Reserve, then check: one caller at a time per person and hospital,
    -- so parallel starts can't all pass the limit (Analysis departure 44).
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('documents.upload|' || h::text || '|' || u::text, 0));
    IF (SELECT count(*) FROM public.documents d
         WHERE d.hospital_id = h AND d.via = 'user' AND d.uploaded_by = u
           AND d.received_at > now() - interval '1 hour') >= 30 THEN
        RETURN QUERY SELECT 'rate_limited'::text, NULL::uuid;
        RETURN;
    END IF;
    IF p_purpose = 'clinical' AND (SELECT count(*) FROM public.documents d
                                    WHERE d.hospital_id = h AND d.status = 'queued') >= 20 THEN
        RETURN QUERY SELECT 'busy'::text, NULL::uuid;
        RETURN;
    END IF;
    -- A named patient must be the hospital's (another hospital's is the
    -- same answer as one that doesn't exist).
    IF p_patient IS NOT NULL AND NOT public.emr_patient_in_hospital(p_patient) THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::uuid;
        RETURN;
    END IF;
    INSERT INTO public.documents (hospital_id, purpose, status, patient_hint_id, uploaded_by, via, mime_type, size_bytes)
    VALUES (h, p_purpose, 'uploading', p_patient, u, 'user', p_mime, p_size)
    RETURNING documents.id INTO v_id;
    RETURN QUERY SELECT 'ok'::text, v_id;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_mark_uploaded(p_document uuid, p_purpose text)
    RETURNS TABLE (outcome text, status text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    u uuid := public.mhai_current_user();
    s text;
    at timestamptz;
BEGIN
    IF h IS NULL OR u IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_mark_uploaded: no hospital or user';
    END IF;
    SELECT d.status, d.received_at INTO s, at FROM public.documents d
     WHERE d.hospital_id = h AND d.id = p_document AND d.via = 'user' AND d.uploaded_by = u AND d.purpose = p_purpose
       FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::text;
        RETURN;
    END IF;
    IF s <> 'uploading' THEN
        -- A second call changes nothing (§4.2).
        RETURN QUERY SELECT 'unchanged'::text, s;
        RETURN;
    END IF;
    IF at <= now() - interval '30 minutes' THEN
        -- §4.2: a row left uploading for 30 minutes is cancelled; the
        -- quarantine lifecycle removes any object.
        UPDATE public.documents d SET status = 'cancelled', version = d.version + 1
         WHERE d.hospital_id = h AND d.id = p_document;
        RETURN QUERY SELECT 'expired'::text, 'cancelled'::text;
        RETURN;
    END IF;
    UPDATE public.documents d SET status = 'uploaded', version = d.version + 1
     WHERE d.hospital_id = h AND d.id = p_document;
    RETURN QUERY SELECT 'uploaded'::text, 'uploaded'::text;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION documents_cancel(p_document uuid, p_purpose text, p_version integer)
    RETURNS TABLE (outcome text, status text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    u uuid := public.mhai_current_user();
    s text;
    v integer;
BEGIN
    IF h IS NULL OR u IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_cancel: no hospital or user';
    END IF;
    SELECT d.status, d.version INTO s, v FROM public.documents d
     WHERE d.hospital_id = h AND d.id = p_document AND d.via = 'user' AND d.uploaded_by = u AND d.purpose = p_purpose
       FOR UPDATE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'not_found'::text, NULL::text;
        RETURN;
    END IF;
    -- Before review only (§10). A scan holding the row loses its lease: its
    -- ready or quarantine then changes nothing, and it deletes what it
    -- stored (§17 departure 25).
    IF p_version IS NULL OR v <> p_version OR s NOT IN ('uploading', 'uploaded', 'scanning', 'ready', 'queued') THEN
        RETURN QUERY SELECT 'conflict'::text, s;
        RETURN;
    END IF;
    UPDATE public.documents d
       SET status = 'cancelled', reason = NULL, scan_lease_id = NULL, scan_lease_until = NULL, version = d.version + 1
     WHERE d.hospital_id = h AND d.id = p_document;
    RETURN QUERY SELECT 'cancelled'::text, 'cancelled'::text;
END
$$;
-- +goose StatementEnd

-- The scan's audit entry, once (core_documents; §17 departures 24, 39): the
-- first call for a row the scan has finished marks it and returns what the
-- entry is made of; any later call, or a row whose scan hasn't finished,
-- returns marked = false and the row as it is.
-- +goose StatementBegin
CREATE FUNCTION documents_scan_audited(p_document uuid)
    RETURNS TABLE (marked boolean, purpose text, status text, reason text, pages integer, scanned boolean)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
BEGIN
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'documents_scan_audited: no hospital';
    END IF;
    RETURN QUERY
    UPDATE public.documents d SET scan_audited = true
     WHERE d.hospital_id = h AND d.id = p_document AND NOT d.scan_audited AND d.scanned_at IS NOT NULL
    RETURNING true, d.purpose, d.status, d.reason, d.pages, true;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, d.purpose, d.status, d.reason, d.pages, d.scanned_at IS NOT NULL
          FROM public.documents d WHERE d.hospital_id = h AND d.id = p_document;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION documents_upload_start(text, text, bigint, uuid) OWNER TO mhai_migrate;
ALTER FUNCTION documents_mark_uploaded(uuid, text) OWNER TO mhai_migrate;
ALTER FUNCTION documents_cancel(uuid, text, integer) OWNER TO mhai_migrate;
ALTER FUNCTION documents_scan_audited(uuid) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION documents_upload_start(text, text, bigint, uuid), documents_mark_uploaded(uuid, text),
    documents_cancel(uuid, text, integer), documents_scan_audited(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION documents_upload_start(text, text, bigint, uuid), documents_mark_uploaded(uuid, text),
    documents_cancel(uuid, text, integer), documents_scan_audited(uuid) TO mhai_gateway;

------------------------------------------------------------------------
-- Clearance letters, live.
------------------------------------------------------------------------
ALTER TABLE clearances ADD COLUMN document_id uuid;
ALTER TABLE clearances ADD CONSTRAINT clearances_document UNIQUE (document_id),
    ADD CONSTRAINT clearances_document_fk FOREIGN KEY (hospital_id, document_id) REFERENCES documents (hospital_id, id);

-- A letter's document never changes (the rules trigger guards the other
-- facts; this one is new).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_document_fixed() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF NEW.document_id IS DISTINCT FROM OLD.document_id THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'a letter''s document never changes';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_clearance_document_fixed() OWNER TO mhai_migrate;
CREATE TRIGGER clearances_document_fixed BEFORE UPDATE OF document_id ON clearances
    FOR EACH ROW EXECUTE FUNCTION mhai_clearance_document_fixed();

-- The rules' owner reads the documents rows it ties letters to.
GRANT SELECT (id, hospital_id, purpose, status, reason, uploaded_by, via, pages, sha256) ON documents
    TO mhai_clearance_rules;
CREATE POLICY clearance_rules ON documents FOR SELECT TO mhai_clearance_rules
    USING (hospital_id = mhai_current_hospital());

-- The upload's checks, with or without the demo-only refusal.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_check_inner(p_letter_date date, p_end_date date, p_renews uuid, p_staff uuid[], p_emails citext[],
    p_slot integer, p_live boolean,
    OUT bad_staff uuid[], OUT bad_emails text[], OUT platform_ids uuid[], OUT renews_ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_current_hospital();
    v_today date;
    v_e     public.citext;
    v_id    uuid;
BEGIN
    PERFORM public.mhai_clearance_gate();
    -- A letter outside the demo only by the live path (00051: the file goes
    -- to the documents service), and nothing is looked up for one
    -- otherwise (security review).
    IF NOT p_live AND NOT public.mhai_clearance_demo_hospital(v_h) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'unavailable';
    END IF;
    v_today := public.mhai_clearance_day(v_h, now());
    -- Reserve, then check: p_slot is the number mhai_clearance_reserve_check
    -- handed this request, within the limit and taken today. The limit is
    -- the reservation's (atomic), not today's total now: a later
    -- reservation must not refuse a check that was already within it
    -- (CI: under load the re-read refused in-limit checks, which then gave
    -- their slots back).
    IF p_slot IS NULL OR p_slot NOT BETWEEN 1 AND 5
       OR p_slot > COALESCE((SELECT r.refused FROM public.clearance_check_refusals r WHERE r.hospital_id = v_h
                               AND r.user_id = public.mhai_current_user() AND r.day = v_today), 0) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'rate_limited';
    END IF;
    IF p_end_date IS NULL OR p_end_date <= v_today OR p_end_date > (v_today + interval '12 months')::date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'end_date_too_late';
    END IF;
    IF p_letter_date IS NULL OR p_letter_date > v_today OR p_letter_date < (v_today - interval '12 months')::date
       OR p_letter_date >= p_end_date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF COALESCE(cardinality(p_staff), 0) + COALESCE(cardinality(p_emails), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    SELECT COALESCE(array_agg(s ORDER BY o), '{}') INTO bad_staff
    FROM unnest(COALESCE(p_staff, '{}')) WITH ORDINALITY AS t(s, o)
    WHERE NOT public.mhai_clearance_staff_ok(s, v_h);
    bad_emails := '{}';
    platform_ids := '{}';
    FOREACH v_e IN ARRAY COALESCE(p_emails, '{}') LOOP
        SELECT u.id INTO v_id FROM public.users u WHERE u.email OPERATOR(public.=) v_e AND u.hospital_id IS NULL;
        IF v_id IS NULL OR NOT public.mhai_clearance_platform_ok(v_id) THEN
            bad_emails := bad_emails || v_e::text;
        ELSE
            platform_ids := platform_ids || v_id;
        END IF;
        v_id := NULL;
    END LOOP;
    renews_ok := p_renews IS NULL OR EXISTS (SELECT 1 FROM public.clearances c WHERE c.id = p_renews AND c.hospital_id = v_h);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_clearance_check(p_letter_date date, p_end_date date, p_renews uuid, p_staff uuid[],
    p_emails citext[], p_slot integer,
    OUT bad_staff uuid[], OUT bad_emails text[], OUT platform_ids uuid[], OUT renews_ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    SELECT i.bad_staff, i.bad_emails, i.platform_ids, i.renews_ok INTO bad_staff, bad_emails, platform_ids, renews_ok
      FROM public.mhai_clearance_check_inner(p_letter_date, p_end_date, p_renews, p_staff, p_emails, p_slot, false) i;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_check_live(p_letter_date date, p_end_date date, p_renews uuid, p_staff uuid[],
    p_emails citext[], p_slot integer,
    OUT bad_staff uuid[], OUT bad_emails text[], OUT platform_ids uuid[], OUT renews_ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    SELECT i.bad_staff, i.bad_emails, i.platform_ids, i.renews_ok INTO bad_staff, bad_emails, platform_ids, renews_ok
      FROM public.mhai_clearance_check_inner(p_letter_date, p_end_date, p_renews, p_staff, p_emails, p_slot, true) i;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_create_live(..., document): a new letter, checking, and its
-- people, tied to the caller's own new clearance document (uploading, not
-- yet any letter's). The triggers hold every other rule.
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_create_live(p_signed_by text, p_signer_title text, p_letter_date date, p_end_date date,
    p_renews uuid, p_staff uuid[], p_platform uuid[], p_document uuid) RETURNS uuid
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h  uuid := public.mhai_current_hospital();
    v_id uuid;
BEGIN
    PERFORM public.mhai_clearance_gate();
    IF COALESCE(cardinality(p_staff), 0) + COALESCE(cardinality(p_platform), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF p_document IS NULL OR NOT EXISTS (SELECT 1 FROM public.documents d
        WHERE d.hospital_id = v_h AND d.id = p_document AND d.purpose = 'clearance' AND d.via = 'user'
          AND d.status = 'uploading' AND d.uploaded_by = public.mhai_current_user()) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict', DETAIL = 'not the caller''s new letter document';
    END IF;
    INSERT INTO public.clearances (hospital_id, signed_by, signer_title, letter_date, end_date, renews, uploaded_by,
                                   document_id)
    VALUES (v_h, p_signed_by, p_signer_title, p_letter_date, p_end_date, p_renews, public.mhai_current_user(), p_document)
    RETURNING id INTO v_id;
    INSERT INTO public.clearance_covered (hospital_id, clearance_id, user_id, side)
    SELECT v_h, v_id, s, 'hospital' FROM unnest(COALESCE(p_staff, '{}')) s;
    INSERT INTO public.clearance_covered (hospital_id, clearance_id, user_id, side)
    SELECT v_h, v_id, s, 'platform' FROM unnest(COALESCE(p_platform, '{}')) s;
    RETURN v_id;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_scanned(document): the documents outcome of a live letter,
-- for core_documents (no user; the gateway under the subject's hospital).
-- The outcome is the documents row's, never the message's: ready makes the
-- letter waiting with the row's pages and SHA-256; quarantined or failed
-- make it refused, the reason mapped to the letter's (§17 departure 38).
-- Returns done (changed now), unchanged (not checking any more), pending
-- (the scan hasn't finished) or not_found (no live letter has the document
-- in this hospital).
-- +goose StatementBegin
CREATE FUNCTION mhai_clearance_scanned(p_document uuid) RETURNS text
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h      uuid := public.mhai_current_hospital();
    v_letter uuid;
    v_status text;
    d        record;
BEGIN
    IF v_h IS NULL OR p_document IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'mhai_clearance_scanned: no hospital or document';
    END IF;
    SELECT c.id, c.status INTO v_letter, v_status FROM public.clearances c
     WHERE c.hospital_id = v_h AND c.document_id = p_document FOR UPDATE;
    IF NOT FOUND THEN
        RETURN 'not_found';
    END IF;
    IF v_status <> 'checking' THEN
        RETURN 'unchanged';
    END IF;
    SELECT x.status, x.reason, x.pages, x.sha256 INTO d FROM public.documents x
     WHERE x.hospital_id = v_h AND x.id = p_document AND x.purpose = 'clearance';
    IF NOT FOUND THEN
        RETURN 'pending';
    END IF;
    IF d.status IN ('uploading', 'uploaded', 'scanning') THEN
        RETURN 'pending';
    END IF;
    PERFORM pg_catalog.set_config('mhai.clearance_outcome', v_letter::text, true);
    IF d.status = 'ready' AND d.pages BETWEEN 1 AND 20 AND d.sha256 IS NOT NULL THEN
        UPDATE public.clearances SET status = 'waiting', pages = d.pages, sha256 = d.sha256 WHERE id = v_letter;
    ELSE
        UPDATE public.clearances SET status = 'refused', refused_reason = CASE
            WHEN d.status = 'quarantined' AND d.reason = 'unsupported_type' THEN 'file_type'
            WHEN d.status = 'quarantined' AND d.reason = 'too_large' THEN 'file_too_large'
            WHEN d.status = 'quarantined' AND d.reason = 'too_many_pages' THEN 'too_many_pages'
            WHEN d.status = 'quarantined' AND d.reason = 'password_protected' THEN 'password_protected'
            WHEN d.status = 'quarantined' AND d.reason = 'malware' THEN 'failed_safety_check'
            ELSE 'unreadable' END
         WHERE id = v_letter;
    END IF;
    PERFORM pg_catalog.set_config('mhai.clearance_outcome', '', true);
    RETURN 'done';
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_clearance_check_inner(date, date, uuid, uuid[], citext[], integer, boolean) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_check_live(date, date, uuid, uuid[], citext[], integer) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_create_live(text, text, date, date, uuid, uuid[], uuid[], uuid) OWNER TO mhai_clearance_rules;
ALTER FUNCTION mhai_clearance_scanned(uuid) OWNER TO mhai_clearance_rules;
REVOKE ALL ON FUNCTION mhai_clearance_check_inner(date, date, uuid, uuid[], citext[], integer, boolean),
    mhai_clearance_check_live(date, date, uuid, uuid[], citext[], integer),
    mhai_clearance_create_live(text, text, date, date, uuid, uuid[], uuid[], uuid), mhai_clearance_scanned(uuid),
    mhai_clearance_document_fixed() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_clearance_check_live(date, date, uuid, uuid[], citext[], integer),
    mhai_clearance_create_live(text, text, date, date, uuid, uuid[], uuid[], uuid), mhai_clearance_scanned(uuid)
    TO mhai_gateway;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- mhai_clearance_check as 00038 made it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_clearance_check(p_letter_date date, p_end_date date, p_renews uuid, p_staff uuid[], p_emails citext[],
    p_slot integer,
    OUT bad_staff uuid[], OUT bad_emails text[], OUT platform_ids uuid[], OUT renews_ok boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h     uuid := public.mhai_current_hospital();
    v_today date;
    v_e     public.citext;
    v_id    uuid;
BEGIN
    PERFORM public.mhai_clearance_gate();
    -- No letter is stored outside the demo until the documents service
    -- exists, and nothing is looked up for one either (security review).
    IF NOT public.mhai_clearance_demo_hospital(v_h) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'unavailable';
    END IF;
    v_today := public.mhai_clearance_day(v_h, now());
    -- Reserve, then check: p_slot is the number mhai_clearance_reserve_check
    -- handed this request, within the limit and taken today. The limit is
    -- the reservation's (atomic), not today's total now: a later
    -- reservation must not refuse a check that was already within it
    -- (CI: under load the re-read refused in-limit checks, which then gave
    -- their slots back).
    IF p_slot IS NULL OR p_slot NOT BETWEEN 1 AND 5
       OR p_slot > COALESCE((SELECT r.refused FROM public.clearance_check_refusals r WHERE r.hospital_id = v_h
                               AND r.user_id = public.mhai_current_user() AND r.day = v_today), 0) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'rate_limited';
    END IF;
    IF p_end_date IS NULL OR p_end_date <= v_today OR p_end_date > (v_today + interval '12 months')::date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'end_date_too_late';
    END IF;
    IF p_letter_date IS NULL OR p_letter_date > v_today OR p_letter_date < (v_today - interval '12 months')::date
       OR p_letter_date >= p_end_date THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF COALESCE(cardinality(p_staff), 0) + COALESCE(cardinality(p_emails), 0) NOT BETWEEN 1 AND 20 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    SELECT COALESCE(array_agg(s ORDER BY o), '{}') INTO bad_staff
    FROM unnest(COALESCE(p_staff, '{}')) WITH ORDINALITY AS t(s, o)
    WHERE NOT public.mhai_clearance_staff_ok(s, v_h);
    bad_emails := '{}';
    platform_ids := '{}';
    FOREACH v_e IN ARRAY COALESCE(p_emails, '{}') LOOP
        SELECT u.id INTO v_id FROM public.users u WHERE u.email OPERATOR(public.=) v_e AND u.hospital_id IS NULL;
        IF v_id IS NULL OR NOT public.mhai_clearance_platform_ok(v_id) THEN
            bad_emails := bad_emails || v_e::text;
        ELSE
            platform_ids := platform_ids || v_id;
        END IF;
        v_id := NULL;
    END LOOP;
    renews_ok := p_renews IS NULL OR EXISTS (SELECT 1 FROM public.clearances c WHERE c.id = p_renews AND c.hospital_id = v_h);
END
$$;
-- +goose StatementEnd
DROP FUNCTION mhai_clearance_scanned(uuid);
DROP FUNCTION mhai_clearance_create_live(text, text, date, date, uuid, uuid[], uuid[], uuid);
DROP FUNCTION mhai_clearance_check_live(date, date, uuid, uuid[], citext[], integer);
DROP FUNCTION mhai_clearance_check_inner(date, date, uuid, uuid[], citext[], integer, boolean);
DROP POLICY clearance_rules ON documents;
REVOKE ALL ON documents FROM mhai_clearance_rules;
DROP TRIGGER clearances_document_fixed ON clearances;
DROP FUNCTION mhai_clearance_document_fixed();
ALTER TABLE clearances DROP CONSTRAINT clearances_document_fk, DROP CONSTRAINT clearances_document,
    DROP COLUMN document_id;

DROP FUNCTION documents_scan_audited(uuid);
DROP FUNCTION documents_cancel(uuid, text, integer);
DROP FUNCTION documents_mark_uploaded(uuid, text);
DROP FUNCTION documents_upload_start(text, text, bigint, uuid);
DROP POLICY upload_definer_insert ON documents;
DROP POLICY gateway_read ON documents;
REVOKE ALL ON documents FROM mhai_gateway;
DROP INDEX documents_uploader;
ALTER TABLE documents DROP COLUMN scan_audited;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_clearance_rules'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
