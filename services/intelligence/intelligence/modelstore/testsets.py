"""The locked test split: built once inside the store, signed, pinned in models/testsets.lock.

**Build** (:func:`build_and_lock`, ``mhai-models testset-build``), as the ``eval-writer``:

1. before anything is written: the names; the signer, which must be trusted for
   ``testset`` by the given policy (``signer_untrusted``) and must hold that key (a probe
   payload, never a valid manifest, is signed and verified); and a fresh 0700 scratch
   folder (``not_allowed_here``/``scratch``). None of these can burn a version;
2. ``eval/<suite>/<version>/claim.json`` is reserved with a conditional create, so a second
   build of the same suite and version stops here (``version_exists``) instead of after
   generating and uploading a split that could never be deleted;
3. the ``test`` purpose of set v1 is built (``doceval.sets.build``: its secret seed is drawn
   inside and never stored; none is passed) into the scratch folder, and the
   synthetic-data scanner runs over it (``dataset_not_synthetic``);
4. every file is uploaded with ``put_new`` (each file's size and SHA-256 checked against
   ``testset.json`` first) under ``eval/<suite>/<version>/files/<path>``;
5. the *locked* ``testset.json`` is the build's manifest plus ``locked: true`` and
   ``store_version_ids`` (path -> the object's store version id), so the signature covers
   the version ids as well as the hashes. It is signed (purpose ``testset``), the signature
   is checked against the policy, and the manifest and then ``testset.sig`` go up last;
6. the scratch copy is removed in a ``finally``, on success and on every failure. A
   removal that fails is logged with a fixed code (``scratch_not_removed``, never the
   path) and reported in ``Built.scratch_removed``. A killed process (SIGKILL, OOM) skips
   the ``finally``: the folder then stays, mode 0700, under the configured scratch root
   (``MHAI_MODELS_SCRATCH``), so the job must run on an ephemeral runner or a per-job
   scratch volume (Task 24).

No refusal, log line or exception chain carries a path: file names carry document ids, so
file and OS errors are re-raised as fixed codes outside their handlers.

What leaves the build is only the lock line ``<suite>@<version>#<sha256 of the locked
testset.json>`` (the CLI prints it, and it goes into ``models/testsets.lock`` by PR), and,
for reports, :meth:`Built.export`: the SHA-256, ``extraction_schema_version``, ``slices``
and ``totals``. Never a document id, path, page or value of the split.

**Fetch** (:func:`fetch`), as the ``eval-reader``, never trusts "latest":

* ``testset.json`` is taken by its recorded version id when the lock entry has one;
  otherwise it is the oldest stored version whose SHA-256 equals the lock's. Versions are
  filtered by hash first: a shadow with other bytes, or one the store cannot vouch for (a
  PUT without a checksum), is ignored, and copies of the same bytes are one candidate;
* ``testset.sig`` likewise by recorded id, else the oldest stored version that verifies
  over those bytes under the trust policy. Signatures are deduplicated by their bytes and
  one that doesn't verify (junk, another purpose, an untrusted key) is ignored; at most
  :data:`MAX_VERSIONS` distinct signatures or failed reads are tried. The build writes ``testset.sig``
  with a conditional create, so the real one is always the oldest version;
* every file is fetched by the ``store_version_id`` recorded in the signed manifest and its
  size and SHA-256 checked. Only the files the signed manifest lists are read: other
  objects under ``files/`` (an extra upload) are ignored, never fetched.

Finding the versions needs ``ListObjectVersions``: the eval reader may list versions under
``eval/`` only (``s3:prefix`` condition, deploy/minio/policies/eval-reader.json), which shows
it no more than it can already read. Any mismatch (the lock's hash, the signature, a file's
hash or size, a file missing from or extra in the manifest's ``store_version_ids``, a
signed manifest that points at a shadowing version) is
``testset_mismatch``; a store that refuses the caller is ``store_unavailable``.
Residual risk: a holder of eval-writer credentials can still make a fetch slow or fail by
uploading very many versions (the store's listing cap is 10,000); the eval writer is
trusted not to deny service. The scratch
folder is filled only after the signature checks, and removed on any failure. The fetched
folder holds the files and the locked ``testset.json`` (the runner's set check accepts it),
not the signature.

**models/testsets.lock** is ASCII, one entry per line (``<suite>@<semver>#<64 hex>``) and
comment lines that start with ``#``; every line ends with ``\\n``; no blank lines, no
whitespace around an entry, no duplicate suite and version. Anything else is
``lock_invalid``.
"""

from __future__ import annotations

import hashlib
import logging
import os
import re
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval import sets
from intelligence.doceval.synth import scanner as synth_scanner
from intelligence.modelstore import canonical, files, names, signing, store
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectStore

log = logging.getLogger(__name__)

PURPOSE = "testset"
CLAIM_SCHEMA = "mhai.models.testset_claim/1"
SUITES = frozenset({sets.SET_NAME})
MAX_TESTSET_BYTES = 64 * 1024 * 1024
MAX_FILE_BYTES = 64 * 1024 * 1024
MAX_FILES = 200_000
MAX_VERSIONS = 32
MAX_LOCK_BYTES = 64 * 1024

_SUITE = r"[a-z][a-z0-9_]{0,62}"
_SEMVER = r"(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})"
_ENTRY = re.compile(rf"({_SUITE})@({_SEMVER})#([0-9a-f]{{64}})", re.ASCII)
_COMMENT = re.compile(r"#[\x20-\x7e]*", re.ASCII)
_SHA = re.compile(r"[0-9a-f]{64}", re.ASCII)
_FILE_KEYS = frozenset({"path", "size", "sha256"})


def _mismatch(detail: str) -> Refused:
    return Refused("testset_mismatch", detail_code=detail)


# ---------------------------------------------------------------- the lock


@dataclass(frozen=True, slots=True)
class LockEntry:
    """One locked split: ``<suite>@<version>#<sha256>``. The optional version ids pin
    ``testset.json`` and ``testset.sig`` once a fetch has found them; the committed lock
    carries only the hash."""

    suite: str
    version: str
    sha256: str
    testset_version_id: str | None = None
    sig_version_id: str | None = None

    def __post_init__(self) -> None:
        text = f"{self.suite}@{self.version}#{self.sha256}" if _all_str(self) else None
        if text is None or _ENTRY.fullmatch(text) is None:
            raise Refused("lock_invalid")
        for vid in (self.testset_version_id, self.sig_version_id):
            if vid is not None:
                names.check_store_version_id(vid)

    @property
    def text(self) -> str:
        return f"{self.suite}@{self.version}#{self.sha256}"

    @classmethod
    def parse(cls, text: object) -> LockEntry:
        if not isinstance(text, str):
            raise Refused("lock_invalid")
        m = _ENTRY.fullmatch(text)
        if m is None:
            raise Refused("lock_invalid")
        return cls(m.group(1), m.group(2), m.group(3))


def _all_str(e: LockEntry) -> bool:
    return all(isinstance(v, str) for v in (e.suite, e.version, e.sha256))


def parse_lock(raw: bytes) -> dict[tuple[str, str], LockEntry]:
    """The entries of ``models/testsets.lock``, keyed by (suite, version)."""
    if not isinstance(raw, (bytes, bytearray)) or len(raw) > MAX_LOCK_BYTES:
        raise Refused("lock_invalid")
    if raw and not raw.endswith(b"\n"):
        raise Refused("lock_invalid")
    failed = False
    try:
        text = bytes(raw).decode("ascii")
    except UnicodeDecodeError:
        failed = True
    if failed:
        raise Refused("lock_invalid")
    out: dict[tuple[str, str], LockEntry] = {}
    for line in text.split("\n")[:-1] if text else []:
        if line.startswith("#"):
            if _COMMENT.fullmatch(line) is None:
                raise Refused("lock_invalid")
            continue
        entry = LockEntry.parse(line)
        if (entry.suite, entry.version) in out:
            raise Refused("lock_invalid")
        out[(entry.suite, entry.version)] = entry
    return out


def read_lock(path: Path | str) -> dict[tuple[str, str], LockEntry]:
    """Read and parse ``models/testsets.lock`` (no links); any failure is ``lock_invalid``."""
    failed = False
    try:
        raw = files.read_regular(path, MAX_LOCK_BYTES)
    except Refused:
        failed = True
    if failed:
        raise Refused("lock_invalid")
    return parse_lock(raw)


# ---------------------------------------------------------------- the locked manifest


def _check_suite(suite: object, version: object) -> tuple[str, str]:
    if not isinstance(suite, str) or suite not in SUITES:
        raise Refused("no_suite")
    if not isinstance(version, str):
        raise Refused("invalid_name")
    return suite, names.check_version(version)


def check_locked(raw: bytes, entry: LockEntry) -> dict[str, tuple[int, str, str]]:
    """The files of a locked ``testset.json`` (path -> size, SHA-256, store version id),
    after checking that it is strict canonical JSON naming ``entry``'s suite and version,
    purpose ``test``, ``locked: true``, and exactly one version id per listed file.
    Anything else is ``testset_mismatch``."""
    failed = False
    try:
        obj = canonical.loads_strict(raw, MAX_TESTSET_BYTES)
        if canonical.dumps(obj) != bytes(raw):
            failed = True
    except Refused:
        failed = True
    if failed or not isinstance(obj, dict):
        raise _mismatch("json")
    if (
        obj.get("schema") != sets.TESTSET_SCHEMA
        or obj.get("name") != entry.suite
        or obj.get("version") != entry.version
        or obj.get("purpose") != "test"
        or obj.get("locked") is not True
        or not isinstance(obj.get("extraction_schema_version"), str)
    ):
        raise _mismatch("identity")
    listed = obj.get("files")
    vids = obj.get("store_version_ids")
    if not isinstance(listed, list) or not isinstance(vids, dict) or len(listed) > MAX_FILES:
        raise _mismatch("files")
    out: dict[str, tuple[int, str, str]] = {}
    for f in listed:
        if not isinstance(f, dict) or set(f) != _FILE_KEYS:
            raise _mismatch("files")
        path, size, sha = f["path"], f["size"], f["sha256"]
        if not isinstance(path, str) or path in out or path in ("testset.json", "testset.sig"):
            raise _mismatch("files")
        if (
            not isinstance(size, int)
            or isinstance(size, bool)
            or not 0 <= size <= MAX_FILE_BYTES
            or not isinstance(sha, str)
            or _SHA.fullmatch(sha) is None
        ):
            raise _mismatch("files")
        bad = False
        try:
            store.eval_file_key(entry.suite, entry.version, path)
            vid = names.check_store_version_id(vids.get(path))  # type: ignore[arg-type]
        except (Refused, TypeError):
            bad = True
        if bad:
            raise _mismatch("files")
        out[path] = (size, sha, vid)
    if set(vids) != set(out):
        raise _mismatch("files")
    return out


# ---------------------------------------------------------------- build


@dataclass(frozen=True, slots=True)
class Built:
    """What a build returns. Only :meth:`export` (aggregates) may leave the job."""

    entry: LockEntry
    extraction_schema_version: str
    slices: dict[str, dict[str, int]]
    totals: dict[str, int]
    scratch_removed: bool = True

    def export(self) -> dict[str, Any]:
        return {
            "sha256": self.entry.sha256,
            "extraction_schema_version": self.extraction_schema_version,
            "slices": self.slices,
            "totals": self.totals,
        }


def _new_scratch(parent: Path | str | None) -> Path:
    """A fresh folder, mode 0700 (``mkdtemp``), under ``parent`` or the system default.
    Any failure is ``not_allowed_here``/``scratch``, raised outside the handler so no
    exception in its chain carries a path."""
    failed = False
    try:
        path = Path(tempfile.mkdtemp(prefix="mhai-testset-", dir=parent))
        os.chmod(path, 0o700)
    except (OSError, TypeError, ValueError):
        failed = True
    if failed:
        raise Refused("not_allowed_here", detail_code="scratch")
    return path


def _remove_scratch(path: Path) -> bool:
    """Remove a scratch folder; on failure log a fixed code (never the path) and return
    ``False``. A killed process skips this altogether: the folder (mode 0700, under the
    configured scratch root) then stays until the job's runner is discarded."""
    shutil.rmtree(path, ignore_errors=True)
    gone = not os.path.lexists(path)
    if not gone:
        log.error("test split scratch not removed: code=scratch_not_removed")
    return gone


# A payload that can never be a locked manifest (check_locked needs the testset schema):
# signed and verified before the claim, to prove the signer holds the trusted key.
_PROBE = canonical.dumps({"schema": "mhai.models.testset_signer_probe/1"})


def _trusted(signer: signing.Signer, policy: signing.TrustPolicy) -> None:
    """The signer's key must be trusted for ``testset`` and its signatures must verify
    under the policy (a key id that names a trusted key but a private key that doesn't
    match it is ``signature_invalid``)."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    if not any((k.key_id, k.alg) == (signer.key_id, signer.alg) for k in policy.signers[PURPOSE]):
        raise Refused("signer_untrusted")
    signing.Verifier().verify(PURPOSE, _PROBE, signer.sign(PURPOSE, _PROBE), policy)


def _read_scratch(path: Path, cap: int) -> bytes:
    """A scratch file's bytes. A refusal is re-raised fresh (outside the handler) with a
    fixed detail: a ``FileRefused`` names the file, and file names carry document ids."""
    reason: str | None = None
    try:
        return files.read_regular(path, cap)
    except Refused as e:
        reason = e.reason
    if reason is None:  # unreachable; an explicit check, since ``assert`` is stripped by -O
        raise Refused("file_not_allowed", detail_code="scratch")
    raise Refused(reason, detail_code="scratch")


def _read_set_manifest(root: Path, expected_sha: str) -> dict[str, Any]:
    raw = _read_scratch(root / "testset.json", MAX_TESTSET_BYTES)
    if hashlib.sha256(raw).hexdigest() != expected_sha:
        raise Refused("hash_mismatch", detail_code="testset")
    obj = canonical.loads_strict(raw, MAX_TESTSET_BYTES)
    if not isinstance(obj, dict) or obj.get("purpose") != "test" or "locked" in obj:
        raise Refused("manifest_invalid", detail_code="testset")
    if not isinstance(obj.get("files"), list) or "store_version_ids" in obj:
        raise Refused("manifest_invalid", detail_code="testset")
    return obj


def build_and_lock(
    target: ObjectStore,
    signer: signing.Signer,
    policy: signing.TrustPolicy,
    *,
    suite: str,
    version: str,
    scale: float,
    retention_days: int,
    scratch_parent: Path | str | None = None,
) -> Built:
    """Build the ``test`` split of ``suite`` ``version``, upload it, sign it, lock it.

    See the module docstring for the order. The names, the signer (trusted for
    ``testset``, and holding that key) and the scratch folder are all checked before the
    claim, so none of them can burn a version. Raises :class:`Refused`; a failed build
    leaves no scratch copy and no ``testset.json`` in the store (files already uploaded
    stay, locked, under a version that can never be locked again: its claim is taken).
    ``Built.scratch_removed`` is ``False`` if the scratch copy could not be removed."""
    suite, version = _check_suite(suite, version)
    _trusted(signer, policy)
    scratch = _new_scratch(scratch_parent)
    removed = False
    try:
        claim = canonical.dumps(
            {"schema": CLAIM_SCHEMA, "suite": suite, "version": version, "signer": signer.key_id}
        )
        target.put_new(store.testset_claim_key(suite, version), claim, retention_days)
        error: str | None = None
        try:
            made = sets.build("test", None, scratch, scale, version=version)
        except sets.SetError as e:
            error = str(e)
        if error is not None:
            raise Refused("evaluation_failed", detail_code=f"build_{error}")
        scan = synth_scanner.scan_set(made.root)
        if not scan.ok:
            rules = sorted({f.rule for f in scan.findings})
            log.error(
                "test split refused by the scanner: findings=%d rules=%s", len(scan.findings), rules
            )
            raise Refused("dataset_not_synthetic", detail_code="scanner")
        manifest = _read_set_manifest(made.root, made.sha256)
        vids: dict[str, str] = {}
        for f in manifest["files"]:
            path, size, sha = f["path"], f["size"], f["sha256"]
            key = store.eval_file_key(suite, version, path)
            data = _read_scratch(made.root / path, MAX_FILE_BYTES)
            if len(data) != size or hashlib.sha256(data).hexdigest() != sha:
                raise Refused("hash_mismatch", detail_code="scratch")
            vids[path] = target.put_new(key, data, retention_days)
        locked = dict(manifest, locked=True, store_version_ids=vids)
        raw = canonical.dumps(locked)
        entry = LockEntry(suite, version, hashlib.sha256(raw).hexdigest())
        check_locked(raw, entry)
        sig = signer.sign(PURPOSE, raw)
        signing.Verifier().verify(PURPOSE, raw, sig, policy)
        tvid = target.put_new(store.testset_key(suite, version), raw, retention_days)
        svid = target.put_new(store.testset_sig_key(suite, version), sig.to_bytes(), retention_days)
    finally:
        removed = _remove_scratch(scratch)
    built = Built(
        entry=LockEntry(suite, version, entry.sha256, tvid, svid),
        extraction_schema_version=str(manifest["extraction_schema_version"]),
        slices=manifest["slices"],
        totals=manifest["totals"],
        scratch_removed=removed,
    )
    log.info(
        "test split locked: suite=%s version=%s documents=%s pages=%s files=%d sha256=%s",
        suite,
        version,
        built.totals.get("documents"),
        built.totals.get("pages"),
        len(vids),
        entry.sha256,
    )
    return built


# ---------------------------------------------------------------- fetch


def _store_refusal(e: Refused) -> bool:
    """A refusal that is the store's (or the caller's role's), not the object's: it
    propagates instead of meaning "this version doesn't match"."""
    return e.reason == "store_unavailable" and e.detail_code != "checksum"


def _get_checked(source: ObjectStore, key: str, vid: str, size: int, sha: str) -> bytes:
    """One object version by id, its size and SHA-256 checked; a missing, oversized or
    changed one is ``testset_mismatch``."""
    failed = False
    try:
        data = source.get(key, vid, max_bytes=size)
    except Refused as e:
        if _store_refusal(e):
            raise
        failed = True
    if failed or len(data) != size or hashlib.sha256(data).hexdigest() != sha:
        raise _mismatch("file")
    return data


def _oldest_first(source: ObjectStore, key: str) -> list[str]:
    """Every version id of ``key``, oldest first (the store lists newest first; its own
    listing cap, ``too_many_files``, bounds the count). None is ``testset_mismatch``."""
    failed = False
    try:
        vids = source.versions(key)
    except Refused as e:
        if _store_refusal(e):
            raise
        failed = True
    if failed:
        raise _mismatch("versions")
    if not vids:
        raise _mismatch("missing")
    return list(reversed(vids))


def _find_testset(source: ObjectStore, entry: LockEntry, key: str) -> bytes:
    """The locked ``testset.json``: by recorded version id, else the oldest stored version
    whose SHA-256 is the lock's. Versions are filtered by hash first, so other versions
    (shadows with other bytes, or that the store cannot vouch for) are ignored, and copies
    with the same bytes are one candidate (the oldest is taken; ``get`` re-hashes it)."""
    if entry.testset_version_id is not None:
        candidate: str | None = entry.testset_version_id
    else:
        candidate = None
        for vid in _oldest_first(source, key):
            skip = False
            try:
                info = source.head(key, vid)
            except Refused as e:
                if _store_refusal(e):
                    raise
                skip = True  # a version the store cannot vouch for is never the locked one
            if not skip and info.sha256 == entry.sha256 and info.size <= MAX_TESTSET_BYTES:
                candidate = vid
                break
        if candidate is None:
            raise _mismatch("lock_hash")
    failed = False
    try:
        raw = source.get(key, candidate, max_bytes=MAX_TESTSET_BYTES)
    except Refused as e:
        if _store_refusal(e):
            raise
        failed = True
    if failed or hashlib.sha256(raw).hexdigest() != entry.sha256:
        raise _mismatch("lock_hash")
    return raw


def _check_signature(
    source: ObjectStore, entry: LockEntry, key: str, raw: bytes, policy: signing.TrustPolicy
) -> None:
    """A ``testset.sig`` version that verifies over ``raw`` under ``policy``: the recorded
    one, else the oldest that does. Versions are deduplicated by their bytes, and a
    version that doesn't verify (junk, another purpose, an untrusted key) is ignored; at
    most :data:`MAX_VERSIONS` distinct signatures or failed reads are tried. None is
    ``testset_mismatch`` (detail ``signer_untrusted`` when every one was by an untrusted
    key)."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    vids = (
        [entry.sig_version_id] if entry.sig_version_id is not None else _oldest_first(source, key)
    )
    reasons: set[str] = set()
    seen: set[bytes] = set()
    failed = 0  # reads that failed count toward the cap too, so gets stay bounded
    verifier = signing.Verifier()
    for vid in vids:
        if len(seen) + failed >= MAX_VERSIONS:
            break
        try:
            sig = source.get(key, vid, max_bytes=signing.MAX_SIGNATURE_DOC_BYTES)
        except Refused as e:
            if _store_refusal(e):
                raise
            reasons.add(e.reason)
            failed += 1
            continue
        if sig in seen:
            continue
        seen.add(sig)
        try:
            verifier.verify(PURPOSE, raw, sig, policy)
        except Refused as e:
            if e.reason == "policy_invalid":
                raise
            reasons.add(e.reason)
            continue
        return
    if reasons == {"signer_untrusted"}:
        raise _mismatch("signer_untrusted")
    raise _mismatch("signature")


def _write_new(root: Path, rel: str, data: bytes) -> None:
    """Write ``root/rel`` once (``O_EXCL``, no links followed), parents made 0700. Any
    failure is a fixed refusal raised outside the handler: an ``OSError`` names the path,
    and paths carry document ids."""
    parts = rel.split("/")
    folder = root
    detail: str | None = None
    try:
        for part in parts[:-1]:
            folder = folder / part
            try:
                os.mkdir(folder, 0o700)
            except FileExistsError:
                if folder.is_symlink() or not folder.is_dir():
                    detail = "path"
                    break
        if detail is None:
            flags = (
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
            )
            fd = os.open(folder / parts[-1], flags, 0o600)
            with os.fdopen(fd, "wb") as fh:
                fh.write(data)
    except OSError:
        detail = "write"
    if detail == "path":
        raise _mismatch("path")
    if detail is not None:
        raise Refused("file_not_allowed", detail_code=detail)


def fetch(
    source: ObjectStore, lock_entry: LockEntry, policy: signing.TrustPolicy, scratch: Path | str
) -> Path:
    """Download the locked split named by ``lock_entry`` into a new 0700 folder under
    ``scratch`` and return it. See the module docstring; any mismatch is
    ``testset_mismatch`` and leaves nothing behind. Only the files the signed manifest
    lists are read: other objects under the split's prefix are ignored."""
    if not isinstance(lock_entry, LockEntry):
        raise Refused("lock_invalid")
    suite, version = _check_suite(lock_entry.suite, lock_entry.version)
    raw = _find_testset(source, lock_entry, store.testset_key(suite, version))
    _check_signature(source, lock_entry, store.testset_sig_key(suite, version), raw, policy)
    listed = check_locked(raw, lock_entry)
    failed = False
    try:
        Path(scratch).mkdir(mode=0o700, parents=True, exist_ok=True)
    except (OSError, TypeError, ValueError):
        failed = True
    if failed:
        raise Refused("not_allowed_here", detail_code="scratch")
    out = _new_scratch(scratch)
    done = False
    try:
        for path, (size, sha, vid) in sorted(listed.items()):
            data = _get_checked(source, store.eval_file_key(suite, version, path), vid, size, sha)
            _write_new(out, path, data)
        _write_new(out, "testset.json", raw)
        done = True
    finally:
        if not done:
            _remove_scratch(out)
    log.info(
        "test split fetched: suite=%s version=%s files=%d sha256=%s",
        suite,
        version,
        len(listed),
        lock_entry.sha256,
    )
    return out


__all__ = [
    "Built",
    "LockEntry",
    "build_and_lock",
    "check_locked",
    "fetch",
    "parse_lock",
    "read_lock",
]
