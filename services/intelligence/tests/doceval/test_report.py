"""The evaluation report, the thresholds per gate and slice, and the comparison (P19)."""

import copy
import dataclasses
import hashlib
import json
import random
from collections.abc import Iterator
from fractions import Fraction
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import extractors, report, runner, sets
from intelligence.doceval.synth import hostile
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused
from tests.doceval import minisets

MARKER = "Zqxmarkerwv"
REPO = Path(__file__).resolve().parents[4]
TINY = extractors.ExtractorInfo("mhai-tiny-extractor", {"extract": "0.1.0"}, "numpy", "cpu")


def _docs(n: int, slice_name: str, seed: int, text: str | None = None) -> list[minisets.MiniDoc]:
    return [
        minisets.MiniDoc(slice_name, minisets.visible_truth(seed + k, text=text)) for k in range(n)
    ]


@pytest.fixture(scope="module")
def mini(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    """Two typed lab slices of five documents, a handwritten one of two, one hostile case.

    Every text-like field carries MARKER, so a value leaking into the report is visible.
    """
    docs = _docs(5, "lab_typed_clean", 0, MARKER) + _docs(5, "lab_typed_poor", 20, MARKER)
    docs += _docs(2, "notes_handwritten", 40, MARKER)
    docs.append(
        minisets.MiniDoc(
            "hostile",
            minisets.visible_truth(60, text=MARKER),
            hostile_expected=minisets.REFUSE_TWO_PATIENTS,
        )
    )
    return minisets.build(tmp_path_factory.mktemp("mini") / "set", docs)


def _oracle(s: minisets.MiniSet, **kw: Any) -> extractors.OracleExtractor:
    return extractors.OracleExtractor(s.root / "truth", random.Random(7), **kw)


@pytest.fixture(scope="module")
def perfect(mini: minisets.MiniSet) -> runner.RunResult:
    return dataclasses.replace(runner.run(_oracle(mini), mini.root), extractor=TINY)


@pytest.fixture(scope="module")
def noisy(mini: minisets.MiniSet) -> runner.RunResult:
    result = runner.run(_oracle(mini, drop=0.3, corrupt=0.2), mini.root)
    return dataclasses.replace(result, extractor=TINY)


def _gates() -> list[report.Thresholds]:
    return [report.packaged_thresholds(g) for g in report.DOCUMENT_GATES]


def _thresholds(tmp_path: Path, gate: str, scopes: dict[str, Any], name: str = "t.json") -> Path:
    obj = {
        "schema": report.THRESHOLDS_SCHEMA,
        "gate": gate,
        "production": gate in report.PRODUCTION_GATES,
        "source": "test",
        "scopes": {
            k: v if isinstance(v, dict) else {"min_documents": 1, "rules": v}
            for k, v in scopes.items()
        },
    }
    path = tmp_path / name
    path.write_bytes(canonical.dumps(obj))
    return path


def _strings(obj: Any) -> Iterator[str]:
    if isinstance(obj, str):
        yield obj
    elif isinstance(obj, dict):
        for k, v in obj.items():
            yield k
            yield from _strings(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from _strings(v)


def _numbers(obj: Any) -> Iterator[Any]:
    if isinstance(obj, dict):
        for v in obj.values():
            yield from _numbers(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from _numbers(v)
    elif not isinstance(obj, str | bool) and obj is not None:
        yield obj


# ---------------------------------------------------------------- the JSON


def test_report_round_trip_canonical(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates())
    raw = report.dumps(rep)
    assert canonical.is_canonical(raw)
    assert all(isinstance(n, int) for n in _numbers(rep))  # no floats anywhere
    parsed = report.parse_report(raw)
    assert parsed == rep
    assert report.dumps(parsed) == raw
    assert rep["schema"] == report.REPORT_SCHEMA
    assert rep["testset"]["sha256"] == perfect.testset_sha256
    assert rep["testset"]["name"] == sets.SET_NAME
    assert rep["model_versions"] == {"extract": "0.1.0"}
    assert rep["engine"] == {"name": "numpy", "version": None}
    assert rep["hardware"] == "cpu"
    assert set(rep["verdicts"]) == set(report.DOCUMENT_GATES)
    # a report that is not canonical, or names something odd, is refused
    with pytest.raises(Refused):
        report.parse_report(json.dumps(rep, indent=1).encode())
    bad = copy.deepcopy(rep)
    bad["scopes"]["slices.x|y"] = bad["scopes"]["overall"]
    with pytest.raises(Refused):
        report.parse_report(canonical.dumps(bad))


def test_report_has_no_per_document_data(
    mini: minisets.MiniSet, perfect: runner.RunResult, noisy: runner.RunResult
) -> None:
    ids = set(mini.ids) | set(mini.twin_ids)
    for result in (perfect, noisy):
        rep = report.build_report(result, _gates(), notebook_metrics={"val_f1": "0.81"})
        raw = report.dumps(rep).decode()
        md = report.render_markdown(rep)
        for text in (raw, md):
            assert MARKER.casefold() not in text.casefold()
            for doc_id in ids:
                assert doc_id not in text
        for s in _strings(rep):
            assert len(s) <= 200
            assert s not in ids


def test_report_records_thresholds_file_and_hash(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates())
    for gate in report.DOCUMENT_GATES:
        path = report.THRESHOLDS_DIR / f"{gate}.json"
        assert rep["thresholds"][gate] == {
            "file": f"thresholds/{gate}.json",
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "production": gate in report.PRODUCTION_GATES,
        }


def test_small_groups_show_verdicts_only(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates())
    small = rep["scopes"]["slices.notes_handwritten"]  # two documents
    assert small["documents"] == 2
    for key in ("metrics", "counts", "outcomes", "refusals", "schema_errors", "timeouts"):
        assert small[key] == report.SUPPRESSED
    big = rep["scopes"]["slices.lab_typed_clean"]  # five documents
    assert big["outcomes"] == {"extracted": 5}
    assert big["metrics"]["field_precision.other"] == "1.000000"
    # one hostile case: no counts, no rate, kinds as suppressed; the gate verdict stays
    h = rep["hostile"]
    assert h["cases"] == report.SUPPRESSED
    assert h["passed"] == report.SUPPRESSED and h["rate"] == report.SUPPRESSED
    assert h["by_kind"] == {"two_patients": report.SUPPRESSED}
    assert rep["scopes"]["slices.hostile"]["metrics"] == report.SUPPRESSED
    assert rep["verdicts"]["phase1"]["results"]["slices.hostile"]["hostile_pass_rate"] == "fail"
    # overall and run contain the small slices: no subtraction gives their counts away
    overall = rep["scopes"]["overall"]
    assert overall["outcomes"] == {"extracted": report.SUPPRESSED}
    assert overall["counts"] == report.SUPPRESSED
    assert overall["metrics"]["field_precision.other"] == "1.000000"
    assert rep["scopes"]["run"]["outcomes"] == {"extracted": report.SUPPRESSED}


def test_shares_and_cells_hidden_together(perfect: runner.RunResult) -> None:
    by_slice = copy.deepcopy(perfect.outcomes_by_slice)
    by_slice["lab_typed_clean"] = {"extracted": 4, "refused:unreadable": 1}
    outcomes = dict(perfect.outcomes)
    outcomes["extracted"] -= 1
    outcomes["refused:unreadable"] = 1
    odd = dataclasses.replace(perfect, outcomes=outcomes, outcomes_by_slice=by_slice)
    rep = report.build_report(odd, _gates())
    lab = rep["scopes"]["slices.lab_typed_clean"]
    assert lab["refusals"] == {"unreadable": report.SUPPRESSED}
    assert lab["outcomes"] == {
        "extracted": report.SUPPRESSED,
        "refused:unreadable": report.SUPPRESSED,
    }
    run = rep["scopes"]["run"]
    assert run["outcomes"]["refused:unreadable"] == report.SUPPRESSED
    assert run["metrics"]["refusal_share"] == report.SUPPRESSED
    assert run["metrics"]["timeout_share"] == "0.000000"
    # the gate still sees the exact share
    assert rep["verdicts"]["rehearsal"]["results"]["run"]["refusal_share"] == "pass"


def test_cells_hide_one_to_four_and_complements() -> None:
    assert report.cell(0, 10) == 0
    assert report.cell(10, 10) == 10
    assert report.cell(5, 10) == 5
    for c in (1, 4, 6, 9):
        assert report.cell(c, 10) == report.SUPPRESSED
    assert report.cell(3, 4) == report.SUPPRESSED


def test_hostile_kinds_closed_set() -> None:
    assert set(report.HOSTILE_KINDS) == set(hostile.KINDS)
    assert report.TESTSET_NAME == sets.SET_NAME


# ---------------------------------------------------------------- the gates


# An independent copy of design section 6.3 (and the rehearsal bounds), written out by
# hand: every scope, every rule, (op, value, hardware), and each scope's minimum size.
_L40S = "nvidia-l40s"
_TYPED_LAB = {
    "field_precision.numeric_lab_value": ("min", "0.99", None),
    "field_recall.numeric_lab_value": ("min", "0.95", None),
    "field_precision.other": ("min", "0.97", None),
    "field_recall.other": ("min", "0.90", None),
    "code_exact_match": ("min", "0.95", None),
    "invented_codes": ("max", "0", None),
    "identifier_precision": ("min", "0.995", None),
    "silent_error_rate": ("max", "0.005", None),
    "calibration_ece": ("max", "0.05", None),
    "grounding_rate": ("min", "0.95", None),
}
_HANDWRITTEN = {
    "handwritten_precision": ("min", "0.92", None),
    "handwritten_recall": ("min", "0.85", None),
    "code_exact_match": ("min", "0.95", None),
    "invented_codes": ("max", "0", None),
    "identifier_precision": ("min", "0.98", None),
    "silent_error_rate": ("max", "0.01", None),
    "calibration_ece": ("max", "0.05", None),
    "grounding_rate": ("min", "0.90", None),
    "handwriting_cer": ("max", "0.08", None),
}
_HOSTILE = {"hostile_pass_rate": ("min", "1", None)}
_GPU = ("max", "24000000000", None)
DESIGN_TABLE: dict[str, dict[str, tuple[int, dict[str, tuple[str, str | None, str | None]]]]] = {
    "phase1": {
        "slices.lab_typed_clean": (20, _TYPED_LAB),
        "slices.lab_typed_poor": (20, _TYPED_LAB),
        "slices.hostile": (20, _HOSTILE),
        "run": (
            20,
            {
                "latency_p50_s": ("max", "8", _L40S),
                "latency_p95_s": ("max", "20", _L40S),
                "gpu_peak_bytes": _GPU,
            },
        ),
    },
    "phase3": {
        "slices.lab_typed_clean": (20, _TYPED_LAB),
        "slices.lab_typed_poor": (20, _TYPED_LAB),
        "slices.charts_handwritten": (20, _HANDWRITTEN),
        "slices.notes_handwritten": (20, _HANDWRITTEN),
        "slices.mixed": (20, _HANDWRITTEN),
        "slices.hostile": (20, _HOSTILE),
        "run": (
            20,
            {
                "latency_p50_s": ("max", "15", _L40S),
                "latency_p95_s": ("max", "40", _L40S),
                "gpu_peak_bytes": _GPU,
            },
        ),
    },
    "rehearsal": {
        "run": (
            5,
            {
                "invalid_output_share": ("max", "0", None),
                "refusal_share": ("max", "0.5", None),
                "timeout_share": ("max", "0.05", None),
                "latency_p50_s": ("not_applicable", None, None),
                "latency_p95_s": ("not_applicable", None, None),
                "gpu_peak_bytes": ("not_applicable", None, None),
            },
        ),
        "overall": (
            1,
            {
                "code_exact_match": ("not_applicable", None, None),
                "invented_codes": ("not_applicable", None, None),
            },
        ),
    },
}


def test_thresholds_files_are_the_design_table() -> None:
    for gate, expected in DESIGN_TABLE.items():
        th = report.packaged_thresholds(gate)
        assert th.production is (gate != "rehearsal")
        raw = (report.THRESHOLDS_DIR / f"{gate}.json").read_bytes()
        assert canonical.is_canonical(raw)
        got = {
            scope: (th.min_documents[scope], {r.metric: (r.op, r.value, r.hardware) for r in rules})
            for scope, rules in th.scopes.items()
        }
        want = {}
        for scope, (n, rules) in expected.items():
            exact = {
                m: (op, None if v is None else Fraction(v), hw) for m, (op, v, hw) in rules.items()
            }
            want[scope] = (n, exact)
        assert got == want, gate
        for rules in th.scopes.values():
            assert len({r.metric for r in rules}) == len(rules)


def test_thresholds_are_under_codeowners() -> None:
    owners = (REPO / ".github" / "CODEOWNERS").read_text()
    assert "/services/intelligence/intelligence/doceval/thresholds/" in owners


def test_bad_thresholds_refused(tmp_path: Path) -> None:
    good = {"run": [{"metric": "timeout_share", "op": "max", "value": "0.05"}]}
    report.load_thresholds(_thresholds(tmp_path, "rehearsal", good))
    cases = [
        {"run": [{"metric": "no_such_metric", "op": "max", "value": "1"}]},
        {"run": [{"metric": "timeout_share", "op": "max", "value": "0.5e1"}]},
        {"run": [{"metric": "timeout_share", "op": "between", "value": "1"}]},
        {"slices.lab|x": [{"metric": "grounding_rate", "op": "min", "value": "1"}]},
        {"overall": [{"metric": "hostile_pass_rate", "op": "min", "value": "1"}]},
        {
            "run": [
                {"metric": "timeout_share", "op": "max", "value": "0.05"},
                {"metric": "timeout_share", "op": "max", "value": "0.01"},
            ]
        },
    ]
    cases += [
        {"run": {"min_documents": 0, "rules": good["run"]}},
        {"run": {"min_documents": True, "rules": good["run"]}},
        {"run": {"rules": good["run"]}},
    ]
    for i, scopes in enumerate(cases):
        with pytest.raises(Refused):
            report.load_thresholds(_thresholds(tmp_path, "rehearsal", scopes, f"b{i}.json"))
    # production must match the gate
    obj = json.loads(_thresholds(tmp_path, "rehearsal", good).read_bytes())
    obj["production"] = True
    (tmp_path / "p.json").write_bytes(canonical.dumps(obj))
    with pytest.raises(Refused):
        report.load_thresholds(tmp_path / "p.json")
    # not canonical
    (tmp_path / "n.json").write_text(json.dumps(obj, indent=2))
    with pytest.raises(Refused):
        report.load_thresholds(tmp_path / "n.json")
    with pytest.raises(Refused):
        report.packaged_thresholds("../phase1")


def _with_bad_slice(result: runner.RunResult, slice_name: str) -> runner.RunResult:
    m = copy.deepcopy(result.metrics)
    pr = m["slices"][slice_name]["field_pr"]
    pr["text"] = {"tp": 1, "fp": 50, "fn": 0, "precision": 1 / 51, "recall": 1.0}
    return dataclasses.replace(result, metrics=m)


def test_failing_slice_fails_gate_even_if_overall_passes(
    tmp_path: Path, perfect: runner.RunResult
) -> None:
    rule = [{"metric": "field_precision.other", "op": "min", "value": "0.9"}]
    path = _thresholds(
        tmp_path,
        "phase1",
        {"overall": rule, "slices.lab_typed_clean": rule, "slices.lab_typed_poor": rule},
    )
    th = report.load_thresholds(path)
    good = report.build_report(perfect, [th])
    assert good["verdicts"]["phase1"]["verdict"] == "pass"
    bad = report.build_report(_with_bad_slice(perfect, "lab_typed_poor"), [th])
    v = bad["verdicts"]["phase1"]
    assert v["results"]["overall"]["field_precision.other"] == "pass"
    assert v["results"]["slices.lab_typed_clean"]["field_precision.other"] == "pass"
    assert v["results"]["slices.lab_typed_poor"]["field_precision.other"] == "fail"
    assert v["verdict"] == "fail"
    assert v["failed"] == ["slices.lab_typed_poor.field_precision.other"]
    # a slice the thresholds name but the set does not have is not measured: fail
    path = _thresholds(tmp_path, "phase1", {"slices.mixed": rule}, "m.json")
    gone = report.build_report(perfect, [report.load_thresholds(path)])
    assert gone["verdicts"]["phase1"]["results"]["slices.mixed"] == {
        "field_precision.other": "not_measured",
        report.SCOPE_DOCUMENTS: "fail",
    }
    assert gone["verdicts"]["phase1"]["verdict"] == "fail"


def test_not_measured_fails_required_metric(tmp_path: Path, perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates())
    p1 = rep["verdicts"]["phase1"]
    lab = p1["results"]["slices.lab_typed_clean"]
    # phase 0 has no code normaliser: code exact match is never measured, never a pass
    assert lab["code_exact_match"] == "not_measured"
    assert lab["invented_codes"] == "not_measured"
    assert lab["field_precision.other"] == "pass"
    # no GPU and no L40S: latency and memory are not measured, so phase 1 cannot pass
    assert p1["results"]["run"] == {
        "gpu_peak_bytes": "not_measured",
        "latency_p50_s": "not_measured",
        "latency_p95_s": "not_measured",
        report.SCOPE_DOCUMENTS: "fail",  # 13 documents, the gate wants 20
    }
    assert p1["verdict"] == "fail"
    assert "slices.lab_typed_clean.code_exact_match" in p1["failed"]
    assert rep["scopes"]["slices.lab_typed_clean"]["metrics"]["code_exact_match"] is None

    # identifier precision with nothing to score is not measured, and fails
    m = copy.deepcopy(perfect.metrics)
    m["slices"]["lab_typed_clean"]["identifier_precision"] = {
        "tp": 0,
        "fp": 0,
        "fn": 0,
        "precision": runner.NOT_MEASURED,
        "recall": runner.NOT_MEASURED,
    }
    rule = [{"metric": "identifier_precision", "op": "min", "value": "0.995"}]
    th = report.load_thresholds(_thresholds(tmp_path, "phase1", {"slices.lab_typed_clean": rule}))
    empty = report.build_report(dataclasses.replace(perfect, metrics=m), [th])
    assert empty["verdicts"]["phase1"]["verdict"] == "fail"
    assert empty["verdicts"]["phase1"]["failed"] == ["slices.lab_typed_clean.identifier_precision"]

    # the one exception: a metric the thresholds file marks not applicable for its gate
    na = [
        {"metric": "code_exact_match", "op": "not_applicable"},
        {"metric": "grounding_rate", "op": "min", "value": "0.95"},
    ]
    th = report.load_thresholds(_thresholds(tmp_path, "phase1", {"overall": na}, "na.json"))
    out = report.build_report(perfect, [th])
    assert out["verdicts"]["phase1"]["results"]["overall"] == {
        "code_exact_match": "not_applicable",
        "grounding_rate": "pass",
        report.SCOPE_DOCUMENTS: "pass",
    }
    assert out["verdicts"]["phase1"]["verdict"] == "pass"
    # a gate whose rules are all not_applicable checks nothing, and does not pass
    only_na = [{"metric": "code_exact_match", "op": "not_applicable"}]
    th = report.load_thresholds(_thresholds(tmp_path, "phase1", {"overall": only_na}, "o.json"))
    assert report.build_report(perfect, [th])["verdicts"]["phase1"]["verdict"] == "fail"
    # a gate that checks nothing is not a pass either
    th = report.load_thresholds(_thresholds(tmp_path, "phase1", {}, "none.json"))
    assert report.build_report(perfect, [th])["verdicts"]["phase1"]["verdict"] == "fail"


def test_latency_measured_only_on_the_named_hardware(perfect: runner.RunResult) -> None:
    l40s = dataclasses.replace(
        perfect,
        extractor=dataclasses.replace(TINY, hardware="nvidia-l40s"),
        gpu_peak_bytes=10 * 1024**3,
    )
    rep = report.build_report(l40s, [report.packaged_thresholds("phase1")])
    assert rep["verdicts"]["phase1"]["results"]["run"] == {
        "gpu_peak_bytes": "pass",
        "latency_p50_s": "pass",
        "latency_p95_s": "pass",
        report.SCOPE_DOCUMENTS: "fail",
    }
    over = dataclasses.replace(l40s, gpu_peak_bytes=24_000_000_001)
    rep = report.build_report(over, [report.packaged_thresholds("phase1")])
    assert rep["verdicts"]["phase1"]["results"]["run"]["gpu_peak_bytes"] == "fail"


def test_gated_scope_needs_enough_documents(tmp_path: Path, perfect: runner.RunResult) -> None:
    rules = [{"metric": "field_precision.other", "op": "min", "value": "0.9"}]
    for minimum, verdict in ((5, "pass"), (6, "fail")):
        scopes = {"slices.lab_typed_clean": {"min_documents": minimum, "rules": rules}}
        th = report.load_thresholds(_thresholds(tmp_path, "phase1", scopes, f"m{minimum}.json"))
        v = report.build_report(perfect, [th])["verdicts"]["phase1"]
        res = v["results"]["slices.lab_typed_clean"]
        assert res["field_precision.other"] == "pass"  # perfect, but only five documents
        assert res[report.SCOPE_DOCUMENTS] == verdict
        assert v["verdict"] == verdict


def test_rehearsal_gate(perfect: runner.RunResult, mini: minisets.MiniSet) -> None:
    rep = report.build_report(perfect, [report.packaged_thresholds("rehearsal")])
    assert rep["verdicts"]["rehearsal"]["verdict"] == "pass"
    assert rep["thresholds"]["rehearsal"]["production"] is False
    broken = runner.run(extractors.InvalidExtractor("schema_violation"), mini.root)
    rep = report.build_report(broken, [report.packaged_thresholds("rehearsal")])
    assert rep["verdicts"]["rehearsal"]["verdict"] == "fail"
    assert "run.invalid_output_share" in rep["verdicts"]["rehearsal"]["failed"]


def test_stubs_pass_only_the_rehearsal_gate(mini: minisets.MiniSet) -> None:
    stub = runner.run(_oracle(mini), mini.root)
    assert stub.extractor.engine == "stub"
    rep = report.build_report(stub, [report.packaged_thresholds("rehearsal")])
    assert rep["verdicts"]["rehearsal"]["verdict"] == "pass"
    for gate in ("phase1", "phase3"):
        with pytest.raises(Refused) as e:
            report.build_report(stub, [report.packaged_thresholds(gate)])
        assert e.value.reason == "not_allowed_here"
    # a name ending -stub is a stub whatever its engine says
    named = dataclasses.replace(stub, extractor=dataclasses.replace(TINY, name="quiet-stub"))
    with pytest.raises(Refused):
        report.build_report(named, [report.packaged_thresholds("phase1")])
    with pytest.raises(Refused):
        report.stopped_report(
            named.extractor,
            testset=report.testset_meta(mini.root),
            reason="timeouts",
            gates=[report.packaged_thresholds("phase3")],
        )


# ---------------------------------------------------------------- split, markdown, stop


def test_dev_split_labelled(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates())
    assert rep["split"] == {"name": "dev", "label": report.DEV_LABEL}
    assert report.DEV_LABEL == "development split, not a promotion result"
    assert report.DEV_LABEL in report.render_markdown(rep)
    test = report.build_report(dataclasses.replace(perfect, purpose="test"), _gates())
    assert test["split"] == {"name": "test", "label": None}
    assert report.DEV_LABEL not in report.render_markdown(test)
    with pytest.raises(Refused):
        report.build_report(dataclasses.replace(perfect, purpose="train"), _gates())


def test_markdown_failing_slice_first_and_definitions(
    tmp_path: Path, perfect: runner.RunResult
) -> None:
    rule = [{"metric": "field_precision.other", "op": "min", "value": "0.9"}]
    th = report.load_thresholds(
        _thresholds(
            tmp_path, "phase1", {"slices.lab_typed_clean": rule, "slices.lab_typed_poor": rule}
        )
    )
    rep = report.build_report(_with_bad_slice(perfect, "lab_typed_poor"), [th])
    md = report.render_markdown(rep)
    assert md.index("## slices.lab_typed_poor") < md.index("## slices.lab_typed_clean")
    assert report.SILENT_ERROR_DEFINITION in md
    assert "high" in report.SILENT_ERROR_DEFINITION and "miss" in report.SILENT_ERROR_DEFINITION
    assert "not_measured" in md and "never a pass" in md
    assert th.sha256 in md
    assert "| field_precision.other |" in md


def test_timeout_stop_reported(mini: minisets.MiniSet) -> None:
    rep = report.stopped_report(
        TINY,
        testset=report.testset_meta(mini.root),
        reason="timeouts",
        gates=_gates(),
    )
    raw = report.dumps(rep)
    assert report.parse_report(raw) == rep
    assert rep["stopped"] == "timeouts"
    for gate in report.DOCUMENT_GATES:
        assert rep["verdicts"][gate]["verdict"] == "fail"
    md = report.render_markdown(rep)
    assert "stopped" in md.lower() and "5%" in md
    with pytest.raises(Refused):
        report.stopped_report(
            TINY, testset=report.testset_meta(mini.root), reason="other", gates=_gates()
        )


def test_notebook_metrics_informational(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates(), notebook_metrics={"val_f1": "0.81", "steps": 900})
    assert rep["notebook_metrics"] == {
        "informational": True,
        "metrics": {"steps": 900, "val_f1": "0.81"},
    }
    assert "informational" in report.render_markdown(rep)
    for bad in ({"x" * 100: 1}, {"a": "b" * 300}, {"a": 0.5}, {"a": {"b": 1}}):
        with pytest.raises(Refused):
            report.build_report(perfect, _gates(), notebook_metrics=bad)


def test_notebook_metrics_signed_decimals(perfect: runner.RunResult) -> None:
    # Ruling D26: a negative metric is a signed fixed decimal, read back strictly.
    metrics = {"delta": "-0.030000", "loss": "12.500000", "big": "-1000000000000000000.000000"}
    rep = report.build_report(perfect, _gates(), notebook_metrics=metrics)
    back = report.parse_report(report.dumps(rep))
    assert back["notebook_metrics"]["metrics"] == metrics
    assert "-0.030000" in report.render_markdown(back)
    for bad in ("-0.000000", "--1.000000", "-1.5", "-01.000000", "-1.0000001", -3):
        with pytest.raises(Refused):
            report.build_report(perfect, _gates(), notebook_metrics={"delta": bad})
    obj = json.loads(report.dumps(rep))
    obj["notebook_metrics"]["metrics"]["delta"] = "-0.000000"
    with pytest.raises(Refused):
        report.parse_report(canonical.dumps(obj))


# ---------------------------------------------------------------- compare


def test_compare_markdown(perfect: runner.RunResult, noisy: runner.RunResult) -> None:
    before = report.build_report(perfect, _gates())
    after = report.build_report(noisy, _gates())
    md = report.compare(before, after)
    assert "| Metric | Before | After | Change |" in md
    lines = [ln for ln in md.splitlines() if ln.startswith("| field_recall.other |")]
    assert lines and all("regression" in ln for ln in lines[:2])
    assert "-0." in lines[0]
    assert "Regressions:" in md and "Regressions: 0" not in md
    # the regressions are summarised before any table
    assert md.index("Regressions:") < md.index("| Metric |")
    same = report.compare(before, before)
    assert "Regressions: 0" in same
    # better is not a regression
    back = report.compare(after, before)
    rows = [ln for ln in back.splitlines() if ln.startswith("| field_recall.other |")]
    assert rows and "regression" not in rows[0]


def test_compare_never_hides_a_regression(
    perfect: runner.RunResult, mini: minisets.MiniSet
) -> None:
    before = report.build_report(perfect, _gates())
    # a metric that became unmeasured, a scope that vanished, a verdict that got worse
    after = copy.deepcopy(before)
    after["scopes"]["slices.lab_typed_clean"]["metrics"]["grounding_rate"] = None
    del after["scopes"]["slices.lab_typed_poor"]
    after["verdicts"]["rehearsal"]["verdict"] = "fail"
    md = report.compare(before, after)
    assert "slices.lab_typed_clean.grounding_rate" in md
    assert "slices.lab_typed_poor" in md
    assert "rehearsal" in md
    reg = md.split("Regressions:", 1)[1].split("\n\n", 1)[0]
    assert "grounding_rate" in reg and "lab_typed_poor" in reg and "rehearsal" in reg
    # a stopped run against a full one: everything it lost is a regression
    stopped = report.stopped_report(
        TINY, testset=report.testset_meta(mini.root), reason="timeouts", gates=_gates()
    )
    assert "Regressions: 0" not in report.compare(before, stopped)
    # different test sets are called out
    other = copy.deepcopy(before)
    other["testset"]["sha256"] = "0" * 64
    assert "different test sets" in report.compare(before, other)


_MARKUP = ("<details>", '<a href="https://example.invalid/x">x</a>', "a|b", "**x**", "`x`")


def _set_key(d: dict[str, Any], old: str, new: str) -> None:
    d[new] = d.pop(old)


def _mutations(rep: dict[str, Any], bad: str) -> list[tuple[str, dict[str, Any]]]:
    """Every field render_markdown or compare interpolates, set to ``bad``."""
    out: list[tuple[str, dict[str, Any]]] = []

    def m(name: str, fn: Any) -> None:
        r = copy.deepcopy(rep)
        fn(r)
        out.append((name, r))

    m("extractor.name", lambda r: r["extractor"].__setitem__("name", bad))
    m("model_versions key", lambda r: r["model_versions"].__setitem__(bad, "0.1.0"))
    m("model_versions value", lambda r: r["model_versions"].__setitem__("extract", bad))
    m("engine.name", lambda r: r["engine"].__setitem__("name", bad))
    m("engine.version", lambda r: r["engine"].__setitem__("version", bad))
    m("hardware", lambda r: r.__setitem__("hardware", bad))
    for k in ("name", "version", "sha256", "extraction_schema_version"):
        m(f"testset.{k}", lambda r, k=k: r["testset"].__setitem__(k, bad))
    m("split.label", lambda r: r["split"].__setitem__("label", bad))
    m("thresholds.file", lambda r: r["thresholds"]["phase1"].__setitem__("file", bad))
    m("thresholds.sha256", lambda r: r["thresholds"]["phase1"].__setitem__("sha256", bad))
    m("scope key", lambda r: _set_key(r["scopes"], "overall", f"slices.{bad}"))
    m(
        "metric key",
        lambda r: _set_key(r["scopes"]["overall"]["metrics"], "grounding_rate", bad),
    )
    m(
        "metric value",
        lambda r: r["scopes"]["overall"]["metrics"].__setitem__("grounding_rate", bad),
    )
    m(
        "outcome key",
        lambda r: _set_key(r["scopes"]["slices.lab_typed_clean"]["outcomes"], "extracted", bad),
    )
    m(
        "outcome count",
        lambda r: r["scopes"]["slices.lab_typed_clean"]["outcomes"].__setitem__("extracted", bad),
    )
    m(
        "refusal key",
        lambda r: r["scopes"]["slices.lab_typed_clean"]["refusals"].__setitem__(bad, 0),
    )
    m("timeouts", lambda r: r["scopes"]["slices.lab_typed_clean"].__setitem__("timeouts", bad))
    m("hostile failure key", lambda r: r["hostile"].__setitem__("failures", {bad: 0}))
    m("hostile kind key", lambda r: _set_key(r["hostile"]["by_kind"], "two_patients", bad))
    m("hostile cases", lambda r: r["hostile"].__setitem__("cases", bad))
    m("verdict failed", lambda r: r["verdicts"]["phase1"]["failed"].append(bad))
    m(
        "verdict result key",
        lambda r: r["verdicts"]["phase1"]["results"]["run"].__setitem__(bad, "fail"),
    )
    m("notebook key", lambda r: r["notebook_metrics"]["metrics"].__setitem__(bad, 1))
    m("notebook value", lambda r: r["notebook_metrics"]["metrics"].__setitem__("val_f1", bad))
    m("stopped", lambda r: r.__setitem__("stopped", bad))
    m("run deadline", lambda r: r["run"].__setitem__("per_page_deadline_s", bad))
    return out


def test_crafted_report_with_markup_refused(perfect: runner.RunResult) -> None:
    rep = report.build_report(perfect, _gates(), notebook_metrics={"val_f1": "0.81"})
    assert report.parse_report(report.dumps(rep)) == rep
    for bad in _MARKUP:
        for name, crafted in _mutations(rep, bad):
            with pytest.raises(Refused):
                report.parse_report(canonical.dumps(crafted))
            assert name  # each interpolated field refused on its own
    # a thresholds file other than the packaged one for that gate is refused too
    crafted = copy.deepcopy(rep)
    crafted["thresholds"]["phase1"]["file"] = "thresholds/phase3.json"
    with pytest.raises(Refused):
        report.parse_report(canonical.dumps(crafted))
    # a failed list that disagrees with the results is refused
    crafted = copy.deepcopy(rep)
    crafted["verdicts"]["phase1"]["failed"] = []
    with pytest.raises(Refused):
        report.parse_report(canonical.dumps(crafted))


def test_compare_flags_suppression_and_not_applicable(perfect: runner.RunResult) -> None:
    before = report.build_report(perfect, _gates())
    after = copy.deepcopy(before)
    after["scopes"]["slices.lab_typed_poor"]["metrics"] = report.SUPPRESSED
    res = after["verdicts"]["phase1"]["results"]["slices.lab_typed_clean"]
    res["code_exact_match"] = "not_applicable"
    md = report.compare(before, after)
    reg = md.split("Regressions:", 1)[1].split("\n\n", 1)[0]
    assert "slices.lab_typed_poor: values suppressed after" in reg
    assert "slices.lab_typed_clean.code_exact_match: not_measured to not_applicable" in reg
    # shown after, suppressed before: not a regression by itself
    back = report.compare(after, before)
    reg = back.split("Regressions:", 1)[1].split("\n\n", 1)[0]
    assert "values suppressed" not in reg


def test_unscored_confidence_is_not_measured(tmp_path: Path, perfect: runner.RunResult) -> None:
    from intelligence.doceval import openai_compat

    info = openai_compat.OpenAICompatExtractor("http://127.0.0.1:9", "m").info()
    assert info.confidence_scored is False
    unscored = dataclasses.replace(perfect, extractor=info)
    rules = [
        {"metric": "calibration_ece", "op": "max", "value": "0.05"},
        {"metric": "silent_error_rate", "op": "max", "value": "0.005"},
        {"metric": "grounding_rate", "op": "min", "value": "0.5"},
    ]
    th = report.load_thresholds(_thresholds(tmp_path, "phase1", {"overall": rules}))
    rep = report.build_report(unscored, [th])
    res = rep["verdicts"]["phase1"]["results"]["overall"]
    assert res["calibration_ece"] == "not_measured"
    assert res["silent_error_rate"] == "not_measured"
    assert res["grounding_rate"] == "pass"
    assert rep["verdicts"]["phase1"]["verdict"] == "fail"
    for scope in ("overall", "slices.lab_typed_clean"):
        m = rep["scopes"][scope]["metrics"]
        assert m["calibration_ece"] is None and m["high_band_share"] is None
    # the same run with scored confidence measures them
    scored = report.build_report(perfect, [th])["verdicts"]["phase1"]["results"]["overall"]
    assert scored["calibration_ece"] == "pass"
    bad = dataclasses.replace(info, confidence_scored="no")  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        extractors.check_info(bad)
