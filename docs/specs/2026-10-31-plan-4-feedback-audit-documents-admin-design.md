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
| `POST /platform/audit/undisplayable {ids}` | yes | 1–50 ids → 204, written as one `audit.undisplayable_reported` entry with `facts.job_count`-style count (`count`) and no ids in free form. **Auditor write.** |
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

- **A sampler, not a per-request computation.** A core loop (advisory-lock leader, every 30 s) takes a reading and stores it in memory and in NATS KV (`health` bucket, one key); the route returns the stored reading with its own `updated_at`. A poll costs one KV get, so a polling client can't make core run the queries or call the workers (§11, T-D2). The reading is 503 when it is older than 2 minutes or missing.
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
- **`GET /kb/search`: deferred.** No screen in either app calls it. If one does, it is a request/reply operation `pipeline.kb_search`, core-initiated (ADR 0011 recommends the core path for anything touching patient data), shared guidelines only until patient ids exist (ADR 0004: with no patient, only guidelines), with the shared replay check (boundary decision 3 applies only to writes, so not needed), a rate limit and an audit row of the query's hash, never its text.
- **Reset the feedback buffer: deferred to the platform settings plan.** It is `POST /platform/settings/feedback-buffer/reset`, shown inside `GET /platform/settings` together with retention and triage, none of which exist. Plan 4 never applies the buffer to the live guardrails (§4.8), so nothing has been learned that a reset would need to undo. Plan 4 makes the reset cheap later: buffer rows carry `model_versions` (so a reset can clear exactly the rows scored by a broken NLI model, the reason KI-5 exists), `cleared_at` and `reset_id`, and the reset becomes one SECURITY DEFINER function plus the route.
