"""Issue #3: corrections need a second clinician before they change anything."""
import numpy as np
import pytest

from mhai.agents.hga_nli import NLIBackedHGA
from mhai.agents.orchestrator import (
    DoctorFeedbackLoop,
    DynamicOrchestratorAgent,
    GuardrailThresholdManager,
    PersonaVectorUpdater,
    TrustWeightOptimizer,
)
from mhai.knowledge_base.kb_builder import KBChunk, RAGClinicalContextAgent
from mhai.security.hipaa import AuditLogger, RBACManager, SecurePipelineWrapper, UserRole

from .conftest import PATIENT_A

QUERY = "What is the correct dosage of metformin for this patient?"
CORRECTION = "Use 500 mg twice daily. Call 555-123-4567, SSN 123-45-6789."


@pytest.fixture
def system(two_patient_kb, retriever, tmp_path):
    pv = PersonaVectorUpdater(d_model=8)
    tw = TrustWeightOptimizer(buffer_size=1)  # optimise on the first correction
    tm = GuardrailThresholdManager()
    loop = DoctorFeedbackLoop(pv, tw, tm, kb=two_patient_kb)
    orch = DynamicOrchestratorAgent(
        rag_cca=RAGClinicalContextAgent(retriever=retriever),
        hga=NLIBackedHGA(), threshold_manager=tm, feedback_loop=loop,
    )
    rbac = RBACManager()
    audit = AuditLogger(log_dir=str(tmp_path / "audit"))
    wrapper = SecurePipelineWrapper(orch, rbac=rbac, audit=audit)
    return {"kb": two_patient_kb, "loop": loop, "orch": orch, "pv": pv,
            "tw": tw, "rbac": rbac, "audit": audit, "wrapper": wrapper}


def _session(system, user, role=UserRole.CLINICIAN):
    return system["rbac"].create_session(user, role).session_id


def _corrections(kb):
    return [c for c in kb.chunks.values() if c.chunk_type == "verified_correction"]


def _submit(system, user="dr_a"):
    w = system["wrapper"]
    sid = _session(system, user)
    q = w.submit_query(sid, QUERY, patient_id=PATIENT_A)
    fb = w.submit_feedback(sid, q["query_id"], "correct", CORRECTION)
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

    result = system["wrapper"].approve_correction(
        _session(system, "dr_a"), fb["correction_id"]
    )

    assert "different clinician" in result["error"]
    assert _corrections(system["kb"]) == []


def test_non_clinician_cannot_submit_or_approve(system):
    q, fb = _submit(system)
    w = system["wrapper"]

    researcher = _session(system, "res_1", UserRole.RESEARCHER)
    assert w.submit_feedback(researcher, q["query_id"], "correct", "x")["allowed"] is False
    assert w.approve_correction(researcher, fb["correction_id"])["allowed"] is False
    assert w.submit_feedback("forged-session", q["query_id"], "correct", "x")["allowed"] is False


def test_second_clinician_approval_applies_scoped_masked_correction(system):
    q, fb = _submit(system, "dr_a")

    result = system["wrapper"].approve_correction(
        _session(system, "dr_b"), fb["correction_id"]
    )

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

    events = [(e["event_type"], e["user_id"]) for e in system["audit"].entries]
    assert ("FEEDBACK", "dr_a") in events
    assert ("CORRECTION_APPROVED", "dr_b") in events
    assert system["audit"].verify_chain()[0]


def test_correction_cannot_be_approved_twice(system):
    _, fb = _submit(system, "dr_a")
    w = system["wrapper"]
    w.approve_correction(_session(system, "dr_b"), fb["correction_id"])

    again = w.approve_correction(_session(system, "dr_c"), fb["correction_id"])

    assert "already approved" in again["error"]
    assert len(_corrections(system["kb"])) == 1


def test_loop_rejects_anonymous_or_unknown_approval(system):
    loop = system["loop"]
    anon = loop.process_feedback("q1", QUERY, "resp", "correct", corrected_response="fix")
    named = loop.process_feedback("q2", QUERY, "resp", "correct",
                                  corrected_response="fix", submitted_by="dr_a")

    assert "Anonymous" in loop.approve_correction(anon["correction_id"], "dr_b")["error"]
    assert "reviewer" in loop.approve_correction(named["correction_id"], "")["error"]
    assert "not found" in loop.approve_correction("corr_nope", "dr_b")["error"]


@pytest.mark.parametrize("submitter,reviewer", [
    ("dr_a", "DR_A"),          # same clinician, different case
    ("dr_a", " dr_a "),        # same clinician, stray whitespace
    ("unknown", "unknown"),    # placeholder ids are nobody
    ("dr_a", "unknown"),
    ("dr_a", "system"),
])
def test_two_person_rule_cannot_be_sidestepped(system, submitter, reviewer):
    loop = system["loop"]
    fb = loop.process_feedback("q1", QUERY, "resp", "correct",
                               corrected_response="fix", submitted_by=submitter)

    assert "error" in loop.approve_correction(fb["correction_id"], reviewer)
    assert _corrections(system["kb"]) == []


def test_session_without_a_real_user_cannot_give_feedback(system):
    w = system["wrapper"]
    q = w.submit_query(_session(system, "dr_a"), QUERY, patient_id=PATIENT_A)
    ghost = _session(system, "unknown")

    assert w.submit_feedback(ghost, q["query_id"], "correct", "x")["allowed"] is False


def test_persona_vector_moves_only_on_approval(system):
    loop = system["loop"]
    fb = loop.process_feedback(
        "q1", QUERY, "resp", "correct", corrected_response="fix",
        hallucinated_activation=np.ones(8), corrected_activation=np.zeros(8),
        submitted_by="dr_a",
    )
    assert system["pv"].update_count == 0

    loop.approve_correction(fb["correction_id"], "dr_b")

    assert system["pv"].update_count == 1


def test_unknown_feedback_action_is_rejected(system):
    orch = system["orch"]
    q = orch.process_query(QUERY, patient_id=PATIENT_A)

    assert "Unknown feedback action" in orch.process_feedback(q["query_id"], "delete")["error"]
