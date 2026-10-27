---
name: bug-hunt
description: Use when looking for bugs in mhai-multiagent: when a test fails or flakes, when asked to find what's broken, or before calling a task done. Covers how to find, reproduce and prove a bug with a failing test, and the edge cases that have bitten this codebase.
---

# Finding and proving bugs in mhai-multiagent

A bug isn't found until a test fails because of it. A fix isn't done until that test passes and stays in the suite.

## Method
1. **Reproduce first.** Write the smallest test that fails. If you can't make it fail, you haven't found the bug yet.
2. **Read the failure, not your guess.** Check the real error, the real values and the real query.
3. **Bisect.** Was it ever green? If so, `git bisect` to the change that broke it. Check whether main fails too: several "regressions" here were date or environment problems on main.
4. **Fix the cause, not the symptom.** Then look for the same pattern elsewhere and fix the whole class.
5. **Keep the test.** Name it after the behaviour, for example `TestRefreshRaceWithin10s`, not after the bug.

## Edge cases to try
- **Concurrency.** Run N parallel requests on one key: lockout, refresh, idempotency, consume-once. Use `-race` and `-count=1`.
- **Time.** Day boundaries, month turns, clock skew of ±1 step or 5 s, an expiry exactly at the limit, tests that pin a count derived from today's date (pin the clock or read the value from the page).
- **Pooled connections.** `SET LOCAL` and session settings leaking, or coming back as `''`, on a reused connection.
- **Empty and odd input.** Missing headers, empty strings, mixed-case emails with spaces, invalid UTF-8, NFC versus NFD, over-long bodies, an unknown JSON field.
- **Policy changes mid-flow.** A role is added or removed, a hospital is deactivated, or MFA is turned on, between two requests.
- **Network.** NATS or Postgres unreachable or slow. Does the code fail closed and recover?
- **Restart and redelivery.** A JetStream redelivery processed twice. An inbox that's gone after a restart.
- **Totals that must add up.** Shown parts versus the total, both scopes, and the CSV versus the screen.

## Flaky tests
A flaky test is a bug in either the test or the code. Run it 20 times with `-count=20`. Find what varies (time, order, shared state, ports) and fix that. Don't add retries or sleeps to hide it.

## Done means
- [ ] A failing test showed the bug, and it passes now.
- [ ] The same pattern was searched for and fixed elsewhere.
- [ ] Full suite green: Go `-race`, the integration tag, and the Python checks with uv.
