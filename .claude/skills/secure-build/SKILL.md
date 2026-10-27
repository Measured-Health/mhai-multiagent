---
name: secure-build
description: Use before writing or changing any code in mhai-multiagent (the Go core service, the Python intelligence service, NATS config, SQL migrations). It holds the rules for building securely here: hospital separation, identity, secrets, logging, tokens, races and failing closed.
---

# Building securely in mhai-multiagent

We build securely from the first line, not just at review time. Read SKILLS.md and ADR 0010 first. The rules below are the ones this codebase has learned, many of them from real findings.

## Before you write code
1. **Name the attacker.** Write one line on who could misuse what you're building: another hospital, a stolen token, a compromised worker, a client replaying requests, an insider with a database login.
2. **Find the trust boundary.** Know where data crosses from untrusted to trusted: browser → BFF → core → NATS → intelligence → Postgres. Validate on the trusted side of every crossing.
3. **Write the failing test first.** That includes the abuse case, not just the happy path.

## Identity and tenancy
- **Identity comes from the verified token, never from the request.** The hospital, user and roles come from the principal. Never take them from a body, query, header or NATS payload.
- **Another hospital's resource is 404, not 403.** That way ids can't be probed.
- **Row-level security is the second wall.** Every hospital-owned table has `FORCE ROW LEVEL SECURITY`. App roles never own tables and never have `BYPASSRLS`. Run every request inside `authz.Tx` (the hospital set with `SET LOCAL`). Never open a transaction directly in a handler.
- **Give a role the least access it needs.** A new table means new grants and policies in the same migration, with column-level UPDATE where you can. Add tests that the other roles are refused.
- **Python never trusts a payload.** The intelligence service checks the core-signed request token (signature, `aud`, `exp`, replay) and the hospital scope before doing any work.

## Secrets, tokens and credentials
- **Hash every stored secret.** That covers refresh tokens, device tokens, mfa_tokens and backup codes. Encrypt TOTP secrets (AES-GCM, unique nonce).
- **Compare secrets in constant time.**
- **Never put a token or code in a URL**, a stored idempotency body, a log or an audit payload. Credential-returning routes use `httpx.NoStore`.
- **Fail on placeholder or missing secrets at startup.** Never ship a default secret.
- **Keep credentials out of errors.** Parse connection strings yourself and pass credentials separately. Wrap only known sentinel errors.

## Logging and audit (rules R1–R4)
- **R1:** never put an email, token, code, password or patient data into a log or error message. Put it in a keyed attribute, where the redactor works.
- **R2:** classify driver errors (for example `ErrEmailTaken`). Don't log raw database errors.
- **R3:** redaction works by key name. A value under a harmless key is not scanned.
- **R4:** every auth endpoint has a test proving that a failing request leaves no email, password or code in the logs.
- **Audit with ids only.** `audit.Write` refuses sensitive keys. Keep the audit write late in a short transaction.

## Races and check-then-act
- **Reserve, then check.** A counter or limit is reserved atomically (Postgres row lock, unique constraint, or KV compare-and-set by revision) *before* the expensive check. Never read, check, act, then write.
- **Fail closed.** If a counter, denylist or lock can't be written or read, refuse with 503 `not_ready`. Never go ahead.
- **Single-use tokens are consumed atomically.** Only one concurrent use wins.
- **Recheck on refresh.** Policy can change between sign-in and refresh, so recheck roles, MFA and status at every refresh.

## Authentication specifics
- **Keep unknown and known accounts indistinguishable.** Same status, body, headers and timing (`DummyVerify`).
- **MFA is enforced everywhere it applies:** login, refresh and middleware. One shared check (`sessionSatisfiesMFA`).
- **Bind half-finished flows to what started them.** An mfa_token is bound to its audience and to the password state that issued it.
- **Changing a password ends every other session** and removes trusted devices.

## NATS
- **Each service has its own credentials and its own inbox prefix.** Workers reply only through `allow_responses`. Nothing but core publishes `inference.v1.>` or `jobs.v1.>`.
- **A new subject needs a matching permission** in `services/nats/nats.conf`, plus a test that the wrong user is refused.

## Data
- **Synthetic data only.** No PHI in code, tests, fixtures, logs, PRs or screenshots.
- **Small-group hiding and subtraction leaks are server rules** (frontend requirements §10, §14.12). Any shown pair of totals where one contains the other must not differ by 1–4.

## Before you commit
- [ ] Every new route is in the permission table, with its roles and step-up rule.
- [ ] Every new table has RLS, grants and refusal tests.
- [ ] Abuse-case tests went red before the fix.
- [ ] No secret, email or PHI in code, logs, errors or test output.
- [ ] Run the `security-review` skill on your own diff.
