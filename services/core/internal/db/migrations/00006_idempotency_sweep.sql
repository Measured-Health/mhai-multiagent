-- The periodic sweep of expired Idempotency-Key rows (Task 9).
--
-- Rows belong to every hospital and to none, and row-level security keeps
-- each app role to its own slice, so no app role can sweep them all. The
-- sweep is a SECURITY DEFINER function owned by mhai_migrate. Its policies
-- let it see and delete only rows that have expired, and only mhai_auth may
-- run it. The core service runs it on a timer (db.SweepIdempotency).

-- +goose Up
CREATE POLICY sweep_read ON idempotency_keys FOR SELECT TO mhai_migrate
    USING (expires_at < now());
CREATE POLICY sweep_delete ON idempotency_keys FOR DELETE TO mhai_migrate
    USING (expires_at < now());

-- Deletes at most batch expired rows and returns how many it deleted.
-- +goose StatementBegin
CREATE FUNCTION mhai_sweep_idempotency(batch integer) RETURNS bigint
    LANGUAGE sql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    WITH doomed AS (
        SELECT k.scope, k.key FROM public.idempotency_keys k
        WHERE k.expires_at < now()
        ORDER BY k.expires_at
        LIMIT GREATEST(LEAST(batch, 10000), 1)
    ), gone AS (
        DELETE FROM public.idempotency_keys k USING doomed d
        WHERE k.scope = d.scope AND k.key = d.key AND k.expires_at < now()
        RETURNING 1
    )
    SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
ALTER FUNCTION mhai_sweep_idempotency(integer) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION mhai_sweep_idempotency(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mhai_sweep_idempotency(integer) TO mhai_auth;

-- +goose Down
DROP FUNCTION mhai_sweep_idempotency(integer);
DROP POLICY sweep_delete ON idempotency_keys;
DROP POLICY sweep_read ON idempotency_keys;
