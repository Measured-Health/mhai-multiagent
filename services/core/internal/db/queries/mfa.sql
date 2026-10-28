-- Queries for two-step verification, step-up and password change
-- (internal/auth: mfa.go, stepup.go, password.go), all run as mhai_auth via
-- db.WithAuth. The Go code holds the same SQL inline until the auth tasks
-- adopt sqlc-generated code. Grants used (00003): INSERT and column UPDATE
-- (secret_enc, enabled, last_used_step, backup_code_hashes) on user_mfa;
-- column UPDATE (password_hash, must_change_password) on users; full on
-- sessions and trusted_devices.

-- name: GetUserMFA :one
SELECT secret_enc, enabled, last_used_step FROM user_mfa WHERE user_id = $1;

-- name: UpsertPendingMFA :execrows
-- secret_enc = nonce || AES-256-GCM(secret); hashes = HMAC-SHA256 of each
-- backup code. Never replaces an enabled enrolment (0 rows = conflict).
INSERT INTO user_mfa (user_id, secret_enc, enabled, last_used_step, backup_code_hashes)
VALUES ($1, $2, false, NULL, $3)
ON CONFLICT (user_id) DO UPDATE
SET secret_enc = EXCLUDED.secret_enc, enabled = false, last_used_step = NULL,
    backup_code_hashes = EXCLUDED.backup_code_hashes
WHERE NOT user_mfa.enabled;

-- name: UseTOTPStep :execrows
-- 1 row only if the step is newer than the last one used: a code can't be
-- used twice, even by two concurrent requests.
UPDATE user_mfa SET last_used_step = $2
WHERE user_id = $1 AND (last_used_step IS NULL OR last_used_step < $2);

-- name: UseBackupCode :execrows
UPDATE user_mfa SET backup_code_hashes = array_remove(backup_code_hashes, $2)
WHERE user_id = $1 AND enabled AND $2 = ANY (backup_code_hashes);

-- name: EnableMFA :exec
UPDATE user_mfa SET enabled = true WHERE user_id = $1;

-- name: MarkSessionMFA :exec
UPDATE sessions SET mfa = true,
       amr = CASE WHEN 'otp' = ANY (amr) THEN amr ELSE array_append(amr, 'otp') END
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: ReplaceBackupCodes :execrows
UPDATE user_mfa SET backup_code_hashes = $2 WHERE user_id = $1 AND enabled;

-- name: InsertTrustedDevice :exec
-- token_hash = hex(sha256(device_token)); expires 30 days on.
INSERT INTO trusted_devices (id, user_id, token_hash, created_at, expires_at) VALUES ($1, $2, $3, $4, $5);

-- name: ChangePassword :exec
UPDATE users SET password_hash = $2, must_change_password = false WHERE id = $1;

-- name: RevokeOtherSessions :many
UPDATE sessions SET revoked_at = $3
WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL
RETURNING id, access_jti;

-- name: DeleteTrustedDevices :exec
DELETE FROM trusted_devices WHERE user_id = $1;
