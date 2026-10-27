# Backend architecture (core service in Go, Python models, NATS) and authentication: design

- **Status:** Approved (product owner, 2026-10-26)
- **Date:** 2026-10-26
- **Decision record:** [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md)
- **Builds on:**
  - the API gateway design (`docs/specs/2026-10-08-api-gateway-phase1-design.md` on `feat/8-api-gateway`). Its §5 auth rules carry over, with the changes listed here.
  - the frontend contract, `docs/specs/2026-10-17-frontend-requirements.md`, especially §2 (auth and sessions), §3 (roles and tenancy) and §14.1 and §14.6 (platform admin app).

## 1. Why

MHAI is about to go from one hospital to several. The product owner is in talks with more hospitals now. Each hospital brings its own staff, its own EMR connections, and its own alerts and audit trail. The backend has to stay reliable, cheap to run and easy to reason about as hospitals are added.

The team is three people, and two of them already write Go. The model code (agents, retrieval, guardrails, PII masking) is Python and depends on the Python ML stack. The API is not built yet: the gateway design and Plan 1 exist, but no Go API code does.

So the API and everything around it is written in Go, the models stay in Python, and NATS connects them.

## 2. Goals, non-goals, anti-goals

**Goals**
- One shared deployment for every hospital, with each hospital's data kept separate. The database enforces the separation, not just the code.
- The core service (Go) that implements the frontend contract exactly, starting with sign-in, sessions, roles and tenancy.
- Python model services that can be reached only through NATS, and that check every request themselves before answering.
- Security rules (permissions, patient isolation, audit) written once, in Go.

**Non-goals (for this design)**
- Rewriting the Python agents, retrieval, guardrails or PII masking. They are wrapped, not rewritten.
- One deployment per hospital, or hospital-hosted installs.
- Single sign-on (SSO) in the first build. The data model and routes are ready for it; it comes when a hospital needs it (§6.6).
- The platform admin, analysis, clearances and EMR endpoints. They follow in later plans, on the foundation built here.

**Anti-goals: these would make it a failure even if it worked**
- Two copies of a security rule, one in Go and one in Python, that can drift apart.
- A Python service that anything other than the core service can call.
- Any route that takes the hospital from the request instead of the session.
- One hospital's data visible to another through a query bug.

## 3. Constraints

- **The frontend contract is fixed.** mhai-web already calls the §2, §3 and §14 shapes, on sample data. The core service must match them field for field, error code for error code.
- **The browser never holds the API token.** Each Next.js app (the BFF) keeps it server-side. Every request reaches the core service from a BFF, never from a browser (§2.1).
- **No rule on where the data must be stored** (product owner, 26 Oct). Nigeria's Data Protection Act treats health data as sensitive wherever it is stored. Someone qualified should check compliance before go-live.
- **Synthetic data only** until go-live. No PHI in code, tests, logs or PRs (SKILLS.md).
- **Small team:** standard libraries over frameworks, and as few moving parts as possible.

## 4. Architecture

```
browser ──> BFF (Next.js, clinical / platform)
              │  HTTPS + service credential, client IP header
              v
        ┌──────────────┐        ┌──────────────┐
        │ core service │──SQL──>│  PostgreSQL  │  (row-level security per hospital)
        │  (API, auth, │        └──────────────┘
        │  tenancy,    │        ┌──────────────┐
        │  audit, EMR) │──S3───>│ object store │  (documents, letters, exports)
        └──────┬───────┘        └──────────────┘
               │ NATS (TLS, per-service credentials)
               │   request/reply: inference.v1.<service>   (someone is waiting)
               │   JetStream:     jobs.v1.<hospital>.<type> (nobody is waiting)
               v
        ┌───────────────────────────────┐
        │ Python services (queue groups)│  agents, retrieval, guardrails,
        │ verify every request token    │  PII masking, embeddings, NLI;
        └───────────────────────────────┘  the LLM through Ollama (ADR 0003)
```

### 4.1 The core service

One Go service. It is the only thing the BFFs talk to and the only thing that publishes to the Python services. It owns:

- sign-in, sessions and two-step verification (§6);
- authorization and tenancy (§7);
- every endpoint in the frontend contract;
- audit, the hash chain from the gateway design §11;
- later: EMR connectors and the event pipeline (ADR 0009), small-group hiding for Analysis, and clearance letters.

It talks to Postgres directly. It never imports model code.

### 4.2 The Python services

These are the existing agents (DIA, QDA, CCA, HGA), retrieval and the knowledge base, guardrails, PII masking, and the inference clients from ADR 0008. They run as NATS workers in queue groups, so NATS spreads requests across however many copies are running.

They:
- never accept a connection from anything but NATS;
- verify the request token on every message before doing anything (§5.3);
- read and write only the tables their database role allows (§7.4).

### 4.3 NATS

NATS is the only link between Go and Python. It replaces Redis as the job queue: `services/intelligence/intelligence/queue` was a stub, so nothing is lost. Redis is kept only if a cache is needed later.

**A clinical question is a job, not a held-open call.** Keeping the contract (gateway design §4 and §7):
- `POST /queries` returns 202 with the query id.
- The question goes on JetStream as `jobs.v1.<hospital_id>.query`.
- Query state and progress live in Postgres.
- The app polls the status and stages, as mhai-web already does.

A full run takes tens of seconds, which is too long to hold a request open. The job's token lives until the job's deadline (§5.2).

**The pipeline stays in one Python service.** The existing orchestrator runs inside a single `pipeline` service: PII masking, retrieval, generation, NLI and HGA, in the order it already uses. Inside that service, the model clients from ADR 0008 call the models (Ollama and the inference service) as they do today. Go never runs the chain, so nothing is rewritten. Python services don't call each other over NATS.

**Request/reply** is for short calls that someone is waiting on and that end in seconds, for example a document's page images or a health check. Subjects are `inference.v1.<service>`. Workers subscribe in a queue group named after the service. The core service sets a timeout per subject. A timeout is a failure the user sees as "Couldn't get an answer, try again"; it is never retried silently.

**JetStream** is for work nobody is waiting on: EMR events to check, feedback to learn from, knowledge-base rebuilds. The details:
- One stream, `JOBS`, on the subjects `jobs.v1.<hospital_id>.<type>`.
- Consumers acknowledge explicitly.
- `max_deliver` is 5, with backoff.
- After that, a message moves to `dead.v1.<hospital_id>.<type>`, which the platform app's Failed jobs view reads.
- Messages carry ids and a versioned payload, not patient data where an id will do.

**NATS KV** holds short-lived shared state: the token denylist (§6.3) and login-attempt counters (§6.2).

### 4.4 PostgreSQL

There is one database. Every hospital-owned table has a `hospital_id` column, and row-level security (RLS) restricts rows to the hospital set for the transaction (§7.3). The schema is owned by migrations in the Go service. Python gets its own database roles (§7.4).

### 4.5 Deployment (my call)

- **Hosting:** AWS, in one region, where the demo already runs. Nothing in the code is tied to a region, so the deployment can move if a hospital or regulator asks.
- **First build:** `docker compose` with the core service, Python services, NATS, Postgres and object storage. Production later is three NATS servers, managed Postgres and S3.
- **Configuration:** from the environment. Startup fails if a required secret is missing or is a placeholder.
- **Encryption:** TLS on every hop, and encryption at rest for Postgres, S3 and NATS JetStream storage.

### 4.6 Repository layout (my call)

The Go service lives in this repository under `services/core/`, with its own `go.mod`. It stays next to the Python code and to the contract docs it implements, so one PR can change the contract and both sides. The Python project is in `services/intelligence/` (package `intelligence`) and the NATS config in `services/nats/` (naming decided by the product owner, 27 Oct).

CI gains a Go job: `go vet`, `staticcheck`, `go test -race`, and an integration test against real Postgres and NATS in containers.

## 5. How Go and Python trust each other

There are two layers, and both must pass.

### 5.1 Connection layer: NATS credentials

- Each service has its own NATS user, with NKey or JWT credentials, over TLS.
- **Gateway:** may publish on `inference.v1.>` and `jobs.v1.>`, and may subscribe to replies.
- **Python workers:** may subscribe on their own `inference.v1.<service>` and on their JetStream consumers, and may publish only replies and `dead.v1.>`.
- Nothing else can publish on `inference.v1.>`.

### 5.2 Request layer: a signed request token

The core service attaches a token to every request and job, in the NATS header `Mhai-Request-Token`:
- signed with EdDSA (Ed25519), using the core service's private key;
- `iss`: `mhai-gateway`;
- `aud`: the service, for example `cca`;
- `sub`: the user id;
- `hid`: the hospital id;
- `roles`;
- `rid`: the request id, also in the audit trail;
- `jti`;
- `iat`, and `exp` 60 seconds later. Jobs get `exp` equal to the job's deadline, up to 24 hours.

### 5.3 What every Python service checks, before anything else

1. The signature verifies against the core service's public key. The key is configured, and two keys are accepted during rotation.
2. `iss` is right, `aud` is this service, and the token hasn't expired, allowing 5 seconds of clock skew.
3. The `jti` hasn't been seen in the last 2 minutes (an in-memory replay cache per worker).
4. Every hospital id and patient id in the payload belongs to `hid`. A payload's hospital is never trusted on its own.
5. The roles allow the action: one small table per service, generated from the core service's permission table (§7.2) so the two can't drift.

Any failure means no work is done. The service replies `{"error": "unauthorized"}` and logs the reason, without PHI. This is the "fail closed" rule of ADR 0005, applied at the boundary.

## 6. Authentication (first build)

The behaviour is the frontend contract §2, built in Go. These are the changes from the gateway design §5.

### 6.1 Accounts

- **Login:** a person signs in with their work email.
  - `users` holds `id`, `email` (unique, case-folded) and `password_hash`.
  - `hospital_id` is null for platform accounts.
  - Also: `is_active`, `must_change_password`, `terms_version`, `terms_accepted_at`, `last_login_at`.
- **Profile:** `display_name`, `full_name`, `role_title`, `specialty`.
- **Roles:** `user_roles (user_id, role)`.
  - `clinician`, `hospital_admin` and `analyst` can be combined, on clinical accounts only.
  - `platform_admin` and `auditor` are on platform accounts, which are separate accounts (frontend §3).
- **Passwords:** argon2id (`golang.org/x/crypto/argon2`), tuned to about 250 ms on the production host.
  - They are checked against a bundled list of breached passwords at set and reset time. There is no call to an outside service.
  - My call: patient-data systems should not depend on a third-party lookup.
- **Creating accounts:** only by invite (from the hospital admin or platform admin endpoints) or with a CLI for the first platform admin. There is no sign-up.

### 6.2 Sign-in and lockout

- `POST /auth/login {username, password, device_token?}` returns either `{access_token, expires_in, user}` or `{mfa_required: true, mfa_token}`. The `user` has every field in frontend §2.2.
- A wrong email and a wrong password get the same error.
- **Lockout:** 5 failures for one (login name, client IP) in 15 minutes gives **429 `login_locked`**.
  - Unknown logins get the same reply, so the response can't reveal which accounts exist.
  - The counter is in NATS KV.
- **Client IP:** comes from the `X-MHAI-Client-IP` header. It is trusted only on requests carrying the BFF's service credential (§8).
- Every attempt is audited, successful or not.

### 6.3 Tokens and sessions

- **Access token:** a JWT signed with EdDSA, valid 15 minutes.
  - Claims: `sub`, `hid`, `roles`, `mfa`, `amr`, `sid` (session id), `jti`, `iat`, `exp`.
  - Asymmetric signing replaces HS256 from the gateway design, so the signing key never has to leave the core service.
- **Sessions:** one row per device in `sessions`, with `id`, `user_id`, `created_at`, `last_seen_at`, a user-agent summary and `ip_prefix`.
  - Each session has a refresh-token family. Refresh tokens rotate on every use.
  - Reusing an old refresh token revokes the whole family. This catches a stolen token.
  - The BFF refreshes while the user is active. Sessions end after the hospital's `idle_timeout_minutes`.
- **Revocation:** signing out, "sign out everywhere", deactivating a user or hospital, or resetting their two-step verification revokes the session or sessions.
  - The `jti`s still live go into the NATS KV denylist, with a TTL equal to their remaining life.
  - Every request also reloads the user's `is_active` flag and the hospital's status, with a short cache of 30 s at most.
- **Device endpoints:** `GET /auth/sessions`, `DELETE /auth/sessions/{id}` and `POST /auth/sessions/revoke-all`, as in frontend §2.6.

**The refresh token (an addition to the contract, my call).** Login and the two-step challenge also return `refresh_token`, for the BFF only. It is never sent on to the browser. `POST /auth/refresh {refresh_token}` returns a new `{access_token, expires_in, refresh_token}`. This is an additive change: the frontend requirements doc §2.2 and mhai-web's BFF are updated to match.

### 6.4 Two-step verification (TOTP)

- **Library:** `github.com/pquerna/otp`.
  - 30-second steps, with one step of tolerance either side.
  - The last step used is stored, so a code can't be used twice.
- **Secrets:** encrypted with AES-256-GCM, under a key from the environment. KMS can come later.
- **Backup codes:** 10 single-use codes, stored hashed. `POST /auth/mfa/recovery-codes` replaces them.
- **Challenge:** `POST /auth/mfa/challenge {mfa_token, code, trust_device?}`.
  - The `mfa_token` lasts 5 minutes and allows 5 attempts.
- **Who must use it:**
  - hospital admins, analysts, platform admins and auditors, always;
  - clinicians, when their hospital's `require_mfa_for_clinicians` is on.
- **Trust this device:**
  - only when the hospital's `allow_trust_device` is on;
  - the `device_token` is bound to the user it was issued to and refused for anyone else;
  - **my call, on the frontend doc's open question:** a trusted device never skips the code for `hospital_admin`, `analyst`, `platform_admin` or `auditor`.
- **Step-up:** sensitive writes take `mfa_code` or `backup_code` in the body (frontend §14.6).
  - A wrong code gets **403 `mfa_invalid`**.
  - 5 wrong codes in 15 minutes gets **429 `mfa_locked`**.

### 6.5 Terms, password change, forgot and reset, invites

- `POST /auth/terms {version}` records the version and the time.
- `POST /auth/password` changes the password. It needs the current password, and signs out other devices.
- `POST /auth/password/forgot {email}` always returns 202. The reset link is valid for 30 minutes and works once.
- `POST /auth/password/reset {token, new_password}` signs out every device.
- **Invites:** a 7-day link to set a password and enrol two-step verification.
- Reset and invite emails go through a mail provider (§11, open item 1).

### 6.6 Single sign-on (later)

The tables and routes from frontend §2.4 and §14.4 are reserved:
- `hospital_sso` holds the per-hospital OIDC settings, with the secret encrypted;
- `users.oidc_issuer` and `users.oidc_subject` link an account.

No hospital in talks uses Microsoft or Google work accounts (product owner, 26 Oct), so SSO waits until one needs it. When it comes, it follows gateway design §5.3: authorization code with PKCE, accounts linked in advance, roles only from MHAI, and an MFA claim required for admins.

## 7. Authorization and tenancy

### 7.1 The principal

After the token checks, middleware builds a principal: user id, hospital id (or none, for platform accounts), roles, `mfa`, `amr` and session id. Handlers get only the principal. They never read identity or hospital from the request.

### 7.2 One permission table

- Each route declares the roles it needs and whether it needs a step-up code, in one Go table.
- A test fails if any route is missing from the table.
- The table generates:
  - the Python services' role checks (§5.3 step 5);
  - a markdown table in the docs, so the contract and the code can't drift.

The rules come from frontend §3:
- roles combine;
- a `hospital_admin` who isn't a `clinician` gets **403** on clinical endpoints;
- platform endpoints need `X-MHAI-Audience: platform` and a platform account, or the request gets **403 `not_platform`**;
- auditors get **403 `forbidden`** on every write.

### 7.3 Hospital separation, enforced twice

1. **In the core service:** every clinical-side query is scoped by `principal.hospital_id`. Another hospital's resource returns **404**, never 403, so ids can't be probed.
2. **In the database:** every hospital-owned table has RLS enabled with a policy `hospital_id = current_setting('mhai.hospital_id')::uuid`.
   - The core service runs each request in a transaction that first runs `SET LOCAL mhai.hospital_id = …` from the principal.
   - A query that forgets the filter returns nothing, never another hospital's rows.
   - The app's database role is not the table owner and has no `BYPASSRLS`, so the policy always applies.

### 7.4 Database roles

| Role | Used by | Can |
|---|---|---|
| `mhai_gateway` | core service, clinical-side requests | hospital tables under RLS; audit insert-only |
| `mhai_platform` | core service, platform endpoints | hospital, user, session and audit metadata across hospitals, through views that carry **no clinical content** |
| `mhai_worker` | Python services | only the tables they use (KB chunks, decisions they write), under RLS with the hospital from the verified token |
| `mhai_migrate` | migrations only | owns the schema |
| `mhai_retention` | retention job | deletes after a checkpoint (gateway design §12) |

### 7.5 What happens to the Python security code

- `RBACManager` and the session checks in `SecurePipelineWrapper` are replaced:
  - token verification (§5.3);
  - the permission check generated from Go's table.

  This happens in step 4.
- `services/intelligence/intelligence/security`'s PII masking stays, because it is part of the model pipeline.
- **Only Go writes the audit hash chain.** A Python service puts its audit facts (what it did, for which request id) in its reply or job result, and Go appends them. Python's `AuditLogger` is removed in step 4.
- **Go's migrations own the whole schema**, including the knowledge-base tables (pgvector) that Alembic or `init-db` create today. Those are ported in step 1, and the Python schema tooling is removed.

### 7.6 Patients and shared content

- **Patient scope.** Before publishing, the core service checks that a patient id belongs to the principal's hospital. The patient id goes in the token. The worker then reads only through `mhai_worker` under RLS, with `mhai.hospital_id` set from the verified token. A patient row from another hospital is simply invisible to it.
- **Shared reference content.** Clinical guidelines and reference text that every hospital may read sit in rows with `hospital_id IS NULL`, and the RLS policy allows them read-only. A hospital's own documents and every patient chunk carry that hospital's id. The product owner decided this on 26 Oct: shared content is allowed, read-only, and never holds patient data.

### 7.7 Audit

Every sign-in, sign-out, failed attempt, lockout, two-step event, role change, session revoke and permission refusal is written to the append-only hash chain (gateway design §11). Each entry records actor, hospital and request id, and never contains PHI.

## 8. The BFF link

- Each BFF has a service credential: a client certificate (mTLS), or failing that a long random key in `X-MHAI-Service`.
- **Trusted headers:** `X-MHAI-Client-IP` and `X-MHAI-Audience` are honoured only on requests that carry the credential.
- **Rate limits** are per user, as frontend §2.1 asks, because every request comes from a BFF.
- **CORS** stays closed, since browsers never call the core service.

## 8a. Where the two contracts disagree

The frontend contract wins, because mhai-web is already built against it (frontend doc §13). That means:

- **Status codes:** `mfa_required` and `mfa_invalid` are **403**.
- **Errors on platform routes** carry only `code`, plus `blocking` on 409 `blocked`.
- **`Idempotency-Key`** is honoured on every write, with keys kept for 72 hours. It is built in step 1 as middleware, along with the error catalogue.

## 8b. Hospitals before the platform endpoints exist

In the first build, hospitals and their settings are created with the core service CLI, using synthetic data only:
- `idle_timeout_minutes`;
- `review_overdue_hours`;
- `require_mfa_for_clinicians`;
- `allow_trust_device`.

The first accounts are created the same way. Invites wait for the staff and platform endpoints. Forgot-password and reset need mail, so they ship once the mail provider is chosen (§11, open item 1). My suggestion is AWS SES, since we're on AWS.

## 9. Testing

- **Unit:** token signing and verification, TOTP replay, argon2 parameters, the permission table's completeness, and lockout counters.
- **Integration**, against real Postgres and NATS in containers:
  - **Every auth route** in frontend §2, with its exact reply and error codes.
  - **Enumeration:** unknown and locked accounts get the same reply.
  - **Refresh reuse:** reusing a refresh token revokes its family.
  - **Revocation:** a denylisted token is refused within one request.
  - **Hospital separation:** another hospital's id gives 404, and a deliberately unscoped query returns nothing under RLS.
  - **Platform accounts:** they can't reach clinical routes, and clinical accounts can't reach platform routes.
- **Boundary tests for the Python side.** Each of these is refused, and nothing runs:
  - a tampered token;
  - a token for the wrong audience;
  - an expired token;
  - a replayed `jti`;
  - a payload whose hospital doesn't match `hid`.
- **Contract:** the core service's OpenAPI document is generated and committed. CI fails if it changes without the frontend requirements doc changing too. mhai-web then runs its live-mode BFF tests against the core service.

## 10. Order of work

1. **Foundation:**
   - the Go module, config, logging (no PHI), health endpoint and CI job;
   - Postgres migrations;
   - NATS with credentials;
   - `docker compose`.
2. **Authentication (§6):** accounts, sign-in, lockout, tokens, sessions, two-step verification, terms, password and devices. End: mhai-web's clinical and platform apps sign in for real against the core service, using synthetic accounts.
3. **Authorization and tenancy (§7):** the principal, the permission table, RLS, database roles and audit. End: the hospital separation and role tests pass.
4. **The Python boundary (§5):** wrap the inference services as NATS workers with token verification. This is Plan 1, rewritten for NATS. End: a clinical question goes browser → BFF → core service → NATS → Python and back.
5. **Then the contracts,** in the order the product owner picks: staff and settings, corrections, alerts and EMR, analysis, clearances, platform operations.

Each step gets its own build plan. This design covers steps 1 to 4. Steps 1 to 3 are the first plan.

## 11. Open items

1. **Mail provider** for invites and password resets. Needed in step 2.
2. **Scale:** sizing assumes up to about 50 hospitals and 5,000 staff accounts in the first year. My assumption; correct it if talks suggest more.
3. **A legal check of the Data Protection Act** before go-live: data processing agreements with each hospital, and where data is stored.
4. **Shared reference content across hospitals** (§7.6): decided by the product owner on 26 Oct. Shared, read-only reference content with no patient data is allowed.

## 12. Calls I made (check these)

- AWS, one region, movable (§4.5).
- Go code in this repository under `services/core/` (§4.6).
- NATS replaces Redis for jobs; Redis is kept only as an optional cache (§4.3).
- EdDSA for access and request tokens instead of HS256 (§5.2, §6.3).
- A bundled breached-password list, not an online lookup (§6.1).
- A trusted device never skips the code for admin, analyst or auditor roles (§6.4).
- Row-level security as the second wall between hospitals (§7.3).
- SSO deferred until a hospital needs it (§6.6).
- A clinical question is a queued job. The pipeline stays one Python service (§4.3).
- A refresh token for the BFF, as an additive contract change (§6.3).
- Python's role checks and audit logger are replaced; Go's migrations own the whole schema (§7.5).
- Where the two contracts disagree, the frontend contract wins (§8a).
- Hospitals are seeded from the CLI until the platform endpoints exist. Invites and password reset wait for mail (§8b).

## 13. Left to the builder

- Exact Go libraries for routing (the standard `net/http` mux is enough), SQL access (`pgx` with `sqlc` suggested) and migrations (`goose` suggested).
- Argon2id parameters, tuned on the real host.
- Subject timeouts per service, set from measured latencies.
- Log format and fields, as long as no PHI or token appears.
