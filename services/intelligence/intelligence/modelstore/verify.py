"""Load-time verification of a pinned model (design section 7.4, plan Task 22).

:func:`verify_pin` takes one ``models.lock`` entry and returns a :class:`VerifiedModel`,
a read-only directory holding exactly the manifest's files, each checked. In this order,
each failure a fixed reason with no fallback:

1. **The environment and the trust policy.** Outside development (``MHAI_MODELS_ENV``
   not exactly ``development``) the policy has no development signer (``policy_invalid``,
   P12). ``MHAI_MODELS_ENV=development`` in a process that declares itself production is
   ``not_allowed_here``: the declaration is ``APP_ENV``, the variable the intelligence
   service's settings already use (:func:`intelligence.boundary.config.is_development`),
   and a process declares production when ``APP_ENV`` is set to anything but
   development. (The service itself counts an unset ``APP_ENV`` as production too; the
   loader in Task 23 refuses ``MHAI_MODELS_ENV=development`` there on that basis.)
2. **The lock's hash.** Among the stored versions of ``models/<name>/<version>/
   manifest.json`` the oldest whose SHA-256 is the lock's is taken; none is
   ``lock_mismatch`` (B19), no manifest at all is ``not_found`` (P8: a promotion that
   crashed before its manifest left no version). This search is the one place a
   version is picked without an id, and it picks only by the lock's hash: a shadow with
   other bytes is ignored, copies with the same bytes are one candidate, and a version
   the store can't vouch for (a shadow PUT without a checksum: ``store_unavailable``/
   ``checksum``) is skipped.
3. **Identity.** Strict canonical parse; ``name``, ``version`` and ``role`` must be the
   lock entry's (``identity_mismatch``, P9).
4. **The manifest's signature.** The manifest cannot record its own signature's version
   (it is signed after it is built), so the versions of ``manifest.sig`` are tried
   oldest first, identical bytes once, at most :data:`MAX_VERSIONS`: the first that
   verifies for purpose ``manifest`` over the exact bytes from step 2 is accepted. A
   signature for another purpose (``signoff``, ``testset``) over the same bytes never
   verifies (domain separation). None is ``signature_invalid``, or ``signer_untrusted``
   when every one was by a key the policy doesn't trust for manifests (B19).
5. **The files.** Each file is streamed by its recorded ``store_version_id`` into
   ``<cache>/<name>/<version>-<hash12>.partial/`` (created ``O_EXCL``, never following a
   link), at most its recorded size (``size_mismatch``), its SHA-256 compared
   (``hash_mismatch``; B18). Then Task 13's :func:`~intelligence.modelstore.files.
   check_run` runs over the directory: a manifest can't vouch for a pickle.
6. **The exact set.** The directory holds the manifest's files and nothing else
   (``extra_file``, ``missing_file``; links and special files are refused by
   ``check_run``; P10). Only the local directory is looked at: the store's prefix is
   never listed, so a stray key under a run's prefix doesn't matter. Neither the
   manifest nor the documents are written into the directory; the parsed manifest is
   returned in :class:`VerifiedModel`.
7. **The sign-off** (``require_signoff``; the loader passes true outside development,
   ``pin`` passes false): a ``signoff.json`` version that parses strictly and names this
   name, version, role, manifest hash, evaluation report hash, gate, promoter and
   trainer, with a ``signoff.sig`` version that verifies for purpose ``signoff`` over it.
   Both are picked by verification, never by "latest". None is ``not_approved`` (P17).
8. **Seal.** Files 0400, then the ``.partial`` directory is renamed to the final one,
   which becomes 0500.

**The cache is never trusted** (P11). Steps 1-4 and 7 always run against the store. An
existing final directory is re-checked in full (``check_run``, the exact set, every
file's size and SHA-256 against the manifest, and the modes); on any difference it is
deleted and downloaded once more, and a second difference is refused. On any refusal the
``.partial`` directory and the final directory of that pin are removed, so nothing
half-verified stays readable.

The cache directory must be a real directory (not a link) owned by this user and not
writable by group or others; it is made 0700 if missing. Deletion only ever happens
under it, through directory descriptors opened without following links, and puts a
directory's write bit back before emptying it. Two processes verifying the same pin
into the same cache take turns on an exclusive lock file per ``<name>/<version>-<hash12>``.

**Store reads always name a version** (Task 16 M4), except the step 2 and 4 listings,
which choose only by hash or by signature.

**Logs** carry the role, name, version, hash, counts and fixed reason codes only: never a
URL, key, credential, path or file content. Nothing is loaded here; warm-up needs the
runtime and lives in the loader (Task 23).
"""

from __future__ import annotations

import fcntl
import hashlib
import logging
import os
import re
import secrets
import stat
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO, Protocol

from intelligence.boundary import config as service_config
from intelligence.modelstore import files, manifest, names, signing, signoff, store
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo

log = logging.getLogger(__name__)

MAX_VERSIONS = 32
HASH12 = 12
DIR_MODE = 0o500
FILE_MODE = 0o400
PARTIAL_SUFFIX = ".partial"
APP_ENV = "APP_ENV"
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
_DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC


class PinEntry(Protocol):
    """One role's pin in ``models.lock`` (Task 20's entry has these fields)."""

    @property
    def name(self) -> str: ...

    @property
    def version(self) -> str: ...

    @property
    def manifest_sha256(self) -> str: ...

    @property
    def gate(self) -> str: ...


class ReadStore(Protocol):
    """The store calls the verifier makes (the reader's credentials suffice)."""

    def versions(self, key: str) -> list[str]: ...

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo: ...

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes: ...

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]: ...


@dataclass(frozen=True, slots=True)
class VerifiedModel:
    """A verified pin: the directory holds exactly the manifest's files, read-only."""

    role: str
    name: str
    version: str
    manifest_sha256: str
    path: Path
    version_string: str
    manifest: manifest.Manifest = field(repr=False, compare=False)


def version_string(name: str, version: str, manifest_sha256: str) -> str:
    """``<name>@<version>#<first 12 hex of the manifest SHA-256>`` (design section 7.5),
    what ``model_versions()`` reports for a pinned role (Task 23)."""
    names.check_name(name)
    names.check_version(version)
    if not isinstance(manifest_sha256, str) or _SHA256.fullmatch(manifest_sha256) is None:
        raise Refused("lock_invalid", detail_code="manifest_sha256")
    return f"{name}@{version}#{manifest_sha256[:HASH12]}"


def requires_signoff() -> bool:
    """What the loader passes as ``require_signoff``: true everywhere but development."""
    return not signing.is_development()


# ---------------------------------------------------------------- step 1


def declares_production() -> bool:
    """True when ``APP_ENV`` is set to anything but development (the service's own rule,
    :func:`intelligence.boundary.config.is_development`)."""
    raw = os.environ.get(APP_ENV)
    return raw is not None and not service_config.is_development({APP_ENV: raw})


def check_environment(policy: signing.TrustPolicy) -> None:
    """Step 1: see the module docstring."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    if signing.is_development():
        if declares_production():
            raise Refused("not_allowed_here", detail_code="development_in_production")
        return
    if policy.has_development_signer():
        raise Refused("policy_invalid", detail_code="development_signer_in_production")


# ---------------------------------------------------------------- store reads


def _store_refusal(e: Refused) -> bool:
    """The store's own failure (propagates), as opposed to "this version is unusable"
    (skipped): a version without a stored checksum is the latter."""
    return e.reason == "store_unavailable" and e.detail_code != "checksum"


def _oldest_first(source: ReadStore, key: str) -> list[str]:
    vids = source.versions(key)
    return list(reversed(vids))[:MAX_VERSIONS]


def _get_version(source: ReadStore, key: str, *, version_id: str, max_bytes: int) -> bytes | None:
    """``version_id`` of ``key``, or None when that version is unusable."""
    if not isinstance(version_id, str):
        raise Refused("invalid_name")
    try:
        return source.get(key, version_id, max_bytes=max_bytes)
    except Refused as e:
        if _store_refusal(e):
            raise
    return None


def find_manifest(source: ReadStore, name: str, version: str, sha256: str) -> bytes:
    """Step 2: the bytes of the oldest stored manifest version whose SHA-256 is
    ``sha256``."""
    key = store.manifest_key(name, version)
    vids = _oldest_first(source, key)
    if not vids:
        raise Refused("not_found", detail_code="manifest")
    for vid in vids:
        skip = False
        try:
            info = source.head(key, vid)
        except Refused as e:
            if _store_refusal(e):
                raise
            skip = True
        if skip or info.size > manifest.MAX_BYTES:
            continue
        if not secrets.compare_digest(info.sha256, sha256):
            continue
        raw = _get_version(source, key, version_id=vid, max_bytes=manifest.MAX_BYTES)
        if raw is not None and secrets.compare_digest(hashlib.sha256(raw).hexdigest(), sha256):
            return raw
    raise Refused("lock_mismatch")


def _signature_docs(source: ReadStore, key: str) -> list[bytes]:
    """The distinct readable versions of a signature key, oldest first."""
    out: list[bytes] = []
    for vid in _oldest_first(source, key):
        doc = _get_version(source, key, version_id=vid, max_bytes=signing.MAX_SIGNATURE_DOC_BYTES)
        if doc is not None and doc not in out:
            out.append(doc)
    return out


def _verified_by_one(
    purpose: str, payload: bytes, docs: list[bytes], policy: signing.TrustPolicy
) -> set[str] | None:
    """None if one of ``docs`` verifies for ``purpose`` over ``payload``; else the reasons
    each failed with. A development signer outside development propagates."""
    verifier = signing.Verifier()
    reasons: set[str] = set()
    for doc in docs:
        try:
            verifier.verify(purpose, payload, doc, policy)
        except Refused as e:
            if e.reason == "policy_invalid":
                raise
            reasons.add(e.reason)
            continue
        return None
    return reasons


def check_manifest_signature(
    source: ReadStore, name: str, version: str, raw: bytes, policy: signing.TrustPolicy
) -> None:
    """Step 4: see the module docstring."""
    docs = _signature_docs(source, store.manifest_sig_key(name, version))
    reasons = _verified_by_one("manifest", raw, docs, policy)
    if reasons is None:
        return
    if reasons == {"signer_untrusted"}:
        raise Refused("signer_untrusted")
    raise Refused("signature_invalid", detail_code="manifest" if docs else "missing")


def check_signoff(
    source: ReadStore,
    role: str,
    entry: PinEntry,
    m: manifest.Manifest,
    policy: signing.TrustPolicy,
) -> None:
    """Step 7: a sign-off for exactly this pin, signed for purpose ``signoff``."""
    key = store.signoff_key(m.name, m.version)
    docs: list[bytes] = []
    for vid in _oldest_first(source, key):
        raw = _get_version(source, key, version_id=vid, max_bytes=signoff.MAX_BYTES)
        if raw is not None and raw not in docs:
            docs.append(raw)
    if not docs:
        raise Refused("not_approved", detail_code="missing")
    sigs = _signature_docs(source, store.signoff_sig_key(m.name, m.version))
    detail = "mismatch"
    for raw in docs:
        parsed: signoff.Signoff | None = None
        try:
            parsed = signoff.parse(raw)
        except Refused:
            parsed = None
        if parsed is None or not _signoff_matches(parsed, role, entry, m):
            continue
        if _verified_by_one("signoff", raw, sigs, policy) is None:
            return
        detail = "signature"
    raise Refused("not_approved", detail_code=detail)


def _signoff_matches(s: signoff.Signoff, role: str, entry: PinEntry, m: manifest.Manifest) -> bool:
    return (
        s.name == m.name
        and s.version == m.version
        and s.role == role == m.role
        and secrets.compare_digest(s.manifest_sha256, entry.manifest_sha256)
        and secrets.compare_digest(s.evaluation_sha256, m.evaluation.report_sha256)
        and s.gate == entry.gate
        and s.promotion_actor == m.promotion.actor
        and s.trained_by == m.trained_by
    )


# ---------------------------------------------------------------- the cache


def _cache_refused(detail: str) -> Refused:
    return Refused("not_allowed_here", detail_code=detail)


def _safe_dir(st: os.stat_result) -> bool:
    return (
        stat.S_ISDIR(st.st_mode)
        and st.st_uid == os.geteuid()
        and not stat.S_IMODE(st.st_mode) & 0o022
    )


def _open_dir(path: Path | str, *, dir_fd: int | None = None, create: bool) -> int:
    """A directory (made 0700 if ``create`` and missing) that is not a link, is ours and
    is writable by nobody else, opened without following a link."""
    failed = False
    fd = -1
    try:
        if create:
            try:
                os.mkdir(path, 0o700, dir_fd=dir_fd)
            except FileExistsError:
                pass
        fd = os.open(path, _DIR_FLAGS, dir_fd=dir_fd)
    except OSError:
        failed = True
    if failed:
        raise _cache_refused("cache_dir")
    if not _safe_dir(os.fstat(fd)):
        os.close(fd)
        raise _cache_refused("cache_dir_unsafe")
    return fd


def _lstat(name: str, dir_fd: int) -> os.stat_result | None:
    try:
        return os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except FileNotFoundError:
        return None


def _remove_tree(parent_fd: int, name: str) -> None:
    """Remove ``name`` under ``parent_fd`` (and, if it is a real directory, everything in
    it), never following a link; a directory's mode is set back to 0700 first so a
    read-only one can be emptied. ``OSError`` propagates to the caller."""
    st = _lstat(name, parent_fd)
    if st is None:
        return
    if not stat.S_ISDIR(st.st_mode):
        os.unlink(name, dir_fd=parent_fd)
        return
    fd = os.open(name, _DIR_FLAGS, dir_fd=parent_fd)
    try:
        os.fchmod(fd, 0o700)
        for child in os.listdir(fd):
            _remove_tree(fd, child)
    finally:
        os.close(fd)
    os.rmdir(name, dir_fd=parent_fd)


def _remove(parent_fd: int, name: str) -> bool:
    """:func:`_remove_tree`, logging a fixed code (never the path) on failure."""
    failed = False
    try:
        _remove_tree(parent_fd, name)
    except OSError:
        failed = True
    if failed:
        log.error("model.cache_not_removed code=cache_not_removed")
    return not failed


@contextmanager
def _pin_lock(name_fd: int, final: str) -> Iterator[None]:
    """An exclusive lock on ``.<final>.lock`` beside the pin's directory, held for the
    whole check, so two processes never build or delete the same directory at once."""
    failed = False
    fd = -1
    try:
        fd = os.open(
            f".{final}.lock",
            os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | _CLOEXEC,
            0o600,
            dir_fd=name_fd,
        )
        fcntl.flock(fd, fcntl.LOCK_EX)
    except OSError:
        failed = True
    if failed:
        if fd >= 0:
            os.close(fd)
        raise _cache_refused("cache_lock")
    try:
        yield
    finally:
        os.close(fd)  # releases the lock


def _download(source: ReadStore, m: manifest.Manifest, name_fd: int, partial: str) -> None:
    """Step 5a: every file by its recorded version into a new ``partial`` directory."""
    limits = files.Limits()
    total = 0
    for f in m.files:
        total += f.size
        if f.size > limits.max_file_bytes or total > limits.max_total_bytes:
            raise Refused("too_large", detail_code="manifest")
    failed = False
    try:
        os.mkdir(partial, 0o700, dir_fd=name_fd)
        dfd = os.open(partial, _DIR_FLAGS, dir_fd=name_fd)
    except OSError:
        failed = True
    if failed:
        raise _cache_refused("partial")
    try:
        for f in m.files:
            _download_one(source, m, f, dfd)
    finally:
        os.close(dfd)


def _download_one(
    source: ReadStore, m: manifest.Manifest, f: manifest.FileRecord, dfd: int
) -> None:
    key = store.run_file_key(m.name, m.version, m.run.id, f.path)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | _CLOEXEC
    fd = -1
    failed = False
    try:
        fd = os.open(f.path, flags, 0o600, dir_fd=dfd)
    except OSError:
        failed = True
    if failed:
        raise _cache_refused("partial_file")
    reason: str | None = None
    detail: str | None = None
    with os.fdopen(fd, "wb") as out:
        try:
            n, sha = source.download(key, version_id=f.store_version_id, out=out, max_bytes=f.size)
        except Refused as e:
            if _store_refusal(e):
                raise
            reason = {"not_found": "missing_file", "too_large": "size_mismatch"}.get(
                e.reason, e.reason
            )
            detail = "store"
        if reason is None:
            sync_failed = False
            try:
                out.flush()
                os.fsync(out.fileno())
                os.fchmod(out.fileno(), FILE_MODE)
            except OSError:
                sync_failed = True
            if sync_failed:
                reason, detail = "file_not_allowed", "partial_write"
    if reason is not None:
        raise Refused(reason, detail_code=detail)
    if n != f.size:
        raise Refused("size_mismatch")
    if not secrets.compare_digest(sha, f.sha256):
        raise Refused("hash_mismatch")


def check_dir(path: Path, m: manifest.Manifest, *, sealed: bool) -> None:
    """Steps 5b and 6 over a local directory: Task 13's ``check_run``, then exactly the
    manifest's files with its sizes and hashes. ``sealed``: every file must also be 0400
    and the directory 0500 (a cached final directory)."""
    entries = files.check_run(path)
    found = {e.path: e for e in entries}
    expected = {f.path: f for f in m.files}
    if set(expected) - set(found):
        raise Refused("missing_file")
    if set(found) - set(expected):
        raise Refused("extra_file")
    for p, rec in expected.items():
        got = found[p]
        if got.size != rec.size:
            raise Refused("size_mismatch")
        if not secrets.compare_digest(got.sha256, rec.sha256):
            raise Refused("hash_mismatch")
    modes_ok = False
    try:
        modes_ok = all(
            stat.S_IMODE(os.stat(path / p, follow_symlinks=False).st_mode) == FILE_MODE
            for p in expected
        ) and (not sealed or stat.S_IMODE(os.stat(path, follow_symlinks=False).st_mode) == DIR_MODE)
    except OSError:
        modes_ok = False
    if not modes_ok:
        raise Refused("file_not_allowed", detail_code="mode")


def _cached_ok(path: Path, m: manifest.Manifest) -> bool:
    """Whether the cached final directory is still exactly the manifest's files."""
    try:
        check_dir(path, m, sealed=True)
    except Refused:
        return False
    return True


def _seal(name_fd: int, partial: str, final: str) -> None:
    """Step 8: rename ``partial`` to ``final`` and make it 0500 (files are 0400)."""
    failed = False
    try:
        os.rename(partial, final, src_dir_fd=name_fd, dst_dir_fd=name_fd)
        fd = os.open(final, _DIR_FLAGS, dir_fd=name_fd)
        try:
            os.fchmod(fd, DIR_MODE)
        finally:
            os.close(fd)
    except OSError:
        failed = True
    if failed:
        raise _cache_refused("seal")


# ---------------------------------------------------------------- the pipeline


def _check_inputs(role: object, entry: PinEntry) -> tuple[str, str, str, str]:
    names.check_role(role)  # type: ignore[arg-type]
    name = names.check_name(entry.name)
    version = names.check_version(entry.version)
    sha = entry.manifest_sha256
    if not isinstance(sha, str) or _SHA256.fullmatch(sha) is None:
        raise Refused("lock_invalid", detail_code="manifest_sha256")
    return str(role), name, version, sha


def verify_pin(
    source: ReadStore,
    role: str,
    entry: PinEntry,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    *,
    require_signoff: bool,
) -> VerifiedModel:
    """Verify ``entry`` for ``role`` into ``cache_dir``; see the module docstring.
    Raises :class:`Refused` with a fixed code on any failure."""
    if not isinstance(require_signoff, bool):
        raise Refused("not_allowed_here", detail_code="require_signoff")
    role, name, version, sha = _check_inputs(role, entry)
    try:
        out = _verify(
            source, role, entry, name, version, sha, policy, Path(cache_dir), require_signoff
        )
    except Refused as e:
        log.warning(
            "model.verify_refused role=%s name=%s version=%s reason=%s detail=%s",
            role,
            name,
            version,
            e.reason,
            e.detail_code,
        )
        raise
    return out


def _verify(
    source: ReadStore,
    role: str,
    entry: PinEntry,
    name: str,
    version: str,
    sha: str,
    policy: signing.TrustPolicy,
    cache_dir: Path,
    require_signoff: bool,
) -> VerifiedModel:
    check_environment(policy)  # 1
    raw = find_manifest(source, name, version, sha)  # 2
    m = manifest.parse(raw)  # 3
    if (m.name, m.version, m.role) != (name, version, role):
        raise Refused("identity_mismatch")
    check_manifest_signature(source, name, version, raw, policy)  # 4

    final = f"{version}-{sha[:HASH12]}"
    partial = final + PARTIAL_SUFFIX
    root_fd = _open_dir(cache_dir, create=True)
    try:
        name_fd = _open_dir(name, dir_fd=root_fd, create=True)
    finally:
        os.close(root_fd)
    final_path = cache_dir / name / final
    cached = False
    try:
        with _pin_lock(name_fd, final):
            done = False
            try:
                if not _remove(name_fd, partial):
                    raise _cache_refused("cache_not_removed")
                if _lstat(final, name_fd) is not None:
                    cached = _cached_ok(final_path, m)
                    if not cached:
                        log.warning(
                            "model.cache_mismatch role=%s name=%s version=%s: downloading again",
                            role,
                            name,
                            version,
                        )
                        if not _remove(name_fd, final):
                            raise _cache_refused("cache_not_removed")
                if not cached:
                    _download(source, m, name_fd, partial)  # 5a
                    check_dir(cache_dir / name / partial, m, sealed=False)  # 5b, 6
                if require_signoff:
                    check_signoff(source, role, entry, m, policy)  # 7
                if not cached:
                    _seal(name_fd, partial, final)  # 8
                    check_dir(final_path, m, sealed=True)
                done = True
            finally:
                if not done:
                    _remove(name_fd, partial)
                    _remove(name_fd, final)
    finally:
        os.close(name_fd)
    log.info(
        "model.verified role=%s name=%s version=%s sha256=%s files=%d cached=%s signoff=%s",
        role,
        name,
        version,
        sha,
        len(m.files),
        cached,
        require_signoff,
    )
    return VerifiedModel(
        role=role,
        name=name,
        version=version,
        manifest_sha256=sha,
        path=final_path,
        version_string=version_string(name, version, sha),
        manifest=m,
    )


__all__ = [
    "DIR_MODE",
    "FILE_MODE",
    "MAX_VERSIONS",
    "PinEntry",
    "ReadStore",
    "VerifiedModel",
    "check_dir",
    "check_environment",
    "check_manifest_signature",
    "check_signoff",
    "declares_production",
    "find_manifest",
    "requires_signoff",
    "verify_pin",
    "version_string",
]
