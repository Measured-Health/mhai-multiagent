# nats

Configuration for the NATS server, the only link between the core service
(`services/core/`, Go) and the Python workers (`services/intelligence/`).
Compose mounts `nats.conf` read-only at `/etc/nats/nats.conf`, and the core
service's integration tests copy the same file into their NATS container
(`services/core/internal/testutil/nats.go`), so the tests exercise the real
permissions.

## Users

Each service has its own NATS user and may do only what its job needs.

| User | Service | May |
|---|---|---|
| `gateway` | the core service | publish `inference.v1.>` and `jobs.v1.>`, use the KV subjects and the JetStream management it needs (no stream delete or purge); subscribe to its own inbox and `dead.v1.>` |
| `worker` | the Python workers | subscribe to `inference.v1.>` and its own inbox; publish only replies (`allow_responses`, one per request), `dead.v1.>` and pull-consumer calls on the `JOBS` stream; no KV access |

The core service's user is still named `gateway`. The name predates the
rename to `core` and is kept on purpose, so the security config, the
credentials and the tests stay as they are.

## Inbox prefixes

Each service receives replies on its own prefix instead of the shared
`_INBOX.>`: `_INBOX_gateway` for the core service and `_INBOX_worker` for
the workers (set with `CustomInboxPrefix`). A user may subscribe only to its
own prefix, so no service can read or forge another's replies.

## Development credentials

In development both users have passwords, read from the environment and
never from this directory or the repository:

- `NATS_GATEWAY_PASSWORD`
- `NATS_WORKER_PASSWORD`

Use letters and digits only, because the config parser treats other
characters as syntax. Compose refuses to start NATS with the placeholder
values from `.env.example`, and the core service refuses them too.

## Production: NKeys

In production, replace each user's `password:` with `nkey: <public key>`:

1. Generate a key pair per service with `nk -gen user -pubout`.
2. Put the public key in `nats.conf`.
3. Give the service only its seed file, through a secret mount. The core
   service reads it from `NATS_NKEY_SEED_PATH`.

Also enable the `tls {}` block in `nats.conf`, and keep the monitoring port
(8222) off the network.
