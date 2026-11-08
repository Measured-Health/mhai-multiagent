# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Platform analytics reads, part 7c-2 (OpenAPI 1.13.0, additive; Plan 7c
  Tasks 6 and 7). `GET /platform/analytics?period=week|month|quarter`
  serves Use, Answer quality and Hospitals of the latest published period,
  with medians over shown values and trends over the latest 12 weeks, 12
  months or 8 quarters; `alerts` is null and `changes` unavailable until
  7c-3; `{not_yet, first_from}` before the first period.
  `GET /platform/analytics/hospitals/{id}/series` serves one hospital's
  stored figure per period (`questions_per_clinician`,
  `needs_review_share`); `GET /platform/analytics/changes/{id}/series` is
  404 until What changed exists. Every route reads stored snapshots only;
  30 reads a minute per session. No migration.
- EMR integration, PR 3 (connections; off by default). Migration 00020:
  the `emr` schema and roles, `emr_connections` (configuration only, no secret column or key),
  credential metadata, sync runs and errors, declared private ranges that
  can't overlap across hospitals. `services/gokit`: `emrurl` (address and
  credential-origin checks), `seal` (credentials sealed to the emr service,
  bound to connection, field and origin, 60 s), `emrtypes` (the connector
  types and fields), and the request token's `stp` claim, which the emr
  responder now requires on step-up ops. The emr service stores and wipes
  credentials and tests a connection with the development stub. Core serves
  `/platform/emr/connector-types`, `/platform/emr/connections` and
  `/platform/hospitals/{id}/emr/connections` (OpenAPI 1.12.0, additive; new
  codes `not_tested`, `hospital_removed`, `no_type`), sealing credentials as
  they arrive; a change of address origin wipes them and switches the
  connection off. With `CORE_EMR_ENABLED` false the writes answer 503
  `not_ready`; `CORE_EMR_CONNECTOR_TYPES` offers nothing unless named; the
  credential routes need `CORE_IDEMPOTENCY_HMAC_KEY_PATH`.
- Patient Analysis, PR 2: the data foundation, **off by default** (OpenAPI
  1.13.0, additive; analysis backend plan Tasks 2 to 4, ADR 0015).
  Migration 00021 (its number is fixed: EMR takes 00020) adds the roles
  `mhai_analysis_writer`, `mhai_analysis_reader` and `mhai_analysis_job`
  (NOLOGIN, reached by SET ROLE as 7c's job role), subjects, coded facts,
  first-seen rows, frozen nested periods, dictionaries, the analysis switch
  (global and per hospital; no row is off) and definer functions that refuse
  a switched-off hospital, the future, and any real ingest under a demo
  switch. A core loop (one copy at a time) ingests, sweeps and freezes per
  switched-on hospital after its local 00:30. Four optional key files
  (`ANALYSIS_*_KEY_FILE`) seal and match patient references.
  `core analysis seed-demo` switches the synthetic demo hospital (created
  with `core hospitals create --demo`; `hospitals.is_demo` never changes)
  on with `DEMO-SYNTHETIC` references and writes a seeded synthetic cohort;
  a switch that was ever signed off never takes demo references again;
  `core analysis switch off` lowers a switch; both are audited. The login
  and session `user` gains `features {analysis, named_access}`.
- Platform analytics on calendar periods, part 7c-1 (OpenAPI 1.11.0,
  additive; Plan 7c, rulings R41a to R41a-4). Migration 00019 adds the
  analytics scheduler's role (`mhai_analytics_job`) and reader, frozen
  per-period counts, `analytics_snapshots` and the release epoch. A core
  loop (one copy at a time) freezes each finished month-aligned week, month
  and quarter once, guards it with its parent set and the published
  history (`internal/smallgroup`), and stores one snapshot. New route
  `GET /platform/guardrails/periods?period=week|month|quarter` serves the
  latest published period's guardrail statistics from that snapshot
  (503 `not_ready` until the first one).
- EMR integration, PR 2 (skeleton; off by default): `services/gokit/`, a Go
  module core and the emr service share, with the request-token package
  (moved from core; its subject rule is now a table that adds
  `emr.v1.ctl.<op>` with audience `emr`, and new shared vectors), a
  bounded replay set, `dbx.WithHospital` over a closed role set, the log
  redactor (moved), the emr control-operations table and the generated
  `mhai.emr.v1` contract. `services/emr/`: `emr serve` answers
  `emr.v1.ctl.health` after verifying every request (token, reply subject,
  replay, caller rule, request id, strict schema); `emr keys seal-init`.
  The `emr` NATS user (dev config), core's rights on `emr.v1.ctl.>` and the
  `EMR` stream, created only when `CORE_EMR_ENABLED=true`; the compose
  profile `emr`. Core's Docker build context is now `services/` (`-f
  services/core/Dockerfile services`) in compose, CI, the deploy workflow
  and the Makefile.
- EMR integration, PR 1 (docs): ADR 0009 accepted; ADR 0014 records the
  separate `services/emr/` with its own credentials and egress, and the
  reviewed route for scanned records; the document pipeline design is
  amended (normalisation by the emr service, `emr.v1.<hid>.fetched`);
  frontend requirements §12.1 and §14.11 filed; TODO lists the people items
  H1–H9 and the live-use gates.
- Cloud model providers, part A: the per-hospital model policy (OpenAPI
  1.10.0, additive). Migration 00018 adds `hospital_model_policy` (off by
  default; no row means local), the model route set by core on each new
  question, and the monthly token ledger reached only through core's
  `mhai_cloud_reserve`/`mhai_cloud_settle` functions. Platform admins read a
  hospital's policy with this month's usage and change it (`GET`/`PUT
  /platform/hospitals/{id}/model-policy`; a change needs step-up MFA and is
  audited as `hospital.model_policy_changed`), and `core hospitals
  model-policy set` does the same from the CLI. `agreement_ref`
  takes only an agreement code (`AGR-`/`DPA-` plus digits, or
  `DEMO-SYNTHETIC`). `query.completed` gains the closed `egress` and
  `cloud_outcome` facts, and a result gains `generated_by` (`local` or
  `cloud` with its provider), read from the decision itself. No cloud call is
  made yet (parts B to E).
- Cloud model providers, part B (the gate; no network code): the worker's
  cloud settings, off by default (`INTELLIGENCE_CLOUD_PROVIDERS` empty), with
  start-up refused on an incomplete provider: no model, key file or
  zero-retention reference, a Fable or Mythos model, a key file not mode
  0400/0440 or holding a placeholder, `DEMO-SYNTHETIC` outside
  `APP_ENV=demo` (new: production in every other respect). Any `OPENAI_*`,
  `ANTHROPIC_*`, `AZURE_OPENAI_*` or `XAI_*` variable refuses start-up for
  every worker, cloud on or off. Keys are held as `ApiKey` (redacted). With a
  provider configured, logging changes: records are redacted at creation
  (`args=None`, exception text pre-rendered), warnings go through logging,
  uncaught exceptions are logged as `CRITICAL`, and the HTTP clients' INFO
  request lines are off. The gate types (`MaskedText`, `CloudContext`'s
  two-field allow-list, sealed `SharedChunk`s, `CloudPrompt`), the egress scan
  over the exact request bytes (shape, this patient's identifiers everywhere,
  number and date patterns on the patient-derived blocks, size), and the
  budget port over core's reserve and settle functions.
- Cloud model providers, part C (the client and the job): strict reply
  validation (citations only of chunks sent, no links), a closed retry table,
  `CloudModelClient` (local retrieval and verification, a gated, scanned,
  budgeted cloud draft, a per-attempt deadline, SDK retries off) and
  `ModelRouter`: each job runs on the route core's claim gives it, and
  records the provider, `egress` and the outcome. Every cloud failure
  escalates; there is no fallback.
- Cloud model providers, part D (adapters): Anthropic (Messages, strict
  structured output), OpenAI (Responses, `store: false`) and xAI (chat
  completions; the `x-zero-data-retention` header checked on every reply),
  against synthetic fixtures. The scan hook refuses any request header not
  on the provider's list (`scan.ALLOWED_HEADERS`), so an SDK update that adds
  one fails closed. A reply that reports more tokens than its reservation is
  logged (`cloud.usage_overrun`), counted (`mhai_cloud_usage_overrun_total`)
  and, above 10% over, alerted (`mhai_cloud_alerts_total{kind="usage_overrun"}`).
- Cloud model providers, part E (deployment, smoke and docs): the
  `egress-proxy` service in `deploy/compose.yml` (Squid pinned by digest,
  profile `cloud`, off by default) on `backend` and a new `egress` network,
  allowing only `CONNECT` to port 443 of the hosts in `allowed-hosts`, matched
  by name with no reverse lookup, IP literals refused;
  `deploy/egress-proxy/test-egress.sh` checks it (A15). The worker's proxy
  setting is `INTELLIGENCE_CLOUD_PROXY` (not the process-wide `HTTPS_PROXY`),
  and with a provider enabled start-up refuses without a CA bundle at
  OpenSSL's paths. `scripts/cloud_smoke.py`, a manual live smoke a person
  runs on their own machine (never the build server or CI) with a key from
  standard input, and `scripts/scrub_cloud_fixture.py`, whose `--check` runs
  on every committed fixture in the unit suite. `deploy/README.md` "Cloud
  models" (enabling, key rotation, switching off); TODO.md lists the human
  gate before real patient data and the people items H1–H7.
- Document pipeline phase 0, part A: the extraction schema
  `mhai.extraction.clinical_document` 1.0.0 and the evaluation harness
  (`services/intelligence/intelligence/doceval/`). Synthetic patients and
  documents rendered from 15+ layouts per type, scan and phone-photo
  degradation, handwriting-like and hostile pages, intake for real scans,
  the synthetic-data scanner, set v1 with its 30/70 split, every design §6.3
  metric, a runner over any extractor (stubs, a tiny safetensors model, an
  OpenAI-compatible endpoint), the evaluation report with pass thresholds per
  gate and slice (clinical change control), and the `mhai-doceval` CLI.
  Synthetic data only; no model is promoted yet (part B).
- Platform operations for platform admins and auditors (Plan 4, part 4c;
  OpenAPI 1.9.0, additive). `GET /platform/health` (the sampler's stored
  reading, one KV get per poll, 503 when none is fresh), `GET
  /platform/jobs` (HMAC-bound cursor), `POST /platform/jobs/retry` (platform
  admins, 10 calls a minute, creator and second-person rules re-checked),
  `POST /platform/jobs/cancel` (step-up code) and `GET
  /platform/guardrails?period=7d|30d|90d` (new: small-group rules, whole UTC
  days, one stored snapshot per period and day with a 15-minute grace, 6
  reads a minute). Migrations 00015 to 00017. New smoke test
  `TestSmokePlatformOps`. Reads of health, jobs and guardrails are not
  audit-recorded; retry and cancel write `job.retried` and `job.cancelled`.
- The audit log for platform admins and auditors (Plan 4, part 4b;
  OpenAPI 1.6.0, additive). `GET /platform/audit` (filters, `q`, HMAC-bound
  cursor), `/platform/audit/{id}`, `/hospitals`, `/people` and
  `POST /platform/audit/undisplayable`; every page read is recorded as
  `audit.viewed` or `audit.searched` (once per session and filter set per
  5 minutes, in the same transaction). Chain checks: `POST
  /platform/audit/verify` (auditors may write here and nowhere else),
  `GET /platform/audit/verify/runs`, `/runs/{id}`, `/status`, and an
  auditor's `POST .../runs/{id}/outcome`; a check also runs daily at
  06:00 UTC and sets the `audit_chain_broken` gauge. A chain row the
  catalogue can't map to an mhai-web kind is sent as the bare entry.
  `SECURITY_CONTACT_NAME` and `SECURITY_CONTACT_EMAIL` are required outside
  development (the status route is 503 without them);
  `deploy/setup-host.sh` asks for them and checks them as core does. Migration
  00014. New smoke test `TestSmokeAuditLog` (a synthetic auditor over
  `api.v1.platform`: start a check, poll it to `intact`, read the status,
  find clinician B's `review_submitted` under `type=review`); the smoke
  stack now has a `bff_platform` password and a synthetic security contact.
- `mfa_enrolled` on the `MFARequired` sign-in reply (`POST /auth/login`,
  OpenAPI 1.4.0). `false`: the account has no two-step set up, so the
  frontend sends the challenge with an empty code to get the enrolment-only
  session; `true`: ask the user for a code. It is the same enrolment value
  the sign-in check read and that the challenge uses, and is sent only
  after the password was proven. Wrong-password replies, audit rows and
  logs are unchanged. Additive (`oasdiff`).
- Feedback and corrections with a two-person rule (Plan 4, part 4a).
  `POST /feedback` (validate, or suggest a correction on your own completed
  question), `GET /feedback/stats/mine`, `GET /corrections`, `/mine` and
  `/reviewed`, and `POST /corrections/{id}/approve|reject|withdraw`. A
  correction is approved only by a different, active clinician of the same
  hospital (403 `self_approval` otherwise, audited); core and the database
  both hold the rule (migrations 00011 to 00013). An approval queues an
  `approve_correction` job in the same transaction; the pipeline worker's
  second consumer, `pipeline_learning`, stores one per-hospital
  `feedback_buffer` row and core records `correction.applied`. Nothing is
  applied to guardrail weights, the knowledge base or any model. The
  `pipeline` NATS user gains the learning consumer's three subjects only.
  New smoke test `TestSmokeCorrectionApproved`. The contract change is
  additive (`oasdiff`).
- `core hospitals activate <id or short code>`: moves a hospital made with
  the CLI from `setup_incomplete` to `active`, so its clinicians can
  `POST /queries`, and appends `hospital.activated` to the audit chain in
  the same transaction. It runs as the provisioning login (`mhai_provision`);
  an already active hospital is a no-op, and any other status is refused
  (a deactivated hospital is reactivated through the platform app).
  Migration 00010 gives `mhai_provision` `SELECT (status)` and
  `UPDATE (status)` on `hospitals`, held by the policy `provision_activate`
  to `setup_incomplete` -> `active` only, and lets it append
  `hospital.activated`. The smoke test activates its hospital with the
  command instead of an UPDATE as the owner.
- The demo's NATS has a `bff_platform` NKey user (publish `api.v1.platform`,
  receive `_INBOX_bff.platform.>`, nothing else), and both BFF seeds are now
  files under `/srv/mhai-backend/secrets/bff/` (mode 400, uid 1001, mhai-web's
  container user) instead of lines in `.env`. `deploy/setup-host.sh` makes the
  platform pair, moves the existing clinical seed into its file (same key
  pair) and removes it from `.env`, and takes `--rotate-bff
  clinical|platform|all`. `deploy/compose.yml` requires
  `NATS_BFF_PLATFORM_NKEY`: **run the new `setup-host.sh` on the instance
  before merging** (deploy/README.md, "Before merging"). A test holds the
  demo's BFF permissions equal to the dev config's.
- The intelligence boundary runs end to end (Plan 3 T15). Compose runs the
  Python workers as `pipeline` (`intelligence worker --service pipeline`, its
  own NATS user and password, its own database login `intelligence_app`, the
  request-token trusted keys read-only) and `echo` (profile `test`), in place
  of the one-shot `intelligence` service; core gets `CORE_HTTP_API`.
  `docker-compose.smoke.yml` (stub models, echo on) and
  `.github/scripts/boundary-smoke.sh` (`make smoke`) run
  `services/core/internal/smoke` as the clinical BFF over NATS: sign in,
  `POST /queries`, poll, the result, and `echo.ping` through the grant, with
  the audit chain verified. New CI job `boundary-smoke`; CI also checks every
  compose file parses and that the image ships `operations.json` and
  `catalogue.json`. New `.env.example` entries: `INTELLIGENCE_DB_USER`,
  `INTELLIGENCE_DB_PASSWORD`. **`INTELLIGENCE_DB_PASSWORD` is now required
  in the dev `.env`**: `core-migrate` (and so `make core-up` and `make up`)
  refuses to start without it.
- The demo deploy stops before migrating if `clinical_queries` holds rows
  migration 00007's constraints reject (`deploy/premigrate-check.sh`).
- Clinical questions end to end on core's side (Plan 3 T12): `POST /queries`
  (clinicians; 202 `{query_id, status}`; 429 `rate_limited` past
  `INTELLIGENCE_QUERY_CAP` per hospital; `Idempotency-Key`), `GET
  /queries/{id}` (`running` shows as `processing`, `cancelled` as `failed` /
  `cancelled`) and `GET /queries/{id}/result`; another clinician's or
  hospital's id is 404. Contract version 1.2.0 (additive; the result also
  carries `trust_band` and `cited_chunk_ids`). Core drains the workers' audit
  outbox onto the chain (identity from the job row, never the fact), consumes
  dead letters (`core_dead`: marks the job failed, audits `job.dead_lettered`,
  re-mints a job refused for a key an emergency rotation removed), and
  deactivating a user or hospital cancels their queued and running questions
  in the database (migration 00009). New optional setting
  `REQUEST_TRUSTED_KEYS_PATH` (re-read on SIGHUP); `core keys untrust` takes
  `--emergency`, and request-key rotations are audited
  (`intelligence.key_rotated`) through the provisioning login in
  `DATABASE_URL`.
- Core serves the frontend contract over NATS (spec §17.1, `internal/natsapi`):
  one HTTP request per message on `api.v1.clinical` and `api.v1.platform`, in
  queue group `core`, run through the same handler chain as HTTP (request id,
  body limit, principal, route rule, idempotency, RLS and audit). The BFF's
  NATS user replaces the `X-MHAI-Service` key there and the subject fixes the
  audience. A reply subject other than the sending app's own inbox gets no
  reply at all. New settings: `CORE_HTTP_API` (`on` by default until
  mhai-web's BFF switches; `off` serves only the probes, on loopback) and
  `CORE_NATS_API_CONCURRENCY` (default 256; past it 503 `unavailable`). New
  error codes, over NATS only: 405 `method_not_allowed`, 413
  `payload_too_large`. Contract version 1.1.0.
- The API docs are now one site for core and intelligence. `/api/docs` gains a
  second tab, `/api/docs/intelligence`: the NATS operations (subject, kind,
  request and response messages, who may call, `bff_direct`, timeout, hospital),
  every message in `proto/` with its comments, the closed error-code set and a
  note on how a call is checked. Generated by `cmd/permgen` (so
  `TestGeneratedFilesUpToDate` covers it) and embedded; same CSP, no script, no
  external origin. Add `location = /api/docs/intelligence` to the api-dev nginx
  (included in `deploy/nginx/mhai-api-dev.conf`) and reload it.
- Public API docs on the demo server at `https://api-dev.measured-health.org/api/docs`
  (`deploy/nginx/mhai-api-dev.conf`): only the docs page, its script and the
  spec, GET only, behind its own allow list; the API itself stays private.
- API docs on the core service: `GET /api/docs` (Redoc 2.5.4, vendored, no
  CDN) and `GET /api/openapi.yaml`, both embedded in the binary, outside the
  authenticated API chain and never served over NATS. Switch:
  `CORE_API_DOCS` (default on in development, off otherwise); strict CSP and
  other headers; on in the demo and dev compose.
- API versioning policy (ADR 0012): major version in the path and subject,
  additive-only changes within it, `Deprecation`/`Sunset` for retirement
  (90 days' notice at the demo stage), `info.version` as semver (now 1.0.1).
  CI fails a pull request with a breaking change to `openapi.yaml`
  (oasdiff 1.34.1). `httpx.Deprecated` sets the retirement headers.

### Deprecated

- `GET /platform/guardrails?period=7d|30d|90d` (ADR 0012, R41a-3): from the
  7c-1 release every count and share is hidden and the responses carry
  `Deprecation`, `Sunset` (90 days on) and `Link`
  (`/docs/notices/guardrails-rolling-windows`); after the sunset the values
  are 422. Use `/platform/guardrails/periods`. Migration 00019 empties
  `guardrail_snapshots` and revokes `mhai_guardrail_counts` from
  `mhai_platform`.

### Changed

- The pipeline worker requires `sslmode=verify-full` for its database
  outside development. Core and the workers warn when
  `OTEL_EXPORTER_OTLP_INSECURE` is on outside development, and log why
  telemetry is off when an endpoint is refused.
- The demo's BFF NKey (`NATS_BFF_NKEY`) is now the clinical app's BFF
  (`bff_clinical`): it may publish `api.v1.clinical` and receive on
  `_INBOX_bff.clinical`. The intelligence workers are not deployed on the
  demo (documented in `deploy/compose.yml`); `CORE_HTTP_API` stays on there.
- The API docs show the Measured Health logo at the top of Redoc's sidebar
  (`info.x-logo`, served from `/api/docs/logo.png`, same origin). `info.version`
  1.0.2 (doc-only).

### Fixed

- `GET /api/openapi.yaml` is now `text/plain` with `Content-Disposition: inline`,
  so a browser shows the spec instead of saving it (`application/yaml` was
  downloaded). Redoc's Download link still saves a file.
- The docs page no longer logs a CSP violation for Redoc's footer logo from
  `cdn.redoc.ly`: a small same-origin script (`/api/docs/redoc-shim.js`)
  drops that one image request (the footer link text stays). The CSP is unchanged and the Redoc bundle is
  not modified. The nginx allow list for api-dev gains the shim and logo paths.

### Removed

- The Python `RBACManager`, `AuditLogger`, `SecurePipelineWrapper` (and
  `UserRole`, `UserSession`, `demo_m4_part2`) in
  `intelligence.security.hipaa`, and the `intelligence/queue/` stubs (Plan 3
  T13). Core decides permissions and writes the audit chain, the worker
  verifies core's request token, and the database helper scopes by hospital.
  `PIIMasker`, `EncryptionManager` and the latency profiler stay.
- Redis: the `redis` compose service and volume, `REDIS_URL`, `REDIS_PORT`,
  the `QUEUE_*` settings in compose and `.env.example`, and the `fakeredis`
  dev dependency (with `redis` and `sortedcontainers` in `uv.lock`). Nothing
  in Go or Python needs it; NATS carries the jobs.

### Security
- Cloud model providers: `intelligence/boundary/cloud_repo.py` (the budget
  port's SQL: core's reserve and settle functions only) is added to
  `tests/boundary/test_no_direct_db.py`'s `REPOS`, the files allowed to hold
  SQL. A security-test change, reviewed in PR B.
- Cloud model providers: no real patient data goes to a cloud provider until
  the human gate in TODO.md is done (masker bar including names without a
  title, legal check, clinical evaluation, core's keyed name-token hashes).
  `test_todo_keeps_the_real_data_gate` fails if that entry is removed, and
  `test_every_abuse_case_has_a_test` checks every A/K case in the plan has
  its named test.
- The patient-scoping and two-person-review regression tests moved off
  `SecurePipelineWrapper` onto the orchestrator and the boundary
  (`chunk_visible_to`, `load_chunks`, `OrchestratorModelClient`) with the same
  assertions: a patient's chunks never reach another patient's answer, and a
  correction applies only after a second, different clinician approves. The
  role and audit checks the wrapper tested are core's and covered by core's
  tests; a new test asserts the boundary exposes no feedback or approval
  operation.
- `core migrate up` no longer lets a login's SCRAM verifier reach the
  Postgres server log when setting the password fails. Postgres logs a
  failed statement in full (`log_min_error_statement`). `EnsureLogin` now
  sends the attributes and the password in separate statements, probes
  with a secret-free statement that it may set the password before sending
  it, and, as a superuser, turns `log_min_error_statement` off for that
  transaction.
- Sign-in has a per-login ceiling across every client IP (20 failures an
  hour, keyed by hash, failing closed) on top of the (login, IP) lockout,
  and argon2 runs behind a process-wide gate (one slot per CPU; 503 when
  saturated).
- `core serve` refuses a database login that is a member of any role
  besides `mhai_app`, and refuses to start with the idempotency middleware
  missing.
- Retrieval and citation checks are scoped to the queried patient. Other
  patients' records are never returned, and citing one is redacted (#1).
- Reserved ids (`reference`, `unassigned`, `unknown`) can no longer be used
  as patient ids, and patient files without an id are skipped.
- The orchestrator fails closed. A missing or failing CCA/HGA escalates the
  result as `degraded` instead of inventing a score, and thresholds are
  passed per call instead of mutating shared state (#2).
- Doctor corrections need approval from a second, different clinician
  before they change the knowledge base, persona vector or trust weights.
  Approved corrections are PII-masked, patient-scoped and attributed (#3).
- NLI verifiers read the label order from the model. The configured model's
  order was being misread, so unsupported claims passed as entailed.

### Added
- Backend demo deployment on the shared preview instance (`deploy/`,
  `.github/workflows/deploy.yml`). After green CI on `main`, the core
  image is built for arm64, pushed to `ghcr.io/measured-health/mhai-multiagent-core`
  and deployed over SSH to `/srv/mhai-backend` (Postgres, NATS with
  JetStream, migrations, core), then checked with `/api/v1/healthz` and
  `/api/v1/readyz`. Off until the repository variable `DEPLOY_ENABLED` is
  `true`. Only core (127.0.0.1:8092) and NATS (127.0.0.1:4222, NKey users
  only) are published, on loopback; every container is memory-capped,
  read-only and without capabilities. `deploy/setup-host.sh` prepares the
  host once: secrets in `.env` (mode 600), the NATS NKeys, the volumes and
  the internal network `mhai-bff` for the frontend's BFF, which has a
  placeholder NATS user with no publish rights until Plan 3. The deploy
  checks the instance's SSH host key (`DEPLOY_HOST_FINGERPRINT`) and
  refuses to run without it.
- The core service (Go, `services/core/`, Plan 2): sign-in with lockout,
  rotating refresh tokens, two-step verification (TOTP, backup codes,
  trusted devices, step-up), password change, device sessions, terms
  acceptance, the permission table and principal middleware, hospital
  separation by row-level security, a hash-chained audit log and
  Idempotency-Key support. The contract is `services/core/openapi.yaml`,
  checked by a test against the served routes and the error codes.
- The core service runs in Docker Compose (`make core-up`): `core-migrate`
  (owner connection) also creates its two logins: `core_app`, a member of
  `mhai_app` only, for the service, and `core_provision`, a member of
  `mhai_provision` only, for the seed CLI (`core-cli`). `core-keys` makes
  the dev signing key. The core service's environment holds only its own
  secrets.
- `GET /api/v1/readyz`: 503 unless Postgres and NATS (with every KV bucket)
  answer. `core keys new|ensure` generates the access-token key.
- Expired Idempotency-Key rows are swept every 10 minutes
  (`mhai_sweep_idempotency()`, migration 00006).
- Docker Compose stack: Postgres, Redis, Ollama with model pull, the app, an
  optional OpenTelemetry collector, and a GPU override, with Make targets.
- The CCA can generate through Ollama (`CCA_BACKEND=ollama`).
- `scripts/e2e_models.py`: end-to-end check with real models on synthetic
  data.
- `SKILLS.md` security rules, contribution workflow for agents and humans,
  ADRs, PR and issue templates, `SECURITY.md`, CODEOWNERS and Dependabot.
- CI enforces lint, format, type checks, tests, coverage of changed lines and
  a secret scan. Tagged releases build and push the container image.

### Changed
- **Breaking:** the services moved under `services/`. The Go service is
  `services/core/` (module `.../mhai-multiagent/services/core`, binary
  `core`, compose services `core-migrate`; `GATEWAY_ENV` is now `CORE_ENV`).
  The Python package `mhai` is now `intelligence`, in
  `services/intelligence/` with its own `pyproject.toml` and `uv.lock`; the
  CLI is `intelligence` and the compose service `intelligence`. The NATS
  config is in `services/nats/`.
- **Breaking:** the package moved from `src/mhai` to `mhai`.
- **Breaking:** a `correct` feedback returns `{correction_id, status:
  "pending_review"}`; it is applied by `approve_correction`.
- **Breaking:** Python 3.12 only.
- Project managed with uv (`uv.lock`); CPU PyTorch by default.
- Two-stage Docker image (7.1 GB to 2.5 GB, CPU), with pinned base images.

### Fixed
- `core migrate up` can run more than once as a non-superuser CREATEROLE
  owner (the managed-Postgres path): resetting a login names only the
  attributes that are wrong, so Postgres 16 no longer refuses it for naming
  `NOBYPASSRLS` and `NOREPLICATION`.
- `PIIMasker` no longer corrupts text when patterns overlap (e.g.
  `[PHONE_REDACTED]TED]`), counts each masked span once, keeps labels such as
  "Patient:" and "MRN:", and masks a leading "+1" or "(" in phone numbers
  (#7).
- `mhai.agents` could not be imported (its `__init__` exported missing
  classes).
- Imports left over from the original notebooks.

## [1.0.0] - 2026-06

### Added
- Initial production release from Colab notebooks (M1–M4, ~18,500 lines).
- Five agents: DIA, QDA, CCA, HGA, Orchestrator.
- Four-layer hallucination cascade.
- HIPAA security layer: PII masking, AES encryption, RBAC, audit chain.
- 1000-case E2E regression suite.
