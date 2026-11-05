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
        ("config.json", {"custom_pipelines": {"x": {"impl": "pipe.P"}}}),
        ("generation_config.json", {"custom_generate": "org/repo"}),
        ("config.json", {"remote_code": "x"}),
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
    _write(run / "config.json", b"{}")
    index = {"metadata": {"total_size": 4}, "weight_map": {"a": "model-00001-of-00002.safetensors"}}
    path = _write(run / "model.safetensors.index.json", json.dumps(index).encode())
    files.check_json(path, "model.safetensors.index.json")
    assert {e.path for e in files.check_run(run)} == {
        "config.json",
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
    _refused(
        "file_not_allowed",
        files.check_json,
        run / "generation_config.json",
        "generation_config.json",
    )

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
    ceiling = files.Limits(max_header_bytes=files.MAX_HEADER_BYTES)
    for limits in (None, ceiling):
        err = _refused("safetensors_invalid", files.check_safetensors, path, limits)
        assert err.detail_code == "header_length"
    # The operational cap (16 MB) refuses a header the format ceiling would admit.
    assert files.Limits().max_header_bytes == files.DEFAULT_HEADER_BYTES == 16 * MB
    with path.open("wb") as f:
        f.write(struct.pack("<Q", files.DEFAULT_HEADER_BYTES + 1))
        f.truncate(files.DEFAULT_HEADER_BYTES + 64)
    err = _refused("safetensors_invalid", files.check_safetensors, path)
    assert err.detail_code == "header_length"
    err = _refused("safetensors_invalid", files.check_safetensors, path, ceiling)
    assert err.detail_code == "header_json"  # past the length check; zeros aren't JSON
    for bad in (7, files.MAX_HEADER_BYTES + 1):
        with pytest.raises(ValueError):
            files.Limits(max_header_bytes=bad)
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
    _write(run / "config.json", b"{}")
    for i in range(256):
        _write(run / f"model-{i:05d}-of-00256.safetensors", _st({}))
    _refused("too_many_files", files.check_run, run)
    (run / "model-00255-of-00256.safetensors").unlink()
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


# --- review fixes ----------------------------------------------------------


def _chain(exc: BaseException) -> list[BaseException]:
    seen: list[BaseException] = []
    todo: list[BaseException | None] = [exc]
    while todo:
        e = todo.pop()
        if e is None or any(e is s for s in seen):
            continue
        seen.append(e)
        todo += [e.__cause__, e.__context__]
    return seen


def _no_trace_of(err: BaseException, *needles: str) -> None:
    for e in _chain(err):
        texts = [repr(e.args), str(getattr(e, "doc", "")), repr(getattr(e, "object", b""))]
        texts += [str(getattr(e, "filename", "")), str(getattr(e, "filename2", ""))]
        for needle in needles:
            assert all(needle not in t for t in texts), (type(e), needle)


def test_header_rewritten_between_check_and_hash(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The digest covers the checked header bytes, not what is on disk afterwards."""
    run = _good(tmp_path)
    path = run / "model.safetensors"
    original = path.read_bytes()
    (n,) = struct.unpack("<Q", original[:8])
    assert b'"F32"' in original[8 : 8 + n]
    hostile = original[: 8 + n].replace(b'"F32"', b'"F99"', 1) + original[8 + n :]
    real_check = files._check_header

    def check_then_rewrite(header: Any, data_len: int, name: str | None) -> None:
        real_check(header, data_len, name)
        with path.open("r+b") as f:  # same inode, same size: only the header changes
            f.write(hostile[: 8 + n])

    monkeypatch.setattr(files, "_check_header", check_then_rewrite)
    entry = {e.path: e for e in files.check_run(run)}["model.safetensors"]
    assert path.read_bytes() == hostile
    assert entry.sha256 == hashlib.sha256(original).hexdigest()
    assert entry.sha256 != hashlib.sha256(hostile).hexdigest()
    monkeypatch.undo()
    # The rewritten file, checked afresh, is refused.
    _refused("safetensors_invalid", files.check_run, run)


def test_empty_or_incomplete_run_refused(tmp_path: Path) -> None:
    run = tmp_path / "empty"
    run.mkdir()
    assert _refused("missing_file", files.check_run, run).detail_code == "no_weights"
    _write(run / "generation_config.json", b"{}")
    _refused("missing_file", files.check_run, run)
    _write(run / "model.safetensors", _st({}))
    assert _refused("missing_file", files.check_run, run).detail_code == "no_config"
    adapter = tmp_path / "adapter"
    adapter.mkdir()
    _write(adapter / "adapter_config.json", b'{"peft_type":"LORA","r":8}')
    _write(adapter / "adapter_model.safetensors", _st({"a": _tensor("U8", [1], 0, 1)}, b"\x00"))
    assert [e.path for e in files.check_run(adapter)] == [
        "adapter_config.json",
        "adapter_model.safetensors",
    ]


def test_os_errors_become_fixed_refusals(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    secret_dir = tmp_path / "SYN-SECRET-DIR"
    err = _refused("missing_file", files.check_json, secret_dir / "config.json", "config.json")
    assert err.__context__ is None and err.__cause__ is None
    _no_trace_of(err, "SYN-SECRET-DIR")
    _write(tmp_path / "plain", b"")
    err = _refused(
        "file_not_allowed", files.check_safetensors, tmp_path / "plain" / "x.safetensors"
    )
    _no_trace_of(err, str(tmp_path))
    err = _refused("not_found", files.check_run, secret_dir)
    _no_trace_of(err, "SYN-SECRET-DIR")

    # An entry removed between listing and stat.
    run = _good(tmp_path, "vanish")
    real_list = files._list_run
    monkeypatch.setattr(
        files, "_list_run", lambda dfd, n: sorted(real_list(dfd, n) + ["special_tokens_map.json"])
    )
    err = _refused("missing_file", files.check_run, run)
    assert err.file == "special_tokens_map.json"
    _no_trace_of(err, str(tmp_path))


def test_check_json_kind_is_the_file_name(tmp_path: Path) -> None:
    other = _write(tmp_path / "other.json", b'{"auto_map":{}}')
    err = _refused("file_not_allowed", files.check_json, other, "tokenizer.json")
    assert err.detail_code == "json_kind"
    real = tmp_path / "real"
    real.mkdir()
    _write(real / "config.json", b"{}")
    (tmp_path / "linked").symlink_to(real)
    files.check_json(real / "config.json", "config.json")
    _refused(
        "file_not_allowed", files.check_json, tmp_path / "linked" / "config.json", "config.json"
    )


def test_type_checked_before_open(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    fifo = tmp_path / "config.json"
    os.mkfifo(fifo)
    opened: list[str] = []
    real_open = os.open

    def spy(p: Any, *args: Any, **kwargs: Any) -> int:
        opened.append(os.fsdecode(p))
        return real_open(p, *args, **kwargs)

    monkeypatch.setattr(files.os, "open", spy)
    err = _refused("file_not_allowed", files.check_json, fifo, "config.json")
    assert err.detail_code == "special"
    assert "config.json" not in opened
    err = _refused("file_not_allowed", files.check_safetensors, fifo)
    assert "config.json" not in opened


def test_exception_chains_carry_no_contents(tmp_path: Path) -> None:
    marker = "SYN-MARKER-41c2"
    path = _write(tmp_path / "config.json", f'{{"a":"{marker}",}}'.encode())
    err = _refused("json_invalid", files.check_json, path, "config.json")
    _no_trace_of(err, marker)
    with pytest.raises(Refused) as exc:
        canonical.loads_strict(f'{{"a":"{marker}" x}}'.encode(), 1000)
    assert exc.value.__context__ is None and exc.value.__cause__ is None
    with pytest.raises(Refused) as exc:
        canonical.loads_strict(b"\xff" + marker.encode(), 1000)
    _no_trace_of(exc.value, marker)
    st = tmp_path / "model.safetensors"
    _write(st, _st(f'{{"{marker}":1,}}'.encode()))
    err = _refused("safetensors_invalid", files.check_safetensors, st)
    _no_trace_of(err, marker)
