"""Issue #2: HGA thresholds are per call, and agent failures escalate."""

import threading
import time

import pytest

from intelligence.agents.hga_nli import NLIBackedHGA
from intelligence.agents.orchestrator import DynamicOrchestratorAgent

MEDICATION_QUERY = "What is the correct dosage of metformin for this patient?"
LIFESTYLE_QUERY = "What diet and exercise changes would help with weight loss?"
EMERGENCY_QUERY = "Patient presenting with acute chest pain, stat ECG needed"


class StubCCA:
    def generate_with_rag(self, query, top_k=5, triage_context=None, patient_id=None):
        return {"response": f"Assessment for: {query}", "citations": [], "n_citations": 0}


class FailingCCA:
    def generate_with_rag(self, *args, **kwargs):
        raise RuntimeError("model OOM")


class FailingHGA(NLIBackedHGA):
    def verify(self, *args, **kwargs):
        raise RuntimeError("NLI model unavailable")


class RecordingHGA(NLIBackedHGA):
    """Records the thresholds each call received as arguments.

    Reading the instance attributes instead would let the old
    mutate-then-restore code pass, so only explicit kwargs count.
    """

    def __init__(self, delay=0.0):
        super().__init__()
        self.delay = delay
        self.seen = {}

    def verify(self, response_text, **kwargs):
        time.sleep(self.delay)  # widen any window for cross-talk
        self.seen.setdefault(response_text, []).append(
            {
                "pass": kwargs.get("pass_threshold"),
                "escalate": kwargs.get("escalate_threshold"),
                "instance_pass": self.pass_threshold,
            }
        )
        return super().verify(response_text, **kwargs)


class OddDecisionHGA(NLIBackedHGA):
    def verify(self, *args, **kwargs):
        return {"trust_score": 0.9, "decision": "review"}


def _expected(orch, query):
    t = orch.threshold_manager.get_thresholds(orch.classifier.classify(query))
    return {"pass": t.trust_pass, "escalate": t.trust_escalate}


def _assert_escalated(result, failed):
    assert result["decision"] == "escalate"
    assert result["badge"] == "RED"
    assert result["degraded"] is True
    assert result["verified"] is False
    assert result["failed_agents"] == failed
    assert result["trust_score"] == 0.0
    assert result["expert_scores"] == {}


def test_hga_failure_escalates_instead_of_inventing_a_score():
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=FailingHGA())

    result = orch.process_query(MEDICATION_QUERY)

    _assert_escalated(result, ["HGA"])


def test_hga_failure_leaves_shared_thresholds_untouched():
    hga = FailingHGA()
    before = (hga.pass_threshold, hga.escalate_threshold, hga.critical_penalty, hga.warning_penalty)
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=hga)

    orch.process_query(LIFESTYLE_QUERY)  # most lenient category
    orch.process_query(EMERGENCY_QUERY)  # strictest category

    assert (
        hga.pass_threshold,
        hga.escalate_threshold,
        hga.critical_penalty,
        hga.warning_penalty,
    ) == before


def test_thresholds_are_passed_per_call():
    hga = RecordingHGA()
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=hga)

    orch.process_query(MEDICATION_QUERY)

    [seen] = hga.seen[f"Assessment for: {MEDICATION_QUERY}"]
    expected = _expected(orch, MEDICATION_QUERY)
    assert expected["pass"] != NLIBackedHGA().pass_threshold
    assert {"pass": seen["pass"], "escalate": seen["escalate"]} == expected
    # The shared instance was never retuned for this query.
    assert seen["instance_pass"] == NLIBackedHGA().pass_threshold


def test_concurrent_queries_keep_their_own_thresholds():
    hga = RecordingHGA(delay=0.02)
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=hga)
    queries = [EMERGENCY_QUERY, LIFESTYLE_QUERY] * 10

    threads = [threading.Thread(target=orch.process_query, args=(q,)) for q in queries]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert _expected(orch, EMERGENCY_QUERY) != _expected(orch, LIFESTYLE_QUERY)
    for q in (EMERGENCY_QUERY, LIFESTYLE_QUERY):
        calls = hga.seen[f"Assessment for: {q}"]
        assert len(calls) == 10
        for c in calls:
            assert {"pass": c["pass"], "escalate": c["escalate"]} == _expected(orch, q)


def test_cca_failure_escalates_without_placeholder_text():
    orch = DynamicOrchestratorAgent(rag_cca=FailingCCA(), hga=RecordingHGA())

    result = orch.process_query(MEDICATION_QUERY)

    _assert_escalated(result, ["CCA"])
    assert result["response"] == ""
    assert result["response_available"] is False


@pytest.mark.parametrize(
    "cca,hga,failed",
    [
        (None, None, ["CCA"]),
        (StubCCA(), None, ["HGA"]),
    ],
)
def test_missing_agents_escalate(cca, hga, failed):
    orch = DynamicOrchestratorAgent(rag_cca=cca, hga=hga)

    _assert_escalated(orch.process_query(MEDICATION_QUERY), failed)


def test_unknown_hga_decision_escalates():
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=OddDecisionHGA())

    result = orch.process_query(MEDICATION_QUERY)

    assert result["decision"] == "escalate"
    assert result["badge"] == "RED"


def test_healthy_pipeline_is_not_degraded():
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=NLIBackedHGA())

    result = orch.process_query(LIFESTYLE_QUERY)

    assert result["degraded"] is False
    assert result["verified"] is True
    assert result["decision"] in ("pass", "regenerate", "escalate")
    assert set(result["expert_scores"]) == {"nli", "coherence", "numerical", "se"}


def test_query_ids_are_random():
    orch = DynamicOrchestratorAgent(rag_cca=StubCCA(), hga=NLIBackedHGA())

    ids = [orch.process_query(LIFESTYLE_QUERY)["query_id"] for _ in range(3)]

    assert len(set(ids)) == 3
    assert all(len(i) >= 32 for i in ids)
    assert "q_00001" not in ids
