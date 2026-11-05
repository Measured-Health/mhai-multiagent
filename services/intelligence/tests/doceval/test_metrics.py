"""The metrics against fixtures whose expected numbers were worked out by hand.

Each fixture in ``fixtures/metrics`` holds the truth, the prediction, the hand working and
the expected numbers; rates are written as exact fractions (``"2/3"``) and compared
exactly, since the metrics compute rates as fractions before turning them into floats.
"""

import copy
import json
import random
from fractions import Fraction
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import metrics, schema
from intelligence.doceval.synth import hostile, layouts, patients, scanner

FIXTURES = Path(__file__).parent / "fixtures" / "metrics"


def _load(name: str) -> dict[str, Any]:
    loaded: dict[str, Any] = json.loads((FIXTURES / f"{name}.json").read_text(encoding="utf-8"))
    return loaded


def _rate(text: str | list[int] | None) -> float | None:
    """A rate written as ``"2/3"``, or as ``[numerator, denominator]``."""
    if text is None:
        return None
    if isinstance(text, list):
        return float(Fraction(text[0], text[1]))
    return float(Fraction(text))


def _expect(expected: dict[str, Any]) -> dict[str, Any]:
    """Fixture numbers with rate strings turned into the floats the metrics return."""
    return {k: _rate(v) if k in metrics.FLOAT_KEYS else v for k, v in expected.items()}


def _pred(value: Any) -> Any:
    if isinstance(value, dict) and set(value) == {"refused"}:
        return metrics.Refusal(value["refused"])
    return value


# --- field precision and recall -------------------------------------------------------


def test_field_pr_hand_computed() -> None:
    fx = _load("field_pr")
    got = metrics.field_pr(fx["truth"], fx["pred"])
    assert set(got) == set(schema.FIELD_TYPES.values())
    for ftype, expected in fx["expected"].items():
        assert got[ftype] == _expect(expected), ftype
    assert metrics.identifier_precision(fx["truth"], fx["pred"]) == _expect(
        fx["expected_identifiers"]
    )


def test_field_pr_sums_counts_over_documents() -> None:
    fx = _load("field_pr")
    one = metrics.field_pr(fx["truth"], fx["pred"])
    two = metrics.field_pr([fx["truth"], fx["truth"]], [fx["pred"], fx["pred"]])
    for ftype in one:
        for k in ("tp", "fp", "fn"):
            assert two[ftype][k] == 2 * one[ftype][k]
        assert two[ftype]["precision"] == one[ftype]["precision"]


def test_field_pr_none_prediction_is_all_missed() -> None:
    fx = _load("field_pr")
    got = metrics.field_pr(fx["truth"], None)
    # visible true values: enum 5, date 2, datetime 1, text 5, identifier 1, name 2, numeric 3, unit 3
    assert {k: (v["tp"], v["fp"], v["fn"]) for k, v in got.items() if v["fn"]} == {
        "enum": (0, 0, 5),
        "date": (0, 0, 2),
        "datetime": (0, 0, 1),
        "text": (0, 0, 5),
        "identifier": (0, 0, 1),
        "name": (0, 0, 2),
        "numeric_lab_value": (0, 0, 3),
        "unit": (0, 0, 3),
    }
    assert got["enum"]["precision"] is None
    assert got["enum"]["recall"] == 0.0


def test_identifier_precision_uses_patient_paths_only() -> None:
    fx = _load("identifiers")
    assert metrics.identifier_precision(fx["truth"], fx["pred"]) == _expect(fx["expected"])


def test_identifier_metric_ignores_clinician_name() -> None:
    fx = _load("identifiers")
    pred = copy.deepcopy(fx["pred"])
    pred["encounter"]["clinician_text"]["value"] = "Dr Arvendal Pemberlyn"
    assert metrics.identifier_precision(fx["truth"], pred) == _expect(fx["expected"])


def test_unreadable_field_never_a_false_negative() -> None:
    truth = {
        "patient": {"mrn": {"value": "SYN-1", "raw_text": "SYN-1", "page": None, "bbox": None}}
    }
    for pred in (None, {"patient": {"mrn": {"value": None}}}):
        got = metrics.field_pr(truth, pred)["identifier"]
        assert (got["tp"], got["fp"], got["fn"]) == (0, 0, 0)
        assert got["precision"] is None and got["recall"] is None
    equal = {"patient": {"mrn": {"value": "syn-1", "page": 1, "bbox": [0, 0, 1, 1]}}}
    got = metrics.field_pr(truth, equal)["identifier"]
    assert (got["tp"], got["fp"], got["fn"]) == (0, 0, 0)  # unverifiable: earns nothing
    other = {"patient": {"mrn": {"value": "SYN-2", "page": 1, "bbox": [0, 0, 1, 1]}}}
    got = metrics.field_pr(truth, other)["identifier"]
    assert (got["tp"], got["fp"], got["fn"]) == (0, 1, 0)  # an invented value is an error


def test_handwritten_pr_hand_computed() -> None:
    fx = _load("cer")
    assert metrics.handwritten_pr(fx["truth"], fx["pred"]) == _expect(fx["expected_handwritten_pr"])


def _f(value: Any, script: str | None = None, **extra: Any) -> dict[str, Any]:
    out: dict[str, Any] = {"value": value, "raw_text": None if value is None else str(value)}
    out.update(page=None if value is None else 1, bbox=None if value is None else [0, 0, 1, 1])
    if script is not None:
        out["script"] = script
    out.update(extra)
    return out


def test_handwritten_extras_take_the_truths_script_not_the_predictions() -> None:
    hand_doc = {
        "patient": {"given_names": _f("Avelor", "handwritten"), "family_name": _f(None)},
        "observations": [{"name_text": _f("Glucose", "handwritten"), "value_text": _f(None)}],
    }
    pred = {
        "patient": {
            "given_names": _f("Avelor", "printed"),
            "family_name": _f("Bellwater", "printed"),  # extra: the document is handwritten
        },
        "observations": [
            {"name_text": _f("Glucose", "printed"), "value_text": _f("5", "printed")},
            {"name_text": _f("Sodium", "printed")},  # extra row: the document decides
        ],
    }
    got = metrics.handwritten_pr(hand_doc, pred)
    assert (got["tp"], got["fp"], got["fn"]) == (2, 3, 0)
    printed_doc = {"patient": {"given_names": _f("Avelor", "printed")}}
    claims = {
        "patient": {
            "given_names": _f("Avelor", "handwritten"),
            "family_name": _f("Bellwater", "handwritten"),
        }
    }
    got = metrics.handwritten_pr(printed_doc, claims)
    assert (got["tp"], got["fp"], got["fn"]) == (0, 0, 0)


# --- normalisation ----------------------------------------------------------------------


def test_number_and_date_normalisation() -> None:
    fx = _load("normalisation")
    for case in fx["cases"]:
        got = metrics.values_equal(case["type"], case["a"], case["b"])
        assert got is case["equal"], case
        assert metrics.values_equal(case["type"], case["b"], case["a"]) is case["equal"], case


def test_number_formats_from_python() -> None:
    assert metrics.values_equal("numeric_lab_value", 5.8, 5.80)
    assert not metrics.values_equal("numeric_lab_value", float("nan"), float("nan"))
    assert not metrics.values_equal("numeric_lab_value", float("inf"), float("inf"))
    with pytest.raises(ValueError):
        metrics.normalise("not_a_type", "x")


def test_null_and_absent_are_the_same() -> None:
    truth: dict[str, Any] = {"patient": {"mrn": {"value": None, "page": None, "bbox": None}}}
    for pred in ({}, {"patient": {}}, {"patient": {"mrn": {"value": None}}}):
        assert metrics.field_pr(truth, pred)["identifier"]["fp"] == 0
    empty = {"patient": {"mrn": {"value": "  ", "page": 1, "bbox": [0, 0, 1, 1]}}}
    assert metrics.field_pr(truth, empty)["identifier"]["fp"] == 0


# --- matching ---------------------------------------------------------------------------


def test_array_matching_reordered_and_missing() -> None:
    fx = _load("matching")
    for case in fx["cases"]:
        got = metrics.match_rows(case["truth"], case["pred"], case["array"])
        assert got == case["expected"], case["name"]


def test_matching_is_deterministic_and_order_free() -> None:
    fx = _load("matching")
    case = fx["cases"][0]
    first = metrics.match_rows(case["truth"], case["pred"], case["array"])
    assert metrics.match_rows(case["truth"], case["pred"], case["array"]) == first
    rows = case["pred"]["observations"]
    flipped = {"observations": list(reversed(rows))}
    got = metrics.match_rows(case["truth"], flipped, case["array"])
    n = len(rows)
    assert sorted([i, n - 1 - j] for i, j in got["pairs"]) == first["pairs"]


def test_matching_unknown_array_refused() -> None:
    with pytest.raises(ValueError):
        metrics.match_rows({}, {}, "patient")


# --- codes ------------------------------------------------------------------------------


def test_code_exact_and_invented() -> None:
    fx = _load("codes")
    got = metrics.code_exact_match(fx["truth_codes"], fx["pred_codes"], fx["terminology"])
    assert got == _expect(fx["expected"])
    without = metrics.code_exact_match(fx["truth_codes"], None, fx["terminology"])
    assert without == _expect(fx["expected_without_predictions"])
    with pytest.raises(ValueError):
        metrics.code_exact_match(["a"], [], fx["terminology"])


def test_terminology_codes_subsets() -> None:
    assert "2345-7" in metrics.terminology_codes("loinc")
    assert metrics.terminology_codes("icd10")
    assert metrics.terminology_codes("rxnorm")
    with pytest.raises(ValueError):
        metrics.terminology_codes("snomed")


# --- silent errors and calibration ------------------------------------------------------


def test_silent_error_rate() -> None:
    fx = _load("silent")
    assert metrics.silent_error_rate(fx["truth"], fx["pred"]) == _expect(fx["expected"])


def test_confidence_pairs_hand_listed() -> None:
    fx = _load("silent")
    got = metrics.confidence_pairs(fx["truth"], fx["pred"])
    assert got == [(c, ok) for c, ok in fx["expected_confidence_pairs"]]


def test_missing_confidence_raises() -> None:
    truth = {"patient": {"given_names": _f("Avelor")}}
    for bad in ({}, {"confidence": None}, {"confidence": "high"}, {"confidence": True}):
        pred = {"patient": {"given_names": _f("Avelor", band="high", **bad)}}
        with pytest.raises(ValueError, match="bad_confidence"):
            metrics.confidence_pairs(truth, pred)


def test_ece_hand_computed() -> None:
    fx = _load("calibration")
    for case in fx["cases"]:
        got = metrics.calibration_ece(case["confidences"], case["correct"], bins=case["bins"])
        assert got == _rate(case["expected"]), case["name"]
    assert (
        metrics.calibration_ece(fx["cases"][-1]["confidences"], fx["cases"][-1]["correct"]) == 0.0
    )


def test_ece_refuses_bad_input() -> None:
    for confidences, correct in (([1.5], [True]), ([-0.1], [True]), ([0.5], [1]), ([0.5], [])):
        with pytest.raises(ValueError):
            metrics.calibration_ece(confidences, correct)  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        metrics.calibration_ece([float("nan")], [True])
    with pytest.raises(ValueError):
        metrics.calibration_ece([0.5], [True], bins=0)


# --- grounding, CER ---------------------------------------------------------------------


def test_grounding_iou_threshold() -> None:
    fx = _load("grounding")
    assert metrics.grounding_rate(fx["truth"], fx["pred"]) == _expect(fx["expected"])


def test_cer_known_strings() -> None:
    fx = _load("cer")
    for case in fx["strings"]:
        assert metrics.cer(case["truth"], case["pred"]) == _rate(case["expected"]), case
    assert metrics.handwriting_cer(fx["truth"], fx["pred"]) == {
        "edits": fx["expected"]["edits"],
        "chars": fx["expected"]["chars"],
        "cer": _rate(fx["expected"]["cer"]),
    }


def test_cer_long_text_matches_quadratic_reference() -> None:
    rng = random.Random(3)
    alphabet = "abcde "
    for _ in range(30):
        a = "".join(rng.choice(alphabet) for _ in range(rng.randrange(0, 40)))
        b = "".join(rng.choice(alphabet) for _ in range(rng.randrange(0, 40)))
        prev = list(range(len(b) + 1))
        for i, ca in enumerate(a, 1):
            cur = [i]
            for j, cb in enumerate(b, 1):
                cur.append(min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (ca != cb)))
            prev = cur
        assert metrics._levenshtein(a, b) == prev[-1]


# --- hostile ----------------------------------------------------------------------------


@pytest.mark.parametrize("case", _load("hostile")["cases"], ids=lambda c: c["name"])
def test_hostile_cases_hand_listed(case: dict[str, Any]) -> None:
    got = metrics.hostile_outcome(case["case"], _pred(case["pred"]))
    assert got == case["expected"], case["working"]
    assert set(got["failures"]) <= set(metrics.HOSTILE_FAILURES)


def test_hostile_other_field_changed_detected() -> None:
    cases = {c["name"]: c for c in _load("hostile")["cases"]}
    for name in (
        "inj_other_field_changed_value",
        "inj_other_field_changed_flag",
        "rot_read_changed",
    ):
        c = cases[name]
        got = metrics.hostile_outcome(c["case"], _pred(c["pred"]))
        assert got["flagged_or_refused"] is True
        assert got["other_fields_unchanged"] is False


def test_hostile_unknown_outcome_raises() -> None:
    c = copy.deepcopy(_load("hostile")["cases"][0]["case"])
    c["expected"]["outcome"] = "anything"
    with pytest.raises(ValueError):
        metrics.hostile_outcome(c, {})


def test_hostile_missing_page_count_fails() -> None:
    cases = {c["name"]: c for c in _load("hostile")["cases"]}
    c = copy.deepcopy(cases["inj_all_flagged"])
    del c["pred"]["document"]["page_count"]
    got = metrics.hostile_outcome(c["case"], c["pred"])
    assert got["other_fields_unchanged"] is False
    assert got["failures"] == ["page_count_mismatch"]


def test_hostile_none_prediction_raises() -> None:
    case = _load("hostile")["cases"][0]["case"]
    with pytest.raises(TypeError):
        metrics.hostile_outcome(case, None)


def _stored(gt: dict[str, Any], page_count: int, flags: dict[str, list[str]]) -> dict[str, Any]:
    doc = copy.deepcopy(gt)
    for path, fld in schema.iter_fields(doc):
        fld.update(confidence=0.9, band="medium", grounded=True, flags=list(flags.get(path, [])))
    doc["document"]["page_count"] = page_count
    return doc


@pytest.fixture(scope="module")
def real_cases() -> dict[str, hostile.HostileCase]:
    found: dict[str, Any] = {}
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            found.setdefault(d["document"]["type"]["value"], copy.deepcopy(d))
    layout = layouts.for_type("lab_report", 5, purpose="train")[0]
    return {
        kind: hostile.make(found["lab_report"], kind, random.Random(1), layout=layout, dpi=100)
        for kind in ("injection", "wrong_patient_label", "rotated", "two_patients")
    }


def test_hostile_real_injection_case(real_cases: dict[str, hostile.HostileCase]) -> None:
    case = real_cases["injection"]
    pages = len(case.hostile.pages)
    good = _stored(case.hostile.ground_truth, pages, case.expected["flags"])
    assert metrics.hostile_outcome(case, good) == {
        "flagged_or_refused": True,
        "other_fields_unchanged": True,
        "failures": [],
    }
    unflagged = _stored(case.hostile.ground_truth, pages, {})
    assert metrics.hostile_outcome(case, unflagged)["flagged_or_refused"] is False
    changed = copy.deepcopy(good)
    changed["patient"]["family_name"]["value"] = "Changed"
    got = metrics.hostile_outcome(case, changed)
    assert got["flagged_or_refused"] is True and got["other_fields_unchanged"] is False
    footer = copy.deepcopy(good)
    footer["notes"][0]["text"]["value"] += " " + case.expected["footer"]["text"]
    assert "injection_unflagged" in metrics.hostile_outcome(case, footer)["failures"]
    assert metrics.hostile_outcome(case, metrics.Refusal("internal"))["flagged_or_refused"] is False


def test_hostile_real_label_rotated_two_patients(
    real_cases: dict[str, hostile.HostileCase],
) -> None:
    wl = real_cases["wrong_patient_label"]
    pred = _stored(wl.hostile.ground_truth, len(wl.hostile.pages), wl.expected["flags"])
    pred["patient"]["mrn"]["value"] = wl.expected["conflicting_mrn"]
    assert metrics.hostile_outcome(wl, pred)["failures"] == []
    rot = real_cases["rotated"]
    read = _stored(rot.hostile.ground_truth, len(rot.hostile.pages), {})
    assert metrics.hostile_outcome(rot, read)["failures"] == []
    assert metrics.hostile_outcome(rot, metrics.Refusal("unreadable"))["failures"] == []
    two = real_cases["two_patients"]
    assert metrics.hostile_outcome(two, metrics.Refusal("multiple_patients"))["failures"] == []
    extracted = _stored(two.hostile.ground_truth, len(two.hostile.pages), {})
    assert metrics.hostile_outcome(two, extracted)["flagged_or_refused"] is False


# --- latency ----------------------------------------------------------------------------


def test_latency_nearest_rank() -> None:
    fx = _load("latency")
    for case in fx["cases"]:
        assert metrics.latency(case["seconds"]) == {"p50": case["p50"], "p95": case["p95"]}
    for case in fx["throughput"]:
        assert metrics.throughput(case["pages"], case["seconds"]) == _rate(case["expected"])
    with pytest.raises(ValueError):
        metrics.latency([1.0, -1.0])
    with pytest.raises(ValueError):
        metrics.latency([float("nan")])


# --- empty inputs and slices ------------------------------------------------------------


def test_empty_inputs_are_none_not_zero() -> None:
    empty: dict[str, Any] = {}
    for counts in metrics.field_pr(empty, empty).values():
        assert counts == {"tp": 0, "fp": 0, "fn": 0, "precision": None, "recall": None}
    assert metrics.field_pr([], [])["text"]["precision"] is None
    assert metrics.identifier_precision(empty, None)["precision"] is None
    assert metrics.handwritten_pr(empty, empty)["recall"] is None
    assert metrics.silent_error_rate(empty, empty) == {"errors": 0, "high_band": 0, "rate": None}
    assert metrics.grounding_rate(empty, empty) == {"grounded": 0, "total": 0, "rate": None}
    assert metrics.handwriting_cer(empty, empty) == {"edits": 0, "chars": 0, "cer": None}
    assert metrics.calibration_ece([], []) is None
    assert metrics.confidence_pairs(empty, empty) == []
    assert metrics.cer("", "anything") is None
    assert metrics.code_exact_match([], [], []) == {
        "matched": 0,
        "total": 0,
        "rate": None,
        "invented": 0,
    }
    assert metrics.latency([]) == {"p50": None, "p95": None}
    assert metrics.throughput(0, 10.0) is None
    assert metrics.by_slice([], lambda xs: metrics.calibration_ece([], [])) == {
        "slices": {},
        "overall": None,
    }


def test_unrendered_truth_measures_nothing() -> None:
    """Truth with no boxes (never rendered) is all not visible: no score, not a perfect one."""
    doc = patients.generate(random.Random(5), 1)[0].documents[0]
    got = metrics.field_pr(doc, copy.deepcopy(doc))
    assert all(v["precision"] is None and v["recall"] is None for v in got.values())


def test_by_slice_keeps_every_slice() -> None:
    fx = _load("field_pr")
    items = [("clean", (fx["truth"], fx["pred"])), ("poor", (fx["truth"], None))]

    def text_pr(xs: list[tuple[Any, Any]]) -> Any:
        return metrics.field_pr([t for t, _ in xs], [p for _, p in xs])["text"]

    got = metrics.by_slice(items, text_pr)
    assert list(got["slices"]) == ["clean", "poor"]
    assert got["slices"]["clean"]["precision"] == _rate("2/3")
    assert got["slices"]["poor"]["recall"] == 0.0
    assert got["overall"]["tp"] == 4 and got["overall"]["fn"] == 1 + 5


def test_mismatched_lengths_refused() -> None:
    with pytest.raises(ValueError):
        metrics.field_pr([{}], [])
    with pytest.raises(TypeError):
        metrics.field_pr([{}], None)


def test_metric_outputs_carry_no_values() -> None:
    """Reports carry aggregates only: no truth or predicted value appears in any output."""
    fx = _load("field_pr")
    outputs = [
        metrics.field_pr(fx["truth"], fx["pred"]),
        metrics.identifier_precision(fx["truth"], fx["pred"]),
        metrics.silent_error_rate(fx["truth"], fx["pred"]),
        metrics.grounding_rate(fx["truth"], fx["pred"]),
        metrics.handwriting_cer(fx["truth"], fx["pred"]),
    ]
    text = json.dumps(outputs)
    for word in ("SYN-", "Glucose", "Ashbrenner", "Eastmere", "Pemberlyn"):
        assert word not in text


def _truths(node: Any) -> list[dict[str, Any]]:
    """Every ground-truth document in a fixture (under ``truth`` or ``clean_truth``)."""
    found: list[dict[str, Any]] = []
    if isinstance(node, dict):
        for key, value in node.items():
            if key in ("truth", "clean_truth") and isinstance(value, dict):
                found.append(value)
            else:
                found.extend(_truths(value))
    elif isinstance(node, list):
        for item in node:
            found.extend(_truths(item))
    return found


def _complete(sub: dict[str, Any], node: Any) -> Any:
    """A sparse fixture truth filled out to a full model-output document (absent = null)."""
    if "$ref" in sub:
        null = {"value": None, "raw_text": None, "page": None, "bbox": None}
        return node if isinstance(node, dict) else null
    if sub.get("type") == "array":
        return [_complete(sub["items"], row) for row in (node or [])]
    if "properties" in sub:
        node = node or {}
        return {
            k: schema.SCHEMA_VERSION if k == "schema_version" else _complete(child, node.get(k))
            for k, child in sub["properties"].items()
        }
    return node


def test_fixture_truths_pass_the_synthetic_data_scanner() -> None:
    """The truths hold only synthetic identifiers, names and facilities.

    Predictions are deliberate misreadings of these values, and the working notes and
    expected codes are prose, so only the truths are checked, as documents.
    """
    n = 0
    for path in sorted(FIXTURES.glob("*.json")):
        for truth in _truths(_load(path.stem)):
            doc = _complete(schema.MODEL_OUTPUT, truth)
            assert scanner.check_document(doc) == [], path.name
            n += 1
    assert n > 40


def test_scores_from_one_alignment_equal_each_metric() -> None:
    from intelligence.doceval import extractors
    from intelligence.doceval import sets as _sets
    from tests.doceval import minisets

    truths = [_sets.stored_view(minisets.visible_truth(k), 1) for k in range(4)]
    preds: list[Any] = [copy.deepcopy(t) for t in truths]
    rng = random.Random(3)
    for p in preds[:3]:
        for _, fld in schema.iter_fields(p):
            r = rng.random()
            if fld.get("value") is not None and r < 0.2:
                fld["value"] = None
            elif isinstance(fld.get("value"), str) and r < 0.4:
                fld["value"] = "Q" + fld["value"]
            fld["confidence"] = rng.choice([0.5, 0.9, 1.0])
            fld["band"] = extractors.band(fld["confidence"])
    preds[3] = None
    sc = metrics.scores(truths, preds)
    assert sc["field_pr"] == metrics.field_pr(truths, preds)
    assert sc["handwritten_pr"] == metrics.handwritten_pr(truths, preds)
    assert sc["identifier_precision"] == metrics.identifier_precision(truths, preds)
    assert sc["silent_error"] == metrics.silent_error_rate(truths, preds)
    assert sc["confidence_pairs"] == metrics.confidence_pairs(truths, preds)
    assert sc["grounding"] == metrics.grounding_rate(truths, preds)
    assert sc["handwriting_cer"] == metrics.handwriting_cer(truths, preds)
    # joining per-document alignments gives the same as aligning the group
    joined = metrics.Aligned(
        tuple(d for t, p in zip(truths, preds, strict=True) for d in metrics.align(t, p).docs)
    )
    assert metrics.scores_of(joined) == sc
