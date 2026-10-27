# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## Decided

- **Go alongside Python.** The core service (Go, `services/core/`) owns the API, auth, tenancy, audit and EMR plumbing. The Python services (`services/intelligence/`) keep the models and are reached only over NATS. See [ADR 0010](docs/adr/0010-api-in-go-models-in-python-over-nats.md).

## Open items for the core service

From §11 of the [design](docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md):

- **Mail provider** for invites and password resets. Sign-in is built (Plan 2) without it; invites and forgot/reset wait for it. AWS SES is the suggestion.
- **Scale assumption.** Sizing assumes up to about 50 hospitals and 5,000 staff accounts in the first year. Correct it if talks suggest more.
- **Data Protection Act check** before go-live: data processing agreements with each hospital, and where data is stored. Needs someone qualified.

## Next for the core service (after Plan 2)

- **Invite flow.** It must close first-sign-in self-enrolment: today a CLI-created account enrols its own two-step at first sign-in, so whoever has the first password owns the second factor. Invites bind enrolment to the invited person. Needs the mail provider.
- **Forgot / reset password** (contract §2.5). Needs the mail provider.
- **The Python boundary plan.** Workers verify the core-signed request token, `RBACManager` and `AuditLogger` move to Go, Redis leaves compose, and the worker NATS client uses the `_INBOX_worker` prefix. Until then the `intelligence` service still reads the whole `.env` (`env_file`), core secrets included; split its environment then.
- **Hospital setup flow.** `setup_incomplete` hospitals can sign in so the first admin can finish setup; revisit when the flow exists.

### Deferred minors to follow up

- **Audit tail deletion.** The hash chain can't show rows deleted from the end. Add signed checkpoints (`audit_checkpoints`) on a timer.
- **Direct `audit_log` inserts.** The app roles can INSERT into `audit_log` without the chain lock. Route every append through a SECURITY DEFINER `audit_append()` and revoke INSERT.
- **NATS wildcards.** The `gateway` user's `$JS.API.CONSUMER.>` and stream wildcards are wider than needed. Narrow them to the KV streams and `JOBS`.
- **One NKey per worker service**, not one shared `worker` user.
- **Idempotency panic after commit** re-runs the handler on retry (fails safe); store the tombstone first.

## Backend contracts the frontend uses on sample data

mhai-web has built these screens against contracts that are marked **Assumed** or proposed. Each needs building here, or confirming as built:

- §10 Analysis (Plan 6), and the hospital side of clearance letters (Plan 11a).
- §14.10 platform clearances and the named-access switch (Plan 11b), including the `today` fields and revoke refusals (`same_person`, `same_side`).
- Named access in Analysis (Plan 11c): `POST /analysis/patients/named` and `clearance/me` with `reason`.
- §14.11 record connections (platform admin) and §12 EMR alert settings, routing and evidence (hospital side) (Plan 13).
- §14.8 platform analysis across hospitals (Plan 12, designed only).
