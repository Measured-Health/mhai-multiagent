"""Issue #3: corrections need a second clinician before they change anything.

Who may submit or approve feedback is core's decision, and the audit trail is
core's chain (the request token carries the verified user). What stays here
is the two-person rule itself, in DoctorFeedbackLoop and the orchestrator,
and the check that nothing in the boundary can reach it without core.
"""

import json
import re
from importlib import resources
from pathlib import Path

import numpy as np
import pytest

from intelligence.agents.hga_nli import NLIBackedHGA
from intelligence.agents.orchestrator import (
    DoctorFeedbackLoop,
    DynamicOrchestratorAgent,
    GuardrailThresholdManager,
    PersonaVectorUpdater,
    TrustWeightOptimizer,
)
from intelligence.knowledge_base.kb_builder import KBChunk, RAGClinicalContextAgent

from .conftest import PATIENT_A

QUERY = "What is the correct dosage of metformin for this patient?"
CORRECTION = "Use 500 mg twice daily. Call 555-123-4567, SSN 123-45-6789."


@pytest.fixture
def system(two_patient_kb, retriever):
    pv = PersonaVectorUpdater(d_model=8)
    tw = TrustWeightOptimizer(buffer_size=1)  # optimise on the first correction
    tm = GuardrailThresholdManager()
    loop = DoctorFeedbackLoop(pv, tw, tm, kb=two_patient_kb)
    orch = DynamicOrchestratorAgent(
        rag_cca=RAGClinicalContextAgent(retriever=retriever),
        hga=NLIBackedHGA(),
        threshold_manager=tm,
        feedback_loop=loop,
    )
    return {"kb": two_patient_kb, "loop": loop, "orch": orch, "pv": pv, "tw": tw}


def _corrections(kb):
    return [c for c in kb.chunks.values() if c.chunk_type == "verified_correction"]


def _submit(system, user="dr_a"):
    """What the worker does with core's verified user id: ask, then give
    feedback as that user."""
    orch = system["orch"]
    q = orch.process_query(QUERY, patient_id=PATIENT_A)
    fb = orch.process_feedback(q["query_id"], "correct", CORRECTION, submitted_by=user)
    return q, fb


def test_correction_changes_nothing_until_approved(system):
    weights_before = dict(vars(system["orch"].threshold_manager.base_thresholds))

    _, fb = _submit(system)

    assert fb["status"] == "pending_review"
    assert _corrections(system["kb"]) == []
    assert system["pv"].update_count == 0
    assert system["tw"].correction_buffer == []
    assert dict(vars(system["orch"].threshold_manager.base_thresholds)) == weights_before
    assert system["loop"].get_stats()["corrections_awaiting_review"] == 1


def test_submitter_cannot_approve_own_correction(system):
    _, fb = _submit(system, "dr_a")

    result = system["orch"].approve_correction(fb["correction_id"], "dr_a")

    assert "different clinician" in result["error"]
    assert _corrections(system["kb"]) == []


def test_feedback_has_no_boundary_operation():
    # Replaces "a non-clinician cannot submit or approve": the worker serves
    # only the operations core's table names, and none is feedback or
    # approval. Adding one needs a role in operations.json, which is core's
    # table to gate (Python holds no permission rule).
    ops = json.loads((resources.files("intelligence.boundary") / "operations.json").read_text())
    for op in ops:
        assert not re.search(r"feedback|correction|approve", op["name"]), op["name"]


def test_boundary_code_never_reaches_the_feedback_loop():
    # And nothing in the boundary package calls it behind the table's back.
    boundary = Path(str(resources.files("intelligence.boundary")))
    for path in boundary.rglob("*.py"):
        text = path.read_text()
        for name in ("process_feedback", "approve_correction", "DoctorFeedbackLoop"):
            assert name not in text, f"{path.name} reaches {name}"


def test_second_clinician_approval_applies_scoped_masked_correction(system):
    q, fb = _submit(system, "dr_a")

    result = system["orch"].approve_correction(fb["correction_id"], "dr_b")

    assert result["status"] == "approved"
    assert result["updates"]["kb_enriched"] is True
    assert "cma_es" in result["updates"]
    [chunk] = _corrections(system["kb"])
    assert isinstance(chunk, KBChunk)
    assert chunk.source_patient_id == PATIENT_A
    assert chunk.metadata["submitted_by"] == "dr_a"
    assert chunk.metadata["approved_by"] == "dr_b"
    assert "123-45-6789" not in chunk.content
    assert "555-123-4567" not in chunk.content
    # The KB can still be re-indexed after a correction lands.
    assert (chunk.chunk_id, chunk.content) in system["kb"].get_all_texts()
    # Submitter and approver are on the record the loop keeps (core's audit
    # chain records the same two ids from the verified token).
    [pending] = system["loop"].pending_corrections.values()
    assert (pending.entry.submitted_by, pending.approved_by) == ("dr_a", "dr_b")


def test_correction_cannot_be_approved_twice(system):
    _, fb = _submit(system, "dr_a")
    orch = system["orch"]
    orch.approve_correction(fb["correction_id"], "dr_b")

    again = orch.approve_correction(fb["correction_id"], "dr_c")

    assert "already approved" in again["error"]
    assert len(_corrections(system["kb"])) == 1


def test_loop_rejects_anonymous_or_unknown_approval(system):
    loop = system["loop"]
    anon = loop.process_feedback("q1", QUERY, "resp", "correct", corrected_response="fix")
    named = loop.process_feedback(
        "q2", QUERY, "resp", "correct", corrected_response="fix", submitted_by="dr_a"
    )

    assert "Anonymous" in loop.approve_correction(anon["correction_id"], "dr_b")["error"]
    assert "reviewer" in loop.approve_correction(named["correction_id"], "")["error"]
    assert "not found" in loop.approve_correction("corr_nope", "dr_b")["error"]


@pytest.mark.parametrize(
    "submitter,reviewer",
    [
        ("dr_a", "DR_A"),  # same clinician, different case
        ("dr_a", " dr_a "),  # same clinician, stray whitespace
        ("unknown", "unknown"),  # placeholder ids are nobody
        ("dr_a", "unknown"),
        ("dr_a", "system"),
    ],
)
def test_two_person_rule_cannot_be_sidestepped(system, submitter, reviewer):
    loop = system["loop"]
    fb = loop.process_feedback(
        "q1", QUERY, "resp", "correct", corrected_response="fix", submitted_by=submitter
    )

    assert "error" in loop.approve_correction(fb["correction_id"], reviewer)
    assert _corrections(system["kb"]) == []


@pytest.mark.parametrize("ghost", ["", "unknown", "  ", "system"])
def test_feedback_without_a_real_user_can_never_be_approved(system, ghost):
    # Replaces "a session with no real user cannot give feedback": the
    # placeholder submitter is nobody, so no reviewer can sign it off.
    orch = system["orch"]
    q = orch.process_query(QUERY, patient_id=PATIENT_A)
    fb = orch.process_feedback(q["query_id"], "correct", "x", submitted_by=ghost)

    assert "error" in orch.approve_correction(fb["correction_id"], "dr_b")
    assert _corrections(system["kb"]) == []


def test_persona_vector_moves_only_on_approval(system):
    loop = system["loop"]
    fb = loop.process_feedback(
        "q1",
        QUERY,
        "resp",
        "correct",
        corrected_response="fix",
        hallucinated_activation=np.ones(8),
        corrected_activation=np.zeros(8),
        submitted_by="dr_a",
    )
    assert system["pv"].update_count == 0

    loop.approve_correction(fb["correction_id"], "dr_b")

    assert system["pv"].update_count == 1


def test_unknown_feedback_action_is_rejected(system):
    orch = system["orch"]
    q = orch.process_query(QUERY, patient_id=PATIENT_A)

    assert "Unknown feedback action" in orch.process_feedback(q["query_id"], "delete")["error"]
