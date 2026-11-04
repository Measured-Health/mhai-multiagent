"""The evaluation runner over any Extractor (P21, P22, testset verification)."""

import json
import logging
import random
import time
from collections.abc import Sequence
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import extractors, runner, sets
from intelligence.modelstore.reasons import Refused
from tests.doceval import minisets

MARKER = "Zqxmarkerwv"


def _docs(
    n: int, slices: Sequence[str] = ("lab", "notes"), seed: int = 0
) -> list[minisets.MiniDoc]:
    return [
        minisets.MiniDoc(slices[k % len(slices)], minisets.visible_truth(seed + k))
        for k in range(n)
    ]


@pytest.fixture(scope="module")
def small(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    return minisets.build(tmp_path_factory.mktemp("small") / "set", _docs(4))


@pytest.fixture(scope="module")
def wide(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    return minisets.build(tmp_path_factory.mktemp("wide") / "set", _docs(24, ("lab",), 100))


def _oracle(s: minisets.MiniSet, **kw: Any) -> extractors.OracleExtractor:
    return extractors.OracleExtractor(s.root / "truth", random.Random(7), **kw)


def _overall_fields(result: runner.RunResult) -> dict[str, int]:
    pr = result.metrics["overall"]["field_pr"]
    return {k: sum(v[k] for v in pr.values()) for k in ("tp", "fp", "fn")}


def test_oracle_perfect_scores_one(small: minisets.MiniSet) -> None:
    result = runner.run(_oracle(small), small.root)
    assert result.outcomes == {"extracted": 4}
    assert result.documents == 4
    for scope in [*result.metrics["slices"].values(), result.metrics["overall"]]:
        measured = [v for v in scope["field_pr"].values() if v["tp"]]
        assert measured
        for v in measured:
            assert v["precision"] == 1.0 and v["recall"] == 1.0
            assert v["fp"] == 0 and v["fn"] == 0
        assert scope["identifier_precision"]["precision"] == 1.0
        assert scope["silent_error"]["rate"] == 0.0
        assert scope["grounding"]["rate"] == 1.0
        assert scope["calibration_ece"] == 0.0
        assert scope["code_exact_match"] == runner.NOT_MEASURED
    assert set(result.metrics["slices"]) == {"lab", "notes"}
    assert result.latency["p50"] != runner.NOT_MEASURED


def test_oracle_drop_rate_moves_recall(small: minisets.MiniSet) -> None:
    full = _overall_fields(runner.run(_oracle(small), small.root))
    half = _overall_fields(runner.run(_oracle(small, drop=0.5), small.root))
    recall = half["tp"] / (half["tp"] + half["fn"])
    # a dropped unit makes its row's numeric values wrong, so precision dips a little
    assert half["tp"] / (half["tp"] + half["fp"]) >= 0.85
    assert 0.35 <= recall <= 0.65
    assert full["fn"] == 0


def test_runs_are_deterministic_apart_from_latency(small: minisets.MiniSet) -> None:
    a = runner.run(_oracle(small, drop=0.2, corrupt=0.2), small.root)
    b = runner.run(_oracle(small, drop=0.2, corrupt=0.2), small.root)
    assert a.metrics == b.metrics
    assert a.outcomes == b.outcomes
    assert a.testset_sha256 == b.testset_sha256


def test_none_rates_are_not_measured(small: minisets.MiniSet) -> None:
    result = runner.run(extractors.RefusingExtractor("unreadable"), small.root)
    overall = result.metrics["overall"]
    assert result.outcomes == {"refused:unreadable": 4}
    assert overall["silent_error"]["rate"] == runner.NOT_MEASURED
    assert overall["calibration_ece"] == runner.NOT_MEASURED
    assert overall["grounding"]["rate"] == runner.NOT_MEASURED
    assert overall["field_pr"]["text"]["precision"] == runner.NOT_MEASURED
    assert overall["field_pr"]["text"]["recall"] == 0.0
    assert result.hostile["cases"] == 0
    assert result.hostile["rate"] == runner.NOT_MEASURED


@pytest.mark.parametrize(
    ("kind", "code"),
    [
        ("bad_json", "bad_json"),
        ("schema_violation", "schema_error"),
        ("oversize", "oversize"),
        ("raises", "extractor_error"),
    ],
)
def test_invalid_outputs_counted_not_crashing(
    small: minisets.MiniSet, kind: str, code: str
) -> None:
    result = runner.run(extractors.InvalidExtractor(kind), small.root)
    assert result.outcomes == {code: 4}
    overall = _overall_fields(result)
    assert overall["tp"] == 0 and overall["fp"] == 0 and overall["fn"] > 0


class _Returns:
    """An extractor returning whatever it is given (not an ExtractionResult, etc.)."""

    def __init__(self, value: Any) -> None:
        self.value = value

    def info(self) -> extractors.ExtractorInfo:
        return extractors.ExtractorInfo("returns", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> Any:
        return self.value


@pytest.mark.parametrize(
    "value",
    [
        None,
        {"not": "a result"},
        extractors.Refused("made up reason"),
        extractors.Extracted({}, (float("nan"),), None),
        extractors.Extracted({}, (0.1,), -5),
        extractors.Extracted({"a": float("nan")}, (0.1,), None),
        extractors.Extracted({"a": {1, 2}}, (0.1,), None),
        extractors.Extracted("[1, 2", (0.1,), None),
        extractors.Extracted(b'{"a": 1, "a": 2}', (0.1,), None),
    ],
)
def test_bad_results_get_fixed_codes(small: minisets.MiniSet, value: Any) -> None:
    result = runner.run(_Returns(value), small.root)  # type: ignore[arg-type]
    assert len(result.outcomes) == 1
    (code,) = result.outcomes
    assert code in runner.OUTCOME_CODES


def test_oversize_dict_counted(small: minisets.MiniSet) -> None:
    big = {"x": "a" * 5000}
    result = runner.run(
        _Returns(extractors.Extracted(big, (0.1,), None)),  # type: ignore[arg-type]
        small.root,
        max_output_bytes=1000,
    )
    assert result.outcomes == {"oversize": 4}


class _SlowFirst:
    """Slow on its first call only, then an oracle."""

    def __init__(self, inner: extractors.Extractor, seconds: float) -> None:
        self.inner, self.seconds, self.calls = inner, seconds, 0

    def info(self) -> extractors.ExtractorInfo:
        return self.inner.info()

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> extractors.ExtractionResult:
        self.calls += 1
        if self.calls == 1:
            time.sleep(self.seconds)
        return self.inner.extract(pages, deadline_s=deadline_s)


def test_deadline_enforced(wide: minisets.MiniSet) -> None:
    ex = _SlowFirst(_oracle(wide), 3.0)
    t = time.perf_counter()
    result = runner.run(ex, wide.root, per_page_deadline_s=0.3)
    assert time.perf_counter() - t < 2.5
    assert result.outcomes == {"deadline": 1, "extracted": 23}
    assert ex.calls == 24


def test_deadline_over_five_percent_stops_the_run(small: minisets.MiniSet) -> None:
    t = time.perf_counter()
    with pytest.raises(Refused) as e:
        runner.run(extractors.SlowExtractor(30), small.root, per_page_deadline_s=0.2)
    assert time.perf_counter() - t < 2.0
    assert e.value.reason == "evaluation_failed"
    assert e.value.detail_code == "timeouts"


@pytest.mark.parametrize("bad", [0, -1, float("nan"), float("inf"), True])
def test_bad_limits_refused(small: minisets.MiniSet, bad: Any) -> None:
    with pytest.raises(ValueError):
        runner.run(_oracle(small), small.root, per_page_deadline_s=bad)
    with pytest.raises(ValueError):
        runner.run(_oracle(small), small.root, max_output_bytes=bad)


class _Spy:
    def __init__(self) -> None:
        self.calls = 0

    def info(self) -> extractors.ExtractorInfo:
        return extractors.ExtractorInfo("spy", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> extractors.ExtractionResult:
        self.calls += 1
        return extractors.Refused("internal")


def _tamper_png(root: Path) -> None:
    page = next((root / "pages").glob("*/1.png"))
    data = bytearray(page.read_bytes())
    data[-5] ^= 1
    page.write_bytes(bytes(data))


def _tamper_extra(root: Path) -> None:
    (root / "truth" / "extra.json").write_text("{}")


def _tamper_missing(root: Path) -> None:
    next((root / "truth").glob("*.json")).unlink()


def _tamper_manifest(root: Path) -> None:
    raw = json.loads((root / "testset.json").read_text())
    (root / "testset.json").write_text(json.dumps(raw, indent=1))


def _tamper_schema_version(root: Path) -> None:
    from intelligence.modelstore import canonical

    raw = json.loads((root / "testset.json").read_text())
    raw["extraction_schema_version"] = "9.9.9"
    (root / "testset.json").write_bytes(canonical.dumps(raw))


def _tamper_symlink(root: Path) -> None:
    page = next((root / "pages").glob("*/1.png"))
    target = root.parent / "elsewhere.png"
    target.write_bytes(page.read_bytes())
    page.unlink()
    page.symlink_to(target)


def _tamper_counts(root: Path) -> None:
    from intelligence.modelstore import canonical

    raw = json.loads((root / "testset.json").read_text())
    raw["totals"]["documents"] += 1
    (root / "testset.json").write_bytes(canonical.dumps(raw))


@pytest.mark.parametrize(
    "tamper",
    [
        _tamper_png,
        _tamper_extra,
        _tamper_missing,
        _tamper_manifest,
        _tamper_schema_version,
        _tamper_symlink,
        _tamper_counts,
    ],
)
def test_testset_tamper_refused_before_running(tmp_path: Path, tamper: Any) -> None:
    s = minisets.build(tmp_path / "set", _docs(2))
    tamper(s.root)
    spy = _Spy()
    with pytest.raises(Refused) as e:
        runner.run(spy, s.root)
    assert e.value.reason == "testset_mismatch"
    assert spy.calls == 0


def test_missing_set_refused(tmp_path: Path) -> None:
    with pytest.raises(Refused) as e:
        runner.run(_Spy(), tmp_path / "nothing")
    assert e.value.reason == "testset_mismatch"


class _RaisesWithValue:
    def info(self) -> extractors.ExtractorInfo:
        return extractors.ExtractorInfo("raiser", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> extractors.ExtractionResult:
        raise RuntimeError(f"could not read {MARKER}")


def test_runner_logs_no_values(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    docs = [minisets.MiniDoc("lab", minisets.visible_truth(k, text=MARKER)) for k in range(3)]
    docs.append(
        minisets.MiniDoc(
            "hostile",
            minisets.visible_truth(9, text=MARKER),
            hostile_expected=minisets.REFUSE_TWO_PATIENTS,
        )
    )
    s = minisets.build(tmp_path / "set", docs)
    caplog.set_level(logging.DEBUG)
    results = [
        runner.run(_oracle(s, drop=0.3, corrupt=0.3), s.root),
        runner.run(_RaisesWithValue(), s.root),
        runner.run(extractors.InvalidExtractor("schema_violation"), s.root),
        runner.run(extractors.RefusingExtractor("no_text"), s.root),
    ]
    with pytest.raises(Refused):
        runner.run(extractors.SlowExtractor(5), s.root, per_page_deadline_s=0.1)
    assert caplog.records
    for rec in caplog.records:
        assert MARKER.casefold() not in rec.getMessage().casefold()
        assert MARKER not in repr(rec.args)
        assert rec.exc_info is None
    for r in results:
        assert MARKER.casefold() not in repr(r).casefold()


def test_refusal_on_hostile_scored_as_flagged(tmp_path: Path) -> None:
    docs = _docs(2, ("lab",))
    docs.append(
        minisets.MiniDoc(
            "hostile", minisets.visible_truth(50), hostile_expected=minisets.REFUSE_TWO_PATIENTS
        )
    )
    s = minisets.build(tmp_path / "set", docs)
    good = runner.run(extractors.RefusingExtractor("multiple_patients"), s.root)
    assert good.hostile["cases"] == 1
    assert good.hostile["flagged_or_refused"] == 1
    assert good.hostile["other_fields_unchanged"] == 1
    assert good.hostile["passed"] == 1
    assert good.hostile["rate"] == 1.0
    assert good.hostile["failures"] == {}
    assert good.documents == 3  # the clean twin is not run or counted
    wrong = runner.run(extractors.RefusingExtractor("no_text"), s.root)
    assert wrong.hostile["passed"] == 0
    assert wrong.hostile["failures"] == {"refused_wrong_reason": 1}
    broken = runner.run(extractors.InvalidExtractor("raises"), s.root)
    assert broken.hostile["passed"] == 0
    assert broken.hostile["flagged_or_refused"] == 0
    assert broken.hostile["failures"] == {runner.HOSTILE_FAILED: 1}
    oracle = runner.run(_oracle(s), s.root)  # extracting a two-patient page fails
    assert oracle.hostile["failures"] == {"extracted_when_refusal_expected": 1}
    assert oracle.hostile["by_kind"] == {"two_patients": {"cases": 1, "passed": 0}}


def test_hostile_field_pr_against_clean_twin(tmp_path: Path) -> None:
    twin = minisets.visible_truth(60)
    hostile_truth = minisets.visible_truth(61)
    expected = dict(minisets.REFUSE_TWO_PATIENTS, outcome="read_or_refuse", refusal_reason=None)
    docs = [
        minisets.MiniDoc("hostile", hostile_truth, hostile_expected=expected, twin_truth=twin),
    ]
    s = minisets.build(tmp_path / "set", docs)
    # the oracle returns the hostile page set's truth; scored against the twin it differs
    result = runner.run(_oracle(s), s.root)
    pr = result.metrics["slices"]["hostile"]["field_pr"]
    assert sum(v["fp"] for v in pr.values()) > 0


@pytest.mark.parametrize("purpose", ["dev"])
def test_runner_on_built_set(tmp_path: Path, purpose: str) -> None:
    built = sets.build(purpose, None, tmp_path, 0.02, version="1.0.0")  # type: ignore[arg-type]
    result = runner.run(
        extractors.OracleExtractor(built.root / "truth", random.Random(0)), built.root
    )
    assert result.testset_sha256 == built.sha256
    assert result.documents == built.documents
    assert result.pages == built.pages
    assert set(result.metrics["slices"]) == set(built.slices)
    assert result.outcomes == {"extracted": built.documents}
    for name, scope in result.metrics["slices"].items():
        if name == "hostile":
            continue
        for v in scope["field_pr"].values():
            assert v["fp"] == 0 and v["fn"] == 0
    assert result.hostile["cases"] == built.slices["hostile"]["documents"]


def _truth_of(s: minisets.MiniSet, i: str) -> dict[str, Any]:
    loaded: dict[str, Any] = json.loads((s.root / "truth" / f"{i}.json").read_text())
    return loaded


def test_band_must_match_confidence(small: minisets.MiniSet) -> None:
    from intelligence.doceval import schema

    doc = _truth_of(small, small.ids[0])
    _, fld = next(iter(schema.iter_fields(doc)))
    fld["confidence"] = 0.5  # still band "high": would hide a wrong value from silent errors
    result = runner.run(
        _Returns(extractors.Extracted(doc, (0.1,), None)),  # type: ignore[arg-type]
        small.root,
    )
    assert result.outcomes == {"schema_error": 4}


def test_high_band_share_reported(small: minisets.MiniSet) -> None:
    perfect = runner.run(_oracle(small), small.root)
    for scope in [*perfect.metrics["slices"].values(), perfect.metrics["overall"]]:
        assert scope["high_band_share"]["rate"] == 1.0
    corrupt = runner.run(_oracle(small, corrupt=1.0), small.root)
    assert corrupt.metrics["overall"]["high_band_share"]["rate"] == 0.0
    refused = runner.run(extractors.RefusingExtractor("internal"), small.root)
    assert refused.metrics["overall"]["high_band_share"]["rate"] == runner.NOT_MEASURED


class _BadLen(tuple):  # type: ignore[type-arg]
    def __len__(self) -> int:
        raise RuntimeError(MARKER)


def test_unexpected_exception_in_checks_is_extractor_error(small: minisets.MiniSet) -> None:
    value = extractors.Extracted({}, _BadLen((0.1,)), None)
    result = runner.run(_Returns(value), small.root)  # type: ignore[arg-type]
    assert result.outcomes == {"extractor_error": 4}


def test_overall_excludes_hostile(tmp_path: Path) -> None:
    docs = _docs(2, ("lab",))
    plain = minisets.build(tmp_path / "plain", docs)
    docs.append(
        minisets.MiniDoc(
            "hostile", minisets.visible_truth(70), hostile_expected=minisets.REFUSE_TWO_PATIENTS
        )
    )
    mixed = minisets.build(tmp_path / "mixed", docs)
    a = runner.run(_oracle(plain), plain.root)
    b = runner.run(_oracle(mixed), mixed.root)
    assert a.metrics["overall"] == b.metrics["overall"]
    assert b.metrics["slices"]["hostile"]["documents"] == 1
    assert b.metrics["overall"]["documents"] == 2


def test_hostile_scoring_error_stops_the_run(tmp_path: Path) -> None:
    expected = dict(minisets.REFUSE_TWO_PATIENTS, outcome="not_an_outcome")
    docs = [minisets.MiniDoc("hostile", minisets.visible_truth(80), hostile_expected=expected)]
    s = minisets.build(tmp_path / "set", docs)
    with pytest.raises(Refused) as e:
        runner.run(extractors.RefusingExtractor("multiple_patients"), s.root)
    assert e.value.reason == "evaluation_failed"
    assert e.value.detail_code == "hostile_scoring"


@pytest.mark.parametrize(
    "doc",
    [
        minisets.MiniDoc("hostile", minisets.visible_truth(90)),  # hostile without a twin
        minisets.MiniDoc(
            "lab", minisets.visible_truth(91), hostile_expected=minisets.REFUSE_TWO_PATIENTS
        ),  # a twin outside the hostile slice
    ],
)
def test_twins_only_and_always_in_hostile_slice(tmp_path: Path, doc: minisets.MiniDoc) -> None:
    s = minisets.build(tmp_path / "set", [*_docs(1, ("lab",)), doc])
    spy = _Spy()
    with pytest.raises(Refused) as e:
        runner.run(spy, s.root)
    assert e.value.reason == "testset_mismatch"
    assert spy.calls == 0


@pytest.mark.parametrize("cap", ["_MAX_JSON_BYTES", "_MAX_PAGE_BYTES"])
def test_listed_size_over_cap_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, cap: str
) -> None:
    s = minisets.build(tmp_path / "set", _docs(1))
    monkeypatch.setattr(runner, cap, 10)
    spy = _Spy()
    with pytest.raises(Refused) as e:
        runner.run(spy, s.root)
    assert e.value.reason == "testset_mismatch"
    assert spy.calls == 0
