# 0009. Integrate many EMR systems through one canonical model

- **Status:** Proposed
- **Date:** 2026-10-17
- **Deciders:** product owner, frontend lead, backend lead (to confirm)

## Context

MHAI will read patient records from many hospitals' EMR and EHR systems.
They differ in almost every way:

- **standards:** FHIR R4 (some R5), older HL7 v2 messages (ADT, ORU),
  vendor REST APIs, database or file exports;
- **delivery:** push (webhooks, FHIR subscriptions, HL7 v2 over MLLP) or
  pull (polling, bulk export);
- **codes:** standard or local codes for tests, diagnoses and
  medications, and different units;
- **identity:** each system has its own patient ids, MRN formats and
  location model (wards, beds, clinics, emergency);
- **auth:** OAuth 2 and SMART backend services, API keys, VPN or mutual
  TLS.

Patient alerts, Analysis and, later, "Ask MHAI about it" from a record
depend on this data. None of them should change when a hospital or an EMR
type is added. The frontend has already decided to stay EMR-agnostic
(mhai-web `docs/adr/0003-many-emr-systems.md`). This ADR is the backend
side.

## Decision

1. **One connector per EMR type, behind one interface.** A connector knows
   one system's transport, auth and format. It does three things only:
   fetch or receive events, map them to the canonical model, and build the
   "open this patient's record" link. A new EMR type is a new connector;
   nothing downstream changes.
2. **A canonical clinical model based on FHIR R4:** Patient,
   Encounter (with location), Observation, Condition,
   MedicationStatement and DiagnosticReport, stored in MHAI's own tables.
   Every record keeps its **provenance**: source system, connector
   version, source id and received-at.
3. **Normalisation in one place**, not in connectors:
   - test results and vitals to LOINC;
   - conditions to SNOMED CT and ICD-10;
   - medications to RxNorm or ATC;
   - units to UCUM;
   - reference ranges kept per source, as given.
4. **Unmapped codes are kept, flagged and never guessed.** The record keeps
   its local code and display text, is marked not standardised, and
   appears in a per-hospital mapping table for someone to map. Mapping
   tables need clinical review, like other clinical changes.
5. **A patient index per hospital.** It links each source's ids to one
   MHAI patient within that hospital, **never across hospitals**. The MRN
   shown is the hospital's own. Patient scoping (ADR 0004) applies to
   EMR-derived records too.
6. **Per-hospital connections**, managed by platform admins: connector,
   endpoint, sync mode, units covered, a connection test and health (last
   event, lag, error rate). **Credentials live in a secrets store**, never
   in the database, logs or the browser. Hospital admins see status only.
7. **One event pipeline:** connectors → queue → normalise → store → the
   alert rules and the analysis jobs. It is **idempotent by source id**,
   so replays and duplicates are safe. Failures go to a **dead-letter
   queue** that platform admins can see and retry, as for other jobs.
8. **Alerts and analysis run only on the canonical model**, never on
   vendor data, so one rule or one view works for every hospital.
9. **Read-only.** Writing back to an EMR is out of scope. If it is added
   later, it goes through the same connector interface with its own
   approval and ADR.
10. **Vendors are invisible to the frontend.** The API never returns
    vendor or product names, vendor codes or formats.
    - Each connected system has a hospital-chosen `source_label` (for
      example "Main record"); the API returns that label only.
    - Alerts and findings carry an `emr_link` built by the backend; the
      frontend never builds EMR URLs.
    - Codes, units and locations are returned normalised with display
      labels, plus `standardised: false` for unmapped findings and a
      "not recorded" location when there is none.
11. **A clinician stays in the loop.** Alerts and suggested causes go to
    clinicians or the hospital, **never to patients**. Suggested causes
    are labelled as suggestions with a confidence word, never as a
    diagnosis.

## Consequences

- Adding an EMR type is a connector plus mappings, with no change to
  alerts, analysis or the frontend.
- The canonical model and mappings are real work, and they are where
  clinical safety lives. Unmapped codes stay visible rather than dropped.
- Provenance on every record supports audit and legal requests.
- Showing patient names and MRNs in alerts and named drill-down needs
  clinical, privacy and legal sign-off before live use. SKILLS.md rules
  hold: no PHI in logs, and queue messages carry ids only.
- New data model: hospitals, connections, canonical records, the
  per-hospital patient index, mapping tables and alerts. The gateway
  design (`docs/specs/2026-10-08-api-gateway-phase1-design.md`) needs
  matching endpoints; see `docs/specs/2026-10-17-frontend-requirements.md`
  §9, §10 and §12.
- Follow-ups: the first connector, the alert rules (each a
  `clinical-review` change), a secrets store choice, and retention for
  EMR-derived records.

## Alternatives considered

- **Point-to-point code per hospital:** fast for one hospital, but every
  rule and view would be rewritten per vendor.
- **Normalising inside each connector:** spreads code mapping across
  connectors, so mappings drift and are hard to review.
- **A shared patient index across hospitals:** would link patients
  between tenants; refused for privacy.
- **Running alerts on raw vendor data:** ties clinical rules to one
  vendor's codes and makes them untestable across hospitals.
- **Letting the frontend call EMRs directly:** would put credentials and
  vendor logic in the browser.
