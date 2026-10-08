# 0007. Write the API in Python, contract-first so it can be replaced

- **Status:** Accepted
- **Date:** 2026-10-08

## Context

The API gateway (#8) sits between mhai-web and the worker. Some comparable
systems have moved their API and orchestration layers to Go for
throughput and simpler deployment, keeping Python only for ML code.

In MHAI, a clinical query spends 8–30 seconds in retrieval, LLM
generation and NLI verification, all inside the worker, and all in
Python (torch, transformers, sentence-transformers). The API's own work
takes milliseconds at Phase 1 load: verify a token, write a row, enqueue a
job, read a status. The API and the worker also share security-critical
code: request schemas, the RBAC permission matrix, and the patient-id and
user-id rules in SKILLS.md.

## Decision

Write the API in Python (FastAPI), in the same package as the worker,
and keep it replaceable:

- The API never imports models, the orchestrator or the KB. It talks
  only to Postgres and Redis.
- The committed `openapi.json` is the contract. mhai-web generates its
  types from it, and CI fails if the app and the committed schema
  diverge.
- Jobs on Redis carry only ids and a documented, versioned payload.
  State lives in Postgres tables owned by migrations, not by Python
  objects.

## Consequences

- One language and one copy of the security rules for the API and the
  worker; no drift between two implementations.
- If API throughput or latency becomes a measured problem, the API can be
  rewritten (for example in Go) behind the same OpenAPI contract, job
  format and schema, without changing the worker or the frontend.
- The boundary has to stay clean. An API change that imports model or
  orchestrator code breaks this decision, and should be caught in review
  (and by an import-boundary test).

## Alternatives considered

- **Go API plus a Python worker now:** better raw throughput, but two
  implementations of auth, RBAC and validation to keep in sync, for no
  measured need at Phase 1 load.
- **The API loads the models itself:** rejected in the #8 design. A heavy
  API process loses in-flight work on restart and can't scale
  independently.
