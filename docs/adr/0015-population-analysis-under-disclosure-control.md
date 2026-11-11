# 0015. Allow population analysis for one hospital under disclosure control

- **Status:** Accepted 2026-11-07 by the product owner (H4: the Analysis backend design approved after three reviews, with this ADR and the SKILLS.md §1 amendment)
- **Date:** 2026-11-07
- **Deciders:** product owner
- **Amends:** the frontend requirements (`docs/specs/2026-10-17-frontend-requirements.md`) §3, the rule that hospital admins see no clinical content; `SKILLS.md` §1, "Never build anything that spans patients".
- **Design:** `docs/specs/2026-11-07-analysis-backend-design.md`
- **Plan:** `docs/plans/2026-11-07-analysis-backend.md`

## Context

mhai-web has built an Analysis page (five views, filters, a de-identified drill-down and an Analyst role) and a clearance-letter workflow, on sample data. The product owner wants population and disease analysis for hospital admins and analysts, beyond the Usage counts.

Two rules stand in the way:

- **Contract §3:** a hospital admin sees "no consultations or patient data". Hospital admins aren't clinicians.
- **SKILLS.md §1:** "Never build anything that spans patients." The rule exists because one patient's data once reached another patient's answer (#1). Analysis is cross-patient by definition.

Both rules protect real things. A count of 1 to 4 can identify a person. A de-identified row still describes one person. Names are the most sensitive output of all. Analysis data must also never reach a clinical answer, which is what §1 was written to stop.

The product owner ruled that statistics show only finished, non-overlapping calendar periods (R41a), refined to nested periods (C3), with exact counts and no noise. The design builds disclosure control on those rulings.

## Decision

1. **Aggregates and pseudonymous rows, for one hospital.** Hospital admins and analysts (clinicians granted the `analyst` role) may see, for their own hospital only:
   - aggregates over its patients, under the design's disclosure control (§5: Layer A per response, Layer D across periods, levels and filters);
   - de-identified, pseudonymous rows behind a shown count of 5 or more, under role, audit, row limits, pids stable within one period only, and a per-person cap on distinct patients (§5.6).
2. **Names, only under a verified letter.** A name is shown only when a verified, current clearance letter covers the person, the named-access switch is On, and the person gives a purpose (§8). Every named view is audited and flagged. Names are never stored, logged or cached.
3. **Contract §3 changes in those two places only.** Everything else a hospital admin can't see today (consultations, answers, one patient's record) stays out of reach.
4. **The SKILLS.md §1 exception is scoped to one package.** `services/core/internal/analysis` may aggregate across patients of one hospital, and serve de-identified rows, under its disclosure rules. The rule stays absolute for intelligence, retrieval, prompts, models and every NATS operation. No worker role, NATS subject or operation touches an analysis table, function or result (T22). The review checklist gains a line for it.
5. **Off by default.** The analysis switch is Off globally and per hospital. While it's Off, every `/analysis/*` route answers 404 and ingest writes nothing. No live hospital is turned on until clinical, privacy and legal sign-off (H1 to H3) are recorded.
6. **The rules live in core.** Every number passes through `internal/smallgroup` or `internal/analysis/disclose` in Go. The hospital comes from the session and, in the database, from RLS. Raw counts never leave core. Python gets nothing (ADR 0010).

## Consequences

- Hospitals get disease and population figures for their own patients. Drill-down users see pseudonymous microdata. The aggregate rules protect people **without** drill access, and the drill controls limit and record what drill users do (design §5.2).
- Five residual risks are accepted, each with a measure or a sign-off:
  - **R-A1:** differencing across filter families within one period. Measured by P2 at a filter cap of 2; the privacy lead accepts it with that number (H1).
  - **R-A2:** narrowing by bounds, within responses and across levels for distinct counts. Measured by P5.
  - **R-A3:** two releases about one hospital, its own Analysis and platform Analysis (Plan 12), at different filter granularities. Only someone with both kinds of account can combine them.
  - **R-A4:** drill-down users see microdata. Their counts are exact, and they can count small subgroups. The controls limit and record this but don't prevent it. This is the central privacy question (H1).
  - **R-A5:** the screen itself (screenshots).
- More to build and to review: frozen periods, stored bodies, the disclosure engine with solvers in its tests, the analyst trigger, letters, named access and the legal resolution path (plan, PRs 2 to 10).
- More to audit: the analysis, analyst, clearance and named-access actions (design §8.7).
- mhai-web's contract changes (H6): a period picker replaces custom ranges, pids are stable per period rather than per hospital, and new error codes appear.
- A reviewer now has a second question under §1: not only "does this span patients?", but "is it inside `internal/analysis`, and does anything from it reach a worker or NATS?".

## Security and threat model

The full model is the design's §13 (assets, boundaries, STRIDE threats AN1 to AN23, controls, abuse-case tests T1 to T29 and P1 to P8, residual risks and sign-off). In short:

- **Assets:** patients' identities and conditions; hospital separation; the audit trail; the guarantee that a frozen period always gives the same answer.
- **Boundaries:** browser → BFF → core (two-step, the hospital from the session) → Postgres (RLS on `mhai_current_hospital()`, definer functions, no grant to worker roles). The only new NATS call is core → EMR service to resolve names for a named view, one audited call, nothing cached.
- **Threats closed by this decision's scope:** analysis data reaching a clinical answer (AN17: no worker grant, no NATS operation, the checklist line); a hospital id taken from a request (session and RLS); a small group worked out by someone without drill access (Layer A and Layer D).
- **Sign-off:** clinical (H2), privacy (H1) and legal (H3), before any live hospital. They are gates in TODO.md.

## Alternatives considered

- **Keep both rules absolute.** No Analysis at all. Declines the product owner's request, and leaves mhai-web's built screens without a backend.
- **Noise instead of suppression.** Rejected: fixed periods remove differencing over time, and exact drill-down counts would undo noise anyway (design §16, question 1).
- **Rolling windows and custom ranges, as mhai-web built them.** Rejected by R41a: overlapping windows can be subtracted to reveal a hidden count.
- **Compute Analysis in the Python services.** Rejected: the security rules are written once, in Go (ADR 0010), and §1 stays absolute for intelligence.
- **A repo-wide exception to SKILLS.md §1.** Rejected: an exception scoped to one Go package can be checked in review and by a test (T22). A wider one can't.
