"""Fixtures for the security regression tests (issues #1-#3).

Retrieval runs on the TF-IDF fallback so these tests need no model
downloads; the end-to-end run (scripts/e2e_models.py) covers the real models.
"""

import json

import pytest

from mhai.knowledge_base.kb_builder import (
    DenseRetriever,
    HybridRetriever,
    MedicalKnowledgeBase,
)

PATIENT_A = "PAT-A"
PATIENT_B = "PAT-B"


def _patient_file(patient_id, subject_id, diagnosis, icd10, lab, medication):
    return {
        "patient_id": patient_id,
        "subjects": [
            {
                "subject_id": subject_id,
                "demographics": {"age_years": 34, "sex": "female"},
                "diagnosis": [{"name": diagnosis, "icd10": icd10}],
                "lab_results": [lab],
                "medications": [medication],
                "plan": f"Follow up on {diagnosis.lower()} in two weeks.",
            }
        ],
    }


@pytest.fixture(autouse=True)
def offline_dense_retriever(monkeypatch):
    """Use the TF-IDF fallback instead of downloading MiniLM."""

    def no_model(self):
        self._loaded = False

    monkeypatch.setattr(DenseRetriever, "_try_load_model", no_model)


@pytest.fixture
def two_patient_kb(tmp_path):
    """KB with a malaria patient (A), a diabetes patient (B) and guidelines."""
    files = {
        "a.json": _patient_file(
            PATIENT_A,
            "A-1",
            "Plasmodium falciparum malaria",
            "B50.9",
            {"test": "Malaria smear", "value": "positive", "unit": ""},
            {
                "name": "Artemether-lumefantrine",
                "dose": "80/480 mg",
                "route": "oral",
                "indication": "malaria",
            },
        ),
        "b.json": _patient_file(
            PATIENT_B,
            "B-1",
            "Type 2 diabetes mellitus",
            "E11.9",
            {"test": "HbA1c glucose", "value": "9.8", "unit": "%"},
            {
                "name": "Metformin",
                "dose": "1000 mg",
                "route": "oral",
                "indication": "diabetes glucose control",
            },
        ),
    }
    for name, data in files.items():
        (tmp_path / name).write_text(json.dumps(data))

    kb = MedicalKnowledgeBase()
    kb.build_from_patient_files(str(tmp_path))
    return kb


@pytest.fixture
def retriever(two_patient_kb):
    r = HybridRetriever(device="cpu")
    r.index_kb(two_patient_kb)
    return r
