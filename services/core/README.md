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
  is published as `docs/specs/generated/permissions.md`, which also lists
  the operations table (`internal/authz/operations.go`: the calls to the
  intelligence workers). The operations table is also published as
  `services/intelligence/intelligence/boundary/operations.json`
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
| `NATS_NKEY_SEED_PATH` | production | the `gateway` user's NKey seed file (see `services/nats/README.md`) |
| `NATS_TLS_CA_PATH`, `NATS_TLS_CERT_PATH`, `NATS_TLS_KEY_PATH` | outside development | PEM files: the CA for the NATS server's certificate, and core's client certificate and key. All three or none; required unless `CORE_ENV` is `development` |
| `NATS_JS_REPLICAS` | no | replicas for `JOBS`, `DEAD` and the KV buckets: `1` (default, dev compose), `3` on the production cluster (R3), or `5` |
| `JOBS_PIPELINE_MAX_ACK_PENDING` | no | jobs the pipeline workers hold at once: worker count × concurrency; default `8` |
| `ACCESS_KEY_PATH` | yes | PKCS#8 Ed25519 PEM from `core keys new` |
| `REQUEST_KEY_PATH` | yes | the request-token key (spec §7.2), from `core keys new --kind request`: another file and another key than `ACCESS_KEY_PATH` (start-up refuses the same key id) |
| `INTELLIGENCE_QUERY_CAP` | no | clinical questions one hospital may have queued or running at once; past it a submit is 429 `rate_limited`. Default `10`, 1 to 1000 |
| `MFA_ENCRYPTION_KEY` | yes | at least 32 bytes; seals TOTP secrets, keys backup-code hashes, and derives the counter key (HKDF-SHA256, label `mhai lockout key v1`) that names the lockout and MFA counters. Changing it forgets live lockouts. |
| `BFF_SERVICE_KEYS` | while `CORE_HTTP_API` is on | comma-separated, each at least 32 bytes; one per BFF |
| `HTTP_ADDR` | no | default `:8080`, or `127.0.0.1:8080` when `CORE_HTTP_API` is off. With the API off it must be a loopback IP (`127.0.0.1`, `[::1]`); anything else is refused at start-up |
| `CORE_HTTP_API` | no | `on` (default) or `off`. On: the frontend contract over HTTP with the BFF key, for the switch-over while mhai-web still uses `fetch`. Off: HTTP serves only the probes, on loopback, and `BFF_SERVICE_KEYS` is no longer required. The NATS API is served either way (below) |
| `CORE_NATS_API_CONCURRENCY` | no | `api.v1` messages handled at once per core copy; past it a message gets 503 `unavailable` at once. Default `256`, 1 to 100000 |
| `CORE_ENV` | no | default `production`, which fails closed: no API docs, NATS TLS required, no test-only intelligence operations. Compose sets `development` |
| `CORE_API_DOCS` | no | `true` or `false`: serve the API docs page. Default: on when `CORE_ENV` is `development`, off for every other value (production included). Anything else is refused at start-up. |

Secrets must not contain `changeme`, `change-me` or `placeholder`. Errors
name the variable, never the value. The core service reads only its own
secrets: the other NATS users' passwords, the JetStream key and the
database owner password are not in its environment.

## API docs

`GET /api/docs` is a Redoc page for [`openapi.yaml`](openapi.yaml);
`GET /api/openapi.yaml` is the file itself. Both are embedded in the binary
(`go:embed`), so the page and the served spec are the committed file.
Redoc 2.5.4 is vendored in `internal/apidocs/assets/` with its licence and
SHA-256 (see the README there); the page loads nothing from a CDN.

`GET /api/docs/intelligence` is the second tab: the Intelligence API, which
is NATS operations, not HTTP. It is a static page **generated** by
`go run ./cmd/permgen` from `proto/` (messages, fields, comments) and the
operations table in `internal/authz/operations.go` (subjects, kind, who may
call, `bff_direct`, timeout, hospital), plus the closed error-code set. It is
committed as `internal/apidocs/assets/intelligence.html` and embedded.
`TestGeneratedFilesUpToDate` fails when it is stale. Subjects and streams:
[`services/nats/README.md`](../nats/README.md). The two pages link each other
with a plain nav (`aria-current`); neither needs a script of its own.

- **Switch:** `CORE_API_DOCS` (above). When off, these paths answer the
  normal 404 `not_found` problem. The demo (`deploy/compose.yml`) and the
  dev compose turn it on.
- **No credentials:** the routes sit outside the authenticated API chain,
  like `/api/v1/healthz`, so they need no BFF key or token. They are not in
  the permission table or the contract; `contract_test.go` compares
  `openapi.yaml` with `server.Table()`, which only holds `authz.Route`
  routes, so they are excluded by construction. Redoc has no "try it out"
  console, so the page can send no request of its own.
- **Headers:** `Content-Security-Policy: default-src 'none'; script-src
  'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src
  'self'; connect-src 'self'; base-uri 'none'; form-action 'none';
  frame-ancestors 'none'`, plus `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer` and `Cache-Control: no-store`. Inline
  styles are allowed because Redoc (styled-components) writes `<style>`
  elements at run time; scripts are `'self'` only. Redoc's search is off,
  since it needs a `blob:` worker.
- **Never over NATS:** `apidocs.Wrap` is applied only around the HTTP
  listener in `cmd/core/serve.go`, and only while `CORE_HTTP_API` is on.
  `internal/natsapi` serves `authz.Mux.NATSHandler`, which doesn't include
  these routes (`TestDocsAndProbesNotOverNATS`). They are for people with
  a browser, not for the BFFs.

**On the demo host,** core listens on loopback only. Open a tunnel and
browse locally:

```bash
ssh -N -L 8092:127.0.0.1:8092 <user>@<demo-host>
# then open http://127.0.0.1:8092/api/docs
```

## Versioning

The API version policy is [ADR 0012](../../docs/adr/0012-api-versioning.md):
the major version is in the path (`/api/v1`) and the NATS subject, changes
inside a version only add, and breaking changes go to `v2` served alongside.
`info.version` in `openapi.yaml` is semver: minor for an additive change,
patch for a documentation-only one. CI runs `oasdiff breaking` (pinned,
`.github/scripts/openapi-breaking.sh`) against the target branch and fails
on a breaking change. `httpx.Deprecated(deprecated, sunset, link)` is the
middleware that sets `Deprecation`, `Sunset` and `Link` on a route being
retired; nothing uses it yet, and the retirement policy will.

## Running it in compose

From the repository root, with every `changeme` in `.env` replaced:

```bash
make core-up      # postgres, nats, core-migrate, core-keys, core-request-key, core on 127.0.0.1:${CORE_PORT:-8080}
curl -s localhost:8080/api/v1/readyz           # {"status":"ready"}

# Seed a synthetic hospital and clinician as the provisioning login:
docker compose run --rm -T core-cli hospitals create --name "Havana General" --short-code HAV --time-zone Africa/Lagos
printf '%s\n' "$SYNTHETIC_PASSWORD" | docker compose run --rm -T core-cli users create \
  --email synthetic.clinician@havana.example --name "Synthetic Clinician" --roles clinician --hospital HAV

curl -s localhost:8080/api/v1/auth/login \
  -H "X-MHAI-Service: $BFF_KEY" -H "X-MHAI-Client-IP: 198.51.100.20" -H 'Content-Type: application/json' \
  -d '{"username":"synthetic.clinician@havana.example","password":"..."}'
```

`core-keys` writes the dev access-token key into the `corekeys` volume once;
`core-request-key` writes the request-token key there too, and its public
half to `request-keys.json` in the `trustedkeys` volume (public keys only,
for the workers to mount read-only). In production, mount keys made with
`core keys new` (and `--kind request`) and point `ACCESS_KEY_PATH` and
`REQUEST_KEY_PATH` at them.

## The API over NATS (`internal/natsapi`)

Spec §17.1. The BFFs call core over NATS: one HTTP request per message on
`api.v1.clinical` (user `bff_clinical`) or `api.v1.platform` (user
`bff_platform`). Every core copy subscribes both in queue group `core`, so
each message is handled by one copy, and a copy that dies loses only the
calls it held: the BFF times out and retries (writes with the same
`Idempotency-Key`), and another copy answers.

| Message | |
|---|---|
| `Mhai-Method` | `GET`, `HEAD`, `POST`, `PUT`, `PATCH` or `DELETE`; else 405 `method_not_allowed` |
| `Mhai-Path` | path and query under `/api/v1/`, at most 2 KB of printable ASCII, no fragment; else 404 `not_found` |
| `Mhai-Timeout-Ms` | optional; the handler's deadline is 30 s or this, whichever is sooner |
| other headers | only `Authorization`, `Content-Type`, `Idempotency-Key`, `If-None-Match`, `X-MHAI-Client-IP` and `traceparent` pass; everything else is dropped (`X-MHAI-Service`, `X-MHAI-Audience`, `X-Request-ID`, `Mhai-Request-Id`, `Cookie`, `X-Forwarded-For`, hop-by-hop headers ...) |
| body | the HTTP body, at most 64 KB (`httpx.MaxBody`); over it, 413 `payload_too_large` |
| reply | `Mhai-Status-Code`, the allow-listed response headers (`Content-Type`, `Cache-Control`, `ETag`, `Retry-After`, `X-Request-ID`, `Idempotent-Replayed`, `Deprecation`, `Sunset`, `Link`, `X-Content-Type-Options`) and the response body |

- **The same chain.** The adapter builds an `*http.Request` and calls
  `authz.Mux.NATSHandler()`: request id, body limit, principal and access
  token, route rule, idempotency, the RLS transaction and audit, exactly
  the handlers HTTP runs. `TestAPIParityOverNATS` sends every route of
  `server.Table()` both ways, for the auth fixture matrix, and compares
  status, body and headers.
- **How the BFF key is bypassed, for NATS only.** `Mux.Handler` (HTTP) is
  `RequestID → httpx.BFF(keys) → body limit → routes`, unchanged.
  `Mux.NATSHandler` is `RequestID → requireNATS → body limit → routes`:
  instead of the key, it requires the mark `httpx.ViaNATS` puts on the
  request's context, which only `internal/natsapi` sets, on requests it
  builds from NATS messages. A context value can't be set from HTTP, so the
  HTTP path still needs the key, and the NATS handler answers 401 to any
  request that reaches it unmarked. What the key proved ("this came from
  our BFF") the NATS user proves: only `bff_<app>` may publish
  `api.v1.<app>` (`services/nats/nats.conf`, tested in
  `internal/bus`). `X-MHAI-Client-IP` is trusted for the same reason.
- **The audience is the subject's.** `api.v1.platform` is the platform
  audience, `api.v1.clinical` the clinical one; an `X-MHAI-Audience`
  header in the message is dropped.
- **The request id is core's.** `httpx.RequestID` makes it, as over HTTP; a
  BFF's `X-Request-ID` or `Mhai-Request-Id` never reaches the audit chain.
- **The reply subject is checked first.** NATS lets the requester choose
  the reply subject, and `allow_responses` lets core publish there once
  with core's permissions (a job on `jobs.v1.…`, a `$KV` write, a
  `$JS.API` call; `services/nats/README.md`). So a message is answered
  only when its reply subject is `_INBOX_bff.<app>.` (the app of the
  subject it came on) followed by literal tokens (non-empty, printable
  ASCII, no `*`, `>` or space). Otherwise nothing is sent and no handler
  runs; the refusal is logged at error level with the app only (never the
  subject) and counted.
- **Bounded.** At most `CORE_NATS_API_CONCURRENCY` messages at once per
  copy; past it, 503 `unavailable` at once. A reply larger than the NATS
  payload limit becomes 500 `internal_error`.
- **Not over NATS:** the probes (HTTP only, for the container's health
  check) and the API docs (`internal/apidocs` wraps only the HTTP
  handler). Both are 404 over NATS.

## Probes

- `GET /api/v1/healthz`: the process is up.
- `GET /api/v1/readyz`: 200 when Postgres answers and NATS is connected
  with every KV bucket reachable; otherwise 503 `not_ready`.

Neither needs the BFF key, and neither says what failed. They are served
over HTTP only; with `CORE_HTTP_API` off they are all the HTTP listener
serves.

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
  `mhai_app`, inherits an app role (`mhai_jobs` included), or owns the
  database, a schema or a table.
- Calls to intelligence (`internal/intel`): core checks the operation's
  roles before minting, a request/reply call is never retried, and no
  answer in time or no worker running is 503 `unavailable` (`overloaded`
  adds `Retry-After: 5`). A worker's `unauthorized`, `forbidden` and
  `invalid_request` all become one fixed 500 `internal_error`, audited as
  `intelligence.refused`. Jobs go only on `jobs.v1.<the caller's
  hospital>.<type>`; a full JOBS subject is 503 `unavailable`.
- The publish sweeper runs every 10 seconds as `mhai_jobs` (job columns
  only, never the question): a queued question with no `published_at`
  after 10 seconds is re-minted as its creator, with the creator's current
  roles, and published again under the same `Nats-Msg-Id`; a creator who
  can no longer use the service gets it cancelled, and one past its
  deadline fails with `deadline_exceeded`.

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
  without TLS on the compose network. Production needs TLS on both; core
  refuses to start outside development without the NATS TLS files.
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
internal/bus    NATS connection (TLS option), KV buckets, the JOBS and DEAD streams
internal/intel  the intelligence client, job publisher, publish sweeper, in-flight cap
internal/natsapi  the frontend contract over NATS (api.v1.<app>), on the same handler chain
internal/audit  the hash-chained audit log
internal/token  access tokens (EdDSA) and refresh tokens
internal/password  argon2id, the breached list, the concurrency gate
internal/config    environment
```
