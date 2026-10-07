# 0005. Fail closed, and require a second clinician for corrections

- **Status:** Accepted
- **Date:** 2026-10-07

## Context

When an agent failed, the orchestrator invented an amber "review" result
with made-up scores. It also tuned a shared HGA by mutating thresholds that
were never restored on error (#2). A single clinician's correction went
straight into the shared knowledge base and retuned guardrails (#3).

## Decision

- Any missing or failing agent escalates the result, marked `degraded`,
  with no invented scores. Unknown decisions escalate too.
- Per-request settings are passed as arguments; shared objects are never
  mutated per request.
- A correction is queued until a different, identified clinician approves
  it. Only then does it update the KB, persona vector or trust weights.
  Identity comes from the session, compared in normalised form.

## Consequences

Fewer auto-approved results when components are unhealthy: a safety
trade-off we accept. The API needs an approval endpoint, and the frontend
must show degraded and escalated results distinctly (#8).
