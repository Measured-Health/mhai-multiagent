# 0010. Write the API in Go, keep the models in Python, and connect them with NATS

- **Status:** Accepted (product owner, 2026-10-26)
- **Date:** 2026-10-26
- **Supersedes:** 0007 (API in Python, contract-first), which was proposed on `feat/8-api-gateway` and never merged
- **Amends:** 0008 (models behind inference services): the services are reached over NATS instead of HTTP
- **Naming (product owner, 2026-10-27):** the Go service this ADR calls for is named the core service and lives in `services/core/`; the Python side is `services/intelligence/` (package `intelligence`), and the NATS config is in `services/nats/`. The NATS user stays `gateway`.

## Context

ADR 0007 chose Python for the API so that the API and the worker would share one copy of the security rules. It also kept the API replaceable: "If API throughput or latency becomes a measured problem, the API can be rewritten (for example in Go)".

Since then, three things have changed:

- **More hospitals.** MHAI is about to go from one hospital to several. The product owner is in talks with more hospitals now, and every hospital adds staff, EMR connections, alerts and audit.
- **The frontend contract is defined.** mhai-web's two apps call a fixed contract (`docs/specs/2026-10-17-frontend-requirements.md`). Most of it is sign-in, tenancy, platform operations and EMR plumbing, not model work.
- **The team knows Go.** Two of the three engineers already write Go.

No Go API code has been written yet, so changing language now costs nothing.

## Decision

- **The core service (Go)** owns the API and the rules around it:
  - the API, authentication, sessions, authorization and tenancy;
  - audit, and the EMR connectors and event pipeline;
  - every endpoint in the frontend contract.

  It is the only service the frontends' servers call.
- **Python services** keep the models: the agents, retrieval and the knowledge base, guardrails, PII masking and inference. They are wrapped, not rewritten.
- **NATS connects them, and nothing else does.**
  - Request/reply carries calls someone is waiting on.
  - JetStream carries background jobs.
  - Each service has its own NATS credentials, and only the core service (NATS user `gateway`) may publish requests to the Python services.
- **Python verifies every request before answering.** Each request carries a short-lived token signed by the core service (EdDSA). Each Python service checks:
  - the signature;
  - the audience;
  - expiry;
  - replay;
  - that the hospital in the token matches the data asked for.

  If any check fails, the service refuses and does no work.
- **Security rules have one owner.** The permission table lives in Go. The Python side's checks are generated from it, so the two can't drift.
- **One shared deployment.** Every hospital's data is kept separate in one database, enforced by row-level security as well as by the code.

The design is in `docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`.

## Consequences

- The team works in the language it knows for most of the backend. Python stays where the ML ecosystem is.
- There are two languages to build, test and deploy. CI gains a Go job, and the repository holds both, with the Go code in `services/core/` and the Python code in `services/intelligence/`.
- The risk ADR 0007 avoided, two copies of the security rules, is contained differently:
  - Go is the only place rules are written;
  - Python only verifies a signed decision and checks hospital scope.
- NATS is a new piece to run. It replaces the Redis job queue, which was a stub (`services/intelligence/intelligence/queue`). Redis stays only if a cache is needed.
- Plan 1 (inference boundary) is rewritten for NATS before it is built.
