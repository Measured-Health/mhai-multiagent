# 0018. A separate notifications service delivers messages core decides on, with no patient data outbound

- **Status:** Proposed (19 Nov 2026), with the design, for the product owner's review
- **Date:** 2026-11-19
- **Deciders:** product owner (to accept); the email provider is the product owner's separate decision N1
- **Extends:** [ADR 0010](0010-api-in-go-models-in-python-over-nats.md) (Go for the rules, NATS between services), [ADR 0011](0011-frontend-ingress-over-nats.md) (the BFF reaches core only), [ADR 0014](0014-separate-emr-service-and-document-route.md) and [ADR 0016](0016-separate-documents-service-with-per-hospital-encryption.md) (a separate Go service holds what core shouldn't)
- **Design:** [`docs/designs/2026-11-19-notifications-service.md`](../designs/2026-11-19-notifications-service.md) (the build plan is its §15)

## Context

Invites, password resets, two-step notices and reviewer reminders are built (#228, #229) but answer 503 `no_delivery` outside development, because nothing sends them. Hospital admin notices, the platform bell and analytics email, and operator alerts (cloud budget, settle failures, paused EMR sources) also wait. The clinician bell and pop-ups already work through core. A pilot at one Nigerian hospital needs invites and resets soon. No email provider is chosen.

Forces: an email or SMS leaves our control (lock screens, shared mailboxes, provider logs); a reset link is a sign-in credential; a provider credential can send mail as us; SMS and WhatsApp are common in Nigeria but need phone numbers we don't hold; and the hospital's critical-result call process must stay primary (EMR design §17.6).

## Decision

1. **In the app first, email next, SMS and WhatsApp later** behind their own design. Clinical alerts stay in the app for the pilot; an "unhandled critical" nudge is designed but off until the clinical lead decides.
2. **A separate Go service, `services/notifications/`** (own module and binary, gokit), holds the provider credential and delivers. **Core decides who is told,** writing an ids-only outbox row in the action's transaction. The in-app inbox stays in core. The service has its own NATS user and a narrow database role with no clinical grants.
3. **No patient data outbound:** fixed templates per kind with an allow-list of variables (link, expiry, hospital name); messages say only that something is waiting behind sign-in; no open or click tracking.
4. **Secret links never rest on our side:** invites and resets go by NATS request/reply with a core request token, never JetStream; failure surfaces to the admin as #229's 503.
5. **Recipients are user ids;** the address is read at send time, only for an active user with a due message.
6. **Providers behind a channel interface;** SMTP submission is the first adapter, so the build doesn't wait for the provider choice.
7. **Off by default:** no compose profile, a no-rights NATS user, `CORE_NOTIFY_ENABLED=false`, no `NOTIFY` stream, no channels.

## Consequences

- Invites and resets can go live after four small PRs and the provider decision, with the provider swappable.
- A core compromise doesn't hand over a credential that sends mail as us; a slow provider doesn't hold core's requests.
- Email can't carry anything clinical, so it can never replace the bell or the hospital's call process.
- One more small Go service to run. Duplicate emails are possible after a crash until the provider's API adapter brings idempotency keys.
- The provider becomes a sub-processor in each hospital's agreement (addresses and fixed text only).

## Security and threat model

The full model is the design's §16: assets, boundaries, STRIDE threats T1–T19, controls, abuse-case tests NT1–NT20 and residual risk. The decisions above close: PHI in outbound messages (T1), stored or tracked links (T2), host-header link injection (T3), attacker-chosen addresses (T4), forged send requests (T5), cross-hospital sends (T6) and the provider credential in core (T13). Sign-off: privacy and counsel (sub-processor, transfer basis), the clinical lead (the nudge), the product owner (provider, SMS and WhatsApp).

## Alternatives considered

- **An email sender inside core** (#228's `Sender` allows it): simplest, but core would hold a credential that sends as us and wait on the provider.
- **Every notification, the clinician bell included, in the new service:** duplicates the alert routing rules and breaks ADR 0011 (the BFF reaches core only).
- **SMS or WhatsApp first:** wider reach in Nigeria, but needs phone numbers, sender registration or template approval, and shows on lock screens; not needed for the pilot's first messages.
- **Links in the JetStream outbox, encrypted:** retries become easier, but a credential would rest on disk; a fresh link from the admin costs little.
- **Patient details in alert emails for convenience:** rejected outright; the content stays behind sign-in.
