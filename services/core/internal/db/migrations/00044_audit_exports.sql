-- Audit exports, the named-access list's letters and the legal path
-- (analysis backend design §8.8, §8.9; plan Tasks 24-26, departures
-- 60-76; contract §14.9). NUMBERED 00044 (coordinator): EMR PR 11 takes
-- 00043.
--
-- Nothing here turns anything on. The export routes and the legal
-- reference route answer 404 until CORE_AUDIT_EXPORTS is set; a named
-- export has rows only where named lists exist (the demo); a legal
-- resolution needs a reference, the hospital's analysis switch on, its
-- named-access position on and a letter covering the admin today.
--
-- What it adds:
--   - mhai_export_rules (NOLOGIN, NOINHERIT, no members): owns the
--     functions and triggers below. Its reads: the named-view, export and
--     legal audit entries; named lists, their rows, the hospital-side
--     pids, periods and subjects' encrypted references; letters'
--     status, dates, verifier and demo flag; users, roles, two-step and
--     hospitals. Its writes: audit_export_rows and legal_references.
--   - audit_exports: one record per export (who, when, why, the filters,
--     the snapshot, the row count, the checksum). No file is kept
--     anywhere (departure 62): the file is built in the record's
--     transaction for its checksum and again in the one download
--     transaction. mhai_platform inserts and reads records and may only
--     mark one downloaded, once, before it expires, in that transaction.
--   - audit_export_rows: a named export's rows (one per named-view entry
--     and list row), each with its opaque export_row_ref. No app grant.
--   - legal_references: one export bound to one legal reference, the
--     hospital admins named on it, and its single use. No app grant.
--   - Commit checks (deferred constraint triggers): an export, a download,
--     a reference and a resolution each need their audit entry.
--   - mhai_audit_export_named, mhai_audit_export_pids, mhai_audit_letters,
--     mhai_legal_reference_create (mhai_platform); mhai_legal_subjects and
--     mhai_legal_references_mine (mhai_gateway).
--   - platform_export_append: a second permissive audit policy for the
--     export and legal-reference actions only, as 00037's
--     platform_admin_append. platform_append is not touched.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_export_rules') THEN
        CREATE ROLE mhai_export_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
    ALTER ROLE mhai_export_rules NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    -- Nobody may already act as it: its grants open pids and references.
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = 'mhai_export_rules') THEN
        RAISE EXCEPTION USING ERRCODE = 'object_in_use',
            MESSAGE = '00044: mhai_export_rules already has members; remove them first';
    END IF;
    -- The runner acts as the owners while it hands functions over and
    -- grants on the cover (00042's rules own it).
    FOREACH r IN ARRAY ARRAY['mhai_export_rules', 'mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO mhai_export_rules;

------------------------------------------------------------------------
-- Export records (contract §14.9; departures 62-67).
------------------------------------------------------------------------
CREATE TABLE audit_exports (
    id            uuid PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN ('events', 'named_access')),
    owner         uuid NOT NULL REFERENCES users (id),
    hospital_id   uuid REFERENCES hospitals (id),
    reason        text NOT NULL CHECK (reason IN ('legal_request', 'hospital_request', 'regulator_request',
                                                  'internal_review', 'security_investigation', 'other')),
    note          text CHECK (char_length(note) BETWEEN 1 AND 200 AND note !~ '[[:cntrl:]]'),
    reference     text CHECK (reference ~ '^[A-Za-z0-9][A-Za-z0-9 ._/:-]{0,59}$'),
    filters       jsonb NOT NULL CHECK (jsonb_typeof(filters) = 'object'),
    max_seq       bigint NOT NULL CHECK (max_seq >= 0),
    rows          integer NOT NULL CHECK (rows BETWEEN 0 AND 50000),
    capped        boolean NOT NULL DEFAULT false CHECK (NOT capped),
    checksum      text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    file_name     text NOT NULL CHECK (file_name ~ '^[A-Za-z0-9._-]{1,120}$'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '15 minutes',
    created_xact  xid8 NOT NULL DEFAULT pg_current_xact_id(),
    downloaded_at timestamptz,
    download_xact xid8,
    UNIQUE (hospital_id, id),
    CONSTRAINT audit_exports_hospital CHECK ((kind = 'named_access') = (hospital_id IS NOT NULL)),
    CONSTRAINT audit_exports_other CHECK (reason <> 'other' OR note IS NOT NULL),
    CONSTRAINT audit_exports_expiry CHECK (expires_at = created_at + interval '15 minutes'),
    CONSTRAINT audit_exports_download CHECK ((downloaded_at IS NULL) = (download_xact IS NULL))
);
CREATE INDEX audit_exports_created_idx ON audit_exports (created_at DESC, id DESC);

CREATE TABLE audit_export_rows (
    export_id   uuid NOT NULL,
    ord         integer NOT NULL CHECK (ord BETWEEN 1 AND 50000),
    ref         text NOT NULL UNIQUE CHECK (ref ~ '^[0-9a-f]{24}$'),
    hospital_id uuid NOT NULL,
    view_id     uuid NOT NULL,
    row_ord     integer NOT NULL CHECK (row_ord BETWEEN 1 AND 200),
    seq         bigint NOT NULL,
    PRIMARY KEY (export_id, ord),
    FOREIGN KEY (hospital_id, export_id) REFERENCES audit_exports (hospital_id, id),
    FOREIGN KEY (view_id, row_ord) REFERENCES analysis_view_rows (view_id, ord)
);

CREATE TABLE legal_references (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    export_id   uuid NOT NULL UNIQUE,
    hospital_id uuid NOT NULL,
    reference   text NOT NULL UNIQUE CHECK (reference ~ '^[A-Za-z0-9][A-Za-z0-9 ._/:-]{0,59}$'),
    admins      uuid[] NOT NULL CHECK (cardinality(admins) BETWEEN 1 AND 3 AND array_position(admins, NULL) IS NULL),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    used_at     timestamptz,
    used_by     uuid REFERENCES users (id),
    used_key    bytea CHECK (octet_length(used_key) = 32),
    replays     integer NOT NULL DEFAULT 0 CHECK (replays BETWEEN 0 AND 3),
    FOREIGN KEY (hospital_id, export_id) REFERENCES audit_exports (hospital_id, id),
    CONSTRAINT legal_references_used CHECK ((used_at IS NULL) = (used_by IS NULL) AND (used_at IS NULL) = (used_key IS NULL)
                                            AND (used_at IS NOT NULL OR replays = 0))
);
CREATE INDEX legal_references_hospital_idx ON legal_references (hospital_id) WHERE used_at IS NULL;

ALTER TABLE audit_exports OWNER TO mhai_migrate;
ALTER TABLE audit_export_rows OWNER TO mhai_migrate;
ALTER TABLE legal_references OWNER TO mhai_migrate;
ALTER TABLE audit_exports ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_export_rows ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE legal_references ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON audit_exports, audit_export_rows, legal_references FROM PUBLIC;

-- The entries the checks and the list look for, found at once.
CREATE INDEX audit_log_export_idx ON audit_log (entity_id) WHERE entity IN ('audit_export', 'legal_reference');
CREATE INDEX audit_log_named_idx ON audit_log (hospital_id, id)
    WHERE action IN ('analysis.named_viewed', 'analysis.named_view_failed');

-- The platform: records it makes and reads, and one download each.
GRANT SELECT, INSERT ON audit_exports TO mhai_platform;
GRANT UPDATE (rows, checksum, file_name, downloaded_at, download_xact) ON audit_exports TO mhai_platform;
CREATE POLICY platform_all ON audit_exports TO mhai_platform USING (true) WITH CHECK (true);

-- The rules' owner: what the functions and checks read and write.
GRANT SELECT ON audit_exports TO mhai_export_rules;
GRANT SELECT, INSERT ON audit_export_rows TO mhai_export_rules;
GRANT SELECT, INSERT, UPDATE ON legal_references TO mhai_export_rules;
CREATE POLICY export_rules ON audit_exports FOR SELECT TO mhai_export_rules USING (true);
CREATE POLICY export_rules ON audit_export_rows TO mhai_export_rules USING (true) WITH CHECK (true);
CREATE POLICY export_rules ON legal_references TO mhai_export_rules USING (true) WITH CHECK (true);
GRANT SELECT (id, ts, hash, hospital_id, actor_id, action, entity, entity_id, payload, xmin) ON audit_log TO mhai_export_rules;
CREATE POLICY export_rules ON audit_log FOR SELECT TO mhai_export_rules
    USING (action IN ('analysis.named_viewed', 'audit.exported', 'audit.named_access_exported', 'audit.export_downloaded',
                      'legal_reference.created',
                      'analysis.legal_resolved'));
GRANT SELECT (id, hospital_id, period_id, level, clearance_id, purpose) ON analysis_views TO mhai_export_rules;
CREATE POLICY export_rules ON analysis_views FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT ON analysis_view_rows TO mhai_export_rules;
CREATE POLICY export_rules ON analysis_view_rows FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (hospital_id, side, scope, period_id, subject_id, pid) ON analysis_pseudonyms TO mhai_export_rules;
CREATE POLICY export_rules ON analysis_pseudonyms FOR SELECT TO mhai_export_rules USING (side = 'hospital' AND scope IS NULL);
GRANT SELECT (id, hospital_id, level, start) ON analysis_periods TO mhai_export_rules;
CREATE POLICY export_rules ON analysis_periods FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (hospital_id, subject_id, ref_enc) ON analysis_subjects TO mhai_export_rules;
CREATE POLICY export_rules ON analysis_subjects FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (id, hospital_id, status, letter_date, end_date, verified_by, verified_at, demo, pages) ON clearances TO mhai_export_rules;
CREATE POLICY export_rules ON clearances FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (id, hospital_id, is_active) ON users TO mhai_export_rules;
CREATE POLICY export_rules ON users FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT ON user_roles TO mhai_export_rules;
CREATE POLICY export_rules ON user_roles FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (user_id, enabled) ON user_mfa TO mhai_export_rules;
CREATE POLICY export_rules ON user_mfa FOR SELECT TO mhai_export_rules USING (true);
GRANT SELECT (id, status, time_zone) ON hospitals TO mhai_export_rules;
CREATE POLICY export_rules ON hospitals FOR SELECT TO mhai_export_rules USING (true);
GRANT EXECUTE ON FUNCTION mhai_analysis_position(uuid), mhai_named_cover(uuid) TO mhai_export_rules;

------------------------------------------------------------------------
-- Helpers (no EXECUTE for anyone but their owner).
------------------------------------------------------------------------

-- A superuser acting as itself (test fixtures): exempt from the checks.
-- +goose StatementBegin
CREATE FUNCTION mhai_export_exempt() RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT pg_catalog.current_setting('role') = 'none'
       AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname = session_user AND r.rolsuper)
$$;
-- +goose StatementEnd

-- A record is made as written, in its own transaction: never already
-- downloaded, its times and transaction the database's. In that
-- transaction its row count, checksum and file name may be set once the
-- file is built. Afterwards only its one download may be marked: from
-- nothing, before it expires, now, by this transaction (P0001 gone
-- otherwise).
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_export_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_export_exempt() THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.created_at := now();
        NEW.expires_at := now() + interval '15 minutes';
        NEW.created_xact := pg_catalog.pg_current_xact_id();
        NEW.capped := false;
        IF NEW.downloaded_at IS NOT NULL OR NEW.download_xact IS NOT NULL THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'gone';
        END IF;
        -- Its owner is an active platform admin or auditor (security
        -- review): no record names a hospital account, or nobody.
        IF NOT EXISTS (SELECT 1 FROM public.users u
                       WHERE u.id = NEW.owner AND u.hospital_id IS NULL AND u.is_active
                         AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id
                                       AND r.role IN ('platform_admin', 'auditor'))) THEN
            RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
        END IF;
        RETURN NEW;
    END IF;
    -- In its creation transaction, before any download: the row count, the
    -- checksum and the file name, once the file is built.
    IF OLD.created_xact = pg_catalog.pg_current_xact_id() AND OLD.downloaded_at IS NULL AND NEW.downloaded_at IS NULL
       AND NEW.download_xact IS NULL
       AND (to_jsonb(NEW) - 'rows' - 'checksum' - 'file_name') IS NOT DISTINCT FROM (to_jsonb(OLD) - 'rows' - 'checksum' - 'file_name') THEN
        RETURN NEW;
    END IF;
    IF OLD.downloaded_at IS NOT NULL OR OLD.expires_at <= now() OR NEW.downloaded_at IS NULL
       OR NEW.downloaded_at IS DISTINCT FROM now() OR NEW.download_xact IS DISTINCT FROM pg_catalog.pg_current_xact_id()
       OR (to_jsonb(NEW) - 'downloaded_at' - 'download_xact') IS DISTINCT FROM (to_jsonb(OLD) - 'downloaded_at' - 'download_xact') THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'gone';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER audit_exports_rules BEFORE INSERT OR UPDATE ON audit_exports
    FOR EACH ROW EXECUTE FUNCTION mhai_audit_export_rules();

-- Whether an audit row was written by this transaction: its xmin is this
-- transaction's id (core writes its entries outside savepoints). An entry
-- committed earlier, by anyone, never passes for this transaction's
-- (security review).
-- +goose StatementBegin
CREATE FUNCTION mhai_export_entry_mine(p_xmin xid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT p_xmin::text::bigint = (pg_catalog.pg_current_xact_id()::text::bigint % 4294967296)
$$;
-- +goose StatementEnd

-- At commit: an export (and its row count and checksum, set in its own
-- transaction) needs its audit.exported (events) or
-- audit.named_access_exported entry by its owner, and a
-- download its audit.export_downloaded entry by its owner.
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_export_audited() RETURNS trigger
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_export_exempt() THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'audit_export' AND a.entity_id = NEW.id::text
                     AND a.action = CASE WHEN NEW.download_xact IS NOT NULL THEN 'audit.export_downloaded'
                                         WHEN NEW.kind = 'named_access' THEN 'audit.named_access_exported'
                                         ELSE 'audit.exported' END
                     AND a.actor_id = NEW.owner AND a.hospital_id IS NULL
                     AND public.mhai_export_entry_mine(a.xmin)) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'audit_exports: an export or a download without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER audit_exports_audited AFTER INSERT OR UPDATE ON audit_exports
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_audit_export_audited();

-- At commit: a reference needs its legal_reference.created entry by its
-- creator; each use or replay its analysis.legal_resolved entry with that
-- replay number, by the admin, in the hospital. Nothing else changes.
-- +goose StatementBegin
CREATE FUNCTION mhai_legal_reference_audited() RETURNS trigger
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_export_exempt() THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                       WHERE a.entity = 'legal_reference' AND a.entity_id = NEW.id::text
                         AND a.action = 'legal_reference.created' AND a.actor_id = NEW.created_by
                         AND a.hospital_id IS NULL AND a.payload ->> 'export_id' = NEW.export_id::text
                         AND public.mhai_export_entry_mine(a.xmin)) THEN
            RAISE EXCEPTION USING ERRCODE = 'check_violation',
                MESSAGE = 'legal_references: a reference without its audit entry';
        END IF;
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'legal_reference' AND a.entity_id = NEW.id::text
                     AND a.action = 'analysis.legal_resolved' AND a.actor_id = NEW.used_by
                     AND a.hospital_id = NEW.hospital_id AND a.payload ->> 'replays' = NEW.replays::text
                     AND public.mhai_export_entry_mine(a.xmin)) THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation',
            MESSAGE = 'legal_references: a resolution without its audit entry';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER legal_references_audited AFTER INSERT OR UPDATE ON legal_references
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mhai_legal_reference_audited();

-- A use changes only used_at, used_by, used_key (once) and replays (up
-- by one); nothing else of a reference ever changes, and none is deleted
-- (no grant).
-- +goose StatementBegin
CREATE FUNCTION mhai_legal_reference_rules() RETURNS trigger
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF public.mhai_export_exempt() THEN
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - 'used_at' - 'used_by' - 'used_key' - 'replays')
           IS DISTINCT FROM (to_jsonb(OLD) - 'used_at' - 'used_by' - 'used_key' - 'replays')
       OR (OLD.used_at IS NULL AND (NEW.used_at IS NULL OR NEW.replays <> 0))
       OR (OLD.used_at IS NOT NULL AND (NEW.used_at IS DISTINCT FROM OLD.used_at OR NEW.used_by IS DISTINCT FROM OLD.used_by
                                        OR NEW.used_key IS DISTINCT FROM OLD.used_key OR NEW.replays <> OLD.replays + 1)) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'already_used';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER legal_references_rules BEFORE UPDATE ON legal_references
    FOR EACH ROW EXECUTE FUNCTION mhai_legal_reference_rules();

------------------------------------------------------------------------
-- The platform's functions.
------------------------------------------------------------------------

-- The named-view entries and list rows an export of a hospital takes, up
-- to its snapshot. Internal.
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_export_candidates(p_hospital uuid, p_max_seq bigint, p_person uuid, p_clearance uuid,
                                             p_from timestamptz, p_to timestamptz)
    RETURNS TABLE (seq bigint, view_id uuid, row_ord integer)
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT a.id, v.id, r.ord
    FROM public.audit_log a
    JOIN public.analysis_views v ON v.hospital_id = a.hospital_id AND v.level = 'named' AND v.id::text = a.entity_id
    JOIN public.analysis_view_rows r ON r.hospital_id = v.hospital_id AND r.view_id = v.id
    WHERE a.action = 'analysis.named_viewed' AND a.entity = 'analysis_view' AND a.hospital_id = p_hospital
      AND a.entity_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
      AND a.id <= p_max_seq
      AND (p_person IS NULL OR a.actor_id = p_person)
      AND (p_clearance IS NULL OR a.payload ->> 'clearance_id' = p_clearance::text)
      AND (p_from IS NULL OR a.ts >= p_from)
      AND (p_to IS NULL OR a.ts < p_to)
$$;
-- +goose StatementEnd

-- mhai_audit_export_named(export, max): the rows of a named export, in its
-- own creation transaction only: one per analysis.named_viewed entry of the
-- export's hospital up to its snapshot (max_seq), under the record's own
-- filters (person, clearance, [from, to); security review: the rows are
-- always the recorded filters'), and per row of the entry's list, each
-- with a fresh opaque ref. Returns the count; over p_max it writes nothing.
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_export_named(p_export uuid, p_max integer) RETURNS integer
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_e         record;
    v_n         integer;
    p_person    uuid;
    p_clearance uuid;
    p_from      timestamptz;
    p_to        timestamptz;
BEGIN
    SELECT e.kind, e.hospital_id, e.max_seq, e.created_xact, e.filters INTO v_e FROM public.audit_exports e WHERE e.id = p_export;
    IF NOT FOUND OR v_e.kind <> 'named_access' OR v_e.created_xact <> pg_catalog.pg_current_xact_id()
       OR p_max IS NULL OR p_max < 0 OR p_max > 50000
       OR EXISTS (SELECT 1 FROM public.audit_export_rows x WHERE x.export_id = p_export) THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'audit: not a new named export';
    END IF;
    IF (SELECT count(*) FROM jsonb_object_keys(v_e.filters) k WHERE k NOT IN ('hospital', 'person', 'clearance', 'from', 'to')) > 0
       OR v_e.filters ->> 'hospital' IS DISTINCT FROM v_e.hospital_id::text THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'audit: not the filters of a named export';
    END IF;
    p_person := (v_e.filters ->> 'person')::uuid;
    p_clearance := (v_e.filters ->> 'clearance')::uuid;
    p_from := (v_e.filters ->> 'from')::timestamptz;
    p_to := (v_e.filters ->> 'to')::timestamptz;
    -- The entries and rows are append-only up to the snapshot: counted,
    -- then written by the same query.
    SELECT count(*) INTO v_n FROM public.mhai_audit_export_candidates(v_e.hospital_id, v_e.max_seq, p_person, p_clearance,
                                                                      p_from, p_to);
    IF v_n > p_max THEN
        RETURN v_n;
    END IF;
    INSERT INTO public.audit_export_rows (export_id, ord, ref, hospital_id, view_id, row_ord, seq)
    SELECT p_export, (row_number() OVER (ORDER BY t.seq, t.row_ord))::integer,
           substr(md5(gen_random_uuid()::text || clock_timestamp()::text || t.seq::text || t.row_ord::text), 1, 24),
           v_e.hospital_id, t.view_id, t.row_ord, t.seq
    FROM public.mhai_audit_export_candidates(v_e.hospital_id, v_e.max_seq, p_person, p_clearance, p_from, p_to) t;
    RETURN v_n;
END
$$;
-- +goose StatementEnd

-- mhai_audit_export_pids(export): a named export's rows in order, with
-- each entry's position, fingerprint, time and actor, the list's letter,
-- purpose and period, and the pid the hospital saw. Only in the export's
-- creation transaction or its one download transaction (departure 65),
-- elsewhere nothing; and only once that transaction holds the export's
-- (or the download's) audit entry (P0001 not_audited otherwise).
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_export_pids(p_export uuid)
    RETURNS TABLE (ref text, seq bigint, hash text, ts timestamptz, actor_id uuid, clearance_id uuid, purpose text,
                   level text, start date, pid text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_e record;
BEGIN
    SELECT e.owner, e.created_xact = pg_catalog.pg_current_xact_id() AS creating,
           e.download_xact IS NOT DISTINCT FROM pg_catalog.pg_current_xact_id() AS downloading
    INTO v_e FROM public.audit_exports e WHERE e.id = p_export AND e.kind = 'named_access';
    IF NOT FOUND OR NOT (v_e.creating OR v_e.downloading) THEN
        RETURN;
    END IF;
    -- The entry first (security reviews): no pid is read before the
    -- export's or the download's audit entry is written by this very
    -- transaction (its xmin), by the export's owner.
    IF NOT EXISTS (SELECT 1 FROM public.audit_log a
                   WHERE a.entity = 'audit_export' AND a.entity_id = p_export::text AND a.actor_id = v_e.owner
                     AND a.hospital_id IS NULL
                     AND a.action = CASE WHEN v_e.downloading THEN 'audit.export_downloaded' ELSE 'audit.named_access_exported' END
                     AND public.mhai_export_entry_mine(a.xmin)) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_audited';
    END IF;
    RETURN QUERY
    SELECT x.ref, x.seq, a.hash, a.ts, a.actor_id, v.clearance_id, v.purpose, p.level, p.start, s.pid
    FROM public.audit_export_rows x
    JOIN public.audit_log a ON a.id = x.seq AND a.action = 'analysis.named_viewed'
    JOIN public.analysis_views v ON v.hospital_id = x.hospital_id AND v.id = x.view_id
    JOIN public.analysis_view_rows r ON r.hospital_id = x.hospital_id AND r.view_id = x.view_id AND r.ord = x.row_ord
    JOIN public.analysis_periods p ON p.hospital_id = v.hospital_id AND p.id = v.period_id
    JOIN public.analysis_pseudonyms s ON s.hospital_id = v.hospital_id AND s.side = 'hospital' AND s.scope IS NULL
                                     AND s.period_id = v.period_id AND s.subject_id = r.subject_id
    WHERE x.export_id = p_export
    ORDER BY x.ord;
END
$$;
-- +goose StatementEnd

-- mhai_audit_letters(ids): the letters named, as the audit shows them: an
-- active letter past its end date in its hospital's zone is ended.
-- +goose StatementBegin
CREATE FUNCTION mhai_audit_letters(p_ids uuid[])
    RETURNS TABLE (id uuid, hospital_id uuid, status text, letter_date date, end_date date, verified_by uuid,
                   demo boolean, pages integer)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT c.id, c.hospital_id,
           CASE WHEN c.status = 'active' AND c.end_date < (now() AT TIME ZONE h.time_zone)::date THEN 'ended' ELSE c.status END,
           c.letter_date, c.end_date, c.verified_by, c.demo, c.pages
    FROM public.clearances c JOIN public.hospitals h ON h.id = c.hospital_id
    WHERE c.id = ANY (p_ids) AND cardinality(p_ids) <= 1000
$$;
-- +goose StatementEnd

-- mhai_legal_reference_create(export, reference, admins, by): binds a
-- named export of one hospital to one legal reference (design §8.9 step
-- 1). P0001 refusals: not_found (no such export, or not a named one),
-- forbidden (by isn't an active platform admin with two-step, or didn't
-- make the export), conflict (the export is bound, or the reference is
-- taken), too_many_rows (over 200), invalid (no rows, or a bad
-- reference), not_eligible (an admin who isn't an active hospital admin
-- of the export's hospital with two-step, or one named twice). The caller
-- appends legal_reference.created in the same transaction.
-- +goose StatementBegin
CREATE FUNCTION mhai_legal_reference_create(p_export uuid, p_reference text, p_admins uuid[], p_by uuid)
    RETURNS TABLE (id uuid, hospital_id uuid, export_rows integer, created_at timestamptz)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_e  record;
    v_n  integer;
    v_id uuid;
    v_at timestamptz;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('mhai.legal_reference'), hashtext(COALESCE(p_export::text, '')));
    PERFORM pg_advisory_xact_lock(hashtext('mhai.legal_reference_text'), hashtext(COALESCE(p_reference, '')));
    IF p_by IS NULL OR NOT EXISTS (
        SELECT 1 FROM public.users u
        JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'platform_admin'
        JOIN public.user_mfa m ON m.user_id = u.id AND m.enabled
        WHERE u.id = p_by AND u.hospital_id IS NULL AND u.is_active) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    SELECT e.kind, e.owner, e.hospital_id, e.rows INTO v_e FROM public.audit_exports e WHERE e.id = p_export;
    IF NOT FOUND OR v_e.kind <> 'named_access' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    IF v_e.owner <> p_by THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'forbidden';
    END IF;
    IF EXISTS (SELECT 1 FROM public.legal_references l WHERE l.export_id = p_export) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    -- The rows the export holds, counted (security review: not the
    -- record's column).
    SELECT count(*) INTO v_n FROM public.audit_export_rows x WHERE x.export_id = p_export;
    IF v_n > 200 OR v_e.rows > 200 THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'too_many_rows';
    END IF;
    IF v_n < 1 OR v_n <> v_e.rows OR p_reference IS NULL OR p_reference !~ '^[A-Za-z0-9][A-Za-z0-9 ._/:-]{0,59}$' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'invalid';
    END IF;
    IF p_admins IS NULL OR cardinality(p_admins) NOT BETWEEN 1 AND 3
       OR (SELECT count(DISTINCT a) FROM unnest(p_admins) a) <> cardinality(p_admins)
       OR (SELECT count(*) FROM public.users u
           WHERE u.id = ANY (p_admins) AND u.hospital_id = v_e.hospital_id AND u.is_active
             AND EXISTS (SELECT 1 FROM public.user_roles r WHERE r.user_id = u.id AND r.role = 'hospital_admin')
             AND EXISTS (SELECT 1 FROM public.user_mfa m WHERE m.user_id = u.id AND m.enabled)) <> cardinality(p_admins) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_eligible';
    END IF;
    IF EXISTS (SELECT 1 FROM public.legal_references l WHERE l.reference = p_reference) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'conflict';
    END IF;
    INSERT INTO public.legal_references AS l (export_id, hospital_id, reference, admins, created_by)
    VALUES (p_export, v_e.hospital_id, p_reference, p_admins, p_by)
    RETURNING l.id, l.created_at INTO v_id, v_at;
    id := v_id;
    hospital_id := v_e.hospital_id;
    export_rows := v_n;
    created_at := v_at;
    RETURN NEXT;
END
$$;
-- +goose StatementEnd

------------------------------------------------------------------------
-- The hospital's functions (design §8.9 step 2).
------------------------------------------------------------------------

-- mhai_legal_subjects(reference, key hash): resolves a reference once,
-- for a hospital admin named on it, at the session's hospital, covered by
-- a letter now (P0001 not_found: analysis off, or not such an admin;
-- named_access_off, not_cleared, clearance_ended, forbidden: the cover).
-- The first call marks it used; the same admin with the same key within
-- 15 minutes replays it, at most 3 times; anything else is
-- already_used. Returns the export's rows in order with each subject's
-- encrypted reference, the period, whether its list's letter is a demo
-- letter, the covering letter and the replay's number. The caller appends
-- analysis.legal_resolved in the same transaction.
-- +goose StatementBegin
CREATE FUNCTION mhai_legal_subjects(p_reference uuid, p_key bytea)
    RETURNS TABLE (ref text, level text, start date, ref_enc bytea, demo boolean, clearance_id uuid, export_id uuid,
                   replays integer)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h   uuid := public.mhai_current_hospital();
    v_u   uuid := public.mhai_current_user();
    v_l   record;
    v_cov record;
    v_rep integer;
BEGIN
    IF v_h IS NULL OR v_u IS NULL OR p_reference IS NULL OR public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    IF octet_length(p_key) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'legal: not a key';
    END IF;
    SELECT l.id, l.export_id, l.used_at, l.used_by, l.used_key, l.replays INTO v_l
    FROM public.legal_references l
    WHERE l.id = p_reference AND l.hospital_id = v_h AND v_u = ANY (l.admins)
    FOR UPDATE;
    IF NOT FOUND OR NOT EXISTS (
        SELECT 1 FROM public.users u JOIN public.user_roles r ON r.user_id = u.id AND r.role = 'hospital_admin'
        WHERE u.id = v_u AND u.hospital_id = v_h AND u.is_active) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    SELECT * INTO v_cov FROM public.mhai_named_cover(v_u);
    IF NOT COALESCE(v_cov.named, false) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = CASE v_cov.reason
            WHEN 'switched_off' THEN 'named_access_off'
            WHEN 'ended' THEN 'clearance_ended'
            WHEN 'role_or_account' THEN 'forbidden'
            ELSE 'not_cleared' END;
    END IF;
    -- Every row of the export still resolves (its list row, period,
    -- subject and letter), or none is sent (security review).
    IF (SELECT count(*) FROM public.audit_export_rows x WHERE x.export_id = v_l.export_id)
       <> (SELECT count(*) FROM public.audit_export_rows x
           JOIN public.analysis_views v ON v.hospital_id = x.hospital_id AND v.id = x.view_id
           JOIN public.analysis_view_rows r ON r.hospital_id = x.hospital_id AND r.view_id = x.view_id AND r.ord = x.row_ord
           JOIN public.analysis_periods p ON p.hospital_id = v.hospital_id AND p.id = v.period_id
           JOIN public.analysis_subjects s ON s.hospital_id = v.hospital_id AND s.subject_id = r.subject_id
           JOIN public.clearances c ON c.hospital_id = v.hospital_id AND c.id = v.clearance_id
           WHERE x.export_id = v_l.export_id AND x.hospital_id = v_h) THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'unavailable';
    END IF;
    IF v_l.used_at IS NULL THEN
        UPDATE public.legal_references l SET used_at = now(), used_by = v_u, used_key = p_key WHERE l.id = v_l.id;
        v_rep := 0;
    ELSIF v_l.used_by = v_u AND v_l.used_key = p_key AND now() < v_l.used_at + interval '15 minutes' AND v_l.replays < 3 THEN
        UPDATE public.legal_references l SET replays = l.replays + 1 WHERE l.id = v_l.id;
        v_rep := v_l.replays + 1;
    ELSE
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'already_used';
    END IF;
    RETURN QUERY
    SELECT x.ref, p.level, p.start, s.ref_enc, c.demo, v_cov.clearance_id, v_l.export_id, v_rep
    FROM public.audit_export_rows x
    JOIN public.analysis_views v ON v.hospital_id = x.hospital_id AND v.id = x.view_id
    JOIN public.analysis_view_rows r ON r.hospital_id = x.hospital_id AND r.view_id = x.view_id AND r.ord = x.row_ord
    JOIN public.analysis_periods p ON p.hospital_id = v.hospital_id AND p.id = v.period_id
    JOIN public.analysis_subjects s ON s.hospital_id = v.hospital_id AND s.subject_id = r.subject_id
    JOIN public.clearances c ON c.hospital_id = v.hospital_id AND c.id = v.clearance_id
    WHERE x.export_id = v_l.export_id AND x.hospital_id = v_h
    ORDER BY x.ord;
END
$$;
-- +goose StatementEnd

-- mhai_legal_references_mine(): the session person's unused references at
-- the session's hospital (not_found while analysis is off).
-- +goose StatementBegin
CREATE FUNCTION mhai_legal_references_mine()
    RETURNS TABLE (id uuid, reference text, export_rows integer, created_at timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
#variable_conflict use_column
DECLARE
    v_h uuid := public.mhai_current_hospital();
    v_u uuid := public.mhai_current_user();
BEGIN
    IF v_h IS NULL OR v_u IS NULL OR public.mhai_analysis_position(v_h) <> 'on' THEN
        RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'not_found';
    END IF;
    RETURN QUERY
    SELECT l.id, l.reference, e.rows, l.created_at
    FROM public.legal_references l JOIN public.audit_exports e ON e.id = l.export_id
    WHERE l.hospital_id = v_h AND v_u = ANY (l.admins) AND l.used_at IS NULL
    ORDER BY l.created_at DESC, l.id
    LIMIT 50;
END
$$;
-- +goose StatementEnd

ALTER FUNCTION mhai_export_exempt() OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_export_entry_mine(xid) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_export_rules() OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_export_audited() OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_legal_reference_audited() OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_legal_reference_rules() OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_export_candidates(uuid, bigint, uuid, uuid, timestamptz, timestamptz) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_export_named(uuid, integer) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_export_pids(uuid) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_audit_letters(uuid[]) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_legal_reference_create(uuid, text, uuid[], uuid) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_legal_subjects(uuid, bytea) OWNER TO mhai_export_rules;
ALTER FUNCTION mhai_legal_references_mine() OWNER TO mhai_export_rules;
REVOKE ALL ON FUNCTION mhai_export_exempt(), mhai_export_entry_mine(xid), mhai_audit_export_rules(),
    mhai_audit_export_candidates(uuid, bigint, uuid, uuid, timestamptz, timestamptz), mhai_audit_export_audited(),
    mhai_legal_reference_audited(), mhai_legal_reference_rules(),
    mhai_audit_export_named(uuid, integer), mhai_audit_export_pids(uuid),
    mhai_audit_letters(uuid[]), mhai_legal_reference_create(uuid, text, uuid[], uuid), mhai_legal_subjects(uuid, bytea),
    mhai_legal_references_mine() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_audit_export_named(uuid, integer),
    mhai_audit_export_pids(uuid), mhai_audit_letters(uuid[]), mhai_legal_reference_create(uuid, text, uuid[], uuid)
    TO mhai_platform;
GRANT EXECUTE ON FUNCTION mhai_legal_subjects(uuid, bytea), mhai_legal_references_mine() TO mhai_gateway;

------------------------------------------------------------------------
-- The platform's audit appends for these actions (as 00037's
-- platform_admin_append: no hospital on the row, an actor).
------------------------------------------------------------------------
CREATE POLICY platform_export_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.exported', 'audit.named_access_exported', 'audit.export_downloaded',
                           'audit.export_download_refused',
                           'legal_reference.created')
                AND hospital_id IS NULL AND actor_id IS NOT NULL);

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_export_rules', 'mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
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
    FOREACH r IN ARRAY ARRAY['mhai_export_rules', 'mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF NOT pg_has_role(current_user, r, 'MEMBER') THEN
            EXECUTE format('GRANT %I TO %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', 'yes', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

DROP POLICY platform_export_append ON audit_log;

DROP TABLE legal_references;
DROP TABLE audit_export_rows;
DROP TABLE audit_exports;
DROP INDEX audit_log_named_idx;
DROP INDEX audit_log_export_idx;

DROP FUNCTION mhai_legal_references_mine();
DROP FUNCTION mhai_legal_subjects(uuid, bytea);
DROP FUNCTION mhai_legal_reference_create(uuid, text, uuid[], uuid);
DROP FUNCTION mhai_audit_letters(uuid[]);
DROP FUNCTION mhai_audit_export_pids(uuid);
DROP FUNCTION mhai_audit_export_named(uuid, integer);
DROP FUNCTION mhai_audit_export_candidates(uuid, bigint, uuid, uuid, timestamptz, timestamptz);
DROP FUNCTION mhai_legal_reference_rules();
DROP FUNCTION mhai_legal_reference_audited();
DROP FUNCTION mhai_audit_export_audited();
DROP FUNCTION mhai_audit_export_rules();
DROP FUNCTION mhai_export_entry_mine(xid);
DROP FUNCTION mhai_export_exempt();

DROP POLICY export_rules ON hospitals;
DROP POLICY export_rules ON user_mfa;
DROP POLICY export_rules ON user_roles;
DROP POLICY export_rules ON users;
DROP POLICY export_rules ON clearances;
DROP POLICY export_rules ON analysis_subjects;
DROP POLICY export_rules ON analysis_periods;
DROP POLICY export_rules ON analysis_pseudonyms;
DROP POLICY export_rules ON analysis_view_rows;
DROP POLICY export_rules ON analysis_views;
DROP POLICY export_rules ON audit_log;
REVOKE ALL ON hospitals, user_mfa, user_roles, users, clearances, analysis_subjects, analysis_periods,
    analysis_pseudonyms, analysis_view_rows, analysis_views, audit_log FROM mhai_export_rules;
REVOKE ALL ON FUNCTION mhai_analysis_position(uuid), mhai_named_cover(uuid) FROM mhai_export_rules;
REVOKE USAGE ON SCHEMA public FROM mhai_export_rules;

-- +goose StatementBegin
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['mhai_export_rules', 'mhai_clearance_rules', 'mhai_analysis_reader'] LOOP
        IF current_setting('mhai.' || r || '_granted', true) = 'yes' THEN
            EXECUTE format('REVOKE %I FROM %I', r, current_user);
            PERFORM set_config('mhai.' || r || '_granted', '', false);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
