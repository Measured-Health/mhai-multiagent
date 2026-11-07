# EMR integration service: reading hospital records and flagging abnormal results

- **Status:** Draft for the product owner (2026-11-07). The questions in §21 need answers before the plan is written. Nothing here is live until the sign-offs in §17.6 are in place: until then everything runs on synthetic data only.
- **Date:** 2026-11-07
- **Decision recorded in:** [ADR 0009](../adr/0009-integrate-many-emr-systems.md) (Proposed, 17 Oct). This design is how ADR 0009 is built. PR 1 of §19 moves ADR 0009 to Accepted and adds ADR 0014 for the two decisions this design adds to it: a separate `services/emr/` with its own credentials and egress (§3), and the reviewed document route for sources that hold only scanned records (§5.3).
- **Builds on (and must stay consistent with):**
  - the intelligence boundary design, `docs/specs/2026-10-28-intelligence-boundary-design.md`, **as built** (§18): the request token (§7.2, `sub = svc:core` for "EMR events"), the subjects (§5.1), the closed audit payloads, the dead-letter path, NATS users per service (§7.6);
  - the document pipeline design, `docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`: its reviewed records land in the canonical tables defined here, and its `records.v1.<hospital>.created` event is the one entry point to the rules (§9);
  - the Plan 4 design, `docs/specs/2026-10-31-plan-4-feedback-audit-documents-admin-design.md`: the typed audit registry, the platform routes and their rules, the generic `jobs` table and the Failed jobs view;
  - the cloud model providers design, `docs/specs/2026-11-03-cloud-model-providers-design.md`: the egress proxy pattern (§6 there) and key files (§7 there);
  - ADR 0004 (one patient's data never reaches another's), ADR 0005 (fail closed), ADR 0010 and 0011 (core owns the API and the rules; NATS only), ADR 0012 (additive changes only in `v1`);
  - mhai-web's contract for the screens it has built on sample data: `docs/specs/2026-10-23-emr-screens-design.md` and `docs/plans/2026-10-23-plan-13-emr-screens.md` (Plan 13), `apps/platform/lib/api/emr-contract.ts`, `apps/clinical/lib/api/emr-contract.ts` and `alerts-contract.ts`, and the frontend requirements §9, §12 and (to be filed) §14.11.
- **Scope:** labs, vital signs and medications, read-only, from hospital record systems, to alerts for clinicians. Conditions, encounters beyond location, analysis (Plan 6) and "Ask MHAI about this patient" use the same model later but are not built here.

## 1. Why

The product owner's requirements (settled): read labs, vitals and medications from each hospital's record system, Havana's first and then FHIR-based systems; find abnormal values and patterns; flag them to clinicians with the evidence, never to patients; the platform admin app configures connections; hospital admins choose units, what to flag and who hears; clinicians get the alerts page, bell, pop-ups and mute that mhai-web has already built.

mhai-web has built every screen on sample data (Plan 13, plus the alerts page, bell and pop-ups). Core serves none of it yet: `services/core/openapi.yaml` (1.10.0) has no `/alerts`, `/hospital/alert-settings`, `/me/notification-settings` or `/platform/emr/*` route. This design says what serves them, and what sits behind them.

### 1.1 What Havana's system actually holds (read 7 Nov)

We read the Havana record system's code (read-only, no configuration or data opened). **It holds no structured clinical data.** It is a search, preview and download service over an archive of scanned paper records: PDF files grouped by box and by patient folder, indexed hourly from a file store, served over a REST API (`/api/v1/files/...`) with user sign-in by JWT and one deployment-wide service key. There is no patient id or MRN field (a patient is a folder name), no encounters, wards or units, and no lab results, vital signs or medications as data. Its own product plan lists structured patients, encounters and live lab, imaging and prescription streams as data the hospital has **not yet provided**.

So "Havana first" can mean two things, and this design supports both (§5.3, §21 question 1):

1. **Havana's scanned records, read through the document pipeline.** A connector lists new files and hands them to the documents service. Values reach the canonical model only after a person has reviewed the extraction and matched the patient (document pipeline design §5). Useful, but slow (hours to days), and it depends on document pipeline phase 1 (typed lab reports).
2. **A structured export from Havana's lab, pharmacy or ward systems**, when the hospital provides one. Read through the scheduled file export connector (§5.4) or FHIR R4 if their systems offer it. This is the only route that gives timely alerts. It needs the hospital (H1 in §20).

The FHIR R4 connector is built first as the reference connector, against a synthetic FHIR server, because the whole pipeline can be proven on it end to end while H1 is pending.

## 2. Goals, non-goals, anti-goals

**Goals**
- A separate service, `services/emr/`, that holds every outside credential and the only route out to hospital systems; core holds neither (§3, §6).
- One connector interface; connectors for FHIR R4 (first), Havana's archive (to the document pipeline) and scheduled file export (structured export from Havana or any hospital).
- A canonical model based on FHIR R4, with provenance on every record (§8); normalisation to LOINC and UCUM for labs and vitals, ATC for medications, in one place (§9).
- A patient index per hospital, never across hospitals, whose ids are the MHAI patient ids that `pids` will carry (§10).
- A deterministic, versioned rules engine under clinical change control, that raises findings with evidence and suggested causes (§11).
- Alerts served by core, routed by the hospital's settings, with fatigue controls, read state and audit, matching mhai-web's contract (§12, §16).
- Fail closed and never silently: a stale feed, an unmapped code or a failed rule is visible, not a quiet "nothing abnormal" (§15).

**Non-goals**
- Writing back to any record system (ADR 0009 rule 9).
- Conditions, notes, imaging, procedures; the Analysis page (Plan 6); "Ask MHAI" from a record (the `pids` work is prepared in §10.4, not wired).
- Push delivery: FHIR Subscriptions, HL7 v2 over MLLP, webhooks (§7.3: later, each with its own inbound design).
- Email or push notifications; they wait for the notifications service. Alerts are in the app (bell, pop-ups, page) in this design.
- Machine-learned scoring (§11.6).
- Mapping and rules editing screens. Mappings and rules change by reviewed pull request in this design (§9.4, §11.5).

**Anti-goals: failure even if it works**
- An alert, message or draft that reaches a patient.
- Any outside credential in core, the database, NATS, a log, a trace, an error, the browser or the repository.
- One hospital's records, patients or alerts visible to another; a patient index link across hospitals.
- An alert raised from a value nobody can trace to its source record, rule version and mapping version.
- A value from an unreviewed document extraction raising an alert.
- A clinician seeing "no alerts" when the feed is down, the rules have stopped, or the code is unmapped, with nothing saying so.
- A hospital admin changing a clinical threshold. Hospitals choose which levels to receive, never what counts as abnormal.
- A vendor or product name, vendor code or vendor URL format reaching the API (ADR 0009 rule 10).

## 3. Architecture and service boundary

```
platform app ─▶ BFF ─▶ NATS api.v1.platform ─▶ core ──(config rows, no secrets)──▶ emr_connections
                                               │
                                               └─ emr.v1.ctl.<op> (request token, aud emr) ─▶ emr service
                                                   test, put_credentials, sync_now, health
emr service (services/emr/, Go)
  scheduler ─▶ connector.Fetch ─▶ egress-emr proxy ─▶ hospital system (allow-listed host per connection)
             └▶ raw records (Postgres, schema emr, RLS) ─▶ emr.v1.<hid>.fetched (ids only)
  normaliser ─▶ canonical tables + patient index ─▶ records.v1.<hid>.created (ids only)
  rules      ◀── records.v1.<hid>.created (also from the document pipeline's finalise_document)
             └▶ emr.findings (RLS) ─▶ emr.v1.<hid>.finding (ids only)
core ◀── emr.v1.<hid>.finding (durable core_emr) ─▶ routing (alert settings) ─▶ alerts, deliveries, audit
clinical app ─▶ BFF ─▶ api.v1.clinical ─▶ core: /alerts, /alerts/unread-count, acknowledge, dismiss
```

### 3.1 Why a separate service

- **Credentials and egress.** The emr service is the only process that holds hospital credentials and the only one with a route to hospital systems. Core keeps no route out and no outside secret. A compromise of core does not hand over every hospital's record-system keys, and a compromise of a connector (parsing hostile input from outside) does not reach sessions, accounts, the audit chain or the request-signing key.
- **Blast radius of parsing.** Connectors parse data we don't control. That code runs in a process with a narrow database role (§13.2) and no NATS rights beyond its own subjects.
- **Language.** Go, like core (ADR 0010: "the EMR connectors and event pipeline" are Go work). It is its own binary and deployment, `services/emr/`, with its own `go.mod`. The pieces it shares with core (request-token verification, `WithHospital`, log redaction) move to a small shared module, `services/gokit/`, in PR 2 (§21 question 9).
- **Who decides what.** The emr service decides **what is abnormal** (rules on the canonical model). Core decides **who sees it** (routing, permissions, read state, audit). Security rules stay written once, in core (ADR 0010); the emr service never decides a recipient and never serves the frontend.

### 3.2 NATS subjects and users

| Subject | Kind | Publisher | Subscriber | Carries |
|---|---|---|---|---|
| `emr.v1.ctl.<op>` (`test`, `put_credentials`, `sync_now`, `health`) | request/reply | core (`gateway`) only | emr (queue group `emr`) | a request token (`aud: emr`), ids; `put_credentials` a sealed box (§6.2) |
| `emr.v1.<hid>.fetched` | JetStream `EMR` | emr | emr (`emr_normalise`) | `{raw_id}` |
| `records.v1.<hid>.created` | JetStream `EMR` | emr; core for `finalise_document` (document pipeline §3) | emr (`emr_rules`) | `{record_ids[], source}` (at most 500) |
| `emr.v1.<hid>.finding` | JetStream `EMR` | emr | core (`core_emr`) | `{finding_id}` |
| `dead.v1.<hid>.emr_<stage>` | JetStream `DEAD` | emr | core (`core_dead`, existing) | job id, stage, error code |
| `_INBOX_gateway.>` | replies | emr through `allow_responses` only | core | |

- **Ids only in messages.** No name, MRN, value or code is ever in a NATS message (ADR 0009 consequences, SKILLS §7). Every payload is protobuf (`proto/mhai/emr/v1/`), `buf breaking` in CI.
- **Stream `EMR`:** subjects `emr.v1.*.fetched`, `emr.v1.*.finding`, `records.v1.>`; file storage, `max_age` 7 days, `MaxMsgsPerSubject` 50,000, `DiscardNewPerSubject` (a hospital can't fill the stream for others; a refused publish leaves the row unpublished and the sweeper retries, §15), `Nats-Msg-Id` = the row id so duplicates drop. JetStream encryption at rest on (`NATS_JS_KEY`) before real data, as for `JOBS` (TODO "Demo NATS").
- **NATS user `emr`** (NKey in production): publishes `emr.v1.*.fetched`, `emr.v1.*.finding`, `records.v1.*.created`, `dead.v1.*.emr_*`, and only its own consumers' `$JS.API.CONSUMER.{INFO,MSG.NEXT}.EMR.emr_*` and `$JS.ACK.EMR.emr_*.>`; subscribes `emr.v1.ctl.> emr` (queue group) and `_INBOX_worker.emr.>`; `allow_responses: {max: 1, expires: "1m"}`. No KV, no `jobs.v1`, no `inference.v1`, no `api.v1`.
- **`gateway` (core)** gains publish `emr.v1.ctl.>`, `records.v1.*.created`, and consumer rights for `core_emr` on `EMR`.
- **Reply subject check:** the emr responder answers only on `_INBOX_gateway.` and the token's `rpl` (TODO "Every NATS responder checks the reply subject").
- The emr service verifies every control request exactly as boundary §7.3 (size, signature, claims, binding of subject and body hash, reply subject, replay, schema, scope, permission), using the same Go verifier and the same `testdata/request-tokens` vectors. `sub` is the platform admin for `test` and `put_credentials`, `svc:core` for `health` and `sync_now`. Operations table entries: `emr.test` and `emr.put_credentials` allow `platform_admin`; `emr.health` and `emr.sync_now` allow `svc:core`.

### 3.3 The database

The same Postgres cluster, a new schema `emr`, owned by `mhai_migrate`. New roles, following 00003:

- **`mhai_emr`** (NOLOGIN), reached by the login `emr_app` through the wrapper `mhai_emr_app` (NOINHERIT, `WITH INHERIT FALSE`), exactly as `intelligence_app` (boundary §18, departure 2). Inside `WithHospital` it has `SELECT, INSERT, UPDATE` on the `emr` tables of the current hospital (RLS), `SELECT (id, hospital_id, type, base_url, settings, enabled, version, poll_seconds)` on `emr_connections`, `SELECT (id, status)` on `hospitals`, and nothing on accounts, sessions, `audit_log`, `clinical_*` or `jobs`.
- **`mhai_emr_sched`** (NOLOGIN): the scheduler's cross-hospital read of enabled connections' ids and hospital ids only, for choosing what to poll, as `mhai_jobs` does for the sweeper.
- **Core's `mhai_gateway`** reads, under RLS, the columns an alert shows: `emr.patients (id, display_name, mrn, birth_date, sex)`, the patient's current location, `emr.findings`, and the evidence rows a finding names. Column grants, never `SELECT *`.
- **`mhai_platform`** writes `emr_connections` (no secret columns exist) and reads health through a `SECURITY DEFINER` function that returns counts and codes only, never a record.
- Every table `ENABLE` and `FORCE` row-level security, with a row in `TestRLSWithoutHospitalSeesNothing`.

## 4. The connector interface

```go
type Connector interface {
    Type() TypeInfo                         // id, label, summary, declared fields, version
    Validate(cfg Config) error              // shape only, no network
    Test(ctx, cfg Config, creds Creds) TestResult              // read-only probe; closed code
    Fetch(ctx, cfg Config, creds Creds, cur Cursor, sink RawSink) (Cursor, error)
    RecordLink(cfg Config, sourcePatientID string) (string, bool) // the "open record" link
}
```

- A connector does three things only (ADR 0009 rule 1): fetch, map its source format to **raw records** (§8.1), and build the record link. It does not normalise codes or units, match patients, or run rules. That is all downstream and the same for every connector.
- `TypeInfo.Fields` is what `GET /platform/emr/connector-types` returns (Plan 13 D4): `key` `[a-z_]+`, `label`, `kind` `text|secret`, `required`, `max`. The base address and source label are common to every type. Type ids name a standard or a method, never a product.
- `Creds` is an opaque struct with a redacting `String()`/`Format()`/`MarshalJSON()`; connectors never log it, and a static test fails if a connector package imports `log`, `slog` or `fmt.Print*` with a `Creds` value in scope (AST check, as the cloud design's import test).
- `Test` and `Fetch` take a context with a deadline (test 20 s; a fetch page 60 s) and use the emr service's HTTP client only (§6.4): TLS verified, no redirects followed across hosts, response size caps.
- `Fetch` writes through `RawSink`, which enforces the allow-list of fields (§14.1) and size limits before anything is stored.

### 4.1 The connector types in this design

| id | label (platform app only) | Fields | Status |
|---|---|---|---|
| `fhir_r4` | FHIR R4 server | `client_id` text; `token_url` text; `private_key` secret (SMART Backend Services, `private_key_jwt`, RS384 or ES384); optional `scope` text | **Build first** (§5.1) |
| `document_archive` | Scanned record archive | `api_key` secret | **Havana, phase H-a** (§5.3) |
| `file_export` | Scheduled file export | `folder` text; `format` text (`mhai_csv_v1`); `access_key` secret | **Havana, phase H-b** (§5.4) |
| `hl7v2_feed` | Results message feed | `sender_id` text; `shared_key` secret | **Not built** (needs an inbound listener; §7.3). Listed by mhai-web's sample data only; the API does not offer it until built |

## 5. Connectors

### 5.1 FHIR R4 (reference connector)

- **Auth:** SMART Backend Services: a signed client assertion (`private_key_jwt`, 5-minute `exp`, unique `jti`) to `token_url`, scope `system/Patient.rs system/Observation.rs system/MedicationRequest.rs system/MedicationStatement.rs system/Encounter.rs system/Location.rs` (or the R4 `.read` forms). The access token is held in memory only, refreshed 60 s before expiry, never stored. Other auth schemes (a static bearer token, mutual TLS) are new connector versions, added when a hospital needs one.
- **Reads (polling, §7):** `Observation?category=laboratory,vital-signs&_lastUpdated=gt<cursor>&_count=200&_sort=_lastUpdated`, `MedicationRequest` and `MedicationStatement` the same way, then `Patient`, `Encounter` and `Location` by reference for what those pages name (batched `_id=` reads, at most 100 ids). Pages are followed through the bundle's `next` link only when its host and base path equal the configured base address.
- **Cursor:** the greatest `meta.lastUpdated` seen, minus a 2-minute overlap for clock skew and late commits; duplicates drop by `(connection, resource type, id, versionId)`.
- **Test:** token request, `GET metadata` (checks `fhirVersion` 4.0.x and that the resources above are supported), and `GET Observation?_summary=count` (a count, no record). Closed outcome code (§15.2).
- **Record link:** a template the platform admin sets (`settings.record_link`, e.g. `https://<host>/patients/{id}`), host equal to the base address host, `https` only, `{id}` URL-path-escaped. No template, no link (`emr_link: null`).

### 5.2 FHIR versions and profiles

R4 (4.0.1) only. R5 or R4B servers are refused at test with `unreadable_data`. US Core or IPS profiles are not required; codings are read from every `coding` entry and the best system chosen in normalisation (§9).

### 5.3 Havana's scanned archive (`document_archive`): to the document pipeline

- **What it does:** lists files added since the cursor (the archive's "since" listing, by `first_seen`), downloads each new PDF (within the archive's rate limits: at most 10 downloads a minute), and hands it to the documents service as an upload by `svc:emr` for the hospital. That is all. It produces **no canonical record itself**.
- **Then:** the document pipeline's `ingest_document` extracts, a person reviews every document and matches the patient (document pipeline §5: "patient match always by a person"), `finalise_document` writes canonical rows with provenance `source_kind = document` and the `records.v1.<hid>.created` event reaches the rules (§11) like any other record.
- **Patient identity:** the archive has no patient id. The folder name is free text and is **never** used to match a patient automatically. It is passed to the reviewer as a hint only.
- **Record link:** none (the archive has no patient page). Alerts from documents carry `emr_link: null` and evidence naming the document date.
- **Freshness:** archive documents are mostly historical. A value whose `effective` time is older than the freshness window (§11.4) is stored but raises no alert.
- **Changes the archive needs (our team's work, not the hospital's):** a per-integration API key that is read-only and scoped to listing and downloading, separate from the deployment-wide service key, and recorded in its audit log as its own actor; a listing of removed files (today deletions leave no trace). Tracked as H2.
- **Depends on:** the documents service, document pipeline phase 1, and its clinical sign-off. Until then, this connector type is not offered.

### 5.4 Scheduled file export (`file_export`, format `mhai_csv_v1`)

For a hospital whose lab, pharmacy or ward system can export files but offers no API (the likely Havana route for timely alerts, H1).

- **Transport:** SFTP pull from a folder the hospital exposes (`folder`, host from the base address, key auth with `access_key`; host key pinned at test, stored as a setting). Files are moved to `processed/` after a successful load, never deleted by us.
- **Format `mhai_csv_v1`:** one file per kind (`patients`, `encounters`, `results`, `vitals`, `medications`), UTF-8, RFC 4180, a header row, fixed columns (for example `results`: `source_result_id, source_patient_id, collected_at, resulted_at, status, local_code, local_name, value, unit, ref_low, ref_high, ref_text, flag`). ISO 8601 times with offset; a time without an offset is refused, not guessed (§9.3). We publish the format and a synthetic sample file; the hospital's IT maps their export to it.
- **Each row is a raw record** with its `source_result_id` as source id. A row that changes keeps its id and gets a new version.
- **Limits:** a file at most 50 MB, a row at most 8 KB, at most 200,000 rows a file; anything larger is refused whole with `unreadable_data`.

## 6. Credentials: storage, entry and rotation, kept out of core

### 6.1 Where they live

- **A managed secret store** (H3: AWS Secrets Manager or GCP Secret Manager, in the region that hosts the data), path `mhai/<env>/emr/<hospital_id>/<connection_id>/<field>`. The emr service's cloud identity may read, write and delete under `mhai/<env>/emr/` only; core's identity has no access to that path. Encrypted by the store's managed key.
- **On the demo** (one host, synthetic data): a file store, `EMR_SECRETS_DIR`, one file per secret, mode 0400, owned by the emr service's user, on a volume only the emr container mounts. The emr service refuses the file store when `APP_ENV` is `production`.
- **The database holds only metadata:** `emr.credentials (connection_id, field, set_at, secret_version, set_by)`. Never the value, a hash of it, or its length.
- **In memory:** loaded per fetch, held in `Creds`, dropped when the fetch ends; OAuth access tokens cached in memory only.

### 6.2 How a credential gets there without core keeping it

- The platform admin types the secret in the platform app; it goes browser → platform BFF → `api.v1.platform` → core (TLS on NATS in production). That much is unavoidable while core is the only API (ADR 0011).
- **Core seals it at once.** The handler, before anything else touches the body, seals each secret field to the emr service's public key (X25519 sealed box; the emr service's key pair is a file on the emr host; core has the public key only, from `CORE_EMR_SEAL_PUBLIC_KEY_PATH`). The plaintext stays in the handler's memory for that request, is zeroed after sealing (best effort in Go), and never reaches a log, an error, a span, the idempotency store or the audit payload.
- Core sends the sealed boxes on `emr.v1.ctl.put_credentials`; the emr service opens them, writes the secret store, records the metadata row and answers `{field, set_at}` only.
- **The idempotency store** must not keep a plain hash of these bodies: the routes that carry credentials store an HMAC-SHA256 of the body under a per-deployment key (TODO "The idempotency store keeps a plain SHA-256", done for these routes first) and the reply, which never contains a secret.
- **Request logging:** the body of the three routes that carry credentials is never logged or traced, in core's HTTP and NATS adapters (`httpx` redaction list, with a test).
- **Later (open question 10):** the platform BFF seals the fields before they leave the Node server, so core never sees plaintext. An additive `credentials_sealed` field; not in this build.

### 6.3 Rotation

- **Replacing a credential** is `PATCH /platform/emr/connections/{id}` with only the fields being replaced (step-up). The emr service writes a new secret version, keeps the previous version for 24 hours (so a mistake can be rolled back by an operator), then deletes it. The edit clears `last_test` (Plan 13 ruling); a connection that is on stays on and the next sync uses the new value; a sign-in failure shows as `sign_in_refused`.
- **Schedule:** every 12 months or as the hospital's policy requires, and at once on suspicion or when someone with access leaves. The connection page shows `set_at`; a runbook in `deploy/README.md` covers rotation with the hospital's IT. FHIR private keys are generated by us (the public key goes to the hospital, the private key never leaves the secret store), so rotation is: generate, give the hospital the new public key (JWKS), switch, revoke the old.
- **Switching a connection off** keeps its credentials (so it can be switched on again); **removing a hospital** (the two-person removal flow, contract §14.3) deletes them from the store, audited.
- **The emr service's sealing key pair** rotates yearly: a second public key is published to core, core seals to the newest, the emr service opens with either for 24 hours.

### 6.4 Egress

- The emr service runs on the `backend` network (no route out) plus a dedicated **`egress-emr` proxy** on its own `egress-emr` network, separate from the cloud models' proxy. The proxy allows `CONNECT` to port 443 (and 22 for `file_export`) only for hosts on its allow-list, which is generated from `emr_connections` that are enabled or being tested, and refreshed within 30 s of a change.
- **SSRF guard in the emr service, before the proxy:** a base address must be `https` (or `sftp` for `file_export`), at most 500 characters, a DNS name (no IP literal outside development), with no userinfo, query or fragment. At connect time every resolved address is checked and refused if private, loopback, link-local, CGNAT, multicast or a cloud metadata address, unless the connection has `private_network: true`, which only a platform admin can set and which needs the H7 network path (a site VPN).
- TLS with system CAs, verification on, no custom CA; a hospital with a private CA is a per-connection pinned CA bundle setting (§21 question 8).

## 7. Polling or subscriptions

### 7.1 Polling (this design)

- Every enabled connection is polled on its own schedule: `poll_seconds` (FHIR 120 s default, minimum 60 s; archive 3,600 s; file export 300 s), with ±10% jitter so hospitals don't line up.
- **One fetch per connection at a time:** a Postgres advisory lock per connection id; a second emr copy skips it.
- **Bounded:** at most 20 pages or 4,000 records per run; the rest continue next run. Backoff on `429` (`Retry-After`, capped at 15 minutes) and `5xx` (doubling up to 15 minutes); the connection's health shows `too_many_requests` or `unreachable`.
- **First sync (backfill):** reads back 30 days (setting, at most 365). Backfilled records are stored and evaluated, but **raise no alerts**: they fill the history that delta rules need. Without this, connecting a hospital would raise every abnormal result of the past month at once. The connection page shows "Catching up" via `last_sync.records`; alerts start when the cursor reaches "now minus one poll".

### 7.2 Why polling first

It needs no inbound endpoint (no public listener on our side, nothing for a hospital to call), works with every FHIR server and file export, and the latency (2 minutes plus processing) is inside what a lab result alert needs. Its costs are load on the source (bounded above) and a minute or two of delay.

### 7.3 Subscriptions later

FHIR R4 Subscriptions (the R4 backport, rest-hook) and HL7 v2 over MLLP need an inbound listener, its own authentication (mutual TLS or a signed notification), DoS limits and a separate design. When built, a notification only **triggers a fetch** ("something changed"); the data is still read by the same `Fetch`, so a forged notification can cost a poll but can't inject a record.

## 8. The canonical model, with provenance

### 8.1 Raw records

`emr.raw_records`: `id` (uuid), `hospital_id`, `connection_id`, `resource_kind` (`patient`, `encounter`, `location`, `observation`, `medication`), `source_id` (≤ 256), `source_version` (≤ 64), `source_updated_at`, `received_at`, `payload` (jsonb, the **allow-listed fields only**, §14.1), `payload_sha256`, `connector_type`, `connector_version`, `status` (`pending`, `normalised`, `rejected`), `reject_code`. Unique `(connection_id, resource_kind, source_id, source_version)`: a replay or duplicate is a no-op (ADR 0009 rule 7). `payload` is cleared after 7 days (§14.2); the hash stays.

### 8.2 Canonical tables (FHIR R4 shapes, MHAI's own tables)

| Table | FHIR basis | Key columns |
|---|---|---|
| `emr.patients` | Patient | `id` (the MHAI patient id, §10), `hospital_id`, `display_name`, `mrn` (the hospital's own), `birth_date`, `sex` (`female`, `male`, `other`, `unknown`), `status` (`active`, `merged_into`, `excluded`) |
| `emr.patient_links` | Patient.identifier | `patient_id`, `connection_id`, `source_patient_id`; unique `(connection_id, source_patient_id)` |
| `emr.encounters` | Encounter | `patient_id`, `class` (`inpatient`, `outpatient`, `emergency`), `status`, `period_start`, `period_end`, `unit_id`, `bed_label` |
| `emr.units` | Location | `id`, `hospital_id`, `connection_id`, `source_location_id`, `label` (≤ 60), `kind` (`ward`, `clinic`, `emergency`, `other`) |
| `emr.observations` | Observation | `patient_id`, `encounter_id`, `category` (`laboratory`, `vital-signs`), `loinc` (null when unmapped), `local_system`, `local_code`, `local_display`, `value_num`, `value_text`, `unit_ucum`, `unit_source`, `value_si` (in the analyte's canonical unit), `ref_low`, `ref_high`, `ref_text` (the source's range as given), `source_flag` (`L`, `H`, `LL`, `HH`, `A`, `AA`, `N` or null), `status` (`preliminary`, `final`, `amended`, `corrected`, `cancelled`, `entered-in-error`), `effective_at`, `issued_at`, `standardised` |
| `emr.medications` | MedicationRequest / MedicationStatement | `patient_id`, `kind` (`request`, `statement`), `atc` (null when unmapped), `local_code`, `local_display`, `dose_value`, `dose_unit_ucum`, `route`, `frequency_text` (≤ 60), `status` (`active`, `on-hold`, `stopped`, `completed`, `cancelled`, `entered-in-error`), `start_at`, `end_at` |

**Provenance on every canonical row** (ADR 0009 rule 2): `raw_id` (or `document_id` and `review_id` for `source_kind = document`), `source_kind` (`connector`, `document`), `connection_id`, `connector_type`, `connector_version`, `source_id`, `source_version`, `received_at`, `mapping_version` (the code map's version used), `normaliser_version`. A row is never updated in place by a new source version: the new version is a new row, the old one gets `superseded_by`. Rules always read the current version; the evidence of an alert points at the exact versions it used.

**Corrections from the source:** `amended` and `corrected` are new versions (rules re-run, §11.4). `entered-in-error` and `cancelled` mark the row; an open alert built on it is withdrawn with reason `source_retracted` (§12.5) and the clinician sees why.

### 8.3 What we do not store

Addresses, phone numbers, email addresses, national ids (NIN, BVN), insurance numbers, next of kin, free-text notes and comments, practitioner names, and any field not on the allow-list (§14.1). `display_name` and `mrn` exist only because an alert must show them (frontend §9, needs sign-off, §17.6).

## 9. Normalisation (one place, not in connectors)

### 9.1 Codes

- **Labs and vitals to LOINC.** The normaliser reads every coding the source gave. If one is LOINC (`http://loinc.org`), it is used as given (and checked against the bundled LOINC table: an unknown or deprecated LOINC is treated as unmapped). Otherwise the per-hospital, per-connection map `emr.code_map (connection_id, local_system, local_code) → loinc` is used.
- **Medications to ATC** (WHO ATC/DDD). RxNorm is US-centred and most drugs used in Nigeria are not in it; local brand names stay as `local_display`. A drug matters to rules only through its ATC class (for example C03C loop diuretics, C03DA potassium-sparing, A12BA potassium supplements, H02AB corticosteroids), so ATC level 4 or 5 is enough. Whether RxNorm is added later is open question 6.
- **Conditions to SNOMED CT and ICD-10:** not read in this design (non-goal). The table and map shapes are the same when they are.
- **Unmapped codes are kept, flagged and never guessed** (ADR 0009 rule 4): `standardised = false`, the local code and display kept, and the code counted in `emr.unmapped_codes (connection_id, local_system, local_code, local_display, kind, first_seen, last_seen, count)`. No fuzzy match on display text, ever. An unmapped result runs only the "source flagged critical" rule (§11.2), so the source lab's own critical flag still reaches a clinician.

### 9.2 Units (UCUM)

- Every source unit string is parsed to a UCUM code through a curated table (`mg/dl`, `mg/dL`, `mg%` → `mg/dL`; `mmol/l` → `mmol/L`; `x10^9/L`, `10*9/L`, `G/L` → `10*9/L`; and so on). An unknown unit string is not guessed: the value keeps `unit_source`, `unit_ucum` is null, the observation is `standardised = false`, and the code is counted as unmapped.
- **Each analyte (LOINC) has one canonical unit** in the rule set (§11), for example potassium `mmol/L`, creatinine `umol/L`, glucose `mmol/L`, haemoglobin `g/dL`. Conversions are per analyte, from a reviewed table (molar conversions depend on the analyte: glucose mg/dL × 0.0555, creatinine mg/dL × 88.4). A unit that has no reviewed conversion for that analyte leaves `value_si` null, and no numeric rule runs on it.
- Values: a decimal parsed strictly (`.` or `,` as decimal separator per connection setting, never inferred from the value). Comparators (`<0.5`, `>100`) are kept as `value_text` plus a comparator column; a rule that needs a number treats `>100` as at least 100 and `<0.5` as at most 0.5, never as equal.

### 9.3 Times

Every time is stored in UTC with the source's offset applied. A source time without an offset is read in the hospital's time zone (`hospitals.time_zone`) only for connectors whose format says so (FHIR `date` without time is a date only); `mhai_csv_v1` refuses it. A time more than 10 minutes in the future is refused (`unreadable_data`).

### 9.4 Mapping change control

Code and unit maps are clinical changes (ADR 0009 rule 4, CONTRIBUTING "Clinical change control"):
- maps live in the repository, `services/emr/maps/<hospital short code>/<connection>.yaml` and the shared `services/emr/maps/common/`, with a `version`;
- a change is a `clinical-review` PR with the evidence (the local catalogue entry, synthetic examples) and a clinically qualified reviewer;
- the emr service loads them at start, refuses a map whose LOINC or ATC codes are not in the bundled tables, and records `mapping_version` on every row;
- after a map change, records of the last 7 days with that local code are re-normalised and re-evaluated (§11.4, no alerts for values older than the freshness window).

A platform report of unmapped codes per connection (counts, codes and display text, no patient data) is a CLI command in this design (`emr unmapped --connection <id>`), and a platform screen later.

## 10. The patient index and MHAI patient ids

### 10.1 Within one hospital only

- **The MHAI patient id** is `emr.patients.id`: a random uuid v4, never sequential, never derived from an MRN or a source id. It can't collide with the reserved ids (`reference`, `unassigned`, `unknown`, `ref`) by shape, and the insert checks it anyway.
- **Every link is inside one hospital** (ADR 0009 rule 5): `patient_links.patient_id` must belong to the connection's hospital (a foreign key on `(hospital_id, id)` pairs and a `CHECK` through it). No query, table or function joins patients across hospitals; a static test greps the emr SQL for any join on `patients` without `hospital_id`.

### 10.2 Matching

- **Same connection:** the source patient id decides, always. A new source id is a new MHAI patient.
- **Across connections of the same hospital** (Havana's lab export and its ward export): link automatically only on an exact match of the normalised hospital MRN **and** birth date **and** sex, with the MRN namespace declared the same for both connections by the platform admin (`settings.mrn_namespace`). Anything less (MRN equal but birth date differs, name similar) creates a separate patient and a **possible-match** row for a person to decide. No fuzzy automatic merge.
- **Merges in the source** (FHIR `Patient.link` `replaced-by`): the old MHAI patient gets `status = merged_into` and its records move with a recorded `merge_id`. A merge is audited (`emr.patient_merged`, ids only). An un-merge is a manual operation.
- **Documents:** the patient is matched by the reviewer in the document pipeline, never automatically (§5.3).
- **Excluded patients:** a hospital can exclude a patient from MHAI (a patient who objects, VIP policy). `status = excluded`: nothing is stored beyond the link, no rule runs, no alert. How a hospital tells us is open question 4.

### 10.3 What the index is not

It is not an enterprise master patient index across hospitals, and it never will be (ADR 0009 alternatives). A patient who attends two hospitals is two unrelated MHAI patients.

### 10.4 Patient ids in questions and tokens (`pids`)

This closes the design half of TODO "Patient ids in questions and tokens". The build of "Ask MHAI about this patient" is a later plan; this design fixes the rules it must follow:
- core accepts an MHAI patient id in a request only after `emr_patient_in_hospital(pid)` (a `SECURITY DEFINER` function, `mhai_gateway` only) confirms it belongs to the caller's hospital and is not `excluded` or `merged_into`; otherwise 404 `not_found` (never 403, so existence doesn't leak);
- the checked ids go in the request token's `pids` claim (boundary §7.2); the worker refuses any patient id in a body not in `pids` (check 8);
- EMR-derived knowledge chunks, when they exist, carry this id as `kb_chunks.patient_id`, so `chunk_visible_to()` and ADR 0004 apply unchanged.

## 11. The abnormality rules engine

### 11.1 Shape

- **Deterministic and data-driven.** A rule set is a YAML file in the repository (`services/emr/rules/ruleset-<version>.yaml`), loaded at start and pinned by its SHA-256; the emr service refuses to start on a rule set that fails its schema or its golden tests (§11.5). Each finding records `rule_set_version`, `rule_id` and the hash.
- **Rules run only on the canonical model** (ADR 0009 rule 8): LOINC, canonical units, ATC classes. Never on a vendor code.
- **Rule kinds:**

| Kind | Example (values for clinical sign-off, H4) |
|---|---|
| `threshold` | potassium ≥ 6.5 mmol/L critical, ≥ 6.0 urgent, ≤ 2.5 critical, ≤ 3.0 urgent |
| `source_flag` | the source lab marked the result critical (`LL`, `HH`, `AA`): critical, even when the code is unmapped |
| `reference_range` | outside the source's own range (`ref_low`/`ref_high`) with no threshold rule hit: info |
| `delta` | creatinine rise ≥ 26.5 µmol/L within 48 h, or ≥ 1.5× the lowest value of the last 7 days (the KDIGO AKI criteria): urgent; haemoglobin fall ≥ 2 g/dL within 24 h: urgent |
| `trend` | three consecutive rising lactate values over 6 h: urgent |
| `score` | NEWS2 from one set of vitals taken within 1 h: total ≥ 7 critical, 5–6 urgent, any single parameter scoring 3 urgent. Computed only when every parameter is present; a missing parameter means no score, never "normal" |
| `combination` | potassium ≥ 5.5 with an active potassium-sparing diuretic or potassium supplement (ATC): raises the severity one step and adds the medication as evidence |

- **Population conditions:** age bands and sex, from the patient row, only where the clinical reference requires it (for example haemoglobin, creatinine). An unknown age or sex uses the rule's `unknown` band, which the clinical reviewer sets (usually the widest range), never a default adult value silently.
- **Units of a rule are the analyte's canonical unit.** A value without `value_si` doesn't reach a numeric rule (§9.2).

### 11.2 Reference ranges, versioned

- **Two kinds, kept apart.** The source lab's own range (`ref_low`, `ref_high`, `ref_text`) is shown as given (frontend `reference_range`) and drives the `reference_range` rule (info). **Critical and urgent thresholds are MHAI's,** in the rule set, from a cited clinical reference chosen by the clinical lead (H4), never taken from a source.
- Each threshold in the rule set has a `source` field (the reference it came from) and a `reviewed_by` list. A threshold without both fails the schema.
- A rule set version is immutable; a change is a new version.

### 11.3 Findings, likely causes and evidence

- A rule hit makes a **finding**: `emr.findings (id, hospital_id, patient_id, rule_set_version, rule_id, category, severity, primary_observation_id, observation_ids[], medication_ids[], effective_at, created_at, status, dedup_key)`.
- **Category** comes from the rule (`blood_chemistry`, `blood_count`, `kidney_function`, `blood_gases_lactate`, `heart_markers`, `blood_sugar`, `vital_signs`, `infection_markers`, the eight in mhai-web's fixtures), with display labels and examples in the rule set.
- **Likely causes** (at most three, strongest first) are rule-authored: each cause has a label ("Loop diuretic", "Low magnesium"), a confidence word, and evidence queries that look for supporting canonical rows of the same patient within a window (an active C03C medication started in the last 30 days; a magnesium below range in the last 72 h). A cause is shown only when at least one evidence query finds something. Each cause carries at most three evidence items: a label, a value with its flag, and a time, never an identifier (Plan 13 D13).
- **Confidence words:** a rule may say `caution` or `review`, never `high`, in this design (open question 3). They are suggestions, labelled as such in the app, never a diagnosis (ADR 0009 rule 11).

### 11.4 When rules run, and freshness

- On every `records.v1.<hid>.created` event, for the patients it names: the rules whose inputs include the new record's LOINC or ATC class run over that patient's current canonical rows. Idempotent: a finding's `dedup_key` is `(patient, rule_id, primary_observation_id)`, unique, so a redelivered event makes no second finding.
- **Only `final`, `amended` and `corrected` observations** raise findings. `preliminary` results are stored and evaluated for critical `threshold` and `source_flag` rules only (a critical preliminary potassium is worth a call), marked "preliminary" in the alert.
- **Freshness window:** a finding whose `effective_at` is older than the category's window (default 72 h for labs, 6 h for vitals; in the rule set, clinically reviewed) is stored with `status = historical` and **never** becomes an alert. Backfill (§7.1) and re-normalisation always produce `historical` findings. This is what keeps old archive documents and a first sync from flooding clinicians.
- **Late data is not hidden:** a finding inside the window but received more than 1 h after `effective_at` becomes an alert that says when the value was taken (`finding.recorded_at`) and that it arrived late (an additive `arrived_late: true` on the alert; mhai-web shows it once it adds the word, §16.4).

### 11.5 Clinical change control for rules

- A rule set change is a `clinical-review` PR (CONTRIBUTING): the YAML diff, the cited references, and the **golden test report**, before and after.
- **Golden tests:** `services/emr/rules/testdata/` holds synthetic patients with timed results, vitals and medications, and the findings each must and must not raise (at least one positive and one negative case per rule, each band edge, missing parameters for NEWS2, unit conversions, preliminary vs final, an amended result, an `entered-in-error` result). CI runs them; a change in any expected finding must be explained in the PR.
- **Shadow first:** a new rule set version is deployed in `shadow` for at least 7 days on each live hospital: it evaluates the same events, writes findings with `shadow = true`, and raises no alert. A report (counts by rule and severity, old vs new, no patient data) goes in the PR that promotes it. Promotion is a setting change (`EMR_RULE_SET_ACTIVE`) with an audited deployment record.
- **Two people:** the clinical lead (or a named delegate) and a second clinician approve, as for corrections (ADR 0005).
- **Hospitals can't change thresholds.** They choose levels per category (§12.2); that filters delivery, it never changes what is abnormal.

### 11.6 No machine learning in v1

Not justified now: there is no labelled outcome data from these hospitals, a learned score would need its own evaluation and clinical sign-off under the model promotion flow, and an unexplainable score breaks "evidence with every alert". Deterministic rules with cited thresholds can be reviewed line by line, tested exhaustively with synthetic cases, and explained to a clinician. ML enters later only as an **additional** signal with its own design, never replacing a threshold, and only through the model promotion flow (document pipeline design §7–§8).

## 12. Alerts: routing, fatigue and audit (core)

### 12.1 From finding to alert

Core's durable consumer `core_emr` takes `emr.v1.<hid>.finding`, and in one transaction under `WithHospital(hid)`:
1. reads the finding; if `historical` or `shadow`, acks and does nothing;
2. checks the hospital is `active` and has a connection that is on; otherwise no alert (the finding stays, audited as `alert.not_raised` with reason code);
3. applies the hospital's **alert settings** (§12.2): the patient's unit is covered, the category's level admits the severity;
4. applies the **fatigue controls** (§12.4);
5. writes `alerts` and one `alert_deliveries` row per recipient, and the audit row `alert.raised`.

A finding that a setting filters out is recorded (`alert.not_raised`, reason `unit_not_covered` or `level_off`), so Usage can count what was filtered and an auditor can see it.

### 12.2 Alert settings (hospital admin)

- `alert_settings (hospital_id, queue boolean, version, updated_by, updated_at)`; `alert_units (hospital_id, unit_id, covered)`; `alert_levels (hospital_id, category, level)`; `alert_recipients (hospital_id, user_id)`.
- **Defaults when a hospital's first connection is switched on:** all units covered, queue on, no named clinicians, levels `critical_urgent` for every category except `vital_signs` at `critical` (open question 5).
- **Unit not recorded:** a patient with no current encounter location is **covered** (fail toward alerting; the alert reads "Location not recorded"). A new unit discovered in the source is covered by default and shown to the hospital admin.
- **Recipients:** the queue, named clinicians, or both; at least one (422 `no_recipients`). Named clinicians must be active users of the hospital with the `clinician` role. If every named clinician is later deactivated and the queue is off, alerts go to the **queue** anyway and the alert settings page shows the problem (fail toward delivery; never to nobody).
- Saved per section with a version (409 `conflict` on a stale one). No step-up (Plan 13 D8).

### 12.3 Who sees an alert

- **Queue alerts:** every clinician of the hospital (Plan 13 D11). **Named alerts:** the named clinicians. Hospital-admin-only and analyst accounts get none (frontend §9).
- `routing` on each alert, for the caller: `to_you` (a delivery row for them), `queue` (on the queue).
- Pop-ups (in the app, from the BFF's polling of `GET /alerts/unread-count` and `GET /alerts?status=new`): critical always; alerts to you by name when your pop-up switch is on; queue-only urgent and info are listed and counted, not popped up (Plan 13 D11, D12). The mute switch is `alert_popups` in `/me/notification-settings`; it stops pop-ups only.

### 12.4 Fatigue controls

- **One open alert per patient, rule and analyte.** A new hit for the same `(patient, rule_id, loinc)` while an alert is `new` updates that alert (latest value, evidence) and does not notify again, unless the severity rises (urgent → critical), which re-notifies.
- **After an alert is handled,** the same rule for the same patient re-alerts only when the severity is higher, or after the rule's `quiet_hours` (default 24 h for urgent, 6 h for critical, 72 h for info).
- **No cap on critical.** Info alerts are capped at 20 per hospital per hour; past it they are stored, listed and counted, not notified (no pop-up, no bell increment), and the platform's metric shows the cap was hit. Urgent is never capped in this design (open question 5).
- **Usage numbers** (frontend §9.1, Plan 13 D15, `by_route`) are counts under the small-group rule; served when `/hospital/usage` is built (§19, PR 9).

### 12.5 Handling

- `POST /alerts/{id}/acknowledge` and `/dismiss {reason, note?}`: first one wins; a second gets 409 `already_handled` with who and when (frontend §9). Dismiss reasons: `not_relevant`, `already_known`, `wrong_patient`, `other`; the note (≤ 500) is PII-masked before storage and never on the audit chain.
- `wrong_patient` dismissals are counted per connection on the platform's health (a matching problem signal) and reviewed weekly.
- **Withdrawn by the source:** an alert whose primary observation becomes `entered-in-error` or `cancelled` is moved to `dismissed` with `handled_by: null` and reason `other`, plus an additive `withdrawn: "source_retracted"` field; never deleted.

### 12.6 Audit (typed registry, closed payloads)

| Action | When | Payload (ids and codes only) |
|---|---|---|
| `emr.connection_added`, `emr.connection_changed`, `emr.connection_switched_on`, `emr.connection_switched_off`, `emr.connection_tested` | platform routes | `hospital_id`, `connection_id`, `type`, `fields_changed[]` (keys only), `credentials_replaced` (bool), `outcome` (test code) |
| `emr.credentials_deleted` | hospital removal | `hospital_id`, `connection_id`, `fields[]` |
| `alert_settings.units_changed`, `.rules_changed`, `.recipients_changed` | hospital admin saves | `hospital_id`, counts (`covered`, `levels_changed`, `recipients`), `queue` |
| `alert.raised` | an alert is created | `alert_id`, `finding_id`, `rule_set_version`, `rule_id`, `category`, `severity`, `routed_queue`, `routed_named` (count) |
| `alert.not_raised` | a finding filtered | `finding_id`, reason code |
| `alert.acknowledged`, `alert.dismissed` | a clinician handles one | `alert_id`, `reason` |
| `alert.withdrawn` | source retraction | `alert_id`, `reason` |
| `alerts.read` | a list page of alerts is served | `count`, `filter` codes (one row per request, like `audit.searched`) |
| `emr.patient_merged` | a source merge | `from`, `to`, `merge_id` |

mhai-web's Activity and audit kinds map to these (Plan 13: `emr_connection_*`, `alert_units_changed`, `alert_rules_changed`, `alert_recipients_changed`); the catalogue in `audit/catalogue.go` gets the rows, and kinds mhai-web has no words for yet are sent bare, as Plan 4 does.

## 13. Roles and permissions

### 13.1 Routes (core's permission table)

| Route | Roles | Rule flags |
|---|---|---|
| `GET /platform/emr/connector-types`, `GET /platform/emr/connections`, `GET /platform/emr/connections/{id}` | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` |
| `POST /platform/hospitals/{id}/emr/connections`, `PATCH /platform/emr/connections/{id}`, `PUT /platform/emr/connections/{id}/enabled` | `platform_admin` | `Platform`, `NeedsMFA`, `Write`, `StepUp` |
| `POST /platform/emr/connections/{id}/test` | `platform_admin` | `Platform`, `NeedsMFA`, `Write` (no step-up: it changes nothing, Plan 13 D5) |
| `GET`, `PATCH /hospital/alert-settings` | `hospital_admin` | `NeedsMFA`; `Write` on PATCH |
| `GET /alerts`, `GET /alerts/unread-count`, `POST /alerts/{id}/acknowledge`, `POST /alerts/{id}/dismiss` | `clinician` | `Write` on the POSTs |
| `GET`, `PATCH /me/notification-settings` | `clinician` | `Write` on PATCH |

### 13.2 Database grants (summary)

- `mhai_platform`: `emr_connections` `SELECT, INSERT, UPDATE` (config columns); `emr_platform_health()` and `emr_credentials_meta(connection_id)` definer functions (codes, counts, `set_at` only). Nothing in `emr.*` patient tables.
- `mhai_gateway`: `alert_settings`, `alert_units`, `alert_levels`, `alert_recipients` `SELECT, INSERT, UPDATE` (own hospital); `alerts` `SELECT` and `UPDATE (status, handled_by, handled_at, dismiss_reason, dismiss_note)` with a restrictive policy (only from `new`); `alert_deliveries` `SELECT, UPDATE (read_at)` own rows; `user_notification_settings` own row; `emr.units` `SELECT (id, label, kind, connection_id)`; the patient and finding columns in §3.3.
- `mhai_emr_core` (core's `core_emr` consumer, new NOLOGIN role like `mhai_jobs`): `emr.findings` `SELECT`; `alerts`, `alert_deliveries` `INSERT, UPDATE`; settings `SELECT`; its audit actions only.
- `mhai_emr`: the `emr` schema of its hospital; nothing in core's tables but the columns in §3.3.

## 14. Data minimisation and retention

### 14.1 The allow-list

Each connector maps its source to raw records through an allow-list of fields per resource kind, enforced in `RawSink` (a field not on the list is dropped before storage, and the count of dropped fields is a metric):

- **Patient:** source id, hospital MRN (one identifier, in the namespace the platform admin named), given and family name (for `display_name`, joined, at most 120 characters), birth date, administrative sex, `link` (merges), active flag.
- **Encounter:** source id, patient, class, status, period, location reference, bed label.
- **Location:** source id, name (at most 60), type.
- **Observation:** source id and version, patient, encounter, category, codings, value (quantity, string, codeable concept), unit, reference range (low, high, text), interpretation, status, effective and issued times.
- **Medication:** source id and version, patient, medication codings and text, dose quantity, route, timing text, status, authored and effective period.

Never: addresses, telecom, national or insurance identifiers, contacts, practitioners' names, notes, `text` narrative, attachments, extensions not named in a connector's reviewed list.

### 14.2 Retention (recommended values, open question 7)

| Data | Kept | Then |
|---|---|---|
| `raw_records.payload` | 7 days | cleared (hash and provenance kept for the canonical row's life) |
| Canonical observations and medications | 13 months after `effective_at` | deleted, with their links; enough for delta rules, trends and year-on-year Usage |
| Patients | while any canonical row, finding or alert references them | deleted |
| Findings that never became alerts | 90 days | deleted |
| Alerts and deliveries | 24 months | patient columns cleared (the alert keeps codes, times and counts for Usage) |
| Audit chain rows | as the audit log (no PHI on the chain) | |
| Credentials | while the connection exists | deleted at hospital removal |

- A **legal hold** (contract §14.8.1) on a hospital stops every deletion for it.
- **Switching a connection off** stops polling; data already held follows the table above.
- **Hospital removal** (two-person flow) deletes the hospital's `emr` schema rows and credentials as one audited job.
- A sweep job (`emr_retention`, `jobs.v1.global.emr_retention`) runs nightly with a cap per run; it is the retention plan's first user.

### 14.3 Logs, traces and metrics

Ids, codes and counts only. No name, MRN, value, code display text, URL path containing a source patient id, or credential. Connector HTTP errors are logged by status code and closed outcome code only, never the response body (it can carry PHI). A test runs a full synthetic sync and greps every log record and span for the synthetic names, MRNs and values.

## 15. Failure modes: fail closed, never silent

### 15.1 Principles

- **Missing data is never shown as normal.** Clinicians and hospital admins always see the age of the data (the source's `last_update_at`, already on the alert settings page) and the app is told when a source is behind (§16.4).
- **Doubt stops the line for that record, not silently for the hospital:** a record that can't be normalised is rejected with a code and counted; a rule that fails stops evaluation for that hospital and alerts operators; nothing evaluates on half a record.
- **No invented values:** no default unit, no inferred time zone, no guessed code, no assumed parameter in a score.

### 15.2 Table

| Failure | Detected by | What happens | Who sees it |
|---|---|---|---|
| Source unreachable, DNS, TLS failure | fetch | backoff; `unreachable` | platform: Needs attention; hospital admin: "Connected with problems"; clinicians: "records may be out of date" when the source is past its staleness limit (§16.4) |
| Credentials refused (401/403, OAuth `invalid_client`) | fetch, test | stop polling that connection after 3 in a row; `sign_in_refused` | as above, plus a platform metric alert |
| Rate limited (429) | fetch | honour `Retry-After`; `too_many_requests` | platform |
| Timeout | fetch | retry next run; `timed_out` | platform |
| Malformed or oversized data | connector, `RawSink` | the record or file rejected whole, `unreadable_data` | platform |
| Unmapped code or unit | normaliser | stored `standardised = false`; only the source-flag rule runs; `unmapped_codes` | platform (counts), mapping report; clinicians see "Not standardised" on any alert from it |
| **Feed stale** (no successful fetch for 3 × `poll_seconds`, or none newer than the source's typical gap) | scheduler | `health = errors`; the hospital's records status is "problems" | all three; never a silent empty alerts list |
| Rules engine error, invalid rule set | rules consumer | that hospital's evaluation stops (no partial run); messages are not acked, so they wait; metric alert to on-call | platform health; clinicians get the out-of-date notice for that hospital |
| Normaliser or rules message fails 5 times | JetStream | `dead.v1.<hid>.emr_normalise` or `emr_rules`; core's Failed jobs view (types `emr_normalise`, `emr_rules`), retry from there | platform |
| Core down | `core_emr` not consuming | findings wait in `EMR` (7 days); on recovery core raises alerts for those still inside the freshness window, marked late | on recovery |
| emr service down | core's health sampler (`emr.v1.ctl.health`) | the source status is computed from `last_sync`; stale after the limit | as "feed stale" |
| Secret store unavailable | fetch | no fetch; `sign_in_refused` is **not** used; the connection shows `unreachable` and a metric alerts | platform |
| Clock skew (source times in the future) | normaliser | refused, `unreadable_data` | platform |
| A patient link conflict (two MHAI patients claim one source id) | unique index | the insert fails, the record is rejected, a metric alerts; never linked to the wrong patient | platform |
| A hospital deactivated | everywhere | polling stops; findings raise no alerts (§12.1 step 2) | |

### 15.3 Never alert on stale data silently

- A finding outside the freshness window never becomes an alert (§11.4).
- An alert inside the window that arrived late says so (`arrived_late`) and shows the time the value was taken.
- An empty alerts list is only shown as "no alerts" when every source of the hospital is current; otherwise the list carries `sources_status` (§16.4) and the app says which source is behind.

## 16. API contract (additive, `openapi.yaml` minor bump)

All additive under ADR 0012: new routes, and new optional fields. Error codes are core's (`validation_failed` 422, `conflict` 409, `not_found` 404, `forbidden` 403, `step_up_required`), plus new codes `not_tested` (409), `no_recipients` (422), `already_handled` (409). mhai-web's mock uses 422 `invalid`; it maps `validation_failed`, as Plan 4c's follow-up already records.

### 16.1 Platform (matches `apps/platform/lib/api/emr-contract.ts`)

| Method and path | Request | Reply |
|---|---|---|
| `GET /platform/emr/connector-types` | | `{types: ConnectorType[]}` (§4.1; only built types) |
| `GET /platform/emr/connections?hospital=&state=all\|needs_attention\|off\|not_connected` | | `ConnectionsOut {connections: EmrConnectionSummary[], not_connected: Person[]}` |
| `POST /platform/hospitals/{id}/emr/connections` | `ConnectionIn` + step-up; `Idempotency-Key` | `EmrConnection`, saved **off** |
| `GET /platform/emr/connections/{id}` | | `EmrConnection` |
| `PATCH /platform/emr/connections/{id}` | `ConnectionPatch` + step-up (`version`) | `EmrConnection`; clears `last_test`; 409 `conflict` |
| `PUT /platform/emr/connections/{id}/enabled` | `{enabled, version}` + step-up | `EmrConnection`; 409 `not_tested` when switching on without a passing test of the saved settings |
| `POST /platform/emr/connections/{id}/test` | `Idempotency-Key` | `EmrConnection` with `last_test {at, ok, code}` |

- `type_label` is the type's label (the platform app may show it, Plan 13 constraints); `source_label` 1–40 characters, no vendor or product name (checked against a list, as mhai-web's `FORBIDDEN_TERMS`); `base_url` https (sftp for `file_export`), ≤ 500; each credential 1–4,000.
- `credentials: [{key, set_at}]`; never a value. `errors`: at most 5, last 24 h, closed codes `unreachable`, `sign_in_refused`, `timed_out`, `unreadable_data`, `unmapped_codes`, `too_many_requests`.
- `health`: `never` (no sync yet), `ok`, `errors` (any error in the last 3 runs, or stale).
- `last_sync {at, ok, records}`; `errors_24h` sum.
- Additive optional fields: `poll_seconds`, `settings.record_link`, `settings.mrn_namespace`, `private_network`. The app ignores them until it shows them.

### 16.2 Hospital (matches `apps/clinical/lib/api/emr-contract.ts`)

- `GET /hospital/alert-settings` → `AlertSettingsOut {connection: {status, sources[{label, status, last_update_at}]}, units[], categories[], recipients {queue, clinicians[]}, updated_by, updated_at, version}`. Status only: no address, type or error code (Plan 13 D7). `connection.status` is the worst of the sources; `not_connected` when none.
- `PATCH /hospital/alert-settings` with exactly one of `units {covered[]}`, `rules {levels}`, `recipients {queue, clinician_ids[]}` plus `version`; `Idempotency-Key`. 422 `validation_failed` (unknown ids, a level outside the list, two sections), 422 `no_recipients`, 409 `conflict`.

### 16.3 Clinicians (matches `alerts-contract.ts` and frontend §9)

- `GET /alerts?status=&severity=&source=&sent_to=you|queue&cursor=&limit=` → `AlertPageOut {alerts[], next_cursor, sources[], queue}`.
- `AlertOut` fields as the contract: `finding` (display label from LOINC or the source's own when unmapped, `value` as recorded, `unit` display, `flag`, `reference_range` as given, `recorded_at`, `code_system` (`loinc` or `local`), `standardised`), `likely_causes[{label, confidence, evidence[]}]`, `patient {full_name, mrn, location}`, `age`, `sex`, `source_label`, `emr_link`, `status`, `handled_by`, `handled_at`, `dismiss_reason`, `created_at`, `category`, `routing`.
- `GET /alerts/unread-count` → `{count}`: new alerts that are yours by name or on the queue, unhandled.
- `POST /alerts/{id}/acknowledge`, `POST /alerts/{id}/dismiss {reason, note?}`: `Idempotency-Key`; 409 `already_handled` with `AlertHandledOut`.
- `GET`, `PATCH /me/notification-settings {alert_popups, correction_popups}`.
- Another hospital's alert id, or an alert not routed to the caller: 404.

### 16.4 Additive fields mhai-web doesn't use yet

Each is optional and ignored until mhai-web shows it (mhai-web follow-ups, §19 PR 10):
- `AlertOut.arrived_late: boolean`, `AlertOut.preliminary: boolean`, `AlertOut.withdrawn: "source_retracted" | null`;
- `AlertPageOut.sources_status: [{label, status: "current" | "behind", last_update_at}]`, so the alerts page can say "Main record last updated 3 hours ago" instead of an empty list;
- `ConnectorType` gains `document_archive` and `file_export` as data (no screen change; Plan 13 D4).

## 17. Security and threat model

### 17.1 Assets

- Patient data read from hospitals: names, MRNs, birth dates, results, vitals, medications, locations.
- Hospital record-system credentials (OAuth private keys, API keys, SFTP keys) and the emr service's sealing key.
- Hospital separation: every canonical row, patient link, finding and alert belongs to one hospital.
- Clinical safety: an alert must be right about the patient and the value, traceable to its source, and its absence must not be read as "all normal" when data is missing.
- The audit trail; availability of the alert path; the hospitals' own systems (we must not overload or misuse them).

### 17.2 Actors and trust boundaries

- **Actors:** platform admins (configure connections), auditors (read), hospital admins (alert settings), clinicians (alerts), core, the emr service, the document pipeline, NATS, Postgres, the secret store, the egress proxy, the hospitals' record systems and their operators (outside our control), anyone who can put data into a hospital's system (staff, devices, an attacker inside the hospital), anyone who obtains a credential.
- **Boundaries and the checks at each:**
  1. browser → BFF → core (`api.v1.*`): session, two-step, role, step-up on connection writes, hospital from the session only, RLS.
  2. core → emr (`emr.v1.ctl.*`): request token (`aud emr`, subject, body hash, expiry, replay, reply subject), operations table; secrets only as sealed boxes.
  3. emr → secret store: the emr identity's path policy; core has none.
  4. emr → egress proxy → hospital: the SSRF guard, the per-connection host allow-list, TLS verified, size and time limits.
  5. hospital → emr (responses, files): untrusted input; the allow-list, strict parsing, size caps, code and unit maps, refusal on doubt.
  6. emr → Postgres: `mhai_emr` under `WithHospital`, RLS, column grants.
  7. emr → core (`emr.v1.<hid>.finding`): ids only; core re-reads the finding under RLS for the subject's hospital and checks the finding's hospital equals the subject's.
  8. core → clinician: routing, 404 for anything not routed to the caller.

### 17.3 Threats and mitigations (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| E1 | Information disclosure | **Credential leak** from core, logs, errors, idempotency store, database, browser | secrets only in the store, emr-only path policy; sealed at core's edge; route bodies never logged; HMAC in the idempotency store; replies carry `set_at` only; redacting `Creds`; AST test | A1, A2, A3 |
| E2 | Spoofing | **A forged control request** asks emr to test against an attacker's host or to replace credentials | only `gateway` may publish `emr.v1.ctl.>`; request token with subject and body binding, `aud emr`, short expiry, replay check; `put_credentials` needs a platform admin `sub` with step-up recorded by core | A4 |
| E3 | Information disclosure / tampering | **SSRF**: a base address that points at our internal services or a cloud metadata endpoint | scheme and shape rules; resolution-time address checks; the egress proxy's host allow-list on its own network; no redirects across hosts; `private_network` only by platform admin with H7 | A5, A6 |
| E4 | Tampering | **Hostile source data**: huge bundles, deep JSON, zip bombs, CSV formula injection, HTML in display text, a `next` link to another host | size, depth and row caps; the allow-list; strict decimal and time parsing; display text length-capped and stripped of control and markup characters (the app renders text only); `next` links on the base host only | A7, A8 |
| E5 | Tampering (clinical) | **Wrong values raise wrong alerts**: unit mix-ups, a guessed code, a wrong time zone | no guessing anywhere (§9); per-analyte reviewed conversions; unknown means unmapped; clinical review of maps and rules; golden tests; shadow runs | A9, A10 |
| E6 | Information disclosure | **Wrong patient**: two patients linked, or a record attached to the wrong patient | links by source id within a connection; cross-connection only on exact MRN, birth date and sex with a declared namespace; possible matches to a person; documents matched by a reviewer; `wrong_patient` dismissals monitored | A11, A12 |
| E7 | Information disclosure | **Cross-hospital leakage** through the index, a finding, an alert or a NATS subject | RLS forced on every table; `(hospital_id, id)` foreign keys; no cross-hospital join (static test); core re-reads a finding under the subject's hospital and refuses a mismatch; 404 for foreign ids | A13, A14 |
| E8 | Elevation of privilege | **A hospital admin raises or hides alerts** beyond their choice, or names a clinician of another hospital | levels filter only; thresholds in the repository; recipients must be active clinicians of the same hospital (checked in SQL); a no-recipient state falls back to the queue | A15, A16 |
| E9 | Elevation of privilege | **A clinician sees alerts not routed to them** or acknowledges another hospital's alert | `alert_deliveries` and queue visibility checked in the query; restrictive policy on `alerts` updates; 404 | A17 |
| E10 | Denial of service | **Alert flood** (first sync, a bad map, a source replay, a noisy rule) hides the critical ones | backfill and historical findings never alert; idempotency by source id and version; dedup and quiet hours; info cap; shadow runs for new rules; per-hospital stream limits | A18, A19 |
| E11 | Denial of service | **We overload a hospital's system** | one fetch per connection; page and record caps per run; `Retry-After` honoured; minimum poll interval | A20 |
| E12 | Repudiation | **Nobody can show why an alert was raised or who changed the settings** | provenance on every row; `rule_set_version`, `rule_id`, `mapping_version` on findings; typed audit actions for every connection, setting and handling | A21 |
| E13 | Information disclosure | **PHI in NATS, logs, traces, dead letters, metrics** | ids only in messages; JetStream encryption at rest; log and span redaction; connector errors by status code only; the synthetic-sync grep test | A22 |
| E14 | Tampering (safety) | **Silent gap**: the feed stops, or the rules stop, and the app shows "no alerts" | staleness detection, `health = errors`, the out-of-date notice (`sources_status`), metric alerts, rules errors stop and wait instead of skipping | A23, A24 |
| E15 | Spoofing | **A compromised connector or emr host** writes forged records for another hospital | `WithHospital` per connection's hospital; RLS `WITH CHECK`; the emr role can't touch core's tables; core takes the hospital from the subject and re-checks it | A13 |
| E16 | Information disclosure | **The record link** sends the clinician to a phishing page or carries an injected URL | the link template's host must equal the connection's host; https only; the source patient id path-escaped; `emr_link` built only in core from stored parts | A25 |
| E17 | Elevation of privilege | **An unreviewed document value raises an alert** | document records reach the canonical tables only through `finalise_document` after review; `source_kind = document` rows need a `review_id` (`CHECK`) | A26 |

### 17.4 Controls (decided)

- **Authentication and step-up:** platform admins with two-step session and a fresh code for every connection change; hospital admins with a two-step session for alert settings; clinicians as today. Service calls use request tokens; the emr service has its own NATS user and database login.
- **Authorisation:** core's permission table (§13.1); RLS on every new table, forced; column grants; the emr service never decides a recipient; core never holds an outside credential.
- **Data handling:** the allow-list (§14.1); ids only in messages; retention (§14.2); no PHI in logs; the masked dismissal note; patient names and MRNs only on alert reads.
- **Concurrency:** one fetch per connection (advisory lock); idempotent raw and canonical writes (unique source id and version); one finding per dedup key; first handler wins on alerts (`UPDATE ... WHERE status = 'new' RETURNING`); settings saved with `version`.
- **Secrets:** a managed store in production, a 0400 file store on the demo only; sealed in transit through core; rotation per §6.3; the emr service refuses to start without its sealing key, and with a placeholder or empty secret in a connection that is on (it marks that connection `sign_in_refused` rather than calling out with nothing).
- **Third parties and hosting:** the hospitals' systems (each under the hospital's data agreement, §17.7); the secret store provider (in-region); no other processor sees the data. No cloud model receives EMR data in this design.

### 17.5 Abuse-case tests (the plan writes these first)

- **A1** A credential sent through `POST` and `PATCH` never appears in any log record, span, error, idempotency row, audit payload, NATS message (other than the sealed box) or reply; the reply has `set_at` only.
- **A2** As `mhai_platform`, `mhai_gateway` or core's cloud identity, no query or API call can read a credential value.
- **A3** A connector that tries to log `Creds` fails the AST test; `fmt.Sprint(creds)` is redacted.
- **A4** A control request with a token for another subject, another body, `aud pipeline`, an expired `exp`, a replayed `jti`, or a reply subject outside `_INBOX_gateway.` is refused with no work done; `put_credentials` with a `svc:core` subject is refused.
- **A5** Base addresses `https://10.0.0.5`, `https://169.254.169.254`, `https://localhost`, `https://user:pw@host`, `http://host`, and a DNS name that resolves to a private address are refused at save or at connect.
- **A6** Through `egress-emr`: `CONNECT` to a host not on any enabled or tested connection is refused; a redirect to another host is not followed.
- **A7** A 200 MB bundle, a 10,000-deep JSON, a page whose `next` link points elsewhere, and a CSV cell starting with `=` are each refused or neutralised; nothing is stored beyond the allow-list.
- **A8** A display text with HTML and control characters is stored stripped and capped.
- **A9** Glucose in mg/dL converts to mmol/L by the reviewed factor; a unit with no reviewed conversion leaves `value_si` null and raises no threshold finding; an unknown LOINC is unmapped.
- **A10** A source time without an offset in `mhai_csv_v1` is refused; a time 1 h in the future is refused.
- **A11** Two connections with equal MRN but different birth dates make two patients and a possible-match row, never one patient.
- **A12** A source merge moves records and is audited; nothing else merges patients.
- **A13** With hospital A set, every `emr` table returns no row of hospital B, and an insert naming hospital B fails `WITH CHECK`; a finding message on `emr.v1.<A>.finding` for a finding of B makes no alert.
- **A14** A clinician of hospital B asking for an alert of hospital A gets 404.
- **A15** A hospital admin's PATCH naming a clinician of another hospital, an inactive user or a non-clinician is 422; recipients with no queue and nobody named is 422 `no_recipients`.
- **A16** All named clinicians deactivated with the queue off: the next alert goes to the queue.
- **A17** A clinician not named on a queue-off alert can't list, count or acknowledge it (404).
- **A18** A first sync of 30 days of synthetic critical results raises no alert; a new critical result after the cursor reaches now raises exactly one.
- **A19** The same result redelivered 5 times, and a source replay of 1,000 records, raise no extra alert; a second hit of the same rule updates the open alert; a severity rise re-notifies.
- **A20** A source answering 429 with `Retry-After: 120` gets no request for 120 s; at most 20 pages are read per run.
- **A21** Every connection change, settings save, raised, filtered, acknowledged, dismissed and withdrawn alert has its audit row with a valid closed payload; an unknown key is refused.
- **A22** A full synthetic sync, rule run and alert cycle leaves no synthetic name, MRN or value in any log, span, metric label, NATS message or dead letter.
- **A23** A source silent for 3 poll intervals turns `health` to `errors`, the hospital status to "problems" and `sources_status` to `behind`.
- **A24** A rule set that throws on one patient stops evaluation for that hospital, leaves the messages unacked, raises the operator alert, and makes no partial findings.
- **A25** A record link template on another host, `javascript:`, or a source patient id containing `../` or `?` produces no link or an escaped one.
- **A26** A canonical row with `source_kind = document` and no `review_id` is refused by the database.
- **A27** No route, rule, template or message can address a patient: there is no patient recipient type, and a test asserts the alert recipients are user ids of clinicians only.

### 17.6 Residual risk and sign-off

- **Clinical sign-off (before live use):** the rule set v1 (thresholds, freshness windows, quiet hours, NEWS2 use, likely causes and their evidence), the code and unit maps per hospital, and showing patient names and MRNs in alerts (frontend §9, mhai-web Plan 5a). H4.
- **Privacy and legal (before any real data):** the DPIA, the hospital data processing agreement addendum covering EMR reading and alerting, the NDPA 2023 basis (§17.7), data location and the retention table. H5.
- **Accepted for now:**
  - core sees credentials in memory for the length of one request (sealed at once; BFF-side sealing is open question 10);
  - patient names and MRNs are stored in clear columns protected by RLS, column grants and disk encryption, not application-level encryption (open question 11);
  - a compromised emr host can read every connected hospital's credentials and records; isolation per hospital (one emr process per hospital) is not in this design;
  - polling adds up to two poll intervals of delay; a critical result is not instant;
  - a clinician may still miss an alert (bell and pop-ups only, no phone or email) until the notifications service exists. Critical alerts must not rely on MHAI alone: the hospital's own critical-result call process stays in place, and the sign-off says so.

### 17.7 Consent and legal basis (NDPA 2023)

To be confirmed by a qualified lawyer (H5); section numbers as we read the Act:
- **Roles:** the hospital is the data controller; Measured Health is its data processor, acting on the hospital's written instructions under a processing agreement (s. 29). The agreement names this purpose (reading labs, vitals and medications to flag abnormal results to the hospital's clinicians), the data categories (§14.1), retention (§14.2), sub-processors (hosting, the secret store) and the location of processing.
- **Lawful basis:** health data is sensitive personal data (s. 30). The basis is the hospital's provision of healthcare by health professionals under a duty of confidentiality, and the patient's vital interests for critical results, not patient consent. Consent is not used as the basis, because it would have to be freely withdrawable and the clinical safety purpose would then depend on it.
- **Transparency and objection:** the hospital's privacy notice covers MHAI; a patient who objects is excluded (§10.2) and the exclusion is honoured within one poll cycle.
- **DPIA** (s. 28): required before live data (health data, systematic processing, automated flagging). We draft it; the hospital's DPO approves it.
- **Data minimisation and storage limitation** (s. 24): §14.
- **Security and breach** (s. 39, s. 40): §17.4; a breach is reported to the hospital at once so it can notify the Commission within 72 hours.
- **Cross-border transfer** (ss. 41–43): if the hosting region or the secret store is outside Nigeria, the transfer needs a basis the lawyer confirms. Recommendation: host EMR data in-region where possible (H5).
- **No automated decisions about the patient** (s. 37): alerts go to a clinician who decides; nothing acts on the patient automatically.

## 18. Migrations

Numbers are taken at build time from the next free one (00018 is the model policy; Plan 4 and the cloud models plan may take more). In order:

1. **`NNNNN_emr_roles.sql`:** schema `emr`; roles `mhai_emr`, `mhai_emr_app`, `mhai_emr_sched`, `mhai_emr_core`; `emr_connections` (in `public`, config only: `id`, `hospital_id`, `type`, `source_label`, `base_url`, `settings jsonb` with a `CHECK` that no secret key name is present, `enabled`, `private_network`, `poll_seconds`, `last_test_at`, `last_test_ok`, `last_test_code`, `version`, `updated_by`, `updated_at`); `emr.credentials` (metadata); `emr.sync_runs` and `emr.connection_errors` (codes and counts); RLS, grants; audit append policies for the `emr.connection_*` actions.
2. **`NNNNN_emr_canonical.sql`:** `emr.raw_records`, `emr.patients`, `emr.patient_links`, `emr.possible_matches`, `emr.units`, `emr.encounters`, `emr.observations`, `emr.medications`, `emr.code_map`, `emr.unmapped_codes`; the `(hospital_id, id)` keys; the `source_kind = document` → `review_id` `CHECK`; `emr_patient_in_hospital()`; grants for the document pipeline's `mhai_worker` (`INSERT` only on observations, medications, patients and links, as its design says).
3. **`NNNNN_emr_findings.sql`:** `emr.findings` with its unique `dedup_key`; `mhai_emr_core` grants.
4. **`NNNNN_alerts.sql`:** `alert_settings`, `alert_units`, `alert_levels`, `alert_recipients`, `alerts`, `alert_deliveries`, `user_notification_settings`; the restrictive update policy; audit append policies for the alert actions.
5. **`NNNNN_emr_retention.sql`:** the retention functions and their roles, with the legal-hold check.

Each migration is additive, has its Down, hands every object to `mhai_migrate`, and adds its tables to `TestRLSWithoutHospitalSeesNothing`.

## 19. Phased build, as pull requests

| PR | What | Depends on | Tests from §17.5 |
|---|---|---|---|
| 1 | **This spec**; ADR 0009 Accepted; ADR 0014 (separate service, document route); file frontend requirements §14.11 and the §12 detail from Plan 13 | | |
| 2 | **emr service skeleton:** `services/gokit/` (request-token verify, `WithHospital`, redaction) moved out of core; `services/emr/` binary; NATS user and subjects; `emr.v1.ctl.health`; the `EMR` stream; compose service; the sealing key pair | 1 | A4 |
| 3 | **Connections, end to end with a stub connector:** migration 1; the seven platform routes; sealing and `put_credentials`; secret store interface (file store and managed store); `emr.v1.ctl.test`; the five audit actions; openapi | 2, H3 for production | A1–A3, A21 |
| 4 | **Egress and SSRF:** `egress-emr` proxy with the generated allow-list; the address checks; the HTTP client limits | 2 | A5, A6 |
| 5 | **Canonical model, normaliser and patient index:** migration 2; `RawSink` and the allow-list; LOINC, ATC and UCUM tables and maps; the patient index and possible matches | 3 | A7–A13 |
| 6 | **FHIR R4 connector:** SMART Backend Services auth; polling, cursor, backfill; test; record link; a synthetic FHIR server on steven-reid with generated synthetic patients | 4, 5 | A18, A20, A25 |
| 7 | **Rules engine and rule set v1:** migration 3; the rule kinds; golden tests; shadow mode; findings and `emr.v1.<hid>.finding`; the dead-letter types | 5, H4 to activate | A24, A26 |
| 8 | **Alerts in core:** migration 4; `core_emr` consumer; settings, routing, fatigue; `/hospital/alert-settings`, `/alerts*`, `/me/notification-settings`; health and staleness; `sources_status`; audit | 7 | A14–A17, A19, A21, A23, A27 |
| 9 | **Retention and Usage:** migration 5; the retention sweep; the `alerts` block of `/hospital/usage` under the small-group rule (when that route is built) | 8 | |
| 10 | **mhai-web (separate repo):** switch Plan 13 screens to live; map `validation_failed`; show `arrived_late`, `preliminary`, `withdrawn`, `sources_status` | 8 | |
| 11 | **Havana, structured (`file_export`, `mhai_csv_v1`):** the format document and synthetic sample; the SFTP connector | 5, H1, H7 | A7, A10 |
| 12 | **Havana, scanned archive (`document_archive`):** the connector to the documents service; the archive's per-integration key and removal listing | documents service, document pipeline phase 1, H2 | A26 |
| 13 | **`pids`:** core's check and the token claim for patient-scoped questions | 5 | |

PRs 2–8 are the backend for the screens mhai-web has built, proven on synthetic FHIR data. PR 11 is the earliest point at which Havana's real data could flow, and only after §17.6.

## 20. What's needed from people

| # | Who | What | Blocks |
|---|---|---|---|
| H1 | Product owner with Havana | A **structured data source** for labs (first), vitals and medications: which system holds them, whether it can export files or speak FHIR, and a synthetic or de-identified sample of its export layout | PR 11; real alerts for Havana |
| H2 | Our team (archive owners) | The archive's per-integration read-only key and removal listing (§5.3) | PR 12 |
| H3 | Product owner, ops | Choose and provision the **managed secret store** (AWS or GCP, in-region) and the emr service's cloud identity | PR 3 in production |
| H4 | Clinical lead, plus a second clinician | **Rule set v1:** critical and urgent thresholds with references, freshness windows, quiet hours, NEWS2 use, likely causes and evidence; review of the code and unit maps; sign-off for names and MRNs in alerts | PR 7 activation; live use |
| H5 | Privacy lead and a Nigerian data protection lawyer | **DPIA**, the processing agreement addendum, the NDPA basis (§17.7), data location, retention values | Any real data |
| H6 | Product owner | **Licences:** LOINC (free, terms to accept), WHO ATC/DDD (needs a licence for commercial use), SNOMED CT and ICD-10 for later | PR 5 |
| H7 | Havana IT | The **network path** (public HTTPS with our egress address allow-listed, or a site VPN) and a contact for credential rotation | PR 11 |
| H8 | Product owner | Answers to §21 | The plan |

## 21. Open questions, with recommendations

1. **What does "Havana first" mean, given Havana's system holds scanned documents only?**
   *Recommendation:* build the pipeline on FHIR R4 with synthetic data first (PRs 2–8), ask Havana now for a structured lab export (H1) and connect it through `file_export` (PR 11) as Havana's first live source. The scanned archive route (PR 12) follows document pipeline phase 1; it is useful for history, not for timely alerts.
2. **Should the source lab's own critical flag raise a critical alert when MHAI's rule doesn't?**
   *Recommendation: yes.* The lab's critical flag is the hospital's own critical-result policy, and it works even for unmapped codes. MHAI's thresholds add to it, never silence it.
3. **May a likely cause carry the confidence word "high"?**
   *Recommendation: no, in v1.* Causes are rule-authored suggestions with evidence; `caution` or `review` only, until the shadow and live data show how often they're right.
4. **How does a hospital exclude a patient who objects?**
   *Recommendation:* the hospital marks it in its own system where it can (a FHIR security label or an export column), and as a fallback a hospital admin can exclude by MRN in a later screen. Until then, by a support request to a platform admin, audited.
5. **Defaults and caps:** levels when a hospital is first connected, and whether urgent alerts are ever capped.
   *Recommendation:* `critical_urgent` for every category, `critical` for vital signs (ward vitals are frequent); cap info at 20 an hour per hospital; never cap critical or urgent, rely on dedup and quiet hours.
6. **Medication codes: ATC only, or RxNorm too?**
   *Recommendation: ATC only.* RxNorm is US-centred; rules need drug classes, which ATC gives. Keep local names for display.
7. **Retention values (§14.2).**
   *Recommendation:* as in the table: raw payloads 7 days, canonical records 13 months, alerts 24 months with patient fields cleared after. The lawyer and the hospitals confirm (H5).
8. **Hospitals with private certificate authorities or VPN-only systems.**
   *Recommendation:* a per-connection pinned CA bundle and `private_network`, both platform-admin-only, added when the first hospital needs them; not in PR 6.
9. **Separate Go module for `services/emr/`, or a second binary in core's module?**
   *Recommendation:* a separate module, with `services/gokit/` for the shared verifier and database helpers, so the emr binary can't import core's internals (auth, sessions, signing keys) by accident.
10. **Should the BFF seal credentials so core never sees them?**
    *Recommendation: yes, later.* Core seals at once in this design; BFF-side sealing (an additive `credentials_sealed` field) is a small mhai-web change worth making before the first real hospital's credentials are entered.
11. **Application-level encryption for patient names and MRNs?**
    *Recommendation: not in v1.* RLS, column grants, disk and backup encryption cover the threat we can name; field encryption would need key sharing between core and emr for every alert read. Revisit with the DPIA.
12. **Do clinicians need email or SMS for critical alerts?**
    *Recommendation:* not from MHAI in v1. The hospital's existing critical-result call process stays primary; email and push come with the notifications service and its own design.
