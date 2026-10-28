# The core service

The Go service the BFFs call: sign-in and sessions, two-step verification,
password change, the permission table, hospital separation, audit and
idempotency. It reaches the Python services only through NATS (ADR 0010).
Design: `docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`.

- **Contract:** [`openapi.yaml`](openapi.yaml), every route with its shapes
  and error codes. `internal/auth/contract_test.go` fails if it and the
  served routes differ, or if it names an error code the service doesn't
  have.
- **Permissions:** every route registers through `authz.Route`. The table
  is published as `docs/specs/generated/permissions.md` and
  `services/intelligence/intelligence/security/permissions.json`
  (`go run ./cmd/permgen`; a test fails when they are stale).

Module `github.com/Measured-Health/mhai-multiagent/services/core`, binary
`core`, Go 1.24.

## Commands

```bash
core serve                         # the HTTP service (HTTP_ADDR, default :8080)
core migrate up | status           # goose migrations, then the deployment logins
core keys new --out PATH           # a new Ed25519 access-token key (PKCS#8 PEM, mode 0600)
core keys ensure --out PATH        # keep PATH if it is a usable key, else make one
core hospitals create --name N --short-code CODE --time-zone TZ \
    [--require-mfa-for-clinicians] [--allow-trust-device]
core users create --email E --name N --roles clinician,hospital_admin [--hospital CODE]
```

`users create` never takes the password as an argument. It prompts on a
terminal and reads one line from stdin otherwise. New accounts must change
their password at first sign-in. Errors and output never contain the
email, name or password.

From `services/core/` (or `make core-test` / `make core-lint` from the root):

```bash
gofmt -l . && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...
go test -race ./...                              # unit tests
go test -race -tags integration ./...            # + Postgres and NATS in containers
go test -race -tags "integration test" ./...     # + the /_test/ fixture routes
```

The integration tests use testcontainers, so they need Docker or Podman.

## Database logins

The service never connects as the owner. There are three logins, each with
one job:

| Login (compose default) | Member of | Used by |
|---|---|---|
| `POSTGRES_USER` (owner / superuser) | — | `core migrate` only |
| `core_app` (`CORE_DB_USER`) | `mhai_app` only | `core serve` |
| `core_provision` (`PROVISION_DB_USER`) | `mhai_provision` only | `core hospitals/users create` |

- `core serve` refuses any other login at start-up (`db.CheckLoginRole`):
  a superuser, `BYPASSRLS`, an owner, a login that inherits an app role, or
  one that is a direct member of any role besides `mhai_app`. The
  provisioning login is refused for that reason.
- `mhai_app` holds nothing itself. Each transaction runs
  `SET LOCAL ROLE mhai_gateway`, `mhai_platform` or `mhai_auth`, so a
  query outside `db.With*` or `authz.Tx` fails.
- `core migrate up` creates or resets the two logins when `CORE_DB_USER` /
  `CORE_DB_PASSWORD` and `PROVISION_DB_USER` / `PROVISION_DB_PASSWORD` are
  set. It sets the password and revokes any other membership. Placeholder
  or short passwords (under 16 characters) are refused before the database
  is touched.
- On managed Postgres, run `migrate` as a `CREATEROLE` admin instead of a
  superuser.

## Environment (`core serve`)

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | the `mhai_app` login; a placeholder password is refused |
| `NATS_URL` | yes | `nats://gateway:PASSWORD@host:4222` in dev; a placeholder password is refused |
| `NATS_NKEY_SEED_PATH` | production | the `gateway` user's NKey seed file (see `services/nats/nats.conf`) |
| `ACCESS_KEY_PATH` | yes | PKCS#8 Ed25519 PEM from `core keys new` |
| `MFA_ENCRYPTION_KEY` | yes | at least 32 bytes; seals TOTP secrets, keys backup-code hashes, and derives the counter key (HKDF-SHA256, label `mhai lockout key v1`) that names the lockout and MFA counters. Changing it forgets live lockouts. |
| `BFF_SERVICE_KEYS` | yes | comma-separated, each at least 32 bytes; one per BFF |
| `HTTP_ADDR` | no | default `:8080` |
| `CORE_ENV` | no | default `development` |

Secrets must not contain `changeme`, `change-me` or `placeholder`. Errors
name the variable, never the value. The core service reads only its own
secrets: the NATS worker password and the database owner password are not
in its environment.

## Running it in compose

From the repository root, with every `changeme` in `.env` replaced:

```bash
make core-up      # postgres, nats, core-migrate, core-keys, core on 127.0.0.1:${CORE_PORT:-8080}
curl -s localhost:8080/api/v1/readyz           # {"status":"ready"}

# Seed a synthetic hospital and clinician as the provisioning login:
docker compose run --rm -T core-cli hospitals create --name "Havana General" --short-code HAV --time-zone Africa/Lagos
printf '%s\n' "$SYNTHETIC_PASSWORD" | docker compose run --rm -T core-cli users create \
  --email synthetic.clinician@havana.example --name "Synthetic Clinician" --roles clinician --hospital HAV

curl -s localhost:8080/api/v1/auth/login \
  -H "X-MHAI-Service: $BFF_KEY" -H "X-MHAI-Client-IP: 198.51.100.20" -H 'Content-Type: application/json' \
  -d '{"username":"synthetic.clinician@havana.example","password":"..."}'
```

`core-keys` writes the dev signing key into the `corekeys` volume once. In
production, mount a key made with `core keys new` and point
`ACCESS_KEY_PATH` at it.

## Probes

- `GET /api/v1/healthz`: the process is up.
- `GET /api/v1/readyz`: 200 when Postgres answers and NATS is connected
  with every KV bucket reachable; otherwise 503 `not_ready`.

Neither needs the BFF key, and neither says what failed.

## Limits the service applies

- Sign-in lockout: 5 failures per (login, client IP) in 15 minutes, and 20
  per login across every IP in an hour (`login_ceiling` bucket). Both give
  429 `login_locked`, the same reply as for an unknown login. If the
  counters can't be read, sign-in fails closed (503).
- At the per-login ceiling, the one way through is the right password
  together with a live trusted-device token of that user, where that device
  may stand in for the code (not for `hospital_admin`, `analyst`,
  `platform_admin` or `auditor`, and only where the hospital allows trusted
  devices) and the account is on its own audience: holding the device is
  the proof, so a stranger can't lock that owner out. Everything
  else (right or wrong password, two-step or not, unknown login) gets the
  same 429 `login_locked`, with the same argon2 and device-lookup work, so
  the reply says nothing about the password. The (login, IP) slot stays
  counted. Each such request writes `auth.login.ceiling` to the audit log
  (ids only) for admin alerting.
- Counter names are HMAC-SHA256 under the counter key (derived from
  `MFA_ENCRYPTION_KEY`, label `mhai lockout key v1`), never the email or a
  bare hash of it, so a copy of the NATS KV store can't confirm an email.
- argon2 runs behind a process-wide gate with one slot per CPU. A request
  that can't get a slot within 2 seconds gets 503 `not_ready` and isn't
  counted as an attempt.
- Expired `Idempotency-Key` rows are swept every 10 minutes, as
  `mhai_auth` through `mhai_sweep_idempotency()`. That function can see
  only expired rows.
- Step-up: 5 wrong codes in 15 minutes give 429 `mfa_locked`.
- `migrate` creates the database logins (`EnsureLogin`) with a SCRAM-SHA-256
  verifier built in Go; the plaintext password never reaches Postgres, its
  logs or `pg_stat_statements`. Login passwords must be printable ASCII.
  The verifier goes in a statement of its own, after a secret-free probe
  that the password may be set, so a failure (a missing privilege, a bad
  attribute) is logged without it. As a superuser, `migrate` also turns
  `log_statement` and `log_min_error_statement` off for that transaction; a
  CREATEROLE owner can't, so a server with `log_statement = ddl` or `all`
  logs the verifier there.
- At startup, `serve` refuses a database login that is a superuser, has
  BYPASSRLS, CREATEROLE, CREATEDB or REPLICATION, holds any membership but
  `mhai_app`, or owns the database, a schema or a table.

## Known limits

- **Third-party lockout.** Someone who knows an email can hold any account
  without a trusted device that may stand in for the code (two-step
  accounts, admins, analysts and platform accounts included) at the per-login
  ceiling for an hour at a time, for about 20 requests an hour. There is
  no admin unlock until the staff endpoints exist; `auth.login.ceiling`
  audit rows are the signal to alert on.
- **Revocation lag.** A deactivated account or hospital, or a revoked
  session, can keep working for up to 30 s (the state cache), and up to
  the 15-minute access-token life if the NATS KV denylist is down when the
  password changes.
- **No TLS inside compose.** Postgres (`sslmode=disable`) and NATS run
  without TLS on the compose network. Production needs TLS on both.
- **The argon2 gate is per process** and sized by CPU count; the BFF must
  rate-limit sign-in in front of it.
- **`setup_incomplete` hospitals are served**, so their first admin can
  finish setup.
- **Self-enrolment of two-step** stays open on first sign-in until the
  invite flow exists: whoever has the first password owns the second factor.
- **The `intelligence` service connects to Postgres as the superuser**
  (T2 M6) until the boundary plan; it no longer gets the core secrets.

## Layout

```
cmd/core        serve, migrate, keys and the seed CLI
cmd/permgen     writes the published permission table
internal/auth   sign-in, refresh, two-step, password, sessions, terms
internal/authz  principal middleware, permission table, authz.Tx
internal/db     pool, migrations (goose), role checks, sweep, logins
internal/httpx  errors, BFF key, request id, idempotency, router
internal/bus    NATS connection and KV buckets
internal/audit  the hash-chained audit log
internal/token  access tokens (EdDSA) and refresh tokens
internal/password  argon2id, the breached list, the concurrency gate
internal/config    environment
```
