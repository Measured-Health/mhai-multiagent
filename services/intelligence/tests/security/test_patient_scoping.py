"""Issue #1: retrieval and citations must stay within one patient."""

from intelligence.agents.hga_nli import (
    CitationVerificationPipeline,
    NLILabel,
    NLIResult,
    VerificationAction,
)
from intelligence.knowledge_base.kb_builder import KBChunk, RAGClinicalContextAgent

from .conftest import PATIENT_A, PATIENT_B

DIABETES_QUERY = "type 2 diabetes HbA1c glucose metformin dose"


def _sources(results):
    return {r.chunk.source_patient_id for r in results}


def test_query_about_a_never_returns_b_records(retriever):
    # The query matches patient B's records far better than A's.
    results = retriever.retrieve(DIABETES_QUERY, top_k=10, patient_id=PATIENT_A)

    assert results
    assert _sources(results) <= {PATIENT_A, "reference"}


def test_patient_b_still_sees_own_records(retriever):
    results = retriever.retrieve(DIABETES_QUERY, top_k=5, patient_id=PATIENT_B)

    assert PATIENT_B in _sources(results)
    assert _sources(results) <= {PATIENT_B, "reference"}


def test_no_patient_means_guidelines_only(retriever):
    results = retriever.retrieve(DIABETES_QUERY, top_k=10)

    assert results
    assert _sources(results) == {"reference"}


def test_scoping_fills_top_k_when_other_patients_dominate(two_patient_kb, retriever):
    # Bury patient A under many strongly matching chunks from other patients.
    for i in range(60):
        two_patient_kb._add_chunk(
            f"Diagnosis: Type 2 diabetes mellitus, HbA1c glucose {i}.",
            "diagnosis",
            f"PAT-X{i}",
            f"X{i}",
        )
    retriever.index_kb(two_patient_kb)

    results = retriever.retrieve(
        DIABETES_QUERY,
        top_k=3,
        chunk_types=["diagnosis", "lab", "medication"],
        patient_id=PATIENT_A,
    )

    assert len(results) == 3
    assert _sources(results) == {PATIENT_A}


def test_rag_cca_cites_only_the_patients_records(two_patient_kb, retriever):
    cca = RAGClinicalContextAgent(retriever=retriever)

    out = cca.generate_with_rag(DIABETES_QUERY, top_k=8, patient_id=PATIENT_A)

    cited = {two_patient_kb.get_chunk(f"KB#{n}").source_patient_id for n in out["citations"]}
    shown = {two_patient_kb.get_chunk(c["id"]).source_patient_id for c in out["retrieved_chunks"]}
    assert cited <= {PATIENT_A, "reference"}
    assert shown <= {PATIENT_A, "reference"}


class AlwaysEntails:
    """NLI stub: every claim is entailed by its source."""

    def verify(self, premise, hypothesis):
        return NLIResult(
            premise=premise, hypothesis=hypothesis, label=NLILabel.ENTAILMENT, confidence=0.99
        )


def test_citing_another_patients_record_is_redacted(two_patient_kb):
    b_chunk = next(c for c in two_patient_kb.chunks.values() if c.source_patient_id == PATIENT_B)
    pipeline = CitationVerificationPipeline(AlwaysEntails(), two_patient_kb)

    report = pipeline.verify_response(
        f"Patient has diabetes [{b_chunk.chunk_id}].", patient_id=PATIENT_A
    )

    actions = [r.action for r in report.citation_results]
    assert actions == [VerificationAction.REDACT]
    assert report.citation_results[0].source_text == ""


def test_citing_own_record_and_guidelines_still_passes(two_patient_kb):
    own = next(c for c in two_patient_kb.chunks.values() if c.source_patient_id == PATIENT_A)
    guideline = next(
        c for c in two_patient_kb.chunks.values() if c.source_patient_id == "reference"
    )
    pipeline = CitationVerificationPipeline(AlwaysEntails(), two_patient_kb)

    report = pipeline.verify_response(
        f"Smear positive [{own.chunk_id}]. Treat per guideline [{guideline.chunk_id}].",
        patient_id=PATIENT_A,
    )

    assert all(r.action == VerificationAction.PASS for r in report.citation_results)


def test_kb_chunk_type_is_what_feedback_writes():
    # Guards the import the feedback loop relies on (issue #3).
    chunk = KBChunk("KB#9999", "x", "verified_correction", PATIENT_A, "q")
    assert chunk.source_patient_id == PATIENT_A


def _write(tmp_path, name, patient_id, diagnosis):
    import json

    data = {"subjects": [{"subject_id": "S1", "diagnosis": [{"name": diagnosis}]}]}
    if patient_id is not None:
        data["patient_id"] = patient_id
    (tmp_path / name).write_text(json.dumps(data))


def test_reserved_or_missing_patient_ids_are_not_ingested(tmp_path):
    from intelligence.knowledge_base.kb_builder import MedicalKnowledgeBase

    _write(tmp_path, "evil.json", "reference", "HIV positive")
    _write(tmp_path, "case.json", "Reference ", "Syphilis")
    _write(tmp_path, "none1.json", None, "Hepatitis C")
    _write(tmp_path, "none2.json", None, "Tuberculosis")
    _write(tmp_path, "blank.json", "", "Gonorrhoea")
    _write(tmp_path, "ok.json", "PAT-OK", "Asthma")
    kb = MedicalKnowledgeBase()
    kb.build_from_patient_files(str(tmp_path))

    dx = {c.content: c.source_patient_id for c in kb.chunks.values() if c.chunk_type == "diagnosis"}
    assert dx == {"Diagnosis: Asthma.": "PAT-OK"}


def test_reserved_patient_ids_see_guidelines_only(retriever, two_patient_kb):
    two_patient_kb._add_chunk(
        "Diagnosis: orphan correction.", "verified_correction", "unassigned", "q"
    )
    retriever.index_kb(two_patient_kb)

    for pid in ("reference", "unassigned", "unknown", "", " Reference"):
        results = retriever.retrieve(
            "diagnosis correction diabetes malaria", top_k=10, patient_id=pid
        )
        assert {r.chunk.chunk_type for r in results} <= {"guideline"}, pid


def test_non_guideline_chunk_tagged_reference_is_not_shared(retriever, two_patient_kb):
    two_patient_kb._add_chunk("Diagnosis: smuggled HIV record.", "diagnosis", "reference", "S1")
    retriever.index_kb(two_patient_kb)

    results = retriever.retrieve("smuggled HIV record", top_k=10, patient_id=PATIENT_A)

    assert "smuggled" not in " ".join(r.chunk.content for r in results)


def test_wrapper_rejects_reserved_patient_id(tmp_path, retriever):
    from intelligence.agents.hga_nli import NLIBackedHGA
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.security.hipaa import (
        AuditLogger,
        RBACManager,
        SecurePipelineWrapper,
        UserRole,
    )

    rbac = RBACManager()
    w = SecurePipelineWrapper(
        DynamicOrchestratorAgent(
            rag_cca=RAGClinicalContextAgent(retriever=retriever), hga=NLIBackedHGA()
        ),
        rbac=rbac,
        audit=AuditLogger(log_dir=str(tmp_path)),
    )
    sid = rbac.create_session("dr_a", UserRole.CLINICIAN).session_id

    assert w.submit_query(sid, "any", patient_id="reference")["allowed"] is False
    assert w.submit_query(sid, "any", patient_id="unassigned")["allowed"] is False
