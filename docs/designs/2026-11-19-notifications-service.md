# Notifications service: in-app items, email first, no patient data outbound

- **Status:** Proposed, for the product owner's review. Nothing here is built. Each call below takes the safer side first and gives its reason; §18 lists them for review. The email provider is **not** chosen here: it is the product owner's decision N1 (§12, §17). Recorded in [ADR 0018](../adr/0018-separate-notifications-service-without-patient-data.md).
- **Date:** 2026-11-19
- **Builds on:**
  - [ADR 0010](../adr/0010-api-in-go-models-in-python-over-nats.md) and [ADR 0011](../adr/0011-frontend-ingress-over-nats.md): Go owns the rules, NATS is the only link between services, the BFF reaches core only;
  - [ADR 0014](../adr/0014-separate-emr-service-and-document-route.md) and [ADR 0016](../adr/0016-separate-documents-service-with-per-hospital-encryption.md): a separate Go service holds what core shouldn't (outside credentials, keys);
  - the [frontend requirements](../specs/2026-10-17-frontend-requirements.md) §9 (alerts and `/me/notification-settings`), §11 (correction decisions in the bell), §14.7 (the platform bell and analytics email, Plan 7d) and mhai-web's notifications design (17 Oct);
  - the [EMR integration design](../specs/2026-11-07-emr-integration-design.md) §12 (who sees an alert, fatigue controls), §17.6 (the hospital's critical-result call process stays primary) and open question 12 (no email or SMS for criticals from MHAI in v1);
  - PRs #228 and #229 (in review): `internal/delivery` (the `Sender` interface, the `log` sender, the `account_messages` outbox, 503 `no_delivery`) and `account_tokens` (single-use invite and reset links, SHA-256 at rest).

## 1. Why

Several built features owe someone a message and have no way to send it:

1. **Account messages** (#228, #229): invites, password resets, two-step reset notices and reminders, reviewer assignments and reminders. Outside development every one answers 503 `no_delivery`, because there is no sender. Without invites and resets, Measured Health creates every pilot account by hand with `core users create`.
2. **Hospital admin notices:** an analyst grant (`analysis.NotifyAnalystGranted` records ids only), clearance letter reminders and the "ended" notice (`clearance.reminder_sent` with `delivery: pending`).
3. **The platform bell and email** (Plan 7d): `GET /platform/notifications` and `/seen` answer 501; the analytics email setting is stored but nothing is sent; a Broken audit chain should ring the bell and email.
4. **Operator alerts:** the cloud budget at 80%, settle failures, `auth_failed`, a paused EMR source, dead letters. Today they are metrics that nothing scrapes.
5. **EMR alerts:** the clinician bell and pop-ups are built and served by core. Whether anything goes outside the app is open.

The pilot at one Nigerian hospital needs (1) soon. The rest can follow.

## 2. Goals, non-goals, anti-goals

**Goals**
- One path for every message core owes a person: recorded in the action's transaction, delivered after commit, retried, idempotent, rate-limited and auditable.
- Email first, behind a channel interface, so SMS or WhatsApp can be added later without touching core.
- The provider's credential outside core.
- A design the pilot can use after four small PRs, with the provider still swappable.

**Non-goals**
- Choosing the email provider (N1) or deciding whether clinical alerts go outside the app (N4).
- Push notifications to phones (no mobile app yet), web push, and an on-call pager. Operator paging stays Alertmanager's job (TODO.md, "Metrics and dashboards"); §8.4 covers the interim.
- The mhai-web screens; §13 lists what they call.

**Anti-goals: failure even if it works**
- A patient's name, MRN, result, diagnosis or any clinical text in an email, SMS, WhatsApp message, push payload, subject line or provider log.
- A reset or invite link stored anywhere at rest on our side, or rewritten by a provider's click tracking.
- A message to an address that isn't a current user's own, chosen by request data.
- MHAI presented as the channel a critical result relies on.
- A broken provider that makes an action look sent when it wasn't.

## 3. What exists today

| Piece | Where | State |
|---|---|---|
| Clinician bell, pop-ups, `/notifications` page (alerts and corrections tabs) | mhai-web `apps/clinical` (`lib/notifications/*`, `lib/api/live-alerts.ts`) | built; polls core |
| `GET /alerts`, `/alerts/unread-count`, acknowledge, dismiss | core `internal/alerts`, migration 00029 (`alerts`, `alert_deliveries.read_at`, `last_notified_at`) | built, behind the EMR switches |
| `GET`/`PATCH /me/notification-settings {alert_popups, correction_popups}` | core, `user_notification_settings` | built; pop-ups only |
| Correction decisions in the bell | `GET /corrections/mine` polling | built |
| Platform bell (`GET /platform/notifications`, `POST …/seen`) | core `internal/platform` | 501 `not_implemented` |
| Platform analytics email mode (`off`, `critical`, `critical_and_digest`) | core, `platform_email_prefs` | stored, nothing sent |
| Account message outbox and `Sender` | #228: `internal/delivery`, `account_messages` (00049) | in review; `log` sender only |
| Single-use links | #229: `account_tokens` (00050), `POST /auth/password/reset` | in review |
| Operator alerts | metrics (`mhai_cloud_alerts_total`, `mhai_cloud_settle_failures_total`, breaker signals) | nothing scrapes them |

So **in-app delivery for clinicians already works** through core. What is missing is everything that leaves the app, and the inbox for the platform and hospital admins.

## 4. The decisions

Each lists the safer option first, then why.

### 4.1 In the app first; email next; SMS and WhatsApp later

- **The bell stays in core and is the primary channel for everything clinical.** It is behind sign-in, two-step, RLS and the session idle limit; it already works; and it is the only place patient details may appear.
- **Email is the first outbound channel**, for messages that can't be in-app because the person can't sign in yet (invites, resets) or isn't looking (platform and operator alerts, reminders). It carries a link, never content (§4.3).
- **SMS and WhatsApp are later, each behind its own small design (N5).** They are common in Nigeria and reach people email doesn't, but: we hold no phone numbers (a new staff data field, with verification and a lawful basis); SMS routes need a registered sender ID and must avoid the DND-filtered promotional routes; WhatsApp needs Meta business verification and pre-approved templates; and SMS is readable on a locked screen by anyone holding the phone. None of this is needed for the pilot's first messages. The channel interface (§12.1) is built so they slot in.
- **Clinical alerts stay in the app for the pilot.** The hospital's own critical-result call process stays primary (EMR design §17.6, open question 12). An optional "you have an unhandled critical alert" nudge is designed (§8.3) but built last and off, waiting for the clinical lead (N4): sending to every queue clinician at night is alert fatigue, and sending to fewer needs a duty roster we don't have.

### 4.2 A separate Go service over NATS; core decides, the service delivers

- **`services/notifications/`** (Go, own module and binary, imports `services/gokit/`), as ARCHITECTURE.md already plans. It holds the provider credential and nothing else of value: no clinical tables, no sessions, no audit chain, no signing key.
- **Core decides who is told and about what,** inside the action's own transaction (the outbox, §9.1). The service never chooses a recipient, never reads clinical data and never serves the BFF.
- **The in-app inbox stays in core** (§6.1), because the BFF reaches only core (ADR 0011) and the inbox is read under the session and RLS like every other screen.
- **Why not inside core:** the simplest option, and #228's `Sender` would allow it. But a provider credential in core lets a core compromise send mail as us (phishing that passes DMARC), a slow provider would hold core's request goroutines, and bounce handling needs an inbound path core shouldn't have. ADR 0014 and 0016 moved credentials out of core for the same reason. The service is small: one consumer, one request handler, one table.
- **Why not in Python:** security rules are written once, in Go (ADR 0010).

### 4.3 No patient data outbound, by construction

- **Every outbound message is a fixed template per kind** (§8), compiled into the service. The message on the bus carries ids only; core never sends text, and there is no free-text field to put PHI in.
- **Template variables are an allow-list per kind:** the link, its expiry time, the hospital's display name (account messages only, so the person knows which hospital invited them), and a fixed count word for digests on the platform side. No recipient name (saves a mail-merge of PII and a header-injection surface), no patient field, no finding, no correction text.
- **The wording says only that something is waiting:** "You have a new item in MHAI. Sign in to see it." The content stays behind sign-in.
- **No tracking:** no open pixels, no click tracking, no external images. Click tracking would route the reset token through the provider's redirect host and logs.

### 4.4 Secret links never rest on our side

#229 stores only the token's SHA-256 and gives the link to the `Sender` in memory. This design keeps that:
- **Link messages go by NATS request/reply, not JetStream** (`notify.v1.ctl.send_link`, §7), with a core request token (`aud: notifications`). JetStream would write the token to disk.
- The service renders and submits at once, holds the link in memory only, retries at most 3 times within 60 seconds, and answers `accepted` or `failed`. On `failed` core answers 503 `no_delivery` as #229 does; the admin tries again, which mints a new token and revokes the old one.
- The outbox row (§6.2) still records that the message was due, with no link.

### 4.5 Recipients are users, never addresses

Every message names a `user_id`. The service gets the address from the database at send time through a definer (§6.3) that answers only for an active user with a due message. Request data never names an address. This closes "send a reset link to an attacker's address" and means a deactivated person gets nothing queued for them.

### 4.6 Operator alerts: interim email, then Alertmanager

The long-term home for operator alerts is Prometheus and Alertmanager, because "the host is down" can't be reported from the host. Until those run, core turns the alerts it already knows about (§8.4) into platform inbox items and email to platform admins who opt in. When Alertmanager is live, it takes the metric-based ones and these stay as bell items.

### 4.7 Provider behind an interface; SMTP submission first

The first email adapter speaks **SMTP submission (port 587, STARTTLS required, authenticated)**, which every shortlisted provider offers. That lets the service be built and tested (against a local mail sink) before N1 is decided, and keeps the provider swappable. A provider's HTTP API adapter (idempotency keys, events) comes with bounce handling (PR 10).

## 5. Components and flow

```
action (core route or consumer)
  │ one transaction: the change + its audit row + the outbox row (ids only)
  ▼
core outbox (message_outbox) ──drainer after commit──▶ NATS NOTIFY: notify.v1.<hid|platform>.due {message_id}
                                                              │
                                     notifications service ◀──┘ (durable consumer notify_send)
                                       │ notify_claim(message_id): row, recipient's address if active, prefs
                                       │ render the kind's template
                                       ├─▶ channel: log (development) | email (SMTP submission) | later sms, whatsapp
                                       └─ message_deliveries: sent | failed | suppressed (+ attempts)

invite / reset (secret link):
core ──notify.v1.ctl.send_link (request token, aud notifications; link in memory)──▶ notifications ──▶ email
     ◀────────────── accepted | failed ─────────────────────────────────────────────┘

in-app: core writes notifications rows in the same transaction; BFF ──▶ core ──▶ GET /platform/notifications etc.
```

| Component | Holds | Can reach | Cannot reach |
|---|---|---|---|
| **notifications service** (new) | the provider credential (SMTP or API key, send-only), the unsubscribe signing key | its NATS user; Postgres as `mhai_notify` (own tables, two definers); the provider's host through the egress proxy | clinical tables, `emr.*`, sessions, audit chain, the request-signing key, any other internet host |
| **core** (existing) | as today | as today, plus publish `notify.v1.*.due` and `notify.v1.ctl.>` | the provider |
| **egress proxy** (existing, `deploy/egress-proxy`) | — | an allow-list entry for the provider's submission host | — |

## 6. Data model

All tables have `hospital_id`, RLS forced, owned by `mhai_migrate`. Platform rows use `hospital_id IS NULL` with a separate platform policy, as the platform tables do today. Migration numbers are set at build time, after #228's 00049 and #229's 00050.

### 6.1 `notifications` (core): the in-app inbox

`id, hospital_id (null for platform), recipient_id, kind, subject_type, subject_id, severity (info | warning | critical), facts jsonb (closed per kind, ≤ 1 KB), created_at, seen_at, read_at`.

- One row per recipient. `seen_at` clears the badge (the platform contract's `seen`); `read_at` marks it opened.
- `facts` holds what the platform contract already returns for `analytics_change` and `audit_integrity` (guarded counts and ids). A schema per kind refuses anything else, as audit payloads do.
- The gateway reads and marks its own rows only (`recipient_id = current user`). Auditors may mark their own seen (frontend §14.7).
- **The clinician bell is unchanged:** it reads `alerts`/`alert_deliveries` and `/corrections/mine`. Copying alerts into this table would duplicate the routing rules (EMR design §12.3).
- **Retention:** 90 days after `created_at`, by core's maintenance loop.

### 6.2 `message_outbox` (core): messages due

#228's `account_messages`, widened in one migration (rename and keep the old name as a view for one release): more kinds (§8), `hospital_id` nullable for platform recipients, `channel` in (`log`, `email`; later `sms`, `whatsapp`), `dedupe_key text UNIQUE` (for example `review_reminder:<correction>:<hour>`), `published_at`.

- Written only in the action's transaction, by the gateway or `mhai_emr_core`, ids only; never a link, token, address or text (unchanged from #228).
- Rate limits (§9.4) count these rows, as #228 does for reminders.
- **Retention:** 90 days.

### 6.3 `message_deliveries` (notifications service)

`message_id (FK, UNIQUE with channel), hospital_id, channel, status (claimed | sent | failed | suppressed), attempts, next_attempt_at, lease_until, outcome_code, provider_ref_hash, sent_at, updated_at`.

- `outcome_code` is a fixed code (`accepted`, `rejected_address`, `rate_limited`, `provider_down`, `unsubscribed`, `inactive_user`, `quiet_hours_held`, …), never the provider's error text, which may echo the address.
- `provider_ref_hash` is SHA-256 of the provider's message id, enough to match a bounce later.
- `mhai_notify` writes it; the gateway reads `status, outcome_code, sent_at` for its own hospital, so the staff screen can say "invite sent" or "couldn't be sent".
- **Retention:** 1 year (ids and codes only), long enough to answer "was I told?".

**Two definers**, `EXECUTE` for `mhai_notify` only:
- `notify_claim(message_id, lease)` reserves the row (inserts or re-leases the delivery, reserve then check) and returns the kind, ids, the recipient's current address, `is_active`, and their preference for that kind. It returns nothing for another hospital than the subject's `<hid>`, an inactive or removed user, or a message already `sent`.
- `notify_link_recipient(user_id, purpose)` for `send_link`: the address only if the user is active (or invited) and has a live `account_tokens` row of that purpose created in the last 2 minutes. That ties every link email to a token core just issued.

### 6.4 `notification_preferences` (core)

`user_id, hospital_id, category, email (bool), updated_at, version`. Categories in §10. A missing row means the category's default. Mandatory kinds have no row and can't be turned off.

### 6.5 `email_suppressions` (notifications service, PR 10)

`address_hash (HMAC-SHA-256 with a service key), reason (hard_bounce | complaint | manual), created_at`. Hashed so the table isn't an address list. Checked at send time.

## 7. NATS: subjects and users

| Subject | Kind | Publisher | Consumer | Body |
|---|---|---|---|---|
| `notify.v1.<hid>.due`, `notify.v1.platform.due` | JetStream `NOTIFY` | core only | notifications (`notify_send`) | `{message_id}` |
| `notify.v1.ctl.send_link` | request/reply | core only | notifications (queue group) | `{user_id, purpose, link, expires_at}`; reply `{outcome}` |
| `notify.v1.ctl.unsubscribe_check` | request/reply | core only | notifications | `{token}`; reply `{user_id, hospital_id, category}` or `invalid` |
| `notify.v1.ctl.health` | request/reply | core only | notifications | `{}` |

- **Control calls** carry a core request token (`aud: notifications`), checked with gokit's `reqtoken`, subject table and replay set, as `emr.v1.ctl.*` and `documents.v1.ctl.*` are. The token's hospital must equal the user's.
- **Stream `NOTIFY`:** work-queue retention, file storage, `max_age` 3 days, `MaxMsgsPerSubject` 10,000 with `DiscardNewPerSubject` (one hospital can't fill it for others), `Nats-Msg-Id` = message id (duplicates drop). Created only when `CORE_NOTIFY_ENABLED=true`.
- The subject's `<hid>` is re-checked against the outbox row; a mismatch is `not_found` and nothing is sent.
- **NATS user `notifications`:** subscribe `notify.v1.ctl.> notifications` (queue group), its `NOTIFY` consumer API and acks; `allow_responses: {max: 1, expires: "1m"}`; publish nothing else. A no-rights placeholder (`notifications-off.conf`) unless enabled, as `documents-off.conf`.
- **`gateway`** gains publish `notify.v1.*.due`, `notify.v1.platform.due` and `notify.v1.ctl.>`.
- **Dead letters:** `dead.v1.<hid>.notify_send`, ids and codes only, shown in the platform's Failed jobs view.

## 8. What is sent

### 8.1 Kinds

| Kind | To | In-app | Email | Category | Timing | Waits on |
|---|---|---|---|---|---|---|
| `invite` | the invitee | — | yes (link, 7 days) | mandatory | immediate, by `send_link` | PR 5 |
| `password_reset` | the person | — | yes (link, 30 min) | mandatory | immediate, by `send_link` | PR 5 |
| `mfa_reset` | the person | — | yes (notice, no link) | mandatory (security) | immediate | PR 5 |
| `mfa_reminder` | the person | — | yes | account | immediate, ≤ 1 a day | PR 5 |
| `review_assigned`, `review_reminder` | the reviewer(s) | review queue (exists) | yes | work | immediate, held in quiet hours | PR 5 |
| `analyst_granted` | every hospital admin | — (no admin bell yet) | yes | security | immediate | PR 9 |
| `clearance_reminder`, `clearance_ended` | hospital admins | — | yes | work | daily, 07:00 hospital time | PR 9 |
| `analytics_change` | platform admins | bell | per `analytics-email` mode | analytics | serious: immediate, ≤ 1 per hospital per measure per day; digest Monday 08:00 Lagos | PR 6, PR 7 |
| `audit_integrity` | platform admins, auditors | bell | yes, and the security contact | mandatory (security) | immediate | PR 6, PR 8 |
| `ops_*` (§8.4) | platform admins who opt in | bell | opt-in | ops | immediate; repeats ≤ 1 an hour per kind | PR 8 |
| `alert_nudge` | the alert's recipients | bell (exists) | off; per-hospital switch | clinical | after N minutes unhandled | PR 11, N4 |

### 8.2 Templates

- Plain text plus a simple HTML part with the same words, no images, no scripts, no remote content, inline styles only. English (en-GB) first; other languages are a template per kind and locale later.
- Subject lines are fixed per kind ("Your MHAI invitation", "Reset your MHAI password", "You have a new item in MHAI"). No patient, hospital count or alert detail in a subject: it shows on lock screens.
- Every email says: "MHAI will never ask for your password or two-step code by email." Links go only to `CORE_APP_URL`'s host.
- The From is a no-reply address on a sending subdomain (N3); the Reply-To is the support address (N6).

### 8.3 The critical nudge (designed, built last, off)

When a critical alert is still `new` after N minutes (default proposal 15; N4 decides), email "You have an unhandled critical alert in MHAI. Sign in to see it." to the alert's recipients who can sign in, once per alert. Never the patient, the value or the ward. Per-hospital switch, off by default; the hospital's call process remains primary and the switch's page says so.

### 8.4 Operator alerts (interim, PR 8)

Core already sees: `cloud.budget_high` (80%), cloud `auth_failed` and `unconfigured`, `usage_overrun`, settle failures, `emr.source_paused` (breaker), EMR rule-engine errors, dead letters, and a Broken audit check. Each becomes an `ops_*` inbox item for platform admins, plus email to those who opt in, at most once an hour per kind and hospital. Payloads are ids, kinds and counts, as the audit rows are.

## 9. Delivery

### 9.1 The outbox

1. The action writes its change, its audit row, and the outbox row (and the inbox row, if any) in one transaction. If the transaction rolls back, nothing is due.
2. After commit, core's drainer publishes `notify.v1.<hid>.due {message_id}` and sets `published_at`. A sweeper republishes rows unpublished after 2 minutes. Duplicates drop on `Nats-Msg-Id` and on the delivery's unique key.
3. With `CORE_NOTIFY_ENABLED=false`, kinds that must be delivered keep #228's behaviour: no sender, 503 `no_delivery` before any change.

### 9.2 Sending

The `notify_send` consumer calls `notify_claim` (a 2-minute lease), then:
- recipient inactive or removed → `suppressed/inactive_user`;
- category off for them → `suppressed/unsubscribed`;
- in quiet hours and not mandatory → `next_attempt_at` = end of quiet hours (`quiet_hours_held`);
- address suppressed (PR 10) → `suppressed/rejected_address`;
- otherwise render, submit, record `sent` and the hashed provider id, then ack.

### 9.3 Failures, retries and idempotency

- **Retryable** (timeouts, 4xx temporary SMTP codes, 429, 5xx): back off 1 min, 5 min, 30 min, 2 h, 6 h; after 24 hours `failed/provider_down`, a metric, and a dead letter. The in-app item, if any, is unaffected.
- **Permanent** (5xx SMTP address rejections): `failed/rejected_address` at once; the staff screen can show it.
- **Circuit breaker:** 10 consecutive provider failures pause sending for 5 minutes (messages wait; nothing is dropped) and raise `notify_provider_down`.
- **Idempotency:** one delivery per `(message_id, channel)`; the claim is reserve-then-check, so parallel consumers can't both send. A crash between the provider's acceptance and our `sent` write can send one duplicate on redelivery; with an API adapter (PR 10) the message id goes as the provider's idempotency key and closes that.
- **Link messages** (§4.4): 3 tries in 60 seconds, then `failed`, and the action reports it.

### 9.4 Rate limits

- Per recipient and kind: #228/#229's limits stay (resets 3 an hour, reminders once a day or hour, invites 500 a hospital a day).
- Per hospital: 200 outbound emails an hour (all kinds but mandatory security notices); past it, messages wait for the next hour, and the platform is told.
- Global: the provider's sending rate, set in config (start at 5 a second); the service paces to it.
- `send_link`: 30 a minute per hospital on top of the route limits.

## 10. Preferences, unsubscribe, quiet hours, digests

- **Categories:** `mandatory` and `security` (always sent: invites, resets, two-step notices, analyst grants, audit integrity), `account`, `work`, `analytics`, `ops`, `clinical`. Defaults: `account` and `work` on, `analytics` as the existing mode, `ops` off, `clinical` off.
- **Settings:** `/me/notification-settings` gains an additive `email` object (`{work, ops, clinical}`), audited `notification_settings.changed`; the platform's `analytics-email` route keeps its contract.
- **One-click unsubscribe** for every non-mandatory email (`List-Unsubscribe` and `List-Unsubscribe-Post`, RFC 8058). The link carries an HMAC-signed token for `(user, category, issued_at)`, valid 60 days, that can only turn a category **off**. The signing key stays in the notifications service: core's public route `POST /notifications/unsubscribe` (through the BFF, no sign-in) passes the token to `notify.v1.ctl.unsubscribe_check`, which verifies it and answers `{user_id, hospital_id, category}` or `invalid`; core then writes the preference under that hospital's RLS and audits `notification.unsubscribed`. Mandatory emails have no unsubscribe and say why.
- **Quiet hours:** per hospital, default 21:00–07:00 in the hospital's time zone (Africa/Lagos for the pilot). `work` and `analytics` email is held until 07:00. Mandatory, security and `clinical` kinds are never held. No per-user quiet hours in v1.
- **Digests:** only the platform analytics weekly digest (frontend §14.7) in v1. Others stay immediate, because the volumes are small.

## 11. Links and tokens

- Tokens are #229's: 32 random bytes, SHA-256 at rest, single use, one live per person and purpose, 7 days for invites and 30 minutes for resets, `410 invalid_token` for every failure.
- **The link's host is always `CORE_APP_URL`** (https), never taken from the request's `Host` or `X-Forwarded-Host`. `CORE_APP_URL` becomes required whenever a real channel is on.
- The reset page reads the token once, clears the address bar and sends `Referrer-Policy: no-referrer` (mhai-web; #229 notes it).
- The link is in the email body only; never in the subject, the outbox, logs, audit, metrics, JetStream or the provider's tracked links (tracking off, §4.3).
- The unsubscribe token (§10) is separate, can only switch email off, and is useless for sign-in.

## 12. Providers

### 12.1 The interface

```go
type Channel interface {
    Name() string                                      // "email", later "sms", "whatsapp"
    Send(ctx context.Context, m Rendered) (Result, error) // Result: accepted (+ provider id) | retryable | permanent
}
```

`Rendered` holds the address, the subject, the text and HTML bodies, the `List-Unsubscribe` headers and the message id. Adapters: `log` (development only; refused elsewhere, as #228), `smtp` (PR 4), the chosen provider's API (PR 10), and later `sms` and `whatsapp`. Startup refuses a missing credential, a placeholder, plain SMTP without TLS, or a From domain that isn't the configured sending domain.

### 12.2 What to choose on (N1)

1. **No PHI goes to it** (§4.3), so the provider processes staff email addresses and our message text only. That lowers the bar, but it is still a sub-processor for the hospital's processing agreement (NDPA 2023 s. 29) and a cross-border transfer if it is outside Nigeria (ss. 41–43; counsel confirms, N2).
2. **Data residency:** where it stores message content and logs, and for how long; can content logging be turned off or shortened.
3. **Deliverability into Nigeria:** into Gmail, Outlook and the hospital's own mail domain (often a smaller host). Dedicated IP is not needed at our volume; DKIM, SPF and DMARC alignment are.
4. **Security:** send-only scoped credentials, TLS required, tracking off by default or switchable, audit of the account.
5. **Cost** at pilot volume (hundreds a month) and at ten hospitals (low tens of thousands).
6. **Events:** bounces and complaints by pull (a queue we read) rather than a webhook into our network.

### 12.3 Shortlist (recommendation, not a choice)

| Option | For | Against |
|---|---|---|
| **AWS SES** (recommended if we stay on AWS) | regions include Cape Town (af-south-1), one cloud agreement we already have, cheapest, send-only IAM credentials, events by pull (SNS to SQS) | sandbox exit and reputation management are on us |
| **Postmark** | strong transactional deliverability, link tracking off by default, simple | US or EU storage; per-message price higher |
| **Mailgun (EU region) or Brevo (EU)** | EU data location, SMTP and API | tracking on by default in some plans; must be turned off and checked |
| **Self-hosted SMTP** | no third party | poor deliverability from a cloud IP; rejected |

SMS and WhatsApp (N5, later): criteria add sender-ID registration and DND-compliant transactional routes in Nigeria, delivery receipts, and a Nigerian presence (for example Termii or Africa's Talking for SMS; Meta's WhatsApp Cloud API directly or through one of them). Not a recommendation yet.

## 13. APIs (core; OpenAPI minor bump, additive)

| Route | Who | What |
|---|---|---|
| `GET /platform/notifications`, `POST /platform/notifications/seen` | platform admins, auditors | built per frontend §14.7 (replaces the 501) |
| `GET`, `PATCH /me/notification-settings` | signed-in users | gains `email: {work, ops, clinical}` (additive) |
| `POST /notifications/unsubscribe` | public (signed token) | turns one category off; 410 `invalid_token` alike for every failure; 10 a minute per client IP |
| `StaffOut` | hospital admins | the latest invite's delivery (`invite_delivery: sent \| failed \| null`) as a new nullable field |
| `GET`, `PUT /platform/hospitals/{id}/notification-settings` | platform admins, step-up on the nudge switch | quiet hours and the `alert_nudge` switch (PR 11) |

mhai-web: the platform bell's live client, the email switches, the unsubscribe page, and the staff screen's delivery state.

## 14. Audit and metrics

- **Audited** (ids and codes only): the action that caused the message (unchanged); `notification_settings.changed`; `notification.unsubscribed`; `notification.address_suppressed` (PR 10); `hospital_notification_settings.changed`; `notify.delivery_failed` for mandatory and security kinds only (so "the reset never arrived" is answerable on the chain). Ordinary sends are in `message_deliveries`, not the chain: volume.
- **Metrics:** `mhai_notify_sent_total{kind,channel}`, `…_failed_total{kind,channel,code}`, `…_held_total`, `…_queue_age_seconds`, `…_provider_breaker_open`. Labels never carry an address, a user id or a hospital name.
- **Logs:** kind, message id, hospital id, outcome code. Never the address, link, token, rendered body or provider error text.

## 15. Phased build plan

Each PR leaves `main` working with notifications **off**: the `notifications` compose profile isn't started, the NATS user is the no-rights placeholder, `CORE_NOTIFY_ENABLED=false` (core publishes nothing and keeps #228's 503 `no_delivery`), no `NOTIFY` stream exists, and `NOTIFY_CHANNELS` is empty (the service refuses to send).

| PR | What | Waits on |
|---|---|---|
| **1: docs** | this design, ADR 0018, TODO.md items | review |
| **2: skeleton** | `services/notifications/` (module, binary, settings, health, startup refusals), `notify.v1.ctl.*` verification with gokit, NATS user and its off config, compose profile, egress allow-list entry (empty) | PR 1 |
| **3: outbox and delivery** | migration: `message_outbox` (widening `account_messages`), `message_deliveries`, `notify_claim`; core's drainer and sweeper; `NOTIFY` stream; the `notify_send` consumer with the `log` channel, retries, breaker, rate limits, dead letters; retention sweeps | PR 2; #228 merged |
| **4: email channel** | fixed templates and the variable allow-list test; the `smtp` adapter (TLS required, no tracking); From-domain check; tests against a local mail sink (mailpit) with synthetic users | PR 3. **Live sending waits on N1, N2, N3** |
| **5: account messages live** | `notify.v1.ctl.send_link`, `notify_link_recipient`; core's `delivery.Sender` for the service (`CORE_DELIVERY=notify`, allowed in production); invites, resets, two-step notices and reminders, review assignment and reminders; `invite_delivery` on `StaffOut` | PR 4; #229 merged |
| **6: in-app inbox** | migration: `notifications`; the platform bell routes; `analytics_change` and `audit_integrity` items; retention | PR 1 only (no outbound needed); mhai-web's live client |
| **7: preferences and unsubscribe** | `notification_preferences`, the `email` settings, `POST /notifications/unsubscribe`, `List-Unsubscribe`, hospital quiet hours, the analytics email modes and weekly digest | PR 5, PR 6; mhai-web |
| **8: operator alerts** | `ops_*` kinds (§8.4) as bell items plus opt-in email | PR 6, PR 7; N7 |
| **9: hospital admin notices** | `analyst_granted`, `clearance_reminder`, `clearance_ended` | PR 5 |
| **10: bounces and suppression** | the chosen provider's API adapter (idempotency key), event intake by pull, `email_suppressions`, `notification.address_suppressed` | PR 5; N1 |
| **11: critical nudge** | `alert_nudge`, the per-hospital switch (off), step-up to switch it on | PR 7; **N4** |
| **12: SMS or WhatsApp** | its own design first: phone numbers, verification, lawful basis, provider; then the channel adapter | **N5**, then that design |

**The pilot needs PRs 2–5** (and #228, #229), plus N1, N2, N3 and N6. **Live use** of any real address waits on those, NATS TLS (already required for live data), and the egress proxy's entry for the provider.

## 16. Security and threat model

### 16.1 Assets

Staff email addresses (and later phone numbers); reset and invite links (each is a sign-in credential for up to 7 days); the provider credential (it can send mail as us, passing DMARC); hospital separation; patient data, which must never leave in a message; the audit trail of who was told; the availability of account recovery; clinical safety (no one must believe an email is how critical results reach them).

### 16.2 Actors and trust boundaries

Actors: clinicians, hospital admins, analysts, platform admins, auditors (recipients); the BFF; core; the notifications service; NATS; Postgres; the egress proxy; the email provider; recipients' mail hosts; an attacker who holds one session, one mailbox, one NATS credential, or controls a display name or hospital name.

| Crossing | What is checked |
|---|---|
| browser → BFF → core | session, permission, hospital from the session, step-up on invites and resets (#229), rate limits, `Idempotency-Key` |
| public → BFF → core (`/auth/password/reset`, `/notifications/unsubscribe`) | the token's hash, single use, expiry; per-IP limits; the same 410 for every failure |
| core → NATS `NOTIFY` | only `gateway` may publish `notify.v1.*.due`; body is one id |
| core → notifications (`ctl`) | request token (`aud: notifications`, subject, body hash, expiry), replay set, the token's hospital against the user |
| notifications → Postgres | `mhai_notify`: own tables; two definers that answer only for active users with a due message or a fresh token; no clinical grants |
| notifications → egress proxy → provider | one allow-listed host; TLS; a send-only credential |
| provider → recipient's mail host | opportunistic TLS (not ours to enforce); DKIM, SPF, DMARC on our sending subdomain |
| mailbox → browser → BFF | the link's host is `CORE_APP_URL`; the page clears the token from the address bar |

### 16.3 Threats (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| T1 | Info | PHI in an email, subject or provider log | ids-only bus; fixed templates; allow-listed variables; no free-text field; no patient columns granted to `mhai_notify` | NT1, NT2 |
| T2 | Info | A reset link stored or logged (DB, JetStream, logs, provider click logs) | `send_link` by request/reply, memory only; tracking off; redacting logs; outbox has no link column | NT3, NT4 |
| T3 | Spoofing | Host-header injection makes the link point at an attacker's host | the host is `CORE_APP_URL` only | NT5 |
| T4 | Tampering | A request names an attacker's address | recipients are user ids; address read at send time by a definer | NT6 |
| T5 | Spoofing | A forged `due` message or `send_link` call | NATS publish rights (core only); request token and replay set on `ctl`; `send_link` needs a token issued in the last 2 minutes | NT7, NT8 |
| T6 | Info | Cross-hospital: a message id from another hospital, or `<hid>` mismatch | RLS; the definer checks the subject's hospital; `not_found` and nothing sent | NT9 |
| T7 | Spoofing | Phishing that looks like MHAI | DMARC `reject` on our sending subdomain; fixed From; every email says we never ask for a password or code; links only to our app host | NT10 (config check) |
| T8 | Tampering | Header or HTML injection through a hospital or display name | no display names in templates; the hospital name is validated (no CR, LF, `<`, `>`) and escaped; text and HTML generated from the same escaped values | NT11 |
| T9 | Repudiation | "I was never sent the reset" or "the admin never invited me" | the action's audit row; `message_deliveries` for a year; failed mandatory sends on the chain | NT12 |
| T10 | DoS | An admin floods addresses (our reputation, the provider's quota) | #229's caps; per-hospital hourly cap; global pacing; breaker | NT13 |
| T11 | DoS | The provider is down and account recovery stops | link sends fail fast with 503 and nothing half-done; other kinds wait and retry 24 h; in-app unaffected; metric and breaker | NT14 |
| T12 | Info | Unsubscribe token abused to change someone else's settings or to learn account existence | HMAC over `(user, category)`; can only turn off; same 410 for every failure | NT15 |
| T13 | EoP | A compromised notifications service | it holds only the send-only credential and unsubscribe key; no clinical data; addresses only one at a time for due messages; provider-side quota caps the blast | NT16 |
| T14 | Info | A deactivated or removed person still receives work email | `is_active` checked at send time; their queued messages are suppressed | NT17 |
| T15 | Tampering | Duplicate sends (redelivery, parallel consumers) | `Nats-Msg-Id`; unique `(message_id, channel)`; reserve-then-check claim | NT18 |
| T16 | Info | Lock-screen or shared-mailbox exposure | subjects and bodies say only that something is waiting; reset links 30 minutes and single use; two-step still needed to sign in | NT2 |
| T17 | Safety | A clinician relies on email or SMS for criticals | in-app only for the pilot; the nudge is off and gated on N4; the hospital's call process stays primary | review gate (N4) |
| T18 | Info | Metrics or errors carry addresses | labels fixed; provider error text dropped for a code | NT19 |
| T19 | Spoofing | A bounce or complaint event forged to suppress someone's address (PR 10) | events pulled from the provider's queue with our credential, not an open webhook; suppression only blocks sending and is shown to admins | NT20 |

### 16.4 Controls (decided)

- **Authentication and step-up:** sessions and two-step as today; step-up on invites and resets (#229) and on the clinical nudge switch; links are single-use credentials with the lifetimes above.
- **Authorisation:** core decides recipients; the service can't choose one. RLS on every table; platform rows by a separate policy. `mhai_notify` has its own tables and two definers only.
- **Data handling:** ids on the bus; fixed templates; no PHI outbound; addresses only in Postgres and in the provider's hands at send time; retention 90 days (inbox, outbox), 1 year (deliveries); provider content logging off or shortest where the provider allows (N1 criterion).
- **Concurrency:** reserve-then-check claims with leases; unique delivery per message and channel; the per-hospital hourly cap as one upsert that serialises.
- **Secrets:** the provider credential and the unsubscribe key in the service's secret file only, startup refuses placeholders and plain-text SMTP; the credential is send-only and scoped to the sending domain.
- **Third parties and hosting:** the provider is the only new third party; it receives addresses and fixed text, never patient data; listed as a sub-processor in each hospital's agreement (N2); reached only through the egress proxy.

### 16.5 Abuse-case tests

The plan writes these first.

- **NT1** For every kind, the rendered email built from a synthetic message contains only the allow-listed variables; a template with any other placeholder fails the build.
- **NT2** A synthetic alert, correction and analysis run produce outbound messages; a scan of every rendered subject and body finds no synthetic patient name, MRN, value or diagnosis string.
- **NT3** After an invite and a reset on a synthetic stack, a search of Postgres (all tables), the JetStream store, every service log and the metrics output finds no token and no part of the link's query.
- **NT4** The rendered HTML has no `<img>`, no remote URL other than `CORE_APP_URL` and the unsubscribe link, and no tracking parameters; the API adapter refuses to start with tracking on.
- **NT5** A reset requested with `Host: evil.example` and `X-Forwarded-Host: evil.example` produces a link on `CORE_APP_URL`'s host.
- **NT6** No route or message accepts an address for a recipient; changing a user's address after the message is due sends to the new address only (and a link kind fails if the token is older than 2 minutes).
- **NT7** A `notify.v1.<hid>.due` published by any NATS user but `gateway` is refused by the server; a `send_link` without a valid token, with a replayed token, or for a user of another hospital is refused and nothing is sent.
- **NT8** `send_link` for a user with no token issued in the last 2 minutes sends nothing.
- **NT9** A `due` message on `notify.v1.<hospital B>.due` carrying a hospital A message id sends nothing and records `not_found`.
- **NT10** Startup refuses a From address outside the configured sending domain; a deployment check (ops) confirms SPF, DKIM and DMARC records before `NOTIFY_CHANNELS` includes `email`.
- **NT11** A hospital name with CR, LF or angle brackets is refused at save; a template never emits a header from data.
- **NT12** A failed mandatory send writes `notify.delivery_failed` to the chain; a sent one is in `message_deliveries`.
- **NT13** 1,000 reminders requested for one hospital in an hour send at most 200; the rest wait; mandatory kinds still go.
- **NT14** With the provider down: an invite answers 503 `no_delivery`, the token is revoked and nothing else changed; a reminder retries on the schedule and is `failed` after 24 hours; the bell still shows platform items.
- **NT15** An unsubscribe token for user A can't change user B; a forged, expired or reused token gets the same 410; no token turns a category on or touches a mandatory kind.
- **NT16** `mhai_notify` gets `permission denied` on `alerts`, `emr.*`, `queries`, `users` (direct), `sessions`, `audit_log`.
- **NT17** A user deactivated after a message is due receives nothing (`suppressed/inactive_user`).
- **NT18** The same `due` message delivered 5 times, and two consumers racing, send one email.
- **NT19** Metric labels and logs from a synthetic run contain no address and no user name.
- **NT20** (PR 10) A suppression needs an event pulled with our credential; a suppressed address shows as a problem to the hospital admin, not silently.

### 16.6 Residual risk and sign-off

- **Email is not end-to-end private:** a link travels in clear on any hop without TLS, and sits in the recipient's mailbox. Accepted with short lifetimes, single use, and two-step still needed to sign in.
- **A compromised mailbox** gets invites and resets sent to it. Two-step limits a reset; an invite before enrolment is the weak point (the attacker could enrol first). Accepted for the pilot; the hospital admin sees who enrolled and can remove them.
- **The provider sees addresses and the fixed text** and keeps logs for its retention. Accepted: no PHI goes; the provider is a listed sub-processor (N2).
- **One duplicate email** is possible after a crash on the SMTP adapter, until PR 10.
- **No delivery guarantee:** email may land in spam or late. That is why it is never the clinical channel (T17) and why link sends report failure to the admin.
- **Operator alerts from inside the host** can't report the host down; the external uptime probe and Alertmanager (TODO.md) close it.
- **Sign-off:** privacy and legal (N2: sub-processor, transfer basis, DPIA note), clinical lead (N4 before PR 11 is switched on), product owner (N1, N5, N6). They are listed in TODO.md.

## 17. What's needed from people

| # | Who | What | Blocks |
|---|---|---|---|
| N1 | Product owner | Choose the email provider and its region from §12.3 (recommendation: AWS SES if hosting stays on AWS) | live email; PR 10 |
| N2 | Privacy lead and counsel (H5) | The provider as a sub-processor in the hospital's processing agreement; the cross-border basis if outside Nigeria; a line in the DPIA | live email to a hospital's staff |
| N3 | Ops | A sending subdomain with SPF, DKIM and DMARC (`p=reject` once clean); the provider account with a send-only credential in the secret store; the egress proxy entry; leaving the provider's sandbox | live email |
| N4 | Clinical lead | Whether unhandled criticals ever go outside the app; if so, after how long, to whom, and the wording | PR 11 switched on |
| N5 | Product owner | Whether and when SMS or WhatsApp is wanted (it starts the phone-number design) | PR 12 |
| N6 | Product owner | The From name, the support Reply-To, and the support contact printed in emails | live email |
| N7 | Product owner, ops | Which platform admins receive operator alerts, until Alertmanager (links to H9 and the cloud H5 item) | PR 8 switched on |
| N8 | Privacy lead | Confirm retention: inbox and outbox 90 days, deliveries 1 year | the retention sweep's values |

## 18. Decisions for later review

Proposed on 19 Nov 2026, each on the safer side:

- **D1. In-app first; email the first outbound channel; SMS and WhatsApp later** behind their own design (§4.1).
- **D2. Clinical alerts stay in the app for the pilot;** the nudge is designed, built last and off (§4.1, §8.3).
- **D3. A separate Go service holds the provider credential;** core decides recipients; the in-app inbox stays in core (§4.2).
- **D4. No patient data outbound,** by fixed templates and an ids-only bus (§4.3).
- **D5. Secret links by request/reply only,** never at rest; failure surfaces to the admin (§4.4).
- **D6. Recipients are user ids,** addresses read at send time (§4.5).
- **D7. Operator alerts as bell items and opt-in email** until Alertmanager runs (§4.6).
- **D8. SMTP submission as the first adapter,** so the build doesn't wait for N1 (§4.7).
- **D9. No open or click tracking, ever** (§4.3).
- **D10. Hospital-level quiet hours** 21:00–07:00 for non-urgent email; none for mandatory, security or clinical kinds (§10).
- **D11. Delivery outcomes in their own table, not on the audit chain,** except failed mandatory sends (§14).
