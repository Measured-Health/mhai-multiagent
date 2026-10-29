# MHAI Architecture

## Services

The backend has two parts: the core service (Go) and the Python model
services, joined by NATS (ADR 0010,
`docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`). Since 28 Oct
the frontends' servers reach the backend over NATS as well (ADR 0011,
`docs/specs/2026-10-28-intelligence-boundary-design.md` §17); HTTP stays
until mhai-web's BFF has switched.

**Built (Plan 2):** the core service in `services/core/`: sign-in and
sessions, two-step verification, password change, the permission table
and principal middleware, hospital separation by row-level security, the
audit chain, idempotency, and the NATS connection with its KV buckets
(config in `services/nats/`). Its contract is `services/core/openapi.yaml`,
and it runs in Docker Compose (`make core-up`). **Built (Plan 3):** the Python
side behind NATS: the pipeline and echo workers, request tokens, grants for
BFF-direct calls, `POST /queries` and the API over NATS (`api.v1.<app>`),
with an end-to-end smoke test, run on the build server and wired into
CI (job `boundary-smoke`). **Next:** the hospital and platform
endpoints.

| Service | Job |
|---|---|
| **Core service** (Go, `services/core/`) | The API, sign-in and sessions, permissions, tenancy and audit. Every frontend call is decided here: either core serves it, or it grants intelligence a per-call token for it. |
| **NATS** (`services/nats/`) | The only link between the BFF, Go and Python (ADR 0011). Request/reply for calls someone waits on; JetStream for background jobs; KV for the core service's denylist and attempt counters. Replaces the old Redis queue stub, which is gone. |
| **Python services** (`services/intelligence/`) | The models: agents, retrieval and the knowledge base, guardrails, PII masking, inference. Reached only through NATS. Each request carries a token signed by the core service, and the service checks it before doing any work. |
| **EMR integration service** (planned, `services/emr/`) | Connects to each hospital's EMR/EHR (one connector per EMR type, ADR 0009): receives or fetches records, maps them to the FHIR-based model, normalises codes and units, keeps the per-hospital patient index, and tells core when new data arrives. Its own service because it holds outside credentials and talks to hospital networks; a fault or breach there must not reach sign-in and permissions. Connections are configured in the platform app. |
| **Documents service** (planned, `services/documents/`) | Uploads, letters and exports: virus scanning, storing files in object storage, signed download links, and handing new documents to intelligence's document reading (DIA) as jobs. Core still decides who may upload or download. |
| **Model serving** (planned, performance plan) | Runs the models on GPUs (vLLM or similar), separate from the pipeline logic, so model hosts stay stateless with no database access. See `docs/specs/2026-10-27-intelligence-performance-design.md`. |
| **PostgreSQL** | One database for all hospitals. Row-level security (RLS) keeps each hospital's rows apart. |
| **Notifications service** (planned, `services/notifications/`) | Handles every alert and notification: EMR abnormality alerts, decisions on clinicians' corrections, job and system updates. Delivers them in the app (the bell and pop-ups) and by opt-in email. Core decides who is told and publishes the event over NATS; the notifications service only delivers it, and never reads patient records itself. Intelligence never notifies anyone directly. |
| **Object storage** (planned) | Documents, letters and exports. |

```
browser → BFF (Next.js) → NATS ─┬→ core service ──→ PostgreSQL (RLS per hospital)
                                ├→ Python services (intelligence) ─→ model serving (planned, GPUs)
                                ├→ notifications service (planned) → bell, pop-ups, email
                                ├→ documents service (planned) ────→ object storage
                                └→ EMR integration (planned) ←────→ hospital EMR/EHR systems
```

Every service sits behind NATS with its own login and permissions. Only
core decides who may do what; the other services act on core's
decisions (a signed request token, or an event core published).

Security rules are written once, in Go. Python never takes a hospital or
user from a payload; it takes them only from the verified token.

## Services and systems flow

The BFF (mhai-web's Next.js servers) talks to the backend over NATS (ADR
0011). It is a NATS client on the server side only, never in the browser,
with one NATS user per app: `bff_clinical` and `bff_platform`. Until
mhai-web switches, the BFF still calls core over HTTP with its service key;
the steps below are the same either way.

API versioning (the major version in the path and subject, additive-only changes, a Sunset policy) is ADR 0012.

### An ordinary API call

```
browser
  → BFF (Next.js, clinical or platform app)
  → NATS  api.v1.clinical | api.v1.platform     only that app's BFF user may publish
  → core service (NATS adapter → the same http.Handler as HTTP)
      1. request id, body size limit; audience from the subject
      2. route lookup (unknown or unclean path → 404)
      3. principal: access token checked (EdDSA, aud mhai-bff, iss mhai-gateway)
      4. permission rule for the route (deny by default)
      5. Idempotency-Key for writes
      6. database transaction as the hospital in the token
         (row-level security: mhai_current_hospital())
      7. handler
      8. audit record on the hash chain, in the same transaction
  → reply over NATS to the BFF's own inbox (_INBOX_bff.<app>)
  → problem response on any error (no detail that helps an attacker)
```

Sign-in, refresh and two-step verification take the same path. Attempt
counters and the token denylist live in NATS KV, and they fail closed: if
NATS is down, sign-in is refused, not waved through. After the switch,
core's HTTP listener serves only health probes, on localhost.

### A call to the intelligence service that core makes

Built (Plan 3,
`docs/specs/2026-10-28-intelligence-boundary-design.md`); the smoke test
in `services/core/internal/smoke` runs it end to end (on the build
server; wired into CI as `boundary-smoke`). This is how a
clinical question runs: the BFF asks core (`POST /queries` over
`api.v1`), and core sends the job.

```
core service
  │ checks the user and their permission (steps 1–6 above)
  │ mints a request token for this one call: service, NATS subject,
  │ body hash, reply inbox, hospital, user, roles, patients, deadline, nonce
  ▼
NATS  inference.v1.<service>.<op>        (request/reply, someone is waiting)
      jobs.v1.<hospital>.<type>          (JetStream, long jobs; only core publishes)
  ▼
intelligence service
  │ checks the token: signature, subject, body hash, reply inbox,
  │ deadline, replay, schema, hospital and patient scope, role
  │ any failure → refused, no work done
  ▼
models and pipeline  (see "Intelligence service" below)
  │ the pipeline service alone reads and writes Postgres, under RLS,
  │ as the hospital in the token; model services have no database access
  ▼
reply over NATS → core service → audit → BFF → browser
```

### A call the BFF makes to intelligence directly

Only for operations marked `bff_direct` in core's operations table (none
in production yet). Intelligence asks core before doing anything:

```
BFF ── inference.v1.<service>.<op> + the user's access token ──▶ intelligence
                                                                   │ 1. size check
                                                                   │ 2. asks core: authz.v1.grant.<service>
core ◀─────── access token, operation, subject, body hash, ────────┘    (only that service may publish it)
  │           patient ids, reply inbox, request id
  │ checks the token, session, two-step, the operation's rule,
  │ hospital and patient scope, replay; audits the decision
  └── a request token bound to this subject, body, service and the BFF's inbox ──▶ intelligence
                                                                   │ 3. every check above, on that token
                                                                   │ core refused, unreachable or late → refused, no work
                                                                   ▼
                                                   reply over NATS to the BFF's inbox
```

It costs one extra NATS round trip, about 1 ms. Long jobs never take this
path: they are always published by core.

Long jobs are claimed with a lease and finished at most once. Their audit
facts go into an outbox table in the same transaction as the result, and
the core service copies them onto the hash chain, so only Go writes the
audit chain.

### Load balancing and scaling

```
browser ──HTTPS──▶ nginx ──▶ frontend (n copies)
                               │
                               ▼
                             NATS ──▶ core (n copies)
                                  └─▶ intelligence (n copies)
```

- **nginx** spreads browser traffic across the frontend copies. It also
  ends TLS and holds the IP allow list.
- **NATS** spreads requests across the core and intelligence copies with
  queue groups.
  - Each service subscribes with a queue group per subject (core in
    `core`, each intelligence service in a group named after it), so one
    member gets each request. Jobs go to one durable consumer per service
    that all its copies share.
  - A new copy joins automatically, and a dead copy stops getting
    messages. A request a dead copy was holding times out and the caller
    retries; a job it was running is redelivered to another copy.
  - There's no address list to maintain.
- **NATS itself** runs as a 3-node cluster in production, so it isn't a
  single point of failure. JetStream (jobs, dead letters, KV) is replicated
  three ways (R3).
- **The limit:** NATS spreads requests evenly, not by GPU load. Smarter,
  GPU-aware routing in front of the model servers is deferred to the
  parked [performance design](docs/specs/2026-10-27-intelligence-performance-design.md).

### Operations

Where each operational area stands. **Built** is in the code on `main`;
**Designed** has an approved spec or plan (linked); **Planned** is agreed
but has no design yet. The demo runs on a single host.

| Area | Status |
|---|---|
| Rate limiting and back-pressure | **Built:** sign-in limits (5 failures per login and client IP in 15 minutes, a per-login ceiling across addresses, two-step code attempts), held in NATS KV and failing closed. **Built (Plan 3):** a cap of 10 questions in flight per hospital (429 `rate_limited`), bounded JetStream streams (JOBS per-subject limits, DEAD), per-worker concurrency with `overloaded`, a bounded NATS API pool (503 `unavailable`) and grant rate limits ([boundary design](docs/specs/2026-10-28-intelligence-boundary-design.md) §9, §17.3). **Planned:** NATS per-user connection and subscription limits. |
| Timeouts, retries and idempotency | **Built** in core: `Idempotency-Key` on writes (72 hours), and retried counter writes with backoff. **Built (Plan 3)** for intelligence: per-operation timeouts, no silent retry of request/reply, job claim and lease with `+WIP`, retries by `Nak(delay)`, completion at most once, dead letters and a 10-minute deadline sweep ([boundary design](docs/specs/2026-10-28-intelligence-boundary-design.md) §9). |
| Failover and HA | **Planned:** a 3-node NATS cluster with JetStream at R3, and a Postgres replica with failover ([core design](docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md) §4.5). Core and intelligence already scale out through queue groups (above). The demo is a single host. |
| Backups and DR | **Planned:** Postgres and JetStream backups, a tested restore, and RPO and RTO targets. None exist today. **Required before any real patient data.** |
| Monitoring and alerting | **Built:** health and readiness probes, JSON logs with redaction, and `audit.Verify`, which finds a break in the audit chain when it is run (nothing runs it on a timer yet). An OpenTelemetry collector is in compose. **Built (Plan 3):** tracing (`traceparent` only, across NATS, always sampled) and metrics in core and the workers, with an attribute allow-list so spans carry ids, codes and numbers only; refusal, verify-time and queue metrics ([boundary design](docs/specs/2026-10-28-intelligence-boundary-design.md) §10.3). Off until `OTEL_EXPORTER_OTLP_ENDPOINT` is set; plaintext only to loopback or on request, with a warning outside development. **Planned:** dashboards and alerts on errors, latency, refused tokens and audit-chain breaks. |
| Secrets and key rotation | **Built:** startup refuses missing or placeholder secrets; each container gets only its own (a compose test checks it); NATS NKey seed files are supported. **Built (Plan 3):** a separate request-signing key, its public half in a trusted-keys file the workers mount read-only, planned and emergency rotation (`core keys`, audited; workers reload on SIGHUP) ([boundary design](docs/specs/2026-10-28-intelligence-boundary-design.md) §7.5). Secrets live in `.env` and key files on the host for now; **Planned:** a secret store. |
| Encryption | **Built:** HTTPS at nginx for the frontends (certbot); two-step secrets encrypted in the database (AES-256-GCM). **Built (Plan 3):** JetStream encrypted at rest in dev and tests (`NATS_JS_KEY`); the workers require NATS TLS and NKeys, and the pipeline `sslmode=verify-full` to Postgres, outside development. **Planned:** the NATS TLS block and encryption at rest on deployed hosts (none on the demo yet), TLS from core to Postgres, and encrypted disks and backups. |
| Deploys and rollback | **Built:** the frontends and the backend demo (core, NATS, Postgres; no intelligence workers) deploy automatically when CI on `main` is green; the backend deploy stops before migrating if existing rows would fail a new constraint. **Planned:** rolling deploys, backward-compatible migrations, and one-step rollback. |
| Environments | Demo only. **Planned:** separate staging and production; production never runs on the demo host. |
| Caching | **Built:** shared reference content (`kb_chunks` with no hospital), readable by every hospital and writable by none, by row-level security; a 30-second cache of user and hospital state in core. **Built (Plan 3):** the pipeline reading shared content under row-level security. **Designed:** a per-hospital prompt-cache salt for the model servers ([boundary design](docs/specs/2026-10-28-intelligence-boundary-design.md) §11). |
| Data lifecycle | **Planned:** retention, legal holds and hospital offboarding, on the platform roadmap (frontend contract §14.8). |

## Intelligence service

The sections below describe the Python pipeline inside
`services/intelligence/`. It runs only after the request has passed the
checks above.

### Pipeline flow

```
Patient Data → DIA (Swin-V2) → QDA (FT-Transformer) → CCA (Qwen-7B + RAG)
                    ↓                  ↓                       ↓
               G1: SSIM          G2: Confidence          G3: NLI Citation
                    ↓                  ↓                       ↓
                    └──────── HGA (DeBERTa-v3 MoE) ────────────┘
                                       ↓
                              T(y) = α·NLI + β·Coh + γ·Num + δ·SE
                                       ↓
                    ┌──────────────────────────────────────┐
                    │  T ≥ 0.55  →  PASS (auto-approve)   │
                    │  0.30 ≤ T  →  REGENERATE             │
                    │  T < 0.30  →  ESCALATE (human)       │
                    └──────────────────────────────────────┘
                                       ↓
                              Orchestrator Agent
                         (routing + feedback + audit)
```

### 4-layer guardrail cascade

Each response must pass ALL layers to auto-approve. Any single failure triggers review.

### Security pipeline order

Core: permission check → request token (audit written by core) → Worker: verify token → hospital-scoped read → PII mask → route → verify → result
