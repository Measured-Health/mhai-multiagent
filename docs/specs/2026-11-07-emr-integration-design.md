# EMR integration service: reading hospital records and flagging abnormal results

- **Status:** Draft for the product owner (2026-11-07), revised the same day after two design reviews (all findings taken; see §22). The questions in §21 need answers before the plan is written. Nothing here is live until the sign-offs in §17.6 are in place: until then everything runs on synthetic data only.
- **Date:** 2026-11-07
- **Decision recorded in:** [ADR 0009](../adr/0009-integrate-many-emr-systems.md) (Proposed, 17 Oct). This design is how ADR 0009 is built. PR 1 of §19 moves ADR 0009 to Accepted and adds ADR 0014 for the two decisions this design adds to it: a separate `services/emr/` with its own credentials and egress (§3), and the reviewed document route for sources that hold only scanned records (§5.3).
- **Builds on (and must stay consistent with):**
  - the intelligence boundary design, `docs/specs/2026-10-28-intelligence-boundary-design.md`, **as built** (§18): the request token (§7.2, `sub = svc:core` for "EMR events"), the subjects (§5.1), the closed audit payloads, the dead-letter path, NATS users per service (§7.6);
  - the document pipeline design, `docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`: its reviewed records become raw records here and go through the one normaliser (§9.0); this design amends its `records.v1.<hospital>.created` event to `emr.v1.<hid>.fetched` (§3.2);
  - the Plan 4 design, `docs/specs/2026-10-31-plan-4-feedback-audit-documents-admin-design.md`: the typed audit registry, the platform routes and their rules, the generic `jobs` table and the Failed jobs view;
  - the cloud model providers design, `docs/specs/2026-11-03-cloud-model-providers-design.md`: the egress proxy pattern (§6 there) and key files (§7 there);
  - ADR 0004 (one patient's data never reaches another's), ADR 0005 (fail closed), ADR 0010 and 0011 (core owns the API and the rules; NATS only), ADR 0012 (additive changes only in `v1`);
  - mhai-web's contract for the screens it has built on sample data: `docs/specs/2026-10-23-emr-screens-design.md` and `docs/plans/2026-10-23-plan-13-emr-screens.md` (Plan 13), `apps/platform/lib/api/emr-contract.ts`, `apps/clinical/lib/api/emr-contract.ts` and `alerts-contract.ts`, and the frontend requirements §9, §12 and (to be filed) §14.11.
- **Scope:** labs, vital signs and medications, read-only, from hospital record systems, to alerts for clinicians. Conditions, encounters beyond location, analysis (Plan 6) and "Ask MHAI about this patient" use the same model later but are not built here.

## 1. Why

The product owner's requirements (settled): read labs, vitals and medications from each hospital's record system, Havana's first and then FHIR-based systems; find abnormal values and patterns; flag them to clinicians with the evidence, never to patients; the platform admin app configures connections; hospital admins choose units, what to flag and who hears; clinicians get the alerts page, bell, pop-ups and mute that mhai-web has already built.

mhai-web has built every screen on sample data (Plan 13, plus the alerts page, bell and pop-ups). Core serves none of it yet: `services/core/openapi.yaml` (1.10.0) has no `/alerts`, `/hospital/alert-settings`, `/me/notification-settings` or `/platform/emr/*` route. This design says what serves them, and what sits behind them.

### 1.1 What Havana's system actually holds (read 7 Nov)

We read the Havana record system's code (read-only, no configuration or data opened). **It holds no structured clinical data.** It is a search, preview and download service over an archive of scanned paper records kept in a file store, indexed into a search index on a schedule (a deployment setting: a daily ingest with an hourly check by default), served over a REST API (`/api/v1/files/...`) with user sign-in by JWT and one deployment-wide service key.

- **A patient is identified only by a file name.** The index keeps each file's name (the extension stripped so the screen shows the patient's name) and its top-level folder (the box); patient folders below the box are not indexed. There is no patient id or MRN. **File names are PHI** (§5.3).
- **Not only PDFs:** the archive also serves word-processor files, spreadsheets, images (JPEG, PNG, TIFF) and plain text.
- **No per-file "since" listing.** The "since" endpoint returns a ZIP export's part layout, not a list of files; listing by date is a search filter on modification time. Deleted files leave no trace.
- No encounters, wards or units, and no lab results, vital signs or medications as data. Its own product plan lists structured patients, encounters and live lab, imaging and prescription streams as data the hospital has **not yet provided**.
- It is live at Havana, so any change to it (H2) needs Havana's agreement as well as our team's work.

So "Havana first" can mean two things, and this design supports both (§5.3, §21 question 1):

1. **Havana's scanned records, read through the document pipeline.** A connector lists new files (once the archive offers a listing, H2) and hands the supported ones to the documents service. Values reach the canonical model only after a person has reviewed the extraction and matched the patient (document pipeline design §5). Useful, but slow (hours to days), and it depends on document pipeline phase 1 (typed lab reports).
2. **A structured export from Havana's lab, pharmacy or ward systems**, when the hospital provides one. Read through the scheduled file export connector (§5.4) or FHIR R4 if their systems offer it. This is the only route that gives timely alerts. It needs the hospital (H1 in §20).

The FHIR R4 connector is built first as the reference connector, against a synthetic FHIR server, because the whole pipeline can be proven on it end to end while H1 is pending.

## 2. Goals, non-goals, anti-goals

**Goals**
- A separate service, `services/emr/`, that holds every outside credential and the only route out to hospital systems; core holds neither (§3, §6).
- One connector interface; connectors for FHIR R4 (first), Havana's archive (to the document pipeline) and scheduled file export (structured export from Havana or any hospital).
- A canonical model based on FHIR R4, with provenance on every record (§8); normalisation to LOINC and UCUM for labs and vitals, ATC for medications, in one place, the emr service, for connector and document records alike (§9).
- A patient index per hospital, never across hospitals, whose ids are the MHAI patient ids that `pids` will carry (§10).
- A deterministic, versioned rules engine under clinical change control, that raises findings with evidence and suggested causes (§11).
- Alerts served by core, routed by the hospital's settings, with fatigue controls, read state and audit, matching mhai-web's contract (§12, §16).
- Fail closed and never silently: a stale feed, an unmapped code or a failed rule is visible, not a quiet "nothing abnormal" (§15).

**Non-goals**
- Writing back to any record system (ADR 0009 rule 9).
- Conditions, notes, imaging, procedures; the Analysis page (Plan 6); "Ask MHAI" from a record (the `pids` work is prepared in §10.5, not wired).
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
  normaliser ◀── emr.v1.<hid>.fetched (also from core after the document pipeline's finalise_document wrote reviewed raw records)
             └▶ canonical tables + patient index ─▶ records.v1.<hid>.created (ids only)
  rules      ◀── records.v1.<hid>.created
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
| `emr.v1.ctl.<op>` (`test`, `put_credentials`, `wipe_credentials`, `sync_now`, `health`) | request/reply | core (`gateway`) only | emr (queue group `emr`) | a request token (`aud: emr`), ids; `put_credentials` sealed boxes (§6.2) |
| `emr.v1.<hid>.fetched` | JetStream `EMR` | emr (connector records); core (document records after `finalise_document`, §5.3) | emr (`emr_normalise`) | `{raw_ids[]}` |
| `records.v1.<hid>.created` | JetStream `EMR` | emr only (the normaliser) | emr (`emr_rules`) | `{record_ids[]}` (at most 500) |
| `emr.v1.<hid>.finding` | JetStream `EMR` | emr | core (`core_emr`) | `{finding_id}` |
| `emr.v1.<hid>.signal` | JetStream `EMR` | emr | core (`core_emr`) | `{kind, connection_id, ref_id}`: a possible match, a source merge request, a breaker trip, a rejected document value (§10.2, §10.3, §12.4); ids and closed codes only |
| `dead.v1.<hid>.emr_<stage>` | JetStream `DEAD` | emr | core (`core_dead`, existing) | job id, stage, error code |
| `_INBOX_gateway.>` | replies | emr through `allow_responses` only | core | |

- **Ids only in messages.** No name, MRN, file name, value or code is ever in a NATS message (ADR 0009 consequences, SKILLS §7). Every payload is protobuf (`proto/mhai/emr/v1/`), `buf breaking` in CI.
- **The document pipeline design is amended here:** its `records.v1.<hospital>.created` event becomes `emr.v1.<hid>.fetched` for the reviewed raw records (§5.3, §9.0), published by core after `finalise_document` completes. Only the normaliser publishes `records.v1`.
- **Stream `EMR`:** subjects `emr.v1.*.fetched`, `emr.v1.*.finding`, `emr.v1.*.signal`, `records.v1.>`; file storage, `max_age` 7 days, `MaxMsgsPerSubject` 50,000, `DiscardNewPerSubject` (a hospital can't fill the stream for others; a refused publish leaves the row unpublished and the sweeper retries, §15), `Nats-Msg-Id` = the row id so duplicates drop. JetStream encryption at rest on (`NATS_JS_KEY`) before real data, as for `JOBS` (TODO "Demo NATS").
- **NATS user `emr`** (NKey in production): publishes `emr.v1.*.fetched`, `emr.v1.*.finding`, `emr.v1.*.signal`, `records.v1.*.created`, `dead.v1.*.emr_*`, and only its own consumers' `$JS.API.CONSUMER.{INFO,MSG.NEXT}.EMR.emr_*` and `$JS.ACK.EMR.emr_*.>`; subscribes `emr.v1.ctl.> emr` (queue group) and `_INBOX_worker.emr.>`; `allow_responses: {max: 1, expires: "1m"}`. No KV, no `jobs.v1`, no `inference.v1`, no `api.v1`.
- **`gateway` (core)** gains publish `emr.v1.ctl.>` and `emr.v1.*.fetched`, and consumer rights for `core_emr` on `EMR`.
- **Reply subject check:** the emr responder answers only on `_INBOX_gateway.` and the token's `rpl` (TODO "Every NATS responder checks the reply subject").
- The emr service verifies every control request exactly as boundary §7.3 (size, signature, claims, binding of subject and body hash, reply subject, replay, schema, scope, permission), using the same Go verifier and the same `testdata/request-tokens` vectors. Today the verifier derives the audience from the `inference.v1.<service>.<op>` subject shape; **PR 2 generalises the subject-to-audience rule** to a table of allowed shapes (`inference.v1.<service>.<op>`, `emr.v1.ctl.<op>` → `emr`), with new vectors for each shape and for a mismatch. `sub` is the platform admin for `test`, `put_credentials` and `wipe_credentials`, `svc:core` for `health` and `sync_now`. Operations table entries: `emr.test`, `emr.put_credentials` and `emr.wipe_credentials` allow `platform_admin`; `emr.health` and `emr.sync_now` allow `svc:core`.
- **Audit from the emr service:** it has no access to `audit_log`. Anything that must be on the chain (a breaker trip, a source merge request) is an id-only `emr.v1.<hid>.signal` that core consumes and records in its own transaction (§12.6).

### 3.3 The database

The same Postgres cluster, a new schema `emr`, owned by `mhai_migrate`. New roles, following 00003:

- **`mhai_emr`** (NOLOGIN), reached by the login `emr_app` through the wrapper `mhai_emr_app` (NOINHERIT, `WITH INHERIT FALSE`), exactly as `intelligence_app` (boundary §18, departure 2). Inside `WithHospital` it has `SELECT, INSERT, UPDATE` on the `emr` data tables of the current hospital (RLS), `SELECT (id, hospital_id, type, base_url, settings, enabled, version, poll_seconds, credentials_origin)` on `emr_connections`, `SELECT (id, status)` on `hospitals`, and nothing on accounts, sessions, `audit_log`, `clinical_*`, `alerts*` or `jobs`. It can't merge or delete patients (§10.2, §10.6): those are core's definer functions.
- **`mhai_emr_sched`** (NOLOGIN): the scheduler's cross-hospital read of enabled connections' ids and hospital ids only, for choosing what to poll, as `mhai_jobs` does for the sweeper.
- **Core's `mhai_gateway`** reads, under RLS, the columns an alert shows: `emr.patients (id, display_name, mrn, birth_date, sex, status)`, the patient's current location, `emr.findings`, and the evidence rows a finding names. Column grants, never `SELECT *`. It changes patients only through `emr_merge_patients()` and `emr_exclude_patient()` (§10.2, §10.6).
- **`mhai_worker`** (the document pipeline) gets `INSERT` on `emr.raw_records` only, for rows with `source_kind = document` (a `WITH CHECK` policy). Nothing on `emr.patients` (the reviewer's search and patient creation are core's, §10.3) and nothing on the canonical tables (§9.0).
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
| `file_export` | Scheduled file export | `folder` text; `host_key_fingerprint` text; `format` text (`mhai_csv_v1`); `access_key` secret | **Havana, phase H-b** (§5.4) |
| `hl7v2_feed` | Results message feed | `sender_id` text; `shared_key` secret | **Not built** (needs an inbound listener; §7.3). Listed by mhai-web's sample data only; the API does not offer it until built |

**Every URL setting gets the base address's checks** (§6.4): `token_url`, `record_link` and the SFTP host are validated for scheme, shape and address exactly as `base_url`, at save and at connect. A `record_link` must be on the base address's host. A `token_url` may be on another host (common for SMART servers); that host joins the connection's **credential origin** (§6.3).

## 5. Connectors

### 5.1 FHIR R4 (reference connector)

- **Auth:** SMART Backend Services: a signed client assertion (`private_key_jwt`, 5-minute `exp`, unique `jti`) to `token_url`, scope `system/Patient.rs system/Observation.rs system/MedicationRequest.rs system/MedicationStatement.rs system/Encounter.rs system/Location.rs` (or the R4 `.read` forms). The access token is held in memory only, refreshed 60 s before expiry, never stored. The assertion and the token are sent only to hosts in the connection's credential origin (§6.3). Other auth schemes (a static bearer token, mutual TLS) are new connector versions, added when a hospital needs one.
- **Reads (polling, §7):** `Observation?category=laboratory,vital-signs&_lastUpdated=gt<cursor>&_count=200&_sort=_lastUpdated`, `MedicationRequest` and `MedicationStatement` the same way, then `Patient`, `Encounter` and `Location` by reference for what those pages name (batched `_id=` reads, at most 100 ids). Pages are followed through the bundle's `next` link only when its host and base path equal the configured base address.
- **Cursor:** the greatest `meta.lastUpdated` seen, minus a 2-minute overlap for clock skew and late commits; duplicates drop by `(connection, resource type, id, versionId)`.
- **Daily reconciliation:** once a day, outside the hospital's busy hours, the connector re-reads the last 24 hours by `_lastUpdated` and loads anything the incremental polls missed (a commit later than the overlap). A record found only by reconciliation is counted (`reconciled_late`, a metric that alerts above zero for a week) and is evaluated with its real `effective_at`, so the freshness window (§11.4) decides whether it may still alert, marked `arrived_late`.
- **Test:** token request, `GET metadata` (checks `fhirVersion` 4.0.x, that the resources above are supported, and that `_lastUpdated` search and `_sort=_lastUpdated` are declared for each; a server without them fails the test with `unreadable_data`), and `GET Observation?_summary=count` (a count, no record). Closed outcome code (§15.2).
- **Record link:** a template the platform admin sets (`settings.record_link`, e.g. `https://<host>/patients/{id}`), host equal to the base address host, `https` only, `{id}` URL-path-escaped. No template, no link (`emr_link: null`).

### 5.2 FHIR versions and profiles

R4 (4.0.1) only. R5 or R4B servers are refused at test with `unreadable_data`. US Core or IPS profiles are not required; codings are read from every `coding` entry and the best system chosen in normalisation (§9).

### 5.3 Havana's scanned archive (`document_archive`): to the document pipeline

- **What it does:** lists files added since the cursor through a **JSON listing of files added since a time** (which the archive doesn't have yet: H2 adds it), downloads each new file of a supported type within the archive's rate limits (at most 10 downloads a minute), and hands it to the documents service as an upload by `svc:emr` for the hospital. That is all. It produces **no canonical record itself**.
- **File types:** only `application/pdf`, `image/jpeg`, `image/png` and `image/tiff` (what the document pipeline reads) are handed on. Word-processor files, spreadsheets and plain text are skipped and counted (`skipped_type`, by MIME type only); they are listed in the connection's health as a count, never by name.
- **File names are PHI.** The archive names files after the patient. The file name is passed to the documents service as the upload's `source_name`, stored with the document under RLS, and shown **only** on the review screen as a hint to the reviewer. It is never in a NATS message, a log, a span, a metric label, a dead letter, an audit payload or a connection's error list; the connector refers to files by the archive's opaque file id only.
- **Then:** the document pipeline's `ingest_document` extracts, a person reviews every document and matches or creates the patient (§10.3), and `finalise_document` writes the reviewed values as **raw records** (`source_kind = document`, with `document_id` and `review_id`). Core publishes `emr.v1.<hid>.fetched`, and the emr normaliser turns them into canonical rows (§9.0) like any connector's. The rules then see them as any other record.
- **Patient identity:** the archive has no patient id. The file name is **never** used to match a patient automatically.
- **Record link:** none (the archive has no patient page). Alerts from documents carry `emr_link: null` and evidence naming the document date.
- **Freshness:** archive documents are mostly historical. A value whose `effective` time is older than the freshness window (§11.4) is stored but raises no alert.
- **Changes the archive needs (H2: our team's work, with Havana's agreement because it is live there):** a JSON listing of files added since a time, with a stable cursor; a per-integration API key that is read-only, scoped to listing and downloading, separate from the deployment-wide service key, and recorded in its audit log as its own actor; a listing of removed files (today deletions leave no trace).
- **Depends on:** H2, the documents service, document pipeline phase 1, and its clinical sign-off. Until then, this connector type is not offered.

### 5.4 Scheduled file export (`file_export`, format `mhai_csv_v1`)

For a hospital whose lab, pharmacy or ward system can export files but offers no API (the likely Havana route for timely alerts, H1).

- **Transport:** SFTP pull from a folder the hospital exposes (`folder`, host from the base address, key auth with `access_key`). The host key's fingerprint is a setting the platform admin types from what the hospital's IT gave out of band (`host_key_fingerprint`); the connector refuses any other key, at test and on every connection. It is never learned from the first connection. Files are moved to `processed/` after a successful load, never deleted by us.
- **Format `mhai_csv_v1`:** one file per kind (`patients`, `encounters`, `results`, `vitals`, `medications`), UTF-8, RFC 4180, a header row, fixed columns (for example `results`: `source_result_id, source_patient_id, collected_at, resulted_at, status, local_code, local_name, value, unit, ref_low, ref_high, ref_text, flag`). ISO 8601 times with offset; a time without an offset is refused, not guessed (§9.3). We publish the format and a synthetic sample file; the hospital's IT maps their export to it.
- **Each row is a raw record** with its `source_result_id` as source id. A row that changes keeps its id and gets a new version.
- **Limits:** a file at most 50 MB, a row at most 8 KB, at most 200,000 rows a file; anything larger is refused whole with `unreadable_data`.

## 6. Credentials: storage, entry and rotation, kept out of core

### 6.1 Where they live

- **A managed secret store** (H3: AWS Secrets Manager or GCP Secret Manager, in the region that hosts the data), path `mhai/<env>/emr/<hospital_id>/<connection_id>/<field>`. The emr service's cloud identity may read, write and delete under `mhai/<env>/emr/` only; core's identity has no access to that path. Encrypted by the store's managed key.
- **The emr service's sealing private key** (§6.2) is in the same store in production (`mhai/<env>/emr/_sealing/<kid>`), read once at start into memory; core's identity can't read it. Core gets only the public key, as a non-secret file.
- **On the demo** (one host, synthetic data): a file store, `EMR_SECRETS_DIR`, one file per secret (the sealing key included), mode 0400, owned by the emr service's user, on a volume only the emr container mounts. The emr service refuses the file store when `APP_ENV` is `production`.
- **The database holds only metadata:** `emr.credentials (connection_id, field, set_at, secret_version, set_by, origin)`. Never the value, a hash of it, or its length.
- **In memory:** loaded per fetch, held in `Creds`, dropped when the fetch ends; OAuth access tokens cached in memory only.

### 6.2 How a credential gets there without core keeping it

- The platform admin types the secret in the platform app; it goes browser → platform BFF → `api.v1.platform` → core (TLS on NATS in production). That much is unavoidable while core is the only API (ADR 0011).
- **Core seals it at once.** The handler, before anything else touches the body, seals each secret field to the emr service's public key (X25519 sealed box; core has the public key only, from `CORE_EMR_SEAL_PUBLIC_KEY_PATH`). **The sealed plaintext binds its use:** `{connection_id, field, origin, issued_at, value}`, where `origin` is the credential origin of §6.3 as saved in the same request. The emr service opens the box and refuses it unless `connection_id` and `field` equal the request's, `origin` equals the connection's current origin, and `issued_at` is within 60 s, so a box can't be replayed into another connection, another field or a changed address. The plaintext stays in the handler's memory for that request, is zeroed after sealing (best effort in Go), and never reaches a log, an error, a span, the idempotency store or the audit payload. **Sealed boxes are treated as secrets too:** never logged or traced, in core or the emr service.
- Core sends the sealed boxes on `emr.v1.ctl.put_credentials`; the emr service writes the secret store, records the metadata row and answers `{field, set_at}` only.
- **The idempotency store** must not keep a plain hash of these bodies: the routes that carry credentials store an HMAC-SHA256 of the body under a per-deployment key (TODO "The idempotency store keeps a plain SHA-256", done for these routes first) and the reply, which never contains a secret.
- **Request logging:** the body of the three routes that carry credentials is never logged or traced, in core's HTTP and NATS adapters (`httpx` redaction list, with a test).
- **Before the first real hospital's credentials (a gate, §17.6):** the platform BFF seals the fields before they leave the Node server, so core never sees plaintext. An additive `credentials_sealed` field (§21 question 10); mhai-web PR 10.

### 6.3 Rotation

**Credentials are bound to the origin they were entered for.** A connection's credential origin is the set of `scheme://host:port` of its base address and, for FHIR, its `token_url`. It is stored with each credential's metadata (`origin`) and inside the sealed box (§6.2).

- **Changing the origin wipes the credentials.** A PATCH that changes `base_url` or `token_url` (host, port or scheme) makes core call `emr.v1.ctl.wipe_credentials` in the same request: the emr service deletes every secret of the connection from the store, its metadata rows go, `credentials[].set_at` becomes null, `last_test` is cleared, and the connection is switched **off**. The same PATCH may carry new credentials, which are then sealed with the new origin. A Test or a switch-on is refused (409 `not_tested`, or the test outcome `sign_in_refused` with no request sent) until every required secret is set for the current origin. Audit: `emr.connection_changed` with `credentials_wiped: true`.
- **Every outbound use checks the origin:** before a connector sends a credential, a client assertion, an access token or an SFTP key, the HTTP or SFTP client checks the destination's scheme, host and port against the credential's stored origin, and refuses (`sign_in_refused`, nothing sent) on any mismatch. This holds for redirects and `next` links too (§5.1).
- **Replacing a credential** (same origin) is `PATCH /platform/emr/connections/{id}` with only the fields being replaced (step-up). The emr service writes a new secret version, keeps the previous version for 24 hours (so a mistake can be rolled back by an operator), then deletes it. The edit clears `last_test` (Plan 13 ruling); a connection that is on stays on and the next sync uses the new value; a sign-in failure shows as `sign_in_refused`.
- **Schedule:** every 12 months or as the hospital's policy requires, and at once on suspicion or when someone with access leaves. The connection page shows `set_at`; a runbook in `deploy/README.md` covers rotation with the hospital's IT. For FHIR, the key pair is made by an operator on a trusted machine (or issued by the hospital), the public key goes to the hospital's registration, and the private key is typed once into the platform form; it is never stored anywhere else by us. A "generate the key pair in the emr service and show the public key" operation is a later improvement.
- **Switching a connection off** keeps its credentials (so it can be switched on again); **removing a hospital** (the two-person removal flow, contract §14.3) deletes them from the store, audited.
- **The emr service's sealing key pair** rotates yearly: a second public key is published to core, core seals to the newest, the emr service opens with either for 24 hours.

### 6.4 Egress

- The emr service runs on the `backend` network (no route out) plus a dedicated **`egress-emr` proxy** on its own `egress-emr` network, separate from the cloud models' proxy. The proxy allows `CONNECT` to port 443 (and 22 for `file_export`) only for hosts on its allow-list, which is generated from the credential origins of `emr_connections` that are enabled or being tested, and refreshed within 30 s of a change.
- **The proxy is the enforcement point for addresses, and it pins what it checked.** On each `CONNECT host:port` it resolves the name once, refuses if **any** resolved address is private, loopback, link-local, CGNAT, multicast, unspecified, a cloud metadata address, or inside the `backend` or `egress-emr` networks, and then connects to the exact address it checked. There is no second resolution between the check and the connection, so a rebinding DNS answer can't swap in an internal address. The proxy is a small Go program of ours (a few hundred lines, reviewed as security code), not a general-purpose proxy whose resolver we don't control.
- **The emr service also checks early**, for a useful error at save and test: a base address (and every URL setting, §4.1) must be `https` (or `sftp` for `file_export`), at most 500 characters, a DNS name (no IP literal outside development), with no userinfo, query or fragment. This check is in core's save handler too (PR 3), so a bad address is refused at the API.
- **Every CONNECT names its connection.** The emr service sends `Proxy-Authorization: Basic <connection_id>:<token>` on each `CONNECT`, where `token = HMAC-SHA256(K_proxy, connection_id)` and `K_proxy` is a key only the emr service and the proxy hold (the secret store in production, a 0400 file on the demo). The proxy recomputes the token (constant-time compare), refuses a missing or wrong one with 407, and then applies **that connection's** rules only: its hosts and ports from the generated allow-list (keyed by connection id, with its hospital id), and its own declared ranges. A request for connection A can't reach a host or range declared for connection B, even in the same hospital, and a CONNECT without a valid connection id reaches nothing. The connector's HTTP and SFTP clients set the header from the connection being fetched, never from configuration a connector can choose.
- **Private networks:** `private_network: true` (platform admin only, step-up) does not open "any private address". It names the declared VPN CIDRs for that connection (`private_cidrs`, each at most a /24, never overlapping the `backend` or `egress-emr` networks or a metadata range, and never overlapping another hospital's declared ranges: a `EXCLUDE` constraint across connections of different hospitals), and the proxy allows a private address only when it is inside the ranges of the connection named in that CONNECT. It needs the H7 network path.
- **Sizes and time:** a response at most 20 MB (a file at most 50 MB, §5.4), JSON depth at most 64, a page read at most 60 s.
- TLS with system CAs, verification on, no custom CA; a hospital with a private CA is a per-connection pinned CA bundle setting (§21 question 8). For SFTP the pinned host key (§5.4) is the equivalent.

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

`emr.raw_records`: `id` (uuid), `hospital_id`, `source_kind` (`connector`, `document`), `connection_id` (null for documents), `document_id` and `review_id` (required for documents, a `CHECK`), `resource_kind` (`patient`, `encounter`, `location`, `observation`, `medication`), `source_id` (≤ 256), `source_version` (≤ 64), `source_updated_at`, `received_at`, `payload` (jsonb, the **allow-listed fields only**, §14.1), `payload_sha256`, `connector_type` (`document` for documents), `connector_version`, `status` (`pending`, `normalised`, `rejected`), `reject_code`. Unique `(hospital_id, source_kind, coalesce(connection_id, document_id), resource_kind, source_id, source_version)`: a replay or duplicate is a no-op (ADR 0009 rule 7). `payload` is cleared after 7 days (§14.2); the hash stays.

### 8.2 Canonical tables (FHIR R4 shapes, MHAI's own tables)

| Table | FHIR basis | Key columns |
|---|---|---|
| `emr.patients` | Patient | `id` (the MHAI patient id, §10), `hospital_id`, `display_name`, `mrn` (the hospital's own; **nullable**: a patient known only from documents may have none, §10.3), `birth_date` (nullable), `sex` (`female`, `male`, `other`, `unknown`), `origin` (`connector`, `document`), `status` (`active`, `merged_into`, `excluded`), `merged_into` |
| `emr.patient_links` | Patient.identifier | `patient_id`, `connection_id`, `source_patient_id`; unique `(connection_id, source_patient_id)`. Document patients have no link row; their records carry `document_id` |
| `emr.encounters` | Encounter | `patient_id`, `class` (`inpatient`, `outpatient`, `emergency`), `status`, `period_start`, `period_end`, `unit_id`, `bed_label` |
| `emr.units` | Location | `id`, `hospital_id`, `connection_id`, `source_location_id`, `label` (≤ 60), `kind` (`ward`, `clinic`, `emergency`, `other`) |
| `emr.observations` | Observation | `patient_id`, `encounter_id`, `category` (`laboratory`, `vital-signs`), `loinc` (null when unmapped), `local_system`, `local_code`, `local_display`, `value_num`, `value_text`, `unit_ucum`, `unit_source`, `value_si` (in the analyte's canonical unit), `ref_low`, `ref_high`, `ref_text` (the source's range as given), `source_flag` (`L`, `H`, `LL`, `HH`, `A`, `AA`, `N` or null), `status` (`preliminary`, `final`, `amended`, `corrected`, `cancelled`, `entered-in-error`), `effective_at`, `issued_at`, `standardised` |
| `emr.medications` | MedicationRequest / MedicationStatement | `patient_id`, `kind` (`request`, `statement`), `atc` (null when unmapped), `local_code`, `local_display`, `dose_value`, `dose_unit_ucum`, `route`, `frequency_text` (≤ 60), `status` (`active`, `on-hold`, `stopped`, `completed`, `cancelled`, `entered-in-error`), `start_at`, `end_at` |

**Provenance on every canonical row** (ADR 0009 rule 2): `raw_id` (always: document records too go through raw records, §9.0), `source_kind` (`connector`, `document`), `connection_id`, `connector_type`, `connector_version`, `source_id`, `source_version`, `received_at`, `mapping_version` (the code map's version used), `normaliser_version`. A row is never updated in place by a new source version: the new version is a new row, the old one gets `superseded_by`. Rules always read the current version; the evidence of an alert points at the exact versions it used.

**Corrections from the source:** `amended` and `corrected` are new versions; rules re-run and an alert built on the old version is updated or withdrawn (§11.4, §12.5), and the clinician sees the change. `entered-in-error` and `cancelled` mark the row; an alert built on it is withdrawn with reason `source_retracted` (§12.5).

### 8.3 What we do not store

Addresses, phone numbers, email addresses, national ids (NIN, BVN), insurance numbers, next of kin, free-text notes and comments, practitioner names, and any field not on the allow-list (§14.1). `display_name` and `mrn` exist only because an alert must show them (frontend §9, needs sign-off, §17.6).

## 9. Normalisation (one place, not in connectors)

### 9.0 One normaliser, in the emr service, for connectors and documents

There is one normaliser and one set of code and unit tables: the emr service's (Go). Document records reach it as raw records written by `finalise_document` (§5.3), so the document pipeline no longer writes canonical rows itself. Why this side:
- **The connectors are already there.** Most records come from connectors, in the emr service; normalising them anywhere else would add a hop and a second copy of the data per record.
- **One place for clinical maps.** The maps are clinical changes (§9.4); with one loader they are reviewed once, recorded as one `mapping_version`, and can't drift between a Go and a Python copy.
- **The document pipeline keeps its strength:** extraction. Its model proposes a LOINC code and a unit to the reviewer as a suggestion; what the reviewer confirms is written as the raw record's coding, and the emr normaliser treats it like a coding a FHIR source gave (checked against the tables, §9.1).
- **This amends the document pipeline design** (§2 there, phase 1 in §10 there): "normalisation to LOINC and UCUM" becomes "propose codes for review; normalisation by the emr service"; `mhai_worker` gets `INSERT` on `emr.raw_records` only (§3.3). The amendment note goes into that design in PR 1.

### 9.1 Codes

- **Labs and vitals to LOINC.** The normaliser reads every coding the source gave. If one is LOINC (`http://loinc.org`), it is used as given (and checked against the bundled LOINC table: an unknown or deprecated LOINC is treated as unmapped). Otherwise the per-hospital, per-connection map (`local_system`, `local_code` → LOINC) from the reviewed YAML files (§9.4) is used. The maps are held in memory by the emr service, loaded at start; there is no map table in the database, so no runtime role can change a map.
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
- maps live in the repository, `services/emr/maps/<hospital short code>/<connection>.yaml` (document maps under `<hospital short code>/documents.yaml`) and the shared `services/emr/maps/common/`, with a `version`; `CODEOWNERS` requires the clinical reviewer on `services/emr/maps/` and `services/emr/rules/`;
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
- **Nothing merges automatically.** Every link between two MHAI patients, and every merge, is confirmed by a person. The emr service only proposes: it writes an `emr.possible_matches` row (`patient_a`, `patient_b`, `reason` code, `proposed_at`) and an id-only `emr.v1.<hid>.signal` (`possible_match`).
- **What proposes a match:** across connections of the same hospital, an exact match of the normalised hospital MRN **and** birth date **and** sex, with the MRN namespace declared the same for both connections by the platform admin (`settings.mrn_namespace`); a source merge (FHIR `Patient.link` `replaced-by`), only when the target is in the **same connection** (a link to another connection's or system's patient is ignored and counted); and a document reviewer's request to link a document patient to a structured one (§10.3).
- **Who confirms:** a hospital admin, on a "Possible duplicate patients" list (`GET /hospital/patient-matches`, `POST /hospital/patient-matches/{id}/confirm` or `/reject`, two-step session, step-up), which shows the two patients' names, MRNs, birth dates, sources and record counts side by side. The confirm calls core's definer function `emr_merge_patients(match_id)`, which re-checks both patients are in the caller's hospital and active, marks the merged one `merged_into`, moves its links and canonical rows with a `merge_id`, re-points open alerts, and core writes `emr.patients_merged` to the audit chain in the same transaction. A reject is audited as `emr.patient_match_rejected` and the pair is not proposed again for the same reason.
- **Until a person confirms,** the two records stay separate patients. Delta and trend rules then see less history; the alert says which source it came from, and a pending match on an alerted patient is listed first.
- **An un-merge** is an operator job on a hospital admin's written request, audited (`emr.patients_unmerged`), reversing by `merge_id`.
- **Documents:** the patient is matched or created by the reviewer in the document pipeline (§10.3), never automatically.

### 10.3 Patients from documents

- **The search is core's, not the worker's.** The document review screen is served by core (document pipeline design §5). When the reviewer looks for the patient, core calls `emr_patient_candidates(review_id, query)`, a `SECURITY DEFINER` function, `EXECUTE` for `mhai_gateway` only, which checks the caller holds `documents.review` and the review is open and theirs, and returns **at most 10** candidates (`id`, `display_name`, `mrn`, age, sex, sources) for a query of an exact MRN, or a name of at least 3 characters together with a birth year. No listing, no wildcard, no paging. Each search is audited (`emr.patient_searched`: `review_id`, query kind, result count; never the query text). The worker (`mhai_worker`) has **no** grant on `emr.patients`; it never sees the patient index.
- **Who creates a patient:** the reviewer, through core: `POST /documents/reviews/{id}/patient` (`documents.review`, two-step session) calls `emr_create_document_patient(review_id, display_name, mrn, birth_date, sex)`, a core-owned `SECURITY DEFINER` function (`EXECUTE` for `mhai_gateway` only). It checks the review is open and the caller's, refuses an MRN that already belongs to a patient of the hospital (the reviewer must pick that one, or propose a match, §10.2), creates the patient with `origin = document`, records it on the review, and writes `emr.patient_created` (`patient_id`, `document_id`, `review_id`; no name) in the same transaction.
- **Fields:** `display_name` (required, as written on the document), `mrn` (optional: Havana's archive has none), `birth_date` (optional), `sex` (required, `unknown` allowed).
- **The patient on the records:** `finalise_document` writes raw records carrying the `patient_id` the review recorded (picked or created). The normaliser accepts it only if that patient is in the record's hospital and `active`.
- **Rejected values go back to review.** If the normaliser rejects a document raw record (a value or unit it can't parse, a time it can't read, a patient no longer active, a code outside the tables), it does not only count it: it sends an id-only `emr.v1.<hid>.signal` (`document_rejected`, `raw_id`, reason code), and core **reopens the document's review** (`needs_changes`, with the reason code per field) so a person corrects or removes the value. The other values of the document stay pending until the review is finished again, so a document never enters the record half-normalised. A value that is merely unmapped (`standardised = false`) is not rejected; it goes in as "Not standardised" (§9.1).
- **No MRN in the contract:** `AlertPatientOut.mrn` becomes `string | null` (§16.4). mhai-web shows "No MRN recorded". This is a type change for mhai-web, made in PR 10, and live document-sourced alerts wait for it.
- **Linking later, safely:** when a structured source arrives (Havana's export, PR 11), document patients without an MRN are never linked by name. The reviewer, or a hospital admin, proposes a link from the "Possible duplicate patients" list (§10.2), with a search by name and birth date shown to the person; a person confirms; the document history then moves to the structured patient by `merge_id`, and rules see it from then on.

### 10.4 What the index is not

It is not an enterprise master patient index across hospitals, and it never will be (ADR 0009 alternatives). A patient who attends two hospitals is two unrelated MHAI patients.

### 10.5 Patient ids in questions and tokens (`pids`)

This closes the design half of TODO "Patient ids in questions and tokens". The build of "Ask MHAI about this patient" is a later plan; this design fixes the rules it must follow:
- core accepts an MHAI patient id in a request only after `emr_patient_in_hospital(pid)` (a `SECURITY DEFINER` function, `mhai_gateway` only) confirms it belongs to the caller's hospital and is not `excluded` or `merged_into`; otherwise 404 `not_found` (never 403, so existence doesn't leak);
- the checked ids go in the request token's `pids` claim (boundary §7.2); the worker refuses any patient id in a body not in `pids` (check 8);
- EMR-derived knowledge chunks, when they exist, carry this id as `kb_chunks.patient_id`, so `chunk_visible_to()` and ADR 0004 apply unchanged;
- `value_text`, `local_display`, `ref_text`, `frequency_text` and unit strings are text the source controls. The design that first puts them in a model prompt must treat them as untrusted input (prompt injection), as the cloud design treats retrieved chunks (§13.3 C3 there).

### 10.6 Excluding a patient

- **Who:** a hospital admin, `POST /hospital/patient-exclusions {mrn}` or `{patient_id}` (two-step session, step-up, a reason code `objection`, `hospital_policy` or `other`), and as a fallback, an operator running `core emr exclude --hospital <id> --mrn <mrn>` on a written request from the hospital, which calls the same function under a provision-like role (`mhai_emr_admin`) and is audited the same way.
- **What happens at once,** in one transaction through core's definer function `emr_exclude_patient()`: the patient becomes `excluded`; every open alert for it is withdrawn (`withdrawn: "excluded"`); no rule runs and nothing new is stored for it (the normaliser drops its records and keeps only the link).
- **Purge of data already held, within 24 hours:** the exclusion writes a durable row in `emr.purge_requests (id, hospital_id, patient_id, kind, requested_at, due_at, state, held_since, done_at)` in the same transaction (`state = pending`, `due_at` = now + 24 h). A job (`jobs.v1.<hid>.emr_exclusion_purge`) is published for it, and the nightly sweep also picks up every `pending` row past `due_at`, so a lost job, a restart or a dead letter can't drop a purge. The purge deletes the patient's canonical rows, findings, raw payloads and possible matches, and clears the patient columns of its alerts (the alerts keep codes and times for Usage counts), leaving only the `patient_links` row with the source patient id, so future records from the source can be recognised and dropped. Backups age out with the backup retention (35 days); restores re-apply exclusions before the restored data is used.
- **Legal hold:** if the hospital is under a legal hold, the patient is excluded at once (no processing, no alerts) and the purge request becomes `state = held` (`held_since` set; the job checks the hold, never deletes under it). The request stays until the hold is lifted; the sweep then moves every `held` request of that hospital back to `pending` with a new `due_at` of 24 hours, and runs it. A held request older than 90 days is listed to the platform's operators and to the hospital's privacy contact, so it isn't forgotten. Audit: `emr.exclusion_purge_held` and `emr.exclusion_purged`.
- **Audit:** `emr.patient_excluded` (`patient_id`, reason code) and `emr.exclusion_purged` (counts).
- **From the source:** a hospital that can flag the patient in its own system (a FHIR security label, an export column) gets the same effect within one poll cycle.


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
| `score` | NEWS2 from one set of vitals taken within 1 h: total ≥ 7 critical, 5–6 urgent, any single parameter scoring 3 urgent. Computed only when every parameter is present, including consciousness level and whether the patient is on supplemental oxygen; a missing parameter means no score, never "normal". SpO2 Scale 2 (hypercapnic respiratory failure) is used only when the source records it for the patient; otherwise Scale 1, and the alert says which. Both rules are H4 decisions |
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
- **Amended, corrected and retracted results.** When a new version supersedes an observation that a finding used, the rules re-run for that patient and the finding is replaced by its successor:
  - if a rule still hits on the new version, the new finding names the old one (`supersedes`), and core **updates the open alert in place** to the new value, marks it `amended: {previous_value, amended_at}`, and re-notifies if the severity rose;
  - if no rule hits any more, core **withdraws** the alert (`withdrawn: "source_amended"`), and it stays on the list, greyed, saying the result was corrected to the new value;
  - if the observation is `entered-in-error` or `cancelled`, core withdraws the alert (`withdrawn: "source_retracted"`);
  - a handled alert is not reopened, but gets the same `amended` or `withdrawn` mark, so the handler sees the change in their history.
- **Freshness window:** a finding whose `effective_at` is older than the category's window (default 72 h for labs, 6 h for vitals; in the rule set, clinically reviewed) is stored with `status = historical` and **never** becomes an alert. Backfill (§7.1) and re-normalisation always produce `historical` findings. This is what keeps old archive documents and a first sync from flooding clinicians.
- **Late data is not hidden:** a finding inside the window but received more than 1 h after `effective_at` becomes an alert that says when the value was taken (`finding.recorded_at`) and that it arrived late (`arrived_late: true`, §16.4).

### 11.5 Clinical change control for rules

- A rule set change is a `clinical-review` PR (CONTRIBUTING): the YAML diff, the cited references, and the **golden test report**, before and after.
- **Golden tests:** `services/emr/rules/testdata/` holds synthetic patients with timed results, vitals and medications, and the findings each must and must not raise (at least one positive and one negative case per rule, each band edge, missing parameters for NEWS2, unit conversions, preliminary vs final, an amended result, an `entered-in-error` result). CI runs them; a change in any expected finding must be explained in the PR.
- **Rule set v1 itself starts in shadow** at the first live hospital: for at least 14 days it raises no alert, and a clinician reviews every critical finding it would have raised (against the record, inside the hospital) before alerts are switched on. The review's counts go in the activation PR.
- **Shadow first, every later version:** a new rule set version is deployed in `shadow` for at least 7 days on each live hospital: it evaluates the same events, writes findings with `shadow = true`, and raises no alert. A report (counts by rule and severity, old vs new, no patient data) goes in the PR that promotes it. Promotion is a setting change (`EMR_RULE_SET_ACTIVE`) with an audited deployment record.
- **Two people, enforced:** the clinical lead (or a named delegate) and a second clinician approve, as for corrections (ADR 0005). `reviewed_by` in the YAML is a record, not the control: `CODEOWNERS` on `services/emr/rules/` and `services/emr/maps/` requires the clinical reviewers' approval on the PR, and promotion (`EMR_RULE_SET_ACTIVE`) is a deploy change that only the release owner merges, recorded on the audit chain at start-up (`emr.rule_set_activated`, version and hash).
- **Hospitals can't change thresholds.** They choose levels per category (§12.2); that filters delivery, it never changes what is abnormal.

### 11.6 No machine learning in v1

Not justified now: there is no labelled outcome data from these hospitals, a learned score would need its own evaluation and clinical sign-off under the model promotion flow, and an unexplainable score breaks "evidence with every alert". Deterministic rules with cited thresholds can be reviewed line by line, tested exhaustively with synthetic cases, and explained to a clinician. ML enters later only as an **additional** signal with its own design, never replacing a threshold, and only through the model promotion flow (document pipeline design §7–§8).

## 12. Alerts: routing, fatigue and audit (core)

### 12.1 From finding to alert

Core's durable consumer `core_emr` takes `emr.v1.<hid>.finding`, and in one transaction under `WithHospital(hid)`:
1. reads the finding; if `historical` or `shadow`, acks and does nothing; if it `supersedes` another, applies §11.4's update or withdrawal to that alert instead of creating one;
2. checks the hospital is `active` and has a connection that is on; otherwise no alert (the finding stays, audited as `alert.not_raised` with reason code);
3. applies the hospital's **alert settings** (§12.2): the patient's unit is covered, the category's level admits the severity;
4. applies the **fatigue controls** (§12.4);
5. writes `alerts` and one `alert_deliveries` row per recipient, and the audit row `alert.raised`.

A finding that a setting filters out is recorded (`alert.not_raised`, reason `unit_not_covered` or `level_off`), so Usage can count what was filtered and an auditor can see it.

### 12.2 Alert settings (hospital admin)

- `alert_settings (hospital_id, queue boolean, version, updated_by, updated_at)`; `alert_units (hospital_id, unit_id, covered)`; `alert_levels (hospital_id, category, level)`; `alert_recipients (hospital_id, user_id)`.
- **Defaults when a hospital's first connection is switched on:** all units covered, queue on, no named clinicians, levels `critical_urgent` for every category except `vital_signs` at `critical` (open question 5).
- **Unit not recorded:** a patient with no current encounter location is **covered** (fail toward alerting; the alert reads "Location not recorded"). A new unit discovered in the source is covered by default and shown to the hospital admin.
- **Recipients:** the queue, named clinicians, or both; at least one (422 `no_recipients`). Named clinicians must be active users of the hospital with the `clinician` role. If every named clinician is later deactivated and the queue is off, new alerts go to the **queue** anyway and the alert settings page shows the problem (fail toward delivery; never to nobody).
- **A recipient leaves:** when a user is deactivated, loses the `clinician` role, or is removed from the named list, core moves every open (`new`) alert delivered only to them **to the hospital queue** in the same transaction (`queue = true`, whatever the setting), audited as `alert.requeued` (`alert_ids`, reason code). Critical ones are re-notified as queue criticals. Nothing addressed to a person who can no longer sign in stays invisible.
- Saved per section with a version (409 `conflict` on a stale one). No step-up (Plan 13 D8).

### 12.3 Who sees an alert

- **Queue alerts:** every clinician of the hospital (Plan 13 D11). **Named alerts:** the named clinicians. Hospital-admin-only and analyst accounts get none (frontend §9).
- `routing` on each alert, for the caller: `to_you` (a delivery row for them), `queue` (on the queue).
- Pop-ups (in the app, from the BFF's polling of `GET /alerts/unread-count` and `GET /alerts?status=new`): critical always; alerts to you by name when your pop-up switch is on; queue-only urgent and info are listed and counted, not popped up (Plan 13 D11, D12). The mute switch is `alert_popups` in `/me/notification-settings`; it stops pop-ups only.

### 12.4 Fatigue controls

- **One open alert per patient, rule and analyte.** A new hit for the same `(patient, rule_id, loinc)` while an alert is `new` updates that alert (latest value, evidence) and does not notify again, unless the severity rises (urgent → critical), which re-notifies.
- **After an alert is handled,** the same rule for the same patient re-alerts only when the severity is higher, or after the rule's `quiet_hours` (default 24 h for urgent, 6 h for critical, 72 h for info).
- **No cap on critical.** Info alerts are capped at 20 per hospital per hour; past it they are stored, listed and counted, not notified (no pop-up, no bell increment), and the platform's metric shows the cap was hit. Urgent is never capped in this design (open question 5).
- **Circuit breakers, per source and per hospital.** A sudden spike usually means a fault (a miscalibrated analyser, a broken export, a bad map), and a flood hides the real criticals.
  - **Per source (connection, or the hospital's documents):** the emr service keeps each source's baseline (findings per 15 minutes by severity, a rolling 14-day median) and trips when a 15-minute window exceeds both a multiple of the baseline and a floor (starting values 5× and 10 criticals, set by H4). Tripping **pauses that source**: no new fetches and no new findings from it until an operator resumes it; records already fetched stay pending. It sends an id-only `emr.v1.<hid>.signal` (`breaker_tripped`), which core records (`emr.source_paused`) and turns into a page to the on-call owner (H9) and "Connected with problems" for the hospital. **A paused source is `behind` at once** in `sources_status` (§15.4), whatever its last poll time, so the alerts page tells clinicians straight away that results from that source, criticals included, may be missing, and the hospital admin's status shows it. The wording of that notice, and whether the hospital should be asked to check the source's criticals by phone while it is paused, are H4 decisions.
  - **Per hospital:** core counts alerts per hospital per 15 minutes. Above its threshold (starting value 30 criticals, H4), the hospital enters **grouped mode** for 30 minutes: every alert is still created, listed, counted on the bell and kept, **criticals included**, but pop-ups are collapsed into one summary per clinician ("12 critical results in 15 minutes from Main record. Open the alerts list.") instead of one pop-up each, and operators are paged.
  - **Nothing is dropped.** A paused source's backlog is evaluated when it resumes; findings still inside the freshness window alert, marked `arrived_late`. Resuming is a platform action (`POST /platform/emr/connections/{id}/resume`, step-up, audited `emr.source_resumed`).
- **Usage numbers** (frontend §9.1, Plan 13 D15, `by_route`) are counts under the small-group rule; served when `/hospital/usage` is built (§19, PR 9).

### 12.5 Handling

- `POST /alerts/{id}/acknowledge` and `/dismiss {reason, note?}`: first one wins; a second gets 409 `already_handled` with who and when (frontend §9). Dismiss reasons: `not_relevant`, `already_known`, `wrong_patient`, `other`.
- **The dismissal note** (≤ 500 characters) is stored in `alerts.dismiss_note` under RLS, readable by the hospital's clinicians on that alert only, never on the audit chain, in a log or in Usage, and cleared with the alert's patient columns (§14.2). It is not masked: core is Go and the masker is Python, and a masking call to intelligence for every dismissal would make dismissing depend on the pipeline being up. The note is clinical context inside the hospital's own record of the alert, like the alert itself.
- `wrong_patient` dismissals are counted per connection on the platform's health (a matching problem signal) and reviewed weekly.
- **Changed or withdrawn by the source** (§11.4): an amended alert is updated in place with `amended`; a withdrawn alert gets `status = dismissed`, `handled_by: null`, `dismiss_reason: other` (so today's app can show it) and `withdrawn: "source_amended" | "source_retracted" | "excluded"`. It is never deleted. Until mhai-web shows `withdrawn` (PR 10), the app would read it as "dismissed by nobody", which is one reason PR 10 gates live use (§17.6).

### 12.6 Audit (typed registry, closed payloads)

Core writes every row (the emr service has no access to `audit_log`; its events arrive as `emr.v1.<hid>.signal`, §3.2).

| Core action | mhai-web kind (Plan 13; `audit/catalogue.go` row) | When | Payload (ids and codes only) |
|---|---|---|---|
| `emr.connection_added` | `emr_connection_added` | platform route | `hospital_id`, `connection_id`, `type` |
| `emr.connection_changed` | `emr_connection_changed` (detail "Credentials replaced" when `credentials_replaced`) | platform route | `hospital_id`, `connection_id`, `fields_changed[]` (keys only), `credentials_replaced`, `credentials_wiped` (bools) |
| `emr.connection_switched_on`, `emr.connection_switched_off` | `emr_connection_switched_on`, `emr_connection_switched_off` | platform route | `hospital_id`, `connection_id` |
| `emr.connection_tested` | `emr_connection_tested` | platform route | `hospital_id`, `connection_id`, `outcome` (test code) |
| `emr.source_paused`, `emr.source_resumed` | none yet (sent bare) | breaker trip; platform resume | `hospital_id`, `connection_id`, reason code |
| `emr.credentials_deleted` | none yet (sent bare) | hospital removal | `hospital_id`, `connection_id`, `fields[]` |
| `alert_settings.units_changed`, `.rules_changed`, `.recipients_changed` | `alert_units_changed`, `alert_rules_changed`, `alert_recipients_changed` | hospital admin saves | `hospital_id`, counts (`covered`, `levels_changed`, `recipients`), `queue` |
| `alert.raised` | none yet | an alert is created | `alert_id`, `finding_id`, `rule_set_version`, `rule_id`, `category`, `severity`, `routed_queue`, `routed_named` (count), `grouped` |
| `alert.updated` | none yet | an amended result updates an alert | `alert_id`, `finding_id`, `severity` |
| `alert.not_raised` | none yet | a finding filtered | `finding_id`, reason code |
| `alert.acknowledged`, `alert.dismissed` | none yet | a clinician handles one | `alert_id`, `reason` |
| `alert.withdrawn` | none yet | amended, retracted or excluded | `alert_id`, `reason` |
| `alert.requeued` | none yet | a recipient left | `alert_ids[]` (≤ 500), reason code |
| `alerts.read` | none yet | a list page of alerts is served | `alert_ids[]` (the ids served, at most the page limit of 100), `filter` codes; one row per request, so "who saw this patient's alert" can be answered |
| `emr.patient_created` | none yet | a reviewer creates a document patient | `patient_id`, `document_id`, `review_id` |
| `emr.patient_searched` | none yet | a reviewer searches for the patient | `review_id`, query kind (`mrn`, `name_year`), result count |
| `document.review_reopened` | none yet | the normaliser rejected a document value | `document_id`, `review_id`, reason codes |
| `emr.patients_merged`, `emr.patient_match_rejected`, `emr.patients_unmerged` | none yet | a hospital admin decides a match; an operator un-merges | `match_id`, `from`, `to`, `merge_id`, reason code |
| `emr.patient_excluded`, `emr.exclusion_purge_held`, `emr.exclusion_purged` | none yet | exclusion; its purge held by a legal hold; its purge | `patient_id`, `purge_request_id`, reason code, counts |
| `emr.rule_set_activated` | none yet | the emr service starts with a new active version (reported by signal) | `rule_set_version`, `sha256` |

Kinds mhai-web has no words for yet are sent as the bare entry ("can't be displayed"), as Plan 4 does (rulings R21 and R24), and listed as mhai-web follow-ups in PR 10.

## 13. Roles and permissions

### 13.1 Routes (core's permission table)

| Route | Roles | Rule flags |
|---|---|---|
| `GET /platform/emr/connector-types`, `GET /platform/emr/connections`, `GET /platform/emr/connections/{id}` | `platform_admin`, `auditor` | `Platform`, `NeedsMFA` |
| `POST /platform/hospitals/{id}/emr/connections`, `PATCH /platform/emr/connections/{id}`, `PUT /platform/emr/connections/{id}/enabled`, `POST /platform/emr/connections/{id}/resume` | `platform_admin` | `Platform`, `NeedsMFA`, `Write`, `StepUp` |
| `POST /platform/emr/connections/{id}/test` | `platform_admin` | `Platform`, `NeedsMFA`, `Write` (no step-up: it changes nothing, Plan 13 D5; it sends credentials only to their own origin, §6.3) |
| `GET`, `PATCH /hospital/alert-settings` | `hospital_admin` | `NeedsMFA`; `Write` on PATCH |
| `GET /hospital/patient-matches`; `POST /hospital/patient-matches/{id}/confirm`, `/reject`; `POST /hospital/patient-exclusions` | `hospital_admin` | `NeedsMFA`; `Write` and `StepUp` on the POSTs |
| `GET /alerts`, `GET /alerts/unread-count`, `POST /alerts/{id}/acknowledge`, `POST /alerts/{id}/dismiss` | `clinician` | `Write` on the POSTs |
| `GET`, `PATCH /me/notification-settings` | `clinician` | `Write` on PATCH |
| `GET /documents/reviews/{id}/patient-candidates`, `POST /documents/reviews/{id}/patient` | `documents.review` holders (document pipeline design) | `NeedsMFA`; `Write` on the POST; routes of the documents work, listed here because they call this design's functions |

### 13.2 Database grants (summary)

- `mhai_platform`: `emr_connections` `SELECT, INSERT, UPDATE` (config columns); `emr_platform_health()` and `emr_credentials_meta(connection_id)` definer functions (codes, counts, `set_at` only). Nothing in `emr.*` patient tables.
- `mhai_gateway`: `alert_settings`, `alert_units`, `alert_levels`, `alert_recipients` `SELECT, INSERT, UPDATE` (own hospital); `alerts` `SELECT` and `UPDATE (status, handled_by, handled_at, dismiss_reason, dismiss_note)` with a restrictive policy (only from `new`); `alert_deliveries` `SELECT, UPDATE (read_at)` own rows; `user_notification_settings` own row; `emr.units` `SELECT (id, label, kind, connection_id)`; the patient and finding columns in §3.3.
- `mhai_emr_core` (core's `core_emr` consumer, new NOLOGIN role like `mhai_jobs`): `emr.findings` `SELECT`; `alerts`, `alert_deliveries` `INSERT, UPDATE`; settings `SELECT`; its audit actions only.
- `emr_merge_patients()`, `emr_exclude_patient()`, `emr_patient_candidates()`, `emr_create_document_patient()`: core-owned `SECURITY DEFINER` functions (owner `mhai_migrate`, pinned `search_path`, bound to `mhai_current_hospital()`), `EXECUTE` for `mhai_gateway` and `mhai_emr_admin` only. The emr service can't call them.
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
| Findings that never became alerts | 90 days | deleted |
| Alerts: the patient reference and note | 13 months, like the records | `patient_id` and `dismiss_note` cleared; the alert keeps category, severity, codes, times, routing and handling for Usage and audit |
| Alerts: the rest | 24 months | deleted with their deliveries |
| Patients | while any canonical row, finding or alert with a `patient_id` references them (so at most about 13 months after their last record) | deleted |
| Excluded patients | purged within 24 hours (§10.6) | the link row only |
| Audit chain rows | as the audit log (no PHI on the chain) | |
| Credentials | while the connection exists | deleted at hospital removal |
| Database backups | 35 days (the operations baseline's value, to confirm) | expired by the backup system; a restore re-applies deletions and exclusions made since the backup before the data is used |

- A **legal hold** (contract §14.8.1) on a hospital stops every deletion for it.
- **Switching a connection off** stops polling; data already held follows the table above.
- **Hospital removal** (two-person flow) deletes the hospital's `emr` schema rows, its alert tables in `public` (`alerts`, `alert_deliveries`, `alert_settings`, `alert_units`, `alert_levels`, `alert_recipients`) and its credentials, as one audited job.
- A sweep job (`emr_retention`, `jobs.v1.global.emr_retention`) runs nightly with a cap per run; it is the retention plan's first user.

### 14.3 Logs, traces and metrics

Ids, codes and counts only. No name, MRN, value, code display text, URL path containing a source patient id, or credential. Connector HTTP errors are logged by status code and closed outcome code only, never the response body (it can carry PHI). **Database errors are logged by SQLSTATE and constraint name only:** a unique-violation's detail echoes key values (a source patient id can be an MRN), so `pgconn.PgError.Detail` and `Message` are never logged in the emr service or in core's new code. File names from the archive are never logged (§5.3). A test runs a full synthetic sync and greps every log record and span for the synthetic names, MRNs and values.

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
| **Feed stale** (the per-type rule in §15.4) | scheduler | `health = errors`; the hospital's records status is "problems" | all three; never a silent empty alerts list |
| Rules engine error, invalid rule set | rules consumer | that hospital's evaluation stops (no partial run); messages are not acked, so they wait; metric alert to on-call | platform health; clinicians get the out-of-date notice for that hospital |
| Normaliser or rules message fails 5 times | JetStream | `dead.v1.<hid>.emr_normalise` or `emr_rules`; core's Failed jobs view (types `emr_normalise`, `emr_rules`), retry from there | platform |
| Core down | `core_emr` not consuming | findings wait in `EMR` (7 days); on recovery core raises alerts for those still inside the freshness window, marked late | on recovery |
| emr service down | core's health sampler (`emr.v1.ctl.health`) | the source status is computed from `last_sync`; stale after the limit | as "feed stale" |
| Secret store unavailable | fetch | no fetch; `sign_in_refused` is **not** used; the connection shows `unreachable` and a metric alerts | platform |
| Clock skew (source times in the future) | normaliser | refused, `unreadable_data` | platform |
| A patient link conflict (two MHAI patients claim one source id) | unique index | the insert fails, the record is rejected, a metric alerts with the constraint name only (§14.3); never linked to the wrong patient | platform |
| A source floods findings | per-source breaker (§12.4) | the source is paused; operators paged; backlog kept | platform, on-call; hospital admin: "Connected with problems" |
| A hospital floods alerts | per-hospital breaker (§12.4) | grouped mode: every alert kept and counted, pop-ups collapsed into a summary; operators paged | clinicians see the summary |
| A named recipient deactivated | core, on the user change | their open alerts move to the queue (§12.2) | the hospital's clinicians |
| A hospital deactivated | everywhere | polling stops; findings raise no alerts (§12.1 step 2) | |

### 15.3 Never alert on stale data silently

- A finding outside the freshness window never becomes an alert (§11.4).
- An alert inside the window that arrived late says so (`arrived_late`) and shows the time the value was taken.
- An amended result updates or withdraws its alert, visibly (§11.4).
- An empty alerts list is only shown as "no alerts" when every source of the hospital is current; otherwise the list carries `sources_status` (§16.4) and the app says which source is behind. **`sources_status` is required for live use:** core always sends it, and live use waits for mhai-web to show it (§17.6).

### 15.4 When a source counts as stale

Defined per connection type, so a quiet source isn't confused with a broken one:

| Type | Stale when | Why |
|---|---|---|
| `fhir_r4` | no successful poll for 3 × `poll_seconds` (6 minutes by default) | a poll succeeds even when there is nothing new; silence means a fault |
| `file_export` | no successful folder check for 3 × `poll_seconds`, **or** no new file for longer than the hospital's declared export interval plus 50% (`settings.expected_every_minutes`, required for this type) | the export is scheduled; a missing file is a fault on the hospital's side |
| `document_archive` | no successful listing for 3 × `poll_seconds` | documents arrive irregularly (days apart), so the age of the newest file is never used; and archive values rarely alert anyway (§5.3) |

`sources_status.status` is `behind` when stale **or paused by the breaker** (§12.4), `current` otherwise; `last_update_at` is the last successful poll, not the newest record.

## 16. API contract (additive, `openapi.yaml` minor bump)

All additive under ADR 0012: new routes, and new optional fields, with one exception listed in §16.4 (`mrn` becomes nullable). Error codes are core's (`validation_failed` 422, `conflict` 409, `not_found` 404, `forbidden` 403, `step_up_required`), plus new codes `not_tested` (409), `no_recipients` (422), `already_handled` (409), `hospital_removed` (409: a connection write for a removed or removal-pending hospital) and `no_type` (422: a connection without a known type), the last two as mhai-web's mock already uses them. mhai-web's mock uses 422 `invalid`; it maps `validation_failed`, as Plan 4c's follow-up already records.

### 16.1 Platform (matches `apps/platform/lib/api/emr-contract.ts`)

| Method and path | Request | Reply |
|---|---|---|
| `GET /platform/emr/connector-types` | | `{types: ConnectorType[]}` (§4.1; only built types) |
| `GET /platform/emr/connections?hospital=&state=all\|needs_attention\|off\|not_connected` | | `ConnectionsOut {connections: EmrConnectionSummary[], not_connected: Person[]}` |
| `POST /platform/hospitals/{id}/emr/connections` | `ConnectionIn` + step-up; `Idempotency-Key` | `EmrConnection`, saved **off** |
| `GET /platform/emr/connections/{id}` | | `EmrConnection` |
| `PATCH /platform/emr/connections/{id}` | `ConnectionPatch` + step-up (`version`) | `EmrConnection`; clears `last_test`; 409 `conflict` |
| `PUT /platform/emr/connections/{id}/enabled` | `{enabled, version}` + step-up | `EmrConnection`; 409 `not_tested` when switching on without a passing test of the saved settings |
| `POST /platform/emr/connections/{id}/test` | `Idempotency-Key` | `EmrConnection` with `last_test {at, ok, code}`; `sign_in_refused` with nothing sent when a required secret isn't set for the current origin (§6.3) |
| `POST /platform/emr/connections/{id}/resume` (new) | `{version}` + step-up | `EmrConnection`; resumes a source the breaker paused |

- `type_label` is the type's label (the platform app may show it, Plan 13 constraints); `source_label` 1–40 characters, no vendor or product name (checked against a list, as mhai-web's `FORBIDDEN_TERMS`); `base_url` https (sftp for `file_export`), ≤ 500; each credential 1–4,000.
- `credentials: [{key, set_at}]`; never a value. `errors`: at most 5, last 24 h, closed codes `unreachable`, `sign_in_refused`, `timed_out`, `unreadable_data`, `unmapped_codes`, `too_many_requests`.
- `health`: `never` (no sync yet), `ok`, `errors` (any error in the last 3 runs, or stale).
- `last_sync {at, ok, records}`; `errors_24h` sum.
- Additive optional fields: `poll_seconds`, `settings.record_link`, `settings.mrn_namespace`, `settings.expected_every_minutes`, `private_network`, `private_cidrs`, `paused` (breaker). The app ignores them until it shows them.
- A PATCH that changes `base_url` or `token_url` returns the connection switched off with every `credentials[].set_at` null (§6.3).

### 16.2 Hospital (matches `apps/clinical/lib/api/emr-contract.ts`)

- `GET /hospital/alert-settings` → `AlertSettingsOut {connection: {status, sources[{label, status, last_update_at}]}, units[], categories[], recipients {queue, clinicians[]}, updated_by, updated_at, version}`. Status only: no address, type or error code (Plan 13 D7). `connection.status` is the worst of the sources; `not_connected` when none.
- `GET /hospital/patient-matches` → `{matches: [{id, reason, a: PatientCard, b: PatientCard, proposed_at}]}` where `PatientCard` is `{id, full_name, mrn: string | null, age, sex, sources: string[], records: number}`; `POST /hospital/patient-matches/{id}/confirm {keep: "a" | "b"}` or `/reject`, step-up, `Idempotency-Key`. New routes; mhai-web builds the screen (PR 10).
- `POST /hospital/patient-exclusions {mrn} | {patient_id}, reason` → 202 `{patient_id, purge_by}`; step-up; 404 when no such patient in the hospital. New route; mhai-web screen in PR 10.
- `PATCH /hospital/alert-settings` with exactly one of `units {covered[]}`, `rules {levels}`, `recipients {queue, clinician_ids[]}` plus `version`; `Idempotency-Key`. 422 `validation_failed` (unknown ids, a level outside the list, two sections), 422 `no_recipients`, 409 `conflict`.

### 16.3 Clinicians (matches `alerts-contract.ts` and frontend §9)

- `GET /alerts?status=&severity=&source=&sent_to=you|queue&cursor=&limit=` → `AlertPageOut {alerts[], next_cursor, sources[], queue}`.
- `AlertOut` fields as the contract: `finding` (`name`: the display label from LOINC, or the source's own when unmapped, `value` as recorded, `unit` display, `flag` (`FindingFlag`: source or rule `L`/`LL` → `low`; `H`/`HH` → `high`; `A`/`AA` → `abnormal`; `N` or none → null; severity carries critical vs urgent), `reference_range` as given, `recorded_at`, `code_system` (`loinc` or `local`), `standardised`), `likely_causes[{label, confidence, evidence[]}]`, `patient {full_name, mrn, location}`, `age`, `sex`, `source_label`, `emr_link`, `status` (`new`; `handled` means acknowledged; `dismissed`), `handled_by`, `handled_at`, `dismiss_reason`, `created_at`, `category`, `routing`. `likely_causes[].confidence` is `caution` or `review`; `high` exists in the contract but is never sent in v1 (§11.3).
- `GET /alerts/unread-count` → `{count}`: new alerts that are yours by name or on the queue, unhandled.
- `POST /alerts/{id}/acknowledge`, `POST /alerts/{id}/dismiss {reason, note?}`: `Idempotency-Key`; 409 `already_handled` with `AlertHandledOut`.
- `GET`, `PATCH /me/notification-settings {alert_popups, correction_popups}`.
- Another hospital's alert id, or an alert not routed to the caller: 404.

### 16.4 Additive fields mhai-web doesn't use yet

Each is optional and ignored until mhai-web shows it; showing them is mhai-web PR 10, and **live use waits for PR 10** (§17.6):
- `AlertOut.arrived_late: boolean`, `AlertOut.preliminary: boolean`, `AlertOut.amended: {previous_value, amended_at} | null`, `AlertOut.withdrawn: "source_amended" | "source_retracted" | "excluded" | null` (until shown, a withdrawn alert reads as "dismissed by nobody");
- `AlertPageOut.sources_status: [{label, status: "current" | "behind", last_update_at}]`, always sent, so the alerts page can say "Main record last updated 3 hours ago" instead of an empty list;
- `AlertPageOut.grouped: {since, count} | null` and the per-clinician summary pop-up for grouped mode (§12.4);
- `ConnectorType` gains `document_archive` and `file_export` as data (no screen change; Plan 13 D4).

**One type change (not additive for mhai-web's types):** `AlertPatientOut.mrn` becomes `string | null` for patients known only from documents (§10.3). Core's `openapi.yaml` declares it nullable from the first version that serves `/alerts`, so the API itself never narrows; mhai-web's hand-written type widens in PR 10, and alerts for document-only patients are not raised until then (a server setting, `EMR_ALLOW_NULL_MRN`, off by default).

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
| E3 | Information disclosure / tampering | **SSRF**: a base address that points at our internal services or a cloud metadata endpoint | scheme and shape rules on every URL setting, in core and emr; **the proxy resolves once, checks every address and connects to the address it checked** (no DNS rebinding window); the host allow-list on its own network; no redirects across hosts; `private_network` limited to declared VPN CIDRs, by platform admin with H7; SFTP host key typed from the hospital, never learned | A5, A6, A29 |
| E4 | Tampering | **Hostile source data**: huge bundles, deep JSON, zip bombs, CSV formula injection, HTML in display text, a `next` link to another host | size, depth and row caps; the allow-list; strict decimal and time parsing; display text length-capped and stripped of control and markup characters (the app renders text only); `next` links on the base host only | A7, A8 |
| E5 | Tampering (clinical) | **Wrong values raise wrong alerts**: unit mix-ups, a guessed code, a wrong time zone | no guessing anywhere (§9); per-analyte reviewed conversions; unknown means unmapped; clinical review of maps and rules; golden tests; shadow runs | A9, A10 |
| E6 | Information disclosure | **Wrong patient**: two patients linked, or a record attached to the wrong patient | links by source id within a connection; **nothing merges automatically**: cross-connection matches and source merges are proposals a hospital admin confirms, audited by core; source merges only within one connection; documents matched by a reviewer; file names never used to match; `wrong_patient` dismissals monitored | A11, A12, A31 |
| E7 | Information disclosure | **Cross-hospital leakage** through the index, a finding, an alert or a NATS subject | RLS forced on every table; `(hospital_id, id)` foreign keys; no cross-hospital join (static test); core re-reads a finding under the subject's hospital and refuses a mismatch; 404 for foreign ids | A13, A14 |
| E8 | Elevation of privilege | **A hospital admin raises or hides alerts** beyond their choice, or names a clinician of another hospital | levels filter only; thresholds in the repository; recipients must be active clinicians of the same hospital (checked in SQL); a no-recipient state falls back to the queue | A15, A16 |
| E9 | Elevation of privilege | **A clinician sees alerts not routed to them** or acknowledges another hospital's alert | `alert_deliveries` and queue visibility checked in the query; restrictive policy on `alerts` updates; 404 | A17 |
| E10 | Denial of service | **Alert flood** (first sync, a bad map, a source replay, a noisy rule) hides the critical ones | backfill and historical findings never alert; idempotency by source id and version; dedup and quiet hours; info cap; shadow runs for new rules; per-hospital stream limits; **per-source breaker** pauses a spiking source and pages operators; **per-hospital grouped mode** keeps every critical but collapses pop-ups | A18, A19, A32 |
| E11 | Denial of service | **We overload a hospital's system** | one fetch per connection; page and record caps per run; `Retry-After` honoured; minimum poll interval | A20 |
| E12 | Repudiation | **Nobody can show why an alert was raised or who changed the settings** | provenance on every row; `rule_set_version`, `rule_id`, `mapping_version` on findings; typed audit actions for every connection, setting and handling | A21 |
| E13 | Information disclosure | **PHI in NATS, logs, traces, dead letters, metrics** | ids only in messages; JetStream encryption at rest; log and span redaction; connector errors by status code only; the synthetic-sync grep test | A22 |
| E14 | Tampering (safety) | **Silent gap**: the feed stops, or the rules stop, and the app shows "no alerts" | staleness detection, `health = errors`, the out-of-date notice (`sources_status`), metric alerts, rules errors stop and wait instead of skipping | A23, A24 |
| E15 | Spoofing | **A compromised connector or emr host** writes forged records for another hospital | `WithHospital` per connection's hospital; RLS `WITH CHECK`; the emr role can't touch core's tables; core takes the hospital from the subject and re-checks it | A13 |
| E16 | Information disclosure | **The record link** sends the clinician to a phishing page or carries an injected URL | the link template's host must equal the connection's host; https only; the source patient id path-escaped; `emr_link` built only in core from stored parts | A25 |
| E17 | Elevation of privilege | **An unreviewed document value raises an alert** | document records reach raw records only through `finalise_document` after review, and canonical rows only through the emr normaliser; `source_kind = document` raw rows need a `review_id` (`CHECK`); `mhai_worker` has no grant on the canonical tables | A26 |
| E18 | Information disclosure / spoofing | **Credentials sent to a new host:** a platform session changes `base_url` or `token_url` and presses Test, which needs no step-up | credentials bound to their origin (in the metadata and inside the sealed box); a change of origin wipes them and switches the connection off; every outbound use checks the destination against the origin; `token_url` and every URL setting get the address checks | A28 |
| E19 | Tampering (safety) | **A stale value shown as current:** an amended or retracted result leaves its alert unchanged | supersession re-runs the rules; the alert is updated in place (`amended`) or withdrawn (`source_amended`, `source_retracted`), visibly | A30 |
| E20 | Denial of service (safety) | **An alert addressed to nobody:** its only named recipient is deactivated | open alerts move to the queue on the user change, audited; new alerts fall back to the queue | A33 |
| E21 | Information disclosure | **Data kept after a patient objects** | exclusion withdraws alerts at once and purges held data within 24 hours (legal hold aside); the source link is kept only to drop future records | A34 |
| E22 | Information disclosure | **Who saw a patient's alert can't be shown** | `alerts.read` records the alert ids served | A21 |
| E23 | Tampering | **A replayed sealed box** puts one connection's secret into another, or into a changed origin | the box binds connection, field, origin and issue time; the emr service checks all four | A35 |

### 17.4 Controls (decided)

- **Authentication and step-up:** platform admins with two-step session and a fresh code for every connection change; hospital admins with a two-step session for alert settings; clinicians as today. Service calls use request tokens; the emr service has its own NATS user and database login.
- **Authorisation:** core's permission table (§13.1); RLS on every new table, forced; column grants; the emr service never decides a recipient; core never holds an outside credential.
- **Data handling:** the allow-list (§14.1); ids only in messages; retention (§14.2); no PHI in logs; the masked dismissal note; patient names and MRNs only on alert reads.
- **Concurrency:** one fetch per connection (advisory lock); idempotent raw and canonical writes (unique source id and version); one finding per dedup key; first handler wins on alerts (`UPDATE ... WHERE status = 'new' RETURNING`); settings saved with `version`; a merge locks both patient rows (`SELECT ... FOR UPDATE` in id order) and re-checks both are active; the re-queue on deactivation runs in the same transaction as the user change; a credential wipe and the origin change commit together (core calls `wipe_credentials` before committing, and fails the PATCH if the wipe isn't confirmed).
- **Secrets:** a managed store in production, a 0400 file store on the demo only; sealed in transit through core; rotation per §6.3; the emr service refuses to start without its sealing key, and with a placeholder or empty secret in a connection that is on (it marks that connection `sign_in_refused` rather than calling out with nothing).
- **Third parties and hosting:** the hospitals' systems (each under the hospital's data agreement, §17.7); the secret store provider (in-region); no other processor sees the data. No cloud model receives EMR data in this design.

### 17.5 Abuse-case tests (the plan writes these first)

- **A1** A credential sent through `POST` and `PATCH` never appears in any log record, span, error, idempotency row, audit payload, NATS message (other than the sealed box) or reply; the reply has `set_at` only.
- **A2** As `mhai_platform`, `mhai_gateway` or core's cloud identity, no query or API call can read a credential value.
- **A3** A connector that tries to log `Creds` fails the AST test; `fmt.Sprint(creds)` is redacted.
- **A4** A control request with a token for another subject, another body, `aud pipeline`, an expired `exp`, a replayed `jti`, or a reply subject outside `_INBOX_gateway.` is refused with no work done; `put_credentials` with a `svc:core` subject is refused.
- **A5** Base addresses and `token_url`s `https://10.0.0.5`, `https://169.254.169.254`, `https://localhost`, `https://user:pw@host`, `http://host`, and a DNS name that resolves to a private address are refused at save (core and emr) or at connect (the proxy).
- **A6** Through `egress-emr`: `CONNECT` to a host not on any enabled or tested connection is refused; a redirect to another host is not followed; with `private_network` and declared CIDRs, an address outside them (or in `backend`) is refused.
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
- **A28** PATCH `base_url` (or `token_url`) to a new host, then Test: no credential, assertion or token leaves (a capturing fake on the new host receives nothing); the connection is off with every `set_at` null; Test reports `sign_in_refused` until new secrets are entered; the audit row says `credentials_wiped`.
- **A29** DNS rebinding: a test resolver answers a public address to the emr service's early check and `169.254.169.254` (then a `backend` address) to the next query; the proxy, which resolves once and connects to what it checked, never connects to the internal address; an SFTP server with a host key other than the typed fingerprint is refused at test and at sync.
- **A30** A critical potassium alert whose result is amended to a normal value is withdrawn with `source_amended` and shows the new value; amended to another critical value, it is updated in place with `amended`; a handled alert gets the mark without reopening; `entered-in-error` withdraws with `source_retracted`.
- **A31** A source `replaced-by` link, and an exact cross-connection MRN match, each create a proposal and change nothing until a hospital admin confirms; the confirm is audited by core; a `replaced-by` pointing at another connection's patient is ignored; a hospital admin of another hospital can't see or confirm the match (404).
- **A32** A source that sends 200 critical results in 15 minutes against a baseline of 2 is paused after the threshold, operators are paged, every finding already made is still an alert, and nothing more is fetched until resume; at the hospital threshold, every critical is listed and counted but each clinician gets one summary pop-up.
- **A33** A named clinician with three open alerts (queue off) is deactivated: all three move to the queue in the same transaction, the criticals re-notify, and `alert.requeued` is written; the same when the clinician role is removed.
- **A34** Excluding a patient withdraws its open alerts at once; within the purge job, its canonical rows, findings, raw payloads and possible matches are gone and its alerts' patient columns are null; new records from the source are dropped; under a legal hold, exclusion happens and the purge waits.
- **A35** A sealed box replayed with another connection id, another field, after an origin change, or older than 60 s is refused by the emr service and nothing is stored.
- **A36** An archive listing with a word-processor file and a spreadsheet hands neither to the documents service; no file name appears in any log, span, message, dead letter or audit payload.
- **A37** Two hospitals, A and B, each with a private-network connection and its own declared range: a CONNECT carrying connection A's credential to an address in B's range, or to a host of B's connection, gets 403 from the proxy; a CONNECT with no `Proxy-Authorization`, a forged token, or connection A's id with B's token gets 407; saving a range for A that overlaps B's is refused by the database.
- **A38** As `mhai_worker`, `SELECT` on `emr.patients` fails; `emr_patient_candidates` returns at most 10 rows, refuses a 2-character name or a name without a birth year, refuses a caller who isn't the review's reviewer, and writes `emr.patient_searched` without the query text.
- **A39** `emr_create_document_patient` for a closed review, by another reviewer, or with an MRN that already exists in the hospital is refused; a successful call writes `emr.patient_created` in the same transaction.
- **A40** A document raw record with an unparseable value reopens its review with the reason code, and none of that document's values reach the canonical tables until it is finished again.
- **A41** An exclusion under a legal hold leaves a `held` purge request; lifting the hold makes the sweep purge within 24 hours; deleting the job message (or dead-lettering it) still purges at the next sweep.
- **A42** A breaker trip turns that source's `sources_status` to `behind` in the next `GET /alerts`, even though its last poll was a minute ago.
- **A20** A source answering 429 with `Retry-After: 120` gets no request for 120 s; at most 20 pages are read per run.
- **A21** Every connection change, settings save, raised, filtered, acknowledged, dismissed and withdrawn alert has its audit row with a valid closed payload; an unknown key is refused.
- **A22** A full synthetic sync, rule run and alert cycle leaves no synthetic name, MRN or value in any log, span, metric label, NATS message or dead letter.
- **A23** A source silent for 3 poll intervals turns `health` to `errors`, the hospital status to "problems" and `sources_status` to `behind`.
- **A24** A rule set that throws on one patient stops evaluation for that hospital, leaves the messages unacked, raises the operator alert, and makes no partial findings.
- **A25** A record link template on another host, `javascript:`, or a source patient id containing `../` or `?` produces no link or an escaped one.
- **A26** A canonical row with `source_kind = document` and no `review_id` is refused by the database.
- **A27** No route, rule, template or message can address a patient: there is no patient recipient type, and a test asserts the alert recipients are user ids of clinicians only.

### 17.6 Residual risk and sign-off

- **Gates before live use (all of them):**
  - mhai-web PR 10 shipped: `sources_status` shown (so a down feed is never an empty "no alerts"), and `arrived_late`, `preliminary`, `amended`, `withdrawn`, grouped mode and the nullable MRN;
  - PR 9 (retention and the exclusion purge) deployed;
  - BFF-side sealing of credentials (§6.2) before the first real hospital's credentials are entered;
  - rule set v1 run in shadow with every critical finding reviewed (§11.5);
  - an on-call owner for the operator alerts and breaker pages (H9);
  - the sign-offs below.
- **Clinical sign-off (before live use):** the rule set v1 (thresholds, freshness windows, quiet hours, NEWS2 use, likely causes and their evidence), the code and unit maps per hospital, and showing patient names and MRNs in alerts (frontend §9, mhai-web Plan 5a). H4.
- **Privacy and legal (before any real data):** the DPIA (including the breadth of the queue), the hospital data processing agreement addendum covering EMR reading and alerting, the hospital's written confirmation that its own critical-result call process stays primary, the NDPA 2023 basis (§17.7), data location and the retention table. H5.
- **Accepted for now:**
  - until BFF-side sealing ships, core sees credentials in memory for the length of one request (synthetic credentials only until then);
  - every clinician of a hospital sees the names and MRNs of every queue alert (Plan 13 D11); the DPIA must weigh this (H5);
  - patient names and MRNs are stored in clear columns protected by RLS, column grants and disk encryption, not application-level encryption (open question 11);
  - a compromised emr host can read every connected hospital's credentials and records; isolation per hospital (one emr process per hospital) is not in this design;
  - polling adds up to two poll intervals of delay; a critical result is not instant;
  - a clinician may still miss an alert (bell and pop-ups only, no phone or email) until the notifications service exists. Critical alerts must not rely on MHAI alone: the hospital's own critical-result call process stays in place, and the sign-off says so.

### 17.7 Consent and legal basis (NDPA 2023)

To be confirmed by a qualified lawyer (H5); section numbers as we read the Act:
- **Roles:** the hospital is the data controller; Measured Health is its data processor, acting on the hospital's written instructions under a processing agreement (s. 29). The agreement names this purpose (reading labs, vitals and medications to flag abnormal results to the hospital's clinicians), the data categories (§14.1), retention (§14.2), sub-processors (hosting, the secret store) and the location of processing.
- **Lawful basis:** health data is sensitive personal data (s. 30). The basis is the hospital's provision of healthcare by health professionals under a duty of confidentiality, and the patient's vital interests for critical results, not patient consent. Consent is not used as the basis, because it would have to be freely withdrawable and the clinical safety purpose would then depend on it.
- **Transparency and objection:** the hospital's privacy notice covers MHAI; a patient who objects is excluded (§10.6): alerts are withdrawn at once and data already held is purged within 24 hours.
- **DPIA** (s. 28): required before live data (health data, systematic processing, automated flagging). We draft it; the hospital's DPO approves it.
- **Data minimisation and storage limitation** (s. 24): §14.
- **Erasure and legal holds:** an objection or erasure is carried out within 24 hours (§10.6), except while the hospital is under a legal hold, when processing stops at once but deletion waits for the hold to end (the Act's exceptions for establishing, exercising or defending legal claims and for legal obligations; counsel to confirm the sections and whether the patient must be told of the delay). The pending request is kept and the purge runs when the hold lifts.
- **Security and breach** (s. 39, s. 40): §17.4; a breach is reported to the hospital at once so it can notify the Commission within 72 hours.
- **Cross-border transfer** (ss. 41–43): if the hosting region or the secret store is outside Nigeria, the transfer needs a basis the lawyer confirms. Recommendation: host EMR data in-region where possible (H5).
- **No automated decisions about the patient** (s. 37): alerts go to a clinician who decides; nothing acts on the patient automatically.

## 18. Migrations

Numbers are taken at build time from the next free one (00018 is the model policy; Plan 4 and the cloud models plan may take more). In order:

1. **`NNNNN_emr_roles.sql`:** schema `emr`; `private_cidrs` with an exclusion constraint against other hospitals' ranges; roles `mhai_emr`, `mhai_emr_app`, `mhai_emr_sched`, `mhai_emr_core`, `mhai_emr_admin`; `emr_connections` (in `public`, config only: `id`, `hospital_id`, `type`, `source_label`, `base_url`, `settings jsonb` with a `CHECK` that no secret key name is present, `credentials_origin text[]`, `enabled`, `paused`, `private_network`, `private_cidrs cidr[]`, `poll_seconds`, `last_test_at`, `last_test_ok`, `last_test_code`, `version`, `updated_by`, `updated_at`); `emr.credentials` (metadata, with `origin`); `emr.sync_runs` and `emr.connection_errors` (codes and counts); RLS, grants; audit append policies for the `emr.connection_*` and `emr.source_*` actions.
2. **`NNNNN_emr_canonical.sql`:** `emr.raw_records` (with `source_kind`, `document_id`, `review_id` and the `CHECK`), `emr.patients` (nullable `mrn` and `birth_date`, `origin`, `merged_into`), `emr.patient_links`, `emr.possible_matches`, `emr.merges`, `emr.units`, `emr.encounters`, `emr.observations`, `emr.medications`, `emr.unmapped_codes` (no code map table: maps are reviewed YAML, §9.1); the `(hospital_id, id)` keys; `emr_patient_in_hospital()`, `emr_merge_patients()`, `emr_exclude_patient()`; `emr_patient_candidates()`, `emr_create_document_patient()`; grants for the document pipeline's `mhai_worker` (`INSERT` on `emr.raw_records` for `source_kind = document` only; nothing on `emr.patients`); audit append policies for the patient actions.
3. **`NNNNN_emr_findings.sql`:** `emr.findings` with its unique `dedup_key` and `supersedes`; `mhai_emr_core` grants.
4. **`NNNNN_alerts.sql`:** `alert_settings`, `alert_units`, `alert_levels`, `alert_recipients`, `alerts` (with `amended`, `withdrawn`, `grouped`), `alert_deliveries`, `user_notification_settings`; the restrictive update policy; the re-queue on user deactivation and role removal (in core's user-change transaction, with a trigger as a backstop); audit append policies for the alert actions.
5. **`NNNNN_emr_retention.sql`:** `emr.purge_requests` (durable, `state` `pending`, `held`, `done`); the retention and exclusion-purge functions and their roles, with the legal-hold check that moves a request to `held` and back.

Each migration is additive, has its Down, hands every object to `mhai_migrate`, and adds its tables to `TestRLSWithoutHospitalSeesNothing`.

## 19. Phased build, as pull requests

| PR | What | Depends on | Tests from §17.5 |
|---|---|---|---|
| 1 | **This spec**; ADR 0009 Accepted; ADR 0014 (separate service, one normaliser, document route); the amendment note in the document pipeline design (§9.0); AGENTS.md and CONTRIBUTING.md layout ("EMR plumbing" moves from `services/core/` to `services/emr/`); file frontend requirements §14.11 and the §12 detail from Plan 13 | | |
| 2 | **emr service skeleton:** `services/gokit/` (request-token verify with the generalised subject-to-audience rule and new vectors, `WithHospital`, redaction) moved out of core; `services/emr/` binary; NATS user and subjects; `emr.v1.ctl.health`; the `EMR` stream; compose service; the sealing key pair | 1 | A4 |
| 3 | **Connections, end to end with a stub connector:** migration 1; the platform routes, **with the URL shape checks in core's handlers**; sealing (bound boxes), `put_credentials` and `wipe_credentials`; origin binding; secret store interface (file store and managed store); `emr.v1.ctl.test`; the audit actions; openapi | 2, H3 for production | A1–A3, A5 (save), A21, A28, A35 |
| 4 | **Egress and SSRF:** the `egress-emr` proxy (ours: resolve once, check, pin; per-connection `Proxy-Authorization`) with the generated per-connection allow-list and declared CIDRs; the HTTP and SFTP client limits and origin checks | 2 | A5 (connect), A6, A29, A37 |
| 5 | **Canonical model, normaliser and patient index:** migration 2; `RawSink` and the allow-list; LOINC, ATC and UCUM tables and the YAML maps; the patient index, proposals, merges and exclusion functions; `CODEOWNERS` on maps and rules | 3, H6 | A7–A13, A31 |
| 6 | **FHIR R4 connector:** SMART Backend Services auth; polling, cursor, daily reconciliation, backfill; test (with the sort check); record link; a synthetic FHIR server on steven-reid with generated synthetic patients | 4, 5 | A18, A20, A25 |
| 7 | **Rules engine and rule set v1:** migration 3; the rule kinds; supersession; golden tests; shadow mode; the per-source breaker; findings, signals and `emr.v1.<hid>.finding`; the dead-letter types | 5, H4 to activate | A24, A26, A32 (source) |
| 8a | **Alert settings and routes in core:** migration 4; `/hospital/alert-settings`, `/alerts*`, `/me/notification-settings`, `/hospital/patient-matches*`, `/hospital/patient-exclusions`; the re-queue on user changes; `sources_status` and staleness; audit | 7 | A14–A17, A21, A23, A27, A33 |
| 8b | **The finding consumer:** `core_emr`; routing, dedup, quiet hours, the info cap, grouped mode; amended and withdrawn alerts; signals (breaker, matches, rule set) | 8a | A19, A30, A32 (hospital) |
| 9 | **Retention, exclusion purge and Usage:** migration 5; the retention sweep; the exclusion purge job; the `alerts` block of `/hospital/usage` under the small-group rule (when that route is built) | 8b | A34 |
| 10 | **mhai-web (separate repo):** switch Plan 13 screens to live; map `validation_failed`; show `sources_status`, `arrived_late`, `preliminary`, `amended`, `withdrawn`, grouped mode, nullable MRN; the possible-duplicates and exclusion screens; BFF-side credential sealing; audit kinds for the new actions | 8b | |
| 11 | **Havana, structured (`file_export`, `mhai_csv_v1`):** the format document and synthetic sample; the SFTP connector with the typed host key; staleness by expected interval | 5, H1, H7 | A7, A10 |
| 12 | **Havana, scanned archive (`document_archive`):** the connector to the documents service; MIME filtering; file names kept out of telemetry | documents service, document pipeline phase 1, H2 | A26, A36 |
| 13 | **`pids`:** core's check and the token claim for patient-scoped questions | 5 | |

PRs 2–8b are the backend for the screens mhai-web has built, proven on synthetic FHIR data. Live use needs every gate in §17.6. PR 11 is the earliest point at which Havana's real data could flow.

## 20. What's needed from people

| # | Who | What | Blocks |
|---|---|---|---|
| H1 | Product owner with Havana | A **structured data source** for labs (first), vitals and medications: which system holds them, whether it can export files or speak FHIR, how often it exports, and a synthetic or de-identified sample of its export layout | PR 11; real alerts for Havana |
| H2 | Our team (archive owners), **with Havana's agreement** (the archive is live there) | The archive's JSON "files added since" listing with a stable cursor, a per-integration read-only key, and a removed-files listing (§5.3) | PR 12 |
| H3 | Product owner, ops | Choose and provision the **managed secret store** (AWS or GCP, in-region) and the emr service's cloud identity | PR 3 in production |
| H4 | **A named clinical lead** (to be named if not yet), plus a second clinician | **Rule set v1:** critical and urgent thresholds with references, freshness windows, quiet hours, NEWS2 use (consciousness level, supplemental oxygen, Scale 2), likely causes and evidence; the breaker thresholds (source multiple and floor, hospital grouped-mode count), the wording of the "source paused" notice, and whether a hospital is asked to check a paused source's criticals by phone; review of the code and unit maps; the shadow review of v1's critical findings; sign-off for names and MRNs in alerts | PR 7 activation; live use |
| H5 | Privacy lead and a Nigerian data protection lawyer | **DPIA** (including queue breadth), the processing agreement addendum, the hospital's written confirmation that its critical-result call process stays primary, the NDPA basis (§17.7), data location, retention values | Any real data |
| H6 | Product owner | **Licences:** LOINC (free, terms to accept), WHO ATC/DDD (needs a licence for commercial use), SNOMED CT and ICD-10 for later | PR 5 |
| H7 | Havana IT | The **network path** (public HTTPS or SFTP with our egress address allow-listed, or a site VPN with declared CIDRs), the SFTP host key fingerprint, and a contact for credential rotation | PR 11 |
| H8 | Product owner | Answers to §21 | The plan |
| H9 | Product owner, ops | **An on-call owner** for the operator alerts the design relies on: stale feeds, rules errors, breaker pages, link conflicts, `reconciled_late` | Live use |

## 21. Open questions, with recommendations

1. **What does "Havana first" mean, given Havana's system holds scanned documents only?**
   *Recommendation:* build the pipeline on FHIR R4 with synthetic data first (PRs 2–8b), ask Havana now for a structured lab export (H1) and connect it through `file_export` (PR 11) as Havana's first live source. The scanned archive route (PR 12) follows document pipeline phase 1 and H2; it is useful for history, not for timely alerts.
2. **Should the source lab's own critical flag raise a critical alert when MHAI's rule doesn't?**
   *Recommendation: yes.* The lab's critical flag is the hospital's own critical-result policy, and it works even for unmapped codes. MHAI's thresholds add to it, never silence it. The per-source breaker (§12.4) covers a source that starts flagging everything.
3. **May a likely cause carry the confidence word "high"?**
   *Recommendation: no, in v1.* Causes are rule-authored suggestions with evidence; `caution` or `review` only, until the shadow and live data show how often they're right.
4. **How does a hospital exclude a patient who objects?**
   *Recommendation:* as §10.6: the hospital marks it in its own system where it can (a FHIR security label or an export column); a hospital admin can exclude by MRN or patient (PR 8a, screen in PR 10); as a fallback, an operator runs the audited CLI on the hospital's written request. Held data is purged within 24 hours.
5. **Defaults and caps:** levels when a hospital is first connected, and whether urgent alerts are ever capped.
   *Recommendation:* `critical_urgent` for every category, `critical` for vital signs (ward vitals are frequent); cap info at 20 an hour per hospital; never cap critical or urgent delivery, rely on dedup, quiet hours and the breakers (which group pop-ups but drop nothing).
6. **Medication codes: ATC only, or RxNorm too?**
   *Recommendation: ATC only.* RxNorm is US-centred; rules need drug classes, which ATC gives. Keep local names for display. The WHO ATC/DDD licence (H6) blocks PR 5.
7. **Retention values (§14.2).**
   *Recommendation:* as in the table: raw payloads 7 days, canonical records and alerts' patient references 13 months, the rest of an alert 24 months, backups 35 days. The lawyer and the hospitals confirm (H5).
8. **Hospitals with private certificate authorities or VPN-only systems.**
   *Recommendation:* a per-connection pinned CA bundle and `private_network` with declared CIDRs, both platform-admin-only, added when the first hospital needs them; not in PR 6.
9. **Separate Go module for `services/emr/`, or a second binary in core's module?**
   *Recommendation:* a separate module, with `services/gokit/` for the shared verifier (its subject rule generalised) and database helpers, so the emr binary can't import core's internals (auth, sessions, signing keys) by accident.
10. **Should the BFF seal credentials so core never sees them?**
    *Recommendation: yes, and it is a gate:* core seals at once in this design, and BFF-side sealing (an additive `credentials_sealed` field, PR 10) must ship before the first real hospital's credentials are entered (§17.6).
11. **Application-level encryption for patient names and MRNs?**
    *Recommendation: not in v1.* RLS, column grants, disk and backup encryption cover the threat we can name; field encryption would need key sharing between core and emr for every alert read. Revisit with the DPIA.
12. **Do clinicians need email or SMS for critical alerts?**
    *Recommendation:* not from MHAI in v1. The hospital's existing critical-result call process stays primary, confirmed in writing (H5); email and push come with the notifications service and its own design.
13. **Who confirms patient merges: hospital admins or clinicians?**
    *Recommendation: hospital admins* (two-step session, step-up), because they already own the hospital's settings and see staff; a clinician can flag a suspected duplicate through `wrong_patient` dismissals. Revisit if hospitals want their records staff to do it.

## 22. Revision after design review (7 Nov)

The review found no Critical issue, 10 Important and 18 Minor. All are taken; none is rejected. Where the review offered two fixes, the choice made:

| Finding | Where fixed | Choice |
|---|---|---|
| I1 credentials to a new host | §4.1, §6.2, §6.3, E18, A28 | credentials bound to their origin; an origin change wipes them and switches the connection off |
| I2 DNS rebinding | §6.4, E3, A29 | the proxy (ours) resolves once, checks and connects to what it checked; `private_network` limited to declared CIDRs |
| I3 two normalisers | §3.2, §5.3, §9.0, §18 | one normaliser in the emr service; documents arrive as reviewed raw records |
| I4 document-only patients | §8.2, §10.3, §16.4 | nullable MRN, a gated type change in mhai-web; linking later only by a person |
| I5 amended results | §8.2, §11.4, §12.5, E19, A30 | update in place or withdraw, visibly |
| I6 floods | §12.4, E10, A32 | per-source breaker (pause, page) and per-hospital grouped mode; criticals kept |
| I7 deactivated recipients | §12.2, E20, A33 | open alerts move to the queue in the same transaction |
| I8 out-of-date notice | §15.3, §16.4, §17.6 | `sources_status` always sent; mhai-web PR 10 gates live use |
| I9 merges | §3.2, §10.2, §12.6, A31 | nothing merges automatically; a hospital admin confirms; core writes the audit |
| I10 exclusion | §10.6, §14.2, E21, A34 | withdraw at once, purge within 24 hours, legal hold respected |
| M1 Havana facts | §1.1, §5.3, H2, A36 | file names are PHI; MIME filter; JSON listing added to H2 |
| M2 dismissal note | §12.5 | stored unmasked under RLS, never on the chain or in logs (masking would tie dismissal to the pipeline) |
| M3 `alerts.read` | §12.6 | records the alert ids served |
| M4 retention | §14.2 | alerts drop `patient_id` at 13 months; backups; removal clears the `public` alert tables |
| M5 database errors | §14.3, §15.2 | SQLSTATE and constraint name only |
| M6 sealed boxes | §6.1, §6.2, E23, A35 | the box binds connection, field, origin and time; sealing key in the secret store |
| M7 FHIR key provenance | §6.3 | the claim is dropped; the private key is typed once into the form; generation in the service is later |
| M8 code map table | §9.1, §18 | no table; reviewed YAML in memory |
| M9 change control | §9.4, §11.5 | `CODEOWNERS`; v1 in shadow with every critical reviewed |
| M10 FHIR gaps | §5.1 | daily reconciliation; the test checks the sort |
| M11 staleness | §15.4 | defined per connection type |
| M12 contract differences | §12.6, §16 | audit kind mapping; `hospital_removed`, `no_type`; flag mapping; `handled`; `high` never sent; `name`; nullable `mrn`; withdrawn display |
| M13 verifier subject rule | §3.2, PR 2 | generalised, with vectors |
| M14 PR plan | §19 | URL checks in PR 3; PR 8 split into 8a and 8b; AGENTS.md in PR 1 |
| M15 future prompts | §10.5 | source text is untrusted when it reaches a model |
| M16 people | §20 | H9 on-call; written call-process confirmation in H5; a named clinical lead |
| M17 NEWS2 | §11.1, H4 | consciousness, oxygen and Scale 2 are H4 decisions; no score without them |
| M18 queue breadth | §17.6, H5 | in the DPIA |

### 22.1 Second review (7 Nov)

The re-review accepted I1–I10 and raised four new findings. All are taken:

| Finding | Where fixed | Choice |
|---|---|---|
| N1 the proxy can't tell which connection a CONNECT is for | §6.4, A37, PR 4 | a per-connection `Proxy-Authorization` (connection id and an HMAC token only the emr service and the proxy can make); the proxy applies only that connection's hosts and ranges; ranges can't overlap across hospitals |
| N2 document patients | §3.3, §10.3, §12.6, §13, §18, A38–A40 | core creates them through `emr_create_document_patient()` (audited); the reviewer's search is core's `emr_patient_candidates()` (at most 10, audited, no listing); `mhai_worker` loses all access to `emr.patients`; a rejected document value reopens its review |
| N3 purges held by a legal hold | §10.6, §17.7, §18, A41 | a durable `emr.purge_requests` row, `held` under a hold and swept back to `pending` when it lifts; the hold exception stated in §17.7 |
| N4 a paused source looks current | §12.4, §15.4, H4, A42 | a paused source is `behind` at once; the notice's wording and any phone check are H4 decisions |
