# API gateway (#8): design

- **Status:** Approved design, pending implementation plan
- **Date:** 2026-10-08
- **Issue:** #8
- **Related:** ADR 0003 (Ollama), 0004 (patient-scoped retrieval), 0005
  (fail closed, reviewed corrections), 0006 (Postgres KB, proposed), 0007
  (API in Python, contract-first), 0008 (models behind inference services);
  `SKILLS.md`
- **Diagrams:** `ARCHITECTURE.md` → *Service Architecture*

## 1. Goal and scope

### Goal

Give `mhai-web` an HTTP API so clinicians can run the clinical workflow
end to end: sign in, submit a patient assessment, follow its progress, read
an evidence-backed result, and agree with it or suggest a correction.
Admins and auditors manage users, watch system health and inspect the
audit trail.

### Success criteria

1. `mhai-web` Phase 1 (the build guide's 7-field form → processing →
   result → feedback) can be built entirely against this API and the
   committed `openapi.json`.
2. Every rule in `SKILLS.md` holds at the HTTP boundary:
   - identity comes only from the session;
   - queries are patient-scoped;
   - failures escalate;
   - corrections need a second clinician;
   - no PHI in logs.
3. CI is green with the new tests, including the security and contract
   tests in §14. The real-model end-to-end check passes against the
   Compose stack.

### In scope

- **Auth:** local login, JWT, TOTP MFA and generic OIDC single sign-on.
- **Clinical:**
  - queries (submit, status, result);
  - feedback and two-person correction approval;
  - document ingest (OCR only);
  - triage (gated off);
  - KB search.
- **Admin and audit:**
  - users;
  - guardrail stats;
  - agent status;
  - failed-job inspector;
  - feedback-buffer reset;
  - read-only audit viewer with hash-chain verification.
- **Operations:** health and readiness, retention policy and purge, and
  Compose services (`api`, `worker`, `migrate`, `inference`, MinIO,
  Keycloak).

### Out of scope (follow-ups)

- Per-patient erasure requests.
- Antivirus scanning of uploads (a known risk).
- Serving original documents back to clients.
- Machine API tokens.
- Researcher and anonymised-export endpoints.
- Persisting the KB in Postgres (#9, ADR 0006).
- Trained layout (Swin-V2) and triage (QDA) models.

## 2. Decisions

| Topic | Decision |
|---|---|
| Language | Python/FastAPI, one package with the worker; contract-first so the API can be replaced (ADR 0007) |
| Processing | API enqueues; **one** worker runs jobs (learning state in memory until #9) |
| Inference | Every model sits behind a client interface and is served by an inference service: Ollama for the LLM, `inference` for embeddings and NLI; OCR is in-process behind its interface for now. The API and worker import no ML frameworks (ADR 0008) |
| Source of truth | Postgres (Alembic migrations); Redis only for delivery, rate limits, short-lived auth state, heartbeats |
| Auth | Local argon2id passwords + JWT (15 min, sliding refresh); TOTP MFA required for admin/auditor; generic OIDC SSO with pre-linked accounts |
| Patient | `patient_id` optional on queries (no patient means a standalone case: guidelines plus the submitted form); required on documents |
| Admin separation | Admin and auditor accounts **cannot** call clinical endpoints; staff who need both hold two accounts |
| Documents | S3-compatible storage (MinIO in Compose), metadata in Postgres |
| Ingest | Honest OCR-only: Tesseract per whole page; fake layout boxes removed |
| Triage | Built but gated off; 503 until validated weights are configured |
| Retention | Per-type, configurable, conservative defaults (unconfirmed placeholders), daily purge, legal hold, audit checkpoints |
| Decision values | Uppercase in the API (`PASS`/`REGENERATE`/`ESCALATE`); lowercase internally |

## 3. Architecture

Components, lifecycle, stages, failure handling, login, approval, job
states and data ownership are drawn in `ARCHITECTURE.md`. Summary:

- **`api`** (`mhai/api/`): FastAPI app factory; `schemas.py` (the contract);
  `auth/` (passwords, JWT, MFA, OIDC); `routes/` (`auth`, `queries`,
  `feedback`, `corrections`, `documents`, `triage`, `kb`, `admin`,
  `security`, `health`); `db.py`/`models.py` (SQLAlchemy 2.0); `jobs.py`
  (enqueue and job rows); `errors.py` (error codes and mapping);
  `logging.py` (request context).
- **`worker`** (`mhai/worker/`): `main.py` builds the KB and the
  `SecurePipelineWrapper` once (wired to inference clients, not in-process
  models), then runs the job loop. There is one handler
  module per job type, plus `progress.py`, `retention.py` and
  `heartbeat.py`.
- **Shared** (`mhai/platform/`): `settings.py` (pydantic-settings),
  `blobstore.py`, `audit.py` (Postgres-backed `AuditLogger`), `errors.py`
  (error classifier), `jobs.py` (payload schema).
- **Inference clients** (`mhai/inference/`): the `Embedder`,
  `NLIClassifier`, `Generator` and `OCR` protocols and their HTTP clients
  (§8a). The only place model calls are made.
- **`inference` service** (`mhai/inference_server/`, separate image): loads
  the embedding and NLI models and serves them over versioned HTTP.

**Boundaries:**

- The API never imports `mhai.agents`, `mhai.knowledge_base`,
  `mhai.guardrails` or `mhai.models`. A test enforces this (§14).
- **Neither the API nor the worker imports `torch`, `transformers` or
  `sentence_transformers`.** Only the `inference` service does, and a test
  enforces this as well. Swapping the inference engine (vLLM, TEI, Triton,
  GPU or CPU) or rewriting the API or worker in another language doesn't
  touch the other side.
- Only the worker calls `SecurePipelineWrapper`, and nothing calls the
  orchestrator directly.
- Redis messages carry ids only, never patient data or text.

## 4. API contract

### Conventions

- Base path `/api/v1`; JSON; Pydantic v2 models with `extra="forbid"`.
- Request bodies are capped at 64 KB, except document upload (20 MB).
- **Errors** are `application/problem+json`:
  `{type, title, status, code, detail, request_id}`.
  - `code` comes from the fixed catalogue (§4.3).
  - `detail` is a fixed message per code, never exception text.
  - Validation errors list `{field, problem}` and **never echo input values**.
- Every response carries an `X-Request-ID` header. Responses that may carry
  PHI send `Cache-Control: no-store`.
- Resources that belong to someone else return **404**, not 403, so ids
  can't be probed.
- `/docs` and the live `/openapi.json` are served only when
  `APP_ENV=local`. The committed `openapi.json` at the repository root is
  the contract.

### 4.1 Endpoints

The *Role* column uses the RBAC matrix in §5.4. "Clin." means clinician.

| Method & path | Role | Behaviour |
|---|---|---|
| `POST /auth/login` | public | `{username, password}`: `{access_token, expires_in, user}` or `{mfa_required: true, mfa_token}` |
| `POST /auth/mfa/challenge` | public (with `mfa_token`) | `{mfa_token, code}`: JWT; a recovery code also works once |
| `POST /auth/mfa/enroll` | signed in | `{otpauth_uri, recovery_codes[10]}` (shown once) |
| `POST /auth/mfa/verify` | signed in | `{code}`: enrolment active |
| `POST /auth/refresh` | signed in | new token; the old token id is denylisted |
| `POST /auth/logout` | signed in | token id denylisted until expiry |
| `POST /auth/password` | signed in | change password (required on first login) |
| `GET /auth/oidc/login` | public | redirect to the IdP (state, nonce, PKCE in Redis, 10 min) |
| `GET /auth/oidc/callback` | IdP redirect | validate ID token; redirect to `mhai-web` with a one-time code (60 s) |
| `POST /auth/oidc/exchange` | public | `{code}`: JWT |
| `POST /queries` | clin. | `QueryCreateIn`: **202** `{query_id, status: "queued"}` |
| `GET /queries/{id}` | clin. (owner) | `QueryStatusOut` |
| `GET /queries/{id}/result` | clin. (owner) | `ClinicalResultOut`; **409** `result_not_ready` |
| `POST /feedback` | clin. (query owner) | `{query_id, action: validate\|correct, corrected_diagnosis?, reason?}`: `{status: recorded\|pending_review, correction_id?}` |
| `GET /corrections?status=pending` | clin. | others' pending corrections (never your own) |
| `POST /corrections/{id}/approve` | clin. | **403** `self_approval` if you submitted it |
| `POST /documents` | clin. | multipart `{patient_id, file}`: **202** `{document_id, status}` |
| `GET /documents/{id}` | clin. (uploader) | metadata and status |
| `GET /documents?patient_id=` | clin. | that patient's documents uploaded by you |
| `POST /triage` | clin. | **503** `triage_unavailable` while gated; otherwise **202** + job |
| `GET /triage/{id}` | clin. (owner) | status and result (shape fixed now, used when enabled) |
| `GET /kb/search?q=&patient_id=&top_k=` | clin. | ≤10 chunks: patient's own plus guidelines; **503** `kb_unavailable` after 5 s |
| `GET /admin/users` · `POST /admin/users` · `PATCH /admin/users/{id}` | admin (MFA) | list; create with temporary password; role, deactivate, link or unlink SSO, reset MFA; no self-demotion or self-deactivation |
| `GET /hga/stats?from=&to=` | admin (MFA) | decision mix, degraded rate and causes, trust distribution, agree/correct rates, counts only |
| `GET /agents/status` | admin (MFA) | worker heartbeat, queue depth, DLQ size, p50/p95 per stage, served model names and versions (from the inference services), triage gate state |
| `GET /admin/dlq` · `POST /admin/dlq/{job_id}/retry` · `POST /admin/jobs/cancel` | admin (MFA) | ids, type, attempts and error class only; retry takes `Idempotency-Key`; bulk cancel by ids |
| `POST /admin/feedback-buffer/reset` | admin (MFA) | `{confirm: "RESET"}`; clears the unapproved trust-weight buffer only |
| `PUT /admin/legal-holds` · `DELETE /admin/legal-holds/{id}` | admin (MFA) | hold or release a query, document or patient |
| `GET /admin/settings` · `PATCH /admin/settings` | admin (MFA) | runtime settings (retention periods, triage gate); secrets never shown |
| `GET /security/audit` | auditor, admin (MFA) | paginated; filter by actor, action, date range |
| `GET /security/audit/verify` | auditor, admin (MFA) | `{valid, entries_checked, from_checkpoint, first_broken_id?}` |
| `GET /healthz` | public | `{status: "ok"}` |
| `GET /readyz` | public | `{db, redis, worker, nli_model_loaded, ollama, blobstore}`; **503** if any is down |

### 4.2 Key schemas

**`QueryCreateIn`**

- `patient_data`: `{age: int 0–120, sex: female|male|other|unknown,
  symptoms: list[str] (1–20, each ≤200 chars), medical_history: str? (≤2000)}`
- `clinical_question: str (10–2000)`
- `urgency: routine|urgent|emergency`
- `specialty: str?`
- `patient_id: str?`: must be a valid, non-reserved id
  (`is_valid_patient_id`) of a known patient; otherwise **422**
  `invalid_patient`.

**`QueryStatusOut`**

`{query_id, status: queued|processing|completed|failed, progress: 0–1,
current_stage: pii_masking|retrieval|generation|nli_verification|hga_verification|decision|null,
error_code?: str}`

**`ClinicalResultOut`**

- `{query_id, decision: PASS|REGENERATE|ESCALATE, verified: bool,
  degraded: bool, failed_agents: [CCA|HGA], response_available: bool,
  diagnosis: str?, icd10_code: str?, response_text: str,
  trust_score: float? (null when degraded), citations: [Citation],
  guardrail_details: GuardrailDetails, retrieval_ms, generation_ms}`
- `Citation`: `{chunk_id, source_title, excerpt (≤300 chars), claim_text,
  nli_label: ENTAILMENT|NEUTRAL|CONTRADICTION, nli_confidence,
  action: PASS|FLAG|REDACT, reason}`
- `GuardrailDetails`: `{l1_persona: null, l2_entropy: null,
  l3_nli: {pass_rate, claims_checked, model_loaded},
  l4_trust: {trust_score, base_trust, flag_penalty, weights},
  cascade_pass}`. L1 and L2 stay `null` until the pipeline computes them;
  no sample values are invented.
- `diagnosis` and `icd10_code` are parsed from the response text (a
  "Diagnosis:" line, or an ICD-10 pattern); `null` when not found.

**`FeedbackIn`**

`action=correct` requires `corrected_diagnosis` (3–500 chars) and
`reason` (3–1000 chars). No user-id or clinician-id fields exist; any
extra field is a **422**.

### 4.3 Error codes

The catalogue, with HTTP statuses:

- `unauthenticated` 401
- `token_expired` 401
- `mfa_required` 401
- `mfa_invalid` 401
- `forbidden` 403
- `self_approval` 403
- `not_found` 404
- `result_not_ready` 409
- `invalid_state` 409
- `validation_failed` 422
- `invalid_patient` 422
- `unsupported_file` 422
- `file_too_large` 413
- `rate_limited` 429
- `login_locked` 429
- `triage_unavailable` 503
- `kb_unavailable` 503
- `not_ready` 503
- `internal_error` 500

Domain exceptions map to codes in **one place** (`mhai/api/errors.py`).

## 5. Authentication and authorisation

### 5.1 Local login and tokens

- **Passwords:** argon2id (`argon2-cffi`). Accounts are created only by
  `mhai users create` (prompts; never takes the password as an argument)
  or `POST /admin/users`. Temporary passwords force a change on first
  login.
- **Login protection:**
  - one generic error for a wrong username or password;
  - 5 failures per (username, IP) in 15 minutes locks the account
    (Redis counter, `login_locked`);
  - every attempt is audited.
- **JWT:**
  - signed HS256 with `JWT_SECRET`; startup **fails** if it's missing,
    under 32 bytes or a known placeholder;
  - claims: `sub` (user id), `role`, `iat`, `exp` (15 min), `jti`,
    `mfa` (bool), `amr`.
- **On each request:**
  - verify the signature and expiry;
  - check the `jti` isn't denylisted (Redis);
  - **reload the user** (inactive → 401);
  - check the role and MFA requirement for the route.

### 5.2 MFA (TOTP)

- **Library:** `pyotp`, 30 s step, one step of tolerance either side. A code
  can't be reused (last used step stored).
- **Secrets** are encrypted with `FERNET_KEY`. Recovery codes are stored
  hashed and work once.
- **Enforcement:** admin and auditor must enrol before any other action
  (every other route returns `mfa_required`). Enrolment is optional for
  clinicians.
- **Limits:** 5 attempts per `mfa_token` (valid 5 min).
- **Reset:** an admin can reset another user's MFA only from an MFA
  session; the reset is audited.

### 5.3 Single sign-on (generic OIDC)

- **Configuration:** `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`,
  `OIDC_SCOPES`, `OIDC_REDIRECT_URI`, `WEB_LOGIN_CALLBACK_URL`. Keycloak
  runs under the Compose `sso` profile for development.
- **Flow:** authorization code with PKCE. The ID token is validated against
  the issuer's JWKS (signature, `iss`, `aud`, `nonce`, `exp`).
- **Accounts:** the SSO identity must already be linked (`oidc_issuer`,
  `oidc_subject` on `users`); there is no just-in-time provisioning. Roles
  come only from MHAI.
- **MFA:** admin and auditor accounts require the IdP to assert MFA
  (`amr` containing `mfa`, `otp` or `hwk`, or `acr` in
  `OIDC_MFA_ACR_VALUES`); otherwise the login is rejected.
- **Token delivery:** the browser receives our JWT only via the one-time
  code exchange, never in a URL.

### 5.4 RBAC matrix (API layer)

| Capability | clinician | researcher | auditor | admin |
|---|---|---|---|---|
| Queries, feedback, approve, documents, triage, KB search | ✓ | | | |
| Users, stats, agent status, DLQ, feedback reset, legal holds, settings | | | | ✓ (MFA) |
| Audit viewer and verify | | | ✓ (MFA) | ✓ (MFA) |

- Researcher has no endpoints in this project.
- `RBACManager.PERMISSIONS` changes to match: ADMIN loses
  `query_submit`, `view_patient_data` and `submit_feedback`.
- The API checks the matrix at the route. The worker then re-checks
  through `SecurePipelineWrapper`, using an ephemeral `RBACManager`
  session for the job's `{user_id, role}`.

## 6. Data model (Alembic)

Migration `0001` creates the schema from scratch. `init-db/init.sql` and its
placeholder admin are removed.

| Table | Purpose and key columns |
|---|---|
| `users` | `id`, `username` (unique), `password_hash`, `role`, `is_active`, `must_change_password`, `oidc_issuer`, `oidc_subject` (unique pair), `created_at`, `last_login` |
| `user_mfa` | `user_id`, `secret_encrypted`, `enabled`, `last_used_step`, `recovery_code_hashes[]` |
| `clinical_queries` | `id`, `created_by` → users, `patient_id?`, `patient_data` (JSONB), `clinical_question`, `urgency`, `specialty?`, `category?`, `status`, `progress`, `current_stage`, `error_code?`, timestamps |
| `clinical_decisions` | `id`, `query_id` (unique), `decision`, `verified`, `degraded`, `failed_agents[]`, `response_available`, `diagnosis?`, `icd10_code?`, `response_text`, `trust_score?`, `citations` (JSONB), `guardrail_details` (JSONB), `retrieval_ms`, `generation_ms` |
| `feedback_events` | `id`, `query_id`, `user_id`, `action`, `created_at` |
| `corrections` | `id`, `query_id`, `submitted_by`, `corrected_diagnosis`, `reason`, `status` (`pending_review`/`approved`), `approved_by?`, `approved_at?` |
| `documents` | `id`, `patient_id`, `uploaded_by`, `sha256`, `size_bytes`, `page_count?`, `blob_key`, `status`, `error_code?`, timestamps |
| `document_pages` | `document_id`, `page`, `ocr_text`, `ocr_engine_version`; this is the OCR cache, keyed by document hash, page and OCR version |
| `kb_chunks` | persisted chunks (`chunk_id`, `patient_id`, `chunk_type`, `content`, `metadata`); the worker rebuilds its in-memory KB from these at start |
| `triage_requests` | `id`, `created_by`, `features` (JSONB), `status`, `result` (JSONB)? |
| `jobs` | `id`, `type`, `subject_id`, `status` (`queued`/`running`/`completed`/`failed`/`cancelled`), `attempts`, `error_class?`, `error_code?`, `run_id?`, `worker_id?`, `request_id`, `not_before`, timestamps |
| `audit_log` | append-only hash chain (`actor`, `action`, `entity`, `entity_id`, `payload` without PHI, `prev_hash`, `hash`, `ts`) |
| `audit_checkpoints` | `last_purged_id`, `last_purged_hash`, `purged_count`, `ts` |
| `legal_holds` | `id`, `subject_type` (query/document/patient), `subject_id`, `created_by`, `reason`, `created_at`, `released_at?` |
| `settings` | runtime settings (retention periods, triage gate) with `updated_by` and `updated_at`; changes are audited |
| `retention_runs` | per type: `last_run_at`, counts |
| `pending_blob_deletes` | S3 keys to delete after rows commit; retried until gone |

The API's database role has `INSERT` and `SELECT` only on `audit_log` and
`audit_checkpoints`; no `UPDATE` or `DELETE`. Only the retention job's role
may delete audit rows, after writing a checkpoint.

## 7. Jobs and the worker

- **Types:**
  - `run_query`
  - `submit_feedback`
  - `approve_correction`
  - `ingest_document`
  - `run_triage`
  - `kb_search` (RPC)
  - `feedback_buffer_reset`
  - `retention_purge`
- **Redis payload (versioned):** `{v: 1, job_id, type, subject_id, user_id,
  role, request_id}`. Ids only.
- **Claim:** the worker pops the id from Redis, then claims it in Postgres
  with `UPDATE jobs SET status='running', run_id=:new, worker_id=:me,
  attempts=attempts+1 WHERE id=:id AND status='queued' RETURNING`. Zero
  rows means another worker has it or it's finished, so the worker skips it.
- **Stale writes are blocked:** every progress, result or status write
  includes `AND run_id=:run_id`, so a superseded attempt can't overwrite a
  newer one.
- **Error classes:** one classifier for the whole system
  (`mhai/platform/errors.py`).
  - *Transient:* network errors, timeouts, Ollama 5xx or 429, database
    connection loss. Retried up to 3 attempts, with backoff
    10 s · 2ⁿ (`not_before`).
  - *Permanent:* validation errors, unreadable PDFs, missing subject.
    The job fails at once and goes to the DLQ.
  - *Unknown errors count as transient once, then as permanent.*
- **Shutdown:** SIGTERM finishes or returns the current job to `queued`
  **without** using an attempt.
- **Time limit:** `JOB_TIMEOUT_S` (default 300) is the single source for both
  the run deadline and the claim lease.
- **Recovery at start:** re-queue only `running` jobs whose `worker_id`
  heartbeat is stale (over 30 s), not every running job.
- **Heartbeat:** Redis key `worker:{id}` with a 30 s TTL, refreshed every
  10 s, including the inference models and versions it is using. Never
  accepted over HTTP.
- **Idempotency:** handlers check the subject's state first, for example
  they skip a query that is already `completed`. Admin DLQ retry takes an
  `Idempotency-Key`.
- **KB search RPC:** the API enqueues a `kb_search` job with a reply key and
  waits ≤5 s on `BLPOP reply:{job_id}`; otherwise `kb_unavailable`. The
  results carry chunk ids and short excerpts only.

## 8. Changes inside the existing pipeline

1. **Per-citation results:** `NLIBackedHGA.verify` returns `citations`
   from `CitationVerificationResult`s, with `source_title` from chunk
   metadata. The orchestrator passes them through.
2. **Stage hook:** an optional `on_stage(stage, progress)` callback, a
   no-op by default, flows through:
   - `SecurePipelineWrapper.submit_query` (pii_masking);
   - `RAGClinicalContextAgent.generate_with_rag` (retrieval, generation);
   - `NLIBackedHGA.verify` (nli_verification, hga_verification);
   - `DynamicOrchestratorAgent.process_query` (decision).
3. **Citations must have been retrieved:** a cited chunk id that isn't in
   the set retrieved for this query and patient is REDACTed with reason
   `not_retrieved`, and counted in the guardrail stats.
4. **Diagnosis extraction:** a small parser for `diagnosis` and
   `icd10_code`, returning `null` when nothing is found.
5. **Persistent audit:** a Postgres-backed `AuditLogger` with the same
   `log_event` interface. Writes are serialised with
   `pg_advisory_xact_lock`, so the API and the worker keep one chain. Query
   text is stored only as a hash.
6. **RBAC change:** admin loses clinical permissions (§5.4).
7. **DIA:** add an OCR-only path (`ingest_pdf_ocr`: whole page, Tesseract,
   page numbers) and remove the hard-coded layout boxes from the ingest
   path. Swin-V2 stays for when trained weights exist.
8. **Inference clients:** `DenseRetriever` uses `Embedder`;
   `NLIEntailmentVerifier` and the L2 bidirectional-entailment check use
   `NLIClassifier`; `RAGClinicalContextAgent` uses `Generator` (the
   existing `OllamaGenerator`); the OCR-only DIA path uses `OCR`. The
   in-process model code stays only for research scripts.
9. **Keep torch out of the worker's import path.** Today, importing
   `mhai.agents.orchestrator` or `mhai.agents.hga_nli` loads torch, because
   `mhai/agents/__init__.py` eagerly imports the DIA and QDA modules, and
   `l2_semantic_entropy.py` imports torch at module level. Changes:
   - the package `__init__` exports lazily (`__getattr__`);
   - the torch-based classes in `l2_semantic_entropy.py` move to a
     research-only module;
   - in `pyproject.toml`, torch, transformers and sentence-transformers move
     from the base dependencies to an `ml` group, used by the `inference`
     image and research scripts only.
10. **QDA gate:** `TriageGate` checks the `TRIAGE_ENABLED` setting, the
   weights' SHA-256 and the validation report before any triage run. When
   closed, the worker refuses and the API returns 503.

## 8a. Inference services

- **Interfaces** (`mhai/inference/`): the pipeline depends on these only.
  - `Embedder.embed(texts) -> vectors` (with model name, dimension and
    version).
  - `NLIClassifier.classify(pairs) -> [{label, scores: {entailment,
    neutral, contradiction}}]`. Labels are **named**, never indexes, so
    label-order bugs can't recur.
  - `Generator.chat(system, user, params) -> text` (Ollama today).
  - `OCR.page_text(image) -> text`.
- **Wire protocol:**
  - embeddings use the OpenAI-compatible `POST /v1/embeddings`, which the
    common engines (TEI, vLLM, Ollama) already speak, so the engine is a
    config change;
  - NLI uses `POST /v1/classify` with named labels;
  - `GET /health` reports loaded models, versions and checksums;
  - every response carries `model` and `model_version`, which are recorded
    with each decision.
- **`inference` service, first version:** a small Python server (FastAPI
  plus the current models: `sentence-transformers/all-MiniLM-L6-v2` and
  `cross-encoder/nli-deberta-v3-base`). It batches requests (max batch
  size and max wait configurable), caches embeddings by
  `(model, version, text hash)` in memory only, and runs on CPU or GPU
  (`TORCH` build arg).
- **Clients:** timeouts, a retry on transient errors (they share the §7
  classifier), and a circuit breaker. Inference down means agents fail,
  which means the result is ESCALATEd, as today.
- **Readiness and status:** `/readyz` includes `inference`, and
  `/agents/status` lists the served models and versions.
- **Later, a config change, not a rewrite:** move embeddings to TEI or
  vLLM, NLI to Triton or a GPU node, OCR into its own service, and scale
  inference replicas on their own.

## 9. Documents and storage

- **`BlobStore` protocol:** `put`, `get`, `delete`, `exists`, `presign(ttl)`
  and `health`. Implementations: S3 (boto3; MinIO in Compose, SSE on) and
  in-memory for tests. `BlobNotFound` is a different exception from
  `BlobBackendError`.
- **Keys:** `patients/{patient_id}/docs/{document_id}.pdf`.
- **Upload checks:**
  - ≤20 MB;
  - starts with `%PDF-`;
  - ≤50 pages, counted after upload in the worker;
  - SHA-256 recorded.
  - The client-supplied file name is never used in keys.
- **Ingest job:**
  1. fetch the PDF;
  2. render the pages (poppler, with time and memory limits);
  3. OCR each page (Tesseract), using the cache (`document_pages`) when
     the hash and OCR version match;
  4. write patient-scoped `kb_chunks` with
     `{document_id, page, ocr_only: true}`;
  5. add them to the in-memory KB and re-index.

## 10. Triage gate

`TRIAGE_ENABLED` (a runtime setting) plus `QDA_WEIGHTS_PATH`,
`QDA_WEIGHTS_SHA256` and `QDA_VALIDATION_REPORT` (JSON with metrics). All
must be present and match before a request is accepted. Enabling triage is
a `clinical-review` change and is audited with the checksum. While gated,
`POST /triage` returns `triage_unavailable`; nothing is simulated.

## 11. Admin and audit

- **Endpoints:** as in §4.1. Every admin action is audited.
- **Self-protection:** an admin can't demote or deactivate themselves.
- **Feedback-buffer reset:** clears only `TrustWeightOptimizer`'s buffer of
  unapproved data (via a worker job). Approved corrections and the KB are
  untouched.
- **Audit verify:** walks the chain from the latest checkpoint and reports
  the first broken id.

## 12. Retention

| Data | Default (placeholder, to be confirmed) |
|---|---|
| Queries, decisions, feedback, approved corrections | 6 years |
| Documents (S3 object, row, chunks, OCR cache) | 6 years |
| Pending corrections | 1 year |
| Audit log | 6 years minimum (settings refuse lower values) |

- **The job:** a daily `retention_purge` job under `pg_try_advisory_lock`.
  It records `last_run_at` per type and works in bounded batches, each in
  its own transaction.
- **Holds and cancellation:** legal holds are excluded in the SQL itself,
  and the job checks for cancellation between batches.
- **S3 deletes** happen after the rows commit, through
  `pending_blob_deletes`.
- **Audit purge:** writes an `audit_checkpoints` row first, then deletes
  the rows.
- **Run summary:** each run writes one audit entry with counts only.
- **Commands:** `mhai retention run --dry-run` reports without deleting.
  `RETENTION_ENABLED=false` by default in `local`.

**Open item:** the numbers above are placeholders until the user or the
hospital's compliance lead confirms the legal requirement.

## 13. Logging, configuration and security limits

### Logging

- Structured JSON. A contextvar carries `request_id`, `job_id` and
  `run_id` into every line.
- `request_id` travels in job payloads, so API and worker logs connect.
- The access log records the **route template**, never the raw path or the
  query string. It also records status, latency and user id.
- **Never logged:**
  - request or response bodies;
  - query text, answers or OCR text;
  - LLM prompts or responses, even in debug mode.

### Configuration

- pydantic-settings with a nested section per component. Secrets are
  `SecretStr`.
- Startup fails on invalid or placeholder secrets: `JWT_SECRET`,
  `FERNET_KEY`, database and S3 credentials.
- Runtime-tunable values (retention periods, triage gate) live in the
  `settings` table, and changes are audited.
- `GET /admin/settings` never returns secrets.

### Security limits

- **Rate limits:** 30 query submissions per user per minute, and 10
  uploads per user per hour. Both return `rate_limited`.
- **CORS:** only the origins in `CORS_ORIGINS`.
- **HSTS:** sent when `APP_ENV != local`. TLS terminates at a reverse
  proxy in front of the API.

## 14. Testing

- **API:** FastAPI `TestClient` against a real Postgres (a CI service
  container, migrations applied) with `fakeredis` and the in-memory
  `BlobStore`.
- **Worker:** handlers tested with the stub CCA and HGA from
  `tests/security`, so CI downloads no models. Job lifecycle tests cover:
  - claim races;
  - `run_id` blocking stale writes;
  - transient versus permanent errors;
  - SIGTERM not costing an attempt;
  - recovery of stale-heartbeat jobs only.
- **Security tests (required):**
  - `clinician_id` or `user_id` in a body → 422;
  - another user's query → 404;
  - self-approval → 403;
  - admin calling a clinical endpoint → 403;
  - expired or logged-out token → 401;
  - login lockout;
  - MFA required for admin and auditor;
  - TOTP code reuse rejected;
  - OIDC state, nonce and audience mismatches rejected;
  - validation errors don't echo input;
  - no PHI in captured logs;
  - a citation outside the retrieved set is REDACTed;
  - triage gated returns 503;
  - an upload that isn't a PDF is rejected;
  - legal hold survives a purge;
  - the audit chain verifies across a checkpoint.
- **Boundary tests:**
  - importing `mhai.api` doesn't import model, agent, guardrail or
    knowledge-base modules;
  - neither `mhai.api` nor `mhai.worker` imports `torch`, `transformers` or
    `sentence_transformers`.
- **Inference contract tests:** the clients run against a stub server
  (fixed vectors and labels). The `inference` server has its own tests,
  marked `models`, run outside CI.
- **Contract test:** CI regenerates the OpenAPI schema and fails if it
  differs from the committed `openapi.json`.
- **Real end-to-end:** `scripts/e2e_models.py --http` drives the Compose
  stack through the API with real models. It's run manually before
  releases.

## 15. Deployment

- **Compose:**
  - `migrate` (one-shot);
  - `inference` (embeddings and NLI; its own image; the GPU override
    applies to it and Ollama);
  - `api` (serves :8000, health check on `/readyz`);
  - `worker` (single replica);
  - `minio` plus `minio-init` (bucket, SSE);
  - `keycloak` under the `sso` profile.
  - The old `mhai` service is replaced.
- **New dependencies:**
  - `redis`
  - `pyjwt`
  - `argon2-cffi`
  - `pyotp`
  - `authlib` (OIDC)
  - `psycopg[binary]`
  - `alembic`
  - `boto3`
  - `pdf2image`
  - `pytesseract`
  - `structlog`
  - `python-multipart`
  - The API and worker image adds `tesseract-ocr` and `poppler-utils`
    **and drops torch**. The ML stack (`torch`, `transformers`,
    `sentence-transformers`) moves to the `inference` image's dependency
    group.
- **CLI:**
  - `mhai serve`
  - `mhai worker`
  - `mhai users create`
  - `mhai seed-demo` (local only)
  - `mhai retention run [--dry-run]`
  - `mhai openapi export`

## 16. Open items and follow-ups

- Confirm the retention periods (legal or compliance).
- Antivirus scanning of uploads.
- Per-patient erasure.
- Machine tokens: hashed, scoped, expiring, if ever needed.
- Persisting the KB in Postgres (#9). The KB-search RPC and the single-worker
  limit go away once it lands.
- L1 persona and L2 entropy values in results, once the pipeline computes
  them.
- Trained DIA layout and QDA models, each a `clinical-review` change, served
  through the inference layer.
- Move inference engines (TEI or vLLM for embeddings, Triton or a GPU node
  for NLI, an OCR service) as load requires. These are configuration
  changes under ADR 0008.
