# Patient Analysis, named access and clearance letters (hospital side): implementation plan

**Status:** PRs 1–4 merged (PR 4 as #141, migration 00025, departure 18). PR 5 (Tasks 12–14) merged as #149, with migration 00028, OpenAPI 1.19.0 and departures 19–24 below. PR 6 (Tasks 15–16) built on `agent/analysis-build-6`, with migration 00030 and departures 25–28; Task 17 moves to the staff-endpoints plan (departure 12). PR 7 (Tasks 18–20) merged as #169, with migration 00038, OpenAPI 1.28.0 and departures 29–44. PR 8 (Tasks 21–23) built on `agent/analysis-build-8`, with migration 00040, OpenAPI 1.30.0 and departures 45–57. The design is approved (product owner, 7 Nov 2026, after three reviews). ADR 0015 and the SKILLS.md §1 amendment are written as **Accepted** in PR 1 (H4 is given). Live use waits on H1–H3 (design §15), which are people's gates in TODO.md. Nothing in this plan opens them.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Serve mhai-web's Analysis page (five views, filters, a de-identified drill-down, the Analyst role) for one hospital at a time, from finished, nested, frozen calendar periods under disclosure control, with every number guarded in Go before it leaves core. Then add clearance letters, named access and the legal path on top. Analysis is off by default in every PR. Phases 2–6 run on the synthetic demo hospital only.

**Architecture:**

- **Database (core's migrations):** facts per subject and day, written only by a scheduler login (`mhai_analysis_job`) through definer functions that refuse a hospital whose analysis switch is Off. Periods freeze per hospital day, block by block, and months and quarters freeze with their last child. Guarded response bodies are stored once and served unchanged.
- **Go (`services/core/internal/analysis/…`):** `disclose` is a pure package: Layer A per view, the parent-and-children guard (D3) and the filter family (D5). It is built on `internal/smallgroup` and on Plan 7c's parent-set guard and calendar. `analysis` orchestrates freezing, computing, storing and serving. The routes live in `internal/analysis/routes`.
- **Nothing new reaches intelligence.** No worker role, NATS subject or operation touches an analysis table or result (T22). The only new NATS call is core → EMR service identity resolution, in PR 8.

**Tech stack:** Go 1.24 (`net/http`, `pgx/v5`, `goose/v3`, `testcontainers-go`, `math/big`), as Plans 4 and 7c. One new **test-only** import: `gonum.org/v1/gonum/optimize/convex/lp`, for P5's bounds. It is imported only from `_test.go` files, and a test pins that.

**Spec:** `docs/specs/2026-11-07-analysis-backend-design.md` (approved 7 Nov 2026). Section numbers (§n), the constants, D1–D7, I1–I5, P1–P8, T1–T29, AN1–AN23 and H1–H7 are the design's. The plan also builds on Plan 7c's design and plan (`origin/agent/plan-7c-analytics`: `docs/specs/2026-11-07-plan-7c-platform-analytics-design.md`, `docs/plans/2026-11-07-plan-7c-platform-analytics.md`). The contract is mhai-web's (design header).

**PRs:** ten, in this order. Each is mergeable on its own, leaves `main` working, and leaves Analysis off.

| PR | Tasks | What | Depends on | Sign-off |
|---|---|---|---|---|
| **1. Decisions on paper** | 1 | ADR 0015, the SKILLS.md §1 amendment, contract §10, TODO gates | none | none (H4 given) |
| **2. Data foundation** | 2–4 | migration A, keys, the nightly run, `seed-demo`, `features` | **7c-1 merged** (calendar, `mhai_reporting_period_ok`) | none |
| **3. Disclosure engine** | 5–8 | `internal/analysis/disclose`, P1, P2, P4, P5 | **7c-1 merged** (`smallgroup` joint guard, `solve`) | the privacy lead reviews the constants (H1) |
| **4. Aggregate routes** | 9–11 | migration B, stored bodies, `GET /analysis/filters` and `/{view}`, the budget measured | 2, 3 | none |
| **5. De-identified drill-down and reveal** | 12–14 | migration C, pids, caps, `GET /analysis/patients`, `GET /platform/audit/{id}/patients` | 4 | none |
| **6. Analyst grant** | 15–17 | migration D, the trigger, the CLI, step-up on the staff route | 2; Task 17 waits for the staff endpoints | none |
| **7. Letters, hospital side** | 18–20 | task level | 6; documents service (live); Plan 11b design | H3 template before live |
| **8. Named access** | 21–23 | task level | 7; EMR service identity call (live) | H1–H3 before live |
| **9. Audit list, exports, legal path** | 24–25 | task level | 7, 8; object storage | H3 (§8.9) |
| **10. EMR facts** | 26–27 | task level | EMR plan PRs 5, 7, 8b, 13; notifications | H2, H7 |

Phases 2–6 need no sign-off because the switch is On only for the demo hospital (departure 5). PRs 7–10 are planned to task level here. Each gets its own detailed plan, or an update of this one, when its dependency exists.

## Global Constraints

The constraints of Plans 2, 3, 4 and 7c still hold. Identity comes only from the verified token. Another hospital's resource answers 404. Problem bodies carry a fixed code only. No PHI, count, filter, pid, name or note goes in logs, spans, audit payloads or NATS messages. Synthetic data only, plain commit messages with no AI attribution, remote runs only. In addition:

- **Off by default.** With no switch row, the effective position is `off`. Every `/analysis/*` route then answers 404 `not_found`, ingest writes nothing, and `features.analysis` is false. No migration turns anything on.
- **Raw counts never leave core.** They exist in the definer functions' results and in `disclose`'s inputs, in memory, for one computation. Response writers take `disclose.Body` (guarded values only). A test fails if `internal/analysis/routes` imports the cells reader (I2, as in 7c).
- **Every number passes through `internal/smallgroup` or `internal/analysis/disclose`** (Layer A). There are no other guard rules. **Don't write a second copy of a rule that `smallgroup`, 7c's joint guard or 7c's calendar already has.** Where an export is missing, export it there in its own commit (Task 5).
- **Finished, nested periods only (R41a, C3).** Weeks are 7c's month-aligned blocks: 1–7, 8–14, 15–21, 22–end. Days are hospital days (`hospitals.time_zone`), taken from the database's `now()`. No rolling window, custom range or multi-period total exists anywhere in the code.
- **Frozen means frozen.** A frozen period's facts never change. A stored body is never updated or deleted, except by the 25-month sweep. A body that can't be stored is never served (503 instead).
- **Constants, one place:** `disclose.Min = smallgroup.Min` (5), `QuerySetMin = 10`, `FilterCap = 2`, `Grace = 3 days` (blocks), `HistoryMonths = 24`, `DrillMax = 200`, `PatientCap = 300` per person per calendar quarter, lists 10 a minute and 50 a day, named views 5 a day, aggregate reads 30 a minute, `clearance/me` 6 a minute, unusual querying above 100 distinct filter sets a day, body cap 20,000 per hospital per period, body at most 4 MiB, cold body 2 s, `statement_timeout` 5 s. All live in `internal/analysis/disclose/constants.go`, except the rates, which live in `internal/analysis/routes/limits.go`. A test pins every value.
- **Security rules are written once, in Go and SQL.** The switch, cover, analyst rules and caps are core's. Python gets nothing.
- **Definer functions** follow 00017/00018: owner the function's owner role, `SET search_path = pg_catalog, pg_temp`, every relation written `public.<name>`, `REVOKE ALL … FROM PUBLIC`, and `EXECUTE` to the roles in design §10 only. Each takes the hospital from `mhai_current_hospital()`, except the job's, which take `p_hospital` and are executable by `mhai_analysis_job` alone.
- **Every new table:** owner `mhai_migrate`, `ENABLE` and `FORCE` RLS, `REVOKE ALL FROM PUBLIC`, a row in `TestRLSWithoutHospitalSeesNothing`, and `WITH CHECK` tests on INSERT and UPDATE.
- **Migrations:** **PR 2's migration A is `00021_analysis_foundation.sql`, and that number is fixed** (coordinator, 8 Nov 2026): EMR takes 00020 and merges first, and the later Plan 7c PR takes 00022 and up. The rest of this bullet describes the original placeholder scheme for later PRs. **`00030+`: renumber at build time against main.** Main ends at 00018, Plan 7c takes 00019–00020, and the EMR plan starts at 00021. This plan's files are written as `00030_analysis_foundation.sql` (PR 2), `00031_analysis_bodies.sql` (PR 4), `00032_analysis_drill.sql` (PR 5), `00033_analyst_role.sql` (PR 6), and then 00038+ for PRs 7–10. **Each PR renumbers its migration to the next free number on `main` when it is built**, and renames the test references with it. Each has a full Down.
- **ADR 0012:** additive only, except the period change that R41a forces (`from`/`to` that aren't one frozen period get 422). `openapi.yaml` gets a minor bump per PR, and `oasdiff breaking` passes.
- **Test-only switches** go behind constructor options, never package globals (TODO M7).

## Review Focus

1. **A history point or a family parent shows what the request's own body hides.** Someone compares `history` under filters with the period's own filtered body, or a filtered request with its unfiltered parent. Expected: history points are copied from stored bodies, hidden whole where those are hidden. A filtered request whose group is hidden in a stored parent is hidden whole. Tests: `TestHistoryPointsAreStoredBodies` (Task 7), `TestRequestHiddenInParentIsHiddenWhole` (Task 8), `TestHistoryUnderFiltersMatchesStoredBody` (Task 10).
2. **A late fact or a new engine version changes a frozen answer** (anti-goal 6), including "new" moving into a frozen block through `analysis_first_seen`. Expected: byte-identical bodies, and a late case is new nowhere. Tests: `TestLateFactNeverChangesFrozenNew` (Task 2), `TestFreezeServesByteIdentical` (P3, Task 9).
3. **Two core copies or parallel cold requests store different bodies for one context**, or a body computed over the cap is served unstored. Tests: `TestColdBodyOnceUnderRace` (T21, Task 9), `TestOverCapIs503AndUnstored` (Task 9).
4. **The distinct-patient cap leaks under concurrency or across levels.** A subject opened in a block list and its month's list counts twice, or 20 parallel lists exceed 300. Tests: `TestReservePatientsRace` (T28), `TestSubjectCountsOnceAcrossLevels` (Task 12).
5. **A filter or period value in a query string reaches a log line or span**, or `from`/`to` spanning two periods is quietly answered. Tests: `TestAnalysisQueryNotLogged`, `TestFromToMustBeOneFrozenPeriod` (Task 10).

## Where this plan departs from the design

1. **Migration numbers are placeholders** (`00030+`), as the Global Constraints say. The design said "00019 or later". 7c and EMR take 00019–00025+.
2. **New problem codes go in `services/core/internal/httpx/errors.go`** (status, title and detail maps). `authz/errorcodes.go` is the worker's closed code set, not the HTTP problem codes. The design's `invalid` is core's existing **`validation_failed`** (422), as 7c's F7 did.
3. **`not_ready` keeps its existing status, 503.** The design says 409. In `httpx`, a code has one status, and `not_ready` is 503 on every route (7c uses it the same way). mhai-web reads the code. H6(e) is updated to say 503.
4. **Ingest is Go, writing through SQL functions.** `ref_hmac` and `ref_enc` need core's keys, and those keys must never be SQL parameters. So `mhai_analysis_ingest_source(p_hospital, p_after, p_until)` returns coded rows plus the reference. Go computes the HMAC and the ciphertext. `mhai_analysis_ingest(p_hospital uuid, p_until timestamptz, p_facts jsonb)` writes the facts and the watermark. Both refuse a hospital whose switch is Off (I5). The consultation source returns no rows until consultations carry a patient reference (PR 10).
5. **Turning the switch on in phases 2–6 is demo-only.** The two-person raise with real sign-off references is Plan 11b's (design §8.10), not in this plan. Until it exists, the only writer is the provisioning CLI, `core analysis seed-demo <hospital id or short code>`. It writes `on` with the references `DEMO-SYNTHETIC`, for that hospital and for the global row. `mhai_analysis_ingest_source` refuses (22023) any hospital whose switch carries `DEMO-SYNTHETIC`, so no real consultation is ever ingested under a demo switch. Demo facts come from the seed only. A CHECK allows `DEMO-SYNTHETIC` or `SO-YYYY-NNNN` references, and the CLI accepts only the first.
6. **7c's calendar is used for dates, not for "latest".** Analysis calls `calendar.Parse`, `Parent`, `Children` and `BlockOf` on civil dates, interpreted in the hospital's time zone. It never calls `calendar.Latest`, which carries 7c's 15-minute UTC grace. What is answerable comes from `analysis_periods.frozen_at`. `analysis_periods` CHECKs `mhai_reporting_period_ok(level, start, end + 1)`, 7c's SQL twin, so the database refuses a non-period. If 7c's calendar has no "period containing a day" function, Task 5 adds `calendar.Of(k Kind, day time.Time) Period` there.
7. **The scheduler login is a new `mhai_analysis_job`,** with its own pool, `db.WithAnalysisJob`. It follows 7c's `mhai_analytics_job` and `WithAnalyticsJob` and shares their pool-opening helper. If 7c wrote that helper inline, Task 3 moves it into `db.openJobPool(role, passwordFile)` in a separate commit. The design said "the scheduler login".
8. **`analysis_first_seen` gains `first_late boolean`.** A late fact may lower `first_day` only by recording the late day with `first_late = true`. A case is "new" in period P when `first_day ∈ P AND NOT first_late`. A late case is therefore new in no period, and a frozen block's "new" never changes (§4.4, anti-goal 6).
9. **D5 is computed parents-first.** A context `(P, F)` is computed only after every filter parent `(P, F \ {f})` and every child period `(c, F)` is stored. Their guarded states are then fixed constraints: a cell equal to a stored parent's hidden cell is hidden, and a request whose own group is hidden in a stored parent is hidden whole. All five views of `(P, F)` are guarded jointly and stored together. This gives "one state per group across the family" without ever changing a stored body. P2 asserts the design's property against it.
10. **Audit action names are dotted:** `analyst.granted` and `analyst.removed` (§8.7; §6.3 writes underscores), and `audit.patients_viewed` (§8.8's `audit_patients_viewed`), as for the existing `audit.searched`.
11. **T8's "301st distinct patient in a period"** is tested per calendar quarter, as §5.6 defines the cap.
12. **`PATCH /hospital/staff/{id}` doesn't exist on main.** PR 6 ships the trigger, the functions and the CLI. Task 17 adds step-up to the route when the staff endpoints exist. If they aren't on main when PR 6 is built, Task 17 moves into the staff-endpoints plan, with its tests named here.
13. **Drill keys are derived at render time,** not stored in bodies: `HMAC(drill key, hospital ‖ period id ‖ canonical filters ‖ view ‖ row key)`, base64url, 43 characters. They are set only on a shown cell of 5 or more. With the key unchanged, a body is served byte-identically (P3). Rotating the key turns old keys into 422 `too_small`.
14. **`mhai_analysis_cells` returns one view's raw values** as the design's signature says. The orchestrator calls it five times inside one `REPEATABLE READ` transaction, so the five views see one snapshot.
15. **`GET /analysis/filters` without `period`** answers the `periods` list and the latest frozen month's lists. With `period` and `start`, it answers that period's lists (M4).
16. **A location and a setting together are refused** (422 `validation_failed`, `disclose.CheckFilters`; PR 3). A location has its setting, so the pair is the location alone or nothing, and its two parents (`{location}` and `{setting}`) can split the unfiltered view between them: P2 found a hidden cell solvable within that family. Refused, those parents share no family, and what is left between them is part of R-A1, which P2 measures. The design treats every filter pair alike; mhai-web's picker should not offer the pair (H6).
17. **One record per period (ruling C10, PR 3 review).** Departure 9's parents-first order stays, but each body is now guarded against every body already published for its period (all families, and its blocks and months), not its parents alone: `disclose.Guard(raw, children, published)`. PR 4's orchestrator passes them. This closes R-A1 (design D6).
18. **PR 4 as built (startup speed, product owner, 11 Nov).** Migration B is **00025** (EMR PR 5 took 00023, the Gemini provider #137 00024). One stored row per context holds the engine's record and all five views (the key hashes the filters, not the view), so `mhai_analysis_cells` returns all five views in one call (no REPEATABLE READ: frozen facts never change). Pairs are counted in `cells`, not materialised at freeze, and the nightly run doesn't precompute bodies. A subject's attributes are fixed by its first fact (`analysis_subject_attrs`), because the guard's equations need one value per patient at every level. Guards of one hospital and period run one at a time (`mhai_analysis_lock`, PR 3 review condition 1); the guard is given the period's source list (condition 2); each period's knowledge is kept between requests (`disclose.Ledger`), and a body not computed within the cold budget answers 503 `not_ready` while it is finished in the background (condition 3). Drill fields are sent as `null` until PR 5's drill route. `analysis.viewed` and `analysis.unusual_querying` are written but sent bare to mhai-web until their facts are agreed (H6). The extras are on TODO.md's "Analysis: before scale".

19. **Drills only where the rows describe the group (PR 5, the more private choice).** A drill key is set only on the affected view's age band, sex and setting rows, a co-occurring pair, and a finding, in a context whose filters are condition, age band, sex or setting. A location, specialty or source filter, a `by_location` or `by_specialty` row, and prevalence have no drill. Pids are stable within a period (§4.5), so two lists split by a dimension the rows don't show (location, specialty, source) could be compared by pid to tell which patients are in a group of 1–4 that the aggregate views hide; with drills only on what the rows show, every membership a list reveals is already on its rows, which is the §5.2 trade P2's drill run measures (its attacker sees age band, sex, setting, conditions and findings only). Prevalence counts cases, not patients (a month's cases are its blocks' sum), so no list could ever equal its cell. A condition's patients are listed through the condition filter. `findings[].source_label` is always null for the same reason. A row lists what its list was opened on first (the context's condition, the pair, the finding) before filling up to 10, so even a patient with more than 10 conditions shows their membership on the row. The database enforces this too (`mhai_analysis_group` refuses other filters and groups).
20. **One function opens a list (PR 5).** The plan's `mhai_analysis_rows`, `mhai_analysis_pseudonyms` and `mhai_analysis_reserve_patients` are internal (no `EXECUTE` for any app role); the gateway runs only `mhai_analysis_open`, which recounts the group, refuses a count that isn't the shown cell (P0001 `too_small`), reserves, records the list and its rows, draws the pids and returns the rows. No subject id or raw count reaches Go, and the gateway has no grant on `analysis_views` or its rows at all (stricter than the plan's `INSERT`/`SELECT` with `WITH CHECK`; the `WITH CHECK (viewer = mhai_current_user())` is on the definer role's policy).
21. **The audit link is the entry's entity (PR 5).** `analysis_views` has no `audit_id`: the list's `analysis.patients_viewed` entry names the list (`entity analysis_view`, `entity_id` the list's id), and a deferred constraint trigger refuses, at commit, a list without that entry by its viewer in its hospital (T18 in SQL). The audit append comes last in the transaction (the chain lock is held to commit only). The reveal, `mhai_audit_view_pseudonyms(audit id)`, follows the entry to its list; it is owned by a new NOLOGIN role, `mhai_analysis_auditor`, whose only reads are those entries, the lists, their rows and the hospital-side pids, and only `mhai_platform` executes it.
22. **The cap is keyed by the quarter's first day,** not a quarter period id: a block is listable weeks before its quarter freezes. **Its opens are kept while any period of the quarter can be listed** (until the quarter ends before the 25-month cut), not "the quarter plus one month" (design §5.6): swept earlier, an older quarter's cap would reset and a person could open 300 new patients of it every day (security review). The list limits (10 a minute, 50 a hospital day) are counted in SQL from the person's lists under a per-person advisory lock, against the wall clock taken once the lock is held (lists are stamped the same way), so parallel requests can't each count from their own start (exact across core copies), not in process memory. A refused list (either limit, or the cap) writes one flagged entry per person, kind and hospital day (`analysis_drill_flags`), not one per refusal.
23. **The new audit actions are sent bare** (`analysis.patients_viewed`, `analysis.list_limit_reached`, `analysis.patient_cap_reached`, `audit.patients_viewed`), as departure 18 does for `analysis.viewed`, until mhai-web's facts and targets are agreed (H6; TODO.md). A drill-down entry's detail answers `patients: {count}` (the rows sent); the pids leave only through the reveal, and every reveal writes its own entry (no deduplication).
24. **Pids draw on `gen_random_uuid()`** (40 bits of the server's strong random source): core's database has no `pgcrypto`, so `gen_random_bytes` isn't available.
25. **Every analyst change is audited through a commit check (PR 6).** The audit chain's hash is computed in Go (`audit.Write`), so neither the trigger nor the functions can append the entry themselves. Instead each grant or removal is recorded in `analyst_changes` (by the triggers, whoever writes `user_roles`), and a deferred constraint trigger refuses, at commit, a change without its `analyst.granted` or `analyst.removed` entry (entity `user`, the change's id in the payload, by its grantor, in its hospital), as departure 21 does for lists. `mhai_analyst_pending()` lists the transaction's unaudited changes; `analysis.AuditAnalystChanges` appends them, and `analysis.GrantAnalyst`/`RemoveAnalyst` call it. The removal that follows a removed `clinician` (N11) is audited the same way (reason `clinician_removed`), so a writer that removes `clinician` without auditing has its whole transaction refused. The entries are sent bare to mhai-web until their facts are agreed (H6), as departure 23 does.
26. **The rules' owner is a new NOLOGIN role, `mhai_analyst_rules`** (the plan said `mhai_migrate`): its only reads are users' id, hospital, active flag and email, `user_mfa.enabled`, `user_roles`, the analyst audit entries and `analyst_changes`; its only writes are analyst rows and `analyst_changes`. `db.CheckLoginRole` refuses a login that can reach it. The grantor is `mhai_current_user()` as `mhai_gateway` and `mhai.operator_id` as `mhai_provision`; any other session (a provisioning login that inherits the role without `SET ROLE`, for one) has no grantor and is refused, so it can't pose as the app (security review). `mhai.operator_id` is set by the CLI only after `mhai_analyst_cli_users(person email, admin email)` has checked that the admin is an active hospital admin of the person's hospital (the more private choice: no general email lookup for the provisioning role). The CLI's entries are by the `--by` admin (`provision_analyst_append`), `via: cli`, and the policy accepts one only for a change its own transaction made with that admin (`mhai_analyst_change_open`), so the provisioning role can't write an analyst entry with nothing behind it. `--by` is the operator's statement of which admin asked, not that admin's authentication: a CLI entry is an operator action attributed to the named admin. A superuser acting as itself (no `SET ROLE`) is exempt from the rules and the check: it can disable any trigger anyway, and core refuses a superuser login. Test fixtures rely on this.
27. **Stricter than the design, where it was silent (PR 6).** An UPDATE that makes, moves or changes an analyst row is refused (`not_eligible`): the role is granted and removed only. A grantor who isn't an active hospital admin of the person's hospital, an admin of another hospital included, is refused with a sixth message, `forbidden` (403); another hospital's person is `not_found` (404). The cap counts every analyst row of the hospital, inactive accounts included. A removal through `mhai_remove_analyst` also needs an active hospital admin (any, the person included: it only takes access away); a direct `DELETE` is never blocked, only audited. Grants and removals of one hospital take one transaction advisory lock, and a grant refuses any transaction that isn't READ COMMITTED (so the lock gives fresh reads), and checks the grantor before saying whether the person already holds the role, so the cap and the clinician rule hold under concurrency. `core users create` no longer accepts `analyst` (a new account has no two-step and no grantor); it says to use `core users roles`. 00030 refuses to run while an analyst row has no `clinician` row (one given at creation before now), naming the count only; the operator removes the role or adds `clinician` and runs it again. Its Down leaves analyst rows in place, with no rules over them, and drops the change record (the audit chain keeps every entry). A statement that deletes both of a person's rows records the removal as `clinician_removed`.
28. **Task 17 moves to the staff-endpoints plan (departure 12).** `PATCH /hospital/staff/{id}` isn't on main, so PR 6 ships no route, no problem codes and no OpenAPI change. The route must call `analysis.GrantAnalyst` after `StepUpAudited` (a missing code 403 `mfa_required`), map the six refusals (`analysis.Err*`: `forbidden` and `self_change` 403, `not_found` 404, `analysis_off` and `analyst_cap` 409, `not_eligible` 422), call `analysis.AuditAnalystChanges` in any transaction that removes `clinician`, and notify after commit. Its tests are named under Task 17: `TestT26RouteRefusals` and `TestRemoveAnalystEndsPatientAccessAtOnce` (T13).

## Conflicts found (for the coordinator)

- **7c calendar and grace.** 7c's `Period` is UTC `time.Time` with a 15-minute grace in `Latest`. Analysis uses hospital days and a 3-day grace (departure 6). There is no rule conflict, but the 7c builder must not export `Latest` as the only way to get a period.
- **7c exports needed.** `smallgroup.AcrossRowsForced` (the forced-hidden form under `TableForced`) and a generic modular exposure check, `smallgroup.ExposedMod(nVars int, eqs, boxes [][]int) []bool`. If 7c leaves them unexported, Task 5 exports them. That moves code; it doesn't copy it.
- **Similar names:** 7c's `mhai_analytics_*`, `analytics_snapshots` and `internal/analytics` next to this plan's `mhai_analysis_*`, `analysis_snapshots` and `internal/analysis`. The design fixed both sets. A test (`TestAnalyticsAndAnalysisGrantsDisjoint`, Task 2) pins that neither job role can execute the other's functions.
- **`platform_append` chain.** Platform-side audit actions here (`audit.patients_viewed` in PR 5; switch, legal and export actions later) replace `mhai_platform`'s `platform_append` policy again. Each Down restores the policy exactly as the previous migration on `main` left it: 7c's 00020 today.
- **EMR identity call.** The design names `emr.v1.identity.resolve`. The EMR design's audience table has `emr.v1.ctl.<op>` only, and defines no identity operation. Proposed: `emr.v1.ctl.identity_resolve` (audience `emr`, `sub` the viewer, operations-table entry allowing `hospital_admin` and `analyst`). This needs H7 and the EMR plan's agreement before PR 8.
- **EMR exclusion and merges versus frozen periods.** EMR §10.6 purges an excluded patient's data within 24 hours, and §10.2 merges two patient ids into one. Analysis freezes facts and keeps `analysis_subjects.ref_enc` for the audit's retention. Proposed for PR 10, and needing H1 and H3: exclusion deletes the subject's unfrozen facts and its `ref_enc`. Frozen bodies are aggregates and stay. A legal resolution of an excluded subject returns "excluded" for that row. A merge maps the merged reference's subject onto the survivor for unfrozen periods only.
- **EMR lets `mhai_gateway` read `emr.patients (display_name, mrn)`** for alerts. Analysis still resolves names over NATS, per its design (one audited call, nothing cached). This is not a conflict in code, but reviewers should know a shorter path exists, and it must not be used.
- **ADR numbers:** EMR's PR 1 takes 0014, and this plan takes 0015. If EMR hasn't merged first, 0015 is still free.

---

## Running the tests

Never on this machine. Go tests, testcontainers and compose run on the steven-reid build server, with mt5-lab as the fallback (capped at 2 CPUs and 8 GB, one job at a time), through `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`:

```bash
SRC=<your worktree>/ DEST=mhai-analysis \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/core || exit 1
go vet ./... || exit 1
go test -race ./internal/analysis/... ./internal/smallgroup/... ./internal/analytics/... ./internal/audit/... ./cmd/core/... || exit 1
go test -race -tags integration ./internal/db/... ./internal/analysis/... ./internal/platform/... ./cmd/core/... || exit 1
CMDS
```

"**Run (Go, remote):** `<cmd>`" means put `<cmd>` in that heredoc after `cd services/core`. `make smoke` runs remotely and in CI. Property tests take `-run` with a seed (`ANALYSIS_SEED`), and print the seed on failure. The long P2 and P4 runs and the budget test run only when `ANALYSIS_LONG=1`. CI's nightly job sets it, and each PR reports the numbers.

---

## File structure

```
services/core/
  internal/db/migrations/00030_analysis_foundation.sql   roles (writer, reader, job login), subjects, facts, first_seen,
                                                         periods, dictionaries, pseudonyms table, switch, watermark;
                                                         ingest_source, ingest, freeze, seed, position, sweep functions
  internal/db/migrations/00031_analysis_bodies.sql       snapshots; cells, filters, snapshot_put/_get functions
  internal/db/migrations/00032_analysis_drill.sql        views, view_rows, patient budget and opens; rows,
                                                         pseudonyms, reserve_patients, audit_view_pseudonyms; platform_append
  internal/db/migrations/00033_analyst_role.sql          user_roles trigger; grant/remove functions
  internal/db/db.go                                      + WithAnalysisJob, openJobPool (shared with 7c)
  internal/db/analysis_integration_test.go, analysis_drill_integration_test.go, analyst_integration_test.go
  internal/config/config.go (+ _test)                    ANALYSIS_*_KEY_FILE, ANALYSIS_JOB_DB_PASSWORD_FILE
  internal/analysis/disclose/                            constants.go, types.go (Raw, Body, View), layer_a.go,
                                                         periods.go (D3, history), family.go (D5), *_test.go,
                                                         property_p1_test.go, property_p2_test.go, history_p4_test.go,
                                                         bounds_p5_test.go
  internal/analysis/synth/                               seeded synthetic cohorts (tests and seed-demo)
  internal/analysis/keys.go                              Keys: ref HMAC, ref encryption, drill HMAC, note key
  internal/analysis/ingest.go, freeze.go, nightly.go     the nightly run (job pool only)
  internal/analysis/engine.go, store.go, filters.go      parents-first compute, advisory lock, cap, store
  internal/analysis/drill.go, pids.go                    drill keys, rows, pids
  internal/analysis/routes/                              filters.go, views.go, patients.go, limits.go, audit.go,
                                                         render.go (Body → JSON, drill keys), *_integration_test.go
  internal/auth/user.go                                  userView.Features
  internal/platform/audit.go (+ audit_patients.go)       patients{count}; GET /platform/audit/{id}/patients
  internal/audit/schemas.go, catalogue.go                the analysis and analyst actions
  internal/httpx/errors.go                               the new problem codes
  internal/server/server.go                              analysis routes, nightly with the job pool
  cmd/core/analysis.go (+ tests)                         core analysis seed-demo | switch off
  cmd/core/roles.go (+ tests)                            core users roles --add|--remove analyst --by
  openapi.yaml
testdata/web-audit-vocabulary.json                       new kinds as pending_in_web
deploy/, docker-compose*.yml                             analysis job login secret; core mounts the four key files
docs/adr/0015-population-analysis-under-disclosure-control.md
SKILLS.md, TODO.md, CHANGELOG.md, ARCHITECTURE.md, services/core/README.md
docs/specs/2026-10-17-frontend-requirements.md           §10 (H6)
```

---

## Tasks

Abuse-case ids: **T1–T29** and **P1–P8** are the design's (§13.5, §5.7). **N1–N14** are this plan's, listed under "Abuse cases → tests". Each task's first step writes its abuse tests, and they must fail before the code exists.

### PR 1: decisions on paper

### Task 1: ADR 0015, the SKILLS.md amendment, the contract and the gates

**Files:**
- Create: `docs/adr/0015-population-analysis-under-disclosure-control.md` (Status: **Accepted**, 7 Nov 2026, product owner, H4)
- Modify: `SKILLS.md` §1, `docs/specs/2026-10-17-frontend-requirements.md` §10 and §14.9, `TODO.md`, `docs/specs/2026-11-07-analysis-backend-design.md` (status line: ADR and amendment Accepted; a §17 "Departures recorded by the plan" pointing here)

**Content (exact):**
- ADR 0015 says that hospital admins and analysts may see aggregates and pseudonymous rows for their own hospital under the design's disclosure control, and names under a verified letter. It changes contract §3's "no clinical content" rule in those two places only, and lists the residuals R-A1 to R-A5.
- SKILLS.md §1, after "Never build anything that spans patients": *"Exception (ADR 0015): `services/core/internal/analysis` may aggregate across patients of one hospital, and serve de-identified rows, under its disclosure rules. The rule stays absolute for intelligence, retrieval, prompts, models and every NATS operation."* The review checklist gains: *"Does any analysis table, function or result reach a worker role or a NATS subject? (T22)"*
- Contract §10: periods (`period`, `start`, `Meta.period`, `history`, `filters.periods`), pids stable per period, empty lists under A7, the new codes (with `not_ready` at 503, departure 3), `mfa_code` when adding Analyst, and the dotted audit actions. These are H6(a)–(i), marked **Assumed until mhai-web confirms**.
- TODO.md, under "What's left", one entry each, owned by people: **H1** (the privacy lead confirms the approach and the numbers P2 and P5 report), **H2** (clinical), **H3** (legal, including §8.9 before PR 9), **H5** (platform: documents service, object storage, scheduler slot, alerts), **H6** (mhai-web list), **H7** (patient references in consultations; `emr.v1.ctl.identity_resolve`). Also: *"Analysis: no live hospital until H1–H3 are signed."* The "Patient analysis" line becomes a pointer to this plan.

- [ ] **Step 1:** Write the ADR, the amendment and the contract changes.
- [ ] **Step 2:** Check that every link resolves (`grep -o '](.*)'` against the tree) and that `TODO.md` names H1–H3 as gates.
- [ ] **Step 3:** Commit: "Accept ADR 0015 and the SKILLS.md analysis exception; record the Analysis gates".

**PR 1:** open it. Nothing runs, and nothing changes behaviour.

---

### PR 2: data foundation

### Task 2: Migration A: roles, facts, periods, the switch and the job's functions

**Files:**
- Create: `services/core/internal/db/migrations/00030_analysis_foundation.sql`, `services/core/internal/db/analysis_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestRLSWithoutHospitalSeesNothing` seeds every new table)

**Interfaces (the SQL others rely on):**
- Roles, as 00017 creates its reader (created if missing, refused if it has members, kept on Down with every grant revoked): `mhai_analysis_writer` and `mhai_analysis_reader` (NOLOGIN, NOINHERIT). The login `mhai_analysis_job` follows 7c's `mhai_analytics_job`, with a password file and no other grants.
- `mhai_analysis_writer` column `SELECT`s, exactly: `clinical_queries (id, hospital_id, created_at, urgency, specialty)`, `clinical_decisions (query_id, hospital_id, diagnosis, icd10_code, hga_decision, trust_score, degraded, created_at)`, `feedback_events (query_id, hospital_id, action, status, created_at)`, `hospitals (id, status, time_zone)`. Never `response_text`, `clinical_question`, `citations_json`, `patient_data_json` or `medical_history`.
- Tables, as design §4.3: `analysis_subjects (hospital_id, subject_id uuid PK default gen_random_uuid(), ref_hmac bytea, ref_enc bytea, created_day date, UNIQUE (hospital_id, ref_hmac))`; `analysis_facts (… §4.3 columns …, period_late bool NOT NULL)`, with CHECKs on `kind`, `age_band`, `sex` and `setting` (the contract's codes); `analysis_first_seen (hospital_id, subject_id, condition_id, first_day, first_late bool, PK (hospital_id, subject_id, condition_id))`; `analysis_conditions`, `analysis_locations`, `analysis_sources`; `analysis_periods (id uuid, hospital_id, level, start date, "end" date, frozen_at, engine_version, UNIQUE (hospital_id, level, start), CHECK (public.mhai_reporting_period_ok(level, start, "end" + 1)))`; `analysis_pseudonyms (hospital_id, side, scope, period_id, subject_id, pid, UNIQUE (hospital_id, side, scope, period_id, subject_id), UNIQUE (hospital_id, side, period_id, pid))`; `analysis_switch (hospital_id uuid NULL UNIQUE NULLS NOT DISTINCT, position text CHECK IN ('off','on'), signoff_clinical, signoff_privacy, signoff_legal text CHECK (~ '^(DEMO-SYNTHETIC|SO-[0-9]{4}-[0-9]{2,8})$'), since, changed_by, CHECK (position = 'off' OR all three refs NOT NULL))`. A NULL `hospital_id` is the global row. `analysis_watermarks (hospital_id PK, ingested_until timestamptz)`.
- Functions:
  - `mhai_analysis_position(p_hospital uuid) RETURNS text`: the lower of the global and hospital positions; `off` when either row is missing. Definer, `STABLE`. `EXECUTE` to `mhai_gateway` (its own hospital only: it returns `off` unless `p_hospital = mhai_current_hospital()`), `mhai_analysis_job`, `mhai_platform`.
  - `mhai_analysis_ingest_source(p_hospital uuid, p_after timestamptz, p_until timestamptz) RETURNS TABLE (…coded columns…, ref text)` (departure 4): owner writer; refuses (22023) when the position is off, when the switch carries `DEMO-SYNTHETIC`, or when `p_until > now()`. It returns no rows until a patient reference column exists (PR 10).
  - `mhai_analysis_ingest(p_hospital uuid, p_until timestamptz, p_facts jsonb) RETURNS integer`: owner writer. It refuses when the switch is off. It upserts subjects by `ref_hmac`, inserts facts with `period_late = (the fact's block is frozen)`, updates `first_seen` (departure 8), advances the watermark, and returns the late count. It refuses a fact with an unknown key, a free-text field, or a day outside the last 25 months.
  - `mhai_analysis_freeze(p_hospital uuid) RETURNS integer`: owner writer. Under `pg_advisory_xact_lock(hashtext('mhai.analysis.freeze'), hashtext(p_hospital::text))`, it creates the period rows and marks `frozen_at` on every block whose `end + 1 day + 3 days` has passed in the hospital's zone (by `now()`). Then it marks months whose last block froze in this call or earlier, and then quarters. Each period is frozen once, with `engine_version` taken from `current_setting('mhai.analysis.engine')`. It returns the number frozen.
  - `mhai_analysis_seed(p_hospital uuid, p_facts jsonb)`: owner writer; `EXECUTE` to `mhai_provision`. It accepts only a hospital whose switch refs are all `DEMO-SYNTHETIC`. It writes subjects (`ref_hmac` is random, `ref_enc` is NULL) and facts through the same insert path as `ingest`.
  - `mhai_analysis_sweep(p_hospital uuid)`: deletes facts and `first_seen` older than 25 months. Subjects are deleted only when no `analysis_view_rows` cites them (the table arrives in PR 5; until then, every unreferenced subject older than 25 months).
- `EXECUTE` on `ingest_source`, `ingest`, `freeze` and `sweep` goes to `mhai_analysis_job` only. Nothing goes to `mhai_gateway`, `mhai_platform`, `mhai_worker` or `intelligence_app`.
- Grants: `mhai_gateway` gets `SELECT` on the dictionaries for its hospital (RLS) and nothing on any other analysis table. The switch: `mhai_platform` gets `SELECT`, plus `UPDATE` and `INSERT` through a `WITH CHECK` that refuses `DEMO-SYNTHETIC` (the platform flow is Plan 11b's). `mhai_provision` gets `INSERT`/`UPDATE` with a `WITH CHECK` that allows only `DEMO-SYNTHETIC` or `position = 'off'`.

- [ ] **Step 1: Write the failing tests** (`-tags integration`):
  - `TestAnalysisTablesRLS` (every table in `TestRLSWithoutHospitalSeesNothing`; INSERT/UPDATE `WITH CHECK`);
  - `TestWorkerRolesSeeNoAnalysis` (**T22**: as `intelligence_app` and `mhai_worker`, `SELECT` on every analysis table and `EXECUTE` on every analysis function fail);
  - `TestGatewayCannotWriteAnalysis` (**T24**: direct `INSERT`/`UPDATE`/`DELETE` on facts, subjects, pseudonyms, periods and the switch fail as `mhai_gateway`, and so does `EXECUTE` on the job's functions);
  - `TestIngestRefusesSwitchOff` (**T25**, SQL half: `mhai_analysis_ingest` and `_ingest_source` for a hospital that is off, or whose global is off, raise 22023 and write nothing);
  - `TestIngestSourceRefusesDemoSwitch` (N2);
  - `TestWriterSeesNoText` (N3: as `mhai_analysis_writer`, `SELECT response_text`, `clinical_question`, `patient_data_json` fail);
  - `TestFreezeBlocksThenParents` (in Africa/Lagos and a UTC+14 zone: a block freezes at local midnight + 3 days after its end, not before; a month freezes with its last block, a quarter with its last month; a second call freezes nothing; parallel calls freeze once);
  - `TestLateFactExcludedEverywhere` (a fact dated in a frozen block is stored with `period_late`; counted in neither the block, the month nor the quarter);
  - `TestLateFactNeverChangesFrozenNew` (N4, Review Focus 2: `first_late` set; "new" in the frozen block and in the next unfrozen block unchanged by it);
  - `TestPeriodsCheckRefusesNonPeriods` (an ISO week, a 30-day range: the CHECK fails; uses 7c's `mhai_reporting_period_ok`);
  - `TestSwitchWriters` (the provisioning login can write only `DEMO-SYNTHETIC` or `off`; `mhai_platform` can't write `DEMO-SYNTHETIC`; `mhai_gateway` can't write at all; `mhai_analysis_position` gives the lower of the two and `off` without rows);
  - `TestTempTableCannotShadow` (N1: a temporary `analysis_switch` or `analysis_facts` created by the calling role is ignored by every definer function);
  - `TestAnalyticsAndAnalysisGrantsDisjoint` (`mhai_analytics_job` can't execute any `mhai_analysis_*` function, and `mhai_analysis_job` can't execute any `mhai_analytics_*` one);
  - `TestSeedOnlyForDemoHospital`;
  - `TestMigration00030DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail (no migration).
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests and the whole `internal/db` integration suite (7c's 00019–00020 tests included).
- [ ] **Step 5:** Commit: "Migration 00030: analysis facts, nested periods and a switch that stops ingest".

### Task 3: Keys, the job pool and the nightly run

**Files:**
- Create: `services/core/internal/analysis/{keys.go,keys_test.go,ingest.go,freeze.go,nightly.go,nightly_test.go,nightly_integration_test.go}`
- Modify: `services/core/internal/config/config.go` (+ `_test`), `services/core/internal/db/db.go` (`WithAnalysisJob`; `openJobPool` shared with 7c, departure 7), `services/core/internal/server/server.go` and `cmd/core/serve.go` (start the nightly loop with the job pool), `deploy/` and `docker-compose.yml` (the job login's password file and the four key files, development values generated by `make` and never committed), `services/core/cmd/core/compose_test.go`

**Interfaces:**
- `config.Config.Analysis{RefHMACKey, RefEncKey, DrillKey, NoteKey []byte; JobDBPasswordFile string}` from `ANALYSIS_REF_HMAC_KEY_FILE`, `ANALYSIS_REF_ENC_KEY_FILE`, `ANALYSIS_DRILL_KEY_FILE` and `ANALYSIS_NOTE_KEY_FILE`. Each file holds 32 random bytes in base64. Start-up is refused for a missing file, a placeholder, a wrong length, or two equal keys. The error names the variable only.
- `analysis.Keys` with `RefHMAC(ref string) []byte` (HMAC-SHA256), `SealRef(ref string) ([]byte, error)` and `OpenRef([]byte) (string, error)` (AES-256-GCM, with the hospital id as associated data), and `Drill(parts ...[]byte) string` (43 characters of base64url). `Keys` has no `String`/`GoString` that prints bytes.
- `analysis.Nightly{Pool (job pool only), Keys, Clock, Log}` with `Run(ctx)`: every 5 minutes under `pg_try_advisory_lock(hashtext('mhai.analysis.nightly'))`. For each hospital whose position is `on` and whose local time is past midnight + 30 minutes with no run today, it runs, in order: ingest (`ingest_source` → keys → `ingest`), sweep, freeze (design §11 steps 1, 2 and 4; step 3, ending letters, comes in PR 7). It logs the hospital id, the step and a fixed reason only. The metric `mhai_analysis_late_facts_total{hospital}` counts late facts. The alert `analysis_freeze_failed` fires on any failure.
- `db.WithAnalysisJob(ctx, pool, fn)`. No route handler gets this pool.

- [ ] **Step 1: Write the failing tests:**
  - `TestAnalysisKeyFiles` (N6: missing, placeholder, short, equal, world-readable: refused; the error names the variable only);
  - `TestKeysNeverPrinted` (`%v`, `%+v`, `slog` of `Keys` show no key bytes);
  - `TestSealRefBoundToHospital` (opening with another hospital's id fails);
  - `TestNightlyRunsOnlySwitchedOnHospitals` (**T25**, Go half: two hospitals, one off; after a run, the off one has no facts and no periods);
  - `TestNightlyOrderAndOneCopy` (two loops wake together: one runs; ingest before freeze);
  - `TestNightlyLogsNoValues` (a forced failure logs ids and a fixed reason; no count, ref or day);
  - `TestRoutesCantReachAnalysisJobPool` (N3: neither `internal/analysis/routes` nor `internal/platform` imports `WithAnalysisJob`; AST test);
  - `TestAnalysisQueryNotLogged` (N5: a request to `/api/v1/analysis/prevalence?condition=X&period=month&start=…` leaves no `X` and no `start` value in core's log output or span attributes; pins today's behaviour so a later access log can't add it).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/... ./internal/config/... && go test -race -tags integration ./internal/analysis/... ./cmd/core/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and `compose_test.go`.
- [ ] **Step 5:** Commit: "Analysis keys, the job's own pool and the nightly ingest and freeze".

### Task 4: `core analysis seed-demo`, the synthetic cohort and `features`

**Files:**
- Create: `services/core/internal/analysis/synth/{synth.go,synth_test.go}`, `services/core/cmd/core/{analysis.go,analysis_test.go,analysis_integration_test.go}`
- Modify: `services/core/cmd/core/main.go` (usage and dispatch), `services/core/internal/auth/user.go` (`userView.Features`), `auth` integration tests (`userKeys` gains `features`), `openapi.yaml` (`User.features`; minor bump)

**Interfaces:**
- `synth.Cohort(seed int64, opts Options) []synth.Subject`, where `Options{Subjects, Months int; SmallBias float64; Start time.Time}`. A subject is `{Ref string; Facts []analysis.Fact}`. It is deterministic per seed, covers every kind (consultation, diagnosis, finding, alert), every age band and sex code, "Not standardised", and runs of zeros. P1–P8 use it too.
- `core analysis seed-demo <hospital id or short code> [--seed N] [--months 24]`, as `mhai_provision`. It sets the global and hospital switch rows to `on` with `DEMO-SYNTHETIC` refs (refusing if the global row carries `SO-` refs, which it must not change), writes the cohort through `mhai_analysis_seed`, and prints `analysis demo <hospital id> <subjects>`. `core analysis switch off <hospital id or short code|--global>` lowers a switch (audited later, with Plan 11b's actions; today it writes a provisioning audit row `analysis.switch_lowered` with `via: cli`).
- `features` on the sign-in and session replies: `{analysis: bool, named_access: bool}` (§6.5). `analysis` = position `on` ∧ (`hospital_admin` ∨ `analyst`) ∧ two-step on. `named_access` stays `false` until PR 8. It is read through `mhai_analysis_position`, in the transaction that builds the reply.

- [ ] **Step 1: Write the failing tests:**
  - `TestCohortDeterministicAndComplete`;
  - `TestSeedDemoRefusesRealSwitch` (N2: the global row with `SO-` refs, or the CLI asked for anything but demo: refused, nothing written);
  - `TestSeedDemoThenNightlyFreezes` (integration: seed 24 months, run the nightly loop with a test clock, every finished period frozen in order);
  - `TestFeaturesOffByDefault` (a hospital admin with two-step, no switch rows: `analysis` false);
  - `TestFeaturesNeedRoleAndMFA` (a clinician without `analyst`, and an admin without two-step: false; an admin at the demo hospital: true);
  - `TestSwitchOffCLI` (global off: every hospital's position becomes `off` at once; its audit row has a closed payload);
  - the openapi contract test with `User.features`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/synth/... ./cmd/core/... ./internal/auth/... && go test -race -tags integration ./cmd/core/... ./internal/auth/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and `oasdiff breaking` (additive).
- [ ] **Step 5:** Commit: "Seed the demo hospital's synthetic analysis and report features on the user".

**PR 2:** CHANGELOG (Added: the analysis data foundation, off by default), `services/core/README.md` (the key files and the job login), ARCHITECTURE.md (the nightly run). Then run `security-review` and open the PR.

---

### PR 3: the disclosure engine

### Task 5: The shared pieces from 7c, exported where they live

**Files:**
- Modify (only if 7c left them unexported): `services/core/internal/smallgroup/{joint.go,rules.go}` (export `AcrossRowsForced` and `ExposedMod`), `services/core/internal/analytics/calendar/calendar.go` (`Of`; and, if 7c's test-only Monday-to-Sunday calendar sits in a `_test.go` file, move it to `internal/analytics/calendar/calendartest`)
- Create: `services/core/internal/analysis/disclose/{constants.go,constants_test.go,types.go}`

**Interfaces:**
- From 7c, used as is: `calendar.Parse`, `Parent`, `Children`, `BlockOf`, the `Kind` values `Week`, `Month`, `Quarter`; `smallgroup.AcrossRowsFrozen` (the parent set: children frozen, only the parent column may hide more); `smallgroup.TableForced`; `solve.Exposed(nVars int, eqs [][]int, boxes [][]int) []bool`.
- Exported here if missing: `smallgroup.AcrossRowsForced(all []int64, rows [][]int64, forcedAll []bool, forced [][]bool) ([]Guarded, [][]Guarded)`, `smallgroup.ExposedMod(nVars int, eqs [][]int, boxes [][]int) []bool`, and `calendar.Of(k Kind, day time.Time) Period` (the day as a civil date).
- `disclose.View` (`prevalence`, `affected`, `co-occurring`, `findings`, `quality`); `disclose.Raw` (per view, the contract's fields as `int64` counts, plus the sorted minute values that medians need); `disclose.Body` (per view, the contract's fields as `smallgroup.Guarded`/`GuardedShare`, plus internal row keys; never a raw value); `disclose.Filters` (at most `FilterCap`, canonical order: `condition`, `age_band`, `sex`, `location`, `setting`, `specialty`, `source`).

- [ ] **Step 1: Write the failing tests:** `TestConstantsPinned` (every Global Constraints value); `TestSharedExportsAreMovesNotCopies` (7c's tests in `smallgroup` and `analytics/calendar` pass unchanged, and `grep -c 'func bridge'` in `internal/` is 1); `TestOfAgreesWithParse` (every day 2024–2030, all three kinds); `TestBodyHasNoRawField` (reflection: `Body` holds no `int`/`int64` field except row keys).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/smallgroup/... ./internal/analytics/... ./internal/analysis/disclose/...` → fail.
- [ ] **Step 3:** Export or move (one commit, no edits), then add `disclose`'s types (a second commit).
- [ ] **Step 4:** Run the tests, plus C19, C20 and 7c's A5–A8 unchanged.
- [ ] **Step 5:** Commits: "Export the forced table guard and the modular exposure check for Analysis" and "Analysis disclosure: constants and types".

### Task 6: Layer A per view (A1–A9)

**Files:**
- Create: `services/core/internal/analysis/disclose/{layer_a.go,layer_a_test.go,property_p1_test.go}`

**Interfaces:**
- `disclose.GuardResponse(raw map[View]Raw, filtered bool, forced Forced) (map[View]Body, error)`. It guards all five views of one context together. `Forced` lists cells that must be hidden (from D3 and D5, Tasks 7 and 8). It runs the rules below until nothing changes:
  - A1 `Count` on every count;
  - A2: the by-condition table and the series table, each as `AcrossRowsForced` with rows `[cases, new, existing]` and `all` = `total`, sharing the `total` row (a cell hidden in one table's `all` is forced in the other's);
  - A3: each partition as `Set(patients, …)`;
  - A4: `ShareOf`, and medians hidden under 5 values or over a hidden count, rounded to a whole or a half;
  - A5: pairs as `Set(a_patients, both)`, top 20 by shown value, hidden pairs after them in name order;
  - the contract's other sets, as guards.ts checks them (findings and alerts, quality's confidence, rated and corrections);
  - A6 order; A9 no repeated key;
  - A7: when `patients` is hidden, or `filtered && patients < QuerySetMin`, every number is hidden and every list is empty.
- Inconsistent input (a set that doesn't add up, a duplicated key) hides the whole context and returns `ErrInconsistent`. The caller raises the alert (§12).
- `disclose.Drillable(b Body, view View, rowKey string) bool`: shown and at least 5 (A8). The key itself is made at render time (departure 13).

- [ ] **Step 1: Write the failing tests:** `TestT1FemaleMaleTotal` (**T1**: female 212, male 3, total 215 → male and the next-smallest member hidden); `TestT3HiddenPairAfterShown` (**T3**); `TestT7NarrowViewEmptyLists` (**T7**: 7 patients with a filter → everything hidden, every list empty); `TestByConditionSumsToTotal`; `TestSeriesSharedTotalForcedBothWays`; `TestMedianHalfAndHidden`; `TestNoRepeatedKeys`; `TestInconsistentInputHidesAll`; `TestDrillableOnlyShownFiveOrMore`; **P1** `TestP1NoHiddenCellSolvable` (synth cohorts biased to small groups; every view, level and filter set within the cap; every equation the response implies, cross-view equalities included; `solve.Exposed` exposes nothing; `ANALYSIS_LONG=1` for 2,000 seeds, 50 otherwise).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/disclose/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Analysis Layer A: every number of a response guarded together".

### Task 7: Nested periods guarded across levels, and history (D1–D3)

**Files:**
- Create: `services/core/internal/analysis/disclose/{periods.go,periods_test.go,history_p4_test.go}`

**Interfaces:**
- `disclose.ParentForced(parent map[View]Raw, children []ChildBodies) Forced`. `ChildBodies{Raw map[View]Raw; Bodies map[View]Body}` are the child periods' raw values and stored bodies, in order. For each additive measure (cases, new, consultations, results, alerts, corrections) and each row key, it runs `smallgroup.AcrossRowsFrozen` with the rows being the row keys plus `total`, and the columns being the parent then its children. The children's hidden pattern is frozen. It returns the parent cells to force hidden. Distinct counts get no equation (R-A2).
- The caller runs `GuardResponse(parent, filtered, ParentForced(…) ∪ family)` and repeats while `ParentForced` on the new body adds cells (the "until nothing changes" of D3).
- `disclose.HistoryPoint(b Body) Point` copies `cases`, `new` and `patients` as published. A body hidden whole gives every field `{value: null, suppressed: true}`. `disclose.History(bodies []Body) []Point` returns at most 7 points with no total (I2).

- [ ] **Step 1: Write the failing tests:** `TestT2MonthTotalHidesLoneHiddenBlock` (**T2**); `TestQuarterWithOneHiddenMonthIsHidden` (**T4**, last clause); `TestChildrenNeverChange` (the stored children's bodies are byte-equal before and after the parent's guard); `TestParentHiddenWhenHiddenChildrenSumOneToFour`; `TestHistoryPointsAreStoredBodies` (Review Focus 1); `TestHistoryHasNoTotal`; **P4** `TestP4HistoryNoExposure` (synth from the hospital's first period over 24 months with zeros and small counts; publish every block, month and quarter; the tree of equations plus each response's own; zeros and the start as constants; `solve.Exposed` exposes no hidden cell), and `TestP4ISOWeeksWouldFail` (the same with 7c's test Monday-to-Sunday calendar is exposed: the C3 regression).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/disclose/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests; `ANALYSIS_LONG=1` for P4's 500 histories.
- [ ] **Step 5:** Commit: "Guard each period with its children; history copied from stored bodies".

### Task 8: The filter family (D4, D5), and the measured residuals (P2, P5)

**Files:**
- Create: `services/core/internal/analysis/disclose/{family.go,family_test.go,property_p2_test.go,bounds_p5_test.go}`

**Interfaces:**
- `disclose.CheckFilters(f Filters) error`: more than `FilterCap` gives `ErrTooManyFilters` (route: 422 `too_many_filters`); an unknown code gives `ErrInvalid`.
- `disclose.FamilyForced(raw map[View]Raw, f Filters, parents []ParentContext) (Forced, hiddenWhole bool)`. `ParentContext{Filters; Bodies map[View]Body}` are the stored filter parents `(P, F \ {f})` (departure 9). A cell of the request that is the same group as a parent's hidden cell is forced hidden. `hiddenWhole` is true when the request's own group (`F`'s last filter value inside parent `F \ {f}`'s breakdown) is hidden in any parent (T5).

- [ ] **Step 1: Write the failing tests:** `TestT5ComplementByFilterHiddenWhole` (**T5**); `TestT6ThirdFilterRefused` (**T6**); `TestRequestHiddenInParentIsHiddenWhole` (Review Focus 1); `TestOneStatePerGroupInFamily`; **P2** `TestP2FamilyExhaustive` (small dimensions; every context of a period at the cap, computed parents-first as the orchestrator will; equations over atomic cells; **assert** that no hidden cell within any family can be solved and every group has one state; **report** the share of hidden cells solvable across families (R-A1), and, in the drill-capable run, the subgroups of 1–4 exposed); **P5** `TestP5BoundsReported` (per response, and per parent with children under the distinct-count bounds; report the share of hidden cells whose feasible range excludes 0 and is narrower than 0–4; `gonum` LP, test-only); `TestGonumOnlyInTests` (N13).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/disclose/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run with `ANALYSIS_LONG=1`. Put the R-A1 and R-A2 numbers, and the drill-run numbers, in the PR description for H1.
- [ ] **Step 5:** Commit: "Analysis filter families: one state per group, parents first; R-A1 and R-A2 measured".

**PR 3:** run `security-review` and open it. Ask the privacy lead to review the constants and the two numbers (H1, "PR 3 review").

---

### PR 4: aggregate routes

### Task 9: Migration B, stored bodies and the orchestrator

**Files:**
- Create: `services/core/internal/db/migrations/00031_analysis_bodies.sql`, `services/core/internal/analysis/{engine.go,store.go,filters.go,engine_test.go,engine_integration_test.go}`
- Modify: `services/core/internal/analysis/nightly.go` (precompute at freeze)

**Interfaces:**
- `analysis_snapshots (hospital_id, period_id, query_hash bytea, body jsonb CHECK (octet_length(body::text) <= 4194304), stored_at, PRIMARY KEY (hospital_id, period_id, query_hash))`. No `UPDATE` grant. `DELETE` only through the sweep.
- `mhai_analysis_cells(p_period uuid, p_filters jsonb, p_view text) RETURNS jsonb` (owner reader; `EXECUTE` to `mhai_gateway`): raw values of one view for one frozen period of `mhai_current_hospital()`. It refuses an unfrozen or foreign period, more than 2 filters, or an unknown filter key (22023). It counts only facts with `period_late = false`, uses `first_late` for "new" (departure 8), and runs under `SET LOCAL statement_timeout = '5s'`. Pairs come from `analysis_pairs (hospital_id, period_id, a, b, a_patients, both)`, materialised by `mhai_analysis_freeze` (§11).
- Migration B also creates `analysis_pairs` and replaces `mhai_analysis_freeze` (`CREATE OR REPLACE`, the same signature and grants) so that it fills the pairs when a period freezes. Down restores migration A's body. It also creates `analysis_view_log (session_id, query_hash, period_id, at)` for Task 10's once-per-session audit, which `mhai_gateway` may `INSERT` and `SELECT` for its hospital, and which the sweep clears after 24 hours.
- `mhai_analysis_filters(p_period uuid) RETURNS jsonb`: conditions with at least 5 patients in that frozen period, plus "Not standardised"; locations, specialties and sources.
- `mhai_analysis_snapshot_put(p_period uuid, p_hash bytea, p_body jsonb) RETURNS boolean` (`INSERT … ON CONFLICT DO NOTHING`; refuses an unfrozen or foreign period, a body over 4 MiB, a JSON number 1–4 in any `value`, or a 20,001st body for the period) and `mhai_analysis_snapshot_get(p_period uuid, p_hash bytea) RETURNS jsonb`.
- `analysis.Engine{Pool, Keys, Version string}` with `Body(ctx, hospital uuid.UUID, period Period, f disclose.Filters, v disclose.View) ([]byte, error)`:
  1. read stored;
  2. otherwise, take `pg_advisory_xact_lock` on `(hospital, period, filters)` and re-read;
  3. ensure, recursively, every filter parent and every child period of the same filters is stored (departure 9);
  4. read the five raw views (one `REPEATABLE READ` transaction, departure 14);
  5. `GuardResponse` with `ParentForced ∪ FamilyForced`, repeated until nothing changes;
  6. store all five bodies.

  A store refused for the cap gives `ErrCap` (503). Nothing is ever served unstored.
- `analysis.Filters` canonical JSON and `Hash(view)` (SHA-256 over the view and the canonical filters).
- Nightly precompute after a freeze: the five unfiltered bodies and the filters body, children before parents.

- [ ] **Step 1: Write the failing tests:** **P3** `TestFreezeServesByteIdentical` (freeze; store; add late facts, change a dictionary label, run a second engine with a newer `Version`; every stored body is byte-identical; a body first computed after that uses frozen inputs only); `TestColdBodyOnceUnderRace` (**T21**: 20 parallel cold requests, one computation (counted by a hook), one row); `TestOverCapIs503AndUnstored` (**T21**: at the cap, a new context gives `ErrCap` and no row); `TestSnapshotPutRefusesSmallShownAndUnfrozen` (N7); `TestCellsRefusesUnfrozenAndForeignPeriod` (**T10**, SQL half); `TestParentsComputedFirst`; `TestFilterListPerFrozenPeriod` (**P6**: a condition with 4 patients isn't offered, one with 5 is; the list for a frozen period never changes; comparing two periods' lists reveals no 4-to-5 change within one); `TestStatementTimeoutGives503`; `TestEngineInconsistentRaisesAlert`; `TestMigration00031DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/... && go test -race -tags integration ./internal/db/... ./internal/analysis/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Store each guarded Analysis body once, parents first, under a lock and a cap".

### Task 10: `GET /analysis/filters` and `GET /analysis/{view}`

**Files:**
- Create: `services/core/internal/analysis/routes/{register.go,views.go,filters.go,limits.go,audit.go,render.go,views_integration_test.go,render_test.go}`
- Modify: `services/core/internal/server/server.go`, `services/core/internal/httpx/errors.go` (`too_many_filters`, `invalid_range`: 422; departure 2), `services/core/internal/audit/{schemas.go,catalogue.go}`, `testdata/web-audit-vocabulary.json`, `openapi.yaml`, `docs/specs/generated/permissions.md` (`go run ./cmd/permgen`)

**Interfaces:**
- Routes: `authz.Rule{AnyRole: [hospital_admin, analyst], NeedsMFA: true}`. Platform accounts get 403 `forbidden`, as for every clinical route. The handler first checks `mhai_analysis_position` for the principal's hospital: off gives 404 `not_found`.
- Query parsing: `period` (`week|month|quarter`) and `start` (`YYYY-MM-DD`, the period's first day), or `from` and `to` that span exactly one period. Anything else gives 422 `invalid_range`. A period older than 24 months gives 422 `invalid_range`. A period closed but not frozen gives 503 `not_ready` (departure 3). An unknown view gives 404. The filters go through `disclose.CheckFilters`.
- The response: the stored body rendered with `Meta {updated_at (the body's stored_at), from, to, bucket, patients, period: {level, start}}`, `history` (up to 7 earlier periods of the same level, each from its stored body for the same view and filters, computed if missing), and drill keys on drillable cells (departure 13). `Cache-Control: no-store`.
- Limits: 30 aggregate reads a minute per session gives 429 `rate_limited`. Over 100 distinct filter sets in a hospital day per person writes one flagged `analysis.unusual_querying` (`rows`) per day.
- Audit `analysis.viewed` (`view`, `period`, `filters` as codes), once per session, filter set and period. The dedup key is held in `analysis_view_log` (migration B, Task 9) and written in the request's transaction.

- [ ] **Step 1: Write the failing tests:**
  - `TestAnalysisOffIs404` (no switch row: every route 404, before any period parsing);
  - `TestAnalysisRolesAndMFA` (a clinician 403, an admin without two-step 403 `mfa_required`, a platform account 403);
  - `TestFromToMustBeOneFrozenPeriod` (**T4**, Review Focus 5: two periods, a rolling 30 days, an ISO week → 422 `invalid_range`; `from`/`to` exactly a frozen month → 200, the same bytes as `period=month&start=`);
  - `TestNotFrozenIs503NotReady`;
  - `TestThirdFilterIs422` (**T6**);
  - `TestHistoryUnderFiltersMatchesStoredBody` (**T4**, Review Focus 1);
  - `TestOtherHospitalGetsNothing` (**T10**: hospital B's session sees only B; a drill key minted for A is 422 at B (with PR 5));
  - `TestNoShownOneToFourAnywhere` (every route's JSON over a synth hospital);
  - `TestRenderImportsNoCellsReader` (I2: `routes` doesn't import the raw reader);
  - `TestAggregateRateLimit`;
  - `TestViewedAuditedOncePerSessionFilterPeriod`;
  - `TestUnusualQueryingFlaggedOnce`;
  - `TestAuditPayloadsClosed` (no filter value outside the code sets, no free text);
  - `TestAnalysisQueryNotLogged` (N5, rerun through the real routes);
  - `TestFiltersRoute` (`periods` lists only frozen periods within 24 months; without `period`, the latest frozen month; departure 15);
  - the openapi contract test and `TestEveryActionHasSchema`/`…CatalogueRow`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/analysis/... ./internal/audit/...` → fail.
- [ ] **Step 3:** Implement. Regenerate `permissions.md`.
- [ ] **Step 4:** Run the tests and `oasdiff breaking` (the period change is the one allowed break, noted in CHANGELOG).
- [ ] **Step 5:** Commit: "Serve the five Analysis views and filters from stored, frozen bodies".

### Task 11: The time budget, smoke and docs

**Files:**
- Create: `services/core/internal/analysis/budget_integration_test.go`
- Modify: `services/core/internal/smoke/smoke_test.go`, `CHANGELOG.md`, `services/core/README.md`, `ARCHITECTURE.md`, `docs/specs/2026-10-17-frontend-requirements.md` (§10 rows to **Built (PR 4)**)

- [ ] **Step 1: Write the failing tests:** `TestColdFamilyUnderTwoSeconds` (`ANALYSIS_LONG=1`: a synth hospital of 200,000 subjects; the worst-case cold request (a quarter with 2 filters, parents and children cold) finishes in 2 s on steven-reid; p50 and p99 in the PR); a smoke step (seed-demo at a test hospital, a test clock past a block's freeze, `GET /analysis/prevalence?period=week&start=…` 200; with the switch lowered, 404).
- [ ] **Step 2: Run (Go, remote):** `ANALYSIS_LONG=1 go test -tags integration -run TestColdFamilyUnderTwoSeconds ./internal/analysis/...` and `make smoke` → fail.
- [ ] **Step 3:** Fix what the measurement shows (indexes `analysis_facts (hospital_id, day)` and `(hospital_id, subject_id)` are in migration A; add only what the plan shows is needed, in migration B).
- [ ] **Step 4:** Run both.
- [ ] **Step 5:** Commit: "Measure the cold Analysis budget; smoke and docs for the aggregate routes".

**PR 4:** run `security-review` and open it. The design requires the budget measurement before merge.

---

### PR 5: de-identified drill-down and reveal

### Task 12: Migration C: views, pids and the distinct-patient cap

**Files:**
- Create: `services/core/internal/db/migrations/00032_analysis_drill.sql`, `services/core/internal/db/analysis_drill_integration_test.go`

**Interfaces:**
- `analysis_views (id uuid, hospital_id, viewer uuid, level, period_id, filters jsonb, drill_hash bytea, count int, purpose text NULL, clearance_id uuid NULL, named_until timestamptz NULL, audit_id bigint, created_at)` and `analysis_view_rows (view_id, ord int, subject_id, PRIMARY KEY (view_id, ord))`. `mhai_gateway` gets `INSERT`/`SELECT` for its hospital, with `WITH CHECK (viewer = mhai_current_user())`. No `UPDATE` or `DELETE`.
- `analysis_patient_budget (hospital_id, user_id, quarter_id, opened int, PRIMARY KEY (…))` and `analysis_patient_opens (hospital_id, user_id, quarter_id, subject_id, PRIMARY KEY (…))`. No grant to any role; reached through the function.
- `mhai_analysis_rows(p_period uuid, p_filters jsonb, p_group jsonb) RETURNS TABLE (count int, subjects uuid[])`: at most 200 subjects in a fixed order (by `subject_id`), and the full count.
- `mhai_analysis_pseudonyms(p_subjects uuid[], p_period uuid) RETURNS TABLE (subject_id uuid, pid text)`: hospital side, one scope. It draws a missing pid as `'P-' || 8 base32 characters from gen_random_bytes(5)`, and draws again on a unique violation (N9).
- `mhai_analysis_reserve_patients(p_quarter uuid, p_subjects uuid[]) RETURNS integer`: as design §5.6 steps 1–3 (`FOR UPDATE` on the counter row; new subjects counted against `analysis_patient_opens`; over 300 raises `P0001` with message `patient_cap`). It returns the new count. User and hospital come from the session settings.
- `mhai_audit_view_pseudonyms(p_audit_id bigint) RETURNS text[]`: owner reader; `EXECUTE` to `mhai_platform`. It reads the view behind the audit id and returns the pids, in order, for that view's period.
- The sweep gains: opens older than their quarter plus one month; subjects kept while a view row cites them.
- `platform_append` is replaced with the previous list plus `audit.patients_viewed`. Down restores it exactly as the previous migration on `main` left it.

- [ ] **Step 1: Write the failing tests:** `TestReservePatientsRace` (**T28**, Review Focus 4: 20 parallel calls of 40 new subjects against 300; at most 300 distinct; the counter equals the rows); `TestSubjectCountsOnceAcrossLevels` (a block's subject reopened in its month costs nothing); `TestCapRefusalRollsBackView` (the refused request leaves no view row); `TestPidShapeAndPerPeriod` (shape `^P-[A-Z2-7]{8}$`; the same subject has the same pid within a period, and different pids in two periods and at another level); `TestPidCollisionRedrawn` (N9: a forced collision through a test hook); `TestViewInsertNeedsSelfAsViewer` (**T19**, SQL half); `TestPlatformCantReadRowsDirectly`; `TestAuditViewPseudonymsOnlyForPlatform`; `TestOpensSwept`; `TestMigration00032DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Migration 00032: drill-down views, pids per period and the distinct-patient cap".

### Task 13: `GET /analysis/patients` (de-identified)

**Files:**
- Create: `services/core/internal/analysis/{drill.go,pids.go,drill_test.go}`, `services/core/internal/analysis/routes/{patients.go,patients_integration_test.go}`, `services/core/internal/analysis/linkage_p8_test.go`
- Modify: `httpx/errors.go` (`too_small`, `level_not_supported`: 422), `audit/{schemas.go,catalogue.go}`, `testdata/web-audit-vocabulary.json`, `openapi.yaml`

**Interfaces:**
- The query is the view's (period, filters) plus `drill` and `level`. `level` other than `deidentified` gives 422 `level_not_supported`.
- In one `WithHospitalUser` transaction:
  1. the gate (404) and two-step;
  2. re-read the caller's role, two-step and active state inside the transaction (§6.4);
  3. the list budget (10 a minute, 50 a day), else 429 and a flagged entry written in a separate transaction;
  4. the drill: find the group whose render-time key equals `drill` among the stored body's drillable cells, and recompute the count with `mhai_analysis_rows`; unless it is shown and at least 5, and equal to the cell, 422 `too_small`;
  5. `mhai_analysis_reserve_patients` (`patient_cap` gives 429 and a flagged `analysis.patient_cap_reached`, separately);
  6. insert the view and its rows, append `analysis.patients_viewed` (`view`, `period`, `filters`, `patient_count`, `rows`), commit;
  7. reply `{level: "deidentified", count, patients}`. Each row is exactly `{pid, age_band, sex, setting, conditions (≤ 10, within the period, fixed order), findings (≤ 10)}`.
- `Cache-Control: no-store`. The reply is never in the idempotency store (a `GET`).

- [ ] **Step 1: Write the failing tests:**
  - `TestT8ListAndCapLimits` (**T8**: the 51st list in a day, and the 301st distinct patient in a quarter (departure 11), each 429 with one flagged entry);
  - `TestT9RowsWithinPeriodAtMostTen` (**T9**);
  - `TestT10ForeignDrillIs422` (**T10**: another hospital's, another period's, or another filter set's key → 422 `too_small`);
  - `TestP1DrillCountEqualsCell` (**P1**, drill half: count equals the cell, with that many rows up to 200, every row matching the group);
  - `TestPatientsRoleRemovedRefusedAtOnce` (**T13** for this route: role removed, refused before the 30-second cache expires);
  - `TestPatientsMFAResetRefused` (**T14** for this route);
  - `TestRowsCarryExactKeys` (no name, MRN, birth-date or address key);
  - `TestAuditFailRollsBack` (**T18**, de-identified: the audit append fails, no rows returned);
  - **P7** `TestP7Shapes` (no drill key, cursor, view id, export id or error matches the pid shape; no aggregate body contains a pid, name, MRN or birth-date key);
  - **P8** `TestP8LinkageMeasured` (one person's full budgets; report how many patients they can link into a three-attribute profile within a period; assert that one subject's pids in two periods match no more often than chance);
  - `TestDrillKeyRotation` (N6: with a new drill key, old keys give 422).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/... && go test -race -tags integration ./internal/analysis/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and `oasdiff`.
- [ ] **Step 5:** Commit: "De-identified Analysis drill-down: per-period pids, caps reserved atomically, every list audited".

### Task 14: Pseudonym reveal on the platform audit log

**Files:**
- Create: `services/core/internal/platform/{audit_patients.go,audit_patients_integration_test.go}`
- Modify: `services/core/internal/platform/audit.go` (`patients: {count}` on drill-down entries), `audit/{schemas.go,catalogue.go}` (`audit.patients_viewed`), `testdata/web-audit-vocabulary.json`, `openapi.yaml`, `docs/specs/generated/permissions.md`, `docs/specs/2026-10-17-frontend-requirements.md` §14.9, `CHANGELOG.md`, `services/core/internal/smoke/smoke_test.go`

**Interfaces:**
- `GET /platform/audit/{id}/patients`: `authz.Rule{Platform: true, NeedsMFA: true, AnyRole: [platform_admin, auditor]}` gives `{pseudonyms: string[]}` from `mhai_audit_view_pseudonyms`, and writes `audit.patients_viewed` (`refs.audit_id`). It answers 404 for an entry that isn't a drill-down. It is the only route outside Analysis that sends a pid.

- [ ] **Step 1: Write the failing tests:** `TestAuditPatientsReveal` (order and count; 404 for other entries; a hospital session gets `not_platform`); `TestT23NoPidElsewhere` (**T23**: every other audit and platform route's JSON over a seeded log holds no pid-shaped string); `TestAuditEntryPatientsCount`; `TestRevealAudited`; smoke: a de-identified list, then the auditor's reveal.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/platform/... ./internal/audit/...` and `make smoke` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Reveal a drill-down's pseudonyms to platform admins and auditors, audited".

**PR 5:** run `security-review` and open it. Built as migration 00028 and OpenAPI 1.19.0, with departures 19–24.

---

### PR 6: the analyst grant

### Task 15: Migration D: the `user_roles` trigger and the grant functions

**Files:**
- Create: `services/core/internal/db/migrations/00033_analyst_role.sql`, `services/core/internal/db/analyst_integration_test.go`

**Interfaces:**
- Trigger `user_roles_analyst BEFORE INSERT OR DELETE ON user_roles FOR EACH ROW` (definer, owner `mhai_migrate`) enforces §6.1 on every writer. Inserting `analyst` needs:
  - an existing `clinician` row;
  - the grantor (`mhai_current_user()`, or `mhai.operator_id` for the CLI) to be an active `hospital_admin` of the same hospital, and not the person;
  - the person active, with `user_mfa.enabled`;
  - `mhai_analysis_position(hospital) = 'on'`;
  - fewer than 10 analysts.

  Refusals raise `P0001` with the messages `not_eligible`, `self_change`, `analysis_off` and `analyst_cap`, which the route maps. Deleting `clinician` deletes `analyst` and writes `analyst.removed` (reason `clinician_removed`), in the same statement's transaction.
- `mhai_grant_analyst(p_user uuid)` and `mhai_remove_analyst(p_user uuid)`: definer; `EXECUTE` to `mhai_gateway` and `mhai_provision`. Each writes the audit row (`analyst.granted` or `analyst.removed`, the person by `display_name` via the catalogue, `via`).

- [ ] **Step 1: Write the failing tests:** `TestT26TriggerRefusals` (**T26**, SQL: self, another hospital's admin, no `clinician`, no two-step, an inactive person, the switch off, an 11th analyst: each refused, even by a direct `INSERT` as `mhai_gateway`); `TestRemovingClinicianRemovesAnalyst` (N11: also through a direct `DELETE`); `TestGrantAuditedInSameTransaction`; `TestPlatformRolesUntouched` (the trigger doesn't block 00003's other rules); `TestMigration00033DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests and the auth integration suite (role changes elsewhere still work).
- [ ] **Step 5:** Commit: "Enforce the analyst rules in a trigger on user_roles".

### Task 16: The CLI and the audit actions

**Files:**
- Create: `services/core/cmd/core/{roles.go,roles_test.go,roles_integration_test.go}`
- Modify: `cmd/core/main.go`, `audit/{schemas.go,catalogue.go}`, `testdata/web-audit-vocabulary.json` (the `analyst` actor role, pending in web)

**Interfaces:**
- `core users roles <email> --add analyst|--remove analyst --by <admin email>`, as `mhai_provision`, with `mhai.operator_id` set to the `--by` admin. It refuses a `--by` who isn't a hospital admin of the person's hospital. It prints `roles <user id> analyst added|removed`.
- The notice to every hospital admin: `analysis.NotifyAnalystGranted(ctx, hospital, person)`, a no-op that logs ids until the notifications service exists. A test pins that it is called once per grant.

- [ ] **Step 1: Write the failing tests:** `TestCLIRolesUsesTheFunction` (**T26**, CLI: the same refusals); `TestCLIRefusesForeignGrantor`; `TestNoticeCalledOncePerGrant` (**T26**, puppet account); `TestAnalystActionsSchemas`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./cmd/core/... ./internal/audit/... && go test -race -tags integration ./cmd/core/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Grant and remove Analyst from the CLI through the same database rules".

### Task 17: Step-up on `PATCH /hospital/staff/{id}` (when the staff endpoints exist)

**Built** with the hospital admin routes (`internal/hospital`, migration 00035, OpenAPI 1.24.0): the route maps the six refusals, step-up covers every staff write, and both named tests are in `internal/hospital/hospital_integration_test.go`.

**Files:**
- Modify: the staff handler (wherever the staff-endpoints plan puts it), `httpx/errors.go` (`self_change` 403, `analysis_off` 409, `analyst_cap` 409, `not_eligible` 422), `openapi.yaml` (optional `mfa_code` and `backup_code`)

**Interfaces:**
- When `roles` adds `analyst`: a missing code gives 403 `mfa_required`; then `d.Auth.StepUpAudited(r, body.MFACode, body.BackupCode)`; then `mhai_grant_analyst` in the request transaction, mapping the trigger's messages to the codes above. Removing `analyst` needs no step-up.

- [ ] **Step 1: Write the failing tests:** `TestT26RouteRefusals` (**T26**, route: each refusal code; without a code, 403 `mfa_required`; a wrong code, refused and audited); `TestRemoveAnalystEndsPatientAccessAtOnce` (**T13**).
- [ ] **Step 2–4:** Run, implement, run (`go test -race -tags integration` on the staff package).
- [ ] **Step 5:** Commit: "Require step-up to grant Analyst".

**PR 6:** run `security-review` and open it. If the staff endpoints aren't on main, open PR 6 with Tasks 15–16 and note that Task 17 moves into the staff plan. Built as migration 00030 with departures 25–28; the staff endpoints weren't on main, so Task 17 moved (departure 28).

---

### PRs 7–10 (task level; detailed when their dependencies exist)

### PR 7: letters, hospital side (design §8.1, §8.10)

Depends on PR 6. The live upload needs the documents service (H5), and the platform-side verify, reject and revoke follow the Plan 11b design. **Built as migration 00038** (after EMR PRs 9 and 13, 00032 and 00033, and the route PRs #163–#165, 00035–00037) and **OpenAPI 1.28.0** (after 1.21.0–1.27.0), with departures 29–44. Off by default: every letter route answers 404 while the hospital's analysis switch is off, and 403 `named_access_off` while its named-access position is `off`, which it is everywhere until the CLI raises the demo hospital to `letters_only`.

### Task 18: Migration 00038: letters, people covered, events and the named-access switch

**Files:**
- Create: `services/core/internal/db/migrations/00038_clearances.sql`, `services/core/internal/db/clearances_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestMigrateUpFromEmpty`, `TestRLSWithoutHospitalSeesNothing`)

**Interfaces:**
- Role `mhai_clearance_rules` (NOLOGIN, NOINHERIT, no members), as 00030's `mhai_analyst_rules`: it owns the triggers and functions. Its reads: users' id, hospital, active flag, email and display name; `user_roles`; `user_mfa.enabled`; hospitals' id, status, time zone and demo flag; the analysis switch; the clearance audit entries. Its writes: the clearance tables only.
- `named_access_switch (hospital_id NULL, position off|letters_only|on, the three sign-off references, since, changed_by, ever_signed_off)`, one row per scope, as `analysis_switch`. A hospital row above the global row is refused (`CHECK (mhai_named_access_within_global(hospital_id, position))`, P0001-free: 23514). `mhai_named_access_position(hospital)` is the lower of the two, `off` when either is missing; a gateway caller learns only its own hospital's. Writers: `mhai_platform` (Plan 11b; no demo references), the CLI's lowering (`mhai_provision`, `position = 'off'` only), and `mhai_named_access_demo_raise(hospital or NULL)` (`letters_only` with `DEMO-SYNTHETIC`, only where the analysis switch of that scope carries demo references and the row was never signed off).
- `clearances (id, hospital_id, status, refused_reason, signed_by, signer_title, letter_date, end_date, renews, uploaded_by/at, verified_by/at, rejected_by/at, reject_reason, reject_note, revoked_by/at, revoked_side, revoke_reason, revoke_note, ended_at, pages, sha256, demo, version)`, with a CHECK per status (`refused` ⇔ a reason, `checking`/`refused` ⇔ no pages, and so on). `clearance_covered (hospital_id, clearance_id, user_id, side hospital|platform, removed_at/by, removed_reason, removed_note)`: people by user id, never by email (departure 34). `clearance_events (id, seq, hospital_id, clearance_id, kind, at, by_user, side, reason, note, person, days, xact)`.
- The rules (triggers, for every writer except a superuser acting as itself, as 00030):
  - insert: as `mhai_gateway`, by `mhai_current_user()`, an active hospital admin with two-step, of `mhai_current_hospital()`; analysis `on` (else `not_found`) and named access `letters_only` or `on` (else `named_access_off`); status `checking`, version 1; `end_date` after the hospital's today and no later than 12 months on (`end_date_too_late`); `letter_date` within the 12 months up to today (`invalid`); `demo` set by the trigger (the hospital is the demo, under a demo analysis switch), never by the caller;
  - update: `version` is the old one plus 1, whatever the caller sends; the letter's facts never change; the transitions are exactly `checking → waiting|refused` (the documents outcome: in this PR, the demo function only), `waiting → active|rejected` (`mhai_platform` only, by `mhai_current_user()`, an active platform admin with two-step who is not covered by the letter, removed or not, `same_person`, and not linked to the hospital, `same_side`: **T15**), `waiting|active → revoked` (a hospital admin of the hospital, side `hospital`; or a platform admin under the same two checks, side `platform`), `active → ended` (once `end_date` is before the hospital's today) and a same-status update that changes nothing but the version (narrowing); anything else is `conflict`; no delete;
  - people covered: inserted only with their letter, in its transaction, each eligible (`not_eligible`, `not_eligible_platform`), at most 20; a person is only ever removed (narrowing), by a hospital admin of the hospital, from a letter that is `checking`, `waiting` or `active`, and never the last (`would_empty`);
  - every insert, transition and removal writes an event (`uploaded`, `ready`, `refused`, `verified`, `rejected`, `narrowed` (one per person), `revoked`, `ended`, `reminded`), and a deferred constraint trigger refuses, at commit, an event without its `clearance.<kind>` audit entry (entity `clearance`, the event's id in the payload, by its actor, in its hospital): departure 33.
- Gateway functions (each re-checks the gate and the caller, and takes the hospital from the session): `mhai_clearance_gate()`, `mhai_clearance_today()` (today, `max_end_date`, the zone, whether letters here are demo letters), `mhai_clearance_check(letter_date, end_date, renews, staff ids, platform emails)` (the refusals before anything is stored: the people who aren't eligible, and the platform admins' ids), `mhai_clearance_create(...)`, `mhai_clearance_demo_ready(id, pages, sha256)` (refused for a letter that isn't a demo letter), `mhai_clearance_revoke(id, version, reason, note)`, `mhai_clearance_narrow(id, version, remove, reason, note)`, `mhai_clearance_names(users)` (display names of this hospital's people and of the platform admins its letters name), `mhai_clearance_pending()` (this transaction's events still to audit). The gateway reads the three tables under RLS for its hospital and writes none of them directly.
- Job function: `mhai_clearance_due(now)` (`mhai_analysis_job`: the hospitals with a letter to end or a reminder due); and, as the gateway for that hospital, `mhai_clearance_nightly(now)` (Task 20).

- [ ] **Step 1: Write the failing tests:** `TestT15TriggerRefusesWrongVerifier` (**T15**: a hospital admin, a covered platform admin, a platform admin with an account domain of the hospital, and an auditor, each refused even by a direct `UPDATE`); `TestClearanceTransitionsTable` (every pair of statuses: only the listed ones pass, each only for its actor); `TestClearanceInsertRules` (12-month cap, an end date not after today, a stale or future letter date, `demo` set by the trigger, another hospital's letter as `renews`); `TestNarrowOnlyRemoves` (`would_empty`, no re-adding, no delete, version up by one); `TestClearanceEventsAudited` (the commit refused without the entry, or with another event's); `TestNamedAccessSwitch` (writers, `above_global`, the demo raise only for the demo); `TestClearanceGrants` (the gateway's reads under RLS, no direct write; the helpers nobody's); `TestMigration00038DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Migration 00038: clearance letters, their people and events, and the named-access switch".

### Task 19: The hospital routes

**Files:**
- Create: `services/core/internal/analysis/letters/{letters.go,routes.go,upload.go,pages.go,*_test.go,routes_integration_test.go}`
- Modify: `internal/server/server.go`, `internal/httpx/errors.go` (`named_access_off` 403, `file_too_large` 413, `file_type` 415, `end_date_too_late`, `not_eligible`, `not_eligible_platform`, `would_empty` 422), `internal/audit/{schemas.go,catalogue.go}`, `openapi.yaml` (1.28.0), `docs/specs/generated/permissions.md`

**Interfaces:**
- `GET /hospital/clearances` → `{clearances, today, max_end_date}` (newest first, at most 200); `GET /hospital/clearances/{id}` → one `ClearanceOut` as contract §10 has it (`revocation.day` the hospital's day; an `active` letter past its end date is sent as `ended`). Hospital admins with two-step; the gate first.
- `POST /hospital/clearances`: multipart, `Idempotency-Key` required. In order: the gate (404, 403 `named_access_off`, 403 `forbidden` when the role, account or two-step is gone); outside the demo 503 `unavailable` whatever the body holds (departure 43); not multipart, 415 `file_type`; a body past the API's limit, 413 `file_too_large`; an unknown or repeated field (a `hospital_id`, an `uploaded_by`, a `status`), a bad shape, 0 or more than 20 people, 422 `validation_failed` (T19); step-up (403 `mfa_required` without a code; `mfa_invalid`, `mfa_locked`); then `mhai_clearance_check`: 429 `rate_limited` past 5 eligibility refusals a day (departure 44), 422 `end_date_too_late`, 422 `not_eligible` with `ids`, 422 `not_eligible_platform` with no list (departure 34), 422 `validation_failed` for `renews`. In the demo: the letter, its people, `ready` with the fixture's pages, the entries, 201 with the letter (`waiting`). The file is never kept, parsed or logged: only its size bound and SHA-256.
- `GET /hospital/clearances/{id}/pages/{n}`: another hospital's letter, an unknown page, 404; no pages yet, 503 `not_ready` (departure 3); not a demo letter, 503 `unavailable`; otherwise `clearance.viewed {page}` is appended and committed, then the fixture page as `image/png`, `Cache-Control: no-store`, `Content-Disposition: inline`, `X-Content-Type-Options: nosniff`.
- `POST …/{id}/revoke {reason, note?, version?, mfa_code|backup_code}` and `POST …/{id}/narrow {remove, reason, note?, version, mfa_code|backup_code}`: `Idempotency-Key` required, step-up, 409 `conflict` on a stale version or a status that can't change, 422 `would_empty`; `other` needs a note (1–500 characters, no control characters). The reply is the letter.
- Audit: `clearance.uploaded`, `.ready`, `.narrowed` (`person`, `reason`), `.revoked` (`side`, `reason`), `.ended`, `.reminder_sent` (`days`, `delivery: pending`), `.viewed` (`page`), and for Plan 11b `.refused`, `.verified`, `.rejected`; ids and codes only, never a note, a signer, a file name or an email. The ones mhai-web has kinds for are mapped (`named_access`, target `clearance`); `clearance.ready` and `named_access.changed` are sent bare.

- [ ] **Step 1: Write the failing tests:** `TestLetterGate` (analysis off 404; named access off 403; a clinician 403); `TestT10OtherHospitalsLetterIs404` (**T10**: the letter, its page, revoke and narrow); `TestT19BodyFieldsRefused` (**T19**); `TestUploadRefusals` (the table above, nothing stored after any); `TestLiveUploadIs503AndStoresNothing`; `TestNonDemoUploadIs503WhateverTheBody`, `TestNotEligiblePlatformReplyIdentical`, `TestPlatformAdminShownOnlyOnceVerified`, `TestEligibilityRefusalsRateLimited`, `TestEligibilityRefusalsLimitUnderRace` (20 parallel bad checks, at most 5 answered) (departures 34, 43, 44); `TestDemoUploadAndPages` (headers, the entry per page, `not_ready`, 404 past the last page); `TestRevokeAndNarrow` (versions, `would_empty`, every entry, and no note, signer or email in an entry or a log line); unit: `TestParseUpload`, `TestFixturePagesArePNG`, `TestLetterActionsSchemas`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analysis/letters/... ./internal/audit/... && go test -race -tags integration ./internal/analysis/letters/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, `permgen` and `oasdiff`.
- [ ] **Step 5:** Commit: "Clearance letters on the hospital side: upload, list, pages, revoke and narrow, every action audited".

### Task 20: Ending and reminders, and the demo switch

**Files:**
- Create: `services/core/internal/analysis/letters/nightly.go` (+ integration test)
- Modify: `internal/analysis/nightly.go` (an `After` step), `cmd/core/serve.go`, `cmd/core/analysis.go` (+ tests)

**Interfaces:**
- Each nightly tick, after the analysis steps: `mhai_clearance_due(now)` as `mhai_analysis_job`, then per hospital, as the gateway with that hospital and no user, `mhai_clearance_nightly(now)`: `active → ended` once `end_date` is before the hospital's today (access already ended at the hospital's midnight through the cover check, PR 8), and one `reminded` event at the smallest of 30, 7 and 1 days that applies, once per letter and number. The entries (`clearance.ended`, `clearance.reminder_sent` with `delivery: pending`) in the same transaction. Delivery waits for notifications.
- `core analysis named-access demo <hospital>` (`letters_only` for the demo hospital and the global row, `DEMO-SYNTHETIC`) and `core analysis named-access off <hospital>|--global`, each audited `named_access.changed`. `on` waits for PR 8.

- [ ] **Step 1: Write the failing tests:** `TestT12EndsAtHospitalMidnightAndReminders` (**T12**, midnight half: hospitals in `Pacific/Kiritimati` and `Pacific/Pago_Pago`; a letter whose end date is the hospital's today stays active, one whose end date was yesterday there is ended and audited; reminders once per letter and number over two runs); `TestNamedAccessDemoCLI` (refuses a hospital that isn't the demo, audits).
- [ ] **Step 2–4:** Run, implement, run.
- [ ] **Step 5:** Commit: "End letters after the hospital's last day, record their reminders, and raise named access for the demo only".

**PR 7:** run `security-review` and open it.

#### Departures (PR 7)

29. **Numbers.** Migration 00038, OpenAPI 1.28.0 (coordinator: EMR PRs 9 and 13 took 00032 and 00033, OpenAPI 1.21.0 and 1.22.0; the hospital and platform route PRs #163–#165 take 00035–00037 and 1.24.0–1.26.0, and #168 takes 1.27.0; all merge first).
30. **Two gates, re-read every time (the more private reading).** A letter route needs the analysis switch on (404, as every Analysis route) and named access at `letters_only` or `on` (403 `named_access_off`, contract §10). The hospital-admin role, the active account and two-step are re-read inside each transaction (403 `forbidden`), not taken from the 30-second cache.
31. **No direct writes.** The design lets the gateway insert into the clearance tables; here it only reads them (its hospital, RLS) and writes through the functions, as departure 20. `mhai_platform` gets no grant on them, nor any write on `named_access_switch`, in this PR: its verify, reject and revoke functions and its two-person raise, each with an audit check, are Plan 11b's (security review: no write path that nothing audits). A signed-off hospital row is never raised under a demo global row, nor the global row given demo references over a signed-off hospital that is on. The trigger already holds the platform side's rules, and T15 is tested with a test-only grant.
32. **`same_side` without a domain list.** No hospital has a list of allowed domains yet, so a platform admin is on the hospital's side when their email's domain is the domain of any account of the hospital, active or not. A shared public domain therefore refuses (fails closed). Plan 11b may add a domain list and account links under the same function, `mhai_clearance_same_side`.
33. **Every letter action is an event, audited at commit.** As departures 21 and 25: the triggers write `clearance_events` whoever writes, and a deferred check refuses an event without its entry. Narrowing writes one event and one entry per person removed (contract §10). The documents outcome `ready` has its own action, `clearance.ready` (additive; sent bare until mhai-web has a kind).
34. **People by id, never by email.** Platform admins named on upload are resolved to their account ids then; no email is stored, audited or logged. The 422 `not_eligible_platform` carries no email list (the contract asks for one): which addresses are active platform admins with two-step is the platform's to say, not a lookup for any hospital admin (security review). It is the same reply for an unknown address, an inactive or two-step-less platform admin, an auditor and a hospital account. A platform admin a letter names is not shown to the hospital (no id, no name, not in `removed`) until Measured Health has verified a letter of that hospital naming them (`mhai_clearance_names`); until then the uploader knows whom they typed, and can revoke the letter rather than narrow a platform person.
35. **The upload stays within the API's 64 KiB body limit.** Core never streams a 10 MB file: the live path waits for the documents service (H5), which should take the file directly, so a live upload is 503 `unavailable` after the checks, with nothing stored. The demo accepts a small letter, keeps only its SHA-256, and serves fixture pages drawn in code (marked SAMPLE, not stamped with the viewer's name, which is the documents service's job). A body over the limit is 413 `file_too_large` where the handler sees it (422 `validation_failed` from the idempotency layer, 413 `payload_too_large` over NATS).
36. **Letter dates.** `letter_date` must be within the 12 months up to the hospital's today (the design is silent; a future or stale letter is refused).
37. **`Idempotency-Key` is required** on the three letter writes (422 `validation_failed` without): one click, one letter. The generic store may keep the reply: it holds no patient data.
38. **Reminders** are recorded once per letter and number, at the smallest of 30, 7 and 1 days that applies (a letter uploaded with 5 days left gets the 7-day one only), with `delivery: pending` until notifications exist. Ending and reminders run for every hospital with a letter due, whatever its switches.
39. **The named-access switch's only writers here are the CLI's demo raise and its lowering** (`named_access.changed`, sent bare), as departure 5 does for analysis. The two-person raise is Plan 11b's.
40. **Revoking a `checking` letter is 409 `conflict`** (the design's lifecycle); in the demo a letter is `waiting` at once.
41. **A platform revoke needs only an active platform admin with two-step** (security review): `same_side` and `same_person` guard who may verify or reject (§8.10); revoking only takes access away, so a hospital can't block it by holding an account in the platform's email domain. mhai-web's Plan 11b ruling refuses a covered or linked admin's revoke too; the more private reading here is that removing access must always be possible.
42. **`mhai_clearance_nightly` acts only for the last hour** (`p_now` no older than an hour; security review), so no caller records a reminder early.
43. **Outside the demo, an upload is 503 `unavailable` first** (coordinator's security review, overriding the design's "checks before 503" order): right after the gate and before the body is read, so every non-demo upload is the same reply whatever it holds, and `mhai_clearance_check` refuses a non-demo hospital too. No eligibility is looked up where no letter can be stored.
44. **Eligibility refusals are rate-limited, reserve then check** (coordinator's security reviews): before the check, each upload reserves a slot per person and hospital day in its own committed transaction (`clearance_check_refusals`, `mhai_clearance_reserve_check`: one upsert, whose row lock serializes parallel checks), and a check that wasn't a `not_eligible` or `not_eligible_platform` gives it back (`mhai_clearance_release_check`); past 5 the upload is 429 `rate_limited`, and `mhai_clearance_check` itself refuses unless given this request's slot number, within the limit and taken today (the limit is the reservation's, not the day's total at check time: re-reading the total refused in-limit checks under load, which then gave their slots back; CI caught it); a limit refusal keeps its slot; reaching the limit writes one flagged `clearance.check_limit_reached` (sent bare).

### PR 8: named access (design §8.2–§8.7)

Depends on PR 7. Live identity needs the EMR service's identity operation (H7, and the conflict above) and NATS TLS. **Built as migration 00040** (the clinician patient lookup, #172, takes 00039) and **OpenAPI 1.30.0** (#172 takes 1.29.0), with departures 45–57. Off by default: a named view needs the hospital's analysis switch on (else 404), its effective named-access position `on` (else 403 `named_access_off`), and an active, verified letter of the hospital that covers the person today. No hospital's position is `on`; the CLI raises only the demo scope, and no letter becomes active without the platform's verify (Plan 11b).

### Task 21: Migration 00040: cover, named views, keys, notes and the named budget

**Files:**
- Create: `services/core/internal/db/migrations/00040_named_access.sql`, `services/core/internal/db/named_access_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestMigrateUpFromEmpty`, `TestRLSWithoutHospitalSeesNothing`), `services/core/internal/db/clearances_integration_test.go` (`TestClearanceGrants`: the new switch functions)

**Interfaces:**
- `mhai_named_cover(p_user uuid) RETURNS TABLE (named, until, clearance_id, version, end_date, demo, reason)`: owner `mhai_clearance_rules`, VOLATILE (it takes `FOR SHARE` on the covering letter and the person's row in it, so a revocation or narrowing that commits first is seen, and one that comes later waits for the view's commit), nothing cached. The hospital is `mhai_current_hospital()`; `p_user` must be `mhai_current_user()` (else `forbidden`: no one asks about another person's cover). Analysis not `on`: `not_found`. Then, in order: the effective named-access position below `on` → `switched_off`; the hospital not active, or the person not active at it with `hospital_admin` or `analyst` and two-step → `role_or_account`; an `active` letter of the hospital with `end_date` on or after the hospital's today naming the person (side `hospital`, not removed), the latest end date first → `named`, with `until` the earlier of the end of that letter's last day in the hospital's zone and now plus 30 minutes. Otherwise `reason` comes from the verified letter whose cover of the person stopped last: `removed_from_letter`, `revoked` or `ended`; none → `none`. `EXECUTE` to `mhai_gateway` and `mhai_analysis_reader`.
- `mhai_named_access_state() RETURNS TABLE (position, since)`: the effective position for the caller's hospital and when it last changed (the later of the two rows; null when there is no row). Analysis not `on`: `not_found`. `EXECUTE` to `mhai_gateway`. `mhai_named_access_position` gains `EXECUTE` for `mhai_auth` (`features.named_access`).
- `analysis_views`: `level` `deidentified` or `named`; a named list has `purpose` (`clinical_follow_up`, `quality_improvement`, `audit`, `other`), its `clearance_id` (a letter of the same hospital, foreign key) and `named_until`; a de-identified one has none of the three. The commit check (`mhai_analysis_view_audited`) asks for `analysis.named_viewed` on a named list.
- `named_view_keys (hospital_id, key_hash, user_id, body_mac, view_id, named_until, replays, created_at)`, primary key `(hospital_id, key_hash)`; `named_view_notes (hospital_id, view_id, note_enc)`; `named_view_budget (hospital_id, user_id, day, used)`. No grant to any app role: reached through the functions. A deferred constraint trigger refuses, at commit, a replay count that rose without its `analysis.named_viewed` entry (`replays` equal to the new count, by the key's user, naming the list).
- `mhai_analysis_named_admit(p_limit, p_now) RETURNS boolean`: one upsert on the person's hospital-day row (the row lock queues parallel requests: reserve, then check, in the request's transaction); false past `p_limit` (at most 5). A refusal or a later failure rolls the slot back.
- `mhai_analysis_named_key(p_key, p_body) RETURNS TABLE (state, view_id)`: takes a transaction advisory lock on the hospital and key first; `new`, `replay`, `mismatch` (another person's key, or another body) or `expired` (after `named_until`).
- `mhai_analysis_open_named(view, period, filters, group, source, drill hash, expected, cap, purpose, note ciphertext, key hash, body mac, now)`: `mhai_analysis_open`'s steps for a named list, after the cover (refusals `named_access_off`, `not_cleared`, `clearance_ended`, `forbidden`), recording the cover's letter and `named_until`, the note and the key; the rows with each subject's `ref_enc`, and the letter's id, end date, demo flag and `named_until`.
- `mhai_analysis_named_replay(key, body mac, period, filters, group, drill hash, expected, cap, now)`: the key (`idempotency_mismatch`, `expired`), the cover, the group recounted against the stored list, the cap re-reserved (nothing new), the replay count raised; the stored rows in order, with `named_until` the earlier of the original and the cover's.
- `mhai_analysis_drill_flag` takes `named_day` too; `mhai_analysis_sweep` also drops keys 24 hours after `named_until` and budget rows after two hospital days; notes stay with their list.
- `mhai_named_access_demo_on(hospital or NULL)`: as `mhai_named_access_demo_raise`, to `on` (provisioning only).

- [ ] **Step 1: Write the failing tests:** `TestNamedCoverRules` (**T11**: letters_only → switched_off; **T12**: revoked, removed and ended letters give their reasons at once, the latest end date wins, `until` is the hospital's midnight or 30 minutes; **T13**: role removed → role_or_account; **T14**: two-step off or the account inactive → role_or_account; another person's id refused; another hospital's letter never covers); `TestNamedCoverRevokeWaits` (a revoke during a view waits for its commit; the next cover sees it); `TestNamedBudgetReserveRace` (20 parallel admits, exactly 5 true); `TestOpenNamedRecordsAndAudits` (the list, rows, note and key; refused at commit without its entry; `named_until`, clearance and purpose set; ref_enc returned); `TestNamedReplayAuditedAndBounded` (a replay without its entry refused at commit; `expired`; `idempotency_mismatch` for another person or body); `TestNamedTablesGrants` (no app role reads or writes them; the functions' grants); `TestMigration00040DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Migration 00040: who is covered, named lists, their keys, notes and budget".

### Task 22: The routes: `clearance/me`, `named-access` and the named list

**Files:**
- Create: `services/core/internal/analysis/named.go`, `services/core/internal/analysis/routes/{named.go,named_test.go,named_integration_test.go}`
- Modify: `routes/{register.go,limits.go}`, `internal/auth/user.go` (+ test), `internal/httpx/errors.go` (`not_cleared`, `clearance_ended` 403; `idempotency_mismatch` 422), `internal/audit/{schemas.go,catalogue.go}`, `openapi.yaml` (1.30.0), `docs/specs/generated/permissions.md`

**Interfaces:**
- `GET /analysis/named-access` → `{position, since}`; `GET /analysis/clearance/me` → `{named, until, clearance_id, version, reason}` (6 a minute per session, not audited). Both: analysis off 404; position `off` 403 `named_access_off`.
- `POST /analysis/patients/named` (`httpx.NoStore`): `Idempotency-Key` required; body exactly `{drill, purpose, note?, filters, period: {level, start}}` (unknown fields, a hospital, viewer or clearance id: 422 `validation_failed`, T19; `other` needs a note of 1–200 characters, no control characters). In one transaction as the person: the key (422 `idempotency_mismatch`, 409 `expired`); the role, two-step and account re-read (403); the named budget (5 a hospital day, replays count; 429 and one flagged `analysis.named_limit_reached` a day); the cover (403 `named_access_off`, `not_cleared`, `clearance_ended`); the drill (422 `too_small`); the list opened (the cap: 429 and `analysis.patient_cap_reached`), the note sealed, the key stored; `analysis.named_viewed` (flagged: view, period, filters, patient_count, rows, purpose, replays, clearance_id) appended last; commit. Then identity (Task 23); on failure 503 `unavailable` and a flagged `analysis.named_view_failed {clearance_id}`. Reply `{level: "named", count, patients: [row + name + mrn], named_until, clearance: {id, end_date}}`, `Cache-Control: no-store`.
- `features.named_access`: `features.analysis` and the effective named-access position not `off`.

- [ ] **Step 1: Write the failing tests:** `TestT11LettersOnlyRefused`; `TestT12RevokedAndEndedAtOnce` (revoke elsewhere → `not_cleared` and `clearance/me` reason `revoked`; an ended letter → `clearance_ended`); `TestT13RoleRemovedRefusedAtOnce`; `TestT14MFAResetRefusedAtOnce`; `TestNamedViewAndReplay` (rows with names, `named_until`, the replay returns the original `named_until` and is audited; another person's key or another body 422); `TestT16NoNamesStored` (after a view and its replay no stored row in any table holds the synthetic names or MRNs); `TestT17NoNamesLogged`; `TestT18AuditFailNoIdentityCall` (the append fails: no rows, the resolver never called); `TestT19BodyFieldsRefused`; `TestNamedBudgetWithReplays` (M7: the sixth view or replay of the day is 429 with one flagged entry); `TestNamedCapSharedWithLists`; `TestClearanceMeRateLimited`; `TestFeaturesNamedAccess`; unit: `TestParseNamedBody`, `TestNamedActionsSchemas`.
- [ ] **Step 2: Run (Go, remote)** → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, `permgen` and `oasdiff`.
- [ ] **Step 5:** Commit: "Named access: who is covered, the named list under a letter, every view and replay audited before names are sent".

### Task 23: The identity seam, the reply check and the demo fixture

**Files:**
- Create: `services/core/internal/analysis/identity.go` (+ `identity_test.go`)
- Modify: `internal/config/config.go` (+ test: `ANALYSIS_NAMED_IDENTITY`), `cmd/core/serve.go`, `internal/server/server.go`, `cmd/core/analysis.go` (+ tests: `named-access demo-on`)

**Interfaces:**
- `analysis.Resolver` (`Resolve(ctx, IdentityRequest) (map[string]Identity, error)`). `Identity` never prints (String, GoString, LogValue and Format redacted). `CheckIdentities(asked, got)`: exactly the asked references, at most 200 (T20).
- The live resolver answers `ErrIdentityUnavailable` until the EMR service has `emr.v1.ctl.identity_resolve` (H7; departure 46). `ANALYSIS_NAMED_IDENTITY=demo-fixture` selects the demo fixture: `NewDemoIdentity` refuses with the EMR integration configured (`CORE_EMR_ENABLED`, T27), and it resolves only a demo letter's list, with synthetic names and MRNs drawn from the reference.

- [ ] **Step 1: Write the failing tests:** `TestT20ReplyMustMatch` (an extra reference, a missing one, over 200); `TestT27DemoFixtureRefusesLive` (constructor and config); `TestDemoIdentityOnlyForDemoLetters`; `TestIdentityNeverPrints`; `TestNamedAccessDemoOnCLI`.
- [ ] **Step 2–4:** Run, implement, run.
- [ ] **Step 5:** Commit: "Resolve names only after the audited commit, check the reply exactly, and keep the fixture to the demo".

**PR 8:** run `security-review` and open it.

#### Departures (PR 8)

45. **Numbers.** Migration 00040 and OpenAPI 1.30.0 (coordinator: the clinician patient lookup, #172, takes 00039 and 1.29.0).
46. **The live identity call waits for H7.** The EMR service defines no identity operation yet, so the live resolver answers 503 `unavailable` and appends `analysis.named_view_failed` (design §8.6, "before the EMR service exists"). This PR ships the seam, the exact-reply check (T20) and the demo fixture; the core-signed, single-use, 30-second token is built with the operation.
47. **Cover is the caller's own** (the more private reading): `mhai_named_cover` refuses any user but the session's, so it is never a lookup of another person. A hospital that isn't active gives `role_or_account`. "The most recent letter that covered the person" is the verified letter whose cover of them stopped last (removal, revocation or the end of its last day).
48. **Below `on`, the gate answers first.** While the effective position is `off`, `GET /analysis/named-access`, `GET /analysis/clearance/me` and the named route are 403 `named_access_off` (the contract's gate: `features.named_access` is false). At `letters_only`, `clearance/me` answers `reason: switched_off` and the named route 403 `named_access_off`. Analysis off is 404 before either.
49. **Every replay is audited** (the more private choice; the design wrote nothing new on a replay and reported the count at the sweep). A replay sends names again, so it appends `analysis.named_viewed` for the same list with its replay number, committed before identity is resolved; a deferred check refuses a replay count that rose without its entry. Keys are swept 24 hours after `named_until`, with no entry.
50. **A replay is bounded by today's cover.** Its `named_until` is the earlier of the original and the current cover's, and the letter in its reply and entry is the one covering the person now.
51. **The named budget is reserved in SQL, in the request's transaction** (one upsert on the person's hospital-day row; its lock queues parallel requests). A refusal, or any later refusal in the request, rolls the slot back; a view that commits keeps it even when identity then fails. Named lists also count toward the de-identified list limits (10 a minute, 50 a day), which count every list of the person (stricter).
52. **The named actions are sent bare** (departure 23): `analysis.named_viewed`, `analysis.named_view_failed` and the new `analysis.named_limit_reached` (flagged, once a person and hospital day). The letter is the payload's `clearance_id` (the design's `refs.clearance_id`).
53. **The purpose note** is 1–200 characters with no control characters, sealed with the note key (the hospital as associated data) in `named_view_notes`. No role reads it in this PR; PR 9's export and legal functions will.
54. **A subject with no reference can't be named.** Its list is recorded and audited, then fails 503 `unavailable` with `analysis.named_view_failed`; no names are sent.
55. **The demo fixture** is chosen by `ANALYSIS_NAMED_IDENTITY=demo-fixture`. It refuses to start when the EMR integration is configured (`CORE_EMR_ENABLED`: live identity), and resolves only a list whose letter is a demo letter (`clearances.demo`). Its names and MRNs are synthetic and say so.
56. **`core analysis named-access demo-on <hospital>`** raises the demo scope (global and the demo hospital) to `on` with `DEMO-SYNTHETIC`, audited `named_access.changed`. No letter becomes active without the platform's verify (Plan 11b), so no one is covered in the demo until then; the tests seed active letters as the superuser.
57. **`Idempotency-Key` is required** on the named route (422 `validation_failed` without). Keys are stored as SHA-256; the body is bound by an HMAC under the drill key with its own label, so a stored key holds no filter, note or drill.

### PR 9: audit list, exports and the legal path (design §8.8, §8.9)

Depends on PRs 7 and 8, object storage (H5), and H3 for §8.9. Migration 00036+.

- **Task 24: The named-access list and exports.** `GET /platform/audit/named-access`, `letter` on entries, and `/platform/audit/exports*` per contract §14.9: a `named_access` export is a ZIP holding the CSV with `export_row_ref`, period and pid (never a name) and the cited letters. The export record is written before release; the file is the owner's only, downloadable once within 15 minutes. Tests: T23 (no pid outside `/{id}/patients` and the export file), the once-only download, the closed schemas.
- **Task 25: Legal references and resolution.** `POST /platform/audit/legal-references` (`mhai_legal_reference_create`; one export per reference; at most 200 rows, else 422 `too_many_rows`; 1–3 named admins) and `POST /hospital/legal-resolutions` (`mhai_legal_subjects`: named admin only, else 404; once only through a conditional `UPDATE`, else 409 `already_used`; cover and step-up; the flagged `analysis.legal_resolved` committed before identity; rows in the app only). Tests: T29, and parallel resolutions giving exactly one success.

### PR 10: EMR facts (design §4.1, §4.4)

Depends on EMR plan PRs 5 (patient index), 7 (findings), 8b (alerts), 13 (`pids` on consultations), notifications, and H7. Migration 00037+.

- **Task 26: Consultations with a patient reference.** `mhai_analysis_ingest_source` reads the reference column that the EMR `pids` work adds to consultations, after `emr_patient_in_hospital`. Consultations without one are never counted (§16 Q2). The confidence mapping is H2's. Tests: a consultation without a reference produces no fact; the mapping table, pinned.
- **Task 27: Canonical EMR events, alerts and the patient-index changes.** Coded facts for diagnoses, findings, alerts, settings, locations and sources, from the EMR service's events, in the same nightly run. Exclusions and merges are handled as the conflict above proposes, once H1 and H3 rule. Tests: the exclusion purge leaves frozen bodies unchanged and removes `ref_enc`; a merge never changes a frozen period; no name or MRN in any fact.

---

## Security and threat model

The design's §13 is the threat model. This section makes it concrete for the build and adds what planning found.

### Assets

These are the design's §13.1. The plan adds two: the four core keys (the reference HMAC and encryption keys, the drill key, the note key), and the `mhai_analysis_job` login, which alone can write facts for a named hospital.

### Actors and trust boundaries

| Crossing | What is checked (task) |
|---|---|
| browser → BFF → core (`api.v1.clinical`) | token, clinical audience, two-step, roles (30-second cache), switch on or 404; query strings never logged (2, 3, 10) |
| core route → Postgres (`mhai_gateway`) | RLS by `mhai.hospital_id`; analysis data only through definer functions on frozen periods of the caller's hospital; role and two-step re-read on patient-level routes; `viewer = mhai_current_user()` (9, 12, 13) |
| core nightly → Postgres (`mhai_analysis_job`) | its own login and pool, which no route can reach; functions that refuse a switched-off or demo-ref hospital, unfrozen or foreign periods, and free text (2, 3) |
| CLI → Postgres (`mhai_provision`) | `DEMO-SYNTHETIC` switch and seed only; analyst grants through the trigger (4, 16) |
| stored body → response | strict decode; no shown 1–4; render takes guarded values only; drill keys at render (9, 10) |
| core → EMR service (PR 8) | signed single-use token, exact reference set, nothing kept (23) |
| platform → audit reveal | platform audience, MFA, roles; pids only on `/{id}/patients` (14) |

### Threats (STRIDE) and mitigations

The design's AN1–AN23 carry over unchanged, with the tests mapped below. The plan adds:

| # | STRIDE | Threat | Mitigation | Tests |
|---|---|---|---|---|
| AN24 | Tampering / Information disclosure | A temporary table shadows a table inside a definer function | `search_path = pg_catalog, pg_temp`, schema-qualified names | N1 |
| AN25 | Elevation of privilege | Real consultations ingested under a demo switch, or the CLI raising a real switch | `DEMO-SYNTHETIC` refused by the ingest source; CLI writes demo or off only; `WITH CHECK` per writer | N2 |
| AN26 | Elevation of privilege | A route reaches the job's functions (writes facts for any hospital) | separate login and pool; no gateway `EXECUTE`; AST test | N3 |
| AN27 | Information disclosure | A late fact moves "new" into a frozen block | `first_late` | N4 |
| AN28 | Information disclosure | Filters in logs or spans | nothing logs the query string; a pinning test | N5 |
| AN29 | Spoofing | Weak, placeholder or reused keys | start-up refusal; distinct keys; rotation | N6 |
| AN30 | Tampering | A body with a shown 1–4 stored by a buggy engine | `snapshot_put` refuses it; strict decode on read | N7 |
| AN31 | Information disclosure | The generic idempotency store keeps a named reply | `httpx.NoStore`; own key table | N8 (PR 8) |
| AN32 | Information disclosure | A pid collision links two subjects | unique index; redraw | N9 |
| AN33 | Denial of service | The opens table grows without bound and slows the cap | sweep after quarter + 1 month | N10 |
| AN34 | Elevation of privilege | Analyst kept after `clinician` is removed by a direct `DELETE` | trigger cascade | N11 |
| AN35 | Tampering | Shared 7c code edited for Analysis changes 7c's guarantees | exports are moves; 7c's tests rerun unchanged | N13 |

### Controls (decided)

These are the design's §13.4, plus:

- **Keys:** four 32-byte files, refused at start if missing, placeholder or equal. The note and reference ciphertexts are bound to the hospital (AEAD associated data).
- **Roles:** `mhai_analysis_writer` and `mhai_analysis_reader` own functions. `mhai_analysis_job` executes the job's functions only. `mhai_gateway` executes the request functions only.
- **Concurrency:** the freeze holds an advisory lock per hospital. Bodies are stored under an advisory lock per context, with `ON CONFLICT DO NOTHING`. The cap is reserved with `FOR UPDATE` on one row per person and quarter.

### Abuse-case tests

Listed in the next section, mapped to tasks.

### Residual risk and sign-off

These are the design's R-A1 to R-A5, with P2's and P5's numbers reported in PR 3 for H1. The plan adds:

- **Demo facts sit in the production database** when the demo hospital lives there. They are synthetic and switch-scoped. Accepted.
- **The parents-first order** (departure 9) can hide more than a symmetric family fixpoint would, because a parent stored first fixes what its children may show. This costs utility, not privacy. P2 reports the share of cells hidden for this reason.
- **Key rotation breaks old drill keys,** and the client re-reads the view. Accepted.
- **Sign-off needed:** as design §15. H1 before any live hospital, with the PR 3 numbers.

---

## Abuse cases → tests

| Case | Test (task) |
|---|---|
| T1 | `TestT1FemaleMaleTotal` (6) |
| T2 | `TestT2MonthTotalHidesLoneHiddenBlock` (7) |
| T3 | `TestT3HiddenPairAfterShown` (6) |
| T4 | `TestFromToMustBeOneFrozenPeriod`, `TestHistoryUnderFiltersMatchesStoredBody` (10); `TestFreezeServesByteIdentical` (9); `TestLateFactExcludedEverywhere` (2); `TestQuarterWithOneHiddenMonthIsHidden` (7) |
| T5 | `TestT5ComplementByFilterHiddenWhole` (8) |
| T6 | `TestT6ThirdFilterRefused` (8), `TestThirdFilterIs422` (10) |
| T7 | `TestT7NarrowViewEmptyLists` (6) |
| T8 | `TestT8ListAndCapLimits` (13) |
| T9 | `TestT9RowsWithinPeriodAtMostTen` (13) |
| T10 | `TestCellsRefusesUnfrozenAndForeignPeriod` (9), `TestOtherHospitalGetsNothing` (10), `TestT10ForeignDrillIs422` (13); `TestT10OtherHospitalsLetterIs404` (19) |
| T11–T12 | Task 21; T12 midnight: `TestT12EndsAtHospitalMidnightAndReminders` (20) |
| T13 | `TestPatientsRoleRemovedRefusedAtOnce` (13), `TestRemoveAnalystEndsPatientAccessAtOnce` (17); named (21) |
| T14 | `TestPatientsMFAResetRefused` (13); named (21) |
| T15 | `TestT15TriggerRefusesWrongVerifier` (18) |
| T16–T17 | Task 22, 23 |
| T18 | `TestAuditFailRollsBack` (13); named (22) |
| T19 | `TestViewInsertNeedsSelfAsViewer` (12); `TestT19BodyFieldsRefused` (19); Task 22 |
| T20 | Task 23 |
| T21 | `TestColdBodyOnceUnderRace`, `TestOverCapIs503AndUnstored` (9) |
| T22 | `TestWorkerRolesSeeNoAnalysis` (2) |
| T23 | `TestT23NoPidElsewhere` (14); exports (24) |
| T24 | `TestGatewayCannotWriteAnalysis` (2) |
| T25 | `TestIngestRefusesSwitchOff` (2), `TestNightlyRunsOnlySwitchedOnHospitals` (3) |
| T26 | `TestT26TriggerRefusals` (15), `TestCLIRolesUsesTheFunction`, `TestNoticeCalledOncePerGrant` (16), `TestT26RouteRefusals` (17) |
| T27 | Task 23 |
| T28 | `TestReservePatientsRace` (12) |
| T29 | Task 25 |
| P1 | `TestP1NoHiddenCellSolvable` (6), `TestP1DrillCountEqualsCell` (13) |
| P2 | `TestP2FamilyExhaustive` (8) |
| P3 | `TestFreezeServesByteIdentical` (9) |
| P4 | `TestP4HistoryNoExposure`, `TestP4ISOWeeksWouldFail` (7) |
| P5 | `TestP5BoundsReported` (8) |
| P6 | `TestFilterListPerFrozenPeriod` (9) |
| P7 | `TestP7Shapes` (13) |
| P8 | `TestP8LinkageMeasured` (13) |
| N1 temp-table shadowing | `TestTempTableCannotShadow` (2) |
| N2 demo switch can't ingest real data; CLI can't raise for real | `TestIngestSourceRefusesDemoSwitch`, `TestSwitchWriters`, `TestSeedOnlyForDemoHospital` (2); `TestSeedDemoRefusesRealSwitch` (4) |
| N3 routes can't reach the job's pool or functions; the writer sees no text | `TestRoutesCantReachAnalysisJobPool` (3), `TestWriterSeesNoText`, `TestGatewayCannotWriteAnalysis` (2) |
| N4 late fact and "new" | `TestLateFactNeverChangesFrozenNew` (2) |
| N5 query strings not logged | `TestAnalysisQueryNotLogged` (3, 10) |
| N6 keys | `TestAnalysisKeyFiles` (3), `TestDrillKeyRotation` (13) |
| N7 body store refuses a shown 1–4 or an unfrozen period | `TestSnapshotPutRefusesSmallShownAndUnfrozen` (9) |
| N8 named replies never in the generic store | Task 22 |
| N9 pid collision | `TestPidCollisionRedrawn` (12) |
| N10 opens swept | `TestOpensSwept` (12) |
| N11 analyst cascade on a direct delete | `TestRemovingClinicianRemovesAnalyst` (15) |
| N12 7c and Analysis job roles disjoint | `TestAnalyticsAndAnalysisGrantsDisjoint` (2) |
| N13 shared code is moved, not copied; test-only LP | `TestSharedExportsAreMovesNotCopies` (5), `TestGonumOnlyInTests` (8) |
| N14 nothing served unstored | `TestOverCapIs503AndUnstored` (9) |

## What's needed from people, and when

Nothing below blocks PRs 1–6, except the 7c-1 merge (an engineering dependency) and the staff endpoints for Task 17.

| # | Who | What, exactly | Needed by |
|---|---|---|---|
| H1 | Privacy lead | Design §15 H1. Review the constants in PR 3, and receive the R-A1 (P2), R-A2 (P5), drill-run and P8 numbers that PR 3 and PR 5 report. Accept or change them. Also accept departure 9's extra hiding and the demo facts in production. | PR 3 review; before any live hospital |
| H2 | Clinical lead | Design §15 H2 (views, definitions, confidence mapping). | Before any live hospital; Task 26 |
| H3 | Legal / compliance lead | Design §15 H3; the letter template before PR 7 goes live; §8.9 before PR 9; the exclusion-versus-frozen-period proposal (conflicts) before PR 10. | As listed |
| H4 | Product owner | Given 7 Nov. Also: confirm that the switch stays demo-only until Plan 11b's two-person raise (departure 5), and name the pilot hospital. | PR 2 |
| H5 | Platform and operations | Design §15 H5. Also: provision the `mhai_analysis_job` login secret and the four key files in each environment, and the `analysis_freeze_failed` and body-cap alerts. | PR 2 deploy; PR 7 live; PR 9 |
| H6 | mhai-web team | Design §15 H6(a)–(i), with `not_ready` at **503** (departure 3) and the dotted audit actions (departure 10). | Before PR 4, 6 and 8 go live |
| H7 | Intelligence lead and the EMR design owner | Design §15 H7, with the operation named `emr.v1.ctl.identity_resolve` (conflicts). The exclusion and merge proposal. | PR 8 live; PR 10 |
| H8 | Coordinator (7c owner) | 7c-1 merged before PRs 2 and 3, and agreement to the exports in Task 5. | PR 2 |

## Out of this plan

- The platform side of clearances and the switches' two-person raise (Plan 11b).
- Platform Analysis across hospitals (Plan 12), which must reuse `disclose` (§5.8).
- The documents, notifications and EMR services, and the staff endpoints.
- mhai-web changes (H6).

## Self-review notes

- **Design coverage:**

  | Design | Task |
  |---|---|
  | §4.3–4.4 tables, ingest, late facts | 2, 3; 26–27 |
  | §4.5 pids | 12 |
  | §5.3 Layer A | 6 |
  | §5.4 D1–D3 | 2 (freeze), 7; D4–D5: 8; D6: 8 (P2); D7: 10, 13 |
  | §5.6 drill controls and cap | 12, 13 |
  | §5.7 P1–P8 | 6, 7, 8, 9, 13 |
  | §6 analyst | 15–17; §6.5 `features`: 4 (21 for `named_access`) |
  | §7 switches | 2, 4 (demo); 18 (named switch); raise: Plan 11b |
  | §8 letters, named, identity, audit, legal | 18–25 |
  | §9 contract | 10, 13, 14; 19, 21, 22, 24, 25 |
  | §10 migrations, RLS | 2, 9, 12, 15 |
  | §11 performance | 9, 11 |
  | §12 failure modes | 3 (freeze failed), 9 (cap, timeout, inconsistent), 10 (not ready) |
  | §13 threat model | this plan's section |
  | §14 PRs 1–10 | PRs 1–10 |

- **Names across tasks:** `disclose.{View, Raw, Body, Filters, Forced, GuardResponse, ParentForced, FamilyForced, CheckFilters, Drillable, HistoryPoint, History}`; `analysis.{Keys, Nightly, Engine, Filters}`; `synth.Cohort`; `db.WithAnalysisJob`; SQL `mhai_analysis_{position, ingest_source, ingest, freeze, seed, sweep, cells, filters, snapshot_put, snapshot_get, rows, pseudonyms, reserve_patients}`, `mhai_audit_view_pseudonyms`, `mhai_grant_analyst`, `mhai_remove_analyst`; 7c's `calendar.{Parse, Parent, Children, BlockOf, Of}`, `smallgroup.{AcrossRowsFrozen, AcrossRowsForced, TableForced, ExposedMod}`, `solve.Exposed`.
- **Proportion:** the design is about 1,100 lines and this plan a little shorter. Code blocks are commands only.
