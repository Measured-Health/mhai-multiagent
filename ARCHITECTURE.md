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

The sections below describe the Python pipeline service.

## System Flow

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

## 4-Layer Guardrail Cascade

Each response must pass ALL layers to auto-approve. Any single failure triggers review.

## Security Pipeline Order

RBAC → PII Mask → Route → Audit → Encrypt
