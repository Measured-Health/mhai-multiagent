-- The platform app's hospitals list and a hospital's page (mhai-web Plan
-- 7b; platform admin design §7.1, §7.3). NUMBERED 00036: EMR PR 13 takes
-- 00033, Analysis PR 7 00034 and the hospital admin routes 00035.
--
-- mhai_platform already reads hospitals, users (no password hash),
-- user_roles and sessions (00003). This adds the legal holds' columns, read
-- only, so the list counts a hospital's open holds and its page lists them.
-- A hold carries a document reference, never a name. Placing and lifting
-- stay the provisioning login's (00032).

-- +goose Up
GRANT SELECT (id, hospital_id, reference, placed_at, lifted_at) ON legal_holds TO mhai_platform;
CREATE POLICY platform_read ON legal_holds FOR SELECT TO mhai_platform USING (true);

-- +goose Down
DROP POLICY platform_read ON legal_holds;
REVOKE SELECT (id, hospital_id, reference, placed_at, lifted_at) ON legal_holds FROM mhai_platform;
