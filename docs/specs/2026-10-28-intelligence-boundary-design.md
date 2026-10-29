# The intelligence boundary: how core calls the Python services over NATS: design

- **Status:** Approved (product owner, 2026-10-28). The ten questions in §16 are decided as recommended, and the five refinements in §14 are accepted.
- **Date:** 2026-10-28
- **Plan:** `docs/plans/2026-10-28-plan-3-intelligence-boundary.md`
- **Amended 2026-10-28 (product owner):** the frontend's servers reach core and intelligence over NATS, and intelligence asks core for a grant before serving a BFF call. See §17 and [ADR 0011](../adr/0011-frontend-ingress-over-nats.md). Where §17 and an earlier section differ, §17 wins; the sections below are updated to match.
- **Builds on (and must stay consistent with):**
  - [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md): API in Go, models in Python, NATS between them.
  - The core design, `docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md` (approved). This document details its §4.3, §5, §7.4–§7.7 and step 4 of its §10. Where this design refines a rule there, it says so and why (§14).
  - Plan 2, `docs/plans/2026-10-26-plan-2-go-gateway-auth.md`, and what it built in `services/core` (`internal/bus`, `internal/token`, `internal/authz`, `internal/audit`, `internal/db`) and `services/nats/nats.conf`.
- **Must not block:** the parked performance design, `docs/specs/2026-10-27-intelligence-performance-design.md` (GPU, millisecond steps, streaming). §11 shows how.

## 1. Why

Plan 2 built the core service: sign-in, sessions, roles, hospital separation and the audit chain. The Python `intelligence` package still runs on its own. It has its own role checks (`RBACManager`), its own audit log (`AuditLogger`), a stub Redis queue, and it connects to Postgres as the superuser.

The next backend plan joins the two. Core sends work to Python over NATS. Python checks that every message really came from core, for this hospital, this user and this operation, before it does anything. This design says exactly how, so the plan can be written from it.

## 2. Goals, non-goals, anti-goals

**Goals**
- One message contract shared by Go and Python, which can't drift silently.
- Every message to intelligence carries a short-lived token that core signs for that one call. Intelligence verifies it before any other work.
- Intelligence reads hospital data only under row-level security, with the hospital taken from the verified token.
- A clinical question runs end to end: BFF → core → NATS → intelligence → Postgres, and the result comes back through core.
- Clear rules for timeouts, retries, duplicates and cancellation, and one mapping from intelligence errors to core's problem responses.
- Only Go writes the audit chain. Intelligence hands Go its audit facts.

**Non-goals**
- The GPU work: vLLM, ONNX, batching, FP8 (the parked performance design).
- Streaming answers to the browser. The boundary reserves room for it (§11) but doesn't build it.
- New frontend endpoints beyond the minimum needed to prove the path (§13, task 10).
- Rewriting the agents, retrieval or guardrails. They are wrapped.

**Anti-goals: these would make it a failure even if it worked**
- Intelligence accepting a message because of where it came from (the NATS subject or the connection) without checking the token.
- Intelligence taking a hospital id or a user id from the message body.
- A second copy of the permission rules written by hand in Python.
- Intelligence writing the audit chain.
- Anything on the hot path that costs more than about a millisecond per message (§11).

## 3. Constraints (already decided)

- One shared deployment. Hospital data is kept apart by Postgres RLS through `mhai_current_hospital()`.
- No data-residency rule (product owner, 26 Oct).
- Go and Python both sit behind NATS. Core is the only service that publishes jobs and the only one that authorises a call to intelligence; since §17 the BFF may also send a `bff_direct` request, which intelligence serves only after core grants it.
- Python verifies every request before answering (ADR 0010).
- Shared reference content (`kb_chunks` with `hospital_id IS NULL`) is read-only for every app role.
- Synthetic data only until go-live.

## 4. Architecture at the boundary

Since §17, the BFF reaches core over NATS too (`api.v1.<audience>`), and may call an intelligence operation directly, in which case the worker first asks core for a grant (`authz.v1.grant.<service>`). The picture below is the core-initiated path, which is unchanged; §17.2 has the BFF-initiated one.

```
BFF ──NATS api.v1──> core ──────────── NATS (TLS, per-service NKeys) ─────────────> intelligence workers
               │  mints a request token per call                                   │ 1. verify token
               │  request/reply: inference.v1.<service>.<op>   (someone waits)    │ 2. check scope + role
               │  JetStream:     jobs.v1.<hospital>.<type>     (nobody waits)     │ 3. do the work
               │  replies only on _INBOX_gateway.>                                 │ 4. reply / write result
               │                                                                    v
               └──SQL (mhai_gateway, RLS)──> Postgres <──SQL (mhai_worker, RLS)── pipeline service only
                                               ^                                   (model services: no DB)
                     audit drainer (Go) ───────┘  appends intelligence's audit facts to the chain
```

**Load balancing.** Every copy of a service subscribes in the same NATS queue group, so NATS gives each request to exactly one member: core's copies share queue group `core` (for `api.v1.*` and `authz.v1.grant.>`, §17), and each intelligence service's copies share a group named after the service (for `inference.v1.<service>.>`). Jobs go to one durable pull consumer per service, which all its copies fetch from. A new copy starts receiving as soon as it subscribes, and a dead one stops; there is no address list. NATS spreads requests evenly, not by GPU load; load-aware routing in front of the model servers belongs to the performance design.

**Services on the Python side** (each its own queue group and its own NATS user):

| Service | Kind | Data access | First build |
|---|---|---|---|
| `pipeline` | JetStream consumer for `run_query` jobs, plus request/reply `health` | `mhai_worker` under RLS | yes |
| `echo` | request/reply only; returns what it was sent, after verification | none | yes, test and smoke only, never deployed to production |
| model services later (`nli`, `embed`, `mask`, `dia`, `qda`) | request/reply | **none** | no; the subject scheme and token already fit them |

The model services hold no database credentials at all. They get text or ids in the message, run a model and answer. That keeps the GPU host stateless and means a compromised model worker can't read the database.

## 5. Subjects and versioning

### 5.1 Subjects

| Pattern | Used for | Publisher | Subscriber |
|---|---|---|---|
| `inference.v1.<service>.<op>` | request/reply calls someone is waiting on, seconds at most | core; and the clinical BFF for operations marked `bff_direct` (§17.2) | the service's queue group (`<service>`) |
| `jobs.v1.<hospital_id>.<type>` | background work on the `JOBS` stream | core | the service's durable pull consumer |
| `jobs.v1.global.<type>` | jobs with no hospital (for example `retention_purge`) | core | as above |
| `dead.v1.<hospital_id\|global>.<type>` | a job that has run out of deliveries or was refused | the worker | core's dead-letter consumer |
| `_INBOX_gateway.>` | replies to core | workers and the BFF's callees, through `allow_responses` only | core |
| `api.v1.clinical`, `api.v1.platform` | the frontend contract over NATS: one HTTP request per message (§17.1) | the BFF of that app only (`bff_clinical`, `bff_platform`) | core (queue group `core`) |
| `authz.v1.grant.<service>` | a worker asking core to approve a BFF call (§17.2) | that service's NATS user only | core (queue group `core`) |
| `_INBOX_bff.<app>.>` | replies to the BFF | core and workers, through `allow_responses` only | that app's BFF |
| `stream.v1.<hospital_id>.<request_id>` | **reserved** for streamed tokens (§11). Not permitted in this build | — | — |

- `<service>` and `<op>` are lower-case `[a-z0-9_]+`. `<hospital_id>` is the canonical lower-case uuid.
- `<type>` is the job type from the frontend contract §14.8 (`run_query`, `submit_feedback`, `ingest_document`, ...), so the platform app's Failed jobs view and the subjects use one vocabulary. The core design's example `jobs.v1.<hospital_id>.query` becomes `jobs.v1.<hospital_id>.run_query`.
- The core design wrote request/reply subjects as `inference.v1.<service>`. This design adds the `.<op>` level, so one service can expose several operations (`pipeline.health`, later `dia.pages`) and the token can bind the exact operation. The existing permission `inference.v1.>` already covers it.

### 5.2 Versioning

- **The major version is in the subject** (`v1`). A breaking change gets new subjects (`inference.v2...`). Core and workers run both majors side by side until every caller has moved, then `v1` is removed. Nothing ever changes meaning under the same subject.
- **Within a major, changes are additive only.** New optional fields, new operations, new job types. The schema tool enforces this in CI (§6.3).
- **The message type is named in a header** (`Mhai-Schema`, §6.2). A worker refuses a schema name it doesn't know with `invalid_request`, never guessing.
- **The request token has its own version** in its `typ` header (`mhai-req+jwt`, version 1 by the claim set). A new claim set is a new `typ`.

## 6. The message envelope and the schema

### 6.1 Recommendation: Protocol Buffers, with `buf`

| | JSON Schema | Protocol Buffers (recommended) |
|---|---|---|
| Generated types | Python is good (pydantic); Go generators are weak, so Go structs end up hand-written and drift | first-class generated code for both Go and Python |
| Stopping drift | no standard breaking-change checker; drift is caught only by tests we write | `buf breaking` fails CI on any incompatible change against `main` |
| Strictness | can reject unknown fields | unknown fields are kept and ignored; required-ness is checked by hand or by `protovalidate` |
| Size and speed | text; float arrays (embeddings, NLI scores, batches) are large and slow to parse | binary; compact floats; microseconds to parse. Matters for the GPU path (§11) |
| Debugging | readable with `nats sub` | needs `buf` or a small decode tool |
| Fit with what exists | core already hand-writes strict JSON | new toolchain (`buf`, `protoc-gen-go`, `mypy-protobuf`), all pinned and local |

**My call: Protocol Buffers (proto3), managed with `buf`.** Drift prevention is the requirement, and `buf breaking` gives it mechanically. Binary floats also keep the later embedding and NLI calls cheap. The cost is one small toolchain and a decode helper for debugging.

The request token stays a JWT (JSON). It is small, it matches core's existing token code, and it is signed over its own bytes, so its format is independent of the body's.

### 6.2 The envelope

A NATS message has headers and a body. The envelope is the headers; the body is one protobuf message.

**Request headers (core → intelligence)**

| Header | Value | Checked by the worker |
|---|---|---|
| `Mhai-Request-Token` | the signed request token (§7) | yes, first |
| `Mhai-Schema` | full protobuf message name, e.g. `mhai.intelligence.v1.RunQueryJob` | must be the schema the operation expects |
| `Mhai-Request-Id` | the request id core already puts in `X-Request-ID` and the audit chain | must equal the token's `rid` |
| `traceparent`, `tracestate` | W3C trace context (§10) | parsed, never trusted for anything but tracing |
| `Nats-Msg-Id` | jobs only: the job id, for JetStream duplicate detection | — |

**Body:** the protobuf message, at most 256 KB for request/reply and 64 KB for jobs (jobs carry ids, not data; core design §4.3). NATS `max_payload` is set to 1 MB on the server as an outer bound.

**Reply (intelligence → core), request/reply only**

| Header | Value |
|---|---|
| `Mhai-Status` | `ok`, or an error code from §9 |
| `Mhai-Schema` | the response message name, or `mhai.intelligence.v1.Error` |
| `Mhai-Request-Id` | echoed |

Body: the response message when `ok`, else `Error{code, retryable}`. An `Error` never holds input text, patient data, a stack trace or the token.

Every response message has a common field `ResponseMeta meta` with:
- `audit_facts`: what the worker did that Go should record (§10.1);
- `model_versions`: the model names and versions used, for clinical traceability;
- `timings_ms`: per-step durations, for the stage timings in frontend contract §7.

### 6.3 Where the schema lives and how drift is stopped

- `proto/mhai/intelligence/v1/*.proto` at the repository root, shared by both services. Package `mhai.intelligence.v1`.
  - `envelope.proto`: `Error`, `ResponseMeta`, `AuditFact`.
  - `pipeline.proto`: `RunQueryJob {query_id}`, `HealthRequest`, `HealthResponse`.
  - `echo.proto`: test only.
- Generated code is committed: Go in `services/core/internal/intelpb/`, Python in `services/intelligence/intelligence/boundary/pb/` (official `protobuf` runtime, `mypy-protobuf` stubs for mypy).
- **CI checks, in the existing `checks` job:**
  1. `buf lint`;
  2. `buf breaking --against` the `main` branch, for every `v1` file;
  3. `buf generate`, then fail if the committed code differs (`git diff --exit-code`).
- **Cross-language golden messages:** a directory of encoded sample messages, written by a Go test and decoded by a Python test (and the reverse), so both sides prove they read the same bytes the same way.

## 7. How intelligence verifies each request

### 7.1 Options

| | Forward the user's access token | Core mints a request token per call (recommended, and what ADR 0010 already decided) |
|---|---|---|
| What it proves | that a user signed in | that **core** authorised **this** call, for this hospital, user, operation, body and deadline |
| Lifetime | 15 minutes, any operation | the call's deadline (seconds), or the job's deadline |
| Audience | `mhai-bff`; intelligence would have to accept BFF tokens, widening who can talk to it | the one service it is for |
| Revocation | intelligence would need the denylist in NATS KV (it has no KV access today) | not needed: core checks the denylist and the account before minting, and the token is too short-lived to matter |
| Background jobs | no user token exists (EMR events, retention) | core mints one for the system (§7.2, `sub`) |
| Binding to the body | none; a stolen token works with any body | the body's hash is signed in |

**Decision: core mints a request token for every call and every job.** The user's access token never leaves core.

### 7.2 The request token

A JWT signed with EdDSA (Ed25519), in the header `Mhai-Request-Token`. This extends the claim set in the core design §5.2; every claim listed there is kept.

**Header:** `{"alg":"EdDSA","typ":"mhai-req+jwt","kid":"<key id>"}`. The `typ` differs from the access token's `JWT`, so neither verifier accepts the other's token even if a key were shared.

| Claim | Meaning |
|---|---|
| `iss` | `mhai-gateway` (as in the core design; the name predates the rename to core and is kept, like the NATS user) |
| `aud` | the service, e.g. `pipeline` |
| `sub` | the user id; or `svc:core` for work no user started (EMR events, retention) |
| `hid` | the hospital id; empty only for `jobs.v1.global.*` |
| `roles` | the user's roles at mint time; empty for `svc:core` |
| `rid` | the request id |
| `jti` | 128 random bits, base64url: the nonce |
| `iat`, `exp` | issue time, and **the deadline**: the worker must not start work after `exp` and abandons it at `exp` |
| `knd` | `req` (request/reply) or `job` |
| `nsub` | the exact NATS subject the message was published on |
| `bh` | base64url(sha256(body bytes)) |
| `pids` | optional: the patient ids core checked belong to `hid` (core design §7.6) |
| `rpl` | the reply prefix the answer must go to: `_INBOX_gateway.` when core calls, `_INBOX_bff.<app>.` for a grant (§17.2). Added by §17 |
| `via` | `core` (core called) or `grant` (core approved a BFF call). Added by §17 |

**Lifetimes:** `req` tokens: `exp − iat` is the call's timeout, at most 60 s. `job` tokens: the job's deadline, at most 24 h (core design §5.2). For `run_query` the deadline is 10 minutes (decision 2, §16).

**Signing key:** a dedicated Ed25519 key for request tokens, `REQUEST_KEY_PATH`, separate from the access-token key (`ACCESS_KEY_PATH`). Both stay inside core. `core keys new|ensure` gains a `--kind request` flag. My call: separating them means a leak or a bug in one verifier can't turn one token kind into the other, and the two keys can rotate on their own schedules.

### 7.3 What every worker checks, in order, before anything else

The order is chosen so that cheap checks run first and no body parsing happens before the signature passes.

1. **Sizes.** The token is at most 4 KB and the body within the limit (§6.2). Otherwise refuse.
2. **Token shape and signature.** Exactly three parts, strict JSON with no unknown fields, `alg` `EdDSA`, `typ` `mhai-req+jwt`, `kid` in the trusted set, and the Ed25519 signature verifies.
3. **Claims.** `iss` is `mhai-gateway`, `aud` is this service, every required claim is present, `iat ≤ now + 5 s`, `now < exp`, and `exp − iat` is within the cap for `knd`.
4. **Binding.** `nsub` equals the subject the message arrived on, `knd` matches how it arrived (core subject versus JetStream), and `bh` equals sha256 of the body bytes, compared in constant time. For `jobs.v1.<hid>.*`, the hospital in the subject equals `hid`.
5. **Reply subject (request/reply).** The reply subject starts with the token's `rpl` (`_INBOX_gateway.` for core's own calls; `_INBOX_bff.<app>.` for a grant, §17.2). A request whose reply goes anywhere else is refused with no reply at all. This keeps an attacker from steering an answer to an inbox it can read.
6. **Replay** (§7.4).
7. **Schema.** `Mhai-Schema` is the operation's request type, `Mhai-Request-Id` equals `rid`; then decode the body strictly.
8. **Scope.** Every hospital id in the body equals `hid`, and every patient id in the body is in `pids`. A body's hospital is never trusted on its own (core design §5.3 step 4). Operations that need a hospital refuse an empty `hid`.
9. **Permission.** The operation's entry in the generated operations table (§8.1) allows at least one of `roles`, or allows `svc:core` when `sub` is `svc:core`.

For a message that arrives **without** a request token but with a user access token (a BFF call, §17.2), the worker runs check 1, then asks core for a grant, and runs checks 2–9 on the token core returns. It never runs checks 2–9 on anything core didn't sign for this call.

Any failure: no work is done. The worker logs the reason code and the ids (`rid`, `jti`, `hid`, `kid`), never the token or the body, increments `boundary_refusals_total{service,reason}`, and answers `unauthorized` (checks 1–6, 9) or `invalid_request` (7–8) if the reply subject passed check 5. A refused job is terminated (`Term`) and sent to `dead.v1` with the reason code.

**The verifier is written by hand in Python**, mirroring core's `token.VerifyAt`: one algorithm, one header shape, fixed claims, using `cryptography`'s Ed25519 (already a dependency). No general JWT library, so there is no algorithm negotiation to get wrong. Cost: about 0.1 ms per message on a CPU (Ed25519 verify plus sha256 of the body).

**Shared test vectors** stop the two sides drifting. A Go test writes `testdata/request-tokens/*.json`: valid tokens plus each kind of bad one (tampered payload, wrong `typ`, unknown `kid`, wrong `aud`, expired, `exp` too far out, wrong `nsub`, wrong `bh`), with the key, the body, the subject and the expected verdict. The Python suite runs every vector. CI regenerates them and fails on any difference.

### 7.4 Replay protection

- **Request/reply.** Each worker keeps an in-memory set of `jti`s it has accepted, each kept until its `exp` plus 5 s (at most 65 s, so the set stays small). A repeat is refused.
  - Copies of a service share a queue group, so a replayed message could reach a different copy, whose set hasn't seen it. That is accepted for now because of the other checks:
    - only the `gateway` NATS user can publish on `inference.v1.>` (§7.6), so a replay needs core's NATS credential or the NATS server;
    - the token binds the subject and the body, and expires within the call's timeout;
    - the answer can only go to `_INBOX_gateway.>`, which only core can read;
    - request/reply operations are read-only (they write no rows), so a replay costs compute and nothing else.
  - A shared check (an atomic `Create` in a NATS KV bucket keyed by `jti`) closes the gap for about one extra NATS round trip per message. Not in the first build (decision 3, §16).
- **Jobs.** JetStream redelivers the *same* message after a crash or a missed ack, so a `jti` cache would wrongly refuse legitimate redeliveries. For jobs the replay rule is in the database instead: a job is **claimed** before it runs and **completed at most once** (§9.2). A duplicate publish is also dropped by JetStream, because `Nats-Msg-Id` is the job id.
- **After a dead letter**, a retry from the platform app's Failed jobs view (frontend §14.8) never reuses the old message. Core checks the job's user and hospital are still active, mints a new token and publishes a new message.

### 7.5 Key distribution and rotation

- **Private key:** only core holds it. In compose, the `core-keys` one-shot makes it in the `corekeys` volume. In production, it comes from the secret store (AWS Secrets Manager or SSM) as a file mounted read-only.
- **Public keys to workers:** a file of trusted keys, `INTELLIGENCE_TRUSTED_KEYS_PATH`, holding `{kid: public key}` for **at most two** kids. In compose, `core keys` writes the public file into a volume the workers mount read-only. In production, it is a separate, non-secret parameter. Workers load it at start-up, refuse to start if it is missing, empty or has more than two keys, and re-read it on `SIGHUP`.
  - Not over NATS: workers would then trust NATS for key integrity, and a compromised NATS could add a key.
- **Planned rotation:**
  1. make the new key; add its public key to the workers' file (now two kids) and roll the workers;
  2. switch core to sign with the new kid;
  3. wait for the longest token lifetime (24 h for jobs) plus the JetStream redelivery backoff;
  4. remove the old kid from the workers' file and roll them.
  Yearly, and whenever someone with access to the key leaves (my call).
- **Emergency rotation** (the private key may have leaked): remove the old kid at once. Jobs still in the stream fail verification and go to `dead.v1`; core's dead-letter consumer recognises `unauthorized` from an unknown kid and re-mints them automatically after checking the user and hospital. This is audited as `intelligence.key_rotated`.
- **Clocks:** every host runs NTP. The 5 s skew allowance is the only tolerance.

### 7.6 NATS permissions: the second layer

The token is the main check. NATS permissions make sure only core can even put a message on the subjects, and that workers can't read what isn't theirs. Changes to `services/nats/nats.conf`:

- **One NKey user per Python service** (`pipeline`, `echo`; later `nli`, `embed`, ...), replacing the shared `worker` user (this closes TODO "One NKey per worker service"). Passwords stay for dev compose; production uses NKeys only.
- **Each service user:**
  - subscribes only to `inference.v1.<service>.>` and `_INBOX_worker.<service>.>` (its own inbox prefix, `_INBOX_worker.<service>`);
  - for a job consumer, may publish only `$JS.API.CONSUMER.INFO.JOBS.<service>`, `$JS.API.CONSUMER.MSG.NEXT.JOBS.<service>` and `$JS.ACK.JOBS.<service>.>`, for its own durable consumer and nothing else in the `$JS` API;
  - publishes `dead.v1.>` and replies through `allow_responses: {max: 1, expires: "1m"}`;
  - has no KV access and cannot publish on `inference.v1.>`, `jobs.v1.>` or `_INBOX_gateway.>`.
- **Each service user** also publishes `authz.v1.grant.<service>` (its own grant subject only) and subscribes nothing new: the grant answer comes to its own `_INBOX_worker.<service>.>` (§17.4).
- **The `gateway` user** keeps its permissions, with the JetStream wildcards narrowed to the KV streams and `JOBS` (TODO "NATS wildcards"). Since §17 it also subscribes `api.v1.>` and `authz.v1.grant.>`, and answers them through `allow_responses`.
- **The BFF users** (`bff_clinical`, `bff_platform`, §17.4) publish only their app's `api.v1.<audience>` subject and, for the clinical app, the `bff_direct` operations' `inference.v1` subjects; they subscribe only their own `_INBOX_bff.<app>.>`.
- **TLS** on every connection, with `verify` on in production (TODO "TLS option").
- **Later, optional:** move intelligence into its own NATS account with explicit imports and exports. That's stronger isolation, but more configuration than three people need right now.

## 8. Authorisation and hospital separation inside intelligence

### 8.1 One permission source: the operations table

- Core gains an **operations table** next to its route table in `internal/authz`: one row per NATS operation (`pipeline.run_query`, `pipeline.health`, `echo.ping`), with the roles allowed, whether `svc:core` may call it, whether it needs a hospital, and (since §17) whether the BFF may call it directly (`bff_direct`).
- Every HTTP route that publishes to intelligence names the operation it calls. A test fails if:
  - a route calls an operation that isn't in the table;
  - a route's roles are not a subset of the operation's roles.
- `cmd/permgen` also writes `services/intelligence/intelligence/boundary/operations.json` and a section of the generated permissions markdown. CI fails if the committed files differ from the generated ones.
- The worker loads `operations.json` at start-up and refuses to start if one of its handlers has no entry. This is check 9 in §7.3: Python never writes a rule of its own; it only reads Go's.

### 8.2 Database access

- **Only the `pipeline` service connects to Postgres.** Model services have no database credentials.
- **Login:** `core-migrate` provisions a login, `INTELLIGENCE_DB_USER` (default `intelligence_app`), the same way it provisions core's login (`db.EnsureLogin`, SCRAM, placeholder passwords refused). The login is a member of `mhai_worker` **with `INHERIT FALSE`**, so it holds no privileges until it runs `SET LOCAL ROLE mhai_worker`. This retires the superuser connection (TODO, Plan 2 review T2 M6).
- **Every query runs inside one helper,** `with_hospital(hid)`:
  ```
  BEGIN; SET LOCAL ROLE mhai_worker; SELECT set_config('mhai.hospital_id', <hid from the verified token>, true); ... COMMIT
  ```
  The hospital comes only from the verified token. A test (the Python twin of core's `nodirectdb_test.go`) fails if any module outside the helper opens a connection or a cursor.
- **Driver:** `psycopg` 3 with `psycopg_pool`, parameterised queries only. No string-built SQL, which matters because RLS guards a forgotten filter, not injected SQL (TODO, T2 M1).
- **Grants:** the plan narrows `mhai_worker` to what the pipeline uses:
  - `clinical_queries`: `SELECT`, and `UPDATE` of the status columns only (column grants);
  - `clinical_decisions`, `hga_evaluations`: `SELECT`, `INSERT`;
  - `kb_chunks`: `SELECT` (own hospital and shared rows). `INSERT`/`UPDATE` stay only if the feedback enrichment path needs them in this plan; otherwise they are revoked;
  - `guardrail_weights`: `SELECT`;
  - `intelligence_audit_outbox` (new, §10.1): `INSERT` only.
- **Shared reference content:** visible through the existing `shared_read` policy. The `hospital_rows` `WITH CHECK` stops any app role writing a row with `hospital_id IS NULL`. Shared content is loaded by the owner-side seed tool, never by a worker.
- **Patient scope inside a hospital:** RLS separates hospitals, not patients. Retrieval keeps its existing per-patient filter (`chunk_visible_to()`, ADR 0004), with the patient id taken from the claimed `clinical_queries` row, never from a message body.
- **Data in messages:** jobs carry ids (`query_id`); the worker reads the question and patient data from `clinical_queries` under RLS. A row from another hospital is invisible, so a forged id gives `not_found`, never data.

### 8.3 Stale roles on long jobs

A job token's `roles` are a snapshot from when the job was queued. Intelligence doesn't decide who may see the result: it writes the result under the job's hospital, and core checks the reader's current session and roles when the result is fetched. When a user or hospital is deactivated, core marks their queued and running jobs `cancelled` (§9.4), so they stop at the next stage.

## 9. Timeouts, retries, idempotency and cancellation

### 9.1 Request/reply

- **Only for calls that finish in seconds** and change no rows: health, later page images or a single model call.
- **Timeouts** are per operation, set in core's operations table from measured latencies (core design §13). The default is 5 s. The token's `exp` is that deadline, so the worker knows its budget without a separate header.
- **No silent retry** (core design §4.3). A timeout, no responders (NATS 503) or `overloaded` reaches the user as 503 `unavailable`, "Couldn't get an answer, try again".
- **Overload:** each worker runs at most N handlers at once (an `asyncio.Semaphore`, sized from measurements). Past that, it answers `overloaded` straight away rather than queueing silently.
- **Cancellation:** core stops waiting when the HTTP request's context ends. The worker stops at `exp`. There is no cancel message for request/reply; calls are short by definition.

### 9.2 Jobs (JetStream)

- **Stream `JOBS`** on `jobs.v1.>`: file storage, encrypted at rest, `max_age` 7 days, a duplicate window of 2 minutes, `discard: new` with a per-subject limit, so a flood is refused at publish (core answers 503) instead of growing without bound.
- **One durable pull consumer per service** (for example `pipeline` on `jobs.v1.*.run_query`): explicit ack, `ack_wait` 60 s, `max_deliver` 5 with backoff 10 s, 30 s, 2 min, 5 min, and `max_ack_pending` set to the worker count × concurrency.
- **Long runs** send `+WIP` (in progress) every 20 s, so a 30-second answer isn't redelivered mid-run.
- **Publishing (core):** in one transaction, core inserts the `clinical_queries` row (status `queued`) and an `Idempotency-Key` record (existing middleware). After the commit it publishes, with `Nats-Msg-Id` = the query id. If the publish fails, a sweeper republishes `queued` rows with no `published_at` after 10 s. JetStream drops the duplicate if both get through.
- **Claiming (worker):** before doing anything costly, the worker runs, under RLS,
  `UPDATE clinical_queries SET status='running', attempts=attempts+1, lease_until=now()+interval '90 s' WHERE id=$1 AND (status='queued' OR (status='running' AND lease_until < now())) RETURNING ...`.
  No row returned: already done, cancelled, running elsewhere or not this hospital's. The worker acks and stops.
- **Completing:** one transaction writes the `clinical_decisions` row (`query_id` is unique, so a second completion fails and rolls back), the `hga_evaluations` rows, the audit outbox rows, and sets `status='completed'`. Then it acks.
- **Failing:** a retryable error (model not loaded, database unavailable) gets `Nak` with the backoff delay. A permanent error (`invalid_request`, `not_found`, `unauthorized`), or the last delivery, gets `Term`, a `dead.v1` message (ids, type, attempts, error `kind` only) and `status='failed'` with a fixed error code.
- **Deadline:** past the token's `exp`, the worker sets `status='failed'` (`deadline_exceeded`) and terminates the message.

Retrying a clinical question re-runs the model from the start. No partial answer is ever shown, so a re-run is safe; it only costs compute.

### 9.3 Idempotency summary

| Duplicate | Stopped by |
|---|---|
| The BFF retries `POST /queries` | the `Idempotency-Key` middleware (72 h) |
| Core publishes twice | JetStream `Nats-Msg-Id` within the duplicate window, then the claim |
| JetStream redelivers | the claim and the lease |
| Two workers finish the same job | the unique `clinical_decisions.query_id` |
| A request/reply message replayed | the per-worker `jti` set, plus §7.4 |

### 9.4 Cancellation of jobs

- Core sets `status='cancelled'` (the platform app's cancel in frontend §14.8, or a user or hospital being deactivated).
- The worker checks the row's status **between pipeline stages** (after masking, after retrieval, after generation, before writing) and stops if it is cancelled. That costs one indexed read per stage.
- Stopping *inside* a long generation needs a signal to the model server. That belongs to the performance work, where a cancel subject can be added (§11). It isn't needed while answers aren't streamed.

## 10. Errors, audit and observability

### 10.1 Error codes and how core maps them

Intelligence answers with a closed set of codes. Core maps each to one of its problem responses. The mapping lives in one Go table with a test that covers every code.

| Intelligence code | Meaning | Core's response (request/reply) | Job outcome | Platform `kind` (frontend §14.8) | Alert |
|---|---|---|---|---|---|
| `unauthorized` | a verification check failed | 500 `internal_error` | `failed`, dead letter | `invalid` | **yes**: either a bug or an attack |
| `forbidden` | role check (§7.3 step 9) failed | 500 `internal_error` | `failed` | `invalid` | **yes**: the Go and Python tables disagree |
| `invalid_request` | schema or scope check failed | 500 `internal_error` | `failed` | `invalid` | yes |
| `not_found` | the row isn't visible under RLS | 404 `not_found` | `failed` | `other` | no |
| `deadline_exceeded` | past `exp` | 503 `unavailable` | `failed` | `timeout` | no, a rate alert |
| `overloaded` | at its concurrency limit | 503 `unavailable`, `Retry-After: 5` | `Nak`, retried | `timeout` | rate alert |
| `unavailable` | a model or the database isn't ready | 503 `unavailable` | `Nak`, retried | `unreachable` | rate alert |
| `cancelled` | the job was cancelled | — | `cancelled` | — | no |
| `internal` | anything else | 500 `internal_error` | `Nak`, then `failed` | `other` | rate alert |
| (NATS timeout) | no reply in time | 503 `unavailable` | — | `timeout` | rate alert |
| (NATS no responders) | no worker running | 503 `unavailable` | — | `unreachable` | **yes** |
| `grant_refused:<code>` | core refused a BFF call's grant (§17.2); `<code>` is core's own problem code (`unauthenticated`, `token_expired`, `mfa_required`, `forbidden`, `not_found`, `rate_limited`) | the BFF answers with core's code and status, unchanged | — | — | rate alert on `forbidden` |
| (grant timeout, core unreachable) | no grant in the budget (§17.3) | 503 `unavailable` | — | `unreachable` | **yes** |

- **503 `unavailable` is added to core's code catalogue** (`httpx.Codes`). The frontend contract already uses it (§10, "the app shows that view as failed and offers Try again"), so this is not a new contract code. `not_ready` stays for readiness.
- `unauthorized`, `forbidden` and `invalid_request` become 500 for the user, not 401 or 403: the user was already authorised by core, so a refusal at the boundary is our fault or an attack, and the response must not tell an attacker which check failed.
- **On a BFF call (§17.2),** a grant refusal is passed through as core's own code, because it is core's decision about the user and the user needs it (sign in again, two-step required). Every other worker refusal is still 500 `internal_error`.
- A failed job's `clinical_queries.error` holds only the code. The app shows "Couldn't complete" (frontend §14.8).

### 10.2 Audit: what goes on the hash chain

Only core writes the chain (core design §7.5). Intelligence gives core its facts:
- **request/reply:** in the reply's `ResponseMeta.audit_facts`. Core appends them in the same transaction as its own request audit;
- **jobs:** in a new table, `intelligence_audit_outbox`, written in the **same transaction** as the result, so a fact can't be lost between the commit and a publish. A core background loop (the **drainer**) reads the outbox, appends each fact with `audit.Write` and deletes it, in one transaction. It runs as a new role, `mhai_audit_drain`: `SELECT, DELETE` on the outbox across hospitals and `INSERT` on `audit_log`, nothing else.

**Core takes the actor, hospital and request id from the token it minted** (it knows them), never from the fact. A fact can only add `action`, `entity`, `entity_id` and a payload, and the payload goes through the existing sensitive-key check in `audit.Write`.

**Entries:**

| Action | Written when | Payload (ids and codes only, never PHI or text) |
|---|---|---|
| `query.submitted` | core queues a question | `query_id`, `urgency`, `category` |
| `query.completed` | drained from the outbox | `query_id`, decision (`PASS`, `REGENERATE`, `ESCALATE`), `trust_band` (low, mid, high), cited `chunk_id`s, `model_versions`, `attempts` |
| `query.failed` | the job ended `failed` | `query_id`, error code, `attempts` |
| `query.cancelled` | core cancelled it | `query_id`, reason code |
| `job.dead_lettered` | core's dead-letter consumer | job id, type, error code, `attempts` |
| `job.retried` | a platform retry | job id, new message id |
| `intelligence.refused` | core gets `unauthorized`, `forbidden` or `invalid_request` | operation, reason code, `kid` |
| `intelligence.key_rotated` | a request-signing key is added or removed | `kid`, planned or emergency |
| `intelligence.granted` | core approves a BFF call (§17.2) | operation, `jti`, `bh`, patient ids checked |
| `intelligence.grant_refused` | core refuses one | operation, problem code |

Each model call inside the pipeline is **not** audited one by one. That would flood the chain and say nothing a clinician or auditor needs. The per-step detail lives in traces (§10.3). Decided this way (decision 4, §16).

Refusals that a worker sees but core never hears about (a forged message with a foreign reply subject gets no reply) go to the worker's logs and the `boundary_refusals_total` metric, which alerts.

### 10.3 Observability: tracing

- **W3C trace context** in NATS headers (`traceparent` only: `tracestate` is caller-controlled free text and is neither read nor sent). Core uses the OpenTelemetry Go SDK with a NATS header carrier; the Python worker uses the OpenTelemetry Python SDK with the same carrier. Both export OTLP to the existing `otel-collector` (`services/intelligence/configs/otel-collector.yaml`).
- **Spans:**
  - core: `intelligence.request <service>.<op>` (client) or `intelligence.publish <type>` (producer);
  - worker: `boundary.verify`, then `handle <op>` (server or consumer), with child spans per pipeline stage (`mask`, `retrieve`, `generate`, `nli`, `hga`): the same steps the performance design measures (its §6.3);
  - jobs use a **span link** from consumer to producer, because the job can run long after the request trace ended.
- **Attributes allowed:** `request_id`, `hospital_id`, `query_id`, operation, status code, attempt, model name and version, timings. **Never** prompt text, answer text, patient fields or the token. A test sends a synthetic message with a marker string and checks the marker appears in no span and no log line.
- **Trace headers are untrusted.** A malformed `traceparent` starts a new trace; it's never an error. The `baggage` header is dropped on both sides, so nothing rides along that could carry PHI.
- **Metrics:** latency per operation (p50, p95, p99), `boundary_refusals_total{service,reason}`, verification time, queue depth and oldest wait per job type (feeds the platform System health view, frontend §14.8), dead letters, NATS reconnects.
- **Logs:** JSON, through the same redaction rules as core (`logx`): no tokens, no message bodies.

## 11. Room for the performance work

The parked performance design wants millisecond steps, GPU batching and streaming. Nothing here gets in its way:

- **Cost per message:** about 0.1 ms to verify, microseconds to decode protobuf, and no database or KV round trip for model services. Well inside the §2 targets there.
- **Batching:** each message is verified on its own, then its request joins the model batch. A batch may hold several hospitals' requests (the models are stateless). Results go back per message, to that message's reply subject.
- **Cross-hospital caches:** vLLM's prefix cache could, in principle, leak through timing whether another hospital's prompt shared a prefix. When vLLM arrives, each request passes a per-hospital cache salt, so only same-hospital prefixes are shared. That's recorded here so the performance plan carries it.
- **Streaming (performance design §11 question 1):** reserved, not built. The likely shape:
  - core subscribes to `stream.v1.<hid>.<request_id>` before publishing;
  - the token gains a `strm` claim naming that subject;
  - the worker publishes token chunks (`StreamChunk{seq, text, final, checking}`) only there;
  - the service's NATS user gets `publish stream.v1.>`.
  It works for both request/reply and jobs, so this design doesn't force the choice the performance design left open. It needs its own security review (a worker able to publish to any stream subject) and clinical sign-off (text before checks finish).
- **Model services without a database** can run on a GPU host with no route to Postgres at all.
- **Cancel inside generation:** a future `cancel.v1.<hid>.<request_id>` subject fits the same scheme.

## 12. Local development and CI

- **Compose:** `pipeline` (CPU, Ollama as today) and `echo` replace the current `intelligence` container command. They get their own NATS users and passwords, the trusted-keys file (read-only volume written by `core-keys`) and, for `pipeline`, the `intelligence_app` database login. Redis leaves compose (core design §4.3).
- **Python integration tests** use `testcontainers` (Python) to start Postgres 16 and NATS 2.10 with the real `services/nats/nats.conf` and core's migrations. Core's `migrate up` runs from the `mhai-core` image, so the schema is the same one production uses. Tests run under a `boundary` pytest marker and **never load models**: the pipeline runs with a stub model client that returns fixed synthetic answers.
- **Go integration tests** extend `internal/testutil` (NATS and Postgres containers) with a Go fake worker for core's own tests: timeouts, no responders, error mapping, publishing and the sweeper.
- **Cross-language checks** without running both languages in one test: the shared token vectors (§7.3), the golden protobuf messages (§6.3) and the generated `operations.json` (§8.1).
- **Queue groups are tested with two copies** of core and of each worker: every request is answered once, each copy answers some, and a copy killed mid-run loses nothing once the caller retries (request/reply) or JetStream redelivers (jobs).
- **One end-to-end smoke test in CI:** compose up with `core`, `nats`, `postgres`, `pipeline` (stub models) and `echo`; sign in as a synthetic clinician; `POST /queries`; poll to `completed`; check that the chain verifies and holds `query.completed`. CI only. Nobody runs it on a laptop that isn't up to it, and heavy builds go to the remote hosts as usual.
- **CI jobs:** `checks` gains `buf lint`, `buf breaking` and the generate-diff check. A new `intelligence-boundary` job runs `uv run pytest -m boundary` with Docker. The smoke test runs after the image builds.

## 13. Security and threat model

### 13.1 Assets

- Patient data in questions, in `clinical_queries`, in prompts and in answers.
- Hospital separation.
- The request-signing private key, the NATS credentials and the `intelligence_app` database login.
- The audit chain, and the facts that feed it.
- Availability of question answering.
- Clinical safety: an unchecked or partial answer must never be shown as final (ADR 0005).

### 13.2 Actors and trust boundaries

| Crossing | What is checked there |
|---|---|
| BFF → NATS → core (`api.v1`) | the BFF's NATS identity (only `bff_<app>` may publish its subject), the access token, denylist, account and hospital state, the route's permission (Plan 2, run unchanged by the adapter, §17.1) |
| BFF → NATS → worker (`bff_direct` operations) | the BFF's NATS identity; then the worker asks core (`authz.v1.grant.<service>`), and core checks everything it checks for an `api.v1` call plus the operation, subject, body hash and reply prefix (§17.2) |
| worker → core (grant) | only that service's user publishes its grant subject; core takes `aud` from the subject; any failure or delay refuses (§17.3) |
| core → NATS | core's NKey and TLS; NATS publish permissions (only `gateway` publishes `inference.v1.>` and `jobs.v1.>`) |
| NATS → worker | the worker's own NKey and subscribe permissions; then **every check in §7.3** |
| worker → Postgres | the `intelligence_app` login (no privileges until `SET LOCAL ROLE mhai_worker`); RLS with the hospital from the verified token; column grants |
| worker → model server (Ollama now, vLLM later) | local network only, on the same host or private subnet; no patient data leaves our network (performance design §4.2) |
| worker → core (replies) | `allow_responses` (one reply per received request); replies only to `_INBOX_gateway.>`, which only core reads |
| outbox → audit chain | the drainer takes actor, hospital and request id from the job's own row and token, never from the fact; sensitive-key check |

Actors: clinicians (through the BFF), core, the Python services, NATS, Postgres, the model servers, platform admins (retry and cancel of jobs), and an attacker who holds one stolen credential.

### 13.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test (§13.5) |
|---|---|---|---|---|
| T1 | Spoofing | A process other than core publishes a request to intelligence | NATS publish permissions (only `gateway`); every message must carry a token signed with core's request key | A1, A2 |
| T2 | Spoofing | A captured message is replayed | `exp` is the call's deadline; per-worker `jti` set; subject and body binding; replies only to core's inbox; jobs claimed once in the DB | A3, A4 |
| T3 | Spoofing | A token for one service or operation is used on another | `aud` is the service; `nsub` is the exact subject | A5 |
| T4 | Spoofing | A user access token, or a request token, is used where the other belongs | different `typ`, `aud` and signing key | A6 |
| T5 | Tampering | The body is changed while the token is kept | `bh` (sha256 of the body) is signed | A7 |
| T6 | Tampering | The body names another hospital or a patient of another hospital | §7.3 step 8 against `hid` and `pids`; RLS with `hid` from the token; another hospital's row is invisible | A8, A9 |
| T7 | Tampering | `alg` confusion, or a forged `kid` | fixed `alg` and `typ`; `kid` only from the local trusted file, at most two | A10 |
| T8 | Tampering | Worker key file replaced through NATS | keys come from a mounted file, never NATS | review |
| T9 | Tampering | A worker writes shared reference content or another hospital's rows | `WITH CHECK` on `hospital_rows`; shared rows read-only; narrowed grants | A11 |
| T10 | Tampering / Repudiation | A worker forges audit facts naming another actor or hospital | core sets actor, hospital and request id itself; facts add detail only; only Go writes the chain | A12 |
| T11 | Repudiation | A completed answer leaves no audit entry (crash between commit and publish) | the transactional outbox; `query.completed` from the drainer | A13 |
| T12 | Information disclosure | PHI in logs, traces, errors or dead letters | `Error` has a code only; dead letters carry ids and kinds; span attribute allow-list; redaction; `baggage` dropped | A14 |
| T13 | Information disclosure | A reply is steered to an inbox the attacker can read | reply prefix check (§7.3 step 5); workers can't subscribe to `_INBOX_gateway.>` | A15 |
| T14 | Information disclosure | A worker subscribes to another service's subjects or replies | per-service users; subscribe only its own subjects and `_INBOX_worker.<service>` | A16 |
| T15 | Information disclosure | Cross-hospital timing leak through a model cache | per-hospital cache salt when vLLM arrives (§11); no question-embedding cache across patients (performance design §5.7) | performance plan |
| T16 | Information disclosure | A refusal reveals which check failed | the user sees 500 `internal_error`; reasons only in logs and metrics | A17 |
| T17 | Denial of service | Message floods or huge bodies | size limits; `max_payload`; `discard: new` with per-subject limits; per-worker concurrency with `overloaded`; core's per-user rate limits | A18 |
| T18 | Denial of service | One hospital's jobs starve the others | a per-hospital cap on queries in flight at `POST /queries` (429 `rate_limited`); `max_ack_pending` | A19 |
| T19 | Denial of service | A poison message is redelivered forever | `max_deliver` 5, then `Term` and `dead.v1` | A20 |
| T20 | Elevation of privilege | A user loses a role or is deactivated while a 24 h job is queued | results are read through core, which checks the reader's current session; deactivation cancels queued and running jobs | A21 |
| T21 | Elevation of privilege | The worker's DB login reaches more than `mhai_worker` | `INHERIT FALSE`; the login can only `SET ROLE mhai_worker`; no connection outside `with_hospital` | A22 |
| T22 | Elevation of privilege | The Go and Python permission rules drift | Python reads the generated `operations.json`; CI fails on a difference; a refusal for `forbidden` alerts | A23 |
| T23 | Spoofing | The request-signing key leaks | it is only on core; separate from the access key; emergency rotation (§7.5) | A24 |
| T24 | Spoofing | **The BFF's NATS credential leaks** | it publishes only its app's `api.v1` subject and the `bff_direct` subjects; every call still needs a user's access token; per-app users; NKeys over TLS; rotation like any NKey; the per-login lockout ceiling holds even with a spoofed client IP | A26, A27 |
| T25 | Spoofing | A worker or other service publishes `api.v1` calls with a harvested access token | only the BFF users may publish `api.v1.>` | A28 |
| T26 | Tampering | **Confused deputy:** a grant obtained for one subject, body or reply inbox is used for another, or a worker asks for another service's operation | the grant is a request token bound to `nsub`, `bh`, `aud` and `rpl`; core takes the service from the grant subject, which only that service's user can publish; `jti` single use | A29, A30 |
| T27 | Denial of service | **Grant flood:** BFF calls (or a compromised worker) flood core with grants and starve the API | per-session and per-hospital grant rate limits in core; a bounded grant handler that answers `rate_limited`/`overloaded` at once; per-worker concurrency limits | A31 |
| T28 | Denial of service | **Core down or unreachable** | every BFF-direct call is refused with 503 `unavailable` and no work; core-initiated calls are unaffected while core is up by definition | A32 |
| T29 | Denial of service | **A slow grant** eats the caller's deadline, or a late grant is used after the worker gave up | the grant budget is the smaller of 250 ms and a quarter of the call's remaining time; a late reply is discarded; the granted token's `exp` is never past the BFF's deadline | A33 |
| T30 | Information disclosure | User access tokens now reach the workers (BFF-direct calls only) | tokens are redacted in worker logs and spans; a worker can only spend one on its own service's grant subject, never on `api.v1`; only operations marked `bff_direct` accept them | A28, A30, A34 |
| T31 | Elevation of privilege | A revoked, expired or not-yet-two-step session gets a grant | core runs the same principal checks as for `api.v1` (denylist, account and hospital state, `SessionSatisfiesMFA`) | A35 |
| T32 | Spoofing | A captured BFF message is replayed to intelligence, getting a fresh grant each time | the BFF's `Mhai-Request-Id` is kept per core copy for 2 minutes and a repeat is refused; only BFF users can publish there; `bff_direct` operations are read-only | A36 |
| T33 | Information disclosure | A grant's answer is steered to another inbox | the grant binds `rpl` to the calling BFF's inbox prefix; check 5 compares it | A37 |
| T34 | Elevation of privilege | The NATS adapter skips a step of core's HTTP chain (request id, body limit, principal, route rule, idempotency, RLS, audit) | the adapter calls the same `http.Handler`; the audience comes from the subject, never a header; contract tests run every route both ways | A38 |

### 13.4 Controls (decided)

- **Authentication between services:** per-service NKeys over TLS (connection), and a per-call Ed25519 request token (message). Both must pass.
- **Authorisation:** core's route table and the new operations table; Python reads the generated operations table only.
- **Hospital scope:** `hid` from the token, checked against the subject and the body; RLS in Postgres through `with_hospital`.
- **Data handling:** jobs carry ids; patient data is read under RLS; model services hold no database credentials; no PHI in logs, traces, errors or dead letters; JetStream storage is encrypted at rest.
- **Concurrency:** the job claim is one atomic `UPDATE ... RETURNING`; completion is one transaction guarded by a unique key; the `jti` set is a per-process map, locked.
- **Secrets:** the request key in the secret store, mounted read-only; workers start only with a valid trusted-keys file; NATS and database passwords refuse placeholders, as today.
- **Third parties:** none. Model servers run inside our network.

### 13.5 Abuse-case tests (the plan writes these first)

- **A1** A message on `inference.v1.echo.ping` with no token gets `unauthorized`, and the handler never runs.
- **A2** Connected as the `pipeline` user, publishing on `inference.v1.echo.ping` or `jobs.v1.>` is a permissions violation.
- **A3** The same valid message sent twice to one worker: the first is answered, the second is refused.
- **A4** A job message redelivered after its result was written is acked with no second decision row.
- **A5** A token minted for `echo.ping` sent on `pipeline.health` is refused (`nsub`, `aud`).
- **A6** A valid core access token placed in `Mhai-Request-Token` is refused.
- **A7** One byte of the body changed: refused (`bh`).
- **A8** A body naming hospital B with a token for hospital A: refused, nothing runs.
- **A9** A `run_query` job for hospital A whose `query_id` belongs to hospital B: `not_found`, and no row of B is read (checked through the database log).
- **A10** Tokens with `alg` `none`, `HS256`, an unknown `kid`, or `exp` 25 h after `iat`: all refused.
- **A11** The worker trying to insert or update a `kb_chunks` row with `hospital_id` null, or with another hospital's id: refused by Postgres.
- **A12** An audit fact naming another actor or hospital is recorded with the token's actor and hospital.
- **A13** Killing the worker right after its commit: `query.completed` still reaches the chain.
- **A14** A synthetic marker string in a question appears in no log line, span or dead letter.
- **A15** A valid token whose reply subject is `_INBOX_worker.x`: no reply, refusal logged.
- **A16** The `pipeline` user subscribing to `_INBOX_gateway.>` or `inference.v1.echo.>`: permissions violation.
- **A17** Each refusal reason gives the user the same 500 `internal_error` body.
- **A18** A 2 MB body is refused by NATS; a 300 KB request/reply body is refused by the worker.
- **A19** A hospital at its in-flight cap gets 429 `rate_limited` while another hospital's question still runs.
- **A20** A job that always fails is delivered 5 times, then lands in `dead.v1` and is `failed`.
- **A21** Deactivating the user cancels their queued job; the worker stops at the next stage.
- **A22** From the worker's login, a query outside `SET LOCAL ROLE` fails; `SET ROLE mhai_gateway` fails.
- **A23** Removing a role from an operation in Go without regenerating fails CI.
- **A24** After an emergency rotation, a job signed with the removed key is refused and re-minted by core.
- **A25** Every token vector in `testdata/request-tokens/` gets the same verdict in Go and Python.
- **A26** Connected as `bff_clinical`, publishing on `jobs.v1.>`, `$KV.>`, `$JS.API.>`, `dead.v1.>`, `authz.v1.grant.pipeline`, `api.v1.platform` or `inference.v1.pipeline.health` (not `bff_direct`) is a permissions violation.
- **A27** `bff_clinical` subscribing to `_INBOX_gateway.>`, `_INBOX_worker.>`, `_INBOX_bff.platform.>` or `api.v1.>`: permissions violation.
- **A28** The `pipeline` user publishing on `api.v1.clinical`: permissions violation.
- **A29** A grant for `echo.ping` with body X, presented with body Y, or on another subject, or with a reply to another inbox: refused by the worker.
- **A30** `pipeline` publishing on `authz.v1.grant.echo`: permissions violation; a grant request on `authz.v1.grant.pipeline` for `echo.ping`: refused by core.
- **A31** 200 parallel grant requests for one session: at most the session's limit are granted, the rest get `rate_limited` within 50 ms each, and an `api.v1` call from another hospital still answers within its normal time.
- **A32** With core stopped, a BFF call to `echo.ping` gets `unavailable` and the handler never runs.
- **A33** A grant delayed past the budget: the worker answers `unavailable`, does no work, and ignores the late reply.
- **A34** A synthetic access token in a BFF call appears in no worker log line or span.
- **A35** A grant asked with a logged-out (denylisted) session's access token, a deactivated user's, or a hospital admin without two-step: refused with core's code, and audited as `intelligence.grant_refused`.
- **A36** The same BFF message (same `Mhai-Request-Id`) sent twice within 2 minutes: the second grant is refused.
- **A37** A granted token whose `rpl` is `_INBOX_bff.clinical.` on a message whose reply is `_INBOX_bff.platform.x`: no reply, refusal logged.
- **A38** Every route in the permission table gives the same status, headers and body over `api.v1` as over HTTP for the auth fixture matrix; an `X-MHAI-Audience` header that contradicts the subject is ignored.

### 13.6 Residual risk and sign-off

- **Replay across worker copies within 60 s** of a request/reply message (§7.4). Accepted: it needs core's NATS credential, gets no data out, and changes no rows. Revisit with a shared KV check if a request/reply operation ever writes data.
- **Job tokens can live 24 h.** Accepted, because they are bound to one subject and one body, jobs are claimed once, and results are read only through core. Query jobs live 10 minutes.
- **A compromised worker** sees the data it is sent, and the pipeline sees its hospital's rows for the jobs it's given. That's inherent; containment is per-service credentials, no database for model services, and RLS.
- **A compromised NATS server** can drop or delay messages, but it can't forge tokens or read the database. Since §17 it also sees access tokens on BFF-direct calls and the frontend's API traffic in the clear unless TLS is on; TLS with `verify` is required in production.
- **Access tokens at the workers** (§17, T30). Accepted while only `bff_direct` operations receive them and none of those is in production yet (§17.6).
- **Replay of a BFF message across core copies** within 2 minutes (T32). Accepted for the same reasons as §7.4: it needs the BFF's NATS credential, and `bff_direct` operations are read-only.
- **Sign-off:** none clinical in this design (no change to what clinicians see). Streaming (§11) will need clinical sign-off. The legal check of the Data Protection Act stays in TODO.md.

## 14. Refinements of the approved core design

These keep the core design's intent and tighten its letter. **All five were accepted by the product owner on 2026-10-28.** The core design and ADR 0010 carry a note at each place they change ("amended by the 2026-10-28 intelligence boundary design").

1. *Accepted.* **Subjects** gain an operation level: `inference.v1.<service>.<op>` (§5.1). Job types use the contract's names (`run_query`).
2. *Accepted.* **The request token** gains `knd`, `nsub`, `bh` and `pids`, a distinct `typ`, and its own signing key (§7.2).
3. *Accepted.* **Replay for jobs** is the database claim, not the `jti` cache, because JetStream redelivers the same message (§7.4). The 2-minute `jti` window for request/reply becomes "until `exp` plus 5 s", which is never shorter than the token's life.
4. *Accepted.* **One NATS user per Python service** instead of one shared `worker` user (§7.6).
5. *Accepted.* **Audit facts from jobs** go through a transactional outbox that core drains (§10.2), rather than a separate result message.

## 15. Task breakdown for the boundary plan

Each task starts with its abuse-case tests (§13.5). The plan, `docs/plans/2026-10-28-plan-3-intelligence-boundary.md`, expands these with files, interfaces and steps.

| # | Task | Main tests |
|---|---|---|
| 1 | **The contract:** `proto/`, `buf.yaml`, generated Go and Python, CI `buf lint`, `buf breaking`, generate-diff; golden messages both ways | golden decode in both languages |
| 2 | **Request tokens in core:** `internal/reqtoken` (mint, and a Go verifier for tests), `REQUEST_KEY_PATH`, `core keys --kind request`, the public trusted-keys file, the shared token vectors | A6, A7, A10, A25 |
| 3 | **The operations table:** `authz` operations, route-to-operation links, `permgen` writes `operations.json` and docs; drift tests | A23 |
| 4 | **NATS config:** per-service NKey users, narrowed `$JS` permissions, inbox prefixes per service, `max_payload`, the `JOBS` stream and consumers, TLS option | A2, A16, A18 |
| 5 | **Core's intelligence client:** `internal/intel`: request/reply with token, headers, deadline, error mapping to problems (adds 503 `unavailable`), trace propagation; the job publisher, `Nats-Msg-Id`, the sweeper; per-hospital in-flight cap | A17, A19, error-table coverage |
| 6 | **The Python verifier:** `intelligence/boundary/verify.py`, the `jti` set, trusted-key loading and `SIGHUP` reload; runs every shared vector | A1, A3, A5–A8, A10, A15, A25 |
| 7 | **The Python worker runtime:** `nats-py` with inbox prefix `_INBOX_worker.<service>`, queue groups, dispatch by subject, concurrency limit, deadlines, reply envelope, `health`, the `echo` service, graceful drain on `SIGTERM` | A1, A15, A18 |
| 8 | **Python database access:** `intelligence_app` login provisioned by `core-migrate`, `INHERIT FALSE`; `with_hospital` on `psycopg`; the no-direct-DB test; narrowed `mhai_worker` grants; the `intelligence_audit_outbox` migration | A9, A11, A22 |
| 9 | **The `run_query` job:** consumer, claim and lease, `+WIP`, the orchestrator behind a model-client interface (stub in tests), completion transaction, outbox facts, `Nak`/`Term`, dead letters, cancellation checks between stages | A4, A9, A13, A20, A21 |
| 10 | **Core's side of questions:** `POST /queries` (202), `GET /queries/{id}` and its result, shapes copied from the frontend contract; the audit drainer (`mhai_audit_drain`); the dead-letter consumer; cancel on deactivation; re-mint on retry | A12, A13, A21, A24 |
| 11 | **Retire the old Python security code:** remove `RBACManager`, `AuditLogger` and the session checks in `SecurePipelineWrapper`; remove the `queue` stubs and Redis from compose; keep PII masking | existing tests still pass |
| 12 | **Observability:** OpenTelemetry in core and the worker, NATS header carriers, span links for jobs, the attribute allow-list, refusal and queue metrics | A14 |
| 13 | **Compose, CI and docs:** compose services and volumes; the `intelligence-boundary` CI job with testcontainers; the end-to-end smoke test; README, ARCHITECTURE and TODO updates; frontend contract statuses | the smoke test |

Order: 1 → 2 → 3 → 4 can run partly in parallel (1 and 2 are independent). 5 needs 1–4. 6 needs 1 and 2. 7 needs 6. 8 is independent of 5–7. 9 needs 7 and 8. 10 needs 5 and 9. 11–13 last.

**Since §17** the plan adds two tasks and widens three: **core's API over NATS** (the adapter, `api.v1`), **the grant endpoint** (`authz.v1.grant.<service>`, rate limits, audit), the `bff` NATS users in task 4, the `rpl`/`via` claims in tasks 2 and 6, and the worker's grant client in task 7. The plan numbers its tasks 1–15 accordingly. The mhai-web BFF's switch from `fetch` to a NATS client is mhai-web work (§17.7).

## 16. Decisions

The product owner approved the design on 2026-10-28 ("approve the design, go ahead") and accepted every recommendation below. Each was an open question in the draft; the answer is now the decision.

1. **Protobuf or JSON Schema for the shared messages?**
   *Decided: Protobuf with `buf`.* CI blocks incompatible changes mechanically, both languages get generated types, and float-heavy model calls stay cheap. The cost is one small toolchain (§6.1).
2. **How long may a clinical question wait before it counts as failed?**
   *Decided: 10 minutes.* After that the clinician has moved on, and a late answer is more confusing than "Couldn't complete, try again". Other job types keep up to 24 hours.
3. **Shared replay check now, or per-worker only?**
   *Decided: per-worker for now.* The subject, body and reply-inbox binding already make a replay worthless. Add the NATS KV check when a request/reply operation first writes data (§7.4).
4. **Audit every model call, or only the user-level events in §10.2?**
   *Decided: user-level events plus every refusal.* Per-call detail goes to traces. Auditing each call would flood the chain without telling an auditor anything more.
5. **Should intelligence write its results to the database itself, or send everything back to core to write?**
   *Decided: intelligence writes results under RLS* (the core design §7.4 already grants this), with audit facts in the outbox. Sending results back would need a new result channel and doubles the data on NATS.
6. **Is `POST /queries` (with status and result) in this plan?**
   *Decided: yes,* with shapes copied from the frontend contract. The core design's end-of-step goal is a question going all the way through, and the endpoint is the only honest test of that.
7. **A separate signing key for request tokens?**
   *Decided: yes* (§7.2). It costs one more key file and keeps the two token kinds from ever standing in for each other.
8. **Per-hospital cap on questions in flight?**
   *Decided: 10 per hospital to start,* tuned from measurements; over it gets 429 `rate_limited`. It stops one hospital starving the others on a shared deployment.
9. **Streaming:** decide with the performance plan, not here?
   *Decided: yes.* Reserve `stream.v1.>` and the `strm` claim now, build nothing, and keep clinical sign-off as the gate (§11).
10. **Its own NATS account for intelligence?**
    *Decided: not yet.* Per-service users and subject permissions are enough for three people to run. Revisit before production if NATS is shared with anything else.

## 17. Decision 28 Oct: frontend ingress over NATS

The product owner decided on 2026-10-28: "frontend talks to nats which can either talk to core or intelligence, when it want to talk to intelligence, intelligence ask core if permission has been given before it go ahead with it". [ADR 0011](../adr/0011-frontend-ingress-over-nats.md) records it and amends ADR 0010. This section is the design.

### 17.1 The BFF talks to core over NATS

- **The BFF is a NATS client** on the Node server side only (nats.js over TCP), never in the browser. Each app has its own NATS user: `bff_clinical` and `bff_platform`, with inbox prefixes `_INBOX_bff.clinical` and `_INBOX_bff.platform`. *My call:* one user per app rather than one shared `bff` user, so a leak of the clinical app's credential can't reach platform routes, and the subject alone fixes the audience. Both are "the `bff` user" in the product owner's sense.
- **Core serves the frontend contract on `api.v1.clinical` and `api.v1.platform`,** in queue group `core`. One message is one HTTP request:
  - headers `Mhai-Method`, `Mhai-Path` (path and query, under `/api/v1`), and an allow-list of HTTP headers (`Authorization`, `Content-Type`, `Idempotency-Key`, `If-None-Match`, `X-MHAI-Client-IP`, `traceparent`); everything else is dropped, `tracestate`, `baggage` and `X-Request-ID` included, because core makes its own request id so none can be forged into the audit chain (as over HTTP today);
  - body: the HTTP body, at most 64 KB as today;
  - reply headers `Mhai-Status-Code` plus the response headers core sets (`Content-Type`, `Cache-Control`, `ETag`, `Retry-After`, `X-Request-ID`); reply body: the response body.
- **A thin adapter** (`internal/natsapi`) turns each message into an `*http.Request` and runs it through **the same `http.Handler`** as HTTP: request id, body limit, principal and access token, route rule, idempotency, the RLS transaction and audit. Nothing in Plan 2's chain is rewritten.
  - The audience comes from the subject (`api.v1.platform` → `platform`). An `X-MHAI-Audience` header in the message is dropped.
  - Each message gets a deadline (30 s, or less if the BFF sends `Mhai-Timeout-Ms`); the handler's context ends with it.
  - Core answers through `allow_responses`, so it can reply to the BFF's inbox without a general publish right there.
- **The `X-MHAI-Service` BFF key is replaced, not doubled,** on the NATS path. *Decision and why:*
  - The key and the NATS seed would live in the same place (the BFF's server environment), so a leak of one is a leak of both: doubling adds a second secret to rotate without adding a second barrier.
  - What the key proved ("this request came from our BFF") is now proved by NATS: only `bff_<app>` may publish `api.v1.<audience>`, which the server enforces per connection over NKeys and TLS, and a test proves no other user can (A26–A28).
  - `X-MHAI-Client-IP` is trusted for the same reason the key made it trusted before: only the BFF can put a message on the subject.
  - The key and the HTTP API stay **only during the switch-over**, behind `CORE_HTTP_API=on`, while mhai-web still uses `fetch`. After it, core's HTTP listener serves only `/healthz` and `/readyz`, bound to `127.0.0.1` (the container's own health check).

### 17.2 The BFF calls intelligence directly, and intelligence asks core

For operations marked `bff_direct` in the operations table (§8.1), the clinical BFF may publish on `inference.v1.<service>.<op>` itself, with the user's access token in `Authorization`. Before any work:

1. The worker runs check 1 (sizes), then publishes `GrantRequest{access_token, operation, subject, body_hash, patient_ids, reply_prefix, request_id, deadline_ms}` on `authz.v1.grant.<service>`, its own grant subject. `reply_prefix` is the BFF inbox prefix the message's reply subject starts with (`_INBOX_bff.clinical.`); a reply subject with any other prefix is refused at once, with no reply.
2. Core verifies the access token (signature, `aud mhai-bff`, expiry, denylist), the session, account and hospital state, and `SessionSatisfiesMFA`, exactly as the principal middleware does; then that the operation exists, is `bff_direct`, belongs to the service named in the subject, and allows one of the user's roles; that every patient id belongs to the user's hospital; and that `request_id` hasn't been seen in the last 2 minutes (T32). It audits `intelligence.granted` or `intelligence.grant_refused` under a request id it makes itself; the BFF's `request_id` is only the replay key and the token's `rid`.
3. Core answers with `GrantResponse{request_token}`, a request token (§7.2) with `aud` the service, `nsub` the subject, `bh` the body hash, `rpl` the BFF inbox prefix, `via` `grant`, `knd` `req`, and `exp` the smaller of the operation's timeout and the BFF's deadline. Or with `GrantRefused{code}`.
4. The worker runs checks 2–9 (§7.3) on that token against the message it holds. Any failure: refuse, no work.

The worker never decides a permission, never trusts a hospital, user or role from the message, and never runs checks 2–9 on the access token. A request token that arrives in the message itself is accepted only from core (`rpl` `_INBOX_gateway.`, `via` `core`), so a BFF can't present a token of its own.

**Jobs are unchanged:** only core publishes `jobs.v1.>`. A BFF request that starts a long job (`POST /queries`) goes to core over `api.v1`.

**What is `bff_direct` in this plan:** only `echo.ping`, which never runs in production. No production operation is opened to the BFF until one needs it (for example DIA page images) and its review says so.

### 17.3 Timeouts, limits and core being down

- **Grant budget:** the smaller of 250 ms and a quarter of the call's remaining time. A late grant reply is discarded; the worker answers `unavailable`.
- **Core unreachable** (no responders, timeout, disconnect): `unavailable`, no work. There is no cache of earlier grants: each call is granted on its own.
- **Limits in core:** a grant handler bounded to N in flight that answers `overloaded` at once past that; per-session (20 per second) and per-hospital (200 per second) grant rates, in memory per core copy, answering `rate_limited`. The per-worker concurrency limit (§9.1) bounds how many grants a worker can ask for at once.
- **Latency:** one extra NATS round trip, about 1 ms on one network, plus an Ed25519 verify of the access token, one sign of the request token and a cached account-state lookup (30 s cache, as the principal middleware). Well under the budget, and only on BFF-direct calls.

### 17.4 NATS users and permissions after §17

| User | Publish | Subscribe | Replies |
|---|---|---|---|
| `bff_clinical` | `api.v1.clinical`; the `bff_direct` operations' `inference.v1.<service>.<op>` subjects, listed one by one | `_INBOX_bff.clinical.>` | — |
| `bff_platform` | `api.v1.platform` | `_INBOX_bff.platform.>` | — |
| `gateway` (core) | `inference.v1.>`, `jobs.v1.>`, the KV and narrowed `$JS` subjects (§7.6) | `_INBOX_gateway.>`, `api.v1.>` (queue `core`), `authz.v1.grant.>` (queue `core`), the `DEAD` consumer | `allow_responses` (to the BFF and the workers) |
| each service (`pipeline`, `echo`, ...) | its consumer's `$JS` subjects, `dead.v1.>`, `authz.v1.grant.<service>` | `inference.v1.<service>.>`, `_INBOX_worker.<service>.>` | `allow_responses` |

No BFF user has `jobs.v1.>`, `$KV.>`, `$JS.>`, `dead.v1.>` or `authz.v1.>`. No worker has `api.v1.>` or another service's grant subject.

### 17.5 Error mapping for the BFF

- Over `api.v1`, the BFF gets exactly what HTTP gave: `Mhai-Status-Code`, the problem body and headers. No responders or a timeout on `api.v1` is the BFF's own 503 `unavailable`.
- On a BFF-direct call, the worker answers with `Mhai-Status`: `ok`; a grant refusal as `grant_refused:<code>` (core's code, which the BFF shows as that code and status); `unavailable` for no grant in time; and every other refusal or failure as in §10.1 (the BFF shows 500 `internal_error` for `unauthorized`, `forbidden` and `invalid_request`).

### 17.6 Security notes

- The threats are T24–T34 (§13.3) and the abuse cases A26–A38 (§13.5).
- **Access tokens reach workers** only on BFF-direct calls. Worker logs and spans redact them (§10.3 allow-list), and a worker can spend one only on its own grant subject.
- **TLS with `verify`** is required in production on every NATS connection, because the frontend's API traffic now crosses NATS.
- **Clinical sign-off:** none; what clinicians see doesn't change.

### 17.7 What changes outside this repository

The mhai-web BFF swaps `fetch` for a NATS client (nats.js over TCP, on the Node server only), with its app's NKey seed in the server environment and the same request and reply shape as §17.1. That is mhai-web work, listed in mhai-web's TODO.md; this plan builds core's side and keeps HTTP on until the switch.

