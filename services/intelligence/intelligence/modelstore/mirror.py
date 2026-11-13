"""The offline copy of the pinned models (own answer model plan, PR 3, Task 9).

``model-fetch`` (profile ``ownmodel``) is the only container with a route to the model
store. :func:`fetch_pinned` copies, for each pin in ``models.lock``, exactly what
:func:`~intelligence.modelstore.verify.verify_for_load` reads: the manifest version whose
SHA-256 is the lock's, every version of ``manifest.sig`` (at most
:data:`~intelligence.modelstore.verify.MAX_VERSIONS`), and each file and document by the
store version id the manifest records. Each object is written as ``<key>@<version id>``
under the mirror root ('@' is in neither a key nor a version id, so the name is
unambiguous), through a temporary file and a rename, mode 0400.

The pipeline then verifies from the mirror with :class:`DirReadStore`, offline. The mirror
is never trusted: verification checks the lock's hash, the signature, the gate and every
file's size and SHA-256 exactly as it does against the store, so a changed or missing
object is refused there (``hash_mismatch``, ``signature_invalid``, ``not_found``).

:class:`DirReadStore` reads only regular files (never through a link: every directory and
the file are opened with ``O_NOFOLLOW``), checks every key and version id with the store's
own rules, and bounds every read. Its ``head`` hashes the file (the store keeps a checksum
with every object; a directory has none). Nothing here logs a path or file content.
"""

from __future__ import annotations

import hashlib
import logging
import os
import secrets
import stat
from pathlib import Path
from typing import BinaryIO

from intelligence.modelstore import lock, manifest, names, signing, store, verify
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo

log = logging.getLogger(__name__)

_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
_DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC
_FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC
_BLOCK = 8 * 1024 * 1024
SEP = "@"
# A manifest document (evaluation, data statement, warm-up) at most: a bound on what one
# document can make model-fetch write.
MAX_DOCUMENT_BYTES = 64 * 1024 * 1024


def _vid(version_id: object) -> str:
    if not isinstance(version_id, str):
        raise Refused("invalid_name")
    return names.check_store_version_id(version_id)


class DirReadStore:
    """The ``ReadStore`` protocol over a mirror directory (see the module docstring)."""

    def __init__(self, root: Path | str) -> None:
        self.root = Path(root)

    def __repr__(self) -> str:
        return "DirReadStore()"

    def _parent_fd(self, key: str) -> tuple[int, str]:
        """A descriptor of the key's directory (no link followed) and the base name."""
        store._check_key(key)
        *dirs, base = key.split("/")
        try:
            fd = os.open(self.root, _DIR_FLAGS)
        except OSError:
            raise Refused("not_found", detail_code="mirror") from None
        try:
            for d in dirs:
                nxt = os.open(d, _DIR_FLAGS, dir_fd=fd)
                os.close(fd)
                fd = nxt
        except OSError:
            os.close(fd)
            raise Refused("not_found", detail_code="missing") from None
        return fd, base

    def _open(self, key: str, version_id: str | None) -> tuple[int, int]:
        """An open regular file of ``key`` at ``version_id`` and its size."""
        if version_id is None:
            raise Refused("not_found", detail_code="latest")  # a mirror has no "latest"
        vid = _vid(version_id)
        dfd, base = self._parent_fd(key)
        try:
            fd = os.open(f"{base}{SEP}{vid}", _FILE_FLAGS, dir_fd=dfd)
        except OSError:
            raise Refused("not_found", detail_code="missing") from None
        finally:
            os.close(dfd)
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            os.close(fd)
            raise Refused("not_found", detail_code="not_regular")
        return fd, st.st_size

    def versions(self, key: str) -> list[str]:
        """The version ids the mirror holds for ``key`` (sorted; every one is checked by
        the verifier, so the order decides nothing)."""
        try:
            dfd, base = self._parent_fd(key)
        except Refused as e:
            if e.detail_code == "missing":
                return []
            raise
        try:
            entries = os.listdir(dfd)
        finally:
            os.close(dfd)
        out = []
        for e in entries:
            if e.startswith(base + SEP):
                try:
                    out.append(_vid(e[len(base) + 1 :]))
                except Refused:
                    continue
        return sorted(out)

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo:
        fd, size = self._open(key, version_id)
        digest = hashlib.sha256()
        with os.fdopen(fd, "rb") as f:
            while block := f.read(_BLOCK):
                digest.update(block)
        assert version_id is not None
        return ObjectInfo(version_id=version_id, size=size, sha256=digest.hexdigest())

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        fd, size = self._open(key, version_id)
        with os.fdopen(fd, "rb") as f:
            if size > max_bytes:
                raise Refused("too_large")
            data = f.read(max_bytes + 1)
        if len(data) > max_bytes:
            raise Refused("too_large")
        return data

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]:
        fd, size = self._open(key, version_id)
        digest = hashlib.sha256()
        n = 0
        with os.fdopen(fd, "rb") as f:
            if size > max_bytes:
                raise Refused("too_large")
            while block := f.read(min(_BLOCK, max_bytes + 1 - n)):
                n += len(block)
                if n > max_bytes:
                    raise Refused("too_large")
                digest.update(block)
                out.write(block)
        return n, digest.hexdigest()


# ---------------------------------------------------------------- fetching


def _write(
    root: Path,
    key: str,
    version_id: str,
    data: bytes | None,
    src: verify.ReadStore,
    max_bytes: int,
) -> bool:
    """``key@version_id`` under ``root`` unless already there; True when copied."""
    store._check_key(key)
    vid = _vid(version_id)
    target_dir = root.joinpath(*key.split("/")[:-1])
    target = target_dir / f"{key.rsplit('/', 1)[-1]}{SEP}{vid}"
    if target.is_file() and not target.is_symlink():
        return False
    target_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    tmp = target_dir / f".{secrets.token_hex(8)}.partial"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | _CLOEXEC
    fd = os.open(tmp, flags, 0o600)
    try:
        with os.fdopen(fd, "wb") as out:
            if data is not None:
                out.write(data)
            else:
                src.download(key, version_id=vid, out=out, max_bytes=max_bytes)
            out.flush()
            os.fsync(out.fileno())
        os.chmod(tmp, 0o400)
        os.replace(tmp, target)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    return True


def fetch_one(src: verify.ReadStore, pin: lock.Pin, root: Path) -> int:
    """Copy what verifying ``pin`` reads; returns the number of objects copied."""
    name, version = names.check_name(pin.name), names.check_version(pin.version)
    copied = 0
    vid, raw = verify.find_manifest_version(src, name, version, pin.manifest_sha256)
    copied += _write(root, store.manifest_key(name, version), vid, raw, src, len(raw))
    sig_key = store.manifest_sig_key(name, version)
    for svid in src.versions(sig_key)[: verify.MAX_VERSIONS]:
        copied += _write(root, sig_key, svid, None, src, signing.MAX_SIGNATURE_DOC_BYTES)
    m = manifest.parse(raw)
    for f in m.files:
        key = store.run_file_key(name, version, m.run.id, f.path)
        copied += _write(root, key, f.store_version_id, None, src, f.size)
    for d, rec in m.documents.items():
        key = store.run_doc_key(name, version, m.run.id, d)
        copied += _write(root, key, rec.store_version_id, None, src, MAX_DOCUMENT_BYTES)
    return copied


def fetch_pinned(src: verify.ReadStore, pinned: lock.Lock, root: Path | str) -> dict[str, int]:
    """:func:`fetch_one` for every role in ``pinned``; ``{role: objects copied}``."""
    root = Path(root)
    out: dict[str, int] = {}
    for role, pin in sorted(pinned.models.items()):
        out[role] = fetch_one(src, pin, root)
        log.info(
            "model.mirrored role=%s name=%s version=%s copied=%d",
            role,
            pin.name,
            pin.version,
            out[role],
        )
    return out


__all__ = ["DirReadStore", "fetch_one", "fetch_pinned"]
