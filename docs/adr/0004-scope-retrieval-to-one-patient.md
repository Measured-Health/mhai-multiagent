# 0004. Scope retrieval and citations to one patient

- **Status:** Accepted
- **Date:** 2026-10-07

## Context

All patients' records were in one index, and retrieval never filtered by
patient, so a query could surface and cite another patient's data (#1).
Reserved values such as `reference` also doubled as patient ids.

## Decision

Every retrieval and citation check takes a `patient_id`. A chunk is visible
only to its own patient. Shared guideline chunks are tagged `reference` and
are visible to all. Reserved ids are refused at ingestion and at query
time. With no patient, only guidelines are returned. Visibility is decided
in one function, `chunk_visible_to()`.

## Consequences

Cross-patient leakage through retrieval is closed and tested
(`tests/security`). The filter is still applied in application code after
scoring; database-level isolation is the follow-up (#9).
