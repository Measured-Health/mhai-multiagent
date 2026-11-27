"""The HGA scores a claim against sentence windows of its cited chunk.

The NLI model (cross-encoder/nli-deberta-v3-base) is trained on sentence pairs: against a
long, many-fact chunk it called a sentence copied word for word from that chunk neutral
(0.99), so every live citation was neutral and answers escalated. The claim is now scored
against the chunk's windows (the whole chunk, each sentence, each adjacent pair; at most
NLI_MAX_WINDOWS). Entailment is the highest over the windows. Contradiction is the whole
chunk's (the check made before) or that of the claim's own sentence (aligned_windows): the
real model calls two different facts of one label contradictory (the adult-dose sentence
against the pediatric-dose one: 0.62), and the highest contradiction over every window
rejected 3 of the 7 sentences of the label below copied word for word. Safety first: PASS
only when a window entails and there is no contradiction; a contradiction vetoes even an
entailed claim.

The stub tests run everywhere; the ``models`` tests load the real NLI model on CPU
(``uv run pytest -m models tests/unit/test_hga_nli_windows.py``). Every text is synthetic,
written in the style of a public drug label.
"""

from __future__ import annotations

from collections.abc import Sequence

import pytest

from intelligence.agents.hga_nli import (
    NLI_MAX_WINDOWS,
    CitationVerificationPipeline,
    NLILabel,
    NLIResult,
    VerificationAction,
    aligned_windows,
    premise_sentences,
    premise_windows,
)
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

LABEL = (
    "Synthetic trace element injection is indicated in adult and pediatric patients as a "
    "source of zinc for parenteral nutrition. "
    "It must be diluted before intravenous infusion and is not for direct intravenous push. "
    "The recommended adult dosage is 3 mg/day for metabolically stable patients. "
    "Patients with increased losses may need additional zinc. "
    "The recommended dosage in pediatric patients is 250 mcg/kg/day for preterm neonates "
    "and 100 mcg/kg/day for term neonates up to 3 months. "
    "Monitor zinc concentrations in patients receiving long-term parenteral nutrition. "
    "Store at room temperature and discard any unused portion after the first puncture."
)
VERBATIM = "The recommended adult dosage is 3 mg/day for metabolically stable patients."
WRONG_DOSE = "The recommended adult dosage is 30 mg/day."
WRONG_ROUTE = "The injection should be given as a direct intravenous push."


class _KB:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.chunks = {c.chunk_id: c for c in chunks}

    def get_chunk(self, cid: str) -> KBChunk | None:
        return self.chunks.get(cid)


def _kb(text: str = LABEL) -> _KB:
    return _KB([KBChunk("KB#0010", text, "guideline", REFERENCE_SOURCE, "")])


class WindowNLI:
    """A stand-in NLI model: ``exact`` scores a premise exactly, else ``table`` maps a
    substring of the premise -> (entailment, contradiction), the highest match winning;
    anything else is neutral. Records every premise."""

    def __init__(self, table: dict[str, tuple[float, float]]) -> None:
        self.table = table
        self.exact: dict[str, tuple[float, float]] = {}
        self.premises: list[str] = []

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        self.premises.append(premise)
        ent = con = 0.0
        if premise in self.exact:
            ent, con = self.exact[premise]
        else:
            for key, (e, c) in self.table.items():
                if key in premise:
                    ent, con = max(ent, e), max(con, c)
        neu = max(0.0, 1.0 - ent - con)
        probs = {"entailment": ent, "contradiction": con, "neutral": neu}
        label = max(probs, key=lambda k: probs[k])
        return NLIResult(premise[:100], hypothesis[:100], NLILabel(label), probs[label], probs)


def _check(nli: object, claim: str, text: str = LABEL) -> tuple[VerificationAction, str]:
    pipe = CitationVerificationPipeline(nli, _kb(text))  # type: ignore[arg-type]
    (r,) = pipe.verify_response(f"{claim[:-1]} [KB#0010].").citation_results
    return r.action, r.code


# ---------------------------------------------------------------- the windows


def test_windows_are_the_whole_chunk_sentences_and_adjacent_pairs() -> None:
    sents = premise_sentences(LABEL)
    assert len(sents) == 7
    w = premise_windows(LABEL)
    assert w[0] == LABEL  # the whole chunk, as before: nothing it found is lost
    assert w[1:8] == sents
    assert w[8:] == [f"{a} {b}" for a, b in zip(sents, sents[1:], strict=False)]


def test_a_one_sentence_chunk_is_one_window() -> None:
    assert premise_windows("Metformin 500 mg twice daily.") == ["Metformin 500 mg twice daily."]
    assert premise_windows("") == [""]


def test_a_short_fragment_joins_the_next_sentence() -> None:
    assert premise_sentences("Dose. Give 5 mg once daily with food.") == [
        "Dose. Give 5 mg once daily with food."
    ]


def test_a_long_chunk_is_capped_and_every_sentence_is_still_read() -> None:
    sents = [f"Synthetic fact number {i} is stated here." for i in range(60)]
    text = " ".join(sents)
    w = premise_windows(text)
    assert len(w) == NLI_MAX_WINDOWS
    assert w[0] == text
    # Every sentence is in a window other than the whole chunk (grouped runs).
    for s in sents:
        assert any(s in x for x in w[1:])
    assert len(premise_windows(text, max_windows=5)) == 5


def test_the_pipeline_scores_at_most_the_cap_per_citation() -> None:
    text = " ".join(f"Synthetic fact number {i} is stated here." for i in range(60))
    nli = WindowNLI({})
    _check(nli, "Synthetic fact number 7 is stated here.", text)
    assert len(nli.premises) == NLI_MAX_WINDOWS


# ---------------------------------------------------------------- the decision rule


def test_a_verbatim_sentence_of_a_long_chunk_is_entailed() -> None:
    # The live failure: the whole chunk reads neutral, the sentence's window entails.
    nli = WindowNLI({"is 3 mg/day for metabolically stable patients.": (0.99, 0.0)})
    nli.exact[LABEL] = (0.01, 0.0)  # the whole chunk: neutral, as the live model said
    assert _check(nli, VERBATIM) == (VerificationAction.PASS, "entailed")
    assert LABEL in nli.premises


def test_a_contradiction_of_the_whole_chunk_vetoes_as_before() -> None:
    nli = WindowNLI({"is 3 mg/day for": (0.99, 0.0)})
    nli.exact[LABEL] = (0.0, 0.96)  # the real model on this label and WRONG_DOSE: 0.96
    assert _check(nli, WRONG_DOSE) == (VerificationAction.REDACT, "contradicted")


def test_a_contradiction_of_the_claims_own_sentence_vetoes() -> None:
    nli = WindowNLI({"is 3 mg/day for": (0.0, 1.0)})
    nli.exact[LABEL] = (0.0, 0.1)  # the whole chunk alone would not have caught it
    assert _check(nli, WRONG_DOSE) == (VerificationAction.REDACT, "contradicted")


def test_a_contradiction_from_another_sentence_does_not_veto() -> None:
    # The real model: the copied adult-dose sentence against the pediatric-dose
    # sentence is 0.62 contradiction; the claim's own sentence entails it (0.997).
    nli = WindowNLI(
        {"is 3 mg/day for metabolically": (0.997, 0.0), "in pediatric patients": (0.0, 0.62)}
    )
    nli.exact[LABEL] = (0.0, 0.002)
    assert _check(nli, VERBATIM) == (VerificationAction.PASS, "entailed")


def test_the_claims_own_sentence_is_the_one_sharing_most_words() -> None:
    w = premise_windows(LABEL)
    sents = premise_sentences(LABEL)
    assert [w[i] for i in aligned_windows(w, VERBATIM)] == [sents[2]]
    assert [w[i] for i in aligned_windows(w, WRONG_DOSE)] == [sents[2]]
    # A claim drawing words from two sentences is aligned with the pair.
    assert [w[i] for i in aligned_windows(w, WRONG_ROUTE)] == [f"{sents[0]} {sents[1]}"]
    # Never the whole chunk, and nothing for a claim that shares no word with it.
    assert all(0 not in aligned_windows(w, s) for s in sents)
    assert aligned_windows(w, "Qwerty zxcvb.") == []
    assert aligned_windows([LABEL], VERBATIM) == []


def test_high_entailment_and_high_contradiction_is_not_a_pass() -> None:
    # The contentless claim the live probe found: entailed (0.996) and contradicted
    # (0.987). Safety first: a contradiction, never a PASS.
    claim = "The recommended dosage in pediatric patients is shown by."
    nli = WindowNLI({"The recommended dosage in pediatric": (0.996, 0.987)})
    nli.exact[LABEL] = (0.0, 0.1)
    action, code = _check(nli, claim)
    assert action != VerificationAction.PASS
    assert (action, code) == (VerificationAction.REDACT, "contradicted")
    # And when only the whole chunk contradicts it (the real model: 0.75).
    nli = WindowNLI({"The recommended dosage in pediatric": (0.988, 0.0)})
    nli.exact[LABEL] = (0.053, 0.75)
    assert _check(nli, claim) == (VerificationAction.REDACT, "contradicted")


def test_entailment_below_the_threshold_in_every_window_is_flagged() -> None:
    nli = WindowNLI({"adult dosage": (0.69, 0.0)})
    assert _check(nli, VERBATIM) == (VerificationAction.FLAG, "not_entailed")


def test_contradiction_just_below_the_threshold_does_not_veto() -> None:
    nli = WindowNLI({"is 3 mg/day for": (0.95, 0.49)})
    nli.exact[LABEL] = (0.0, 0.49)
    assert _check(nli, VERBATIM) == (VerificationAction.PASS, "entailed")


def test_a_verifier_that_scores_nothing_never_supports() -> None:
    class Empty:
        def verify_pairs(self, pairs: list[tuple[str, str]]) -> list[NLIResult]:
            return []

    assert _check(Empty(), VERBATIM) == (VerificationAction.FLAG, "not_entailed")


def test_the_shown_result_carries_the_scores_counted() -> None:
    nli = WindowNLI({"is 3 mg/day for": (0.95, 0.0), "pediatric": (0.0, 0.8)})
    nli.exact[LABEL] = (0.0, 0.2)
    pipe = CitationVerificationPipeline(nli, _kb())  # type: ignore[arg-type]
    (r,) = pipe.verify_response(f"{VERBATIM[:-1]} [KB#0010].").citation_results
    assert r.action == VerificationAction.PASS
    assert r.nli_result.label == NLILabel.ENTAILMENT
    assert r.nli_result.logits["entailment"] == pytest.approx(0.95)
    assert r.nli_result.logits["contradiction"] == pytest.approx(0.2)
    assert r.nli_result.logits["contradiction_any_window"] == pytest.approx(0.8)


def test_the_rule_based_fallback_reads_the_whole_chunk() -> None:
    from intelligence.agents.hga_nli import NLIEntailmentVerifier

    v = NLIEntailmentVerifier.__new__(NLIEntailmentVerifier)
    v._loaded, v.model = False, None
    assert v.windows_for(LABEL) == [LABEL]


# ---------------------------------------------------------------- the real model (CPU)


@pytest.fixture(scope="module")
def real_pipeline() -> CitationVerificationPipeline:
    from intelligence.agents.hga_nli import NLIEntailmentVerifier

    nli = NLIEntailmentVerifier(device="cpu", require_model=True)
    return CitationVerificationPipeline(nli, _kb())


def _real(pipe: CitationVerificationPipeline, claim: str) -> tuple[VerificationAction, str]:
    (r,) = pipe.verify_response(f"{claim[:-1]} [KB#0010].").citation_results
    return r.action, r.code


@pytest.mark.models
def test_real_model_reads_the_whole_long_chunk_as_not_entailing(real_pipeline) -> None:
    # Why windows are needed: the whole chunk alone does not entail the copied sentence.
    r = real_pipeline.nli.verify(LABEL, VERBATIM)
    assert not (r.label == NLILabel.ENTAILMENT and r.confidence >= 0.7)


@pytest.mark.models
@pytest.mark.parametrize(
    "claim",
    [
        VERBATIM,
        "It must be diluted before intravenous infusion and is not for direct intravenous push.",
    ],
)
def test_real_model_entails_a_verbatim_sentence_of_a_long_chunk(real_pipeline, claim) -> None:
    assert _real(real_pipeline, claim) == (VerificationAction.PASS, "entailed")


@pytest.mark.models
@pytest.mark.parametrize("claim", [WRONG_DOSE, WRONG_ROUTE])
def test_real_model_contradicts_a_wrong_dose_or_route(real_pipeline, claim) -> None:
    assert _real(real_pipeline, claim) == (VerificationAction.REDACT, "contradicted")


@pytest.mark.models
def test_real_model_never_passes_a_contentless_claim(real_pipeline) -> None:
    action, _ = _real(real_pipeline, "The recommended dosage in pediatric patients is shown by.")
    assert action != VerificationAction.PASS


@pytest.mark.models
def test_real_model_batched_windows_match_one_by_one(real_pipeline) -> None:
    nli = real_pipeline.nli
    windows = premise_windows(LABEL)
    batched = nli.verify_pairs([(w, VERBATIM) for w in windows])
    assert len(batched) == len(windows)
    for w, b in zip(windows, batched, strict=True):
        one = nli.verify(w, VERBATIM)
        assert b.label == one.label
        assert b.confidence == pytest.approx(one.confidence, abs=1e-3)
