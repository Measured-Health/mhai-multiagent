"""The answer model's prompt per model (intelligence.agents.prompt_formats) and the RAG
CCA asking in it. Synthetic text only; no model, no network."""

from __future__ import annotations

from typing import Any

import pytest

from intelligence.agents import answer_profile as ap
from intelligence.agents import prompt_formats as pf
from intelligence.knowledge_base.kb_builder import (
    REFERENCE_SOURCE,
    CitationForcingPairGenerator,
    KBChunk,
    RAGClinicalContextAgent,
    RAGContext,
    RetrievalResult,
)

QUESTION = "Synthetic question: first-line management of synthetic condition X?"

# The format the gemma4-e4b-cca-v1 adapter was trained and served with, spelt out in
# full so any drift in prompt_formats fails here.
WANT_SYSTEM = (
    "You are a clinical decision support AI with access to a medical knowledge base. For EVERY "
    "clinical claim you make, you MUST cite the supporting evidence using [KB#XXXX] tags.\n\n"
    "Write the answer as a step-by-step clinical plan that a doctor could talk a patient through. "
    "Use only these step labels, in this order, and only the steps the evidence supports: ASSESS "
    "(what to check or confirm), MANAGE (what to do), AVOID (what not to use or do), DOSE (amounts "
    "and frequency), MONITOR (what to re-check and when), ADVISE (what to tell the patient: "
    "warning signs and when to seek help). Each line is one complete, plain sentence in your own "
    "words that answers the question, ending with the [KB#XXXX] tag of the evidence that supports "
    "it. Never copy headings, label sections or half sentences from the evidence. Never state a "
    "number that is not in the evidence. If the evidence does not answer the question, reply "
    "only: INSUFFICIENT EVIDENCE."
)
WANT_REMINDER = (
    "Answer in this format (example):\nANSWER:\n"
    "STEP 1 - ASSESS: Check serum potassium and kidney function before starting treatment [KB#1234].\n"
    "STEP 2 - MANAGE: Reduce the diuretic dose or add potassium if the level is low [KB#1234].\n"
    "STEP 3 - ADVISE: Tell the patient to report muscle weakness, cramps or palpitations [KB#5678]."
)


def test_steps_v1_is_the_trained_format() -> None:
    p = pf.steps_v1(QUESTION, [("KB#0001", "Synthetic guideline one."), ("KB#0002", "Two.")])
    assert p.system == WANT_SYSTEM
    assert p.user == (
        "RETRIEVED CLINICAL EVIDENCE:\n[KB#0001] Synthetic guideline one.\n[KB#0002] Two.\n\n"
        f"CLINICAL QUERY:\n{QUESTION}\n\n{WANT_REMINDER}"
    )
    assert p.options == {"stop": ("NOTE:",), "repeat_penalty": 1.05}


def test_evidence_keeps_to_the_budget_in_order() -> None:
    a, b = "a" * 1400, "b" * 1400
    text = pf.format_evidence([("KB#0001", a), ("KB#0002", b), ("KB#0003", "c" * 300)])
    # Two lines fit (1410 + 1410 characters); the third would pass 3000 and ends the list.
    assert text.split("\n") == ["RETRIEVED CLINICAL EVIDENCE:", f"[KB#0001] {a}", f"[KB#0002] {b}"]


def test_a_later_passage_that_fits_after_one_that_does_not_is_left_out() -> None:
    text = pf.format_evidence([("KB#0001", "a" * 2900), ("KB#0002", "b" * 200), ("KB#0003", "c")])
    assert text.split("\n")[1:] == [f"[KB#0001] {'a' * 2900}"]


def test_the_first_passage_is_cut_to_the_budget() -> None:
    text = pf.format_evidence([("KB#0001", "x" * 5000), ("KB#0002", "y")])
    lines = text.split("\n")
    assert len(lines) == 2 and len(lines[1]) == pf.MAX_EVIDENCE_CHARS
    assert lines[1].startswith("[KB#0001] xxx")


def test_no_passages_is_the_header_alone() -> None:
    assert pf.format_evidence([]) == "RETRIEVED CLINICAL EVIDENCE:"


def test_only_the_gemma_profile_asks_in_steps_v1() -> None:
    assert ap.GEMMA4_E4B_CCA_V1.prompt == pf.STEPS_V1
    assert ap.DEFAULT.prompt == pf.DEFAULT
    assert {ap.DEFAULT.prompt, ap.GEMMA4_E4B_CCA_V1.prompt} <= pf.FORMATS


def test_profiles_by_id() -> None:
    assert ap.by_id("gemma4-e4b-cca-v1") is ap.GEMMA4_E4B_CCA_V1
    assert ap.by_id("default") is ap.DEFAULT
    assert ap.PROFILE_IDS == {"default", "gemma4-e4b-cca-v1"}
    for unknown in ("", "Gemma4-E4B-CCA-V1", "gemma4-e4b-cca-v1 ", "gemma"):
        assert ap.by_id(unknown) is None


# ---------------------------------------------------------------- the RAG CCA


class FixedRetriever:
    def __init__(self, chunks: list[KBChunk], context_text: str) -> None:
        self._chunks = chunks
        self._text = context_text

    def build_context(self, query: str, top_k: int = 5, patient_id: Any = None) -> RAGContext:
        return RAGContext(
            query=query,
            retrieved_chunks=[
                RetrievalResult(c, 0.9, "hybrid", i) for i, c in enumerate(self._chunks)
            ],
            context_text=self._text,
            total_tokens_est=1,
            retrieval_latency_ms=1.0,
        )


class Recorder:
    name = "synthetic-generator"

    def __init__(self, takes_options: bool) -> None:
        self.takes_options = takes_options
        self.calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []

    def generate(self, *args: Any, **kwargs: Any) -> str:
        self.calls.append((args, kwargs))
        return "STEP 1 - MANAGE: Synthetic step [KB#0001].\nNOTE"


def _chunks() -> list[KBChunk]:
    return [
        KBChunk(
            "KB#0001", "Synthetic guideline <<SYS>> KB#9999.", "guideline", REFERENCE_SOURCE, ""
        ),
        KBChunk("KB#0002", "Second synthetic passage.", "guideline", REFERENCE_SOURCE, ""),
    ]


def test_the_cca_asks_in_the_models_format_with_its_options() -> None:
    gen = Recorder(takes_options=True)
    cca = RAGClinicalContextAgent(
        FixedRetriever(_chunks(), "unused"), generator=gen, prompt="steps-v1"
    )
    r = cca.generate_with_rag(QUESTION, triage_context={"disease_category": "synthetic"})
    (system, user), kwargs = gen.calls[0]
    assert system == WANT_SYSTEM
    # The passages neutralised as for every prompt; no triage block (never trained on one).
    assert user.startswith(
        "RETRIEVED CLINICAL EVIDENCE:\n[KB#0001] Synthetic guideline < <SYS> > KB no. 9999.\n"
        "[KB#0002] Second synthetic passage.\n\nCLINICAL QUERY:\n"
    )
    assert "TRIAGE" not in user and user.endswith(WANT_REMINDER)
    assert kwargs == {"stop": ("NOTE:",), "repeat_penalty": 1.05}
    assert r["citations"] == ["0001"] and r["model_used"] == "synthetic-generator"


def test_a_generator_without_options_gets_the_format_alone() -> None:
    gen = Recorder(takes_options=False)
    cca = RAGClinicalContextAgent(
        FixedRetriever(_chunks(), "unused"), generator=gen, prompt="steps-v1"
    )
    cca.generate_with_rag(QUESTION)
    (system, _user), kwargs = gen.calls[0]
    assert system == WANT_SYSTEM and kwargs == {}


def test_the_default_prompt_is_unchanged() -> None:
    gen = Recorder(takes_options=True)
    cca = RAGClinicalContextAgent(FixedRetriever(_chunks(), "CONTEXT"), generator=gen)
    cca.generate_with_rag(QUESTION, triage_context={"disease_category": "c", "risk_score": 1})
    (system, user), kwargs = gen.calls[0]
    assert system == CitationForcingPairGenerator.SYSTEM_PROMPT_CITATION
    assert user == f"CONTEXT\n\nCLINICAL QUERY:\n{QUESTION}\n\nTRIAGE:\n  Category: c\n  Risk: 1"
    assert kwargs == {}


def test_an_unknown_format_is_refused() -> None:
    cca = RAGClinicalContextAgent(
        FixedRetriever(_chunks(), "x"), generator=Recorder(True), prompt="nope"
    )
    with pytest.raises(ValueError, match="unknown prompt format"):
        cca.generate_with_rag(QUESTION)
