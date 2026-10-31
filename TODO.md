# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## Decided

- **Go alongside Python.** The core service (Go, `services/core/`) owns the API, auth, tenancy, audit and EMR plumbing. The Python services (`services/intelligence/`) keep the models and are reached only over NATS. See [ADR 0010](docs/adr/0010-api-in-go-models-in-python-over-nats.md).
- **The intelligence boundary (product owner, 28 Oct).** Core calls the Python services over NATS with a per-call Ed25519 request token; Protobuf with `buf` for the messages; a 10-minute limit on clinical questions; per-worker replay check; user-level audit plus refusals; intelligence writes results under RLS with audit facts in an outbox; `POST /queries` in the plan; a separate request-signing key; 10 questions in flight per hospital; streaming left to the performance plan; no separate NATS account yet. See the [design](docs/specs/2026-10-28-intelligence-boundary-design.md) §16 and [Plan 3](docs/plans/2026-10-28-plan-3-intelligence-boundary.md).
- **Frontend ingress over NATS (product owner, 28 Oct).** The BFF talks to core over NATS (`api.v1.<audience>`), its NATS identity replaces the `X-MHAI-Service` key, and the BFF may call intelligence operations marked `bff_direct`, which intelligence serves only after core grants them (`authz.v1.grant.<service>`). See [ADR 0011](docs/adr/0011-frontend-ingress-over-nats.md) and the design §17; built in Plan 3.

## Open items for the core service

From §11 of the [design](docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md):

- **Mail provider** for invites and password resets. Sign-in is built (Plan 2) without it; invites and forgot/reset wait for it. AWS SES is the suggestion.
- **Scale assumption.** Sizing assumes up to about 50 hospitals and 5,000 staff accounts in the first year. Correct it if talks suggest more.
- **Data Protection Act check** before go-live: data processing agreements with each hospital, and where data is stored. Needs someone qualified.

## Next for the core service (after Plan 2)

- **Plan: operations baseline** — backups and tested restore, monitoring and alerts, failover — before any real patient data. Status of each area: ARCHITECTURE.md, "Operations".
- **Invite flow.** It must close first-sign-in self-enrolment: today a CLI-created account enrols its own two-step at first sign-in, so whoever has the first password owns the second factor. Invites bind enrolment to the invited person. Needs the mail provider.
- **Forgot / reset password** (contract §2.5). Needs the mail provider.
- **Turn core's HTTP API off** (`CORE_HTTP_API=off`, probes only on localhost) once mhai-web's BFF talks to core over NATS. That switch is mhai-web work (its TODO.md, "BFF over NATS"). The same change brings mhai-web's `contract.ts` in line with Plan 3: `ClinicalResult` gains `trust_band` and `cited_chunk_ids`, and the problem codes gain 405 `method_not_allowed`, 413 `payload_too_large` (both on the NATS path) and 503 `unavailable`.
- **Citations are empty** in `GET /queries/{id}/result` until the pipeline stores citation details: today it stores only `cited_chunk_ids`. Flagged to the product owner and the intelligence lead (Plan 3 T12).
- **Patient ids in questions and tokens** (`pids`, plan departure 7): with the EMR plan. Until then retrieval returns shared guidelines only.
- **Platform job views:** retry and cancel endpoints and the Failed jobs list (frontend §14.8). Plan 3 built the dead-letter consumer and the re-mint they will call.
- **The shared replay check** (NATS KV) when a request/reply operation first writes data (boundary design, decision 3). Today each worker copy keeps its own `jti` set, which is enough while request/reply operations only read.
- **Hospital setup flow.** `setup_incomplete` hospitals can sign in so the first admin can finish setup; revisit when the flow exists (T8 M8). Nothing activates a hospital yet (no CLI command or endpoint), and `POST /queries` asks for an `active` hospital, so a clinician of a CLI-made hospital gets 401 `unauthenticated` on every question until an operator sets `hospitals.status = 'active'` by hand (found by the Plan 3 smoke test, which does exactly that). Add `core hospitals activate` or the setup flow, and decide whether `setup_incomplete` should be 403 rather than 401.
- **Admin unlock** for the per-login ceiling, with the staff endpoints (only a trusted device gets past it today); alert on `auth.login.ceiling` audit rows.

### Deferred minors to follow up

From the Plan 2 reviews (task ids in brackets). None blocks merge.

**Database and audit**
- **Audit tail deletion.** The hash chain can't show rows deleted from the end. Add signed checkpoints (`audit_checkpoints`) on a timer. [T4 M6]
- **Direct `audit_log` inserts.** The app roles can INSERT into `audit_log` without the chain lock. Route every append through a SECURITY DEFINER `audit_append()` and revoke INSERT. [T4 M8]
- **Lateral `SET ROLE` / `set_config`** inside a request transaction can reach `mhai_auth` or another hospital; it needs injected SQL first (RLS guards a forgotten filter, not injection). [T2 M1]
- **RLS tests:** seed every table in `TestRLSWithoutHospitalSeesNothing`; add WITH CHECK tests for UPDATE and the account tables. [T2 M3, M4]
- **Old dev volumes** (from `init.sql`) break `migrate`; document `docker compose down -v` once, or detect and hint. [T2 M5]
- **Uppercase email CHECK** in the DB: moot while `email` is `citext UNIQUE`. [T5 M8]
- **Provisioning can add roles to existing accounts.** Limit it to users made in the same transaction, or a `provision_user()` function; a DB check that a platform account has at most one role. [T5 N1]

**NATS**
- **Every NATS responder checks the reply subject** before answering. NATS lets an `allow_responses` reply go to any subject the requester named, past the allow and deny lists, JetStream API calls with core's account included (`TestReplySubjectIsTheResponderToCheck`; services/nats/README.md). Core on `api.v1.<app>` answers only `_INBOX_bff.<app>.`, and on `authz.v1.grant.<service>` only `_INBOX_worker.<service>.`; workers follow spec §17.2. The rest of the subject must be literal tokens. (Plan 3, Tasks 6, 7, 9)
- **Split NATS accounts before production** (boundary design, decision 10), and in any case before NATS is shared with anything else. This is the only server-side defence against reply-subject abuse: the BFFs and the workers each go in their own account and reach core's subjects through service exports and imports, so a responder's reply can't reach core's JetStream API or KV. It also closes the residual Plan 3 accepted: any worker credential can publish `dead.v1.>` for any hospital (per-hospital permissions or the split fix it).
- **NATS TLS for production.** `deploy/nats/nats.conf` documents the block (`verify: true`, `handshake_first: 300ms`, the fallback still TLS-only); none of it is on the demo, which keeps NATS on one host. Required before any worker runs on another host.
- **Demo NATS (`deploy/nats/nats.conf`):** no JetStream encryption at rest yet (turning `NATS_JS_KEY` on needs the store converted or recreated), and no `bff_platform` user (it needs its own NKey from `setup-host.sh` before mhai-web's platform BFF moves to NATS). The clinical BFF is `NATS_BFF_NKEY`.
- **Intelligence workers on a deployed host.** Not on the demo in Plan 3 (small host, heavy models): questions there queue and fail at their deadline. Deploying the pipeline needs its own NKey, TLS, its database login with a password file and `sslmode=verify-full` (the worker refuses anything else outside development); see the sketch in `deploy/compose.yml`.

**Database (Plan 3)**
- **Postgres TLS.** The pipeline worker requires `sslmode=verify-full` outside development; core's own `DATABASE_URL` (and the demo's) still uses `sslmode=disable` on a private network. Require verified TLS for core too before Postgres leaves the host.
- **Role-level defaults on the logins** (deploy note). `core serve` (`db.CheckLoginRole`) and the pipeline refuse a login that gets a role or hospital at connect time from `ALTER ROLE ... SET` or `PGOPTIONS`. Deployments must not set either for `core_app`, `core_provision` or `intelligence_app`.

**Auth**
- **Idempotency panic after commit** re-runs the handler on retry (fails safe); store the tombstone first. [T4 N4]
- **Password pattern checks:** more pass-cases (passphrases containing 1234/qwerty/aaaa, non-ASCII). Optional sorted-blob breached list. [T5 N2, N3]
- **`MFATokens.Consume` retry** on a revision conflict (a double-submitted challenge is refused; fails safe). [T6 N1]
- **Audit `auth.login.failed`** when issuing the mfa_token fails. [T7 N1]
- **Concurrent rehash** cancels an outstanding mfa_token (fails safe). [T7 N2]
- **SelfService** is guarded by path prefix only; explicit HEAD/OPTIONS routes; the auth package is exempt from the direct-tx test. [T8 N1, N2, N3]
- **`TestNoOtherMux`** matches by identifier only (misses aliases, `new(http.ServeMux)`, `http.Handle`). On refresh, run the audience check after the usable check. [T8 M5, M10]
- **argon2 gate sizing.** `runtime.NumCPU()` counts host CPUs in a container; use `GOMAXPROCS` with the cgroup quota (Go 1.25) or a `CORE_ARGON_SLOTS` setting. [final m4]
- **`Cache-Control: no-store` on guard errors** (401/403/503 from authenticate or check on `/auth/*`); set it before `authz.Route`. [final m5]
- **`/readyz` cost.** Unauthenticated, a DB ping plus N KV calls per hit; cache the result for 1–2 s. [final m8]

**Repo and CI**
- **CODEOWNERS** entries without a space (`orchestrator.py@ojies` and two more) match nothing; fix them and add `/services/core/internal/{auth,authz,db,httpx}/` and `/services/nats/`. [final m6]

## Under consideration

- **Fast intelligence service (parked 27 Oct, product owner).** Serve the models on a rented cloud GPU, aiming for millisecond steps and a checked answer in about 3 s instead of 8–30 s. The approach: vLLM, compiled small models, streaming, FP8 and speculative decoding. The draft design, with the questions to answer first, is in `docs/specs/2026-10-27-intelligence-performance-design.md`. It needs the Python-over-NATS boundary in place, plus a GPU budget (about $1,400 a month for one L40S) and clinical sign-off for FP8 and streaming.

## Backend contracts the frontend uses on sample data

mhai-web has built these screens against contracts that are marked **Assumed** or proposed. Each needs building here, or confirming as built:

- §10 Analysis (Plan 6), and the hospital side of clearance letters (Plan 11a).
- §14.10 platform clearances and the named-access switch (Plan 11b), including the `today` fields and revoke refusals (`same_person`, `same_side`).
- Named access in Analysis (Plan 11c): `POST /analysis/patients/named` and `clearance/me` with `reason`.
- §14.11 record connections (platform admin) and §12 EMR alert settings, routing and evidence (hospital side) (Plan 13).
- §14.8 platform analysis across hospitals (Plan 12, designed only).

- **Notifications service** (planned, `services/notifications/`): one service for every alert and notification (EMR abnormality alerts, correction decisions, job and system updates), delivered in the app (bell, pop-ups) and by opt-in email. Needs a design: event subjects from core, delivery and read state, email provider (open), per-user settings (`/me/notification-settings`, frontend contract §9), and rate limits. Core decides recipients; the service only delivers.

- **EMR integration service** (planned, `services/emr/`): connectors per EMR type, mapping, normalisation, patient index (ADR 0009). Its own service, isolated from core because it holds outside credentials. Needs a design (the EMR design work).
- **Documents service** (planned, `services/documents/`): uploads, letters, exports, virus scanning, object storage, signed links, DIA jobs. Needs a design.
- **Model serving** (planned): GPU model hosts, from the parked performance plan.

- **Document structuring pipeline** (designed 28 Oct, product owner approved the design work): scanned documents and records (PDF or image, later handwriting) become reviewed, normalised clinical records. Upload through the documents service (virus scan, sandboxed rasterising, per-hospital object storage), the `ingest_document` job, OCR and layout, vision-language extraction into a versioned schema with per-field confidence and page location, normalisation to the ADR 0009 canonical model, validation, a review queue (patient match always by a person), `finalise_document`, then events into EMR abnormality screening. Self-hosted open-weight models only; the model is chosen by an evaluation on a synthetic set. Phase 1 is typed lab reports. Open questions in §11. See the [design](docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md) §3–§6 and §10.
- **Model promotion flow (notebooks → model store → production):** notebooks save weights to Google Drive (synthetic or approved de-identified data only); a CI workflow runs `mhai-models promote` with a read-only, one-folder Drive account; safetensors only; SHA-256, our own evaluation, model card, data statement; an immutable, signed version in the model store (object storage with object lock); production pins it in `models.lock` through a `clinical-review` PR, verifies every hash at load, and rolls back by pinning the previous approved version. `model_versions()` is recorded with every result. See the [design](docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md) §7–§8.
