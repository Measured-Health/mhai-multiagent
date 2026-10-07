# 0006. Persist the knowledge base in Postgres with pgvector and RLS

- **Status:** Proposed
- **Date:** 2026-10-07

## Context

The KB lives in memory. It's rebuilt on start, chunk ids change on every
rebuild, approved corrections need a full re-index, and patient isolation
relies on application code only. See #9.

## Decision (proposed)

Store chunks in Postgres (already in the stack) with pgvector embeddings and
full-text search. Enforce row-level security keyed on a per-request patient
setting, use content-addressed chunk ids, and index incrementally.

## Consequences

Isolation holds even if application code regresses. Citations stay stable
for the audit trail. This adds a migration path and makes database
performance part of retrieval latency.
