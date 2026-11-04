import hashlib
import json
import logging
import re
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import schema, sets
from intelligence.doceval.synth import layouts, scanner
from intelligence.modelstore import canonical

SCALE = 0.02
MARKER = hashlib.sha256(b"task-8 secret seed marker").digest()


def _build(tmp: Path, purpose: str, seed: int | None = None) -> sets.SetManifest:
    return sets.build(purpose, seed, tmp, SCALE, version="1.0.0")  # type: ignore[arg-type]


@pytest.fixture(scope="module")
def dev_a(tmp_path_factory: pytest.TempPathFactory) -> sets.SetManifest:
    return _build(tmp_path_factory.mktemp("dev_a"), "dev")


@pytest.fixture(scope="module")
def dev_b(tmp_path_factory: pytest.TempPathFactory) -> sets.SetManifest:
    return _build(tmp_path_factory.mktemp("dev_b"), "dev", sets.DEV_SEED_V1)


@pytest.fixture(scope="module")
def test_a(tmp_path_factory: pytest.TempPathFactory) -> sets.SetManifest:
    return _build(tmp_path_factory.mktemp("test_a"), "test")


@pytest.fixture(scope="module")
def test_b(tmp_path_factory: pytest.TempPathFactory) -> sets.SetManifest:
    return _build(tmp_path_factory.mktemp("test_b"), "test")


def _manifest(m: sets.SetManifest) -> dict[str, Any]:
    raw = (m.root / "testset.json").read_bytes()
    assert canonical.is_canonical(raw)
    doc: dict[str, Any] = json.loads(raw)
    return doc


def _files(root: Path) -> Iterator[Path]:
    return (p for p in sorted(root.rglob("*")) if p.is_file())


def test_composition_v1_is_the_design_table() -> None:
    comp = sets.COMPOSITION_V1
    assert len(comp) == 8
    assert sum(s.documents for s in comp) == 1025
    assert sum(s.pages for s in comp) == 2000
    assert len({s.name for s in comp}) == 8
    assert [s.documents for s in comp] == [250, 200, 150, 100, 100, 100, 75, 50]
    assert [s.pages for s in comp] == [400, 320, 450, 200, 200, 200, 150, 80]


@pytest.mark.parametrize("scale", [1.0, 0.02, 0.1, 0.37])
def test_split_counts_add_up(scale: float) -> None:
    for s in sets.COMPOSITION_V1:
        total = sets.slice_documents(s, scale)
        dev = sets.split_documents(s, scale, "dev")
        test = sets.split_documents(s, scale, "test")
        assert dev + test == total
        assert abs(dev - 0.3 * total) <= 0.5
    full = {s.name: sets.split_documents(s, 1.0, "dev") for s in sets.COMPOSITION_V1}
    assert full["lab_typed_clean"] == 75
    assert (
        sum(full.values()) + sum(sets.split_documents(s, 1.0, "test") for s in sets.COMPOSITION_V1)
        == 1025
    )


def test_split_ratios_per_slice(dev_a: sets.SetManifest, test_a: sets.SetManifest) -> None:
    dev, test = _manifest(dev_a)["slices"], _manifest(test_a)["slices"]
    for s in sets.COMPOSITION_V1:
        total = sets.slice_documents(s, SCALE)
        assert dev[s.name]["documents"] == sets.split_documents(s, SCALE, "dev")
        assert test[s.name]["documents"] == sets.split_documents(s, SCALE, "test")
        assert dev[s.name]["documents"] + test[s.name]["documents"] == total
    assert dev_a.slices == dev and test_a.slices == test


def test_documents_not_split_across_pages(dev_a: sets.SetManifest) -> None:
    man = _manifest(dev_a)
    pages_dir = dev_a.root / "pages"
    ids = {d["id"] for d in man["documents"]}
    assert {p.name for p in pages_dir.iterdir()} == ids
    for d in man["documents"]:
        pngs = sorted(p.name for p in (pages_dir / d["id"]).iterdir())
        assert pngs == sorted(f"{n}.png" for n in range(1, d["pages"] + 1))
        truth = json.loads((dev_a.root / "truth" / f"{d['id']}.json").read_text())
        schema.validate_stored(truth)
        assert truth["document"]["page_count"] == d["pages"]
        for _, fld in schema.iter_fields(truth):
            assert fld["page"] is None or 1 <= fld["page"] <= d["pages"]


def test_ids_are_random_and_reveal_nothing(dev_a: sets.SetManifest) -> None:
    man = _manifest(dev_a)
    ids = [d["id"] for d in man["documents"]]
    assert ids == sorted(ids)
    assert len(set(ids)) == len(ids)
    for i in ids:
        assert re.fullmatch(r"[a-z2-7]{26}", i)
        assert not re.search(r"[0-9]{3}", i)
    # Every document is in exactly one slice; twins only in the hostile slice.
    for d in man["documents"]:
        assert d["slice"] in {s.name for s in sets.COMPOSITION_V1}
        if d["role"] == "clean_twin":
            assert d["slice"] == "hostile"


def test_hostile_files_name_their_twin(test_a: sets.SetManifest) -> None:
    man = _manifest(test_a)
    by_id = {d["id"]: d for d in man["documents"]}
    hostile = [d for d in man["documents"] if d["slice"] == "hostile" and d["role"] == "document"]
    assert hostile, "test at this scale still has a hostile document"
    for d in hostile:
        h = json.loads((test_a.root / "hostile" / f"{d['id']}.json").read_text())
        assert h["kind"] in ("injection", "two_patients", "wrong_patient_label", "blank", "rotated")
        assert h["expected"]["outcome"] in ("extract_with_flags", "refuse", "read_or_refuse")
        twin = by_id[h["clean_twin_id"]]
        assert twin["role"] == "clean_twin" and twin["twin_of"] == d["id"]


def test_dev_deterministic_with_public_seed(
    dev_a: sets.SetManifest, dev_b: sets.SetManifest
) -> None:
    assert dev_a.sha256 == dev_b.sha256
    a = [(p.relative_to(dev_a.root), p.read_bytes()) for p in _files(dev_a.root)]
    b = [(p.relative_to(dev_b.root), p.read_bytes()) for p in _files(dev_b.root)]
    assert a == b


def test_dev_refuses_another_seed(tmp_path: Path) -> None:
    with pytest.raises(sets.SetError, match="dev_seed_fixed"):
        _build(tmp_path, "dev", sets.DEV_SEED_V1 + 1)


def test_test_refuses_explicit_seed(tmp_path: Path) -> None:
    for seed in (0, 1, sets.DEV_SEED_V1):
        with pytest.raises(sets.SetError, match="seed_not_allowed"):
            _build(tmp_path, "test", seed)
    assert not (tmp_path / "test").exists()


def test_bad_arguments_refused(tmp_path: Path) -> None:
    with pytest.raises(sets.SetError, match="bad_purpose"):
        _build(tmp_path, "eval")
    with pytest.raises(sets.SetError, match="seed_required"):
        _build(tmp_path, "train")
    with pytest.raises(sets.SetError, match="bad_scale"):
        sets.build("dev", None, tmp_path, 0.0, version="1.0.0")
    with pytest.raises(sets.SetError, match="bad_scale"):
        sets.build("dev", None, tmp_path, float("nan"), version="1.0.0")
    with pytest.raises(sets.SetError, match="bad_version"):
        sets.build("dev", None, tmp_path, SCALE, version="1.0")


def test_two_test_builds_differ(test_a: sets.SetManifest, test_b: sets.SetManifest) -> None:
    assert test_a.sha256 != test_b.sha256
    ids_a = {d["id"] for d in _manifest(test_a)["documents"]}
    ids_b = {d["id"] for d in _manifest(test_b)["documents"]}
    assert not ids_a & ids_b
    truth_a = {p.read_bytes() for p in (test_a.root / "truth").iterdir()}
    truth_b = {p.read_bytes() for p in (test_b.root / "truth").iterdir()}
    assert not truth_a & truth_b
    # Same shape: the composition, not the content, is fixed.
    assert _manifest(test_a)["slices"].keys() == _manifest(test_b)["slices"].keys()


def test_test_seed_not_in_outputs_or_logs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    calls: list[int] = []

    def fake(n: int | None = None) -> bytes:
        calls.append(32 if n is None else n)
        return MARKER[: 32 if n is None else n]

    monkeypatch.setattr(sets.secrets, "token_bytes", fake)
    caplog.set_level(logging.DEBUG)
    m = _build(tmp_path, "test")
    assert calls == [32]
    needles = [
        MARKER,
        MARKER.hex().encode(),
        MARKER.hex().upper().encode(),
        str(int.from_bytes(MARKER, "big")).encode(),
        str(int.from_bytes(MARKER, "little")).encode(),
    ]
    files = list(_files(tmp_path))
    assert files
    for p in files:
        data = p.read_bytes()
        for needle in needles:
            assert needle not in data, p.name
    for rec in caplog.records:
        text = (rec.getMessage() + repr(rec.args) + repr(rec.__dict__)).encode()
        for needle in needles:
            assert needle not in text
    assert any(rec.name == "intelligence.doceval.sets" for rec in caplog.records)
    shown = repr(m).encode() + repr(m.__dict__).encode()
    for needle in needles:
        assert needle not in shown
    man = _manifest(m)
    assert "seed" not in json.dumps(man)


def test_manifest_hash_stable_and_complete(dev_a: sets.SetManifest) -> None:
    raw = (dev_a.root / "testset.json").read_bytes()
    assert hashlib.sha256(raw).hexdigest() == dev_a.sha256
    man = _manifest(dev_a)
    assert man["schema"] == "mhai.evaluation.testset/1"
    assert man["name"] == "clinical_document"
    assert man["version"] == "1.0.0"
    assert man["purpose"] == "dev"
    assert man["extraction_schema_version"] == schema.SCHEMA_VERSION
    assert "schema_version" not in man  # the scanner would read it as a document
    assert set(man["generator"]) == {"package", "commit"}
    listed = [f["path"] for f in man["files"]]
    assert listed == sorted(listed)
    on_disk = sorted(
        str(p.relative_to(dev_a.root)) for p in _files(dev_a.root) if p.name != "testset.json"
    )
    assert listed == on_disk
    for f in man["files"]:
        data = (dev_a.root / f["path"]).read_bytes()
        assert f["size"] == len(data)
        assert f["sha256"] == hashlib.sha256(data).hexdigest()
    pages = sum(d["pages"] for d in man["documents"])
    assert pages == sum(1 for p in listed if p.endswith(".png"))
    docs = [d for d in man["documents"] if d["role"] == "document"]
    assert man["totals"]["documents"] == len(docs) == dev_a.documents
    assert man["totals"]["pages"] == sum(d["pages"] for d in docs) == dev_a.pages


def test_never_overwrites(dev_a: sets.SetManifest) -> None:
    with pytest.raises(sets.SetError, match="exists"):
        sets.build("dev", None, dev_a.root.parent, SCALE, version="1.0.0")


def test_scanner_passes_on_built_set(dev_a: sets.SetManifest, test_a: sets.SetManifest) -> None:
    for m in (dev_a, test_a):
        res = scanner.scan_set(m.root)
        assert res.ok, [str(f) for f in res.findings]
        assert res.scanned > 0
        for p in _files(m.root):
            for run in re.findall(r"[0-9]+", str(p.relative_to(m.root))):
                assert len(run) <= 5


def test_eval_layouts_only(dev_a: sets.SetManifest) -> None:
    assert sets.template_seeds("dev") == layouts.EVAL_TEMPLATE_SEEDS
    assert sets.template_seeds("test") == layouts.EVAL_TEMPLATE_SEEDS
    assert sets.template_seeds("train") == layouts.TRAIN_TEMPLATE_SEEDS


def test_train_builds_with_any_seed(tmp_path: Path) -> None:
    a = sets.build("train", 7, tmp_path / "a", 0.01, version="1.0.0")
    b = sets.build("train", 7, tmp_path / "b", 0.01, version="1.0.0")
    c = sets.build("train", 8, tmp_path / "c", 0.01, version="1.0.0")
    assert a.sha256 == b.sha256 != c.sha256
    assert {k: v["documents"] for k, v in a.slices.items()} == {
        s.name: sets.slice_documents(s, 0.01) for s in sets.COMPOSITION_V1
    }
    assert scanner.scan_set(a.root).ok


def test_document_tripping_the_scanner_is_redrawn(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    real = sets._findings
    calls = {"n": 0}

    def every_other(made: Any) -> list[str]:
        calls["n"] += 1
        return ["long_digits"] if calls["n"] % 2 else real(made)

    monkeypatch.setattr(sets, "_findings", every_other)
    caplog.set_level(logging.INFO)
    m = sets.build("train", 3, tmp_path, 0.01, version="1.0.0")
    assert scanner.scan_set(m.root).ok
    assert calls["n"] >= 2 * m.documents
    assert any("redrawn_for_scanner=[('long_digits'" in r.getMessage() for r in caplog.records)


def test_scanner_refusal_fails_closed(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    refused = scanner.ScanResult(False, (scanner.Finding("truth/x.json#patient.mrn", "mrn"),), 1)
    monkeypatch.setattr(sets.synth_scanner, "scan_set", lambda _p: refused)
    with pytest.raises(sets.SetError, match="scanner_refused"):
        sets.build("train", 3, tmp_path, 0.01, version="1.0.0")
    assert list(tmp_path.iterdir()) == []  # nothing kept, not even the temporary folder


@pytest.mark.slow
def test_full_v1_composition(tmp_path: Path) -> None:
    dev = sets.build("dev", None, tmp_path, 1.0, version="1.0.0")
    test = sets.build("test", None, tmp_path, 1.0, version="1.0.0")
    assert dev.documents + test.documents == 1025
    pages = dev.pages + test.pages
    assert 1900 <= pages <= 2100, pages
    for s in sets.COMPOSITION_V1:
        d, t = dev.slices[s.name], test.slices[s.name]
        assert d["documents"] + t["documents"] == s.documents
        assert abs(d["pages"] + t["pages"] - s.pages) <= 0.1 * s.pages, s.name
    for m in (dev, test):
        res = scanner.scan_set(m.root)
        assert res.ok, [str(f) for f in res.findings]
    logging.getLogger(__name__).info(
        "full v1: dev %d docs %d pages, test %d docs %d pages",
        dev.documents,
        dev.pages,
        test.documents,
        test.pages,
    )
