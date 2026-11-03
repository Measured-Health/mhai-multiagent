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
