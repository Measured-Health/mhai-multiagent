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
`core`, Go 1.26.

## Commands

```bash
core serve                         # the HTTP service (HTTP_ADDR, default :8080)
core migrate up | status           # goose migrations, then the deployment logins
core keys new --out PATH           # a new Ed25519 access-token key (PKCS#8 PEM, mode 0600)
core keys ensure --out PATH        # keep PATH if it is a usable key, else make one
core hospitals create --name N --short-code CODE --time-zone TZ \
    [--require-mfa-for-clinicians] [--allow-trust-device]
core hospitals activate ID|CODE    # setup_incomplete -> active, audited (see "Running it in compose")
core users create --email E --name N --roles clinician,hospital_admin [--hospital CODE]
core users roles EMAIL --add analyst|--remove analyst --by ADMIN_EMAIL   # the analyst rules apply (migration 00030)
core hospitals mark-demo ID|CODE                             # once: a hospital made before is_demo (the live demo's), audited
core analysis seed-demo ID|CODE [--seed N] [--months 1-24]   # a hospital created with --demo only
core analysis switch off ID|CODE | --global                  # lower an analysis switch, audited
core hospitals legal-hold place|lift ID|CODE --reference REF  # EMR: no deletion while a hold is open, audited
core emr exclude --hospital ID|CODE (--patient ID | --mrn-stdin) --reason CODE   # as the EMR operator login
core emr remove-hospital-data ID|CODE                        # removal_scheduled, no hold, credentials wiped first
```

`emr exclude` reads an MRN from stdin (`--mrn-stdin`), never from an
argument. EMR purges and retention run in `core serve` itself (only with
`CORE_EMR_ENABLED`; the nightly sweep also needs
`CORE_EMR_RETENTION_SWEEP`), one core copy at a time.

`users create` never takes the password as an argument. It prompts on a
terminal and reads one line from stdin otherwise. New accounts must change
their password at first sign-in. Errors and output never contain the
email, name or password.

From `services/core/` (or `make core-test` / `make core-lint` from the root):

```bash
gofmt -l . && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...
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
| `core_provision` (`PROVISION_DB_USER`) | `mhai_provision` only | `core hospitals create`, `core hospitals activate`, `core users create`, `core users roles`, `core keys` rotations |
| (none; `EMR_ADMIN_DB_USER`) | `mhai_emr_admin` only | `core emr exclude` (the operator's fallback for a hospital's written request) |

- `core serve` refuses any other login at start-up (`db.CheckLoginRole`):
  a superuser, `BYPASSRLS`, an owner, a login that inherits an app role, or
  one that is a direct member of any role besides `mhai_app`. The
  provisioning login is refused for that reason.
- `mhai_app` holds nothing itself. Each transaction runs
  `SET LOCAL ROLE mhai_gateway`, `mhai_platform` or `mhai_auth`, so a
  query outside `db.With*` or `authz.Tx` fails.
- `core migrate up` creates or resets the logins when `CORE_DB_USER` /
  `CORE_DB_PASSWORD`, `PROVISION_DB_USER` / `PROVISION_DB_PASSWORD` and
  `INTELLIGENCE_DB_USER` (default `intelligence_app`) /
  `INTELLIGENCE_DB_PASSWORD` are set. The pipeline's login is a member of
  `mhai_worker_app` only, which holds `mhai_worker` without inheriting it
  (00008), so it holds nothing until `SET LOCAL ROLE mhai_worker`. It sets the password and revokes any other membership. Placeholder
  or short passwords (under 16 characters) are refused before the database
  is touched.
- On managed Postgres, run `migrate` as a `CREATEROLE` admin instead of a
  superuser.

## Environment (`core serve`)

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | the `mhai_app` login; a placeholder password is refused. Outside development, a `postgres://` URL with `sslmode=verify-full` |
| `NATS_URL` | yes | `nats://gateway:PASSWORD@host:4222` in dev; a placeholder password is refused |
| `NATS_NKEY_SEED_PATH` | production | the `gateway` user's NKey seed file (see `services/nats/README.md`) |
| `NATS_TLS_CA_PATH`, `NATS_TLS_CERT_PATH`, `NATS_TLS_KEY_PATH` | outside development | PEM files: the CA for the NATS server's certificate, and core's client certificate and key. All three or none; required unless `CORE_ENV` is `development` |
| `NATS_JS_REPLICAS` | no | replicas for `JOBS`, `DEAD` and the KV buckets: `1` (default, dev compose), `3` on the production cluster (R3), or `5`. Outside development, 3 or more unless `CORE_ALLOW_SINGLE_NODE` |
| `CORE_ALLOW_SINGLE_NODE` | no | `true` lets a non-development core run with fewer than 3 replicas (a single-node install); serve logs a warning. Default `false` |
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
| `ANALYSIS_REF_HMAC_KEY_FILE`, `ANALYSIS_REF_ENC_KEY_FILE`, `ANALYSIS_DRILL_KEY_FILE`, `ANALYSIS_NOTE_KEY_FILE` | no (all four or none) | Patient Analysis's keys: each file holds 32 random bytes in base64 (`openssl rand -base64 32`), mode 0600 or 0640, all four different. Start-up refuses a missing file, a placeholder, a wrong length, a file others can read or two equal keys. Without them, the nightly run fails closed for a signed-off hospital with consultations to ingest; without them no count has a drill and every de-identified list is 422 `too_small` (the demo needs them only for the drill-down) |
| `CORE_API_DOCS` | no | `true` or `false`: serve the API docs page. Default: on when `CORE_ENV` is `development`, off for every other value (production included). Anything else is refused at start-up. |

Secrets must not contain `changeme`, `change-me` or `placeholder`. Errors
name the variable, never the value. The core service reads only its own
secrets: the other NATS users' passwords, the JetStream key and the
database owner password are not in its environment.

## Patient Analysis (off by default)

Analysis (ADR 0015; plan `docs/plans/2026-11-07-analysis-backend.md`) is
off until a switch row says otherwise: with no row, a hospital's position is
`off`, and ingest refuses it in the database itself. Before the people's
sign-offs (H1 to H3), the only way on is `core analysis seed-demo`, which
uses the references `DEMO-SYNTHETIC` and writes a seeded synthetic cohort,
for a hospital created with `core hospitals create --demo` only
(`hospitals.is_demo`, which no role can change; the database refuses demo
references on any other hospital). A hospital made before the flag existed,
such as the live demo's `SYN`, is marked once with
`core hospitals mark-demo SYN` (audited `hospital.marked_demo`); the
database refuses it for a hospital with a real cloud agreement and for any
hospital once an analysis switch has been signed off. A switch row that has ever carried a
real sign-off (`ever_signed_off`) never takes demo references again, and
provisioning can only lower a switch. A hospital under a demo switch never
ingests a real consultation.

The nightly run is a core loop (one copy at a time, an advisory lock) that
runs as `mhai_analysis_job`: a NOLOGIN role the core login reaches only by
`SET LOCAL ROLE` inside `db.WithAnalysisJob`, which no route handler names
(an AST test). Per hospital whose switch is on, after its local 00:30, once
a hospital day: ingest (signed-off hospitals only; references become an
HMAC and an AES-GCM ciphertext bound to the hospital), the 25-month sweep,
then the freeze: a block at local midnight 3 days after its end, a month
with its last block, a quarter with its last month. Late facts are stored
and never counted. It logs hospital ids, the step and a fixed reason only,
and counts `analysis_late_facts_total` and `analysis_freeze_failed_total`
(alert `analysis_freeze_failed`).

The aggregate reads (PR 4, migration 00025) are `GET /analysis/filters` and
`GET /analysis/{view}`, for hospital admins and analysts with two-step,
answering 404 while the hospital's switch is off. Each answers one frozen
period (`period` and `start`, or `from` and `to` spanning exactly one) from
a stored body: the engine (`internal/analysis`) computes a context the first
time it is asked for, parents first (child periods, then filter parents),
under an advisory lock per hospital and period, against that period's
record of everything already published (`disclose.Ledger`, kept in memory
between requests and synced from the stored bodies), and stores it once
through `mhai_analysis_snapshot_put`, which refuses a shown count of 1 to 4.
The raw reader (`internal/analysis/cells`) is the engine's only; the routes
never import it. A body that can't be computed within 2 s answers 503
`not_ready` and is finished in the background. Nothing logs a query string.

The de-identified drill-down (PR 5, migration 00028) is
`GET /analysis/patients`: the patients behind one shown count of a stored
body, found by its drill key (derived when the view is served, never
stored; `ANALYSIS_DRILL_KEY_FILE`). Drills exist only where the rows show the
group (age band, sex, setting, a pair, a finding, under condition, age
band, sex and setting filters). In one transaction as the person
(`db.WithHospitalUser`): the role, two-step and account re-read, the list
limits (`mhai_analysis_list_admit`), the list opened
(`mhai_analysis_open`: recounted and checked against the cell, patients
reserved against the quarter's cap of 300, rows recorded, pids drawn), and
its `analysis.patients_viewed` entry last; a deferred trigger refuses a list
without its entry. `GET /platform/audit/{id}/patients` reveals a list's pids
to platform admins and auditors through `mhai_audit_view_pseudonyms`, owned
by `mhai_analysis_auditor`, and records each reveal.

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
docker compose run --rm -T core-cli hospitals activate HAV   # or the hospital's id
printf '%s\n' "$SYNTHETIC_PASSWORD" | docker compose run --rm -T core-cli users create \
  --email synthetic.clinician@havana.example --name "Synthetic Clinician" --roles clinician --hospital HAV

curl -s localhost:8080/api/v1/auth/login \
  -H "X-MHAI-Service: $BFF_KEY" -H "X-MHAI-Client-IP: 198.51.100.20" -H 'Content-Type: application/json' \
  -d '{"username":"synthetic.clinician@havana.example","password":"..."}'
```

`make up` also starts the pipeline worker (and Ollama); `docker compose
--profile test up -d` adds the echo worker. A hospital made with the CLI
is `setup_incomplete`, and `POST /queries` needs an `active` one, so
activate it before asking a question.

`core hospitals activate <id or short code>` runs as the provisioning login
(`SET LOCAL ROLE mhai_provision`, like `hospitals create`). It moves the
hospital from `setup_incomplete` to `active` and appends
`hospital.activated` (no actor, `{"via": "cli", "from":
"setup_incomplete"}`) in the same transaction. An already active hospital
is left alone ("already active; nothing changed", nothing audited). Any
other status is refused: a `deactivated` hospital is reactivated through
the platform app, with a reason and two-step (frontend §14.2), and the
removal states never return to `active`. Migration 00010 gives
`mhai_provision` `SELECT (status)` and `UPDATE (status)` on `hospitals`,
held by the policy `provision_activate` to exactly `setup_incomplete` →
`active`. There is no `deactivate` command: deactivation takes a reason,
signs staff out and cancels the hospital's questions (00009), which is the
platform app's.

### The end-to-end smoke test

`make smoke` (`.github/scripts/boundary-smoke.sh --build`, CI job
`boundary-smoke`) starts `docker-compose.yml` with
`docker-compose.smoke.yml` (the stub models, echo on, no Ollama) under
throwaway generated secrets and runs `go test -tags smoke
./internal/smoke/...` against it, as the clinical BFF over NATS:
`TestQuestionEndToEnd` (sign in, `POST /queries`, poll, the result, and
`query.completed` on a verified chain), `TestEchoNeedsToken`,
`TestBFFDirectEchoEndToEnd` (`echo.ping` through a grant; the chain holds
`intelligence.granted`) and `TestQuestionOverHTTPStillWorks`. `TestSmokeCorrectionApproved` (Plan 4a)
runs the two-person rule: clinician A asks and suggests a correction, A's
approve is 403 `self_approval`, clinician B of the same hospital approves
(204), `correction.applied` lands on a verified chain, and exactly one
`feedback_buffer` row exists. `TestSmokeAuditLog` (Plan 4b) signs in a
synthetic auditor over `api.v1.platform` (two-step enrolled with a code
the test computes), starts `POST /platform/audit/verify` and polls the run
to `intact`, reads `GET /platform/audit/verify/status` (200 only with the
stack's synthetic `SECURITY_CONTACT_NAME` and `SECURITY_CONTACT_EMAIL`), and
finds clinician B's `review_submitted` in the log filtered to `type=review`.
`TestSmokePlatformOps` (Plan 4c) signs in a synthetic platform admin, waits
for the sampler's first reading of `GET /platform/health` (fresh, `workers`
ok), forces a dead letter and finds it in `GET /platform/jobs`, retries it
with `POST /platform/jobs/retry`, and reads `GET /platform/guardrails?period=7d`
(deprecated: the headers, every figure hidden) and
`GET /platform/guardrails/periods` for each kind (`not_ready` until a period
after the release is published, then the shape), then
`GET /platform/analytics?period=month` (not yet published, or the latest
month with no count of 1 to 4 shown) and one hospital's monthly series;
then stores two synthetic 2020 snapshots with a jump in questions, runs the
scheduler's evaluation on them, follows the flagged change up as a platform
admin, and checks that an auditor sees the follow-up and is refused a `PUT`.
Set
`POSTGRES_PORT`, `NATS_PORT`, `CORE_PORT` and `SMOKE_PROJECT` when the
defaults clash.

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

## Feedback and corrections (Plan 4, part 4a)

`POST /feedback` (clinicians; `validate` or `correct`, on their own
completed question), `GET /feedback/stats/mine`, and
`GET /corrections`, `/corrections/mine`, `/corrections/reviewed`,
`POST /corrections/{id}/approve|reject|withdraw` (`internal/feedback`).
Rules: the decider is always the token's user, never the body's; the
submitter can't decide their own correction (403 `self_approval`); a
decider must be an active clinician of the same hospital; another
hospital's correction is 404. The same rule is held by the database (a
check on `feedback_events`, and the RESTRICTIVE policy
`buffer_needs_approval` on `feedback_buffer`, migrations 00011 to 00013).
An approval writes its audit row and queues an `approve_correction` job in
one transaction, then publishes it after the commit (the publish sweeper
retries it). Free text (`corrected_diagnosis`, `reason`, the rejection
note) is never logged or audited; refusals are audited with the key
`reason`. Core's `pipeline_learning` JetStream consumer delivers
`jobs.v1.*.approve_correction` to the pipeline worker, and the drainer
turns the worker's fact into `correction.applied` against the correction.

## The audit log (Plan 4, part 4b)

Platform admins and auditors (platform audience, two-step always) read the
hash chain as the frontend's audit events (`internal/platform`,
`internal/audit`):

- `GET /platform/audit` lists newest first with the filters `hospital`,
  `person`, `type`, `outcome`, `flagged`, `from`, `to` and an exact-id `q`;
  `/{id}`, `/hospitals` and `/people` read one entry and the pickers. The
  cursor is a hex HMAC bound to the session and the filters. A pseudonym
  shape, in a query, a cursor or a response, is refused or never sent.
- A catalogue maps each action to an mhai-web `type` and `kind`. An action
  with no kind (17 from Plans 2 and 3, and the per-job `job.retried` and
  `job.cancelled` rows) stays a withheld row: the bare entry (`type:
  security`, `kind: unknown`), so nothing leaks and the app reports it as
  undisplayable. `review_submitted` carries no `facts.state` yet.
- Every page, detail, people and hospitals read is itself recorded
  (`audit.viewed`, or `audit.searched` for `q`, a person or hospital search
  and a read of the chain checks), deduped per session and filter set for 5
  minutes in Postgres (`idempotency_keys`, scope `audit.read`) in the same
  transaction as the row.
- Chain checks: `POST /platform/audit/verify` starts one (or returns the
  running one); it walks the whole chain in keyset pages inside one
  read-only snapshot as `mhai_platform`, with a 10-minute budget, and is
  `intact` only when the whole chain was walked. A daily check runs at 06:00
  UTC. `GET .../verify/status` is the header's read (503 `not_ready` without
  `SECURITY_CONTACT_NAME` and `SECURITY_CONTACT_EMAIL`); an auditor can add
  an outcome note to a broken check. Auditors can write on these routes only
  (`AuditorWrite`).

## Platform operations (Plan 4, part 4c)

Platform audience, two-step always; none of these reads is audit-recorded.

- `GET /platform/health` serves the reading the sampler stored (one core
  copy samples every 30 s, others stand by); a poll is one KV get, never a
  database statement or worker call. 503 `unavailable` when the reading is
  missing, older than 2 minutes, dated ahead, or the wrong shape.
- `GET /platform/jobs` lists failed, retrying, retried and cancelled
  questions and jobs, newest failure first, 30 days. `POST /platform/jobs/retry`
  (platform admins, 10 calls a minute per session) re-queues each job and
  publishes it as its creator, after re-checking that the creator is still
  usable and the second-person rule holds; `POST /platform/jobs/cancel`
  needs a fresh code, and the note is stored apart and never logged. A bad
  body is 422 `validation_failed`.
- `GET /platform/guardrails/periods?period=week|month|quarter` (Plan 7c-1)
  serves the HGA decision counts of the latest published month-aligned week,
  month or quarter from its one stored snapshot (`analytics_snapshots`); no
  request computes anything. The analytics scheduler
  (`internal/analytics/scheduler`, every 5 minutes, one copy under an
  advisory lock, as `mhai_analytics_job`) freezes each finished period once
  through migration 00019's functions, which refuse any window that isn't a
  nested period, and guards it with `internal/smallgroup`'s joint guard (the
  parent set and an exact check over the published history). 503
  `not_ready` until the first period of a kind after the release.
- `GET /platform/analytics?period=week|month|quarter` (Plan 7c-2) renders
  Use, Answer quality and Hospitals of the latest published period from its
  stored snapshot, each cell as stored; medians over the hospitals' shown
  values (a count median between 0 and 5 hidden), trends from the stored
  all-hospitals cells of the latest 12 weeks, 12 months or 8 quarters (one
  period each). `alerts` is null; an area is `{unavailable: true}` when its snapshot couldn't be
  computed or read, and `{not_yet: true, first_from}` before the first
  period. `GET /platform/analytics/hospitals/{id}/series?measure&period`
  (`questions_per_clinician`, `needs_review_share`) is the hospital's stored
  cell per period with the other hospitals' median. 30 reads a minute per
  session across the three; no read computes a figure or touches a clinical
  table.
- What changed (Plan 7c-3, migration 00022): after each new week snapshot
  the scheduler compares the week with the month that finished before it,
  from the two stored bodies' shown cells only (`analytics.Changes`, the
  thresholds in `analytics.Thresholds`), and opens, updates or closes rows
  of `analytics_changes` in one transaction with the week's
  `analytics_change_runs` row. `GET /platform/analytics` serves them as
  `what_changed {updated_at, window, items}` (the same for every period;
  `{unavailable: true}` before the first evaluation; `changes` stays
  `{unavailable: true}`, as widening it would not be additive);
  `GET /platform/analytics/changes/{id}/series?period` is an open change's
  measure per stored period. `PUT` and `DELETE
  /platform/analytics/changes/{id}/follow-up {note?, version}` are platform
  admins' (`Idempotency-Key`; 409 on a stale version or a closed change, 403
  for another admin's note; ten writes a minute per session, 429), each
  audited in its transaction with ids only;
  the note stays in `analytics_follow_ups`.
- `GET /platform/guardrails?period=7d|30d|90d` is deprecated: every count
  and share hidden, `Deprecation`, `Sunset` and `Link` headers, 422 after
  the sunset. 6 reads a minute per session across both routes (429).

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
  finish setup, but `POST /queries` needs an `active` hospital (401
  `unauthenticated` otherwise): an operator runs `core hospitals
  activate` first (TODO.md, the setup flow).
- **Self-enrolment of two-step** stays open on first sign-in until the
  invite flow exists: whoever has the first password owns the second factor.
- **Refusal audit rows are unthrottled** (a refused approval is audited
  every time; residual risk, TODO.md).
- **Postgres TLS.** The pipeline worker requires `sslmode=verify-full`
  outside development; core does not yet (TODO.md).

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
internal/smoke  the end-to-end smoke test (build tag smoke; make smoke)
internal/natsapi  the frontend contract over NATS (api.v1.<app>), on the same handler chain
internal/audit  the hash-chained audit log
internal/token  access tokens (EdDSA) and refresh tokens
internal/password  argon2id, the breached list, the concurrency gate
internal/config    environment
```
