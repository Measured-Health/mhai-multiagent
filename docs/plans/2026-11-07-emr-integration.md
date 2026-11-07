# EMR integration (records in, abnormal results flagged to clinicians): implementation plan

**Status:** Not started. The design is approved (product owner, 7 Nov 2026); the recommended answers in its §21 stand. Nothing in this plan makes EMR live: live use needs every gate in design §17.6 and the people items H1–H9, which are TODO.md items owned by people. An agent never marks them done.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Read labs, vital signs and medications from hospital record systems into a canonical model with provenance, flag abnormal values with a deterministic, clinically reviewed rule set, and serve the alerts, settings and connection screens mhai-web has already built, with every outside credential and every route out held by a separate `services/emr/` service, and everything off by default.

**Architecture:**

- **`services/gokit/` (Go, new module):** the pieces core and the emr service share: the request-token package (moved from core, its subject rule generalised to a table), a replay set, `WithHospital` taking a role, the redacting logger, the EMR control-operations table, URL and origin checks, credential sealing, the record-link builder, and the generated `emr/v1` protobuf.
- **`services/emr/` (Go, new module and binary):** the control responder (`emr.v1.ctl.*`), the secret store, the connectors (stub, FHIR R4, file export, document archive), `RawSink` and the allow-list, the one normaliser, the patient index, the rules engine and the per-source breaker; plus `egress-emr`, our own small CONNECT proxy, as a second binary.
- **Core (Go):** migrations 00021–00025 (00026 in PR 12), the platform connection routes with sealing at the edge, the hospital routes (alert settings, possible duplicates, exclusions), the clinician routes (`/alerts*`, `/me/notification-settings`), the `core_emr` finding consumer (routing, fatigue, amended and withdrawn alerts, grouped mode), the purge and retention loop, the audit actions.
- **Deploy:** an `emr` compose profile (the emr service, `egress-emr`, the `egress-emr` network); the `EMR` JetStream stream, created by core.

**Tech stack:** Go 1.24 (`pgx/v5`, `goose/v3`, `nats.go`, `testcontainers-go`, `golang.org/x/crypto` for `nacl/box` and `ssh`), as core today. New Go dependencies (exact versions pinned in `go.sum`, Dependabot weekly): `gopkg.in/yaml.v3` (already in core) for maps and rule sets, `github.com/pkg/sftp` for `file_export` (PR 11 only), the chosen secret store's Go SDK (Task 12, after H3). No Python change except one test in PR 13. No new dependency for FHIR: the connector reads JSON with `encoding/json` into our own allow-listed structs.

**Spec:** `docs/specs/2026-11-07-emr-integration-design.md` (approved 7 Nov 2026). Section numbers (§n) are the design's; A1–A42 are its abuse cases (§17.5), H1–H9 its people items (§20). It builds on the intelligence boundary design as built, the document pipeline design, the Plan 4 design, the cloud model providers design, and ADRs 0004, 0005, 0009, 0010, 0011 and 0012.

## The pull requests

Every PR leaves `main` working, with EMR off: no connection can be saved, no emr process runs, no stream exists and no alert is raised until a person switches each piece on (Global Constraints, "Off by default").

| PR | Tasks | What | Start | Waits on |
|---|---|---|---|---|
| **1: docs** | 1 | ADR 0009 Accepted, ADR 0014, the document pipeline amendment, layout docs, frontend §14.11 and §12, TODO gates | **now** | |
| **2: skeleton** | 2–5 | `services/gokit/`, the emr binary, control verification, `health`, NATS user, `EMR` stream, compose profile, sealing key pair | **now** | PR 1 |
| **3: connections** | 6–12 | migration 00021, URL checks, sealing, `Connector` and the stub, the secret store, `put`/`wipe_credentials` and `test`, the platform routes | **now** (Tasks 6–11) | Task 12 (managed store) waits on **H3**; production use too |
| **4: egress** | 13–15 | `egress-emr` (resolve once, pin, per-connection auth), the HTTP and SFTP clients, the network tests | **now** | PR 2 |
| **5: canonical** | 16–21 | migration 00022, `RawSink`, code tables and maps, the normaliser, the patient index | **now** (Tasks 16–20, on synthetic code tables) | Task 21 (the real LOINC and ATC tables) waits on **H6** |
| **6: FHIR R4** | 22–25 | SMART auth and `test`, `Fetch`, the scheduler, the synthetic FHIR server and the end-to-end run | **now** | PRs 4, 5 |
| **7: rules** | 26–30 | migration 00023, the rule kinds and golden tests, the rules consumer, the per-source breaker | **now** (Tasks 26–29, on a synthetic rule set) | Task 30 (rule set v1) and activation wait on **H4** |
| **8a: alert routes** | 31–34 | migration 00024, alert settings, `/alerts*` and notification settings, possible duplicates and exclusions | **now** | PR 7 |
| **8b: finding consumer** | 35–38 | `core_emr`, fatigue and grouped mode, amended and withdrawn alerts, signals, EMR dead letters | **now** | PR 8a |
| **9: retention** | 39–41 | migration 00025, the purge and retention loop, legal hold and hospital removal hooks, the Usage counts | **now** (values as recommended) | H5 confirms the values before live use |
| **10: mhai-web** | 42 (core side) | core accepts BFF-sealed credentials; the mhai-web PR itself is in the other repository (handoff list below) | **now** (core side) | mhai-web PR after 8b |
| **11: Havana structured** | 43–44 | `mhai_csv_v1`, the SFTP connector, staleness by expected interval | **not before H1** | **H1** (what Havana can export); live: **H7** |
| **12: Havana archive** | 45–47 | migration 00026, the reviewer's patient search and creation, the archive connector, rejected values back to review | **not yet** | **H2**, the documents service, document pipeline phase 1 and its clinical sign-off |
| **13: `pids`** | 48 | core's patient check and the token claim | **now** | PR 5 |

PRs 4 and 5 can run in parallel after PR 3 (PR 4 needs only PR 2). The migrations are linear: 00021 (PR 3), 00022 (PR 5), 00023 (PR 7), 00024 (PR 8a), 00025 (PR 9), 00026 (PR 12).

**Live use** (design §17.6) waits on all of: mhai-web PR 10 shipped, PR 9 deployed, BFF-side sealing (Task 42 plus mhai-web), rule set v1 in shadow with every critical reviewed (H4), the on-call owner (H9), the clinical sign-off (H4) and the privacy and legal sign-off (H5). None of these is code in this plan.

## Global Constraints

Plans 2, 3 and 4 and the cloud plan's constraints still hold: identity only from the verified token or session; 404 for another hospital's resource; problem bodies carry a fixed code only; no PHI or secrets in logs, spans, audit payloads, dead letters or NATS messages; synthetic data only; plain commit messages with no AI attribution; tests run remotely. In addition:

- **Migrations start at 00021.** `main` has 00018 (`00018_model_policy.sql`); Plan 7c takes 00019 and 00020. If either lands in a different order, or another PR takes a number first, the file is renumbered at build time with every test reference, and the PR says so.
- **Off by default.** `CORE_EMR_ENABLED` is `false` by default: the platform connection write routes answer 503 `not_ready`, `GET /platform/emr/connector-types` answers `{types: []}`, core creates no `EMR` stream and runs no `core_emr` consumer. `CORE_EMR_CONNECTOR_TYPES` is empty by default (no type offered). The emr service and `egress-emr` run only under the compose profile `emr`. `EMR_RULE_SET_ACTIVE` is empty by default, so every finding is `shadow` and nothing alerts. `CORE_EMR_ALLOW_NULL_MRN` is `false`. The clinician and hospital routes always answer (an empty list, `not_connected`), so mhai-web's bell and settings page keep working.
- **No outside credential in core**, the database, NATS (other than sealed boxes on `emr.v1.ctl.put_credentials`), a log, a span, an error, the idempotency store, an audit payload, the browser or the repository. Credential values exist in the emr service's memory (`connector.Creds`) and the secret store only.
- **Ids only in messages.** Every NATS payload on `emr.v1.*` and `records.v1.*` is a protobuf of `proto/mhai/emr/v1/` with ids, closed codes and counts only. `buf lint` and `buf breaking` run in CI as for `intelligence/v1`.
- **Security rules are written once.** Who may call a control op: `gokit/emrops`. Who sees an alert: core's SQL and routes. What is abnormal: the rule set. The emr service never decides a recipient, never serves the frontend and never writes `audit_log`.
- **Every table** has `ENABLE` and `FORCE` row-level security, owner `mhai_migrate`, column grants (never `SELECT *` to an application role), a row in `TestRLSWithoutHospitalSeesNothing`, and a Down. Every `SECURITY DEFINER` function: owner `mhai_migrate`, `SET search_path = pg_catalog, pg_temp`, every relation schema-qualified, `REVOKE ALL … FROM PUBLIC`, `EXECUTE` to the roles named only.
- **No guessing** (§9, §15.1): no default unit, no inferred time zone, no fuzzy code match, no assumed score parameter, no automatic patient merge, no file name used for matching.
- **Logs:** ids, codes and counts. Database errors by SQLSTATE and constraint name only (never `pgconn.PgError.Detail` or `.Message`), connector HTTP errors by status and outcome code only, never a response body, never an archive file name.
- **The emr module imports nothing from `services/core/`** (Go forbids `internal/` across modules; a test also pins the `go.mod` requires).
- **No real data, no real hospital endpoint.** Connector tests use `httptest` servers and the synthetic FHIR generator. The end-to-end run uses a synthetic FHIR server on steven-reid.
- **Closed vocabularies**, pinned by a test on each side that uses them: connector types `fhir_r4`, `document_archive`, `file_export`; outcome and error codes `ok`, `unreachable`, `sign_in_refused`, `timed_out`, `unreadable_data`, `unmapped_codes`, `too_many_requests`; severities `critical`, `urgent`, `info`; the eight categories of §11.3; withdrawn reasons `source_amended`, `source_retracted`, `excluded`; dismiss reasons `not_relevant`, `already_known`, `wrong_patient`, `other`; signal kinds `possible_match`, `source_merge`, `breaker_tripped`, `document_rejected`, `rule_set_activated`.

## Review Focus

1. **Times that look valid but are not what they say.** A FHIR `effectiveDateTime` of `2026-11-07` (a date, no time), `2026-11-07T08:00:00` (no offset), a `+01:00` value across midnight UTC, and a value 9 minutes vs 11 minutes in the future. Expected: a date-only value is a date (no hour invented, so no 6-hour vitals window can be met and no alert raised from it), FHIR `dateTime` without an offset is refused as `unreadable_data` (FHIR requires an offset when a time is present), the UTC value is stored with the offset applied, 9 minutes ahead is accepted and 11 minutes is refused. Tests: `TestTimesDateOnlyNoOffsetOffsetAndFuture` (Task 19), `TestDateOnlyVitalsNeverAlert` (Task 28).
2. **The cursor at a page boundary.** Many resources share one `meta.lastUpdated`; a page ends in the middle of them; the next poll starts from the cursor; a page fails half-way. Expected: the cursor is the greatest `lastUpdated` of a **completed** page minus the 2-minute overlap, never moves backward and never moves past a failed page; duplicates drop by `(connection, kind, id, versionId)`. Tests: `TestCursorEqualTimestampsAcrossPages`, `TestCursorNotAdvancedOnFailedPage` (Task 23).
3. **Numbers in source text.** `5,5` (comma decimal on a connection set to `.`), `1,234.5`, `<0.5`, `>100`, `6.5 mmol/l`, ` 6.5 `, a Unicode minus `−3`, `6.5e1`, `NaN`. Expected: the strict parser of §9.2 accepts only the configured separator, comparators go to `value_text` plus the comparator, the unit in the value string is refused (the unit has its own field), exponent and `NaN` are refused; nothing is half-parsed. Test: `TestStrictDecimal` (table, Task 19).
4. **Who sees an alert after settings change.** A queue alert is raised, then the hospital admin turns the queue off; a named clinician loses the `clinician` role while viewing. Expected: visibility is fixed at creation (`alerts.routed_queue` and `alert_deliveries`), never recomputed from current settings, so a queue alert stays visible to every clinician until handled; the role loss re-queues per §12.2. Tests: `TestQueueAlertStaysVisibleAfterQueueOff` (Task 33), `TestRoleRemovalRequeues` (Task 31).
5. **An amendment racing a handler.** An amended result arrives while a clinician acknowledges the same alert. Expected: exactly one of "acknowledged, then marked `amended`" or "updated in place, then acknowledged" happens; the acknowledgement is never lost, the alert never reopens, and the evidence shown matches the stored version. Test: `TestAmendRacesAcknowledge` (20 parallel pairs, Task 37).

## Where this plan departs from the design

1. **Migration numbers are 00021–00026.** Design §18 takes "the next free one". Plan 7c holds 00019 and 00020 (Global Constraints).
2. **The control operations have their own table, `gokit/emrops`, not core's operations table.** Core's `authz.validateOperation` and the worker's `operations.py` both refuse any role that isn't clinical, and the table is published as `operations.json` to Python. The design's rows allow `platform_admin` and `svc:core`. `emrops.Ops` (name, subject, `AnyPlatformRole`, `SvcCore`, `StepUp`, timeout, request and response schema) is read by core's `emrctl` client and by the emr responder, and is not exported to Python. A test pins that no `emr.*` row is in `operations.json`.
3. **The Go verifier gains a replay set.** `reqtoken.VerifyAt` runs checks 1–4 and `CheckReply` check 5; replay is in the Python worker (`boundary/replay.py`, per process) and in `grant` (its own set). `gokit/replay.Set` is the same bounded per-process set; the emr responder checks it after verification. As for the workers, a replay delivered to another emr copy isn't caught by that copy; every control op is idempotent or self-limiting (`put_credentials` boxes expire in 60 s and bind connection, field and origin; `test` and `sync_now` repeat harmlessly). Recorded as residual risk.
4. **The subject rule is a table in `reqtoken`.** Today `validateMint` and `VerifyAt` hard-code `inference.v1.<aud>.<op>` for `req`. `reqtoken.SubjectRules` lists the allowed shapes (`inference.v1.<aud>.<op>`; `emr.v1.ctl.<op>` → aud `emr`; jobs as today), and both functions use it. `emr.health` uses the platform sampler's fixed probe hospital id (`healthProbeHospital`), as `pipeline.health` does, so no hospital-less request token is needed.
5. **`services/gokit/` is a module, and core's image builds from `services/`.** Core `require`s gokit with `replace ../gokit`, so its Docker build context moves from `services/core` to `services` (`dockerfile: core/Dockerfile`) in `docker-compose.yml`, `.github/workflows/deploy.yml`, the CI image job and the Makefile. The emr image builds the same way.
6. **`dbx.WithHospital` takes the role.** Core's `db.WithHospital` is hard-wired to `mhai_gateway`. The gokit version is `WithHospital(ctx, pool, role, hospitalID, fn)` over a closed role set (`mhai_gateway`, `mhai_emr`, `mhai_emr_core`); core's `db.WithHospital` becomes a one-line wrapper.
7. **Document review functions move to PR 12.** `emr_patient_candidates()`, `emr_create_document_patient()`, `mhai_worker`'s `INSERT` on `emr.raw_records`, the gateway's publish on `emr.v1.*.fetched` and `document.review_reopened` need the document review tables, which aren't on `main` (the documents service isn't built). Migration 00022 creates `raw_records` with `document_id` and `review_id` columns and the `CHECK` (so A26 holds from the start), with no foreign key and no grant to `mhai_worker`. Migration 00026 (PR 12) adds the keys, functions and grants. Least privilege: no grant exists before its user does.
8. **The re-queue on a recipient leaving is a trigger, not a route.** No core route changes `users.is_active` or `user_roles` today (they change by CLI or SQL; 00009 cancels questions the same way). Migration 00024 adds `AFTER UPDATE` on `users` and `AFTER DELETE` on `user_roles` triggers (definer, as 00009), which move the alerts and append an `alert.requeued` fact to `intelligence_audit_outbox`; core's drainer puts it on the chain. The drain policy and schema gain that action. When a core user route exists, it inherits the behaviour.
9. **The purge and the retention sweep run in core, not as `jobs.v1` jobs.** The design publishes `jobs.v1.<hid>.emr_exclusion_purge` and `jobs.v1.global.emr_retention`, but the emr NATS user has no `jobs.v1` right (§3.2), and the work is SQL. Core runs `emrmaint.Loop` (leader-locked like the health sampler): every 5 minutes it purges `pending` `emr.purge_requests` past `due_at` or created since the last run, and nightly it runs the capped retention functions. The durable request row, the 24-hour bound and the legal-hold states are the design's.
10. **Legal holds: a minimal table until the platform feature exists.** Core has no legal hold (frontend §14.8.1 is mhai-web's contract). Migration 00025 adds `legal_holds (id, hospital_id, placed_at, lifted_at, reference)`, written only by `mhai_provision` through `core hospitals legal-hold place|lift <hospital> --reference <ref>`, audited. `mhai_hospital_on_hold(hid)` is the one reader. When the legal-hold plan lands, it writes this table (or replaces the function's body).
11. **Hospital removal: a hook, not the flow.** The two-person removal flow isn't built. Migration 00025 adds `emr_remove_hospital_data(hid)` (definer, `mhai_provision` only, refused unless the hospital is `removal_scheduled`), and `core emr remove-hospital-data <hospital>` calls `emr.v1.ctl.wipe_credentials` for each connection first and refuses to delete data if any wipe isn't confirmed. The removal flow calls the same function when built.
12. **EMR dead letters reach the Failed jobs view as `jobs` rows; retry is `emr.v1.ctl.redrive`.** `jobs.type` allows only `approve_correction`, and a jobs retry re-mints a JOBS token. Migration 00023 widens the `CHECK` to `emr_normalise` and `emr_rules`; core's dead-letter handler records an `emr_*` letter as a `failed` row (`subject_id` = the letter's batch id); retry calls the new control op `emr.v1.ctl.redrive {stage, batch_id}` (platform admin, step-up as `jobs/retry`), and the emr service re-publishes from its own rows. `platform.JobTypes` gains the two types: a contract addition for mhai-web (PR 10 handoff).
13. **Real code tables wait on H6; PR 5 merges with synthetic ones.** The loader reads `services/emr/codes/{loinc,atc}.csv`. Until H6, the repository holds `services/emr/codes/testdata/` (a few dozen synthetic rows marked `synthetic`), and the service refuses synthetic tables outside `APP_ENV=development`. Task 21 commits the real tables after the licences are accepted.
14. **Rule set v1 waits on H4; PR 7 merges with a synthetic rule set.** `services/emr/rules/testdata/ruleset-0-synthetic.yaml` (thresholds marked `source: synthetic`) drives the engine's tests and the end-to-end run; the loader refuses a `-synthetic` set outside development; with `EMR_RULE_SET_ACTIVE` empty every finding is `shadow`. Task 30 adds `ruleset-1.yaml` from the clinical lead.
15. **`egress-emr` reads its allow-list from a file the emr service writes.** The proxy has no database or NATS access: the emr service writes `allow.json` (connection id, hospital id, host:port list, declared CIDRs) atomically to a volume mounted read-only in the proxy, within 30 s of a change; the proxy reloads on change and refuses everything if the file is missing, stale (over 120 s) or malformed.
16. **The record link is built by `gokit/recordlink`, at read time in core.** `Connector.RecordLink` delegates to it, and core builds `emr_link` from the stored template, base address and source patient id (§5.1, E16).
17. **`EMR_ALLOW_NULL_MRN` is core's setting, named `CORE_EMR_ALLOW_NULL_MRN`**, because core's consumer raises the alert.
18. **Core creates the `EMR` stream and all three durables** (`emr_normalise`, `emr_rules`, `core_emr`), as it does for `JOBS` and `DEAD`. The emr user gets only `INFO`, `MSG.NEXT` and `ACK` on its own two durables, as §3.2 lists.

---

## Running the tests

Never on this machine. Go tests, testcontainers, Docker builds and compose run on the steven-reid build server (`steven-reid@75.46.59.136`), with mt5-lab as the fallback (`REMOTE_HOST`/`REMOTE_KEY`; the runner then caps the job at 2 CPUs and 8 GB, one job at a time). GitHub Actions runs the full gate as confirmation.

The runner is `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`. It rsyncs the checkout to `~/claude-work/<DEST>/` and runs the commands from stdin in `/work` inside `golang:1.24` with the host's Docker socket. Each step on its own line ending `|| exit 1`, the run wrapped in `timeout`:

```bash
SRC=<your worktree>/ DEST=mhai-emr \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/gokit || exit 1
go vet ./... && go test -race ./... || exit 1
cd ../emr || exit 1
go vet ./... && go test -race ./... || exit 1
go test -race -tags integration ./... || exit 1
cd ../core || exit 1
go vet ./... && go test -race ./... || exit 1
go test -race -tags integration ./... || exit 1
CMDS
```

"**Run (gokit|emr|core, remote):** `<cmd>`" means put `<cmd>` after the matching `cd`. emr integration tests start Postgres with testcontainers and apply `../core/internal/db/migrations` with goose (`emr/internal/testpg`), and start NATS with core's dev config. The egress tests (Task 15) run `deploy/egress-emr/test-egress.sh` remotely against the compose profile. Nothing calls a real hospital system.

---

## File structure

```
proto/mhai/emr/v1/control.proto, events.proto     ctl requests/replies; fetched, created, finding, signal, dead letter (ids only)
services/gokit/                                    module github.com/Measured-Health/mhai-multiagent/services/gokit
  reqtoken/        moved from core; SubjectRules (Task 2)
  replay/          Set (Task 2)
  dbx/             WithHospital(ctx, pool, role, hid, fn), roles (Task 3)
  logx/            moved from core (Task 3)
  emrops/          Ops, Lookup (Task 4)
  emrpb/           generated from proto/mhai/emr/v1 (Task 4)
  emrurl/          CheckURL, Origin, OriginsOf (Task 7)
  seal/            Seal, Open, Binding (Task 8)
  recordlink/      Build (Task 25)
services/emr/                                      module …/services/emr; Dockerfile; README.md
  cmd/emr/         main: serve, keys seal-init, unmapped (Tasks 4, 5, 18)
  cmd/egress-emr/  the proxy (Task 13)
  internal/config/ Settings.FromEnv (Task 4)
  internal/ctl/    responder: verify, replay, emrops, handlers (Tasks 4, 10, 38)
  internal/connector/  Connector, TypeInfo, Creds, registry; stub/ (Task 9); fhir/ (22–23); fileexport/ (43–44); archive/ (46)
  internal/secrets/    Store, FileStore (Task 10), managed (Task 12)
  internal/egress/     HTTP and SFTP clients, allow-list writer (Task 14)
  internal/sink/       RawSink, allow-list, text cleaning (Task 17)
  internal/codes/      tables, maps, units, conversions (Task 18)
  internal/normalise/  consumer, parsing, supersession (Task 19)
  internal/index/      patient index and proposals (Task 20)
  internal/scheduler/  polling, locks, cursor, health (Task 24)
  internal/rules/      ruleset, eval, consumer, breaker (Tasks 27–29)
  internal/signals/    id-only signal publisher (Task 20)
  internal/synth/      synthetic patients and a fake FHIR server (Task 25)
  internal/testpg/     migrated Postgres for integration tests (Task 4)
  codes/  maps/  rules/                              CODEOWNERS: clinical reviewers (Task 18)
services/core/
  internal/db/migrations/00021_emr_roles.sql … 00025_emr_retention.sql (00026_emr_documents.sql in PR 12)
  internal/db/emr_*_integration_test.go, alerts_integration_test.go
  internal/emrctl/          client: mint, seal, call (Tasks 5, 11)
  internal/platform/emrconnections.go (+ tests)   /platform/emr/* (Task 11, 37)
  internal/alerts/          settings, alerts, notification settings, matches, exclusions routes (Tasks 32–34); consumer.go (35–37)
  internal/emrmaint/        purge and retention loop (Task 40)
  internal/intel/deadletter.go                   emr letters (Task 38)
  internal/httpx/idempotency.go                  HMAC request hash for credential routes (Task 11)
  internal/audit/schemas.go, catalogue.go        the §12.6 actions
  internal/bus/streams.go                        EMR stream and durables (Task 5)
  internal/config/config.go                      CORE_EMR_* (Tasks 5, 11, 35)
  cmd/core/main.go, seed.go                      EMR_DB_USER login; emr exclude; legal-hold; remove-hospital-data
  openapi.yaml                                   additive routes and fields (minor bumps)
services/nats/nats.conf, deploy/nats/nats.conf   user emr; gateway rights (Task 5)
docker-compose.yml, deploy/compose.yml          profile emr; egress-emr network; build context services/ (Tasks 3, 5, 15)
deploy/egress-emr/test-egress.sh                (Task 15)
.github/workflows/ci.yml, deploy.yml, .github/CODEOWNERS, Makefile
docs/adr/0009-…md, docs/adr/0014-separate-emr-service-and-document-route.md
docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md, docs/specs/2026-10-17-frontend-requirements.md
AGENTS.md, CONTRIBUTING.md, ARCHITECTURE.md, TODO.md, CHANGELOG.md, deploy/README.md
```

---

## Tasks

Each task's first step writes its abuse-case tests (A-numbers are the design's, K-numbers this plan's, listed under "Abuse cases → tests"), and they must fail before the code exists. Every task ends with `go vet`, the module's tests, and a commit staged by path.

### PR 1: docs

### Task 1: ADRs, the amendment note, layout docs and the TODO gates

**Files:**
- Modify: `docs/adr/0009-integrate-many-emr-systems.md` (Status: Accepted, 7 Nov 2026, link to the design), `docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md` (an amendment note at the top and in §2 and §10: "propose codes for review; normalisation by the emr service"; `records.v1.<hospital>.created` becomes `emr.v1.<hid>.fetched` published by core; `mhai_worker` gets `INSERT` on `emr.raw_records` only), `docs/specs/2026-10-17-frontend-requirements.md` (§14.11 record connections and the §12 detail from Plan 13, as additive requirements, with `sources_status`, `arrived_late`, `preliminary`, `amended`, `withdrawn`, grouped mode, nullable MRN, possible duplicates, exclusions), `AGENTS.md` and `CONTRIBUTING.md` (layout: "EMR plumbing" moves from `services/core/` to `services/emr/`; `services/gokit/`), `TODO.md`, `CHANGELOG.md`
- Create: `docs/adr/0014-separate-emr-service-and-document-route.md` (the separate service with its own credentials and egress, §3; the one normaliser and the reviewed document route, §5.3, §9.0)

- [ ] **Step 1:** Write the two ADR changes and the notes above.
- [ ] **Step 2:** In `TODO.md`, replace the "EHR/EMR integration … isn't finished" line with "designed and planned (links)", and add one human-owned item per gate of §17.6 and per H1–H9, each naming its owner and what it blocks (the table under "What's needed from people"). Mark them clearly as people items an agent never closes.
- [ ] **Step 3:** Check the links resolve (`grep -o '](.*)'` on the changed files).
- [ ] **Step 4:** Commit: "Accept ADR 0009, add ADR 0014 and record the EMR gates".

### PR 2: skeleton

### Task 2: `services/gokit/`: the request token with a subject table, and a replay set

**Files:**
- Create: `services/gokit/go.mod` (module `github.com/Measured-Health/mhai-multiagent/services/gokit`, go 1.24), `gokit/reqtoken/` (moved by `git mv` from `services/core/internal/reqtoken/`, in its own commit with no edits), `gokit/reqtoken/subjects.go`, `subjects_test.go`, `gokit/replay/replay.go`, `replay_test.go`
- Modify: every core import of `internal/reqtoken` (`go.mod` `require … gokit` with `replace ../gokit`), `testdata/request-tokens/` (new vectors)

**Interfaces:**
- `reqtoken.SubjectRule{Kind Kind; Prefix string; AudFromSegment int; Aud string; HospitalSegment int}` and `var SubjectRules = []SubjectRule{…}`: `inference.v1.<aud>.<op>` (req, aud from segment 2); `emr.v1.ctl.<op>` (req, aud `emr`); `jobs.v1.<hid|global>.<type>` (job, as today).
- `reqtoken.AudienceFor(kind Kind, subject string) (aud string, ok bool)`; `validateMint` and `VerifyAt` call it in place of the hard-coded `inference` checks. Behaviour for existing subjects is unchanged.
- `replay.New(window time.Duration, max int) *Set`; `(*Set).SeenOrAdd(jti string, exp time.Time) (seen bool, err error)`; `ErrFull` at the bound (fail closed: the caller refuses).

- [ ] **Step 1: Write the failing tests:** `TestSubjectRulesEMRCtl` (mint and verify `emr.v1.ctl.test` with aud `emr`; aud `pipeline` on that subject refused at mint and at verify); `TestSubjectRulesUnchangedForInferenceAndJobs` (every existing vector still verifies as before); new vectors `valid_emr_ctl.json`, `wrong_aud_emr_ctl.json`, `emr_ctl_wrong_shape.json` (`emr.v1.ctl` with 3 segments, `emr.v1.ctl.test.x`); `TestReplaySetSeenFullAndExpiry` (a jti seen twice is `seen`; past `exp` plus window it is forgotten; at `max` returns `ErrFull`). The Python vector test (`tests/boundary/test_verify_vectors.py` or its current name) loads the new vectors and refuses the `emr` ones by audience (the worker's aud is `pipeline`).
- [ ] **Step 2: Run (gokit, core, remote):** `go test ./reqtoken/... ./replay/...` → fail; core builds against gokit.
- [ ] **Step 3:** Implement the table and `AudienceFor`; the replay set as `grant`'s (bounded map plus expiry sweep, under a mutex).
- [ ] **Step 4:** Run gokit's tests, core's full unit suite and the Python vector test (remote).
- [ ] **Step 5:** Commits: "Move reqtoken to services/gokit" (the move only), then "Allow emr.v1.ctl subjects in request tokens and add a replay set".

### Task 3: `dbx`, `logx` in gokit; core builds from `services/`

**Files:**
- Create: `gokit/dbx/dbx.go`, `dbx_integration_test.go`
- Move: `services/core/internal/logx/` → `gokit/logx/` (move-only commit)
- Modify: `services/core/internal/db/db.go` (`WithHospital` and `WithHospitalUser` call `dbx`), `services/core/Dockerfile` (paths relative to `services/`), `docker-compose.yml`, `docker-compose.smoke.yml`, `.github/workflows/deploy.yml` and `ci.yml` (build context `services`, file `core/Dockerfile`), `Makefile`

**Interfaces:**
- `dbx.Role` with constants `Gateway = "mhai_gateway"`, `EMR = "mhai_emr"`, `EMRCore = "mhai_emr_core"`; `dbx.WithHospital(ctx, pool *pgxpool.Pool, role Role, hospitalID uuid.UUID, fn func(pgx.Tx) error) error` (sets `ROLE` and `mhai.hospital_id` transaction-local, as core's `inRole`; refuses `uuid.Nil` with `ErrNoHospital` and any role outside the constants).

- [ ] **Step 1: Write the failing tests:** `TestWithHospitalRoleIsClosed` (an unknown role string is refused before any SQL); `TestWithHospitalResetsBetweenTransactions` (one pooled connection, hospital A then B, no row of A visible in B, as core's `TestRLSWithoutHospitalSeesNothing` does); core's `compose_test.go` asserts the core build context is `services` and the Dockerfile path `core/Dockerfile`.
- [ ] **Step 2: Run (gokit, core, remote):** fail.
- [ ] **Step 3:** Implement; switch core over; update every build context.
- [ ] **Step 4:** Run core's full unit and integration suites and `docker build -f services/core/Dockerfile services` (remote).
- [ ] **Step 5:** Commits: "Move logx to services/gokit" (move only), "Share WithHospital through gokit and build core from services/".

### Task 4: The emr binary, settings and the control responder

**Files:**
- Create: `proto/mhai/emr/v1/control.proto` (`HealthRequest/Reply`, and the messages later tasks fill: `TestRequest/Reply`, `PutCredentialsRequest/Reply`, `WipeCredentialsRequest/Reply`, `SyncNowRequest/Reply`, `RedriveRequest/Reply`), `gokit/emrpb/` (generated; `buf.gen.yaml` gains the output), `gokit/emrops/emrops.go`, `emrops_test.go`, `services/emr/go.mod`, `services/emr/Dockerfile`, `services/emr/README.md`, `services/emr/cmd/emr/main.go`, `services/emr/internal/config/config.go` (+ test), `services/emr/internal/ctl/responder.go` (+ test), `services/emr/internal/testpg/testpg.go`, `services/emr/internal/imports_test.go`
- Modify: `.github/workflows/ci.yml` (jobs `gokit` and `emr`: gofmt, vet, staticcheck, test, integration), `services/core/internal/authz/generate.go` test (no `emr.` row in `operations.json`)

**Interfaces:**
- `emrops.Op{Name, Op string; AnyPlatformRole []string; SvcCore, StepUp bool; Timeout time.Duration; Request, Response string}`; `emrops.Ops` = `emr.test` (`platform_admin`, 30 s), `emr.put_credentials` (`platform_admin`, StepUp, 10 s), `emr.wipe_credentials` (`platform_admin`, 10 s), `emr.redrive` (`platform_admin`, StepUp, 10 s), `emr.health` (SvcCore, 5 s), `emr.sync_now` (SvcCore, 10 s); `emrops.Lookup(subject string) (Op, bool)`; `emrops.Subject(op string) string` = `"emr.v1.ctl." + op`.
- `config.Settings` from env: `EMR_NATS_URL`, `EMR_NATS_NKEY_FILE` or `EMR_NATS_PASSWORD` (development only), `EMR_DATABASE_URL`, `EMR_TRUSTED_KEYS_PATH`, `EMR_SEAL_KEY_PATH`, `EMR_SECRETS_DIR`, `EMR_PROXY_URL`, `EMR_PROXY_KEY_PATH`, `EMR_RULE_SET_ACTIVE`, `APP_ENV`. Refusal messages name the variable only.
- `ctl.Responder.Serve(ctx)`: subscribes `emr.v1.ctl.>` in queue group `emr`; for each message, in order: size, `reqtoken.VerifyAt` with `Expect{Aud: "emr", Subject: m.Subject, Kind: req}`, `CheckReply(c, m.Reply, false)`, replay, `emrops.Lookup`, the op's caller rule (`SvcCore` needs `sub = svc:core`; otherwise `sub` is a uuid and `roles` holds one of `AnyPlatformRole`), the protobuf schema; any failure: no reply, a log line with the reason code and `rid` only, a metric.
- `health` replies `{database_ready, secret_store_ready, rule_set_version, proxy_allow_list_age_s}`.

- [ ] **Step 1: Write the failing tests:** `TestCtlRefusals` (A4: a token for another subject, another body, `aud pipeline`, expired `exp`, a replayed `jti`, a reply subject outside `_INBOX_gateway.`, `put_credentials` with `sub = svc:core`, `health` with a user `sub`; each gets no reply and no handler runs — a counting fake handler); `TestHealthAsSvcCore`; `TestSettingsRefusals` (missing variables; `EMR_NATS_PASSWORD` outside development); `TestNoEMROpsInOperationsJSON` (K-case in core); `TestEMRImportsNoCoreModule` (K12: `go list -deps ./...` of the emr module has no `services/core` path; `go.mod` requires no core module).
- [ ] **Step 2: Run (emr, gokit, core, remote):** fail.
- [ ] **Step 3:** Implement; `buf generate`; `buf lint` and `buf breaking` pass.
- [ ] **Step 4:** Run all three modules' suites.
- [ ] **Step 5:** Commit: "Add the emr service with a verified control responder and health".

### Task 5: NATS user, the `EMR` stream, compose profile, sealing key pair, core's health reading

**Files:**
- Modify: `services/nats/nats.conf` and `deploy/nats/nats.conf` (user `emr` exactly as §3.2; `gateway` gains publish `emr.v1.ctl.>`, `$JS.API.STREAM.{CREATE,UPDATE,INFO}.EMR`, `$JS.API.CONSUMER.{CREATE,DURABLE.CREATE,INFO}.EMR.>`, `$JS.API.CONSUMER.MSG.NEXT.EMR.core_emr`, `$JS.ACK.EMR.core_emr.>`; no `emr.v1.*.fetched` until PR 12), `services/core/internal/bus/streams.go` (+ test), `services/core/internal/config/config.go` (+ test: `CORE_EMR_ENABLED`, `CORE_EMR_SEAL_PUBLIC_KEY_PATH`), `services/core/internal/platform/sampler.go` (an `emr` part when enabled), `services/core/cmd/core/main.go` (`EMR_DB_USER`/`EMR_DB_PASSWORD` → login `emr_app`, role `mhai_emr_app`, after migration 00021 exists: the login request is skipped with a log line until the role exists), `docker-compose.yml`, `deploy/compose.yml` (service `emr`, profile `emr`, volume `emr-secrets` mounted only by `emr`), `services/core/cmd/core/compose_test.go`, `services/emr/cmd/emr/keys.go`

**Interfaces:**
- `bus.EnsureEMRStream(ctx, js, replicas int) error`: stream `EMR` (subjects `emr.v1.*.fetched`, `emr.v1.*.finding`, `emr.v1.*.signal`, `records.v1.>`; file storage; `MaxAge` 7 days; `MaxMsgsPerSubject` 50000; `DiscardNewPerSubject`; duplicate window 2 minutes) and durables `emr_normalise` (`emr.v1.*.fetched`), `emr_rules` (`records.v1.*.created`), `core_emr` (`emr.v1.*.finding`, `emr.v1.*.signal`), each `MaxDeliver` 5. Called by `core serve` only when `CORE_EMR_ENABLED`.
- `emr keys seal-init --out <dir>`: writes `seal-<kid>.key` (0400) and `seal-<kid>.pub` (0444) for X25519; refuses to overwrite.

- [ ] **Step 1: Write the failing tests:** `TestEMRUserPermissions` (bus integration, as `TestReplySubjectIsTheResponderToCheck`: `emr` can't publish `api.v1.>`, `jobs.v1.>`, `inference.v1.>`, `$KV.>`, can't create a stream or consumer, can only pull its own two durables; `gateway` can publish `emr.v1.ctl.test`); `TestEMRStreamNotCreatedWhenDisabled` (K4); `TestEMRStreamConfig` (limits and discard policy as above); `TestComposeEMRProfile` (the emr service and its volume exist only under profile `emr`; only `emr` mounts `emr-secrets`; ports bound to 127.0.0.1); `TestSealInitModes`.
- [ ] **Step 2: Run (core, emr, remote):** fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run; `docker compose --profile emr config` validates (remote).
- [ ] **Step 5:** Commit: "Give the emr service its NATS user, the EMR stream and a compose profile".

**Off by default after PR 2:** no profile, no stream, no connection; core runs exactly as before.

### PR 3: connections

### Task 6: Migration 00021: roles, connections, credential metadata, sync runs

**Files:**
- Create: `services/core/internal/db/migrations/00021_emr_roles.sql`, `services/core/internal/db/emr_roles_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestRLSWithoutHospitalSeesNothing` lists `emr_connections`, `emr.credentials`, `emr.sync_runs`, `emr.connection_errors`), `services/core/internal/db/maintenance.go` (`loginRoles` gains `mhai_emr_app`)

**Interfaces (SQL):**
- Schema `emr` owned by `mhai_migrate`; roles (created if missing, never dropped): `mhai_emr` (NOLOGIN), `mhai_emr_app` (NOLOGIN NOINHERIT; `GRANT mhai_emr TO mhai_emr_app WITH INHERIT FALSE`), `mhai_emr_sched`, `mhai_emr_core`, `mhai_emr_admin`.
- `public.emr_connections` as §18 item 1, plus `CHECK (NOT (settings ?| ARRAY['api_key','private_key','shared_key','access_key','password','secret','token','client_secret']))`, `CHECK (poll_seconds >= 60)`, `CHECK (cardinality(private_cidrs) = 0 OR private_network)`, `source_label` 1–40, `base_url` ≤ 500, `UNIQUE (id, hospital_id)`.
- `emr.connection_ranges (connection_id, hospital_id, cidr cidr, EXCLUDE USING gist (cidr inet_ops WITH &&, hospital_id WITH <>))`, kept equal to `private_cidrs` by a trigger, so two hospitals' ranges can't overlap (A37). `btree_gist` extension created if missing.
- `emr.credentials (connection_id, hospital_id, field, set_at, secret_version, set_by, origin text[])`, `emr.sync_runs (id, hospital_id, connection_id, started_at, finished_at, ok, records, outcome)`, `emr.connection_errors (hospital_id, connection_id, at, code)` (closed codes).
- Grants: `mhai_platform` `SELECT, INSERT, UPDATE` on `emr_connections` (config columns), `EXECUTE` on `emr_platform_health()` (counts and codes per connection) and `emr_credentials_meta(connection_id)` (field, `set_at`); `mhai_emr` under RLS `SELECT (id, hospital_id, type, base_url, settings, enabled, paused, version, poll_seconds, credentials_origin, private_cidrs)` on `emr_connections`, `SELECT (id, status)` on `hospitals`, `SELECT, INSERT, UPDATE, DELETE` on `emr.credentials`, `SELECT, INSERT, UPDATE` on `emr.sync_runs`, `INSERT` on `emr.connection_errors`; `mhai_emr_sched` `SELECT (id, hospital_id, enabled, paused, poll_seconds)` on enabled connections, `USING (true)`.
- `platform_append` (00018's list) recreated with `emr.connection_added`, `emr.connection_changed`, `emr.connection_switched_on`, `emr.connection_switched_off`, `emr.connection_tested`, `emr.source_resumed`, same conditions; Down restores 00018's exact list.

- [ ] **Step 1: Write the failing tests:** `TestEMRConnectionsRefuseSecretKeysInSettings`; `TestEMRRangesCannotOverlapAcrossHospitals` (A37 database part: an overlap across hospitals is refused; within one hospital it is allowed, per §6.4); `TestEMRRoleGrants` (A2: `mhai_platform`, `mhai_gateway` and `mhai_emr` read no credential value because none exists, and `mhai_gateway` has no grant on `emr_connections` or `emr.credentials`); `TestEMRRLSWithHospital` (A13 for these tables: with A set, no row of B; an insert naming B fails `WITH CHECK`); `TestEMRAppLoginIsNoInherit` (`emr_app` can `SET ROLE mhai_emr`, doesn't inherit it); `TestPlatformAppendAllowsEMRConnectionActionsOnly` (K3); `TestDefinersIgnoreTempTables` (K2: a temp `emr_connections` created by `mhai_platform` doesn't change `emr_platform_health()`); `TestMigration00021DownUp`.
- [ ] **Step 2: Run (core, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration following the definer pattern.
- [ ] **Step 4:** Run the whole `internal/db` integration suite.
- [ ] **Step 5:** Commit: "Add the emr schema, its roles, connections and credential metadata".

### Task 7: URL and origin checks (`gokit/emrurl`)

**Files:** Create `services/gokit/emrurl/emrurl.go`, `emrurl_test.go`

**Interfaces:**
- `type Origin string` (`scheme://host:port`, port always explicit).
- `emrurl.CheckURL(raw string, allowed []string /* "https", "sftp" */, development bool) (*url.URL, Origin, error)`: ≤ 500 characters; scheme in `allowed`; a DNS name (no IP literal unless `development`); no userinfo, query or fragment; host lowercased, IDNA-normalised, no trailing dot; closed errors `ErrScheme`, `ErrShape`, `ErrAddress`.
- `emrurl.OriginsOf(connType, baseURL string, settings map[string]string, development bool) ([]Origin, error)`: base address plus `token_url` for `fhir_r4`; `record_link` must be on the base host (`ErrLinkHost`).

- [ ] **Step 1: Write the failing tests:** `TestCheckURLRefusals` (A5 save: `https://10.0.0.5`, `https://169.254.169.254`, `https://[::1]`, `https://localhost`, `https://user:pw@host`, `http://host`, `https://host/?q=1`, `https://host/#f`, 501 characters, `https://HOST.` normalised); `TestOriginsOfFHIR` (token on another host joins the origin); `TestRecordLinkMustShareHost`.
- [ ] **Step 2–4:** Run (gokit) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Check EMR addresses and derive their credential origin in one place".

### Task 8: Sealing with a bound box (`gokit/seal`)

**Files:** Create `services/gokit/seal/seal.go`, `seal_test.go`

**Interfaces:**
- `seal.Binding{ConnectionID uuid.UUID; Field string; Origin []emrurl.Origin}`.
- `seal.Seal(pub *[32]byte, kid string, b Binding, value []byte, now time.Time) ([]byte, error)`: `nacl/box.SealAnonymous` over a strict JSON `{connection_id, field, origin, issued_at, value}`; output `kid || box`. `value` 1–4000 bytes.
- `seal.Open(keys map[string]*[32]byte /* public→private by kid, at most two */, box []byte, want Binding, now time.Time) (Secret, error)`: refuses an unknown kid, a different connection, field or origin (set equality), `issued_at` older than 60 s or more than 5 s ahead; `Secret` has a redacting `String`, `Format`, `GoString`, `MarshalJSON` and `Bytes() []byte`, and `Zero()`.

- [ ] **Step 1: Write the failing tests:** `TestOpenRefusesReplayedBox` (A35: other connection, other field, changed origin, 61 s old, unknown kid; each refused, nothing returned); `TestSecretRedacted` (`fmt.Sprint`, `%#v`, `%+v`, `json.Marshal`, `slog` attribute all show `[redacted]`); `TestTwoKeysDuringRotation` (§6.3: a box sealed to the old key opens while both are loaded).
- [ ] **Step 2–4:** Run (gokit) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Seal credentials to the emr service, bound to connection, field and origin".

### Task 9: The connector interface, `Creds` and the stub connector

**Files:** Create `services/emr/internal/connector/connector.go`, `registry.go`, `creds.go`, `connector_test.go`, `ast_test.go`, `stub/stub.go`, `stub/stub_test.go`

**Interfaces:**
- `Connector` exactly as §4, with `Config{ConnectionID, HospitalID uuid.UUID; Type, BaseURL string; Settings map[string]string; Origin []emrurl.Origin; PollSeconds int}`, `Cursor []byte`, `TestResult{OK bool; Code string}` (closed codes), `RawSink` (Task 17 fills it; here an interface `Put(ctx, RawRecord) error`).
- `TypeInfo{ID, Label, Summary string; Version int; Fields []Field}`, `Field{Key, Label, Kind /* text|secret */ string; Required bool; Max int}`.
- `Creds` with unexported fields, `Get(field string) seal.Secret`, redacting `String`, `Format`, `GoString`, `MarshalJSON`, `LogValue`.
- `registry.Offered(enabled []string) []TypeInfo`; the stub type `stub` exists only when `APP_ENV=development`.

- [ ] **Step 1: Write the failing tests:** `TestCredsRedacted` (A3: every formatting path); `TestNoConnectorLogsCreds` (A3, AST: in `internal/connector/...`, any call to `log.*`, `slog.*`, `fmt.Print*`, `fmt.Fprint*` or a `*slog.Logger` method with an argument of type `Creds`, `seal.Secret` or a field selected from them fails; a fixture package that does it is detected); `TestStubOnlyInDevelopment`; `TestTypeIDsNameNoProduct` (ids and labels checked against the `FORBIDDEN_TERMS` list copied from mhai-web into `connector/forbidden.txt`).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Define the connector interface with redacted credentials and a development stub".

### Task 10: The secret store (file store), `put_credentials`, `wipe_credentials` and `test` in the emr service

**Files:** Create `services/emr/internal/secrets/store.go`, `filestore.go`, `filestore_test.go`; `services/emr/internal/ctl/credentials.go`, `test.go`, `credentials_integration_test.go`

**Interfaces:**
- `secrets.Store{Put(ctx, path Path, value seal.Secret) (version string, err error); Get(ctx, path Path) (seal.Secret, error); DeleteAll(ctx, connection uuid.UUID) error; DeleteVersionsBefore(ctx, path Path, keep time.Duration) error; Ready(ctx) error}`; `Path{Env string; Hospital, Connection uuid.UUID; Field string}` → `mhai/<env>/emr/<hospital>/<connection>/<field>`.
- `secrets.NewFileStore(dir string, appEnv string) (*FileStore, error)`: refuses `APP_ENV=production`; files 0400, dir 0700, owner check; writes via temp file and rename; keeps the previous version 24 h.
- `put_credentials`: opens each box (`seal.Open` with the connection's current origin read under `WithHospital(mhai_emr)`), writes the store, upserts `emr.credentials` metadata, replies `[{field, set_at}]`. Any refusal stores nothing (all fields or none).
- `wipe_credentials`: deletes every secret and metadata row of the connection; idempotent; replies `{wiped: true}` only after both.
- `test`: loads the config and creds; if any required secret is unset for the current origin, replies `sign_in_refused` without calling the connector (A28); otherwise `Connector.Test` with a 20 s deadline; records nothing but the reply (core records `last_test`).

- [ ] **Step 1: Write the failing tests:** `TestFileStoreRefusedInProduction`; `TestFileStoreModes`; `TestPutCredentialsAllOrNothing` (A35 path: one bad box among two stores neither); `TestPutCredentialsReplyHasSetAtOnly` (A1 emr side); `TestWipeThenTestSendsNothing` (A28 emr side: a capturing stub records zero calls, outcome `sign_in_refused`); `TestPlaceholderSecretRefused` (§17.4: an empty or `changeme` value marks the connection `sign_in_refused`).
- [ ] **Step 2–4:** Run (emr, integration) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Store connection secrets in the emr service only, write-once per request".

### Task 11: Core's platform connection routes, sealing at the edge, HMAC idempotency

**Files:**
- Create: `services/core/internal/emrctl/client.go` (+ `_integration_test.go`), `services/core/internal/platform/emrconnections.go`, `emrconnections_test.go`, `emrconnections_integration_test.go`
- Modify: `internal/platform/audit.go` (`Register` calls `registerEMRConnections`; `Deps.EMR *emrctl.Client`, `Deps.EMREnabled bool`, `Deps.EMRTypes []string`), `internal/server/server.go`, `internal/config/config.go` (`CORE_EMR_CONNECTOR_TYPES`, `CORE_IDEMPOTENCY_HMAC_KEY_PATH`), `internal/httpx/idempotency.go` (+ tests), `internal/audit/schemas.go`, `catalogue.go` (+ tests), `openapi.yaml` (the eight routes of §16.1 and the new error codes, minor bump), `docs/specs/generated/permissions.md` (`go run ./cmd/permgen`)

**Interfaces:**
- `emrctl.Client{Request(ctx, caller Caller, op string, req, resp proto.Message) error}`: mints with aud `emr`, subject `emrops.Subject(op)`, `rpl _INBOX_gateway.`; caller is the platform admin (with `hid` = the connection's hospital) or `svc:core`.
- Routes and rules exactly §13.1 (the `resume` route is added in Task 37). Strict bodies; unknown keys 422 `validation_failed`. `ConnectionIn{hospital: path, type, source_label, base_url, settings, poll_seconds?, private_network?, private_cidrs?, credentials: {field: value}, mfa_code | backup_code}`; `ConnectionPatch` the same, all optional, plus `version`. `source_label` checked against the forbidden terms list; every URL by `emrurl` (A5 at save); `type` in `CORE_EMR_CONNECTOR_TYPES` or 422 `no_type`; hospital `removal_*` or `removed` → 409 `hospital_removed`.
- The handler seals every `credentials` value with `seal.Seal` **before** any other use of the body; the plaintext buffers are zeroed after sealing; the sealed boxes go to `put_credentials`. A PATCH that changes the origin calls `wipe_credentials` first, and the transaction (connection row and audit) commits only after the wipe's reply; then new credentials, if any, are sealed with the new origin (A28).
- `httpx.IdempotentKeyed(pool, log, key []byte)`: `requestHash` becomes HMAC-SHA256 under the key for the routes that carry credentials (POST, PATCH connections); other routes unchanged. Missing key with `CORE_EMR_ENABLED` → core refuses to start.
- Request bodies of the credential routes are never logged or traced: `httpx` and `natsapi` mark them (`httpx.NoBodyLog`), and a test sends a canary.
- Audit schemas for the six `emr.connection_*` actions as §12.6 (keys only for `fields_changed`, closed `outcome`).
- `CORE_EMR_ENABLED=false`: write routes 503 `not_ready`; `connector-types` `{types: []}`; reads answer an empty list.

- [ ] **Step 1: Write the failing tests:** `TestCredentialNeverStoredOrLogged` (A1: a canary secret in POST and PATCH appears in no log record, span attribute, error body, `idempotency_keys` row, `audit_log` payload, NATS message other than the sealed box (a NATS tap decrypts nothing: the box is opaque) or reply); `TestIdempotencyHashIsKeyedForCredentialRoutes`; `TestOriginChangeWipesAndSwitchesOff` (A28 core side: PATCH `base_url` → `wipe_credentials` called before commit, connection off, every `set_at` null, audit `credentials_wiped: true`; if the wipe fails, the PATCH fails and nothing changes); `TestConnectionRoutesRoles` (auditor reads, can't write; clinical audience 403; no step-up 403 `step_up_required`); `TestSwitchOnNeedsPassingTestOfSavedSettings` (409 `not_tested`); `TestVersionConflict`; `TestAuditSchemasEMRConnection` (A21 part: unknown key refused); `TestEMRDisabledAnswersNotReady` (K4); the openapi contract test and the permissions table.
- [ ] **Step 2: Run (core, remote):** unit and integration for `platform`, `httpx`, `audit`, `config`, `emrctl` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run core's whole suite, then the cross-service test `TestConnectionLifecycleWithStub` (integration, `-tags integration` in `emrctl`, with the emr binary built from `../emr` and run against the same NATS and Postgres): create, test, switch on, PATCH a secret, PATCH the origin, test again.
- [ ] **Step 5:** Commit: "Serve the platform connection routes, sealing credentials at core's edge".

### Task 12: The managed secret store (waits on H3)

**Files:** Create `services/emr/internal/secrets/managed.go`, `managed_test.go` (against the provider's local emulator or a recorded fake transport), `deploy/README.md` section "EMR secrets"

**Interfaces:** `secrets.NewManaged(ctx, provider string, region string) (*Managed, error)` implementing `Store`; path policy documented for the emr identity (`mhai/<env>/emr/*` read, write, delete) and verified at start by a write-read-delete of `mhai/<env>/emr/_probe`; `APP_ENV=production` requires it.

- [ ] **Step 1:** Write `TestManagedStoreContract` (the same contract suite as the file store, shared as `storetest.Run(t, Store)`), `TestProductionRefusesFileStore`.
- [ ] **Step 2–4:** Run → fail; implement for the provider H3 chose; pass.
- [ ] **Step 5:** Commit: "Keep EMR secrets in the managed secret store in production".

**Off by default after PR 3:** `CORE_EMR_ENABLED=false` and no connector type offered; the routes exist and refuse writes.

### PR 4: egress

### Task 13: `egress-emr`: resolve once, check, pin; per-connection authorisation

**Files:** Create `services/emr/cmd/egress-emr/main.go`, `proxy.go`, `allowlist.go`, `resolve.go`, `proxy_test.go`; `services/emr/Dockerfile` builds both binaries

**Interfaces:**
- `allow.json`: `{generated_at, connections: [{id, hospital_id, hosts: ["host:port"], cidrs: ["10.1.2.0/24"]}]}`; reloaded on change; missing, malformed or older than 120 s → every CONNECT refused (fail closed).
- On `CONNECT host:port`: `Proxy-Authorization: Basic base64(<connection_id>:<hex HMAC-SHA256(K_proxy, connection_id)>)` checked constant-time (407 otherwise); host:port must be in that connection's `hosts` (403); port 443, or 22 for an sftp origin; one `net.Resolver.LookupNetIP`; refuse (403) if **any** address is private, loopback, link-local, CGNAT (100.64/10), multicast, unspecified, `169.254.169.254`/`fd00:ec2::254`, or inside the configured `backend` and `egress-emr` subnets, unless inside **this** connection's `cidrs`; then dial the first checked address (`net.Dialer` on the `netip.AddrPort`, no second lookup); copy bytes with an idle timeout of 60 s.
- Logs: connection id, decision code, port, and the host as a hash only (§14.3).

- [ ] **Step 1: Write the failing tests:** `TestCONNECTNotOnAllowList` (A6); `TestRebindingPinned` (A29: a test resolver answers a public address first and `169.254.169.254` second; the proxy resolves once and the dialer receives the first address; a resolver returning one public and one private address in one answer is refused); `TestCrossConnectionRefused` (A37: A's credential to B's host or to B's range → 403; no header, forged token, A's id with B's token → 407); `TestPrivateOnlyWithinDeclaredRanges` (an address in `backend` refused even when declared); `TestStaleAllowListRefusesAll` (K5).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Add egress-emr, a CONNECT proxy that pins the address it checked".

### Task 14: The emr HTTP and SFTP clients, and the allow-list writer

**Files:** Create `services/emr/internal/egress/http.go`, `sftp.go`, `allowlist.go`, `egress_test.go`

**Interfaces:**
- `egress.HTTP(conn Config, proxyURL *url.URL, key []byte) *http.Client`: proxy fixed to `egress-emr` with the connection's `Proxy-Authorization` (from `Config.ConnectionID`, never from a connector argument); `CheckRedirect` refuses any redirect to another origin and any redirect at all for a request carrying a credential; TLS 1.2+ with system roots, no custom CA; `MaxResponseHeaderBytes` 64 KiB; body wrapped in `io.LimitReader` at 20 MB (`ErrTooLarge`); `egress.CheckOrigin(req, origin []emrurl.Origin) error` called by a `RoundTripper` before every request that carries a credential (A28: refuse, nothing sent).
- `egress.DecodeJSON(r io.Reader, v any, maxDepth int) error` (64).
- `egress.SFTP(conn Config, fingerprint string) (*sftp.Client, error)`: through the proxy, `ssh.FixedHostKey` from the typed SHA256 fingerprint, never `InsecureIgnoreHostKey` (PR 11 uses it; written here for the shared proxy dial).
- `egress.WriteAllowList(ctx, pool, dir string)`: every 15 s (inside the 30 s bound of §6.4), reading enabled and under-test connections' origins and ranges under `WithHospital(mhai_emr)` per hospital id listed by `mhai_emr_sched`.

- [ ] **Step 1: Write the failing tests:** `TestNoRedirectAcrossHosts` (A6); `TestCredentialOnlyToOrigin` (A28: a capturing fake on another host receives nothing; a `next` link elsewhere is refused before sending); `TestResponseCaps` (A7: a 200 MB body and a 10,000-deep JSON fail with closed errors; nothing stored); `TestAllowListWithin30s`.
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Send connector traffic only through egress-emr, to the credential's own origin".

### Task 15: Networks, compose and the egress test script

**Files:** Modify `docker-compose.yml`, `deploy/compose.yml` (network `egress-emr`; `egress-emr` service on `egress-emr` and a small `emr-proxy` internal network shared with `emr`; `emr` on `backend` and `emr-proxy` only; volume `emr-egress-conf` read-write for `emr`, read-only for the proxy; `K_proxy` file mounted in both), `services/core/cmd/core/compose_test.go`; Create `deploy/egress-emr/test-egress.sh`

- [ ] **Step 1: Write the failing tests:** `TestEMRNeverOnEgressNetwork` (compose: `emr` has no route out; only `egress-emr` joins `egress-emr`); `test-egress.sh` (A6, A29, A37 end to end: from the `emr` container, a CONNECT to a public test host not listed → 403; to `169.254.169.254` → 403; direct dial out without the proxy fails; with a listed host → connected).
- [ ] **Step 2–4:** Run remotely under the profile → fail; implement; pass.
- [ ] **Step 5:** Commit: "Put the emr service behind its own egress network".

**Off by default after PR 4:** both containers are in profile `emr` only.

### PR 5: canonical model, normaliser and patient index

### Task 16: Migration 00022: raw records, canonical tables, patient index and core's patient functions

**Files:** Create `services/core/internal/db/migrations/00022_emr_canonical.sql`, `services/core/internal/db/emr_canonical_integration_test.go`, `services/emr/internal/sqlcheck_test.go`; Modify `db_integration_test.go`

**Interfaces (SQL):** the tables of §8 and §18 item 2 with the `(hospital_id, id)` composite keys; `raw_records` with `document_id`, `review_id` (no foreign key yet, departure 7) and `CHECK (source_kind <> 'document' OR (document_id IS NOT NULL AND review_id IS NOT NULL AND connection_id IS NULL))`; the unique key of §8.1 as an expression index; every canonical table with the provenance columns of §8.2, `superseded_by`, `merge_id`; `emr.patients.id` default `gen_random_uuid()` and `CHECK (id::text NOT IN ('reference','unassigned','unknown','ref'))`; `emr.possible_matches (id, hospital_id, patient_a, patient_b, reason, proposed_at, decided_at, decision)` with `UNIQUE (hospital_id, least(patient_a,patient_b), greatest(…), reason)`; `emr.merges`; `emr.unmapped_codes`.
- Functions (definers, `EXECUTE` to `mhai_gateway` and `mhai_emr_admin` only): `emr_patient_in_hospital(pid uuid) RETURNS boolean`; `emr_merge_patients(match_id uuid, keep text) RETURNS uuid` (locks both patients `FOR UPDATE` in id order, re-checks both active and in `mhai_current_hospital()`, moves links and canonical rows with a new `merge_id`, re-points open alerts once 00024 exists (written defensively: `IF to_regclass('public.alerts')`), returns the merge id; the audit row is written by core in the same transaction); `emr_exclude_patient(pid uuid, reason text) RETURNS uuid` (sets `excluded`; withdraws alerts once 00024 exists; the purge request row arrives with 00025, so here it records nothing else).
- `mhai_emr`: `SELECT, INSERT, UPDATE` on the `emr` data tables under RLS; no `DELETE`; no `EXECUTE` on the functions above.
- `mhai_gateway`: `SELECT (id, display_name, mrn, birth_date, sex, status)` on `emr.patients`; `SELECT` on the alert-facing columns of `emr.observations`, `emr.medications`, `emr.encounters`, `emr.units (id, label, kind, connection_id)`.
- Audit: `gateway_append` already admits its hospital's rows; add schemas for `emr.patients_merged`, `emr.patient_match_rejected`, `emr.patient_excluded` (Task 34 uses them).

- [ ] **Step 1: Write the failing tests:** `TestDocumentRawRecordNeedsReview` (A26); `TestPatientLinkSameHospitalOnly` (a link to another hospital's patient fails the key); `TestRawRecordReplayIsNoOp` (§8.1 unique key); `TestEMRCanonicalRLS` (A13); `TestEMRCannotMergeOrDelete` (`mhai_emr` has no `EXECUTE` on the definers and no `DELETE`); `TestMergeRechecksAndLocks` (two merges racing on the same pair: one wins, the other errors; a merged patient can't be merged again); `TestReservedIDsRefused`; `TestNoCrossHospitalJoinInEMRSQL` (static, `services/emr/internal/sqlcheck_test.go`: every SQL string literal in the emr module that joins `patients` names `hospital_id` in its join condition); `TestDefinersIgnoreTempTables` (K2); `TestMigration00022DownUp`.
- [ ] **Step 2–4:** Run (core, emr) → fail; write the migration; pass.
- [ ] **Step 5:** Commit: "Add the canonical model, the per-hospital patient index and core's merge and exclusion functions".

### Task 17: `RawSink`, the allow-list and text cleaning

**Files:** Create `services/emr/internal/sink/sink.go`, `allowlist.go`, `text.go`, `sink_test.go`, `sink_integration_test.go`

**Interfaces:**
- `sink.New(pool, hospital, connection uuid.UUID, connType string, connVersion int, publish func(ctx, rawIDs []uuid.UUID) error) *Sink`, implementing `connector.RawSink`; `Put` accepts a `RawRecord{Kind, SourceID, SourceVersion string; SourceUpdatedAt time.Time; Fields map[string]any}`, drops every field not on §14.1's list for its kind (counted, `emr_sink_dropped_fields_total{kind}`), caps `SourceID` 256 and `SourceVersion` 64, computes `payload_sha256`, inserts with the unique key (a replay is a no-op), and batches ids for `emr.v1.<hid>.fetched` (at most 500 per message, `Nats-Msg-Id` = the first raw id).
- `sink.CleanText(s string, max int) string`: NFC, removes C0/C1 controls and Unicode `Cf`, strips anything between `<` and `>`, collapses whitespace, caps by runes. Used on every display string.
- `sink.CSVCell(s string) string`: a cell beginning `=`, `+`, `-`, `@`, tab or CR is prefixed with `'` before storage (A7).

- [ ] **Step 1: Write the failing tests:** `TestSinkDropsUnlistedFields` (address, telecom, identifier other than the MRN namespace, contact, practitioner name, `text`, an unlisted extension: none stored); `TestCleanText` (A8: `<script>`, `‮`, `\x00`, 10,000 characters); `TestCSVFormulaNeutralised` (A7); `TestSinkReplayNoOp` (A19 part).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Keep only allow-listed fields from every source, cleaned and capped".

### Task 18: Code tables, maps, units and conversions; CODEOWNERS

**Files:** Create `services/emr/internal/codes/{tables,maps,units,convert}.go` (+ tests), `services/emr/codes/testdata/{loinc,atc}.csv` (synthetic, departure 13), `services/emr/maps/common/units.yaml`, `services/emr/maps/common/conversions.yaml` (synthetic factors marked `reviewed_by: []`, refused outside development), `services/emr/maps/testdata/synthetic/fhir.yaml`, `services/emr/cmd/emr/unmapped.go`; Modify `.github/CODEOWNERS` (`/services/emr/maps/`, `/services/emr/rules/`, `/services/emr/codes/`: the platform owner and the clinical reviewers named in H4; until named, the platform owner and a TODO line)

**Interfaces:**
- `codes.Load(dir string, development bool) (*Tables, error)`: refuses synthetic tables outside development; `Tables.LOINC(code) (Entry, ok)`, deprecated entries treated as unknown; `Tables.ATC(code)`.
- `codes.LoadMaps(dir string, t *Tables) (*Maps, error)`: YAML `{version, hospital, connection, entries: [{system, code, loinc|atc, display}]}`; refuses a code not in the tables; `Maps.Version() string` (hash of the files, recorded as `mapping_version`).
- `codes.ParseUnit(s string) (ucum string, ok bool)` from `units.yaml` only.
- `codes.Convert(loinc string, value float64, fromUCUM string) (canonical float64, ok bool)` from `conversions.yaml` only.
- `emr unmapped --connection <id>`: prints counts, codes and display text; no patient data.

- [ ] **Step 1: Write the failing tests:** `TestGlucoseConversion` (A9: mg/dL × 0.0555 in the synthetic table's test, the factor pinned by the table not the code); `TestNoConversionNoValueSI` (A9); `TestUnknownLOINCIsUnmapped` (A9); `TestMapWithUnknownCodeRefused`; `TestSyntheticTablesRefusedOutsideDevelopment` (K10); `TestNoFuzzyMatch` (a display text equal to a LOINC long name doesn't map).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Load reviewed code maps, units and conversions; never guess a code".

### Task 19: The normaliser

**Files:** Create `services/emr/internal/normalise/{consumer,parse,times,observation,medication,patient,supersede}.go` (+ tests, + `consumer_integration_test.go`), `proto/mhai/emr/v1/events.proto` (`Fetched{raw_ids}`, `Created{record_ids}`, `DeadLetter{stage, batch_id, error_code}`)

**Interfaces:**
- `normalise.Run(ctx, js, pool, tables, maps, idx *index.Index)`: pulls `emr_normalise`; per message, under `dbx.WithHospital(mhai_emr, hid from the subject)`: loads the raw rows (refuses any row whose `hospital_id` differs: K-case), builds canonical rows, writes them, marks raw `normalised` or `rejected` with a code, publishes `records.v1.<hid>.created` (≤ 500 ids, `Nats-Msg-Id` = first id), acks. A transient error naks with backoff; the 5th delivery dead-letters `dead.v1.<hid>.emr_normalise` (ids and code only).
- `normalise.ParseDecimal(s string, sep rune) (value float64, comparator string, err error)` (Review Focus 3).
- `normalise.ParseFHIRTime(s string, hospitalTZ *time.Location) (t time.Time, dateOnly bool, err error)` and `normalise.CheckFuture(t, now)` (10 minutes) (Review Focus 1; A10).
- Supersession: a new `source_version` makes a new row and sets the old row's `superseded_by`; `entered-in-error` and `cancelled` set status only.
- Excluded patients: records dropped, link kept (§10.6).
- Unmapped: `standardised = false`, `unmapped_codes` upserted (count, last_seen).
- A publish refused by `DiscardNewPerSubject` leaves the raw row `pending` with `published_at` null; `normalise.Sweep` re-publishes pending rows older than 2 minutes every minute.

- [ ] **Step 1: Write the failing tests:** `TestTimesDateOnlyNoOffsetOffsetAndFuture` (Review Focus 1, A10); `TestStrictDecimal` (Review Focus 3); `TestUnmappedStoredFlagged` (§9.1); `TestAmendedIsNewVersion` (§8.2); `TestExcludedPatientDropped`; `TestFetchedForAnotherHospitalRefused` (A13 message part, K-case: a `Fetched` on `emr.v1.<A>.fetched` naming B's raw ids normalises nothing, because RLS hides them, and is acked with a metric); `TestNormaliseDeadLetterAfterFive`; `TestSweepRepublishes`.
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Normalise raw records in one place, strictly, with provenance and versions".

### Task 20: The patient index, proposals and signals

**Files:** Create `services/emr/internal/index/index.go`, `match.go` (+ tests), `services/emr/internal/signals/signals.go` (+ test); `events.proto` gains `Signal{kind, connection_id, ref_id, code}`

**Interfaces:**
- `index.Resolve(ctx, tx, conn Config, sourcePatientID string, p PatientFields) (patientID uuid.UUID, err error)`: by `(connection_id, source_patient_id)`; new → a new MHAI patient; never by name.
- `index.Propose(ctx, tx, …)`: exact normalised MRN **and** birth date **and** sex, across connections of one hospital with equal `settings.mrn_namespace`; `Patient.link` `replaced-by` only within the same connection (otherwise counted, ignored); writes `emr.possible_matches` (unique, so re-proposal is a no-op; a rejected pair isn't proposed again for the same reason) and sends `signals.Send(ctx, hid, "possible_match" | "source_merge", connectionID, matchID)`.
- `signals.Send` publishes `emr.v1.<hid>.signal` with `Nats-Msg-Id` = `kind:ref_id`.

- [ ] **Step 1: Write the failing tests:** `TestEqualMRNDifferentBirthDateTwoPatients` (A11); `TestReplacedByProposesOnly` (A31/A12 emr side: nothing merges; proposal and signal written); `TestReplacedByOtherConnectionIgnored` (A31); `TestNoNameMatching`; `TestSignalCarriesIdsOnly` (A22 part).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Index patients per hospital by source id, and only propose matches".

### Task 21: The real LOINC and ATC tables (waits on H6)

**Files:** Create `services/emr/codes/loinc.csv`, `atc.csv`, `services/emr/codes/LICENCES.md` (the accepted terms, version, date, who accepted)

- [ ] **Step 1:** Write `TestBundledTablesLoad` (version recorded; at least the analytes the rule set names are present; no deprecated LOINC is used by a map).
- [ ] **Step 2–4:** Run → fail; add the tables H6 cleared; pass.
- [ ] **Step 5:** Commit: "Bundle the LOINC and WHO ATC tables under their licences".

**Off by default after PR 5:** nothing fetches yet; the normaliser runs only in the `emr` profile.

### PR 6: FHIR R4

### Task 22: SMART Backend Services auth and the FHIR `test`

**Files:** Create `services/emr/internal/connector/fhir/{fhir,auth,test}.go` (+ tests)

**Interfaces:**
- `fhir.New() connector.Connector` with `TypeInfo` of §4.1 (`client_id`, `token_url`, `private_key` secret, optional `scope`).
- `auth.Token(ctx, client *http.Client, cfg, creds) (string, time.Time, error)`: a client assertion signed RS384 or ES384 (from the PEM's key type; anything else `sign_in_refused`), `exp` 5 minutes, random `jti`, `aud = token_url`; the token cached in memory per connection until 60 s before expiry; never stored, never logged.
- `Test`: token; `GET metadata` (`fhirVersion` `4.0.x`; Patient, Observation, MedicationRequest, MedicationStatement, Encounter, Location with search `_lastUpdated` and `_sort` declared, else `unreadable_data`); `GET Observation?_summary=count`. R4B and R5 → `unreadable_data`.

- [ ] **Step 1: Write the failing tests:** `TestAssertionClaimsAndAlg`; `TestTokenOnlyToTokenOrigin` (A28 part); `TestCapabilityChecks` (R5, missing sort, missing resource → `unreadable_data`); `TestTokenNeverLogged` (A1 emr side: a captured log of a full `Test` has no token or assertion).
- [ ] **Step 2–4:** Run (emr) → fail; implement against an `httptest` FHIR server; pass.
- [ ] **Step 5:** Commit: "Sign in to FHIR servers with SMART Backend Services and test them read-only".

### Task 23: FHIR `Fetch`: polling, cursor, paging, references, limits

**Files:** Create `services/emr/internal/connector/fhir/{fetch,cursor,bundle,map}.go` (+ tests)

**Interfaces:**
- `Fetch` reads `Observation?category=laboratory,vital-signs&_lastUpdated=gt<cursor>&_count=200&_sort=_lastUpdated`, then `MedicationRequest` and `MedicationStatement` the same, then `Patient`, `Encounter`, `Location` by `_id=` batches of ≤ 100; follows `next` only when host and base path equal the base address; at most 20 pages or 4,000 records per run; maps each resource to a `RawRecord` through the allow-list structs (unknown fields never decoded).
- Cursor `{"lu": <RFC 3339>, "v": 1}`: the greatest `meta.lastUpdated` of completed pages minus 2 minutes; unchanged on a failed page.
- Backoff: `429` honours `Retry-After` (capped 15 minutes); `5xx` doubles to 15 minutes; outcome codes `too_many_requests`, `unreachable`, `timed_out`.
- `Fetch` with `backfill=true` (first sync): `_lastUpdated=gt<now − backfill_days>`; the records are flagged `backfill` on the raw row (findings from them are `historical`, Task 28).

- [ ] **Step 1: Write the failing tests:** `TestCursorEqualTimestampsAcrossPages`, `TestCursorNotAdvancedOnFailedPage` (Review Focus 2); `TestNextLinkElsewhereRefused` (A7); `TestRetryAfterHonoured` (A20: no request for 120 s on a fake clock); `TestRunLimits` (A20: 20 pages); `TestUnknownFieldsNeverStored`.
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Poll FHIR R4 servers by last update, bounded, with a safe cursor".

### Task 24: The scheduler, reconciliation, sync runs, health and `sync_now`

**Files:** Create `services/emr/internal/scheduler/{scheduler,lock,health}.go` (+ tests, + `_integration_test.go`); `services/emr/internal/ctl/syncnow.go`

**Interfaces:**
- `scheduler.Run(ctx, pool, registry, store, egress)`: every 10 s reads due connections as `mhai_emr_sched`; per connection `pg_try_advisory_lock(hashtext('emr:'||id))` on a dedicated connection (skip if held); jitter ±10%; one `emr.sync_runs` row per run; `emr.connection_errors` on failure (closed code); stops polling after 3 consecutive `sign_in_refused`; a paused connection (breaker, Task 29) is skipped.
- Daily reconciliation at 02:00 hospital local time: re-read 24 h; raw rows found only then increment `emr_reconciled_late_total`.
- `scheduler.Stale(connType string, lastOK time.Time, pollSeconds int, expectedEveryMinutes *int, lastNewFile *time.Time, now time.Time) bool` (§15.4).
- `sync_now` (svc:core): marks the connection due now; replies `{queued: true}`.
- Secret store unavailable → `unreachable`, never `sign_in_refused` (§15.2).

- [ ] **Step 1: Write the failing tests:** `TestOneFetchPerConnection` (two schedulers, one lock); `TestBackfillThenLive` (A18 emr side: the first run is `backfill`; the cursor reaching now − poll ends it); `TestStaleRules` (A23 emr side, per type); `TestSecretStoreDownIsUnreachable`; `TestThreeSignInFailuresStopPolling`.
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Schedule one bounded fetch per connection and record its health".

### Task 25: The synthetic FHIR server, record links and the end-to-end run

**Files:** Create `services/emr/internal/synth/{patients,server}.go` (+ test), `services/gokit/recordlink/recordlink.go` (+ test), `services/emr/scripts/e2e-fhir.sh`, `services/emr/README.md` section "Synthetic end to end"

**Interfaces:**
- `synth.Generate(seed int64, n int) []Patient` (names from a synthetic list marked as such, MRNs `SYN-…`, timed labs, vitals and medications, including critical potassium, a creatinine rise and an amended result).
- `synth.Server(patients) *httptest.Server` and `emr synth-fhir --addr :8443 --seed 1` (a TLS server with a self-signed CA used only with `APP_ENV=development`).
- `recordlink.Build(template, baseURL, sourcePatientID string) (string, bool)`: `https` only; host equal to the base host; `{id}` path-escaped with `url.PathEscape`; anything else `("", false)`.

- [ ] **Step 1: Write the failing tests:** `TestRecordLink` (A25: other host, `javascript:`, `../`, `?`); `TestSyntheticSyncNoPHIInTelemetry` (A22 for connector to canonical: run a full synthetic sync; grep every log record, span and NATS message for the synthetic names, MRNs and values: none).
- [ ] **Step 2: Run (emr, remote):** fail.
- [ ] **Step 3:** Implement; `e2e-fhir.sh` starts the synthetic server on steven-reid inside the compose `emr` profile (development) and runs a sync through `egress-emr` with a development-only allowance for the synthetic server's address.
- [ ] **Step 4:** Run the e2e script remotely: records reach the canonical tables; the log grep is empty.
- [ ] **Step 5:** Commit: "Prove the FHIR path end to end on synthetic patients".

### PR 7: rules

### Task 26: Migration 00023: findings, and EMR dead-letter job types

**Files:** Create `services/core/internal/db/migrations/00023_emr_findings.sql`, `emr_findings_integration_test.go`; Modify `db_integration_test.go`

**Interfaces (SQL):** `emr.findings` as §11.3 plus `rule_set_sha256`, `shadow boolean`, `preliminary boolean`, `arrived_late boolean`, `historical` status, `supersedes uuid`, `UNIQUE (hospital_id, dedup_key)`; `emr.breaker_state (hospital_id, connection_id, window_start, counts jsonb, paused_at, resumed_at)`; `jobs.type` `CHECK` widened to `('approve_correction','emr_normalise','emr_rules')` (departure 12); `mhai_emr_core` gets `SELECT` on `emr.findings` and the evidence columns under RLS; `mhai_emr` `SELECT, INSERT, UPDATE (status, superseded …)` on findings, no `DELETE`.

- [ ] **Step 1: Write the failing tests:** `TestFindingDedupKeyUnique` (A19 database part); `TestFindingsRLS` (A13); `TestJobsTypeAllowsEMRStages`; `TestMigration00023DownUp`.
- [ ] **Step 2–4:** Run (core) → fail; write; pass.
- [ ] **Step 5:** Commit: "Add findings and let EMR dead letters reach the jobs view".

### Task 27: The rule set: schema, loader, rule kinds and golden tests

**Files:** Create `services/emr/internal/rules/{ruleset,schema,eval,kinds,news2,causes}.go` (+ tests), `services/emr/rules/testdata/ruleset-0-synthetic.yaml`, `services/emr/rules/testdata/golden/*.yaml`, `services/emr/rules/golden_test.go`

**Interfaces:**
- `rules.Load(path string, development bool) (*RuleSet, error)`: strict YAML; every threshold has `source` and non-empty `reviewed_by` unless the file is `-synthetic` (refused outside development); categories from the closed eight; `confidence` only `caution` or `review` (§21 question 3); freshness windows and `quiet_hours` per category; `SHA256()` over the file bytes.
- `rules.Evaluate(rs *RuleSet, p PatientHistory, trigger RecordRef, now time.Time) ([]Finding, error)`: pure; the seven kinds of §11.1; `>100` as at least 100, `<0.5` as at most 0.5; unknown age or sex use the `unknown` band; NEWS2 only with every parameter (consciousness and supplemental oxygen included), Scale 2 only when the source records it; unmapped observations run `source_flag` only; preliminary results run critical `threshold` and `source_flag` only.
- Golden files: `{patients: [...], expect: [{rule_id, severity, observation}], expect_none: [...]}`; at least one positive and one negative per rule, each band edge, NEWS2 missing parameters, unit conversion, preliminary vs final, an amended and an `entered-in-error` result.

- [ ] **Step 1: Write the failing tests:** `TestGoldenSynthetic` (runs every golden file); `TestThresholdWithoutSourceRefused`; `TestHighConfidenceRefused`; `TestNEWS2MissingParameterNoScore`; `TestComparatorBounds`; `TestUnmappedRunsSourceFlagOnly` (§21 question 2); `TestSyntheticRuleSetRefusedOutsideDevelopment` (K10).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Evaluate deterministic, versioned rules against golden synthetic cases".

### Task 28: The rules consumer: findings, freshness, shadow, supersession, stop on error

**Files:** Create `services/emr/internal/rules/consumer.go` (+ `_integration_test.go`)

**Interfaces:**
- `rules.Run(ctx, js, pool, active, shadow *RuleSet)`: pulls `emr_rules`; per message, per patient, under `dbx.WithHospital(mhai_emr, hid)`: loads current canonical rows; evaluates; writes findings with `dedup_key = (patient, rule_id, primary_observation_id)` (`ON CONFLICT DO NOTHING`); a finding is `historical` when older than the category window, from a `backfill` raw row or from re-normalisation; `shadow = true` for every finding when `EMR_RULE_SET_ACTIVE` is empty or names another version; `arrived_late` when received more than 1 h after `effective_at`; on supersession the new finding names the old (`supersedes`); publishes `emr.v1.<hid>.finding` per new non-historical finding (`Nats-Msg-Id` = finding id).
- Any evaluation error: rollback, no ack (`NakWithDelay`), the hospital's evaluation paused for 5 minutes (`emr_rules_stopped{hospital}` metric that pages), after 5 deliveries `dead.v1.<hid>.emr_rules`. No partial findings.
- On start, the active version and hash are sent as a `rule_set_activated` signal (core records `emr.rule_set_activated`).

- [ ] **Step 1: Write the failing tests:** `TestRuleErrorStopsHospitalNoPartialFindings` (A24); `TestBackfillAndRenormaliseHistorical` (A18 emr side); `TestNoActiveRuleSetAllShadow` (K-case off by default); `TestRedeliveredRecordOneFinding` (A19 emr side); `TestDateOnlyVitalsNeverAlert` (Review Focus 1); `TestSupersedesNamesOld` (A30 emr side).
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Raise findings once, fresh and versioned, and stop a hospital's evaluation on any rule error".

### Task 29: The per-source breaker

**Files:** Create `services/emr/internal/rules/breaker.go` (+ tests)

**Interfaces:** `breaker.Observe(ctx, tx, hid, source SourceRef, severity string, at time.Time) (tripped bool, err error)`: a rolling 14-day median of findings per 15 minutes by severity per source (a connection, or the hospital's documents); trips when a window exceeds both `multiple × baseline` and `floor` (settings `EMR_BREAKER_MULTIPLE` default 5, `EMR_BREAKER_FLOOR_CRITICAL` default 10, both H4 values); on trip sets `emr_connections.paused` (through a definer `emr_pause_connection(id)`, `EXECUTE` to `mhai_emr`), sends `breaker_tripped`; findings already made still publish; the scheduler skips a paused source.

- [ ] **Step 1: Write the failing tests:** `TestBreakerTripsAndPauses` (A32 source: 200 criticals in 15 minutes against a baseline of 2; paused after the threshold; every finding before the trip still published; no fetch after); `TestBreakerNeedsBothMultipleAndFloor`.
- [ ] **Step 2–4:** Run (emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Pause a source whose findings spike, and say so".

### Task 30: Rule set v1 (waits on H4)

**Files:** Create `services/emr/rules/ruleset-1.yaml`, `services/emr/rules/testdata/golden/v1/*.yaml`, `services/emr/rules/REFERENCES.md`

- [ ] **Step 1:** Write the v1 golden files from the clinical lead's thresholds, windows, quiet hours, NEWS2 decisions and causes (H4); they fail with no `ruleset-1.yaml`.
- [ ] **Step 2–4:** Add `ruleset-1.yaml` with `source` and `reviewed_by` on every threshold; run; pass; attach the golden report to a `clinical-review` PR.
- [ ] **Step 5:** Commit: "Add rule set v1 as reviewed by the clinical lead". Activation (`EMR_RULE_SET_ACTIVE=1`) is a separate deploy change after the shadow review (§11.5), never in this PR.

### PR 8a: alert settings and routes

### Task 31: Migration 00024: alerts, settings, deliveries, the re-queue trigger

**Files:** Create `services/core/internal/db/migrations/00024_alerts.sql`, `alerts_integration_test.go`; Modify `db_integration_test.go`, `internal/audit/schemas.go` (+ `drain_append` admits `alert.requeued`)

**Interfaces (SQL):** the tables of §12.2 and §18 item 4; `alerts (id, hospital_id, finding_id, patient_id, rule_id, loinc, category, severity, status, routed_queue, grouped, preliminary, arrived_late, amended jsonb, withdrawn, handled_by, handled_at, dismiss_reason, dismiss_note ≤ 500, last_notified_at, created_at, updated_at)` with a partial unique index `(hospital_id, patient_id, rule_id, loinc) WHERE status = 'new'` (§12.4); `alert_deliveries (alert_id, hospital_id, user_id, read_at, notified_at)`; `user_notification_settings (user_id, hospital_id, alert_popups, correction_popups)`; `mhai_gateway`: grants of §13.2 with a restrictive `UPDATE` policy `USING (status = 'new')`; `mhai_emr_core`: `INSERT, UPDATE` on `alerts` and `alert_deliveries`, `SELECT` on settings; recipients must be active clinicians of the same hospital (a trigger `CHECK`, A15); the re-queue triggers (departure 8): on `users` `is_active` true → false and on `user_roles` delete of `clinician`, every `new` alert whose only delivery is that user gets `routed_queue = true`, `last_notified_at = NULL` for criticals (so the consumer's notifier re-notifies), and one `alert.requeued` fact (`alert_ids` ≤ 500 per fact, reason code) in `intelligence_audit_outbox`.

- [ ] **Step 1: Write the failing tests:** `TestRequeueOnDeactivation` (A33: three open alerts, queue off; all three move in the same transaction; the fact is written); `TestRoleRemovalRequeues` (A33, Review Focus 4); `TestRecipientMustBeActiveClinicianSameHospital` (A15 database part); `TestAlertUpdateOnlyFromNew` (first handler wins at the database); `TestAlertsRLS` (A13, A14 database part); `TestNoPatientRecipientType` (A27: `alert_deliveries.user_id` references `users`; no other recipient column exists); `TestMigration00024DownUp`.
- [ ] **Step 2–4:** Run (core) → fail; write; pass.
- [ ] **Step 5:** Commit: "Add alerts, their settings and deliveries, and re-queue alerts when a recipient leaves".

### Task 32: `/hospital/alert-settings`

**Files:** Create `services/core/internal/alerts/settings.go`, `settings_test.go`, `settings_integration_test.go`, `services/core/internal/alerts/register.go`; Modify `internal/server/server.go`, `openapi.yaml`, `internal/audit/schemas.go`, `catalogue.go`

**Interfaces:** `alerts.Register(m *authz.Mux, d Deps)`; `GET` → `AlertSettingsOut` (§16.2: `connection.status` is the worst source; `not_connected` with no connection; sources `{label, status, last_update_at}` only); `PATCH` exactly one section plus `version`; `Idempotency-Key`; 422 `validation_failed` (unknown ids, two sections, a level outside the list), 422 `no_recipients`, 409 `conflict`; defaults written when a hospital's first connection is switched on (`emrctl` hook in Task 11's switch-on, in the same transaction); audit `alert_settings.units_changed`, `.rules_changed`, `.recipients_changed` with counts only.

- [ ] **Step 1: Write the failing tests:** `TestRecipientsOtherHospitalInactiveOrNonClinician422` (A15); `TestNoRecipients422`; `TestAllNamedDeactivatedQueueOffFallsBackToQueue` (A16, with Task 35's routing function `alerts.Route` unit-tested here); `TestSettingsVersionConflict`; `TestSettingsAuditCountsOnly` (A21 part); `TestNotConnected`.
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Serve hospital alert settings with versioned sections".

### Task 33: `/alerts`, unread count, acknowledge and dismiss, `/me/notification-settings`, `sources_status`

**Files:** Create `services/core/internal/alerts/{list,handle,notify_settings,sources}.go` (+ tests); Modify `openapi.yaml` (`AlertOut` with the §16.4 optional fields; `AlertPatientOut.mrn` nullable from the first version; `AlertPageOut.sources_status` required), `internal/audit/schemas.go`

**Interfaces:**
- `GET /alerts` filters of §16.3, cursor (opaque HMAC as the audit list), `limit` ≤ 100; visibility in SQL: `routed_queue` or a delivery row for the caller (Review Focus 4); the evidence built from the finding's exact versions; `finding.flag` mapping of §16.3; `emr_link` via `recordlink.Build`; `likely_causes[].confidence` only `caution` or `review`; writes `alerts.read` with the ids served.
- `GET /alerts/unread-count`; `POST /alerts/{id}/acknowledge`, `/dismiss {reason, note?}` via `UPDATE … WHERE status = 'new' RETURNING`; 409 `already_handled` with `AlertHandledOut`; 404 for an alert not routed to the caller or of another hospital.
- `sources_status`: per connection of the hospital `{label, status, last_update_at}`, `behind` when `scheduler.Stale`'s rule (re-implemented in SQL as `emr_source_status()`, a definer reading `emr.sync_runs`, with a test asserting it agrees with the Go function on a shared table of cases) or `paused`.
- `/me/notification-settings` `GET`/`PATCH {alert_popups, correction_popups}`.

- [ ] **Step 1: Write the failing tests:** `TestOtherHospitalAlert404` (A14); `TestNotRoutedAlert404ListCountAck` (A17); `TestQueueAlertStaysVisibleAfterQueueOff` (Review Focus 4); `TestAcknowledgeFirstWins` (20 parallel); `TestAlertsReadAudited` (A21, E22); `TestSourcesStatusBehindWhenStaleOrPaused` (A23, A42 core side); `TestEmptyListCarriesSourcesStatus` (§15.3); `TestAlertRecipientsAreClinicians` (A27 route part); `TestDismissNoteNeverAudited`.
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Serve clinicians their alerts with source status, read audit and first-wins handling".

### Task 34: Possible duplicates and exclusions

**Files:** Create `services/core/internal/alerts/{matches,exclusions}.go` (+ tests); Modify `cmd/core/seed.go` (`core emr exclude --hospital <id|code> (--mrn <mrn> | --patient <id>) --reason <code>` as `mhai_emr_admin`), `openapi.yaml`

**Interfaces:** `GET /hospital/patient-matches`; `POST …/{id}/confirm {keep: "a"|"b", mfa_code|backup_code}` (calls `emr_merge_patients`, audit `emr.patients_merged` in the same transaction) and `/reject` (`emr.patient_match_rejected`); `POST /hospital/patient-exclusions {mrn}|{patient_id}, reason` (step-up; `emr_exclude_patient`; audit `emr.patient_excluded`; answers 202 `{patient_id, purge_by}` — `purge_by` is set once 00025 exists; until then the route answers 503 `not_ready`, so it ships live only with PR 9); 404 when no such patient in the caller's hospital.

- [ ] **Step 1: Write the failing tests:** `TestMatchOtherHospital404` (A31 core part); `TestConfirmAuditedMergeOnly` (A12, A31); `TestRejectNotProposedAgain`; `TestExclusionWithdrawsAlertsAtOnce` (A34 immediate part; meaningful after Task 35 creates alerts: seeded rows here); `TestCLIExcludeAudited`.
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Let hospital admins confirm patient matches and exclude patients".

**Off by default after PR 8a:** no finding consumer runs, so no alert exists; the routes return empty lists and `not_connected`.

### PR 8b: the finding consumer

### Task 35: `core_emr`: finding to alert

**Files:** Create `services/core/internal/alerts/consumer.go`, `route.go` (+ `_integration_test.go`); Modify `internal/server/server.go` (start when `CORE_EMR_ENABLED`), `internal/config/config.go` (`CORE_EMR_ALLOW_NULL_MRN`)

**Interfaces:** `alerts.RunConsumer(ctx, pool, js)`: pulls `core_emr`; for a finding message, under `dbx.WithHospital(mhai_emr_core, hid from the subject)`: re-reads the finding by id (not found → ack, metric `emr_finding_foreign_total`: the finding of another hospital is invisible under RLS, A13); steps 1–5 of §12.1; a null-MRN patient while `CORE_EMR_ALLOW_NULL_MRN=false` → `alert.not_raised` reason `null_mrn`; `alerts.Route(settings, patientUnit, severity, category) (queue bool, users []uuid.UUID, reason string)`; writes `alert.raised` with the closed payload.

- [ ] **Step 1: Write the failing tests:** `TestFindingOfOtherHospitalNoAlert` (A13 message part); `TestHistoricalAndShadowAckedNoAlert` (A18 core side, K11); `TestInactiveHospitalOrNoConnectionNotRaised`; `TestUnitNotCoveredAndLevelOffRecorded`; `TestNoLocationIsCovered`; `TestNullMRNGated`.
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Turn findings into routed alerts in one transaction".

### Task 36: Fatigue controls and grouped mode

**Files:** Create `services/core/internal/alerts/fatigue.go`, `grouped.go` (+ tests)

**Interfaces:** the open-alert update for the same `(patient, rule_id, loinc)` (no re-notify unless severity rises); quiet hours from the finding's rule (carried on the finding row as `quiet_hours`); info cap 20 per hospital per hour (an `alert_caps` counter row updated atomically: `UPDATE … SET n = n + 1 WHERE n < 20 RETURNING`); grouped mode when criticals per hospital in 15 minutes exceed `CORE_EMR_GROUP_THRESHOLD` (default 30, H4): `grouped = true`, `AlertPageOut.grouped {since, count}`, one summary per clinician; operators paged (`emr_grouped_mode_total` metric).

- [ ] **Step 1: Write the failing tests:** `TestRedeliveryAndReplayNoExtraAlert` (A19: the same finding 5 times; 1,000 replayed records); `TestSecondHitUpdatesOpenAlert`; `TestSeverityRiseRenotifies`; `TestQuietHours`; `TestInfoCapUnderRace` (20 parallel, 20 notified, the rest stored and counted); `TestGroupedModeKeepsEveryCritical` (A32 hospital).
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Control alert fatigue without dropping a critical".

### Task 37: Amended and withdrawn alerts, signals and `resume`

**Files:** Create `services/core/internal/alerts/amend.go`, `signals.go` (+ tests); Modify `internal/platform/emrconnections.go` (`POST /platform/emr/connections/{id}/resume`), `openapi.yaml`, `internal/audit/schemas.go`

**Interfaces:** a finding with `supersedes`: the open alert updated in place (`amended {previous_value, amended_at}`, re-notify if severity rose) or withdrawn (`source_amended`); a retracted observation (signalled by the normaliser's `Created` for a status-only change, then the rules consumer's withdrawal finding kind `retracted`) withdraws with `source_retracted`; a handled alert gets the mark without reopening; signals: `breaker_tripped` → `emr.source_paused` and a page metric; `possible_match` and `source_merge` → nothing but a count (the list reads the table); `rule_set_activated` → `emr.rule_set_activated`; `resume` (step-up, `version`) clears `paused`, calls `sync_now`, audits `emr.source_resumed`.

- [ ] **Step 1: Write the failing tests:** `TestAmendedToNormalWithdrawn`, `TestAmendedToCriticalUpdatedInPlace`, `TestHandledGetsMarkNotReopened`, `TestEnteredInErrorWithdrawn` (A30); `TestAmendRacesAcknowledge` (Review Focus 5); `TestBreakerSignalAuditedAndBehind` (A42); `TestResumeStepUpAudited`; `TestFullCycleNoPHIInTelemetry` (A22: synthetic sync → rules → alert → acknowledge, across emr and core; grep logs, spans, metric labels, NATS messages and dead letters).
- [ ] **Step 2–4:** Run (core, emr cross-service) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Update or withdraw alerts when the source changes its result, visibly".

### Task 38: EMR dead letters and `redrive`

**Files:** Modify `services/core/internal/intel/deadletter.go` (+ tests), `services/core/internal/platform/jobs.go` (`JobTypes` gains `emr_normalise`, `emr_rules`; retry of those types calls `emr.v1.ctl.redrive`), `services/emr/internal/ctl/redrive.go` (+ test)

**Interfaces:** `HandleDeadLetter` accepts `dead.v1.<hid>.emr_normalise|emr_rules` (`DeadLetter{stage, batch_id, error_code}`), writes a `jobs` row (`failed`, `subject_id = batch_id`, `error` = the code if it matches `^[a-z_]{1,32}$`, else `unknown`); `redrive {stage, batch_id}` re-publishes the batch from the emr service's own rows for the token's hospital only.

- [ ] **Step 1: Write the failing tests:** `TestEMRDeadLetterBecomesFailedJob`; `TestRedriveOwnHospitalOnly` (K6: a batch id of another hospital re-publishes nothing); `TestRedriveNeedsStepUp`.
- [ ] **Step 2–4:** Run (core, emr) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Show EMR dead letters in Failed jobs and redrive them".

**Off by default after PR 8b:** `core_emr` runs only with `CORE_EMR_ENABLED`; with no active rule set every finding is shadow, so even an enabled synthetic deployment raises no alert until `EMR_RULE_SET_ACTIVE` names a version.

### PR 9: retention, exclusion purge and Usage

### Task 39: Migration 00025: purge requests, legal holds, retention and removal functions

**Files:** Create `services/core/internal/db/migrations/00025_emr_retention.sql`, `emr_retention_integration_test.go`; Modify `db_integration_test.go`

**Interfaces (SQL):** `emr.purge_requests` as §10.6; `public.legal_holds` (departure 10); `emr_exclude_patient` replaced to also insert the purge request (`due_at = now() + 24 h`, `held` with `held_since` when `mhai_hospital_on_hold(hid)`); `emr_purge_patient(request_id) RETURNS jsonb` (counts; refuses under a hold; deletes canonical rows, findings, raw payloads, possible matches; nulls alerts' `patient_id` and `dismiss_note`; keeps the link row); `emr_release_holds()` (moves `held` to `pending` with a new `due_at` when the hold has lifted); `emr_retention_sweep(cap int) RETURNS jsonb` (§14.2 table: raw payloads 7 days, canonical 13 months, non-alert findings 90 days, alert patient columns 13 months, alerts 24 months, unreferenced patients; skips held hospitals); `emr_remove_hospital_data(hid)` (departure 11). Owner role `mhai_emr_maint` (new NOLOGIN) with `EXECUTE`; nothing else holds it.

- [ ] **Step 1: Write the failing tests:** `TestPurgeRemovesEverythingButLink` (A34); `TestHoldDefersPurgeThenReleases` (A41); `TestRetentionAges` (each row of §14.2 on a fake clock via `now()` overridden by a test setting); `TestRetentionSkipsHeldHospital`; `TestRemoveHospitalDataNeedsRemovalScheduled`; `TestMigration00025DownUp`.
- [ ] **Step 2–4:** Run (core) → fail; write; pass.
- [ ] **Step 5:** Commit: "Purge excluded patients within a day and age EMR data out, holds respected".

### Task 40: The maintenance loop and CLI

**Files:** Create `services/core/internal/emrmaint/loop.go` (+ `_integration_test.go`); Modify `cmd/core/seed.go` (`core hospitals legal-hold place|lift`, `core emr remove-hospital-data`), `internal/server/server.go`, `internal/audit/schemas.go` (`emr.exclusion_purged`, `emr.exclusion_purge_held`, `emr.credentials_deleted`, `legal_hold.placed`, `legal_hold.lifted`)

**Interfaces:** `emrmaint.Loop.Run(ctx, every 5 min)` under a session advisory lock (leader); per run: `emr_release_holds()`, then every due `pending` request (`emr_purge_patient`, audit `emr.exclusion_purged` with counts), held requests older than 90 days listed by a metric and a log line with ids only; nightly at 03:00 UTC `emr_retention_sweep(10000)` until it returns fewer.

- [ ] **Step 1: Write the failing tests:** `TestPurgeWithin24hWithoutJobMessage` (A41: no message exists; the loop alone purges); `TestHeldOver90DaysListed`; `TestRemoveHospitalDataWipesCredentialsFirst` (refuses the delete when a wipe isn't confirmed); `TestLegalHoldCLIProvisionOnly` (K8).
- [ ] **Step 2–4:** Run (core) → fail; implement; pass.
- [ ] **Step 5:** Commit: "Run EMR purges and retention from core, leader-locked".

### Task 41: Usage counts for alerts

**Files:** Create `services/core/internal/alerts/usage.go` (+ test)

**Interfaces:** `alerts.UsageBlock(ctx, tx, hid, from, to) (UsageOut, error)`: counts by severity, category, route (`by_route`) and handling, each through `smallgroup` guarding; served from `/hospital/usage` when that route is built (not in this plan).

- [ ] **Step 1–5:** `TestUsageSmallGroupsSuppressed`; implement; commit "Count alerts for Usage under the small-group rule".

### PR 10: core side of mhai-web PR 10

### Task 42: Core accepts BFF-sealed credentials

**Files:** Modify `services/core/internal/platform/emrconnections.go` (+ tests), `internal/config/config.go` (`CORE_EMR_REQUIRE_SEALED`, default `false` in development, `true` otherwise), `openapi.yaml` (`credentials_sealed: {field: base64 box}`, additive); `GET /platform/emr/seal-key` (platform admin, returns `{kid, public_key}`)

**Interfaces:** with `credentials_sealed`, core forwards the boxes unopened (it can't open them); with `CORE_EMR_REQUIRE_SEALED`, plain `credentials` are refused 422 `validation_failed`; boxes bind connection, field and origin, so the BFF creates the connection without credentials first (the reply gives its `id`), then sends the sealed boxes in a PATCH.

- [ ] **Step 1–5:** `TestSealedCredentialsForwardedUnopened`, `TestPlainRefusedWhenRequired`; implement; commit "Accept credentials sealed by the platform BFF".

**mhai-web PR 10 (other repository), the handoff list:** switch the Plan 13 screens to live; map `validation_failed`; show `sources_status`, `arrived_late`, `preliminary`, `amended`, `withdrawn`, grouped mode, nullable MRN ("No MRN recorded"); the possible-duplicates and exclusion screens; BFF-side sealing (Task 42's flow); the new audit kinds of §12.6; `JobTypes` gains `emr_normalise`, `emr_rules` (departure 12). Live use waits for it.

### PR 11: Havana structured export (not before H1)

### Task 43: `mhai_csv_v1`: the format, a synthetic sample and the parser

**Files:** Create `docs/specs/formats/mhai_csv_v1.md`, `services/emr/internal/connector/fileexport/{format,parse}.go` (+ tests), `services/emr/internal/connector/fileexport/testdata/sample/*.csv` (synthetic)

**Interfaces:** `fileexport.Parse(kind string, r io.Reader) iter.Seq2[connector.RawRecord, error]`: RFC 4180, UTF-8, the fixed header per kind (§5.4), ISO 8601 with offset only, file ≤ 50 MB, row ≤ 8 KB, ≤ 200,000 rows, anything over refused whole (`unreadable_data`); cells through `sink.CSVCell`.

- [ ] **Step 1–5:** `TestCSVNoOffsetRefused` (A10), `TestCSVLimitsRefuseWhole`, `TestCSVFormulaCells` (A7), `TestHeaderExact`; implement; commit "Define and parse the mhai_csv_v1 export format".

### Task 44: The SFTP connector and staleness by expected interval

**Files:** Create `services/emr/internal/connector/fileexport/{connector,sftp}.go` (+ tests with an in-process SSH server)

**Interfaces:** `TypeInfo` of §4.1 plus required `settings.expected_every_minutes`; host key only by the typed fingerprint (`egress.SFTP`); files moved to `processed/` after a successful load, never deleted; staleness per §15.4.

- [ ] **Step 1–5:** `TestWrongHostKeyRefusedAtTestAndSync` (A29 SFTP), `TestProcessedMoveNotDelete`, `TestStaleByExpectedInterval`; implement; commit "Read scheduled file exports over SFTP with a pinned host key". The type joins `CORE_EMR_CONNECTOR_TYPES` only by a deploy change after H1 and H7.

### PR 12: Havana scanned archive (not yet: H2, documents service, document pipeline phase 1)

### Task 45: Migration 00026: the reviewer's patient search and creation

**Files:** Create `services/core/internal/db/migrations/00026_emr_documents.sql` (number at build time), `emr_documents_integration_test.go`

**Interfaces (SQL):** foreign keys from `emr.raw_records.document_id` and `review_id` to the documents tables; `emr_patient_candidates(review_id, query)` and `emr_create_document_patient(review_id, display_name, mrn, birth_date, sex)` exactly §10.3; `mhai_worker` `INSERT` on `emr.raw_records` with `WITH CHECK (source_kind = 'document')`; nothing on `emr.patients`; `gateway` NATS publish on `emr.v1.*.fetched`.

- [ ] **Step 1–5:** `TestWorkerCannotReadPatients`, `TestCandidatesLimitsAndAudit` (A38), `TestCreateDocumentPatientChecks` (A39); write; commit "Let reviewers find or create a document's patient through core only".

### Task 46: The `document_archive` connector

**Files:** Create `services/emr/internal/connector/archive/{archive,list,upload}.go` (+ tests)

**Interfaces:** lists by the archive's JSON "files added since" (H2) with a stable cursor; downloads ≤ 10 a minute; hands `application/pdf`, `image/jpeg`, `image/png`, `image/tiff` to the documents service as `svc:emr` uploads with `source_name`; skips others counted by MIME type only; refers to files by the archive's opaque id only.

- [ ] **Step 1–5:** `TestArchiveSkipsOtherTypes` and `TestFileNamesNeverInTelemetry` (A36); implement; commit "Hand the archive's new scans to the document pipeline".

### Task 47: Reviewed records in, rejected values back to review

**Files:** Modify the documents service's `finalise_document` path in core (publish `emr.v1.<hid>.fetched` after commit), `services/emr/internal/normalise/` (document rows; `document_rejected` signal), `services/core/internal/alerts/signals.go` (reopen the review `needs_changes`, audit `document.review_reopened`)

- [ ] **Step 1–5:** `TestRejectedDocumentValueReopensReview` (A40), `TestUnreviewedDocumentNeverCanonical` (A26 end to end); implement; commit "Normalise reviewed document values, and send rejected ones back to review".

### PR 13: `pids`

### Task 48: Core's patient check and the token claim

**Files:** Modify `services/core/internal/intel/client.go` (`Caller.PatientIDs`, set only from `emr_patient_in_hospital` results), `services/core/internal/queries/handlers.go` (an optional `patient_id` on `POST /queries`, checked; 404 `not_found` otherwise), `openapi.yaml`; `services/intelligence/tests/boundary/` (a test that a body patient id outside `pids` is refused, check 8; added if missing)

- [ ] **Step 1–5:** `TestPatientIDOfOtherHospital404`, `TestExcludedOrMergedPatient404`, `TestPidsClaimOnlyCheckedIDs`, the worker test; implement; commit "Carry checked MHAI patient ids in request tokens".

---

## Security and threat model

The design's §17 is the threat model: its assets, actors, boundaries, threats E1–E23 and controls stand, and its abuse cases A1–A42 are mapped to tests below. This plan adds the following, from the code as it is on `main`.

### Assets

As design §17.1, plus: the shared module `services/gokit/` (a defect there reaches core and the emr service at once), the proxy's `K_proxy` key, and the idempotency HMAC key.

### Boundaries added or made concrete by this plan

| Crossing | What is checked |
|---|---|
| core → emr (`emr.v1.ctl.*`) | `reqtoken` with the subject table, `CheckReply`, `replay.Set`, `emrops` caller rule, protobuf schema (Tasks 2, 4) |
| emr → `egress-emr` | `Proxy-Authorization` per connection, the allow-list file's age and shape (Tasks 13, 14) |
| `egress-emr` → hospital | resolve once, refuse any private answer outside the connection's declared ranges, dial the checked address (Task 13) |
| emr → `allow.json` → proxy | written only by the emr service; read-only mount; stale or malformed refuses all (Tasks 13, 15) |
| trigger → outbox → chain | the re-queue fact through `drain_append`, closed schema (Task 31) |
| core loop → purge | `mhai_emr_maint`, leader lock, hold check in SQL (Tasks 39, 40) |
| operator CLI → exclusion, legal hold, removal | `mhai_emr_admin` and `mhai_provision`, audited with no actor and `via: cli` (Tasks 34, 40) |

### Threats added (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| P1 | Spoofing | A control token replayed to a second emr copy | ops idempotent or self-limiting; boxes bound and 60 s; per-process replay set | `TestCtlRefusals` (replayed `jti`), A35 |
| P2 | Elevation of privilege | `emr.*` rows leak into `operations.json`, and a worker accepts a platform role | separate `emrops` table; a test pins no `emr.` row | `TestNoEMROpsInOperationsJSON` |
| P3 | Tampering | A temporary table shadows a relation in a definer | `search_path = pg_catalog, pg_temp`, schema-qualified names | `TestDefinersIgnoreTempTables` (K2) |
| P4 | Information disclosure | The proxy keeps forwarding with a stale allow-list after a connection is switched off | 120 s age limit; refuse all when stale | `TestStaleAllowListRefusesAll` (K5) |
| P5 | Elevation of privilege | `redrive` re-publishes another hospital's batch | the token's hospital scopes the read under RLS | `TestRedriveOwnHospitalOnly` (K6) |
| P6 | Tampering (safety) | A synthetic rule set or code table reaches production | refused outside development | K10 |
| P7 | Denial of service (safety) | A purge is lost because no job message exists | the loop reads durable rows, not messages | `TestPurgeWithin24hWithoutJobMessage` |
| P8 | Information disclosure | The emr binary links core's auth or signing code | separate module; import test | K12 |

### Controls (decided)

As design §17.4, implemented in the tasks named; plus: the subject table (Task 2), the replay set (Task 2), the role-taking `WithHospital` with a closed role set (Task 3), the HMAC idempotency hash (Task 11), the allow-list file contract (Tasks 13–15), the re-queue trigger (Task 31), core's purge loop (Task 40).

### Residual risk and sign-off

As design §17.6, plus:
- **Replay across emr copies** (departure 3): accepted, as for the Python workers; every op is idempotent or bound and short-lived.
- **The legal hold is a minimal CLI table** until the platform feature exists (departure 10); placing a hold needs the operator. The privacy lead knows this before live data (H5).
- **Hospital removal is a hook** (departure 11); until the two-person flow exists, removal of EMR data is an operator action on a written request.
- **Synthetic tables and rule set in the repository** are refused outside development; a mistake in `APP_ENV` would accept them. `APP_ENV` is set by deploy configuration reviewed by the release owner.

## Abuse cases → tests

| Case | Test (task) |
|---|---|
| A1 credential in no log, span, error, idempotency row, audit, message, reply | `TestCredentialNeverStoredOrLogged`, `TestIdempotencyHashIsKeyedForCredentialRoutes` (11); `TestPutCredentialsReplyHasSetAtOnly` (10); `TestTokenNeverLogged` (22) |
| A2 no role or identity reads a credential value | `TestEMRRoleGrants` (6); `TestManagedStoreContract` path policy (12) |
| A3 a connector that logs `Creds` fails | `TestCredsRedacted`, `TestNoConnectorLogsCreds` (9) |
| A4 forged or replayed control requests refused | `TestCtlRefusals` (4); `TestSubjectRulesEMRCtl` (2) |
| A5 bad addresses refused at save and connect | `TestCheckURLRefusals` (7); `TestRebindingPinned` (13); `test-egress.sh` (15) |
| A6 proxy refuses unlisted hosts, redirects, out-of-range private | `TestCONNECTNotOnAllowList`, `TestPrivateOnlyWithinDeclaredRanges` (13); `TestNoRedirectAcrossHosts` (14); `test-egress.sh` (15) |
| A7 huge, deep, off-host, formula input refused or neutralised | `TestResponseCaps` (14); `TestNextLinkElsewhereRefused` (23); `TestCSVFormulaNeutralised` (17); `TestCSVLimitsRefuseWhole`, `TestCSVFormulaCells` (43) |
| A8 display text stripped and capped | `TestCleanText` (17) |
| A9 conversions reviewed; no conversion, no number; unknown LOINC unmapped | `TestGlucoseConversion`, `TestNoConversionNoValueSI`, `TestUnknownLOINCIsUnmapped` (18) |
| A10 no-offset and future times refused | `TestTimesDateOnlyNoOffsetOffsetAndFuture` (19); `TestCSVNoOffsetRefused` (43) |
| A11 equal MRN, different birth date: two patients and a proposal | `TestEqualMRNDifferentBirthDateTwoPatients` (20) |
| A12 a source merge is audited; nothing else merges | `TestReplacedByProposesOnly` (20); `TestConfirmAuditedMergeOnly` (34) |
| A13 cross-hospital rows and messages | `TestEMRRLSWithHospital` (6); `TestEMRCanonicalRLS` (16); `TestFetchedForAnotherHospitalRefused` (19); `TestFindingsRLS` (26); `TestAlertsRLS` (31); `TestFindingOfOtherHospitalNoAlert` (35) |
| A14 another hospital's alert is 404 | `TestOtherHospitalAlert404` (33) |
| A15 bad recipients 422 | `TestRecipientMustBeActiveClinicianSameHospital` (31); `TestRecipientsOtherHospitalInactiveOrNonClinician422`, `TestNoRecipients422` (32) |
| A16 all named deactivated, queue off: queue | `TestAllNamedDeactivatedQueueOffFallsBackToQueue` (32) |
| A17 a clinician can't see an alert not routed to them | `TestNotRoutedAlert404ListCountAck` (33) |
| A18 a first sync raises no alert; the next new critical raises one | `TestBackfillThenLive` (24); `TestBackfillAndRenormaliseHistorical` (28); `TestHistoricalAndShadowAckedNoAlert` (35) |
| A19 redelivery and replay add nothing; second hit updates; rise re-notifies | `TestSinkReplayNoOp` (17); `TestRedeliveredRecordOneFinding` (28); `TestFindingDedupKeyUnique` (26); `TestRedeliveryAndReplayNoExtraAlert`, `TestSecondHitUpdatesOpenAlert`, `TestSeverityRiseRenotifies` (36) |
| A20 `Retry-After` and page limits | `TestRetryAfterHonoured`, `TestRunLimits` (23) |
| A21 every action audited with a closed payload | `TestAuditSchemasEMRConnection` (11); `TestSettingsAuditCountsOnly` (32); `TestAlertsReadAudited` (33); `TestConfirmAuditedMergeOnly` (34); `TestResumeStepUpAudited` (37) |
| A22 no synthetic PHI in telemetry | `TestSyntheticSyncNoPHIInTelemetry` (25); `TestFullCycleNoPHIInTelemetry` (37); `TestSignalCarriesIdsOnly` (20) |
| A23 a silent source is `behind` | `TestStaleRules` (24); `TestSourcesStatusBehindWhenStaleOrPaused` (33) |
| A24 a rule error stops the hospital, no partial findings | `TestRuleErrorStopsHospitalNoPartialFindings` (28) |
| A25 record links safe | `TestRecordLink` (25) |
| A26 a document row without review refused | `TestDocumentRawRecordNeedsReview` (16); `TestUnreviewedDocumentNeverCanonical` (47) |
| A27 no patient recipient | `TestNoPatientRecipientType` (31); `TestAlertRecipientsAreClinicians` (33) |
| A28 origin change wipes; nothing leaves to a new host | `TestOriginChangeWipesAndSwitchesOff` (11); `TestWipeThenTestSendsNothing` (10); `TestCredentialOnlyToOrigin` (14); `TestTokenOnlyToTokenOrigin` (22) |
| A29 DNS rebinding; SFTP host key | `TestRebindingPinned` (13); `test-egress.sh` (15); `TestWrongHostKeyRefusedAtTestAndSync` (44) |
| A30 amended and retracted results | `TestSupersedesNamesOld` (28); `TestAmendedToNormalWithdrawn`, `TestAmendedToCriticalUpdatedInPlace`, `TestHandledGetsMarkNotReopened`, `TestEnteredInErrorWithdrawn` (37) |
| A31 proposals only; other hospital 404 | `TestReplacedByProposesOnly`, `TestReplacedByOtherConnectionIgnored` (20); `TestMatchOtherHospital404`, `TestConfirmAuditedMergeOnly` (34) |
| A32 source breaker and grouped mode | `TestBreakerTripsAndPauses` (29); `TestGroupedModeKeepsEveryCritical` (36) |
| A33 deactivation and role removal re-queue | `TestRequeueOnDeactivation`, `TestRoleRemovalRequeues` (31) |
| A34 exclusion withdraws at once and purges | `TestExclusionWithdrawsAlertsAtOnce` (34); `TestPurgeRemovesEverythingButLink` (39) |
| A35 replayed sealed boxes refused | `TestOpenRefusesReplayedBox` (8); `TestPutCredentialsAllOrNothing` (10) |
| A36 archive types and file names | `TestArchiveSkipsOtherTypes`, `TestFileNamesNeverInTelemetry` (46) |
| A37 proxy per-connection rules; ranges can't overlap across hospitals | `TestCrossConnectionRefused` (13); `TestEMRRangesCannotOverlapAcrossHospitals` (6) |
| A38 the worker can't read patients; candidates limited and audited | `TestWorkerCannotReadPatients`, `TestCandidatesLimitsAndAudit` (45) |
| A39 document patient creation checks | `TestCreateDocumentPatientChecks` (45) |
| A40 a rejected document value reopens review | `TestRejectedDocumentValueReopensReview` (47) |
| A41 holds defer purges; a lost message still purges | `TestHoldDefersPurgeThenReleases` (39); `TestPurgeWithin24hWithoutJobMessage` (40) |
| A42 a paused source is `behind` at once | `TestSourcesStatusBehindWhenStaleOrPaused` (33); `TestBreakerSignalAuditedAndBehind` (37) |
| K2 temp tables can't shadow definers | `TestDefinersIgnoreTempTables` (6, 16) |
| K3 widened audit policies admit only the new actions | `TestPlatformAppendAllowsEMRConnectionActionsOnly` (6) |
| K4 off by default | `TestEMRStreamNotCreatedWhenDisabled` (5); `TestEMRDisabledAnswersNotReady` (11); `TestNoActiveRuleSetAllShadow` (28) |
| K5 stale allow-list refuses all | `TestStaleAllowListRefusesAll` (13) |
| K6 redrive own hospital only | `TestRedriveOwnHospitalOnly` (38) |
| K8 legal-hold CLI only by provisioning | `TestLegalHoldCLIProvisionOnly` (40) |
| K10 synthetic tables and rule sets refused outside development | `TestSyntheticTablesRefusedOutsideDevelopment` (18); `TestSyntheticRuleSetRefusedOutsideDevelopment` (27) |
| K11 historical and shadow findings never alert | `TestHistoricalAndShadowAckedNoAlert` (35) |
| K12 the emr module imports nothing of core | `TestEMRImportsNoCoreModule` (4) |

## What's needed from people, and when

Tasks marked "now" in the PR table need nothing below. The rest wait as shown.

| # | Who | What | Blocks in this plan |
|---|---|---|---|
| H1 | Product owner with Havana | A structured source for labs (then vitals, medications), export or FHIR, frequency, a synthetic or de-identified layout sample | PR 11 start; real alerts for Havana |
| H2 | Our team with Havana's agreement | The archive's JSON "files added since" listing, a per-integration read-only key, a removed-files listing | PR 12 (Task 46) |
| H3 | Product owner, ops | Choose and provision the managed secret store (AWS or GCP, in-region) and the emr service's identity | Task 12; any production deployment of PR 3 |
| H4 | Named clinical lead and a second clinician | Rule set v1 (thresholds with references, windows, quiet hours, NEWS2, causes), breaker and grouped-mode values, the paused-source wording and phone check, map review, the shadow review, names and MRNs in alerts; also the clinical reviewers for `CODEOWNERS` | Task 30; activation; live use |
| H5 | Privacy lead and a Nigerian data protection lawyer | DPIA (queue breadth), processing agreement addendum, written confirmation that the hospital's call process stays primary, NDPA basis, data location, retention values (Task 39's constants) | Any real data |
| H6 | Product owner | Licences: LOINC terms, WHO ATC/DDD | Task 21; any real data |
| H7 | Havana IT | Network path, SFTP host key fingerprint, rotation contact | PR 11 live |
| H8 | Product owner | §21 answers | Done: approved as recommended, 7 Nov |
| H9 | Product owner, ops | On-call owner for stale feeds, rule errors, breaker pages, link conflicts, `reconciled_late`, grouped mode | Live use |

## Decisions (approved 7 Nov 2026, all as recommended)

From design §21, unchanged: FHIR R4 first on synthetic data, Havana through `file_export` when H1 gives a source, the archive after document pipeline phase 1; the lab's critical flag alerts even when MHAI's rule doesn't; no `high` confidence in v1; exclusion as §10.6; `critical_urgent` defaults (`critical` for vitals), info capped at 20 an hour, critical and urgent never capped; ATC only; the §14.2 retention values; pinned CA and declared CIDRs added when a hospital needs them; a separate Go module with `services/gokit/`; BFF-side sealing as a live gate; no field encryption in v1; no email or SMS from MHAI in v1; hospital admins confirm merges.

## Out of this plan

- mhai-web's PR 10 itself (handoff list under Task 42).
- `/hospital/usage` (Task 41 provides the alert counts it will serve).
- The documents service and document pipeline phase 1 (PR 12 depends on them).
- The platform legal-hold feature and the two-person hospital removal flow (departures 10 and 11 give their hooks).
- Push delivery (FHIR Subscriptions, HL7 v2), the `hl7v2_feed` type, a pinned CA bundle setting, key generation inside the emr service, per-hospital emr processes, mapping and rule editing screens, ML scoring, conditions and notes, "Ask MHAI about this patient" beyond `pids`.

## Self-review notes

- **Design coverage:**

  | Design | Task |
  |---|---|
  | §3.1–3.2 service, subjects, users, verification | 2, 4, 5 |
  | §3.3 database and roles | 6, 16, 26, 31, 39 |
  | §4 connector interface | 9 |
  | §5.1–5.2 FHIR | 22, 23, 24, 25 |
  | §5.3 archive | 46, 47 |
  | §5.4 file export | 43, 44 |
  | §6.1–6.3 credentials, sealing, rotation | 8, 10, 11, 12, 42 |
  | §6.4 egress | 7, 13, 14, 15 |
  | §7 polling, backfill | 23, 24 |
  | §8 canonical model and provenance | 16, 19 |
  | §9 normalisation, codes, units, times, change control | 17, 18, 19, 21 |
  | §10.1–10.4 index and matching | 16, 20, 34 |
  | §10.3 document patients | 45, 47 |
  | §10.5 `pids` | 48 |
  | §10.6 exclusion and purge | 34, 39, 40 |
  | §11 rules, freshness, change control | 27, 28, 29, 30 |
  | §12 alerts, routing, fatigue, handling, audit | 31–38 |
  | §13 permissions and grants | 6, 11, 16, 31, 32–34 |
  | §14 minimisation, retention, telemetry | 17, 25, 37, 39, 40 |
  | §15 failure modes | 19, 24, 28, 33, 35, 38 |
  | §16 API contract | 11, 32, 33, 34, 37, 42 |
  | §17 threat model | this plan's section; A1–A42 |
  | §18 migrations | 6, 16, 26, 31, 39, 45 |
  | §19 PR split | the PR table |
  | §20 people | "What's needed from people" |

- **Names across tasks:** `reqtoken.SubjectRules`, `reqtoken.AudienceFor`; `replay.New`, `Set.SeenOrAdd`; `dbx.WithHospital(ctx, pool, role, hid, fn)`, `dbx.Gateway/EMR/EMRCore`; `emrops.Ops`, `emrops.Lookup`, `emrops.Subject`; `emrurl.CheckURL`, `emrurl.Origin`, `emrurl.OriginsOf`; `seal.Seal`, `seal.Open`, `seal.Binding`, `seal.Secret`; `recordlink.Build`; `connector.Connector/TypeInfo/Field/Config/Creds/RawRecord/RawSink/TestResult`; `secrets.Store/Path/NewFileStore/NewManaged`; `egress.HTTP/SFTP/CheckOrigin/DecodeJSON/WriteAllowList`; `sink.New/CleanText/CSVCell`; `codes.Load/LoadMaps/ParseUnit/Convert`; `normalise.Run/ParseDecimal/ParseFHIRTime/CheckFuture/Sweep`; `index.Resolve/Propose`; `signals.Send`; `scheduler.Run/Stale`; `rules.Load/Evaluate/Run`, `breaker.Observe`; `emrctl.Client.Request`; `alerts.Register/RunConsumer/Route/UsageBlock`; `emrmaint.Loop`; `bus.EnsureEMRStream`; SQL `emr_platform_health`, `emr_credentials_meta`, `emr_patient_in_hospital`, `emr_merge_patients`, `emr_exclude_patient`, `emr_pause_connection`, `emr_source_status`, `emr_purge_patient`, `emr_release_holds`, `emr_retention_sweep`, `emr_remove_hospital_data`, `mhai_hospital_on_hold`, `emr_patient_candidates`, `emr_create_document_patient`; settings `CORE_EMR_ENABLED`, `CORE_EMR_SEAL_PUBLIC_KEY_PATH`, `CORE_EMR_CONNECTOR_TYPES`, `CORE_IDEMPOTENCY_HMAC_KEY_PATH`, `CORE_EMR_ALLOW_NULL_MRN`, `CORE_EMR_GROUP_THRESHOLD`, `CORE_EMR_REQUIRE_SEALED`, `EMR_*` of Task 4, `EMR_RULE_SET_ACTIVE`, `EMR_BREAKER_MULTIPLE`, `EMR_BREAKER_FLOOR_CRITICAL`.
