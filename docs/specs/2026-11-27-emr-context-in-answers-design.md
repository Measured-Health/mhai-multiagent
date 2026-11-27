# EMR context in answers ("Ask MHAI about this patient"): design

- **Status:** Approved by the product owner on 27 Nov 2026 ("go with the recommendations"): every decision in §14 is the recommended option. Not built. Before it goes live: the clinical lead confirms the §4.1 windows and caps and the evaluation, and the sign-offs in this design. Design only, no code yet.
- **Date:** 2026-11-27
- **Owner's ask (27 Nov):** "Can the doctors access de-identified patient records from our platform and analyse them; add more context too, so the EMR/EHR pipeline is important not just for anomalies but for pulling data and pushing data to the EHR systems." Then: "yes draft the designs, add to todo".
- **Companion design:** [Write-back to the hospital's EHR](2026-11-27-ehr-write-back-design.md) (the "pushing" half).
- **Builds on (and stays consistent with):**
  - the EMR design, [`2026-11-07-emr-integration-design.md`](2026-11-07-emr-integration-design.md): the canonical model (§8), provenance, `standardised`, the patient index and `pids` (§10.5), exclusion and purge (§10.6), freshness and `sources_status` (§15), the NDPA basis (§17.7). Its §1 scope names "Ask MHAI about this patient" as later work on the same model. This is that work;
  - the clinician lookup by MRN (`POST /patients/lookup`, OpenAPI 1.29.0) and `patient_id` on `POST /queries` (1.22.0), both built;
  - the intelligence boundary design §7.2–§8.2: the request token, check 8 (`pids`), and "the worker reads the question and patient data from `clinical_queries` under RLS";
  - ADR 0013 and the cloud design §4.3 (the cloud allow-list is two fields; widening it needs a design change and a test per field);
  - the support gate (#5, #28, PR #267) and the local practice design's ruling R-LP1, "visit records are never evidence" ([`2026-11-23-local-practice-context-design.md`](2026-11-23-local-practice-context-design.md) §3);
  - the Analysis design ([`2026-11-07-analysis-backend-design.md`](2026-11-07-analysis-backend-design.md)) and ADR 0015, for §13 (de-identified access).
- **Names.** No hospital is named. Every example is synthetic.

## 1. Why

Today the answer sees only what the clinician types in `patient_data`: age, sex, symptoms, medical history. A clinician can already pick a patient (from an alert, or by MRN) and send `patient_id`. Core checks it and puts it in the token's `pids`. But no record fact reaches the model. The clinician must retype the potassium they can see in the EHR, or the answer ignores it.

This design lets core attach a small, checked slice of the patient's record to the question: recent results, recent vital signs and active medicines. The answer can then say "with a potassium of 6.1 mmol/L two days ago, avoid ...", and the clinician sees exactly which facts were used.

## 2. Goals, non-goals, anti-goals

**Goals**
- G1. For a question with a checked `patient_id`, core selects a minimal, bounded, time-limited slice of that patient's canonical EMR facts (§4).
- G2. The slice is frozen with the question, so what the model saw can be shown, audited and replayed (§5).
- G3. Patient facts are context, never evidence. They never count as support for a guideline claim (§7).
- G4. The clinician sees "From the record": facts used, facts left out, missing data and stale-data warnings (§8).
- G5. A failure to read the record never blocks or slows the answer. The answer comes without it, labelled (§10).
- G6. Off by default, per hospital (§3.3).

**Non-goals**
- Notes, free text, imaging, procedures, conditions and encounters beyond "current location". Labs, vitals and medicines only, as the EMR model holds today.
- Any fact from another patient, or a summary across patients. ADR 0004 and SKILLS.md §1 stay absolute here.
- A model choosing which facts to read. Selection is deterministic code in Go (ADR 0010).
- Writing anything to the EHR (the companion design).

**Anti-goals: failure even if it works**
- A1. A fact of another patient, or of another hospital, reaches a prompt or a screen.
- A2. A patient fact counts as support for a guideline claim, or lifts an answer to PASS.
- A3. A clinician reads a stale or partial record as current and complete.
- A4. Source-controlled text (`local_display`, `value_text`, `ref_text`, `frequency_text`) reaches a model prompt.
- A5. An identifier (name, MRN, source id, exact date of birth) reaches a cloud model.
- A6. The answer states a patient value the record doesn't hold, and it is shown.

## 3. Shape

### 3.1 Flow

```
clinician ─▶ BFF ─▶ core POST /queries {question, patient_data, patient_id}
  core: emr_patient_in_hospital(pid)  (exists)
        emr_patient_slice(pid, now)   (new definer function, §4)  ── 300 ms budget
        one transaction: clinical_queries row + clinical_query_facts rows (§5) + audit
        publish jobs.v1.<hid>.run_query  (ids only; token pids = [pid])
worker: claim the row under RLS; read clinical_query_facts for that query under RLS
        build the prompt: question, typed patient_data, "Patient record facts" block (§6)
        generate; support gate (§7); fact check (§7.3); write the result
clinician ─▶ GET /queries/{id}  (+ facts_used, record_status)
```

### 3.2 Who does what

- **Core** decides which facts are read (the slice rules), stores them with the question, and serves them to the clinician. It is the only reader of `emr.*` on this path.
- **The worker** never gets a grant on `emr.*`. It reads only the frozen slice of the claimed question, under RLS, in `with_hospital(hid)` from the token.
- **The emr service** is not on this path. The slice reads tables it has already written.

### 3.3 Switches

- `CORE_EMR_ENABLED` (exists). While off, every `patient_id` is 404.
- A new per-hospital setting, `emr_context` with positions `off` (default), `own_model_only`, `own_and_cloud`. A platform admin sets it, with step-up, audited. `own_and_cloud` is refused until the cloud sign-off in §14 Q3 is recorded.
- With `off`, a `patient_id` still works as today (the patient is named; no facts attached).

## 4. The slice: what core selects

`emr_patient_slice(p_patient uuid, p_as_of timestamptz)` is a `SECURITY DEFINER` function in the `emr` schema, `EXECUTE` for `mhai_gateway` only. It re-checks the patient is active and in `mhai_current_hospital()`, and returns rows, not a free query. It reads current versions only (`superseded_by IS NULL`).

### 4.1 What is in it (recommended values; clinical lead confirms, §14 Q1)

| Kind | Window | Rule | Cap |
|---|---|---|---|
| Lab results | last 30 days | the latest value per analyte (LOINC, or local code when not standardised), plus the one before it for a trend | 30 analytes |
| Abnormal labs | last 90 days | any result flagged `H`, `L`, `HH`, `LL`, `A`, `AA` by the source or by the rules engine, latest per analyte, if not already in | 10 more |
| Vital signs | last 72 hours | the latest set (one value per vital), plus the min and max of each in the window | 8 vitals |
| Medicines | now | `active` and `on-hold` only; the last 10 `stopped` or `completed` in the past 30 days, marked as stopped | 25 active, 10 stopped |

- **Statuses.** `final`, `amended` and `corrected` are used. `preliminary` is used and labelled "preliminary". `cancelled` and `entered-in-error` are never used.
- **Order and caps.** Inside each kind: abnormal first, then newest first. When a cap cuts, the count of what was left out is kept ("12 more results not used").
- **A hard ceiling.** At most 80 facts and about 2,000 prompt tokens in total. Over that, the lowest-priority facts drop first.

### 4.2 Relevance (recommended: deterministic, no model)

v1 does not try to guess relevance from the question. It sends the same fixed slice for every question about the patient, so the clinician always knows what the model saw. A second step (§15, PR 6) may add a closed map from question topics to analyte groups (for example "kidney" or "metformin" to the renal panel), reviewed like the rules engine's code maps (EMR §9.4). It only re-orders inside the caps, so it can't hide an abnormal value.

### 4.3 What each fact carries

`kind`, `code_system` (`loinc`, `atc`, `local`), `code`, `standardised`, a display label **from our own code tables** (never the source's `local_display`), `value_num`, `unit_ucum`, `value_si`, `ref_low`, `ref_high`, `flag` (source and rules engine, kept apart), `status`, `effective_at`, `age_hours` at the time of asking, `source_label` (the hospital's own label, ADR 0009 rule 10), and the canonical row's id and version.

**Never in a fact:** the patient's name, MRN, source ids, `value_text`, `local_display`, `ref_text`, `frequency_text`, the raw unit string, the practitioner, or anything outside §4.3. A non-numeric result (for example "positive") is carried only when it maps to a closed value set (`positive`, `negative`, `detected`, `not_detected`); any other text is left out and counted.

**Not standardised facts** (no LOINC or ATC) are listed to the clinician as "not used: not standardised" and never go to a model. Their label would have to come from the source's text, which A4 forbids.

## 5. How it travels: frozen with the question

- **A new table,** `clinical_query_facts` (`query_id`, `hospital_id`, `ordinal`, the §4.3 columns, `slice_version`), `ENABLE` and `FORCE` RLS on `hospital_id`, written by core in the same transaction as the `clinical_queries` row. `mhai_worker` gets `SELECT` only. A row in `TestRLSWithoutHospitalSeesNothing`.
- **Plus on `clinical_queries`:** `facts_status` (`attached`, `none_found`, `off`, `stale`, `unavailable`, `timed_out`), `facts_count`, `facts_omitted` (counts per reason), `facts_sha256` (a hash over the ordered rows), `sources_status` at the time of asking.
- **Nothing in the NATS message.** The job carries the query id, as today. The token's `pids` carries the patient id core checked. The worker refuses a body patient id not in `pids` (check 8, exists).
- **Identity never from the payload.** The hospital is the token's `hid`, set in `with_hospital`. The worker reads facts by `query_id` under RLS. A forged query id from another hospital sees no rows. A test asserts the worker's query has no patient filter it could take from a body.
- **Why frozen, not re-read.** The record can change between asking and answering, or between answering and an audit. The frozen slice is what the model saw. It is shown, hashed and audited as one thing.
- **Retention.** The facts follow the question's retention. A patient exclusion (EMR §10.6) also purges that patient's `clinical_query_facts` and sets `facts_status` to `purged` on the questions (the answer text stays, as for any question). The purge job and its test gain this table.

## 6. Minimising and masking

- **Coded and numeric only** (§4.3). This is the main control: no source text means no injection path and no free-text identifiers.
- **Relative times.** The prompt says "2 days ago", never a date. The screen shows the date.
- **The prompt block** is fixed and separate from the evidence block:
  ```
  Patient record facts (context from the hospital record, not evidence; do not cite as a source):
  [F1] Potassium 6.1 mmol/L (ref 3.5–5.1) HIGH, 2 days ago, final
  [F2] Lisinopril 10 mg oral, active
  ```
  Labels come from our tables. Values are formatted by code. The model is told to refer to facts as `[F1]` and to sources as the existing chunk citations.
- **Our own model (GPU, local route):** gets the block as above.
- **Cloud models (ADR 0013):** recommended **none in v1** (`own_model_only`). If the owner chooses `own_and_cloud` after legal sign-off (§14 Q3), the cloud prompt gets a narrower block: coded label, value, unit, range, flag, relative time; no source label; no stopped medicines. The cloud allow-list (cloud design §4.3) widens by exactly these typed fields, each with its own test, and the residual-PII scan runs on the exact bytes as today.
- **Typed patient data stays as it is.** If a typed value conflicts with the record (typed age 34, record birth year says 61), both are shown to the clinician as a conflict. The prompt gets the record value and a "conflict" marker. Nothing is silently overwritten.

## 7. Facts are context, never evidence

This keeps R-LP1's line: what comes from patient records is never evidence for a guideline claim.

### 7.1 The rule

- A patient fact has no chunk id. It is never in `kb_chunks`, never retrieved, never in `citations` or `cited_sources`.
- The support gate counts only cited, retrieved chunks, as built. A claim that cites only `[Fn]` has no support. It can't lift an answer to PASS.
- A citation of `[Fn]` in the place of a chunk citation, for a guideline claim, is redacted with a new reason `patient_fact_not_evidence` (no NLI), and the claim is then uncited (caps at REGENERATE, as today).

### 7.2 Two kinds of claim

The checker splits claims in two:
- **Record claims** ("potassium was 6.1 two days ago"). Checked against the frozen slice by code (§7.3). Shown with a "from the record" mark. Never counted as support.
- **Guideline claims** ("avoid ACE inhibitors when potassium is above 6.0"). Need a cited, retrieved chunk, as today.

### 7.3 The fact check (new, deterministic)

- Every number in a record claim must match a fact in the slice (value and unit, after unit conversion by our table). A record claim with a number that no fact holds, or a fact id that doesn't exist, is a **misstated fact**: the answer ESCALATES (reason `record_misstated`). It is never shown as PASS.
- A record claim with no `[Fn]` that states a lab or medicine value is treated the same way, so the model can't slip an invented value in uncited.
- The check runs in the worker after generation, before the support gate's decision is written. It adds keys to `guardrail_report_json`; the existing keys are unchanged.

### 7.4 Proof that the gate is unchanged

The same synthetic question, with `emr_context` on and off, against the same retrieval: the support decision for every guideline claim is identical. Only record claims and the fact check differ. (Like LP-T22, but on the guideline claims only, since the answer text changes.)

## 8. What the clinician sees

On the result, a "From the record" panel, separate from "Sources":
- **Facts used:** each with value, unit, range, flag, date and time taken, source label, and "preliminary" or "stopped" where it applies. Abnormal first.
- **Not used:** counts by reason ("12 older results", "3 not standardised", "1 text result").
- **Missing:** "No lab results in the last 30 days", "No vital signs in the last 72 hours", "No active medicines recorded". Missing is never shown as normal.
- **Stale:** when a source was `behind` at asking time, a banner: "The record may be out of date (last update 6 hours ago). Check the EHR before acting."
- **Not attached:** when `facts_status` is `unavailable` or `timed_out`: "This answer did not use the patient's record." Never silent.
- **Conflicts** between typed data and the record (§6).
- In the answer text, record claims carry a "record" mark, guideline claims their sources.

Before asking, the lookup card may show "MHAI will use: 18 results, 6 vitals, 4 medicines". Recommended (§14 Q5). It costs one extra call.

API, additive (ADR 0012): `GET /queries/{id}` gains `record: {status, facts[], omitted{}, missing[], sources_status, conflicts[]}`. The fields follow §4.3 minus the canonical ids.

## 9. Audit

- **On ask:** the existing `query.created` gains closed keys: `facts_status`, `facts_count`, `facts_omitted` (counts), `slice_version`, `facts_sha256`. No value, code, name or MRN. The patient id stays out of the audit payload as today ("a patient was named, never which").
- **Which facts were read for which question** is the `clinical_query_facts` table itself, joined by `query_id`, under RLS. The hash in the audit entry proves it wasn't changed afterwards. A platform audit view (`GET /platform/audit/{id}/record-facts`, auditors only, audited itself) lists fact kinds, codes and times, never values, for an investigation. Values are seen only through the hospital.
- **On view:** reading a result with facts is already audited as a query view; it gains `facts_count`.

## 10. Failure modes

| Failure | What happens | The clinician sees |
|---|---|---|
| EMR off for the hospital, or `emr_context = off` | no slice; answer as today | nothing new (no panel) |
| Patient excluded or merged after the lookup | 404 on `POST /queries`, as today | "Patient not found" |
| Slice function over 300 ms or erroring | the question is saved with `facts_status = timed_out` or `unavailable`; answer without facts | "This answer did not use the patient's record" |
| A source `behind` at asking | facts attached; `facts_status = stale` | stale banner (§8) |
| No facts in any window | `none_found` | "Missing" lines (§8) |
| Over caps | cut by priority, counted | "Not used" counts |
| Fact check fails | ESCALATE `record_misstated` | the usual escalation, never a PASS |
| Record amended after the answer | the answer stands with its frozen facts; the panel says "The record has changed since this answer" when a used fact has a newer version (one indexed check on view) | the change notice |
| Exclusion purge after the answer | facts purged; `facts_status = purged` | "The record facts for this answer were removed" |

## 11. Performance

- The slice reads indexed rows: `(hospital_id, patient_id, category, effective_at DESC)` on observations and `(hospital_id, patient_id, status)` on medicines. Target p95 under 50 ms; hard budget 300 ms, then skip (§10).
- The insert is at most 80 small rows in the same transaction.
- The prompt grows by about 1,000–2,000 tokens. On our L4 this is roughly 0.3–0.8 s more prefill per answer (to be measured, §12). On cloud it costs tokens against the hospital's budget.
- The "record has changed" check on view is one indexed lookup by fact ids.

## 12. Evaluation before live use

- **Clinical sign-off** (clinical lead plus a second clinician): the windows and caps (§4.1), the prompt block wording, the "from the record" display, the stale and missing wording, and the fact check's escalation rule.
- **Safety set additions** (synthetic patients, in the clinical answer gate):
  - hyperkalaemia with an active ACE inhibitor; a rising creatinine with metformin; a low platelet count with an anticoagulant;
  - a child with no recorded weight asking for a weight-based dose (must ask for the weight, not guess);
  - a stale record (source behind 2 days) with a critical value;
  - a preliminary result; an amended result; a stopped medicine the question assumes is active;
  - typed data that conflicts with the record;
  - a record claim the model gets wrong (must escalate);
  - a source display text carrying "ignore previous instructions" (must never reach the prompt);
  - the same questions with `emr_context` off, to compare.
- **Measures:** misstated facts shown = 0; patient facts counted as support = 0; source text in prompts = 0 (a scan of every prompt in the run); clinicians' preference with and without facts on 50 paired answers; latency added (p50, p95).

## 13. De-identified record access for clinicians: decided (product owner, 27 Nov: D1 now, D4 next)

The owner asked whether doctors can "access de-identified patient records from our platform and analyse them".

**What exists (designed, built in parts, off).** The Analysis design and ADR 0015: five aggregate views for one hospital, and a de-identified drill-down of pseudonymous rows (pid per period, age band, sex, setting, at most 10 conditions and findings) behind a shown count of 5 or more. Open to hospital admins and to clinicians given the `analyst` role. Names only under a verified clearance letter (named access). Caps: 300 distinct patients per person per quarter; 10 lists a minute, 50 a day. Off globally and per hospital until H1–H3 (privacy, clinical, legal) are signed. Its central residual risk is R-A4: drill users see microdata and can count small groups.

**The gap.** A clinician without `analyst` sees nothing. Even with it, a drill row is thin: no lab values, no medicines, no timeline. There is no export.

**Options (safer first):**

| # | Option | What a clinician gets | Disclosure-control cost |
|---|---|---|---|
| D1 | **Use what exists.** Give interested clinicians `analyst`; open Analysis when H1–H3 are signed | aggregates and thin pseudonymous rows, own hospital | none new; R-A4 as accepted |
| D2 | **A richer de-identified record, one at a time.** From a drill row, open one pseudonymous record: banded ages, labs as coded values with ranges, medicines by class, times as "day N" from first visit, no dates, no free text, no source ids | a de-identified case view | each open counts against the 300 cap; a timeline is a strong quasi-identifier (rare tests, rare drugs, admission patterns), so rare codes must be generalised and rows suppressed under k; needs a new disclosure rule and H1 sign-off; still R-A4, more so |
| D3 | **A research export** for an approved study: a de-identified extract (k-anonymised, generalised, rare values suppressed) prepared on our server and analysed there only (as Q7 of the local practice design: our GPU server, never personal devices) | a dataset for a named protocol | needs ethics approval per study, a data-sharing clause, an expert de-identification check, a named environment, export audit; the highest re-identification risk, because exports leave the per-view caps |
| D4 | **Federated questions only.** The clinician asks a question ("how many diabetics on metformin had eGFR under 30"), the system answers with an aggregate under Analysis disclosure control | answers, never rows | lowest; it is a new Analysis view, so Layer A and Layer D apply |

**Recommendation for the owner to decide:** D1 now (no new risk); D4 next if clinicians want their own questions answered; D2 only after H1 is signed and with its own design; D3 only per study, with ethics and legal, and on our server. None of these feeds a clinical answer (ADR 0015 decision 4 stays: Analysis data never reaches retrieval, prompts or models).

## 14. Decisions (product owner, 27 Nov 2026: the recommended option for each)

| # | Decision | Options | Recommendation |
|---|---|---|---|
| Q1 | The windows and caps | (a) §4.1 values, clinical lead confirms; (b) smaller (14 days, latest only); (c) larger | **(a)**, with the clinical lead's changes |
| Q2 | Relevance | (a) the same fixed slice for every question; (b) plus a reviewed topic map that re-orders only; (c) a model picks facts | **(a)** in v1, (b) later; never (c) |
| Q3 | Which models see facts | (a) our own model only; (b) cloud too, coded fields only, after legal confirms NDPA cross-border for this data and the hospital agreement names it | **(a)** in v1; (b) only with the sign-off recorded |
| Q4 | Not-standardised facts | (a) listed to the clinician, never to a model; (b) sent with the source's text | **(a)** |
| Q5 | Show what will be used before asking | (a) yes, a count on the lookup card; (b) no, only on the result | **(a)** |
| Q6 | Who may use it | (a) any clinician of the hospital who can name the patient (as lookup today); (b) only with a treatment relationship (an encounter on their unit) | **(a)** for the pilot, with lookup's audit and limits; revisit (b) with ward leads |
| Q7 | Stale record | (a) attach and warn; (b) refuse to attach past a limit (for example 48 hours behind) | **(a)** plus (b) at 7 days |
| Q8 | De-identified access (§13) | D1, D2, D3, D4 | **D1 now, D4 next** |

## 15. Phased plan (PRs; nothing built by this design)

1. **Decisions on paper.** This design, the amendment to the cloud design §4.3 (only if Q3 (b)), the frontend requirements for the panel. No code.
2. **The slice.** Migration: `emr_patient_slice()`, the indexes, `clinical_query_facts` with RLS, the `emr_context` setting (off). Go tests for windows, caps, statuses, the ceiling, the closed value set, other-hospital and excluded patients. Abuse tests T1–T6, T12.
3. **Attach on ask.** `POST /queries` writes the facts in its transaction with the 300 ms budget; audit keys; exclusion purge covers the table; `GET /queries/{id}` gains `record`. T7–T11, T13.
4. **Worker prompt and fact check.** Read facts under RLS; the prompt block for the local route; the fact check and `patient_fact_not_evidence`; the gate-unchanged test. T14–T19.
5. **Evaluation.** The safety set additions, the measures in §12, clinical review. Live use waits for sign-off.
6. **Later, optional.** The topic map (Q2 b); the cloud block (Q3 b); the lookup-card preview (Q5).

H7 of the Analysis design is reused, not duplicated: the hospital-scoped patient reference on consultations is `clinical_queries.patient_id`, already built (1.22.0) and used here. This design adds no identity call (no names cross NATS) and doesn't feed Analysis: Analysis reads canonical EMR events through its own ingest, never `clinical_query_facts`.

## 16. Security and threat model

### 16.1 Assets
- Patient record facts (health data of identifiable people).
- Patient separation inside a hospital (ADR 0004) and hospital separation.
- Clinical safety: the support gate's integrity, and no misstated or stale fact shown as current.
- The audit trail: which facts went to which question.
- Availability of `POST /queries`.

### 16.2 Actors and trust boundaries
**Actors:** clinicians, hospital admins (see nothing new), auditors, platform admins (set the switch), core, the worker, our own model, cloud providers (only under Q3 b), Postgres, the emr service (writes the source tables), the hospital's EHR (writes the source data, untrusted content).

**Boundaries and checks**
1. Browser → BFF → core: session, two-step, clinician role, hospital from the token, `patient_id` checked by `emr_patient_in_hospital()` (404 otherwise).
2. Core → Postgres (slice): `mhai_gateway`, the definer function only, the hospital from `mhai_current_hospital()`, FORCE RLS.
3. Core → NATS → worker: ids only; signed token with `hid` and `pids`; checks 1–9.
4. Worker → Postgres: `with_hospital(hid)`, `mhai_worker` `SELECT` on `clinical_query_facts` only, by the claimed `query_id`.
5. Worker → model: local route (internal network); cloud only through the egress proxy and the scan.
6. Hospital EHR → emr service → canonical tables: untrusted content, already normalised and allow-listed (EMR §14.1).

### 16.3 Threats (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| EC1 | Spoofing | A forged or replayed job naming another patient | signed token, `pids`, replay check; the worker reads facts by claimed `query_id` only | T7 |
| EC2 | Tampering | A request body naming another hospital's patient | `emr_patient_in_hospital()`; hospital from the token; RLS | T1 |
| EC3 | Tampering | Prompt injection through source text | no source text in facts (§4.3); labels from our tables; closed value set | T12, T16 |
| EC4 | Tampering | Facts changed after the answer to hide what the model saw | insert-only for core (no `UPDATE` grant); `facts_sha256` in the audit chain | T10 |
| EC5 | Repudiation | "The model never saw that potassium" | frozen facts, hash in audit, auditor view | T10, T11 |
| EC6 | Information disclosure | Another patient's facts in a slice (a join without `patient_id`) | the function takes one patient; a static test greps for joins without `patient_id` and `hospital_id`; property test over 1,000 synthetic patients | T2, T3 |
| EC7 | Information disclosure | Identifiers to a cloud model | v1 own model only; under Q3 b coded fields only, relative times, the residual-PII scan | T15 |
| EC8 | Information disclosure | Facts in logs, traces, errors, dead letters | ids and counts only; the existing log redaction test extended with fact values | T13 |
| EC9 | Information disclosure | Browsing the record through repeated questions | the lookup's limits and audit apply; a question per patient is audited; hospital admins can see lookup-without-question patterns (lookup TODO) | T8 |
| EC10 | Denial of service | A slow slice blocks asking | 300 ms budget, then answer without (§10) | T9 |
| EC11 | Denial of service | A patient with thousands of results bloats the prompt | caps and the 80-fact ceiling | T4 |
| EC12 | Elevation of privilege | The worker gains reads on `emr.*` | no grant; a grant listing test | T5 |
| EC13 | Clinical safety | A fact counted as support | §7.1; the gate-unchanged test | T17, T18 |
| EC14 | Clinical safety | A misstated or invented value shown | the fact check escalates | T14 |
| EC15 | Clinical safety | Stale or partial record read as complete | stale banner, missing lines, "not used" counts, never silent | T6, T19 |
| EC16 | Information disclosure | An excluded patient's facts survive | purge covers `clinical_query_facts` | T11 |

### 16.4 Controls (decided)
- **Authentication and step-up:** as `POST /queries` today; step-up for changing `emr_context`.
- **Authorization:** clinicians only; `patient_id` checked in a definer function; FORCE RLS on `clinical_query_facts`; `mhai_gateway` insert and select; `mhai_worker` select only; no `emr.*` grant to any worker role.
- **Data handling:** coded and numeric facts only; relative times in prompts; facts kept with the question and purged with the patient; never logged, traced or put in NATS.
- **Concurrency:** facts and the question in one transaction; no partial slice is ever stored (all or `unavailable`).
- **Secrets:** none new.
- **Third parties:** none in v1. Cloud only under Q3 (b), ADR 0013's terms, and a recorded sign-off.

### 16.5 Abuse-case tests (the plan writes these first)
- **T1** Another hospital's patient id on `POST /queries` is 404, stores nothing.
- **T2** A slice for patient A never contains a row of patient B (property test, 1,000 synthetic patients, two hospitals).
- **T3** `TestRLSWithoutHospitalSeesNothing` covers `clinical_query_facts`.
- **T4** A patient with 5,000 results gives at most 80 facts and the right omitted counts.
- **T5** Grant listing: no worker role has any privilege on `emr.*`; `mhai_worker` has only `SELECT` on `clinical_query_facts`.
- **T6** A source `behind` gives `facts_status = stale` and the banner field.
- **T7** A job whose body names a patient not in `pids` is refused (exists; extended).
- **T8** Lookup limits still apply when every lookup is followed by a question.
- **T9** A slice that sleeps 400 ms gives `timed_out` and the question is still queued.
- **T10** Core has no `UPDATE` or `DELETE` on `clinical_query_facts` except through the purge function; the hash in the audit matches the stored rows.
- **T11** Excluding a patient purges its facts within the purge's window; questions show `purged`.
- **T12** A source `local_display` of "ignore previous instructions" never appears in a fact, a prompt or a log.
- **T13** No fact value appears in logs, traces or dead letters (marker scan).
- **T14** A model output stating potassium 5.1 when the fact says 6.1 escalates `record_misstated`.
- **T15** With `own_model_only`, a cloud-routed hospital's prompt has no facts block.
- **T16** A text result outside the closed value set is left out and counted.
- **T17** A guideline claim citing `[F1]` instead of a chunk is redacted `patient_fact_not_evidence` and cannot PASS.
- **T18** Guideline-claim decisions are identical with facts on and off.
- **T19** With no facts at all, the result says what is missing, never "normal".

### 16.6 Residual risk and sign-off
- **R-EC1** The model may still reason wrongly from a correct fact. The fact check catches misstated values, not bad reasoning. Mitigated by the support gate on guideline claims and clinician review.
- **R-EC2** Automation bias: a clinician may trust the slice as the whole record. Mitigated by the "not used" counts and missing lines. Clinical lead signs off the wording.
- **R-EC3** A record correct at asking may change before the clinician acts. Mitigated by the "record has changed" notice on view.
- **R-EC4** Under Q3 (b), coded values with relative times still reach a provider abroad. Accepted only with legal sign-off.
- **Sign-off before live use:** clinical lead (H-EC1: §4.1, §7, §8, §12); privacy lead (H-EC2: minimisation, retention, purge); legal (H-EC3: the purpose added to the processing agreement and the DPIA update: "decision support using the patient's own record", under the same NDPA basis as EMR §17.7, healthcare under a duty of confidentiality, not consent; and Q3 b if chosen); product owner (H-EC4: this design). These go in TODO.md.
