"""Load-time verification of a pinned model (design section 7.4, plan Task 22).

:func:`verify_pin` takes one ``models.lock`` entry and returns a :class:`VerifiedModel`,
a read-only directory holding exactly the manifest's files, each checked. In this order,
each failure a fixed reason with no fallback:

1. **The environment and the trust policy.** Development needs both switches:
   ``MHAI_MODELS_ENV=development`` *and* ``APP_ENV=development``. ``APP_ENV`` is the
   variable the intelligence service's settings already use, and unset means production
   there (:func:`intelligence.boundary.config.is_development`), so it does here too.
   Outside development the policy has no development signer (``policy_invalid``, P12),
   and ``MHAI_MODELS_ENV=development`` in a production process (``APP_ENV`` unset or
   anything but development) is ``not_allowed_here``.
2. **The lock's hash.** Among the stored versions of ``models/<name>/<version>/
   manifest.json`` the oldest whose SHA-256 is the lock's is taken; none is
   ``lock_mismatch`` (B19), no manifest at all is ``not_found`` (P8: a promotion that
   crashed before its manifest left no version). This search is the one place a
   version is picked without an id, and it picks only by the lock's hash: a shadow with
   other bytes is ignored, copies with the same bytes are one candidate, and a version
   the store can't vouch for (a shadow PUT without a checksum: ``store_unavailable``/
   ``checksum``) is skipped and not counted. More than :data:`MAX_VERSIONS` usable
   versions before the match is ``lock_mismatch``/``too_many_versions`` (a flood, told
   apart from a wrong lock); the same cap holds for ``manifest.sig``, ``signoff.json``
   and ``signoff.sig``.
3. **Identity.** Strict canonical parse; ``name``, ``version`` and ``role`` must be the
   lock entry's (``identity_mismatch``, P9), and the manifest's own result for the
   pinned gate must be ``pass`` (``gate_failed``, P14 at load).
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
7. **The sign-off.** :func:`verify_for_load`, the loader's only entry point, requires it
   everywhere but development and has no switch; ``pin`` calls :func:`verify_pin` with
   ``require_signoff=False``. It needs a ``signoff.json`` version that parses strictly and names this
   name, version, role, manifest hash, evaluation report hash, gate, promoter and
   trainer, whose reviewers are neither the promoter nor the trainer and not both the
   clinical and the platform reviewer (P15 again at load), with a ``signoff.sig``
   version that verifies for purpose ``signoff`` over it.
   Both are picked by verification, never by "latest". None is ``not_approved`` (P17).
8. **Seal.** Files 0400, then the ``.partial`` directory is renamed to the final one,
   which becomes 0500.

**The cache is never trusted** (P11). Steps 1-4 and 7 always run against the store. An
existing final directory is re-checked in full (``check_run``, the exact set, every
file's size and SHA-256 against the manifest, and the modes); on any difference it is
deleted and downloaded once more, and a second difference is refused.

**What a refusal removes** (ruling D29). The ``.partial`` directory always goes. The
final directory goes when this call built it, when it differs from the manifest, and on a
definitive refusal (:data:`DEFINITIVE`: lock, identity, signature, policy, gate or
sign-off), including one in steps 1-4, which takes the pin's lock to remove it. A passing
store or disk error (``store_unavailable``) keeps a verified directory, which another
process may be reading. Nothing is removed outside the pin's lock.

The cache directory must be a real directory (not a link) owned by this user and not
writable by group or others; it is made 0700 if missing. Every directory above it, up to
``/``, must be owned by root or this user, and none that others can write may lack the
sticky bit (``cache_parent_unsafe``). Deletion only ever happens
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
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO, Protocol

from intelligence.boundary import config as service_config
from intelligence.doceval.report import GATES
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


def is_development() -> bool:
    """Development only when both switches say so: ``MHAI_MODELS_ENV=development`` and
    ``APP_ENV=development``. An unset ``APP_ENV`` is production, as it is for the service
    (:func:`intelligence.boundary.config.is_development`)."""
    return signing.is_development() and service_config.is_development()


def requires_signoff() -> bool:
    """The sign-off rule at load: required everywhere but development (both switches)."""
    return not is_development()


# ---------------------------------------------------------------- step 1


def declares_production() -> bool:
    """True unless ``APP_ENV`` names development; unset is production (the service's own
    rule, :func:`intelligence.boundary.config.is_development`)."""
    return not service_config.is_development()


def check_environment(policy: signing.TrustPolicy) -> None:
    """Step 1: see the module docstring."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    if signing.is_development() and declares_production():
        raise Refused("not_allowed_here", detail_code="development_in_production")
    if not is_development() and policy.has_development_signer():
        raise Refused("policy_invalid", detail_code="development_signer_in_production")


# ---------------------------------------------------------------- store reads


def _store_refusal(e: Refused) -> bool:
    """The store's own failure (propagates), as opposed to "this version is unusable"
    (skipped): a version without a stored checksum is the latter."""
    return e.reason == "store_unavailable" and e.detail_code != "checksum"


def _usable[T](vids: list[str], read: Callable[[str], T | None], reason: str) -> Iterator[T]:
    """Each usable version of ``key``, oldest first, as ``read`` returns it. A version
    ``read`` finds unusable (None: no stored checksum, gone, too large) is skipped and not
    counted; past :data:`MAX_VERSIONS` usable versions the walk stops with ``reason`` and
    the detail ``too_many_versions`` (a flood of junk versions, not a wrong lock).
    ``vids`` is the store's listing, newest first; its own cap bounds the walk."""
    used = 0
    for vid in reversed(vids):
        got = read(vid)
        if got is None:
            continue
        if used >= MAX_VERSIONS:
            raise Refused(reason, detail_code="too_many_versions")
        used += 1
        yield got


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


def _head_version(source: ReadStore, key: str, version_id: str) -> tuple[str, ObjectInfo] | None:
    try:
        return version_id, source.head(key, version_id)
    except Refused as e:
        if _store_refusal(e):
            raise
    return None


def find_manifest(source: ReadStore, name: str, version: str, sha256: str) -> bytes:
    """Step 2: the bytes of the oldest stored manifest version whose SHA-256 is
    ``sha256``."""
    key = store.manifest_key(name, version)
    vids = source.versions(key)
    if not vids:
        raise Refused("not_found", detail_code="manifest")
    for vid, info in _usable(vids, lambda v: _head_version(source, key, v), "lock_mismatch"):
        if info.size > manifest.MAX_BYTES or not secrets.compare_digest(info.sha256, sha256):
            continue
        raw = _get_version(source, key, version_id=vid, max_bytes=manifest.MAX_BYTES)
        if raw is not None and secrets.compare_digest(hashlib.sha256(raw).hexdigest(), sha256):
            return raw
    raise Refused("lock_mismatch")


def _first_verified(
    source: ReadStore,
    key: str,
    purpose: str,
    payload: bytes,
    policy: signing.TrustPolicy,
    reason: str,
) -> set[str] | None:
    """None as soon as one version of the signature ``key`` verifies for ``purpose`` over
    ``payload``; else the reasons the distinct versions failed with (empty: none at all).
    A development signer outside development propagates."""
    verifier = signing.Verifier()
    reasons: set[str] = set()
    tried: list[bytes] = []
    max_bytes = signing.MAX_SIGNATURE_DOC_BYTES
    for doc in _usable(
        source.versions(key),
        lambda v: _get_version(source, key, version_id=v, max_bytes=max_bytes),
        reason,
    ):
        if doc in tried:
            continue
        tried.append(doc)
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
    key = store.manifest_sig_key(name, version)
    reasons = _first_verified(source, key, "manifest", raw, policy, "signature_invalid")
    if reasons is None:
        return
    if reasons == {"signer_untrusted"}:
        raise Refused("signer_untrusted")
    raise Refused("signature_invalid", detail_code="manifest" if reasons else "missing")


def check_gate(entry: PinEntry, m: manifest.Manifest) -> None:
    """The manifest's own result for the pinned gate must be ``pass`` (``gate_failed``)."""
    gate = entry.gate
    if not isinstance(gate, str) or gate not in GATES:
        raise Refused("lock_invalid", detail_code="gate")
    if m.evaluation.gates.get(gate) != "pass":
        raise Refused("gate_failed")


def check_signoff(
    source: ReadStore,
    role: str,
    entry: PinEntry,
    m: manifest.Manifest,
    policy: signing.TrustPolicy,
) -> None:
    """Step 7: a sign-off for exactly this pin, signed for purpose ``signoff``, whose
    reviewers are neither the promoter nor the trainer and not both kinds at once."""
    key = store.signoff_key(m.name, m.version)
    sig_key = store.signoff_sig_key(m.name, m.version)
    tried: list[bytes] = []
    detail = "missing"
    for raw in _usable(
        source.versions(key),
        lambda v: _get_version(source, key, version_id=v, max_bytes=signoff.MAX_BYTES),
        "not_approved",
    ):
        if raw in tried:
            continue
        tried.append(raw)
        parsed: signoff.Signoff | None = None
        try:
            parsed = signoff.parse(raw)
        except Refused:
            parsed = None
        if parsed is None or not _signoff_matches(parsed, role, entry, m):
            if detail == "missing":
                detail = "mismatch"
            continue
        if not _separated(parsed):
            detail = "separation"
            continue
        if _first_verified(source, sig_key, "signoff", raw, policy, "not_approved") is None:
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


def _separated(s: signoff.Signoff) -> bool:
    """P15 again at load: no reviewer promoted or trained the model, and no one is both
    the clinical and the platform reviewer."""
    clinical, platform = set(s.clinical_reviewers), set(s.platform_reviewers)
    return not (clinical & platform) and not (
        (clinical | platform) & {s.promotion_actor, s.trained_by}
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


def _check_ancestors(cache_dir: Path) -> None:
    """Every directory above ``cache_dir`` (resolved) up to ``/`` is owned by root or this
    user, and none that others can write lacks the sticky bit; otherwise someone else
    could swap the tree after the checks (``cache_parent_unsafe``)."""
    unsafe = False
    failed = False
    try:
        parent = Path(os.path.realpath(cache_dir.absolute().parent))
        for d in (parent, *parent.parents):
            st = os.stat(d)
            if st.st_uid not in (0, os.geteuid()) or (
                st.st_mode & stat.S_IWOTH and not st.st_mode & stat.S_ISVTX
            ):
                unsafe = True
                break
    except (OSError, ValueError):
        failed = True
    if failed:
        raise _cache_refused("cache_dir")
    if unsafe:
        raise _cache_refused("cache_parent_unsafe")


def _open_cache(cache_dir: Path, name: str, *, create: bool) -> int:
    """The pin's ``<cache>/<name>`` directory, every level checked; the caller closes it."""
    _check_ancestors(cache_dir)
    root_fd = _open_dir(cache_dir, create=create)
    try:
        return _open_dir(name, dir_fd=root_fd, create=create)
    finally:
        os.close(root_fd)


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
    n, sha = -1, ""
    # Unbuffered, so nothing is left to flush at close; a close that still fails (an
    # I/O error) becomes the fixed refusal, never a bare OSError.
    out = os.fdopen(fd, "wb", buffering=0)
    try:
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
                os.fsync(out.fileno())
                os.fchmod(out.fileno(), FILE_MODE)
            except OSError:
                sync_failed = True
            if sync_failed:
                reason, detail = "file_not_allowed", "partial_write"
    finally:
        close_failed = False
        try:
            out.close()
        except OSError:
            close_failed = True
        if close_failed and reason is None:
            reason, detail = "file_not_allowed", "partial_close"
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
    Raises :class:`Refused` with a fixed code on any failure.

    ``require_signoff=False`` is for ``mhai-models pin`` (Task 20), which checks a version
    before it has a sign-off. Loading a model never calls this directly: it calls
    :func:`verify_for_load`, which has no such switch."""
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


def verify_for_load(
    source: ReadStore,
    role: str,
    entry: PinEntry,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
) -> VerifiedModel:
    """The only entry point for loading a pinned model (Task 23's loader): as
    :func:`verify_pin`, with the sign-off required unless both ``MHAI_MODELS_ENV`` and
    ``APP_ENV`` say development (:func:`requires_signoff`). It cannot be turned off."""
    return verify_pin(source, role, entry, policy, cache_dir, require_signoff=requires_signoff())


# Refusals that decide a pin is bad (not a passing store or disk problem): the pin's
# cached directory is removed (ruling D29). Store and transport errors keep it.
DEFINITIVE = frozenset(
    {
        "lock_mismatch",
        "identity_mismatch",
        "signature_invalid",
        "signer_untrusted",
        "policy_invalid",
        "not_approved",
        "gate_failed",
        "not_canonical",
        "manifest_invalid",
        "json_invalid",
    }
)
# The local copy differs from the manifest.
MISMATCH = frozenset(
    {"hash_mismatch", "size_mismatch", "missing_file", "extra_file", "file_not_allowed"}
)


def _purge(cache_dir: Path, name: str, final: str) -> None:
    """After a definitive refusal in steps 1-4: remove the pin's final and ``.partial``
    directories, under the pin's lock. A cache that is missing or unsafe is left alone
    (nothing in it is ever handed out without a full verification)."""
    name_fd = -1
    try:
        name_fd = _open_cache(cache_dir, name, create=False)
    except Refused:
        return
    try:
        with _pin_lock(name_fd, final):
            gone = _remove(name_fd, final + PARTIAL_SUFFIX) and _remove(name_fd, final)
        if gone:
            log.info("model.cache_removed name=%s dir=%s", name, final)
    except Refused:
        pass
    finally:
        os.close(name_fd)


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
    final = f"{version}-{sha[:HASH12]}"
    partial = final + PARTIAL_SUFFIX
    definitive: str | None = None
    try:
        check_environment(policy)  # 1
        raw = find_manifest(source, name, version, sha)  # 2
        m = manifest.parse(raw)  # 3
        if (m.name, m.version, m.role) != (name, version, role):
            raise Refused("identity_mismatch")
        check_gate(entry, m)
        check_manifest_signature(source, name, version, raw, policy)  # 4
    except Refused as e:
        definitive = e.reason if e.reason in DEFINITIVE else None
        raise
    finally:
        if definitive is not None:
            _purge(cache_dir, name, final)

    name_fd = _open_cache(cache_dir, name, create=True)
    final_path = cache_dir / name / final
    cached = False
    try:
        with _pin_lock(name_fd, final):
            done = False
            refused: str | None = None
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
            except Refused as e:
                refused = e.reason
                raise
            finally:
                if not done:
                    _remove(name_fd, partial)
                    # A directory this call built (or found wrong) goes; a verified one
                    # another process may be using stays on a passing store or disk
                    # error (M6), and goes on a definitive refusal (D29).
                    if not cached or refused in DEFINITIVE | MISMATCH:
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
    "DEFINITIVE",
    "DIR_MODE",
    "FILE_MODE",
    "MAX_VERSIONS",
    "PinEntry",
    "ReadStore",
    "VerifiedModel",
    "check_dir",
    "check_environment",
    "check_gate",
    "check_manifest_signature",
    "check_signoff",
    "declares_production",
    "find_manifest",
    "is_development",
    "requires_signoff",
    "verify_for_load",
    "verify_pin",
    "version_string",
]
