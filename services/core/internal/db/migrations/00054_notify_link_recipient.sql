-- The recipient of a secret link (notifications design §4.4, §6.3; §15
-- PR 5; ADR 0018). Numbered 00054, after the outbox (00053).
--
-- notify_link_recipient(user_id, purpose), EXECUTE for mhai_notify only,
-- runs under the hospital of core's request token (dbx.WithHospital). It
-- answers the user's address and the hospital's display name only when the
-- user is of that hospital, active and not removed, the hospital is active
-- or still being set up, and the user holds a live account_tokens row of
-- that purpose (unused, unrevoked, unexpired) created in the last 2
-- minutes: every link email is tied to a token core has just issued (NT8),
-- and request data never names an address (NT6). Otherwise it answers
-- nothing. It never reads the token's hash.
--
-- The owner reads users through 00003's pairing_check policy; it gains a
-- read of account_tokens and of hospitals, for this definer.

-- +goose Up
CREATE POLICY owner_link_read ON account_tokens FOR SELECT TO mhai_migrate USING (true);
CREATE POLICY owner_link_read ON hospitals FOR SELECT TO mhai_migrate USING (true);

-- +goose StatementBegin
CREATE FUNCTION notify_link_recipient(p_user uuid, p_purpose text)
    RETURNS TABLE (address text, hospital_name text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT u.email::text, h.name
    FROM public.users u JOIN public.hospitals h ON h.id = u.hospital_id
    WHERE u.id = p_user AND u.hospital_id = public.mhai_current_hospital()
      AND u.is_active AND u.removed_at IS NULL AND h.status IN ('active', 'setup_incomplete')
      AND p_purpose IN ('invite', 'password_reset')
      AND EXISTS (SELECT 1 FROM public.account_tokens t
                  WHERE t.user_id = u.id AND t.hospital_id = u.hospital_id AND t.purpose = p_purpose
                    AND t.used_at IS NULL AND t.revoked_at IS NULL AND t.expires_at > now()
                    AND t.created_at > now() - interval '2 minutes')
$$;
-- +goose StatementEnd
ALTER FUNCTION notify_link_recipient(uuid, text) OWNER TO mhai_migrate;
REVOKE ALL ON FUNCTION notify_link_recipient(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notify_link_recipient(uuid, text) TO mhai_notify;

-- +goose Down
DROP FUNCTION notify_link_recipient(uuid, text);
DROP POLICY owner_link_read ON hospitals;
DROP POLICY owner_link_read ON account_tokens;
