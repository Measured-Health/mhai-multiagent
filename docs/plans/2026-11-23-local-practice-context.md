# Local practice context and shared improvement: implementation plan

**Status:** Proposed (23 Nov 2026), with the design. Nothing is built. No PR below starts until the product owner approves the design (H-LP4).

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Import de-identified visit data per hospital, under a recorded agreement and scope; serve guarded practice figures (own, k ≥ 10; pooled, ≥ 20 visits and ≥ 3 hospitals) to clinicians beside PASS answers and to admins in Analysis; gate model-improvement use on the scope. Practice is never evidence. Every PR leaves the feature off.

**Architecture:**

- **Database (core's migrations):** `lp_agreements`, `lp_switch`, `lp_batches`, `lp_visits`, `lp_visit_codes`, `lp_bodies`, `lp_rule_sets`, `lp_rules`; roles `mhai_lp_importer` (insert only), `mhai_lp_reader` (definer reads of stored bodies), `mhai_lp_pool` (the one cross-hospital reader, scheduler only). `FORCE` RLS on all.
- **Go:** `services/core/internal/localpractice` (normaliser, guard, bodies, routes), built on `internal/smallgroup` and `internal/analysis/disclose`. core-cli: `core local-practice agreement|import|delete|scope|rules|window`.
- **Nothing reaches intelligence.** No worker grant, no NATS operation, no `kb_chunks` write.

**Spec:** `docs/specs/2026-11-23-local-practice-context-design.md`. Section numbers, R-LP*, LP*, LP-T*, H-LP* and Q* are the design's.

**Migrations:** the highest on `main` is 00056; 00051 is taken by the open documents PR (#230). This plan uses **00057 to 00060**, renumbered to the next free ones at build time, with the migration-count tests updated.

## PRs

| PR | Tasks | What | Depends on | Sign-off |
|---|---|---|---|---|
| 1. Decisions on paper | 1 | ADR 0020, the SKILLS.md §1 exception and checklist line, contract notes for mhai-web, TODO gates | design approved | H-LP4 |
| 2. Agreements, switch, storage | 2–3 | migration 00057; `agreement record/approve/narrow/revoke`; switch | 1 | none |
| 3. Normaliser | 4 | formulary pin, deterministic mapping, property tests | formulary library merged (`feat/formulary-guidelines-testset`) | none |
| 4. Importer | 5–6 | `import` (path checks, manifest, dry run, stage), `delete`, retention sweep | 2, 3; de-identification TODO (§5.1) | none |
| 5. Own bodies | 7–8 | migration 00058; window; guard (k, complements, D3); stored bodies; membership-change rule for own bodies | 4 | H-LP1 reviews constants |
| 6. Analysis Practice view | 9 | `GET /analysis/practice` under the Analysis machinery | 5; Analysis switch | H1–H3 (Analysis) |
| 7. Divergence rules | 10 | migration 00059; rule file format, two-clinician check, pin, `rules load` | 5 | H-LP2 (rule set) |
| 8. The panel route | 11–12 | `GET /queries/{id}/local-practice`, permission, audit, OpenAPI; the evidence-isolation tests | 5, 7 | H-LP2 |
| 9. Pooled bodies | 13–14 | migration 00060; `mhai_lp_pool`; scheduled run; contributor, dominance, rounding; membership-change rule | 5, 8 | H-LP1, H-LP3 |
| 10. Model-improvement scope gate | 15 | `core local-practice scope`; the test-set builder and training export call it; export audit | 2; the private test-set builder | H-LP3, Q7 |

mhai-web builds the panel and the Practice view against the OpenAPI changes of PRs 6 and 8 (its own repository and plan).

## Tasks

Each task starts with its abuse-case tests (design §13.5), written failing first.

### PR 1: decisions on paper

### Task 1: ADR 0020 and the rules

- [ ] Write `docs/adr/0020-local-practice-context-never-evidence.md` (Proposed): R-LP1 to R-LP3; the scoped exception for `internal/localpractice` (across patients of one hospital, and across hospitals for pooled bodies only); the constraint in design §6.6 (a wider query surface reopens the noise question).
- [ ] SKILLS.md §1: add the exception beside ADR 0015's, and the checklist line (design §3).
- [ ] `docs/specs/2026-10-17-frontend-requirements.md` §7: the panel route and its rules (PASS only, separate from citations, label text, one 404); §10: the Practice view.
- [ ] TODO.md: H-LP1 to H-LP4, the per-hospital onboarding record, Q1 to Q9.

### PR 2: agreements, switch, storage

### Task 2: Migration 00057

**Tests first:** LP-T11 (RLS on every table), LP-T15, LP-T20 (grant listing), LP-T17 (the `CHECK`s).

- [ ] Roles `mhai_lp_importer`, `mhai_lp_reader`, `mhai_lp_pool` (NOLOGIN, NOINHERIT, no members), as 00015–00017 create theirs.
- [ ] Tables `lp_agreements`, `lp_switch`, `lp_batches`, `lp_visits`, `lp_visit_codes`, `lp_settings` (`k >= 10`, `k_pool >= 20`, `h_min >= 3`, `d_max <= 0.60`). `ENABLE` and `FORCE` RLS, `REVOKE ALL FROM PUBLIC`.
- [ ] `provision_append` gains the `local_practice.*` provisioning actions.
- [ ] The hospital removal checks list local-practice data and delete it.
- [ ] Down reverses everything.

### Task 3: Agreement and switch commands

**Tests first:** `same_person` refused; a narrowed scope refuses an import at once; LP-T25 for agreement actions.

- [ ] `core local-practice agreement record|approve|narrow|revoke`, step-up per person, two people for record and approve.
- [ ] Switch raise (two-person, needs the agreement and the clinical sign-off reference for `panel`) and lower (one person).
- [ ] Audit catalogue entries, withheld for mhai-web.

### PR 3: normaliser

### Task 4: Deterministic mapping

**Tests first:** LP-T12 (injection text gives a code, never text), property tests with synthetic rows: no output string equals any input cell; brand to generic; unmapped counts.

- [ ] Pin the formulary artefact (version and SHA-256 compiled in), refuse a mismatch.
- [ ] Age bands (Analysis's, plus `0-4` and `5-17`), sex, conditions (cap 3), drugs to generic and class, the closed lab list, the malaria set's mapped columns.
- [ ] Duplicates within a file and month dropped and counted.
- [ ] No free text in any return value, error or log (LP-T18).

### PR 4: importer

### Task 5: `core local-practice import`

**Tests first:** LP-T13, LP-T14, LP-T15, LP-T18; a half-bad file writes nothing.

- [ ] Root-only path checks with `O_NOFOLLOW` and the resolved path; the recorded folder; manifest hospital equals the argument; size cap.
- [ ] One transaction per batch, `SET LOCAL ROLE mhai_lp_importer`, advisory lock per hospital.
- [ ] `--dry-run` (counts only), `--stage` (switch Off).
- [ ] `local_practice.imported` with counts and hashes.
- [ ] deploy/README.md: "Importing a hospital's visit data", stating that only already de-identified exports may be imported until the pipeline's de-identification step exists (TODO.md, "Hospital visit data: de-identification in the pipeline").

### Task 6: Deletion and retention

**Tests first:** deletion removes visits, codes and own bodies; batch metadata and counts remain; the sweep removes visits older than `LP_HISTORY_MONTHS`.

- [ ] `core local-practice delete --hospital [--batch]`, step-up, reason, audit with counts.
- [ ] Monthly sweep in the scheduler.

### PR 5: own bodies

### Task 7: Migration 00058 and the window

**Tests first:** LP-T3.

- [ ] `lp_bodies` (insert-only per version), `lp_windows`; definer `mhai_lp_body_get` for `mhai_gateway`.
- [ ] `core local-practice window set`: one finished period, non-overlapping, audited.

### Task 8: The guard

**Tests first:** LP-T1, LP-T2 (solver in the tests), LP-T4.

- [ ] Body per `(hospital, window, condition)` and per month and quarter for Analysis, guarded with `smallgroup` at `Min = k`, complements, partitions with `AcrossRows`, D3 with child periods.
- [ ] Whole percentages; hidden-whole below `k`; `ErrInconsistent` stores hidden whole with an alert.
- [ ] The membership-change rule for own bodies (replace within rounding, else withdraw).
- [ ] Bodies computed by the scheduler after an import, never on request.

### PR 6: Analysis Practice view

### Task 9: `GET /analysis/practice`

**Tests first:** 404 with either switch below its position; filters capped at 2 (age band, sex); `Min = 10`; no drill keys; `analysis.viewed` with `view = practice`.

- [ ] Route in `internal/analysis/routes`, reading stored own bodies.
- [ ] OpenAPI (minor version), contract note for mhai-web.

### PR 7: divergence rules

### Task 10: Rule sets

**Tests first:** LP-T19, LP-T23; a version mismatch gives no note.

- [ ] Migration 00059: `lp_rule_sets`, `lp_rules` (shared, no hospital), loaded by the provisioning login only.
- [ ] File format `models/practice-rules/<guideline>-<version>.json`: rules, author, approver (distinct), clinical sign-off reference. A synthetic example file for tests only.
- [ ] `core local-practice rules load`, pinned by SHA-256.
- [ ] Matching on shown cells only, threshold, priority, one note.

### PR 8: the panel route

### Task 11: `GET /queries/{id}/local-practice`

**Tests first:** LP-T10, LP-T24, LP-T25, LP-T28, LP-T29.

- [ ] Permission `local_practice:read` for clinicians (permgen).
- [ ] `created_by` and hospital as the result route; PASS and verified only; condition from `icd10_code` or the condition dictionary; own body, else pooled when the scope allows.
- [ ] One 404 body for every refusal; `local_practice.viewed` with counts.
- [ ] OpenAPI (minor version).

### Task 12: Evidence isolation

**Tests first:** LP-T20, LP-T21, LP-T22.

- [ ] A grant test listing every role's privileges on `lp_*` and `kb_chunks`.
- [ ] A test that scans `operations.json` and the NATS subjects for practice fields.
- [ ] A boundary test: one synthetic question, feature on and off, byte-identical `guardrail_report_json`, `hga_decision`, `trust_score`, `citations`.

### PR 9: pooled bodies

### Task 13: Migration 00060 and the pooled run

**Tests first:** LP-T5, LP-T6 (solver), LP-T8, LP-T9, LP-T16.

- [ ] `mhai_lp_pool` and `mhai_lp_pool_run()`: contributors by scope at run time; cells under `k_pool`, `h_min`, `d_max`; 5-point rounding; generics and classes only; no hospital id in the body.
- [ ] Scheduler job, global advisory lock, alert `local_practice_pool_failed`; no execute grant to `mhai_gateway`.

### Task 14: The membership-change rule for pooled bodies

**Tests first:** LP-T7.

- [ ] Track each body's contributor set and per-contributor share (stored server-side only, never served).
- [ ] On a change: replace when every changed hospital's share of every shown cell is ≤ 25% in both versions and §6.4 still holds; otherwise withdraw the window.

### PR 10: model-improvement scope gate

### Task 15: `core local-practice scope`

**Tests first:** LP-T26, LP-T27.

- [ ] `core local-practice scope --hospital <id> --purpose testset|training_export` answers allowed or refused, and audits it.
- [ ] The private test-set builder (on `feat/formulary-guidelines-testset`) calls it before reading a folder, writes splits only under `/srv/mhai-data/testsets/` (root-only), and keeps a server-only hospital key per case for revocation.
- [ ] The training export: refuses without the scope, encrypts to the recipient key named in the agreement, writes `local_practice.training_exported`. Blocked on Q7 for real data.
- [ ] A repository check that no workflow references `/srv/`.

## Rollout

1. All PRs merged with the global switch off and every hospital `off`.
2. Per hospital: the onboarding record (two platform admins), the folder in place, `import --dry-run` reviewed (counts only), then `import --stage`.
3. The clinical lead reviews the hospital's own bodies in a staged dry run on the server (figures only, no rows) and the rule set (H-LP2).
4. Raise to `analysis_only`, then `panel`, each two-person, after H-LP1 to H-LP3.
5. Pooled bodies start at the first window with three contributing hospitals; until then no pooled panel exists.

## Running the tests

All tests use synthetic, seeded visits. CI runs them as part of the core suites. No real data is used anywhere in a test, a fixture or a workflow.
