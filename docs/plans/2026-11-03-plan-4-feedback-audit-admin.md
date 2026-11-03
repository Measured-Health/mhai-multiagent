# Plan 4: feedback and corrections, the audit log, platform operations: implementation plan

**Status:** Not started. Design approved by the product owner on 2026-11-03 ("yes"), with all eleven decisions in its §15 taken as recommended.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Let a clinician agree with an answer or suggest a correction, and let a *different* clinician of the same hospital approve or reject it. An approved correction becomes one row in a per-hospital learning buffer, written by intelligence through a job. Nothing changes the live guardrails. Give auditors and platform admins the audit log with chain verification. Give platform admins system health, the failed-jobs inspector (retry, cancel) and guardrail statistics with the small-group rules.

**Architecture:** Every endpoint is a core route behind the Plan 2 chain, reached over `api.v1.<audience>` (ADR 0011). One new job, `approve_correction`, goes to the `pipeline` worker. It follows the `run_query` path: published only by core, a request token, a claim, completion at most once, an outbox fact and dead letters. The two-person rule is enforced in core's handler, in core's publisher (a new `SecondPerson` operation flag) and in Postgres (a CHECK, triggers, and a RESTRICTIVE policy on the buffer). Python holds no rule. New job types get one generic `jobs` table, so the document pipeline plan can add `ingest_document` without a new table. Platform routes read clinical tables only through SECURITY DEFINER functions that return aggregates.

**Tech stack:** as Plan 3. Go 1.24 (`net/http`, `pgx/v5`, `goose/v3`, `nats.go`, `testcontainers-go`, `google.golang.org/protobuf`, OpenTelemetry). Python 3.12 with uv (`nats-py`, `psycopg` 3, `protobuf`). `buf` for the schema. No new dependencies.

**Spec:** `docs/specs/2026-10-31-plan-4-feedback-audit-documents-admin-design.md` (approved 2026-11-03). Section numbers below (§n) are the spec's. It builds on the intelligence boundary design as built (its §18) and Plan 3 (`docs/plans/2026-10-28-plan-3-intelligence-boundary.md`).

**The contract:**
- `docs/specs/2026-10-17-frontend-requirements.md` §4 (corrections), §8 (retry-safe writes), §14.6, §14.7 (small-group rules), §14.8.2 and §14.8.3 (health, failed jobs) and §14.9 (the audit log).
- mhai-web's `apps/clinical/lib/api/contract.ts`: `FeedbackIn`, `FeedbackOut`, `CorrectionOut`, `RejectIn`, `FeedbackStatsOut`.
- mhai-web's `apps/platform/lib/api/audit-contract.ts` and `ops-contract.ts`, and `apps/platform/lib/audit/kinds.ts` (the event vocabulary).

**Phases:** three PRs, one per part, each mergeable on its own:
- **4a, feedback and corrections:** Tasks 1–7, then Task 14's 4a slice.
- **4b, the audit log:** Tasks 8–10, then Task 14's 4b slice.
- **4c, platform operations:** Tasks 11–13, then Task 14's 4c slice.

4b and 4c need 4a's migrations merged first, because the migration chain is linear.

## Global Constraints

Plan 2 and Plan 3's constraints still hold:
- problem bodies carry a fixed code only;
- 404 for another hospital's resource;
- identity only from the verified token;
- no PHI or secrets in logs, spans, audit payloads or NATS messages;
- synthetic data only;
- plain commit messages with no AI attribution;
- remote runs only.

In addition:

- **The two-person rule has four guards, and all four must hold:**
  1. the handler (`self_approval`);
  2. the conditional `UPDATE ... AND submitted_by <> $me`;
  3. the database (`CHECK (decided_by <> submitted_by)`, the decider and transition triggers);
  4. the publisher (`SecondPerson`), plus `buffer_needs_approval` on the buffer.

  No task removes or weakens one because another exists.
- **`decided_by` comes only from the token.** No request body field, header or job field can set it.
- **Free text stays in `feedback_events`:** `reason`, `corrected_diagnosis`, `rejection_note`, `jobs.cancel_note` and `audit_verify_runs.outcome_note`.
  - None of it is logged, traced, audited, published on NATS, readable by `mhai_worker` or returned on a platform route.
  - The reason and the corrected diagnosis are shown only on the clinical corrections routes. The rejection note goes only to the submitter and the reviewer.
- **Reviewers see masked text only:** `clinical_decisions.masked_question` and `masked_patient_json`. They never see `clinical_queries.clinical_question` or `patient_data_json`.
- **Python writes no permission rule.** The worker reads `operations.json` and never checks who approved. `SecondPerson` is core's flag; Python carries it as data and never acts on it.
- **Nothing in Plan 4 writes `guardrail_weights`, `kb_chunks` or a persona vector** (decisions 3 and 4).
- **Platform roles get no row grant on a clinical table.** `mhai_platform` reads clinical data only through the five SECURITY DEFINER functions (§10.3), which return aggregates or the contract's columns.
- **Small-group rules** (§6.3) are applied in Go (`internal/smallgroup`) before encoding, so a raw count of 1–4 never reaches a response writer.
- **Every new audit action has a closed payload schema** (§9), registered in `internal/audit/schemas.go`. A payload that doesn't fit is a programming error: the write fails and the transaction rolls back.
- **New problem codes**, additive under ADR 0012: 403 `self_approval` and 409 `already_decided`.
- **Migrations are 00011–00014.** PR #82 claims 00010; if it never merges, the gap is harmless. Each migration has a full `Down`. Every new table has `ENABLE` and `FORCE` row-level security, owner `mhai_migrate`, and a row in `TestRLSWithoutHospitalSeesNothing`.
- **Every write route takes `Idempotency-Key`.** Platform writes are audited once per call: one `job.retried` or `job.cancelled` row with `job_count`, not one row per id.
- **`AuditorWrite`** is allowed only under `/platform/audit/`, never with `StepUp`, and only on the three routes a test pins.
- **Remote runs only.** No build, test, container or compose run on the laptop.

## Review Focus

1. **The two-person rule, end to end.**
   - A submitter can't approve their own correction by any route: same session, second session, raced requests, a forged body, a retry, or a job re-mint.
   - A non-clinician can't approve or reject at all.
   - Tested in Tasks 1, 4, 5, 6 and 12 (C1–C6, C28).
2. **The reviewer's view is masked.** Tested in Task 5 (C14, C15) and Task 7 (the worker writes the masked copy).
3. **The worker can't put an unapproved correction in the buffer,** even as a compromised worker. Tested in Tasks 3 and 7 (C8, C9).
4. **Platform routes never reach clinical rows.** Tested in Tasks 8, 11, 12 and 13 (C17, C18).
5. **The small-group rules,** including subtraction across `all`. Tested in Task 13 (C19, C20).
6. **"Intact" means the whole chain was walked.** Tested in Task 10 (C24, C25).
7. **The audit catalogue sends nothing unmapped,** and nothing shaped like a pseudonym. Tested in Task 8 (C21, C22).

## Where this plan departs from the spec

1. **`db.WithHospital` gains a user.** The transition trigger (§4.3 item 4) needs the session's user in a local setting. Today `db.WithHospital(ctx, pool, hid, fn)` sets only the hospital.
   - Task 1 adds `db.WithHospitalUser(ctx, pool, hid, uid, fn)`, which also runs `set_config('mhai.user_id', uid, true)`.
   - The corrections handlers use it; every other caller is unchanged.
   - `mhai_current_user()` returns null when the setting is unset, so a trigger that needs it fails closed.
2. **The masked consultation and `timings_ms` come from a change to Plan 3's `run_query` worker** (Task 7). The spec puts them in the migration only.
3. **The health `queue` block reads `intel.QueueStats`** (Plan 3), extended to a per-consumer pair. The spec says "JetStream consumer info"; `QueueStats` already wraps it.
4. **Job ids in the Failed jobs list** are `q_<uuid>` for `run_query` rows and `j_<uuid>` for `jobs` rows. These are opaque to the app and match `[A-Za-z0-9_-]+`, so one call can carry both kinds and core resolves the table by the prefix. The spec says "the `type` tells core which table".

## Running the tests

Never on this machine. Go tests, testcontainers, Docker builds and compose all run on the steven-reid build server (`steven-reid@75.46.59.136`). mt5-lab is the fallback (`REMOTE_HOST`/`REMOTE_KEY`); there the runner caps the job at 2 CPUs and runs one job at a time. GitHub Actions runs the full gate as confirmation.

**Go** uses `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`.
- It rsyncs the checkout (without `.venv`, `.superpowers`, `__pycache__` or `data`) to `~/claude-work/<DEST>/`.
- It runs the commands from stdin in `/work` inside `golang:1.24`, with the host's Docker socket for testcontainers and persistent module and build caches.
- Put each step on its own line ending `|| exit 1`, and wrap the run in `timeout`:

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-plan4/ DEST=mhai-plan4 \
  timeout 1800 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/core || exit 1
go vet ./... || exit 1
go test -race ./internal/authz/... ./internal/audit/... ./internal/smallgroup/... || exit 1
go test -race -tags integration ./internal/db/... ./internal/feedback/... || exit 1
CMDS
```

In the task steps below, "**Run (Go, remote):** `<cmd>`" means put `<cmd>` in that heredoc after `cd services/core`.

**Python** runs remotely too, with uv, through the same script. uv's caches go in the persistent module-cache volume:

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-plan4/ DEST=mhai-plan4 \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
export UV_CACHE_DIR=/go/pkg/mod/cache/uv UV_PYTHON_INSTALL_DIR=/go/pkg/mod/cache/uv-python
curl -LsSf https://astral.sh/uv/0.12.23/install.sh | sh || exit 1
export PATH=/root/.local/bin:$PATH
docker build -t mhai-core:test services/core || exit 1
cd services/intelligence || exit 1
uv sync --locked || exit 1
uv run pytest -q -m boundary tests/boundary/test_approve_correction.py || exit 1
CMDS
```

"**Run (Python, remote):** `<cmd>`" means put `<cmd>` in that heredoc after `uv sync --locked`. Tests marked `boundary` start Postgres 16 and NATS 2.10 with testcontainers and apply core's migrations from `mhai-core:test migrate up`. They never load models.

**Proto:** `make proto` and `make proto-check` run in the same container after installing the pinned `buf` and `protoc-gen-go` (see the Makefile).

**The smoke test:** `make smoke` (`services/core/internal/smoke`) runs remotely and in CI as `boundary-smoke`.

---

## File structure

```
proto/mhai/intelligence/v1/learning.proto          ApproveCorrectionJob {job_id, correction_id}
testdata/web-audit-vocabulary.json                 mhai-web's audit types, kinds and facts keys (vendored by hand)
services/core/
  internal/db/db.go                                + WithHospitalUser
  internal/db/migrations/00011_feedback.sql        feedback_events widened, triggers, masked columns, timings_ms
  internal/db/migrations/00012_jobs.sql            jobs, policies, outbox job_id, drainer, expiry, cancel-on-deactivate
  internal/db/migrations/00013_feedback_buffer.sql feedback_buffer, buffer_needs_approval, waiting()
  internal/db/migrations/00014_platform_reads.sql  audit_verify_runs, platform append, SECURITY DEFINER aggregates
  internal/db/feedback_integration_test.go         C2, C6, C8 (DB side), RLS
  internal/db/platform_integration_test.go         C17
  internal/authz/operations.go                     + pipeline.approve_correction, SecondPerson
  internal/authz/permissions.go                    + Rule.AuditorWrite and its validation
  internal/httpx/errors.go                         + self_approval, already_decided
  internal/audit/schemas.go                        closed payload schemas for every action core writes
  internal/audit/catalogue.go                      action → contract type, kind, target, outcome, flag, facts
  internal/audit/reader.go                         list, detail, cursor, totals
  internal/audit/verify.go                         runs, scheduler, progress-reporting walk
  internal/feedback/                               POST /feedback, /corrections/*, /feedback/stats/mine
    handlers.go, shapes.go, store.go, handlers_integration_test.go
  internal/intel/jobs.go                           PublishJob honours SecondPerson (subject loader)
  internal/intel/sweeper.go, deadletter.go, drain.go, cap.go, factschema.go   widened to the jobs table
  internal/platform/                               /platform/audit, /platform/health, /platform/jobs, /platform/guardrails
    audit.go, health.go, sampler.go, jobs.go, guardrails.go, *_integration_test.go
  internal/smallgroup/                             the §6.3 rules, table and property tests
  internal/server/server.go                        registers the new routes and loops
  cmd/permgen/main.go                              (unchanged code; regenerated output)
services/nats/nats.conf                            pipeline user: the pipeline_learning consumer subjects
services/core/internal/bus/streams.go              the pipeline_learning consumer and the health KV bucket
services/intelligence/
  intelligence/boundary/operations.json            regenerated
  intelligence/boundary/pb/learning_pb2.py(.pyi)   generated
  intelligence/boundary/learning.py                LearningClient, TrustBufferLearningClient, StubLearningClient
  intelligence/boundary/learning_repo.py           the job's SQL (allow-listed)
  intelligence/boundary/jobs/approve_correction.py the job handler
  intelligence/boundary/jobs/run_query.py          + masked copy and timings_ms at completion
  intelligence/boundary/services/pipeline.py       binds pipeline_learning
  tests/boundary/test_approve_correction.py, test_learning.py
  tests/security/test_feedback_review.py           the operation test replaced (Task 4)
```

---
## Phase 4a: feedback and corrections

### Task 1: Migration 00011, feedback (spec §4.3, §4.4, §4.9)

**Files:**
- Create: `services/core/internal/db/migrations/00011_feedback.sql`, `services/core/internal/db/feedback_integration_test.go`
- Modify: `services/core/internal/db/db.go` (`WithHospitalUser`), `services/core/internal/db/db_integration_test.go` (seed the widened table in `TestRLSWithoutHospitalSeesNothing`)

**Schema:**
- `feedback_events`:
  - add `query_id uuid NOT NULL`, with an FK on `(query_id, hospital_id)` to `clinical_queries`;
  - rename `clinician_id` to `submitted_by` (NOT NULL);
  - add `status`, `decided_by`, `decided_at`, `rejection_code`, `rejection_note`, `client_created_at`, `request_id` and `assigned_to` (always null in Plan 4);
  - drop `decision_id`, `hga_weight_suggestions_json`, `processed` and `processed_at`.
- CHECKs:
  - `action` is `validate` or `correct`;
  - status matches action (`recorded` for validate; `pending_review`, `approved`, `rejected` or `withdrawn` for correct);
  - `decided_by <> submitted_by`;
  - decided means `decided_by` and `decided_at` are set;
  - withdrawn means `decided_by` is null;
  - text lengths: reason 3–1000, diagnosis 1–200, rejection note ≤ 500;
  - `rejection_code` in the four codes.
- `feedback_one_live` unique index `(query_id) WHERE status <> 'withdrawn'`.
- `mhai_current_user()`: SQL, stable, returns `nullif(current_setting('mhai.user_id', true), '')::uuid`.
- The `feedback_decider_check` constraint trigger runs on a move to `approved` or `rejected`. It is SECURITY DEFINER, owned by `mhai_migrate`, with owner `SELECT` policies on `users` and `user_roles` only. `decided_by` must be:
  - `is_active`;
  - in the same `hospital_id`;
  - holding `clinician`.
- The `feedback_transition_check` trigger:
  - allows only `pending_review` → `approved`, `rejected` or `withdrawn`;
  - never changes `recorded` or a decided row, and no update changes `submitted_by`, `query_id`, `action` or the text;
  - allows `withdrawn` only when `submitted_by = mhai_current_user()`, and `approved` or `rejected` only when `decided_by = mhai_current_user()`.
- `clinical_decisions`:
  - add `masked_question text` (≤ 8000);
  - add `masked_patient_json jsonb` (an object, ≤ 32 KB);
  - add `timings_ms jsonb`: an object whose keys ⊆ {`privacy`, `guidelines`, `answer`, `evidence`}, with non-negative integer values.
- Grants:
  - `mhai_gateway` gets `SELECT`, `INSERT` and `UPDATE (status, decided_by, decided_at, rejection_code, rejection_note)` on `feedback_events`;
  - `mhai_worker` gets `SELECT (id, hospital_id, query_id, action, status, submitted_by, decided_by)`;
  - `mhai_worker` `INSERT` on `clinical_decisions` gains the three new columns.

**Interfaces:**
- `db.WithHospitalUser(ctx, pool, hid, uid uuid.UUID, fn func(pgx.Tx) error) error`: the same transaction as `WithHospital`, plus `set_config('mhai.user_id', uid, true)`.

- [ ] **Step 1: Write the failing tests** (`feedback_integration_test.go`, tag `integration`):
  - `TestFeedbackSelfDecisionFailsCheck` (C2): as `mhai_gateway` under hospital A, and as `mhai_migrate`, `UPDATE ... SET status='approved', decided_by=submitted_by` fails with a check violation.
  - `TestFeedbackDeciderMustBeActiveClinicianSameHospital` (C6, DB part). The trigger refuses a decider who:
    - is a hospital admin without `clinician`;
    - has been deactivated;
    - belongs to hospital B;
    - is a platform account.
  - `TestFeedbackTransitionsAreOneWay`: approved → rejected, rejected → pending, recorded → anything, and a change of `reason` after submission all fail.
  - `TestFeedbackWithdrawOnlyBySubmitter`: `withdrawn` with `mhai.user_id` unset fails, and so does one with another user; it succeeds with the submitter.
  - `TestFeedbackOneLivePerQuery` (C11, DB part): a second live row for one query fails, and a new row after a withdrawal succeeds.
  - `TestWorkerCannotReadFeedbackText`: as `mhai_worker`, `SELECT reason` and `SELECT corrected_diagnosis` fail with a permission error.
  - `TestTimingsKeysClosed`: `{"privacy": 3, "x": 1}` and a negative value fail.
  - `TestRLSWithoutHospitalSeesNothing` now seeds a `feedback_events` row.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration -run 'Feedback|Timings|RLSWithout' ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration (Up and a full Down that takes back every grant, policy, function and column) and `WithHospitalUser`.
- [ ] **Step 4:** Run the tests above, then `go test -race -tags integration ./internal/db/... ./internal/queries/...`. Plan 3's tests must still pass.
- [ ] **Step 5:** Commit: "Feedback table: two-person rule in the database, masked consultation columns".

**Security checklist**
- [ ] The triggers are SECURITY DEFINER with `SET search_path = pg_catalog, pg_temp`, `REVOKE ALL FROM PUBLIC`, and owner policies on `users` and `user_roles` only.
- [ ] No grant lets any role set `decided_by` without the trigger running (a CONSTRAINT trigger, `DEFERRABLE INITIALLY IMMEDIATE`).
- [ ] The worker's column grant excludes all free text.

### Task 2: Migration 00012, the `jobs` table (spec §6.5)

**Files:**
- Create: `services/core/internal/db/migrations/00012_jobs.sql`
- Modify: `services/core/internal/db/jobs_integration_test.go`, `services/core/internal/db/worker_integration_test.go`, `db_integration_test.go` (RLS seeding)

**Schema:**
- `jobs` has the columns in spec §6.5:
  - `type` CHECK in (`approve_correction`);
  - `error` and `cancel_reason` codes `^[a-z_]{1,32}$`;
  - `cancel_note` ≤ 500;
  - `UNIQUE (id, hospital_id)`;
  - `jobs_one_live` unique `(type, subject_id) WHERE status IN ('queued','running')`.
- RLS `hospital_rows` for `mhai_gateway` and `mhai_worker`, `FORCE`.
- `mhai_gateway`: `SELECT`, `INSERT`, `UPDATE (published_at)`.
- `mhai_worker`: `SELECT`, plus `UPDATE (status, attempts, lease_until, error, completed_at, failed_at)`, under RESTRICTIVE policies copied from 00008 and 00009:
  - `worker_status`: only from `queued` or `running`, only to `running`, `completed` or `failed`, and attempts never down;
  - `worker_hospital_active`.
- `mhai_jobs`:
  - column `SELECT` and `UPDATE (status, error, published_at, cancel_reason, completed_at, failed_at, retry_count, cancelled_by, cancelled_at, cancel_note)`;
  - its policy as 00007: rows `queued`, `failed` or `cancelled` only, both before and after the update;
  - its `jobs_append` audit policy gains `job.cancelled`.
- `mhai_fail_expired_jobs(batch)`: a twin of `mhai_fail_expired_queries`.
- 00009's cancel-on-deactivate triggers also cancel the user's or hospital's `queued` and `running` rows in `jobs`.
- Outbox:
  - `intelligence_audit_outbox.query_id` becomes nullable;
  - add `job_id uuid` with an FK on `(job_id, hospital_id)` to `jobs`;
  - add `CHECK (num_nonnulls(query_id, job_id) = 1)`;
  - the action CHECK gains `correction.applied` and `correction.apply_failed`.
- `mhai_audit_drain`:
  - `SELECT (id, hospital_id, created_by, request_id, status, type)` on `jobs`;
  - its append policy gains the two correction actions.

- [ ] **Step 1: Write the failing tests:**
  - `TestJobsWorkerStatusMoves`: the worker can't cancel a row, re-queue it, re-open a completed row or lower `attempts`.
  - `TestJobsWorkerOnlyActiveHospital`.
  - `TestJobsJobsRoleCantTouchRunning`: `mhai_jobs` can't update a `running` or `completed` row (used by C29).
  - `TestJobsOneLivePerSubject`.
  - `TestJobsExpiredFailed`.
  - `TestDeactivationCancelsJobs` (A21's twin).
  - `TestOutboxExactlyOneParent`: both parents null, or both set, fails.
  - `TestRLSWithoutHospitalSeesNothing` seeds `jobs`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration -run 'Jobs|Outbox|Deactivation|RLSWithout' ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration and its Down.
- [ ] **Step 4:** Run the tests, then `go test -race -tags integration ./internal/db/... ./internal/intel/...`. Plan 3's drainer and dead-letter tests must still pass.
- [ ] **Step 5:** Commit: "A generic jobs table for new job types; outbox facts by job".

### Task 3: Migration 00013, `feedback_buffer` (spec §4.8, §4.9)

**Files:**
- Create: `services/core/internal/db/migrations/00013_feedback_buffer.sql`
- Modify: `feedback_integration_test.go`, `db_integration_test.go`

**Schema:**
- `feedback_buffer`:
  - `id bigserial`;
  - `hospital_id`;
  - `correction_id uuid UNIQUE`, with an FK on `(correction_id, hospital_id)` to `feedback_events`;
  - `nli_score`, `coherence_score`, `numerical_score` and `se_score`, each `real` in [0,1] or null;
  - `system_decision` in PASS, REGENERATE or ESCALATE;
  - `model_versions jsonb`;
  - `created_at`;
  - `cleared_at`;
  - `reset_id uuid`.
- RLS `FORCE`.
- `mhai_worker`: `INSERT` with `hospital_rows`, plus the RESTRICTIVE `buffer_needs_approval` policy: `EXISTS (SELECT 1 FROM feedback_events f WHERE f.id = correction_id AND f.hospital_id = hospital_id AND f.status = 'approved' AND f.decided_by IS NOT NULL AND f.decided_by <> f.submitted_by)`. This needs `mhai_worker` `SELECT` on those columns, which Task 1 granted.
- No other role gets a grant.
- `mhai_feedback_buffer_waiting()`: SECURITY DEFINER, returns `bigint` (rows with `cleared_at IS NULL`), `EXECUTE` for `mhai_platform` only.

- [ ] **Step 1: Write the failing tests:**
  - `TestBufferRefusesUnapproved` (C8). As `mhai_worker` under A, an insert fails for a correction that is:
    - `pending_review`;
    - `rejected`;
    - `withdrawn`;
    - forced (as superuser, with the CHECK dropped in a scratch schema copy) to self-approved.
  - `TestBufferAcceptsApprovedOnce`.
  - `TestBufferOtherHospitalInvisible`.
  - `TestNoPlan4RoleWritesWeightsOrKB` (C10, grants): `mhai_worker` and `mhai_gateway` have no `INSERT`, `UPDATE` or `DELETE` on `guardrail_weights` or `kb_chunks`, checked through `has_table_privilege`.
  - `TestBufferWaitingOnlyPlatform`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration -run 'Buffer|NoPlan4Role' ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests and the package.
- [ ] **Step 5:** Commit: "Feedback buffer: only approved, two-person corrections, per hospital".

### Task 4: The operations table, the contract and error codes (spec §4.7, §10.2)

**Files:**
- Create: `proto/mhai/intelligence/v1/learning.proto`, `services/core/internal/intel/subjects.go` (the subject loaders)
- Modify: `services/core/internal/authz/operations.go`, `operations_test.go`, `services/core/internal/httpx/errors.go`, `errors_test.go`, `services/core/internal/intel/jobs.go`, `jobs_integration_test.go`
- Generated: `services/core/internal/intelpb/learning.pb.go`, `services/intelligence/intelligence/boundary/pb/learning_pb2.py(.pyi)`, `services/intelligence/intelligence/boundary/operations.json`, the generated permissions doc, the Intelligence API docs page, `testdata/proto-golden/approve_correction.*`
- Modify: `services/intelligence/tests/security/test_feedback_review.py`

**Interfaces:**
- `ApproveCorrectionJob { string job_id = 1; string correction_id = 2; }`
- `authz.Operation.SecondPerson bool`, validated: `Kind job`, at least one role, and a subject loader registered in `intel`. The JSON field is `second_person`.
- The row `pipeline.approve_correction`: `Kind job`, `AnyRole [clinician]`, `NeedsHospital`, `Timeout 5m`, `SecondPerson`, schema `mhai.intelligence.v1.ApproveCorrectionJob`.
- `intel.SubjectLoader func(ctx, tx pgx.Tx, subjectID uuid.UUID) (submittedBy, decidedBy uuid.UUID, err error)`, registered per operation. `PublishJob` and `Republish` call it for a `SecondPerson` operation and refuse with `ErrSecondPerson` unless `decidedBy == caller.Sub` and `decidedBy != submittedBy`.
- `httpx.Codes`: `self_approval` (403, title "Not allowed", detail "You can't decide your own correction.") and `already_decided` (409, "Already decided", "This correction has already been decided.").

- [ ] **Step 1: Write the failing tests:**
  - `TestApproveCorrectionOperationRow`: clinician only, not `bff_direct`, not `SvcCore`, `SecondPerson` set.
  - `TestSecondPersonNeedsLoader` (a row without a loader panics at init in a test table).
  - `TestPublishJobRefusesSecondPersonMismatch` (C28, unit and integration): `PublishJob` refuses when `sub` is the submitter, when `decided_by` is null, and when `sub` isn't the decider.
  - `TestErrorCodesComplete` covers the two new codes.
  - Golden decode in Go (`TestGoldenMessages`).
  - Python `tests/boundary/test_golden_messages.py` runs on the new file.
  - Python `test_feedback_review.py`: replace `test_feedback_has_no_boundary_operation` with `test_only_feedback_operation_is_approve_correction` (the only name matching `feedback|correction|approve` is `pipeline.approve_correction`, roles `["clinician"]`, `bff_direct` false). Keep `test_boundary_code_never_reaches_the_feedback_loop` unchanged.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/authz/... ./internal/httpx/...` and `go test -race -tags integration -run SecondPerson ./internal/intel/...` → fail. **Run (Python, remote):** `uv run pytest -q tests/security/test_feedback_review.py tests/boundary/test_golden_messages.py` → fail.
- [ ] **Step 3:** Add the proto, run `make proto`, add the row, the field, the loader registry, the codes and the `PublishJob` check.
- [ ] **Step 4:** Run the tests above, `make proto-check`, and the permgen drift test (`go test ./cmd/permgen/...`).
- [ ] **Step 5:** Commit: "Operations: pipeline.approve_correction with SecondPerson; ApproveCorrectionJob; self_approval, already_decided".

### Task 5: Core, `POST /feedback` and the corrections routes (spec §4.2, §4.4, §9)

**Files:**
- Create: `services/core/internal/feedback/{handlers.go,shapes.go,store.go,handlers_integration_test.go,export_test.go}`, `services/core/internal/audit/schemas.go`, `schemas_test.go`
- Modify: `services/core/internal/audit/audit.go` (`Write` checks the registry), `services/core/internal/server/server.go` (register), `services/core/openapi.yaml`, `services/core/internal/authz/nodirectdb_test.go` (allow-list `feedback/store.go`)

**Interfaces:**
- Routes (spec §10.1), all with `AnyRole: clinician`:
  - `POST /feedback`;
  - `GET /corrections`, `/corrections/mine` and `/corrections/reviewed`;
  - `POST /corrections/{id}/approve` (`Calls: pipeline.approve_correction`), `/reject` and `/withdraw`;
  - `GET /feedback/stats/mine`.
- `store.go`:
  - `Submit`;
  - `ListPending(hid, me)`, `ListMine` and `ListReviewed`;
  - `Decide(hid, me, id, decision)`, which returns `ErrNotFound`, `ErrSelf`, `ErrForbidden` or `ErrDecided`;
  - `Stats`.
  - Every call runs inside `db.WithHospitalUser` with the token's hospital and user.
- `audit.Schema`: `{Fields map[string]Check; Required []string}`, and `audit.Register(action, schema)`. `audit.Write` refuses an action with no schema, or a payload that doesn't fit (`ErrSchema`); the caller's transaction rolls back.
- Schemas for every action in spec §9 table rows 1–6. The test `TestEveryActionHasSchema` parses every `Action:` literal under `internal/` (go/ast) and requires a registry entry. It also registers the Plan 2 and Plan 3 actions, whose existing payloads are frozen as their schemas.
- `CorrectionOut` is built from:
  - `feedback_events`;
  - `clinical_decisions` (`hga_decision`, `degraded`, `diagnosis`, `icd10_code`, `response_text`, citations as stored);
  - `masked_question` and `masked_patient_json` (age, sex, symptoms; the question is the masked one);
  - `clinical_queries` (`urgency`, `category` as specialty).

  People come from `users` (`display_name`, `full_name`, `specialty`) and the hospital name.

- [ ] **Step 1: Write the failing tests** (`handlers_integration_test.go`, through the real mux with Plan 2's auth fixtures):
  - `TestSubmitValidateAndCorrect`: the shapes are exactly `FeedbackOut`, and the audit rows are `feedback.validated` and `correction.submitted` with schema-only payloads.
  - `TestSubmitOnOthersQueryIs404` and `TestSecondFeedbackIs409` (C11).
  - `TestSubmitWithoutMaskedCopyIs409`.
  - `TestSubmitValidation`: lengths, control characters, `client_created_at` more than 5 minutes ahead, unknown action → 422.
  - `TestSelfApprovalRefusedAndAudited` (C1): 403 `self_approval`, a `correction.decision_refused` row, and no `jobs` row.
  - `TestParallelApprovals` (C3): 2 submitter sessions and 1 reviewer session approve at once. Exactly one `approved` with `decided_by` = the reviewer; the others get 403 or 409.
  - `TestApproveBodyCannotNameDecider` (C4): a body `{"decided_by": "<other uuid>"}` is ignored (or 422 for unknown fields); `decided_by` is the token's user.
  - `TestNonCliniciansRefused` (C5): a hospital admin without `clinician`, an analyst, a platform admin and an auditor get 403 on approve, reject, withdraw and `GET /corrections`.
  - `TestDeciderLostRoleBetweenLoadAndApprove` (C6): the role is removed after loading. The approve fails: 409 `conflict` from the trigger, mapped, with no job. A deactivated decider gets 401 (Plan 2 principal).
  - `TestOtherHospitalIs404` (C7): every corrections route given hospital A's id, called from hospital B.
  - `TestWithdrawThenApprove409` and `TestApproveThenWithdraw409` (C12).
  - `TestPendingListShowsMaskedTextOnly` (C14): the stored question holds the synthetic name "Zebedee Quist" and MRN "MRN-SYN-0001", and the masked copy holds placeholders. The pending list contains neither marker.
  - `TestOnlyCorrectionsReaderSelectsMaskedColumns` (C15, a Go AST/SQL scan test in `feedback/export_test.go`):
    - only `feedback/store.go` names `masked_question` or `masked_patient_json`;
    - `feedback/store.go` never names `clinical_question` or `patient_data_json`.
  - `TestFeedbackTextNeverLeaves` (C16, core part). A marker string in `reason`, `corrected_diagnosis` and `rejection.note` appears in no captured log line or span (Plan 3's capture helpers in `internal/telemetry`), and in no `audit_log.payload`.
  - `TestStatsMine`.
  - `TestEveryActionHasSchema` (part of C21).
  - `TestAuditWriteRefusesBadPayload`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/audit/... && go test -race -tags integration ./internal/feedback/...` → fail.
- [ ] **Step 3:** Implement the store, handlers and shapes, the schema registry, the route registration and `openapi.yaml` (minor version bump).
- [ ] **Step 4:** Run the tests, `go test -race -tags integration ./internal/...`, and the `oasdiff` check that CI runs (additive only).
- [ ] **Step 5:** Commit: "Feedback and corrections: submit, review, withdraw, stats; closed audit payloads for core's actions".

**Security checklist**
- [ ] Guards run in the order: route role → 404 → 403 → 409 → body validation.
- [ ] Every query runs through `WithHospitalUser`; no handler reads a hospital or user from the body.
- [ ] Free text never reaches an audit payload, a log line or a span.

### Task 6: Core, approval publishes the job (spec §4.5, §6.5)

**Files:**
- Modify:
  - `services/core/internal/feedback/handlers.go`;
  - `services/core/internal/intel/{jobs.go,sweeper.go,deadletter.go,drain.go,cap.go,factschema.go,facts.go}` and their integration tests;
  - `services/core/internal/server/server.go`.

**Interfaces:**
- Approve, in one `WithHospitalUser` transaction:
  1. the conditional `UPDATE`;
  2. `INSERT jobs (type 'approve_correction', subject_id, created_by me, request_id, deadline now()+5m)`;
  3. `audit.Write correction.approved {correction_id, job_id}`.

  Then commit; then `client.PublishJob(ctx, intel.FromPrincipal(p, rid), "pipeline.approve_correction", jobID, &ApproveCorrectionJob{...}, deadline)`. On a publish error, reply 204 anyway: the sweeper republishes.
- `intel.ReserveInFlight` gains a job-type budget: 20 for `approve_correction`. Past it, the decision commits, `published_at` stays null, and the sweeper publishes when a slot frees.
- The sweeper, the dead-letter consumer (`HandleDeadLetter`) and the deadline expiry handle both `clinical_queries` and `jobs`; the subject's hospital must equal the row's hospital, as Plan 3 ruling F5 requires.
- The drainer takes actor, hospital and request id from `jobs` for a `job_id` fact. `factschema.go` gains:
  - `correction.applied` (`correction_id`, `attempts`, `model_versions`, `persona_update`, status `completed`);
  - `correction.apply_failed` (`correction_id`, `error`, `attempts`, status `failed`).

- [ ] **Step 1: Write the failing tests:**
  - `TestApprovePublishesOneJob` (C3, publish side): one `jobs.v1.<hid>.approve_correction` message, `Nats-Msg-Id` = job id, token `sub` = the reviewer, `hid` = the hospital.
  - `TestApproveIdempotent` (C13): the same `Idempotency-Key` twice gives 204 twice and one message. A new key gives 409 `already_decided`.
  - `TestApprovalAuditInSameTransaction` (C23): a forced failure after `audit.Write` (a test hook) rolls back both the approval and the audit row.
  - `TestApprovalBudget` (C32): a 21st approval in flight for one hospital gives 204 and stays unpublished; after one completes, the sweeper publishes it.
  - `TestDrainerJobFactUsesJobsIdentity`: a fact naming another actor is recorded with the job's creator (the A12 twin).
  - `TestDrainerRefusesMismatchedAction`.
  - `TestDeadLetterApproveCorrectionMarksFailed`.
  - `TestSweeperRepublishesJobsRows`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/feedback/... ./internal/intel/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and `go test -race -tags integration ./internal/...`.
- [ ] **Step 5:** Commit: "Approving a correction publishes approve_correction; loops widened to the jobs table".

### Task 7: Python, the `approve_correction` job (spec §4.7, §10.5, §10.6)

**Files:**
- Create:
  - `services/intelligence/intelligence/boundary/{learning.py,learning_repo.py,jobs/approve_correction.py}`;
  - `services/intelligence/tests/boundary/{test_approve_correction.py,test_learning.py}`.
- Modify:
  - `services/intelligence/intelligence/boundary/{services/pipeline.py,jobs/run_query.py,jobs_repo.py,config.py,worker.py}`;
  - `services/intelligence/tests/boundary/{test_run_query.py,test_no_direct_db.py}`;
  - `services/nats/nats.conf`;
  - `services/core/internal/bus/streams.go`, `bus_integration_test.go`, `conf_test.go`.

**Interfaces:**
- `learning.LearningClient` (Protocol):
  - `buffer_entry(scores: Scores, system_decision: str) -> BufferEntry`;
  - `persona_update(correction_id: str) -> Literal["skipped","applied"]`;
  - `model_versions() -> dict[str, str]`.
- `TrustBufferLearningClient`: `buffer_entry` clamps each score to [0,1] or None; `persona_update` returns `"skipped"`.
- `StubLearningClient` can be told to raise per method. `build_learning_client(settings)` refuses the stub when `APP_ENV=production`, mirroring Plan 3 ruling F14.
- `learning_repo` exposes:
  - `claim(cur, job_id, correction_id)`: `UPDATE jobs SET status='running', attempts=attempts+1, lease_until=now()+interval '60 s' WHERE id=%s AND type='approve_correction' AND subject_id=%s AND (status='queued' OR (status='running' AND lease_until<now())) RETURNING attempts`;
  - `load(cur, correction_id)`: the correction's permitted columns, the decision's `hga_decision` and `model_versions`, and the latest `hga_evaluations` scores for its query;
  - `complete(cur, ...)`: buffer insert, outbox `correction.applied`, job `completed`;
  - `fail(cur, ...)`.
- `jobs/approve_correction.handle(verified, msg, ctx)`: the `run_query` skeleton. A correction that isn't `approved` gets `Term` with `invalid_request`, a dead letter, and an outbox `correction.apply_failed`.
- `run_query` completion also writes `masked_question`, `masked_patient_json` (the masked tree `_mask_all` returned) and `timings_ms` (from its stage timings, keys mapped: `pii_masking`→`privacy`, retrieval→`guidelines`, generation→`answer`, verification→`evidence`).
- NATS:
  - `streams.go` adds the durable `pipeline_learning`: filter `jobs.v1.*.approve_correction`, explicit ack, `AckWait` 60 s, `MaxDeliver` 5, `MaxAckPending` 4, no `BackOff`.
  - `nats.conf` gives the `pipeline` user the three `pipeline_learning` `$JS` subjects only.

- [ ] **Step 1: Write the failing tests** (marker `boundary`, stub learning client):
  - `test_approved_correction_writes_one_buffer_row`: one `feedback_buffer` row, one outbox `correction.applied` with `persona_update: "skipped"`, job `completed`.
  - `test_redelivery_after_completion_acks_without_second_row` (the A4 twin).
  - `test_commit_then_crash_still_audited` (A13 twin, C23 worker side).
  - `test_poison_job_dead_letters_after_five` (the A20 twin).
  - `test_foreign_correction_is_not_found_and_reads_nothing` (C9): a job for hospital A naming hospital B's correction gives `not_found`, `Term`, a dead letter, no buffer row, and only the claim statement in the database log.
  - `test_unapproved_correction_terminated` (C8, worker side): the row is forced back to `pending_review` as superuser before delivery, which gives `Term` with `invalid_request` and no buffer row. A second variant bypasses the handler check and inserts directly; it is refused by `buffer_needs_approval`.
  - `test_worker_never_selects_free_text` (C16, worker side): the database statement log during a run never names `reason` or `corrected_diagnosis`.
  - `test_no_learning_code_writes_weights_or_kb` (C10, code side): grep the `boundary` package for `guardrail_weights`, `kb_chunks` writes, `DoctorFeedbackLoop` and `TrustWeightOptimizer.optimize`.
  - `test_stub_learning_client_refused_in_production`.
  - `test_run_query_writes_masked_copy_and_timings`: the masked text holds no synthetic marker, and `timings_ms` keys are closed.
  - Go `TestPipelineUserLearningConsumerOnly` (`bus/conf_test.go`): the `pipeline` user can't publish `jobs.v1.>`, can't fetch from consumer `pipeline` with the learning subjects or the reverse, and can't reach `$JS.API.STREAM.>`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q -m boundary tests/boundary/test_approve_correction.py tests/boundary/test_run_query.py` and `uv run pytest -q tests/boundary/test_learning.py tests/boundary/test_no_direct_db.py` → fail. **Run (Go, remote):** `go test -race -tags integration -run PipelineUser ./internal/bus/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, then the whole Python suite (`uv run pytest -q -m "not models"`), `make lint typecheck`, and `go test -race -tags integration ./internal/bus/...`.
- [ ] **Step 5:** Commit: "Pipeline: approve_correction fills the per-hospital buffer; run_query stores the masked copy and step timings".

**Security checklist**
- [ ] The hospital comes only from `Verified`; every statement runs inside `with_hospital`.
- [ ] Nothing checks who approved: the database does.
- [ ] No free text is read; the job body holds ids only; dead letters hold ids and codes.

**Phase 4a done when:** Task 14's 4a slice passes (the smoke test: a clinician asks, a second clinician approves, the buffer row and `correction.applied` reach the chain), CI is green, and the PR is reviewed.

---
