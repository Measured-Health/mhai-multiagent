-- Cloud models: the stale budget reservation reaper (cloud model providers
-- plan, failure case 4 and the TODO "stale budget reservations").
--
-- A reservation is settled by the worker through mhai_cloud_settle (00018).
-- Three cases leave its cloud_reservations row behind: a crashed worker, a
-- reserve that timed out in Python after core committed, and a settle that
-- failed (mhai_cloud_settle_failures_total). Each held its tokens as
-- "reserved" until the month ended. Core's reaper loop now settles every
-- row older than its age (one hour by default, never under 30 minutes) as
-- fully used.
--
-- The rule (settle against reap): the reservation row is consumed exactly
-- once, by whichever DELETE commits first. The reaper's DELETE locks its
-- rows with SKIP LOCKED, so a settle in progress keeps its row; a settle
-- that arrives after the reaper finds no row, returns false and changes
-- nothing (00018: "one already settled, changes nothing"). The reaper
-- charges the whole reservation (reserved -> used), the same as a settle
-- with an unknown use, so it never frees budget: used + reserved stays the
-- same, and a late reply that used less is not refunded. It fails toward
-- sending less, never more, and reserved can't go negative (each row's
-- tokens leave reserved once; the CHECK would roll the reap back).
--
-- mhai_cloud_reaper: a NOLOGIN role core's login reaches by SET LOCAL ROLE
-- only (WITH INHERIT FALSE; db.CheckLoginRole refuses a login that inherits
-- it). It holds EXECUTE on the two functions below and its audit append,
-- nothing on the ledger tables. mhai_cloud_reap is bound to
-- mhai_current_hospital(), so one call reaps one hospital and the audit row
-- (cloud.reservation_reaped: ids and counts only) is written under that
-- hospital in the same transaction: a reap and its audit commit together.
--
-- Every definer function follows 00018: owner mhai_migrate, search_path
-- pinned to pg_catalog, pg_temp, every relation written public.<name>, no
-- PUBLIC execute, EXECUTE for its one role. Under FORCE the owner reads and
-- writes the ledger through 00018's owner policies (read, update, delete),
-- plus owner_reap_lock below (lock, never update).
--
-- Additive: no table changes shape. With no stale row the reaper does
-- nothing.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mhai_cloud_reaper') THEN
        CREATE ROLE mhai_cloud_reaper NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd
GRANT mhai_cloud_reaper TO mhai_app WITH INHERIT FALSE;

-- The reap locks its rows (SELECT ... FOR UPDATE SKIP LOCKED), and under
-- FORCE a row lock needs an UPDATE policy as well as a SELECT one (as
-- 00003's pairing_lock): without it the owner's locking read sees no row and
-- the reap silently finds nothing. This one lets the owner lock the visited
-- hospital's rows and update none (WITH CHECK false).
CREATE POLICY owner_reap_lock ON cloud_reservations FOR UPDATE TO mhai_migrate
    USING (hospital_id = mhai_current_hospital()) WITH CHECK (false);

-- The reaper's scan: rows by age.
CREATE INDEX cloud_reservations_created ON cloud_reservations (created_at);

-- The hospitals holding a reservation older than p_age (at most 1000 per
-- call; the loop's next run takes the rest). p_age is at least 30 minutes:
-- a reservation younger than that may still be in flight.
-- +goose StatementBegin
CREATE FUNCTION mhai_cloud_stale_hospitals(p_age interval) RETURNS SETOF uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    IF p_age IS NULL OR p_age < interval '30 minutes' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_stale_hospitals: the age is at least 30 minutes';
    END IF;
    RETURN QUERY
    SELECT DISTINCT r.hospital_id FROM public.cloud_reservations r
    WHERE r.created_at < now() - p_age
    ORDER BY 1
    LIMIT 1000;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cloud_stale_hospitals(interval) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cloud_stale_hospitals(interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_cloud_stale_hospitals(interval) TO mhai_cloud_reaper;

-- Settle as fully used up to p_limit of the caller's hospital's
-- reservations older than p_age, oldest first, and return them. A row a
-- settle holds is skipped (SKIP LOCKED); a row already settled is gone.
-- Each row's tokens move from reserved to used on its own month, once.
-- +goose StatementBegin
CREATE FUNCTION mhai_cloud_reap(p_age interval, p_limit integer)
    RETURNS TABLE (reservation uuid, month date, tokens bigint, age_seconds bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    r record;
BEGIN
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege',
            MESSAGE = 'mhai_cloud_reap: no hospital';
    END IF;
    IF p_age IS NULL OR p_age < interval '30 minutes' THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reap: the age is at least 30 minutes';
    END IF;
    IF p_limit IS NULL OR p_limit < 1 OR p_limit > 1000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reap: the limit is 1 to 1000';
    END IF;
    FOR r IN
        DELETE FROM public.cloud_reservations d
        WHERE d.hospital_id = h
          AND d.id IN (SELECT s.id FROM public.cloud_reservations s
                       WHERE s.hospital_id = h AND s.created_at < now() - p_age
                       ORDER BY s.created_at, s.id
                       LIMIT p_limit
                       FOR UPDATE SKIP LOCKED)
        RETURNING d.id, d.month, d.tokens, d.created_at
    LOOP
        UPDATE public.cloud_usage u
        SET reserved = u.reserved - r.tokens,
            used = u.used + r.tokens
        WHERE u.hospital_id = h AND u.month = r.month;
        IF NOT FOUND THEN
            -- A reservation without its month's row: the ledger is not as
            -- reserve left it. Roll the whole reap back.
            RAISE EXCEPTION USING ERRCODE = 'data_exception',
                MESSAGE = 'mhai_cloud_reap: a reservation has no ledger row';
        END IF;
        reservation := r.id;
        month := r.month;
        tokens := r.tokens;
        age_seconds := greatest(extract(epoch FROM now() - r.created_at), 0)::bigint;
        RETURN NEXT;
    END LOOP;
END
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_cloud_reap(interval, integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_cloud_reap(interval, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_cloud_reap(interval, integer) TO mhai_cloud_reaper;

------------------------------------------------------------------------
-- The audit append: the visited hospital, no actor, its one action.
------------------------------------------------------------------------
GRANT INSERT ON audit_log TO mhai_cloud_reaper;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO mhai_cloud_reaper;
GRANT EXECUTE ON FUNCTION audit_chain_head() TO mhai_cloud_reaper;
CREATE POLICY cloud_reaper_append ON audit_log FOR INSERT TO mhai_cloud_reaper
    WITH CHECK (hospital_id = mhai_current_hospital() AND actor_id IS NULL
                AND action = 'cloud.reservation_reaped');

-- +goose Down

DROP POLICY cloud_reaper_append ON audit_log;
REVOKE EXECUTE ON FUNCTION audit_chain_head() FROM mhai_cloud_reaper;
REVOKE USAGE ON SEQUENCE audit_log_id_seq FROM mhai_cloud_reaper;
REVOKE INSERT ON audit_log FROM mhai_cloud_reaper;

DROP FUNCTION mhai_cloud_reap(interval, integer);
DROP FUNCTION mhai_cloud_stale_hospitals(interval);
DROP INDEX cloud_reservations_created;
DROP POLICY owner_reap_lock ON cloud_reservations;

REVOKE mhai_cloud_reaper FROM mhai_app;
