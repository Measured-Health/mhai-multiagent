"""The extractor interface and the stub extractors (tests and the CI rehearsal only)."""

import json
import random
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import extractors, metrics, schema
from tests.doceval import minisets


def _pages(root: Path, doc_id: str) -> list[bytes]:
    folder = root / "pages" / doc_id
    return [p.read_bytes() for p in sorted(folder.iterdir(), key=lambda p: int(p.stem))]


def _truth(root: Path, doc_id: str) -> dict[str, Any]:
    loaded: dict[str, Any] = json.loads((root / "truth" / f"{doc_id}.json").read_text())
    return loaded


@pytest.fixture(scope="module")
def mini(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    docs = [minisets.MiniDoc("a", minisets.visible_truth(s)) for s in (1, 2, 3)]
    return minisets.build(tmp_path_factory.mktemp("ex") / "set", docs)


def test_oracle_returns_truth_by_pages(mini: minisets.MiniSet) -> None:
    oracle = extractors.OracleExtractor(mini.root / "truth", random.Random(0))
    for i in mini.ids:
        got = oracle.extract(_pages(mini.root, i), deadline_s=10)
        assert isinstance(got, extractors.Extracted)
        schema.validate_stored(got.document)
        assert got.document == _truth(mini.root, i)
        assert len(got.page_seconds) == 1
        assert got.gpu_peak_bytes is None


def test_oracle_unknown_pages_refused_internal(mini: minisets.MiniSet) -> None:
    oracle = extractors.OracleExtractor(mini.root / "truth", random.Random(0))
    got = oracle.extract([minisets.png(999_999)], deadline_s=10)
    assert got == extractors.Refused("internal")


def test_oracle_is_deterministic_and_schema_valid_with_errors(mini: minisets.MiniSet) -> None:
    def run(seed: int) -> list[Any]:
        o = extractors.OracleExtractor(
            mini.root / "truth", random.Random(seed), drop=0.3, corrupt=0.3
        )
        out = []
        for i in mini.ids:
            r = o.extract(_pages(mini.root, i), deadline_s=10)
            assert isinstance(r, extractors.Extracted)
            schema.validate_stored(r.document)
            out.append(r.document)
        return out

    a, b, c = run(1), run(1), run(2)
    assert a == b
    assert a != c
    truths = [_truth(mini.root, i) for i in mini.ids]
    pr = metrics.field_pr(truths, a)
    tp = sum(v["tp"] for v in pr.values())
    fp = sum(v["fp"] for v in pr.values())
    fn = sum(v["fn"] for v in pr.values())
    assert fp > 0 and fn > 0 and tp > 0


def test_oracle_corrupt_every_field_changes_every_value(mini: minisets.MiniSet) -> None:
    o = extractors.OracleExtractor(mini.root / "truth", random.Random(3), corrupt=1.0)
    for i in mini.ids:
        r = o.extract(_pages(mini.root, i), deadline_s=10)
        assert isinstance(r, extractors.Extracted)
        schema.validate_stored(r.document)
        truth = _truth(mini.root, i)
        pr = metrics.field_pr(truth, r.document)
        assert sum(v["tp"] for v in pr.values()) == 0
        # honest default confidence: every changed field is low band, so no silent error
        assert metrics.silent_error_rate(truth, r.document)["errors"] == 0


def test_oracle_confidence_model_drives_bands(mini: minisets.MiniSet) -> None:
    o = extractors.OracleExtractor(
        mini.root / "truth",
        random.Random(3),
        corrupt=1.0,
        confidence_model=lambda correct, rng: 0.99,
    )
    i = mini.ids[0]
    r = o.extract(_pages(mini.root, i), deadline_s=10)
    assert isinstance(r, extractors.Extracted)
    assert {f["band"] for _, f in schema.iter_fields(r.document)} == {"high"}
    s = metrics.silent_error_rate(_truth(mini.root, i), r.document)
    assert s["rate"] == 1.0


def test_band_thresholds() -> None:
    assert extractors.band(0.95) == "high"
    assert extractors.band(0.9499) == "medium"
    assert extractors.band(0.80) == "medium"
    assert extractors.band(0.7999) == "low"


@pytest.mark.parametrize("bad", [-0.1, 1.5, -1.0])
def test_oracle_rates_checked(mini: minisets.MiniSet, bad: float) -> None:
    with pytest.raises(ValueError, match="bad_rate"):
        extractors.OracleExtractor(mini.root / "truth", random.Random(0), drop=bad)
    with pytest.raises(ValueError, match="bad_rate"):
        extractors.OracleExtractor(mini.root / "truth", random.Random(0), corrupt=bad)


def test_refusing_extractor() -> None:
    r = extractors.RefusingExtractor("multiple_patients")
    assert r.extract([b"x"], deadline_s=1) == extractors.Refused("multiple_patients")
    with pytest.raises(ValueError, match="bad_reason"):
        extractors.RefusingExtractor("not a reason")
    assert set(extractors.REFUSAL_REASONS) == {
        "multiple_patients",
        "unreadable",
        "no_text",
        "no_patient_identifiers",
        "internal",
    }


def test_slow_extractor_sleeps() -> None:
    import time

    s = extractors.SlowExtractor(0.05)
    t = time.perf_counter()
    s.extract([b"x"], deadline_s=1)
    assert time.perf_counter() - t >= 0.05


@pytest.mark.parametrize("kind", extractors.INVALID_KINDS)
def test_invalid_extractor_kinds(kind: str) -> None:
    x = extractors.InvalidExtractor(kind)
    if kind == "raises":
        with pytest.raises(RuntimeError):
            x.extract([b"x"], deadline_s=1)
        return
    r = x.extract([b"x"], deadline_s=1)
    assert isinstance(r, extractors.Extracted)
    if kind == "bad_json":
        assert isinstance(r.document, bytes)
        with pytest.raises(ValueError):
            json.loads(r.document)
    elif kind == "oversize":
        assert isinstance(r.document, bytes)
        assert len(r.document) > 2_000_000
    else:
        with pytest.raises(schema.SchemaError):
            schema.validate_stored(r.document)
    with pytest.raises(ValueError, match="bad_kind"):
        extractors.InvalidExtractor("nope")


def test_info_is_checked() -> None:
    for ex in (
        extractors.RefusingExtractor("internal"),
        extractors.SlowExtractor(0.0),
        extractors.InvalidExtractor("raises"),
    ):
        info = ex.info()
        extractors.check_info(info)
    with pytest.raises(ValueError, match="bad_extractor_info"):
        extractors.check_info(extractors.ExtractorInfo("Bad Name!", {}, "stub", "cpu"))
    with pytest.raises(ValueError, match="bad_extractor_info"):
        extractors.check_info(extractors.ExtractorInfo("ok-name", {"r": "a b"}, "stub", "cpu"))
