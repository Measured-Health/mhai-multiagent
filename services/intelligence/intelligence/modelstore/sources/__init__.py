"""Run sources: where a model run's files come from before promotion.

* :mod:`.local`: ``LocalSource(root)`` reads ``root/models/<name>/<run>/`` (the intelligence
  lead's dry runs and CI);
* :mod:`.drive`: ``DriveSource(api, folder_id)`` walks ``MHAI models/models/<name>/<run>/``
  in Google Drive by folder id; ``drive_check`` is B22;
* :mod:`.hf`: ``HFSource(repo, revision, cache_dir=...)`` reads a Hugging Face repo at a
  pinned commit.

A source lists a run (:class:`RemoteFile`, allow-listed names only, by
:func:`intelligence.modelstore.files.check_file_name`) and fetches one file into the scratch
directory the caller made with :func:`scratch`. The fetch streams to disk, counts bytes,
and hashes in the same pass; on any failure (an overrun, a short read, a checksum
mismatch, an error) the partial file is removed. A source's checks are a first wall:
the promote step (Task 19) still runs :func:`intelligence.modelstore.files.check_run` on
the scratch run directory after the fetch, and that is what decides what is promoted.

Refusals are fixed codes: ``source_invalid`` (the source holds something we refuse),
``source_unavailable`` (the source can't be reached or read) and ``drive_scope`` (B22),
each with a short fixed ``detail_code``. No refusal carries a path, a URL, a token or a
response body, and none is chained to the exception that caused it.
"""

from __future__ import annotations

import hashlib
import logging
import os
import shutil
import stat
import tempfile
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

from intelligence.modelstore import files
from intelligence.modelstore.reasons import Refused

log = logging.getLogger(__name__)

_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
_WRITE_FLAGS = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | _CLOEXEC


@dataclass(frozen=True, slots=True)
class RemoteFile:
    """One listed file of a run. ``name`` passed the allow-list; ``size`` is what the
    source reported; ``checksum_sha256``/``checksum_md5`` are the source's own checksums
    (lowercase hex) when it has them; ``ref`` is the source's handle (a Drive file id, a
    path in a Hugging Face repo, a local file name)."""

    name: str
    size: int
    checksum_sha256: str | None
    checksum_md5: str | None
    ref: str


class Source(Protocol):
    def list_run(self, name: str, run: str) -> list[RemoteFile]: ...

    def fetch(self, file: RemoteFile, dest: Path, max_bytes: int) -> str:
        """Write ``file`` to ``dest`` (created new) and return its SHA-256 (hex)."""
        ...


def invalid(detail: str) -> Refused:
    return Refused("source_invalid", detail_code=detail)


def unavailable(detail: str) -> Refused:
    return Refused("source_unavailable", detail_code=detail)


def allowed_name(name: object) -> str:
    """``name`` if Task 13's allow-list admits it, else ``source_invalid/name``. Raised
    outside the handler: the original refusal names the file."""
    ok = False
    if isinstance(name, str):
        try:
            files.check_file_name(name)
            ok = True
        except Refused:
            pass
    if not ok or not isinstance(name, str):
        raise invalid("name")
    return name


def check_listing(found: list[RemoteFile], limits: files.Limits) -> list[RemoteFile]:
    """Caps on a listed run, before anything is downloaded: the file count, each reported
    size and the total (Task 13's limits), and no two files with one name."""
    if len(found) > limits.max_files:
        raise invalid("count")
    names = [f.name for f in found]
    if len(set(names)) != len(names):
        raise invalid("duplicate")
    total = 0
    for f in found:
        if f.size > limits.max_file_bytes:
            raise invalid("size")
        total += f.size
        if total > limits.max_total_bytes:
            raise invalid("total")
    return sorted(found, key=lambda f: f.name)


class Sink:
    """A file being written: counts bytes against ``limit`` and hashes them as they come.

    SHA-256 is always computed: it is our identity hash (Task 13). MD5 is computed only
    when the source has nothing better to compare with (Drive files with only
    ``md5Checksum``), and only for that comparison; it is never stored or trusted beyond it.
    """

    def __init__(self, fd: int, limit: int, *, md5: bool = False, git_size: int | None = None):
        self._fd = fd
        self._limit = limit
        self.count = 0
        self.sha256 = hashlib.sha256()
        self.md5: Any = hashlib.md5(usedforsecurity=False) if md5 else None
        # A git blob id (Hugging Face's id for a non-LFS file): SHA-1 over a header and the
        # bytes. Like MD5, only compared with the source's own value.
        self.git: Any = None
        if git_size is not None:
            self.git = hashlib.sha1(usedforsecurity=False)
            self.git.update(b"blob %d\0" % git_size)

    def write(self, chunk: bytes) -> None:
        if not isinstance(chunk, (bytes, bytearray, memoryview)):
            raise invalid("chunk")
        self.count += len(chunk)
        if self.count > self._limit:
            raise invalid("overrun")
        self.sha256.update(chunk)
        if self.md5 is not None:
            self.md5.update(chunk)
        if self.git is not None:
            self.git.update(chunk)
        view = memoryview(chunk)
        failed = False
        try:
            while view:
                n = os.write(self._fd, view)
                view = view[n:]
        except OSError:
            failed = True
        if failed:
            raise invalid("write")


def stream_to(
    file: RemoteFile,
    dest: Path,
    max_bytes: int,
    feed: Callable[[Sink], None],
    *,
    git_oid: str | None = None,
) -> str:
    """Create ``dest`` (new, 0600, no link followed), let ``feed`` write ``file``'s bytes
    to a :class:`Sink`, then check the count and the source's checksums. Returns the
    SHA-256. On any failure the partial file is removed."""
    allowed_name(file.name)
    if Path(dest).name != file.name:
        raise invalid("dest")
    if not isinstance(max_bytes, int) or file.size > max_bytes:
        raise invalid("size")
    sha_expected, md5_expected = file.checksum_sha256, file.checksum_md5
    fd = -1
    try:
        fd = os.open(dest, _WRITE_FLAGS, 0o600)
    except OSError:
        pass
    if fd < 0:
        raise invalid("dest")
    ok = False
    try:
        sink = Sink(
            fd,
            file.size,
            md5=sha_expected is None and md5_expected is not None,
            git_size=file.size if git_oid is not None else None,
        )
        feed(sink)
        if sink.count != file.size:
            raise invalid("short")
        sha = sink.sha256.hexdigest()
        if sha_expected is not None and sha != sha_expected:
            raise invalid("checksum")
        if sha_expected is None and md5_expected is not None:
            if sink.md5 is None or sink.md5.hexdigest() != md5_expected:
                raise invalid("checksum")
        if git_oid is not None and (sink.git is None or sink.git.hexdigest() != git_oid):
            raise invalid("checksum")
        synced = False
        try:
            os.fsync(fd)
            synced = True
        except OSError:
            pass
        if not synced:
            raise invalid("write")
        ok = True
        return sha
    finally:
        os.close(fd)
        if not ok:
            _unlink(dest)


def _unlink(path: Path) -> None:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    except OSError:
        log.warning("source partial_file_not_removed")


@dataclass(frozen=True, slots=True)
class Scratch:
    """A promote job's scratch: ``run`` receives the run's files (what ``check_run`` reads)
    and ``cache`` is for a source's own cache (Hugging Face's), never ``~/.cache``. Both
    are 0700, inside ``root`` (0700)."""

    root: Path
    run: Path
    cache: Path


@contextmanager
def scratch(parent: Path | str | None = None) -> Iterator[Scratch]:
    """Make a fresh 0700 scratch directory and remove it on exit, failures included.

    If it can't be removed the context logs ``scratch_not_removed`` (no path) and does not
    hide the original exception. A SIGKILL leaves it behind; the runner's disk is wiped.
    """
    root: Path | None = None
    try:
        root = Path(tempfile.mkdtemp(prefix="mhai-run-", dir=parent))
        os.chmod(root, 0o700)
        for sub in ("run", "cache"):
            os.mkdir(root / sub, 0o700)
    except OSError:
        pass
    if root is None or not (root / "cache").is_dir():
        if root is not None:
            shutil.rmtree(root, ignore_errors=True)
        raise Refused("not_allowed_here", "scratch")
    st = os.lstat(root)
    if not stat.S_ISDIR(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o700:
        shutil.rmtree(root, ignore_errors=True)
        raise Refused("not_allowed_here", "scratch")
    try:
        yield Scratch(root, root / "run", root / "cache")
    finally:
        shutil.rmtree(root, ignore_errors=True)
        if os.path.lexists(root):
            log.warning("source scratch_not_removed")


def within(path: str, root: str) -> bool:
    """``path`` is ``root`` or below it (both already real paths)."""
    return os.path.commonpath([path, root]) == root


def read_into(sink: Sink, fd: int, *, block: int = files.BLOCK_SIZE) -> None:
    """Copy an open file descriptor into ``sink`` until EOF."""
    while True:
        chunk: Any = b""
        failed = False
        try:
            chunk = os.read(fd, block)
        except OSError:
            failed = True
        if failed:
            raise invalid("read")
        if not chunk:
            return
        sink.write(chunk)


__all__ = [
    "RemoteFile",
    "Scratch",
    "Sink",
    "Source",
    "allowed_name",
    "check_listing",
    "invalid",
    "read_into",
    "scratch",
    "stream_to",
    "unavailable",
    "within",
]
