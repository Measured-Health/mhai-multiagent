# Plan 7c: platform analytics across hospitals (backend): design

- **Status:** Draft for the product owner's review (7 Nov 2026). Ruling R41a and its refinement R41a-2 (nested periods) are decided (§2); the questions in §16 are open.
- **Date:** 2026-11-07
- **Plan:** `docs/plans/2026-11-07-plan-7c-platform-analytics.md`, built from the task breakdown in §15.
- **Builds on (and must stay consistent with):**
  - [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md), [ADR 0011](../adr/0011-frontend-ingress-over-nats.md), [ADR 0012](../adr/0012-api-versioning.md) (additive changes only inside `v1`; retiring a value takes `Deprecation` and `Sunset` with at least 90 days' notice).
  - The Plan 4 design as built, `docs/specs/2026-10-31-plan-4-feedback-audit-documents-admin-design.md` §6.3 and §16 (R39–R43): `GET /platform/guardrails`, `internal/smallgroup`, `mhai_guardrail_counts`, `guardrail_snapshots`, whole UTC days with a 15-minute grace, the bridge check and the property test (C19, C20).
  - mhai-web's analytics design and plans: `docs/specs/2026-10-22-platform-analytics-design.md` (§4, §5, §7, §11), `docs/specs/2026-10-17-usage-alert-analytics-design.md` (the fail-closed rules), `docs/plans/2026-10-22-plan-7c-platform-analytics.md` (the built contract, `apps/platform/lib/api/analytics-contract.ts`) and `docs/plans/2026-10-17-plan-5b-usage-alert-analytics.md`.
  - The frontend contract, `docs/specs/2026-10-17-frontend-requirements.md` §14.6 and §14.7 (Assumed).
  - TODO.md: the R41a entry, the bound-based narrowing residual and snapshot retention (Plan 4c follow-ups, Task 13 review).

## 1. Why

mhai-web built the platform admin app's Analytics page on sample data (Plan 7c, frontend): What changed across hospitals, with Use, Alert safety, Answer quality and Hospitals tabs, a hospital-versus-the-rest comparison, follow-ups and a CSV. It needs a backend. Plan 4c built the one piece of it that exists, guardrail statistics, and left one question open: rolling 7, 30 and 90-day windows can be subtracted from each other, and from the next day's windows, to reveal a hidden count of 1 to 4 (R41). Plan 7c reuses those statistics, so the question had to be answered first.

## 2. Ruling R41a (decided)

**Decided by the product owner on 7 Nov 2026.** Platform statistics show only finished, non-overlapping calendar periods: the last full week, month and quarter, in UTC. They never show rolling windows. A period's figures don't change from day to day while it is the latest, so there is nothing to subtract to reveal a hidden small count. The accepted cost is that the newest figure can be up to a week old. The ruling applies to Plan 7c, and also to Plan 4c's existing `GET /platform/guardrails`, which moves to the new periods as an additive change under ADR 0012 (§9).

**R41a-2, the refinement (decided, 7 Nov 2026, relayed by the coordinator after a review).** Monday-to-Sunday weeks don't nest in calendar months. Publishing both finished weeks and finished months lets someone solve the month-edge days exactly, by chaining the equations from the hospital's first day or from any period shown as 0. So:

- **Nested periods only.** A "week" is a month-aligned block: days 1–7, 8–14, 15–21, and 22 to the month's end. Weeks sit inside months, and months inside quarters.
- **Each period is guarded together with its parent as one set, using the bridge check:** a month with its four weeks; a quarter with its three months.
- **Each finished period is computed once**, after it closes plus the grace period, **and then frozen.** Late data never changes a closed period.
- **No total may span several periods.**
- **A solver test runs over the history of published periods, anchored at onboarding.**

How this design makes the rulings hold in full:

1. **One stored snapshot per finished period** (R43's approach, generalised): every figure is computed once, after the period ends plus the 15-minute grace, and served unchanged by every core copy. No request computes anything.
2. **Frozen counts.** A week's raw counts are frozen when it closes; a month's are the sum of its four frozen weeks, and a quarter's the sum of its three frozen months (§6.2). Parents and children always add up exactly, and a row committed late never moves a closed period.
3. **Parent and children are guarded as one set** with rule 2 and the bridge check (§6.3), and an exact solver then checks the whole history, using everything an attacker knows: a shown 0 makes every part of it 0, and nothing exists before a hospital's onboarding (§6.4).
4. **Nothing sent spans several periods.** Changes compare one period with one period, and medians, trends and series use one period's shown cells each (§7). They add nothing to what can be solved for.

## 3. Scope

| Item | Plan 7c? | Why |
|---|---|---|
| Calendar periods, stored snapshots, the joint guard | **In** | The ruling, and the base for every figure below |
| `GET /platform/guardrails?period=week\|month\|quarter`, rolling windows deprecated | **In** | R41a covers it |
| `GET /platform/analytics?period` with `changes`, `use`, `quality`, `hospitals` | **In** | The page's main read |
| `alerts` area | **Sent as `null`** (not switched on) | There are no patient alerts in the backend: the EMR integration and the notifications service don't exist. Frontend change F4 (§13) |
| The two series routes | **In**, from stored snapshots | The detail panel and the Hospitals tab |
| What changed and follow-ups (`PUT`, `DELETE …/follow-up`) | **In** | Most important area to the product owner |
| `admins` on `GET /platform/hospitals/{id}` | **Out** | Core doesn't serve `GET /platform/hospitals/{id}` (contract §14.2) yet. It belongs with the hospitals read, not here. Frontend change F9 |
| Plan 7d: the bell, email, digest | **Out** | Needs the notifications service |
| Serious (Critical-type) changes, `critical_overdue` | **Out until alerts exist** | Every serious change in the frontend design comes from patient alerts |

## 4. Periods and time

All in UTC. A **period** is `(kind, start)`:

| Kind | Periods | Example |
|---|---|---|
| `week` | days 1–7, 8–14, 15–21, 22–end of each month (7 to 10 days) | 22–31 Oct 2026 |
| `month` | a calendar month | 1–31 Oct 2026 |
| `quarter` | Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec | 1 Jul–30 Sep 2026 |

- A period is **finished** 15 minutes after its end (`GuardrailsGrace`, R43): longer than any write transaction, so a commit dated inside it is counted. A row committed after that is never counted in that period or any parent of it (§6.2).
- The **latest** period of a kind is the newest finished one. It stays the latest until the next one finishes: up to 10 days for a week, a month, a quarter.
- **The parent** of a week is its month; of a month, its quarter. **The block** of a period is its quarter. Every week and month lies inside one quarter, so blocks share no day (§6).
- One Go package, `internal/analytics/calendar`, owns these rules: `Periods(kind)`, `Latest(kind, now)`, `Contains`, `BlockOf`. The same rules are written once more in SQL (`mhai_reporting_period_ok(kind, from, to)`), so the database refuses any window that isn't one of these periods (§8.2). A test runs both over every day from 2024 to 2030.

## 5. Figures and sources

Figures are grouped in **families**. A family's counts add up (across hospitals, within a row, and over time), so each family is one linear system for the guard. Different families share no count.

| Family | Base counts per (week, hospital) | Published cells | Source |
|---|---|---|---|
| **Q** (use) | questions | `questions` | `clinical_queries`, by `created_at` |
| **G** (answers) | pass, regenerate, escalate, each split degraded / not degraded (6) | `answers`, `pass` (high), `regenerate` (medium), `escalate` (low, needs review), `degraded`, and the three shares | `clinical_decisions` (`hga_decision`, `degraded`, `created_at`), as `mhai_guardrail_counts` counts them today |
| **R** (reviews) | agreed (`validate`), corrected (`correct`) | `reviews`, `agreement_rate` | `feedback_events` (`action`, `created_at`) |

Outside the linear families (not additive over time):

- `active_clinicians`: distinct `clinical_queries.created_by` per hospital in the period. Across hospitals it is a set that adds up (every user belongs to one hospital today), so the column is guarded by `smallgroup.Set`; over time it isn't linear (§11.6, residual).
- `median_review_hours`: the median of `decided_at − created_at` over corrections decided in the period, per hospital; hidden when fewer than 5 were decided.
- `staff`: active accounts with a hospital role at compute time. A plain number, as the contract has it: it counts accounts that platform admins provision, not patients or activity.

Derived values, each hidden when an input is hidden (rule 3, `ShareOf`):

- `questions_per_clinician` = questions ÷ active clinicians, two decimals.
- `needs_review_share` = escalate ÷ answers. **"Needs review" is an escalated answer**, and the confidence mix is the HGA decision: high = pass, medium = regenerate, low = escalate. This keeps one partition of answers, so the G family stays a two-way table per period.
- `agreement_rate` = agreed ÷ reviews.

## 6. The joint guard over time

### 6.1 Model

Within a block (a quarter), the **atoms** are its 12 weeks. For each family, the unknowns are the base counts `x[week, hospital, category]`. Every published cell is a **box**: the sum of the base counts over a set of weeks (the period's), hospitals (one, or the snapshot's whole list for `all`) and categories. A shown cell is an equation. A hidden cell is nothing. A box is **exposed** when what is known determines its value exactly: its indicator vector is in the row space of the known equations. This is the property test's criterion (C20), extended from one table to the history.

**What is known** (the attacker's equations, and so the guard's):

1. every shown cell of every stored snapshot;
2. **a shown 0 makes every base count in its box 0** (counts are never negative), one equation per base count;
3. **onboarding:** every base count of a hospital before `hospitals.created_at` is 0, and so is every base count of a hospital in a week where it is not listed.

Items 2 and 3 are the chain the review found: without them a linear check misses month edges solved from a shown 0 or from the first day.

### 6.2 Freezing

When a period closes (its end plus the grace), its raw counts are frozen in `analytics_period_counts` (§8.1), once, and never recounted:

- a **week**'s linear counts are counted from the clinical tables over its days;
- a **month**'s linear counts are **the sum of its four frozen weeks**, and a **quarter**'s **the sum of its three frozen months**, so a parent always equals the sum of its children exactly, and a row committed after a week closed is in no period at all;
- the non-linear figures (active clinicians, review-time medians) are counted over the period's own days at its close and frozen with it.

The raw counts are aggregates per hospital (no row, user or text), readable only by the reader role through SECURITY DEFINER functions; `mhai_platform` can't select them (§8.2).

### 6.3 Guarding a new period with its parent set

A new period `S` is guarded with every earlier snapshot **frozen** (served; nothing may change it). Periods that close at the same instant are guarded in the order week, month, quarter, so a parent is always guarded after its children.

1. **Cross-sectional rules**, as today: `smallgroup.Table` per family on `S`'s frozen counts (rules 1, 2 and 4, the bridge check, `Within` for degraded).
2. **The parent set (R41a-2).** For a month, per published category, the set is a table whose rows are the hospitals and `all`, and whose columns are the month and its four weeks: each row's month cell is the total of its week cells, and each column's `all` cell the total of its hospitals. A quarter is the same with its three months. `smallgroup.AcrossRowsFrozen` runs rule 2 on every row and column and the bridge check on the hidden-cell graph, with the children's cells **frozen** as published: only the parent's cells may be newly hidden. A bridge that only a child cell could close is closed by hiding parent cells instead.
3. **The history check.** Exact elimination over the block's base counts with everything known (§6.1), including `S`'s tentative shown cells. It fails if any hidden cell of the block is exposed, or if the sum of the hidden members of any set (a row, a column, a parent set) is exposed with a value of 1 to 4. On a failure, hide one more `S` cell (the shown `S` cell with the smallest non-zero value whose box overlaps the exposed one; else the smallest shown `S` cell), re-run steps 1 and 2 with that cell forced hidden, and repeat.
4. **It ends.** With every `S` cell hidden (zeros included), `S` adds no equation: what is known is what was known before, which the earlier steps left safe. The children's own guards already keep a lone hidden child out of any column whose `all` is shown, so the parent set with the parent fully hidden has no bridge (a test proves it). If a failure remains anyway (only conceivable when a hospital list changed inside the block), `S` is stored **`unavailable`**: no figures, and the area shows "Couldn't load", never zeros.

The elimination runs modulo two fixed 61-bit primes; a box counts as exposed if it is exposed modulo either. An exposure over the rationals is missed only when both primes divide the same minor of the equation matrix, so the check errs only by hiding more, except with negligible probability (§11.6). A reference solver in exact rationals (`math/big`, moved from today's property test into `smallgroup/solve`) checks it in the tests.

### 6.4 Why the block is enough, and the history test

Weeks nest in months and months in quarters, so no published period crosses a quarter boundary, and no total spans several periods (§7). Two blocks share no unknown: the known zeros of §6.1 are per base count, so they don't link blocks either. The guard therefore checks one quarter at a time.

The history test (A7) doesn't rely on that argument. It builds random multi-year histories from each hospital's onboarding, publishes every week, month and quarter through the guard in calendar order, and runs the exact rational solver over **all** published periods at once, with the shown-zero and onboarding equations. No hidden cell may be exposed, and no hidden-member sum of 1 to 4.

### 6.5 Order and cost

A block has at most 12 + 3 + 1 = 16 snapshots. With 50 hospitals the G family has 12 × 50 × 6 = 3,600 unknowns. That is a background job once a period, not a request. The plan sets a budget (60 s per snapshot on the build host at 50 hospitals) and a test that enforces it. The loop in step 3 re-runs elimination only over the components the newly hidden cell touches.

## 7. What the routes send

**Invariant I2:** a route sends only cells stored in a guarded snapshot, or values computed from cells already shown. The render code takes the stored guarded body as its only input (`analytics.Body`, with `smallgroup.Guarded` values). It has no access to raw counts, and a test fails if the render package imports the snapshot builder.

### 7.1 `GET /platform/analytics?period=week|month|quarter`

`{period, from, to, changes, use, alerts, quality, hospitals}`, the frontend's `AnalyticsOut` with these differences (each in §13):

- `period` is `week`, `month` or `quarter`; `from` and `to` bound the latest finished period of that kind. Any other value is 422 `validation_failed`.
- `use`: `{updated_at, rows, median, trend}`. Each row is `{hospital, questions, active_clinicians, questions_per_clinician}`.
- `quality`: each row is `{hospital, answers, needs_review_share, confidence: {high, medium, low}, reviews, agreement_rate, median_review_hours}`.
- `alerts`: `null` (not switched on), until patient alerts exist (§3).
- `hospitals`: `{updated_at, rows: [{hospital, size: {staff, active_clinicians, questions}}]}`.
- `updated_at` is the snapshot's `computed_at`. An area whose snapshot is missing or `unavailable` is `{unavailable: true}`.
- **`median`** in each area is the median over the hospitals whose value is shown, under the same rules. A count median strictly between 0 and 5 is sent hidden, as the frontend requires. With no shown value, it is `{value: null, suppressed: false}`.
- **`trend`** is `[{start, value}]` over the 12 latest weeks, 12 months or 8 quarters (oldest first), taken from the stored snapshots' `all` cells: `questions` for Use, `needs_review_share` for Quality.
- Rate-limited: 30 reads a minute per session (429 `rate_limited`). A read touches stored rows only. Not audited (R39: aggregates about no person).

### 7.2 The series routes

- `GET /platform/analytics/changes/{id}/series?period` and `GET /platform/analytics/hospitals/{id}/series?measure&period` → `{measure, hospital, points: [{start, value, median}]}`.
- The points are that hospital's stored cell for each of the latest 12 weeks, 12 months or 8 quarters, not days inside one period (frontend change F3). `median` is the median of the other hospitals' shown values for that period, or null.
- `measure` is one of `questions_per_clinician` and `needs_review_share` for now. `critical_median_ack_minutes` and `critical_within_target` are 422 until alerts exist.
- An unknown change or hospital id, or a hospital in no stored snapshot, is 404 `not_found`. 30 reads a minute per session.

### 7.3 What changed

- **Computed once a week** by the scheduler, right after the week's snapshot, **from stored, shown cells only**: the latest week against the latest month that finished before it started. Nothing in it can add an equation.
- **One period against one period (R41a-2: no total spans several periods).** **`current`** is the latest week's shown cell. **`previous`** is that month's shown cell, per day, scaled to the week's length and rounded to one decimal. Weeks run 7 to 10 days and months 28 to 31, so comparisons are per day. The month replaces the frontend's "four weeks before", which would be a total over four periods.
- **A change is flagged only when both sides are shown** and above the minimums below. A change is never flagged on a hidden value, so its existence says nothing about one.

  | Measure | Kind | Flagged when (design call; one Go table, the product owner may change it) |
  |---|---|---|
  | `questions`, `active_clinicians` | rising / falling | both sides ≥ 20 and the change ≥ 50% |
  | `questions_per_clinician` | rising / falling | active clinicians ≥ 5 on both sides and the change ≥ 50% |
  | `needs_review_share`, `agreement_rate` | worse / better | denominators ≥ 20 on both sides and the change ≥ 15 points |
  | `median_review_hours` | worse / better | at least 10 decided on both sides and the ratio ≥ 2 or ≤ 0.5 |

- Changes are evaluated per hospital and across all hospitals (`hospital: null`). `serious` is always `false` in 7c.
- **Ids.** A change is a row in `analytics_changes` keyed while it is open by `(hospital, measure, direction)`, with a random uuid id. It stays open, with the same id, while each weekly evaluation flags it again, and it closes when one doesn't. A closed change leaves the list; its follow-up history stays in the audit log.
- **`summary`** is plain words with the two shown numbers and no hospital name (the frontend's digit check would fail on a name with a digit in it), e.g. "Questions down 41% (52 → 31 a week)".
- **`window`** is `{from, to}` from the start of the month compared to the end of the week. Ranking is by size of change; follow-ups go below.

### 7.4 Follow-ups

| Endpoint | Roles | Behaviour |
|---|---|---|
| `PUT /platform/analytics/changes/{id}/follow-up {note?, version}` | `platform_admin` (`Write`) | Marks the change followed up, or edits the note. `version` must be the change's current version, or 409 `conflict`. If someone else's follow-up is in place: 403 `forbidden` (they undo first). A closed change: 409 `conflict`. Unknown id: 404. → the change |
| `DELETE /platform/analytics/changes/{id}/follow-up {version}` | `platform_admin` (`Write`) | Undoes it. A stale version, or no follow-up to undo: 409 `conflict`. → the change |

- `Idempotency-Key` on both, as every core write. No step-up: a follow-up is a note between staff and changes no figure.
- **The note:** 1 to 500 characters, under the R11 rules (newlines allowed; Unicode Cf, Zl and Zp refused), otherwise 422 `validation_failed`. Stored in `analytics_follow_ups`, a table of its own (as R36 isolated the cancel note). It is returned in the changes list to platform admins and auditors, and nowhere else. It is never logged, traced or written in the audit payload.
- `change.version` moves on with every follow-up write; `follow_up.version` moves on with each note edit. Each write is one conditional `UPDATE … WHERE version = $v`. `by_you` is set per caller.
- **Audit:** `analytics.followed_up {change_id, has_note}`, `analytics.follow_up_edited {change_id}`, `analytics.follow_up_undone {change_id}`, with closed schemas, in the same transaction as the write. `mhai_platform`'s append policy gains the three actions.

## 8. Data

### 8.1 Tables

| Table | Columns | Grants (`mhai_platform`) |
|---|---|---|
| `analytics_snapshots` | `kind` (`week`, `month`, `quarter`), `period_from date`, `period_to date`, `state` (`ok`, `unavailable`), `body jsonb` (guarded output only: every family's cells, the shares, medians, staff, the hospital list with names), `computed_at`; PK `(kind, period_from)`; CHECK `mhai_reporting_period_ok(kind, period_from, period_to)`; `body` null exactly when `unavailable`; at most 4 MiB | `SELECT`, `INSERT` with policy `period_to + 15 min ≤ now()`; no `UPDATE` or `DELETE` |
| `analytics_period_counts` | `kind`, `period_from`, `hospital_id`, the linear base counts (questions; the six G counts; agreed, corrected), `active_clinicians`, `reviews_decided`, `median_review_hours`, `frozen_at`; PK `(kind, period_from, hospital_id)`. Aggregates per hospital only. A month's and a quarter's linear counts are the sums of their children's rows (CHECKed by the freeze function) | **none.** Owned by `mhai_analytics_reader`; reached only through the functions in §8.2. No `UPDATE` or `DELETE` for anyone: frozen |
| `analytics_changes` | `id uuid` (random), `hospital_id uuid null`, `area`, `measure`, `direction` (CHECKed vocabularies), `serious bool`, `current jsonb`, `previous jsonb` (guarded), `summary text` (≤ 200), `opened_week`, `last_week`, `closed_week null`, `version int`; unique open `(coalesce(hospital_id, nil-uuid), measure, direction) WHERE closed_week IS NULL` | `SELECT`, `INSERT`, `UPDATE (current, previous, summary, last_week, closed_week, version)` |
| `analytics_follow_ups` | `change_id` PK FK, `by_user uuid`, `at`, `note text null` (≤ 500, CHECK on Cf, Zl, Zp), `version int` | `SELECT`, `INSERT`, `UPDATE (note, version)`, `DELETE` |

Every table: owner `mhai_migrate`, `ENABLE` and `FORCE` row-level security, `REVOKE ALL FROM PUBLIC`, and a row in `TestRLSWithoutHospitalSeesNothing`. These are platform tables, with no hospital scope, as `guardrail_snapshots` is.

### 8.2 Reading the clinical tables

A new owner role, `mhai_analytics_reader` (NOLOGIN, NOINHERIT, member of nothing, as 00017's `mhai_guardrail_reader`), holds column `SELECT` on exactly:

- `clinical_queries (hospital_id, created_by, created_at)`;
- `clinical_decisions (hospital_id, hga_decision, degraded, created_at)`;
- `feedback_events (hospital_id, action, status, created_at, decided_at)`;
- `hospitals (id, status)`, and the user and role columns `staff` needs.

Policies limit each to rows of the last 120 days. Four SECURITY DEFINER functions, owned by it, `SET search_path = pg_catalog, pg_temp`, `EXECUTE` for `mhai_platform` only, return aggregates only:

| Function | Returns |
|---|---|
| `mhai_analytics_freeze(p_kind, p_from)` | freezes one finished period (§6.2), once (`ON CONFLICT DO NOTHING`): a week from the clinical tables; a month or quarter as the sum of its children's frozen rows, refused (22023) if any child isn't frozen. Distinct clinicians and review times over the period's own days. Every hospital listed or active at the time. Returns nothing |
| `mhai_analytics_frozen(p_quarter_from)` | the frozen rows of one block, per (period, hospital), for the guard. Every hospital (any status), because a stored snapshot may list one deactivated since |
| `mhai_analytics_onboarding()` | per hospital: `created_at` (the onboarding anchor of §6.1) |
| `mhai_analytics_staff()` | per active hospital: active accounts with a hospital role |

`mhai_analytics_freeze` refuses (22023) a window that `mhai_reporting_period_ok` rejects, one that hasn't finished plus the grace, or a week older than 118 days. **A rolling window can't be computed even by SQL run as `mhai_platform`.** No row, user id, question or decision text ever leaves them.

### 8.3 The scheduler

A core loop under an advisory lock (one copy runs it, as the audit verify schedule does) wakes every 5 minutes:

1. For each finished period without a stored snapshot, oldest first and week, month, quarter at the same instant: freeze it (`mhai_analytics_freeze`), guard it (§6) from the block's frozen rows, insert the snapshot with `ON CONFLICT DO NOTHING`.
2. After a new week snapshot: evaluate What changed (§7.3) in one transaction.
3. A week more than 118 days old that was never frozen can't be counted. It, and its month and quarter, are stored `unavailable`, so the gap is visible rather than silent.

Outside `analytics_period_counts`, raw counts exist only in the loop's memory for one computation. They are never logged or put in an error. A failure logs the period and a fixed reason.

## 9. `GET /platform/guardrails` moves to calendar periods (ADR 0012)

- **Added:** `period` accepts `week`, `month` and `quarter` (new enum values on an existing query parameter: additive). The response shape is unchanged: `GuardrailsOut`, rendered from the G family of the same stored snapshot that Answer quality reads. **The two routes show the same cells, with the same hidden pattern, for a period.** Otherwise two different hiding patterns of one box could be combined.
- **Deprecated:** `7d`, `30d` and `90d`. From the release, a response for them carries `Deprecation: @<release time>`, `Sunset: <release + 90 days>` and `Link: </docs/notices/guardrails-rolling-windows>; rel="deprecation"`, set by `httpx.Deprecated` for those values only. The notice says to move to `week`, `month` or `quarter`.
- **After the sunset:** the three values are removed. They become 422 `validation_failed`, `guardrail_snapshots` stops being written, and a later migration drops it once its rows are past retention.
- **During the notice** the rolling figures overlap the calendar ones and are outside the joint guard. Q2 (§16) asks the product owner whether to serve them unchanged (ADR 0012's default) or fully hidden (recommended).
- **The calendar values follow R41a-2:** a `week` is a month-aligned week, each period is frozen at close, and migration 00019's `mhai_reporting_period_ok` and `analytics_snapshots` CHECK hold the guardrail figures to nested periods. 00017's `mhai_guardrail_counts` and `guardrail_snapshots` are used only by the deprecated values.
- `openapi.yaml`: minor version bump; `oasdiff breaking` passes.

## 10. Routes and permissions

| Method and path | Audience | Roles | Rule flags |
|---|---|---|---|
| `GET /platform/analytics` | platform | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` |
| `GET /platform/analytics/changes/{id}/series`, `/hospitals/{id}/series` | platform | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` |
| `PUT`, `DELETE /platform/analytics/changes/{id}/follow-up` | platform | `platform_admin` | `Platform`, `NeedsMFA`, `Write` |
| `GET /platform/guardrails` (widened) | platform | `platform_admin`, `auditor` | unchanged |

None is `bff_direct`. Nothing new crosses NATS: the routes come over `api.v1.platform` like every platform route, and no job or operation goes to intelligence. The generated permissions file changes with the routes; `cmd/permgen`'s drift test catches a hand edit. Auditors are refused the writes (no `AuditorWrite`: that stays under `/platform/audit/`).

## 11. Security and threat model

### 11.1 Assets

- **Small counts about hospitals:** a count of 1 to 4 (questions, escalations, corrections, reviews, clinicians), whether published, hidden, or over a window that was never published. Small counts can point to a clinician or a patient.
- **Hospital separation:** platform roles see aggregates only. No platform route reads a clinical row.
- **Follow-up notes:** staff free text that may name people.
- **The audit trail** of follow-up writes.
- **Availability:** a read must never trigger a scan of clinical tables.

### 11.2 Actors and trust boundaries

Actors: platform admins, auditors, hospital roles (who must get nothing), the platform BFF, core (routes and the scheduler), Postgres; an attacker with a stolen platform session; a curious insider who records every figure, every day, for months.

| Crossing | What is checked there |
|---|---|
| BFF → core (`api.v1.platform`) | Plan 2/3 chain: the BFF's NATS identity, access token, denylist, platform audience, two-step, route roles, `Write`, idempotency |
| core routes → Postgres | `mhai_platform`: snapshots and change tables only; no grant on clinical tables |
| core scheduler → Postgres | `mhai_platform` calling the four SECURITY DEFINER functions: freezing refuses non-calendar windows and unfinished periods; frozen rows are read back only through them |
| functions → clinical tables | `mhai_analytics_reader`: listed columns only, rows of the last 120 days |
| stored snapshot → response | strict decode (`smallgroup` decoders refuse a shown 1–4), period and kind checked, render takes guarded values only |

### 11.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| T1 | Info disclosure | **Day-to-day differencing:** two reads on different days of the same period | figures fixed per finished period; one stored snapshot; no request computes | A1, A2 |
| T2 | Info disclosure | **Rolling-window differencing** through `period=7d` on analytics, or by calling the counts function directly | analytics accepts only `week`, `month`, `quarter`; the SQL functions refuse non-calendar windows | A3, A4 |
| T3 | Info disclosure | **Nesting differencing:** quarter − shown months, month − shown weeks | nested periods only (R41a-2); parent and children as one set with the bridge check; the history check; fallback, `unavailable` | A5–A8 |
| T3a | Info disclosure | **Chaining from known zeros:** solving month-edge or hidden cells from a shown 0 or from the days before onboarding | shown zeros and onboarding as equations in the guard and in the history test (§6.1) | A7, A26 |
| T3b | Tampering / Info disclosure | **Late data** makes a parent differ from the sum of its published children, so parent − children reveals the late rows | counts frozen at close; parents are sums of frozen children; no recount (§6.2) | A2, A27 |
| T4 | Info disclosure | **Cross-route differencing:** guardrails and quality hide one box differently | both render from one snapshot; a test compares the cells | A9 |
| T5 | Info disclosure | **Old rolling windows combined with calendar figures** during the notice | Q2; sunset after 90 days; Option B hides them now | A10 |
| T6 | Info disclosure | **Derived values reveal a hidden one:** a share, a ratio, a median, a change, `previous`, a trend or series point | I2: render from guarded cells only; `ShareOf`; changes only on shown values; medians over shown values; count medians in (0, 5) hidden | A11–A13 |
| T7 | Info disclosure | **Change existence leaks:** a flag on a hidden value says it moved | flags need both sides shown and above the minimums | A12 |
| T8 | Info disclosure | A platform role reads **clinical rows** | no grant; functions return aggregates; C17 extended to the new functions and tables | A14 |
| T9 | Info disclosure | **The follow-up note** reaches logs, spans, the audit payload or NATS | own table; closed audit schemas; `logx` redaction; marker test | A15 |
| T10 | Info disclosure | **A stored body tampered with** or written wrongly serves a raw 1–4 | strict decode on read: a shown 1–4 or an unknown field is refused, and the area is `unavailable` | A16 |
| T11 | Tampering | A snapshot written for a period **not yet finished**, or rewritten after serving | insert policy `period_to + 15 min ≤ now()`; no `UPDATE`/`DELETE` grant; `ON CONFLICT DO NOTHING` | A17 |
| T12 | Tampering | **Lost follow-up:** two admins write at once; a non-author edits a note | conditional `UPDATE` on `version`; 409; author-only edit, 403 | A18, A19 |
| T13 | EoP | An **auditor or a hospital role** writes a follow-up | route roles; no `AuditorWrite`; hospital audiences get `not_platform` | A20 |
| T14 | Repudiation | Who followed up, edited or undid | three audit actions in the write's transaction | A21 |
| T15 | Spoofing | A **forged change or hospital id** reads another series or writes another change | ids are random uuids, resolved in the tables; unknown → 404 | A22 |
| T16 | DoS | **Polling** analytics or series as an expensive query | reads touch stored rows only; 30 a minute per session | A23 |
| T17 | DoS | **The guard's elimination** runs long or never ends | background job; budget test at 50 hospitals; the loop ends (all hidden, then `unavailable`); context timeout → no row, retried next wake | A24, A7 |
| T18 | Tampering | **Two core copies** compute different snapshots for one period | advisory lock; `ON CONFLICT DO NOTHING`; every copy serves the stored row | A25 |

### 11.4 Controls (decided)

- **Authentication:** Plan 2 sessions, two-step always on platform routes. No step-up for follow-ups (§7.4).
- **Authorization:** core's route table only; `platform_admin` for writes, `platform_admin` and `auditor` for reads. Database grants as §8, with no row grant on a clinical table to any platform role.
- **Data handling:** raw counts frozen per period in `analytics_period_counts`, which no platform role can select, and otherwise only in the scheduler's memory; snapshots hold guarded output only; the note stays in `analytics_follow_ups`; no figure, note or count in any log line or error.
- **Concurrency:** snapshot insert is idempotent per period; the scheduler holds an advisory lock; follow-up writes are conditional on `version`; a change is open at most once per key (partial unique index).
- **Secrets:** none new.
- **Third parties:** none. Nothing leaves our network.

### 11.5 Abuse-case tests (the plan writes these first)

- **A1** Two reads of `period=month` on the 3rd and the 27th are byte-identical, `updated_at` included (it is the stored `computed_at`).
- **A2** A decision committed late (after its week closed) changes no served figure: not the week, not its month or quarter when they close later.
- **A3** `GET /platform/analytics?period=30d` is 422 `validation_failed`.
- **A4** As `mhai_platform`, every function called with a rolling window, a period not finished plus 15 minutes, or a block older than 118 days fails with 22023.
- **A5** A quarter whose three months show every cell but one, and whose quarter cell would let `Q − M1 − M2` give a hidden 1–4: the quarter cell is hidden.
- **A6** A month whose frozen hidden week cells sum to 3: the month total is hidden (rule 2 on the parent set); a month with exactly one hidden week cell in a row and the month shown: the month cell is hidden (bridge).
- **A7** **The history test, anchored at onboarding** (property): 500 random histories of 1–3 years from each hospital's onboarding (1–8 hospitals, onboarding dates spread over the history, all three families, hospitals deactivated and added, counts biased to 0–7 with runs of zeros), every week, month and quarter published through the guard in calendar order. The exact rational solver, over **all** published periods at once, with every shown 0 and every pre-onboarding day as zero equations, exposes no hidden cell and no hidden-member sum of 1 to 4; and the guard always ends.
- **A26** The review's case, hand-built: a hospital onboarded mid-month and a month shown as 0 next to weeks that would, chained, solve a month-edge week; the guard hides enough that the solver exposes nothing. The same case with Monday-to-Sunday weeks (a test-only calendar) is exposed, so the test is proven to catch it.
- **A27** `mhai_analytics_freeze` for a month whose weeks aren't all frozen fails with 22023; a frozen month's counts equal the sum of its weeks, whatever was committed since.
- **A8** The modular exposure test agrees with the rational one on every A7 case; a hand-built case where only one prime divides a minor is still exposed.
- **A9** For the same period, `GET /platform/guardrails?period=month` and the analytics quality area show the same cells shown and hidden.
- **A10** A deprecated `period=30d` response carries `Deprecation`, `Sunset` (at least 90 days later) and the `Link`; after the sunset date it is 422. Under Option B of Q2, every hospital and `all` cell is hidden.
- **A11** No route's JSON anywhere contains a shown value of 1–4, and every share, ratio and median over a hidden input is hidden.
- **A12** A change is never flagged when either side is hidden or below the minimum; a `summary` contains only the two shown numbers and no hospital name.
- **A13** Medians and series medians are computed only from shown cells (a test feeds a body whose hidden raw values would change the median and checks it doesn't); a count median of 2.5 is sent hidden.
- **A14** As `mhai_platform`, `SELECT` on `clinical_queries`, `clinical_decisions` and `feedback_events` fails; the four functions return only their declared columns.
- **A15** A synthetic marker in a follow-up note appears in no log line, span, audit payload or NATS message, only in the changes list.
- **A16** A stored body with a shown 3, or an unknown key, is not served: the area is `{unavailable: true}`.
- **A17** Inserting a snapshot for a period that ends in 10 minutes fails the policy; `UPDATE` and `DELETE` fail for `mhai_platform`.
- **A18** Two `PUT`s with the same version, in parallel: one 200, one 409 `conflict`; nothing is silently overwritten.
- **A19** A note edit by another platform admin is 403; that admin's `DELETE` with the current version is 200.
- **A20** An auditor's `PUT` and `DELETE` are 403; a clinician's or hospital admin's session gets `not_platform` on every route.
- **A21** Each follow-up write writes exactly one audit row with its closed payload, in the same transaction (a forced failure after the write rolls both back).
- **A22** A random uuid as a change or hospital id is 404 on every route that takes one.
- **A23** 100 parallel analytics reads run no statement against a clinical table (statement log), and the 31st read in a minute is 429.
- **A24** Guarding a full block at 50 hospitals finishes inside the budget on the build host.
- **A25** Two core copies whose scheduler loops wake together store one snapshot per period, and both serve it.

### 11.6 Residual risk and sign-off

- **Bound-based narrowing** (TODO, from Plan 4c) remains: the guard proves no small box is exactly solvable, not that none can be narrowed to a range using non-negativity. The time dimension adds more equations, so more narrowing is possible. A full check needs linear programming over the bounds. Accepted for the same reason as R41: platform admins and auditors only, MFA, counts of AI decisions and staff activity, never patients.
- **Distinct counts and medians** (`active_clinicians`, `median_review_hours`) aren't linear. Nested periods give only bounds (a month's clinicians are at least the most in any of its weeks). Same acceptance.
- **The modular exposure test** can miss an exposure only when both 61-bit primes divide the same minor of the equation matrix. That is negligible, and A8 tests the agreement.
- **Combinations of hidden cells that aren't a set** (for example, weeks 1 and 3 of a month together) are checked only when they are the hidden members of a set (§6.3 step 3). Another combination could be exposed with a small value. The history test measures how often; if it isn't rare, the check widens to every run of weeks.
- **Distinct counts and medians at a parent's close** may include rows committed after a child closed (they aren't sums of children). They are non-linear and give bounds only; accepted with them.
- **Frozen raw counts at rest.** `analytics_period_counts` keeps per-hospital aggregate counts, small ones included. No platform role can select them, and they hold no row, user or text.
- **During the deprecation notice** (Q2, Option A), the rolling guardrail windows keep R41's residual and can be combined with calendar figures.
- **Sign-off needed:** the product owner on Q2 and Q3, the privacy lead on Q4 (§16). Privacy: that changes flagged only on shown values, and the staff count as a plain number, are acceptable.

## 12. Retention

`analytics_snapshots` and `analytics_period_counts` keep at least 12 weeks, 12 months and 8 quarters, the most any trend or series shows. It holds guarded figures only. The retention plan adds the sweep, with the `guardrail_snapshots` sweep already in TODO. `analytics_changes` and follow-ups are kept for 13 months (a year's comparison); the follow-up's history is also in the audit log.

## 13. Changes mhai-web needs

| # | Change | Why |
|---|---|---|
| F1 | `Period` becomes `"week" \| "month" \| "quarter"`; the picker reads **Last week**, **Last month**, **Last quarter** (default month), and shows the period's dates ("1–31 Oct 2026") and that figures change only when a period ends | R41a: no rolling windows |
| F2 | What changed compares **the latest week with the latest finished month before it, per day**, recomputed weekly; copy "the last 7 days vs the 4 weeks before" changes; `window` runs from the month's start to the week's end | R41a, R41a-2 (no total spans several periods) |
| F3 | `trend` and both series are **successive periods** (12 weeks, 12 months or 8 quarters), not days within one period; reporting weeks are 7 to 10 days, so bucket labels show the range ("22–31 Oct") | Figures exist only per finished period |
| F4 | **`alerts: null`** means "not switched on" (as Plan 5b's `UsageOut.alerts`): no Alert safety tab content, a line saying so, and **the Critical guard treats `null` as nothing to check** instead of failing What changed closed | No alerts exist in the backend; today the guard would fail What changed closed for good |
| F5 | Weeks are month-aligned (1–7, 8–14, 15–21, 22–end): week labels by date range, never "week of" a Monday | R41a-2 |
| F6 | `previous` can carry one decimal (a month scaled to the week); `summary` never names the hospital | §7.3 |
| F7 | Follow-up errors: a long or bad note is 422 **`validation_failed`**, not `invalid` (as for the failed-jobs writes) | Core's single validation code |
| F8 | Audit kinds for `analytics.followed_up`, `analytics.follow_up_edited`, `analytics.follow_up_undone` in `kinds.ts`; until then those rows are withheld (R21) | Activity |
| F9 | `Hospital.admins` stays on sample data: core doesn't serve `GET /platform/hospitals/{id}` yet. The contact block must handle `admins` missing | §3 |
| F10 | The hospital series measure select offers `questions_per_clinician` and `needs_review_share` only until alerts exist (the two Critical measures are 422) | §7.2 |
| F11 | Definitions to show in help text: confidence high/medium/low are pass/regenerate/escalate; needs review is an escalated answer; agreement rate is agreed reviews over all reviews | §5 |
| F12 | Shares come with 4 decimals, as on every core route; the app's two-place backstop still works | `smallgroup.ShareDigits` |
| F13 | A weekly view can be `{unavailable: true}` for an area for a whole week (a snapshot the guard couldn't publish) | §6.3 step 4 |

The contract file `docs/specs/2026-10-17-frontend-requirements.md` §14.7 is updated in this repository (Task 10), and these changes go to mhai-web's TODO.

## 14. Other changes outside this design's code

- **TODO.md:** R41a moves to Decided; the R41a open entry goes; "bound-based narrowing" is widened to the time dimension; "snapshot retention" names `analytics_snapshots`; Plan 7d and the alerts area wait on the notifications service and the EMR integration.
- **ARCHITECTURE.md, services/core/README.md:** the scheduler, the snapshot tables, the analytics routes.
- **CHANGELOG.md:** Added (analytics routes, calendar periods for guardrails) and Deprecated (rolling guardrail windows).
- **Tracker:** 7c backend rows to their parts.

## 15. Task breakdown for the plan

| # | Part | Task | Main tests |
|---|---|---|---|
| 1 | 7c-1 | The nested calendar (Go and SQL twins) | calendar table test, A3 (unit) |
| 2 | 7c-1 | The joint guard in `smallgroup`: parent sets with the bridge check, the history check (known zeros, onboarding), reference solver, fallback | A5–A8, A24, A26 |
| 3 | 7c-1 | Migration 00019: reader role, frozen counts, four functions, `analytics_snapshots` | A4, A14, A17, A27 |
| 4 | 7c-1 | The snapshot builder and the scheduler | A2, A16, A25, A7 end to end |
| 5 | 7c-1 | Guardrails on calendar periods; deprecation of rolling windows | A9, A10 |
| 6 | 7c-2 | `GET /platform/analytics` | A1, A3, A11, A13, A23 |
| 7 | 7c-2 | The series routes | A13, A22 |
| 8 | 7c-3 | Migration 00020 and the weekly What changed | A12 |
| 9 | 7c-3 | Follow-ups: writes, audit, note rules | A15, A18–A22 |
| 10 | all | Smoke, contract, docs (each part's slice) | smoke, `oasdiff` |

## 16. Open questions (need a person)

Q1 (which weeks) was decided as R41a-2: month-aligned weeks.

2. **Q2, product owner: the rolling guardrail windows during the 90-day notice.** Option A, ADR 0012's default: serve them unchanged, with R41's residual, until the sunset. Option B (*recommended*): from the deprecation release, serve them in the same shape with every cell hidden. No client calls `GET /platform/guardrails` (mhai-web has no screen for it), and the rolling windows can be combined with the new calendar figures. Option B is a change in what a response means, which ADR 0012 rule 3 counts as breaking unless the product owner rules that a guarded value hidden more is within the contract.
3. **Q3, product owner: the change thresholds** in §7.3 (minimums 20, changes of 50% or 15 points, review times 2×). They are a design call held in one Go table. Anti-goal 1 (false alarms at small hospitals) depends on them.
4. **Q4, privacy lead: the staff count as a plain number,** and the median review time shown for 5 or more decisions. The frontend contract has both; they are flagged here because a median of five is a real person's review time.
