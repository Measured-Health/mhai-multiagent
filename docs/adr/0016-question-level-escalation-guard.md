# 0016. A question-level escalation guard before any answer can pass

- **Status:** Proposed 2026-11-14 (clinical review: the red-flag list is a pilot list pending clinical sign-off)
- **Date:** 2026-11-14
- **Deciders:** product owner; clinical lead and a second clinician for the rule set
- **Related:** ADR 0005 (fail closed); #183 (answer profile); #185 (citation tags stripped before NLI)

## Context

Answer verification (`boundary/models._verify_with`, the HGA with NLI grounding) checks that
each sentence of an answer is supported by the chunk it cites. It does not check that the
answer is the right response to the question.

#185 fixed a bug: the NLI model read each claim with its `[KB#nnnn]` tag and scored every
cited claim neutral. With the fix, a synthetic reply that only recites the on-topic chunk
passes, with high trust, on all 36 must-escalate dev items: red flags (confusion with a very
high glucose, "can they go home?"), missing patient information, and doses no guideline
gives. Before the fix, those replies mostly landed in `REGENERATE` by accident of the bug.
The bug was hiding a missing safety layer. The cloud route escalates only when the provider
itself declares `insufficient_evidence`; the trained local model declines on 7 of 36.

## Decision

1. **A deterministic guard runs on every route, before an NLI decision can stand.** It is in
   `_verify_with`, which the local client, every cloud client, the answer gate and
   `eval-served` share. It can only make a decision stricter.
2. **Four checks** (`guardrails/escalation_guard.py`):
   - **`red_flag`:** danger signs in the masked question or the patient's current data
     (symptoms; structured vitals and labs where present). Values beat words: a number with
     its label, in the analyte's canonical unit, decides over a vague word. ESCALATE.
   - **`missing_information`:** the question asks for a judgement about this patient (a
     diagnosis, a severity, whether they can go home, a child's dose) and none of the values
     it needs is given. ESCALATE.
   - **`out_of_scope`:** the question asks for a dose, regimen or titration and no cited
     chunk gives a dose for the drug (or, with no drug named, the condition). ESCALATE.
   - **`recital`:** every answer sentence is a near-verbatim copy of a chunk sentence and the
     answer is not relevant to the question (cosine below 0.20 on the pinned
     all-MiniLM-L6-v2 embedder; without it, no shared content word). A PASS becomes
     REGENERATE. Conservative on purpose: the first three checks carry the safety weight.
3. **The rules are data, versioned and pinned.** `guardrails/rules/escalation-guard.yaml`
   (schema `mhai.escalation_guard.rules/1`) has a version and a status, and cites a source for
   each rule. Its SHA-256 is pinned in the code: an unreviewed edit refuses to load. Nothing
   is learned at run time.
4. **Fail closed.** A rule set that is missing, malformed or not the pinned one stops the
   pipeline worker at start-up (exit 2). A verification that can't load it, or a check that
   raises, escalates with `guard_unavailable`. Input too long to read whole escalates; it is
   never truncated.
5. **Audited as codes.** The reasons go to the decision row (`guardrail_report.guard_reasons`
   and the rule ids, `guard_rules`) and to the `query.completed` fact (`guard_reasons`, a
   closed set core checks: `config.GuardReasons`). The rule set's version string
   (`escalation-guard@<version>#<sha12>`) is in every decision's `model_versions`. Never text.

## Consequences

- Recitals to must-escalate questions escalate on every route; the PR records the measured
  counts and the over-escalation on the dev split's answerable items.
- The trust score is still computed and stored, so a reviewer sees that the answer was
  grounded and why it was escalated anyway. The band is `low` (the decision's).
- Over-escalation is the accepted cost: a pattern list escalates some answerable questions
  (a danger word in a general question). The pilot list errs that way.
- Clinical change control: the list is a pilot list. Clinical sign-off (the clinical lead and
  a second clinician) is a TODO gate before a second hospital, as for the EMR rule set.

## Security and threat model

- **Assets:** clinical safety (an unchecked or unsafe answer must never be shown as final);
  the audit trail; patient data in the question.
- **Actors and boundaries:** the clinician's question crosses browser → BFF → core → NATS →
  intelligence (masked) → the guard. The rule set is a file in the image, read once at start.
- **Threats and controls:**
  - *Tampering with the rules* (an edited YAML in the image or the repo): the SHA-256 pin
    refuses it; a YAML duplicate key is refused (it would silently replace a rule); the safe
    loader builds plain data only.
  - *Evasion by phrasing* (a red flag the patterns miss): residual; mitigated by values over
    words, negation scoped to a short phrase (a list joined by "and"/"but" ends it), and the
    model's own decline and NLI still running. Not a security boundary on its own.
  - *Denial of service* (a crafted question against the regexes): patterns are bounded
    (`{0,40}` style gaps, no nested quantifiers on user text), input is capped at 40,000
    characters and core already caps the question at 4,000.
  - *Information disclosure:* nothing logs the question, patient data or answer; the
    verdict, the log line and the fact carry codes only (tested).
  - *Repudiation:* every decision records the rule set's version and SHA prefix, and the
    reasons, in the decision row and the hash-chained audit fact.
- **Abuse-case tests:** `tests/unit/test_escalation_guard.py` (bad, edited, unreadable rule
  sets; overlong input; a raising check; codes only) and
  `tests/unit/test_escalation_guard_wiring.py` (every route; the worker refuses to start; the
  CLI exits 2); `TestQueryCompletedGuardReasons` and `TestGuardReasonsMatchWorker` in core.
- **Residual risk and sign-off:** the list's clinical coverage (pilot, pending clinical
  sign-off); regex matching is lexical (misspellings, other languages). TODO.md carries the
  sign-off gate.

## Alternatives considered

- **Raise the NLI or trust thresholds.** NLI measures grounding, not appropriateness; a
  grounded recital scores high on any threshold.
- **Leave it to the model's decline.** The trained model declines on 7 of 36; cloud models
  vary by provider. A deterministic layer is needed whatever the model.
- **A learned classifier.** Not deterministic, not reviewable line by line by a clinician;
  may come later as an extra layer, never instead of the rule set.
