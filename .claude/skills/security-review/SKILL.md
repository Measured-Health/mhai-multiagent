---
name: security-review
description: Use when reviewing a change, PR or branch in mhai-multiagent for vulnerabilities, before merge or when asked to check something is secure. Gives the method and the checklist of vulnerability classes this codebase has actually had.
---

# Security review for mhai-multiagent

Review as an attacker would, not as the author. Every finding needs a concrete way to exploit it, a file:line and a fix. A suspicion with no exploit path is at most Minor.

## Method
1. **Scope.** List what the diff touches: routes, tables, roles, NATS subjects, tokens, logs. Read SKILLS.md, ADR 0010 and the design section for each.
2. **Trust boundaries.** For each input, ask: who controls it, and where is it validated? Follow it to every place it's used.
3. **Try each class below.** For each one, write what you tried and what stopped it. "Looks fine" is not a result.
4. **Prove it.** Where you can, write a throwaway test or SQL snippet that shows the exploit, then remove it. Mark findings CONFIRMED (shown) or PLAUSIBLE (reasoned).
5. **Rate severity.**
   - **Critical:** cross-hospital data, an auth bypass or a credential leak.
   - **Important:** a real exploit path with conditions, or a fail-open path.
   - **Minor:** hardening.

## Classes this codebase has actually had
- **Missing RLS or over-broad grants:** a table with no hospital scoping, or a role with INSERT, UPDATE or ALL where SELECT or column-level access would do, or `USING (true)` policies.
- **Roles granted by inheritance:** a login that inherits an owner or app role through another role. Check with `pg_has_role(…, 'USAGE')`, not direct membership.
- **Check-then-act races:** lockout counters, single-use tokens, idempotency reservations. Ask: what do 20 parallel requests do?
- **Fail-open paths:** a counter or denylist write error that is logged but not acted on.
- **Stale claims:** a refresh that reuses old MFA or role state while the policy has changed.
- **Half-finished flows not bound to their start:** an mfa_token usable from another audience, or after a password change.
- **Secrets in errors or URLs:** a wrapped driver error that echoes a connection string with its password.
- **Shared reply channels:** a NATS worker able to read or spoof another service's replies (`_INBOX.>`).
- **Logs and audit leaking PII:** values under nested keys, `slog.Any` maps or structs, groups named after a secret.
- **Over-broad deny lists:** a credential filter that matches exact names only, so `session_token` slips through. Or the opposite: so broad it breaks real behaviour.
- **Account enumeration:** different status, body, headers or timing for unknown, locked and wrong-password attempts.
- **Small-group subtraction leaks:** shown totals or pairs that differ by 1–4, within one reply or across scopes.

## Also check
- **Identity and hospital** come only from the principal.
- **Another hospital's id** returns 404.
- **Every write** has a version or idempotency, and step-up where the contract asks for it.
- **Startup** refuses superuser, BYPASSRLS, owner or placeholder configuration.
- **Tests** are real abuse-case tests that would fail without the fix.

## Report
Write findings with: severity, CONFIRMED or PLAUSIBLE, file:line, the exploit in one or two sentences, and the fix. Reply with a verdict line, then one line per Critical or Important finding.
