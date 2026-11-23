# Documents service: intake, encrypted storage, scanning, extraction handoff and review

- **Status:** Approved: merged 14 Nov 2026 (#182) under the product owner's delegation ("just do everything"); each call below takes the safe and private side, and §16 lists them for later review. Recorded in [ADR 0016](../adr/0016-separate-documents-service-with-per-hospital-encryption.md). Being built, off by default: §13 is the plan, §17 lists where the build departs from it. Nothing is live.
- **Date:** 2026-11-14
- **Builds on:**
  - the [document pipeline design](../specs/2026-10-28-document-pipeline-and-model-promotion-design.md) (approved 28 Oct): the flow (§3), the extraction schema (§4), review (§5), the threat model T1–T16 and abuse cases B1–B27 (§9). This design is the "documents service" that design names but leaves unspecified, plus phase 1's review tables and routes;
  - the [phase 0 plan](../plans/2026-11-03-document-pipeline-phase0.md) (built, #91 and #94): `intelligence/modelstore` (pinned, verified weights) and `intelligence/doceval` (the extractor interface, the synthetic set and the phase 1 gate);
  - [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md) (Go API, Python models, NATS between them; only core publishes jobs), [ADR 0011](../adr/0011-frontend-ingress-over-nats.md) (the BFF reaches core over NATS), [ADR 0013](../adr/0013-allow-masked-text-to-cloud-language-models.md) (document images never go to a cloud model), [ADR 0014](../adr/0014-separate-emr-service-and-document-route.md) (the emr service holds every outside credential; scanned records reach the canonical model only after review; one normaliser);
  - the [EMR integration design](../specs/2026-11-07-emr-integration-design.md) §5.3, §9.0, §10.3, §10.6, §14.2 and the [EMR plan](../plans/2026-11-07-emr-integration.md): PR 9 (retention, exclusion purge, legal hold), PR 11 (#179, departures 62–72) and PR 12 (the pilot hospital's scanned archive, Tasks 45–47, waiting on this service and H2);
  - the [Analysis plan](../plans/2026-11-07-analysis-backend.md) PRs 7–8: clearance letters, whose live upload answers 503 `unavailable` until "the documents service" exists (departures 35 and 43).
- **Companion:** [Pilot hospital archive contract (H2)](2026-11-14-pilot-archive-contract-h2.md), what the pilot hospital's side provides for EMR PR 12.

## 1. Why

Three things are waiting on one missing service:

1. **Clearance letters** (Analysis PR 7) can't be uploaded outside the demo. Core keeps a 64 KiB body limit and NATS a 1 MB payload, so core can't take a 10 MB letter, and nothing exists to store, check and render it.
2. **Document pipeline phase 1** (typed lab reports) needs uploads, a malware scan, a sandboxed rasteriser, the `ingest_document` and `finalise_document` jobs, review tables and the review routes.
3. **EMR PR 12** (the pilot hospital's scanned archive) needs somewhere to hand each new scan, a reviewer's patient search, and a path from reviewed values to `emr.raw_records`.

This design is that service and the pieces around it in core, the pipeline worker and the emr service.

## 2. Goals, non-goals, anti-goals

**Goals**
- Take documents in from three places (a user's upload, a hospital admin's clearance letter, the emr service's archive connector) through one intake path.
- Store every document encrypted under its hospital's own key, so a leaked bucket credential, a copied object or a restored backup can't be read across hospitals, and a hospital's data can be destroyed by destroying its key.
- Refuse anything that isn't a plain PDF or image, scan for malware, and render pages only inside a sandbox with no network and no credentials.
- Hand clean page images to the pipeline worker for extraction (phase 1), queue every field that needs a person, and hand reviewed values to the emr service as raw records.
- Tie storage to the retention, exclusion purge, legal hold and hospital removal rules EMR PR 9 built.

**Non-goals**
- Choosing the extraction model (the phase 0 harness does that) or the confidence thresholds (clinical sign-off, pipeline design §4.5).
- The mhai-web screens. §10 lists what they call.
- Phases 2–4 of the pipeline (letters and charts, handwriting, less review). The tables here hold them without change; their routes and rules are later work.
- Platform verification of clearance letters (Plan 11b).

**Anti-goals: failure even if it works**
- A document readable by anyone outside its hospital, or by a platform admin.
- An original file opened by anything but the sandbox.
- A file name from the archive in a log, a message, a metric, an audit row or a URL.
- An unreviewed value in the canonical record.
- A document kept after its patient was excluded, its hospital removed, or its retention passed, except under a legal hold.

## 3. Components

```
                   browser (upload by one-time POST policy)
                        │                                  ▲ page images (≤ 900 KB, stamped)
BFF ──api.v1──▶ core ───┼── documents.v1.ctl.* (request token, aud documents) ──▶ documents service
                 │      ▼                                                         │  │  │
                 │   object store: quarantine/<hid>/<doc>  ◀──────────────────────┘  │  │
                 │      (encrypted: originals/<hid>/<doc>, pages/<hid>/<doc>/<n>)     │  │
                 │                                                                   │  │
                 │   DOCS stream: documents.v1.<hid>.uploaded ──▶ documents (scan)    │  │
                 │                documents.v1.<hid>.scanned  ◀── documents           │  │
                 │                                                                   │  ├─▶ clamav (clamd; no route out)
                 ├─ jobs.v1.<hid>.ingest_document / finalise_document ─▶ pipeline     │  └─▶ docsandbox (pdfium + Pillow under nsjail; no network)
                 │                                   │  documents.v1.<hid>.page ─────┘
                 │                                   ├─▶ model host (OCR, extraction; mTLS)
                 │                                   └─▶ Postgres: extraction, fields, review, emr.raw_records
                 └─ emr.v1.<hid>.fetched (after finalise) ─▶ emr normaliser ─▶ rules ─▶ alerts

emr service (document_archive connector) ── the pilot hospital's archive (H2 contract, through egress-emr)
     ├─ documents_register_archive_file() (Postgres definer)
     └─ documents.v1.<hid>.intake ──▶ documents service: a one-time POST policy for that row's key
```

| Component | What it is | Holds | Can reach | Cannot reach |
|---|---|---|---|---|
| **documents service** (`services/documents/`, Go, own module and binary, imports `services/gokit/`) | intake, encryption, scanning orchestration, page storage, page serving | the object store credential, the hospital key-encryption keys (KEKs) or the KMS identity | the object store, its own NATS user, Postgres as `mhai_documents`, `clamav`, `docsandbox` | the clinical tables, `emr.*`, users, sessions, the audit chain, the model host, the internet |
| **docsandbox** (separate container) | renders one file per fresh process: pdfium (BSD) for PDF, Pillow for images, under nsjail | nothing: no credential, no key, no network namespace | one input file and one output directory on a per-job tmpfs | network, other jobs' files, the object store |
| **clamav** (separate container) | `clamd`, scanning by `INSTREAM` | signatures only | nothing (it only answers) | network; signature updates come from a `freshclam` sidecar through an egress proxy that allows only the signature mirror |
| **core** (existing) | the routes, permissions, the review queue, patient search and creation, job publishing, `emr.v1.<hid>.fetched` after finalise, audit | the request-signing key | as today, plus `documents.v1.ctl.*` and the `DOCS` stream | object storage, keys |
| **pipeline worker** (existing, `services/intelligence/`) | `ingest_document` and `finalise_document` | no storage credential, no key | Postgres via `with_hospital`, the model host, `documents.v1.<hid>.page` | object storage, the internet |
| **emr service** (existing) | the `document_archive` connector (EMR Task 46) | the archive key (in its secret store) | the archive through `egress-emr`; Postgres definers below; `documents.v1.<hid>.intake`; the quarantine key a POST policy names | any document after upload, pages, keys |

**Why a separate Go service, not core:** core holds sessions, accounts, the audit chain and the request-signing key; it should not also hold every hospital's storage keys or parse hostile files next to them (the same reasoning as ADR 0014 for the emr service). Go, because the team writes Go and gokit already has request-token verification, the replay set and redacting logs.

**Why the worker holds no storage credential** (a change from pipeline design §3.3, which let it read `pages/` with a prefix check; that design's §12 left it to the security review): the worker parses model output and talks to the GPU host, so it is the likeliest component to be turned. With this design it can read a page only for a document it holds a live claim on, through the documents service, which checks that claim in the database. It never holds a KEK or a bucket credential.

## 4. Intake

### 4.1 Purposes and limits

Every document has one `purpose`, fixed at creation:

| Purpose | Who starts it | Types | Size | Pages | Then |
|---|---|---|---|---|---|
| `clinical` | a user with `documents.upload` (clinicians, records clerks, hospital admins; never platform roles) | PDF, PNG, JPEG, TIFF | ≤ 25 MB | ≤ 50 | `ingest_document` |
| `archive` | the emr service's `document_archive` connector (`svc:emr`) | PDF, PNG, JPEG, TIFF | ≤ 25 MB | ≤ 50 | `ingest_document` |
| `clearance` | a hospital admin with step-up (Analysis PR 7) | PDF, PNG, JPEG | ≤ 10 MB | ≤ 20 | the letter's `checking → waiting` or `refused` |

Every purpose: at most 10,000 × 10,000 px per page and 200 megapixels per document; a decompression ratio of at most 100:1 per stream; no encrypted or password-protected PDF (`password_protected`); no BigTIFF.

### 4.2 Upload from the browser (`clinical`, `clearance`)

1. The BFF calls core (`POST /documents`, or `POST /hospital/clearances` for a letter, §10). Core checks the permission, hospital, patient scope (`emr_patient_in_hospital` when a patient is named, else 404), rate limits and the in-flight cap, then inserts a `documents` row (`uploading`) in the caller's hospital.
2. Core asks the documents service (`documents.v1.ctl.presign_upload {document_id}`, request token `aud: documents`). The service reads the row under the token's hospital, refuses unless it is `uploading`, and returns an **S3 POST policy** for exactly `quarantine/<hid>/<document_id>`, with `content-length-range` 1 byte to the purpose's limit, the declared `Content-Type` as a condition, and a 10-minute expiry. A POST policy, not a presigned PUT, because only a policy can enforce the size range.
3. The browser posts the file straight to the object store. Bytes never cross NATS or core.
4. The BFF calls `POST /documents/{id}/uploaded`. Core moves the row to `uploaded` (only from `uploading`; a second call is a no-op) and publishes `documents.v1.<hid>.uploaded {document_id}` after the commit. A sweeper in core republishes for any row left `uploaded` for 2 minutes.

A row still `uploading` after 30 minutes becomes `cancelled`; the quarantine lifecycle rule (§5.4) removes any object.

### 4.3 Upload from the archive connector (`archive`)

The emr service never gets a general write credential. For each new file in the archive listing (companion contract):

1. **Register** through `documents_register_archive_file(connection_id, archive_file_id, revision, mime_type, size_bytes, sha256, source_name)`, a `SECURITY DEFINER` function, `EXECUTE` for `mhai_emr` only, run under `WithHospital` for the connection's hospital. It checks the connection is that hospital's, enabled, and of type `document_archive`; refuses a type or size outside §4.1 (the connector skips those and counts them by MIME type only); and returns `{document_id, state}` where `state` is `new`, `duplicate` (the same file and revision already registered) or `tombstoned` (an earlier copy was purged for an exclusion, §7.3: never fetched again).
2. **Get a policy:** request `documents.v1.<hid>.intake {document_id}`. Only the `emr` NATS user may publish it. The documents service checks in the database that the row is in that hospital, `purpose = archive`, `uploading`, and was registered by the emr service, then returns the POST policy as in §4.2.
3. **Download** the file from the archive through `egress-emr` and post it to the object store with the policy. The emr service reaches the object store's endpoint through `egress-emr` like any other origin (an allow-list entry for it).
4. **Mark uploaded** through `documents_archive_uploaded(document_id)` and publish `documents.v1.<hid>.uploaded`.

The file name (`source_name`) goes only into the definer's argument, inside a database transaction. It is never in a NATS message, a log, a metric label, an error row or an audit payload (EMR design §5.3, A36).

**Back-pressure** (sizing, §12): the connector registers at most `daily_intake_cap` documents per connection a day (default 100) and stops registering while the hospital has more than 300 open reviews. It resumes on its own; health shows `paused_for_review` with counts only.

### 4.4 Statuses

`uploading → uploaded → scanning → ready | quarantined | failed`; then for `clinical` and `archive`: `ready → queued → extracting → in_review → finalising → completed`, with `rejected`, `cancelled` and `purged` as ends. `quarantined` always carries a fixed reason: `unsupported_type`, `too_large`, `too_many_pages`, `too_many_pixels`, `password_protected`, `malware`, `unreadable`, `checksum_mismatch`. `failed` carries `scan_unavailable` (§5.3) or the pipeline's codes (`no_text`, `no_patient_identifiers`, `multiple_patients`, `unreadable`).

## 5. Scanning, type checks and rendering

On `documents.v1.<hid>.uploaded` (durable `documents_scan` on `DOCS`), the documents service claims the row (`uploaded → scanning`, a lease of 10 minutes) and:

1. **Reads the object's size** first; over the purpose's limit is `too_large` without reading it.
2. **Checks the bytes against the declaration.** Magic bytes at offset 0: `%PDF-`, the PNG signature, `FF D8 FF`, `II*\0` or `MM\0*`. The sniffed type must equal the declared type and be allowed for the purpose; anything else (a DOCX, ZIP or HTML renamed `.pdf`, a mismatch) is `unsupported_type`. For an archive document, the SHA-256 must equal the one the listing gave (`checksum_mismatch`).
3. **Scans for malware** with `clamd` `INSTREAM`: `MaxFileSize 30M`, `MaxScanSize 100M`, `AlertExceedsMax yes`, `AlertEncrypted yes`, `AlertBrokenExecutables yes`, `AlertOLE2Macros yes`, PUA detection on. Any hit, or a limit exceeded, is `malware`. The scan runs on the bytes as received, before any parser sees them.
4. **Renders in the sandbox.** The service writes the file to a fresh per-job directory on the shared tmpfs and asks `docsandbox` to render it. Each file gets a fresh nsjail process: no network namespace, read-only root, only its input file and output directory mounted, 2 GB memory, 60 s CPU, seccomp allow-list, no new privileges, killed on any limit (`unreadable`). pdfium renders PDF pages at 200 DPI (longest side at most 4,000 px); Pillow decodes images with `MAX_IMAGE_PIXELS` set to the limit; multi-page TIFF counts pages. Page count, pixel and password checks happen here (`too_many_pages`, `too_many_pixels`, `password_protected`). Output is PNG, greyscale or RGB, metadata stripped.
5. **Checks the output, outside the sandbox:** each PNG is decoded again by the documents service with the same limits, its dimensions match what the sandbox reported, and the page count is within the limit. Anything wrong is `unreadable`. JavaScript, launch actions, embedded files and form layers in a PDF never run: only pixels leave the sandbox (B2, B5).
6. **Seals and stores** the original under `originals/<hid>/<doc>` and each page under `pages/<hid>/<doc>/<n>`, encrypted (§6), deletes the quarantine object, and in one transaction writes the page rows, the object rows with their wrapped keys, `ready` and an outbox fact (`document.ready` or `document.quarantined {reason}`).
7. **Announces** `documents.v1.<hid>.scanned {document_id, outcome}` (ids and a code only).

A quarantined document's file is never stored outside quarantine: the quarantine object is deleted at once and the row keeps the reason, size and SHA-256.

### 5.1 Why pdfium and not the others

pdfium is Chrome's PDF engine (BSD-3), continuously fuzzed. MuPDF and Ghostscript are AGPL, and Ghostscript has a long history of sandbox escapes; poppler is GPL and less fuzzed than pdfium. The renderer sits behind one interface, so it can change by a reviewed PR.

### 5.2 Signatures

`freshclam` updates signatures through an egress proxy whose only allowed host is the signature mirror (or a private mirror the ops owner runs). The documents service checks the signature database's age at every scan: older than 48 hours, it refuses to scan.

### 5.3 Failing closed

`clamd` unreachable, signatures stale, or the sandbox unavailable: the document stays `scanning`, the message is `Nak`ed with a back-off, and an alert fires (`documents_scan_blocked`). A document is never marked `ready` without a clean scan. After 6 hours it becomes `failed` with `scan_unavailable`, and the uploader sees "We couldn't check this file. Try again later."

### 5.4 Quarantine

The quarantine prefix is written only by POST policies and holds the only unwrapped bytes the service ever stores. It is protected by the provider's server-side encryption only (the browser and the emr service can't do our envelope encryption), so it is kept short: the documents service deletes each object as soon as the scan finishes either way, and a bucket lifecycle rule deletes anything under `quarantine/` after 24 hours as the backstop.

## 6. Storage and encryption

### 6.1 The object store

- **One bucket per environment** (`mhai-documents-<env>`), S3-compatible, in our cloud account and the data's region (H5 decides the region). Separate from the model store's bucket.
- **Prefixes per hospital:** `quarantine/<hid>/`, `originals/<hid>/`, `pages/<hid>/`. Object keys hold ids only: never a name or a file name.
- **Bucket settings:** public access blocked; a policy that refuses non-TLS requests; versioning on, with non-current versions expired after 7 days (so a deletion is complete within 7 days, and is already unreadable at once, §6.2); a CORS rule allowing `POST` from the clinical app's origin only.
- **Credentials:** one credential, the documents service's, scoped to this bucket. Nobody else holds one. The POST policies it signs are the only way anything else writes, each for one key.

### 6.2 Envelope encryption, per hospital

- **A key-encryption key (KEK) per hospital,** created when the hospital's first document arrives.
- **A data key (DEK) per object,** random 256-bit. Objects are encrypted with Tink's streaming AEAD (AES-256-GCM-HKDF, 1 MB segments), so a page can be read without holding the whole file in memory.
- **Associated data** binds the object to where it belongs: `mhai.documents/1|<hid>|<document_id>|<kind>|<n>`. An object copied to another hospital's prefix, another document or another page fails to decrypt.
- **The wrapped DEK lives in Postgres** (`document_objects.wrapped_dek`, with `kek_version`), never in the object's metadata. Deleting the row makes the object, and every older version of it, unreadable at once, whatever the bucket's versioning keeps.
- **KEK providers,** behind one interface:
  - `kms` (production): one cloud KMS key per hospital, used only by the documents service's workload identity (encrypt and decrypt, no export). Removal schedules the key's destruction.
  - `file` (pilot and development): a keyring file of per-hospital keys on a volume only the documents container mounts, mode 0400, its own backup encrypted with the operator's `age` key. The service refuses to start with the `file` provider when `APP_ENV` is production unless `DOCUMENTS_ALLOW_FILE_KEYRING=pilot` is set, and logs a warning at every start.
- **Rotation:** a new KEK version yearly or on suspicion; DEKs are re-wrapped in the background (the objects themselves are not rewritten). Old KEK versions are destroyed once nothing is wrapped under them.
- **Startup refusal:** no keyring or KMS access, a placeholder key, or a KEK whose test unwrap fails: the service stays not ready.

### 6.3 Retention and purge

The retention rules follow EMR PR 9 (design §14.2, plan departures 43–47) and take the shorter, more private value where the design is silent. Every deletion first asks `mhai_hospital_on_hold(hid)`; a held hospital deletes nothing, and the request waits as `held`, exactly as `emr.purge_requests` does.

| Data | Kept | Then |
|---|---|---|
| Quarantine objects | until the scan finishes; 24 hours at most (lifecycle) | deleted |
| A `quarantined`, `failed`, `cancelled` or `rejected` document's stored objects | 7 days (time to retry or appeal) | objects and keys deleted; the row keeps reason, size and SHA-256 for 90 days, then goes |
| Page images of a `completed` document | 30 days after completion | deleted; re-rendered from the original if a review is reopened or a detach needs them |
| Originals of `clinical` and `archive` documents | 13 months after completion, the life of the canonical rows they produced (EMR §14.2) | deleted with their keys |
| Extractions, fields and review decisions | 13 months after completion | deleted |
| The archive file name (`source_name`) | until the review finishes, plus 30 days (for a reopened review) | set to null |
| Clearance letters (original and pages) | while the letter is `checking`, `waiting` or `active`; then 25 months, the life of the Analysis periods that may cite it, until H3 sets the value | deleted; a `refused` letter's file is never kept past the 7-day rule |
| `documents` rows | while anything above remains | deleted, except a tombstone for excluded patients (§7.3) |

Retention values become the frontend contract's `documents` retention item (§14.8.4) when the platform settings feature reads them; until then they are constants in one place, confirmed by the privacy lead (H5) before real data, as EMR PR 9's are. The sweep is off by default (`DOCUMENTS_RETENTION_SWEEP=false`), like EMR's.

**The sweep** runs in the documents service's maintenance loop (leader lock, a cap per run, every 5 minutes) under its own role, `mhai_documents_maint`, which can only call the purge functions. A purge deletes the rows and wrapped keys in one transaction first (the crypto-shred), then deletes the objects; an object whose deletion fails is retried and is already unreadable.

### 6.4 Exclusion, legal hold and hospital removal

- **Exclusion** (EMR §10.6): `emr_purge_patient(request_id)` gains a step that calls `documents_purge_for_patient(hid, patient_id)`. Every document whose review attached that patient is marked `purge_pending` in the same transaction; the maintenance loop deletes its keys and objects within 24 hours. The row stays as a tombstone (`purged`, no file name, no extraction), keeping `archive_file_id` and `revision` so the connector never fetches that file again. Under a hold, the EMR purge request is `held`, so this step never runs until the hold is released (A41 holds for documents too).
- **Legal hold:** no document deletion of any kind for a held hospital, including the retention sweep, and including the scheduled destruction of a KEK. The minimal `legal_holds` table (EMR departure 10) holds a whole hospital; the platform feature's date scope (contract §14.8.1), when it lands, narrows that.
- **Hospital removal:** `documents_remove_hospital(hid)` deletes every row of the hospital and schedules the KEK's destruction (KMS) or deletes it from the keyring (pilot). The objects are deleted afterwards; they are unreadable from the moment the KEK is gone, including in bucket versions and storage backups. It refuses while the hospital is on hold, and runs inside the two-person removal flow when that exists (EMR departure 11 gives the hook).
- **Backups:** Postgres backups (35 days) can bring back wrapped keys of purged documents. The restore runbook already re-applies exclusions and deletions before use (TODO, EMR PR 9 follow-ups); it gains "re-run document purges" as a line. A KEK destroyed for a removed hospital makes its restored keys useless anyway.

### 6.5 Archive files removed at the source

The companion contract gives a removed-files listing. When the connector sees a removal for a file MHAI holds:

- **Not yet finalised** (`uploading` to `in_review`): the document is `cancelled` with `source_removed`, any open review closed, and the 7-day rule deletes it. No value from it ever reaches the record.
- **Finalised:** nothing is deleted on our side by itself, because a hospital tidying its archive is not a clinical retraction. The document is flagged `source_removed`, a hospital admin sees it in a "Removed from the archive" list (counts and dates; the file name only to a `documents.review` holder), and chooses **keep** or **detach** (detach retracts its values as in pipeline design §5.3, step-up). The choice is audited.

## 7. Database: tables, tenancy and RLS

### 7.1 Tables (migration numbered at build time, after EMR PR 11's 00043: `documents` and `document_objects` are 00046, §17 departure 11)

All tables have `hospital_id`, composite keys `(hospital_id, id)`, `ENABLE` and `FORCE ROW LEVEL SECURITY`, and policies on `hospital_id = mhai_current_hospital()`.

- `documents (id, hospital_id, purpose, status, reason, patient_hint_id, uploaded_by, via (user|emr), connection_id, archive_file_id, archive_revision, source_name, source_removed, mime_type, size_bytes, sha256, pages, received_at, scanned_at, completed_at, purge_state, version)`. `source_name` has no column grant to any role: it is read only through `documents_source_name(review_id)` (§9.2). Unique `(hospital_id, connection_id, archive_file_id, archive_revision)`.
- `document_objects (hospital_id, document_id, kind original|page, n, object_key, wrapped_dek, kek_version, sha256, size_bytes)`.
- `document_extractions (id, hospital_id, document_id, schema_version, model_versions, extraction jsonb, created_at)`: one per `ingest_document` run.
- `extraction_fields (hospital_id, extraction_id, path, value jsonb, raw_text, page, bbox, script, confidence, band, grounded, flags, proposed_code, proposed_unit, must_check, priority)`.
- `document_reviews (id, hospital_id, document_id, status open|needs_changes|finished|rejected, leased_by, lease_until, patient_id, patient_decision picked|created, needs_clinician, opened_at, finished_by, finished_at, reject_reason, version)`. Unique open review per document.
- `review_decisions (hospital_id, review_id, path, action accept|correct|not_on_document, value jsonb, code, unit, decided_by, decided_at, reopened_reason)`: corrections kept as the pipeline design §5.5 says (inside production only).
- `review_search_counts (hospital_id, user_id, day, used)` and a per-review counter on `document_reviews`, for the search limits (§9.2).

### 7.2 Roles and grants

| Role | Who | Grants |
|---|---|---|
| `mhai_gateway` | core | `SELECT` on `documents` (not `source_name`), extractions, fields, reviews, decisions under RLS; writes only through definers that check the caller's permission, lease and version and write the audit row in the same transaction |
| `mhai_worker` | pipeline worker | `documents`: `SELECT` and the claim columns' `UPDATE`; extractions and fields: `SELECT, INSERT, UPDATE` of the pipeline's columns; reviews: `INSERT` (open) and `SELECT`; `emr.raw_records`: `INSERT` with `WITH CHECK (source_kind = 'document')` (EMR Task 45); nothing on `emr.patients` or any canonical table |
| `mhai_documents` | documents service | `SELECT` on `documents` (status, purpose, limits, lease, claim columns; not `source_name`) and `document_objects`; writes only through `documents_scan_*` definers; nothing on extraction or review tables |
| `mhai_emr` | emr service | `EXECUTE` on `documents_register_archive_file`, `documents_archive_uploaded`, `documents_archive_removed` only |
| `mhai_documents_maint` | the maintenance loop | `EXECUTE` on the purge and retention functions only |

Platform roles and auditors hold nothing on these tables; every document route is 404 for them (B27). Every definer sets `search_path = pg_catalog, pg_temp` and schema-qualifies names (EMR K2).

### 7.3 Hospital separation, end to end

- **Routes:** the hospital comes from the session, never a body; another hospital's id is 404.
- **Database:** RLS with the hospital from `WithHospital` (Go) or `with_hospital` (Python), from the verified token.
- **Messages:** every subject carries `<hid>`; every receiver re-reads the row under that hospital and refuses a mismatch.
- **Objects:** keys carry the hospital; the documents service checks the segment against the row's hospital before any read, and the associated data and the per-hospital KEK make a cross-hospital read fail cryptographically even if a check were missed.
- **Model host:** stateless; vLLM's prefix cache salted per hospital (boundary §11).

## 8. NATS: subjects, jobs and users

| Subject | Kind | Publisher | Consumer | Body |
|---|---|---|---|---|
| `documents.v1.ctl.<op>` (`presign_upload`, `page_image`, `health`) | request/reply | core only | documents (queue group `documents`) | ids; `page_image` also the viewer's display name and the stamp time |
| `documents.v1.<hid>.intake` | request/reply | emr only | documents | `{document_id}` |
| `documents.v1.<hid>.page` | request/reply | pipeline only | documents | `{document_id, page, offset, lease_id}`; reply ≤ 512 KiB of PNG bytes plus `{total, sha256}` |
| `documents.v1.<hid>.uploaded` | JetStream `DOCS` | core, emr | documents (`documents_scan`) | `{document_id}` |
| `documents.v1.<hid>.scanned` | JetStream `DOCS` | documents | core (`core_documents`) | `{document_id, outcome}` |
| `jobs.v1.<hid>.ingest_document` | JetStream `JOBS` | core only | pipeline (`pipeline_documents`) | `IngestDocumentJob {document_id}` |
| `jobs.v1.<hid>.finalise_document` | JetStream `JOBS` | core only | pipeline (`pipeline_documents`) | `FinaliseDocumentJob {document_id, review_id}` |
| `emr.v1.<hid>.fetched` | JetStream `EMR` | core, after finalise | emr (`emr_normalise`) | `{raw_ids[]}` (unchanged) |

- **Control calls** carry a core-signed request token (`aud: documents`), verified with gokit's `reqtoken` and subject table, a replay set and the token's hospital, as the emr service verifies `emr.v1.ctl.*` (EMR Tasks 2 and 4).
- **The two calls without a core token** (`intake` from emr, `page` from the pipeline) are authorised by NATS permissions (only that user may publish the subject) **and** database state: for `intake`, a row of that hospital registered by emr and still `uploading`; for `page`, a row of that hospital in `extracting` or `finalising` whose current claim's `lease_id` (a random 128-bit value the worker received when it claimed the row) matches and has not expired. A forged or stale request gets `not_found` and nothing else.
- **Stream `DOCS`:** subjects `documents.v1.*.uploaded` and `documents.v1.*.scanned`; work-queue retention, file storage, `max_age` 7 days, `MaxMsgsPerSubject` 10,000 with `DiscardNewPerSubject` (one hospital can't fill it for others), `Nats-Msg-Id` = document id plus kind (duplicates drop). JetStream encryption at rest on before real data, as for `JOBS`. The sweepers in core (§4.2) and the documents service (stuck `scanning` leases) cover a lost message.
- **`ingest_document`** (pipeline design §3.2): token `sub` the uploader, or `svc:emr` for archive documents; deadline 30 minutes, `+WIP` every 20 s; claim and lease on `documents`, the status re-checked between stages; at most 5 in flight per hospital (core holds the rest as `queued` and publishes as slots free; a user's upload past 20 queued in the hospital is 503 `busy`); 50 pages. It reads pages through `documents.v1.<hid>.page`, sends them to the model host, computes confidence, grounding and flags, validates, proposes codes (normalisation is the emr service's, ADR 0014), and in one transaction writes the extraction, fields, the review (`open`, `needs_clinician` when a critical value is present), status `in_review` and the outbox fact `document.extracted`.
- **`finalise_document`:** token `sub` the reviewer; completes at most once (unique `review_id`). It re-validates the decided values, writes them as `emr.raw_records` (`source_kind = document`, `document_id`, `review_id`, the review's `patient_id`, the confirmed code and unit), sets `completed`, and writes `document.finalised` to the outbox. Core then publishes `emr.v1.<hid>.fetched` with those raw ids (EMR Task 47). A value the normaliser rejects comes back as `document_rejected` and core reopens the review `needs_changes` (EMR §10.3, A40).
- **Emergency stop** (pipeline design §7.6): `CORE_DOCUMENTS_INGEST_PAUSED=true` stops new `ingest_document` jobs; documents wait as `queued`.
- **Dead letters:** `dead.v1.<hid>.ingest_document` and `.finalise_document`, shown in the platform's Failed jobs view with ids and codes only (B27); a platform retry re-publishes through core.

**NATS users:**
- `documents` (new): subscribe `documents.v1.ctl.> documents` and `documents.v1.*.intake documents`, `documents.v1.*.page documents` (queue group only); publish `documents.v1.*.scanned`, its own `DOCS` consumer API and acks; `allow_responses: {max: 1, expires: "1m"}`. No `jobs.v1`, no `api.v1`, no KV.
- `gateway` (core) gains publish `documents.v1.ctl.>`, `documents.v1.*.uploaded`, and the `core_documents` consumer on `DOCS`.
- `pipeline` gains publish `documents.v1.*.page` and the `pipeline_documents` consumer on `JOBS`.
- `emr` gains publish `documents.v1.*.intake` and `documents.v1.*.uploaded`.

## 9. Review: tables, queue and the reviewer's patient search

### 9.1 The queue

- **Phase 1 reviews every document.** Within a document, fields follow pipeline design §5.1–5.2: high band, grounded and unflagged are pre-filled and collapsed; medium must be ticked one by one; low or ungrounded are greyed suggestions beside an empty box. Nothing is filled silently.
- **Order:** documents with a critical value first (an `HH`/`LL` flag, or past an active rule's critical limits), then a patient conflict, then the most low-band fields, then oldest. The list (`GET /review/documents`) shows document type, date, page count, priority and age: no name, no file name, no values.
- **Who:** `documents.review` (clinicians and the records-clerk role, pipeline decision 1). A review with `needs_clinician` (a critical value in phase 1; diagnoses and medications in phase 2) can be prepared by a clerk but finished only by a clinician.
- **One reviewer at a time:** opening a review takes a 30-minute lease (`leased_by`, `lease_until`), renewed by each save; another reviewer sees it as taken. Every write carries `version` (409 `conflict` on a stale one).

### 9.2 Finding or creating the patient

Exactly EMR design §10.3, served by core, with these calls made concrete:

- **Suggestions** (pipeline design §5.3): on opening, core's definer `emr_patient_suggestions(review_id)` scores the extracted identifiers against the hospital's index and returns at most 3, each with the matching parts marked and the score as a word. Nothing attaches without a person. An upload made from a patient's page proposes that patient, and a printed MRN or birth date that disagrees blocks finishing with `conflict` until a person resolves it.
- **Search:** `POST /review/documents/{id}/patient-search` with either `{mrn}` (exact) or `{name, birth_year}` (name at least 3 characters). A POST, so no name ever sits in a URL, a proxy log or browser history. It calls `emr_patient_candidates(review_id, query)`: the caller must hold `documents.review` and the review's lease; at most 10 results (`id`, `display_name`, `mrn`, age in years, sex, sources); no wildcard, no listing, no paging. Limits: 20 searches per review and 200 per reviewer a day (429 `rate_limited`). Each search is audited `emr.patient_searched` with `review_id`, the query kind and the result count, never the query text.
- **Creation:** `POST /review/documents/{id}/patient {create: {display_name, mrn?, birth_date?, sex}}` (two-step session; `emr_create_document_patient`, EMR §10.3): refuses an MRN already in the hospital (pick that patient instead), records `origin = document`, audits `emr.patient_created` without the name. `{patient_id}` picks an existing one (`document.patient_confirmed`).
- **The file name hint** (archive documents): `GET /review/documents/{id}` includes `source_name` only for the lease holder, read through `documents_source_name(review_id)`, and the opening is audited. It is never in the queue, a search, a log or an export. It is a hint for the person; it never matches a patient by itself (ADR 0014).

### 9.3 Finishing

`POST /review/documents/{id}/finish {version}` needs every must-check field decided, the patient confirmed, the document type and date confirmed, and a clinician when `needs_clinician`. Core sets the review `finished`, the document `finalising`, and publishes `finalise_document` after the commit. `POST …/reject {reason, version}` (`not_clinical`, `wrong_hospital`, `unreadable`, `multiple_patients`, `duplicate`, `other` with a note) ends the document as `rejected`; nothing reaches the record.

## 10. APIs (additive; OpenAPI minor bump)

Core serves all of them over `api.v1.clinical` (ADR 0011). Writes take `Idempotency-Key`; replies to page routes are `Cache-Control: no-store`, `Content-Disposition: inline`, `X-Content-Type-Options: nosniff`.

| Route | Who | What |
|---|---|---|
| `POST /documents {purpose: "clinical", content_type, size_bytes, patient_id?}` | `documents.upload` | → `{document_id, upload: {url, fields, expires_at}}`; 422 for a type or size outside §4.1; 404 for a patient not in the hospital; 429 past 30 uploads an hour per user; 503 `busy` past the queue cap |
| `POST /documents/{id}/uploaded` | the uploader | → 202, `scanning` |
| `GET /documents/{id}` | the uploader; `documents.review` holders | status, reason code, pages, purpose |
| `POST /documents/{id}/cancel {version}` | the uploader | before `in_review` only |
| `GET /review/documents?status&cursor` | `documents.review` | the queue (§9.1) |
| `GET /review/documents/{id}` | `documents.review` | takes the lease; fields, flags, suggestions, `source_name` for the lease holder |
| `GET /review/documents/{id}/pages/{n}` | `documents.review` (lease holder) | a page image (§10.1) |
| `PATCH /review/documents/{id}/fields {version, decisions[]}` | lease holder | accept, correct, not on document |
| `POST /review/documents/{id}/patient-search` | lease holder | §9.2 |
| `POST /review/documents/{id}/patient` | lease holder, two-step | pick or create (§9.2) |
| `POST /review/documents/{id}/finish` and `/reject` | lease holder | §9.3 |
| `POST /review/documents/{id}/release` | lease holder | gives the lease up |
| `POST /documents/{id}/detach {reason, mfa_code}` | `documents.detach` (hospital admin), step-up | pipeline design §5.3 |
| `GET /hospital/documents/source-removed` and `POST …/{id}/keep` or `/detach` | hospital admin | §6.5 |
| `POST /hospital/clearances` (live) | hospital admin, step-up | the letter's metadata and the checks of Analysis design §8.1, then `{clearance, upload: {url, fields, expires_at}}` with the letter `checking` |
| `POST /hospital/clearances/{id}/uploaded` | the uploader | the scan's outcome moves the letter to `waiting` or `refused` |
| `GET /hospital/clearances/{id}/pages/{n}` (live) | hospital admin | a stamped page (§10.1) |

**Clearance letters change shape for live use.** The demo keeps today's multipart route (a small file, fixture pages). Live, the file goes to the object store by POST policy, as every other upload, and the letter waits in `checking` until the scan reports; `mhai_clearance_demo_ready`'s live counterpart, `mhai_clearance_scanned(id, outcome, pages, sha256)`, is called by core's `core_documents` consumer. This is a contract change for mhai-web (the hand-off list in PR 5, §13).

The EMR design names the patient route `POST /documents/reviews/{id}/patient`; this design uses `/review/documents/{id}/patient` to match the pipeline design's other review routes. Same function, same checks.

### 10.1 Page images

Core asks `documents.v1.ctl.page_image {document_id, n, viewer_name, at}`. The documents service decrypts the page, scales it to at most 1,600 px wide, **stamps it with the viewer's display name and the time** across the page (clinical review pages as well as letters, as a deterrent to screenshots leaving the hospital), and encodes JPEG at the highest quality that fits 900 KB, so the reply fits NATS's 1 MB payload with room for the envelope. Each view is audited (`document.page_viewed {document_id, n}`, or `clearance.viewed {page}` for letters) and the audit row commits before the bytes are sent.

## 11. Audit

Typed, closed payloads, ids and codes only (B24), through core's audit registry; the documents service and the pipeline write facts to the outbox, which core drains into the chain (the intelligence boundary's pattern). The actor is the user, `svc:emr` or `svc:documents`.

| Action | Payload |
|---|---|
| `document.upload_started`, `document.uploaded` | `document_id`, `purpose`, `via` |
| `document.ready`, `document.quarantined` | `document_id`, `pages` or `reason` |
| `document.extracted` | `document_id`, `extraction_id`, `model_versions`, field counts by band |
| `document.review_opened`, `document.page_viewed` | `document_id`, `review_id`, `n` |
| `emr.patient_searched` | `review_id`, query kind, result count |
| `document.patient_confirmed`, `emr.patient_created` | `document_id`, `review_id`, `patient_id` |
| `document.field_corrected` | `review_id`, field path, action (no values) |
| `document.finalised`, `document.rejected`, `document.review_reopened` | `document_id`, `review_id`, counts or reason |
| `document.detached`, `document.source_removed`, `document.source_removed_decided` | `document_id`, reason or choice |
| `document.purged`, `document.purge_held` | counts and kind (`retention`, `exclusion`, `source_removed`, `hospital_removed`) |
| `documents.key_rotated`, `documents.key_destroyed` | `hospital_id`, `kek_version` |

Never in any payload, log line, span or metric label: a file name, a page's text, an extracted value, an identifier, a search query, an object key with a name, a POST policy, a signed URL, a key.

## 12. Sizing

These are planning numbers for the pilot; the pilot measures the real ones.

| Quantity | Assumption | Result |
|---|---|---|
| Archive intake | the 100-a-day cap per connection (§4.3); the pilot hospital's archive is about 194 GB, so a full backfill isn't attempted (the companion contract starts at "latest") | ≤ 100 documents a day |
| Direct uploads | 50 a day | |
| Pages | about 8 per document | about 1,200 pages a day |
| Extraction | pipeline design §6.3: p50 8 s, p95 20 s a page on one L40S | about 3 GPU-hours a day at p50; fits the shared GPU host |
| Review | 3 to 5 minutes a document | 8 to 12 reviewer-hours a day at the cap: **the real bottleneck**, hence the cap and the 300-open-review back-pressure |
| Originals | about 2 MB each, kept 13 months | about 120 GB in a year |
| Pages | about 1 MB each, kept 30 days after completion | about 40 GB at any time |
| Postgres | about 60 field rows a document | about 3.5 million rows in 13 months; no partitioning needed yet |
| documents service | 2 vCPU, 2 GB | |
| docsandbox | 2 vCPU, 4 GB (two files at once, 2 GB each) | about 1 s a page |
| clamav | 1 vCPU, 3 GB (signatures in memory) | about 1 s a file |
| `DOCS` stream | ids only | kilobytes |

Object storage at these volumes costs a few US dollars a month. A second hospital with a large archive raises the cap only with the reviewers to match.

## 13. Phased build plan

Each PR leaves `main` working with documents **off**: the `documents` compose profile isn't started, `CORE_DOCUMENTS_ENABLED=false` makes every route 404 (and the letter upload keeps its 503), no `DOCS` stream exists, and the `document_archive` type isn't in `CORE_EMR_CONNECTOR_TYPES`.

| PR | What | Waits on |
|---|---|---|
| **1: docs** *(built with PR 2's skeleton, §17)* | this design and the companion contract; ADR 0016 (a separate documents service; per-hospital envelope encryption; the worker without storage credentials; DB-state-authorised `intake` and `page` calls); amendment notes on the pipeline design §3.3 and the EMR design §10.3 route name; TODO gates | review |
| **2: skeleton** *(in PR 1; the `DOCS` stream and the sidecars move to PR 4, §17)* | `services/documents/` (module, binary, settings, health, `documents.v1.ctl.*` verification with gokit), NATS user `documents`, the `DOCS` stream (created only when enabled), compose profile `documents` with `clamav` and `docsandbox` | PR 1 |
| **3: storage and keys** | migration: `documents`, `document_objects`, roles and RLS; the KEK interface with the `file` provider; streaming AEAD with associated data; POST policies; quarantine lifecycle; startup refusals | PR 2 |
| **4: scanning and sandbox** | *(from PR 2, §17: the `DOCS` stream with `CORE_DOCUMENTS_ENABLED`, and the `clamav` and `docsandbox` sidecars)* magic bytes, limits, `clamd` and signature age, the nsjail renderer (pdfium, Pillow), output re-checks, outcomes, `documents_scan` consumer, sweepers; the B1–B6 abuse tests | PR 3 |
| **5: uploads and letters live** | core's `POST /documents`, `/uploaded`, `GET`, cancel, `core_documents` consumer; the letter's live upload and `mhai_clearance_scanned`; stamped page images; the mhai-web hand-off list | PR 4; H3 template before a live letter |
| **6: review** | migration: extractions, fields, reviews, decisions, search counts; the queue, lease, fields, finish, reject, release routes; suggestions, patient search and creation (this **is** EMR Task 45, numbered at build time) | PR 5 |
| **7: ingest** | `ingest_document` in the pipeline worker behind `doceval`'s extractor interface, with `modelstore`'s verified load; page fetch over `documents.v1.<hid>.page`; confidence, grounding, validation, code proposals; the in-flight cap and emergency stop. Runs end to end on the synthetic set with the stub extractor | PR 6; a real model needs the GPU (phase 0 H6) and a pinned `extract` role |
| **8: finalise** | `finalise_document` to `emr.raw_records`; core's `emr.v1.<hid>.fetched`; rejected values reopen the review; detach (needs an emr `document_detached` signal that marks derived rows `entered-in-error` and withdraws their alerts). This is EMR Task 47 | PR 7 |
| **9: retention and purge** | the maintenance loop, the retention table, the exclusion hook into `emr_purge_patient`, legal holds, hospital removal with key destruction, the KMS provider, key rotation | PR 8; H5 confirms the values before the sweep is switched on |
| **10: archive connector** | EMR Task 46 against the companion contract: listing, removals, downloads, register, intake, back-pressure, `source_removed` handling | PR 9; **H2** (the pilot hospital implements the contract) |

**Live use** (any real document) waits on all of: phase 1's evaluation passing pipeline design §6.3 on the locked split with clinical sign-off; the GPU host; the KMS provider (or the product owner's written acceptance of the `file` keyring for the pilot, §16 D5); H5 (privacy, retention values, region); NATS TLS and JetStream encryption at rest; and, for the archive, H2.

## 14. Security and threat model

### 14.1 Assets

Patient data in originals, pages, extractions, review decisions and archive file names; hospital separation; correct values and correct patient attachment (an unchecked guess must never become a record or an alert); the per-hospital KEKs and the bucket credential; the audit chain; availability of upload, review and letter verification.

### 14.2 Actors and trust boundaries

Actors: clinicians, records clerks and hospital admins (upload, review, letters); platform admins (jobs view only, no documents); auditors (none); the BFF; core; the documents service; docsandbox; clamav and its updater; the pipeline worker; the model host; the emr service; the pilot hospital's archive; the object store; an attacker who controls a file's content, holds one stolen session or credential, or compromises one component.

| Crossing | What is checked |
|---|---|
| browser → BFF → core | session, permission, hospital from the session, patient via `emr_patient_in_hospital`, `Idempotency-Key`, `version`, rate limits, step-up where named |
| browser or emr → object store | a POST policy for one key in `quarantine/<hid>/`, a size range, a content type, 10 minutes |
| core → documents (`ctl`) | request token (`aud: documents`, subject, body hash, expiry), replay set, the token's hospital against the row |
| emr → documents (`intake`) | NATS user; the row registered by emr in that hospital and `uploading` |
| pipeline → documents (`page`) | NATS user; the row's hospital, status and live `lease_id` |
| documents → clamav, docsandbox | internal network with no route out; the sandbox has no network at all; per-job directory |
| documents → object store | its credential only; the key's hospital segment; decryption bound to hospital, document, kind and page |
| documents, pipeline, emr → Postgres | `WithHospital` / `with_hospital` from verified tokens or the connection's hospital; narrow roles; definers |
| emr → archive | `egress-emr` (resolve once, pin, per connection), the archive key from the secret store (companion contract) |
| core → JetStream → pipeline | the boundary design's checks (§7.3 there) |

### 14.3 Threats (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| S1 | Tampering / EoP | A crafted PDF or image exploits the parser | type by magic bytes; malware scan first; parsing only in docsandbox (no network, no credential, nsjail limits, seccomp); output re-checked outside; pdfium | DS6–DS9, B1–B5 |
| S2 | EoP | A sandbox escape reaches keys or the bucket | the sandbox container holds neither; it shares only a tmpfs directory with the documents service | DS9 |
| S3 | Spoofing | An upload policy reused, widened or used for another hospital | one key per policy, chosen from the row; 10 minutes; size range; the row must be `uploading`; quarantine prefix only | DS5 |
| S4 | Spoofing | A component other than emr asks for an intake policy, or emr for a row it didn't register | NATS publish rights; the definer records `via = emr`; the row's state and hospital | DS17 |
| S5 | Spoofing / Info | A turned worker reads pages it isn't working on | no storage credential; `page` needs a live lease on a row in `extracting`/`finalising` of that hospital | DS2 |
| S6 | Info | Cross-hospital reads (rows, messages, objects) | RLS; hospital from tokens; subject `<hid>` re-checked; per-hospital KEK and associated data | DS1, DS3 |
| S7 | Info | A leaked bucket credential or a copied bucket | envelope encryption; wrapped keys only in Postgres; the KEK only in KMS or the documents volume | DS3, DS4 |
| S8 | Info | Archive file names leak into logs, messages, metrics, audit | the name only crosses in a definer argument; no column grant; a grep test over a synthetic run | DS10 |
| S9 | Info | Patient names in URLs | search is a POST body; object keys hold ids | DS12 |
| S10 | Info | A reviewer trawls the patient index | lease required; exact MRN or name ≥ 3 with birth year; ≤ 10 results; 20 a review, 200 a day; audited | DS12 |
| S11 | Info | Data kept past exclusion, removal or retention | the purge hook, the sweep, crypto-shred, non-current versions expire, restore runbook | DS15, DS16 |
| S12 | Tampering (clinical) | An unreviewed or wrong value reaches the record | review of every document in phase 1; `raw_records` only from `finalise_document` with a `review_id` (A26); clinician for criticals; rejected values reopen | DS13, DS23 |
| S13 | Tampering (clinical) | Wrong patient | a person always confirms; upload-context conflict blocks; no automatic match on a file name; detach | B9–B12 |
| S14 | Tampering | Prompt injection in a page | pipeline design T4 unchanged; no tools, grammar-constrained, grounding, `suspicious_text` | B7, B8 |
| S15 | Tampering | The archive serves a different file than it listed | SHA-256 from the listing checked before scanning | DS19 |
| S16 | Repudiation | Who uploaded, viewed, searched, corrected, finished, detached or purged can't be shown | §11, the audit row in the same transaction; page views audited before the bytes | DS21, B24 |
| S17 | DoS | Upload floods, huge files, a flood from the archive, GPU starvation | per-user upload limit; queue cap; 5 ingest jobs per hospital; page and pixel caps; the archive's daily cap and back-pressure; `DiscardNewPerSubject` | DS20, DS24 |
| S18 | DoS (safety) | The scanner silently passes files when it's broken | stale signatures or no `clamd` keep documents `scanning`, alert, then `failed` | DS6 |
| S19 | EoP | A platform admin or auditor reads documents | no permission; 404 on every route; jobs view shows ids only | DS11 |
| S20 | Tampering | A stale review screen overwrites a newer one | `version` on every write; the lease | DS14 |

### 14.4 Controls (decided)

- **Authentication and step-up:** Plan 2 sessions; step-up for clearance upload, patient creation (two-step session), detach, and the source-removed decision.
- **Authorisation:** `documents.upload`, `documents.review`, `documents.detach` in core's table; `pipeline.ingest_document` and `pipeline.finalise_document` in the operations table; RLS on every table (§7); definers for every write that crosses a role.
- **Data handling:** per-hospital envelope encryption (§6.2); TLS everywhere; quarantine ≤ 24 hours; retention as §6.3; images never to a cloud model (ADR 0013); never logged as §11.
- **Concurrency:** the scan claim (lease 10 minutes) and the job claim (boundary design); one review lease; `finalise_document` once per review; the search counters and the in-flight cap as single upserts that serialise (reserve, then check, as Analysis departure 44).
- **Secrets:** the bucket credential and KMS identity in the documents service only; the keyring file on its own volume; startup refuses placeholders; the archive key in the emr service's secret store.
- **Third parties and hosting:** the object store and KMS in our cloud account and the data's region; ClamAV signatures fetched through a one-host egress proxy; nothing else leaves our network.

### 14.5 Abuse-case tests

The plan writes these first. B1–B27 from the pipeline design stand.

- **DS1** Another hospital's document id: 404 on every route; an `ingest_document` job for hospital A naming B's document gives `not_found` and reads no page.
- **DS2** A `page` request with a wrong, expired or released `lease_id`, for a document not in `extracting`/`finalising`, or under another hospital's subject: `not_found`, no bytes.
- **DS3** An original copied to another hospital's prefix, another document or another page number fails to decrypt.
- **DS4** After a purge, the object and its older bucket versions can't be decrypted (the wrapped key is gone).
- **DS5** A POST policy used twice, after 10 minutes, for another key, or over the size limit is refused, and a row not `uploading` ignores the upload.
- **DS6** The EICAR string in a PDF and in a TIFF: `malware`. `clamd` stopped, or signatures 49 hours old: the document stays `scanning`, never `ready`, and the alert fires; after 6 hours `failed` `scan_unavailable`.
- **DS7** A DOCX, ZIP and HTML renamed `.pdf`, a PNG declared as PDF: `unsupported_type`. A PDF-JPEG polyglot and a PNG with a trailing payload: the stored pages hold pixels only.
- **DS8** A password-protected PDF: `password_protected`, for every purpose.
- **DS9** Inside docsandbox a test file that tries to open a socket, read another job's directory and allocate 3 GB: no network, no file, killed, `unreadable`; the sandbox's environment holds no credential.
- **DS10** A full synthetic archive run: no synthetic file name in any log, span, metric label, NATS message, dead letter, error row or audit payload.
- **DS11** A platform admin and an auditor get 404 on every document and review route.
- **DS12** Patient search without the lease: 403; a 2-character name or a name without a birth year: 422; more than 10 matches: 10; the 21st search on a review and the 201st a day: 429; the audit row has no query text; no route accepts a name in the query string.
- **DS13** A review with a critical value can't be finished by a records clerk.
- **DS14** Two reviewers saving the same document: the second gets 409 `conflict`; a second reviewer can't open a leased review.
- **DS15** Excluding a patient purges their documents' objects, keys, extractions and decisions within 24 hours and leaves a tombstone without a file name; under a legal hold nothing is deleted until release; the connector doesn't fetch a tombstoned file again.
- **DS16** Removing a hospital destroys its KEK; every object of it is unreadable; removal under a hold is refused.
- **DS17** The emr service can't register a file for another hospital's connection, a disabled one, or a type outside §4.1; can't get a policy for a row it didn't register; holds no grant on any document table.
- **DS18** An archive removal of an unreviewed document cancels it and the 7-day rule deletes it; of a finalised one, flags it and deletes nothing until a hospital admin decides.
- **DS19** An archive file whose SHA-256 differs from the listing: `checksum_mismatch`, nothing stored.
- **DS20** With 301 open reviews the connector registers nothing new, and resumes below 300.
- **DS21** A letter page and a review page are stamped with the viewer's name and time; the audit row commits before the bytes; a refused letter's file is gone within 7 days.
- **DS22** Every served page is at most 900 KB with `no-store`, `inline` and `nosniff`.
- **DS23** No `emr.raw_records` row of `source_kind = document` exists without a finished review (A26 end to end).
- **DS24** The sixth concurrent ingest of a hospital waits as `queued`; a user's upload past the queue cap is 503 `busy`; the emergency stop holds every new job.

### 14.6 Residual risk and sign-off

- **Quarantine bytes are protected only by the provider's encryption,** for at most 24 hours. Accepted: the browser and the emr service can't do our envelope encryption without holding keys, which is worse.
- **The pilot's `file` keyring** keeps hospital KEKs on the documents host's disk. A host compromise reads every hospital's documents. Accepted for the pilot only with the product owner's written acceptance (§16 D5); KMS before a second hospital.
- **A sandbox escape** through a pdfium zero-day reaches only the sandbox's tmpfs. Images are patched by dependabot; the sandbox holds nothing worth taking.
- **ClamAV is a weak signal** against targeted files. The sandbox is the real control; the scan catches the commodity cases and gives an audit point.
- **Stamped pages deter, they don't prevent,** a reviewer photographing a screen. Policy and audit cover it.
- **The review queue is the bottleneck.** If reviewers can't keep up, archive values arrive late (they are mostly historical anyway, EMR §5.3) and the back-pressure holds intake.
- **Backups** keep wrapped keys of purged documents for up to 35 days; the restore runbook re-applies purges, and destroyed KEKs cover removed hospitals.
- **Needs sign-off:** clinical (phase 1 thresholds and pass gate, the clinician-for-criticals rule, suggestion weights); privacy and legal (H5: region, retention values, the file name as a reviewer hint, the source-removed rule); legal (H3: the letter template and letter retention); the product owner (D5).

## 15. What's needed from people

| # | Who | What | Blocks |
|---|---|---|---|
| P1 | Product owner, ops | The object store bucket and KMS in our cloud account and region; the documents service's identity | PR 9's KMS provider; live use (or D5) |
| P2 | Ops | A ClamAV signature mirror or the egress rule for the public one | PR 4 outside development |
| P3 | The pilot hospital with our team | The companion contract implemented (H2) | PR 10 |
| P4 | Clinical lead | Phase 1 thresholds, the pass gate on the locked split, the clinician-for-criticals rule | live use |
| P5 | Privacy lead (H5) | Region, the retention values in §6.3, the file name hint, the source-removed rule | the sweep; live use |
| P6 | Legal (H3) | Letter template and letter retention | live letters |
| P7 | Product owner | GPU host (phase 0 H6) | a real extraction model |

## 16. Decisions for later review

Made on 14 Nov 2026 under the product owner's delegation, each on the safe and private side:

- **D1. A separate Go service,** not core and not the Python service, holds storage and keys (§3).
- **D2. The pipeline worker holds no storage credential;** it reads pages through the documents service with a lease check (§3, §8). Amends pipeline design §3.3.
- **D3. `intake` and `page` are authorised by NATS rights plus database state,** not a core token, because only core mints tokens and neither caller can get one for this (§8).
- **D4. Envelope encryption with a per-hospital KEK and per-object DEK, wrapped keys in Postgres,** so deleting a row is a crypto-shred and destroying a KEK removes a hospital (§6.2).
- **D5. The pilot may use a file keyring** only with the product owner's written acceptance; KMS before a second hospital (§6.2). **Accepted by the product owner on 15 Nov 2026 for the pilot, including on more than one documents server until the move to KMS;** with several servers the keyring is provisioned ahead and copied to each, never created at runtime (TODO.md, documents service items, which also track the move to KMS).
- **D6. POST policies, not presigned PUTs,** for uploads (size range enforced) (§4.2).
- **D7. pdfium in nsjail in its own container with no network;** MuPDF and Ghostscript excluded (§5.1).
- **D8. Stale signatures or no scanner keep documents out,** never pass them (§5.3).
- **D9. Retention takes the shorter value where silent:** pages 30 days after completion, failed or rejected files 7 days, file names 30 days after the review, originals with their canonical rows (13 months) (§6.3).
- **D10. A finalised document removed from the archive is not deleted automatically;** a hospital admin chooses keep or detach. Unreviewed ones are cancelled and deleted (§6.5).
- **D11. Every served page is stamped** with the viewer and time, clinical as well as letters (§10.1).
- **D12. A clinician must finish a review holding a critical value** in phase 1 (§9.1).
- **D13. Patient search is a POST,** with tighter limits than the EMR design gave (20 a review, 200 a day) (§9.2).
- **D14. Archive intake is capped at 100 a day per connection, with back-pressure at 300 open reviews,** and starts from "latest", with no backfill (§4.3, §12).
- **D15. Clearance letters move to the POST-policy upload when live;** the demo keeps multipart (§10).
- **D16. The review routes use `/review/documents/{id}/…`** throughout, renaming the EMR design's `/documents/reviews/{id}/patient` (§10).
- **D17. Excluded patients leave a tombstone** (archive file id and revision, no name) so the file is never fetched again (§6.4).

## 17. Departures in the build

Calls made while building, under the same delegation and on the same side as §16. Each names the PR that made it.

**PR 1 (ADR 0016 and the skeleton, 14 Nov 2026)**

1. **Plan PRs 1 and 2 are built as one PR, and PR 2 is split.** The design and the companion contract had already merged (#182), so PR 1 was only ADR 0016, two amendment notes and the TODO gates: too small to stand alone. This PR is those plus PR 2's service skeleton: the `services/documents/` module and binary, its settings, `documents.v1.ctl.health` behind gokit's request-token checks, the `documents` NATS user (off by default), the compose profile `documents` and the CI job.
2. **The `DOCS` stream, `CORE_DOCUMENTS_ENABLED`, `clamav` and `docsandbox` move to PR 4,** with the scan they serve: the stream with its first durable (`documents_scan`) and the switch that creates it, each sidecar with its abuse tests (DS6, DS9), so nothing lands that no code uses or tests. PR 3 (storage and keys) is next and needs none of them. Until PR 4 the `documents` profile runs the documents service alone, and core has no documents setting: nothing in core calls the service yet.
3. **The `documents` NATS user grows with the build.** PR 1 gives it only what it uses: `documents.v1.ctl.>` in queue group `documents`, replies through `allow_responses`, and no publish right and no inbox at all (it asks nothing yet; `_INBOX_worker.documents.>` comes with the first request it makes). `intake`, `page`, `scanned` and the `DOCS` consumer come with the PRs that use them (4, 7 and 10). Like the emr user, it is an include: `documents.conf` when `NATS_DOCUMENTS_CONF=documents.conf` (with `NATS_DOCUMENTS_PASSWORD`), else `documents-off.conf`, a user nobody can sign in as, needing no variable. Core's `gateway` user gains publish `documents.v1.ctl.>` now, in the dev and the demo configs, so the rights are tested from the start; the demo config has no `documents` user until ops provision its NKey (TODO.md).
4. **Health reports readiness, honestly.** `documents.v1.ctl.health` (as `svc:core` only, 5 s) answers `database_ready`, `keyring_ready`, `scanner_ready` and `sandbox_ready`. The last three are false until PRs 3 and 4 build what they report, so the service never claims it can take a document before it can. The compose healthcheck is off (the image is distroless, with no shell), as for the emr service.
5. **The control contract is protobuf, in gokit,** as the emr service's: `proto/mhai/documents/v1/control.proto`, generated into `services/gokit/docspb` (`make proto`), and the operations table `services/gokit/docops` (who may call each op, its timeout and messages), which core's client (PR 5) and the responder will both read. `services/gokit/reqtoken`'s subject table gains `documents.v1.ctl.<op>`: audience always `documents`, core only (no grant token), no step-up claim, no hospital segment (the hospital is the token's `hid`, checked against the row from PR 3).
6. **The responder repeats the emr responder's checks, in the same order,** in `services/documents/internal/ctl` (size, token, subject binding, kind, reply prefix, replay, op and caller, request id, strict schema). The emr responder is in `services/emr/internal/`, which another module can't import; moving one shared responder into gokit would change the live emr service in a documents PR. TODO.md records the extraction as a follow-up, and both copies are tested against the same abuse cases.
7. **No shared request-token vectors for `documents.v1.ctl`.** The vectors in `testdata/request-tokens` are checked by the Python verifier too, which never receives these tokens; gokit's own tests cover the new rule (mint and verify, wrong audience, grant, shapes).
8. **No migration and no OpenAPI change in PR 1.** The first migration (`documents`, `document_objects`, the roles) is PR 3's; it takes the next free number when it is pushed (00045 at the time of PR 1: main has 00043 from EMR PR 11, and Analysis PR 9 takes 00044). Compose names the service's future login (`documents_app`, `DOCUMENTS_DB_PASSWORD`) and the service starts without it: the pool connects lazily and health reports `database_ready: false`.
9. **The container is locked down from the start:** distroless, non-root, a read-only root filesystem, every capability dropped, `no-new-privileges`, and only the internal `backend` network (Postgres and NATS, no route out). It publishes no port.
10. **Rules the next PRs must keep (security review of PR 1).** The control subject carries no hospital segment, so every op that reads a row takes the hospital from the token's `hid` and compares it with the row's, answering `not_found` otherwise: PR 3 gives `docops.Op` a `HospitalBound` flag that the responder enforces (a token without a hospital is refused on such an op) and a test that another hospital's id is `not_found` on every row-reading op (DS1). PR 3 also refuses to start when the database login is a superuser, has `BYPASSRLS`, or can act as a table owner (`pg_has_role(..., 'USAGE')`), and health checks the same.

**PR 3 (storage and keys, 15 Nov 2026)**

11. **Migration 00046 grants only what PR 3 uses.** `documents` and `document_objects` as §7.1 (composite key `(hospital_id, id)`, `id` unique too; RLS enabled and forced; owner `mhai_migrate`), with two additions: the object key is derived from the row's ids by a CHECK (so it can only hold ids, under the row's own hospital), and `document_objects` rows go with their document (`ON DELETE CASCADE`: deleting the row is the crypto-shred). Roles: `mhai_documents` (SELECT on its hospital's rows; never `source_name`, `uploaded_by` or `patient_hint_id`; no write: the `documents_scan_*` definers come with the scan, PR 4) and `mhai_documents_app` (the login `documents_app`, made by `core migrate up` from `DOCUMENTS_DB_PASSWORD`). §7.2's grants to `mhai_gateway` (PR 5), `mhai_worker` (PRs 6-7), `mhai_emr` (PR 10) and the role `mhai_documents_maint` (PR 9) come with the PRs that use them, as the NATS user grows (departure 3). gokit's `dbx` gains the role `Documents`.
12. **The file keyring, concretely.** A JSON file (`mhai.documents.keyring/1`) of per-hospital keys by version, each with a check value sealed under it (the §6.2 "test unwrap"). It is made empty by `documents keyring init <path>` (never overwriting one); a hospital's KEK is added at its first seal and written (temporary file, 0400, fsync, rename) before it is used, under an exclusive lock on `<keyring>.lock` with the file re-read inside it, so two processes on one keyring never overwrite each other's keys (the second adopts the first's), and a key another process added is found by re-reading the file. A keyring that is missing, readable by group or others, a symlink, malformed, holding a placeholder key (one repeated byte, or all printable text) or failing a check leaves the service running but not ready (`keyring_ready` false; no upload is signed); the file provider in production without `DOCUMENTS_ALLOW_FILE_KEYRING=pilot`, or `DOCUMENTS_KEK_PROVIDER=kms` before PR 9, stops it. Staging may use the file keyring (§6.2 names production only). A wrapped DEK (AES-256-GCM) is bound to its hospital, KEK version and the object's associated data, so a wrapped key copied to another row doesn't open either.
13. **Tink without keysets.** Objects are sealed with tink-go's streaming AEAD primitive (`streamingaead/subtle`, AES-256-GCM-HKDF with HKDF-SHA256, 1 MB segments) keyed directly by the random DEK: the wrapped DEK in Postgres is the only key material, so Tink's keyset format isn't used. The ciphertext is held in memory to give the store its length (25 MB at most).
14. **Our own S3 client, not an SDK.** A small SigV4 client (put, get, head, delete, the lifecycle read) keeps the dependency surface small: it signs every payload, follows no redirect, takes no proxy from the environment, refuses any key outside `quarantine/`, `originals/` and `pages/` or not made of ids, and keeps only status codes from errors. It is tested against MinIO, pinned by digest (the model store's image).
15. **A POST policy can't be made single-use (DS5, in part).** S3 enforces the key, the size range, the type and the expiry (all tested against MinIO, with a widened policy and a changed signature too), but not "used twice": within its 10 minutes a second post overwrites the same quarantine key. What holds instead: one policy names one row's key; the row leaves `uploading` at `/uploaded`; the scan (PR 4) claims only `uploaded` rows, reads the object once and deletes it; anything posted after that is never read and the lifecycle deletes it within a day. `DOCUMENTS_S3_UPLOAD_ENDPOINT` names what browsers post to when it differs from the endpoint the service uses (the signature doesn't cover the host).
16. **`presign_upload` is served now, hospital-bound.** As `svc:core` (no user role reaches it), with departure 10's `HospitalBound` flag: the responder refuses a token whose `hid` isn't a canonical, non-nil uuid, and the handler reads the row under that hospital with RLS, so another hospital's id is `not_found` (DS1). The reply is a closed outcome (`ok`, `not_found`, `conflict` for a row not `uploading`, `unavailable` while the keyring or the store isn't ready); an `archive` row is `not_found` to core (its policy comes through `intake`, PR 10). Core calls it from PR 5.
17. **Health gains `storage_ready`; compose configures no storage yet.** `HealthReply` field 5 is true when the store answers and its lifecycle is in force. The `documents` profile sets no keyring and no object store, so it reports `keyring_ready` and `storage_ready` false and signs nothing; PR 5 adds a development MinIO and the keyring volume with the flow that uses them (TODO.md).
18. **The quarantine lifecycle is deployed by ops and checked by the service.** `deploy/documents/bucket-lifecycle.xml` (quarantine/ within a day, non-current versions within 7 days) is applied with the operator's credential; the service's credential only reads it. Only a rule on exactly `quarantine/` counts (a whole-bucket expiry would delete originals), only a whole-bucket non-current rule without `NewerNoncurrentVersions` counts, and the answer is cached for a minute.
19. **The login check (departure 10), in full.** At start and in every health answer, the login must not be a superuser, `BYPASSRLS`, `CREATEROLE`, `CREATEDB` or `REPLICATION`, a member of `mhai_migrate` or the database's owner, own any schema or relation, or be a member of any other `mhai_` role, and it must reach `mhai_documents`. A failing login stops the service at start; a database that doesn't answer yet doesn't (the pool is lazy, and health says `database_ready` false).

**The provisioned keyring (after PR 3, 15 Nov 2026)**

20. **Keys are provisioned ahead, never made by a server.** The product owner accepted the file keyring for the pilot on more than one documents server (D5). Departure 12's lock serialises processes on one machine only, so two servers could each make a different KEK for one hospital and refuse each other's documents. `DOCUMENTS_KEYRING_MODE` now chooses: `provisioned`, the default and the only mode outside `APP_ENV=development` (anything else stops the service), reads the keyring once, read-only, and never writes it, takes its lock or makes a key; `create` keeps departure 12's behaviour for one development machine. Keys are added on an admin machine with `documents keyring add-hospital <path> <hospital id>` (version 1 with its check value, under the lock, a new file renamed over the old; a hospital that already has a key, or an id that isn't a canonical non-nil uuid, is refused), and the same file is copied to every server, which restarts (services/documents/README.md, "The keyring"). This departs from §6.2's "created when the hospital's first document arrives": a hospital is provisioned before its first document. A hospital without a key gets `unavailable` from `presign_upload` (the handler asks the provider for the token's hospital before reading the row) and from any seal, with a log line naming only the hospital id (at most once an hour per hospital); the keyring stays ready for the others. A provisioned server doesn't re-read the file (not even on an unknown key at unwrap), so what it holds is what it reported at start. Every server logs the keyring's fingerprint, the SHA-256 of the file (what `sha256sum` prints; it reveals nothing of the keys), and its hospital count when it loads and at start, and `HealthReply` gains them as fields 6 and 7 (additive), so a server holding an old copy shows up. The keyring is now opened with `O_NOFOLLOW` and its mode re-checked on the open descriptor, closing the window between the symlink check and the read. The residual risk of §14.6 grows: the keyring sits on every documents server's disk and in each one's backups; accepted with D5 until the move to KMS (TODO.md).

**PR 4 (scanning and the sandbox, 16 Nov 2026)**

21. **Migration 00048 (00047 is #217's) gives the documents service its writes, as definers only.** The lease columns (`scan_lease_id`, `scan_lease_until`, `scan_started_at`) and five `SECURITY DEFINER` functions, `EXECUTE` for `mhai_documents` only, each under the caller's hospital (the subject's `<hid>`, set by `dbx.WithHospital`): `documents_scan_claim` (`uploaded`, or `scanning` with an expired lease, becomes `scanning` with a 10-minute lease; a claim on a row `scanning` for 6 hours whose lease has ended makes it `failed` `scan_unavailable`, §5.3), `_renew`, `_release`, `_quarantine` (a closed reason only) and `_ready` (exactly the original and pages 1..n, the object keys derived in SQL, the archive's SHA-256 unchanged, all in one transaction). The definers' owner gets a policy on the caller's hospital only (FORCE RLS applies to it). A new NOLOGIN role, `mhai_documents_sweep`, held by the login `WITH INHERIT FALSE`, reads ids, status and times of `uploaded` and `scanning` rows across hospitals, nothing else; the login check (departure 19) allows that one extra role.
22. **The 6-hour rule lives in the claim, and the 2-minute republish in the documents service.** §4.2 puts the sweeper that republishes `uploaded` rows in core; core has no documents code until PR 5, so the documents service's own sweeper (every minute, at most 50 rows) scans rows `uploaded` for 2 minutes and rows whose lease has expired directly. The claim is atomic, so a message and the sweeper never scan one row twice. Core's sweeper (PR 5) may still republish; it changes nothing.
23. **A blocked scan releases its lease instead of holding it.** When clamd, its signatures, docsandbox, the hospital's key or the store isn't there (§5.3), the row stays `scanning`, the lease is released at once (so the next attempt claims it without waiting 10 minutes), the message is `Nak`ed with a back-off (30 s doubling to 10 minutes, at most 10 deliveries, then the sweeper takes over), and `documents_scan_blocked` is logged with a reason code (`scanner`, `sandbox`, `keyring`, `storage`) at most once a minute per reason, with a counter. The alert is a log line and a metric until the platform alerting routes it.
24. **No outbox fact yet; the row and `scanned` are the record.** §5 step 6 writes `document.ready` / `document.quarantined` to an outbox in the same transaction. The only outbox today (`intelligence_audit_outbox`) is tied to queries and jobs and drained by core's audit roles; core's `core_documents` consumer (PR 5) writes those audit entries when it reads `documents.v1.<hid>.scanned`, which carries `{document_id, outcome, reason, pages}` (`docspb.Scanned`, with `docspb.Uploaded` for `uploaded`). A lost `scanned` is found from the row (`status`, `scanned_at`).
25. **Lease and storing.** Object keys are fixed by the row's ids, so two scanners storing one document would overwrite each other's objects. A scan runs at most 8 minutes, renews its lease (10 minutes) just before storing, stores nothing if the renewal fails, and deletes what it stored if `_ready` refuses (the objects have no wrapped key anywhere, so they were never readable).
26. **What a missing quarantine object is.** A row marked `uploaded` with nothing at its quarantine key (the browser never posted, or the object went) is `quarantined` `unreadable`: there is no closed reason for "no file", and nothing is lost by refusing it.
27. **The decompression ratio is checked by the renderer, inside the sandbox.** Before pdfium opens a PDF, every `stream … endstream` body that is zlib data is inflated with an output cap of 100 times its length; one that would produce more is `unreadable` (B3). Other filters are left to the jail's memory and CPU limits. A flate-compressed, nearly blank page image can exceed 100:1 and is refused; §4.1 sets the ratio, so this stays until the evaluation shows it costs real documents.
28. **docsandbox, concretely.** A Python server (`services/docsandbox`) on a Unix socket in the jobs tmpfs the two containers share (`docsandbox-jobs`, owned by uid 65532); the docsandbox container has no network at all (`network_mode: none`). It runs as root in the container with `CAP_SYS_ADMIN` (plus `SETPCAP` to drop every capability in the jail, and five others), a read-only root and `no-new-privileges`, but without Docker's seccomp and AppArmor profiles (tested: Docker's default seccomp profile refuses nsjail's `pivot_root`, and the server then answers `unavailable`, so every scan waits; a custom Docker profile narrower than `unconfined` is a follow-up, TODO.md), because nsjail builds a jail (new mount, PID, IPC, UTS and network namespaces, no interface, an empty tmpfs root, `/usr` read-only, no `/proc`) for each file; the renderer runs in it as uid 65532 with no capability, `no_new_privs`, 2 GB of address space, 60 s of CPU, 90 s of wall time, 64 files, 128 MB per written file and an empty environment. nsjail is built from its 3.6 commit, pypdfium2 and Pillow installed by hash. The server never acts on a path in the shared tmpfs: it opens the job's directory, `in` and `out` once with `O_NOFOLLOW`, copies `in` into a fresh staging directory under its private `/run/docsandbox` (root's, not shared), binds only the staging copies into the jail, and copies back only a regular `manifest.json` and `page-<n>.png` into the `out` it opened (security review of this PR: a name swapped for a link between a check and a use, or a linked self-test directory, would otherwise have let root mount, chown or write elsewhere).
29. **The seccomp filter is an allow-list measured from the renderer.** DEFAULT KILL; allowed are the 34 calls strace saw the renderer and the self-test make over every synthetic sample (PDF, PNG, JPEG, TIFF, the refusals and the bombs), plus a small margin acting only on the process itself (signals, time, its own ids and memory); `clone` only without a namespace flag, `clone3` `ENOSYS`, socket calls `EPERM`. An allow-list also closes the x32 ABI and 32-bit `int 0x80` calls (security review of this PR found the first version, a deny-list, could be stepped around through them). A real document that needs a call outside the list is killed and refused `unreadable`, never passed; the list grows by a reviewed PR with the strace that shows the need.
30. **The documents service re-encodes every page.** §5 step 5 decodes each PNG again; the service also encodes it again (Go's `image/png`: IHDR, IDAT, IEND only), opens every output file with `O_NOFOLLOW` and `O_NONBLOCK`, refuses anything but a regular file within its size, and checks the manifest strictly (pages 1..n, at most the purpose's pages, at most 4,000 px a side, 200 megapixels together) before decoding a page. Only these pixels are stored (B5).
31. **clamd, concretely.** `deploy/documents/clamd.conf` (§5 step 3's limits, `StreamMaxLength 30M`, PUA and heuristic alerts), the official `clamav/clamav` image pinned by digest on an internal network (`docscan`: the documents service and clamav only), and a second container of the same image running only `freshclam` into the shared database volume. In development `freshclam` reaches the public mirror directly; elsewhere it goes through an egress proxy that allows only the signature mirror (P2, TODO.md). The age check reads the database date from `VERSION` at every scan; a reply with no date (no database loaded) is stale.
32. **Tests that need privileges run in their own CI job.** The real jail (DS9, B2, the x32 and namespace probes) runs in the docsandbox image's test stage with the privileges compose gives it; the real clamd with EICAR built at run time (alone, in a PDF, after a TIFF header) runs under the Go build tag `sidecars`. Both are in the `docsandbox` CI job and were run on the build server. The documents service and a real docsandbox are not run together in a test (the jobs tmpfs can't be shared between the test process and a container here); each side of the socket protocol is tested against a fake of the other.
33. **NATS rights.** The `documents` user gains publish `documents.v1.*.scanned`, its own durable's `CONSUMER.INFO`, `MSG.NEXT` and `ACK`, and its inbox `_INBOX_worker.documents.>`; still no `uploaded` (core's and the emr service's), no stream or consumer management. Core's `gateway` gains the `DOCS` stream and consumer management and publish `documents.v1.*.uploaded` (§8), in the dev and the demo configs; core sends `uploaded` from PR 5, and the stream's test uses the right now. `DOCS` is 16 MB at most (ids only), and with JOBS, DEAD and EMR leaves the KV buckets 128 MiB of the dev store (1 GB); the demo's 512 MB store is already full with JOBS, DEAD and EMR, so its `max_file_store` must grow before `CORE_DOCUMENTS_ENABLED` is turned on there (TODO.md).
34. **DS6 in part: the real clamd finds EICAR alone and inside a PDF, not inside a TIFF.** Run against the pinned image and the deployed `clamd.conf`: EICAR as a whole file and as a PDF's embedded file is `malware`; EICAR after a TIFF header is clean, because ClamAV matches the test signature only on a whole file or one it extracts, and it doesn't extract a TIFF's contents. The test pins that answer so a change shows up; the scanner's handling of a hit is tested for every type with a fake clamd. This is §14.6's "ClamAV is a weak signal": the sandbox, not the scan, is the control against a crafted image.
