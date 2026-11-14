-- A clinician's patient lookup by MRN (POST /patients/lookup; TODO.md's
-- design note "Clinician patient lookup by MRN", approved 14 Nov 2026).
-- NUMBERED 00039: 00038 is Patient Analysis PR 7's (letters).
--
-- The lookup itself reads emr.patients as mhai_gateway (00023's column
-- grant and gateway_read policy: the caller's hospital only); nothing here
-- widens that. What this adds is the lookup's rate limits, which count
-- misses as well as hits so MRNs can't be enumerated:
--
--   - patient_lookup_counts: one row per clinician and hospital: the
--     lookups in the last minute (their times, a sliding window), the
--     lookups of the hospital day, and the day a refusal was last flagged.
--   - patient_lookup_hospital_counts: one row per hospital: its lookups of
--     the hospital day.
--   - mhai_patient_lookup_reserve(per minute, per day, hospital per day,
--     time zone): takes one lookup for the caller (mhai.hospital_id and
--     mhai.user_id) under the two rows' locks (the clinician's, then, only
--     if the clinician is within their own limits, the hospital's: parallel
--     lookups queue, so the counts are exact) and
--     answers 'ok', or the limit that refuses ('minute', 'day',
--     'hospital_day'), with first_refusal true on the clinician's first
--     refusal of the hospital day (core writes the one flagged audit entry
--     then, in the same transaction). Core calls it in its own transaction
--     and commits before it looks the patient up (reserve, then check). A
--     refused lookup takes nothing.
--
-- The time zone is the hospital's, read by the caller (mhai_gateway, its
-- own hospital's row) in the same statement: the owner's only policy on
-- hospitals is 00009's (inactive hospitals), and this migration doesn't
-- widen what any mhai_migrate definer can read.
--
-- Roles: no application role holds anything on either table; mhai_gateway
-- may only EXECUTE the function. The owner's policies admit the caller's
-- hospital only.

-- +goose Up

CREATE TABLE patient_lookup_counts (
    hospital_id uuid NOT NULL REFERENCES hospitals (id),
    user_id     uuid NOT NULL,
    day         date NOT NULL,
    day_count   integer NOT NULL DEFAULT 0 CHECK (day_count >= 0),
    recent      timestamptz[] NOT NULL DEFAULT '{}' CHECK (cardinality(recent) <= 100),
    flagged_on  date,
    PRIMARY KEY (hospital_id, user_id)
);
CREATE TABLE patient_lookup_hospital_counts (
    hospital_id uuid PRIMARY KEY REFERENCES hospitals (id),
    day         date NOT NULL,
    day_count   integer NOT NULL DEFAULT 0 CHECK (day_count >= 0)
);
ALTER TABLE patient_lookup_counts OWNER TO mhai_migrate;
ALTER TABLE patient_lookup_hospital_counts OWNER TO mhai_migrate;
ALTER TABLE patient_lookup_counts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
ALTER TABLE patient_lookup_hospital_counts ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON patient_lookup_counts, patient_lookup_hospital_counts FROM PUBLIC;
-- The definer below runs as the owner, bound to the caller's hospital.
CREATE POLICY owner_scoped ON patient_lookup_counts TO mhai_migrate
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());
CREATE POLICY owner_scoped ON patient_lookup_hospital_counts TO mhai_migrate
    USING (hospital_id = public.mhai_current_hospital()) WITH CHECK (hospital_id = public.mhai_current_hospital());

-- +goose StatementBegin
CREATE FUNCTION mhai_patient_lookup_reserve(p_minute integer, p_day integer, p_hospital_day integer, p_time_zone text,
                                            OUT admit text, OUT first_refusal boolean)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    v_h      uuid := public.mhai_current_hospital();
    v_u      uuid := public.mhai_current_user();
    v_now    timestamptz;
    v_day    date;
    v_recent timestamptz[];
    c        public.patient_lookup_counts%ROWTYPE;
    hc       public.patient_lookup_hospital_counts%ROWTYPE;
BEGIN
    IF v_h IS NULL OR v_u IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege', MESSAGE = 'patient lookup: no caller';
    END IF;
    IF p_minute IS NULL OR p_minute < 1 OR p_minute > 100 OR p_day IS NULL OR p_day < 1 OR p_day > 1000
       OR p_hospital_day IS NULL OR p_hospital_day < 1 OR p_hospital_day > 100000 OR p_time_zone IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value', MESSAGE = 'patient lookup: not a limit';
    END IF;
    -- An unknown time zone raises here, before anything is written.
    v_day := (clock_timestamp() AT TIME ZONE p_time_zone)::date;
    -- The clinician's row first. A clinician already past their own
    -- limit is refused under that lock alone: the hospital's row is
    -- neither locked nor written, so looping refusals don't queue behind
    -- the hospital's lookups (security review).
    INSERT INTO public.patient_lookup_counts (hospital_id, user_id, day) VALUES (v_h, v_u, v_day)
        ON CONFLICT DO NOTHING;
    SELECT * INTO STRICT c FROM public.patient_lookup_counts r
        WHERE r.hospital_id = v_h AND r.user_id = v_u FOR UPDATE;
    -- The wall clock once the lock is held (not the transaction's start):
    -- a lookup that waited behind another is timed after it.
    v_now := clock_timestamp();
    v_day := (v_now AT TIME ZONE p_time_zone)::date;
    v_recent := ARRAY(SELECT t FROM unnest(c.recent) t WHERE t > v_now - interval '1 minute' ORDER BY t);
    -- A day only moves forward: a later call with an earlier date (another
    -- time zone) never resets the count.
    IF c.day < v_day THEN
        c.day := v_day;
        c.day_count := 0;
    END IF;
    admit := CASE WHEN cardinality(v_recent) >= p_minute THEN 'minute'
                  WHEN c.day_count >= p_day THEN 'day'
                  ELSE 'ok' END;
    IF admit = 'ok' THEN
        -- Then the hospital's row (always after the clinician's).
        INSERT INTO public.patient_lookup_hospital_counts (hospital_id, day) VALUES (v_h, v_day)
            ON CONFLICT DO NOTHING;
        SELECT * INTO STRICT hc FROM public.patient_lookup_hospital_counts r
            WHERE r.hospital_id = v_h FOR UPDATE;
        IF hc.day < v_day THEN
            hc.day := v_day;
            hc.day_count := 0;
        END IF;
        IF hc.day_count >= p_hospital_day THEN
            admit := 'hospital_day';
        ELSE
            v_recent := v_recent || v_now;
            c.day_count := c.day_count + 1;
            UPDATE public.patient_lookup_hospital_counts r SET day = hc.day, day_count = hc.day_count + 1
                WHERE r.hospital_id = v_h;
        END IF;
    END IF;
    first_refusal := admit <> 'ok' AND c.flagged_on IS DISTINCT FROM c.day;
    IF first_refusal THEN
        c.flagged_on := c.day;
    END IF;
    -- A refusal that flags nothing changes nothing.
    IF admit = 'ok' OR first_refusal THEN
        UPDATE public.patient_lookup_counts r SET day = c.day, day_count = c.day_count, recent = v_recent,
            flagged_on = c.flagged_on
            WHERE r.hospital_id = v_h AND r.user_id = v_u;
    END IF;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_patient_lookup_reserve(integer, integer, integer, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_patient_lookup_reserve(integer, integer, integer, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_patient_lookup_reserve(integer, integer, integer, text) TO mhai_gateway;

-- +goose Down
DROP FUNCTION mhai_patient_lookup_reserve(integer, integer, integer, text);
DROP TABLE patient_lookup_hospital_counts;
DROP TABLE patient_lookup_counts;
