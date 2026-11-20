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

**PR 3, the outbox and deliveries** (design §6.2, §6.3, §9; migration
00053).

- Core's outbox: #228's `account_messages` is renamed `message_outbox`
  (the old name stays a security-invoker view for one release) and gains
  `channel` email, `dedupe_key` and `published_at`. Rows are still
  written only in the action's own transaction, ids only.
- Core's drainer (`services/core/internal/notify`, only with
  `CORE_NOTIFY_ENABLED=true`, which also makes the `NOTIFY` stream and the
  `notify_send` durable): after each commit (a kick from the sender) and
  every 30 s it publishes each due row's id as `notify.v1.<hid>.due`
  (`Nats-Msg-Id` = the id) and marks it published, as `mhai_notify_outbox`
  through three definers. It deletes outbox rows after 90 days.
- This service's sender (`internal/deliver`, only with a channel in
  `NOTIFY_CHANNELS`): `notify_claim` reserves the one delivery per message
  and channel and then checks it, under the subject's hospital; a
  deactivated recipient is suppressed; per-recipient (10 an hour) and
  per-hospital (200 an hour, mandatory kinds aside) allowances are
  reserved by one upsert each; the global pace is 5 a second; 10 failures
  in a row pause sending for 5 minutes; failures retry after 1 min, 5 min,
  30 min, 2 h, then every 6 h, and after 24 hours are failed and
  dead-lettered (`dead.v1.<hid>.notify_send`). Deliveries are kept a year,
  counters two days.
- Channels: `log` only (development). It logs kind and ids, never an
  address.

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
| `NOTIFY_RATE_PER_SECOND` | the global pace (default 5, 1 to 50) |
| `NOTIFY_HOSPITAL_PER_HOUR` | one hospital's messages an hour, mandatory kinds aside (default 200) |
| `NOTIFY_RECIPIENT_PER_HOUR` | one person's messages an hour (default 10) |

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
3. **The outbox keeps #228's kinds, and `hospital_id` stays NOT NULL.**
   Each later kind is added with the PR that sends it. A nullable hospital
   without the platform policy would be an RLS gap; it comes with the first
   platform kind (PR 6 or 8).
4. **`dedupe_key` is unique per hospital** (not globally), so one
   hospital's key can never collide with, and so reveal, another's. Its
   shape is checked: a kind and ids only.
5. **`message_deliveries` keeps `kind` and `recipient_id` and has no
   foreign key to the outbox.** Outbox rows go after 90 days and deliveries
   after a year; the delivery must still answer "was I told?". Rows are
   made only by `notify_claim`, which checks the outbox row exists.
6. **The drainer never publishes invite or password_reset rows.** Those go
   by `send_link` with their link in memory (PR 5); their outbox rows are
   records only. It also skips rows more than 24 hours old, so switching
   `CORE_NOTIFY_ENABLED` on doesn't send a backlog of stale reminders.
7. **More definers than §6.3's two:** the drainer's three
   (`mhai_outbox_due`, `mhai_outbox_published`, `mhai_outbox_retention`, as
   `mhai_notify_outbox`) and `notify_retention`. Each does one thing and
   is executable by one role.
8. **`notify_claim` returns the address only for the email channel.** The
   log channel never sees one.
9. **A per-recipient hourly cap in the service** (10, configurable) on top
   of core's per-action limits.
10. **The global pace is per process.** With more than one instance, set
    `NOTIFY_RATE_PER_SECOND` to the provider's rate divided by the count.
11. **Failed mandatory sends are not yet on the audit chain**
    (`notify.delivery_failed`): this service can't write to it. Core will
    record it in PR 5, where it learns of a failed link send directly.
12. **Dead letters go to `DEAD`, but nothing shows them yet.** Core's
    `core_dead` consumer acks and ignores them (not a job type). The
    platform's Failed jobs view reads them later (PR 8).
