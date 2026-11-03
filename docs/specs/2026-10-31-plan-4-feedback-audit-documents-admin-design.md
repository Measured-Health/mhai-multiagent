# Plan 4: feedback and corrections, the audit log, platform operations, and what to do about documents: design

- **Status:** Draft, for the product owner's approval. Design only; no code.
- **Date:** 2026-10-31 (product owner approved starting Plan 4 on 31 Oct)
- **Plan:** to be written from §14 as `docs/plans/2026-11-xx-plan-4-feedback-audit-admin.md` once this is approved; the task breakdown is §14.
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

## 5. The audit log (`GET /security/audit`, served as `/platform/audit`)

### 5.1 Which path

The tracker and the gateway design name `GET /security/audit` and `GET /security/audit/verify`. Frontend contract §14.9 says the platform app calls their platform-audience forms, `/platform/audit/*`, "over the same log", and that is what the built screens call. **Plan 4 serves `/platform/audit/*` only**, and records in the contract that these are the `/security/audit` rows. A second path for the same read would be a second thing to secure for no caller. **Open question 1.**

### 5.2 Endpoints (platform audience)

Every route: `Platform: true`, two-step always (`NeedsMFA`), `AnyRole [platform_admin, auditor]` unless marked. Ids are opaque (`[A-Za-z0-9_-]+`); errors carry only `code`.

| Endpoint | Plan 4 | Behaviour |
|---|---|---|
| `GET /platform/audit?hospital&person&type&outcome&flagged&from&to&q&cursor&limit` | yes | `{events, next_cursor, total, total_capped, oldest_kept_at, last_checkpoint}`, newest first. `limit` 1–100 (the app sends 50). `total` counts up to 10,000, then `total_capped: true`. `q` is an exact-id search (event id, request id) and ignores the other filters. Keyset paging on `id`; the cursor is opaque (base64url of the last id and a hash of the filters, so a cursor can't be replayed under other filters). |
| `GET /platform/audit/{id}` | yes | the event, plus `patients: null` and `letter: null` (no named access or pseudonyms exist yet) |
| `GET /platform/audit/hospitals` | yes | every hospital the log names, removed ones included |
| `GET /platform/audit/people?q` | yes | `q` ≥ 2 characters; at most 20 `{id, name, email, role, kind, hospital}`. Platform-wide by design (auditors search everyone) |
| `POST /platform/audit/undisplayable {ids}` | yes | 1–50 ids → 204, written as one `audit.undisplayable_reported` entry (a count and the first and last ids, §9). **Auditor write.** |
| `POST /platform/audit/verify` | yes | → `{run_id}`, or the running one. **Auditor write.** |
| `GET /platform/audit/verify/runs?cursor`, `GET .../runs/{id}`, `GET .../verify/status` | yes | as contract §14.9. `status` → `{latest, running, open_failure, security_contact, oldest_kept_at}`; `security_contact` from configuration |
| `POST /platform/audit/verify/runs/{id}/outcome {note, version}` | yes | **auditors only** (`AnyRole [auditor]`); broken runs only; note ≤ 1000 characters, stored on the run row and written as a new `audit.check_outcome` entry whose payload holds the run id only, never the note |
| `GET /platform/audit/{id}/patients`, `GET /platform/audit/named-access`, `POST/GET /platform/audit/exports*` | **no** | They need pseudonyms (Analysis, Plan 6 backend), clearance letters (Plan 11) and export files (object storage). Until then the app's calls get 404 `not_found` (§5.6). |

**A new rule field, `AuditorWrite`.** Plan 2's route rule refuses every unsafe method to an auditor (`Write`), and requires `Write` on every unsafe platform route. Contract §14.9 gives auditors exactly four audit writes. Plan 4 adds `Rule.AuditorWrite`: an unsafe platform route that admits auditors. `authz.validate` allows it only on patterns under `/platform/audit/` and never with `StepUp`, and a test pins the exact list of such routes (undisplayable, verify, outcome), so it can't spread.

**Reads are recorded** (contract §14.9): the first page of a filter set writes one `audit.viewed` (payload `{list: "events", filters}` with the filters as closed codes and ids; `q` never appears in `filters`); an exact-id search writes `audit.searched` (`{searched_kind}`; the id searched goes in `entity_id`). Paging with a cursor writes nothing. The dedupe key is the session, the list and a hash of the filters, held for the page load (5 minutes) in the existing NATS KV idempotency bucket.

### 5.3 Mapping chain rows to the contract's event

The chain stores `action`, `entity`, `entity_id`, `payload` (Plan 2). The contract's event wants `type`, `kind`, `target`, `outcome`, `flag`, `reason`, `facts`, and fails closed on any value it doesn't know (an "undisplayable" entry).

- **One table in Go, `audit/catalogue.go`:** for every action core writes, its contract `type` and `kind`, how its `target`, `outcome` and `flag` are derived, and which payload keys become which `facts` keys. A test walks the source for every `audit.Event{Action: ...}` literal and every outbox action and fails if one has no row; a second test fails if a row emits a `type`, `kind` or `facts` key that isn't in mhai-web's lists (vendored as `testdata/web-audit-vocabulary.json`, refreshed by hand with the mhai-web PR that adds them).
- **Nothing is sent that isn't mapped:** an action with no catalogue row (an old test action, a future one) is sent with only `id`, `seq`, `hash`, `at` and `type: "security"`, `kind: "unknown"`, which the app shows as undisplayable and reports once. Payload values never pass through unmapped.
- **New kinds need the app first** (contract §14.9: "a new kind ... needs a matching app change first"). Plan 4's new actions map to these kinds; mhai-web must add the ones marked new before the backend sends them, or they show as undisplayable (safe, but noisy):

| Action (core) | `type` / `kind` | New in mhai-web? |
|---|---|---|
| `query.submitted` | `consultation` / `question_asked` | no |
| `correction.submitted` | `consultation` / `answer_sent_for_review` | no |
| `feedback.validated` | `consultation` / `answer_agreed` | **new** |
| `correction.approved`, `.rejected` | `review` / `review_submitted` (`facts.state` `approved`/`rejected`) | no (`state` is a known facts key) |
| `correction.withdrawn` | `review` / `review_withdrawn` | **new** |
| `correction.decision_refused` | `security` / `review_refused` | **new** |
| `correction.applied`, `.apply_failed` | `review` / `learning_recorded` | **new** |
| `job.retried` ×n, `job.cancelled` ×n | `platform` / `jobs_retried`, `jobs_cancelled` with `facts.job_count`, no target | no |
| `audit.viewed`, `audit.searched`, `audit.check_outcome` | `audit` / `audit_viewed`, `audit_searched`, `audit_check_outcome` | no |
| `audit.verify_started`, `audit.verify_finished` | `audit` / `audit_check_started`, `audit_check_finished` (`facts.state`, `first_broken_seq`) | **new** |
| `audit.undisplayable_reported` | `audit` / `audit_undisplayable_reported` | **new** |
| `auth.*` (Plan 2) | `sign_in` / `signed_in`, `sign_in_refused`, `sign_in_locked`, `signed_out`; `account` / `password_changed`, `two_step_set_up`, ... | no |

- **People:** `actor` is looked up by `actor_id` (`users`, through `mhai_platform`'s existing column grant): `{id, name: display_name, role, kind}`; `role` is the primary role at read time (`reviewer` for a clinician acting on a correction, per the app's `ActorRole`), `kind` `staff`, `platform` or `system` (`svc:core` and the drainer's system facts).
- **`ip` and `device`:** sent when Plan 2 recorded them in the payload (sign-in rows), otherwise null.
- **`after_break`:** true for every entry after the latest broken run's `first_broken_seq` while `open_failure` holds.
- **No patient data:** no audit payload holds any (Plan 3's closed schemas and the sensitive-key check); a patient target is `{kind: "patient"}` with no id. The catalogue test also fails if any mapped `facts` value could match the pseudonym shape `^P-[A-Z0-9]{4,12}$`.

### 5.4 Chain verification

- **Runs table `audit_verify_runs`** (global, platform-only, no hospital): `id, started_at, finished_at, by (uuid or null for the schedule), state (running|intact|broken|error), entries_checked, from_first, from_checkpoint, to_seq, to_at, first_broken_seq, first_broken_id, first_broken_at, error_code, outcome_note, outcome_by, outcome_at, version`. A partial unique index `WHERE state = 'running'` allows one run at a time; `POST /verify` returns the running one.
- **The walk** reuses `audit.Verify` (Plan 2), widened to report how far it got: a core loop runs it as `mhai_platform` (which already reads `audit_log` and `audit_checkpoints`), in a goroutine with a 10-minute budget; `intact` only when the whole range was walked (`entries_checked > 0`, `to_at` set, `from_first` true); a timeout or error is `error` with a code, never `intact` (contract proof fields).
- **Daily at 06:00 UTC** by a scheduler in core that takes a Postgres advisory lock, so only one core copy starts it.
- **Broken:** state `broken`, `audit.verify_finished` on the chain, `open_failure` true until a later run is `intact` and the outcome note exists. The contract also wants a bell and an email to every auditor and platform admin; the notifications service doesn't exist, so Plan 4 raises an alert (`audit_chain_broken` metric, alerting, ARCHITECTURE "Monitoring") and the bell and email wait for that service (§13).
- **Known gap, unchanged:** the chain can't show rows deleted from its end (TODO "Audit tail deletion"); `from_checkpoint` is always null until signed checkpoints exist. Recorded in §11.6.

### 5.5 Database for the audit reader

- `mhai_platform` keeps its `SELECT` on `audit_log`, `audit_checkpoints`, `hospitals`, `users` (columns), `user_roles` (00003). Plan 4 adds: `SELECT, INSERT, UPDATE` on `audit_verify_runs`; `INSERT` on `audit_log` with the sequence and `audit_chain_head()`, under a policy `platform_append` that allows only the actions `audit.viewed`, `audit.searched`, `audit.verify_started`, `audit.verify_finished`, `audit.check_outcome`, `audit.undisplayable_reported`, `job.retried`, `job.cancelled`. Platform routes run inside `db.WithPlatform`, which sets no hospital.
- A platform account has no hospital, so none of these routes can reach a hospital's clinical tables: `mhai_platform` has no grant on them (the guardrail counts in §6.3 go through one SECURITY DEFINER function).

### 5.6 What the app sees for the deferred audit routes

`/patients`, `/named-access` and `/exports*` answer 404 `not_found`, which the app already treats as "Couldn't load" for that panel. Named-access entries can't exist yet, so nothing is hidden by their absence. The exports wait for object storage and the documents service (signed, one-time files).

## 6. Platform operations: health, failed jobs and guardrail statistics

### 6.1 `GET /platform/health` ("agent status")

Contract §14.8.2, unchanged: `{updated_at, overall, parts, queue, failed_jobs, timing, errors, versions, triage, clean_up}`. Roles: `platform_admin`, `auditor` (the contract lets auditors read every 14.8 `GET`; the tracker's "admin" is read as the platform roles). No hospital, user or content anywhere in it.

- **A sampler, not a per-request computation.** A core loop (advisory-lock leader, every 30 s) takes a reading and stores it in memory and in NATS KV (`health` bucket, one key); the route returns the stored reading with its own `updated_at`. A poll costs one KV get, so a polling client can't make core run the queries or call the workers (§11, P17). The reading is 503 when it is older than 2 minutes or missing.
- **Where each figure comes from:**

| Block | Source |
|---|---|
| `parts.app` | core itself (always `ok` if it answers) |
| `parts.workers` | `pipeline.health` request/reply as `svc:core` (Plan 3): `ok`; a slow answer (> 1 s) `slow`; no responders or timeout `problem` |
| `parts.evidence` | the NLI model's `loaded` flag in `HealthResponse` |
| `parts.guidelines` | at least one shared `kb_chunks` row (count only) |
| `parts.documents`, `parts.clean_up` | **not sent** until those services exist (open question 7) |
| `queue` | JetStream `JOBS` consumer info: pending count and the oldest pending message's age |
| `failed_jobs` | `{now, last_24h}` counts over `clinical_queries` and `jobs` (job columns only) |
| `timing` | p50 and p95 over the last hour and 24 hours of `clinical_queries.completed_at − created_at` (overall) and per step from a new `clinical_decisions.timings_ms` (closed keys `privacy`, `guidelines`, `answer`, `evidence`, written by the worker from its stage timings; 00010) |
| `errors` | counts of failed jobs by `kind`, from the error code through Plan 3's mapping table (§10.1 there) |
| `versions` | `HealthResponse.model_versions`, filtered to the safe pattern |
| `triage` | `{enabled: false}` until triage exists |
| `clean_up` | not sent until retention exists (open question 7) |

- **Small groups:** none of these figures is about a hospital or a person (no split by hospital, no counts of patients), so no suppression applies; the contract's §14.7 rules are for per-hospital figures.
- The cross-hospital reads (`clinical_queries` job columns, `clinical_decisions` timings, `jobs`) go through SECURITY DEFINER functions owned by `mhai_migrate` that return aggregates only (`mhai_health_timings(window)`, `mhai_health_failures()`), executable by `mhai_platform` only. No row-level grant on a clinical table is given to a platform role.

### 6.2 `GET /platform/jobs`, retry and cancel (the dead-letter inspector)

Contract §14.8.3, unchanged.

| Endpoint | Roles | Behaviour |
|---|---|---|
| `GET /platform/jobs?status&type&hospital&kind&cursor` | `platform_admin`, `auditor` | `{jobs, next_cursor}`, newest failure first; each `{id, type, hospital, created_at, failed_at, attempts, kind, status, cancelled_by, cancelled_at}`. **Never the subject, the user or an error message.** |
| `POST /platform/jobs/retry {ids}` | `platform_admin` (`Write`) | 1–50 ids → `{retried, skipped}`. No step-up. |
| `POST /platform/jobs/cancel {ids, reason, note?, mfa_code \| backup_code}` | `platform_admin` (`Write`, `StepUp`) | 1–50 ids → `{cancelled, skipped}`. `reason` ∈ `no_longer_needed`, `cant_succeed`, `other` (note required); note ≤ 500, stored, **never logged or audited**. |

- **Which jobs:** `run_query` (rows of `clinical_queries`, Plan 3) and `approve_correction` (rows of `jobs`, §6.5). A view would join two tables of different shapes under two RLS regimes, so the list is built by one SECURITY DEFINER function `mhai_platform_jobs(filters, cursor, limit)` returning only the contract's columns, executable by `mhai_platform` only.
- **Job ids** are the row ids, opaque to the app. The `type` tells core which table an id belongs to; an id in neither is `skipped`.
- **Retry** runs as `mhai_jobs` (Plan 3's cross-hospital job role), widened to `jobs`: for each `failed` row, check the creator and hospital are still active, increment `retry_count`, set `queued`, commit, then `intel.Republish` (Plan 3: a new token minted as the creator with their **current** roles, a new message id). A creator who is no longer active or no longer holds the operation's role: `skipped`, never minted as someone else. An `approve_correction` retry also re-checks `SecondPerson` (§4.7). One `job.retried` audit row per call with `facts.job_count`, not one per id.
- **Cancel** moves `failed`, `queued` or `retrying` rows to `cancelled` (Plan 3's `mhai_jobs` policy already forbids touching a `running` or `completed` row; a running one is `skipped`), sets `cancelled_by`, `cancelled_at`, `cancel_reason`, `cancel_note`. One `job.cancelled` row with `facts.job_count` and the reason code.
- **Status shown:** `cancelled` if cancelled; `failed` if failed; `retrying` if queued or running with `retry_count > 0`; `retried` if completed with `retry_count > 0`. Retried and cancelled jobs stay listed for 30 days.

### 6.3 `GET /platform/guardrails` (HGA statistics), new

Not in the frontend contract; added as **New** (additive, ADR 0012), so mhai-web can show it and Plan 7c's `quality` area can reuse its query.

`GET /platform/guardrails?period=7d|30d|90d` → `{period, from, to, updated_at, all: Rates, hospitals: [{hospital: {id, name}, ...Rates}]}`, active hospitals only, where `Rates` is `{answers, pass, regenerate, escalate, degraded, pass_share, regenerate_share, escalate_share}` and every value is guarded: `{value, suppressed}`. Counts from `clinical_decisions.hga_decision` and `degraded`. Roles: `platform_admin`, `auditor`.

**Small-group rules (the server applies them, contract §14.7 and §9.1):**
1. Any count of 1–4 is sent as `{value: null, suppressed: true}`; zero is sent as 0.
2. `pass + regenerate + escalate = answers` is a set that adds up: if any member is hidden, hide enough others that the hidden ones together are at least 5, and never exactly one hidden member (secondary suppression).
3. A share is hidden when its numerator or its complement (`answers − numerator`) is 1–4, or when `answers` is hidden.
4. `all` is computed over every active hospital and follows the same rules. It is not used to back out a hidden hospital row: with any hospital row hidden, `all` is still shown, so the rules also hide one more hospital's member when a single hospital's hidden value could be found as `all − Σ shown` (the app can't check this; a property test can).
5. `degraded` is its own count, rule 1 only.

The counts come from one SECURITY DEFINER function (`mhai_guardrail_counts(from, to)`, aggregates per hospital only, `mhai_platform` only); suppression is in Go (`internal/smallgroup`, with the rules as table-driven and property tests), applied before the value is encoded, so a raw 1–4 never reaches the response writer.

Not audited per read (aggregates, no person, polled like health); the route is rate-limited per session (6 per minute) because it scans `clinical_decisions` (cached 60 s per period).

### 6.4 Why not a hospital-side "HGA stats" route

Hospital admins see confidence counts in `GET /hospital/usage` (contract §5.2, `confidence: high/caution/review/failed`), which the hospital admin plan builds over the same function with the same rules. Plan 4 doesn't add a second hospital route.

### 6.5 The generic `jobs` table

`run_query` keeps its job columns on `clinical_queries` (Plan 3; moving them is churn with no gain). Every new job type gets a row in one table, so the documents plan's `ingest_document` and `finalise_document` and a later `feedback_buffer_reset` drop in without a new table each:

| Column | Notes |
|---|---|
| `id uuid` | the job id and `Nats-Msg-Id` |
| `hospital_id uuid` | null only for `global` jobs (none in Plan 4); RLS |
| `type text` | CHECK in the job-type vocabulary; Plan 4 allows `approve_correction` only |
| `subject_id uuid NOT NULL` | the row the job is about (the correction) |
| `created_by uuid`, `request_id text` | the drainer's actor and request id (never from a fact) |
| `status` | `queued`, `running`, `completed`, `failed`, `cancelled` (as `clinical_queries`) |
| `attempts`, `lease_until`, `published_at`, `deadline_at` | as Plan 3 |
| `error text`, `cancel_reason text` | codes only (`^[a-z_]{1,32}$`) |
| `cancelled_by uuid`, `cancelled_at`, `cancel_note text` | the platform cancel; note ≤ 500, never logged |
| `retry_count int`, `failed_at`, `completed_at`, `created_at` | |

Unique `(type, subject_id) WHERE status IN ('queued','running')`: one live job per subject. Policies as 00008/00009 for `clinical_queries`: `mhai_worker` may move only `queued`/`running` rows to `running`, `completed` or `failed`, of an active hospital (`worker_status` RESTRICTIVE, `worker_hospital_active`); `mhai_jobs` only `queued`/`failed`/`cancelled` rows among those three states; `mhai_gateway` `INSERT` and `UPDATE (published_at)`. The sweeper, the dead-letter consumer, the deadline expiry function and cancel-on-deactivate (00009's triggers) are widened to `jobs`.

**The outbox, widened.** `intelligence_audit_outbox.query_id` becomes nullable and gains `job_id uuid` (FK `(job_id, hospital_id)` → `jobs`), with `CHECK (num_nonnulls(query_id, job_id) = 1)`; the action CHECK adds `correction.applied` and `correction.apply_failed`. The drainer takes the actor, hospital and request id from `jobs` for a `job_id` fact, exactly as it does from `clinical_queries` today, and appends a fact only if its action matches the job's status. `mhai_audit_drain` gains `SELECT (id, hospital_id, created_by, request_id, status, type)` on `jobs` and the two actions in its append policy.

## 7. Document upload and `ingest_document`: recommendation

**Recommendation: Plan 4 builds no document work. All of it goes to a separate document pipeline plan, phases 0 and 1 of the approved design (its §10).**

Building "the upload endpoint plus quarantine storage" first looks cheap, but it would:

- **store untrusted patient files with nothing behind them.** The approved flow is upload → magic-byte check → virus scan → sandboxed rasterising → `ingest_document` (§3 there). Without the documents service, uploaded files sit in `quarantine/` unscanned, with no consumer, no retention run and no way for anyone to see them. That is PHI at rest with no purpose, which the Data Protection Act check (TODO) would ask us to justify.
- **need infrastructure that doesn't exist:** object storage with per-hospital prefixes and keys, versioning and presigned PUTs (ARCHITECTURE: "Object storage (planned)"), and a bucket policy review. That is the documents plan's first task anyway, and it belongs with the scanner.
- **build a screen nobody can use:** mhai-web has no upload screen (tracker F-44, "not in any plan yet"); the review screens are a mhai-web plan of their own.
- **not unblock anything:** `ingest_document` needs the GPU host, the evaluation (phase 0) and the canonical tables. Upload is the last thing in that chain to be useful, not the first.

**What Plan 4 does that the documents plan will reuse:** the generic `jobs` table and its policies (§6.5), the outbox's `job_id` path, the second durable consumer pattern, `SecondPerson`-style operation flags, the Failed jobs list that already knows `ingest_document` as a type, and the `AuditorWrite`-style discipline for new rule fields. The documents plan then adds `documents`, `document_extractions` and the rest as the approved design §9.4 lists, with `ingest_document` as one more `jobs.type`.

## 8. The other listed items

- **`POST /triage` (QDA): deferred.** The QDA model's integrated accuracy is about 30% (tracker KI-2, Integration Guide §13), and contract §14.8.4 only lets a platform admin turn triage on with clinical sign-off and a model whose report, files and checksums are present (`not_ready_reason`). Exposing an endpoint before that would put an unvalidated clinical classifier one request away. When it comes, it is `pipeline.run_triage` (a job, like `run_query`, because QDA reads patient data) behind the triage switch, and its own design with clinical sign-off. Plan 4 sends `triage: {enabled: false}` in health.
- **`GET /kb/search`: deferred.** No screen in either app calls it. If one does, it is a request/reply operation `pipeline.kb_search`, core-initiated (ADR 0011 recommends the core path for anything touching patient data), shared guidelines only until patient ids exist (ADR 0004: with no patient, only guidelines) (read-only, so Plan 3's per-worker replay check is enough), a rate limit and an audit row of the query's hash, never its text.
- **Reset the feedback buffer: deferred to the platform settings plan.** It is `POST /platform/settings/feedback-buffer/reset`, shown inside `GET /platform/settings` together with retention and triage, none of which exist. Plan 4 never applies the buffer to the live guardrails (§4.8), so nothing has been learned that a reset would need to undo. Plan 4 makes the reset cheap later: buffer rows carry `model_versions` (so a reset can clear exactly the rows scored by a broken NLI model, the reason KI-5 exists), `cleared_at` and `reset_id`, and the reset becomes one SECURITY DEFINER function plus the route.

## 9. Audit: the closed payload schemas for each new fact

Plan 3 closed the payloads of intelligence's facts (§18 there: every key named, every value a closed-set code, an id of a fixed shape or a bounded integer; anything else refused). Plan 4 applies the same rule to **core's own** new actions, not only intelligence's: `audit.Write` callers pass a typed payload, and a registry `audit/schemas.go` holds one schema per action, checked at write time (a payload that doesn't fit is a programming error: the write fails and the request answers 500, so nothing half-checked reaches the append-only chain). A test fails if any `Action` literal in the code has no schema.

Ids are canonical uuids unless stated. No action carries free text, a name, an email, a diagnosis, a reason note, a cancel note, an outcome note or a search string.

| Action | Written by | Entity / entity_id | Payload (all keys; required in bold) |
|---|---|---|---|
| `feedback.validated` | core, `POST /feedback` | `query` / query id | **`query_id`** |
| `correction.submitted` | core, `POST /feedback` | `correction` / id | **`correction_id`**, **`query_id`** |
| `correction.approved` | core, approve | `correction` / id | **`correction_id`**, **`job_id`** |
| `correction.rejected` | core, reject | `correction` / id | **`correction_id`**, **`reason_code`** ∈ the four codes |
| `correction.withdrawn` | core, withdraw | `correction` / id | **`correction_id`** |
| `correction.decision_refused` | core, approve/reject/withdraw refused | `correction` / id | **`correction_id`**, **`code`** ∈ `self_approval`, `forbidden`, `already_decided`; **`attempted`** ∈ `approve`, `reject`, `withdraw` |
| `correction.applied` | outbox (job `completed`) | `correction` / id | **`correction_id`**, **`attempts`** (int ≤ 2^16), `model_versions` (map ≤ 16, Plan 3 patterns), **`persona_update`** ∈ `skipped`, `applied` |
| `correction.apply_failed` | outbox (job `failed`) | `correction` / id | **`correction_id`**, **`error`** ∈ worker error codes, **`attempts`** |
| `job.dead_lettered`, `job.retried`, `job.cancelled` (Plan 3, widened) | core | `job` / id, or none for the batch rows | `job_type` ∈ vocabulary; `job_count` (int 1–50) for the batch rows; `reason` ∈ the three cancel codes for `job.cancelled` |
| `audit.viewed` | core, first page | `audit_list` / `events` | **`list`** ∈ `events`, `named_access`; `filters`: an object with only `hospital` (uuid or `platform`), `person` (uuid), `type` (list of known types), `outcome`, `flagged` (bool), `from`, `to` (RFC 3339) |
| `audit.searched` | core, exact-id search | `audit_search` / the searched id if it matches the id shape, else none | **`searched_kind`** ∈ `event`, `request` |
| `audit.verify_started` | core | `audit_run` / run id | **`run_id`**, **`by`** ∈ `person`, `schedule` |
| `audit.verify_finished` | core | `audit_run` / run id | **`run_id`**, **`state`** ∈ `intact`, `broken`, `error`, `entries_checked` (int), `first_broken_seq` (int), `error_code` ∈ `timed_out`, `unreachable`, `unexpected_reply` |
| `audit.check_outcome` | core | `audit_run` / run id | **`run_id`** (the note is on the run row only) |
| `audit.undisplayable_reported` | core | none | **`count`** (int 1–50), **`first_id`**, **`last_id`** (event ids) |

Actor, hospital and request id are always set by core from the session or, for outbox facts, from the `jobs` row: never from a payload. The drainer refuses a fact whose action doesn't match the job's status, as Plan 3 does for `run_query`.

## 10. Permissions, roles and the database

### 10.1 Route rows (core's permission table)

| Method and path | Audience | Roles | Rule flags | Calls |
|---|---|---|---|---|
| `POST /feedback` | clinical | `clinician` | `Write` | — |
| `GET /corrections`, `/corrections/mine`, `/corrections/reviewed` | clinical | `clinician` | — | — |
| `POST /corrections/{id}/approve` | clinical | `clinician` | `Write` | `pipeline.approve_correction` |
| `POST /corrections/{id}/reject`, `/withdraw` | clinical | `clinician` | `Write` | — |
| `GET /feedback/stats/mine` | clinical | `clinician` | — | — |
| `GET /platform/audit`, `/{id}`, `/hospitals`, `/people`, `/verify/runs`, `/verify/runs/{id}`, `/verify/status` | platform | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` | — |
| `POST /platform/audit/undisplayable`, `/verify` | platform | `platform_admin`, `auditor` | `Platform`, `NeedsMFA`, **`AuditorWrite`** | — |
| `POST /platform/audit/verify/runs/{id}/outcome` | platform | `auditor` | `Platform`, `NeedsMFA`, **`AuditorWrite`** | — |
| `GET /platform/health`, `/platform/jobs`, `/platform/guardrails` | platform | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` | — |
| `POST /platform/jobs/retry` | platform | `platform_admin` | `Platform`, `NeedsMFA`, `Write` | (re-mints the job's own operation as its creator) |
| `POST /platform/jobs/cancel` | platform | `platform_admin` | `Platform`, `NeedsMFA`, `Write`, `StepUp` | — |

None of these is `bff_direct`. The generated permissions markdown and `operations.json` change with them; `cmd/permgen`'s drift test catches a hand edit.

### 10.2 The operations table

One new row, `pipeline.approve_correction` (§4.7), and one new field, `SecondPerson bool`, validated: only on a `job` operation with a role, and only where core's publisher knows the subject table (a Go map from operation to "subject loader", tested). `submit_feedback`, `ingest_document`, `finalise_document`, `run_triage`, `kb_search` and `feedback_buffer_reset` get **no** rows in Plan 4: a row without a handler would advertise a permission nothing enforces.

### 10.3 Database roles and grants, per table

| Table / object | `mhai_gateway` (clinical requests) | `mhai_worker` (pipeline) | `mhai_platform` (platform requests) | `mhai_jobs` | `mhai_audit_drain` |
|---|---|---|---|---|---|
| `feedback_events` | `SELECT`, `INSERT`, `UPDATE (status, decided_by, decided_at, rejection_code, rejection_note)`; RLS hospital | `SELECT (id, hospital_id, query_id, action, status, submitted_by, decided_by)`; RLS hospital | none | none | none |
| `clinical_decisions` (+ masked, `timings_ms`) | `SELECT` incl. masked columns (one reader) | `INSERT` incl. new columns | none (functions only) | none | none |
| `feedback_buffer` (new) | none | `INSERT` + RESTRICTIVE `buffer_needs_approval` | `EXECUTE mhai_feedback_buffer_waiting()` | none | none |
| `jobs` (new) | `SELECT`, `INSERT`, `UPDATE (published_at)` | `SELECT`, `UPDATE (status, attempts, lease_until, error, completed_at, failed_at)` with `worker_status`, `worker_hospital_active` | none (functions only) | job columns, as 00007 on `clinical_queries` | `SELECT (id, hospital_id, created_by, request_id, status, type)` |
| `intelligence_audit_outbox` (+ `job_id`) | none | `INSERT` (own hospital) | none | none | `SELECT`, `DELETE` |
| `audit_verify_runs` (new) | none | none | `SELECT`, `INSERT`, `UPDATE` | none | none |
| `audit_log` | as today | none | + `INSERT` for the eight platform actions (§5.5) | + `job.retried`, `job.cancelled` | + the two correction actions |
| SECURITY DEFINER functions `mhai_health_timings`, `mhai_health_failures`, `mhai_platform_jobs`, `mhai_guardrail_counts`, `mhai_feedback_buffer_waiting` | — | — | `EXECUTE` (only role) | `mhai_platform_jobs`: no | — |

Every function: owned by `mhai_migrate`, `SET search_path = pg_catalog, pg_temp`, `REVOKE ALL FROM PUBLIC`, returns aggregates or the contract's columns only, parameters bounded inside the function (limits clamped, periods from a closed set). Every new table: `ENABLE` and `FORCE` row-level security, owner `mhai_migrate`, a row in `TestRLSWithoutHospitalSeesNothing` (which TODO says should seed every table: Plan 4 does it for the new ones).

### 10.4 Migrations

Numbered from 00010 (main ends at 00009; no open PR adds a migration as of 3 Nov). If another migration lands first, these renumber; goose orders by number.

| Migration | Contents |
|---|---|
| `00010_feedback.sql` | widen `feedback_events` (§4.3), its CHECKs, the decider and transition triggers, the partial unique index; `clinical_decisions` masked columns and `timings_ms` with key CHECK; grants and policies for `mhai_gateway` and `mhai_worker` |
| `00011_jobs.sql` | `jobs` with RLS and the worker and `mhai_jobs` policies; widen the deadline-expiry function and 00009's cancel-on-deactivate triggers to `jobs`; widen the outbox (`job_id`, actions) and `mhai_audit_drain` |
| `00012_feedback_buffer.sql` | `feedback_buffer`, its RLS, `buffer_needs_approval`, `mhai_feedback_buffer_waiting()` |
| `00013_platform_reads.sql` | `audit_verify_runs`; `mhai_platform` append policy; the four platform SECURITY DEFINER functions |

Each has a full `Down` that removes every grant it made (Plan 3 convention), and the backend deploy's pre-migrate check (rows that would fail a new constraint) gains the new CHECKs on `feedback_events`.

### 10.5 NATS

- The `JOBS` stream already covers `jobs.v1.>`; its per-subject limits apply to `jobs.v1.<hid>.approve_correction` as they do to `run_query`.
- New durable consumer `pipeline_learning` (filter `jobs.v1.*.approve_correction`, explicit ack, `ack_wait` 60 s, `max_deliver` 5, `max_ack_pending` 4, no `BackOff`: Plan 3 departure 4).
- The `pipeline` user gains exactly `$JS.API.CONSUMER.INFO.JOBS.pipeline_learning`, `$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline_learning`, `$JS.ACK.JOBS.pipeline_learning.>`. Test: it still can't publish `jobs.v1.>`, `inference.v1.>` or reach another consumer.
- The health sampler uses a new KV bucket `health` (one key, history 1, TTL 5 minutes), written and read by `gateway` only.
- `dead.v1.<hid>.approve_correction` is handled by Plan 3's dead-letter consumer, which learns the `jobs` table.

### 10.6 What intelligence's side needs, and what it must not do

Within the boundary rules (Python writes no permission rule; hospital and user only from the token):

- `proto/mhai/intelligence/v1/learning.proto`: `ApproveCorrectionJob {job_id, correction_id}`; regenerated Go and Python; golden messages both ways.
- `boundary/learning.py`: `LearningClient` Protocol (`buffer_entry`, `persona_update`, `model_versions`), `TrustBufferLearningClient` (a pure function over the four scores and the decision, the same arithmetic `TrustWeightOptimizer.add_correction` stores) and `StubLearningClient`. Built by `build_learning_client(settings)`; the stub is refused in production, as `INTELLIGENCE_MODEL_CLIENT=stub` is (Plan 3 ruling F14).
- `boundary/jobs/approve_correction.py` on the `run_query` skeleton (claim, lease, `+WIP` if ever needed, complete once, `Nak`/`Term`, dead letter, cancellation check before writing).
- `boundary/learning_repo.py`: the job's SQL, allow-listed in the no-direct-DB test, as `jobs_repo.py` is.
- `boundary/jobs/run_query.py`: write `masked_question`, `masked_patient_json` and `timings_ms` in the completion transaction (already computed; one change to the insert).
- `services/pipeline.py`: bind the second consumer; refuse to start if `operations.json` lacks `pipeline.approve_correction` (it already refuses a handler with no entry).
- **Must not:** check who approved, read `reason` or `corrected_diagnosis`, call `DoctorFeedbackLoop`, write `kb_chunks` or `guardrail_weights`, or decide anything from `SecondPerson` (core's flag). The intelligence lead keeps model internals (persona vectors, CMA-ES runs) outside the boundary until open questions 3 and 4 are decided.

## 11. Security and threat model

### 11.1 Assets

- **Clinical safety and the guardrails:** a correction must not change what any clinician is shown until a different clinician has approved it (ADR 0005), and never another hospital's guardrails.
- **Patient data:** the consultation and answer shown to a reviewer; the clinician's free-text reason and corrected diagnosis.
- **Hospital separation:** corrections, buffer rows and jobs of one hospital invisible to another; platform roles see aggregates and metadata only.
- **The audit chain** and its verification result: auditors rely on "Intact" meaning intact.
- **Availability** of the API under health and statistics polling.

### 11.2 Actors and trust boundaries

Actors: clinicians (submitter, reviewer), hospital admins (non-clinician), platform admins, auditors, the clinical and platform BFFs, core and its background loops (sampler, scheduler, sweeper, drainer, dead-letter consumer), the pipeline worker, NATS, Postgres; and an attacker with one stolen clinical or platform session, a compromised worker, or a malicious insider clinician.

| Crossing | What is checked there |
|---|---|
| BFF → core (`api.v1.clinical`, `api.v1.platform`) | Plan 2/3 unchanged: the BFF's NATS identity, access token, denylist, account and hospital state, route rule (roles, platform audience, two-step, `Write`/`AuditorWrite`, step-up), idempotency, RLS transaction, audit |
| core → Postgres (clinical) | `mhai_gateway`, hospital from the token, `mhai.user_id` from the token; column grants; the two-person CHECK and triggers |
| core → Postgres (platform) | `mhai_platform`, no hospital; no row grant on clinical tables; aggregates through SECURITY DEFINER functions |
| core → JetStream | only `gateway` publishes `jobs.v1.>`; the token binds subject, body, hospital and `sub`; `SecondPerson` checked before minting |
| JetStream → worker | every Plan 3 check (§7.3 there); operation row for `approve_correction` |
| worker → Postgres | `with_hospital`; claim; column grants without the free text; RESTRICTIVE `buffer_needs_approval` |
| outbox → chain | the drainer takes actor, hospital and request id from `jobs`; closed payload schemas; action must match job status |
| core loops → workers (`pipeline.health`) | `svc:core` request token (Plan 3) |

### 11.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test (§11.5) |
|---|---|---|---|---|
| P1 | EoP (clinical) | **A clinician approves their own correction** (directly, by a raced request, by a forged body, or with a second session) | identity from the token only; the handler refuses `self_approval`; the conditional `UPDATE ... AND submitted_by <> $me`; `CHECK (decided_by <> submitted_by)`; `SecondPerson` before minting; the buffer's RESTRICTIVE policy | C1–C4 |
| P2 | EoP | **A non-clinician** (hospital admin, analyst, platform admin) approves or rejects | route role `clinician`; operation role `clinician`; the decider trigger checks the `clinician` role, active, same hospital at decision time | C5, C6 |
| P3 | EoP | A reviewer from **another hospital** decides, or reads the queue | RLS (another hospital's correction is invisible: 404); the decider trigger's same-hospital check | C7 |
| P4 | Tampering | A **compromised or buggy worker** puts an unapproved correction, or another hospital's, into the buffer | the worker has no write on `feedback_events`; `buffer_needs_approval`; `with_hospital` RLS; unique `correction_id` | C8, C9 |
| P5 | Tampering (clinical) | **Feedback poisoning:** one clinician (or two colluding) floods corrections to steer the guardrails, including other hospitals' | nothing in Plan 4 changes `guardrail_weights` or the persona; one live feedback per query and only on one's own completed queries; every correction needs a second clinician; buffer rows per hospital with `model_versions`; any later CMA-ES result is a proposal under clinical change control (open question 3) | C10, C11 |
| P6 | Tampering | **A stale decision:** approve after withdraw, approve twice, approve and reject at once | conditional `UPDATE ... WHERE status='pending_review'`; transition trigger; 409 `already_decided`; one live job per subject | C12, C13 |
| P7 | Information disclosure | A reviewer sees the **unmasked** question or patient details | the reviewer's view reads only `masked_question` and `masked_patient_json`; a test that no other handler selects them and that the corrections reader never selects `clinical_queries.clinical_question`/`patient_data_json` | C14, C15 |
| P8 | Information disclosure | Free-text **reason, diagnosis, rejection note, cancel note or outcome note** reaches logs, traces, the audit chain, NATS or the worker | closed payload schemas (§9); worker column grants exclude the text; job bodies hold ids; `logx` redaction; marker-string test across the flow | C16 |
| P9 | Information disclosure | A platform admin or auditor reaches **clinical content** through the new platform routes | `mhai_platform` has no row grant on clinical tables; SECURITY DEFINER functions return aggregates or contract columns only; jobs list never shows subject, user or message | C17, C18 |
| P10 | Information disclosure | **Small-group leaks** in guardrail statistics: a 1–4 count, a share from a small numerator, or subtraction across `all` and shown hospitals | §6.3 rules applied in Go before encoding; secondary suppression; property test over random tables that no hidden value can be derived from shown ones | C19, C20 |
| P11 | Information disclosure | The audit reader sends a value the contract doesn't know (an unmapped payload key), a pseudonym, or free text | the catalogue maps every action; unmapped actions send no payload values; pseudonym-shape test; vocabulary test against mhai-web's lists | C21, C22 |
| P12 | Repudiation | Who submitted, approved, rejected, withdrew, retried, cancelled, viewed or searched the audit log can't be shown | §9's actions, each in the same transaction as the change (or from the outbox for job facts); `audit.viewed`/`searched` for reads; refusals audited | C23 |
| P13 | Repudiation / Tampering | **A false "Intact"**: a run that didn't walk the whole chain reported intact, or two runs racing | `intact` only with the proof fields; timeout is `error`; one running run (partial unique index); advisory-lock scheduler | C24, C25 |
| P14 | Tampering | Auditors' new writes widen into other writes | `AuditorWrite` allowed only under `/platform/audit/`, never with step-up; a test pins the three routes; auditors still get 403 on every other write | C26 |
| P15 | EoP | **Retry re-mints as someone else**, or for a user who lost the role or was deactivated, or re-runs an approval that no longer has a second person | `intel.Republish` mints as the job's creator with current roles, after checking user and hospital are active and the role still allows the operation; `SecondPerson` re-checked; else `skipped` | C27, C28 |
| P16 | Tampering | Cancel of a running or completed job, or a cancel without step-up | `mhai_jobs` policies (running and completed untouchable); `StepUp` on cancel | C29 |
| P17 | DoS | **Polling** `/platform/health`, `/platform/guardrails` or the audit list as an expensive query | health from the sampler's stored reading; guardrails cached 60 s and rate-limited; audit list keyset-paged, `limit` ≤ 100, `total` capped at 10,000, indexed filters | C30, C31 |
| P18 | DoS | A **flood of approvals** or retries fills `JOBS` | 20 `approve_correction` jobs in flight per hospital (the decision still commits; the job waits); `JOBS` per-subject limits; retry 1–50 ids per call and per-session rate limit | C32 |
| P19 | DoS | A verify run that never ends blocks all later checks | 10-minute budget; the run becomes `error`; the partial unique index frees | C25 |
| P20 | Spoofing | A cursor or job id forged to read other filters or another table's rows | cursors bind a hash of the filters; job ids resolved only through `mhai_platform_jobs` by type; unknown ids `skipped` | C33 |

### 11.4 Controls (decided)

- **Authentication and step-up:** Plan 2 sessions; two-step always on platform routes; step-up (`mfa_code` or `backup_code`) on job cancel, as the contract says. Approving a correction needs no step-up (the contract doesn't ask for one; the 60-second undo is the safeguard).
- **Authorisation:** core's route table (§10.1) and operations table (§10.2) are the only rules; the two-person rule is enforced by core's handler, core's publisher (`SecondPerson`) and the database (CHECK, triggers, RESTRICTIVE policy). Python holds no rule. RLS on every new table, with the hospital from the token; platform reads only through aggregate functions.
- **Data handling:** free text stays in `feedback_events`, under RLS, readable by the hospital's clinicians through the corrections routes only; never logged, traced, audited, published or given to the worker. Reviewers see the masked consultation only. Retention of corrections follows the contract's `pending_corrections` period when the retention plan builds it (TODO).
- **Concurrency:** every decision is one conditional `UPDATE`; one live feedback per query and one live job per subject by partial unique indexes; job claim and completion as Plan 3; one verify run by a partial unique index; schedulers by advisory lock.
- **Secrets:** none new. The `health` KV bucket holds no secret and no patient data.
- **Third parties:** none. The audit-integrity email waits for the notifications service and a mail provider (TODO).

### 11.5 Abuse-case tests (the plan writes these first)

- **C1** A clinician approving their own correction gets 403 `self_approval`, an audit row `correction.decision_refused`, and no job.
- **C2** As the database login, `UPDATE feedback_events SET status='approved', decided_by=submitted_by ...` fails the CHECK, whatever the role.
- **C3** Two sessions of the submitter and one of a reviewer approve in parallel: exactly one approval, by the reviewer; the others get 403 or 409.
- **C4** A forged approve body naming another `decided_by` is ignored; `decided_by` is the token's user.
- **C5** A hospital admin without `clinician`, an analyst, a platform admin and an auditor each get 403 on approve, reject and the pending list.
- **C6** A clinician whose `clinician` role is removed (or who is deactivated) between loading the queue and approving: refused (the decider trigger), 409 `conflict` or 401 per Plan 2, and nothing published.
- **C7** A clinician of hospital B gets 404 on hospital A's correction id for every corrections route.
- **C8** As `mhai_worker` under hospital A, inserting a `feedback_buffer` row for a `pending_review`, `rejected` or self-approved (forced as superuser) correction fails the RESTRICTIVE policy.
- **C9** An `approve_correction` job for hospital A naming hospital B's correction: `not_found`, dead letter, no read of B's rows (database log), no buffer row.
- **C10** No Plan 4 code path writes `guardrail_weights` or `kb_chunks`: a grant test (`mhai_worker`, `mhai_gateway` have no write) and a grep test in the boundary package.
- **C11** A second `POST /feedback` for the same query gives 409; a feedback on another clinician's query gives 404.
- **C12** Withdraw then approve: 409 `already_decided`; approve then withdraw: 409.
- **C13** The same approval replayed with the same `Idempotency-Key` returns 204 once and publishes one job; with a new key, 409.
- **C14** The pending list for a correction whose stored question contains a synthetic name and MRN shows only the masked text.
- **C15** A Go test fails if any handler other than the corrections reader selects `masked_question` or `masked_patient_json`, or if the corrections reader selects `clinical_queries.clinical_question` or `patient_data_json`.
- **C16** A synthetic marker in `reason`, `corrected_diagnosis`, `rejection.note`, a cancel note and an outcome note appears in no log line, span, audit payload, NATS message or worker-readable column.
- **C17** As `mhai_platform`, `SELECT` on `clinical_queries`, `clinical_decisions`, `feedback_events`, `feedback_buffer`, `jobs` fails; the functions return only their declared columns.
- **C18** `GET /platform/jobs` items carry exactly the contract's keys (no subject, user or message).
- **C19** Guardrail tables with counts 0–6 in every combination: no value 1–4 is sent; no set has exactly one hidden member; shares follow rule 3.
- **C20** Property test: for random per-hospital tables, no hidden cell can be computed from the response (row sums, `all` minus shown rows).
- **C21** Every audit action in the code has a catalogue row and a payload schema; every emitted `type`, `kind` and `facts` key is in mhai-web's vocabulary file.
- **C22** No audit response anywhere contains a string matching the pseudonym shape.
- **C23** Each action in §9 is written once, in the same transaction as its change (a forced failure after the change rolls both back); outbox facts reach the chain after a worker crash post-commit.
- **C24** A verify run stopped by its budget ends `error`, never `intact`; an `intact` run has every proof field.
- **C25** Two `POST /verify` at once return the same `run_id`; the 06:00 schedule on two core copies starts one run.
- **C26** An auditor gets 403 on `POST /platform/jobs/retry` and every other write outside the three audit routes; a route with `AuditorWrite` outside `/platform/audit/` panics at registration.
- **C27** Retry of a job whose creator was deactivated, or lost `clinician`: `skipped`, nothing published.
- **C28** Retry of an `approve_correction` whose correction was forced (as superuser) to `decided_by = submitted_by`: refused by `SecondPerson`, `skipped`.
- **C29** Cancel without a code: 403 `mfa_required`/`mfa_invalid`; cancel of a `running` job: `skipped`.
- **C30** 100 parallel `GET /platform/health` cause no database query and no `pipeline.health` call beyond the sampler's.
- **C31** `GET /platform/guardrails` past 6 per minute per session: 429 `rate_limited`.
- **C32** A 21st approval in flight for one hospital still returns 204 and the job is published by the sweeper when a slot frees.
- **C33** A cursor reused with other filters is 422; a job id from neither table is `skipped`.

### 11.6 Residual risk and sign-off

- **Two accounts, one person.** The database can tell two accounts apart, not two people. A clinician with two accounts at one hospital could approve their own correction. Accepted: accounts are made by hospital admins (Plan 2 provisioning), and nothing a correction does in Plan 4 changes any answer.
- **Collusion between two clinicians** passes the two-person rule. Accepted for the same reason; the buffer is never applied automatically (open question 3), and the audit chain names both.
- **The masker misses an identifier** in the reviewer's view: a peer clinician of the same hospital sees it. Accepted: within-hospital peer review is normal practice, and the reviewer is a clinician bound by the same duty.
- **Audit tail deletion** is still undetectable until signed checkpoints exist (TODO). "Intact" means "no break in what is there".
- **No bell or email on a broken chain** until the notifications service exists; an alert covers it.
- **Sign-off needed:** clinical: that correction review needs no step-up, and open questions 3 and 4 (any change to live guardrails). Privacy: the masked consultation shown to peers (§4.4), and the retention period for corrections. Product owner: every open question in §15.

## 12. Phasing

Plan 4 ships in three parts, each mergeable on its own (one PR each, per the agent workflow):

| Part | Ships | Depends on |
|---|---|---|
| **4a. Feedback and corrections** | 00010–00012, the corrections routes, `POST /feedback`, stats, the `jobs` table, `approve_correction` end to end, the outbox widening, masked consultation and `timings_ms` in `run_query` | Plan 3 (merged) |
| **4b. The audit log** | 00013 (runs table, platform append), `/platform/audit/*` (Plan 4 subset), the catalogue, verify runs and the schedule, `AuditorWrite` | 4a for the new actions' catalogue rows (can start in parallel; the catalogue test pins whatever is merged) |
| **4c. Platform operations** | `/platform/health` and the sampler, `/platform/jobs` with retry and cancel, `/platform/guardrails` and `internal/smallgroup` | 4a (the `jobs` table) |

## 13. Changes outside this design's code

- **Frontend contract** (`docs/specs/2026-10-17-frontend-requirements.md`): §4 rows to **Built (Plan 4)**; `POST /feedback`'s `client_created_at` and `Idempotency-Key` to Built; §14.8.2, §14.8.3 and the Plan 4 subset of §14.9 to Built, with the deferred §14.9 rows named; a new **New** row for `GET /platform/guardrails`; a note that `/platform/audit` is the `/security/audit` of the gateway design.
- **mhai-web** (its TODO, not this repository): add the audit kinds marked new in §5.3 before 4b deploys; handle a missing `documents` and `clean_up` health part if open question 7 is decided as recommended; a screen for guardrail statistics when wanted.
- **TODO.md:** remove "Platform job views"; add the deferred items (document pipeline plan, triage, KB search, buffer reset, audit exports and named access, the audit bell and email, CMA-ES proposals, persona capture); keep "Audit tail deletion".
- **ARCHITECTURE.md:** "Built (Plan 4)" lines; the `jobs` table and the second consumer in the jobs flow.
- **Tracker:** B-14, B-20, B-21, B-22 to their Plan 4 parts; B-17, B-18, B-19, B-23, KI-5 to "deferred" with the plan named.

## 14. Task breakdown for the plan

In the style of Plan 3: each task starts with its abuse-case tests (§11.5), runs on the remote build hosts only, and ends with the `secure-build` checklist and a `security-review` of its own diff. The plan expands each into files, interfaces and checkbox steps.

| # | Part | Task | Main tests |
|---|---|---|---|
| 1 | 4a | **Migration 00010: feedback.** Widen `feedback_events`; CHECKs; decider and transition triggers; partial unique index; `mhai.user_id` local setting in `db.WithHospital`; masked columns and `timings_ms` on `clinical_decisions`; grants | C2, C6 (trigger part), C8 setup, RLS seeding |
| 2 | 4a | **Migration 00011: the `jobs` table.** RLS, worker and `mhai_jobs` policies, deadline expiry and cancel-on-deactivate widened, outbox `job_id` and actions, drainer grants | the 00008/00009 policy tests repeated for `jobs`; A21-style cancel on deactivation |
| 3 | 4a | **Migration 00012: `feedback_buffer`.** RLS, `buffer_needs_approval`, `mhai_feedback_buffer_waiting()` | C8, C10 (grants) |
| 4 | 4a | **The operations table and the contract.** `pipeline.approve_correction`; the `SecondPerson` field and its subject loader; `learning.proto`; `make proto`; `operations.json`; problem codes `self_approval`, `already_decided`; replace the Plan 3 feedback-operation test | C28 (unit), drift test, golden messages |
| 5 | 4a | **Core: `POST /feedback` and the corrections routes.** Handlers, `CorrectionOut` from the masked consultation, guards in order, audit actions with schemas (`audit/schemas.go`) | C1, C3–C5, C7, C11–C15 |
| 6 | 4a | **Core: approval publishes the job.** One transaction (decision, `jobs` row, audit), publish after commit, sweeper and dead-letter consumer and drainer widened to `jobs`, the per-hospital approval budget | C3, C13, C23, C32 |
| 7 | 4a | **Python: `approve_correction`.** `learning.py` (`LearningClient`, stub refused in production), `learning_repo.py`, the job handler, the `pipeline_learning` consumer, NATS permissions for it; `run_query` writes masked text and `timings_ms` | C8, C9, C16 (worker side), A4/A13/A20 equivalents for the new job |
| 8 | 4b | **Migration 00013 and the audit catalogue.** `audit_verify_runs`; `mhai_platform` append policy; `audit/catalogue.go`; the vocabulary file and its tests | C21, C22 |
| 9 | 4b | **Core: `/platform/audit` reads.** List (filters, keyset cursor, `total` cap), detail, hospitals, people; `audit.viewed` and `audit.searched` with KV dedupe; 404 for the deferred routes | C17, C22, C33 |
| 10 | 4b | **Core: verification.** `audit.Verify` reporting progress; runs; scheduler with advisory lock; `POST /verify`, runs, status, outcome; `AuditorWrite`; undisplayable reports; the broken-chain alert | C24–C26 |
| 11 | 4c | **Core: health.** The sampler, the `health` KV bucket, the SECURITY DEFINER aggregates, `GET /platform/health` | C30, staleness 503 |
| 12 | 4c | **Core: failed jobs.** `mhai_platform_jobs()`, list, retry (Republish with checks), cancel (step-up), batch audit rows | C18, C27–C29, C33 |
| 13 | 4c | **Core: guardrail statistics.** `internal/smallgroup` (rules, property tests), `mhai_guardrail_counts()`, the route, cache and rate limit | C19, C20, C31 |
| 14 | all | **The smoke test, docs and contract.** Smoke: a clinician asks, a second clinician approves, the buffer row and `correction.applied` appear, the chain verifies `intact` through `POST /platform/audit/verify`; `openapi.yaml` (minor version bump); README, ARCHITECTURE, TODO, frontend contract statuses | the smoke test; `oasdiff` additive |

**Order:** 1 → 2 → 3 (one migration chain). 4 needs nothing. 5 needs 1. 6 needs 2, 4, 5. 7 needs 2–4. 8 can start after 2 (actions settle with 5–7; its test pins the merged set). 9 and 10 need 8. 11 and 12 need 2. 13 needs 1 (`timings_ms` not needed; the counts are). 14 last in each part for that part's slice.

**Review focus for the plan** (where a reviewer should look hardest):
1. The two-person rule in all four places (handler, publisher, CHECK/triggers, buffer policy), and that no path writes `decided_by` from anything but the token (Tasks 1, 4, 5, 6).
2. The reviewer's view reads masked text only (Task 5).
3. `mhai_platform` never gains a row grant on a clinical table (Tasks 8, 11–13).
4. The small-group rules, including subtraction across `all` (Task 13).
5. `intact` means the whole range was walked (Task 10).

## 15. Open questions for the product owner

Each has a recommendation; "approve as recommended" decides them all.

1. **Serve the audit log as `/platform/audit/*` only, or also as `/security/audit`?**
   *Recommendation:* `/platform/audit/*` only. It is what the built platform app calls, over the same log; a second path would be a second thing to secure for no caller. The tracker row B-22 is marked done by it.
2. **Build a `submit_feedback` job now?**
   *Recommendation:* no; reserve the name. Submitting needs no model, and ADR 0005 says it changes nothing until approval. Add the job when submission needs model work.
3. **What may a full CMA-ES buffer change?** Today's Python retunes the global trust weights after 50 corrections, automatically, for every hospital.
   *Recommendation:* in Plan 4, nothing: the buffer accumulates only. Later (a plan with the intelligence lead), a CMA-ES run produces a *proposed* weight set, evaluated offline on the synthetic set and applied only through clinical change control (a `clinical-review` PR, as model pins are), never automatically and never from one hospital's corrections alone.
4. **Persona vector updates?** They need the model's activations for the wrong and corrected answers, which the pipeline doesn't capture.
   *Recommendation:* defer to the intelligence lead (model internals are his): Plan 4 records `persona_update: skipped` and keeps every approved correction's ids, so a later capture can be back-filled for new answers only. Like question 3, any change to live behaviour goes through clinical change control.
5. **Who may review a correction?**
   *Recommendation:* any active clinician of the same hospital other than the submitter, until the hospital admin plan adds `can_review` and `review_specialties` (contract §5.1); then the decider trigger checks those too. `reviewers_share_specialty` stays off by default (contract §4: "every clinician sees the whole queue").
6. **Does approving a correction need the two-step code again?**
   *Recommendation:* no. The contract doesn't ask for it, the app has a 60-second undo, and in Plan 4 an approval changes no answer. Revisit when approvals start changing live guardrails (question 3).
7. **Health parts that don't exist yet (`documents`, `clean_up`) and `clean_up` block:** send them as `problem`, as `ok`, or leave them out?
   *Recommendation:* leave them out and ask mhai-web to accept a missing part (a one-line change there), because `ok` would be false and `problem` would raise an alarm about something that doesn't exist. If mhai-web prefers not to change, send `problem` with `since` null.
8. **Who may read health, failed jobs and guardrail statistics?** The tracker says "admin"; the contract lets auditors read every §14.8 `GET`.
   *Recommendation:* platform admins and auditors read; only platform admins retry and cancel; hospital roles never.
9. **Document upload in Plan 4?**
   *Recommendation:* no (§7). Start the document pipeline plan with phase 0 (evaluation harness, model store, GPU host) and the documents service, then upload and `ingest_document` together, as the approved design phases them.
10. **Triage, KB search and the feedback buffer reset?**
    *Recommendation:* defer all three (§8): triage until QDA passes evaluation and clinical sign-off; KB search until a screen needs it; the reset into the platform settings plan, which Plan 4 makes small.
11. **The migration that changes `feedback_events`:** it renames `clinician_id` and drops three unused columns.
    *Recommendation:* accept; the table is empty outside tests (synthetic data only), and the deploy's pre-migrate check will stop if it isn't.
