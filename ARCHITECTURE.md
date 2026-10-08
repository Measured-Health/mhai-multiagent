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

> **Status: design in progress, not yet implemented.** Phase 1 API gateway
> (#8). Approved: scope, JWT login, optional `patient_id`, Redis worker,
> API contract, auth and identity, data and job lifecycle.

### 1. Components

```mermaid
flowchart LR
    web["mhai-web<br/>(Next.js)"] -- "HTTPS + JWT" --> api

    subgraph stack["Compose stack"]
        api["api<br/>FastAPI :8000<br/>auth · validation · routing"]
        worker["worker<br/>models + KB loaded once<br/>SecurePipelineWrapper"]
        pg[("Postgres<br/>source of truth")]
        redis[("Redis<br/>job queue · rate limits<br/>token denylist")]
        ollama["Ollama<br/>CCA LLM"]
        migrate["migrate<br/>(one-shot)<br/>alembic upgrade head"]
    end

    api -- "read/write status,<br/>results, users, audit" --> pg
    api -- "enqueue job id" --> redis
    redis -- "claim job" --> worker
    worker -- "stage, progress,<br/>results, corrections, audit" --> pg
    worker -- "generate" --> ollama
    migrate -. "schema" .-> pg
```

The API never loads models or calls the orchestrator. Only the worker calls
`SecurePipelineWrapper`. **There is exactly one worker in Phase 1**,
because learning state stays in the worker's memory until #9.

### 2. Query lifecycle

```mermaid
sequenceDiagram
    autonumber
    actor C as Clinician (mhai-web)
    participant A as api
    participant P as Postgres
    participant R as Redis
    participant W as worker

    C->>A: POST /api/v1/queries (JWT, form, patient_id?)
    A->>A: verify JWT, reload user, check query_submit
    A->>P: insert query (status=queued)
    A->>R: enqueue run_query(query_id, user_id, role)
    A-->>C: 202 {query_id, status: queued}

    W->>R: claim job
    W->>P: status=processing
    W->>W: SecurePipelineWrapper.submit_query(...)<br/>on_stage(stage, progress) per step
    W->>P: update current_stage / progress (each stage)
    W->>P: insert decision + citations, status=completed
    W->>R: ack

    loop every 2–3 s
        C->>A: GET /api/v1/queries/{id}
        A->>P: read status (owner or admin only)
        A-->>C: {status, progress, current_stage}
    end
    C->>A: GET /api/v1/queries/{id}/result
    A-->>C: ClinicalResultOut (409 until completed)
```

### 3. Pipeline stages (progress)

```mermaid
flowchart LR
    q([queued]) --> s1["pii_masking<br/>SecurePipelineWrapper"]
    s1 --> s2["retrieval<br/>patient-scoped RAG"]
    s2 --> s3["generation<br/>CCA via Ollama"]
    s3 --> s4["nli_verification<br/>per-citation NLI"]
    s4 --> s5["hga_verification<br/>coherence · numbers · SE"]
    s5 --> s6["decision<br/>PASS / REGENERATE / ESCALATE"]
    s6 --> done([completed])
    s1 & s2 & s3 & s4 & s5 -. "unrecoverable error /<br/>retries exhausted" .-> failed([failed])
```

Each arrow is an `on_stage(stage, progress)` callback that the worker writes
to Postgres. The frontend's progress stepper reads it.

### 4. Failure handling (fail closed)

```mermaid
flowchart TD
    start{{"agent result"}} --> cca{"CCA produced<br/>a response?"}
    cca -- no --> esc["ESCALATE · degraded=true<br/>failed_agents=[CCA]<br/>response_available=false"]
    cca -- yes --> hga{"HGA verified<br/>it?"}
    hga -- no --> esc2["ESCALATE · degraded=true<br/>failed_agents=[HGA]<br/>trust_score=null"]
    hga -- yes --> known{"decision is<br/>pass/regenerate/escalate?"}
    known -- no --> esc3["ESCALATE"]
    known -- yes --> out["decision as scored<br/>verified=true"]

    job{{"worker job"}} --> retry{"attempt < N?"}
    retry -- yes --> job
    retry -- no --> dlq["dead-letter queue<br/>query status=failed"]
```

### 4b. Job and query states

```mermaid
stateDiagram-v2
    [*] --> queued: POST /queries<br/>(row in Postgres, job id in Redis)
    queued --> processing: worker claims job<br/>(moved to processing list)
    note left of processing
        writes current_stage and
        progress at each stage
    end note
    processing --> completed: decision + status written<br/>in one transaction
    processing --> queued: worker crashed or error,<br/>attempt < 3 (re-queued)
    processing --> failed: 3 attempts used or<br/>300 s time limit
    failed --> [*]: job in dead-letter list,<br/>short error for the API
    completed --> [*]: result readable,<br/>later retries skipped
```

Redis holds only IDs (`{job_id, type, query_id or correction_id, user_id,
role, attempt}`), never patient data. The worker writes a heartbeat every
10 s, and `/readyz` reports it down after 30 s. Nothing is deleted
automatically in Phase 1; a retention policy is a follow-up decision.

### 5. Login and request authentication

```mermaid
sequenceDiagram
    autonumber
    actor C as Clinician
    participant A as api
    participant R as Redis
    participant P as Postgres

    C->>A: POST /api/v1/auth/login {username, password}
    A->>R: check failed-attempt counter (5 per 15 min)
    A->>P: load user, verify argon2id hash, is_active
    A->>P: audit LOGIN (success/failure)
    A-->>C: {access_token (15 min), user: {username, role}}

    C->>A: any request + Bearer token
    A->>A: verify signature, exp
    A->>R: token id not on denylist?
    A->>P: reload user (deactivation applies at once)
    A->>A: route permission check (same matrix as RBACManager)
    Note over A: identity comes from the token only<br/>(no user or clinician ids accepted in bodies)
```

### 6. Feedback and two-person correction approval

```mermaid
sequenceDiagram
    autonumber
    actor A1 as Clinician A
    actor B1 as Clinician B
    participant API as api
    participant P as Postgres
    participant W as worker

    A1->>API: POST /feedback {query_id, action: correct,<br/>corrected_diagnosis, reason}
    API->>P: store feedback, correction status=pending_review
    API->>W: enqueue submit_feedback (via Redis)
    API-->>A1: {status: pending_review, correction_id}
    Note over W: nothing changes in KB, persona vector<br/>or trust weights yet

    B1->>API: GET /corrections?status=pending
    API-->>B1: corrections by others (never your own)
    B1->>API: POST /corrections/{id}/approve
    API->>API: reviewer ≠ submitter, else 403
    API->>W: enqueue approve_correction (via Redis)
    W->>W: apply: PII-mask, patient-scoped KB entry,<br/>persona vector, trust weights
    W->>P: correction status=approved, audit APPROVE
```

### 7. Data ownership

```mermaid
flowchart LR
    subgraph PG["Postgres (durable)"]
        users[(users)]
        cq[(clinical_queries<br/>status · progress · stage)]
        cd[(clinical_decisions<br/>result · citations)]
        fb[(feedback_events<br/>corrections)]
        audit[(audit_log<br/>hash chain)]
    end
    subgraph RD["Redis (ephemeral)"]
        jobs[[job queue + DLQ]]
        rl[[login rate limits]]
        deny[[token denylist]]
    end
    subgraph WM["worker memory (until #9)"]
        kb[[KB + retrievers]]
        learn[[persona vector ·<br/>trust weights]]
    end
    cq --> cd --> fb
```

### Deployment changes

| Before | After |
|---|---|
| `mhai` service runs the CLI and exits | `api` serves on :8000 with a real health check, plus a `worker` service |
| schema from `init-db/init.sql` | Alembic migrations, run by a one-shot `migrate` service |
| placeholder `admin` with a fake hash | `mhai users create` (real hashes); `mhai seed-demo` for dev-only synthetic clinicians |
