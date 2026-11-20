# The notifications service

Delivers the messages core decides someone is owed: invites, resets,
two-step notices, review reminders, later hospital admin notices and
platform alerts ([ADR 0018](../../docs/adr/0018-separate-notifications-service-without-patient-data.md),
[design](../../docs/designs/2026-11-19-notifications-service.md); the build
plan is the design's §15). It will be the only process that holds the email
provider's credential. Core decides who is told and about what, writing an
ids-only outbox row in the action's own transaction; this service never
chooses a recipient, never reads clinical data and never serves the BFF. No
patient data ever goes into a message.

**Off by default.** It runs only under the compose profile `notifications`:

```bash
NATS_NOTIFICATIONS_CONF=notifications.conf NATS_NOTIFICATIONS_PASSWORD=... NOTIFY_DB_PASSWORD=... \
  docker compose --profile notifications up -d
```

The first two switch its NATS user on; without them NATS runs as before and
the user can't sign in (`services/nats/notifications-off.conf`). With
`NOTIFY_CHANNELS` empty (the default) it sends nothing.

## What is built

**PR 2, the skeleton.**

- `notifications serve`: the control responder on `notify.v1.ctl.>` (queue
  group `notifications`). Every request is checked before any handler runs,
  in the documents responder's order: size, the request token
  (`aud: notifications`, the subject table in `services/gokit/reqtoken`:
  core only, no grant token, no step-up), the reply subject
  (`_INBOX_gateway.` only), the replay set, the op and its caller rule
  (`services/gokit/notifyops`: every op is `svc:core`'s alone), the request
  id and a strict schema (`services/gokit/notifypb`). A refusal gets no
  reply, one log line with its reason code, and a metric. The one op is
  `health`: `database_ready` and the enabled channels' names.
- Settings that fail closed (`internal/config`): errors name the variable,
  never its value; TLS NATS, an NKey seed and `sslmode=verify-full`
  outside development; `NOTIFY_CHANNELS` from a closed set (`log` in
  development only; `email` is refused until PR 4 builds it).
- Its own database role (migration 00052): `mhai_notify`, which holds
  nothing yet but the schema, reached by the login `notify_app` (a member of
  `mhai_notify_app` only, made by `core migrate up` from
  `NOTIFY_DB_PASSWORD`) through `SET ROLE`. The service refuses a login that
  is a superuser, bypasses RLS, can act as an owner or holds another
  service's role (`internal/dblogin`). NT16 (core's
  `internal/db/notify_role_integration_test.go`) checks it holds no
  privilege on any table.
- Its NATS user, an opt-in include, and the compose profile: distroless,
  non-root, read-only root, no capabilities, the internal backend network
  only, no port.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset is production) |
| `NOTIFY_NATS_URL` | `tls://` outside development; no credentials in it |
| `NOTIFY_NATS_NKEY_FILE` | the NKey seed (required outside development) |
| `NOTIFY_NATS_PASSWORD` | development only |
| `NOTIFY_DATABASE_URL` | the `notify_app` login; `sslmode=verify-full` outside development |
| `NOTIFY_TRUSTED_KEYS_PATH` | core's request-token trusted keys (public), an absolute path |
| `NOTIFY_CHANNELS` | empty (nothing is sent), or `log` in development |

## Departures from the design

1. **No egress allow-list entry yet** (§15 PR 2 lists an empty one). The
   service has no route out at all until the email channel (PR 4) needs
   the provider's host; an empty entry would be configuration nothing
   reads.
2. **The off NATS user's NKey is a "nothing up my sleeve" key** (the
   prime-order Ed25519 point with y = 26) rather than a random key whose
   seed was thrown away: nobody can know its private key, and it isn't a
   high-entropy string the secret scanner must be told to ignore. (The
   all-zero key was rejected in review: it is a small-order point, for
   which an Ed25519 signature can be forged.)
