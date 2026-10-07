# Contributing to MHAI

1. Create a branch off `main` (see [Branches and commits](#branches-and-commits))
2. `make install-dev` once (uv environment + pre-commit hooks)
3. Run `make lint`, `make typecheck` and `make test` before pushing
4. Write tests; security fixes need a failing-first test in `tests/security`
   (see [SKILLS.md](SKILLS.md))
5. Open a PR and fill in the template

## What CI enforces

Every PR must pass these before merge:

- the lockfile is current (`uv lock --check`);
- ruff lint and format;
- mypy;
- tests;
- at least **80% coverage of changed lines** (`diff-cover`);
- a gitleaks scan of history.

Code ported from the original notebooks is grandfathered by explicit
baselines in `pyproject.toml`:

- `[tool.ruff.lint.per-file-ignores]`
- `[tool.ruff.format] exclude`
- `[[tool.mypy.overrides]]`

**Never add to these lists.** Remove a file from them when you clean it up.
Everything new must pass the full rule set.

One Python version: **3.12** (`.python-version`; Docker uses the same).
Container images and tools are pinned to exact versions, and Dependabot
proposes upgrades weekly.

## Branches and commits

- **Branches:** `feat/<topic>`, `fix/<issue-number>-<topic>`, `chore/<topic>`,
  `docs/<topic>`; agents use `agent/<topic>`. Branch off `main`. Stack on
  another PR's branch only when the work depends on it, and say so in the PR.
- **Commit subject:** imperative, at most 72 characters, no trailing full stop
  ("Scope retrieval to the patient", not "Scoped…" or "scoping…").
- **Commit body:** *why* the change was needed and anything a reviewer
  wouldn't guess from the diff. Reference issues ("Closes #12").
- One logical change per commit; keep pure moves or renames separate from
  edits.
- No AI attribution in commits or PRs (see
  [Agent contributions](#agent-contributions)).

## Clinical change control

Changes that alter what clinicians see need clinical sign-off as well as
code review:

- guardrail thresholds and trust weights;
- prompts;
- the NLI, embedding or LLM models (including version bumps);
- decision mapping;
- PII rules.

The process:

1. Label the PR `clinical-review`.
2. Attach the `scripts/e2e_models.py` report from before **and** after the
   change, and say which numbers moved and why that's acceptable.
3. A clinically qualified reviewer approves it in addition to the code
   review.
4. Record significant changes as an ADR (see below).

## Testing

- Every bug fix comes with a test that fails without the fix. Security fixes
  go in `tests/security/`. Check that the test fails for the right reason, not
  because of a missing argument.
- Tests that load real ML models are marked `@pytest.mark.models` and stay
  out of CI. Run the end-to-end check (`scripts/e2e_models.py`) with
  synthetic data on a machine that can load the models before every release
  and for any clinical change.
- Don't mock away the behaviour under test. Stubs are fine for slow or
  external dependencies (LLM, HTTP), as `tests/security` does.

## Data

- **Synthetic or properly anonymised data only, everywhere outside
  production:** tests, fixtures, notebooks, issues, PRs, screenshots, logs
  and bug reports.
- Never commit `.env` files, keys, credentials, patient exports or model
  weights. `.gitignore`, pre-commit (gitleaks, private-key detection) and CI
  enforce this. If something slips through, treat it as compromised: rotate
  it, then report it through [SECURITY.md](SECURITY.md).

## Releases

- [Semantic versioning](https://semver.org/). Breaking API or behaviour
  changes bump the major version; clinical-output changes are at least a
  minor bump.
- Keep [CHANGELOG.md](CHANGELOG.md) current: add user-visible changes under
  *Unreleased* in the same PR.
- To release:
  1. Move *Unreleased* to a new version heading, set `version` in
     `pyproject.toml`, and merge.
  2. A maintainer tags `vX.Y.Z` on `main`.
  3. The release workflow builds and pushes
     `ghcr.io/measured-health/mhai-multiagent:X.Y.Z` (CPU image) and creates
     the GitHub release.
- Run the end-to-end check before tagging.

## API contract

- Once the API gateway exists (#8), its **OpenAPI schema is the source of
  truth** for clients. Commit it as `openapi.json`, generated from the
  FastAPI app, so contract changes show up in review.
- `mhai-web` generates its TypeScript types from that file. Never hand-write
  them.
- Breaking a field, enum or endpoint needs a major version bump and a
  matching `mhai-web` PR. When #8 lands, add a CI step that fails if the
  committed schema and the app's schema diverge.

## Architecture decisions

Record decisions that are hard to reverse, or that others will ask "why?"
about, in `docs/adr/` using the [template](docs/adr/0000-template.md):

- tooling;
- infrastructure;
- data model;
- security design;
- clinical logic.

Number them in sequence and never delete one. Mark it *Superseded by
NNNN* instead.

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
