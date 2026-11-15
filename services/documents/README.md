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

## What is built (PR 1: the skeleton)

- `documents serve`: the control responder on `documents.v1.ctl.>` (queue
  group `documents`), answering `documents.v1.ctl.health` as `svc:core`.
  Every request is checked before any handler runs: size, the request token
  (`aud: documents`, the subject table in `services/gokit/reqtoken`: core
  only, no grant token), the reply subject (`_INBOX_gateway.` only), the
  replay set, the op and its caller rule (`services/gokit/docops`; no op
  admits a platform role or an auditor), the request id and a strict
  schema (`services/gokit/docspb`). A refusal gets no reply, one log line
  with its reason code, and a metric.
- Health reports `database_ready`, and `keyring_ready`, `scanner_ready` and
  `sandbox_ready` as false until PRs 3 and 4 build them: the service never
  claims it can take a document before it can.

Storage and keys (PR 3), scanning and the sandbox with `clamav` and
`docsandbox` (PR 4), uploads (PR 5), review (PR 6), ingest (PR 7), finalise
(PR 8), retention (PR 9) and the archive connector (PR 10) come later.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset: production) |
| `DOCUMENTS_NATS_URL` | no credentials in it; `tls://` outside development |
| `DOCUMENTS_NATS_NKEY_FILE` / `DOCUMENTS_NATS_PASSWORD` | exactly one; the password in development only, never a placeholder |
| `DOCUMENTS_DATABASE_URL` | its own login (role `mhai_documents`, PR 3's migration); `postgres://`, with `sslmode=verify-full` outside development |
| `DOCUMENTS_TRUSTED_KEYS_PATH` | the request-token trusted-keys file core writes (public keys), an absolute path |

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

The NATS rights of the `documents` user are tested from core
(`services/core/internal/bus/documents_integration_test.go`), with the real
`services/nats/nats.conf`.
