"""Checks on the files of a model run, before anything else reads them.

* :func:`check_file_name`: the allow-list. Weights are safetensors only; pickle
  formats are ``pickle_refused``, code is ``code_refused``, anything else is
  ``file_not_allowed``. Names are flat and match ``^[A-Za-z0-9._-]{1,128}$``.
* :func:`check_json`: strict parse with a size cap; no remote code references.
* :func:`check_safetensors`: our own header parser (not the library), with bounded
  reads. It never maps or loads tensor data.
* :func:`check_run`: every file of a run directory: names, links, special files,
  caps, contents, and a SHA-256 streamed in 8 MB blocks over the same open file and
  in the same pass as the checks, so the hash covers exactly the checked bytes.

Every refusal is a :class:`FileRefused`: ``str()`` is the reason code, ``file`` is
the file's name (only when the name passed the allow-list alphabet, so it is safe to
log) and ``detail_code`` a short fixed code. No file contents are ever included.
"""

import hashlib
import io
import os
import re
import stat
import struct
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

BLOCK_SIZE = 8 * 1024 * 1024
MAX_HEADER_BYTES = 100 * 1024 * 1024  # the format ceiling
DEFAULT_HEADER_BYTES = 16 * 1024 * 1024  # the operational cap (see Limits)
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
# Keys that make transformers fetch or run code from the model's files or hub repo.
_CODE_KEYS = frozenset(
    {
        "auto_map",
        "auto_mapping",
        "trust_remote_code",
        "custom_pipelines",
        "custom_generate",
        "remote_code",
    }
)
_NAME = re.compile(r"[A-Za-z0-9._-]{1,128}", re.ASCII)
_IDENTIFIER = re.compile(r"[A-Za-z_][A-Za-z0-9_]{0,127}", re.ASCII)
_TENSOR_KEYS = frozenset({"dtype", "shape", "data_offsets"})
_CLOEXEC = getattr(os, "O_CLOEXEC", 0)


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
    """Caps for one run. ``max_header_bytes`` bounds the memory one safetensors header
    can take (its bytes, the decoded text and the parsed tree): real headers are well
    under 1 MB even for 70B-parameter shards, so 16 MB leaves a wide margin while
    keeping a worker's peak small. It may be raised up to the format ceiling,
    :data:`MAX_HEADER_BYTES` (100 MB)."""

    max_file_bytes: int = 40 * 1024**3
    max_files: int = 256
    max_total_bytes: int = 200 * 1024**3
    max_header_bytes: int = DEFAULT_HEADER_BYTES

    def __post_init__(self) -> None:
        if not 8 <= self.max_header_bytes <= MAX_HEADER_BYTES:
            raise ValueError("max_header_bytes out of range")


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
#
# Every OSError becomes a FileRefused with a fixed code, raised outside the
# ``except`` block so no exception in the chain carries a path.


def _check_stat(st: os.stat_result, name: str | None) -> None:
    if stat.S_ISLNK(st.st_mode):
        raise FileRefused("file_not_allowed", name, "symlink")
    if not stat.S_ISREG(st.st_mode):
        raise FileRefused("file_not_allowed", name, "special")
    if st.st_nlink > 1:
        raise FileRefused("file_not_allowed", name, "hardlink")


def _os_refusal(exc: OSError, name: str | None, detail: str) -> FileRefused:
    reason = "missing_file" if isinstance(exc, FileNotFoundError) else "file_not_allowed"
    return FileRefused(reason, name, detail)


def _lstat(name: str, dir_fd: int) -> os.stat_result:
    try:
        return os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except OSError as exc:
        refusal = _os_refusal(exc, name, "stat")
    raise refusal


_OPEN_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC


@contextmanager
def _open(
    name: str, dir_fd: int, expect: os.stat_result | None = None
) -> Iterator[tuple[io.FileIO, int]]:
    """Open a regular, singly linked file without following links; yield it and its size.

    The type is checked with ``lstat`` before the open (so no device or FIFO is ever
    opened) and again with ``fstat`` after it, against the same inode and size.
    """
    before = expect if expect is not None else _lstat(name, dir_fd)
    _check_stat(before, name)
    refusal: FileRefused | None = None
    try:
        fd = os.open(name, _OPEN_FLAGS, dir_fd=dir_fd)
    except OSError as exc:  # ELOOP for a symlink swapped in, among others
        refusal = _os_refusal(exc, name, "open")
    if refusal is not None:
        raise refusal
    try:
        st = os.fstat(fd)
        _check_stat(st, name)
        if (st.st_ino, st.st_dev, st.st_size) != (before.st_ino, before.st_dev, before.st_size):
            raise FileRefused("size_mismatch", name, "changed")
        f = os.fdopen(fd, "rb", buffering=0)
    except BaseException:
        os.close(fd)
        raise
    with f:
        yield f, st.st_size


def _read_exact(f: io.FileIO, n: int, name: str | None, reason: str) -> bytes:
    """Read exactly ``n`` bytes into one preallocated buffer."""
    buf = bytearray(n)
    view = memoryview(buf)
    got = 0
    failed = False
    try:
        while got < n:
            k = f.readinto(view[got : got + BLOCK_SIZE])
            if not k:
                raise FileRefused(reason, name, "short_read")
            got += k
    except OSError:
        failed = True
    finally:
        view.release()
    if failed:
        raise FileRefused("file_not_allowed", name, "read")
    return bytes(buf)


def _read_block(f: io.FileIO, name: str) -> bytes:
    try:
        return f.read(BLOCK_SIZE) or b""
    except OSError:
        pass
    raise FileRefused("file_not_allowed", name, "read")


@contextmanager
def _parent(path: Path) -> Iterator[tuple[int, str]]:
    """The directory of ``path`` (its last component must not be a link) and the name."""
    name = path.name
    refusal: FileRefused | None = None
    try:
        dfd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC)
    except OSError as exc:
        refusal = _os_refusal(exc, name, "parent")
    if refusal is not None:
        raise refusal
    try:
        yield dfd, name
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
    reason: str | None = None
    try:
        obj = canonical.loads_strict(raw, _json_cap(kind), allow_floats=True)
    except Refused as exc:
        reason = exc.reason
    if reason is not None:
        raise FileRefused(reason, name, "parse")
    if not isinstance(obj, dict):
        raise FileRefused("json_invalid", name, "not_object")
    if kind in _CODE_CHECKED:
        _code_refs(obj, name)
    if kind == INDEX_JSON:
        _check_index(obj, name, present)


def _listing(dfd: int, name: str) -> frozenset[str]:
    try:
        return frozenset(os.listdir(dfd))
    except OSError as exc:
        refusal = _os_refusal(exc, name, "list")
    raise refusal


def check_json(path: Path | str, kind: str, *, present: frozenset[str] | None = None) -> None:
    """Check one JSON file; ``kind`` is its allow-listed name and must be the file's name.

    For ``model.safetensors.index.json`` the shards it names must be in ``present``
    (default: the names in the file's directory). This is a helper and test entry
    point: :func:`check_run` is what the promotion flow calls.
    """
    path = Path(path)
    if kind not in JSON_FILES or path.name != kind:
        raise FileRefused("file_not_allowed", kind, "json_kind")
    with _parent(path) as (dfd, name), _open(name, dfd) as (f, size):
        if size > _json_cap(kind):
            raise FileRefused("too_large", name)
        raw = _read_exact(f, size, name, "json_invalid")
        if _read_block(f, name):
            raise FileRefused("size_mismatch", name, "grew")
        _check_json_bytes(raw, kind, name, _listing(dfd, name) if present is None else present)


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


def _check_safetensors_file(
    f: io.FileIO, size: int, name: str | None, max_header: int, digest: Any = None
) -> None:
    """Validate the header read from ``f``'s current position (0).

    Each byte read is fed to ``digest`` (when given) as it is read, so the bytes
    validated are exactly the bytes hashed; the caller then streams the data region
    into the same digest from where this stopped, without seeking.
    """
    if size < 8:
        raise _bad(name, "header_length")
    prefix = _read_exact(f, 8, name, "safetensors_invalid")
    if digest is not None:
        digest.update(prefix)
    (n,) = struct.unpack("<Q", prefix)
    if n == 0 or n > max_header or n > size - 8:
        raise _bad(name, "header_length")
    raw = _read_exact(f, n, name, "safetensors_invalid")
    if digest is not None:
        digest.update(raw)
    raw = raw.rstrip(b" ")
    if not raw.startswith(b"{"):
        raise _bad(name, "header_json")
    header: Any = None
    failed = False
    try:
        header = canonical.loads_strict(raw, max_header)
    except Refused:
        failed = True
    del raw
    if failed:
        raise _bad(name, "header_json")
    _check_header(header, size - 8 - n, name)


def check_safetensors(path: Path | str, limits: Limits | None = None) -> None:
    """Validate a safetensors file's header and layout; tensor data is never read."""
    max_header = (limits or Limits()).max_header_bytes
    with _parent(Path(path)) as (dfd, name), _open(name, dfd) as (f, size):
        _check_safetensors_file(f, size, name, max_header)


# --- the whole run ---------------------------------------------------------


def _stream_rest(f: io.FileIO, size: int, done: int, digest: Any, name: str) -> str:
    """Stream from the current position to the end into ``digest``; ``done`` bytes
    were already fed. The total must equal ``size``."""
    total = done
    while chunk := _read_block(f, name):
        total += len(chunk)
        if total > size:
            raise FileRefused("size_mismatch", name, "grew")
        digest.update(chunk)
    if total != size:
        raise FileRefused("size_mismatch", name, "shrank")
    hexdigest: str = digest.hexdigest()
    return hexdigest


def _open_run_dir(root: Path) -> int:
    refusal: Refused
    try:
        return os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC)
    except FileNotFoundError:
        refusal = Refused("not_found")
    except OSError:
        refusal = Refused("file_not_allowed", "run_dir")
    raise refusal


def _list_run(dfd: int, max_files: int) -> list[str]:
    names: list[str] = []
    failed = False
    try:
        with os.scandir(dfd) as it:
            for entry in it:
                names.append(entry.name)
                if len(names) > max_files:
                    break
    except OSError:
        failed = True
    if failed:
        raise FileRefused("file_not_allowed", None, "list")
    if len(names) > max_files:
        raise FileRefused("too_many_files")
    return sorted(names)


def check_run(dir: Path | str, limits: Limits | None = None) -> list[FileEntry]:
    """Check every file of the run in ``dir``; return them sorted by name, with hashes.

    Refuses (fail closed) a missing or linked directory, an entry that isn't on the
    allow-list, a symlink, hard link, directory or special file, more than
    ``max_files`` files, a file over ``max_file_bytes``, a total over
    ``max_total_bytes``, a run without a ``*.safetensors`` file or without
    ``config.json`` or ``adapter_config.json`` (``missing_file``), and any JSON or
    safetensors file that fails its check. Each file is checked and hashed in one
    pass over one open file: the digest covers exactly the bytes that were checked.
    """
    limits = limits or Limits()
    dfd = _open_run_dir(Path(dir))
    try:
        names = _list_run(dfd, limits.max_files)

        stats: dict[str, os.stat_result] = {}
        total = 0
        for name in names:
            check_file_name(name)
            st = _lstat(name, dfd)
            _check_stat(st, name)
            if st.st_size > limits.max_file_bytes:
                raise FileRefused("too_large", name)
            total += st.st_size
            if total > limits.max_total_bytes:
                raise FileRefused("too_large", name, "total")
            stats[name] = st
        if not any(n.endswith(SAFETENSORS_SUFFIX) for n in names):
            raise FileRefused("missing_file", None, "no_weights")
        if not {"config.json", "adapter_config.json"} & set(names):
            raise FileRefused("missing_file", None, "no_config")

        present = frozenset(names)
        out: list[FileEntry] = []
        for name in names:
            with _open(name, dfd, stats[name]) as (f, size):
                if name in JSON_FILES:
                    if size > _json_cap(name):
                        raise FileRefused("too_large", name)
                    # One read: the bytes checked are the bytes hashed.
                    raw = _read_exact(f, size, name, "size_mismatch")
                    if _read_block(f, name):
                        raise FileRefused("size_mismatch", name, "grew")
                    _check_json_bytes(raw, name, name, present)
                    sha = hashlib.sha256(raw).hexdigest()
                else:
                    digest = hashlib.sha256()
                    _check_safetensors_file(f, size, name, limits.max_header_bytes, digest)
                    done = f.tell()
                    sha = _stream_rest(f, size, done, digest, name)
                out.append(FileEntry(name, size, sha))
        return out
    finally:
        os.close(dfd)


__all__ = [
    "ALLOWED",
    "BLOCK_SIZE",
    "CODE_SUFFIXES",
    "DEFAULT_HEADER_BYTES",
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
