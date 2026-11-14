-- EMR integration, patient ids in questions (`pids`; EMR design §10.5,
-- plan Task 48, PR 13). Numbered 00033: 00032 is EMR PR 9's. Nothing here
-- is reachable while CORE_EMR_ENABLED is false: core refuses a question's
-- patient_id (404 not_found) without reading the database.
--
-- clinical_queries.patient_id: the MHAI patient a question is about, or
-- null. The key (hospital_id, patient_id) → emr.patients (hospital_id, id)
-- holds a question to a patient of its own hospital (ADR 0009 rule 5);
-- deleting the patient (a purge, a hospital's removal) clears the column
-- and nothing else.
--
-- A question names an active patient: a trigger checks it under a share
-- lock on the patient row, as 00031 does for alerts, so an exclusion or a
-- merge that commits while core is inserting either waits for the insert
-- (and then clears it, below) or is seen by it (and the insert is refused).
--
-- A patient that stops being active (excluded or merged away) is unlinked
-- from every question that names it, in the same transaction (plan
-- departure 57): the questions stay, about nobody, and core mints no `pids`
-- claim for them again.
--
-- Roles:
--   mhai_gateway  inserts patient_id (its table INSERT grant; RLS as for
--                 the rest of the row).
--   mhai_jobs     reads patient_id, to re-mint a queued question's job
--                 with the patient core checks again (sweeper, retry,
--                 dead letters).
--   mhai_migrate  the two trigger functions (definers, owner, search_path
--                 pinned, schema-qualified, EXECUTE for nobody); its
--                 policies let it clear the column, in the patient's
--                 hospital only, and nothing else.

-- +goose Up

ALTER TABLE clinical_queries
    ADD COLUMN patient_id uuid,
    ADD CONSTRAINT clinical_queries_patient FOREIGN KEY (hospital_id, patient_id)
        REFERENCES emr.patients (hospital_id, id) ON DELETE SET NULL (patient_id);
CREATE INDEX clinical_queries_patient ON clinical_queries (hospital_id, patient_id) WHERE patient_id IS NOT NULL;

GRANT SELECT (patient_id) ON clinical_queries TO mhai_jobs;

------------------------------------------------------------------------
-- A question names an active patient of its hospital.
------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION mhai_query_patient_check() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    prev text := current_setting('mhai.hospital_id', true);
    ok   boolean;
BEGIN
    IF NEW.patient_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- A caller bound to a hospital writes its own hospital's questions
    -- only: refused here, before the lock, so it can neither probe nor
    -- hold another hospital's patient row.
    IF nullif(prev, '') IS NOT NULL AND nullif(prev, '')::uuid IS DISTINCT FROM NEW.hospital_id THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege', MESSAGE = 'question patient: another hospital';
    END IF;
    -- The owner's policy on the patients is scoped to the hospital set:
    -- the question's own, for this statement, then put back.
    PERFORM set_config('mhai.hospital_id', NEW.hospital_id::text, true);
    PERFORM 1 FROM emr.patients p
    WHERE p.hospital_id = NEW.hospital_id AND p.id = NEW.patient_id AND p.status = 'active'
    FOR SHARE;
    ok := FOUND;
    PERFORM set_config('mhai.hospital_id', coalesce(prev, ''), true);
    IF NOT ok THEN
        RAISE EXCEPTION USING ERRCODE = 'check_violation', MESSAGE = 'question patient: not an active patient of the hospital';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_query_patient_check() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_query_patient_check() FROM PUBLIC;
CREATE TRIGGER clinical_queries_patient_check BEFORE INSERT OR UPDATE OF patient_id ON clinical_queries
    FOR EACH ROW EXECUTE FUNCTION mhai_query_patient_check();

------------------------------------------------------------------------
-- A patient that stops being active is unlinked from its questions.
------------------------------------------------------------------------
-- The owner sees and changes only questions of the hospital set that name
-- a patient, and may only clear the column.
CREATE POLICY patient_unlink_read ON clinical_queries FOR SELECT TO mhai_migrate
    USING (hospital_id = mhai_current_hospital() AND patient_id IS NOT NULL);
CREATE POLICY patient_unlink ON clinical_queries FOR UPDATE TO mhai_migrate
    USING (hospital_id = mhai_current_hospital() AND patient_id IS NOT NULL)
    WITH CHECK (hospital_id = mhai_current_hospital() AND patient_id IS NULL);

-- +goose StatementBegin
CREATE FUNCTION mhai_query_patient_unlink() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    prev text := current_setting('mhai.hospital_id', true);
BEGIN
    PERFORM set_config('mhai.hospital_id', NEW.hospital_id::text, true);
    UPDATE public.clinical_queries q SET patient_id = NULL
    WHERE q.hospital_id = NEW.hospital_id AND q.patient_id = NEW.id;
    PERFORM set_config('mhai.hospital_id', coalesce(prev, ''), true);
    RETURN NULL;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_query_patient_unlink() OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_query_patient_unlink() FROM PUBLIC;
CREATE TRIGGER patients_unlink_queries AFTER UPDATE OF status ON emr.patients
    FOR EACH ROW WHEN (OLD.status = 'active' AND NEW.status <> 'active')
    EXECUTE FUNCTION mhai_query_patient_unlink();

-- +goose Down

DROP TRIGGER patients_unlink_queries ON emr.patients;
DROP FUNCTION mhai_query_patient_unlink();
DROP POLICY patient_unlink ON clinical_queries;
DROP POLICY patient_unlink_read ON clinical_queries;
DROP TRIGGER clinical_queries_patient_check ON clinical_queries;
DROP FUNCTION mhai_query_patient_check();
REVOKE SELECT (patient_id) ON clinical_queries FROM mhai_jobs;
DROP INDEX clinical_queries_patient;
ALTER TABLE clinical_queries DROP CONSTRAINT clinical_queries_patient, DROP COLUMN patient_id;
