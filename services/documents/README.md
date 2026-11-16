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
  makes an empty keyring, mode 0400). The pilot and development only:
  refused in production unless `DOCUMENTS_ALLOW_FILE_KEYRING=pilot`, and a
  warning at every start. See [The keyring](#the-keyring) for how keys are
  provisioned.
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

**The provisioned keyring** (design §17 departure 20; TODO.md "One keyring
shared by several documents servers").

- `DOCUMENTS_KEYRING_MODE=provisioned`, the default and the only mode
  outside development: the service reads the keyring once, read-only, and
  never writes it, takes its lock or makes a key, so several documents
  servers can share one keyring. `create` (PR 3's behaviour: a hospital's
  key made at its first document) is for `APP_ENV=development` on one
  machine.
- `documents keyring add-hospital <path> <hospital id>` adds a hospital's
  key on the admin machine.
- A hospital without a key gets `unavailable` from `presign_upload` (and
  from any seal), with one log line naming the hospital id (at most once
  an hour per hospital); the service stays ready for the others.
- The keyring's fingerprint and hospital count are logged when it loads
  and at start, and reported by health (`keyring_fingerprint`,
  `keyring_hospitals`).

PR 4, the scan (design §5; §17 departures 21-33):

- The `documents_scan` durable on `DOCS` (core makes both when
  `CORE_DOCUMENTS_ENABLED=true`) and a sweeper (every minute) claim an
  `uploaded` row under its hospital (migration 00048's definers), read the
  quarantine object's size, then its bytes, check the magic bytes against the
  declared type and purpose and an archive file's SHA-256, scan with clamd
  (`INSTREAM`, after checking the signatures are at most 48 hours old),
  render in docsandbox (`services/docsandbox`: pdfium and Pillow under
  nsjail), re-check and re-encode every page here, seal and store the
  original and the pages and mark the row `ready`, delete the quarantine
  object and publish `documents.v1.<hid>.scanned`.
- A refusal is `quarantined` with its closed reason; nothing is stored.
- Fail closed: without clamd, fresh signatures, docsandbox, the hospital's
  key or the store, the row stays `scanning` (logged
  `documents_scan_blocked`), the message is retried with a back-off and
  the sweeper retries; after 6 hours the row is `failed`
  `scan_unavailable`.
- Health reports `scanner_ready` and `sandbox_ready`.

Uploads (PR 5), review (PR 6), ingest (PR 7), finalise (PR 8), retention
and the KMS provider (PR 9) and the archive connector (PR 10) come later.

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
| `DOCUMENTS_KEYRING_MODE` | `provisioned` (default: read-only, never makes a key) or `create` (`APP_ENV=development` only); only with `file` |
| `DOCUMENTS_ALLOW_FILE_KEYRING` | `pilot` lets production use the file keyring (the product owner's written acceptance, design D5) |
| `DOCUMENTS_S3_ENDPOINT` | `https://host[:port]` (`http://` in development only); unset with the four below: no store, not ready |
| `DOCUMENTS_S3_BUCKET`, `DOCUMENTS_S3_REGION`, `DOCUMENTS_S3_ACCESS_KEY_ID` | the bucket (`mhai-documents-<env>`), its region, the service's own access key id |
| `DOCUMENTS_S3_SECRET_KEY_FILE` | the file holding the secret access key, an absolute path; a missing, short or placeholder secret stops the service |
| `DOCUMENTS_S3_UPLOAD_ENDPOINT` | optional: what browsers post to, when it isn't `DOCUMENTS_S3_ENDPOINT` |
| `DOCUMENTS_CLAMD_ADDR` | clamd's `host:port` (compose: `clamav:3310`); unset: the scanner is not ready and every scan waits |
| `DOCUMENTS_SANDBOX_SOCKET`, `DOCUMENTS_SANDBOX_JOBS` | docsandbox's Unix socket and the jobs tmpfs both containers mount, absolute; both or neither (neither: every scan waits) |

Any missing, malformed or relaxed setting stops the service. Errors name the
variable, never its value.

## The keyring

The `file` keyring holds every hospital's key-encryption key (KEK). Losing
it makes every document unreadable; a copy in the wrong hands reads every
document. With more than one documents server (the pilot, design D5), the
keyring is made ahead on one admin machine and the same file is copied to
every server. A server never makes a key, so two servers can never hold two
different keys for one hospital.

**Provision** (on the admin machine, as the user that will own the file;
the image runs these as `documents keyring ...`):

```bash
documents keyring init /secure/keyring.json            # once; never overwrites
documents keyring add-hospital /secure/keyring.json <hospital id>
```

`add-hospital` takes the hospital's id as core has it (a lower-case uuid),
refuses a hospital that already has a key (a key is never replaced: the
hospital's documents would be lost) and an id in any other form, and writes
a new file (mode 0400, fsynced) that it renames over the old one, so the
keyring is never half written. It prints the new fingerprint. Run it before
the hospital's first document; until the copy reaches the servers the
hospital's uploads are `unavailable`.

**Copy to each documents server**, onto the volume only the documents
container mounts. Copy to a temporary name in the same directory and rename,
so the server never sees half a file:

```bash
install -m 0600 -o <service uid> -g <service gid> keyring.json /keyring/.keyring.new
mv /keyring/.keyring.new /keyring/keyring.json
```

The file must be a regular file (not a symlink) readable by its owner only
(0600 or 0400) and owned by the user the container runs as (distroless
`nonroot`, uid 65532); anything else leaves the keyring not ready. Move it
over a channel only you control (scp to the host as an operator), never
through chat, email or a ticket, and keep its backup encrypted with the
operator's `age` key (design §6.2).

**Restart each server.** A provisioned server reads the keyring once; a copy
changed under it is picked up only by a restart.

**Check the fingerprints match.** The fingerprint is the SHA-256 of the
file, so on the admin machine and on each host:

```bash
sha256sum /keyring/keyring.json
```

Each server logs it when the keyring loads and at start
(`"msg":"documents: keyring loaded"` and `"documents serving"`, fields
`keyring_fingerprint` and `keyring_hospitals`), and health reports both.
Every server must show the admin machine's value; a different one is a
server holding an old copy, which answers `unavailable` for the hospitals
it lacks. The fingerprint reveals nothing of the keys; the file itself
must never be printed, attached or pasted.

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
