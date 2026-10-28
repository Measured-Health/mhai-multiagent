# MHAI Architecture

## Services

The backend has two parts: the core service (Go) and the Python model
services, joined by NATS (ADR 0010,
`docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`).

**Built (Plan 2):** the core service in `services/core/`: sign-in and
sessions, two-step verification, password change, the permission table
and principal middleware, hospital separation by row-level security, the
audit chain, idempotency, and the NATS connection with its KV buckets
(config in `services/nats/`). Its contract is `services/core/openapi.yaml`,
and it runs in Docker Compose (`make core-up`). **Next:** the Python side
behind NATS (the boundary plan), then the hospital and platform endpoints.

| Service | Job |
|---|---|
| **Core service** (Go, `services/core/`) | The API, sign-in and sessions, permissions, tenancy, audit, and the EMR connectors. The only service the frontends call. |
| **NATS** (`services/nats/`) | The only link between Go and Python. Request/reply for calls someone waits on; JetStream for background jobs; KV for the core service's denylist and attempt counters. Replaces the Redis queue stub once the Python side moves over. |
| **Python services** (`services/intelligence/`) | The models: agents, retrieval and the knowledge base, guardrails, PII masking, inference. Reached only through NATS. Each request carries a token signed by the core service, and the service checks it before doing any work. |
| **PostgreSQL** | One database for all hospitals. Row-level security (RLS) keeps each hospital's rows apart. |
| **Object storage** (planned) | Documents, letters and exports. |

```
browser → BFF (Next.js) → core service → NATS → Python services
                              │
                              ├→ PostgreSQL (RLS per hospital)
                              └→ object storage
```

Security rules are written once, in Go. Python never takes a hospital or
user from a payload; it takes them only from the verified token.

## Services and systems flow

### An ordinary API call

Every request from a browser goes through the BFF, which adds its service
key. The core service then runs the same steps for every route:

```
browser
  → BFF (Next.js, clinical or platform app)        adds X-MHAI-Service key
  → core service
      1. request id, body size limit, BFF key check  (unknown caller → 401)
      2. route lookup (unknown or unclean path → 404)
      3. principal: access token checked (EdDSA, aud mhai-bff, iss mhai-gateway)
      4. permission rule for the route (deny by default)
      5. Idempotency-Key for writes
      6. database transaction as the hospital in the token
         (row-level security: mhai_current_hospital())
      7. handler
      8. audit record on the hash chain, in the same transaction
  → problem response on any error (no detail that helps an attacker)
```

Sign-in, refresh and two-step verification take the same path. Attempt
counters and the token denylist live in NATS KV, and they fail closed: if
NATS is down, sign-in is refused, not waved through.

### A call to the intelligence service

Approved design, being built (Plan 3,
`docs/specs/2026-10-28-intelligence-boundary-design.md`):

```
core service
  │ checks the user and their permission (steps 1–6 above)
  │ mints a request token for this one call: service, NATS subject,
  │ body hash, hospital, user, roles, patients, deadline, nonce
  ▼
NATS  inference.v1.<service>.<op>        (request/reply, someone is waiting)
      jobs.v1.<hospital>.<type>          (JetStream, long jobs)
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

Long jobs are claimed with a lease and finished at most once. Their audit
facts go into an outbox table in the same transaction as the result, and
the core service copies them onto the hash chain, so only Go writes the
audit chain.

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

RBAC → PII Mask → Route → Audit → Encrypt
