-- Queries for sign-in, refresh, logout, sessions and terms (internal/auth),
-- all run as mhai_auth via db.WithAuth. The Go code holds the same SQL
-- inline until the auth tasks adopt sqlc-generated code.

-- name: GetAccountByEmail :one
-- Pass NormaliseEmail(login); citext matches case-insensitively.
SELECT u.id, u.hospital_id, u.email, u.password_hash, u.display_name, u.full_name, u.role_title, u.specialty,
       u.is_active, u.must_change_password, u.terms_accepted_at,
       h.name, h.status,
       COALESCE(hs.idle_timeout_minutes, 15), COALESCE(hs.review_overdue_hours, 4),
       COALESCE(hs.require_mfa_for_clinicians, false), COALESCE(hs.allow_trust_device, false),
       COALESCE(m.enabled, false), COALESCE(cardinality(m.backup_code_hashes), 0),
       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
FROM users u
LEFT JOIN hospitals h          ON h.id = u.hospital_id
LEFT JOIN hospital_settings hs ON hs.hospital_id = u.hospital_id
LEFT JOIN user_mfa m           ON m.user_id = u.id
WHERE u.email = $1;

-- name: InsertSession :exec
INSERT INTO sessions (id, user_id, created_at, last_seen_at, ua_summary, ip_prefix, access_jti, access_expires_at, mfa, amr)
VALUES ($1, $2, $3, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertRefreshToken :exec
-- token_hash is hex(sha256(token)); the token itself is never stored.
INSERT INTO refresh_tokens (id, session_id, token_hash, issued_at) VALUES ($1, $2, $3, $4);

-- name: LockRefreshToken :one
-- Serialises concurrent refreshes of one token (Review Focus 2).
SELECT rt.id, rt.rotated_at, s.id, s.revoked_at, s.last_seen_at, s.user_id, s.mfa, s.amr, s.access_jti
FROM refresh_tokens rt JOIN sessions s ON s.id = rt.session_id
WHERE rt.token_hash = $1
FOR UPDATE OF rt, s;

-- name: RotateRefreshToken :exec
UPDATE refresh_tokens SET rotated_at = $2, successor_id = $3 WHERE id = $1;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = $2, access_jti = $3, access_expires_at = $4 WHERE id = $1;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeUserSessions :many
UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL RETURNING id, access_jti;

-- name: ListUserSessions :many
SELECT id, created_at, last_seen_at, ua_summary, ip_prefix FROM sessions
WHERE user_id = $1 AND revoked_at IS NULL ORDER BY last_seen_at DESC, id;

-- name: TrustedDeviceExists :one
SELECT EXISTS (SELECT 1 FROM trusted_devices WHERE user_id = $1 AND token_hash = $2 AND expires_at > $3);

-- name: AcceptTerms :execrows
UPDATE users SET terms_version = $2, terms_accepted_at = $3 WHERE id = $1;

-- name: SetLastLogin :exec
UPDATE users SET last_login_at = $2 WHERE id = $1;

-- name: RehashPassword :exec
UPDATE users SET password_hash = $2 WHERE id = $1;
