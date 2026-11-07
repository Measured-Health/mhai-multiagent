# Patient Analysis, named access and clearance letters (hospital side): backend design

- **Status:** Draft for review (2026-11-07). Design only, no code. **Live use needs clinical, privacy and legal sign-off** (§15): this design changes the rule that hospital admins see no clinical content, and its disclosure control depends on the same privacy ruling as R41a (§5.4).
- **Date:** 2026-11-07
- **Covers:** the backend for frontend contract §10 (Analysis, Plan 6), named access in Analysis (Plan 11c: `POST /analysis/patients/named`, `GET /analysis/clearance/me` with `reason`), the hospital side of clearance letters (Plan 11a), and the deferred audit-log parts that depend on them (pseudonym reveal, the named-access list and exports; contract §14.9).
- **The contract is mhai-web's** (read-only for this repo):
  - `mhai-web/docs/specs/2026-10-17-analysis-design.md` (§9 the assumed contract) and `docs/plans/2026-10-22-plan-6-analysis.md`;
  - `mhai-web/docs/specs/2026-10-22-clearance-letters-design.md` (§10) and `docs/plans/2026-10-22-plan-11-clearance-letters.md` ("Rulings made while planning", Task 13);
  - `mhai-web/apps/clinical/lib/api/analysis-contract.ts`, `clearances-contract.ts` and `lib/analysis/guards.ts` (where the spec and the code differ, the code is right);
  - for consistency: `docs/specs/2026-10-22-platform-analysis-design.md` (Plan 12; present on the `mhai-web-cloud` branch, not yet on mhai-web `main`).
  - In this repo: `docs/specs/2026-10-17-frontend-requirements.md` §3, §5.1, §10 and §14.9.
- **Builds on (and must stay consistent with):**
  - the Plan 4 design (`2026-10-31-plan-4-feedback-audit-documents-admin-design.md`) and Plan 4c as built: `internal/smallgroup` (the count, set, share and across-rows rules, the bridge check and property test C20), whole UTC days and stored snapshots (rulings R42, R43, `guardrail_snapshots`, migration 00017), the audit catalogue's pseudonym test (C22);
  - Plan 2: RLS by `mhai_current_hospital()`, the role split (`mhai_gateway`, `mhai_platform`, `mhai_auth`), the audit hash chain, step-up (`auth.Service.StepUpAudited`), `Idempotency-Key`, the principal cache (`authz.MaxCacheTTL`, 30 s);
  - ADR 0004 (retrieval scoped to one patient), ADR 0009 (many EMR systems, canonical model, patients matched within a hospital), ADR 0010 and 0011 (Go owns rules; BFF over NATS), ADR 0012 (additive changes only in `v1`).
- **Not in this design:** the platform side of clearances and the named-access switch (contract §14.10, Plan 11b; outlined in §8.9 only as far as the hospital side needs it), platform Analysis across hospitals (Plan 12, contract §14.12; §5.7 says what it must reuse), the EMR integration service, the documents service, the notifications service.

## 1. Why

mhai-web built the Analysis page (five views, filters, a de-identified drill-down, the Analyst role) and the clearance-letter workflow on sample data, behind `ANALYSIS_SIGNED_OFF` and `NAMED_ACCESS_SIGNED_OFF`. The product owner wants population and disease analysis that goes beyond Usage counts, for hospital admins, analysts and (later, across hospitals) platform admins.

Three things make this harder than Plan 4c's guardrail statistics:

1. **It is clinical content shown to people who today see none.** Hospital admins are not clinicians. The rule "hospital admins see no clinical content" (contract §3) is changed only for aggregates and pseudonymous rows under disclosure control, and for names only under a verified clearance letter.
2. **The queries are flexible.** Any day range up to 24 months and up to seven filters. Plan 4c had three fixed periods and still left differencing across periods open (R41a). Flexible queries make differencing the main threat, not a corner case.
3. **The drill-down is patient-level microdata.** A de-identified row (pseudonym, age band, sex, setting, conditions, findings) is still about one person.

This design says where the data comes from, what is stored, the disclosure rules and how they are tested, the role and its grants, the clearance and named-access flow, the API, the database, performance, failure modes, the threat model and the build order.

## 2. Goals, non-goals, anti-goals

**Goals**

- Serve contract §10 exactly (and additively where §9 proposes changes): `GET /analysis/filters`, `GET /analysis/{view}` for the five views, `GET /analysis/patients` (de-identified), `POST /analysis/patients/named`, `GET /analysis/clearance/me`, `GET /analysis/named-access`, and the hospital clearance routes.
- Disclosure control **at least as strong as Plan 4c** on every response, plus rules against differencing across periods and filters, stated as properties with a proof sketch and property tests.
- One computation path in Go and Postgres: the hospital from the session (and in the database, from `mhai_current_hospital()`), raw counts never leaving core, no analysis in Python (ADR 0010 rule: security rules once, in Go).
- Stored, frozen daily inputs, so the same question gets the same answer all day on every core copy (R42/R43 generalised).
- The `analyst` role: granted by a hospital admin, only with `clinician`, audited, effective and revocable at once.
- Named access: a verified, current letter covering the person, the switch On, a purpose, every view audited and flagged, names never stored, logged or cached by us.
- The deferred audit parts: pseudonym reveal (`GET /platform/audit/{id}/patients`), the named-access list and the legal exports.

**Non-goals**

- Statistical inference (confidence intervals, significance), forecasting, free-form queries or SQL.
- Any patient-level view across hospitals (Plan 12 forbids it).
- Matching one person across hospitals (ADR 0009).
- Judging a letter's legal sufficiency (Plan 11 §2).
- Feeding anything from Analysis into retrieval, prompts, models or the guardrails.

**Anti-goals: failure even if it works**

1. **A small group worked out** (a count of 1–4) from one response, from several responses (periods, filters, views), from a response and its CSV, or from a response and the drill-down count.
2. **A name or MRN served** without a verified, current letter covering the caller, with the switch below On, or after revocation, expiry, a role change or deactivation.
3. **Names or pseudonymous rows persisted** by us outside the mapping and audit tables: in logs, traces, the idempotency store, a snapshot, an audit payload or a NATS message other than the one identity call.
4. **Cross-hospital leakage:** one hospital's figures, pseudonyms or letters reachable from another hospital's session, or a hospital id taken from a request.
5. **Analysis data reaching a clinical answer:** any path from the analysis tables into retrieval, a prompt or a model (SKILLS.md §1).
6. **Different answers to the same question** on two core copies or at two times on the same day.

## 3. Who uses it

| Who | Analysis aggregates | De-identified drill-down | Names | Letters |
|---|---|---|---|---|
| Hospital admin (MFA) | own hospital | own hospital | only when covered by a letter, switch On | upload, view, narrow, revoke, renew |
| Clinician with `analyst` (MFA) | own hospital | own hospital | only when covered, switch On | none |
| Clinician | no | no | no | no |
| Platform admin | no (Plan 12, its own routes) | no (Plan 12b, its own route, under a letter naming them) | never | verify, reject, revoke (Plan 11b) |
| Auditor | no | no | no | read, view letters; audit reveal and exports |

- Every route checks the role against the database inside the request transaction for the patient-level routes (§6.3), not only the 30-second principal cache.
- Analysis routes are clinical-audience only. Platform accounts are refused (403 `forbidden`), as contract §10 says.

## 4. Data sources and what is stored

### 4.1 Sources

| Source | Gives | When |
|---|---|---|
| **MHAI consultations** (`clinical_queries`, `clinical_decisions`, `feedback_events`) | the assessed condition (`diagnosis`, `icd10_code`), age (→ band), sex, specialty, urgency, guardrail decision (→ confidence level), review outcome (agreed, corrected), correction outcome | now, **but see §4.2** |
| **EMR records** via the EMR integration service (ADR 0009 canonical model) | recorded diagnoses, lab results with flags, setting (admitted, outpatient, emergency), location (ward or clinic), source label | with the EMR plan |
| **Patient alerts** (Plan 5a, notifications and EMR) | findings, severity, acknowledgement time, dismissal reason | with the EMR plan and the notifications service |

Mapping from consultations: confidence `high` = `PASS` with trust band high, `caution` = `PASS` otherwise, `review` = `REGENERATE` or escalated with an answer, `failed` = escalated or degraded with none (the exact mapping is a clinical sign-off item, H2). Agreement: a clinician rating recorded as "agree" over all rated consultations. Corrections: `feedback_events` with `pending_review`, `approved`, `rejected` (withdrawn excluded).

### 4.2 The counting unit: a patient key (a finding that shapes the phases)

The contract counts **patients** (`Meta.patients`, "new" versus "existing", co-occurring pairs, the drill-down rows). **Today a consultation carries no patient identity.** `clinical_queries.patient_data_json` holds age, sex, symptoms and history, and no patient id; adding one is deferred to the EMR plan (TODO.md, "Patient ids in questions and tokens", plan departure 7).

So:

- A consultation is counted only when it carries a hospital-scoped **patient reference** (the EMR patient index id, or a consultation `pid` once questions carry one). Consultations without one are not counted as patients and are not guessed into one (§16 Q2).
- Until patient references exist, live Analysis has nothing honest to show. The backend is built and demonstrated on **synthetic subjects** (a seeder for the sample hospital, §14), as "build until weights" asks. The switch stays Off for real hospitals regardless (§7).

### 4.3 What is stored

All in core's database, under RLS, written only by core.

| Table | Holds | Never holds |
|---|---|---|
| `analysis_subjects` | `hospital_id`, `subject_id` (random uuid, internal only), `ref_hmac` (HMAC-SHA256 of the patient reference with a core key, for matching), `ref_enc` (the reference encrypted with a core key, used only for the identity call, §8.6), `rkey` (a random 32-bit record key, §5.4), `created_day`. The reference is the EMR service's own opaque patient-index id (or a consultation `pid`), never an MRN | an MRN, a name, a date of birth, the reference in clear |
| `analysis_facts` | one row per event: `hospital_id`, `subject_id`, `day` (the hospital's day), `kind` (`consultation`, `diagnosis`, `finding`, `alert`), `condition_id`, `age_band`, `sex`, `setting`, `location_id`, `specialty`, `source_id`, and the kind's own coded fields (confidence, rated, agreed, correction state; finding id and flag; severity, ack minutes, dismissal reason), `ingested_day` | free text, question or answer text, exact age, exact time of day, any identifier |
| `analysis_first_seen` | `(hospital_id, subject_id, condition_id) → first_day`, for "new" versus "existing" | anything else |
| `analysis_conditions` | the canonical condition dictionary (`id`, `icd10`, `name`); one "Not standardised" entry per hospital and source (name null) | local free-text names from a source |
| `analysis_locations`, `analysis_sources` | per hospital: opaque id, the hospital's own label, the setting | vendor names (ADR 0009) |
| `analysis_pseudonyms` | `hospital_id`, `subject_id`, `side` (`hospital`, `platform`), `scope` (null for the hospital side; the platform admin and letter for Plan 12b), `epoch`, `pid` | anything derivable from a reference |
| `analysis_snapshots` | the guarded response bodies of a hospital day (a cache, §11) | a raw count |
| `analysis_views`, `analysis_view_rows` | one row per drill-down (de-identified or named): viewer, level, snapshot day, filters (codes), drill, count, purpose, clearance id, `named_until`, audit id; and the subject ids of the rows sent, in order | names, MRNs, pseudonyms (derived on reveal) |
| `named_view_notes` | the purpose note for "Other" (≤ 200 characters), encrypted at rest with a core key, linked to the view | anything in an audit payload or log |
| `clearances`, `clearance_covered`, `clearance_events`, `named_access_switch`, `named_view_keys` | §8 and §10 | the letter's bytes (object storage, §8.1) |

**Pseudonyms** (`^P-[A-Z0-9]{4,12}$`, contract §10 and §14.9): `P-` plus **8 characters of the RFC 4648 base32 alphabet** (A–Z, 2–7), **random, not derived**, drawn per `(hospital, side, scope, epoch, subject)` and kept in `analysis_pseudonyms` with a unique index per `(hospital, side, scope, epoch, pid)`; a collision (40 bits: about 0.5% likely at 100,000 subjects in one hospital) is a unique-violation retry. Random rather than an HMAC of the patient reference, so a leaked key can't let anyone who knows an MRN compute the pseudonym; the table is the only map, behind RLS and definer functions. No other id this service emits may match the shape (C22 stays true: audit cursors, export ids and drill keys use other alphabets or lengths, §5.6 P7).

**Retention:** facts and first-seen rows are kept 25 months (the 24-month maximum range plus a month) and swept nightly; snapshots 2 days; views, view rows, notes and pseudonym rows for as long as the audit records that cite them (the retention plan; legal holds win). Letters at least as long as the audit records citing them (Plan 11 Q5).

### 4.4 How data gets in

- **Consultations:** a nightly ingest per hospital (core, role `mhai_analysis_writer`, §10) reads decisions and feedback committed since its watermark, keeps those with a patient reference, and writes facts. No worker change; the intelligence boundary is untouched.
- **EMR and alerts (later):** the EMR service publishes canonical, coded events to core (subject to the EMR design); core maps them to facts in the same nightly run. Identity (name, MRN) **stays in the EMR service**; core gets only the hospital-scoped reference, which it HMACs.
- **Frozen inputs:** facts are written only by the nightly run. A day's answers are a pure function of the facts ingested before that day's cutoff (§5.3 D1), which is what makes them repeatable without trusting a cache.

## 5. Disclosure control

Two layers. Layer A guards each response on its own; it is Plan 4c's rules applied to the Analysis views. Layer D guards against putting responses together.

### 5.1 Constants (Measured Health's, held by the backend)

| Name | Value | Meaning |
|---|---|---|
| `Min` | 5 | `smallgroup.Min`: a count of 1–4 is hidden |
| `QuerySetMin` | 10 (proposed, §16 Q3) | a filtered view whose patient count is under this is hidden whole |
| `FilterCap` | 3 (proposed, §16 Q4) | non-date filters per request; more is 422 `too_many_filters` |
| `MaxRange` | 24 months | contract §10 |
| `DrillMax` | 200 rows | contract §10 |
| noise table | §5.4 (if ruled) | bounded, zero-mean, consistent |

### 5.2 Layer A: each response

Every number goes through `internal/smallgroup` or a new `internal/analysis/disclose` built on it. Raw counts never reach a response writer.

- **A1 Count.** 0 is 0; 1–4 hidden; 5 and up shown (`smallgroup.Count`). Applies to every count: patients, cases, consultations, results, alerts, reviews, corrections, pairs.
- **A2 Sets.** Every set that adds up keeps rule 2 (`smallgroup.Set`, `Within`): never exactly one member hidden; with the total shown, hidden members total 0 or at least 5; a hidden total hides any non-zero member. The sets are exactly the ones `guards.ts` checks (contract §10 list), plus two the app doesn't name:
  - **series "new" against total "new".** A case is new in exactly one bucket, so `Σ series[i].new = total.new`. The series' new column and `total.new` form one set. (The app checks each point alone; the server must check the column.)
  - **by-condition rows against the total row.** `by_condition` lists **every** condition with a case in the filtered set (no truncation, "Not standardised" included), so its `cases`, `new`, `existing` columns sum to `total`. Rows × {cases, new, existing} with the total row is a table, guarded by `smallgroup.AcrossRows` (rows, columns and the bridge check).
- **A3 Partitions of patients.** `by_age`, `by_sex`, `by_setting`, `by_location`, `by_specialty` each partition `patients` completely (null and "not recorded" rows included), each guarded as its own set against `patients`. They share only the `patients` cell, so if `patients` is shown each partition's equations are independent of the others (§5.5 lemma 2).
- **A4 Shares, rates, medians.** `smallgroup.ShareOf`: hidden when the numerator, the denominator or the complement is 1–4, or either count is hidden; sent with four decimal places (`ShareDigits`), never rounded further (the app's `numeratorLeaks` check needs the precision). A median over fewer than 5 values, or over a hidden count, is hidden; a median is whole minutes or a half.
- **A5 Pairs.** For each co-occurring pair, `a_patients` against "both" is a set; `share_of_a` is `ShareOf(both, a_patients)`. A pair whose "both" is hidden never ranks in the top 20 by its true size: the top 20 are chosen by **shown** value, then hidden pairs in name order (contract: "rows never reveal rank").
- **A6 Order.** Shown rows by value; hidden rows after them in a fixed order (by name, "Not standardised" and unknowns last). Fixed-order dimensions (age, sex, setting) in their fixed order.
- **A7 Whole view.** When `patients` is hidden, or under `QuerySetMin` with any non-date filter set, every number in the view is hidden (zeros included), no row has a `drill`, and **lists are sent empty** (no condition, finding or pair names), so a view of 1–9 people can't say which conditions they have (§16 Q6; the contract today lists rows with hidden values, so this needs mhai-web to accept empty lists beside a hidden total: H6).
- **A8 Drill keys.** `drill` is set only on a shown count of 5 or more. It is an opaque token (`[A-Za-z0-9_-]+`, 43 characters of base64url, so never pseudonym-shaped): an HMAC over `(hospital, snapshot day, effective range, filters, group)` with a core key, bound to the session's hospital. `GET /analysis/patients` re-derives the group from the token and the filters, recomputes the count from the same snapshot, and answers 422 `too_small` unless that count is shown and ≥ 5.
- **A9 No repeated keys.** No row repeats its key (condition, band, bucket, pair, finding name and flag).

These are the rules Plan 4c proved per response. Layer A on its own leaves the cross-query attacks below open; that is why Layer D exists.

### 5.3 Layer D: across responses

The attacks, each concrete:

- **Sliding periods.** `[d1, d2]` minus `[d1, d2−1]` is one day's count, often 1–4. (R41a's "the day that entered minus the day that left".)
- **Late data.** The same range asked on two days differs by the records ingested in between.
- **Complementary cells by filter.** In the unfiltered `affected` view, `by_sex` hides "male" (true 3) and, by rule 2, "other" (true 7) to protect it. Ask `sex=other`: its `patients` is 7, shown. Then male = patients − female − not recorded − 7 = 3. Any cell hidden only to protect another can be asked for directly as a filter, and once it is known the primary cell falls out by subtraction.
- **The same group in two contexts.** `{age=80+}`'s `by_sex.male` and `{sex=male}`'s `by_age.80+` are the same patients. Each response guards it in its own sets; one may hide it where the other shows it.
- **Views against each other.** `prevalence.by_condition[a].cases` and `co-occurring` `a_patients` count the same patients.

Rules:

- **D1 Frozen whole days.** Ranges are whole **hospital days** (`hospitals.time_zone`). The latest day served is yesterday; a request reaching today is clamped. Each hospital day's answers come from the facts ingested before that day's cutoff (the hospital's midnight plus a 30-minute grace); until the cutoff the previous day's inputs serve. Every core copy gives the same answer all day (R42 and R43, per hospital time zone).
- **D2 Whole buckets.** `from` and `to` snap outward to whole buckets: ISO weeks (Monday to Sunday, hospital time) when the snapped range is 13 weeks or less (`bucket: week`), calendar months beyond (`bucket: month`). The response's `from` and `to` are the **effective** range (the app already reads them from `Meta`). A one-day slide no longer changes the answer; the smallest difference between two answerable ranges is a whole week or month. Still over 24 months after snapping: 422 `invalid_range`.
- **D3 Filter cap and query-set minimum.** At most `FilterCap` non-date filters; a filtered view under `QuerySetMin` patients is hidden whole (A7). This removes the narrowest trackers and bounds the number of contexts a group can be reached from.
- **D4 Hide once, hide everywhere (within a request's family).** A request's family is its own context `(range, F)` plus each parent `(range, F \ {f})`. All five views of every family member are computed from the same frozen inputs; each group (canonical key: range, filter set, breakdown value) gets **one** guarded state across the family and views; any cell hidden anywhere is hidden everywhere, and the rules A1–A9 run again until nothing changes (monotone: cells only become hidden; it ends). A filtered request whose own group was hidden in a parent's breakdown is answered hidden whole. This closes "complementary cells by filter" and "views against each other". With `FilterCap` 3 a family has at most 8 contexts.
- **D5 Differencing that D1–D4 can't close.** Two answerable ranges that differ by one bucket, and the same group reached from two contexts that aren't in one family (the `{80+}` / `{male}` case), are not closed by exact suppression. Making suppression consistent across every context a group can be reached from means guarding the whole filter lattice for every answerable range at once: intractable at request time, and the bridge argument of §5.5 does not extend beyond two-way tables. **D5 is the rule that depends on the privacy ruling (§5.4).**
- **D6 Budgets and audit.** Aggregate reads are rate-limited (30 a minute per session, then 429 `rate_limited`) and recorded (one `analysis.viewed` entry per session, filter set and day, unflagged, with the view, effective range and filter codes). A person whose distinct filter sets in a day pass a threshold (200, proposed) gets a flagged `analysis.unusual_querying` entry for the hospital's Activity and the platform audit. This doesn't prevent differencing; it makes it visible and attributable.

### 5.4 The differencing rule for D5, and R41a

**Proposed rule (one rule for every aggregate Measured Health publishes):** *exact counts are published only in fixed, non-overlapping windows; any flexible or overlapping release is protected by consistent, bounded perturbation.*

For Analysis this means **consistent cell-key perturbation** (the method statistical agencies use for flexible table builders):

- Each subject has a permanent random record key `rkey` (32 bits, `analysis_subjects`, never leaves core). A group's cell key is `Σ rkey mod 2³²` over its distinct subjects; for event counts (results, alerts, consultations) a per-fact key is summed the same way.
- The published count is `true + noise(cell key, true)`, with noise from a fixed table conditioned on the true count: integers in −2..+2, zero mean (proposed probabilities 0.1, 0.2, 0.4, 0.2, 0.1 away from the boundary, for the privacy lead to set, H1), and never producing a published 1–4 or a negative (for true counts of 5 and 6 the table puts no weight below 5). Zero stays zero. Then Layer A runs on the perturbed values (primary hiding still uses the true 1–4).
- **Consistency:** the noise depends only on the group's set of subjects, so a group reached by any path (a filter, a breakdown, another view, another day with the same frozen inputs) gets the same published value. Asking again or another way adds nothing; averaging doesn't work.
- **Differencing:** any exact identity an attacker uses (`t_hidden = Σ cᵢ tᵢ`) gives `Σ cᵢ pᵢ = t_hidden + Σ cᵢ nᵢ`. The error term has at least one noise term, so the attacker's estimate of a 1–4 group is spread over several values. With the table above the chance a two-term difference is exact is 0.26 and the spread covers ±4, enough that "3" can't be told from "0" or "6" with confidence.
- **Late data** changes a group's record set, so its key and noise change too: the before-and-after difference is noisy.
- **Cost:** published counts are off by up to 2; sets no longer add exactly; a drill-down's `count` (exact, it is a list) can differ from the chart cell. The contract needs one additive field, `Meta.perturbed: true`, and mhai-web must relax its set checks when it is set (the checks exist to catch server bugs; they would fail on honest noise) and say in plain words that chart counts are slightly adjusted (H6).

**If the privacy lead rules noise out,** Analysis goes live in exact mode with stricter limits: whole calendar months only (as Plan 12), `FilterCap` 2, `QuerySetMin` 10, D1–D4 and D6, and residual risk R-A1 (§13.6) recorded. Month-level differences of 1–4 patients for a filter combination remain possible and are accepted in writing.

**How it relates to R41a.** R41a (TODO.md, Plan 4c follow-ups) is the same problem on a small fixed table: 7d, 30d and 90d windows overlap, so 30d − 7d can be 1–4, and so can consecutive days. Analysis is the general case (any range, any filters). The two need **one ruling**, and Plan 7c and Plan 12 reuse it. Under the proposed rule:

- guardrail statistics (fixed table) switch to non-overlapping windows (the last 7 days, days 8–30, days 31–90), exact, each window and their sums guarded; the app adds them up;
- Analysis and Plan 12 (flexible) use cell-key perturbation;
- the **bound-based narrowing residual** (TODO.md: Plan 4c proves no hidden cell is solved exactly, not that its range can't be narrowed by non-negativity) applies to Analysis Layer A too; perturbation also widens those ranges, and the property test reports the narrowing rate rather than claiming it away (§5.6 P5).

### 5.5 Proof sketch

What is claimed, per mode:

- **Exact mode, one response (Layer A).** No hidden cell of a response is determined by the response's own equations.
  - *Lemma 1 (two-way tables).* For a table with margins (by-condition × {cases, new, existing} with the total row; series new column with total new), the response's equations are the row and column sums. Writing the margins negated, every row and column sums to zero; hidden cells are unknowns; the system is the incidence matrix of the bipartite row–column graph whose edges are hidden cells. A hidden cell is determined exactly when its edge is a bridge. `AcrossRows` hides cells until no hidden edge is a bridge. (This is Plan 4c's argument, `smallgroup` package comment, unchanged.)
  - *Lemma 2 (partitions sharing a total).* The five partitions of `patients` share only the `patients` cell. If `patients` is shown it is a constant, the partitions' equations have disjoint unknowns, and each is safe by the one-set rule (rule 2: no single hidden member; hidden members total 0 or ≥ 5, so `patients − Σ shown` is never 1–4). If `patients` is hidden, A7 hides everything.
  - *Lemma 3 (cross-view equalities).* Equal groups in two views (by-condition cases and a pair's `a_patients`) are one cell under D4, so they add an equation `x = x`, not new information.
  - The per-response claim follows; it is checked by an independent solver (P1).
- **Exact mode, a family (D4).** Each group has one guarded state across its family, so no group is shown in one family member and hidden in another; the union of the family's equations is one system over shared unknowns, and the fixpoint re-runs A1–A9 over it. The claim is checked by P2's solver over the family's union, not proved for all contexts.
- **Not claimed in exact mode:** safety across contexts outside one family, or across two answerable ranges. That is D5, and the residual R-A1 if noise is ruled out.
- **Perturbation mode.** Consistency (the published value is a function of the group's record set and the frozen inputs) is proved by construction and tested (P3); the differencing bound is the noise argument in §5.4, measured by P4.

### 5.6 Property and abuse tests (the plan writes these first)

Synthetic cohorts only, generated per test with a seed; the solvers are the tests' own, not the engine's.

- **P1 (per response, exact rational elimination, extends C20).** Random cohorts biased to small groups (the C20 `count` generator), small dimensions (3 conditions, 5 age bands, 4 sexes, 2 locations, 2 settings). For every view and every filter set within the cap: write every equation the response implies (sets, partitions, the two-way tables, the series new column, cross-view equalities), shown cells as constants, Gaussian elimination over the rationals; assert no hidden cell's reduced row is a single unknown. 2,000 cohorts per run in CI.
- **P2 (family attacker, exact mode).** For a request, collect every published number of every family member and view as equations over **atomic cells** (the full cross-classification of subjects by condition set × age × sex × location × setting); assert no hidden published cell is determined, and that every group shown in one member is shown with the same value in every member (D4). Then widen the attacker to every context within the cap (all families) and **report** (not assert) how often a hidden cell is determined: this is the measured size of R-A1 for the privacy lead.
- **P3 (consistency, perturbation mode).** For random cohorts, every path to the same group (filter versus breakdown, view versus view, two requests on one snapshot day, two core copies) gives the same published value; noise is within ±2; no published value is 1–4 or negative; a group's value changes only when its record set does.
- **P4 (differencing under noise).** For random pairs of answerable ranges and contexts whose exact difference is a group of 1–4, the attacker's best estimate is exact in at most the table's bound (0.26 for two terms) over many cohorts; the empirical spread is reported.
- **P5 (bounds, reported).** An LP over the response's equations with non-negativity: the share of hidden cells whose feasible range is narrower than 0–4 and excludes 0. Reported as a number for the privacy lead (Plan 4c's residual, measured here).
- **P6 (snapping).** Every `(from, to)` pair over two years maps to an effective range of whole weeks (≤ 13 weeks) or months; two requests differing by a day inside a bucket get byte-identical bodies.
- **P7 (shapes).** No drill key, cursor, view id, export id or error contains a pseudonym-shaped string; no aggregate body contains a `pid`, a name key, an MRN key or a date of birth key (fuzzed over every view).

### 5.7 Consistency with platform Analysis (Plan 12)

- The engine (`internal/analysis/disclose`) is shared; Plan 12 adds the across-rows rule over hospitals, the three-hospital minimum and the comparison rules on top. Plan 12's periods are already whole months.
- **Pseudonyms differ by side** (`side = platform`, scoped to the platform admin and letter), as Plan 12 §6 requires; a hospital-side and a platform-side list can't be matched.
- Plan 12 §5.2's "same cell in both scopes" holds by D4's canonical group key within the platform's own releases. The hospital page and the platform page are different releases about the same hospital; no role sees both, and a person with accounts on both sides is refused verifying that hospital's letters (`same_side`), which the threat model records (AN16).
- The perturbation rule (§5.4), if ruled, applies to Plan 12 unchanged; its "Download CSV" carries the perturbed values on screen, never more.

## 6. The analyst role and its grants

### 6.1 The rule

- `analyst` is already a clinical role in `user_roles` (00001) and allowed by `mhai_gateway`'s policy (00003). New: a database trigger refuses `analyst` without `clinician` for the same user, and removing `clinician` removes `analyst` in the same transaction (audited as `analyst_removed` with reason `clinician_removed`).
- **Who grants:** a hospital admin of the same hospital, through `PATCH /hospital/staff/{id} {version, roles}` (contract §5.1; core builds the staff endpoints with or before this). Until those exist, an operator uses `core users roles --add analyst` (audited the same way, actor `system` with the operator recorded).
- **Eligibility:** active, MFA set up, a clinician. A hospital admin can't grant it to themselves (they already have Analysis) and can't grant it while the hospital's analysis switch is Off (409 `analysis_off`; the box isn't shown then either).
- **Cap:** at most 10 analysts per hospital (proposed, §16 Q11), 409 `analyst_cap`.
- **Step-up:** none on the grant (the contract's PATCH has no code), because the role opens aggregates and pseudonymous rows only; names still need a letter. Revisit if the privacy ruling asks for it.

### 6.2 Audit

`analyst_granted` and `analyst_removed` (type admin), each with the actor, the person (by `display_name`) and the hospital; `roles_changed` stays for other role changes (Plan 6 ruling). When the notifications service exists, the hospital's admins are told of each grant.

### 6.3 Taking effect

- Grants and removals take effect on the next request: Analysis routes check roles against the principal (cache ≤ 30 s), and the **patient-level routes** (`/analysis/patients`, `/analysis/patients/named`, `/analysis/clearance/me`) re-read the role, MFA state and account status inside the request transaction, so removal ends access at once there.
- Removal, deactivation and MFA reset also end named access (§8.3), with `reason: role_or_account` on `clearance/me`.

### 6.4 `features` on the user

`User.features` (additive, on the login and session replies): `{analysis: boolean, named_access: boolean}`.

- `analysis`: the hospital's effective analysis switch is On **and** the user holds `hospital_admin` or `analyst` **and** MFA is on.
- `named_access`: `analysis` **and** the effective named-access position is not Off (the page shows letters at Letters only).
- The BFF's `ANALYSIS_SIGNED_OFF` and `NAMED_ACCESS_SIGNED_OFF` can only keep these off; the backend switch is the authority.

## 7. Switches

- **Analysis aggregates:** a global switch plus one per hospital, positions `off` and `on`, effective = the lower. Raising (global or hospital) is a two-person action with the three sign-off references (clinical, privacy, legal) and two different platform admins' step-up codes, the same machinery as the named-access switch (Plan 11 §4; one table, two kinds). Lowering is one platform admin with a reason and a code, at once. Live starts Off; the demo hospital On. While Off: every `/analysis/*` route answers 404 `not_found` (contract §10), and the nightly run still ingests (so turning on doesn't start from empty) but computes nothing.
- **Named access:** `off`, `letters_only`, `on`, global plus per hospital, effective = the lower (Plan 11 rulings). A hospital can't be raised above global (409 `above_global`). Every raise needs all three sign-off references (409 `signoff_missing`). Below On, the named route answers 403 `named_access_off`; at Off the clearance routes answer as the contract says.
- Both switches are written only by `mhai_platform` (a table the gateway role can read but not write, by grant and RLS), and every request, approval, cancellation and change is on the audit chain.

## 8. Clearance letters and named access (hospital side)

### 8.1 The letter

- **Upload:** `POST /hospital/clearances`, multipart (contract §10 Plan 11a): `file`, `signed_by`, `signer_title`, `letter_date`, `covered` (staff ids), `covered_platform` (emails, optional), `end_date`, `renews` (optional), `mfa_code` or `backup_code`, `Idempotency-Key`. Hospital admins only, step-up via `auth.Service.StepUpAudited`.
- **Checks in core before anything is stored:** body ≤ 10 MB + 64 KB (413 `file_too_large`); `end_date` after the hospital's today and ≤ `max_end_date` (today + 12 months, hospital days; 422 `end_date_too_late`); 1–20 covered in total; each covered staff id active, `hospital_admin` or `analyst`, MFA on (422 `not_eligible` with the ids); each `covered_platform` email an active platform admin with MFA (422 `not_eligible_platform` with the emails); `renews` a letter of the same hospital.
- **The file never touches core's parsers.** Core streams it to the **documents service** (planned, `services/documents/`), which stores the original encrypted in per-hospital object storage, checks the real type from the bytes (PDF, PNG, JPEG), refuses password-protected PDFs and over 20 pages, scans for malware, rasterises in a sandbox into flat PNG pages, and reports back on NATS with a closed outcome (`ready {pages, sha256}` or `refused {reason}`). Core records the outcome; the letter goes `checking → waiting` or `checking → refused`. Until the documents service exists, upload answers 503 `unavailable` in live mode (§16 Q15); the demo uses fixture pages.
- **Pages:** `GET /hospital/clearances/{id}/pages/{n}`: core fetches page `n` from the documents service, which stamps the viewer's display name and the time on it (Plan 11 §6.2), and passes it through as `image/png` with `Cache-Control: no-store`, `Content-Disposition: inline`, `X-Content-Type-Options: nosniff`. 409 `not_ready` while `pages` is 0. Each view is audited (`clearance_viewed`, with the page). The original is released only into the legal export (§8.8).
- **Lifecycle:** `checking → refused | waiting`; `waiting → active` (verified by a platform admin, Plan 11b) `| rejected | revoked`; `active → ended | revoked`. Narrowing removes covered people (never all: 422 `would_empty`); widening is a new letter. `version` rises on every change; revoke and narrow with a stale `version` are 409 `conflict`. A database trigger enforces the transitions, the 12-month cap, narrowing-only and `would_empty`, in the style of `feedback_transition_check` (00011).
- **Ending:** at the end of `end_date` in the hospital's time zone (the cover check compares with the hospital's today, so no job is needed for access to end); a nightly run sets `status: ended` and records `clearance_ended`. Reminders at 30, 7 and 1 days are recorded as `clearance_reminder_sent` and delivered by the notifications service when it exists.

### 8.2 Who is covered (one function, used everywhere)

`mhai_named_cover(user)` (SECURITY DEFINER, hospital from `mhai_current_hospital()`, never a parameter) returns the covering letter or none, and the reason when none. A person is covered when **all** hold, read in the request's own transaction with nothing cached:

1. the effective named-access position for the hospital is `on`;
2. the user is active, holds `hospital_admin` or `analyst`, and has MFA on, at this hospital;
3. a letter of this hospital is `active` (verified, not revoked), its `end_date` ≥ the hospital's today, and the user is in its covered list and not removed;
4. the hospital is active.

With several covering letters, the one with the latest `end_date` is used (the renewal case). `named_until` for a view is the earliest of: the letter's end of day (hospital time) and now + 30 minutes (the view lifetime, §16 Q10).

### 8.3 The named view: `POST /analysis/patients/named`

Body `{drill, purpose, note?, filters}`; `Idempotency-Key` required (the BFF refuses a missing or malformed key with 422 `invalid`). Purpose is `clinical_follow_up`, `quality_improvement`, `audit` or `other`; `other` needs a note of 1–200 characters (422 `invalid`).

In one transaction, in this order:

1. **Idempotency** (§8.5): a key seen before with another person or body → 422 `idempotency_mismatch`, before anything else.
2. The analysis gate (404 `not_found` when Off), MFA (403 `mfa_required`).
3. `mhai_named_cover`: position below On → 403 `named_access_off`; not covered → 403 `not_cleared`, or 403 `clearance_ended` when the only letter that covered them has just ended.
4. The drill (A8): 422 `too_small` unless it names a shown count of 5 or more under these filters on this snapshot day.
5. Select the subject ids (≤ 200, `count` the full number), insert `analysis_views` (level `named`, purpose, clearance id, `named_until`, the effective range and filter codes) and `analysis_view_rows`, store the note in `named_view_notes`, and **append the audit entry** (`analysis.named_viewed`, flagged security). Commit. **No audit, no names.**
6. After the commit, resolve identity (§8.6) for exactly those subjects, and answer `{level: "named", count, patients: [row + name + mrn], named_until, clearance: {id, end_date}}`. If resolution fails, answer 503 `unavailable` and append `analysis.named_view_failed` referring to the view (the view row stays: it records the attempt).

The de-identified route `GET /analysis/patients?drill&level=deidentified&…` follows steps 2, 4 and 5 (level `deidentified`, unflagged `analysis.patients_viewed` with the row count) and never calls identity. Any other `level` is 422 `level_not_supported` (the contract keeps names on the POST route only, so the purpose note never travels in a URL).

**Row minimisation.** A row carries the pseudonym, age band, sex, setting, and only the conditions and findings **recorded within the effective range** (not the patient's lifetime record), at most 10 of each, in a fixed order. Whether to narrow further (only the drilled condition and its co-occurring ones) is §16 Q7.

### 8.4 `GET /analysis/clearance/me`

`{named, until, clearance_id, version, reason}`, computed by `mhai_named_cover` on every call (no cache, so a revocation elsewhere is seen within the app's 30-second poll):

- `named`: covered now; `until` the earliest end of the covering letter (hospital end of day as an instant), or null; `clearance_id` the covering letter; `version` the letter's version (0 when none);
- `reason` when not covered (null when covered), from the most recent letter that covered the person: `none` (never covered), `ended`, `revoked`, `removed_from_letter`, `role_or_account` (role removed, deactivated, MFA reset), `switched_off` (position below On).
- Cheap: one indexed lookup; rate-limited at 6 a minute per session (the app polls every 30 s); not audited (it returns no patient data).

`GET /analysis/named-access` → `{position, since}`: the effective position for the hospital.

### 8.5 Idempotency for named views (not the generic store)

The generic `Idempotency-Key` middleware stores the reply body for 72 hours. **A named reply holds names; it must never be stored.** Named views use their own table, `named_view_keys`:

- the key, the user, an HMAC of the canonical body (drill, filters, purpose, note), the view id, `named_until`, created at;
- a replay with the same person and body **re-runs every check** (switch, cover, role, drill), re-reads the stored subject ids from `analysis_view_rows`, re-resolves identity, and returns the **original** `named_until`; it records nothing new on the chain;
- a replay after `named_until` is 409 `expired`; the key is spent and never renewed;
- a mismatched person or body is 422 `idempotency_mismatch`, before any other check;
- keys are swept 24 hours after `named_until`.

### 8.6 Identity: names come from the EMR service, at the moment of viewing

- Core never stores a name or an MRN. Consultations carry none (they are masked), and the patient index lives in the EMR integration service (ADR 0009, its own service because it holds outside credentials).
- Core sends `emr.v1.identity.resolve` over NATS: a core-signed request token (as for intelligence operations: audience the EMR service, hospital, user, expiry ≤ 30 s, single use) carrying, for each subject in the view, its patient-index reference (decrypted from `ref_enc` for this call only), plus the view id and the clearance id. The reply is `{reference: {name, mrn}}` for exactly the references asked, ≤ 200; anything else is dropped and the view fails (T20). The operation's shape is settled with the EMR design (H7).
- The EMR service checks the token, logs the view id and count (never names), and answers. Core passes the names through to the reply and drops them; nothing is cached, logged, traced or put in a dead letter. NATS TLS is required before this runs on more than one host (TODO.md).
- Until the EMR service exists, the named route answers 503 `unavailable` in live mode; the demo resolves from a synthetic identity fixture inside core's demo seeder.

### 8.7 Audit entries

All closed schemas in the audit catalogue, none with a name, MRN, pseudonym or free text:

| Action (mhai-web kind) | Type, flag | Facts |
|---|---|---|
| `analysis.viewed` | security, none | `view`, `from`, `to`, `filters` (codes) |
| `analysis.unusual_querying` | security, flagged | `rows`: the number of distinct filter sets that day |
| `analysis.patients_viewed` (`analysis_patients_viewed`) | security, none | `view`, `from`, `to`, `filters`, `patient_count`, `rows` |
| `analysis.named_viewed` (`analysis_named_viewed`) | security, **flagged** | as above plus `purpose`; `refs.clearance_id` |
| `analysis.named_view_failed` | security, flagged | `refs.clearance_id` |
| `analyst.granted`, `analyst.removed` | admin | the person by `display_name` |
| `clearance.uploaded`, `.refused`, `.verified`, `.rejected`, `.narrowed` (one per person), `.revoked`, `.ended`, `.viewed`, `.reminder_sent` | admin (a refused file with `failed_safety_check`: security, flagged) | reason codes, `refs.clearance_id`, pages viewed |
| `named_access.requested`, `.approved`, `.cancelled`, `.changed` | admin | position, scope, sign-off references (as reference codes) |

The purpose note is in `named_view_notes`, never in a payload. mhai-web must ship the kinds it doesn't yet have and the `analyst` actor role (TODO.md, mhai-web dependencies; H6).

### 8.8 Pseudonym reveal, the named-access list and exports (contract §14.9, deferred from Plan 4)

- **`GET /platform/audit/{id}`** gains `patients: {count}` for a drill-down entry and `letter` for a named one.
- **`GET /platform/audit/{id}/patients`** → `{pseudonyms}`: the hospital-side pseudonyms of the entry's rows, in the order shown (from `analysis_view_rows` and the epoch at the time), for platform admins and auditors; it writes `audit_patients_viewed`. This is the only route that sends a pseudonym outside Analysis; C22 stays true everywhere else.
- **`GET /platform/audit/named-access`** → named-view entries with `letter {id, status, valid_from, valid_to, verified_by}`; its first page writes `audit_viewed` with `facts.list: "named_access"`.
- **Exports** (`POST/GET /platform/audit/exports*`): as contract §14.9, built when object storage exists (the documents service). A `named_access` export is a ZIP of the CSV (pseudonyms only, never names) and a copy of each letter cited, from the documents service; the record is written before the file is released; owner only, once only, 15 minutes. **Names are never exported.** A legal request that needs names is answered outside the app by the hospital, which holds the identity: the export gives it the pseudonyms and the hospital resolves them with its own records (§16 Q14).

### 8.9 What the hospital side needs from the platform side (Plan 11b, its own design)

`POST /platform/clearances/{id}/verify|reject|revoke` with step-up and `version`; `same_side` (the verifier has or had an account at the hospital, or an email in its allowed domains) and `same_person` (the verifier is in `covered_platform`) refused **in the database trigger** as well as in the handler; `verifiable_by_you` computed by the same function; the switch requests and approvals. The hospital side reads only their results.

## 9. API contract (additive to `services/core/openapi.yaml`)

All under the clinical audience, MFA required, hospital from the session, errors as `{code}`; opaque ids `[A-Za-z0-9_-]+`. Nothing existing changes (ADR 0012).

| Method and path | Who | Notes |
|---|---|---|
| `GET /analysis/filters` | hospital admin, analyst | `{updated_at, conditions, locations, specialties, sources}`; conditions only those with at least `Min` patients in the last 24 months (a rare condition's existence is itself a disclosure; §16 Q6), plus "Not standardised" |
| `GET /analysis/{view}?from&to&condition&age_band&sex&location&setting&specialty&source` | same | the five views; `Meta.from`/`to` are the effective range (D2); additive `Meta.perturbed` if ruled (§5.4); 404 `not_found` (unknown view or gate off), 422 `invalid_range`, 422 `too_many_filters` (additive), 429 `rate_limited`, 503 `unavailable` |
| `GET /analysis/patients?drill&level=deidentified&…filters` | same | ≤ 200 rows, `count`; 422 `too_small`, 422 `level_not_supported`; audited |
| `POST /analysis/patients/named` | same, covered | §8.3; 403 `named_access_off`, `not_cleared`, `clearance_ended`; 409 `expired`; 422 `idempotency_mismatch`, `invalid`, `too_small`; 503 `unavailable` |
| `GET /analysis/clearance/me` | same | §8.4 |
| `GET /analysis/named-access` | same | `{position, since}` |
| `GET /hospital/clearances` | hospital admin | `{clearances, today, max_end_date}` (hospital days) |
| `POST /hospital/clearances` | hospital admin, step-up | multipart; §8.1 refusals; 503 `unavailable` until the documents service |
| `GET /hospital/clearances/{id}` | hospital admin | one letter (`ClearanceOut`, including `covered_platform`, `renews`, `revocation.day`, `history`, `version`) |
| `GET /hospital/clearances/{id}/pages/{n}` | hospital admin | PNG only, no-store; 409 `not_ready`; audited |
| `POST /hospital/clearances/{id}/revoke` | hospital admin, step-up | `{reason, note?, version?, mfa_code}`; 409 `conflict` |
| `POST /hospital/clearances/{id}/narrow` | hospital admin, step-up | `{remove, reason, note?, version, mfa_code}`; 422 `would_empty`; 409 `conflict` |
| `PATCH /hospital/staff/{id}` (roles) | hospital admin | the analyst rules of §6.1; 409 `analysis_off`, `analyst_cap` |
| `GET /platform/audit/{id}/patients`, `GET /platform/audit/named-access`, `/platform/audit/exports*` | platform admin, auditor | §8.8 |

- `User.features {analysis, named_access}` on the session replies (additive).
- New problem codes join `authz/errorcodes.go` and the generated docs: `too_many_filters`, `too_small`, `level_not_supported`, `invalid_range`, `named_access_off`, `not_cleared`, `clearance_ended`, `expired`, `idempotency_mismatch`, `would_empty`, `end_date_too_late`, `not_eligible`, `not_eligible_platform`, `not_ready`, `analysis_off`, `analyst_cap`, `above_global`, `signoff_missing`.
- **Not logged:** query strings of `/analysis/*` are dropped from access logs and traces (the BFF already doesn't log them); handlers log only the route, status, view and counts.

## 10. Migrations and RLS

The next free numbers at build time (00019 or later; cloud models part B may take 00019). One migration per PR (§14).

**Roles**, created like 00015–00017 (NOLOGIN, NOINHERIT, no members, refused if they already have members):

- `mhai_analysis_writer`: owns the nightly ingest functions; SELECT on the columns the ingest reads (decisions' `hospital_id`, `diagnosis`, `icd10_code`, `hga_decision`, `degraded`, `created_at`, `query_id`; queries' age, sex, specialty, urgency and the patient reference column once it exists; feedback's state columns), INSERT on the analysis tables. Never SELECT on `response_text`, `clinical_question`, `citations_json` or `medical_history`.
- `mhai_analysis_reader`: owns the read functions; SELECT on the analysis tables only.

**Functions** (SECURITY DEFINER, fixed `search_path`, the hospital from `mhai_current_hospital()`, never a parameter, refusing when it is null or the analysis switch is off):

- `mhai_analysis_cells(p_from date, p_to date, p_filters jsonb, p_view text)`: aggregates only (and the cell keys in perturbation mode), bounded to 24 months and to facts ingested before the hospital's current cutoff; executable by `mhai_gateway`.
- `mhai_analysis_rows(p_from, p_to, p_filters, p_group jsonb)`: subject ids (≤ 200) and the count; executable by `mhai_gateway`.
- `mhai_analysis_pseudonyms(p_subjects uuid[], p_side text)`: draws missing ones, returns the pids; executable by `mhai_gateway` (hospital side only).
- `mhai_named_cover(p_user uuid)`: §8.2; executable by `mhai_gateway`.
- `mhai_audit_view_pseudonyms(p_audit_id bigint)`: for `mhai_platform`, the hospital-side pids of one view (the reveal).
- `mhai_analysis_ingest(p_hospital uuid, p_until timestamptz)`: executable by core's scheduler login only.

**Tables and policies** (all `ENABLE` and `FORCE ROW LEVEL SECURITY`, `REVOKE ALL FROM PUBLIC`):

- `analysis_subjects`, `analysis_facts`, `analysis_first_seen`, `analysis_pseudonyms`: no grant to `mhai_gateway` or `mhai_platform` at all; reached only through the functions.
- `analysis_conditions` (shared), `analysis_locations`, `analysis_sources`: SELECT for `mhai_gateway` scoped by hospital (labels are the hospital's own).
- `analysis_snapshots`: SELECT and INSERT for `mhai_gateway`, own hospital; INSERT only for a day that has started in the hospital's zone; no UPDATE or DELETE except the sweep (definer).
- `analysis_views`, `analysis_view_rows`, `named_view_notes`, `named_view_keys`: INSERT and SELECT for `mhai_gateway`, own hospital, `viewer = current user` on INSERT (a `WITH CHECK` on a session setting set by core from the token); SELECT for `mhai_platform` on views and rows only through the reveal function; notes readable only by the export function.
- `clearances`, `clearance_covered`, `clearance_events`: `mhai_gateway` SELECT own hospital, INSERT own hospital, UPDATE only through definer functions that enforce the transitions; `mhai_platform` SELECT all, UPDATE only through the verify/reject/revoke functions. The transition trigger checks actor kinds (a hospital user can't verify; a platform user can't upload), `same_side` and `same_person`.
- `named_access_switch` and its requests (and the analysis switch rows): `mhai_platform` only for writes; `mhai_gateway` SELECT of the effective position for its hospital through a function.
- The analyst trigger on `user_roles` (§6.1).

**Indexes:** `analysis_facts (hospital_id, day)` and `(hospital_id, subject_id)`; `analysis_first_seen` primary key; `clearance_covered (user_id) WHERE removed_at IS NULL`; `analysis_snapshots (hospital_id, day, query_hash)` primary key.

**RLS tests** extend `TestRLSWithoutHospitalSeesNothing` to every new table, with WITH CHECK tests for INSERT and UPDATE (the open Plan 2 follow-up, done here for these tables).

## 11. Performance: frozen inputs and stored snapshots

- **Nightly per hospital,** after its midnight plus 30 minutes: ingest (§4.4), sweep (§4.3 retention), end letters that passed their date, then **precompute** the landing bodies: the three presets (last 30 days, last 90 days, last 12 months, each snapped) × five views, no filters, plus `filters`. These are stored in `analysis_snapshots` and served from there all day.
- **Other requests** are computed on first ask and stored (`INSERT … ON CONFLICT DO NOTHING`, then read back, as `guardrail_snapshots`), keyed by `(hospital, day, sha256 of the canonical request)`: the effective range, sorted filters, view. Because the inputs are frozen, a body computed twice is identical; the store is a cache, not the source of consistency. A per-key advisory lock stops two copies computing the same body at once.
- **Budget:** a view (its family under D4: up to 8 contexts) must compute within 2 s for a hospital with 200,000 subjects and 24 months of facts (about 5 million rows); `statement_timeout` 5 s on the read functions, then 503 `unavailable` (never a partial or zero body). The plan measures this on a synthetic hospital of that size on steven-reid before PR 4 merges.
- **Bounds:** at most 5,000 stored bodies per hospital per day (beyond that, compute without storing; consistency holds anyway); bodies ≤ 4 MB (as 00017).
- **Drill-down** reads `analysis_view_rows`-sized results only (≤ 200 subject ids plus a count) and is not cached.
- **The co-occurring view** is the expensive one (pairs per subject). It is computed from a nightly `analysis_pairs (hospital_id, day_bucket, subject_id, a, b)` materialisation per week, then filtered; if the budget fails on large hospitals, the plan restricts pairs to conditions with at least `Min` patients in the range before pairing (which A1 would hide anyway).

## 12. Failure modes

| What fails | What the user sees | What the backend does |
|---|---|---|
| Nightly ingest fails or is late | the previous day's figures, `updated_at` older | serves the last good inputs up to 7 days, then 503 `unavailable`; alert `analysis_ingest_failed`; never a partial day |
| A view exceeds its time budget | "Couldn't load {view}" with Try again | 503 `unavailable`; metric; no body stored |
| The disclosure engine sees inconsistent inputs (a set that doesn't add up, a negative) | that view fails | everything in the view hidden (`AcrossRows` fails closed), error metric, alert; never shows raw values |
| A drill key from another day, hospital or filter set | 422 `too_small` | the HMAC binds them; nothing else is revealed |
| Pseudonym collision | none | unique-violation retry with a new draw |
| Documents service down | letter stays "Checking the file"; pages 503 | upload accepted only when the service acknowledges; otherwise 503 and nothing stored |
| EMR identity service down or slow (> 5 s) | named view fails: "Couldn't show names" | 503 `unavailable`, `analysis.named_view_failed` appended; no names, no retry by the server |
| Audit append fails | the request fails | the transaction rolls back: no rows, no names, no view record |
| Letter revoked or switch lowered while names are open | names go within 30 s (poll) or at once on the next call | `clearance/me` changes; the named route refuses; the app drops names |
| Role removed or account deactivated mid-view | as above | patient-level routes re-read roles in the transaction; `reason: role_or_account` |
| Named replay after `named_until` | the view can't be reopened | 409 `expired`; the key is spent |
| A hospital's time zone changes | the day boundary moves once | the next nightly run uses the new zone; snapshots are keyed by the hospital day, so the changeover day is computed afresh |
| Late EMR data for past days | past figures change the next day | in perturbation mode the noise changes with the record set; in exact mode this is part of R-A1 |
| Clock skew between copies | none | days come from the database's `now()` in the hospital zone, not the copy's clock |

## 13. Security and threat model

### 13.1 Assets

Patient-level clinical facts (conditions, findings, age band, sex, setting, by subject); the subject-to-pseudonym map; names and MRNs at the moment of a named view; the clearance letters (legal records, possibly with staff names and signatures); small-group secrecy of every aggregate; hospital separation; the audit trail (who saw which patients, under which letter, for what purpose); the switches and their sign-off references; availability of the page; clinical safety (Analysis never feeding an answer).

### 13.2 Actors and trust boundaries

- **Actors:** hospital admins, analysts, clinicians without the role, platform admins, auditors, the clinical BFF, core, the nightly scheduler, the EMR service, the documents service, NATS, Postgres, a person who holds both a hospital and a platform account, anyone who obtains a screenshot or CSV.
- **Boundaries and what is checked at each:**
  1. browser → BFF: the session cookie; `ANALYSIS_SIGNED_OFF` / `NAMED_ACCESS_SIGNED_OFF` (can only refuse); the named key is required; query strings not logged.
  2. BFF → core (NATS `api.v1.clinical`): the access token, audience, MFA, roles (principal cache ≤ 30 s), route table; the hospital from the token only.
  3. core → Postgres: `SET ROLE mhai_gateway` with `mhai.hospital_id` from the principal; RLS on every table; analysis data only through definer functions that read the hospital themselves; cover, role and status re-read in the transaction for patient-level routes.
  4. core → documents service (NATS): a core-signed token naming the hospital and letter; the service answers closed outcomes; core never parses the file.
  5. core → EMR service (NATS): a core-signed, single-use, 30-second token naming the hospital, view id, clearance id and subject ids; the reply is checked to contain exactly those subjects.
  6. scheduler → Postgres: `mhai_analysis_writer` through the ingest function only.

### 13.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| AN1 | Information disclosure | **small group from one response** (a set, a share, a rank, a pair, the series column) | Layer A (§5.2) on `smallgroup`; full precision shares; complete partitions; app re-checks | P1, T1–T3 |
| AN2 | Information disclosure | **differencing across periods** (sliding days, late data) | D1 frozen whole days, D2 whole buckets, D5 perturbation (or recorded residual R-A1) | P3, P4, P6, T4 |
| AN3 | Information disclosure | **differencing across filters and views** (complementary cell asked directly; same group in two contexts) | D3 cap and query-set minimum, D4 family fixpoint, D5 perturbation; D6 audit and unusual-query flag | P2, P3, T5, T6 |
| AN4 | Information disclosure | **attribute disclosure from a narrow view** (1–9 patients and their condition names) | A7 empty lists; `QuerySetMin`; conditions in `filters` only with ≥ 5 patients | T7 |
| AN5 | Information disclosure | **profiling through the drill-down** (stable pseudonyms linked across many lists into a re-identifiable record) | rows limited to the range and to 10 conditions; pseudonym epochs (§16 Q8); drill budgets (10/min, 100/day per person, then 429 and a flagged entry); every list audited | T8, T9 |
| AN6 | Information disclosure | **cross-hospital read** (a drill key, a letter id, a pseudonym from another hospital) | hospital from the token and from `mhai_current_hospital()` in every function; drill keys HMAC-bound to the hospital; 404 for another hospital's letter | T10 |
| AN7 | Elevation of privilege | **names without cover** (switch below On, letter not verified, expired, revoked, person removed, role lost, deactivated, MFA reset) | `mhai_named_cover` in the request transaction, no cache; the BFF key can only refuse; switch writable by `mhai_platform` only | T11–T14 |
| AN8 | Elevation of privilege | **self-verification** or a hospital user verifying | Plan 11b's checks, and the transition trigger refusing a hospital actor, `same_side` and `same_person` | T15 |
| AN9 | Information disclosure | **names persisted** (idempotency store, snapshot, log, trace, dead letter, audit) | named views bypass the generic idempotency store (§8.5); identity resolved after commit and dropped; closed audit schemas; telemetry tests | T16, T17 |
| AN10 | Repudiation | **a named view nobody can account for** | the audit entry is committed before identity is resolved; view rows stored; purpose and note kept; the hash chain | T18 |
| AN11 | Tampering | **a request names another user, hospital or letter** (the clearance id, `viewer`, hospital) | the server picks the letter; viewer and hospital from the token; `WITH CHECK` on view inserts | T19 |
| AN12 | Spoofing | **a forged identity or documents reply on NATS** | core-signed tokens with audience and expiry; replies on `_INBOX_` subjects checked as for intelligence (the reply-subject rule); NATS account split before production (TODO.md) | T20 |
| AN13 | Tampering | **a malicious letter file** (PDF exploit, polyglot, script) | core never parses it; the documents service checks type, scans, rasterises in a sandbox; the browser gets flat PNGs only | (documents design) |
| AN14 | Denial of service | **expensive views or drill floods** | rate limits; time budget and `statement_timeout`; snapshot cache; family ≤ 8 contexts; drill budgets | T21 |
| AN15 | Information disclosure | **Analysis data feeding clinical answers** | analysis tables have no grant to `mhai_worker`/`intelligence_app`; no NATS operation exposes them; ADR 0014 amends SKILLS.md §1 to say so | T22 |
| AN16 | Information disclosure | **a person with both a hospital and a platform account** combines the hospital's Analysis with Plan 12's per-hospital figures | pseudonyms differ by side; `same_side` refuses their verifying; residual: aggregate differencing across the two releases (R-A3) | T15 |
| AN17 | Elevation of privilege | **stale role after removal** (principal cache) | patient-level routes re-read roles in the transaction; aggregates may lag ≤ 30 s (accepted: aggregates are guarded) | T13 |
| AN18 | Information disclosure | **pseudonyms leaking through audit or ids** | random pids; C22 everywhere except `/platform/audit/{id}/patients` (itself audited); P7 | P7, T23 |
| AN19 | Tampering | **a direct write to facts or switches** by the app roles | no table grants; definer functions only; switch rows platform-only | T24 |
| AN20 | Information disclosure | **the purpose note carries a patient's name** | 200 characters, encrypted at rest, never in audit or logs, released only in a legal export to the hospital that wrote it | T17 |

### 13.4 Controls (decided)

- **Authentication and step-up:** MFA on every Analysis and clearance route; step-up on letter upload, narrow and revoke, and on every switch action; none on reading or on the analyst grant (§6.1).
- **Authorisation:** roles in the route table; for patient-level routes, role, account and cover re-read in the transaction; hospital scope in code and in every definer function; RLS with `FORCE` on every new table; least-privilege roles for ingest and reads.
- **Data handling:** facts are coded, banded and dated by day only; no names or MRNs stored; identity resolved from the EMR service per view and dropped; pseudonyms random and stored; snapshots hold guarded output only; nothing from Analysis in logs, traces or audit beyond codes and counts; retention as §4.3.
- **Concurrency:** snapshots insert-once-read-back; advisory lock per body; idempotency keys for named views in their own table with the original `named_until`; letter `version` checks; pseudonym draws under a unique index.
- **Secrets:** the drill-key HMAC key, the reference HMAC and encryption keys (`ref_hmac`, `ref_enc`) and the note-encryption key are core secrets (files, refused when missing or placeholders at start, rotated yearly; rotating the drill key invalidates open drill keys only).
- **Third parties and hosting:** none new. The EMR and documents services are ours; object storage is per hospital and encrypted.

### 13.5 Abuse-case tests (the plan writes these first)

- **T1** female 212, male 3, total 215: male and the next-smallest hidden; no shown pair leaves 1–4.
- **T2** series: the new column's hidden points can't be recovered from `total.new`.
- **T3** a pair with "both" 3 ranks after every shown pair whatever its true size.
- **T4** `[d, d+29]` and `[d, d+30]` give byte-identical bodies when they snap to the same weeks; across the cutoff, a fact ingested today changes nothing until tomorrow.
- **T5** the complementary-cell attack: a hidden-by-complement "80+" asked as a filter is hidden whole (D4).
- **T6** a fourth non-date filter: 422 `too_many_filters`.
- **T7** a filter set matching 7 patients: every number hidden, every list empty, no drill.
- **T8** the 101st drill-down in a day: 429 `rate_limited` and a flagged entry.
- **T9** a drill row carries no condition recorded outside the effective range, and at most 10.
- **T10** another hospital's drill key, letter id or page: 422 `too_small` / 404, and nothing about the other hospital in the reply or timing beyond the code.
- **T11** with the position at Letters only, a covered person's named request: 403 `named_access_off`.
- **T12** a letter revoked in another transaction: the next named request is 403 `not_cleared` and `clearance/me` says `revoked`; an expired one at the hospital's midnight: `clearance_ended`.
- **T13** analyst removed, then within the 30-second principal cache: the named and de-identified routes refuse.
- **T14** MFA reset or deactivation of a covered person: refused at once, `reason: role_or_account`.
- **T15** verify by a hospital user, by a platform admin linked to the hospital, by a covered platform admin: refused by the trigger even when called directly in SQL.
- **T16** after a named view and its replay, no row of `idempotency_keys`, `analysis_snapshots`, `audit_log` or any log line contains the synthetic names or MRNs.
- **T17** logs, spans and dead letters from a named view (and from each failure) contain no name, MRN, pseudonym or note.
- **T18** an audit append forced to fail: no rows returned, identity never called.
- **T19** a body naming a clearance id, a hospital or a viewer: ignored or 422; the view records the server's letter and the token's user.
- **T20** an identity reply with an extra subject or on the wrong reply subject: dropped, 503.
- **T21** 20 parallel cold requests for the same body: one computation, one row, identical replies.
- **T22** as `intelligence_app` and `mhai_worker`, SELECT on every analysis table fails; no `operations.json` entry reaches them.
- **T23** every audit and export response (named-access list, exports) has no pseudonym except `/{id}/patients`.
- **T24** as `mhai_gateway`, INSERT or UPDATE on facts, subjects, pseudonyms, switches: denied.

### 13.6 Residual risk and sign-off

- **R-A1 differencing in exact mode.** Without perturbation, two answerable ranges one bucket apart, or one group reached from two families, can give a 1–4 group exactly. P2 measures how often on synthetic cohorts. Accepted only by the privacy lead in writing, with the stricter exact-mode limits (§5.4). **Same ruling as R41a.**
- **R-A2 bound-based narrowing.** As Plan 4c: exact solving is excluded, range narrowing is not; P5 reports it.
- **R-A3 two releases about one hospital** (hospital Analysis and Plan 12) differenced by someone who sees both; only possible for a person with both kinds of account; perturbation (one cell key per subject set) makes it noisy.
- **R-A4 the drill-down is microdata by design.** A hospital admin or analyst sees pseudonymous rows with conditions; a determined insider who knows a patient's age band, sex and rare condition can recognise them. Controls limit and record it; they don't prevent it. This is the core privacy sign-off question (H1).
- **R-A5 the person at the screen.** Screenshots of names or rows; the watermark and audit deter, nothing prevents.
- **Sign-off:** clinical (the views' definitions, confidence mapping, that hospital admins may see them, noise acceptable if ruled), privacy (the rules and constants, the differencing ruling with R41a, pseudonym policy, row minimisation, retention), legal (NDPA 2023 basis for this secondary use; hospital agreements; the letter as authority for named access; letter retention). TODO.md items in the plan's first PR.

## 14. Phased PRs

Each its own PR, reviewed and merged by a person; each runs on the demo only (the switches stay Off for real hospitals). The build can proceed to the end of phase 5 without any sign-off; nothing goes live without H1–H3.

| PR | Contents | Depends on |
|---|---|---|
| **0** (this) | this design | — |
| **1. Decisions on paper** | ADR 0014 "Population analysis under disclosure control" (changes the "admins see no clinical content" rule for aggregates and pseudonymous rows, and amends SKILLS.md §1 so it reads: Analysis aggregates may span patients, never feed retrieval, prompts or models); the frontend-requirements §10 updates (effective range, `too_many_filters`, empty lists under A7, `features`, `perturbed` if ruled); TODO.md sign-off items | H1 ruling preferred but not required (both modes written) |
| **2. Data foundation** | migration: subjects, facts, first-seen, dictionaries, pseudonyms, analysis switch, the two roles and the ingest function; the analyst trigger; `features` on the user; the nightly scheduler; `core analysis seed-demo` (synthetic subjects for the sample hospital); RLS tests; T19, T22, T24 | — |
| **3. Disclosure engine** | `internal/analysis/disclose` (pure Go on `smallgroup`): Layer A, D2 snapping, D4 fixpoint, the cell-key option behind a constant; P1–P7; reviewable by the privacy lead without the routes | — |
| **4. Aggregate routes** | `GET /analysis/filters` and `/analysis/{view}`, snapshots and precompute, rate limits, `analysis.viewed`, unusual-query flag, openapi; T1–T7, T21; the 2-second budget measured on steven-reid | 2, 3 |
| **5. De-identified drill-down and reveal** | `GET /analysis/patients`, drill keys, row minimisation, drill budgets, `analysis_views`/rows, pseudonym draws, `GET /platform/audit/{id}/patients` and `patients`/`letter` on the detail; T8–T10, T23, P7 | 4 |
| **6. Analyst grant** | the analyst rules in `PATCH /hospital/staff/{id}` (or with the staff endpoints if they land first), `analyst_granted`/`removed`, cap, CLI | 2; staff endpoints |
| **7. Letters, hospital side, and the switch** | clearance tables and trigger, upload/list/detail/pages/revoke/narrow, `named_access_switch` reads, the nightly end, reminders recorded; T15 (trigger parts) | documents service (live); demo with fixture pages; Plan 11b design for verify |
| **8. Named access** | `mhai_named_cover`, `POST /analysis/patients/named`, `named_view_keys`, notes, `GET /analysis/clearance/me` and `/named-access`, the identity call; T11–T14, T16–T18, T20 | 7; EMR service (live); demo with a synthetic identity fixture |
| **9. Audit named-access list and exports** | `GET /platform/audit/named-access`, exports with letters | 7, 8; object storage |
| **10. EMR facts** | findings, alerts, settings, locations and sources into facts; the `findings` view's live data | the EMR plan, notifications (alerts) |

## 15. What's needed from people

| # | Who | What, exactly | Needed by |
|---|---|---|---|
| H1 | Privacy lead | **One differencing ruling for R41a and Analysis** (§5.4): accept bounded, consistent perturbation for flexible releases (and set the noise table), or accept residual R-A1 with the exact-mode limits. Also: `QuerySetMin` (10), `FilterCap` (3), drill row minimisation (§16 Q7), pseudonym epochs (§16 Q8), drill budgets, retention (§4.3). Then sign-off on the rules as built (P2/P4/P5 reports attached). | PR 1 (ruling); before any live hospital (sign-off) |
| H2 | Clinical lead | Sign-off that hospital admins and analysts may see the five views; the definitions ("case", "new", "existing"), the confidence mapping from guardrail decisions (§4.1), condition grouping (full ICD-10 or 3-character), and whether ±2 noise is acceptable for population insight. | Before any live hospital |
| H3 | Legal / compliance lead | NDPA 2023 lawful basis for this secondary use; a clause in each hospital's data agreement; the clearance letter as the authority for named access (a template helps); letter and audit retention; who answers a legal request that needs names (§8.8). | Before any live hospital; letter template before PR 7 goes live |
| H4 | Product owner | Approve this design and the open questions (§16); name the pilot hospital; confirm Analysis waits for patient references (§16 Q2). | PR 1 |
| H5 | Platform / ops | Object storage and the documents service (scan, rasterise, page stamping) for letters; per-hospital encryption; the scheduler slot for the nightly run; alerts for `analysis_ingest_failed`. `SECURITY_CONTACT_*` set (the integrity page needs it). | PR 7 live; PR 9 |
| H6 | mhai-web team | Show the **effective** range from `Meta`; handle 422 `too_many_filters` and 429 `rate_limited`; accept **empty lists beside a hidden patient count** (A7); if perturbation is ruled, read `Meta.perturbed`, relax the set checks and say counts are adjusted; ship the audit kinds of §8.7 and the `analyst` actor role (TODO.md); `searched_kind` values already pending. | Before PR 4 and PR 8 go live |
| H7 | Intelligence lead and the EMR design owner | Consultations carrying a hospital-scoped patient reference (plan departure 7); the EMR service's canonical events into core and the `emr.v1.identity.resolve` operation with its subject map (§8.6). | PR 10; PR 8 live |

## 16. Open questions, each with a recommendation

1. **Differencing across periods and filters: perturb or accept?** *Recommendation: perturb.* Consistent cell-key perturbation (±2, zero mean) for Analysis and Plan 12, non-overlapping exact windows for guardrail statistics and 7c, decided as **one ruling with R41a**. Exact suppression can't be made consistent across a flexible filter lattice (§5.3 D5); noise is the method agencies use for exactly this, and it also blunts R-A2. If noise is refused, exact mode with whole months, two filters and R-A1 accepted in writing.
2. **Count consultations without a patient reference?** *Recommendation: no.* Counting each consultation as a patient overstates "patients", breaks "new versus existing" and co-occurrence, and gives the drill-down nothing to point at. Build and demo on synthetic subjects; go live when references exist.
3. **`QuerySetMin` for filtered views?** *Recommendation: 10.* Stronger than the per-cell 5, cheap, and it removes the narrowest trackers (Plan 12 hides filtered groups under 5; 10 is at least as strong).
4. **Filter cap?** *Recommendation: 3* non-date filters (Plan 12 allows 4 for one hospital, 3 across hospitals); 2 in exact mode.
5. **Snap ranges to whole weeks or months?** *Recommendation: yes,* with the effective range returned in `Meta` (compatible with the app's reader; H6 shows it).
6. **Lists under a hidden patient count, and rare conditions in the filter list?** *Recommendation: send empty lists, and list a condition in `filters` only when it has at least 5 patients in the last 24 months* (others are searchable only as "Other conditions" aggregated). A named condition beside "Fewer than 5" is itself a disclosure.
7. **What a de-identified row shows.** *Recommendation: conditions and findings recorded in the effective range only, at most 10 each.* Narrowing to the drilled condition alone would make the list useless for co-occurrence work; lifetime records would make every row a profile.
8. **Pseudonym stability.** *Recommendation: stable within the hospital (hospital side) per calendar-year epoch;* platform side per admin and letter (Plan 12). Yearly epochs bound longitudinal linking; the audit reveal uses the epoch shown at the time.
9. **Drill budgets.** *Recommendation: 10 a minute and 100 a day per person for de-identified lists, 20 a day for named views;* over budget is 429 and a flagged entry.
10. **Named view lifetime.** *Recommendation: 30 minutes* (or the letter's end, if sooner). The demo used an hour; the 30-second poll ends access faster on revocation, the lifetime bounds an abandoned screen.
11. **Analyst grant controls.** *Recommendation: no step-up (the contract's PATCH has none), at most 10 analysts per hospital, admins told of each grant once notifications exist.*
12. **A per-hospital switch for aggregates, raised by two people with sign-off references?** *Recommendation: yes,* the named-access machinery reused, so Analysis starts with one pilot hospital.
13. **Audit every aggregate read?** *Recommendation: yes, deduplicated per session, filter set and day, unflagged,* so differencing attempts are visible (D6); the hospital spec audits only drill-downs today, so mhai-web needs the `analysis.viewed` kind (H6).
14. **Legal requests that need names.** *Recommendation: the export carries pseudonyms only; the hospital resolves them from its own records.* Measured Health never assembles a named file.
15. **Letters before the documents service exists?** *Recommendation: wait.* Core must not parse untrusted PDFs; live upload answers 503 until the service is there, the demo uses fixture pages.
16. **SKILLS.md §1 says never build anything that spans patients.** *Recommendation: amend it by ADR 0014* to scope that rule to retrieval, prompts and models, and state that Analysis aggregates live in core under disclosure control and never reach the pipeline (AN15).

**Relation to R41a, in one line:** Analysis is R41a's general case; the recommendation is one privacy ruling ("exact only in fixed, non-overlapping windows; flexible or overlapping releases perturbed"), which closes R41a by non-overlapping guardrail windows and gives Analysis and Plan 12 their D5.
