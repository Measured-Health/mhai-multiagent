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

**PR 4, the email channel** (design §4.3, §4.7, §8.2, §12.1; the product
owner's 20 Nov decision: send from our own mailbox over SMTP submission).

- `internal/templates`: one fixed template per kind, compiled in. The only
  variables are the link, its expiry and (invitations only) the hospital's
  name; a test fails the build if a template names anything else (NT1).
  Subjects are fixed and say only that something is waiting. Links must be
  on `NOTIFY_APP_URL`'s host (NT5); no images, remote content or tracking
  (NT4); every email says MHAI never asks for a password or code by email.
  A hospital name with a line break, control character or angle bracket
  is refused, and the HTML is escaped (NT11).
- `internal/email`: SMTP submission over implicit TLS (465) or STARTTLS
  (587), the server's certificate always verified (`NOTIFY_SMTP_CA_FILE`
  adds a private CA; verification is never off); a 587 server without
  STARTTLS is refused before anything is sent. AUTH PLAIN with the user and
  the password from `NOTIFY_SMTP_PASSWORD_FILE` (never the environment,
  never logged). The From and Reply-To are settings; the To is the
  recipient's address read at send time. A refused recipient (5xx) fails
  at once; anything else is retried by the sender. Only the SHA-256 of
  the Message-ID is stored.
- Tested against an in-process SMTP server with a certificate made at run
  time (`internal/testsmtp`), never a real mailbox.

**PR 5, invites, resets and reminders go out** (design §4.4, §6.3, §9.4;
migration 00054).

- Core: `CORE_DELIVERY=notify` (allowed in production; needs
  `CORE_APP_URL` and `CORE_NOTIFY_ENABLED=true`). An invite or a reset goes
  at once by `notify.v1.ctl.send_link` with its link in the request body
  only (`internal/notifyctl`; request/reply, never JetStream); anything but
  `accepted` answers the admin's action with 424 `not_delivered` (the
  invite or reset stands; 503 `no_delivery` stays for no sender, nothing
  changed). Every other kind (two-step notices and
  reminders, review assignments and reminders) is written to the outbox
  with channel `email` and delivered by PR 3's sender.
- This service: `send_link` (hospital-bound) sends only to a user named by
  `notify_link_recipient`: an active user of the token's hospital holding
  a live token of that purpose issued in the last 2 minutes (NT6, NT8). 30
  links a minute per hospital; the recipient's hourly allowance (mandatory
  kinds skip the hospital's cap); 3 tries in about 25 seconds. The link is
  never logged or stored.

**Security notices** (the product owner, 24 Nov 2026; migration 00058).

- The account's own address is told when its password is changed (by the
  person) or set with a reset link, two-step is turned on, moved to a new
  authenticator app or reset by a hospital admin (the existing
  `mfa_reset`), a backup code is used (at sign-in or as a step-up), or new
  backup codes are made. One kind per event: `security_password_changed`,
  `security_password_reset`, `security_mfa_enabled`,
  `security_mfa_replaced`, `security_backup_code_used`,
  `security_backup_codes_made`.
- Core records each in the change's own transaction (a savepoint, so a
  failure to record never blocks or undoes the change; it is logged by
  ids) through `mhai_security_notice`, the only way `mhai_auth` reaches the
  outbox: security kinds only, the recipient's hospital read from the
  database, nothing for a platform account (no hospital yet) or an inactive
  one. A rolled-back change leaves no notice. At most one per kind, account
  and calendar minute (the dedupe key); later ones in that minute are
  dropped. With no sender (`CORE_DELIVERY` unset) none is recorded. An
  invite's first password and a TOTP sign-in or step-up tell nobody.
- They are mandatory kinds (the hospital's hourly cap doesn't hold them;
  the recipient's 10 an hour does: an eleventh waits for the next hour,
  never dropped). There is no email opt-out yet; when one comes, these
  kinds must stay outside it.
- The template states what changed, when (UTC, from the outbox row's
  `created_at`, which `notify_claim` now returns), that it is about the
  clinical app, and "If this wasn't you, contact your hospital
  administrator or info@measured-health.org straight away." It carries
  no link of any kind, no code, secret, address or patient data, and no
  tracking; From and Reply-To are the settings, as for every email.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset is production) |
| `NOTIFY_NATS_URL` | `tls://` outside development; no credentials in it |
| `NOTIFY_NATS_NKEY_FILE` | the NKey seed (required outside development) |
| `NOTIFY_NATS_TLS_CA_FILE`, `NOTIFY_NATS_TLS_CERT_FILE`, `NOTIFY_NATS_TLS_KEY_FILE` | the private CA and this service's NATS client certificate (NATS verifies one): all three or none, absolute paths; required outside development |
| `NOTIFY_NATS_PASSWORD` | development only |
| `NOTIFY_DATABASE_URL` | the `notify_app` login; `sslmode=verify-full` outside development |
| `NOTIFY_TRUSTED_KEYS_PATH` | core's request-token trusted keys (public), an absolute path |
| `NOTIFY_CHANNELS` | empty (nothing is sent), `log` (development only), `email` |
| `NOTIFY_APP_URL` | with email: the app's https address (core's `CORE_APP_URL`); every link must be on it |
| `NOTIFY_SMTP_HOST`, `NOTIFY_SMTP_PORT` | with email: the submission server; the port is 465 (implicit TLS) or 587 (STARTTLS), nothing else |
| `NOTIFY_SMTP_USERNAME`, `NOTIFY_SMTP_PASSWORD_FILE` | with email: the mailbox's sign-in; the password only from a file (an absolute path); `NOTIFY_SMTP_PASSWORD` is refused |
| `NOTIFY_SMTP_FROM`, `NOTIFY_SMTP_REPLY_TO`, `NOTIFY_SMTP_FROM_NAME` | with email: bare addresses; the From name defaults to MHAI |
| `NOTIFY_SMTP_CA_FILE` | optional: extra trusted roots for the server's certificate |
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
11. **Failed mandatory sends are not on the audit chain**
    (`notify.delivery_failed`): this service can't write to it, and core
    learns of a failed link only after the action's transaction has
    committed. The action's own audit row is there, core logs the failure
    by ids, and the admin gets 424 `not_delivered` and can send again.
12. **Dead letters go to `DEAD`, but nothing shows them yet.** Core's
    `core_dead` consumer acks and ignores them (not a job type). The
    platform's Failed jobs view reads them later (PR 8).
13. **The provider is our own mailbox over SMTP submission** (the product
    owner, 20 Nov), not one of §12.3's services. No provider API, events
    or idempotency keys yet: one duplicate is possible after a crash
    between the server's acceptance and our record (§16.6).
14. **The dev compose profile doesn't wire SMTP.** Development uses the
    `log` channel. The demo deploy does (`deploy/compose.yml`, profile
    `notifications`; deploy/README.md, "Turn on email for the demo"): the
    `NOTIFY_SMTP_*` settings from `.env`, the password as a mounted 0400
    file, and `mail-relay` as the only route out (TCP to the SMTP host and
    port alone) in place of the design's egress-proxy entry, since SMTP
    submission isn't an HTTPS CONNECT. On the demo the service runs as
    development, as core does there (NATS and Postgres without TLS on one
    host).
15. **The From domain isn't checked against a separate sending domain**
    (§12.1): with our own mailbox, the From is the mailbox's own address.
    SPF, DKIM and DMARC on that domain are a people item (TODO.md).
16. **`send_link` tries 3 times in about 25 seconds** (design: 60), so
    the admin's request answers within core's 30-second control timeout.
17. **No `invite_delivery` on `StaffOut` yet** (§13): a link's outcome is
    the admin's own reply (200 or 424 `not_delivered`), and invites leave no
    delivery row. The staff screen's field comes with mhai-web's change.
18. **A failed link's token isn't revoked at once.** Nobody received it;
    the admin's "send again" mints a new token and revokes the old one, as
    #229 does.
19. **The 30-a-minute `send_link` limit is per process.** With more than
    one instance it scales with the count; the per-person hourly cap is in
    the database and holds across instances.
20. **A hospital name unfit for a template is refused at send, not at
    save** (NT11): the invite then answers 503 and the name must be fixed.
