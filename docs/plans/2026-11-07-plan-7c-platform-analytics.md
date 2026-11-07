# Plan 7c: platform analytics across hospitals (backend): implementation plan

**Status:** Not started. The design is a draft for the product owner's review (7 Nov 2026). Rulings R41a and R41a-2 (nested periods) are decided; Q2–Q4 in its §16 are open. Task 5 depends on Q2, and Task 8 on Q3 (it ships the recommended values, which are one table to change).

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Serve the platform admin app's Analytics page (What changed, Use, Answer quality, Hospitals, the series and follow-ups) from figures that cover only finished, nested calendar periods (the latest month-aligned week, month and quarter, UTC). Each period's counts are frozen at close and its figures are one stored snapshot, guarded with its parent as one set and checked against the whole published history, so no hidden count of 1 to 4 can be worked out by subtraction or by chaining from zeros. Move `GET /platform/guardrails` to the same periods and deprecate its rolling windows.

**Architecture:** A core scheduler (advisory-lock leader) computes each finished period's snapshot once, 15 minutes after the period ends. It freezes each period's counts through SECURITY DEFINER functions that refuse any window that isn't a nested reporting period (a week from the clinical tables; a month or quarter as the sum of its frozen children). It guards the counts with `internal/smallgroup` (the existing cross-sectional rules, the parent set with the bridge check, and an exact check with shown zeros and onboarding as known), and stores the guarded output in `analytics_snapshots`. Routes only read stored snapshots and render from guarded values. What changed is evaluated weekly from shown cells only. Follow-ups are platform-admin writes with a version check and an audit row. Nothing goes over NATS to intelligence.

**Tech stack:** as Plan 4. Go 1.24 (`net/http`, `pgx/v5`, `goose/v3`, `testcontainers-go`, `math/big`). No new dependencies.

**Spec:** `docs/specs/2026-11-07-plan-7c-platform-analytics-design.md`. Section numbers below (§n) are the spec's. Abuse cases A1–A25 are its §11.5.

**The contract:**
- `docs/specs/2026-10-17-frontend-requirements.md` §14.6 and §14.7.
- mhai-web's `apps/platform/lib/api/analytics-contract.ts`, with the changes in spec §13 (F1–F14).
- `services/core/openapi.yaml` for `GET /platform/guardrails`.

**Phases:** three PRs, each mergeable on its own, in this order (the migration chain is linear):
- **7c-1, calendar periods and the joint guard:** Tasks 1–5, then Task 10's 7c-1 slice. It ships the guardrails route on calendar periods; the analytics routes don't exist yet.
- **7c-2, the analytics reads:** Tasks 6–7, then Task 10's slice.
- **7c-3, What changed and follow-ups:** Tasks 8–9, then Task 10's slice.

## Global Constraints

Plan 2, 3 and 4's constraints still hold: problem bodies carry a fixed code only; identity only from the verified token; no PHI, counts or free text in logs, spans, audit payloads or NATS messages; synthetic data only; plain commit messages with no AI attribution; remote runs only.

In addition:

- **No rolling windows anywhere new, and no total over several periods.** Every window is a nested period from `internal/analytics/calendar` (week 1–7, 8–14, 15–21, 22–end; month; quarter), and the SQL functions refuse anything else (22023). No route computes a figure: routes read `analytics_snapshots`.
- **Frozen at close.** A period's counts are frozen once (`analytics_period_counts`); a parent's linear counts are the sum of its children's. Nothing recounts a closed period.
- **Raw counts never leave the scheduler's computation or the frozen-counts table.** No platform role can select that table; the counts aren't logged, put in an error or passed to the render code. The render package takes `analytics.Body` (guarded values only) and a test fails if it imports the builder (I2, spec §7).
- **Everything a route sends is a stored guarded cell or computed from shown cells only:** shares, ratios, medians, trends, series, `previous`, changes.
- **The guard never changes a stored snapshot.** Earlier snapshots in the block are frozen; only the new one can hide more. If it can't be made safe, it is stored `unavailable`.
- **Guardrails and Answer quality render from the same snapshot.** No second computation of the G family.
- **Platform roles get no row grant on a clinical table.** `mhai_platform` reads clinical data only through the four new functions and the existing ones.
- **The follow-up note** stays in `analytics_follow_ups`. It is returned only in the changes list on platform routes; never logged, traced, audited or published.
- **Migrations are 00019 and 00020** (main ends at 00018). If another PR merges a migration first, renumber. Each has a full `Down` that revokes every grant it made. Every new table has `ENABLE` and `FORCE` row-level security, owner `mhai_migrate`, and a row in `TestRLSWithoutHospitalSeesNothing`.
- **ADR 0012:** additive only. `openapi.yaml` gets a minor bump per PR and `oasdiff breaking` passes. New problem codes: none.
- **Test-only switches** go behind constructor options or build-tagged files, not package globals (TODO M7 names the existing ones; don't add more).

## Review Focus

1. **The guard is complete over the history** (Task 2): the reference solver is independent of the production one; the history test runs from onboarding with shown zeros as known; the review's chaining case (A26) fails with Monday-to-Sunday weeks and passes with nested ones; the fallback publishes nothing.
2. **Routes can't compute or see raw counts** (Tasks 6, 7): I2, and A23's statement log.
3. **The database refuses rolling windows** (Task 3): A4 for each function.
4. **Guardrails and quality agree cell for cell** (Task 5, A9).
5. **Changes are flagged only on shown values** (Task 8, A12).
6. **Follow-up writes can't be lost or forged** (Task 9, A18–A22).

## Where this plan departs from the spec

None at the start. Record rulings made while building in the spec's new §17, as Plan 4 did.

## Running the tests

As Plan 4: never on this machine. Go tests, testcontainers and compose run on the steven-reid build server through `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh` (mt5-lab is the fallback, capped at 2 CPUs):

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-7c/ DEST=mhai-7c \
  timeout 1800 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/core || exit 1
go vet ./... || exit 1
go test -race ./internal/analytics/... ./internal/smallgroup/... || exit 1
go test -race -tags integration ./internal/db/... ./internal/platform/... || exit 1
CMDS
```

"**Run (Go, remote):** `<cmd>`" below means put `<cmd>` in that heredoc after `cd services/core`. `make smoke` runs remotely and in CI as `boundary-smoke`.

---

## File structure

```
services/core/
  internal/analytics/calendar/calendar.go          month-aligned weeks, months, quarters; Latest, Parent, Children, BlockOf
  internal/analytics/calendar/calendar_test.go     Go and SQL twins agree, 2024–2030
  internal/smallgroup/solve/solve.go               exact rational reference solver (moved out of property_test.go)
  internal/smallgroup/joint.go                     AcrossRowsFrozen (parent set), Block, Box, known zeros, exposure (two primes), GuardSnapshot
  internal/smallgroup/joint_test.go, history_property_test.go
  internal/db/migrations/00019_analytics_snapshots.sql   reader role, mhai_reporting_period_ok, analytics_period_counts, four functions, analytics_snapshots
  internal/db/migrations/00020_analytics_changes.sql     analytics_changes, analytics_follow_ups, audit append policy
  internal/analytics/body.go                       Body: the stored guarded shape (decode strict)
  internal/analytics/builder.go                    reads the functions, families, guards, stores (scheduler side only)
  internal/analytics/scheduler.go                  advisory-lock loop; catch-up; unavailable
  internal/analytics/changes.go                    the weekly evaluation; thresholds table
  internal/platform/analytics.go                   GET /platform/analytics, render from Body
  internal/platform/analytics_series.go            the two series routes
  internal/platform/analytics_followup.go          PUT / DELETE follow-up
  internal/platform/guardrails.go                  + week|month|quarter; deprecated rolling values
  internal/audit/schemas.go, catalogue.go          three analytics actions
  openapi.yaml
docs/notices/guardrails-rolling-windows.md         the deprecation notice the Link names
```

---

## Phase 7c-1: calendar periods and the joint guard

### Task 1: The nested calendar (spec §4, R41a-2)

**Files:**
- Create: `services/core/internal/analytics/calendar/{calendar.go,calendar_test.go}`
- The SQL twin `mhai_reporting_period_ok` is written in Task 3; its agreement test lives here, tagged `integration`.

**Interfaces:**
- `type Kind string` (`Week`, `Month`, `Quarter`); `type Period struct{ Kind Kind; From, To time.Time }` (UTC, `[From, To)`).
- `Latest(k Kind, now time.Time) Period`: the newest period whose `To + Grace ≤ now`; `Grace = platform.GuardrailsGrace` (15 minutes, one constant).
- `Previous(p Period, n int) []Period` (oldest first), `Parent(p) (Period, bool)`, `Children(p) []Period`, `BlockOf(p Period) Period` (its quarter), `Parse(kind, from string) (Period, error)`.
- Weeks: days 1–7, 8–14, 15–21, 22–end of each month (R41a-2). No other week exists anywhere in the code.

- [ ] **Step 1: Write the failing tests:**
  - `TestWeeksTileEveryMonth`: for every month 2024–2030 the weeks cover it with no gap or overlap, 7–10 days each, the last ending at the month's end.
  - `TestEveryPeriodNestsInItsParent`: every week lies in exactly one month and every month in one quarter; `Children(Parent(p))` contains `p`.
  - `TestLatestWaitsForGrace`: at 00:14 UTC on 1 Nov the latest month is September; at 00:15, October.
  - `TestLatestIsStableWithinPeriod`: `Latest(Month, t)` is the same for every minute from 1 Nov 00:15 to 1 Dec 00:14.
  - `TestParseRefusesNonPeriods`: `from` not on a period start, a rolling 30 days, a future period → error.
  - `TestCalendarMatchesSQL` (`integration`): `mhai_reporting_period_ok` agrees with `Parse` on every candidate `(kind, from, to)` over 2024–2030, plus 2,000 random non-periods.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analytics/calendar/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests (the SQL twin test passes after Task 3).
- [ ] **Step 5:** Commit: "Add the nested calendar: month-aligned weeks, months and quarters".

---

### Task 2: The guard over time: parent sets and the history check (spec §6)

**Files:**
- Create: `services/core/internal/smallgroup/solve/solve.go` (+ test), `services/core/internal/smallgroup/{joint.go,joint_test.go,joint_property_test.go}`
- Modify: `services/core/internal/smallgroup/property_test.go` (use `solve`), `rules.go` (`TableForced`: `Table` with cells forced hidden)

**Interfaces:**
- `solve.Exposed(nVars int, eqs [][]int, boxes [][]int) []bool`: exact rational elimination (`math/big`); a box is exposed when its indicator is in the row space of the equations. Tests only, and the A8 cross-check.
- `smallgroup.AcrossRowsFrozen(all, rows []int64s, frozen [][]bool, newCols []int)`: the parent set (rows hospitals and `all`, columns the parent then its children), rule 2 on every row and column and the bridge check, where only the parent column's cells may be newly hidden (spec §6.3 step 2).
- `smallgroup.Box{Weeks, Hospitals, Categories []int}`; `smallgroup.Block` holds the weeks, the frozen snapshots (which cells are shown), the frozen base counts and each hospital's onboarding time. Known zeros: every base count inside a shown 0, and every base count before onboarding or in a week where the hospital isn't listed.
- `smallgroup.GuardSnapshot(b Block, s Snapshot) (Guarded snapshot, ok bool)`: the four steps of spec §6.3. `ok == false` means store `unavailable`.
- The exposure check: elimination modulo two fixed 61-bit primes, exposed if exposed under either; re-runs only on the connected components a newly hidden cell touches.
- `TableForced(width, rows, sub, forced [][]bool) TableOut`: rules 1, 2, 4 with given cells hidden first.

- [ ] **Step 1: Write the failing tests:**
  - `TestSolveMatchesOldPropertySolver`: on the C20 random tables, `solve.Exposed` finds exactly what the old in-test solver found (the move changes nothing).
  - `TestQuarterMinusMonthsHidden` (A5).
  - `TestParentSetRule2AndBridge` (A6): frozen hidden week cells summing to 3 hide the month total; one hidden week cell in a hospital's row with the month shown hides the month cell; children are never changed.
  - `TestParentFullyHiddenHasNoBridge`: with every parent cell hidden and children guarded by their own tables, the parent set has no bridge.
  - `TestShownZeroIsKnown`: a hidden cell solvable only through a shown 0 (non-negativity) is caught by the guard and by `solve`.
  - `TestReviewChainingCase` (A26): the hand-built month-edge chain from onboarding and a shown 0 is exposed under a test-only Monday-to-Sunday calendar, and not under the nested one after the guard.
  - `TestFallbackAllHiddenAddsNothing`: a snapshot forced fully hidden leaves the exposed set of the block unchanged.
  - `TestUnavailableWhenNewAllBoxExposed`: a hospital deactivated mid-block, built so the month's `all` box is exposed even with everything hidden → `ok == false`.
  - `TestHistoryAnchoredAtOnboarding` (A7, property, `history_property_test.go`): 500 random histories of 1–3 years (1–8 hospitals with spread onboarding dates, families Q, G and R, hospitals added and deactivated, counts biased to 0–7 with runs of zeros), every week, month and quarter guarded in calendar order; `solve.Exposed` over **all** published periods at once, with shown zeros and pre-onboarding days as zero equations, exposes no hidden cell and no hidden-member sum of 1–4, and every call returns.
  - `TestModularAgreesWithRational` (A8), including a hand-built matrix whose minor one prime divides.
  - `TestJointBudget` (A24): a full block, 50 hospitals, G family, under 60 s (skipped unless `-run` names it or `ANALYTICS_BUDGET=1`; CI runs it on the build host).
  - `TestUnionsOfHiddenWeeksMeasured`: reports, without failing, how often a combination of hidden cells that isn't a set is exposed with a value 1–4 in the A7 sample (spec §11.6); the number goes in the PR.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/smallgroup/...` → fail.
- [ ] **Step 3:** Move the solver; implement `joint.go`.
- [ ] **Step 4:** Run the tests; then run C19 and C20 unchanged (the cross-sectional rules must not move).
- [ ] **Step 5:** Commit: "Guard each period with its parent set and check the published history".

---

### Task 3: Migration 00019, the reader, frozen counts and `analytics_snapshots` (spec §6.2, §8.1, §8.2)

**Files:**
- Create: `services/core/internal/db/migrations/00019_analytics_snapshots.sql`, `services/core/internal/db/analytics_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestRLSWithoutHospitalSeesNothing` seeds the new table)

Follow 00017's pattern for the owner role (created if missing, no members, refused if it has any, kept on Down with every grant revoked) and the definer pattern (owner, `SET search_path = pg_catalog, pg_temp`, every relation written `public.<name>`, `REVOKE ALL … FROM PUBLIC`, `EXECUTE` to `mhai_platform` only).

- [ ] **Step 1: Write the failing tests:**
  - `TestAnalyticsFunctionsRefuseRollingWindows` (A4): `mhai_analytics_freeze` with a 30-day window, an ISO week, a period ending in the future or inside the grace, and a week 119 days old → 22023.
  - `TestFreezeParentIsSumOfChildren` (A27): a month with a week not frozen → 22023; a frozen month equals the sum of its frozen weeks after rows are committed late; a second freeze changes nothing.
  - `TestFrozenCountsNotSelectable`: `mhai_platform` can't select `analytics_period_counts`; nobody can `UPDATE` or `DELETE` it.
  - `TestAnalyticsFunctionsReturnAggregatesOnly` (A14): the declared columns only; every hospital, zeros included; no user id.
  - `TestPlatformRoleHasNoClinicalRows` (A14, extended from C17): `mhai_platform` can't `SELECT` `clinical_queries`, `clinical_decisions` or `feedback_events`.
  - `TestAnalyticsReaderColumnsOnly`: `mhai_analytics_reader` has exactly the listed column grants and rows of the last 120 days.
  - `TestSnapshotInsertPolicy` (A17): an unfinished period → refused; `UPDATE` and `DELETE` → refused; the CHECK refuses a non-period and a `body` with `state = 'unavailable'`.
  - `TestMigration00019DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fail.
- [ ] **Step 3:** Write the migration.
- [ ] **Step 4:** Run the tests, and Task 1's `TestCalendarMatchesSQL`.
- [ ] **Step 5:** Commit: "Migration 00019: calendar-only analytics counts and stored snapshots".

---

### Task 4: The snapshot builder and the scheduler (spec §5, §8.3)

**Files:**
- Create: `services/core/internal/analytics/{body.go,builder.go,scheduler.go}` with unit and integration tests
- Modify: `services/core/internal/server/server.go` (start the scheduler)

**Interfaces:**
- `analytics.Body`: per hospital (id, name, staff) and `all`: Q, G, R cells, shares, `active_clinicians`, `questions_per_clinician`, `median_review_hours`, all `smallgroup` types. `DecodeBody(b []byte, p calendar.Period) (Body, error)` is strict: unknown fields, a shown 1–4, a wrong kind or period → error.
- `Builder.Build(ctx, p calendar.Period) error`: freezes `p`, reads the block's frozen rows and onboarding times, builds families, `GuardSnapshot`, `Within` for degraded, `Set` for active clinicians, `ShareOf`, inserts `ON CONFLICT DO NOTHING`.
- `Scheduler.Run(ctx)`: every 5 minutes under `pg_try_advisory_lock(<new key>)`; finished periods without a row, oldest first, week, month, quarter at one instant; a week more than 118 days old that was never frozen → it and its parents `unavailable`. Logs the period and a fixed reason only.

- [ ] **Step 1: Write the failing tests:**
  - `TestBodyDecodeStrict` (A16): a stored shown 3, an unknown key, another period → refused.
  - `TestBuildStoresGuardedOnly`: the stored JSON holds no shown 1–4 and no raw-count field.
  - `TestLateCommitDoesNotMoveStoredFigures` (A2).
  - `TestSchedulerOneCopy` (A25): two schedulers, one row per period, both read the same bytes.
  - `TestSchedulerOrderWeekMonthQuarter`: on 1 Jan 00:15 the week, December and Q4 are built in that order, and the quarter sees the month frozen.
  - `TestSchedulerCatchUpAndUnavailable`: a missed week is built on the next wake; a period whose block is out of reach is stored `unavailable`.
  - `TestBuilderLogsNoCounts`: a forced failure logs ids and a fixed reason; no number from the data.
  - `TestHistoryEndToEnd` (A7 against the database): two synthetic quarters of decisions from a hospital's onboarding; the stored snapshots, re-read, pass `solve.Exposed` with shown zeros and onboarding as known.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analytics/... && go test -race -tags integration ./internal/analytics/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Build one guarded snapshot per finished period".

---

### Task 5: Guardrails on calendar periods; the rolling windows deprecated (spec §9)

**Files:**
- Modify: `services/core/internal/platform/{guardrails.go,guardrails_integration_test.go}`, `openapi.yaml`
- Create: `docs/notices/guardrails-rolling-windows.md`

**Interfaces:**
- `period` accepts `week|month|quarter`: render `GuardrailsOut` from the latest stored snapshot's G family (`from`, `to` the period; `updated_at` the stored `computed_at`). Missing or `unavailable` snapshot → 503 `not_ready`.
- `7d|30d|90d`: served as today (Q2 Option A) or with every cell hidden (Option B), wrapped in `httpx.Deprecated(release, release+90 days, "/docs/notices/guardrails-rolling-windows")` for those values only. Release and sunset dates are constants in one place, set when the PR is merged.

- [ ] **Step 1: Write the failing tests:**
  - `TestGuardrailsCalendarPeriods`: `week`, `month`, `quarter` give the stored period's bounds; another value is 422.
  - `TestGuardrailsMatchesQuality` (A9): same period, same cells shown and hidden as the stored body's G family (in 7c-2, against the analytics route too).
  - `TestGuardrailsRollingDeprecated` (A10): `Deprecation`, `Sunset` (≥ 90 days later) and `Link` on `7d`, `30d`, `90d`, errors included; none on the calendar values; after the sunset (a test clock), 422. Under Option B: every cell hidden.
  - `TestGuardrailsCalendarNoComputation`: a calendar read runs no statement on `clinical_decisions`.
  - `TestGuardrailsNoRawSmallCount` (C19) rerun over the calendar values.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/platform/...` → fail.
- [ ] **Step 3:** Implement; `openapi.yaml` (enum values added, the deprecated values described, minor bump).
- [ ] **Step 4:** Run the tests and `oasdiff breaking` against main.
- [ ] **Step 5:** Commit: "Guardrail statistics on calendar periods; deprecate rolling windows".

**Phase 7c-1 done when:** Task 10's 7c-1 slice passes and CI is green.

---

## Phase 7c-2: the analytics reads

### Task 6: `GET /platform/analytics` (spec §7.1)

**Files:**
- Create: `services/core/internal/platform/{analytics.go,analytics_integration_test.go}`; render helpers in `internal/analytics/render.go`
- Modify: `server.go`, the route table, `openapi.yaml`

**Interfaces:**
- `AnalyticsOut{period, from, to, changes, use, alerts, quality, hospitals}` per spec §7.1. `alerts` is JSON `null`. `changes` is `{unavailable: true}` until 7c-3 lands. Medians over shown values; count medians in (0, 5) hidden; `trend` from the stored `all` cells of the latest 12/12/8 periods.
- 30 reads a minute per session (`reserveRate`, scope `platform.analytics.rate`). `Cache-Control: no-store`. Not audited.

- [ ] **Step 1: Write the failing tests:**
  - `TestAnalyticsShape`: every area's keys as the contract, with `alerts: null`.
  - `TestAnalyticsSameAllPeriod` (A1): reads on the 3rd and the 27th of a month are byte-identical.
  - `TestAnalyticsRefusesRollingPeriod` (A3).
  - `TestAnalyticsNoRawSmallCount` (A11): no shown 1–4 anywhere; shares, ratios and medians over hidden inputs hidden.
  - `TestAnalyticsMedianFromShownOnly` (A13).
  - `TestAnalyticsAreaUnavailable` (A16, F13): a missing, `unavailable` or undecodable snapshot makes that area `{unavailable: true}` and leaves the others.
  - `TestAnalyticsReadsNoClinicalTable` (A23): 100 parallel reads, no clinical statement in the log; the 31st in a minute is 429.
  - `TestRenderImportsNoBuilder` (I2): the render package doesn't import `builder.go`'s package or the functions' row types.
  - `TestAnalyticsRoles` (A20, reads): auditors and platform admins 200; hospital audiences `not_platform`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/platform/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Serve platform analytics from stored snapshots".

---

### Task 7: The series routes (spec §7.2)

**Files:**
- Create: `services/core/internal/platform/{analytics_series.go,analytics_series_integration_test.go}`
- Modify: the route table, `openapi.yaml`

- [ ] **Step 1: Write the failing tests:**
  - `TestHospitalSeriesPoints`: 12 weeks, 12 months or 8 quarters, oldest first, each the stored cell; `median` over the other hospitals' shown values or null.
  - `TestHospitalSeriesMeasures`: `questions_per_clinician`, `needs_review_share` → 200; the two Critical measures → 422 (F10).
  - `TestSeriesUnknownIds` (A22): a random uuid as hospital or change → 404.
  - `TestSeriesMedianFromShownOnly` (A13).
  - `TestChangeSeries`: available once Task 8 lands; until then every change id is 404.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/platform/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Add the analytics series routes".

**Phase 7c-2 done when:** Task 10's 7c-2 slice passes and CI is green.

---

## Phase 7c-3: What changed and follow-ups

### Task 8: Migration 00020 and the weekly What changed (spec §7.3, §8.1)

**Files:**
- Create: `services/core/internal/db/migrations/00020_analytics_changes.sql`, `services/core/internal/analytics/{changes.go,changes_test.go}`
- Modify: `scheduler.go` (evaluate after a new week snapshot), `analytics.go` (serve `changes`)

**Interfaces:**
- `Thresholds`: one table of the §7.3 minimums (Q3).
- `Evaluate(ctx, tx, latest calendar.Period) error`: reads the latest week and the latest month finished before it from `analytics_snapshots` (decoded `Body`; one period each, never a total over several), flags, then in one transaction opens, updates (`last_week`, `current`, `previous`, `summary`) or closes rows.

- [ ] **Step 1: Write the failing tests:**
  - `TestChangeNeedsBothSidesShown` (A12): a hidden `current`, fewer than two shown weeks before, or values under the minimum → no change.
  - `TestChangeUsesStoredCellsOnly`: a body whose hidden raw values would flag a change flags none (the evaluator receives no raw value).
  - `TestPreviousIsOnePeriod`: `previous` is the previous month's shown cell per day, scaled to the week's length, one decimal; no code path sums cells of several periods.
  - `TestChangeIdStable`: the same id while flagged each week; closed when not; a new id when it comes back.
  - `TestSummaryWording` (A12): the two shown numbers only, no hospital name, no other digit.
  - `TestOneOpenChangePerKey`: the partial unique index refuses a second open row.
  - `TestMigration00020DownUp`; `TestRLSWithoutHospitalSeesNothing` rows for both tables.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/analytics/... && go test -race -tags integration ./internal/db/... ./internal/analytics/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Evaluate What changed weekly from shown figures".

---

### Task 9: Follow-ups (spec §7.4)

**Files:**
- Create: `services/core/internal/platform/{analytics_followup.go,analytics_followup_integration_test.go}`
- Modify: `internal/audit/{schemas.go,catalogue.go}`, `testdata/web-audit-vocabulary.json` (the three kinds as `pending_in_web`), the route table, `openapi.yaml`

- [ ] **Step 1: Write the failing tests:**
  - `TestFollowUpParallelSameVersion` (A18): one 200, one 409.
  - `TestFollowUpNonAuthorEdit` (A19): 403; the same admin's `DELETE` 200.
  - `TestFollowUpRoles` (A20): auditor 403, hospital audiences `not_platform`.
  - `TestFollowUpAuditedOnce` (A21): one row per write, closed payload, rolled back with the write.
  - `TestFollowUpUnknownOrClosed` (A22): random id 404; a closed change 409.
  - `TestFollowUpNoteRules`: 501 characters, a bidi control, U+2028 → 422 `validation_failed`; a newline accepted.
  - `TestFollowUpNoteNeverLeaves` (A15): a synthetic marker in no log line, span, audit payload or NATS message; present in the changes list.
  - `TestFollowUpIdempotent`: the same `Idempotency-Key` replays the result, one audit row.
  - `TestByYouPerCaller`: `by_you` true for the author only.
  - `TestEveryActionHasSchema`, `TestEveryActionHasCatalogueRow` rerun.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/platform/... ./internal/audit/...` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests.
- [ ] **Step 5:** Commit: "Let platform admins follow up a change, with a version check and audit".

**Phase 7c-3 done when:** Task 10's 7c-3 slice passes and CI is green.

---

### Task 10: Smoke, contract and docs (each phase's slice)

**Files:**
- Modify: `services/core/internal/smoke/smoke_test.go`, `services/core/openapi.yaml`, `README.md`, `ARCHITECTURE.md`, `TODO.md`, `CHANGELOG.md`, `services/core/README.md`, `docs/specs/2026-10-17-frontend-requirements.md`

- [ ] **7c-1 slice:** smoke: with a test clock past a week's end plus the grace, the scheduler stores the week; `GET /platform/guardrails?period=week` serves it; `period=30d` carries the deprecation headers. Contract: the guardrails row gets the new values and the deprecation. CHANGELOG: Added and Deprecated. TODO: R41a decided (done in the design PR); retention names `analytics_snapshots`.
- [ ] **7c-2 slice:** smoke reads `GET /platform/analytics?period=month` and one hospital series. Contract §14.7: the analytics and series rows to **Built (Plan 7c-2)** with F1, F3, F4, F10–F12.
- [ ] **7c-3 slice:** smoke: a synthetic jump flags a change, a platform admin follows it up, an auditor sees `follow_up` and is refused a `PUT`. Contract §14.7: the follow-up rows to Built, with F2, F6, F7, F8. mhai-web's TODO gets F1–F14.
- [ ] **Each slice:** `make smoke` (remote), `oasdiff` additive, docs, TODO entries.
- [ ] **Commit per slice:** "Smoke and docs: Plan 7c-1" (7c-2, 7c-3).

---

## Abuse cases → tests

| Case | Test (task) |
|---|---|
| A1 | `TestAnalyticsSameAllPeriod` (6), `TestLatestIsStableWithinPeriod` (1) |
| A2 | `TestLateCommitDoesNotMoveStoredFigures` (4) |
| A3 | `TestParseRefusesNonPeriods` (1), `TestAnalyticsRefusesRollingPeriod` (6) |
| A4 | `TestAnalyticsFunctionsRefuseRollingWindows` (3) |
| A5 | `TestQuarterMinusMonthsHidden` (2) |
| A6 | `TestTimeSetHidesTotal` (2) |
| A7 | `TestHistoryAnchoredAtOnboarding` (2), `TestHistoryEndToEnd` (4) |
| A8 | `TestModularAgreesWithRational` (2) |
| A9 | `TestGuardrailsMatchesQuality` (5, and 6) |
| A10 | `TestGuardrailsRollingDeprecated` (5) |
| A11 | `TestAnalyticsNoRawSmallCount` (6), `TestGuardrailsNoRawSmallCount` (5) |
| A12 | `TestChangeNeedsBothSidesShown`, `TestSummaryWording` (8) |
| A13 | `TestAnalyticsMedianFromShownOnly` (6), `TestSeriesMedianFromShownOnly` (7) |
| A14 | `TestAnalyticsFunctionsReturnAggregatesOnly`, `TestPlatformRoleHasNoClinicalRows` (3) |
| A15 | `TestFollowUpNoteNeverLeaves` (9) |
| A16 | `TestBodyDecodeStrict` (4), `TestAnalyticsAreaUnavailable` (6) |
| A17 | `TestSnapshotInsertPolicy` (3) |
| A18 | `TestFollowUpParallelSameVersion` (9) |
| A19 | `TestFollowUpNonAuthorEdit` (9) |
| A20 | `TestAnalyticsRoles` (6), `TestFollowUpRoles` (9) |
| A21 | `TestFollowUpAuditedOnce` (9) |
| A22 | `TestSeriesUnknownIds` (7), `TestFollowUpUnknownOrClosed` (9) |
| A23 | `TestAnalyticsReadsNoClinicalTable` (6), `TestGuardrailsCalendarNoComputation` (5) |
| A24 | `TestJointBudget` (2) |
| A25 | `TestSchedulerOneCopy` (4) |
| A26 | `TestReviewChainingCase`, `TestShownZeroIsKnown` (2) |
| A27 | `TestFreezeParentIsSumOfChildren` (3), `TestLateCommitDoesNotMoveStoredFigures` (4) |

## Order

1 → 2 → 3 → 4 → 5 within 7c-1 (1 and 2 can run in parallel; 3 needs 1's calendar rules; 4 needs 2 and 3). 6 → 7 in 7c-2. 8 → 9 in 7c-3. Task 10 last in each part.

## Out of this plan

- **The `alerts` area, serious changes and `critical_overdue`:** when the EMR integration and patient alerts exist.
- **Plan 7d** (the bell, email, the weekly digest): the notifications service.
- **`admins` on `GET /platform/hospitals/{id}`:** with the hospitals read (contract §14.2).
- **Removing the rolling guardrail values and dropping `guardrail_snapshots`:** after the sunset date, in its own PR.
- **The snapshot retention sweep:** the retention plan.
- **Checking bounds (linear programming):** TODO, residual.

## Self-review notes

- **Spec coverage:** §4 is Task 1; §5 is Task 4; §6 is Task 2; §7.1 is Task 6; §7.2 is Task 7; §7.3 is Task 8; §7.4 is Task 9; §8.1 is Tasks 3 and 8; §8.2 is Task 3; §8.3 is Task 4; §9 is Task 5; §10 is Tasks 5–9; §11.5 is the table above; §13 and §14 are Task 10.
- **Real names on main (77739e6):** `smallgroup.Table`, `AcrossRows`, `Within`, `Set`, `ShareOf`, `Guarded`, `GuardedShare`; `platform.GuardrailsGrace`, `guardWindow`, `Deps.reserveRate`, `decodeSnapshot`; `httpx.Deprecated`; `audit.Write`; `mhai_guardrail_counts` and `guardrail_snapshots` (00017); the property test's in-file solver (`recovered`), which Task 2 moves.
- **Twelve weeks per block**, nested (R41a-2); a test-only Monday-to-Sunday calendar exists only to prove A26 catches the chain.
