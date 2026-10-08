# MHAI Architecture

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

## Service Architecture (planned, #8)

> **Status: design approved, not yet implemented.** This describes the
> Phase 1 API gateway being designed in #8. The detailed spec will live in
> `docs/superpowers/specs/`. Update this section as parts land.

Decisions so far:

- **Scope:** the Phase 1 slice: login, query submit/status/result, feedback
  and correction approval, `/healthz` and `/readyz`. Admin, document
  ingest, triage, KB search and audit endpoints come in a follow-up project.
- **Auth:** real JWT login with an argon2-hashed `users` table.
- **Patient:** an optional `patient_id` per query. Without one, the
  consultation is its own case, and retrieval uses shared guidelines plus
  the submitted form.
- **Processing:** the API enqueues jobs on Redis, and a separate worker
  runs them.

### Processes

Two new processes, built from the same image, each with one job:

```
mhai-web ──HTTPS/JWT──▶ api (FastAPI, :8000) ──▶ Postgres (source of truth)
                           │ enqueue job id            ▲
                           ▼                           │ status, results,
                         Redis ──────▶ worker ─────────┘ corrections, audit
                                   (models + KB loaded once;
                                    SecurePipelineWrapper → orchestrator
                                    → CCA via Ollama → HGA)
```

- **api** (`mhai/api/`):
  - `app.py`: app factory, startup checks and error handlers;
  - `schemas.py`: Pydantic v2 request and response models; this is the
    contract that `openapi.json` and mhai-web's types come from;
  - `auth.py`: argon2 password hashing, JWT issue and verify, and the
    `current_user` dependency (`{user_id, role}`);
  - `routes/`: `auth`, `queries`, `feedback`, `health`;
  - `db.py` and `models.py`: SQLAlchemy 2.0 over Postgres;
  - `queue.py`: Redis jobs (`enqueue`, `claim`, `ack`, dead-letter after N
    attempts).
- **worker** (`mhai/worker/`): `main.py` loads the models and KB once and
  loops over jobs (`run_query`, `submit_feedback`, `approve_correction`);
  `progress.py` records each pipeline stage and its progress.

### Boundaries

- The API never imports the models or the orchestrator. It only reads and
  writes Postgres and enqueues jobs.
- Only the worker calls `SecurePipelineWrapper`; nothing calls the
  orchestrator directly.
- Postgres is the source of truth for what users see and keep: query
  status, results, pending corrections and the audit trail.
- **Exactly one worker in Phase 1.** Learning state (pending corrections,
  persona vector, trust weights, KB) lives in the worker's memory. Feedback
  and approvals therefore run in that same worker until #9 moves the state
  to Postgres; after that, workers can scale.

### Deployment (Compose)

- `mhai` becomes `api`. It serves on :8000, with a real health check.
- A sibling `worker` service runs the jobs.
- A one-shot `migrate` service runs `alembic upgrade head` before either
  starts. Alembic migrations replace `init-db/init.sql`.
- The placeholder admin seed is removed. Accounts are created with
  `mhai users create`, with real hashes; `mhai seed-demo` creates synthetic
  test clinicians for development only.
