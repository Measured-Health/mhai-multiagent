"""The answer model's prompt, per model: how the CCA asks a trained ``cca`` model.

A trained model answers best when it is asked the way it was trained. The prompt is part
of the model's answer profile (:mod:`intelligence.agents.answer_profile`), so a format here
reaches only the models whose profile names it; every other model gets the default prompt
(``CitationForcingPairGenerator.SYSTEM_PROMPT_CITATION`` and the retriever's context),
unchanged.

:data:`STEPS_V1` is the format the ``gemma4-e4b-cca-v1`` adapter was trained and served
with (the researcher's serving code, reviewed as data, never imported or run):

* **System:** the citation system prompt (one sentence: cite every claim with
  ``[KB#XXXX]``), a blank line, then the step instruction (labels ASSESS, MANAGE, AVOID,
  DOSE, MONITOR, ADVISE, in that order; one sentence per line ending with its tag; reply
  only ``INSUFFICIENT EVIDENCE`` when the evidence doesn't answer).
* **User:** ``RETRIEVED CLINICAL EVIDENCE:`` then one ``[KB#id] text`` line per retrieved
  passage, in retrieval order, within a 3000-character budget (a passage that would pass
  the budget ends the list, except the first, which is cut to the budget so the model
  always sees one); a blank line, ``CLINICAL QUERY:`` and the question; a blank line and
  the answer-format reminder with its three-step example. No triage block: the model was
  never shown one.
* **Generation:** greedy (the local route's settings), a repetition penalty of 1.05, and
  a stop at ``NOTE:`` (the adapter appends a boilerplate note, which is discarded).

The passages are neutralised as for every prompt (``neutralise_chunk_text``): a passage
can't forge a citation tag or a prompt marker. Nothing here logs a question or a passage.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass, field
from typing import Any

DEFAULT = "default"
STEPS_V1 = "steps-v1"
FORMATS = frozenset({DEFAULT, STEPS_V1})

EVIDENCE_HEADER = "RETRIEVED CLINICAL EVIDENCE:"
MAX_EVIDENCE_CHARS = 3000

STEPS_V1_SYSTEM = (
    "You are a clinical decision support AI with access to a medical knowledge base. For EVERY "
    "clinical claim you make, you MUST cite the supporting evidence using [KB#XXXX] tags."
)
STEPS_V1_INSTRUCTION = (
    "Write the answer as a step-by-step clinical plan that a doctor could talk a patient through. "
    "Use only these step labels, in this order, and only the steps the evidence supports: "
    "ASSESS (what to check or confirm), MANAGE (what to do), AVOID (what not to use or do), DOSE "
    "(amounts and frequency), MONITOR (what to re-check and when), ADVISE (what to tell the "
    "patient: warning signs and when to seek help). Each line is one complete, plain sentence in "
    "your own words that answers the question, ending with the [KB#XXXX] tag of the evidence that "
    "supports it. Never copy headings, label sections or half sentences from the evidence. Never "
    "state a number that is not in the evidence. If the evidence does not answer the question, "
    "reply only: INSUFFICIENT EVIDENCE."
)
STEPS_V1_REMINDER = (
    "Answer in this format (example):\n"
    "ANSWER:\n"
    "STEP 1 - ASSESS: Check serum potassium and kidney function before starting treatment "
    "[KB#1234].\n"
    "STEP 2 - MANAGE: Reduce the diuretic dose or add potassium if the level is low [KB#1234].\n"
    "STEP 3 - ADVISE: Tell the patient to report muscle weakness, cramps or palpitations "
    "[KB#5678]."
)


@dataclass(frozen=True)
class Prompt:
    """One prompt for the generator: the system and user messages, and the generation
    options the format needs (empty for the default: the generator's own settings)."""

    system: str
    user: str
    options: dict[str, Any] = field(default_factory=dict)


def format_evidence(passages: Sequence[tuple[str, str]], budget: int = MAX_EVIDENCE_CHARS) -> str:
    """The evidence block of :data:`STEPS_V1`: ``passages`` are (chunk id, neutralised text)
    in retrieval order."""
    lines, total = [EVIDENCE_HEADER], 0
    for chunk_id, text in passages:
        line = f"[{chunk_id}] {text}"
        if total + len(line) > budget:
            if total == 0:  # the model always sees one passage
                lines.append(line[:budget])
            break
        lines.append(line)
        total += len(line)
    return "\n".join(lines)


def steps_v1(question: str, passages: Sequence[tuple[str, str]]) -> Prompt:
    """The :data:`STEPS_V1` prompt for ``question`` over ``passages`` (see the module
    docstring)."""
    return Prompt(
        system=f"{STEPS_V1_SYSTEM}\n\n{STEPS_V1_INSTRUCTION}",
        user=f"{format_evidence(passages)}\n\nCLINICAL QUERY:\n{question}\n\n{STEPS_V1_REMINDER}",
        options={"stop": ("NOTE:",), "repeat_penalty": 1.05},
    )


__all__ = [
    "DEFAULT",
    "EVIDENCE_HEADER",
    "FORMATS",
    "MAX_EVIDENCE_CHARS",
    "STEPS_V1",
    "Prompt",
    "format_evidence",
    "steps_v1",
]
