# Patient Analysis, named access and clearance letters (hospital side): backend design

- **Status:** Draft, revised after review (2026-11-07). Design only, no code. **Live use needs clinical, privacy and legal sign-off** (§15). This design changes the rule that hospital admins see no clinical content.
- **Date:** 2026-11-07
- **Rulings this design follows:**
  - **R41a (product owner, 7 Nov 2026): fixed periods.** Statistics show only finished, non-overlapping calendar periods (the last full week, month or quarter), never rolling windows.
  - **C3 (refining R41a): nested periods only.** A "week" is a month-aligned block (days 1–7, 8–14, 15–21, and 22 to the month's end), so weeks sit inside months and months sit inside quarters. A quarter is guarded together with its three months as one set.
  - **Exact counts, no noise.** `FilterCap` is 2.
- **Covers:**
  - the backend for frontend contract §10 (Analysis, Plan 6);
  - named access in Analysis (Plan 11c): `POST /analysis/patients/named`, and `GET /analysis/clearance/me` with `reason`;
  - the hospital side of clearance letters (Plan 11a);
  - the audit-log parts that wait on these: pseudonym reveal, the named-access list, exports, and a legal resolution path (contract §14.9).
- **The contract is mhai-web's** (read-only for this repo):
  - `mhai-web/docs/specs/2026-10-17-analysis-design.md` (§9, the assumed contract) and `docs/plans/2026-10-22-plan-6-analysis.md`;
  - `mhai-web/docs/specs/2026-10-22-clearance-letters-design.md` (§10) and `docs/plans/2026-10-22-plan-11-clearance-letters.md` ("Rulings made while planning", Task 13);
  - `mhai-web/apps/clinical/lib/api/analysis-contract.ts`, `clearances-contract.ts` and `lib/analysis/guards.ts`. Where the spec and the code differ, the code is right;
  - for consistency, `mhai-web/docs/specs/2026-10-22-platform-analysis-design.md` (Plan 12, on mhai-web `main`).

  The contract changes that the rulings force are listed for mhai-web in §15, item H6.
- **In this repo:** `docs/specs/2026-10-17-frontend-requirements.md` §3, §5.1, §10 and §14.9.
- **Builds on (and must stay consistent with):**
  - **The Plan 4 design and Plan 4c as built:**
    - `internal/smallgroup`: the count, set, share and across-rows rules, the bridge check, and property test C20;
    - whole days and stored snapshots (R42, R43, `guardrail_snapshots`, migration 00017);
    - the audit catalogue's pseudonym test (C22).
  - **Plan 2:**
    - row-level security (RLS) keyed on `mhai_current_hospital()`;
    - the database roles `mhai_gateway`, `mhai_platform` and `mhai_auth`;
    - the audit hash chain;
    - step-up through `auth.Service.StepUpAudited`;
    - `Idempotency-Key`;
    - the principal cache (`authz.MaxCacheTTL`, 30 seconds).
  - **ADRs:** 0004 (retrieval scoped to one patient), 0009 (many EMR systems, one canonical model, patients matched within a hospital), 0010 and 0011 (Go owns the rules; the BFF talks to core over NATS) and 0012 (additive changes only in `v1`).
- **Not in this design:**
  - the platform side of clearances and of the named-access switch (contract §14.10, Plan 11b), outlined in §8.10 only as far as the hospital side needs it;
  - platform Analysis across hospitals (Plan 12; §5.8 says what it must reuse);
  - the EMR integration service, the documents service and the notifications service.

## 1. Why

mhai-web has built three things on sample data:

- the Analysis page, with five views, filters, a de-identified drill-down and the Analyst role;
- the clearance-letter workflow;
- two gates: `ANALYSIS_SIGNED_OFF` and `NAMED_ACCESS_SIGNED_OFF`.

The product owner wants population and disease analysis that goes beyond the Usage counts. It is for hospital admins and analysts now, and later for platform admins across hospitals.

Three things make it harder than Plan 4c's guardrail statistics:

1. **It shows clinical content to people who see none today.** Hospital admins are not clinicians, and contract §3 says they see no clinical content. This design changes that rule in two places only:
   - aggregates and pseudonymous rows, under disclosure control;
   - names, only under a verified clearance letter.
2. **Filters multiply what can be asked.**
   - Fixed, nested periods (R41a and C3) remove differencing over time.
   - The filters remain. Two filters over seven dimensions still reach the same group by several routes.
3. **The drill-down is patient-level microdata.** A de-identified row still describes one person: pseudonym, age band, sex, setting, conditions and findings. Anyone who can drill can count subgroups for themselves (§5.2).

## 2. Goals, non-goals, anti-goals

**Goals**

- **Serve contract §10.** The routes are:
  - `GET /analysis/filters`;
  - `GET /analysis/{view}` for the five views;
  - `GET /analysis/patients`, de-identified;
  - `POST /analysis/patients/named`;
  - `GET /analysis/clearance/me`;
  - `GET /analysis/named-access`;
  - the hospital clearance routes.

  Changes are additive, except the move to fixed periods, which the R41a ruling requires (§9).
- **Answer only for finished, nested calendar periods.** Each period is computed once after it closes and is then frozen.
- **Disclosure control at least as strong as Plan 4c on every response.** In addition:
  - a period is guarded together with the periods inside it;
  - a group has one guarded state across the filter contexts that reach it.

  Both come with a proof sketch, solvers in tests, and a measured residual.
- **One computation path, in Go and Postgres.**
  - The hospital comes from the session, and in the database from `mhai_current_hospital()`.
  - Raw counts never leave core.
  - Nothing is computed in Python.
- **The `analyst` role.**
  - A hospital admin grants it, with step-up, and only to someone who is also a clinician.
  - The rules are enforced in the database.
  - Grants and removals are audited and take effect at once.
- **Named access.** Names show only when all of these hold:
  - a verified, current letter covers the person;
  - the named-access switch is On;
  - the person gives a purpose.

  Every named view is audited and flagged. We never store, log or cache names.
- **The audit-log parts that were waiting:**
  - pseudonym reveal;
  - the named-access list;
  - exports;
  - a legal resolution path that works.

**Non-goals**

- Statistical inference (confidence intervals, significance tests), forecasting, free-form queries or SQL.
- Rolling windows, custom day ranges and perturbation (noise).
- Patient-level views across hospitals (Plan 12 forbids them).
- Matching one person across hospitals (ADR 0009).
- Judging whether a letter is legally sufficient (Plan 11 §2).
- Feeding anything from Analysis into retrieval, prompts, models or the guardrails.

**Anti-goals: failures even if everything else works**

1. **Someone without drill-down access works out a small group (1–4).**
   - They have a response, several responses (across periods, levels, filters or views), a CSV, or a screenshot.
   - This is what the aggregate rules protect against. People with drill-down access see microdata by design (§5.2).
2. **A name or MRN is served when it shouldn't be.** That means without a verified, current letter covering the caller, with the switch below On, or after revocation, expiry, a role change or deactivation.
3. **Names, or rows that carry pseudonyms, are stored outside the mapping and audit tables.** For example in logs, traces, the idempotency store, a snapshot, an audit payload, or any NATS message except the one identity call.
4. **Data crosses hospitals.** One hospital's figures, pseudonyms or letters can be reached from another hospital's session, or a hospital id is taken from a request.
5. **Analysis data reaches a clinical answer** through retrieval, a prompt or a model (SKILLS.md §1).
6. **A finished period gives two different answers**, whether on two core copies, on two days, or after late data arrives.

## 3. Who uses it

| Who | Analysis aggregates | De-identified drill-down | Names | Letters |
|---|---|---|---|---|
| Hospital admin (with two-step) | own hospital | own hospital | only when covered by a letter and the switch is On | upload, view, narrow, revoke, renew |
| Clinician with `analyst` (with two-step) | own hospital | own hospital | only when covered and the switch is On | none |
| Clinician | no | no | no | no |
| Platform admin | no (Plan 12 has its own routes) | no (Plan 12b has its own route, under a letter naming them) | never | verify, reject, revoke (Plan 11b) |
| Auditor | no | no | no | read and view letters; audit reveal and exports |

- **Role checks on patient-level routes.** These routes re-read the caller's role, account status and cover inside the request transaction (§6.4).
- **Audience.** Analysis routes serve the clinical app only. A platform account gets 403 `forbidden`.

## 4. Data sources and what is stored

### 4.1 Sources

| Source | Gives | When |
|---|---|---|
| **MHAI consultations** (`clinical_queries`, `clinical_decisions`, `feedback_events`) | assessed condition (`diagnosis`, `icd10_code`), age (stored as a band), sex, specialty, urgency, the guardrail decision (mapped to a confidence level), the review outcome, the correction outcome | now, **but see §4.2** |
| **EMR records**, through the EMR integration service (ADR 0009 canonical model) | recorded diagnoses, lab results with flags, setting, location, source label | with the EMR plan |
| **Patient alerts** (Plan 5a) | findings, severity, time to acknowledge, dismissal reason | with the EMR plan and the notifications service |

**Mapping consultations to confidence levels.** This is a clinical sign-off item (H2).

- `high`: `PASS` with a high trust band.
- `caution`: any other `PASS`.
- `review`: `REGENERATE`, or escalated with an answer.
- `failed`: escalated or degraded with no answer.

**Agreement and corrections.**

- Agreement is the "agree" ratings divided by all rated consultations.
- Corrections come from `feedback_events` in `pending_review`, `approved` or `rejected`. Withdrawn ones are left out.

### 4.2 The counting unit is a patient key

The contract counts **patients**: `Meta.patients`, new versus existing, co-occurring pairs and the drill-down rows. **No consultation carries a patient identity today.**

- `patient_data_json` holds age, sex, symptoms and history, with no id.
- Adding an id is deferred to the EMR plan (TODO.md, "Patient ids in questions and tokens").

So:

- **A consultation counts only when it carries a hospital-scoped patient reference.** That is either the EMR patient-index id or, later, a consultation `pid`. A consultation without one is never counted as a patient and never guessed into one (§16 Q2).
- **Until patient references exist, live Analysis has nothing honest to show.**
  - The backend is built and demonstrated on synthetic subjects in the demo hospital (§14).
  - Facts are ingested only for hospitals whose analysis switch is On (§4.4). In phases 2–6 that is the demo hospital only.

### 4.3 What is stored

Everything below is in core's database, under RLS, and written only by core.

| Table | Holds | Never holds |
|---|---|---|
| `analysis_subjects` | `hospital_id`; `subject_id` (a random UUID, internal only); `ref_hmac` (an HMAC of the patient reference with a core key, for matching); `ref_enc` (the reference encrypted with a core key, used only for the identity call, §8.6); `created_day` | an MRN, a name, a date of birth, the reference in clear. The reference is the EMR service's own opaque patient-index id, never an MRN |
| `analysis_facts` | One row per event: `hospital_id`, `subject_id`, `day` (hospital day), `kind` (`consultation`, `diagnosis`, `finding`, `alert`), `condition_id`, `age_band`, `sex`, `setting`, `location_id`, `specialty`, `source_id`, the coded fields of its kind, `ingested_at`, and `period_late` (true when it arrived after its period froze) | free text, question or answer text, exact age, time of day, any identifier |
| `analysis_first_seen` | `(hospital_id, subject_id, condition_id)` → `first_day` | anything else |
| `analysis_conditions` | the canonical dictionary, plus one "Not standardised" entry per hospital and source (its name is null) | local free-text names |
| `analysis_locations`, `analysis_sources` | per hospital: an opaque id, the hospital's own label, and the setting | vendor names |
| `analysis_periods` | One row per hospital and period: `level` (`week`, `month`, `quarter`), `start`, `end`, `frozen_at`, `engine_version` | anything else |
| `analysis_snapshots` | Guarded response bodies, keyed by hospital, period and canonical request. Stored the first time they are computed, then always served as stored; kept 25 months (§11) | a raw count |
| `analysis_pseudonyms` | `hospital_id`, `subject_id`, `side`, `scope`, `period_id`, `pid` (§4.5) | anything derived from a reference |
| `analysis_views`, `analysis_view_rows` | One row per drill-down: viewer, level, period, filter codes, drill, count, purpose, clearance id, `named_until`, audit id; plus, in order, the subject ids of the rows sent | names, MRNs, pids (these are derived on reveal) |
| `named_view_notes` | The purpose note for "Other" (at most 200 characters), encrypted, linked to the view | anything that goes into audit or logs |
| `clearances`, `clearance_covered`, `clearance_events`, `named_access_switch`, `named_view_keys`, `legal_resolutions` | §8 and §10 | the letter's bytes, which go to object storage |

**Retention**

| Data | Kept for |
|---|---|
| Facts and `analysis_first_seen` rows | 25 months: 24 months of answerable periods plus one month |
| Snapshots | 25 months, the same as the periods they answer |
| Views, view rows, notes, pseudonym rows, and the `analysis_subjects` rows they cite (including `ref_enc`) | as long as the audit records that cite them, so the legal path (§8.9) still works after the facts are gone. A legal hold takes precedence |
| Letters | at least as long as the audit records that cite them |

### 4.4 How data gets in

**Which hospitals.** Only hospitals whose analysis switch (§7) is On.

- Ingest refuses any other hospital, in the ingest function itself.
- A test checks that a hospital with the switch Off has no facts (T25).
- Turning a hospital's switch Off stops ingest. Its facts are kept and age out under retention.

**Consultations.** A nightly ingest per hospital, run by core as `mhai_analysis_writer`:

- it reads decisions and feedback committed since its last watermark;
- it keeps only those with a patient reference;
- it writes facts.

No worker changes, and the intelligence boundary is untouched.

**EMR events and alerts (later).** The EMR service publishes coded canonical events, and the same nightly run writes facts from them. Name and MRN stay in the EMR service.

**Late facts.** A fact whose day falls in a period that has already frozen is stored with `period_late = true`.

- It is never counted in that period, and never in a later one.
- It still updates `analysis_first_seen`, which affects only periods that have not frozen yet.
- Late facts are counted in a metric, so lateness is visible to operations.

### 4.5 Pseudonyms: stable per period

- **Shape.** `P-` followed by 8 characters from the RFC 4648 base32 alphabet (A–Z and 2–7). This fits the contract's `^P-[A-Z0-9]{4,12}$`.
- **Random, not derived.** A pid is drawn per `(hospital, side, scope, period, subject)`.
  - A unique index catches collisions. At 40 bits, the chance is about 0.5% at 100,000 subjects; a collision is simply drawn again.
  - Because pids aren't derived, nobody can compute one from an MRN or a reference.
- **Stable within a period, different between periods.** One person's pid is the same in every list from one period, and different in every other period (and at a different level). Lists from different periods therefore can't be linked by pid (I3).
- **Side and scope.**
  - The hospital side has one scope.
  - The platform side (Plan 12b) is scoped to the platform admin and the letter.
- **Contract change.** mhai-web's contract says pids are "stable within the hospital". This design makes them stable per period instead (H6).
- **No look-alikes.** Nothing else this service emits matches the pid shape:
  - drill keys are 43 characters of base64url;
  - audit cursors are hex;
  - export ids follow §8.8.

  C22 still holds (P7).

## 5. Disclosure control

### 5.1 Constants (Measured Health's, held by the backend)

| Name | Value | Meaning |
|---|---|---|
| `Min` | 5 | `smallgroup.Min`: a count of 1–4 is hidden |
| `QuerySetMin` | 10 | a filtered view with fewer patients than this is hidden whole |
| `FilterCap` | 2 | at most two filters besides the period; a third gets 422 `too_many_filters` |
| Grace | 3 days | how long after a period closes before it is computed and frozen |
| History | 24 months | the oldest answerable period starts within 24 months |
| `DrillMax` | 200 rows | per list |
| Distinct-patient cap | 300 (proposed, §16 Q9) | distinct patients one person may open in drill-downs per period |

### 5.2 Who the aggregate rules protect, stated plainly

- **Who they protect against.** The small-group rules protect against people **without drill-down access**:
  - anyone who sees a chart or the CSV on the device, or a screenshot of either;
  - anyone given an exported aggregate;
  - Plan 12's platform admins, who see aggregates only.
- **What drill-down users can already see.** A drill-down shows an exact `count`, and rows carrying age band, sex, setting, conditions and findings. Anyone allowed to drill (hospital admins and analysts) can count any subgroup of a drillable group (5 or more) for themselves, including groups of 1–4.
  - No rule on the charts can stop that.
  - Noise on the charts would be undone by the exact drill counts, which is a second reason, besides the ruling, that this design uses none.
- **What controls drill-down.** Role, purpose (for names), audit, row minimisation, pid stability limited to one period, and per-person caps (§5.6). The privacy lead confirms this trade (H1).
- **Drill counts match the charts.** A drill count always equals the shown cell it was opened from. Both are exact and from the same frozen period, so a list never contradicts its chart.

### 5.3 Layer A: each response

Every number passes through `internal/smallgroup` or through `internal/analysis/disclose`, which is built on it. A raw count never reaches a response writer.

- **A1 Count.** `smallgroup.Count`: 0 shows as 0, 1–4 are hidden, 5 and above are shown. It applies to every count: patients, cases, consultations, results, alerts, reviews, corrections and pairs.
- **A2 Sets.** Rule 2 holds for every set that adds up (`smallgroup.Set`, `Within`). The sets are those `guards.ts` checks, plus:
  - **by-condition rows against the period's total.** `by_condition` lists every condition with a case in the filtered set, with no truncation and including "Not standardised". So its cases, new and existing columns sum to `total`. The table is guarded with `smallgroup.AcrossRows`.
  - **the period's series against its total.** The series points are the period's children (§5.4). For additive measures (cases, new, consultations, results, alerts), the points sum to the period's total, and the column is guarded together with the total. Distinct patients don't add up, so `patients` per point is guarded per point only (see A3 and §5.5).
- **A3 Partitions of patients.**
  - Each of `by_age`, `by_sex`, `by_setting`, `by_location` and `by_specialty` is a complete partition of `patients`, including null and "not recorded" rows.
  - Each is guarded as its own set against `patients`.
- **A4 Shares, rates and medians.**
  - Shares and rates use `smallgroup.ShareOf`, with four decimal places.
  - A median over fewer than 5 values, or over a hidden count, is hidden.
  - A median is a whole number of minutes or a half.
- **A5 Pairs.** For each co-occurring pair:
  - `a_patients` and "both" form a set;
  - `share_of_a` is `ShareOf(both, a_patients)`;
  - the top 20 pairs are chosen by shown value, and hidden pairs follow in name order.
- **A6 Order.** Shown rows come first, sorted by value. Hidden rows come after, in a fixed order.
- **A7 Whole view.** When `patients` is hidden, or below `QuerySetMin` with any filter set:
  - every number in the view is hidden;
  - no row has a `drill`;
  - **lists are sent empty**, so a narrow view can't reveal which conditions its few patients have (H6).
- **A8 Drill keys.** A drill key is set only on a shown count of 5 or more.
  - It is an HMAC over `(hospital, period, filters, group)` with a core key, encoded as 43 characters of base64url.
  - `GET /analysis/patients` re-derives the group and recomputes the count from the frozen period. It answers 422 `too_small` unless that count is shown and 5 or more.
- **A9 No repeated keys** within a list.

### 5.4 Layer D: across periods, levels and filters

**D1 Finished, nested periods only.** Rulings R41a and C3.

- **The levels:**
  - **week**: a month-aligned block of days 1–7, 8–14, 15–21, or 22 to the month's end (7 to 10 days);
  - **month**: a calendar month;
  - **quarter**: a calendar quarter.
- **Days.** All are hospital days (`hospitals.time_zone`).
- **What is answerable.** A request names exactly one finished period within the last 24 months. Each level partitions the one above it: blocks partition months, and months partition quarters.
- **No rolling windows, no custom ranges.** Two answerable periods are either disjoint or one contains the other. This removes R41a's differencing (two overlapping windows, or "the day that entered minus the day that left").

**D2 Freeze after close plus grace.**

- After a period closes, the nightly run waits three days for late facts. At the hospital's next local midnight plus 30 minutes, it marks the period `frozen_at` in `analysis_periods`.
- From then on, the period's inputs never change: late facts are excluded (§4.4).
- Each response body is computed from the frozen inputs. The first time a body is computed, it is stored in `analysis_snapshots`, and from then on it is served unchanged for 25 months. That holds even if the engine changes; a new `engine_version` applies only to periods frozen afterwards.
- A period that is closed but not yet frozen can't be answered (`409 not_ready`). The period picker offers only frozen periods.
- Result: every core copy, every day, gives the same answer for a finished period (R42 and R43, generalised).

**D3 Nesting guarded across levels (C3).** For additive measures:

- **The equations.** A month's value equals the sum of its blocks, and a quarter's value equals the sum of its months. The same holds for `new`, because each case is new in exactly one period.
- **Children first.** A parent period is computed after its children are frozen, and their guarded states are fixed constants.
- **The parent's cell is published only if it gives no hidden child away.** The parent and its children are guarded as one set: rule 2, where only the parent cell may be hidden further (the children were already published).
  - If exactly one child is hidden, or the hidden children total 1–4, the parent cell is hidden.
  - The parent's own response then runs Layer A again with that cell hidden, until nothing changes.
- **The quarter rule.** C3's "a quarter guarded together with its three months as one set" is this rule at the top level. The same rule links months to blocks.
- **Distinct counts don't add.** `patients` and the partitions in A3 aren't additive across levels: a patient seen in two blocks counts once in the month. They have no exact equation, only bounds: the largest child ≤ the parent ≤ the sum of the children. The remaining risk is narrowing by those bounds (R-A2), measured by P5.
- **No grand totals.** Responses never carry a total across several periods (I2). A trend is the `history` array (§9): earlier periods of the same level, each point identical to that period's own frozen total, with no total over them.

**D4 Filter cap and query-set minimum.**

- At most `FilterCap` (2) filters besides the period.
- A filtered view with fewer than `QuerySetMin` patients is hidden whole (A7).

**D5 Hide once, hide everywhere, within a family.**

- **The family.** A request's family is:
  - its own context, `(period, F)`;
  - its parents, `(period, F \ {f})`;
  - with a cap of 2, at most 4 contexts.
- **One state per group.** All five views of every family member are computed from the frozen period. Each group gets one guarded state across the family and its views:
  - a cell hidden anywhere is hidden everywhere;
  - Layer A runs again until nothing changes.
- **A request hidden in its parent.** A filtered request whose own group was hidden in a parent's breakdown is answered hidden whole.
- **What D5 closes:**
  - asking for a complementary cell directly as a filter;
  - comparing views against each other.

**D6 The filter-only residual (R-A1).**

- **What is left.** The same group can be reached from two families that don't overlap. Within one period, for example, `{age=80+}` shows `by_sex.male` while `{sex=male}` hides `by_age.80+`.
- **Why the design doesn't remove it.** Making suppression consistent across every context in the filter lattice is intractable at request time.
- **How big it is.** P2 runs exhaustively at `FilterCap` 2 on synthetic cohorts and reports how often a hidden cell can be solved. That rate is the measured size of R-A1, for the privacy lead (H1).

**D7 Budgets and audit.** These don't prevent differencing. They make it visible.

- Aggregate reads are limited to 30 a minute per session, then 429 `rate_limited`.
- Reads are recorded as `analysis.viewed`, once per session, filter set and period, unflagged.
- More than 100 distinct filter sets in a day from one person (proposed) writes a flagged `analysis.unusual_querying` entry.

### 5.5 Proof sketch

**One response (Layer A).** No hidden cell can be worked out from the response's own equations.

- **Lemma 1 (two-way tables).** By-condition rows against the total, and the series of additive measures against the period's total, are tables with margins. Their row and column sums form the incidence matrix of a bipartite graph whose edges are the hidden cells. A hidden cell can be worked out exactly when its edge is a bridge. `AcrossRows` hides more cells until no hidden edge is a bridge. This is Plan 4c's argument, unchanged.
- **Lemma 2 (partitions that share a total).** The five partitions of `patients` share only `patients`.
  - If `patients` is shown, it is a constant, and each partition is safe by the one-set rule.
  - If it is hidden, A7 hides everything.
- **Lemma 3 (cross-view equalities).** Under D5, the same group in two views is one cell. That adds the equation x = x, which says nothing new.

**History (D1–D3).** No hidden cell can be worked out from all the periods published since the hospital started.

- Because periods nest, the equations across all published periods form a tree: quarter = Σ months, and month = Σ blocks. Each parent node is guarded with its children by rule 2.
- Shown zeros, and the hospital's first period (everything before it is zero), are constants. They give no exact equation that the per-node rule hasn't already guarded.
- The tree has no cycles across nodes, so the per-node argument extends to the whole history. P4 checks this with its own solver.
- **The C3 failure this removes.** ISO weeks cross month boundaries. Publishing ISO weeks and months together gives equations whose solutions isolate the days at month edges, anchored at the start or at any shown zero. Month-aligned blocks have no such equations.

**A family (D5).** One guarded state per group across up to 4 contexts. P2 checks this against the union of the family's equations.

**Not claimed:** consistency across families (R-A1, measured), and bounds on distinct counts (R-A2, measured).

### 5.6 Drill-down controls

**Rows.** A row carries:

- the period's pid;
- age band, sex and setting;
- the conditions and findings recorded **within the period**, at most 10 of each, in a fixed order.

**Per-person caps.**

| Cap | Limit | Over the limit |
|---|---|---|
| De-identified lists | 10 a minute and 50 a day | 429 `rate_limited`, and a flagged entry |
| Distinct patients opened per period, by one person, across all their lists | 300 (proposed) | 429 `rate_limited`, and a flagged `analysis.patient_cap_reached` entry |
| Named views | 5 a day; replays count (M7) | 429 `rate_limited`, and a flagged entry |

**Profiling.** Pids last one period only, so the work of linking lists restarts every period. The distinct-patient cap bounds how much of one period a single person can profile. P8 measures both.

### 5.7 Property and abuse tests (the plan writes these first)

All tests use synthetic cohorts with a seed. The solvers belong to the tests, not to the engine.

- **P1 (one response, exact rational elimination; extends C20).**
  - Random cohorts biased toward small groups.
  - Every view, every period level, every filter set within the cap.
  - Every equation the response implies: sets, partitions, the two-way tables, the series of additive measures against the total, cross-view equalities.
  - Assert that no hidden cell can be solved.
  - Also assert that a drill opened from a cell returns `count` equal to the cell, with exactly that many rows (up to 200), and that every row's attributes match the group.
- **P2 (filters, exhaustive at `FilterCap` 2).**
  - Use small dimensions so that every context of a period can be listed.
  - Collect every number published across all contexts and views as equations over atomic cells: the full cross-classification of subjects.
  - **Assert,** within each family, that no hidden cell can be solved and that every group has one state.
  - **Report,** across all families, the share of hidden cells that can be solved. That share is R-A1.
  - A second run models a drill-capable attacker, who gets every shown group of 5 or more as rows. It reports the subgroups of 1–4 those rows expose, to show the §5.2 trade in numbers.
- **P3 (freeze).**
  - Freeze a period, then add late facts dated inside it, change a dictionary label, and recompute on a second core copy with a newer engine version.
  - Every stored body of the period is served byte-for-byte as before.
  - A body not computed before freezing is computed from the frozen inputs only (late facts excluded).
- **P4 (history solver).**
  - Simulate a hospital from its first period over 24 months, with zeros and small counts.
  - Publish every block, month and quarter as served.
  - Write the tree of equations plus every response's own equations, with zeros and the start as constants.
  - Assert that no hidden cell can be solved.
  - Show that ISO weeks would fail the same test (the C3 regression).
- **P5 (bounds, reported).**
  - A linear program over each response, and over parent and children with the distinct-count bounds.
  - Report the share of hidden cells whose feasible range excludes 0 and is narrower than 0–4. That share is R-A2.
- **P6 (filter list).**
  - A condition is offered in `GET /analysis/filters?period=` only if it has at least 5 patients in that frozen period.
  - The list for a frozen period never changes.
  - Assert that comparing two periods' lists can't reveal a 4-to-5 change within one period (each list is fixed when its period freezes).
- **P7 (shapes).**
  - No drill key, cursor, view id, export id or error matches the pid shape.
  - No aggregate body contains a pid, a name key, an MRN key or a date-of-birth key.
- **P8 (pseudonym linkage, measured).**
  - Have one person run their full daily and per-period drill budgets.
  - Report how many distinct patients they can link into a profile with three or more attributes within a period.
  - Assert that the pids of the same subject in two periods match no more often than chance.

### 5.8 Consistency with platform Analysis (Plan 12)

- **Shared engine.** Plan 12 uses `internal/analysis/disclose` and adds:
  - the across-hospitals rule;
  - the three-hospital minimum;
  - the comparison rules.
- **Periods (M3).** Plan 12's "last 3, 6 or 12 months" are rolling windows, which R41a forbids. Plan 12 changes to the same finished, nested periods: the last full month or quarter, and the last four quarters as a `history` (H6).
- **Time zones per product.**

  | Product | Days |
  |---|---|
  | Analysis (this design) | the hospital's days |
  | Guardrail statistics (Plan 4c) | UTC days |
  | Plan 12, one-hospital scope | that hospital's frozen periods, so the figures match the hospital's own Analysis |
  | Plan 12, all-hospitals totals | the sum of each hospital's periods (every hospital today is in West Africa Time) |
- **Pseudonyms differ by side.** Plan 12 §6 holds, and is now per period.
- **Two releases about one hospital.** The hospital's own Analysis and Plan 12 describe the same hospital in different releases. With identical periods and frozen inputs, the overlapping cells are the same group. Plan 12's coarser filters (20-year age bands, 3-character ICD-10) differ, though, and that is residual R-A3.

## 6. The analyst role and its grants

### 6.1 The rules (in the database, not just in the handler)

A trigger on `user_roles` enforces all of these, whoever writes the row, and refuses the change if any fails (M1):

- **Needs `clinician`.** Granting `analyst` requires `clinician`. Removing `clinician` removes `analyst` (`analyst_removed`, reason `clinician_removed`).
- **The grantor.** The grantor (from the session setting `mhai.user_id`, which core sets from the token) is a hospital admin at the **same hospital**, and is **not** the person receiving the role.
- **The person.** The person is active and has two-step set up.
- **The switch.** The hospital's analysis switch is On.
- **The cap.** The hospital has fewer than 10 analysts (`analyst_cap`).

### 6.2 Who grants, and how

- **The route.** `PATCH /hospital/staff/{id} {version, roles, mfa_code | backup_code}` (contract §5.1; core builds the staff endpoints with or before this).
  - **Step-up is required when `roles` adds `analyst`.** That is an optional field today, required for this change. mhai-web adds it to the dialog (H6).
  - A missing code gets 403 `mfa_required`.
- **Refusals:**
  - 409 `analysis_off`;
  - 409 `analyst_cap`;
  - 403 `self_change`;
  - 422 `not_eligible`.
- **The CLI uses the same path.** `core users roles --add analyst --by <admin>` calls the same definer function, so the trigger applies. It records the operator. It refuses a grantor who isn't a hospital admin of the hospital.

### 6.3 Audit and notice

- **Audit.** `analyst_granted` and `analyst_removed` (type admin) record the actor, the person (by `display_name`) and the hospital.
- **Notice.** When the notifications service exists, every hospital admin is told of each grant.
- **Tests.** T26 tests the cap and the notice against a synthetic puppet account.

### 6.4 Taking effect

- **Aggregate routes.** These use the principal: roles cached for up to 30 seconds.
- **Patient-level routes** (`/analysis/patients`, `/analysis/patients/named`, `/analysis/clearance/me`). These re-read role, two-step state and account status inside the transaction, so removing the role ends access at once.

### 6.5 `features` on the user

`User.features` is additive on the sign-in and session replies: `{analysis, named_access}`.

- **`analysis`** is true when all of these hold:
  - the effective analysis switch is On;
  - the user is a `hospital_admin` or an `analyst`;
  - two-step is on.
- **`named_access`** is true when `analysis` is true **and** the effective named-access position is not Off.
- **The BFF's keys can only turn these off.** `ANALYSIS_SIGNED_OFF` and `NAMED_ACCESS_SIGNED_OFF` never turn anything on.

## 7. Switches

**Analysis aggregates**

- **Positions.** `off` and `on`, set globally and per hospital. The effective position is the lower of the two.
- **Raising** (Off to On, globally or for one hospital) is a two-person action with the clinical, privacy and legal sign-off references. It uses the same machinery as the named-access switch.
- **Lowering** is one platform admin, with a reason and a step-up code. It takes effect at once.
- **While Off:**
  - every `/analysis/*` route returns 404 `not_found`;
  - **ingest stops** (§4.4).

**Named access**

- **Positions.** `off`, `letters_only` and `on`, globally and per hospital. The effective position is the lower of the two.
- **Limits on raising:**
  - a hospital can't be set above the global position (409 `above_global`);
  - every raise needs all three sign-off references (409 `signoff_missing`).
- **Lowering** is one platform admin and takes effect at once.

**Both switches**

- Only `mhai_platform` writes them.
- Every request, approval, cancellation and change is recorded on the audit chain.

## 8. Clearance letters and named access (hospital side)

### 8.1 The letter

**Upload.** `POST /hospital/clearances`, a multipart request (contract §10, Plan 11a), with:

- `file`, `signed_by`, `signer_title`, `letter_date`;
- `covered` (staff ids) and, optionally, `covered_platform` (emails);
- `end_date`, and `renews` if it replaces a letter;
- `mfa_code` or `backup_code`, and an `Idempotency-Key`.

Only hospital admins can upload, and upload needs step-up.

**Checks before anything is stored**

| Check | Refusal |
|---|---|
| Body at most 10 MB + 64 KB | 413 `file_too_large` |
| `end_date` after the hospital's today and no later than `max_end_date` (12 months on) | 422 `end_date_too_late` |
| Between 1 and 20 people covered | 422 `invalid` |
| Each staff member eligible: active, `hospital_admin` or `analyst`, two-step on | 422 `not_eligible`, with the ids |
| Each platform email belongs to an active platform admin with two-step | 422 `not_eligible_platform`, with the emails |
| `renews` is a letter of the same hospital | 422 `invalid` |

**The file never reaches core's parsers.** Core streams it to the **documents service**, which:

- stores the original, encrypted, in that hospital's object storage;
- checks the real file type from its bytes;
- refuses password-protected PDFs and files over 20 pages;
- scans it for malware;
- renders each page as a flat PNG in a sandbox;
- reports back with a fixed set of outcomes (`ready {pages, sha256}` or `refused {reason}`).

Until the documents service exists, live upload returns 503 `unavailable`. The demo uses fixture pages.

**Pages.** `GET /hospital/clearances/{id}/pages/{n}`:

- the documents service stamps each page with the viewer's name and the time;
- the response is `image/png` with `Cache-Control: no-store`, `Content-Disposition: inline` and `X-Content-Type-Options: nosniff`;
- 409 `not_ready` while `pages` is 0;
- every page view is audited.

**Lifecycle.**

- The statuses:
  - `checking` becomes `refused` or `waiting`;
  - `waiting` becomes `active` (verified, Plan 11b), `rejected` or `revoked`;
  - `active` becomes `ended` or `revoked`.
- **Narrowing** only removes people, and never everyone (422 `would_empty`).
- **Versions.** `version` goes up on every change. A stale version gets 409 `conflict`.
- **Enforcement.** A trigger enforces the transitions, the 12-month cap, narrowing-only and `would_empty`.

**Ending.**

- A letter ends at the end of `end_date` in the hospital's time zone.
- The cover check compares against the hospital's today, so access ends exactly then with no job involved.
- A nightly run then sets `ended` and records `clearance_ended`.
- Reminders at 30, 7 and 1 days are recorded. The notifications service delivers them once it exists.

### 8.2 Who is covered: one function

`mhai_named_cover(user)` is a SECURITY DEFINER function. It takes the hospital from `mhai_current_hospital()` and caches nothing. A person is covered only when **all** of these hold:

1. the hospital's effective named-access position is `on`;
2. the user is active, is a `hospital_admin` or an `analyst`, and has two-step on, at this hospital;
3. an `active` letter of this hospital has an `end_date` on or after the hospital's today, and covers the user (who hasn't been removed from it);
4. the hospital is active.

If several letters qualify, the one with the latest end date counts.

**`named_until`** is the earlier of:

- the end of that letter's last day, in the hospital's time;
- now plus 30 minutes.

### 8.3 The named view: `POST /analysis/patients/named`

**The request.**

- Body: `{drill, purpose, note?, filters, period}`, plus a required `Idempotency-Key`.
- **Purpose** is one of `clinical_follow_up`, `quality_improvement`, `audit` or `other`.
- `other` needs a note of 1–200 characters.

**In one transaction, in this order:**

1. **The idempotency key** (§8.5). The same key from another person, or with another body, gets 422 `idempotency_mismatch`.
2. **The analysis gate** (404) and **two-step** (403 `mfa_required`).
3. **The named-view budget** (§5.6). Replays count.
4. **Cover** (`mhai_named_cover`):
   - 403 `named_access_off` when the position is below On;
   - 403 `not_cleared` when no letter covers the person;
   - 403 `clearance_ended` when the only letter that covered them has just ended.
5. **The drill** (A8, on the frozen period). 422 `too_small` unless the count is shown and 5 or more.
6. **Record the view, then commit.**
   - Select the subject ids: at most 200, with `count` as the full number.
   - Insert the view and its rows.
   - Store the note, if any.
   - Append the flagged audit entry `analysis.named_viewed`.
   - Commit. **No audit entry, no names.**
7. **Resolve identity** (§8.6) for exactly those subjects, and reply:

   ```
   {level: "named", count, patients: [row + name + mrn],
    named_until, clearance: {id, end_date}}
   ```

   If identity resolution fails, reply 503 `unavailable` and append `analysis.named_view_failed`.

**The de-identified route.** `GET /analysis/patients?drill&level=deidentified&period…&filters…`

- It runs steps 2, 3 (its own budget, plus the distinct-patient cap), 5 and 6, writing the unflagged `analysis.patients_viewed`.
- It never resolves identity.
- Any other `level` gets 422 `level_not_supported`.

### 8.4 `GET /analysis/clearance/me`

**The reply:** `{named, until, clearance_id, version, reason}`, computed by `mhai_named_cover` on every call.

- **`reason`** is null when the person is covered. Otherwise it is one of:
  - `none`;
  - `ended`;
  - `revoked`;
  - `removed_from_letter`;
  - `role_or_account`;
  - `switched_off`.

  It is taken from the most recent letter that covered the person.
- **Limits.** The route is rate-limited to 6 a minute per session (the app polls every 30 seconds). It is not audited.

**`GET /analysis/named-access`** returns `{position, since}`, the effective position for the hospital.

### 8.5 Idempotency for named views (not the generic store)

The generic idempotency store keeps the reply body for 72 hours. A named reply holds names, so it must never be stored there. Named views use their own table, `named_view_keys`.

- **What it holds.** The key, the user, an HMAC of the canonical body, the view id, `named_until`, and a replay count.
- **A replay** with the same person and body:
  - re-runs every check: the switch, cover, role, budget and drill;
  - re-reads the stored subject ids and resolves identity again;
  - returns the original `named_until`;
  - increments the replay count (M7).

  It writes nothing new to the audit chain. The replay count is reported in `analysis.named_viewed` when the key is swept, and counts toward the named budget.
- **After `named_until`,** a replay gets 409 `expired`.
- **A key from another person, or with another body,** gets 422 `idempotency_mismatch`.
- **Keys are swept** 24 hours after `named_until`.

### 8.6 Identity: names come from the EMR service, at the moment of viewing

- **Core never stores a name or an MRN.**
- **The request.** Core sends `emr.v1.identity.resolve` over NATS, with:
  - a core-signed request token: audience the EMR service; the hospital and the user; at most 30 seconds; single use;
  - the patient-index reference of each subject, decrypted from `ref_enc` for this call only;
  - the view id and the clearance id.
- **The reply.** `{reference: {name, mrn}}` for exactly the references asked, at most 200. Any extra entry, or a reply on the wrong subject, is dropped and the view fails (T20).
- **Nothing is kept.** Core passes the names through and drops them. Nothing is cached, logged, traced or dead-lettered.
- **Prerequisite.** NATS TLS is required before this call runs across hosts.
- **Before the EMR service exists:**
  - live mode returns 503 `unavailable`;
  - the demo resolves names from a synthetic fixture. That fixture **refuses to start outside the demo configuration**, and a test proves it (M2, T27).

### 8.7 Audit entries

Every entry has a closed schema. No entry carries a name, MRN, pid or free text.

| Action | Type and flag | Facts |
|---|---|---|
| `analysis.viewed` | security, unflagged | `view`, `period`, `filters` (codes) |
| `analysis.unusual_querying` | security, flagged | `rows`: the number of distinct filter sets |
| `analysis.patient_cap_reached` | security, flagged | `period` |
| `analysis.patients_viewed` | security, unflagged | `view`, `period`, `filters`, `patient_count`, `rows` |
| `analysis.named_viewed` | security, **flagged** | the same as above, plus `purpose`, `replays`, and `refs.clearance_id` |
| `analysis.named_view_failed` | security, flagged | `refs.clearance_id` |
| `analysis.legal_resolved` | security, **flagged** | `refs.export_id`, `refs.clearance_id`, `rows` (§8.9) |
| `analyst.granted`, `analyst.removed` | admin | the person, by `display_name` |
| `clearance.uploaded`, `.refused`, `.verified`, `.rejected`, `.narrowed`, `.revoked`, `.ended`, `.viewed`, `.reminder_sent` | admin. A file refused for `failed_safety_check` is security, flagged | reason codes, `refs.clearance_id`, pages |
| `named_access.requested`, `.approved`, `.cancelled`, `.changed` | admin | position, scope, sign-off reference codes |

**Purpose notes** live in `named_view_notes`, never in an audit payload.

### 8.8 Pseudonym reveal, the named-access list and exports (contract §14.9)

- **`GET /platform/audit/{id}`** adds:
  - `patients: {count}` on drill-down entries;
  - `letter` on named ones.
- **`GET /platform/audit/{id}/patients`** returns `{pseudonyms}`:
  - the pids the hospital saw, for that entry's period, in order;
  - for platform admins and auditors;
  - it writes `audit_patients_viewed`;
  - it is the only route outside Analysis that sends a pid.
- **`GET /platform/audit/named-access`** lists named-view entries, each with its `letter`.
- **Exports** follow contract §14.9 and are built when object storage exists:
  - a `named_access` export is a ZIP: a CSV plus a copy of each letter cited;
  - each row carries the entry's `export_row_ref` (an opaque id for view and row), the period and the pid. It never carries a name.
  - The export record is written before the file is released.
  - The file is for its owner only, downloadable once, within 15 minutes.

### 8.9 The legal resolution path (replaces the earlier Q14 answer)

**Why a new path.** Pids are random and last one period, so a hospital can't turn an exported pid back into a person from its own records. A legal request that needs names is therefore resolved through MHAI, under the same controls as a named view, plus a legal reference. Legal confirms the path (H3).

**`POST /hospital/legal-resolutions`** takes `{export_id, reference, purpose: "legal_request", mfa_code}` and an `Idempotency-Key`.

- **Who:** a hospital admin of the hospital the export concerns.
- **Conditions:**
  - the admin is covered by a current letter, and the switch is On;
  - step-up is required;
  - `reference` is the legal request's reference, at most 60 characters.
- **What it does:**
  - core reads the subject ids behind the export's rows (`analysis_view_rows`, kept for the audit's retention);
  - it appends the flagged `analysis.legal_resolved` and commits;
  - it calls `emr.v1.identity.resolve`, and returns the rows with name and MRN **in the app only**, never as a file. Print styles hide them, as for named views.
- **Its own limit:** 5 a day per hospital.
- **Platform staff never see the names.** A platform admin or auditor can start the export, not the resolution.

### 8.10 What the hospital side needs from the platform side (Plan 11b, its own design)

- **Routes.** `POST /platform/clearances/{id}/verify|reject|revoke`, each with step-up and `version`.
- **Who may verify.** These two refusals are checked in the transition trigger as well as in the handler:
  - `same_side`: the verifier holds, or has held, an account at the hospital, or their email is in the hospital's allowed domains;
  - `same_person`: the verifier is in `covered_platform`.
- **`verifiable_by_you`** is computed by the same function.
- **The switch.** The requests and approvals for both switches.

## 9. API contract (additive to `services/core/openapi.yaml`, plus the R41a change)

All routes:

- serve the clinical audience;
- require two-step;
- take the hospital from the session;
- return errors as `{code}`;
- use opaque ids matching `[A-Za-z0-9_-]+`.

**Periods (the R41a and C3 change).**

- A new parameter, `period` (`week`, `month` or `quarter`), plus `start` (the period's first day, `YYYY-MM-DD`).
- `from` and `to` are still accepted, but only when they span exactly one frozen period. Anything else gets 422 `invalid_range`. This keeps today's client working while it moves to the picker (H6).
- `Meta.from` and `Meta.to` are the period's first and last days.
- `Meta.bucket` is the series level:
  - a quarter's series is its months (`month`);
  - a month's series is its weeks (`week`);
  - a week's series is one point, the week itself (`week`).
- **Additive: `Meta.period`** is `{level, start}`.
- **Additive: `history`** lists up to 7 earlier periods of the same level as `{start, cases, new, patients}`. Each value is guarded, and each equals that period's own frozen value. There is no total.
- A period that has closed but not yet frozen gets 409 `not_ready`.

| Method and path | Who | Notes |
|---|---|---|
| `GET /analysis/filters?period&start` | hospital admin, analyst | `{updated_at, periods, conditions, locations, specialties, sources}`. **`periods`** (additive) lists the answerable frozen periods per level, for the picker. Conditions appear only if they have 5 or more patients in that frozen period (M4), plus "Not standardised" |
| `GET /analysis/{view}?period&start&condition&age_band&sex&location&setting&specialty&source` | same | The five views, for one frozen period. Errors: 404 `not_found`, 409 `not_ready`, 422 `invalid_range`, 422 `too_many_filters` (more than 2), 429 `rate_limited`, 503 `unavailable` |
| `GET /analysis/patients?drill&level=deidentified&period&start&…` | same | At most 200 rows, plus `count`. Errors: 422 `too_small`, 422 `level_not_supported`, 429 `rate_limited`. Audited |
| `POST /analysis/patients/named` | same, when covered | §8.3. Errors: 403 `named_access_off`, `not_cleared`, `clearance_ended`; 409 `expired`; 422 `idempotency_mismatch`, `invalid`, `too_small`; 429; 503 |
| `GET /analysis/clearance/me` | same | §8.4 |
| `GET /analysis/named-access` | same | `{position, since}` |
| `GET /hospital/clearances` | hospital admin | `{clearances, today, max_end_date}` |
| `POST /hospital/clearances` | hospital admin, with step-up | Multipart, with the refusals in §8.1. 503 until the documents service exists |
| `GET /hospital/clearances/{id}`, `GET …/pages/{n}` | hospital admin | §8.1 |
| `POST /hospital/clearances/{id}/revoke` and `…/narrow` | hospital admin, with step-up | §8.1 |
| `POST /hospital/legal-resolutions` | hospital admin, covered, with step-up | §8.9 |
| `PATCH /hospital/staff/{id}` (roles) | hospital admin | Analyst rules in §6. Additive optional `mfa_code` / `backup_code`, required when adding `analyst` |
| `GET /platform/audit/{id}/patients`, `GET /platform/audit/named-access`, `/platform/audit/exports*` | platform admin, auditor | §8.8 |

**Other contract changes.**

- `User.features {analysis, named_access}`, additive.
- New error codes in `authz/errorcodes.go`: `too_many_filters`, `too_small`, `level_not_supported`, `invalid_range`, `not_ready`, `named_access_off`, `not_cleared`, `clearance_ended`, `expired`, `idempotency_mismatch`, `would_empty`, `end_date_too_late`, `not_eligible`, `not_eligible_platform`, `analysis_off`, `analyst_cap`, `above_global` and `signoff_missing`.
- **Nothing logged.** `/analysis/*` query strings are dropped from access logs and traces.

## 10. Migrations and RLS

Migration numbers are the next free ones at build time: 00019 or later. Cloud models part B may take 00019.

**Roles.** Both are created the way 00015–00017 create theirs (NOLOGIN, NOINHERIT, no members).

- **`mhai_analysis_writer`** owns the ingest functions.
  - It can SELECT only the columns ingest reads. It never sees `response_text`, `clinical_question`, `citations_json` or `medical_history`.
  - It can INSERT into the analysis tables.
  - Its ingest function refuses any hospital whose switch is Off (I5).
- **`mhai_analysis_reader`** owns the read functions, with SELECT on the analysis tables only.

**Functions.** Each is SECURITY DEFINER, has a fixed `search_path`, and takes the hospital from `mhai_current_hospital()`. Each refuses when the hospital is null or the switch is Off.

| Function | What it does | Who may run it |
|---|---|---|
| `mhai_analysis_cells(p_period uuid, p_filters jsonb, p_view text)` | aggregates over one frozen period only | `mhai_gateway` |
| `mhai_analysis_rows(p_period, p_filters, p_group jsonb)` | subject ids (at most 200) and the count | `mhai_gateway` |
| `mhai_analysis_snapshot_put(p_period, p_hash, p_body)` and `_get` | stores and reads bodies (M5). Insert only, with no overwrite, for a frozen period of the caller's hospital; checks the body's size and shape | `mhai_gateway` |
| `mhai_analysis_pseudonyms(p_subjects uuid[], p_period uuid)` | the hospital-side pids | `mhai_gateway` |
| `mhai_named_cover(p_user uuid)` | §8.2 | `mhai_gateway` |
| `mhai_grant_analyst(p_user uuid)` and `mhai_remove_analyst(p_user uuid)` | the analyst rules (§6.1). The CLI uses them too | `mhai_gateway`, and the provisioning login |
| `mhai_audit_view_pseudonyms(p_audit_id bigint)` | the audit reveal | `mhai_platform` |
| `mhai_legal_subjects(p_export_id uuid)` | subject ids for §8.9, for the caller's hospital only | `mhai_gateway` |
| `mhai_analysis_ingest(p_hospital uuid, p_until timestamptz)` and `mhai_analysis_freeze(p_hospital uuid)` | ingest and freezing | the scheduler login |

**Tables and policies.** Every table has `ENABLE` and `FORCE ROW LEVEL SECURITY`, and `REVOKE ALL FROM PUBLIC`.

- **Subjects, facts, first-seen, periods, pseudonyms and snapshots.** No grant to `mhai_gateway` or `mhai_platform`. They are reached through the functions only.
- **Dictionaries and labels.** `mhai_gateway` may SELECT them, scoped by hospital.
- **Views, view rows, notes, named-view keys and legal resolutions.** `mhai_gateway` may INSERT and SELECT them, for its own hospital only. On INSERT, `WITH CHECK` requires `viewer = current_setting('mhai.user_id')`. Notes are readable only by the export and legal functions.
- **Clearance tables.**
  - `mhai_gateway` may SELECT and INSERT for its own hospital. It updates only through functions that enforce the transitions.
  - `mhai_platform` may SELECT all, and updates only through the verify, reject and revoke functions.
  - The trigger checks the actor's kind, `same_side` and `same_person`.
- **Switches.** Only `mhai_platform` writes them. `mhai_gateway` reads its effective position through a function.

**The analyst trigger** on `user_roles` (§6.1).

**Indexes:**

- `analysis_facts (hospital_id, day)`;
- `analysis_facts (hospital_id, subject_id)`;
- `analysis_periods (hospital_id, level, start)`, unique;
- `clearance_covered (user_id) WHERE removed_at IS NULL`;
- the snapshot primary key `(hospital_id, period_id, query_hash)`.

**RLS tests.** `TestRLSWithoutHospitalSeesNothing` covers every new table, with `WITH CHECK` tests on INSERT and UPDATE.

## 11. Performance: freezing and stored bodies

**The nightly run**, per hospital whose switch is On, after its local midnight plus 30 minutes:

1. ingest;
2. sweep for retention;
3. end letters whose last day has passed;
4. **freeze** every period whose grace has run out.

**Precomputed at freeze.** For each frozen period:

- the five views with no filters;
- `filters`;
- the family parents that D5 will need.

Children are computed before parents (D3).

**Computed on first request.** Any other body is computed when first asked for and stored through `mhai_analysis_snapshot_put`. Once stored, it is served as stored for the period's 25 months (I1).

- An advisory lock per body stops two core copies computing it at the same moment.
- **The cap:** 20,000 stored bodies per hospital per period. Beyond that, a new context gets 503 `unavailable` and raises an alert. It is never computed without being stored, because that would break the freeze.
- Bodies are at most 4 MB each.

**Time budget.** A cold body, together with its family, must compute in 2 seconds for a hospital with 200,000 subjects. That is easier than before, because a request now covers one period (at most a quarter). `statement_timeout` is 5 seconds; past it, the reply is 503. The plan measures this on steven-reid before PR 4 merges.

**Co-occurring pairs.** Pairs are materialised per period when it freezes.

## 12. Failure modes

| What fails | What the user sees | What the backend does |
|---|---|---|
| The nightly ingest or freeze fails | the period picker doesn't offer the new period yet | Frozen periods keep being served. Alert `analysis_freeze_failed`. Never a partial period |
| A body runs over its time budget, or a hospital reaches its body cap | "Couldn't load {view}" | 503. Nothing stored. Metric and alert |
| The engine sees inconsistent inputs | that view fails | Everything in it is hidden, and an alert is raised |
| Late facts arrive for a frozen period | nothing changes | Stored with `period_late` and counted in a metric |
| A request for a closed period that isn't frozen yet | not offered | 409 `not_ready` |
| A drill key from another period, hospital or filter set | 422 `too_small` | The HMAC binds all of them |
| Two pids collide | nothing | The pid is drawn again |
| The documents service is down | the letter stays at "Checking the file" | 503. Nothing stored |
| The identity service is down | "Couldn't show names" | 503, and `analysis.named_view_failed` |
| An audit append fails | the request fails | The transaction rolls back: no rows, no names |
| A letter is revoked, the switch is lowered, or the role is removed while names are on screen | names disappear within 30 seconds, or at once on the next call | `clearance/me` changes, and the named route refuses |
| A named view is replayed after `named_until` | the view can't be reopened | 409 `expired` |
| The hospital's time zone changes | the next periods use the new day boundary | Frozen periods keep their own boundaries |
| A core copy's clock is off | nothing | Days come from the database's `now()`, in the hospital's time zone |

## 13. Security and threat model

### 13.1 Assets

- patient-level clinical facts, per subject;
- the map from subjects to pids;
- names and MRNs, at the moment of viewing;
- the letters;
- the secrecy of small groups in aggregates;
- hospital separation;
- the audit trail;
- the switches and their sign-off references;
- availability;
- clinical safety: nothing from Analysis may reach an answer.

### 13.2 Actors and trust boundaries

**Actors**

- hospital admins, analysts, and clinicians without the role;
- platform admins and auditors;
- the BFF, core, the scheduler, the EMR service, the documents service, NATS and Postgres;
- a person who holds both a hospital account and a platform account;
- anyone who gets hold of a screenshot or a CSV.

**Boundaries, and what is checked at each**

1. **Browser → BFF.**
   - The session cookie.
   - The sign-off keys, which can only refuse.
   - Named views must carry a key.
   - Query strings are not logged.
2. **BFF → core (NATS `api.v1.clinical`).**
   - The token's signature, audience and two-step status.
   - Roles, cached for up to 30 seconds.
   - The route table.
   - The hospital comes from the token only.
3. **Core → Postgres.**
   - Core runs as `mhai_gateway`, with `mhai.hospital_id` and `mhai.user_id` set from the token.
   - RLS applies everywhere.
   - Analysis data is reached only through definer functions.
   - On patient-level routes, cover and role are re-read inside the transaction.
4. **Core → documents service (NATS).** A core-signed token. Outcomes come from a closed set.
5. **Core → EMR service (NATS).** A core-signed, single-use token valid for 30 seconds. The reply must contain exactly the references asked.
6. **Scheduler → Postgres.** Through the ingest and freeze functions only.

### 13.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| AN1 | Information disclosure | A small group worked out from one response | Layer A | P1, T1–T3 |
| AN2 | Information disclosure | **Differencing over time:** overlapping windows, month-edge days, the start of the hospital's data, zeros, late data | Finished, nested periods (D1); freezing (D2); guarding across levels (D3); no grand totals | P3, P4, T4 |
| AN3 | Information disclosure | **Differencing across filters and views** | Cap of 2 and `QuerySetMin` (D4); family fixpoint (D5); budgets and audit (D7); residual R-A1 measured | P2, T5, T6 |
| AN4 | Information disclosure | Revealing attributes through a narrow view or the filter list | Empty lists under A7; the filter list per frozen period with at least 5 patients | P6, T7 |
| AN5 | Information disclosure | **Profiling through drill-downs** | Pids per period; rows limited to the period; caps on distinct patients and on lists; every list audited | P8, T8, T9 |
| AN6 | Information disclosure | **Drill-down users counting small subgroups** | Accepted by design and stated (§5.2); role, audit and caps; H1 confirms | P2 (drill run) |
| AN7 | Information disclosure | Reading across hospitals | Hospital from the token and from `mhai_current_hospital()`; drill keys bound to the hospital; 404 for other hospitals | T10 |
| AN8 | Elevation of privilege | Names without cover | `mhai_named_cover` inside the transaction, nothing cached; switches only writable by the platform | T11–T14 |
| AN9 | Elevation of privilege | Self-verification, or a hospital user verifying | Plan 11b, plus the trigger (`same_side`, `same_person`, actor kind) | T15 |
| AN10 | Elevation of privilege | Self-granting the analyst role, or granting across hospitals | Trigger on `user_roles` (§6.1); step-up; CLI through the same function | T26 |
| AN11 | Information disclosure | Names stored (idempotency, snapshots, logs, audit) | A separate key table; identity resolved after commit and dropped; closed audit schemas | T16, T17 |
| AN12 | Repudiation | A named view or legal resolution that can't be accounted for | Audit before identity; view rows; replays counted | T18 |
| AN13 | Tampering | A body naming another user, hospital or letter | The server picks the letter; `WITH CHECK` on the viewer | T19 |
| AN14 | Spoofing | A forged reply from the identity or documents service | Signed tokens; reply subjects checked; NATS accounts split before production | T20 |
| AN15 | Tampering | A malicious letter file | Core never parses it; the documents service sandboxes it; only flat PNGs are shown | (documents design) |
| AN16 | Denial of service | Expensive bodies, or floods of requests | Rate limits; time budget; the body cap fails closed; families of at most 4 | T21 |
| AN17 | Information disclosure | Analysis data feeding clinical answers | No grant to the worker roles; no NATS operation exposes it; the SKILLS.md amendment and checklist line | T22 |
| AN18 | Information disclosure | Someone with accounts on both sides differences the hospital's own Analysis against Plan 12 | Same periods and frozen inputs; pids differ by side; `same_side`; residual R-A3 | T15 |
| AN19 | Elevation of privilege | A stale role after removal | Patient-level routes re-read the role | T13 |
| AN20 | Information disclosure | Pids leaking through audit or ids | Random pids per period; C22 holds outside `/{id}/patients` | P7, T23 |
| AN21 | Tampering | Direct writes to facts, snapshots or switches | No table grants; definer functions only | T24 |
| AN22 | Information disclosure | Ingesting hospitals that never agreed | Ingest only with the switch On | T25 |
| AN23 | Elevation of privilege | The demo identity fixture running in live mode | It refuses to start outside the demo configuration | T27 |

### 13.4 Controls (decided)

- **Authentication and step-up.**
  - Two-step on every route.
  - Step-up for: letter upload, narrow and revoke; the analyst grant; legal resolution; every switch action.
- **Authorisation.**
  - The route table.
  - On patient-level routes, role and cover re-read inside the transaction.
  - Hospital scope enforced in code and in every definer function.
  - `FORCE` RLS on every table.
  - The analyst rules and the letter rules enforced by triggers.
- **Data handling.**
  - Facts are coded, in bands, and at day level.
  - No names or MRNs are stored.
  - Identity is resolved per view, then dropped.
  - Pids are random and last one period.
  - Bodies hold guarded output only and never change once frozen.
  - Retention follows §4.3.
- **Concurrency.**
  - Bodies are stored once, then read back, under an advisory lock.
  - Named-view keys keep the original `named_until`.
  - Letters carry a `version`.
  - Pids are unique per period.
  - Freezing runs once per period.
- **Secrets.** These core secrets live in files, are refused at startup if missing or placeholders, and are rotated yearly:
  - the drill-key HMAC key;
  - the `ref_hmac` and `ref_enc` keys;
  - the note-encryption key.
- **Third parties.** No new ones.

### 13.5 Abuse-case tests (the plan writes these first)

- **T1** Female 212, male 3, total 215: male and the next-smallest member are hidden.
- **T2** A month's hidden blocks can't be recovered from the month's total.
- **T3** A pair with a hidden "both" comes after every shown pair.
- **T4** A request for any range that isn't exactly one period gets 422. A frozen period's body stays byte-identical after late facts arrive. A quarter whose single hidden month the other two months would give away is hidden.
- **T5** Asking for a complementary cell through a filter: the request is answered hidden whole.
- **T6** A third filter gets 422 `too_many_filters`.
- **T7** A filter set matching 7 patients: everything is hidden and every list is empty.
- **T8** The 51st list in a day, and the 301st distinct patient in a period, each get 429 and a flagged entry.
- **T9** Rows carry no condition from outside the period, and at most 10.
- **T10** Another hospital's drill key, letter or page gets 422 or 404.
- **T11** At "Letters only", a named request gets 403 `named_access_off`.
- **T12** After a revocation elsewhere, the next request gets `not_cleared` and `revoked`. At the hospital's midnight, `clearance_ended`.
- **T13** With the role removed, refusal comes before the 30-second cache expires.
- **T14** Two-step reset or deactivation gets `role_or_account`, at once.
- **T15** Verification attempted by a hospital user, a linked platform admin, or a covered platform admin: the trigger refuses it, even in SQL.
- **T16** After a view and its replay, no stored row or log line holds the synthetic names.
- **T17** Logs, spans and dead letters from named views and legal resolutions hold no name, MRN, pid or note.
- **T18** When the audit append fails, no rows are returned and identity is never called.
- **T19** A clearance id, hospital or viewer in the body is ignored or refused.
- **T20** An identity reply with an extra reference, or on the wrong subject: the view fails with 503.
- **T21** 20 parallel cold requests for one body produce one computation and one stored row. Over the cap gives 503, and nothing is computed unstored.
- **T22** As `intelligence_app` and `mhai_worker`, every analysis table is denied.
- **T23** No audit or export response holds a pid, except `/{id}/patients`.
- **T24** As `mhai_gateway`, direct writes to facts, subjects, pids, snapshots and switches are denied.
- **T25** A hospital with the analysis switch Off gets no facts after the nightly run, and calling the ingest function for it fails.
- **T26** An analyst grant to oneself, across hospitals, without step-up, without `clinician`, or past the cap (10) is refused by the trigger and the route. The puppet account's grant notifies every hospital admin.
- **T27** The demo identity fixture refuses to start with live configuration.

### 13.6 Residual risk and sign-off

- **R-A1: differencing across filter families within one period.** The same group reached from two families that don't overlap. P2 measures how often, exhaustively at a cap of 2. The privacy lead accepts it with that number (H1).
- **R-A2: narrowing by bounds.** Bounds within responses (Plan 4c's residual), and distinct counts across levels (largest child ≤ parent ≤ sum of children). Measured by P5.
- **R-A3: two releases about one hospital.** The hospital's own Analysis and Plan 12, which use different filter granularities. Only a person with both kinds of account can combine them.
- **R-A4: drill-down users see microdata.** Their counts are exact, and they can count small subgroups (§5.2). The controls limit and record this but don't prevent it. **This is the central privacy question.**
- **R-A5: the screen itself.** Screenshots.
- **Sign-off needed:**
  - **Clinical:** the definitions, the confidence mapping, and whether admins may see this at all.
  - **Privacy:** H1.
  - **Legal:** NDPA basis, hospital agreements, letters, the legal resolution path.

  Each goes into TODO.md in the plan's first PR.

## 14. Phased PRs

Each phase is its own PR, reviewed and merged by a person. Phases 2–6 run on the demo hospital only, because ingest needs the switch On and only the demo has it (I5). That means **phases 2 to 6 need no sign-off**. Nothing goes live without H1–H3.

| PR | Contents | Depends on |
|---|---|---|
| **0** (this one) | this design | none |
| **1. Decisions on paper** | ADR 0014, "Population analysis under disclosure control", **Proposed until H4**. It changes the "admins see no clinical content" rule for aggregates and pseudonymous rows. It also proposes the SKILLS.md §1 amendment, **Proposed until H4**: the cross-patient exception applies only to `services/core/internal/analysis` (aggregates and drill-down); the rule stays absolute for intelligence, retrieval, prompts, models and every NATS operation; and the review checklist gains "Does any analysis table, function or result reach a worker role or a NATS subject? (T22)". Also: the frontend-requirements §10 updates (H6) and the TODO.md sign-off items | none |
| **2. Data foundation** | The migration: subjects, facts, first-seen, periods, dictionaries, pids, the analysis switch, the roles, and the ingest and freeze functions, with the switch-On check. `features`. The scheduler. `core analysis seed-demo`. RLS tests. T22, T24, T25 | none |
| **3. Disclosure engine** | `internal/analysis/disclose`: Layer A, the period hierarchy (D1–D3), the family fixpoint (D5). P1–P8. Reviewable by the privacy lead | none |
| **4. Aggregate routes** | `GET /analysis/filters` and `/analysis/{view}` with `period`, freezing and stored bodies, rate limits, `analysis.viewed`, the unusual-query flag, openapi. T1–T7, T21. The time budget measured | 2, 3 |
| **5. De-identified drill-down and reveal** | `GET /analysis/patients`, drill keys, row limits, caps, pids per period, `GET /platform/audit/{id}/patients`. T8–T10, T23 | 4 |
| **6. Analyst grant** | The trigger and functions, step-up on the PATCH, the CLI through the function, the cap, the notice. T26 | 2; the staff endpoints |
| **7. Letters, hospital side** | The clearance tables and trigger, the routes, ending, reminders. T15 (trigger) | the documents service (live); fixture pages (demo); the Plan 11b design |
| **8. Named access** | `mhai_named_cover`, the named route, the key table, notes, `clearance/me`, `/named-access`, the identity call, the demo fixture with its live refusal. T11–T14, T16–T20, T27 | 7; the EMR service (live) |
| **9. Audit list, exports and legal resolution** | `GET /platform/audit/named-access`, exports, `POST /hospital/legal-resolutions` | 7, 8; object storage; H3 |
| **10. EMR facts** | Findings, alerts, settings, locations and sources | the EMR plan; notifications |

## 15. What's needed from people

| # | Who | What, exactly | Needed by |
|---|---|---|---|
| H1 | Privacy lead | **Confirm the approach.** (1) Exact counts, no noise, and the aggregate rules protect against people without drill access, while drill users see pseudonymous microdata under role, audit and caps (§5.2, R-A4). (2) The C3 choice: month-aligned week blocks, months and quarters, nested and guarded across levels (D1–D3). (3) The filter residual R-A1, accepted with the number P2 measures at a cap of 2, and R-A2 with P5's number. (4) The constants: `Min` 5, `QuerySetMin` 10, `FilterCap` 2, 3 days' grace, pids per period, the distinct-patient cap of 300, the list and named-view budgets, retention. | PR 3 review (constants); before any live hospital (sign-off) |
| H2 | Clinical lead | Sign-off that hospital admins and analysts may see the five views; the definitions ("case", "new", "existing"); the confidence mapping (§4.1); condition grouping. | Before any live hospital |
| H3 | Legal / compliance lead | NDPA 2023 basis for this secondary use; the clause in hospital agreements; the letter as authority (a template); retention of letters and audit; **the legal resolution path (§8.9)** as the way to answer a request that needs names. | Before any live hospital; the template before PR 7 goes live; §8.9 before PR 9 |
| H4 | Product owner | Approve this design and the open questions; move ADR 0014 and the SKILLS.md amendment from Proposed to Accepted; name the pilot hospital; confirm Analysis waits for patient references. | PR 1 |
| H5 | Platform and operations | Object storage and the documents service; the scheduler slot; alerts for `analysis_freeze_failed` and the body cap; `SECURITY_CONTACT_*`. | PR 7 live; PR 9 |
| H6 | mhai-web team | **Contract changes forced by R41a and C3.** (a) **The period picker replaces the presets and Custom range** in Analysis: choose a level (week, month, quarter) and one finished period from `filters.periods`, and send `period` and `start`; "week" means the month-aligned block, labelled with its dates ("8–14 Mar 2027"). (b) Show `Meta.period`, and draw `history` as a trend with no total. (c) **Plan 12's "Last 3/6/12 months" changes** to the last full month or quarter, plus the last four quarters as history. (d) **Pids become stable per period**, not per hospital: the copy and the Plan 6 and Plan 12 contracts say so, and nothing in the app links pids across periods. (e) Handle 409 `not_ready`, 422 `too_many_filters` (more than 2) and 429 `rate_limited`. (f) Accept empty lists next to a hidden patient count (A7). (g) Send `mfa_code` when adding Analyst in the staff panel. (h) The legal resolution screen (§8.9). (i) Ship the audit kinds in §8.7 and the `analyst` actor role. | Before PR 4, PR 6 and PR 8 go live |
| H7 | Intelligence lead and the EMR design owner | Consultations carry a hospital-scoped patient reference; canonical EMR events go into core; the `emr.v1.identity.resolve` operation. | PR 10; PR 8 live |

## 16. Open questions, each with a recommendation

1. **Exact counts or noise?** *Decided: exact counts, no noise.* R41a's fixed periods remove differencing over time. Noise would be undone by exact drill counts anyway (§5.2). Differencing across filters is measured and accepted (R-A1, H1).
2. **Count consultations that have no patient reference?** *No.* Build on synthetic subjects, and go live once references exist.
3. **`QuerySetMin` for filtered views?** *10.*
4. **Filter cap?** *Decided: 2.*
5. **How are periods chosen?** *A period picker* over finished, nested periods (H6a), replacing the presets and Custom.
6. **When are periods computed, and how does the filter list behave?** *Freeze each period three days after it closes, and compute the filter list per frozen period* (D2, M4). Late data never changes a frozen period.
7. **What does a de-identified row show?** *Conditions and findings within the period only, at most 10 of each.*
8. **How stable are pids?** *Stable per period* (and per side and scope). Lists can't be linked across periods.
9. **Drill-down budgets?** *10 lists a minute and 50 a day per person; 300 distinct patients per person per period; 5 named views a day, replays included.*
10. **How long does a named view last?** *30 minutes, or until the letter's end if that comes first.*
11. **Controls on granting Analyst?** *Step-up when adding the role (an optional `mfa_code`, required for this change), every rule enforced in a database trigger and shared with the CLI, a cap of 10 per hospital, and hospital admins notified.*
12. **A per-hospital switch for aggregates?** *Yes: two people to raise it, with sign-off references.* Ingest follows the switch.
13. **Audit every aggregate read?** *Yes:* once per session, filter set and period, unflagged.
14. **How are legal requests that need names answered?** *Through the legal resolution path (§8.9):* a covered hospital admin, with step-up and a legal reference, resolves an export's rows in the app. Flagged and limited. Never a named file. Platform staff never see names. The earlier answer ("the hospital resolves pids itself") doesn't work, because pids are random and per period.
15. **Upload letters before the documents service exists?** *No:* 503 until then.
16. **How is SKILLS.md §1 amended?** *Through ADR 0014, scoped to `services/core/internal/analysis`.* The rule stays absolute for intelligence and every NATS operation, with a checklist line tied to T22.

**Relation to R41a.** The product owner's R41a ruling (fixed periods), refined by C3 (nested periods), gives Analysis differencing control over time: finished, frozen periods, guarded together across levels. What remains is differencing across filters within one period (R-A1). It is measured, not closed, and the privacy lead accepts it in H1.
