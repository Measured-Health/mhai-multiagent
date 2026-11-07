# 0014. A separate EMR service with its own credentials and egress, and a reviewed route for scanned records

- **Status:** Accepted 2026-11-07 by the product owner (the EMR integration design, approved as recommended)
- **Date:** 2026-11-07
- **Deciders:** product owner
- **Extends:** [ADR 0009](0009-integrate-many-emr-systems.md) (one canonical model for many EMR systems) and [ADR 0010](0010-api-in-go-models-in-python-over-nats.md) (Go for "the EMR connectors and event pipeline")
- **Amends:** the document pipeline design (`docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`) §2 and §10: normalisation moves to the emr service
- **Design:** [`docs/specs/2026-11-07-emr-integration-design.md`](../specs/2026-11-07-emr-integration-design.md) §3, §5.3, §6 and §9.0; **plan:** [`docs/plans/2026-11-07-emr-integration.md`](../plans/2026-11-07-emr-integration.md)

## Context

ADR 0009 decides what the backend reads (one connector per EMR type, a canonical model, normalisation in one place, a per-hospital patient index) but not where it runs. Two facts from the design work decide that:

- **Connectors hold outside credentials and parse outside data.** A connector holds a hospital's record-system keys and reads JSON and files we don't control. AGENTS.md put "EMR plumbing" in core, which also holds sessions, accounts, the audit chain and the request-signing key.
- **Havana's record system holds no structured clinical data** (design §1.1). It is an archive of scanned paper records, named after the patient. Values can come from it only through the document pipeline, with a person reviewing every extraction and every patient match. The document pipeline design had its own normalisation step, which would be a second copy of the code and unit maps.

## Decision

1. **A separate service, `services/emr/` (Go, its own module and binary),** holds every outside credential and the only route out to hospital systems. Core holds neither.
   - Credentials live in a secret store only the emr service can read; core seals each credential to the emr service's public key at once and never stores it (design §6).
   - The emr service reaches hospitals only through its own egress proxy, `egress-emr`, which resolves once, checks and pins each address, per connection (design §6.4).
   - Core calls it only on `emr.v1.ctl.<op>` with a request token (`aud: emr`), verified as the workers verify theirs; it publishes ids only. It has a narrow database role and no access to accounts, sessions, `audit_log`, alerts or jobs (design §3).
2. **The emr service decides what is abnormal; core decides who sees it.** Routing, permissions, read state and audit stay in core (ADR 0010). The emr service never decides a recipient, never serves the frontend and never writes the audit chain.
3. **The pieces both services need are a shared Go module, `services/gokit/`:** request-token verification with a subject table, a replay set, `WithHospital` taking a role, log redaction and the control-operations table. The emr module imports nothing from `services/core/`.
4. **One normaliser, in the emr service, for connectors and documents.** The document pipeline proposes codes for review; what the reviewer confirms is written as a raw record (`source_kind = document`) and normalised by the emr service like any connector's record. Its worker gets `INSERT` on `emr.raw_records` only (design §9.0).
5. **Scanned records reach the canonical model only after review.** The `document_archive` connector lists new files and hands them to the documents service; it produces no canonical record itself. A file name is PHI and never used to match a patient (design §5.3).
6. **Off by default.** No connection can be saved, no emr process runs, no stream exists and no alert is raised until a person switches each piece on (`CORE_EMR_ENABLED`, the `emr` compose profile, an active rule set).

## Consequences

- A compromise of core doesn't hand over hospital credentials, and a compromise of a connector doesn't reach sessions, accounts, the audit chain or the signing key.
- One more service to run and watch, and a shared module whose defect reaches core and the emr service at once; it is reviewed as security code.
- Core's Docker image builds from `services/` (it needs `services/gokit/`), so the build context changes in compose, CI and the deploy workflow.
- Clinical maps are reviewed once, in one place.
- Havana's scanned records give alerts only after review (hours to days). Timely alerts need a structured source from the hospital (design §20, H1).
- A compromised emr host can read every connected hospital's credentials; one emr process per hospital is not in this design (design §17.6).

## Alternatives considered

- **Connectors inside core:** one service fewer, but core would hold every hospital's keys and parse hostile input next to the audit chain and the signing key.
- **One emr process per hospital:** stronger isolation, more to run; left for later.
- **Normalisation in the document pipeline as well:** two copies of the maps in two languages, which drift.
- **Matching patients from archive file names:** a guess on PHI; refused.
