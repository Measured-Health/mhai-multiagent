---
name: secure-design
description: Use when writing or reviewing a design, spec, ADR or plan in mhai-multiagent, before any code. Every design must carry a "Security and threat model" section; this skill says what goes in it and how to check it.
---

# Security is part of the design

A design isn't ready for approval until it has a **Security and threat model** section that a reviewer can check. Retrofitting security costs far more than designing it in. Most of the serious findings in Plan 2 were design gaps that a threat model would have caught.

## The section every design must have

### 1. Assets
What we protect in this design: patient data, credentials and tokens, hospital separation, the audit trail, availability, and clinical safety (an unchecked answer must never be shown as final).

### 2. Actors and trust boundaries
- **Actors:** who touches it. Clinicians, hospital admins, analysts, platform admins, auditors, the BFF, core, intelligence workers, NATS, the database, third parties.
- **Boundaries:** draw or list every crossing (browser → BFF → core → NATS → intelligence → Postgres → GPU host), and say what is checked at each one.

### 3. Threats
Walk through STRIDE for each boundary, and list each threat with its mitigation. At minimum, cover:
- **Spoofing:** stolen or replayed tokens, a forged service, a mixed-up audience (platform or clinical).
- **Tampering:** a request body that names another hospital or user; NATS message spoofing; audit tampering.
- **Repudiation:** which actions are audited, and how audit integrity is checked.
- **Information disclosure:** cross-hospital reads, PII in logs, errors, URLs or caches, small-group subtraction leaks.
- **Denial of service:** brute force, lockout abuse, expensive endpoints, unbounded queues.
- **Elevation of privilege:** role changes, inherited database roles, a stale claim on refresh, half-finished flows.

### 4. Controls (decided, not "to be added")
- **Authentication and step-up:** what proves identity, and which actions need the code again.
- **Authorization:** which roles, how hospital scope is enforced in code *and* in the database (RLS), and what the least-privilege grants are.
- **Data handling:** where PHI flows, where it's stored, encryption, retention, and what must never be logged.
- **Concurrency:** which counters, tokens or reservations are atomic, and how each fails closed.
- **Secrets:** where they live, how they rotate, startup refusal of placeholders.
- **Third parties and hosting:** what leaves our network, and under what agreement.

### 5. Abuse-case tests
The tests that prove each control, written as behaviour. For example: "another hospital's id gets 404", "20 parallel wrong passwords run at most 5 password checks". The plan carries these into its tasks' first steps.

### 6. Residual risk and sign-off
- **Residual risk:** what's left open, and why it's accepted.
- **Sign-off:** what needs clinical, privacy or legal approval, and what goes in TODO.md.

## Reviewing a design
- **Is the section complete?** Reject a design without it, or with "TBD" controls.
- **Check each threat** against the controls: is it actually closed, or just named?
- **Check each control has a test** in the plan.
- **Feed lessons back.** Add new classes found in reviews to the `security-review` skill's list.

## Then
- **While building:** follow the `secure-build` skill.
- **Before merge:** run the `security-review` skill on the branch.
