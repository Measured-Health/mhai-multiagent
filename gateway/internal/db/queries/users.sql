-- Queries for accounts. The seed CLI (internal/account) runs the Insert*
-- statements on the owner/migrate connection with the same SQL; the Get*
-- queries are for the sign-in path (mhai_auth), added to sqlc's dbq package
-- when the first auth task adopts generated code.

-- name: GetHospitalIDByShortCode :one
SELECT id FROM hospitals WHERE short_code = $1;

-- name: InsertHospital :exec
-- The caller supplies the id: mhai_provision can read only hospitals(id, short_code).
INSERT INTO hospitals (id, name, short_code, time_zone) VALUES ($1, $2, $3, $4);

-- name: InsertHospitalSettings :exec
INSERT INTO hospital_settings (hospital_id, require_mfa_for_clinicians, allow_trust_device)
VALUES ($1, $2, $3);

-- name: InsertUser :exec
-- The caller supplies the id (mhai_provision cannot read users back). email must
-- already be NormaliseEmail'd; must_change_password starts true.
INSERT INTO users (id, hospital_id, email, password_hash, display_name, must_change_password)
VALUES ($1, $2, $3, $4, $5, true);

-- name: InsertUserRole :exec
INSERT INTO user_roles (user_id, role) VALUES ($1, $2);

-- name: GetUserByEmail :one
-- Pass NormaliseEmail(login). The column is citext, so the match ignores case
-- even for rows stored before normalisation.
SELECT id, hospital_id, email, password_hash, display_name, is_active, must_change_password
FROM users WHERE email = $1;
