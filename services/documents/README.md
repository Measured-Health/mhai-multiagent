# The documents service

Intake, encrypted storage, scanning and page images for uploaded and
archived documents ([ADR 0016](../../docs/adr/0016-separate-documents-service-with-per-hospital-encryption.md),
[design](../../docs/designs/2026-11-14-documents-service.md); the build plan
is the design's §13, and §17 lists where the build departs from it). It
will be the only process that holds the object store credential and the
hospitals' key-encryption keys. Core holds neither, and the pipeline worker
reads pages only through this service.

**Off by default.** It runs only under the compose profile `documents`:

```bash
NATS_DOCUMENTS_CONF=documents.conf NATS_DOCUMENTS_PASSWORD=... docker compose --profile documents up -d
```

The first two switch its NATS user on; without them NATS runs as before and
the user can't sign in (`services/nats/documents-off.conf`).

## What is built

**PR 1, the skeleton.**

- `documents serve`: the control responder on `documents.v1.ctl.>` (queue
  group `documents`). Every request is checked before any handler runs:
  size, the request token (`aud: documents`, the subject table in
  `services/gokit/reqtoken`: core only, no grant token), the reply subject
  (`_INBOX_gateway.` only), the replay set, the op and its caller rule
  (`services/gokit/docops`; no op admits a platform role or an auditor; a
  hospital-bound op needs the token's `hid` to be a hospital id), the
  request id and a strict schema (`services/gokit/docspb`). A refusal gets
  no reply, one log line with its reason code, and a metric.

**PR 3, storage and keys** (design §6, §7; §17 departures 11-19).

- Migration 00046 (`services/core/internal/db/migrations/`): `documents`
  and `document_objects`, RLS enabled and forced; the service reads them as
  `mhai_documents` through its login `documents_app` (a member of
  `mhai_documents_app` only). It refuses to start with a login that is a
  superuser, bypasses RLS, can act as an owner or holds another service's
  role (`internal/dblogin`).
- `internal/kek`: the per-hospital key-encryption keys behind one
  interface, with the `file` provider (`documents keyring init <path>`
  makes an empty keyring, mode 0400; a hospital's key is added at its first
  seal and stored before use). The pilot and development only: refused in
  production unless `DOCUMENTS_ALLOW_FILE_KEYRING=pilot`, and a warning at
  every start.
- `internal/envelope`: each object sealed under a fresh data key with
  Tink's streaming AEAD (AES-256-GCM-HKDF, 1 MB segments), bound to
  `mhai.documents/1|<hospital>|<document>|<kind>|<n>`; a copy anywhere else
  doesn't decrypt.
- `internal/s3`: a SigV4 client for the one bucket (ids-only keys in
  `quarantine/`, `originals/`, `pages/`; no redirects), POST policies (one
  quarantine key, the purpose's size range, the declared type, 10 minutes)
  and the lifecycle check against `deploy/documents/bucket-lifecycle.xml`.
- `internal/storage`: put, open and delete sealed objects; the store's
  readiness.
- `documents.v1.ctl.presign_upload` (`internal/upload`): as `svc:core`, for
  a `clinical` or `clearance` row of the token's hospital that is
  `uploading`; anything else is `not_found`, `conflict` or `unavailable`.
  Core calls it from PR 5.
- Health reports `database_ready`, `keyring_ready` and `storage_ready`
  honestly, and `scanner_ready` and `sandbox_ready` as false until PR 4:
  the service never claims it can take a document before it can.

Scanning and the sandbox with `clamav` and `docsandbox` (PR 4), uploads
(PR 5), review (PR 6), ingest (PR 7), finalise (PR 8), retention and the
KMS provider (PR 9) and the archive connector (PR 10) come later.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset: production) |
| `DOCUMENTS_NATS_URL` | no credentials in it; `tls://` outside development |
| `DOCUMENTS_NATS_NKEY_FILE` / `DOCUMENTS_NATS_PASSWORD` | exactly one; the password in development only, never a placeholder |
| `DOCUMENTS_DATABASE_URL` | its own login (`documents_app`: a member of `mhai_documents_app` only, migration 00046); `postgres://`, with `sslmode=verify-full` outside development |
| `DOCUMENTS_TRUSTED_KEYS_PATH` | the request-token trusted-keys file core writes (public keys), an absolute path |
| `DOCUMENTS_KEK_PROVIDER` | `file`, or unset (no keyring: not ready); `kms` is PR 9's |
| `DOCUMENTS_KEYRING_PATH` | the file keyring, an absolute path (with `file`) |
| `DOCUMENTS_ALLOW_FILE_KEYRING` | `pilot` lets production use the file keyring (the product owner's written acceptance, design D5) |
| `DOCUMENTS_S3_ENDPOINT` | `https://host[:port]` (`http://` in development only); unset with the four below: no store, not ready |
| `DOCUMENTS_S3_BUCKET`, `DOCUMENTS_S3_REGION`, `DOCUMENTS_S3_ACCESS_KEY_ID` | the bucket (`mhai-documents-<env>`), its region, the service's own access key id |
| `DOCUMENTS_S3_SECRET_KEY_FILE` | the file holding the secret access key, an absolute path; a missing, short or placeholder secret stops the service |
| `DOCUMENTS_S3_UPLOAD_ENDPOINT` | optional: what browsers post to, when it isn't `DOCUMENTS_S3_ENDPOINT` |

Any missing, malformed or relaxed setting stops the service. Errors name the
variable, never its value.

## Rules

- The module imports nothing from `services/core/` or `services/emr/`
  (`internal/imports_test.go`).
- Ids, closed codes and counts in messages and logs; never a file name, a
  patient's name, a value, an object key with a name, a POST policy, a
  signed URL or a key (design §11).
- The container: distroless, non-root, read-only root, no capabilities,
  `no-new-privileges`, the internal `backend` network only, no port.

## Tests

Run remotely, never on a laptop:

```bash
cd services/documents
gofmt -l . && go vet ./... && go test -race ./...
go test -race -tags integration ./...
```

The integration tests start Postgres 16 with core's migrations and MinIO
(the model store's image, pinned by digest) in Docker.

The NATS rights of the `documents` user are tested from core
(`services/core/internal/bus/documents_integration_test.go`), with the real
`services/nats/nats.conf`.
