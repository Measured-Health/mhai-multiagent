## What and why

<!-- What changed and the reason. Link issues: "Closes #123". -->

## How it was tested

<!-- Commands run and their results. For model, threshold or prompt changes,
attach the scripts/e2e_models.py report. -->

## What wasn't verified

<!-- Anything you couldn't check, and why. Write "Nothing" if so. Reviewers
treat claims not listed in the testing section as unverified. -->

## Checklist

- [ ] `make lint` and `make test` pass locally
- [ ] Tests added or updated; security fixes have a test in `tests/security` that fails without the fix
- [ ] Walked through the [SKILLS.md review checklist](../blob/main/SKILLS.md#review-checklist) (patient scoping, fail closed, identity, PHI)
- [ ] No real patient data, secrets or credentials anywhere in the diff, tests or this description
- [ ] Clinical change (thresholds, trust weights, prompts, models)? Labelled `clinical-review`, with the end-to-end report attached
- [ ] CHANGELOG.md updated under *Unreleased* for user-visible changes
- [ ] New architectural decision? ADR added in `docs/adr/`
