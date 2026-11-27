# MHAI architecture

How the whole MHAI system fits together, as it is on 27 Nov 2026. Start
here. This file explains the parts and how they connect; the detail is in
the linked designs, ADRs and run-books. The two apps are described in
mhai-web's ARCHITECTURE.md.

Status words used below:

- **Built:** in the code on `main`.
- **On live:** running on the live site (ai. and platforms.measured-health.org).
- **Off on live:** built, but switched off on the live site.
- **Designed, not built:** an approved design, no code yet.
- **To do:** agreed, not designed.

Real patient data still waits on the go-live sign-offs listed in TODO.md.
Switching a part on does not sign anything off.

## 1. Product context

- **MHAI** is clinical decision support. A clinician asks a question; the
  answer is written from reviewed guidelines by our own model on our GPU, or
  by an approved cloud model; every claim is checked against what was
  retrieved; anything unsafe or unsupported goes to a person. It has two
  apps (mhai-web: the clinical app and the platform admin app), the core
  service, and the model pipeline (this repository).
- **DocFlow** is Measured Health's scanned-records product (org repos
  `emr-api` and `emr-web`). Hospitals keep their paper history in it. Its
  link to MHAI will be the documents pipeline (section 6), reached through
  the EMR connector `document_archive`, which the platform app lists as
  "DocFlow". **Not built yet**: the connector is declared only.
- **Lab results explained to patients:** an idea in TODO.md only. Not
  designed.

## 2. The system at a glance

```
   Clinicians, hospital admins              Measured Health staff
              │ HTTPS                               │ HTTPS, IP allow list
┌─────────────┼─────────────────────────────────────┼ live host (one GPU server) ┐
│                                                                                │
│  nginx ─▶ clinical app (BFF)          nginx ─▶ platform admin app (BFF)        │
│            └──────────────┐   ┌──────────────────┘                             │
│                           ▼   ▼     network mhai-bff (internal)                │
│    NATS (TLS, a client certificate and an NKey for every client)               │
│      │            │                 │                  │                       │
│      ▼            ▼                 ▼                  ▼                       │
│    core      pipeline worker    notifications         emr                      │
│      │            │                 │                  │                       │
│      ▼            ▼                 ▼                  ▼                       │
│    Postgres (TLS only, row-level security per hospital)                        │
│    network backend: internal, no route out                                     │
│                                                                                │
│  Ways out (each on its own network; each refuses what it wasn't told):         │
│    pipeline ─▶ gpu-model: our own model on the L4 (internal, stays here)       │
│    pipeline ─▶ egress-proxy ─▶ cloud provider (masked text; off by default)    │
│    notifications ─▶ mail-relay ─▶ email provider (SMTP over TLS)               │
│    emr ─▶ egress-emr ─▶ hospital systems (outbound only, see below)            │
│                                                                                │
│  Not deployed: the documents service (section 6).                              │
└────────────────────────────────────────────────────────────────────────────────┘
   Hospital side (all reached by us, never the other way):
     FHIR R4 server · SFTP folder with CSV exports
     on-site HL7 bridge (the hospital's engine pushes MLLP to it on the LAN)
     DocFlow (connector not built)
```

- Every arrow out of the host starts on the host. Nothing outside can open
  a connection to core, NATS, Postgres or the workers.
- NATS is the only link between the apps' servers, core and the workers
  (ADR 0010, ADR 0011). Each service has its own NATS user and its own
  database login.
- **Only core decides who may do what.** The other services act on core's
  decisions: a signed request token, or an event core published. Security
  rules are written once, in Go; Python never takes a hospital or user from
  a payload.

| Part | Where | Job |
|---|---|---|
| Core service | `services/core/` (Go) | The API, sign-in and sessions, two-step, permissions, hospital separation (RLS), the audit chain, job publishing, alert routing. Contract: `services/core/openapi.yaml`. |
| NATS | `services/nats/`, `deploy/nats/` | Request/reply for calls someone waits on; JetStream for jobs and dead letters; KV for the token denylist, sign-in counters and health. |
| Pipeline worker | `services/intelligence/` (Python) | Masking, retrieval, generation, the checks, the learning job. Reached only through NATS, each job with core's signed token. |
| Our own model | `deploy/gpu-model/` | llama.cpp `server-cuda` on the GPU, on an internal network shared with the pipeline only. |
| EMR service | `services/emr/` (Go) | Hospital record connections: connectors, outside credentials, normaliser, patient index, rules (ADR 0014). |
| Notifications | `services/notifications/` (Go) | Email for invites, reset links, security notices and reminders. Core decides who is told; it never reads patient records (ADR 0018). |
| Documents service | `services/documents/`, `services/docsandbox/` | Intake, encryption, scanning and page rendering for uploads and scanned records (ADR 0016). Built in part; not deployed. |
| PostgreSQL | | One database for all hospitals; row-level security keeps each hospital's rows apart. |
| Shared code | `services/gokit/` | Request tokens, replay set, `WithHospital`, log redaction, connector types. Used by core and emr. |

## 3. The live deployment

One host: an AWS g6.2xlarge (x86_64, one NVIDIA L4 GPU), running Docker
Compose. The backend is in `/srv/mhai-backend` (this repository's
`deploy/`), the two apps in `/srv/mhai-web` and `/srv/mhai-web-platform`
(mhai-web's `deploy/`). Run-books: `deploy/README.md` and
`deploy/NEW-SERVER.md`.

### 3.1 Services

| Service | Switch | What it is |
|---|---|---|
| `postgres`, `nats`, `core` | always | The database, the message bus and the core service. One-shots before core: `core-migrate`, `core-keys`, `core-request-key`. |
| `pipeline` | `PIPELINE_ENABLED=true` (profile `pilot`) | The answer worker, from the pilot pipeline image (weights baked in). |
| `gpu-model` | `GPU_MODEL_ENABLED=true` and a matching `GPU_MODEL_SHA256` (profile `gpu-model`) | Our own model on the L4. Which hospitals use it is each hospital's model policy. |
| `egress-proxy` | profile `cloud` | Squid, the cloud providers' only way out (`CONNECT` to port 443 of the enabled providers only). |
| `notifications`, `mail-relay` | `NOTIFY_ENABLED=true` (profile `notifications`) | Email. The relay forwards TCP to the one SMTP host; TLS is end to end. |
| `emr`, `egress-emr` | `EMR_ENABLED=true` (profile `emr`) | The EMR service and its own `CONNECT` proxy. Needs production mode first. |
| clinical app, platform admin app | mhai-web's deploy | Each app's Next.js server (the BFF) behind a small proxy container, behind the host's nginx (TLS, certbot; the platform app also behind an IP allow list). |

The deploy starts each optional service only when its switch is set in
`.env`, and stops it otherwise. Every container has a read-only root
filesystem, no Linux capabilities and `no-new-privileges`.

### 3.2 Networks

- **`backend`** (internal, no route out): core, Postgres, NATS, the
  pipeline, notifications, emr.
- **`mhai-bff`** (internal): the two BFFs and NATS only.
- **`gpu-model`** and `model` (internal): the pipeline and its model server
  only. Questions answered there never leave the host.
- **Egress-only paths**, one per service that must reach outside, each
  on its own network: `egress` (egress-proxy to cloud providers),
  `mail-egress` (mail-relay to SMTP), `egress-emr` (egress-emr to hospital
  systems). Each proxy refuses everything it was not told to allow.
- `edge`: only so core and NATS can publish their loopback ports.

### 3.3 Production mode (since 27 Nov)

The live backend runs in production mode
(`deploy/production/enable-production.sh`; design and threat model
`docs/deploy/production-tls-emr.md`; run-book deploy/README.md,
"Production mode"):

- **A private CA** (ECDSA P-256) issues every certificate; its key is kept
  off the host. Leaf certificates last a year; each deploy warns 30 days
  before expiry (`production/check-mode.sh`).
- **NATS over TLS** (TLS 1.2 minimum by config; our clients negotiate 1.3)
  with `verify`: every client shows its own client certificate (core, the
  pipeline, notifications, emr and both BFFs) **and** signs in with its
  NKey user.
- **Postgres TLS only**, `sslmode=verify-full` for every client;
  `pg_hba.production.conf` rejects anything without TLS.
- **Core runs `CORE_ENV=production`** (no API docs, production start-up
  checks, `CORE_ALLOW_SINGLE_NODE=true`: JetStream has one replica on one
  host).
- It is a compose overlay (`deploy/compose.production.yml`) named in
  `COMPOSE_FILE`, so every later deploy keeps it. `--rollback` takes it out.

### 3.4 Deploys

- **"Deploy backend demo"** (`.github/workflows/deploy.yml`, this
  repository) runs on every green `main`: it builds the multi-arch images,
  copies the deploy files to the host, checks the database before
  migrating, migrates, restarts core, checks health, then runs each
  optional service's switch (`gpu-model/gpu-model.sh`, `pilot/pipeline.sh`,
  notifications, `emr/emr.sh`). After any backend merge, watch it until it
  succeeds.
- **"Deploy demo"** (mhai-web) deploys the two apps. Deploy the backend
  first: the apps' deploy requires the backend's live-site marker, and adds
  the NATS TLS overrides when it finds the backend's production-mode marker.
- **The live-site guard** (`deploy/live-site.sh`, with `SITE_MODE=live` in
  both repositories). It runs before core starts and again after: it marks
  the host live (the demo scripts then refuse), removes the demo sample
  answers, and **stops core** if any hospital marked `is_demo` is still
  active. **Never mark a hospital `is_demo` on the live host**: on 25 Nov
  that took the live site down for about 30 hours.
- **"Pilot pipeline image"** (`.github/workflows/pilot-image.yml`) builds
  the pipeline worker's image with its pinned weights. It is started by
  hand from `main` and does nothing on the host.
- Other manual workflows: "Enable own-model answers", "Model promotion",
  "Model lock check", "Model test split".

## 4. The answer path

```
clinical app ─▶ BFF ─NATS api.v1.clinical─▶ core
   core: session and two-step, permission, RLS as the hospital, audit,
         POST /queries stores the question with core's model route,
         publishes jobs.v1.<hospital>.run_query with a signed request token
                                   │ JetStream
                                   ▼
pipeline worker: verify the token ─▶ claim the job (lease) ─▶ mask PII
   ─▶ retrieve chunks from the reference library (under RLS)
   ─▶ generate: own model (gpu-model) │ local model │ cloud provider
   ─▶ check: escalation guard rules ─▶ HGA support gate with NLI grounding
   ─▶ decision PASS / REGENERATE / ESCALATE, stored with the citations
                                   │
clinical app polls GET /queries/{id} and reads the result
   ─▶ agree, or suggest a correction ─▶ a second clinician reviews
   ─▶ approve_correction job (the learning job) ─▶ feedback_buffer
```

1. **Core** checks the user, the permission and the hospital, writes the
   question, and sets its **model route** from the hospital's model policy
   (a database trigger; the worker re-checks it when it claims the job).
   It signs a request token for this one job: service, subject, body hash,
   hospital, user, roles, deadline, nonce.
2. **The pipeline worker** checks the token before any work, claims the job
   with a lease (finished at most once; retries, dead letters and a
   10-minute deadline), and masks names and identifiers in the question and
   patient data.
3. **Retrieval** reads the shared reference library (`kb_chunks` with no
   hospital, readable by every hospital, writable by none). Two libraries
   are loaded by the live run-book: `medref` and `local-formulary`
   (deploy/README.md, "Live site: the shared reference library"). No patient record is used
   yet (section 5, EMR context in answers).
4. **Generation** follows the route:
   - `own`: our own model on the GPU, asked in its own prompt format. The
     first model (`gemma4-e4b-cca-v1`) **failed the `clinical_answer`
     gate**: it can test the path, not answer a real hospital, until a
     retrained model passes. Swapping models: `deploy/gpu-model/swap-model.sh`.
   - `local`: a pinned model on the CPU sidecar (profile `ownmodel`).
   - cloud (Anthropic, OpenAI, xAI, Google Gemini): **off by default per
     hospital**, demo and synthetic data only until the human gate in
     TODO.md is done. Only masked text leaves, through `egress-proxy`; a
     scan of the exact bytes refuses anything identifying (ADR 0013,
     `docs/specs/2026-11-03-cloud-model-providers-design.md`).
   - Any failure escalates. There is no fallback to another route.
   - Which model version runs is pinned in `models.lock` (name, version and
     the hash of its signed manifest); the worker verifies every pin at
     start-up and takes no jobs until all pass.
5. **The checks** run locally for every route (`boundary/models.py`,
   `_verify_with`):
   - **The escalation guard** (ADR 0017, rules in
     `guardrails/rules/escalation-guard.yaml`): red flags in the question or
     the patient's data, missing patient information, a dose outside the
     cited chunks, or an answer that only recites its chunks. It can only
     make a decision stricter. A guard that can't run escalates.
   - **The HGA support gate** (`agents/hga_nli.py`): no PASS unless every
     claim cites a chunk retrieved for this question that NLI entails and
     that holds the claim's numbers. **Since #297 NLI scores each claim over
     sentence windows of the cited chunk** (the whole chunk, each sentence,
     each pair of adjacent sentences): entailment is the best window;
     contradiction is the whole chunk's or the claim's own sentence's. A
     claim is entailed only with no contradiction, and a contradiction
     vetoes it. An unsupported claim caps the answer at REGENERATE; a
     contradiction or another patient's record escalates.
   - A trust score from the NLI, coherence, numbers and consistency scores
     is compared with the question type's thresholds.
6. **The decision** is PASS (shown as high confidence), REGENERATE (use
   with caution) or ESCALATE (needs review). It is stored with the masked
   question, the scores, the guard's reason codes and the cited sources.
7. **Feedback and corrections:** a clinician agrees or suggests a
   correction (`POST /feedback`); a different clinician approves it
   (two-person rule). The approval runs the `approve_correction` job, which
   fills the hospital's `feedback_buffer`. Nothing changes live guardrails,
   the reference library or any model: the buffer is a record for a later,
   change-controlled training run.

## 5. EMR and EHR integration

Two-way by design: reading records now, writing clinician-approved notes
back later. Design: `docs/specs/2026-11-07-emr-integration-design.md`,
ADR 0009 and ADR 0014. Run-book: deploy/README.md, "EMR integration".

```
hospital FHIR R4 server ◀──HTTPS── ┐
hospital SFTP (CSV export) ◀──SFTP─┤
on-site HL7 bridge ◀──────SFTP─────┤ egress-emr ◀── emr ──NATS──▶ core ──▶ alerts, lookup
  ▲ MLLP on the hospital LAN       │ (outbound only)   │
hospital interface engine          │                   └─▶ Postgres (emr schema, RLS)
DocFlow (not built) ◀──────HTTPS───┘
```

- **The emr service** is the only process that holds hospital
  credentials and the only way out to hospital systems. Core seals
  credentials to it and never holds them. It reaches outside only through
  `egress-emr`, which allows each connection's own hosts and never an
  internal address. **Our side is outbound only**: no inbound port.
- **Connectors built:**
  - `fhir_r4`: FHIR R4 with SMART Backend Services (a signed client
    assertion for the token).
  - `file_export`: CSV files (`mhai_csv_v1`) pulled over SFTP with a pinned
    host key.
  - `hl7v2_file` (#292, ADR 0019): HL7 v2 ORU^R01 (results, vital signs)
    and RDE^O11 (medication orders) files over SFTP. Off until listed in
    `CORE_EMR_CONNECTOR_TYPES`.
  - **The on-premises HL7 bridge** (#294, `deploy/hl7-bridge/`), shipped to
    the hospital: it takes MLLP pushes on the hospital LAN, sends the ACK
    only after the message is written to disk (fsync), and serves the files
    over its own key-only SFTP for our pull.
  - `document_archive` ("DocFlow"): declared, **not built**.
- **Normaliser, rules and alerts:** records map to one FHIR-based model,
  codes and units are normalised, rules find abnormal results, and core
  decides who sees each alert. **Patient lookup by MRN** is built
  (`POST /patients/lookup`).
- **On live: being switched on 27 Nov** with `deploy/emr/enable-emr.sh`
  (after production mode; `--disable` turns it off). The emr service runs as
  staging: the file secret store is the only one built. Until the real
  code tables arrive (H6) the normaliser and scheduler do not start:
  connections can be added and tested, but nothing is polled. Rule sets
  stay in shadow until clinical sign-off.
- **Designed and approved, not built:**
  - EMR context in answers: core freezes a small slice of the patient's
    record with the question; facts are context, never evidence for the
    support gate
    (`docs/specs/2026-11-27-emr-context-in-answers-design.md`).
  - Write-back of clinician-approved notes (FHIR `DocumentReference`, HL7
    MDM through the bridge), never automatic, with step-up approval
    (`docs/specs/2026-11-27-ehr-write-back-design.md`). Switching on is slow
    and two-person; **switching off is one person, at once**: any hospital
    admin for their hospital, or any platform admin for any hospital or all.
  - De-identified record access for clinicians: **D1 now** (clinicians with
    the analyst role use Analysis, off until its sign-offs), **D4 next**
    (aggregate questions under disclosure control). EMR context design §13.

## 6. Documents pipeline

The path for uploads and scanned records (DocFlow) into a patient's EMR
records. Design: `docs/designs/2026-11-14-documents-service.md`, ADR 0016.
**Paused by the product owner since 19 Nov; off by default and not
deployed.**

| Built | To do |
|---|---|
| Documents service intake (`services/documents/`) with per-hospital encryption (sealed objects, a keyring) | Image to text for typed records (a self-hosted model; never a cloud model) |
| Virus scanning (clamd, with a signature-age check) | Handwriting, with its own gate |
| `docsandbox`: page rendering (pdfium, Pillow) under nsjail | PDF to text from the embedded text layer |
| `doceval`: the extraction evaluation harness | Layout and table extraction engine (build or adopt: the owner decides) |
| `modelstore`: signed manifests, pin and verify (`models.lock`) | Review, ingest, finalise, retention, the DocFlow connector (documents PRs 6-10) |

The image, handwriting, PDF and layout items are TODO only: the owner said
not to build them yet. Uploads and letters (documents PR 5) are #230, open.

## 7. Platform admin features (on live)

Served by core on the platform audience (`/platform/*`), used by the
platform admin app:

- **Add a hospital** with its first admin, with step-up (a fresh two-step
  code).
- **Deactivate and reactivate** a hospital (#287), with step-up and a
  reason; deactivating ends its users' sessions.
- **Model policy** per hospital, with step-up and audit: cloud provider,
  budget, agreement reference, and `own_model` (our own model on the GPU).
- **Audit log:** the hash-chained log, read through a catalogue that maps
  each action to a kind the app shows (an action without a kind is sent
  bare). The integrity check walks the whole chain (by hand and daily at
  06:00 UTC). mhai-web shows six more kinds since its #129; mapping the
  withheld actions is #299 (open).
- **Analytics:** usage and answer quality over finished calendar weeks,
  months and quarters, frozen once and guarded against small groups; What
  changed, with follow-ups.
- **Health and failed jobs:** a health reading every 30 s; retry and cancel.
- **EMR connections:** add, test, switch on and off (each change with
  step-up), once the EMR is on.

Known gaps: **activating a new hospital is CLI only** (`core hospitals
activate`); platform team invites, notifications, platform settings and
named access answer **501**; audit exports stay off (`CORE_AUDIT_EXPORTS`)
until legal sign-off H3.

## 8. Accounts and security

- **Two-step for everyone.** Platform accounts and hospital admins always;
  clinicians too (every new hospital starts with two-step required).
- **One renewable backup code** per account. Each time it is used, core
  replaces it in the same transaction and the app shows the new code once,
  with Copy and a 3-2-1 countdown before Continue works.
- **Move to a new authenticator** (`/auth/mfa/replace`) with step-up.
- **Security notice emails** for a changed or reset password, two-step
  turned on or moved, and a backup code used or made.
- **Step-up** (a fresh code) on sensitive actions: adding, deactivating or
  removing a hospital, model policy, EMR credentials, cancelling jobs,
  removing people.
- **Messages:** with no sender set up, a route that must send something
  refuses before changing anything (**503 `no_delivery`**). If the change is
  saved but the sender can't take the message, the reply is **424
  `not_delivered`**: the invite or reset stands ("Saved, but the email didn't
  go through").
- Sign-in limits and the token denylist live in NATS KV and fail closed.
  Every write is audited on the hash chain in the same transaction.

## 9. Analysis

Population analysis for hospital admins and analysts, and across hospitals
for platform admins (ADR 0015, `docs/specs/2026-11-07-analysis-backend-design.md`).
Built: nightly ingestion of coded facts, frozen periods, five views, every
count guarded against small groups. **Off on live**: no live hospital is
switched on until the privacy, clinical and legal sign-offs H1 to H3; the
apps also need `ANALYSIS_SIGNED_OFF`.

## 10. How a call travels

### An ordinary API call

```
browser → BFF (Next.js) → NATS api.v1.clinical | api.v1.platform  (that app's BFF user only)
  → core (NATS adapter → the same http.Handler as HTTP)
      1. request id, body size limit; audience from the subject
      2. route lookup (unknown path → 404)
      3. access token checked (EdDSA, aud mhai-bff, iss mhai-gateway)
      4. the route's permission rule (deny by default)
      5. Idempotency-Key for writes
      6. a database transaction as the token's hospital (RLS)
      7. handler
      8. audit record on the hash chain, same transaction
  → reply to the BFF's own inbox; a problem response on any error
```

API versioning is ADR 0012.

### A call or job core sends to a worker

Core mints a request token for one call (`inference.v1.<service>.<op>`,
request/reply) or one job (`jobs.v1.<hospital>.<type>`, JetStream; only
core publishes). The worker checks signature, subject, body hash, reply
inbox, deadline, replay, schema, hospital, patient scope and role; any
failure is refused with no work done. Job audit facts go into an outbox
in the same transaction as the result, and core copies them onto the
chain, so only Go writes the audit chain.

A BFF may call a worker directly only for operations marked `bff_direct`
(none in production); the worker then asks core for a grant first
(`authz.v1.grant.<service>`). Design:
`docs/specs/2026-10-28-intelligence-boundary-design.md`.

## 11. Operations

| Area | Status |
|---|---|
| Limits | Built: sign-in limits, 10 questions in flight per hospital, bounded streams, per-worker concurrency. |
| Retries | Built: idempotency keys (72 h), job leases, retries with backoff, dead letters, a deadline sweep. |
| Encryption | On live: HTTPS at nginx; NATS and Postgres TLS (production mode); two-step secrets encrypted in the database. Not yet: JetStream encryption at rest, encrypted backups. |
| Backups | Built, opt-in: `deploy/ops/` (daily backup, weekly restore test, health alerts). Required before real patient data. |
| Monitoring | Built: health and readiness probes, JSON logs with redaction, tracing and metrics (off until an OTLP endpoint is set). Planned: dashboards and alerts. |
| Failover | Planned: a 3-node NATS cluster and a Postgres replica. Live is one host. |
| Split mode | Built, off: the pipeline on its own intelligence host (`docs/deploy/remote-intelligence.md`). |
| Environments | Live only. Planned: separate staging. |

Scaling: NATS spreads requests across copies of core and the workers with
queue groups; a new copy joins by itself. GPU-aware routing is deferred
(`docs/specs/2026-10-27-intelligence-performance-design.md`).

## 12. Where to read more

- Security rules: SKILLS.md. Conventions: CONTRIBUTING.md. Decisions:
  `docs/adr/`. Open work: TODO.md.
- Core: `services/core/README.md`. Workers and model promotion:
  `services/intelligence/README.md`. EMR: `services/emr/README.md`.
  Documents: `services/documents/README.md`. Email:
  `services/notifications/README.md`.
- Deploying: `deploy/README.md`; production mode and the EMR switch:
  `docs/deploy/production-tls-emr.md`; HL7 bridge install guide:
  `deploy/hl7-bridge/README.md`.
