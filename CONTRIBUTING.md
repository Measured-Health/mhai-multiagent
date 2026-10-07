# Contributing to MHAI

1. Create a feature branch off `main`
2. Run `make lint` and `make test` before committing
3. Write tests for new functionality; security fixes need a failing-first
   test in `tests/security` (see [SKILLS.md](SKILLS.md))
4. Submit a PR with a clear description

## Agent contributions

AI coding agents (Claude Code, Cursor and similar) may implement changes,
but a human decides what reaches `main`. Every agent change follows this
loop:

```
 agent implements ──▶ agent opens PR ──▶ human reviews
        ▲                                     │
        │                         ┌───────────┴───────────┐
        │                      passes                  doesn't pass
        │                         │                        │
        │                 human merges by hand    human requests changes
        │                                          (optional correction notes)
        └──────── agent reads the notes, fixes the same PR ┘
```

### The agent

- Works on its own branch (`agent/<short-topic>` or `fix/<issue>`), never
  on `main`.
- Before opening the PR: runs `make lint` and `make test`, follows
  [SKILLS.md](SKILLS.md), and adds tests for what it changed.
- Opens one PR per task. The description says what changed and why, how
  it was tested (commands and results), what it could not verify, and
  which issues it closes.
- **Never merges, approves or force-merges its own PR,** and never pushes
  to `main`. Merging is always a human action.
- Doesn't add AI attribution to commits or PRs (no co-author trailers or
  "generated with" footers). The human who merges owns the change.
- When changes are requested: reads every review comment and correction
  note, pushes fixes to **the same branch** (no new PR), replies to each
  comment with what it did, or why not, then re-requests review.
- Stops and asks in the PR when a note is ambiguous or would need a
  decision outside the task (clinical thresholds, security trade-offs,
  data handling). It doesn't guess.

### The human reviewer

- Reviews the diff, the tests, and the agent's claims about what was
  verified. Treat unverified claims as unverified.
- **Passes:** approve and merge by hand.
- **Doesn't pass:** choose *Request changes*. Correction notes are
  optional but make the next round faster. Inline comments on the exact
  lines work best, and a short summary in the review body says what must
  change before merge. The agent works only from what's written in the
  PR, so put everything it needs there.
- **Wrong approach entirely:** close the PR with a note explaining why,
  and open or update an issue if the work should be redone.
- Anything touching patient data handling, guardrail thresholds, or
  feedback/identity code (SKILLS.md §1–§6) needs a reviewer who
  understands the clinical and security impact, not just the code.

### Repository settings that back this up

Protect `main`: require a pull request, at least one approving review
from a human, and passing CI before merging, and block force pushes.
Give agents tokens that can push branches and open PRs but can't
approve, merge or change branch protection.
