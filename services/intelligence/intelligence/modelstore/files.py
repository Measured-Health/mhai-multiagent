"""Checks on the files of a model run, before anything else reads them.

* :func:`check_file_name`: the allow-list. Weights are safetensors only; pickle
  formats are ``pickle_refused``, code is ``code_refused``, anything else is
  ``file_not_allowed``. Names are flat and match ``^[A-Za-z0-9._-]{1,128}$``.
* :func:`check_json`: strict parse with a size cap; no remote code references.
* :func:`check_safetensors`: our own header parser (not the library), with bounded
  reads. It never maps or loads tensor data.
* :func:`check_run`: every file of a run directory: names, links, special files,
  caps, contents, and a SHA-256 streamed in 8 MB blocks.

Every refusal is a :class:`FileRefused`: ``str()`` is the reason code, ``file`` is
the file's name (only when the name passed the allow-list alphabet, so it is safe to
log) and ``detail_code`` a short fixed code. No file contents are ever included.
"""

import hashlib
import os
import re
import stat
import struct
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, BinaryIO

from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

BLOCK_SIZE = 8 * 1024 * 1024
MAX_HEADER_BYTES = 100 * 1024 * 1024
JSON_CAP = 16 * 1024 * 1024
TOKENIZER_JSON_CAP = 64 * 1024 * 1024

INDEX_JSON = "model.safetensors.index.json"
SAFETENSORS_SUFFIX = ".safetensors"
JSON_FILES: frozenset[str] = frozenset(
    {
        INDEX_JSON,
        "config.json",
        "generation_config.json",
        "tokenizer.json",
        "tokenizer_config.json",
        "special_tokens_map.json",
        "preprocessor_config.json",
        "chat_template.json",
        "adapter_config.json",
        "training_manifest.json",
    }
)
# ``*.safetensors`` (shards, adapters) plus the exact JSON names above.
ALLOWED: frozenset[str] = JSON_FILES | {"*" + SAFETENSORS_SUFFIX}

PICKLE_SUFFIXES: frozenset[str] = frozenset(
    {".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle", ".joblib", ".npy", ".npz"}
)
CODE_SUFFIXES: frozenset[str] = frozenset({".py", ".so", ".dll", ".dylib", ".sh", ".ipynb"})

DTYPE_SIZES: dict[str, int] = {
    "F64": 8,
    "F32": 4,
    "F16": 2,
    "BF16": 2,
    "F8_E4M3": 1,
    "F8_E5M2": 1,
    "I64": 8,
    "I32": 4,
    "I16": 2,
    "I8": 1,
    "U8": 1,
    "BOOL": 1,
}

# Files whose keys are model settings, where a module path means remote code. The
# tokenizer's vocabulary, the index's tensor names and the training manifest are
# data whose keys are not settings.
_CODE_CHECKED: frozenset[str] = JSON_FILES - {
    "tokenizer.json",
    INDEX_JSON,
    "training_manifest.json",
}
_CODE_KEYS = frozenset({"auto_map", "auto_mapping", "trust_remote_code"})
_NAME = re.compile(r"[A-Za-z0-9._-]{1,128}", re.ASCII)
_IDENTIFIER = re.compile(r"[A-Za-z_][A-Za-z0-9_]{0,127}", re.ASCII)
_TENSOR_KEYS = frozenset({"dtype", "shape", "data_offsets"})


class FileRefused(Refused):
    """A refused file: ``str()`` is the reason; ``file`` is a safe name or None."""

    def __init__(
        self, reason: str, file: str | None = None, detail_code: str | None = None
    ) -> None:
        super().__init__(reason, detail_code)
        self.file = file if isinstance(file, str) and _NAME.fullmatch(file) else None


@dataclass(frozen=True, slots=True)
class FileEntry:
    path: str
    size: int
    sha256: str


@dataclass(frozen=True, slots=True)
class Limits:
    max_file_bytes: int = 40 * 1024**3
    max_files: int = 256
    max_total_bytes: int = 200 * 1024**3


def _suffix(name: str) -> str:
    dot = name.rfind(".")
    return name[dot:].lower() if dot > 0 else ""


def check_file_name(name: str) -> str:
    """Return ``name`` if the allow-list admits it, else raise :class:`FileRefused`."""
    if not isinstance(name, str) or _NAME.fullmatch(name) is None or name in (".", ".."):
        raise FileRefused("file_not_allowed", None, "name")
    suffix = _suffix(name)
    if suffix in PICKLE_SUFFIXES:
        raise FileRefused("pickle_refused", name)
    if suffix in CODE_SUFFIXES:
        raise FileRefused("code_refused", name)
    if name.startswith("."):
        raise FileRefused("file_not_allowed", name, "hidden")
    if name in JSON_FILES or name.endswith(SAFETENSORS_SUFFIX):
        return name
    raise FileRefused("file_not_allowed", name)


def _json_cap(kind: str) -> int:
    return TOKENIZER_JSON_CAP if kind == "tokenizer.json" else JSON_CAP


# --- opening files safely ----------------------------------------------------


def _check_stat(st: os.stat_result, name: str | None) -> None:
    if stat.S_ISLNK(st.st_mode):
        raise FileRefused("file_not_allowed", name, "symlink")
    if not stat.S_ISREG(st.st_mode):
        raise FileRefused("file_not_allowed", name, "special")
    if st.st_nlink > 1:
        raise FileRefused("file_not_allowed", name, "hardlink")


_OPEN_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0)


@contextmanager
def _open(
    name: str, dir_fd: int | None, expect: os.stat_result | None = None
) -> Iterator[tuple[BinaryIO, int]]:
    """Open a regular, singly linked file without following links; yield it and its size."""
    try:
        fd = os.open(name, _OPEN_FLAGS, dir_fd=dir_fd)
    except FileNotFoundError as exc:
        raise FileRefused("missing_file", name) from exc
    except OSError as exc:  # ELOOP for a symlink, among others
        raise FileRefused("file_not_allowed", name, "open") from exc
    try:
        st = os.fstat(fd)
        _check_stat(st, name)
        if expect is not None and (st.st_ino, st.st_dev, st.st_size) != (
            expect.st_ino,
            expect.st_dev,
            expect.st_size,
        ):
            raise FileRefused("size_mismatch", name, "changed")
        f = os.fdopen(fd, "rb", buffering=0)
    except BaseException:
        os.close(fd)
        raise
    with f:
        yield f, st.st_size


def _read_exact(f: BinaryIO, n: int, name: str | None, reason: str) -> bytes:
    chunks: list[bytes] = []
    left = n
    while left > 0:
        chunk = f.read(min(left, BLOCK_SIZE))
        if not chunk:
            raise FileRefused(reason, name, "short_read")
        chunks.append(chunk)
        left -= len(chunk)
    return b"".join(chunks)


@contextmanager
def _parent(path: Path) -> Iterator[tuple[int, str]]:
    dfd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | getattr(os, "O_CLOEXEC", 0))
    try:
        yield dfd, path.name
    finally:
        os.close(dfd)


# --- JSON ------------------------------------------------------------------


def _code_refs(obj: Any, name: str) -> None:
    stack: list[Any] = [obj]
    while stack:
        item = stack.pop()
        if isinstance(item, list):
            stack.extend(item)
        elif isinstance(item, dict):
            for key, value in item.items():
                if key in _CODE_KEYS:
                    raise FileRefused("code_refused", name, "remote_code_key")
                if key.endswith("_class"):
                    values = value if isinstance(value, list) else [value]
                    for v in values:
                        if v is None:
                            continue
                        if not isinstance(v, str) or _IDENTIFIER.fullmatch(v) is None:
                            raise FileRefused("code_refused", name, "class_path")
                stack.append(value)


def _check_index(obj: dict[str, Any], name: str, present: frozenset[str]) -> None:
    weight_map = obj.get("weight_map")
    metadata = obj.get("metadata", {})
    if not isinstance(weight_map, dict) or not weight_map or not isinstance(metadata, dict):
        raise FileRefused("json_invalid", name, "index")
    if set(obj) - {"weight_map", "metadata"}:
        raise FileRefused("json_invalid", name, "index")
    for shard in set(weight_map.values()):
        if (
            not isinstance(shard, str)
            or _NAME.fullmatch(shard) is None
            or shard.startswith(".")
            or not shard.endswith(SAFETENSORS_SUFFIX)
        ):
            raise FileRefused("json_invalid", name, "index_shard")
        if shard not in present:
            raise FileRefused("missing_file", name, "index_shard")


def _check_json_bytes(raw: bytes, kind: str, name: str, present: frozenset[str]) -> None:
    try:
        obj = canonical.loads_strict(raw, _json_cap(kind), allow_floats=True)
    except Refused as exc:
        raise FileRefused(exc.reason, name, "parse") from None
    if not isinstance(obj, dict):
        raise FileRefused("json_invalid", name, "not_object")
    if kind in _CODE_CHECKED:
        _code_refs(obj, name)
    if kind == INDEX_JSON:
        _check_index(obj, name, present)


def _listing(dfd: int) -> frozenset[str]:
    return frozenset(os.listdir(dfd))


def check_json(path: Path | str, kind: str, *, present: frozenset[str] | None = None) -> None:
    """Check one JSON file of kind ``kind`` (its allow-listed name).

    For ``model.safetensors.index.json`` the shards it names must be in ``present``
    (default: the names in the file's directory).
    """
    if kind not in JSON_FILES:
        raise FileRefused("file_not_allowed", kind, "json_kind")
    with _parent(Path(path)) as (dfd, name), _open(name, dfd) as (f, size):
        if size > _json_cap(kind):
            raise FileRefused("too_large", name)
        raw = _read_exact(f, size, name, "json_invalid")
        if f.read(1):
            raise FileRefused("size_mismatch", name, "grew")
        _check_json_bytes(raw, kind, name, _listing(dfd) if present is None else present)


# --- safetensors -----------------------------------------------------------


def _bad(name: str | None, detail: str) -> FileRefused:
    return FileRefused("safetensors_invalid", name, detail)


def _is_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool)


def _numel_fits(shape: list[Any], limit: int, name: str | None) -> int:
    if not all(_is_int(d) and d >= 0 for d in shape):
        raise _bad(name, "shape")
    if 0 in shape:
        return 0
    n = 1
    for d in shape:
        n *= d
        if n > limit:  # stop early: no huge products from a hostile shape
            raise _bad(name, "size")
    return n


def _check_header(header: Any, data_len: int, name: str | None) -> None:
    if not isinstance(header, dict):
        raise _bad(name, "not_object")
    spans: list[tuple[int, int]] = []
    for key, entry in header.items():
        if key == "__metadata__":
            if not isinstance(entry, dict) or not all(isinstance(v, str) for v in entry.values()):
                raise _bad(name, "metadata")
            continue
        if not key or not isinstance(entry, dict) or set(entry) != _TENSOR_KEYS:
            raise _bad(name, "tensor")
        dtype, shape, offsets = entry["dtype"], entry["shape"], entry["data_offsets"]
        if not isinstance(dtype, str) or dtype not in DTYPE_SIZES:
            raise _bad(name, "dtype")
        if not isinstance(shape, list):
            raise _bad(name, "shape")
        if (
            not isinstance(offsets, list)
            or len(offsets) != 2
            or not all(_is_int(o) for o in offsets)
            or not 0 <= offsets[0] <= offsets[1]
        ):
            raise _bad(name, "offsets")
        itemsize = DTYPE_SIZES[dtype]
        numel = _numel_fits(shape, data_len // itemsize, name)
        if offsets[1] - offsets[0] != numel * itemsize:
            raise _bad(name, "size")
        spans.append((offsets[0], offsets[1]))
    cursor = 0
    for start, end in sorted(spans):
        if start != cursor:  # a gap or an overlap
            raise _bad(name, "offsets")
        cursor = end
    if cursor != data_len:  # past the end, or trailing bytes
        raise _bad(name, "offsets")


def _check_safetensors_file(f: BinaryIO, size: int, name: str | None) -> None:
    if size < 8:
        raise _bad(name, "header_length")
    (n,) = struct.unpack("<Q", _read_exact(f, 8, name, "safetensors_invalid"))
    if n == 0 or n > MAX_HEADER_BYTES or n > size - 8:
        raise _bad(name, "header_length")
    raw = _read_exact(f, n, name, "safetensors_invalid").rstrip(b" ")
    if not raw.startswith(b"{"):
        raise _bad(name, "header_json")
    try:
        header = canonical.loads_strict(raw, MAX_HEADER_BYTES)
    except Refused:
        raise _bad(name, "header_json") from None
    _check_header(header, size - 8 - n, name)


def check_safetensors(path: Path | str) -> None:
    """Validate a safetensors file's header and layout; tensor data is never read."""
    with _parent(Path(path)) as (dfd, name), _open(name, dfd) as (f, size):
        _check_safetensors_file(f, size, name)


# --- the whole run ---------------------------------------------------------


def _hash(f: BinaryIO, size: int, name: str) -> str:
    f.seek(0)
    digest = hashlib.sha256()
    total = 0
    while chunk := f.read(BLOCK_SIZE):
        total += len(chunk)
        if total > size:
            raise FileRefused("size_mismatch", name, "grew")
        digest.update(chunk)
    if total != size:
        raise FileRefused("size_mismatch", name, "shrank")
    return digest.hexdigest()


def check_run(dir: Path | str, limits: Limits | None = None) -> list[FileEntry]:
    """Check every file of the run in ``dir``; return them sorted by name, with hashes.

    Refuses (fail closed) a missing or linked directory, an entry that isn't on the
    allow-list, a symlink, hard link, directory or special file, more than
    ``max_files`` files, a file over ``max_file_bytes``, a total over
    ``max_total_bytes``, and any JSON or safetensors file that fails its check.
    """
    limits = limits or Limits()
    root = Path(dir)
    try:
        dfd = os.open(
            root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        )
    except FileNotFoundError as exc:
        raise Refused("not_found") from exc
    except OSError as exc:
        raise Refused("file_not_allowed", "run_dir") from exc
    try:
        names: list[str] = []
        with os.scandir(dfd) as it:
            for entry in it:
                names.append(entry.name)
                if len(names) > limits.max_files:
                    raise FileRefused("too_many_files")
        names.sort()

        stats: dict[str, os.stat_result] = {}
        total = 0
        for name in names:
            check_file_name(name)
            st = os.stat(name, dir_fd=dfd, follow_symlinks=False)
            _check_stat(st, name)
            if st.st_size > limits.max_file_bytes:
                raise FileRefused("too_large", name)
            total += st.st_size
            if total > limits.max_total_bytes:
                raise FileRefused("too_large", name, "total")
            stats[name] = st

        present = frozenset(names)
        out: list[FileEntry] = []
        for name in names:
            with _open(name, dfd, stats[name]) as (f, size):
                if name in JSON_FILES:
                    if size > _json_cap(name):
                        raise FileRefused("too_large", name)
                    # One read: the bytes checked are the bytes hashed.
                    raw = _read_exact(f, size, name, "size_mismatch")
                    if f.read(1):
                        raise FileRefused("size_mismatch", name, "grew")
                    _check_json_bytes(raw, name, name, present)
                    digest = hashlib.sha256(raw).hexdigest()
                else:
                    _check_safetensors_file(f, size, name)
                    digest = _hash(f, size, name)
                out.append(FileEntry(name, size, digest))
        return out
    finally:
        os.close(dfd)


__all__ = [
    "ALLOWED",
    "BLOCK_SIZE",
    "CODE_SUFFIXES",
    "DTYPE_SIZES",
    "JSON_FILES",
    "MAX_HEADER_BYTES",
    "PICKLE_SUFFIXES",
    "FileEntry",
    "FileRefused",
    "Limits",
    "check_file_name",
    "check_json",
    "check_run",
    "check_safetensors",
]
