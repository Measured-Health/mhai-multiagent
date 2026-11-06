# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## What's left (5 Nov 2026)

Plans 2, 3 and 4 are built and live on the demo, and so is document pipeline phase 0 part A (PR #91). What remains:

- **Document pipeline phase 0, part B (Tasks 13–25), in progress.** Model file checks, training manifest and registries, signed manifests, the object-locked model store, run sources, `mhai-models promote`, `models.lock`, sign-off and rollback, load-time verification, the intelligence service's pinned roles, the model workflows and the end-to-end tests. See the [plan](docs/plans/2026-11-03-document-pipeline-phase0.md).
- **Document pipeline Tasks 26–28: waiting on people.** The production signing backend (H1), the production model store (H2) and real Google Drive access (H3).
- **Cloud models.** The design is approved (PR #85, 4 Nov). The plan isn't written yet (about 11 tasks, migrations from 00018). Masked text only, off by default per hospital.
- **EHR/EMR integration.** The EMR integration service (`services/emr/`): a connector per EMR type, mapping to the canonical model, the patient index and abnormality flags. The design was started on 23 Oct and isn't finished.
- **Patient analysis (§10 Analysis, Plan 6).** The backend for the Analysis page that mhai-web built on sample data. It needs a privacy design first.
- **Platform admin 7c: analytics.** Waiting on decision R41a (cross-period differencing in guardrail statistics).
- **The notifications service and the documents service.** Both are planned (see "Backend contracts the frontend uses on sample data" below) and need designs.
- **Trained model weights.** These come from the AI researcher and are the one thing we don't build. Until they arrive, the demo has no AI pipeline: a question waits in the queue and fails at its deadline.
- **Demo settings.** `SECURITY_CONTACT_*` isn't set, so the audit integrity status returns 503. We need a name and an email from the product owner.
- **Dependabot backlog.** 21 open PRs (Go modules, Python groups, GitHub Actions, NATS 2.15); each needs CI and a merge.

## Decided

- **Go alongside Python.** The core service (Go, `services/core/`) owns the API, auth, tenancy, audit and EMR plumbing. The Python services (`services/intelligence/`) keep the models and are reached only over NATS. See [ADR 0010](docs/adr/0010-api-in-go-models-in-python-over-nats.md).
- **The intelligence boundary (product owner, 28 Oct).** Core calls the Python services over NATS with a per-call Ed25519 request token; Protobuf with `buf` for the messages; a 10-minute limit on clinical questions; per-worker replay check; user-level audit plus refusals; intelligence writes results under RLS with audit facts in an outbox; `POST /queries` in the plan; a separate request-signing key; 10 questions in flight per hospital; streaming left to the performance plan; no separate NATS account yet. See the [design](docs/specs/2026-10-28-intelligence-boundary-design.md) §16 and [Plan 3](docs/plans/2026-10-28-plan-3-intelligence-boundary.md).
- **Frontend ingress over NATS (product owner, 28 Oct).** The BFF talks to core over NATS (`api.v1.<audience>`), its NATS identity replaces the `X-MHAI-Service` key, and the BFF may call intelligence operations marked `bff_direct`, which intelligence serves only after core grants them (`authz.v1.grant.<service>`). See [ADR 0011](docs/adr/0011-frontend-ingress-over-nats.md) and the design §17; built in Plan 3.
- **Plan 4: feedback and corrections, the audit log, platform operations (product owner, 3 Nov).** Built in three parts (4a, 4b and 4c, all built). Corrections are approved by a second clinician, with the rule enforced in core and in the database; approved corrections only fill a per-hospital buffer, which nothing applies to the live guardrails; the audit log is served as `/platform/audit`; platform operations (health, failed jobs, guardrail statistics) are served as `/platform/health`, `/platform/jobs` and `/platform/guardrails`. See the [design](docs/specs/2026-10-31-plan-4-feedback-audit-documents-admin-design.md) §15 and [Plan 4](docs/plans/2026-11-03-plan-4-feedback-audit-admin.md).

## Document pipeline phase 0: notes for later tasks

- **Layout separation is by seed only (ruling D7).** An eval layout can look like a training one, so the eval score does not measure unseen template families. Held-out layout families (unseen templates) come with set v1.1.
- **File names (ruling D6).** Generated file names must keep digit runs to 5 or fewer: the scanner refuses 6 or more as `filename`. Task 8 names files and must follow this. The renderer itself generates no file names.

- **Volunteer handwriting is not machine-checked (H5).** The intake registers a filled form and records its consent id and role, but nothing reads or verifies what was written. A person must look at each filled sheet (or sign it off) before it joins set v1.1.
- **Consent ids are checked for format only (H5).** Intake accepts `CN-<base32>` and a role from a closed set; it does not look the id up in any consent register. The register itself is a human process outside this repo.
- **Scanner false positive on second-patient result lines.** On `two_patients` pages the second patient's block drops the unit from each result line and labels the MRN `Record:` rather than `MRN:`, because the scanner reads the drawn text as free text: a UCUM-style unit gives a false positive, and a line such as `TSH 156.96` reads as an id (letters, then five digits), so such lines are left out. Teach the scanner lab-line and UCUM unit shapes in drawn text, then restore the units, the left-out lines and the `MRN:` label (Task 6 minor).
- **Twin-prediction delta for hostile pages (ruling D11).** "Other fields unchanged" compares the prediction on the hostile page with the clean twin's truth. A stricter metric compares it with the extractor's own prediction on the clean twin, so only the change the hostile part caused is counted. Add it as a second hostile metric.
- **VLM refusals and flags (ruling D13).** The OpenAI-compatible extractor cannot refuse or flag today. The response contract becomes `anyOf[MODEL_OUTPUT, {refusal: {reason}}]` mapped to `extractors.Refused`, and phase 1's evaluation wraps the VLM in the pipeline's validation, flagging and confidence step. Until then a raw VLM cannot pass the hostile gate (GPU task / Task 19).
- **Printable pages need a margin of 22 mm.** Only `layouts.printable(...)` layouts carry registration marks; the blank form and record sheet are in `synth/forms.py`.
- **MinIO is test-only (Task 16 departure).** MinIO (server and client) is AGPL: it runs only in the `store` tests and `docker-compose.models.yml`, and is never shipped in an image or used in production (Task 27 picks the cloud store). The `minio/minio` and `minio/mc` repositories are no longer published, so the pins are the last community builds still available, `bitnamilegacy/minio` 2025.7.23 and `bitnamilegacy/minio-client` 2025.7.21 (frozen in August 2025), by digest. They get no more fixes: replace them if a maintained build appears, and never expose them beyond loopback.
- **Locking a new object needs `s3:PutObjectRetention` (Task 16).** The store refuses a PUT that sets `ObjectLockMode`/`ObjectLockRetainUntilDate` without it, so the promoter and eval-writer policies grant it, limited to compliance mode. They can therefore lengthen (never shorten) the lock on an object they may write; the plan said "no retention changes" for the promoter. A Deny on `s3:object-lock-remaining-retention-days` above `MODEL_STORE_MAX_RETENTION_DAYS` caps every extension, and `S3Store` refuses longer locks itself. The pinned MinIO doesn't evaluate that key for the lock headers of a new PUT (an Allow conditioned on it denies every locked PUT; a Deny on it never matches), so on MinIO a raw holder of promoter or eval-writer credentials can still lock a *new* object past the cap (`test_retention_capped_by_policy` pins this). AWS evaluates the key on PutObject; the cloud policies (Task 27) must carry the cap and test it on a new PUT.
- **The model store's retention cap needs the compliance lead (Task 16).** The design sets no period for model versions (§13 question 9 recommends "as long as any stored record names them, and at least the `documents` period"), and retention periods need the compliance lead's sign-off. Until then `MODEL_STORE_MAX_RETENTION_DAYS` defaults to 3650 days, in `S3Store` and in the store's policies; the compliance lead sets the real value.
- **Legal holds on MinIO (Task 16).** The pinned MinIO applies an `x-amz-object-lock-legal-hold: ON` PUT header without checking `s3:PutObjectLegalHold`, so the explicit Deny only stops the legal-hold call itself. The root user can clear a hold (unlike a compliance lock), so the harm is a nuisance. The cloud policies (Task 27) must deny it on PUT too and test it (`test_legal_hold_denied` pins the MinIO behaviour).
- **Policy wildcards cross `/`.** `models/*/*/signoff.*` also matches a run file named `signoff.*`. The promoter is denied those (fail closed) and the sign-off role is denied everything under `models/*/runs/*`.
- **One PUT is at most 5 GiB.** `S3Store.put_new` refuses larger objects (`too_large`); `files.Limits` allows 40 GiB weight files. Real weights (Task 19 onward) need a multipart upload whose completion is conditional (`If-None-Match: *` on `CompleteMultipartUpload`), checked against the pinned store before it is relied on.
- **The SDK loggers are held at ERROR.** `S3Store` raises the `boto3`, `botocore`, `s3transfer` and `urllib3` loggers to ERROR, because their debug output has endpoints, keys and signed request details. Don't lower them in a service that holds store credentials.
- **The locked test split's retention needs the compliance lead (Task 17).** `mhai-models testset-build` locks every object for `MODEL_STORE_TESTSET_RETENTION_DAYS` (required, at most the store's cap). A split should outlive every model evaluated on it; the compliance lead sets the period with the model store's.
- **The eval reader may list versions under `eval/` (Task 17).** `testsets.fetch` finds the locked `testset.json` as the one version whose SHA-256 equals `models/testsets.lock` (never "latest"), so `eval-reader` has `s3:ListBucketVersions` with `s3:prefix` `eval/*` only. It reveals no more than the reader can already read. The cloud policies (Task 27) need the same grant and its test.
- **A test split's version keeps five digits or fewer in all (Task 17).** The synthetic-data scanner refuses a joined digit run of 6 or more anywhere in `testset.json`, the `version` field included (`1.10.100` fails the build; `1.0.0` and `2.15.3` pass).
- **Clinical and platform reviewer logins are needed (H-item, Task 20).** `models/reviewers.json` is committed with empty `clinical` and `platform` lists, so `lock-check` refuses every new pin, rollback and removal until the product owner adds the reviewers' GitHub logins (lowercase) by PR. The list is read from the PR's base, so the PR that adds a reviewer can't use them. No login is filled in by an agent.
- **Pinnable roles are the roles with an evaluation suite (Task 20).** `lock.ROLES` is `promote.SUITES`' keys (`extract` today), since only those can pass a gate. A role such as `ocr`, `nli` or `cca` joins the production lock when its suite exists.
- **Eval claim (Task 17).** A build first reserves `eval/<suite>/<version>/claim.json` (conditional create), so a second build of the same version stops before it generates or uploads anything. A build that fails after the claim leaves locked files under a version that can never be built again: use the next version. Every policy denies keys under `eval/*/claim.json/`.
- **TODO(H4): what keeps the model workflows honest (rulings D35, D35b, D37).** Main's *code* runs in `lock-check` and the sign-off, but the *workflow file* is the PR's: `pull_request` runs `model-lock.yml` from the PR, and `push` runs `model-signoff.yml` from the merge commit. A PR that edits `model-lock.yml` gets a green `lock-check` whatever it pins (a required check matches by name, not by file), and one that also edits `model-signoff.yml` has its own copy run with the sign-off identity and secrets. `code_changed_with_lock` (a lock change may touch only `models.lock`, `models/**`, `docs/**` and `CHANGELOG.md`) holds only while those files are unedited. What stops the rest is these settings, which a person makes before PR-B merges (no agent changes them):
  - **CODEOWNERS** on `/.github/`, `/models/`, `/models.lock` and `/services/intelligence/intelligence/modelstore/` (also `/services/intelligence/scripts/rehearse-promotion.sh`, `/services/intelligence/pyproject.toml`), each with at least two owners, and "Require review from Code Owners" on `main` with two code-owner approvals.
  - **"Require approval of the most recent reviewable push"**, so a workflow edit pushed after approval needs a new approval.
  - **"Require branches to be up to date before merging"**, and **merge commits only** (no squash, rebase or merge-queue batching): the sign-off refuses anything else.
  - **Required status check** `lock-check`, with GitHub Actions as its source. `model-lock.yml` has no path filter, so it reports on every PR.
  - **Three environments, created first:** `model-eval`, `model-promotion` and `model-signoff` must exist before PR-B merges (a job naming a missing environment creates it unprotected), each with deployment branches limited to `main`. `model-promotion` and `model-signoff` get required reviewers from the platform team and "prevent self-review", so no store write secret or `id-token` is released to a job (a merge's own edited `model-signoff.yml` included) without a person approving it. Each job also refuses to run on any ref but `main`.
  - **Write secrets as environment secrets only:** the promoter, sign-off, eval-reader and eval-writer credentials live in their environments, never as repository secrets. `MODEL_STORE_ENDPOINT` (the rehearsal switch) is a repository variable.
  - **Residual risk:** `MODEL_STORE_READER_*` is a repository secret (lock-check needs it on every PR), so any workflow a writer edits can read it; it is read-only, until Task 27 replaces it with OIDC.
  - **Optional:** an organisation ruleset "Require workflows to pass" pinned to main's `model-lock.yml`, so the check runs from main's file (needs a ruling: `pull_request_target` stays forbidden).

## Open items for the core service

From §11 of the [design](docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md):

- **Mail provider** for invites and password resets. Sign-in is built (Plan 2) without it; invites and forgot/reset wait for it. AWS SES is the suggestion.
- **Scale assumption.** Sizing assumes up to about 50 hospitals and 5,000 staff accounts in the first year. Correct it if talks suggest more.
- **Data Protection Act check** before go-live: data processing agreements with each hospital, and where data is stored. Needs someone qualified.

## Next for the core service (after Plan 2)

- **Plan: operations baseline** — backups and tested restore, monitoring and alerts, failover — before any real patient data. Status of each area: ARCHITECTURE.md, "Operations".
- **Invite flow.** It must close first-sign-in self-enrolment: today a CLI-created account enrols its own two-step at first sign-in, so whoever has the first password owns the second factor. Invites bind enrolment to the invited person. Needs the mail provider.
- **Forgot / reset password** (contract §2.5). Needs the mail provider.
- **Turn core's HTTP API off** (`CORE_HTTP_API=off`, probes only on localhost) once mhai-web's BFF talks to core over NATS. That switch is mhai-web work (its TODO.md, "BFF over NATS"; rollout step 6 of its plan `docs/plans/2026-10-31-bff-nats-transport.md`). The same change brings mhai-web's `contract.ts` in line with Plan 3: `ClinicalResult` gains `trust_band` and `cited_chunk_ids`, and the problem codes gain 405 `method_not_allowed`, 413 `payload_too_large` (both on the NATS path) and 503 `unavailable`.
- **Citations are empty** in `GET /queries/{id}/result` until the pipeline stores citation details: today it stores only `cited_chunk_ids`. Flagged to the product owner and the intelligence lead (Plan 3 T12).
- **Patient ids in questions and tokens** (`pids`, plan departure 7): with the EMR plan. Until then retrieval returns shared guidelines only.
- **The shared replay check** (NATS KV) when a request/reply operation first writes data (boundary design, decision 3). Today each worker copy keeps its own `jti` set, which is enough while request/reply operations only read.
- **Hospital setup flow.** `setup_incomplete` hospitals can sign in so the first admin can finish setup; revisit when the flow exists (T8 M8), and decide whether `POST /queries` for a `setup_incomplete` hospital should be 403 rather than 401. Until then an operator activates a hospital with `core hospitals activate`.
- **Admin unlock** for the per-login ceiling, with the staff endpoints (only a trusted device gets past it today); alert on `auth.login.ceiling` audit rows.

### Deferred minors to follow up

From the Plan 2 reviews (task ids in brackets). None blocks merge.

**Database and audit**
- **Audit tail deletion.** The hash chain can't show rows deleted from the end. Add signed checkpoints (`audit_checkpoints`) on a timer. [T4 M6]
- **A cool-down on starting audit chain checks.** One check runs at a time, but `POST /platform/audit/verify` starts a new whole-chain walk (up to 10 minutes) as soon as the last one ends, so an auditor or platform admin can keep one running all the time. Add a cool-down (for example, return the latest run if one finished in the last few minutes). [Plan 4 Task 10]
- **Keep the outcome reply out of the idempotency store.** `POST /platform/audit/verify/runs/{id}/outcome` returns the run with its note, and the Idempotency-Key middleware stores the reply for 72 hours, so the auditor's free-text note sits in `idempotency_keys` too (core-only, not a log or audit payload). Store a reply without the note, or mark the route as not storing its body. [Plan 4 Task 10]
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
- **Demo NATS (`deploy/nats/nats.conf`):** no JetStream encryption at rest yet (turning `NATS_JS_KEY` on needs the store converted or recreated) (`NATS_BFF_NKEY` is the clinical BFF, `NATS_BFF_PLATFORM_NKEY` the platform one; the clinical name is renamed when the production config is written).
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

## Plan 4a follow-ups (built 3 Nov)

- **`run_query`'s lifetime attempts cap blocks a platform retry of a failed question.** `clinical_queries` has no `retry_count` (jobs has), so a retried question runs out of attempts. Decide in Task 12 (the platform retry).
- **`deploy/nats/nats.conf` needs the pipeline user's learning subjects** (`$JS.API.CONSUMER.INFO.JOBS.pipeline_learning`, `$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline_learning`, `$JS.ACK.JOBS.pipeline_learning.>`) when the pipeline is deployed. `services/nats/nats.conf` (dev, smoke) has them.
- **Refusal audit rows are unthrottled** (`correction.decision_refused`): a signed-in clinician can write one per request. Residual risk, accepted for 4a; the request rate limits bound it.
- **`auth.terms.accepted.version` is user text in a closed schema.** The version string the client sends is stored in the audit payload; pin it to a known list of terms versions.
- **4a: an approved correction whose `approve_correction` job fails or expires has no recovery until 4b's platform retry (Task 12).** The decision stands; on the demo, with no pipeline deployed, every approval ends `job.dead_lettered`/`deadline_exceeded`.
- **`deploy/nats/nats.conf` lacks the pipeline user's learning subjects.** Without them the worker can bind neither durable, so `run_query` stops too. Add them when the pipeline is deployed.
- **The idempotency store keeps a plain SHA-256 of the request body** (for `POST /feedback` this includes free text); key it with an HMAC.

## Deferred from Plan 4 (decided 3 Nov)

- **Document upload and the `ingest_document` job:** moved to the document pipeline plan, with the documents service, object storage and the scanner (Plan 4 design §7). Nothing is uploaded before files can be scanned and read. Plan 4's generic `jobs` table is ready for `ingest_document` and `finalise_document`.
- **`POST /triage` (QDA):** deferred until QDA passes its evaluation and has clinical sign-off. Integrated accuracy is about 30% (tracker KI-2). It will be a `run_triage` job behind the platform's triage switch (design §8).
- **`GET /kb/search`:** deferred until a screen needs it; then a core-initiated, read-only request/reply operation, shared guidelines only until patient ids exist (design §8).
- **Reset the feedback buffer** (tracker B-23, KI-5): moved to the platform settings plan, with `GET /platform/settings`. Buffer rows carry `model_versions`, `cleared_at` and `reset_id`, so the reset is one function plus the route (design §8).
- **Applying corrections to live behaviour:** a CMA-ES run produces only a *proposed* weight set, under clinical change control. The persona vector waits for the pipeline to capture model activations (the intelligence lead's work). KB enrichment waits for patient ids (design §4.8, decisions 3 and 4).
- **The rest of the audit log:** exports, named access and pseudonym reveal (`/platform/audit/exports*`, `/named-access`, `/{id}/patients`). They need Analysis pseudonyms, clearance letters and object storage. The bell and email on a broken chain wait for the notifications service.
- **mhai-web dependency:** six audit event kinds must ship in mhai-web before Plan 4b deploys, or those entries show as "can't be displayed": `answer_agreed`, `review_withdrawn`, `review_refused`, `learning_recorded`, `audit_check_started` and `audit_check_finished` (design §5.3 marks seven new, but `audit_undisplayable_reported` is already in mhai-web, so six are pending; see `pending_in_web` in `testdata/web-audit-vocabulary.json`, to be dropped once they ship). mhai-web must also accept a health reading without the `documents` and `clean_up` parts (decision 7).
- **mhai-web: approved and rejected on `review_submitted`.** Core sends no `facts.state` on a review entry, because mhai-web's `state` takes only `running`, `intact`, `broken` and `error`. Until mhai-web adds `approved` and `rejected` (and core sends them), the audit list does not show how a correction was decided (Plan 4 ruling R22).
- **mhai-web: kinds for the 17 Plan 2 and 3 actions that have none** (for example `intelligence.granted`, `query.completed`, `auth.mfa.verified`), and for the per-job `job.retried` and `job.cancelled` rows. Core sends each as a withheld row, the bare undisplayable entry, so nothing leaks; the audit list is noisy until mhai-web adds the kinds (Plan 4 rulings R21 and R24).
- **mhai-web: `searched_kind` values `person`, `hospital` and `run`.** Core records a person-picker search, a read of the audit hospitals list and a read of the chain checks (the runs list, a run, the integrity status) as `audit.searched` with `searched_kind` `person`, `hospital` or `run` (Plan 4 Tasks 9 and 10). mhai-web's `facts.searched_kind` takes only `event`, `request`, `export`, `clearance` and `pseudonym`, so the catalogue gates those rows out (`searchedInWeb`) and they show as "can't be displayed". When mhai-web takes the values, widen `searchedInWeb` and the reader's `audit.searched` gate in `services/core/internal/audit/`, and refresh `testdata/web-audit-vocabulary.json`.
- **mhai-web: an `analyst` actor role.** mhai-web's `ActorRole` has no `analyst`, so an audit entry whose actor is an analyst and nothing else is sent as the bare entry ("can't be displayed"), and the person picker (`GET /platform/audit/people`) leaves analyst-only accounts out (Plan 4 Task 9 ruling). When mhai-web adds the role, add it to `actorRoles` in `services/core/internal/audit/reader.go` and to `AuditPerson`/`AuditEvent` in `openapi.yaml`.

## Plan 4b follow-ups (final review)

- **Audit list filters still see attributes of rows sent bare.** A filter (`person`, `hospital`, `type`, `outcome`, `flagged`, `from`, `to`) matches on a row's real attributes even when the row is sent as the bare entry, so a filtered result can tell a platform admin or auditor something about a withheld row. Metadata only, no payload or free text (final review M2).
- **A user name that can't be shown makes the integrity page 500.** A run's `by` or note author whose name fails the display rules fails the whole read. Validate names at provisioning so none can be stored (M4).
- **The contact name is counted in runes in core and in UTF-16 units in mhai-web, and U+FFFD is not refused.** A name of 120 runes with characters outside the basic plane can pass core and fail mhai-web. Count UTF-16 units in `SecurityContactOK` and refuse U+FFFD, in `deploy/setup-host.sh` too (M7).
- **The `audit_chain_broken` gauge is stale on the copy walking the scheduled check.** The copy that is walking the scheduled check does not refresh its gauge until the walk ends, so it can show an old value for up to the 10-minute budget (M8).

## Plan 4c follow-ups (Task 11 review)

- **The non-superuser migrate path for 00015, 00016 and 00017.** On a CREATEROLE (not superuser) migration login, 00015's `ALTER FUNCTION ... OWNER TO mhai_health_reader` needs the new owner to hold CREATE on schema `public`, which it doesn't (it has USAGE only); 00016 and 00017 hand functions and tables to their reader roles the same way. On PG16 a CREATEROLE runner also gets ADMIN membership of roles it creates automatically, which the hand-over and the temp-grant path must account for. Grant for the hand-over and revoke after, or run these steps as a superuser. The demo migrates as a superuser, so it isn't affected.
- **Bound the health failures scan with retention.** `mhai_health_failures()` counts every failed question and job, however old (partial indexes on failed rows keep it to those rows). Once the retention plan purges old jobs and questions, the scan is bounded by it; until then it grows with the failed rows kept.

## Plan 4c follow-ups (Task 12 review)

- **mhai-web: map `validation_failed` on the failed jobs writes.** Core answers a bad id list, reason or note on `POST /platform/jobs/retry` and `/cancel` with 422 `validation_failed`, the code every core route uses; contract §14.8.3 says 422 `invalid`. mhai-web must map `validation_failed` there (recorded in spec §16, 4c).
- **mhai-web: a facts key for the cancel reason.** The `jobs_cancelled` audit entry sends `facts.job_count` only: the batch row stores the reason (`no_longer_needed`, `cant_succeed`, `other`), but mhai-web has no facts key for it, so Activity can't show the reason in words as §14.8.3 asks. When mhai-web adds one, map `reason` in the `job.cancelled` catalogue row (`services/core/internal/audit/catalogue.go`) and refresh `testdata/web-audit-vocabulary.json`.
- **Deploy order for 00016.** The `run_query` worker's claim reads `clinical_queries.retry_count` (00016): migrate before deploying the worker (the usual order).

## Plan 4c follow-ups (Task 13 review)

- **Guardrail statistics: differencing across periods and days (R41a, needs a privacy ruling).** `GET /platform/guardrails` guards each period on its own. 30d contains 7d and 90d contains 30d, so a shown 30d count minus the shown 7d count of the same hospital (days 8 to 30) can be 1 to 4, and so can the difference between one day's figures and the next (the day that entered minus the day that left). Rulings R42 and R43 remove differencing within a day: the window is whole UTC days, and each period's day is one stored snapshot (`guardrail_snapshots`, computed after a 15-minute grace past midnight and served by every core copy all day), so late commits and mid-day deactivations change nothing until the next day. Options for the rest: guard the three periods jointly (each cell of a shorter period nested in the longer one, a three-way problem the bridge check doesn't cover); serve non-overlapping windows (the last 7 days, days 8 to 30, days 31 to 90) and let the app add them up, with the sums guarded too; round shown counts (to 5s) or add bounded noise; or fix the windows to calendar weeks or months so a window changes only when it rolls over. Plan 7c reuses this query, so decide before then.
- **Guardrail statistics: bound-based narrowing is residual risk.** The rules and the property test prove that no hidden cell can be solved for exactly (linear elimination over every row, column and total). They don't check bounds: someone using non-negativity, and knowing that a first-hidden cell is 1 to 4, can sometimes narrow a hidden cell to a range (a full check needs linear programming over the bounds).
- **Guardrail statistics: snapshot retention.** `guardrail_snapshots` keeps three rows a day (one per period) and nothing deletes them (`mhai_platform` holds SELECT and INSERT only). Add a sweep with the retention plan; the rows hold guarded figures only. Concurrent first requests on several core copies may each compute the day once (only one row is kept and served).

## Plan 4c follow-ups (final)

- **mhai-web: `HealthOut` must accept `clean_up` and `documents` missing** (decision 7); core does not send them until those services exist.
- **Health `versions` and the evidence part.** `HealthResponse` carries no NLI flag or model versions, so `versions` is `[]` and `evidence` is ok only when `models_ready` (R33). Extend the intelligence boundary's health reply.
- **Smoke: no check that a retried dead letter completes.** The forced dead letter has no stored patient data, so the pipeline may fail it again; the smoke test checks only that the retry is accepted.

- **Tag dead letters with their retry generation.** A `deadline_exceeded` letter for a retried row is ignored because the row's own deadline hasn't passed (final review fix 1), but a stale letter with another code (`unavailable`, `internal`) can still fail a freshly retried row. Put the row's `retry_count` in the job message and the letter, and ignore a letter whose generation isn't the row's.
- **Health `overall` timing includes the time before a retry.** A retried question's timings run from its first creation (M2).
- **One bad row aborts the whole retry batch.** Use a savepoint per row, so one failing row is skipped, not the call (M4).
- **The two `job.cancelled` shapes accept each other's reasons.** The platform's cancel batch (`job_count`, `reason`) and the system's per-job rows share an action name and validation; separate them (M6).
- **Test-only mutable globals in production code:** `guardClock`, `skipBridge` (smallgroup) and `jobsTableFault` can be set by any code in the package; move them behind build-tagged files or constructor options (M7).

## Test follow-ups

- **Interop test of the health probe against the real Python worker.** The sampler's `pipeline.health` token (svc:core, no roles, the fixed probe hospital id) is tested only against a Go fake. Add a test, in the smoke run or the boundary tests, that the Python `pipeline.health` handler accepts it and answers `models_ready` and `database_ready`.

- **`TestTwoCoreCopiesShareGrants` (`internal/grant`) is flaky.** Seen once in CI during Plan 4b; it passed on a re-run. Find the race (two core copies sharing the grants KV) before it hides a real fault.

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
