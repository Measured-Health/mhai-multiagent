# 0011. Bring the frontend's traffic in over NATS, and let intelligence ask core before serving it

- **Status:** Accepted (product owner, 2026-10-28)
- **Date:** 2026-10-28
- **Deciders:** product owner
- **Amends:** 0010 (API in Go, models in Python, over NATS)

## Context

ADR 0010 put NATS between Go and Python and kept HTTP between the frontends' servers (the BFF) and core. The BFF proved itself to core with a shared service key (`X-MHAI-Service`), and only core could call intelligence.

On 28 Oct the product owner decided: "frontend talks to nats which can either talk to core or intelligence, when it want to talk to intelligence, intelligence ask core if permission has been given before it go ahead with it".

The constraints that stay: permission rules are written once, in Go; intelligence never trusts a hospital, user or role from a payload; jobs are published only by core; any doubt means refuse.

## Decision

- **The BFF is a NATS client,** on the Node server side only (nats.js over TCP), never in the browser. Each app has its own NATS user (`bff_clinical`, `bff_platform`) and inbox prefix (`_INBOX_bff.<app>`). It may publish only its app's API subject and, for the clinical app, the intelligence operations marked `bff_direct`. It has no access to jobs, KV, JetStream, dead letters or grants.
- **Core serves the frontend contract over NATS** on `api.v1.clinical` and `api.v1.platform`, through a thin adapter that runs each message through the same `http.Handler` chain Plan 2 built (request id, body limit, principal, route rule, idempotency, RLS transaction, audit). The audience comes from the subject.
- **The BFF's NATS identity replaces the `X-MHAI-Service` key** on that path. The key and the NKey seed would sit in the same place, so doubling them adds a secret, not a barrier. HTTP stays only during the switch-over, and afterwards serves only health probes on localhost.
- **The BFF may call intelligence directly** for `bff_direct` operations, sending the user's access token. Before any work, the worker asks core on `authz.v1.grant.<service>`. Core checks the token, session, two-step state, the operation's rule and the hospital and patient scope, audits the decision, and answers with a per-call request token bound to the subject, body, service and the BFF's reply inbox. The worker then runs all its existing checks on that token. Core unreachable or late means refuse.
- **Jobs stay core's.** A BFF request that starts a long job goes to core.

The design is §17 of `docs/specs/2026-10-28-intelligence-boundary-design.md`.

## Consequences

- One transport between every backend part. The BFF needs no HTTP route to core, and core needs no public HTTP listener after the switch.
- Plan 2's request handling is reused as it is; the adapter is small, and a contract test runs every route both ways.
- A BFF-direct call costs one extra NATS round trip (about 1 ms) plus a token verify and a sign in core. Core-initiated calls cost nothing more.
- Core is now on the path of every BFF-direct call: if core is down, those calls fail. That is the intended fail-closed behaviour.
- User access tokens now reach intelligence workers on BFF-direct calls. They are redacted in logs and spans, and a worker can spend one only on its own grant subject. No production operation is `bff_direct` until a need is reviewed.
- NATS carries the frontend's API traffic, so TLS with `verify` is required in production.
- mhai-web's BFF must switch from `fetch` to a NATS client. That is mhai-web work.

## Alternatives considered

- **Keep HTTP from the BFF to core (ADR 0010 as it was).** Simpler, and nothing new reaches the workers, but not what the product owner chose.
- **Double the BFF key on NATS.** Rejected: same storage as the NKey seed, so no independent barrier; one more secret to rotate.
- **Workers verify the user's access token themselves.** Rejected: it would copy the permission, session and two-step rules into Python and need the denylist in the workers, which ADR 0010 rules out.
- **Route every BFF call to intelligence through core** (core calls the worker). Safer for tokens and still available: core-initiated calls are unchanged. It is the recommended default for any operation that touches patient data.
- **One shared `bff` NATS user.** Rejected for per-app users, so the clinical app's credential can't reach platform routes.
