# The emr service

The EMR integration service ([ADR 0014](../../docs/adr/0014-separate-emr-service-and-document-route.md),
[design](../../docs/specs/2026-11-07-emr-integration-design.md),
[plan](../../docs/plans/2026-11-07-emr-integration.md)). It is the only
process that holds hospital record-system credentials and the only route
out to hospital systems. Core holds neither.

**Off by default.** It runs only under the compose profile `emr`, with its
NATS user switched on, and core talks to it only when `CORE_EMR_ENABLED=true`:

```bash
NATS_EMR_CONF=emr.conf NATS_EMR_PASSWORD=<letters and digits> docker compose --profile emr up -d
```

Without `NATS_EMR_CONF` NATS includes `services/nats/emr-off.conf` (no
variable needed, nobody signs in as emr), so today's dev setups run as
before. **The demo has neither the emr NATS user nor the emr service yet:**
both come in a later PR, once ops provision `NATS_EMR_NKEY` on the host
(TODO.md, people/ops item); an undefined NKey variable would stop the demo's
NATS.

## What is built (PR 2: the skeleton)

- `emr serve`: the control responder on `emr.v1.ctl.>` (queue group `emr`),
  answering `emr.v1.ctl.health` as `svc:core`. Every request is checked
  before any handler runs: size, the request token (`aud: emr`, the
  subject table in `services/gokit/reqtoken`), the reply subject
  (`_INBOX_gateway.` only), the replay set, the op and its caller rule
  (`services/gokit/emrops`), the request id and a strict schema. A refusal
  gets no reply, one log line with its reason code, and a metric.
- `emr keys seal-init --out <dir>`: the X25519 sealing key pair core seals
  credentials to (design §6.2).

## What is built (PR 3: connections)

- `emr.v1.ctl.put_credentials` (platform admin; the token must carry
  core's `stp` claim, a checked step-up code): opens each sealed box
  (`services/gokit/seal`) against the connection's current credential
  origin, read under the token's hospital, and stores every field or none;
  the reply is each field and its set time.
- `emr.v1.ctl.wipe_credentials` (platform admin): every secret of the
  connection, then its metadata rows; idempotent.
- `emr.v1.ctl.test` (platform admin): `sign_in_refused` without calling
  the connector unless every required secret is set, and usable, for the
  origin the saved addresses give now (design §6.3, A28); otherwise the
  connector's read-only probe, 20 s.
- The file secret store (`EMR_SECRETS_DIR`): development and staging only,
  0400 files in 0700 directories owned by the service's user; the previous
  version is kept 24 hours. Production needs the managed store (plan Task
  12, waiting on H3). The database holds metadata only (`emr.credentials`).
- The connector interface and the development `stub` (registered only with
  `APP_ENV=development`); `Creds` never print, and `TestNoConnectorLogsCreds`
  fails any connector code that hands them to a logger.
- The service refuses to start without a sealing key.

Egress, the real connectors, the normaliser and the rules come in PRs 4
to 9.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset: production) |
| `EMR_NATS_URL` | no credentials in it; `tls://` outside development |
| `EMR_NATS_NKEY_FILE` / `EMR_NATS_PASSWORD` | one of them; the password in development only |
| `EMR_DATABASE_URL` | the `emr_app` login (migration 00020, PR 3); `sslmode=verify-full` outside development |
| `EMR_TRUSTED_KEYS_PATH` | the request-token trusted-keys file core writes (public keys) |
| `EMR_SEAL_KEY_PATH` | the directory of the sealing private keys (`seal-<kid>.key`) |
| `EMR_SECRETS_DIR` | the file secret store (development and staging only); empty: no store, the credential ops answer nothing |
| `EMR_PROXY_URL`, `EMR_PROXY_KEY_PATH`, `EMR_RULE_SET_ACTIVE` | later PRs; empty by default |

Errors name the variable, never its value.

## Rules

- The module imports nothing from `services/core/`
  (`internal/imports_test.go`).
- Ids, closed codes and counts in messages and logs; never a name, an
  MRN, an address, a value or a credential.

## Tests

Run remotely, never on a laptop (see the plan's "Running the tests"):

```bash
cd services/emr
go vet ./... && go test -race ./...
go test -race -tags integration ./...   # Postgres with core's migrations (internal/testpg)
```
