"""Model file checks: the allow-list, JSON caps, safetensors headers, SHA-256."""

import dataclasses
import hashlib
import json
import os
import struct
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore import canonical, files, fixtures
from intelligence.modelstore.reasons import Refused

GB = 1024**3
MB = 1024**2


def _st(header: bytes | dict[str, Any], data: bytes = b"", *, pad: bool = True) -> bytes:
    """A safetensors file built byte by byte: u64 LE length, header, data."""
    raw = header if isinstance(header, bytes) else json.dumps(header).encode("ascii")
    if pad:
        raw += b" " * (-len(raw) % 8)
    return struct.pack("<Q", len(raw)) + raw + data


def _tensor(dtype: str, shape: list[int], start: int, end: int) -> dict[str, Any]:
    return {"dtype": dtype, "shape": shape, "data_offsets": [start, end]}


def _write(path: Path, raw: bytes) -> Path:
    path.write_bytes(raw)
    return path


def _refused(reason: str, fn: Any, *args: Any, **kwargs: Any) -> Refused:
    with pytest.raises(Refused) as err:
        fn(*args, **kwargs)
    assert err.value.reason == reason, (err.value.reason, err.value.detail_code)
    return err.value


def _good(tmp_path: Path, name: str = "run") -> Path:
    return fixtures.make_run(tmp_path / name, "good", 7)


# --- the allow-list (B20) ---------------------------------------------------


@pytest.mark.parametrize("variant", ["pickle", "pkl", "py_file"])
def test_pickle_and_code_refused(tmp_path: Path, variant: str) -> None:
    run = fixtures.make_run(tmp_path / "run", variant, 3)
    reason = fixtures.EXPECTED_REASONS[variant]
    assert reason is not None
    err = _refused(reason, files.check_run, run)
    assert err.file in {"pytorch_model.bin", "optimizer.pkl", "modeling_x.py"}


@pytest.mark.parametrize(
    ("name", "reason"),
    [
        *[
            (f"weights{ext}", "pickle_refused")
            for ext in (".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle", ".joblib", ".npy")
        ],
        ("arrays.npz", "pickle_refused"),
        ("MODEL.BIN", "pickle_refused"),
        *[
            (f"thing{ext}", "code_refused")
            for ext in (".py", ".so", ".dll", ".dylib", ".sh", ".ipynb")
        ],
        ("tokenizer.model", "file_not_allowed"),
        ("model.gguf", "file_not_allowed"),
        ("model.onnx", "file_not_allowed"),
        ("weights.zip", "file_not_allowed"),
        ("weights.tar.gz", "file_not_allowed"),
        (".gitattributes", "file_not_allowed"),
        (".model.safetensors", "file_not_allowed"),
        ("README.md", "file_not_allowed"),
        ("vocab.json", "file_not_allowed"),
        ("Config.json", "file_not_allowed"),
        (".safetensors", "file_not_allowed"),
        ("sub/config.json", "file_not_allowed"),
        ("..", "file_not_allowed"),
        ("é.safetensors", "file_not_allowed"),
        ("a" * 129, "file_not_allowed"),
        ("", "file_not_allowed"),
        ("config.json\n", "file_not_allowed"),
    ],
)
def test_allow_list_refusals(name: str, reason: str) -> None:
    _refused(reason, files.check_file_name, name)


@pytest.mark.parametrize(
    "name",
    [
        "model.safetensors",
        "model-00001-of-00003.safetensors",
        "adapter_model.safetensors",
        "model.safetensors.index.json",
        "config.json",
        "generation_config.json",
        "tokenizer.json",
        "tokenizer_config.json",
        "special_tokens_map.json",
        "preprocessor_config.json",
        "chat_template.json",
        "adapter_config.json",
        "training_manifest.json",
    ],
)
def test_allow_list_accepts(name: str) -> None:
    assert files.check_file_name(name) == name


# --- JSON (P6) -------------------------------------------------------------


@pytest.mark.parametrize("variant", ["auto_map", "remote_code_tokenizer"])
def test_auto_map_and_remote_code_refused(tmp_path: Path, variant: str) -> None:
    run = fixtures.make_run(tmp_path / "run", variant, 3)
    err = _refused("code_refused", files.check_run, run)
    assert err.file == ("config.json" if variant == "auto_map" else "tokenizer_config.json")


@pytest.mark.parametrize(
    ("kind", "obj"),
    [
        ("config.json", {"trust_remote_code": False}),
        ("config.json", {"text_config": {"auto_map": {"AutoModel": "m.M"}}}),
        ("tokenizer_config.json", {"tokenizer_class": "tokenization_x.Tok"}),
        ("tokenizer_config.json", {"tokenizer_class": "org/repo--tok.Tok"}),
        ("preprocessor_config.json", {"processor_class": ["proc.P", None]}),
        ("config.json", {"model_class": {"a": "b"}}),
        ("adapter_config.json", {"auto_mapping": {"base_model_class": "x.Y"}}),
    ],
)
def test_code_references_in_json_refused(tmp_path: Path, kind: str, obj: Any) -> None:
    path = _write(tmp_path / kind, json.dumps(obj).encode())
    _refused("code_refused", files.check_json, path, kind)


def test_json_plain_class_names_and_floats_pass(tmp_path: Path) -> None:
    cfg = {"tokenizer_class": "PreTrainedTokenizerFast", "processor_class": None, "eps": 1e-06}
    path = _write(tmp_path / "tokenizer_config.json", json.dumps(cfg).encode())
    files.check_json(path, "tokenizer_config.json")
    # tokenizer.json is vocabulary: a token spelled like a refused key is just a token.
    tok = {"model": {"vocab": {"auto_map": 0, "trust_remote_code": 1}}, "scores": [0.5]}
    files.check_json(
        _write(tmp_path / "tokenizer.json", json.dumps(tok).encode()), "tokenizer.json"
    )


@pytest.mark.parametrize(
    "raw",
    [
        b'{"a":1,"a":2}',
        b'{"a":NaN}',
        b'{"a":1e999}',
        b"\xef\xbb\xbf{}",
        b"[1]",
        b'{"a":1}x',
        b"",
    ],
)
def test_json_strict(tmp_path: Path, raw: bytes) -> None:
    _refused("json_invalid", files.check_json, _write(tmp_path / "config.json", raw), "config.json")


def test_json_unknown_kind_refused(tmp_path: Path) -> None:
    _refused("file_not_allowed", files.check_json, _write(tmp_path / "x.json", b"{}"), "vocab.json")


def test_index_names_only_present_shards(tmp_path: Path) -> None:
    run = tmp_path / "run"
    run.mkdir()
    shard = _st({"a": _tensor("U8", [4], 0, 4)}, b"\x01\x02\x03\x04")
    _write(run / "model-00001-of-00002.safetensors", shard)
    index = {"metadata": {"total_size": 4}, "weight_map": {"a": "model-00001-of-00002.safetensors"}}
    path = _write(run / "model.safetensors.index.json", json.dumps(index).encode())
    files.check_json(path, "model.safetensors.index.json")
    assert {e.path for e in files.check_run(run)} == {
        "model-00001-of-00002.safetensors",
        "model.safetensors.index.json",
    }
    index["weight_map"]["b"] = "model-00002-of-00002.safetensors"
    _write(path, json.dumps(index).encode())
    _refused("missing_file", files.check_json, path, "model.safetensors.index.json")
    _refused("missing_file", files.check_run, run)
    for bad in ({"weight_map": {"a": "../x.safetensors"}}, {"weight_map": {"a": "config.json"}}):
        _write(path, json.dumps(bad).encode())
        _refused("json_invalid", files.check_json, path, "model.safetensors.index.json")
    _write(path, b'{"metadata":{}}')
    _refused("json_invalid", files.check_json, path, "model.safetensors.index.json")


# --- links and special files (P1) ------------------------------------------


def test_symlink_and_hardlink_refused(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "sym", "symlink", 3)
    assert _refused("file_not_allowed", files.check_run, run).file == "generation_config.json"
    _refused("file_not_allowed", files.check_json, run / "generation_config.json", "config.json")

    run = _good(tmp_path, "hard")
    outside = _write(tmp_path / "outside.json", b"{}")
    os.link(outside, run / "special_tokens_map.json")
    err = _refused("file_not_allowed", files.check_run, run)
    assert (err.file, err.detail_code) == ("special_tokens_map.json", "hardlink")

    run = _good(tmp_path, "fifo")
    os.mkfifo(run / "chat_template.json")
    err = _refused("file_not_allowed", files.check_run, run)
    assert (err.file, err.detail_code) == ("chat_template.json", "special")

    run = _good(tmp_path, "subdir")
    (run / "adapter_config.json").mkdir()
    _refused("file_not_allowed", files.check_run, run)

    link = tmp_path / "link"
    link.symlink_to(_good(tmp_path, "target"))
    _refused("file_not_allowed", files.check_run, link)
    _refused("not_found", files.check_run, tmp_path / "missing")


def test_symlink_safetensors_refused(tmp_path: Path) -> None:
    run = _good(tmp_path)
    os.rename(run / "model.safetensors", tmp_path / "real.safetensors")
    os.symlink(tmp_path / "real.safetensors", run / "model.safetensors")
    _refused("file_not_allowed", files.check_safetensors, run / "model.safetensors")
    _refused("file_not_allowed", files.check_run, run)


# --- safetensors headers (B21) ---------------------------------------------


@pytest.mark.parametrize("variant", ["bad_header_overlap", "bad_header_past_end", "trailing_bytes"])
def test_header_overlap_past_end_gap_trailing_refused(tmp_path: Path, variant: str) -> None:
    run = fixtures.make_run(tmp_path / "run", variant, 3)
    err = _refused("safetensors_invalid", files.check_run, run)
    assert err.file == "model.safetensors"
    _refused("safetensors_invalid", files.check_safetensors, run / "model.safetensors")


def test_header_gap_refused(tmp_path: Path) -> None:
    header = {"a": _tensor("U8", [2], 0, 2), "b": _tensor("U8", [2], 4, 6)}
    path = _write(tmp_path / "model.safetensors", _st(header, b"\x00" * 6))
    assert _refused("safetensors_invalid", files.check_safetensors, path).detail_code == "offsets"
    header = {"a": _tensor("U8", [2], 2, 4)}
    path = _write(tmp_path / "model.safetensors", _st(header, b"\x00" * 4))
    _refused("safetensors_invalid", files.check_safetensors, path)


def test_bad_dtype_and_size_mismatch_refused(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "bad_dtype", 3)
    _refused("safetensors_invalid", files.check_run, run)
    cases: list[dict[str, Any]] = [
        {"a": _tensor("F32", [2, 2], 0, 12)},
        {"a": _tensor("F32", [2, -2], 0, 16)},
        {"a": _tensor("F32", [True, 4], 0, 16)},
        {"a": _tensor("F32", [4], 0, 16) | {"extra": 1}},
        {"a": {"dtype": "F32", "shape": [4]}},
        {"a": _tensor("F32", [4], 16, 0)},
        {"a": _tensor("f32", [4], 0, 16)},
        {"a": _tensor("F32", [4], 0, 16), "__metadata__": {"k": 1}},
        {"a": _tensor("F32", [4], 0, 16), "__metadata__": ["k"]},
        {"a": {"dtype": "F32", "shape": [4], "data_offsets": [0, 8, 16]}},
        {"": _tensor("F32", [4], 0, 16)},
        {"a": _tensor("F32", [2**40, 2**40, 2**40], 0, 16)},
    ]
    for header in cases:
        path = _write(tmp_path / "model.safetensors", _st(header, b"\x00" * 16))
        _refused("safetensors_invalid", files.check_safetensors, path)
    for dtype, size in files.DTYPE_SIZES.items():
        header = {"a": _tensor(dtype, [3], 0, 3 * size), "__metadata__": {"format": "np"}}
        path = _write(tmp_path / "model.safetensors", _st(header, b"\x00" * 3 * size))
        files.check_safetensors(path)


def test_duplicate_tensor_name_refused(tmp_path: Path) -> None:
    raw = (
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[0,1]},'
        b'"a":{"dtype":"U8","shape":[1],"data_offsets":[1,2]}}'
    )
    path = _write(tmp_path / "model.safetensors", _st(raw, b"\x00\x00"))
    _refused("safetensors_invalid", files.check_safetensors, path)


@pytest.mark.parametrize(
    "raw",
    [
        b"",
        b"\x08\x00\x00",
        struct.pack("<Q", 0),
        struct.pack("<Q", 40) + b"{}",
        _st(b"[]"),
        _st(b' {"a":{"dtype":"U8","shape":[1],"data_offsets":[0,1]}}', b"\x00"),
        _st(b'{"a":{"dtype":"U8","shape":[1.0],"data_offsets":[0,1]}}', b"\x00"),
        _st(b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[0,1]}}\x00', b"\x00", pad=False),
        _st(b"\xff\xfe", b""),
    ],
)
def test_header_malformed_refused(tmp_path: Path, raw: bytes) -> None:
    path = _write(tmp_path / "model.safetensors", raw)
    _refused("safetensors_invalid", files.check_safetensors, path)


def test_header_length_cap(tmp_path: Path) -> None:
    # Sparse: the file is long enough for the claimed header, so only the cap refuses it.
    path = tmp_path / "model.safetensors"
    with path.open("wb") as f:
        f.write(struct.pack("<Q", files.MAX_HEADER_BYTES + 1))
        f.truncate(files.MAX_HEADER_BYTES + 64)
    err = _refused("safetensors_invalid", files.check_safetensors, path)
    assert err.detail_code == "header_length"
    path.write_bytes(struct.pack("<Q", 64) + b"{}" + b" " * 6)
    err = _refused("safetensors_invalid", files.check_safetensors, path)
    assert err.detail_code == "header_length"


def test_empty_tensors_and_unpadded_header_pass(tmp_path: Path) -> None:
    header = {"z": _tensor("F32", [0, 3], 0, 0), "a": _tensor("U8", [2], 0, 2)}
    path = _write(tmp_path / "model.safetensors", _st(header, b"\x00\x00", pad=False))
    files.check_safetensors(path)


# --- the whole run ---------------------------------------------------------


@pytest.mark.parametrize("variant", ["good", "good_v2"])
def test_good_run_passes_with_hashes(tmp_path: Path, variant: str) -> None:
    run = fixtures.make_run(tmp_path / "run", variant, 7)
    entries = files.check_run(run)
    assert [e.path for e in entries] == sorted(p.name for p in run.iterdir())
    for entry in entries:
        raw = (run / entry.path).read_bytes()
        assert entry.size == len(raw)
        assert entry.sha256 == hashlib.sha256(raw).hexdigest()
    assert files.BLOCK_SIZE == 8 * MB


def test_large_file_hash_is_streamed(tmp_path: Path) -> None:
    # A sparse 20 MB tensor: more than two blocks, so the hash crosses block boundaries.
    run = _good(tmp_path)
    n = 20 * MB + 3
    path = run / "adapter_model.safetensors"
    head = _st({"a": _tensor("U8", [n], 0, n)})
    with path.open("wb") as f:
        f.write(head)
        f.truncate(len(head) + n)
    entries = {e.path: e for e in files.check_run(run)}
    assert entries[path.name].size == len(head) + n
    assert entries[path.name].sha256 == hashlib.sha256(path.read_bytes()).hexdigest()


def test_limits(tmp_path: Path) -> None:
    run = tmp_path / "many"
    run.mkdir()
    for i in range(257):
        _write(run / f"model-{i:05d}-of-00257.safetensors", _st({}))
    _refused("too_many_files", files.check_run, run)
    (run / "model-00256-of-00257.safetensors").unlink()
    assert len(files.check_run(run)) == 256

    run = _good(tmp_path, "big")
    with (run / "adapter_model.safetensors").open("wb") as f:
        f.truncate(40 * GB + 1)
    err = _refused("too_large", files.check_run, run)
    assert err.file == "adapter_model.safetensors"
    # Past the caps, the sparse file's all-zero header is refused by a bounded read.
    big = files.Limits(max_file_bytes=41 * GB, max_total_bytes=41 * GB)
    _refused("safetensors_invalid", files.check_run, run, big)

    run = _good(tmp_path, "total")
    with pytest.raises(Refused) as exc:
        files.check_run(run, files.Limits(max_total_bytes=100))
    assert exc.value.reason == "too_large"


def test_json_size_caps(tmp_path: Path) -> None:
    path = tmp_path / "config.json"
    with path.open("wb") as f:
        f.truncate(16 * MB + 1)
    _refused("too_large", files.check_json, path, "config.json")
    body = b'{"a":"' + b"x" * (17 * MB) + b'"}'
    tok = _write(tmp_path / "tokenizer.json", body)
    files.check_json(tok, "tokenizer.json")
    _refused("too_large", files.check_json, _write(tmp_path / "config.json", body), "config.json")
    with tok.open("wb") as f:
        f.truncate(64 * MB + 1)
    _refused("too_large", files.check_json, tok, "tokenizer.json")


def test_refusals_name_the_file_not_its_contents(tmp_path: Path) -> None:
    run = _good(tmp_path)
    marker = "SYN-MARKER-7f3a"
    _write(run / "config.json", json.dumps({"auto_map": marker}).encode())
    err = _refused("code_refused", files.check_run, run)
    assert str(err) == "code_refused"
    assert err.file == "config.json"
    assert marker not in repr(err.args) and marker not in str(err.detail_code)

    # A name outside the allowed alphabet is never echoed back.
    run = _good(tmp_path, "hostile")
    _write(run / "x\x1b[31m.bin", b"")
    err = _refused("file_not_allowed", files.check_run, run)
    assert err.file is None


def test_entries_are_plain_values(tmp_path: Path) -> None:
    entry = files.check_run(_good(tmp_path))[0]
    assert isinstance(entry, files.FileEntry)
    assert [f.name for f in dataclasses.fields(entry)] == ["path", "size", "sha256"]


def test_canonical_floats_only_when_asked() -> None:
    # Anything hashed or signed keeps refusing floats; model config files may carry them.
    _refused("json_invalid", canonical.loads_strict, b'{"a":1.5}', 100)
    assert canonical.loads_strict(b'{"a":1.5}', 100, allow_floats=True) == {"a": 1.5}
    for raw in (b'{"a":1e999}', b'{"a":NaN}', b'{"a":-Infinity}'):
        _refused("json_invalid", canonical.loads_strict, raw, 100, allow_floats=True)
