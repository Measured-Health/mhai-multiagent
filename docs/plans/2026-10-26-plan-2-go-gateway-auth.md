# Core service: foundation, sign-in, roles and tenancy (Plan 2): implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the core service's foundation and its authentication and authorization, so mhai-web's two apps can sign in for real against it. Hospitals stay separate in the code and in the database.

**Architecture:** The Go service lives in `services/core/`. It owns the Postgres schema through goose migrations, talks to NATS for shared short-lived state, and serves the frontend contract's §2 and §3 auth routes under `/api/v1`. Each request runs in a transaction that sets the hospital for row-level security. The Python side is not touched, except for removing the old `init-db` schema.

**Tech stack:**
- Go 1.24 and `net/http` (Go 1.22+ routing patterns)
- `pgx/v5`, with `sqlc` for queries and `goose/v3` for migrations
- `nats.go` (core and KV)
- `golang-jwt/jwt/v5` (EdDSA)
- `golang.org/x/crypto/argon2`
- `pquerna/otp`
- `testcontainers-go` for integration tests (Postgres 16 and NATS 2.10)

**Spec:** `docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`, steps 1–3 of its §10, and ADR 0010.

**The contract:** `docs/specs/2026-10-17-frontend-requirements.md` §2, §3, §14.1 and §14.6. Shapes and error codes are copied from there. Where it and the older gateway design disagree, it wins (design §8a).

## Global Constraints

- Base path `/api/v1`. JSON only. Request bodies are capped at 64 KB, and unknown fields are rejected with **422 `validation_failed`**.
- **Errors** are `application/problem+json` `{type, title, status, code, detail, request_id}`. On platform routes (`X-MHAI-Audience: platform`) the body is `{code}` only. `detail` is fixed per code and never echoes input or error text.
- **Error codes and statuses:**

  | Code | Status |
  |---|---|
  | `unauthenticated` | 401 |
  | `token_expired` | 401 |
  | `mfa_required` | 403 |
  | `mfa_invalid` | 403 |
  | `mfa_locked` | 429 |
  | `forbidden` | 403 |
  | `not_platform` | 403 |
  | `not_found` | 404 |
  | `conflict` | 409 |
  | `validation_failed` | 422 |
  | `weak_password` | 422 |
  | `rate_limited` | 429 |
  | `login_locked` | 429 |
  | `not_ready` | 503 |
  | `internal_error` | 500 |

- **Logs:**
  - never contain a password, token, TOTP code, backup code, email address or PHI;
  - user ids, hospital ids and request ids are fine.
- **Ownership:** another hospital's resource is **404**, never 403.
- **Identity:** the hospital, user and roles come only from the verified token, never from a request body, query or header.
- **Response headers:** every response has `X-Request-ID`. Auth responses also send `Cache-Control: no-store`.
- **Access token:**
  - EdDSA (Ed25519), valid 15 minutes;
  - claims `sub`, `hid` (empty for platform accounts), `roles`, `mfa`, `amr`, `sid`, `jti`, `iat`, `exp`;
  - `iss` = `mhai-gateway`, `aud` = `mhai-bff`.
- **Lockout:** 5 failures per (login, client IP) in 15 minutes gives **429 `login_locked`**. An unknown login gets the same reply.
- **TOTP:**
  - 30-second step, ±1 step tolerance, and no reuse of a step;
  - `mfa_token` lasts 5 minutes with 5 attempts;
  - backup codes: 10, single use, stored hashed.
- **Step-up:** **403 `mfa_invalid`** for a wrong code. 5 wrong codes in 15 minutes gives **429 `mfa_locked`**.
- **Passwords:** at least 12 characters, and not on the bundled breached list (`services/core/internal/password/breached.txt.gz`, at least the top 100,000).
- **Trusted device:** never skips the code for `hospital_admin`, `analyst`, `platform_admin` or `auditor`.
- **Roles:**
  - clinical accounts: `clinician`, `hospital_admin`, `analyst` (combinable);
  - platform accounts: `platform_admin`, `auditor` (separate accounts). `admin` is accepted as an alias of `platform_admin` on read.
- **Synthetic data only.** No real names, emails or patients in seeds, tests or docs.
- **Commits:** plain messages, with no AI attribution.

## Review Focus

1. **The same reply for unknown, locked and wrong-password logins, including timing.** An unknown email still runs argon2 against a fixed dummy hash, so its response time matches a real account's. Tested in Task 6.
2. **Two BFF requests refreshing the same token at once** (two tabs). Reusing a refresh token within **10 seconds** of its rotation returns **409 `conflict`** and does not revoke anything; the BFF retries with its newer token. Reuse after 10 seconds revokes the whole family. Tested in Task 6.
3. **An account or hospital deactivated mid-session.** The next request after the 30-second user/hospital cache expires gets **401 `unauthenticated`**, and a refresh fails at once. Tested in Task 8.
4. **A login email in mixed case or with spaces** (`" Grace.Nwosu@Havana.example "`) signs in as the stored lowercase email, and two accounts can't differ only by case. Tested in Task 5.
5. **A query that forgets its hospital filter.** It returns nothing under row-level security; it never returns another hospital's rows. That includes a handler that runs outside `WithHospital`, which must fail rather than see all rows. Tested in Task 2.

---

## File structure

```
services/core/
  go.mod, go.sum
  cmd/core/main.go              serve | migrate | hospitals create | users create
  internal/config/              env config; fails on missing or placeholder secrets
  internal/logx/                slog JSON logger with a redacting handler
  internal/db/                  pool, WithHospital / WithPlatform transactions, sqlc output
  internal/db/migrations/       goose SQL files (embedded)
  internal/db/queries/          sqlc .sql files
  internal/bus/                 NATS connect, KV buckets
  internal/httpx/               router, problem errors, request id, BFF credential, idempotency
  internal/audit/               hash-chain writer
  internal/password/            argon2id, breached list, policy
  internal/token/               EdDSA access tokens, refresh tokens
  internal/auth/                login, lockout, sessions, MFA, terms, password change handlers
  internal/authz/               principal middleware, permission table, generators
  internal/testutil/            containers for Postgres and NATS, fixtures
  openapi.yaml                  the committed contract for the routes built here
services/nats/
  nats.conf                     accounts and permissions (dev)
```

---

### Task 1: The Go module, config, logging, health and CI

**Files:**
- Create: `services/core/go.mod` (module `github.com/Measured-Health/mhai-multiagent/services/core`, go 1.24)
- Create: `services/core/cmd/core/main.go`, `services/core/internal/config/config.go`, `services/core/internal/logx/logx.go`, with tests beside them
- Modify: `.github/workflows/ci.yml` (add a `core` job), `Makefile` (`core-test`, `core-lint`)

**Interfaces:**
- Produces:
  - `config.Load() (Config, error)`
  - `Config{Env, HTTPAddr, DatabaseURL, NATSURL, NATSCreds, AccessKeyPath, MFAKey []byte, BFFServiceKeys []string, BreachedListPath}`
  - `logx.New(w io.Writer) *slog.Logger`

- [ ] **Step 1: Write the failing tests.**
  - `TestLoadFailsOnMissingSecret`: with no `MFA_ENCRYPTION_KEY`, `Load` errors and names the variable.
  - `TestLoadRejectsPlaceholder`: the values `change-me`, `changeme`, `placeholder`, and any key shorter than 32 bytes, all error.
  - `TestRedactingHandler`: logging attrs named `password`, `token`, `code`, `email`, `access_token`, `refresh_token`, `mfa_token`, `backup_code` writes `"[redacted]"`. Nested group attrs are redacted too.
  - `TestHealthz`: `GET /api/v1/healthz` → 200 `{"status":"ok"}`.
- [ ] **Step 2:** Run `cd services/core && go test ./...`. It should fail to compile.
- [ ] **Step 3:** Implement `config`, `logx` (wrapping `slog.JSONHandler`) and a `serve` command with `/api/v1/healthz`.
- [ ] **Step 4:** Add the CI `core` job: setup-go 1.24, `go vet ./...`, `staticcheck ./...`, `go test -race ./...`. Integration tests use the `integration` build tag, run in the same job, and the runner provides Docker.
- [ ] **Step 5:** Run `go test -race ./...`. It should pass.
- [ ] **Step 6:** Commit: "Add the Go gateway module with config, logging and health" (made before the rename).

### Task 2: The schema, database roles and row-level security

**Files:**
- Create:
  - `services/core/internal/db/migrations/00001_core.sql`, `00002_ported.sql`, `00003_rls_roles.sql`
  - `services/core/internal/db/db.go`
  - `services/core/internal/testutil/pg.go`
  - `services/core/sqlc.yaml`
- Modify:
  - `docker-compose.yml`: remove the `init-db/init.sql` mount, and add a `core-migrate` one-shot service.
  - Delete `init-db/init.sql`, including its placeholder admin. Keep `init-db/seed_kb.py`, adapted to the new `kb_chunks` columns.

**Interfaces:**
- Produces:
  - `db.Open(ctx, url) (*pgxpool.Pool, error)`
  - `db.WithHospital(ctx, pool, hospitalID uuid.UUID, fn func(pgx.Tx) error) error`: runs `SET LOCAL ROLE mhai_gateway; SET LOCAL mhai.hospital_id = $1`
  - `db.WithPlatform(ctx, pool, fn func(pgx.Tx) error) error`: role `mhai_platform`
  - `db.WithAuth(ctx, pool, fn)`: role `mhai_auth`, used only by sign-in before a hospital is known (see the table below)

**Tables in 00001_core:**

| Table | Columns |
|---|---|
| `hospitals` | `id` uuid, `name`, `short_code` unique, `status` (`active`, `setup_incomplete`, `deactivated`, …), `time_zone`, `created_at` |
| `hospital_settings` | `hospital_id` PK, `idle_timeout_minutes` (default 15), `review_overdue_hours` (default 4), `require_mfa_for_clinicians` (default false), `allow_trust_device` (default false) |
| `users` | `id` uuid, `hospital_id` (null = platform account), `email` citext unique, `password_hash`, `display_name`, `full_name`, `role_title`, `specialty`, `is_active`, `must_change_password`, `terms_version`, `terms_accepted_at`, `last_login_at`, `oidc_issuer`, `oidc_subject`, `created_at`. Check: `hospital_id` is null exactly when the user has a platform role (enforced in code; tested in Task 5). |
| `user_roles` | `(user_id, role)` PK, with a check `role IN ('clinician','hospital_admin','analyst','platform_admin','auditor')` |
| `user_mfa` | `user_id` PK, `secret_enc` bytea, `enabled`, `last_used_step`, `backup_code_hashes` text[] |
| `sessions` | `id`, `user_id`, `created_at`, `last_seen_at`, `ua_summary`, `ip_prefix`, `revoked_at` |
| `refresh_tokens` | `id`, `session_id`, `token_hash` unique, `issued_at`, `rotated_at`, `successor_id` |
| `trusted_devices` | `id`, `user_id`, `token_hash`, `created_at`, `expires_at` (30 days) |
| `audit_log` | `id` bigserial, `ts`, `actor_id`, `hospital_id`, `request_id`, `action`, `entity`, `entity_id`, `payload` jsonb, `prev_hash`, `hash` |
| `audit_checkpoints` | |
| `idempotency_keys` | `(scope, key)` PK, `request_hash`, `status`, `body`, `created_at`; 72-hour expiry |

**00002_ported:** the tables from `init-db/init.sql`, each with `hospital_id uuid`:
- `hospital_id` is `NOT NULL` on `clinical_queries`, `clinical_decisions`, `hga_evaluations` and `feedback_events`;
- it is nullable on `kb_chunks` (null = shared reference content, design §7.6);
- `guardrail_weights` stays global.

**00003_rls_roles:**
- Create the roles `mhai_gateway`, `mhai_platform`, `mhai_auth`, `mhai_worker` and `mhai_migrate`.
- Enable and force RLS on every table that has `hospital_id`, with the policy `hospital_id = current_setting('mhai.hospital_id', true)::uuid`. `kb_chunks` also has `OR hospital_id IS NULL` for SELECT only.
- `audit_log` grants `INSERT, SELECT` only, with no `UPDATE` or `DELETE` for any app role.
- `mhai_auth` may read and write `users`, `user_roles`, `user_mfa`, `sessions`, `refresh_tokens`, `trusted_devices` and `hospital_settings`, and read `hospitals`. It has no access to clinical tables.
- `mhai_platform` reads `hospitals`, `users` (without `password_hash`, through a view `platform_users`), `user_roles`, `sessions` and `audit_log`. It has no access to clinical tables.

- [ ] **Step 1: Write the failing integration tests** (tag `integration`), against a real Postgres 16 container.
  - `TestMigrateUpFromEmpty`: all three migrations apply. `goose status` shows them applied.
  - `TestRLSHidesOtherHospital`: seed hospitals A and B, each with one `clinical_queries` row. Inside `WithHospital(A)`, `SELECT count(*) FROM clinical_queries` = 1, and B's id isn't found.
  - `TestRLSWithoutHospitalSeesNothing` (Review Focus 5): a transaction as `mhai_gateway` without `SET LOCAL mhai.hospital_id` sees 0 rows.
  - `TestSharedKBVisible`: a `kb_chunks` row with null `hospital_id` is visible inside `WithHospital(A)` and `WithHospital(B)`. An UPDATE of it under `mhai_gateway` affects 0 rows.
  - `TestAuditAppendOnly`: `UPDATE audit_log` and `DELETE FROM audit_log` as `mhai_gateway` fail with permission denied.
  - `TestPlatformRoleNoClinical`: `SELECT` from `clinical_queries` as `mhai_platform` fails with permission denied.
- [ ] **Step 2:** Run `go test -tags integration ./internal/db/...`. It should fail.
- [ ] **Step 3:** Write the migrations and `db.go`. Embed the migrations with `//go:embed`, and add a `core migrate up` command.
- [ ] **Step 4:** Run the tests. They should pass. Run `docker compose config` to check the compose file is valid.
- [ ] **Step 5:** Commit: "Own the schema in Go migrations with row-level security per hospital".

### Task 3: NATS, credentials and KV

**Files:**
- Create:
  - `services/nats/nats.conf`
  - `services/core/internal/bus/bus.go`
  - `services/core/internal/testutil/nats.go`
- Modify: `docker-compose.yml` (add `nats:2.10-alpine` with JetStream, a volume and the config). Keep `redis` for now; its removal waits for the Python boundary plan.

**Interfaces:**
- Produces:
  - `bus.Connect(ctx, url, credsPath) (*nats.Conn, jetstream.JetStream, error)`
  - `bus.Buckets{Denylist, LoginAttempts, MFAAttempts jetstream.KeyValue}`
  - `bus.OpenBuckets(ctx, js) (Buckets, error)`. All three have per-key TTL: Denylist 15 min, LoginAttempts 15 min, MFAAttempts 15 min.

**`nats.conf` sets up two users:**
- `gateway`: may publish `inference.v1.>` and `jobs.v1.>`, and use KV and JetStream APIs.
- `worker`: may subscribe `inference.v1.>` and publish `_INBOX.>` and `dead.v1.>`. It may not publish `inference.v1.>`.

The dev passwords come from env. Production NKeys are documented in the file's comments.

- [ ] **Step 1: Write the failing integration tests.**
  - `TestWorkerCannotPublishInference`: connected as `worker`, publishing to `inference.v1.cca` gets a permissions violation.
  - `TestGatewayCanRequest`: `gateway` requests `inference.v1.echo`, and a `worker` subscriber replies.
  - `TestKVTTL`: a denylist key put with a 2 s TTL is gone after 3 s.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement `bus` and the config.
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add NATS with per-service permissions and KV buckets".

### Task 4: HTTP foundation: errors, BFF credential, idempotency and audit

**Files:**
- Create:
  - `services/core/internal/httpx/{router.go,errors.go,requestid.go,bff.go,idempotency.go}`
  - `services/core/internal/audit/audit.go`
  - tests beside them

**Interfaces:**
- Produces:
  - `httpx.Error(w, r, code string)`, which writes the problem body, or `{code}` on platform audience.
  - `httpx.Codes map[string]int`: the catalogue in Global Constraints.
  - `httpx.BFF(keys []string) func(http.Handler) http.Handler`. It rejects requests without a valid `X-MHAI-Service` key with **401 `unauthenticated`**. It reads `X-MHAI-Client-IP` and `X-MHAI-Audience` into the context only after the key passes. It compares keys in constant time.
  - `httpx.ClientIP(ctx) netip.Addr`
  - `httpx.Audience(ctx) string` (`clinical` or `platform`)
  - `httpx.Idempotent(pool)`: on POST, PUT, PATCH and DELETE with an `Idempotency-Key`, it replays the stored status and body for the same key, scope and body hash. The same key with a different body gets **409 `conflict`**. Keys are kept 72 hours. The scope is the principal's user id, or `anon:<client ip>` before sign-in.
  - `audit.Write(ctx, tx pgx.Tx, e audit.Event) error`. `Event{ActorID, HospitalID, RequestID, Action, Entity, EntityID string; Payload map[string]any}`. `hash = sha256(prev_hash || canonical JSON of the event)`, and the chain is serialised with `pg_advisory_xact_lock`.
  - `audit.Verify(ctx, pool) (ok bool, firstBroken int64, err error)`

- [ ] **Step 1: Write the failing tests.**
  - `TestProblemBody`: `forbidden` gives 403 with `code`, `request_id` and a fixed `detail`.
  - `TestPlatformBodyIsCodeOnly`: on the platform audience the body is exactly `{"code":"forbidden"}`.
  - `TestBFFRejectsMissingKey`.
  - `TestClientIPIgnoredWithoutKey`.
  - `TestUnknownFieldRejected`: decoding `{"username":"a","x":1}` into a strict struct gives 422 `validation_failed`, and the body doesn't contain `x`.
  - `TestIdempotentReplay`.
  - `TestIdempotentConflict`.
  - `TestAuditChainVerifies`, then `TestAuditTamperDetected`: an edit made as superuser in the test is detected at its id.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement the code. The router mounts `/api/v1` behind `requestid` → `BFF` → body limit (64 KB).
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add problem errors, the BFF credential, idempotency and the audit chain".

### Task 5: Accounts, passwords and the seed CLI

**Files:**
- Create:
  - `services/core/internal/password/{argon.go,policy.go,breached.go,breached.txt.gz}`
  - `services/core/internal/db/queries/users.sql`
  - `services/core/cmd/core/seed.go`
  - tests beside them

**Interfaces:**
- Produces:
  - `password.Hash(pw string) (string, error)`: argon2id, encoded as PHC (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`).
  - `password.Verify(pw, encoded string) (bool, error)`
  - `password.DummyVerify(pw string)`: runs one verify against a fixed hash, for timing.
  - `password.Check(pw string) error`: returns `ErrTooShort` or `ErrBreached`, which map to 422 `weak_password`.
  - `NormaliseEmail(s string) string`: trim, then lowercase.
  - CLI:
    - `core hospitals create --name --short-code --time-zone [--require-mfa-for-clinicians] [--allow-trust-device]`
    - `core users create --email --name --roles clinician,hospital_admin [--hospital SHORTCODE]`. It prompts for the password and never takes it as an argument. It sets `must_change_password=true`.

- [ ] **Step 1: Write the failing tests.**
  - `TestHashVerifyRoundTrip`.
  - `TestVerifyWrongPassword`.
  - `TestBreachedRejected`: `"password1234"` is breached.
  - `TestShortRejected`: 11 characters.
  - `TestNormaliseEmail` (Review Focus 4): `" Grace.Nwosu@Havana.example "` → `"grace.nwosu@havana.example"`.
  - Integration `TestEmailUniqueIgnoresCase`.
  - `TestPlatformRoleNeedsNoHospital`: creating `platform_admin` with `--hospital` errors, and `clinician` without `--hospital` errors.
  - `TestRolesCombineOnlyClinical`: `clinician,platform_admin` errors.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement the code. Tune the argon2 parameters to about 250 ms on the CI runner, with memory of at least 64 MiB. Record the values in a comment.
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add accounts, argon2id passwords, the breached list and the seed CLI".

### Task 6: Sign-in, lockout, tokens, refresh and sessions

**Files:**
- Create:
  - `services/core/internal/token/{access.go,refresh.go}`
  - `services/core/internal/auth/{login.go,refresh.go,logout.go,sessions.go,terms.go,user.go}`
  - `services/core/internal/db/queries/auth.sql`
  - tests beside them

**Interfaces:**
- Consumes:
  - `password.*` (Task 5);
  - `bus.Buckets` (Task 3);
  - `db.WithAuth` (Task 2);
  - `audit.Write`, `httpx.*` (Task 4).
- Produces:
  - `token.Issuer{Key ed25519.PrivateKey, KeyID string}`
  - `(i Issuer) Access(c Claims) (string, time.Time, error)`
  - `token.Verify(tok string, keys map[string]ed25519.PublicKey) (Claims, error)`
  - `Claims{Sub, Hid string; Roles []string; MFA bool; AMR []string; Sid, Jti string}`
  - `token.NewRefresh() (plain, hash string)`
  - **Routes**, with shapes from the contract §2.2 and §2.6 plus the design §6.3 addition:
    - `POST /auth/login {username, password, device_token?}` → `{access_token, expires_in, refresh_token, user}` or `{mfa_required:true, mfa_token}`;
    - `POST /auth/refresh {refresh_token}` → `{access_token, expires_in, refresh_token}`;
    - `POST /auth/logout`;
    - `POST /auth/terms {version}` → 204;
    - `GET /auth/sessions`;
    - `DELETE /auth/sessions/{id}` → 204;
    - `POST /auth/sessions/revoke-all` → 204.
  - `user` holds exactly these fields: `id`, `role`, `roles`, `email`, `display_name`, `full_name`, `role_title`, `specialty`, `hospital` (`{id, name}` or null), `mfa_enabled`, `must_change_password`, `terms_accepted_at`, `recovery_codes_left`, `idle_timeout_minutes`, `review_overdue_hours`.
    - `role` is the primary role, in the order `clinician` > `hospital_admin` > `analyst`, or `platform_admin` > `auditor`.
    - On the platform audience, an account without a platform role gets **403 `not_platform`**, and a clinical account always does.

- [ ] **Step 1: Write the failing integration tests.**
  - `TestLoginSuccessShape`: the `user` keys exactly match the list above, and `access_token` verifies with `aud=mhai-bff`, `exp`−`iat`=900.
  - `TestLoginWrongPasswordGeneric` and `TestLoginUnknownSameReply` (Review Focus 1): the same status, body (minus `request_id`) and headers. Unknown-email latency is within 30% of the wrong-password median over 5 runs.
  - `TestLockoutAfterFive`: the 6th attempt gets 429 `login_locked`, even with the right password. Another client IP is unaffected.
  - `TestLockoutUnknownLoginSame`.
  - `TestMFARequiredBranch`: a `hospital_admin` gets `{mfa_required, mfa_token}` and no token.
  - `TestPlatformAudienceRefusesClinical`: 403 `not_platform`.
  - `TestRefreshRotates`: the old refresh token is then unusable.
  - `TestRefreshRaceWithin10s` (Review Focus 2): it gets 409 `conflict`, and the family stays valid.
  - `TestRefreshReuseAfter10sRevokesFamily`: the session is revoked, and its access `jti` is denylisted.
  - `TestLogoutDenylists`: the old access token is refused with 401 on the next request.
  - `TestRevokeAllKeepsNone`.
  - `TestDeleteOtherUsersSession404`.
  - `TestTermsRecorded`.
  - Every attempt writes an audit row with action `auth.login.succeeded`, `auth.login.failed` or `auth.login.locked`, and the row's payload has no email.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement the code. Load the signing key from `ACCESS_KEY_PATH` (PEM, Ed25519), with `kid` in the header. The access check middleware for later tasks verifies the signature, `aud`, `exp` and the denylist.
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add sign-in, lockout, EdDSA tokens, rotating refresh and sessions".

### Task 7: Two-step verification, trusted devices, step-up and password change

**Files:**
- Create:
  - `services/core/internal/auth/{mfa.go,stepup.go,password.go}`
  - `services/core/internal/db/queries/mfa.sql`
  - tests beside them

**Interfaces:**
- Consumes: Tasks 3–6.
- Produces:
  - **Routes** (contract §2.3 and §2.5):
    - `POST /auth/mfa/challenge {mfa_token, code, trust_device?}` → `{access_token, expires_in, refresh_token, user, device_token?}`. A backup code works in `code`.
    - `POST /auth/mfa/enroll` → `{otpauth_uri, recovery_codes[10]}`.
    - `POST /auth/mfa/verify {code}` → 204.
    - `POST /auth/mfa/recovery-codes` → `{recovery_codes[10]}`. Needs a step-up code.
    - `POST /auth/password {current_password, new_password}` → 204. Signs out the user's other sessions.
  - `auth.StepUp(ctx, tx, userID, mfaCode, backupCode string) error`. Returns `ErrMFAInvalid` (403 `mfa_invalid`) or `ErrMFALocked` (429 `mfa_locked`). Later plans call this on sensitive writes.
  - Secrets are encrypted with AES-256-GCM under `Config.MFAKey`. The nonce is prepended.
- **Policy** (contract §2.3, design §6.4):
  - always required for `hospital_admin`, `analyst`, `platform_admin` and `auditor`;
  - required for `clinician` when `require_mfa_for_clinicians` is on;
  - a user who must have MFA but hasn't enrolled gets a token whose only allowed routes are enrol, verify, password and logout. Every other route gets 403 `mfa_required`.

- [ ] **Step 1: Write the failing tests.**
  - `TestChallengeValidCode`.
  - `TestCodeReuseSameStepRejected`.
  - `TestStepBoundaryTolerance`: a code from the step before now passes, and one from two steps before fails.
  - `TestFiveWrongCodesLocks`: 429 `mfa_locked`.
  - `TestMfaTokenExpires`: after 5 minutes, 403 `mfa_invalid`.
  - `TestBackupCodeOnce`.
  - `TestRegenerateInvalidatesOld`.
  - `TestTrustDeviceOnlyWhenHospitalAllows`.
  - `TestTrustedDeviceSkipsForClinician`.
  - `TestTrustedDeviceNeverSkipsForAdmin`: for `hospital_admin` the code is still required.
  - `TestDeviceTokenBoundToUser`: another user's login with that `device_token` still needs a code.
  - `TestUnenrolledAdminLimitedRoutes`.
  - `TestPasswordChangeRequiresCurrent`.
  - `TestPasswordChangeSignsOutOthers`.
  - `TestPasswordChangeBreachedRejected`: 422 `weak_password`.
  - `TestSecretEncryptedAtRest`: the raw column doesn't contain the base32 secret.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement the code.
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add TOTP, backup codes, trusted devices, step-up and password change".

### Task 8: Authorization: the principal, the permission table and hospital separation

**Files:**
- Create:
  - `services/core/internal/authz/{principal.go,permissions.go,generate.go}`
  - `services/core/cmd/permgen/main.go`
  - generated: `docs/specs/generated/permissions.md` and `services/intelligence/intelligence/security/permissions.json`
  - tests beside them
- Modify: `services/core/internal/httpx/router.go`. Every route registers through `authz.Route`.

**Interfaces:**
- Consumes: `token.Verify`, the denylist, `db.With*`.
- Produces:
  - `authz.Principal{UserID, HospitalID uuid.UUID; Roles []string; MFA bool; AMR []string; SessionID string; Platform bool}`
  - `authz.FromContext(ctx) (Principal, bool)`
  - `authz.Route(mux, method, pattern string, rule Rule, h http.Handler)`
  - `Rule{Public bool; AnyRole []string; Platform bool; NeedsMFA bool; StepUp bool; Write bool}`
  - `authz.Table() []RouteRule`, used by the generators.
  - `authz.Tx(ctx, pool, fn)`. It picks `WithHospital(principal.HospitalID)` for clinical principals and `WithPlatform` for platform ones. Handlers never call `db.With*` directly. An analyzer test greps for direct calls outside `authz` and `auth`.
- **Rules** (contract §3, §14.1):
  - a route with `AnyRole` needs one of those roles in `Roles`;
  - `Platform` routes need `X-MHAI-Audience: platform` and a platform principal, or 403 `not_platform`;
  - auditors get 403 `forbidden` on every `Write` route;
  - a `hospital_admin` without `clinician` gets 403 on clinical-only routes;
  - the user's `is_active` and the hospital's `status`=`active` are checked on each request, cached for at most 30 s; when either fails, 401 `unauthenticated`.
- **Permission table:** Plan 2 registers only the auth routes and `healthz`. Two fixture routes exist under the `test` build tag only, `GET /api/v1/_test/clinical` and `GET /api/v1/_test/platform`, to prove the rules.

- [ ] **Step 1: Write the failing tests.**
  - `TestEveryRouteInTable`: walking the mux finds no route registered outside `authz.Route`.
  - `TestRoleRequired`.
  - `TestPlatformAudienceRequired`.
  - `TestAuditorWriteForbidden`.
  - `TestHospitalAdminNotClinician403`.
  - `TestOtherHospitalResource404`: under the test tag, a fixture handler reading a `clinical_queries` row by id gives 404 for hospital B's row when signed in at A.
  - `TestDeactivatedUserNextRequest401` (Review Focus 3): uses a cache clock override.
  - `TestDeactivatedHospital401`.
  - `TestRefreshFailsForDeactivated`.
  - `TestGeneratedFilesUpToDate`: running `permgen` produces no diff. CI also runs this.
  - `TestNoDirectDBTx`.
- [ ] **Step 2:** Run the tests. They should fail.
- [ ] **Step 3:** Implement the code. `permissions.json` lists `{method, pattern, any_role, platform, step_up}`, for the Python side's checks in a later plan.
- [ ] **Step 4:** Run the tests. They should pass.
- [ ] **Step 5:** Commit: "Add the principal, one permission table and hospital separation in every transaction".

### Task 9: Contract, compose stack and docs

**Files:**
- Create:
  - `services/core/openapi.yaml`, covering every route built in Tasks 6–7, with the exact shapes and error codes;
  - `services/core/README.md`, with commands, env, the seed CLI and a key-generation helper (`core keys new`);
  - a contract test `services/core/internal/auth/contract_test.go`.
- Modify:
  - `docker-compose.yml`: add a `core` service, using the `core-migrate` dependency, NATS, Postgres and the config from `.env`;
  - `.env.example`: the core service variables, with placeholders that `config` rejects;
  - `CHANGELOG.md`;
  - `docs/specs/2026-10-17-frontend-requirements.md` §2: mark the routes built here as **Built (Plan 2)**.

- [ ] **Step 1: Write the failing test** `TestOpenAPIMatchesHandlers`. For every route in `authz.Table()` there's a path and method in `openapi.yaml`, and the reverse. Each documented error code is in `httpx.Codes`.
- [ ] **Step 2:** Run the test. It should fail.
- [ ] **Step 3:** Write the spec, the README and the compose service.
- [ ] **Step 4:** Run `docker compose up -d postgres nats core-migrate core`. Then use the CLI to seed a synthetic hospital `HAV` and a clinician, and run `curl` against `/api/v1/auth/login` with the BFF key. Expected: 200 with `access_token`. Record the transcript in the PR, with no secrets.
- [ ] **Step 5:** Run the full suite: `go test -race -tags integration ./...`, plus `uv run pytest -q`, to check the Python side still passes after `init.sql` was removed.
- [ ] **Step 6:** Commit: "Commit the auth contract, run the core service in compose and document it".

---

## Out of this plan (next plans)

- **Invites and forgot/reset password:** they need the mail provider (design §8b, open item 1).
- **The Python boundary over NATS:** token verification in workers, replacing `RBACManager` and `AuditLogger`, and removing Redis (design §5, §7.5; step 4).
- **SSO:** design §6.6.
- **The contract endpoints:** staff, settings, queries, and everything after them.
- **mhai-web's BFF:** switching it to live mode against this core service. This is a mhai-web plan. It needs `refresh_token` handling and the `X-MHAI-Service` key.

## Self-review notes

- **Spec coverage:**

  | Design section | Task |
  |---|---|
  | §4.1, §4.6 | Tasks 1 and 9 |
  | §4.3 (KV) | Task 3 |
  | §4.4, §7.3, §7.4 | Task 2 |
  | §5.1 | Task 3 |
  | §6.1 | Task 5 |
  | §6.2, §6.3 | Task 6 |
  | §6.4, §6.5 (change only) | Task 7 |
  | §7.1, §7.2 | Task 8 |
  | §7.7, audit | Task 4, used in Tasks 6–8 |
  | §8 | Task 4 |
  | §8a | Global Constraints and Task 4 |
  | §8b | Task 5 |
  | §9 | across Tasks 2–9 |

  §5.2–5.3 and §7.5 are deferred to the boundary plan, as the design's §10 orders.
- **Names across tasks:**
  - `db.WithHospital`, `WithPlatform`, `WithAuth`
  - `bus.Buckets`
  - `httpx.Error`, `BFF`, `Idempotent`, `ClientIP`, `Audience`
  - `audit.Write`, `Verify`
  - `password.Hash`, `Verify`, `DummyVerify`, `Check`
  - `token.Issuer`, `Verify`, `NewRefresh`
  - `auth.StepUp`
  - `authz.Principal`, `Route`, `Rule`, `Tx`, `Table`
