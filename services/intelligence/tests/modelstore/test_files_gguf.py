"""GGUF in the run allow-list, behind our own bounded header check (own model PR 1)."""

import hashlib
import struct
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore import files, fixtures
from intelligence.modelstore.reasons import Refused
from tests.modelstore.fakes import gguf as g

GGUF_NAME = "model-q4_k_m.gguf"


def _code(fn: Any, *args: Any) -> tuple[str, str | None]:
    with pytest.raises(Refused) as err:
        fn(*args)
    return err.value.reason, err.value.detail_code


def _file(tmp_path: Path, raw: bytes) -> Path:
    path = tmp_path / GGUF_NAME
    path.write_bytes(raw)
    return path


def _run_with(tmp_path: Path, raw: bytes) -> Path:
    run = fixtures.make_run(tmp_path / "run", "good", 5)
    (run / GGUF_NAME).write_bytes(raw)
    return run


def test_gguf_name_allowed() -> None:
    assert files.check_file_name(GGUF_NAME) == GGUF_NAME
    assert "*.gguf" in files.ALLOWED


def test_valid_gguf_passes_and_is_hashed_in_the_run(tmp_path: Path) -> None:
    raw = g.tiny()
    files.check_gguf(_file(tmp_path, raw))
    run = _run_with(tmp_path, raw)
    entries = {e.path: e for e in files.check_run(run)}
    assert entries[GGUF_NAME].size == len(raw)
    assert entries[GGUF_NAME].sha256 == hashlib.sha256(raw).hexdigest()


@pytest.mark.parametrize("arch", sorted(files.GGUF_ARCHITECTURES))
def test_each_listed_architecture_passes(tmp_path: Path, arch: str) -> None:
    files.check_gguf(_file(tmp_path, g.tiny(arch.encode("ascii"))))


def _long_template() -> list[bytes]:
    kvs = g.tiny_kvs()
    kvs[1] = g.kv_string(b"tokenizer.chat_template", b"x" * (1024 * 1024))
    return kvs


@pytest.mark.parametrize(
    ("raw", "detail"),
    [
        (g.tiny()[:20], "truncated"),
        (b"GGUF", "truncated"),
        (g.gguf(g.tiny_kvs(), g.TINY_INFOS, g.TINY_DATA, magic=b"GGML"), "magic"),
        (g.gguf(g.tiny_kvs(), g.TINY_INFOS, g.TINY_DATA, version=2), "version"),
        (g.gguf(_long_template(), g.TINY_INFOS, g.TINY_DATA), "string"),
        (g.tiny(b"gpt2"), "architecture"),
        (g.gguf(g.tiny_kvs()[1:], g.TINY_INFOS, g.TINY_DATA), "architecture"),
        (
            g.gguf(g.tiny_kvs(), [g.TINY_INFOS[0], g.tensor(b"o", [32], g.Q8_0, 64)], g.TINY_DATA),
            "offsets",
        ),
        (
            g.gguf(g.tiny_kvs(), [g.TINY_INFOS[0], g.tensor(b"o", [32], g.Q8_0, 33)], g.TINY_DATA),
            "offsets",
        ),
        (g.gguf(g.tiny_kvs(), [g.tensor(b"t", [4], 99, 0)], bytes(32)), "tensor_type"),
        (g.gguf(g.tiny_kvs(), [g.tensor(b"t", [1, 1, 1, 1, 1], g.F32, 0)], bytes(32)), "tensor"),
        (g.gguf(g.tiny_kvs(), g.TINY_INFOS + g.TINY_INFOS[:1], g.TINY_DATA), "tensor_duplicate"),
        (g.gguf(g.tiny_kvs(), [], b"", n_tensors=10**9), "counts"),
        (g.gguf(g.tiny_kvs(), [], b"", n_kv=10**9), "counts"),
        (g.gguf([*g.tiny_kvs(), g.kv_string("é".encode(), b"x")], [], b""), "key"),
        (g.gguf([*g.tiny_kvs(), g.kv_string(b"k" * 256, b"x")], [], b""), "key"),
        (g.gguf([*g.tiny_kvs(), g.tiny_kvs()[0]], [], b""), "duplicate_key"),
        (
            g.gguf([*g.tiny_kvs(), g.kv_array(b"nested", g.ARRAY, [b""])], [], b""),
            "array",
        ),
        (g.gguf([*g.tiny_kvs(), g.string(b"bad.type") + struct.pack("<I", 77)], [], b""), "type"),
        (g.gguf([*g.tiny_kvs(), g.kv_u32(b"general.alignment", 3)], [], b""), "alignment"),
    ],
)
def test_malformed_gguf_refused(tmp_path: Path, raw: bytes, detail: str) -> None:
    assert _code(files.check_gguf, _file(tmp_path, raw)) == ("gguf_invalid", detail)
    assert _code(files.check_run, _run_with(tmp_path, raw)) == ("gguf_invalid", detail)


def test_two_gguf_files_refused(tmp_path: Path) -> None:
    run = _run_with(tmp_path, g.tiny())
    (run / "model-q8_0.gguf").write_bytes(g.tiny())
    assert _code(files.check_run, run) == ("gguf_invalid", "count")


def test_gguf_without_safetensors_refused(tmp_path: Path) -> None:
    run = _run_with(tmp_path, g.tiny())
    (run / "model.safetensors").unlink()
    assert _code(files.check_run, run) == ("missing_file", "no_weights")


def test_gguf_chat_template_read_from_the_header(tmp_path: Path) -> None:
    assert files.gguf_chat_template(_file(tmp_path, g.tiny())) == g.TINY_TEMPLATE
    no_template = g.gguf([g.tiny_kvs()[0]], g.TINY_INFOS, g.TINY_DATA)
    assert files.gguf_chat_template(_file(tmp_path, no_template)) is None


def test_gguf_over_the_weights_cap_refused(tmp_path: Path) -> None:
    run = _run_with(tmp_path, g.tiny())
    limits = files.Limits(max_file_bytes=len(g.tiny()) - 1)
    with pytest.raises(Refused) as err:
        files.check_run(run, limits)
    assert err.value.reason == "too_large"
