"""The HGA's support gate (#5) and retrieved-only citations (#28).

An answer can PASS only when every claim has a cited chunk, retrieved for this question,
that entails it (NLI entailment at or above the pipeline's threshold), and every number in
the claim appears in the claim's cited chunks. An unsupported claim caps the decision at
REGENERATE; a contradicted claim, or a citation of another patient's record, escalates. The
weighted trust score is still computed and stored, but it can't override the gate.

A citation of a chunk that wasn't retrieved for the question is redacted (not_retrieved)
before any NLI call, and the claim loses that support. The per-citation results (chunk id,
NLI label and score, action, reason code) go into the job's decision row.

Every text here is synthetic.
"""

from __future__ import annotations

from collections.abc import Callable, Sequence
from typing import Any

import pytest

from intelligence.agents.hga_nli import (
    CitationVerificationPipeline,
    NLIBackedHGA,
    NLILabel,
    NLIResult,
    VerificationAction,
    numbers_in,
)
from intelligence.agents.orchestrator import DynamicOrchestratorAgent
from intelligence.boundary.jobs.run_query import RunQuery
from intelligence.boundary.models import Generation, Verification, _verify_with
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

PATIENT = "PAT-GATE"
OTHER_PATIENT = "PAT-OTHER"
SMEAR = "Malaria smear positive for Plasmodium falciparum."
DOSE = "Artemether-lumefantrine 80/480 mg twice daily for 3 days."
OTHER = "Type 2 diabetes mellitus, HbA1c 9.8 percent."
UNSUPPORTED = "The patient has stage 4 pancreatic cancer and requires immediate chemotherapy"


class ScriptedNLI:
    """ENTAILMENT (0.99) unless the hypothesis contains one of ``labels``' keys; records
    each (premise, hypothesis) it is given."""

    def __init__(self, labels: dict[str, tuple[NLILabel, float]] | None = None) -> None:
        self.labels = labels or {}
        self.calls: list[tuple[str, str]] = []

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        self.calls.append((premise, hypothesis))
        for key, (label, conf) in self.labels.items():
            if key in hypothesis:
                return NLIResult(premise[:100], hypothesis[:100], label, conf)
        return NLIResult(premise[:100], hypothesis[:100], NLILabel.ENTAILMENT, 0.99)


class _KB:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.chunks = {c.chunk_id: c for c in chunks}

    def get_chunk(self, cid: str) -> KBChunk | None:
        return self.chunks.get(cid)


def _kb() -> _KB:
    return _KB(
        [
            KBChunk("KB#0001", SMEAR, "lab_result", PATIENT, "S-1"),
            KBChunk("KB#0002", DOSE, "guideline", REFERENCE_SOURCE, ""),
            KBChunk("KB#0003", OTHER, "diagnosis", OTHER_PATIENT, "S-2"),
        ]
    )


ALL = frozenset({"KB#0001", "KB#0002"})


def _hga(nli: ScriptedNLI | None = None) -> NLIBackedHGA:
    return NLIBackedHGA(citation_pipeline=CitationVerificationPipeline(nli or ScriptedNLI(), _kb()))


def _verify(text: str, nli: ScriptedNLI | None = None, **kw: Any) -> dict[str, Any]:
    kw.setdefault("allowed_chunk_ids", ALL)
    return _hga(nli).verify(text, patient_id=PATIENT, **kw)


NEUTRAL = {"chemotherapy": (NLILabel.NEUTRAL, 0.95)}


# ---------------------------------------------------------------- #5: the support gate


def test_a_cited_claim_no_source_supports_does_not_pass() -> None:
    """The issue's case: fluent, cited, and neutral to its source."""
    out = _verify(f"{UNSUPPORTED} [KB#0001].", ScriptedNLI(NEUTRAL))

    # The weighted score alone clears the pass bar (0.57 against 0.55)...
    assert out["trust_score"] >= 0.55
    # ...but no cited chunk entails the claim, so it can't pass.
    assert out["decision"] == "regenerate"
    assert out["badge"] == "AMBER"
    assert out["support_gate"] == "unsupported_claim"


def test_the_trust_score_cannot_override_the_gate() -> None:
    out = _verify(f"{UNSUPPORTED} [KB#0001].", ScriptedNLI(NEUTRAL), pass_threshold=0.0)
    assert out["decision"] == "regenerate"


def test_one_unsupported_claim_among_supported_ones_does_not_pass() -> None:
    text = f"{SMEAR[:-1]} [KB#0001]. {UNSUPPORTED} [KB#0001]. {DOSE[:-1]} [KB#0002]."
    out = _verify(text, ScriptedNLI(NEUTRAL))
    assert out["nli_score"] > 0.6  # two of three entailed
    assert out["decision"] == "regenerate"
    assert out["support_gate"] == "unsupported_claim"


def test_an_uncited_claim_does_not_pass() -> None:
    out = _verify(f"{SMEAR[:-1]} [KB#0001]. Start chemotherapy at once today.")
    assert out["decision"] == "regenerate"
    assert out["support_gate"] == "unsupported_claim"


def test_a_contradicted_claim_escalates() -> None:
    text = f"{SMEAR[:-1]} [KB#0001]. Malaria smear negative, no treatment needed [KB#0001]."
    nli = ScriptedNLI({"negative": (NLILabel.CONTRADICTION, 0.9)})
    out = _verify(text, nli)
    assert out["decision"] == "escalate"
    assert out["support_gate"] == "contradicted"
    [c] = [c for c in out["citations"] if c["action"] == "REDACT"]
    assert (c["nli_label"], c["reason"]) == ("CONTRADICTION", "contradicted")


def test_a_citation_of_another_patients_record_escalates() -> None:
    out = _verify(f"{SMEAR[:-1]} [KB#0001]. HbA1c is 9.8 percent [KB#0003].")
    assert out["decision"] == "escalate"
    assert out["support_gate"] == "other_patient"


def test_no_citation_pipeline_never_passes() -> None:
    """Without the NLI citation check nothing can show a claim is supported."""
    out = NLIBackedHGA().verify(f"{SMEAR[:-1]} [KB#0001].", patient_id=PATIENT)
    assert out["decision"] != "pass"
    assert out["support_gate"] == "no_citation_check"


def test_an_answer_with_no_claims_does_not_pass() -> None:
    out = _verify("OK.")
    assert out["decision"] != "pass"


# ---------------------------------------------------------------- numbers


def test_a_number_not_in_the_cited_source_does_not_pass() -> None:
    """The NLI model says entailed; the dose isn't the source's."""
    out = _verify("Artemether-lumefantrine 160/960 mg twice daily for 3 days [KB#0002].")
    assert out["decision"] == "regenerate"
    assert out["support_gate"] == "unsupported_claim"
    [c] = out["citations"]
    assert (c["nli_label"], c["action"], c["reason"]) == (
        "ENTAILMENT",
        "FLAG",
        "number_not_in_source",
    )


def test_numbers_in_the_cited_source_pass() -> None:
    out = _verify("Give artemether-lumefantrine 80/480 mg twice daily for 3 days [KB#0002].")
    assert out["decision"] == "pass"


def test_a_number_may_come_from_any_of_the_claims_cited_sources() -> None:
    text = "Smear positive for falciparum, so give 80/480 mg twice daily [KB#0001] [KB#0002]."
    assert _verify(text)["decision"] == "pass"


def test_a_list_number_is_not_a_claim_number() -> None:
    assert _verify(f"1. {DOSE[:-1]} [KB#0002].")["decision"] == "pass"


@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("80/480 mg for 3 days", {"80", "480", "3"}),
        ("1,000 mg", {"1000"}),
        ("5.0 mg or 0.50 mg", {"5", "0.5"}),
        ("days 1,2,3", {"1", "2", "3"}),
        ("5-10 mg", {"5", "10"}),
        ("ICD-10 code B50.9, HbA1c, SpO2, T2DM, COVID-19", set()),
        ("Smear positive [KB#0001].", set()),
        ("no numbers here.", set()),
    ],
)
def test_numbers_in(text: str, want: set[str]) -> None:
    assert numbers_in(text) == want


# ---------------------------------------------------------------- #28: retrieved only


def test_a_citation_not_retrieved_is_redacted_without_nli() -> None:
    nli = ScriptedNLI()
    pipe = CitationVerificationPipeline(nli, _kb())
    report = pipe.verify_response(
        f"{SMEAR[:-1]} [KB#0001].", patient_id=PATIENT, allowed_chunk_ids={"KB#0002"}
    )
    [r] = report.citation_results
    assert r.action == VerificationAction.REDACT
    assert r.code == "not_retrieved"
    assert nli.calls == []


def test_a_claim_whose_only_citation_was_not_retrieved_does_not_pass() -> None:
    out = _verify(f"{SMEAR[:-1]} [KB#0001].", allowed_chunk_ids={"KB#0002"})
    assert out["decision"] != "pass"
    [c] = out["citations"]
    assert (c["chunk_id"], c["action"], c["reason"]) == ("KB#0001", "REDACT", "not_retrieved")


def test_nothing_retrieved_means_nothing_passes() -> None:
    assert _verify(f"{SMEAR[:-1]} [KB#0001].", allowed_chunk_ids=set())["decision"] != "pass"


def test_a_fully_supported_answer_passes_with_per_citation_results() -> None:
    text = f"{SMEAR[:-1]} [KB#0001]. {DOSE[:-1]} [KB#0002]."
    out = _verify(text)
    assert out["decision"] == "pass"
    assert out["support_gate"] == "passed"
    assert out["citations"] == [
        {
            "chunk_id": "KB#0001",
            "claim_index": 0,
            "claim_text": f"{SMEAR[:-1]} [KB#0001].",
            "nli_label": "ENTAILMENT",
            "nli_confidence": 0.99,
            "action": "PASS",
            "reason": "entailed",
        },
        {
            "chunk_id": "KB#0002",
            "claim_index": 1,
            "claim_text": f"{DOSE[:-1]} [KB#0002].",
            "nli_label": "ENTAILMENT",
            "nli_confidence": 0.99,
            "action": "PASS",
            "reason": "entailed",
        },
    ]


# ---------------------------------------------------------------- the job's verification

QUESTION = "Synthetic question: how is malaria confirmed?"
MALARIA = "Malaria requires parasitological confirmation by microscopy or RDT."
CLAIM = "Malaria requires parasitological confirmation by microscopy or RDT"


def _orch() -> DynamicOrchestratorAgent:
    kb = _KB(
        [
            KBChunk("KB#0002", MALARIA, "guideline", REFERENCE_SOURCE, ""),
            KBChunk("KB#0005", "URTI: viral majority.", "guideline", REFERENCE_SOURCE, ""),
        ]
    )
    hga = NLIBackedHGA(citation_pipeline=CitationVerificationPipeline(ScriptedNLI(), kb))
    return DynamicOrchestratorAgent(rag_cca=None, hga=hga)


def _gen(retrieved: tuple[str, ...]) -> Generation:
    return Generation(
        question=QUESTION,
        response=f"{CLAIM} [KB#0002].",
        cited_chunk_ids=tuple(c for c in ("KB#0002",) if c in retrieved),
        retrieved_chunk_ids=retrieved,
        available=True,
    )


def test_the_job_verifies_against_the_retrieved_chunks_only() -> None:
    v = _verify_with(_orch(), _gen(("KB#0005",)))
    assert v.verified and v.decision != "PASS"
    assert v.support_gate == "unsupported_claim"
    [c] = v.citation_checks
    assert (c["chunk_id"], c["action"], c["reason"]) == ("KB#0002", "REDACT", "not_retrieved")


def test_the_job_passes_a_supported_answer_and_keeps_its_checks() -> None:
    v = _verify_with(_orch(), _gen(("KB#0002", "KB#0005")))
    assert v.decision == "PASS"
    assert v.support_gate == "passed"
    [c] = v.citation_checks
    assert (c["chunk_id"], c["nli_label"], c["nli_confidence"], c["action"], c["reason"]) == (
        "KB#0002",
        "ENTAILMENT",
        0.99,
        "PASS",
        "entailed",
    )
    # Never text in the Verification's repr (it can reach a log line).
    assert CLAIM not in repr(v)


def test_the_decision_row_stores_the_per_citation_results() -> None:
    class _Client:
        def model_versions(self) -> dict[str, str]:
            return {}

    g = _gen(("KB#0002",))
    v = _verify_with(_orch(), g)
    decide: Callable[..., Any] = RunQuery._decision
    d = decide(None, _Client(), "local", g, v)
    assert d.decision == "PASS"
    assert d.guardrail_report["support_gate"] == "passed"
    assert d.guardrail_report["citation_checks"] == [dict(c) for c in v.citation_checks]
    assert d.guardrail_report["citation_checks"][0]["chunk_id"] == "KB#0002"


def test_an_unverified_decision_stores_no_checks() -> None:
    class _Client:
        def model_versions(self) -> dict[str, str]:
            return {}

    g = Generation(question=QUESTION, failed_agents=("CCA",))
    decide: Callable[..., Any] = RunQuery._decision
    d = decide(None, _Client(), "local", g, Verification())
    assert d.decision == "ESCALATE"
    assert d.guardrail_report["citation_checks"] == []
