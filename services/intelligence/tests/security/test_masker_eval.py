"""The masker's measured bar on the synthetic Nigerian PII set (TODO.md,
the real-data gate, item 1; docs/privacy/masker-evaluation.md).

The numbers are deterministic (a fixed seed). This test keeps them from
sliding back; it doesn't sign the gate off, which is the privacy lead's.
"""

from __future__ import annotations

import random
import re
from pathlib import Path
from typing import Any

import pytest

from intelligence.security import masker_eval, ng_pii

pytestmark = pytest.mark.disable_socket

DEV = Path(__file__).resolve().parents[4] / "testdata" / "answers" / "dev" / "items.jsonl"
DOC = Path(__file__).resolve().parents[4] / "docs" / "privacy" / "masker-evaluation.md"


@pytest.fixture(scope="module")
def result() -> dict[str, Any]:
    return masker_eval.evaluate(DEV)


def test_numeric_identifiers_meet_the_bar(result: dict[str, Any]) -> None:
    for cls in ("NIN", "BVN", "PHONE", "MRN", "NUMERIC"):
        assert result["recall"][cls]["recall"] >= 0.999, cls
        assert result["egress_recall"][cls]["recall"] >= 0.999, cls


def test_name_recall_holds(result: dict[str, Any]) -> None:
    # Measured 0.9883 by the masker and 0.9909 on the egress path (the
    # document records why the masker figure sits just under 0.99).
    assert result["recall"]["NAME"]["n"] >= 3000
    assert result["recall"]["NAME"]["recall"] >= 0.985
    assert result["egress_recall"]["NAME"]["recall"] >= 0.99
    assert result["names_by"]["held_out"]["recall"] >= 0.98


def test_false_refusals_stay_low(result: dict[str, Any]) -> None:
    assert result["clean_questions"] >= 600 + 76
    assert result["false_refusal_rate"] <= 0.01
    assert result["named_question_refusal_rate"] <= 0.01
    assert result["remask_detections"] == 0


def test_document_carries_the_measured_numbers(result: dict[str, Any]) -> None:
    doc = DOC.read_text("utf-8")
    for cls in ("NAME", "NIN", "BVN", "PHONE", "MRN"):
        assert f"{result['recall'][cls]['recall']:.4f}" in doc, cls
    assert f"{result['false_refusal_rate']:.4f}" in doc


def test_evaluation_is_synthetic_and_seeded() -> None:
    a = masker_eval.number_samples(random.Random(1), 5)
    b = masker_eval.number_samples(random.Random(1), 5)
    assert a == b
    for s in masker_eval.name_samples(random.Random(2), 50):
        text = s.history or s.text
        for start, end in s.spans:
            assert re.search(r"[^\W\d_]", text[start:end])


def test_held_out_names_are_not_in_the_gazetteer() -> None:
    gaz = ng_pii.gazetteer()
    words = {
        ng_pii.fold(w) for given, family in masker_eval.GROUPS.values() for w in given + family
    }
    held_out = words - gaz.names - gaz.words
    assert len(held_out) >= 50  # the context rules are measured, not just the list


def test_calibration_runs_on_the_files_only() -> None:
    rows = masker_eval.calibrate(folds=2, thresholds=(ng_pii.NAME_SCORE_MIN,))
    assert rows[0]["threshold"] == ng_pii.NAME_SCORE_MIN
    assert 0.5 < rows[0]["name_recall"] <= 1 and rows[0]["known_false_positive"] < 0.05
