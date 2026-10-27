# MHAI Architecture

## Services

The backend has two parts: the core service (Go) and the Python model
services, joined by NATS. This is the planned design (ADR 0010,
`docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`).

**Today:** only the Python code exists. **Planned:** the core service, in
`services/core/`, and the NATS link (config in `services/nats/`). No Go code has been written yet.

| Service | Job |
|---|---|
| **Core service** (Go, planned) | The API, sign-in and sessions, permissions, tenancy, audit, and the EMR connectors. The only service the frontends call. |
| **NATS** (planned) | The only link between Go and Python. Request/reply for calls someone waits on; JetStream for background jobs. Replaces the Redis queue stub. |
| **Python services** (today, `services/intelligence/`) | The models: agents, retrieval and the knowledge base, guardrails, PII masking, inference. Reached only through NATS. Each request carries a token signed by the core service, and the service checks it before doing any work. |
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
