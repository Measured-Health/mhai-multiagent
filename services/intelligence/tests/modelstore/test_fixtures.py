"""The tiny synthetic run fixtures: every variant builds, with exactly its defect."""

import json
import struct
from pathlib import Path
from typing import Any

import pytest
from safetensors.numpy import load_file

from intelligence.modelstore import canonical, fixtures
from intelligence.modelstore.reasons import REASONS, Refused

_GOOD_FILES = {
    "config.json",
    "model.safetensors",
    "tokenizer.json",
    "tokenizer_config.json",
    "preprocessor_config.json",
    "generation_config.json",
    "training_manifest.json",
}


def _header(path: Path) -> tuple[dict[str, Any], int]:
    raw = path.read_bytes()
    (n,) = struct.unpack("<Q", raw[:8])
    return json.loads(raw[8 : 8 + n]), len(raw) - 8 - n


def _spans(header: dict[str, Any]) -> list[tuple[int, int]]:
    return sorted(tuple(v["data_offsets"]) for k, v in header.items() if k != "__metadata__")


def _tm(run: Path) -> dict[str, Any]:
    loaded: dict[str, Any] = canonical.loads_strict(
        (run / "training_manifest.json").read_bytes(), 1 << 20
    )
    return loaded


@pytest.mark.parametrize("variant", fixtures.VARIANTS)
def test_every_fixture_variant_builds(tmp_path: Path, variant: str) -> None:
    run = fixtures.make_run(tmp_path / "run", variant, 5)
    names = {p.name for p in run.iterdir()}
    reason = fixtures.EXPECTED_REASONS[variant]
    assert reason is None or reason in REASONS
    assert (run / "model.safetensors").stat().st_size < 16 * 1024

    extra = {"pickle": "pytorch_model.bin", "pkl": "optimizer.pkl", "py_file": "modeling_x.py"}
    expected = set(_GOOD_FILES)
    if variant in extra:
        expected.add(extra[variant])
    if variant == "no_training_manifest":
        expected.discard("training_manifest.json")
    assert names == expected

    for name in names - {"model.safetensors", "pytorch_model.bin", "optimizer.pkl"}:
        if name.endswith(".json"):
            raw = (run / name).read_bytes()
            assert canonical.is_canonical(raw), name

    header, data_len = _header(run / "model.safetensors")
    spans = _spans(header)
    if variant in ("good", "good_v2") or reason not in ("safetensors_invalid",):
        assert spans[0][0] == 0 and spans[-1][1] == data_len
        assert all(a[1] == b[0] for a, b in zip(spans, spans[1:], strict=False))
        assert len(load_file(str(run / "model.safetensors"))) == 7
    if variant == "bad_header_overlap":
        assert any(a[1] > b[0] for a, b in zip(spans, spans[1:], strict=False))
        assert spans[-1][1] == data_len
    if variant == "bad_header_past_end":
        assert spans[-1][1] > data_len
    if variant == "trailing_bytes":
        assert spans[-1][1] < data_len
    if variant == "bad_dtype":
        assert any(v.get("dtype") == "F128" for k, v in header.items() if k != "__metadata__")

    config = json.loads((run / "config.json").read_bytes())
    assert config["architectures"] == ["MhaiTinyExtractor"]
    assert config["model_type"] == "mhai-tiny"
    assert ("auto_map" in config) == (variant == "auto_map")
    tok = json.loads((run / "tokenizer_config.json").read_bytes())
    assert ("auto_map" in tok) == (variant == "remote_code_tokenizer")
    assert (run / "generation_config.json").is_symlink() == (variant == "symlink")

    if variant != "no_training_manifest":
        tm = _tm(run)
        assert tm["schema"] == "mhai.models.training/1"
        assert tm["role"] == "extract"
        want = {
            "unknown_dataset": fixtures.UNKNOWN_DATASET,
            "non_synthetic_dataset": fixtures.NON_SYNTHETIC_DATASET,
        }.get(variant, fixtures.TRAIN_DATASET)
        assert tm["datasets"] == [want]
        base = tm["base_model"]
        assert (base["licence"] == fixtures.BAD_LICENCE) == (variant == "bad_licence")
        if variant == "unpinned_base":
            assert base["revision"] == "main"
        else:
            assert len(base["revision"]) == 40
            int(base["revision"], 16)


def test_good_v2_has_different_weights(tmp_path: Path) -> None:
    a = fixtures.make_run(tmp_path / "a", "good", 5)
    b = fixtures.make_run(tmp_path / "b", "good_v2", 5)
    assert (a / "model.safetensors").read_bytes() != (b / "model.safetensors").read_bytes()
    for name in ("config.json", "tokenizer.json", "training_manifest.json"):
        assert (a / name).read_bytes() == (b / name).read_bytes()


def test_seed_changes_weights_deterministically(tmp_path: Path) -> None:
    a = fixtures.make_run(tmp_path / "a", "good", 5)
    b = fixtures.make_run(tmp_path / "b", "good", 6)
    c = fixtures.make_run(tmp_path / "c", "good", 5)
    weights = "model.safetensors"
    assert (a / weights).read_bytes() != (b / weights).read_bytes()
    assert (a / weights).read_bytes() == (c / weights).read_bytes()


def test_unknown_variant_or_seed_refused_before_writing(tmp_path: Path) -> None:
    for variant, seed in (("nope", 1), ("good", -1), ("good", 2**32), ("good", True)):
        with pytest.raises(Refused) as err:
            fixtures.make_run(tmp_path / "x", variant, seed)  # type: ignore[arg-type]
        assert err.value.reason == "fixture_name"
    assert not (tmp_path / "x").exists()


def test_existing_dir_not_overwritten(tmp_path: Path) -> None:
    fixtures.make_run(tmp_path / "a", "good", 1)
    with pytest.raises(FileExistsError):
        fixtures.make_run(tmp_path / "a", "good_v2", 1)


def test_name_and_role_go_into_the_training_manifest(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "a", "good", 1, name="other-model", role="ocr")
    tm = _tm(run)
    assert (tm["name"], tm["role"]) == ("other-model", "ocr")
    assert _tm(fixtures.make_run(tmp_path / "b", "good", 1))["name"] == fixtures.MODEL_NAME
    for kw in ({"name": "../x"}, {"role": "Bad Role"}):
        with pytest.raises(Refused) as err:
            fixtures.make_run(tmp_path / "c", "good", 1, **kw)
        assert err.value.reason == "invalid_name"
    assert not (tmp_path / "c").exists()
