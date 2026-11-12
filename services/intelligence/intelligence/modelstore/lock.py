"""``models.lock`` (``mhai.models.lock/1``): the production pins, ``pin``, ``lock-check``
and ``rollback`` (plan Task 20, design sections 7.4, 7.6 and 8).

The lock
--------
Canonical JSON (:mod:`intelligence.modelstore.canonical`) followed by one newline (the
repository's end-of-file rule), nothing else::

    {"models":{"<role>":{"gate","manifest_sha256","name","version"}},"schema":"mhai.models.lock/1"}

A role matches the role pattern, a name, version and hash their patterns, and the gate is
one of :data:`intelligence.doceval.report.GATES`; anything else, a non-canonical body or
unknown keys, is ``lock_invalid``. The production lock (the repository's ``models.lock``)
also needs a known role (one with an evaluation suite, :data:`ROLES`), a production gate
and no ``fixture-`` name (:func:`check_production`, P14). It changes only by a normal PR,
and **one reviewer approving that PR is the sign-off**: there is no separate sign-off
record or workflow (TODO.md, "Before scale").

``pin``
-------
:func:`pin` writes one role's entry. Before writing it:

* names first (role, name, version, gate), then P14 for the production lock;
* the manifest: of the stored versions of ``manifest.json``, the one whose signature
  verifies for purpose ``manifest`` (never "latest"; two different signed manifests are
  ``lock_mismatch``/``ambiguous``);
* Task 22's verifier, :func:`intelligence.modelstore.verify.verify_pin` (signature, every
  file, the gate's ``pass`` in the manifest);
* :func:`check_evaluation`: the split is ``test`` and its test set is the entry in
  ``models/testsets.lock``; ``evaluation.json`` (read by its recorded version id, its
  SHA-256 the manifest's) agrees with the manifest's gates, says ``pass`` for the gate,
  names the same split and test set, and was measured against the committed thresholds
  file for that gate.

``lock-check``
--------------
:func:`lock_check` is an ordinary CI check on every PR (``model-lock.yml``). It compares
the lock on ``main`` (the base) with the PR's lock (the head). Every head entry must pass
:func:`check_production`, and each changed role is checked against the store:

* **new version** (no base pin, another model, or a later version of the same model):
  verified as for ``pin``, evaluation included;
* **rollback** (an earlier version of the base pin's model whose exact entry ``main``
  pinned for this role before, from ``main``'s ``models.lock`` history): verified (signature, every
  file's hash, the gate's ``pass`` in the signed manifest, measured on the ``test``
  split). Its evaluation was checked
  when it was first pinned, and a promotion signs only what it evaluated, so it is not
  re-run against today's thresholds;
* **removed role:** nothing to verify; the reviewer decides.

Two roles whose manifests pin the same model at different versions or hashes (directly or
as a dependency) are ``lock_invalid``/``dependencies``. A lock with no changed role needs
no store access. The trust policy, ``models/testsets.lock`` and the thresholds are the
caller's: the workflow passes ``main``'s trust policy when ``main`` has one, so a PR can't
trust its own new key in the same change that pins with it (a key change goes in its own
PR first).

``rollback``
------------
:func:`rollback` pins a role back to the latest older version of its pinned name (semver
order) that verifies for this role and gate (:func:`intelligence.modelstore.verify.
verify_pin`) and passes :func:`check_production`, or to ``--to``, which must be one of
those; the lock is written with :func:`write`, and the change goes in a normal PR.

A promoted base model
---------------------
:func:`base_verifier` is what ``training.check`` calls for a base named by its pin
``<name>@<version>#<manifest sha256>``: Task 22's verifier, the role taken from that
manifest, no gate. A base that fails is ``base_unverified``, the inner reason as its
detail code (a store outage stays ``store_unavailable``).
"""

from __future__ import annotations

import hashlib
import os
import re
import secrets
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Any, Protocol

from intelligence.doceval.report import GATES, PRODUCTION_GATES, THRESHOLDS_DIR
from intelligence.doceval.report import load_thresholds as _load_thresholds
from intelligence.doceval.report import parse_report as _parse_report
from intelligence.modelstore import canonical, files, manifest, names, signing, store, verify
from intelligence.modelstore.promote import FIXTURE_PREFIX, SUITES
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.testsets import LockEntry as TestsetEntry

SCHEMA = "mhai.models.lock/1"
MAX_BYTES = 64 * 1024
MAX_ROLES = 16
# The roles a production lock may pin: those with an evaluation suite (promote's
# ``SUITES``), since only they can pass a gate.
ROLES: frozenset[str] = frozenset(SUITES)
EVALUATION = "evaluation.json"
MAX_REPORT_BYTES = 8 * 1024 * 1024
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)


def _invalid(detail: str) -> Refused:
    return Refused("lock_invalid", detail_code=detail)


# ---------------------------------------------------------------- the lock


@dataclass(frozen=True, slots=True)
class Pin:
    """One role's entry (:class:`intelligence.modelstore.verify.PinEntry`)."""

    name: str
    version: str
    manifest_sha256: str
    gate: str

    def __post_init__(self) -> None:
        if not _ok(names.check_name, self.name):
            raise _invalid("name")
        if not _ok(names.check_version, self.version):
            raise _invalid("version")
        if not isinstance(self.manifest_sha256, str) or not _SHA256.fullmatch(self.manifest_sha256):
            raise _invalid("manifest_sha256")
        if not isinstance(self.gate, str) or self.gate not in GATES:
            raise _invalid("gate")

    def to_obj(self) -> dict[str, str]:
        return {
            "gate": self.gate,
            "manifest_sha256": self.manifest_sha256,
            "name": self.name,
            "version": self.version,
        }


def _ok(check: Callable[[str], str], value: object) -> bool:
    try:
        check(value)  # type: ignore[arg-type]
    except Refused:
        return False
    return True


@dataclass(frozen=True, slots=True)
class Lock:
    models: Mapping[str, Pin]

    def __post_init__(self) -> None:
        if not isinstance(self.models, Mapping) or len(self.models) > MAX_ROLES:
            raise _invalid("models")
        for role, entry in self.models.items():
            if not _ok(names.check_role, role):
                raise _invalid("role")
            if not isinstance(entry, Pin):
                raise _invalid("entry")
        object.__setattr__(self, "models", MappingProxyType(dict(sorted(self.models.items()))))

    def with_pin(self, role: str, entry: Pin) -> Lock:
        return Lock({**self.models, role: entry})


_PIN_KEYS = frozenset({"name", "version", "manifest_sha256", "gate"})


def parse(raw: bytes) -> Lock:
    """A lock's bytes: canonical JSON and one newline; anything else is ``lock_invalid``."""
    if not isinstance(raw, (bytes, bytearray)) or len(raw) > MAX_BYTES:
        raise _invalid("size")
    raw = bytes(raw)
    if not raw.endswith(b"\n"):
        raise _invalid("newline")
    body = raw[:-1]
    failed = False
    obj: Any = None
    try:
        obj = canonical.loads_strict(body, MAX_BYTES)
        failed = canonical.dumps(obj) != body
    except Refused:
        failed = True
    if failed:
        raise _invalid("not_canonical")
    if not isinstance(obj, dict) or set(obj) != {"schema", "models"} or obj["schema"] != SCHEMA:
        raise _invalid("schema")
    models = obj["models"]
    if not isinstance(models, dict):
        raise _invalid("models")
    out: dict[str, Pin] = {}
    for role, entry in models.items():
        if not isinstance(entry, dict) or set(entry) != _PIN_KEYS:
            raise _invalid("entry")
        out[role] = Pin(entry["name"], entry["version"], entry["manifest_sha256"], entry["gate"])
    return Lock(out)


def to_bytes(lock: Lock) -> bytes:
    obj = {"schema": SCHEMA, "models": {r: e.to_obj() for r, e in lock.models.items()}}
    return canonical.dumps(obj) + b"\n"


def empty() -> Lock:
    return Lock({})


def read(path: Path | str) -> Lock:
    """Read a lock file (a regular file, never through a link); unreadable is
    ``lock_invalid``."""
    failed = False
    raw = b""
    try:
        raw = files.read_regular(path, MAX_BYTES)
    except Refused:
        failed = True
    if failed:
        raise _invalid("read")
    return parse(raw)


def write(path: Path | str, lock: Lock) -> None:
    """Write ``lock`` to ``path``: a new file next to it (created exclusively), synced, then
    renamed over it."""
    path = Path(path)
    data = to_bytes(lock)
    tmp = path.with_name(f".{path.name}.{secrets.token_hex(8)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def check_rehearsal(rehearsal: bool) -> None:
    """A rehearsal (``--rehearsal``: fixture names and the ``rehearsal`` gate allowed) runs
    in development only, both switches (:func:`intelligence.modelstore.verify.
    is_development`); anywhere else it is ``not_allowed_here``/``rehearsal``. Production
    is the default and needs no flag."""
    if rehearsal is not False and (rehearsal is not True or not verify.is_development()):
        raise Refused("not_allowed_here", detail_code="rehearsal")


def check_production(role: str, entry: Pin, *, rehearsal: bool = False) -> None:
    """P14: a production pin names a known role, a production gate and no fixture. A
    rehearsal (development only, :func:`check_rehearsal`) still needs a known role, and
    allows a ``fixture-`` name and the ``rehearsal`` gate (Task 24's CI rehearsal)."""
    check_rehearsal(rehearsal)
    if role not in ROLES:
        raise _invalid("role")
    if rehearsal:
        return
    if entry.name.startswith(FIXTURE_PREFIX):
        raise Refused("fixture_name")
    if entry.gate not in PRODUCTION_GATES:
        raise Refused("gate_failed", detail_code="not_production")


# ---------------------------------------------------------------- the manifest to pin


def _read_version(source: verify.ReadStore, key: str, vid: str, max_bytes: int) -> bytes | None:
    try:
        return source.get(key, vid, max_bytes=max_bytes)
    except Refused as e:
        if e.reason == "store_unavailable" and e.detail_code != "checksum":
            raise
    return None


def find_signed_manifest(
    source: verify.ReadStore, name: str, version: str, policy: signing.TrustPolicy
) -> str:
    """The SHA-256 of the one stored ``manifest.json`` of ``name@version`` whose signature
    verifies for purpose ``manifest``. Picked by signature, never by "latest"."""
    verify.check_environment(policy)
    key = store.manifest_key(name, version)
    vids = source.versions(key)
    if not vids:
        raise Refused("not_found", detail_code="manifest")
    docs: list[bytes] = []
    for reads, vid in enumerate(reversed(vids)):
        if reads >= verify.MAX_VERSION_READS:
            raise Refused("lock_mismatch", detail_code="too_many_versions")
        raw = _read_version(source, key, vid, manifest.MAX_BYTES)
        if raw is None or raw in docs:
            continue
        if len(docs) >= verify.MAX_VERSIONS:
            raise Refused("lock_mismatch", detail_code="too_many_versions")
        docs.append(raw)
    signed: list[bytes] = []
    refusal: Refused | None = None
    for raw in docs:
        try:
            verify.check_manifest_signature(source, name, version, raw, policy)
        except Refused as e:
            if e.reason not in ("signature_invalid", "signer_untrusted"):
                raise
            refusal = e
            continue
        signed.append(raw)
    if len(signed) > 1:
        raise Refused("lock_mismatch", detail_code="ambiguous")
    if not signed:
        raise refusal or Refused("signature_invalid", detail_code="missing")
    return hashlib.sha256(signed[0]).hexdigest()


def check_evaluation(
    source: verify.ReadStore,
    m: manifest.Manifest,
    gate: str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    thresholds_dir: Path | str = THRESHOLDS_DIR,
) -> None:
    """The evaluation behind a new pin (see the module docstring). ``testsets_lock`` and
    ``thresholds_dir`` are the committed copies."""
    ev = m.evaluation
    if ev.split != "test":
        raise Refused("testset_mismatch", detail_code="split")
    named = TestsetEntry.parse(ev.testset)
    locked = testsets_lock.get((named.suite, named.version))
    if locked is None or not secrets.compare_digest(locked.sha256, named.sha256):
        raise Refused("testset_mismatch", detail_code="not_locked")
    doc = m.documents[EVALUATION]
    if not secrets.compare_digest(doc.sha256, ev.report_sha256):
        raise Refused("hash_mismatch", detail_code="evaluation")
    key = store.run_doc_key(m.name, m.version, m.run.id, EVALUATION)
    raw = _read_version(source, key, doc.store_version_id, MAX_REPORT_BYTES)
    if raw is None:
        raise Refused("missing_file", detail_code="evaluation")
    if not secrets.compare_digest(hashlib.sha256(raw).hexdigest(), doc.sha256):
        raise Refused("hash_mismatch", detail_code="evaluation")
    failed = False
    rep: dict[str, Any] = {}
    try:
        rep = _parse_report(raw)
    except Refused:
        failed = True
    if failed:
        raise Refused("evaluation_failed", detail_code="report")
    if rep["split"]["name"] != "test":
        raise Refused("testset_mismatch", detail_code="report_split")
    ts = rep["testset"]
    if f"{ts['name']}@{ts['version']}#{ts['sha256']}" != ev.testset:
        raise Refused("testset_mismatch", detail_code="report_testset")
    verdicts = {g: v["verdict"] for g, v in rep["verdicts"].items()}
    if verdicts != dict(ev.gates):
        raise Refused("gate_failed", detail_code="disagree")
    if verdicts.get(gate) != "pass":
        raise Refused("gate_failed")
    if rep["thresholds"][gate]["sha256"] != thresholds_sha256(thresholds_dir, gate):
        raise Refused("gate_failed", detail_code="thresholds")


def check_test_split(m: manifest.Manifest) -> None:
    """A rollback's manifest must have been measured on the locked ``test`` split (its
    full evaluation was checked when it was first pinned)."""
    if m.evaluation.split != "test":
        raise Refused("testset_mismatch", detail_code="split")


def thresholds_sha256(thresholds_dir: Path | str, gate: str) -> str:
    """The SHA-256 of ``<thresholds_dir>/<gate>.json``, checked as a thresholds file for
    that gate (``policy_invalid`` otherwise)."""
    if gate not in GATES:
        raise _invalid("gate")
    t = _load_thresholds(Path(thresholds_dir) / f"{gate}.json")
    if t.gate != gate:
        raise Refused("policy_invalid", detail_code="thresholds")
    return t.sha256


# ---------------------------------------------------------------- pin


def _check_names(role: object, name: object, version: object, gate: object) -> None:
    names.check_role(role)  # type: ignore[arg-type]
    names.check_name(name)  # type: ignore[arg-type]
    names.check_version(version)  # type: ignore[arg-type]
    if not isinstance(gate, str) or gate not in GATES:
        raise _invalid("gate")


def pin(
    role: str,
    name: str,
    version: str,
    gate: str,
    *,
    source: verify.ReadStore,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    lock_path: Path | str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    production: bool = True,
    thresholds_dir: Path | str = THRESHOLDS_DIR,
) -> Pin:
    """Verify ``name@version`` for ``role`` and ``gate`` and write it into the lock at
    ``lock_path``; returns the entry written. Raises :class:`Refused` on any failure, and
    then the lock is unchanged."""
    _check_names(role, name, version, gate)
    if production:
        if role not in ROLES:
            raise _invalid("role")
        if name.startswith(FIXTURE_PREFIX):
            raise Refused("fixture_name")
        if gate not in PRODUCTION_GATES:
            raise Refused("gate_failed", detail_code="not_production")
    current = read(lock_path)
    sha = find_signed_manifest(source, name, version, policy)
    entry = Pin(name, version, sha, gate)
    verified = verify.verify_pin(source, role, entry, policy, cache_dir)
    check_evaluation(source, verified.manifest, gate, testsets_lock, thresholds_dir)
    write(lock_path, current.with_pin(role, entry))
    return entry


# ---------------------------------------------------------------- lock-check


def changed_roles(base: Lock, head: Lock) -> list[str]:
    roles = set(base.models) | set(head.models)
    return sorted(r for r in roles if base.models.get(r) != head.models.get(r))


def check_dependencies(head: Lock, manifests: Mapping[str, manifest.Manifest]) -> None:
    """No two roles may pin one model at different versions or hashes, directly or as a
    dependency."""
    seen: dict[str, tuple[str, str]] = {}

    def note(name: str, version: str, sha: str) -> None:
        if seen.setdefault(name, (version, sha)) != (version, sha):
            raise _invalid("dependencies")

    for entry in head.models.values():
        note(entry.name, entry.version, entry.manifest_sha256)
    for m in manifests.values():
        for d in m.dependencies:
            note(d.name, d.version, d.manifest_sha256)


def pinned_before(history: Iterable[bytes]) -> frozenset[tuple[str, Pin]]:
    """Every ``(role, entry)`` a lock in ``history`` (the locks ``main`` has held, from
    git) pinned. A lock that doesn't parse is ``lock_invalid``/``history``."""
    out: set[tuple[str, Pin]] = set()
    for raw in history:
        failed = False
        try:
            past = parse(raw)
        except Refused:
            failed = True
        if failed:
            raise _invalid("history")
        out.update(past.models.items())
    return frozenset(out)


def _is_rollback(
    old: Pin | None, role: str, entry: Pin, before: frozenset[tuple[str, Pin]]
) -> bool:
    """An earlier version of the model the base pins for this role, whose exact entry
    (name, version, manifest hash, gate) ``main`` pinned for this role before. Anything
    else is a new version and gets the full evaluation."""
    return (
        old is not None
        and old.name == entry.name
        and names.Version.parse(entry.version) < names.Version.parse(old.version)
        and (role, entry) in before
    )


def lock_check(
    base_raw: bytes,
    head_raw: bytes,
    *,
    source: verify.ReadStore,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    thresholds_dir: Path | str = THRESHOLDS_DIR,
    rehearsal: bool = False,
    history: Iterable[bytes] = (),
) -> dict[str, str]:
    """Check a PR's change to ``models.lock`` (see the module docstring); returns
    ``{role: new|rollback|removed}``. ``base_raw`` is ``main``'s lock (the empty lock
    when ``main`` has none), ``head_raw`` the PR's, ``history`` every lock ``main`` has
    held (a rollback must be to an entry pinned there before). ``rehearsal`` (development only)
    allows fixture names and the rehearsal gate (:func:`check_production`). Raises
    :class:`Refused` on any failure."""
    check_rehearsal(rehearsal)
    base, head = parse(base_raw), parse(head_raw)
    before = pinned_before(history)
    for role, pinned in head.models.items():
        check_production(role, pinned, rehearsal=rehearsal)
    roles = changed_roles(base, head)
    if not roles:
        return {}
    verify.check_environment(policy)
    out: dict[str, str] = {}
    manifests: dict[str, manifest.Manifest] = {}
    for role in roles:
        entry = head.models.get(role)
        if entry is None:
            out[role] = "removed"
            continue
        m = verify.verify_pin(source, role, entry, policy, cache_dir).manifest
        manifests[role] = m
        if _is_rollback(base.models.get(role), role, entry, before):
            check_test_split(m)
            out[role] = "rollback"
            continue
        check_evaluation(source, m, entry.gate, testsets_lock, thresholds_dir)
        out[role] = "new"
    for role, pinned in head.models.items():
        if role not in manifests:
            raw = verify.find_manifest(source, pinned.name, pinned.version, pinned.manifest_sha256)
            manifests[role] = manifest.parse(raw)
    check_dependencies(head, manifests)
    return out


# ---------------------------------------------------------------- rollback


class ListStore(verify.ReadStore, Protocol):
    """The reads ``rollback`` makes: the verifier's, and listing a model's prefix."""

    def list(self, prefix: str) -> list[str]: ...


@dataclass(frozen=True, slots=True)
class RollbackResult:
    role: str
    previous: Pin
    entry: Pin


def rollback_candidates(
    source: ListStore,
    role: str,
    current: Pin,
    *,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    only: str | None = None,
    first: bool = False,
    rehearsal: bool = False,
) -> list[Pin]:
    """The versions ``role`` may roll back to, latest first: versions of the pinned name
    older than ``current`` (semver order, never string order) with a signed manifest
    (:func:`find_signed_manifest`) that passes :func:`check_production` with the current
    gate and verifies for this role (:func:`verify.verify_pin`: signature, every file,
    the gate's ``pass``). Being in the store is not enough (P16). ``only`` looks at that
    one version, ``first`` stops at the latest candidate (each check downloads and hashes
    a whole version). ``rehearsal`` (development only) relaxes :func:`check_production`."""
    names.check_role(role)
    check_rehearsal(rehearsal)
    verify.check_environment(policy)
    name = current.name
    now = names.Version.parse(current.version)
    prefix = f"models/{name}/"
    versions: set[str] = set()
    for key in source.list(prefix):
        parts = key[len(prefix) :].split("/")
        if len(parts) == 2 and parts[1] == "manifest.json" and _ok(names.check_version, parts[0]):
            versions.add(parts[0])
    older = sorted(
        (v for v in versions if names.Version.parse(v) < now),
        key=names.Version.parse,
        reverse=True,
    )
    out: list[Pin] = []
    for version in older:
        if only is not None and version != only:
            continue
        entry = _verified_entry(
            source, role, name, version, current.gate, policy, cache_dir, rehearsal
        )
        if entry is not None:
            out.append(entry)
            if first:
                break
    return out


def _verified_entry(
    source: ListStore,
    role: str,
    name: str,
    version: str,
    gate: str,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    rehearsal: bool,
) -> Pin | None:
    """The pin of ``name@version`` for ``role`` and ``gate`` if it verifies, else None. A
    store outage propagates; any other refusal means "not a candidate"."""
    try:
        sha = find_signed_manifest(source, name, version, policy)
        entry = Pin(name, version, sha, gate)
        check_production(role, entry, rehearsal=rehearsal)
        check_test_split(verify.verify_pin(source, role, entry, policy, cache_dir).manifest)
    except Refused as e:
        if e.reason == "store_unavailable":
            raise
        return None
    return entry


def rollback(
    role: str,
    *,
    source: ListStore,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    lock_path: Path | str,
    to: str | None = None,
    rehearsal: bool = False,
) -> RollbackResult:
    """``mhai-models rollback ROLE [--to VERSION]``: pin ``role`` to the latest of
    :func:`rollback_candidates` (or ``to``, which must be one of them: ``not_found``/
    ``not_a_candidate`` otherwise, P16) and write the lock with :func:`write`. A role the
    lock doesn't pin is ``lock_invalid``/``role``; no candidate at all is
    ``not_found``/``no_candidate``. The change goes in a normal PR, where ``lock-check``
    verifies it again. ``rehearsal`` (development only) relaxes :func:`check_production`."""
    names.check_role(role)
    if to is not None:
        names.check_version(to)
    check_rehearsal(rehearsal)
    current_lock = read(lock_path)
    current = current_lock.models.get(role)
    if current is None:
        raise _invalid("role")
    candidates = rollback_candidates(
        source,
        role,
        current,
        policy=policy,
        cache_dir=cache_dir,
        only=to,
        first=True,
        rehearsal=rehearsal,
    )
    if not candidates:
        detail = "no_candidate" if to is None else "not_a_candidate"
        raise Refused("not_found", detail_code=detail)
    chosen = candidates[0]
    write(lock_path, current_lock.with_pin(role, chosen))
    return RollbackResult(role, current, chosen)


# ---------------------------------------------------------------- a promoted base


@dataclass(frozen=True, slots=True)
class _BasePin:
    """A base's pin as a :class:`~intelligence.modelstore.verify.PinEntry`: no gate."""

    name: str
    version: str
    manifest_sha256: str
    gate: str = ""


def parse_base_pin(text: object) -> tuple[str, str, str]:
    """``<name>@<version>#<manifest sha256>`` into its parts (``base_unverified``)."""
    from intelligence.modelstore import training

    if not training.is_pin(text):
        raise Refused("base_unverified", detail_code="pin")
    assert isinstance(text, str)
    name, rest = text.split("@")
    version, sha = rest.split("#")
    return name, version, sha


def base_verifier(
    source: verify.ReadStore, policy: signing.TrustPolicy, cache_dir: Path | str
) -> Callable[[str], None]:
    """The promoted-base verifier ``training.check`` takes (``verify_pin=``): Task 22's
    verifier over the base's pin, its role from its own manifest, no gate."""

    def verify_base(text: str) -> None:
        name, version, sha = parse_base_pin(text)
        inner: str | None = None
        try:
            verify.check_environment(policy)
            m = manifest.parse(verify.find_manifest(source, name, version, sha))
            verify.verify_pin(
                source,
                m.role,
                _BasePin(name, version, sha),
                policy,
                cache_dir,
                require_gate=False,
            )
        except Refused as e:
            if e.reason == "store_unavailable":
                raise
            inner = e.reason
        if inner is not None:
            # The inner reason is a fixed code from REASONS: no name, path or content.
            raise Refused("base_unverified", detail_code=inner)

    return verify_base


__all__ = [
    "FIXTURE_PREFIX",
    "ROLES",
    "SCHEMA",
    "ListStore",
    "Lock",
    "Pin",
    "RollbackResult",
    "base_verifier",
    "changed_roles",
    "check_dependencies",
    "check_evaluation",
    "check_production",
    "check_rehearsal",
    "check_test_split",
    "empty",
    "find_signed_manifest",
    "lock_check",
    "parse",
    "parse_base_pin",
    "pinned_before",
    "pin",
    "read",
    "rollback",
    "rollback_candidates",
    "thresholds_sha256",
    "to_bytes",
    "write",
]
