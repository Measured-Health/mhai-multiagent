# Plan 4: feedback and corrections, the audit log, platform operations, and what to do about documents: design

- **Status:** Draft, for the product owner's approval. Design only; no code.
- **Date:** 2026-10-31 (product owner approved starting Plan 4 on 31 Oct)
- **Plan:** to be written from §15 as `docs/plans/2026-11-xx-plan-4-feedback-audit-admin.md` once this is approved.
- **Builds on (and must stay consistent with):**
  - [ADR 0004](../adr/0004-scope-retrieval-to-one-patient.md) (retrieval scoped to one patient), [ADR 0005](../adr/0005-fail-closed-and-review-corrections.md) (fail closed; a correction changes nothing until a different clinician approves it), [ADR 0009](../adr/0009-integrate-many-emr-systems.md), [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md), [ADR 0011](../adr/0011-frontend-ingress-over-nats.md), [ADR 0012](../adr/0012-api-versioning.md) (additive changes only inside `v1`).
  - The intelligence boundary design, `docs/specs/2026-10-28-intelligence-boundary-design.md`, **as built** (its §18): request tokens, the operations table, `with_hospital`, the outbox and its closed payload schemas, dead letters, `mhai_jobs`, `mhai_audit_drain`, the per-hospital cap. Plan 4 mirrors the `run_query` path; where it widens a Plan 3 piece it says so.
  - The document pipeline design, `docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md` (approved 28 Oct).
  - The frontend contract, `docs/specs/2026-10-17-frontend-requirements.md` §4, §6, §8, §14.6–§14.9, and mhai-web's `apps/clinical/lib/api/contract.ts` (`FeedbackIn`, `FeedbackOut`, `CorrectionOut`, `RejectIn`, `FeedbackStatsOut`) and `apps/platform/lib/api/{audit,ops}-contract.ts`, `apps/platform/lib/audit/kinds.ts`.
  - The tracker's not-started backend rows B-14 (feedback), B-17 (document ingest), B-18 (triage), B-19 (KB search), B-20 (HGA stats), B-21 (agent status and the dead-letter inspector), B-22 (audit log), B-23 and KI-5 (reset the feedback buffer); TODO.md.

## 1. Why

Plan 3 joined core and intelligence: a clinical question now runs BFF → core → NATS → intelligence → Postgres and back. The tracker's remaining backend endpoints are all on top of that path, and mhai-web has already built their screens on sample data: corrections (F-60), the platform audit log (F-77), system health and failed jobs (Plan 8b). Without the backend, those screens can't go live.

This design says which of those endpoints Plan 4 builds, how each one keeps hospital separation, the two-person rule and the audit chain intact, and what is left for later plans and why.

## 2. Scope: what is in Plan 4 and what isn't

| Item (tracker) | Plan 4? | Why |
|---|---|---|
| **Feedback and corrections** (B-14): `POST /feedback`, the correction review endpoints of contract §4, `GET /feedback/stats/mine`, the `approve_correction` job | **In** | P0. The clinical app's feedback and review screens are built and waiting (F-17, F-60). ADR 0005 asks for exactly this. |
| `submit_feedback` job type | **Reserved, not built** | Submitting feedback needs no model work: it is a row core writes. A job would add a failure mode and nothing else (§4.6). The name stays reserved in the job-type vocabulary. |
| **The audit log** (B-22): read, filter, page, chain verification | **In**, as the platform app's `/platform/audit` (contract §14.9): events, detail, hospitals, people, verify runs and status, the outcome note, undisplayable reports | P1. The chain exists since Plan 2 with no way to read it; the platform app's audit screens are built. Exports, named access and pseudonym reveal wait (§5.6). |
| **System health** (B-21, "agent status"): `GET /platform/health` | **In** | Plan 3 built the metrics and `pipeline.health`; this is a reading of them. |
| **The dead-letter inspector** (B-21): `GET /platform/jobs`, retry, cancel | **In** | Plan 3 built the dead-letter consumer and the re-mint the endpoints call (TODO "Platform job views"). |
| **Guardrail statistics** (B-20, "HGA stats") | **In**, as a new platform route `GET /platform/guardrails` with the §14.7 small-group rules | No screen asks for it in the contract yet; the data is in `clinical_decisions`. A small, additive route (§6.3). |
| **Document upload** (B-17) and the `ingest_document` job | **Out: a separate document pipeline plan** | §7. Upload without the documents service would be a store of untrusted patient files with nothing scanning, reading or deleting them. |
| **`POST /triage`** (B-18, QDA) | **Out** | QDA's integrated accuracy is about 30% (tracker KI-2); the contract gates triage behind clinical sign-off and a ready model (§14.8.4). Nothing to build safely yet (§8). |
| **`GET /kb/search`** (B-19) | **Out** | No screen in either app calls it. When one does, it is a request/reply operation with its own review (§8). |
| **Reset the feedback buffer** (B-23, KI-5) | **Out, to the platform settings plan**, but Plan 4 builds the buffer so the reset is one small task | The reset's screen lives in `GET /platform/settings`, whose other blocks (retention, triage) don't exist. Plan 4 never applies the buffer automatically, so there is nothing to undo yet (§4.8, §8). |
| Hospital admin oversight of corrections (contract §5.4: assign, remind, eligible reviewers) | **Out**, to the hospital admin plan | Needs the staff endpoints (`can_review`, `review_specialties`). Plan 4's tables leave room for it (§4.3). |

## 3. Architecture

Nothing new crosses the boundary. Every endpoint below is a core route over `api.v1.<audience>` (ADR 0011), behind the Plan 2 chain (principal, route rule, idempotency, RLS transaction, audit). One new job type goes to intelligence, published only by core, and it mirrors `run_query` exactly.

```
clinical BFF ──api.v1.clinical──▶ core
   POST /feedback, /corrections/*          │ writes feedback_events (RLS, hospital from the token)
                                           │ approve: one transaction ─ correction approved
                                           │                          ─ jobs row (approve_correction, queued)
                                           │                          ─ audit correction.approved
                                           │ after commit: publish jobs.v1.<hid>.approve_correction {job_id, correction_id}
                                           ▼
                                   JetStream JOBS ──▶ pipeline worker (consumer pipeline_learning)
                                                        │ verify token (spec §7.3, unchanged)
                                                        │ claim the jobs row under with_hospital(hid)
                                                        │ read scores (hga_evaluations), never the correction's text
                                                        │ LearningClient.buffer_entry(...)  (pure, no model)
                                                        │ one transaction: feedback_buffer row + outbox correction.applied + job completed
                                                        ▼
                                              core drainer → audit chain

platform BFF ──api.v1.platform──▶ core
   /platform/audit/*      reads audit_log as mhai_platform; verify runs in a core loop
   /platform/health       a cached reading, refreshed every 30 s by a core sampler (pipeline.health + DB + metrics)
   /platform/jobs/*       reads and moves job rows as mhai_platform / mhai_jobs; retry re-mints (Plan 3 intel.Republish)
   /platform/guardrails   counts from clinical_decisions with the small-group rules
```

The rules from Plan 3 hold unchanged: Python takes the hospital and the user only from the verified token; Python writes no permission rule; only Go writes the audit chain; jobs carry ids, never text.

## 4. Feedback and corrections

### 4.1 What the clinician does

After an answer, the clinician who asked either agrees ("validate") or suggests a correction with a corrected diagnosis and a reason. A correction then waits until **a different clinician of the same hospital** approves or rejects it (ADR 0005). The submitter may withdraw it while it waits. Only an approved correction reaches intelligence, and only as ids.

### 4.2 Endpoints (clinical audience)

Shapes are mhai-web's `contract.ts` and frontend contract §4, unchanged. Every write takes `Idempotency-Key` (72 hours, as Plan 2). None is `bff_direct`.

| Endpoint | Roles | Behaviour |
|---|---|---|
| `POST /feedback` `FeedbackIn` + optional `client_created_at` | `clinician` | `{query_id, action: "validate"}` → **200** `{status: "recorded"}`. `{query_id, action: "correct", corrected_diagnosis, reason}` → **200** `{status: "pending_review", correction_id}`. The query must be the caller's own and `completed`; anything else is **404** `not_found` (another clinician's, another hospital's, unknown, not finished). One live feedback per query: a second one (any action) is **409** `conflict`; after a withdrawal a new correction is allowed. `reason` 3–1000 characters, `corrected_diagnosis` 1–200, both trimmed, no control characters; `client_created_at` ISO 8601, not more than 5 minutes ahead of core's clock; otherwise **422** `validation_failed`. |
| `GET /corrections?status=pending` | `clinician` | `CorrectionOut[]`: waiting corrections of the caller's hospital, **never the caller's own**, oldest first, at most 100. Any other `status` value is 422. |
| `GET /corrections/mine` | `clinician` | the caller's own corrections, every status, newest first, at most 200. |
| `GET /corrections/reviewed` | `clinician` | corrections the caller decided in the last 30 days, at most 50, newest first. |
| `POST /corrections/{id}/approve` | `clinician` (operation `pipeline.approve_correction`) | **204**. **403** `self_approval` on your own; **409** `already_decided` if no longer waiting; **404** `not_found` for an unknown id or another hospital's. |
| `POST /corrections/{id}/reject` `{reason_code, note?}` | `clinician` | **204**. `reason_code` ∈ `not_supported`, `needs_rewording`, `unsafe`, `out_of_scope`; `note` ≤ 500 characters. Same refusals as approve. |
| `POST /corrections/{id}/withdraw` | `clinician` | **204**. Your own waiting correction only; someone else's is **403** `forbidden`; decided is **409** `already_decided`. |
| `GET /feedback/stats/mine` | `clinician` | `FeedbackStatsOut`: `agreed`, `corrections_waiting`, `corrections_approved`, `corrections_rejected`, `corrections_withdrawn`. The caller's own counts; no small-group rule applies to a person's own numbers. |

- **New problem codes,** additive under ADR 0012: **403 `self_approval`**, **409 `already_decided`**. Both are in the contract already.
- **Guards before work,** in this order: route role → `not_found` → `self_approval` / `forbidden` → `already_decided` → body validation. A refused request writes nothing but its audit row (§9).
- **The 60-second undo** is the app's: it delays the approve call, and a `keepalive` approve is an ordinary request.
- A hospital admin who isn't a clinician gets 403 on every route above (Plan 2's route rule).

**`CorrectionOut`** is built by core from three rows: the correction (`feedback_events`), the decision (`clinical_decisions`) and the **masked** consultation (§4.4). People are `PersonOut` (`display_name`, `full_name`, `specialty`, `hospital` name), never ids. `rejection.note` is shown to the submitter and the reviewer only.

### 4.3 Data: `feedback_events`, widened

The table exists since Plan 2's port (00002) and is empty outside tests. Migration 00010 widens it rather than adding a second table, so a validation and a correction are one kind of row:

| Column | Notes |
|---|---|
| `id uuid` | the `correction_id` for a correction |
| `hospital_id uuid NOT NULL` | from the token; RLS |
| `query_id uuid NOT NULL` | FK `(query_id, hospital_id)` → `clinical_queries`; replaces `decision_id` as the link (the decision is found by `query_id`, unique) |
| `submitted_by uuid NOT NULL` | renamed from `clinician_id`; FK users |
| `action text` | `validate` or `correct` (CHECK) |
| `status text NOT NULL` | `recorded` (validate) or `pending_review`, `approved`, `rejected`, `withdrawn` (correct); CHECK ties status to action |
| `corrected_diagnosis text`, `reason text` | correct only; length CHECKs as §4.2. **Free text, may hold patient details:** never audited, logged, sent to intelligence or copied into the buffer |
| `decided_by uuid`, `decided_at timestamptz` | set on approve and reject; `decided_at` also on withdraw |
| `rejection_code text`, `rejection_note text` | reject only; CHECK on the code set, note ≤ 500 |
| `client_created_at timestamptz`, `request_id text`, `created_at` | |
| `assigned_to uuid` | reserved for the hospital admin plan (§5.4 assign); always null in Plan 4 |

`hga_weight_suggestions_json`, `processed` and `processed_at` are dropped (nothing reads them; the buffer replaces them).

**The two-person rule in the database.** Core's handler checks it first (that's what gives the clean 403), but the database is what makes it impossible to break:

1. `CHECK (decided_by IS NULL OR decided_by <> submitted_by)`.
2. `CHECK ((status IN ('approved','rejected')) = (decided_by IS NOT NULL AND decided_at IS NOT NULL))`, and `decided_by IS NULL` for `withdrawn`.
3. A constraint trigger `feedback_decider_check` (SECURITY DEFINER, owned by `mhai_migrate`, the 00003 `pairing_*` pattern) on every move to `approved` or `rejected`: `decided_by` is an **active** user of the **same hospital** holding the **`clinician`** role at that moment. It reads `users` and `user_roles` through owner policies and nothing else.
4. A transition trigger: status moves only `pending_review → approved | rejected | withdrawn`; a decided row never changes again (`recorded` never changes at all). `withdrawn` only when `submitted_by` is the session's user: the handler passes it, and the trigger checks `submitted_by = current_setting('mhai.user_id')`, which core sets with `set_config(..., true)` from the verified token in the same transaction as the hospital (a new, local setting; RLS reads stay hospital-only).
5. A partial unique index `(query_id) WHERE status <> 'withdrawn'`: one live feedback per query, so two parallel submits give one row and one 409.

User ids are uuids, so "different person" is an exact comparison; the Python loop's string normalisation (`_normalize_user`) isn't needed and isn't used.

### 4.4 What a reviewer may see: the masked consultation

A reviewer isn't the query's owner, so `GET /queries/{id}/result` refuses them, and they must not see the patient details the question was asked with. `CorrectionConsultationOut` is "the consultation as it was asked (patient details already masked)". Core can't mask (masking is Python's), and `clinical_queries` holds the raw question.

**Decision:** the `run_query` job already masks the question and the patient data before generation (`_mask_all`). Migration 00010 adds `masked_question text` and `masked_patient_json jsonb` to `clinical_decisions`, and the worker writes them in the same completion transaction as the decision (Plan 3's Task 11 path; a column grant for `mhai_worker` INSERT). The reviewer's view reads only these columns, never `clinical_queries.clinical_question` or `patient_data_json`. A correction on a decision without the masked copy (made before Plan 4) is refused with 409 `conflict` at submit; synthetic data only, so nothing real is lost.

`mhai_gateway`'s column grants on `clinical_decisions` gain the masked columns for this one reader; a Go test (the twin of `nodirectdb_test.go`) fails if any handler other than the corrections reader selects them.

### 4.5 Approval: core's transaction, then the job

`POST /corrections/{id}/approve`, inside `db.WithHospital` with `mhai.user_id` set:

1. `UPDATE feedback_events SET status='approved', decided_by=$me, decided_at=now() WHERE id=$1 AND action='correct' AND status='pending_review' AND submitted_by <> $me RETURNING ...`. No row: read the row to choose 404, 403 `self_approval` or 409 `already_decided` (and audit the refusal, §9).
2. Insert a `jobs` row (§6.5): `type 'approve_correction'`, `subject_id` the correction id, `created_by $me`, `status 'queued'`, deadline 5 minutes.
3. `audit.Write` `correction.approved`.
4. Commit, then publish `jobs.v1.<hid>.approve_correction` with `Nats-Msg-Id` = job id and a job token (`sub` the reviewer, `roles` now, `hid`, `exp` the deadline). If the publish fails, the Plan 3 sweeper (widened to `jobs`) republishes.
5. Reply 204.

The per-hospital in-flight cap (Plan 3, `intel/cap.go`) gains a second budget, 20 `approve_correction` jobs per hospital; past it the approval still commits and the job waits for the sweeper, because refusing a reviewer's decision for a queue limit would be wrong (the decision is the human act; the job is bookkeeping).

Reject and withdraw are core-only: one conditional `UPDATE` and an audit row. Nothing goes to intelligence.

### 4.6 Why no `submit_feedback` job

ADR 0005 says a correction changes nothing until it is approved, and a validation changes nothing at all (`DoctorFeedbackLoop.process_feedback` only counts it). So nothing on submission needs a model or intelligence's state. A job would add a queue, a token, a dead-letter path and a failure the clinician sees as "Couldn't save", for no work. `submit_feedback` stays in the job-type vocabulary (boundary spec §5.1, the platform app's `JobType`) for the day submission needs a model (for example, a check that a corrected diagnosis is codable). **Open question 2.**

### 4.7 The `approve_correction` job (intelligence's side)

A copy of the `run_query` path, minus the model.

- **Operation row** (core's table, the only rule): `pipeline.approve_correction`, `Kind job`, `AnyRole [clinician]`, `NeedsHospital true`, `SvcCore false`, `BFFDirect false`, `Timeout 5m`, `RequestSchema mhai.intelligence.v1.ApproveCorrectionJob`. It gains one new field, **`SecondPerson: true`**, read by core only: `intel.PublishJob` refuses to mint a token for this operation unless the subject row's `decided_by` equals the token's `sub` and differs from `submitted_by` (a Go check that a wrong caller can't skip, plus the DB rules in §4.3). `operations.json` carries the flag for documentation; Python never acts on it.
- **Message:** `ApproveCorrectionJob {job_id, correction_id}` in `proto/mhai/intelligence/v1/learning.proto` (additive; `buf breaking` passes).
- **Consumer:** a second durable pull consumer, `pipeline_learning`, on `jobs.v1.*.approve_correction`, so learning jobs never wait behind clinical questions (its own `max_ack_pending`, 4). The `pipeline` NATS user gains that consumer's three `$JS` subjects only.
- **Worker steps** (`boundary/jobs/approve_correction.py`, the `run_query` skeleton reused):
  1. Verify (spec §7.3, unchanged; check 9 reads `operations.json`).
  2. **Claim** under `with_hospital(verified)`: `UPDATE jobs SET status='running', attempts=attempts+1, lease_until=now()+'60 s' WHERE id=$job_id AND type='approve_correction' AND subject_id=$correction_id AND (status='queued' OR (status='running' AND lease_until<now())) RETURNING ...`. No row → as `run_query` (another hospital's id is `not_found`, `Term` and a dead letter, no write).
  3. **Read** (column grants, §10.3): the correction's `id, query_id, status, submitted_by, decided_by`; the decision's `hga_decision, model_versions`; the `hga_evaluations` scores (`nli_score, coherence_score, numerical_score, se_score, trust, decision`). Never `reason` or `corrected_diagnosis`. If the correction isn't `approved`, `Term` with `invalid_request` (core or the DB went wrong; it alerts).
  4. **Compute** through a new seam, `LearningClient` (beside `ModelClient`, same file pattern): `buffer_entry(scores, system_decision) -> BufferEntry` (a pure function: the four expert scores and the system decision, the input `TrustWeightOptimizer.add_correction` takes today) and `persona_update(...) -> "skipped" | "applied"` (returns `"skipped"` in Plan 4: §4.8). `StubLearningClient` for tests.
  5. **Complete** in one transaction: insert `feedback_buffer` (unique `correction_id`), the outbox fact `correction.applied`, `jobs.status='completed'`. Ack after commit. A unique violation means another delivery finished it: roll back, ack.
  6. **Fail:** as `run_query`: `Nak(delay)` for retryable codes, `Term` + `dead.v1.<hid>.approve_correction` + `status='failed'` + outbox `correction.apply_failed` for permanent ones or the last delivery. The correction stays `approved`: the human decision is never undone by a failed job; the platform's Failed jobs view retries it.
- **What the worker never does:** check who approved (the DB did), read the free text, write `kb_chunks`, change `guardrail_weights`, or call `DoctorFeedbackLoop`. The two Plan 3 tests that pin this (`test_feedback_has_no_boundary_operation`, `test_boundary_code_never_reaches_the_feedback_loop` in `tests/security/test_feedback_review.py`) are **replaced, not deleted**: the first becomes "the only feedback operation is `pipeline.approve_correction`, it names `clinician` only, and it is not `bff_direct`"; the second stays as it is (the boundary code still never names `process_feedback`, `approve_correction` the method, or `DoctorFeedbackLoop`).

### 4.8 What an approved correction feeds, in Plan 4

The tracker says corrections feed the persona vector, the CMA-ES buffer and the audit log. Plan 4 does the last two and records the first's input; it applies nothing to the live guardrails.

| Feed | Plan 4 | Why |
|---|---|---|
| **Audit log** | `feedback.validated`, `correction.submitted`, `.approved`, `.rejected`, `.withdrawn`, `.decision_refused` (core), `correction.applied`, `.apply_failed` (outbox) | ids and codes only (§9) |
| **CMA-ES buffer** | a `feedback_buffer` row per approved correction: hospital, correction id, the four expert scores, the system decision, `model_versions`, created time; `cleared_at` and `reset_id` for the later reset | the data `TrustWeightOptimizer` needs, kept in Postgres instead of a Python list that a restart loses |
| **Running CMA-ES and changing the weights** | **not in Plan 4** | `guardrail_weights` is global: one hospital's corrections would retune every hospital's guardrails, which is a cross-hospital influence and a clinical change. **Open question 3** recommends that a CMA-ES run produce a *proposed* weight set that goes through clinical change control (CONTRIBUTING), never an automatic update. |
| **Persona vector** | **not in Plan 4**; `persona_update` returns `skipped` | The update needs the model's hidden activations for the wrong and the corrected answer. The pipeline doesn't capture them, and capturing them is model internals (the intelligence lead's scope). **Open question 4.** |
| **KB enrichment** (verified correction chunks) | **not in Plan 4** | It writes clinician free text into the knowledge base, and without patient ids in queries (Plan 3 departure 7) every such chunk would be `UNASSIGNED`, visible to no query (ADR 0004). `mhai_worker` keeps no `kb_chunks` write. |

### 4.9 RLS, roles and grants for feedback

- `feedback_events`: RLS `hospital_rows` (exists, 00003) for `mhai_gateway` and `mhai_worker`. `mhai_gateway`: `SELECT`, `INSERT`, and `UPDATE (status, decided_by, decided_at, rejection_code, rejection_note)` only. `mhai_worker`: `SELECT (id, hospital_id, query_id, action, status, submitted_by, decided_by)` only, no write.
- `clinical_decisions`: `mhai_worker` `INSERT` gains the masked columns; `mhai_gateway` `SELECT` gains them (one reader, test-enforced).
- `feedback_buffer` (new): RLS hospital rows, `FORCE`. `mhai_worker`: `INSERT` only, with a **RESTRICTIVE** policy `buffer_needs_approval`: the row's `correction_id` names a `feedback_events` row of the same hospital with `status='approved'` and `decided_by <> submitted_by`. So even a worker bug or a compromised worker can't put an unapproved correction into the buffer. `mhai_gateway`: none. The platform count for the later settings screen is a SECURITY DEFINER function `mhai_feedback_buffer_waiting()` returning one integer, executable by `mhai_platform` only.
