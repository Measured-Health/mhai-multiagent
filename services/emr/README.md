# The emr service

The EMR integration service ([ADR 0014](../../docs/adr/0014-separate-emr-service-and-document-route.md),
[design](../../docs/specs/2026-11-07-emr-integration-design.md),
[plan](../../docs/plans/2026-11-07-emr-integration.md)). It is the only
process that holds hospital record-system credentials and the only route
out to hospital systems. Core holds neither.

**Off by default.** It runs only under the compose profile `emr`
(`docker compose --profile emr up`), and core talks to it only when
`CORE_EMR_ENABLED=true`.

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

Connections, the secret store, egress, connectors, the normaliser and the
rules come in PRs 3 to 9.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset: production) |
| `EMR_NATS_URL` | no credentials in it; `tls://` outside development |
| `EMR_NATS_NKEY_FILE` / `EMR_NATS_PASSWORD` | one of them; the password in development only |
| `EMR_DATABASE_URL` | the `emr_app` login (migration 00021, PR 3); `sslmode=verify-full` outside development |
| `EMR_TRUSTED_KEYS_PATH` | the request-token trusted-keys file core writes (public keys) |
| `EMR_SEAL_KEY_PATH` | the sealing private key |
| `EMR_SECRETS_DIR`, `EMR_PROXY_URL`, `EMR_PROXY_KEY_PATH`, `EMR_RULE_SET_ACTIVE` | later PRs; empty by default |

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
