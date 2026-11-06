"""``models.lock`` (``mhai.models.lock/1``): the production pins, ``pin`` and ``lock-check``
(plan Task 20, design sections 7.4, 7.6 and 8).

The lock
--------
Canonical JSON (:mod:`intelligence.modelstore.canonical`) followed by one newline (the
repository's end-of-file rule), nothing else::

    {"models":{"<role>":{"gate","manifest_sha256","name","version"}},"schema":"mhai.models.lock/1"}

A role matches the role pattern, a name, version and hash their patterns, and the gate is
one of :data:`intelligence.doceval.report.GATES`; anything else, a non-canonical body or
unknown keys, is ``lock_invalid``. The production lock (the repository's ``models.lock``)
also needs a known role (one with an evaluation suite, :data:`ROLES`), a production gate
and no ``fixture-`` name (:func:`check_production`, P14). It is changed only by PR.

``pin``
-------
:func:`pin` writes one role's entry. Before writing it:

* names first (role, name, version, gate), then P14 for the production lock;
* the manifest: of the stored versions of ``manifest.json``, the one whose signature
  verifies for purpose ``manifest`` (never "latest"; two different signed manifests are
  ``lock_mismatch``/``ambiguous``);
* Task 22's verifier, :func:`intelligence.modelstore.verify.verify_pin` with
  ``require_signoff=False`` (signature, identity, every file, the gate's ``pass`` in the
  manifest), never :func:`~intelligence.modelstore.verify.verify_for_load`;
* :func:`check_evaluation`: the split is ``test`` and its test set is the entry in
  ``models/testsets.lock``; ``evaluation.json`` (read by its recorded version id, its
  SHA-256 the manifest's) agrees with the manifest's gates, says ``pass`` for the gate,
  names the same split and test set, and was measured against the committed thresholds
  file for that gate.

``lock-check``
--------------
:func:`lock_check` compares the PR's base and head locks. Every head entry must pass
:func:`check_production`. For each changed role:

* **new version:** verified as for ``pin`` (signature, hashes, gate, evaluation); the PR
  carries the ``clinical-review`` label (``label_missing``); its approvals on the head
  commit include a clinical and a platform reviewer from the **base** ``reviewers.json``,
  two different people, neither the PR's author nor the manifest's ``promotion.actor`` or
  ``trained_by`` (``separation_of_duties`` when only those rules stand in the way,
  ``not_approved`` otherwise; P15);
* **rollback** (the version already has a valid sign-off for this role, P16): verified,
  no new evaluation; the label, one platform approval and ``Clinical note:`` in the PR
  body. A version older than the base's pin of the same name without a sign-off is
  ``not_approved``/``never_approved``;
* **removed role:** one platform approval.

Two roles whose manifests pin the same model at different versions or hashes (directly or
as a dependency) are ``lock_invalid``/``dependencies``.

Approvals: GitHub's reviews, ordered by id; only each reviewer's latest ``APPROVED``,
``CHANGES_REQUESTED`` or ``DISMISSED`` review counts (a comment changes nothing), and an
approval counts only when it is the latest and its ``commit_id`` is the head commit. The
PR author's approval never counts. Logins come from the API in display case and are
lowercased on reading, then compared exactly (ruling D30).

The GitHub API
--------------
:class:`GitHubClient` makes two GET calls with the job's ``GITHUB_TOKEN``
(``pull-requests: read``): the pull request and its reviews (paginated, at most
:data:`MAX_REVIEW_PAGES` pages). No redirect is followed. The token is never in a log, an
error or a reason; failures are ``github_unavailable`` with no exception chained.

A promoted base model
---------------------
:func:`base_verifier` is what ``training.check`` calls for a base named by its pin
``<name>@<version>#<manifest sha256>``: Task 22's verifier, the role taken from that
manifest, no gate and no sign-off required.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Any, Protocol

from intelligence.doceval.report import GATES, PRODUCTION_GATES
from intelligence.doceval.report import packaged_thresholds as _packaged_thresholds
from intelligence.doceval.report import parse_report as _parse_report
from intelligence.modelstore import canonical, files, manifest, names, signing, store, verify
from intelligence.modelstore.promote import SUITES
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.testsets import LockEntry as TestsetEntry

SCHEMA = "mhai.models.lock/1"
MAX_BYTES = 64 * 1024
MAX_ROLES = 16
# The roles a production lock may pin: those with an evaluation suite (promote's
# ``SUITES``), since only they can pass a gate.
ROLES: frozenset[str] = frozenset(SUITES)
FIXTURE_PREFIX = "fixture-"
LABEL = "clinical-review"
CLINICAL_NOTE = "Clinical note:"
EVALUATION = "evaluation.json"
MAX_REPORT_BYTES = 8 * 1024 * 1024
REVIEWERS_MAX_BYTES = 64 * 1024
MAX_REVIEWERS = 64
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


def check_production(role: str, entry: Pin) -> None:
    """P14: a production pin names a known role, a production gate and no fixture."""
    if role not in ROLES:
        raise _invalid("role")
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
) -> None:
    """The evaluation behind a new pin (see the module docstring)."""
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
    if rep["thresholds"][gate]["sha256"] != _packaged_thresholds(gate).sha256:
        raise Refused("gate_failed", detail_code="thresholds")


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
    verified = verify.verify_pin(source, role, entry, policy, cache_dir, require_signoff=False)
    check_evaluation(source, verified.manifest, gate, testsets_lock)
    write(lock_path, current.with_pin(role, entry))
    return entry


# ---------------------------------------------------------------- reviewers


@dataclass(frozen=True, slots=True)
class Reviewers:
    """``models/reviewers.json``: the clinical and platform reviewers' logins."""

    clinical: frozenset[str]
    platform: frozenset[str]


def _logins(value: object) -> frozenset[str]:
    if not isinstance(value, list) or len(value) > MAX_REVIEWERS:
        raise Refused("policy_invalid", detail_code="reviewers")
    if not all(names.is_login(v) for v in value) or len(set(value)) != len(value):
        raise Refused("policy_invalid", detail_code="reviewers")
    return frozenset(value)


def parse_reviewers(raw: bytes) -> Reviewers:
    """Strict: exactly ``clinical`` and ``platform``, lists of distinct lowercase logins."""
    failed = False
    obj: Any = None
    try:
        obj = canonical.loads_strict(raw, REVIEWERS_MAX_BYTES, trailing_whitespace=True)
    except Refused:
        failed = True
    if failed or not isinstance(obj, dict) or set(obj) != {"clinical", "platform"}:
        raise Refused("policy_invalid", detail_code="reviewers")
    return Reviewers(_logins(obj["clinical"]), _logins(obj["platform"]))


def read_reviewers(path: Path | str) -> Reviewers:
    """``reviewers.json`` as it is on the PR's **base** (the caller passes that copy)."""
    failed = False
    raw = b""
    try:
        raw = files.read_regular(path, REVIEWERS_MAX_BYTES)
    except Refused:
        failed = True
    if failed:
        raise Refused("policy_invalid", detail_code="reviewers")
    return parse_reviewers(raw)


# ---------------------------------------------------------------- GitHub


@dataclass(frozen=True, slots=True)
class Review:
    id: int
    login: str
    state: str
    commit_id: str | None


@dataclass(frozen=True, slots=True)
class PullRequest:
    number: int
    author: str
    head_sha: str
    labels: frozenset[str]
    body: str
    reviews: tuple[Review, ...]


class GitHubApi(Protocol):
    def pull_request(self, number: int) -> PullRequest: ...


class Session(Protocol):
    """A ``requests.Session``-like object that carries the token in its own headers."""

    def get(
        self,
        url: str,
        *,
        params: Mapping[str, str],
        timeout: Any,
        stream: bool,
        allow_redirects: bool,
    ) -> Any: ...


API_URL = "https://api.github.com"
TIMEOUT = 30
MAX_RESPONSE_BYTES = 4 * 1024 * 1024
PER_PAGE = 100
MAX_REVIEW_PAGES = 10
MAX_PR = 2**31 - 1
MAX_BODY = 256 * 1024
_REPO = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}", re.ASCII)
_API = re.compile(
    r"https://[A-Za-z0-9.-]{1,253}(?::[0-9]{1,5})?(?:/[A-Za-z0-9._/-]{0,200})?", re.ASCII
)
_COMMIT = re.compile(r"[0-9a-f]{40}", re.ASCII)
_CHUNK = 64 * 1024
TOKEN_ENV = "GITHUB_TOKEN"
REPOSITORY_ENV = "GITHUB_REPOSITORY"
API_URL_ENV = "GITHUB_API_URL"


def _unavailable(detail: str) -> Refused:
    return Refused("github_unavailable", detail_code=detail)


def check_pr_number(number: object) -> int:
    if isinstance(number, str):
        if re.fullmatch(r"[1-9][0-9]{0,9}", number, re.ASCII) is None:
            raise Refused("invalid_name", detail_code="pr")
        number = int(number)
    if not isinstance(number, int) or isinstance(number, bool) or not 1 <= number <= MAX_PR:
        raise Refused("invalid_name", detail_code="pr")
    return number


def _login(user: object) -> str | None:
    """A user's login in lowercase (GitHub returns the display case), or None."""
    if not isinstance(user, dict):
        return None
    login = user.get("login")
    if not isinstance(login, str):
        return None
    login = login.lower()
    return login if names.is_login(login) else None


class GitHubClient:
    """The two reads ``lock-check`` makes. ``session`` carries the token in its headers
    (:meth:`from_env`); this class never sees or prints it."""

    def __init__(self, session: Session, repository: str, api_url: str = API_URL) -> None:
        if not isinstance(repository, str) or _REPO.fullmatch(repository) is None:
            raise Refused("not_allowed_here", detail_code="repository")
        if not isinstance(api_url, str) or _API.fullmatch(api_url) is None or ".." in api_url:
            raise Refused("not_allowed_here", detail_code="api_url")
        self._session = session
        self._base = f"{api_url.rstrip('/')}/repos/{repository}"

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> GitHubClient:
        env = os.environ if env is None else env
        token = env.get(TOKEN_ENV)
        repository = env.get(REPOSITORY_ENV)
        if not token or not repository:
            raise Refused("not_allowed_here", detail_code="github_env")
        import requests

        session = requests.Session()
        session.trust_env = False  # no .netrc credentials or proxies from the environment
        session.headers.update(
            {
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            }
        )
        return cls(session, repository, env.get(API_URL_ENV) or API_URL)

    def _json(self, path: str, params: Mapping[str, str] | None = None) -> Any:
        resp: Any = None
        try:
            resp = self._session.get(
                f"{self._base}{path}",
                params=dict(params or {}),
                timeout=TIMEOUT,
                stream=True,
                allow_redirects=False,
            )
        except Exception:  # never the exception: it may carry the URL or a header
            resp = None
        if resp is None:
            raise _unavailable("network")
        body = bytearray()
        status = getattr(resp, "status_code", None)
        failed: Refused | None = None
        try:
            if status == 404:
                failed = Refused("not_found", detail_code="pull_request")
            elif status != 200:
                failed = _unavailable("http")
            else:
                for chunk in resp.iter_content(_CHUNK):
                    body += chunk
                    if len(body) > MAX_RESPONSE_BYTES:
                        failed = _unavailable("response_size")
                        break
        except Exception:
            failed = _unavailable("network")
        finally:
            try:
                resp.close()
            except Exception:
                pass
        if failed is not None:
            raise failed
        obj: Any = None
        bad = False
        try:
            obj = json.loads(bytes(body))
        except ValueError:
            bad = True
        if bad:
            raise _unavailable("response")
        return obj

    def _reviews(self, number: int) -> tuple[Review, ...]:
        out: list[Review] = []
        for page in range(1, MAX_REVIEW_PAGES + 1):
            items = self._json(
                f"/pulls/{number}/reviews", {"per_page": str(PER_PAGE), "page": str(page)}
            )
            if not isinstance(items, list) or len(items) > PER_PAGE:
                raise _unavailable("response")
            for item in items:
                review = _review(item)
                if review is not None:
                    out.append(review)
            if len(items) < PER_PAGE:
                return tuple(out)
        raise Refused("not_approved", detail_code="too_many_reviews")

    def pull_request(self, number: int) -> PullRequest:
        number = check_pr_number(number)
        obj = self._json(f"/pulls/{number}")
        if not isinstance(obj, dict) or obj.get("number") != number:
            raise _unavailable("response")
        author = _login(obj.get("user"))
        head = obj.get("head")
        sha = head.get("sha") if isinstance(head, dict) else None
        if author is None or not isinstance(sha, str) or _COMMIT.fullmatch(sha) is None:
            raise _unavailable("response")
        labels_raw = obj.get("labels")
        if not isinstance(labels_raw, list):
            raise _unavailable("response")
        labels = frozenset(
            lab["name"]
            for lab in labels_raw
            if isinstance(lab, dict) and isinstance(lab.get("name"), str)
        )
        body = obj.get("body")
        if body is None:
            body = ""
        if not isinstance(body, str) or len(body) > MAX_BODY:
            raise _unavailable("response")
        return PullRequest(number, author, sha, labels, body, self._reviews(number))


def _review(item: object) -> Review | None:
    """One review, or None for one that can't count (a deleted user, an odd shape)."""
    if not isinstance(item, dict):
        return None
    rid, state, commit = item.get("id"), item.get("state"), item.get("commit_id")
    login = _login(item.get("user"))
    if login is None or not isinstance(rid, int) or isinstance(rid, bool):
        return None
    if not isinstance(state, str):
        return None
    if commit is not None and (not isinstance(commit, str) or _COMMIT.fullmatch(commit) is None):
        commit = None
    return Review(rid, login, state, commit)


# ---------------------------------------------------------------- approvals

_DECISIVE = frozenset({"APPROVED", "CHANGES_REQUESTED", "DISMISSED"})


def approvals(pr: PullRequest, *, include_author: bool = False) -> frozenset[str]:
    """The logins whose latest decisive review approves the head commit."""
    latest: dict[str, Review] = {}
    for review in sorted(pr.reviews, key=lambda r: r.id):
        if review.state in _DECISIVE:
            latest[review.login] = review
    return frozenset(
        login
        for login, r in latest.items()
        if r.state == "APPROVED"
        and r.commit_id is not None
        and secrets.compare_digest(r.commit_id, pr.head_sha)
        and (include_author or login != pr.author)
    )


@dataclass(frozen=True, slots=True)
class Change:
    role: str
    kind: str  # new, rollback or removed
    manifest: manifest.Manifest | None = None


def _pair(clinical: Iterable[str], platform: Iterable[str]) -> bool:
    """Two different people: one clinical, one platform."""
    platform = set(platform)
    return any(platform - {c} for c in clinical)


def check_approvals(pr: PullRequest, reviewers: Reviewers, changes: Iterable[Change]) -> None:
    changes = list(changes)
    kinds = {c.kind for c in changes}
    if kinds & {"new", "rollback"} and LABEL not in pr.labels:
        raise Refused("label_missing")
    if "rollback" in kinds and CLINICAL_NOTE not in pr.body:
        raise Refused("not_approved", detail_code="clinical_note")
    excluded = {pr.author}
    for c in changes:
        if c.kind == "new" and c.manifest is not None:
            excluded |= {c.manifest.promotion.actor, c.manifest.trained_by}
    strict = approvals(pr) - excluded
    loose = approvals(pr, include_author=True)
    clinical, platform = strict & reviewers.clinical, strict & reviewers.platform
    if "new" in kinds:
        if _pair(clinical, platform):
            return
        loose_ok = bool(loose & reviewers.clinical) and bool(loose & reviewers.platform)
    else:
        if platform:
            return
        loose_ok = bool(loose & reviewers.platform)
    if loose_ok:
        raise Refused("separation_of_duties")
    raise Refused("not_approved", detail_code="approvals")


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


def lock_check(
    base_raw: bytes,
    head_raw: bytes,
    number: int,
    *,
    github: GitHubApi,
    reviewers: Reviewers,
    source: verify.ReadStore,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    head_sha: str | None = None,
) -> dict[str, str]:
    """Check a PR's change to ``models.lock``; returns ``{role: new|rollback|removed}``.
    ``reviewers`` and ``testsets_lock`` are the **base** ref's copies. ``head_sha``, when
    given, is the commit ``head_raw`` was read from: a PR whose head has moved since is
    ``not_approved``/``head_moved`` (the new head gets its own check). Raises
    :class:`Refused` on any failure."""
    number = check_pr_number(number)
    if head_sha is not None and (not isinstance(head_sha, str) or not _COMMIT.fullmatch(head_sha)):
        raise Refused("invalid_name", detail_code="head_sha")
    if not isinstance(reviewers, Reviewers):
        raise Refused("policy_invalid", detail_code="reviewers")
    base, head = parse(base_raw), parse(head_raw)
    for role, pinned in head.models.items():
        check_production(role, pinned)
    roles = changed_roles(base, head)
    if not roles:
        return {}
    changes: list[Change] = []
    manifests: dict[str, manifest.Manifest] = {}
    for role in roles:
        entry = head.models.get(role)
        if entry is None:
            changes.append(Change(role, "removed"))
            continue
        m = verify.verify_pin(
            source, role, entry, policy, cache_dir, require_signoff=False
        ).manifest
        manifests[role] = m
        kind = "rollback"
        try:
            verify.check_signoff(source, role, entry, m, policy)
        except Refused as e:
            if e.reason != "not_approved":
                raise
            kind = "new"
        if kind == "new":
            old = base.models.get(role)
            if (
                old is not None
                and old.name == entry.name
                and names.Version.parse(entry.version) < names.Version.parse(old.version)
            ):
                raise Refused("not_approved", detail_code="never_approved")
            check_evaluation(source, m, entry.gate, testsets_lock)
        changes.append(Change(role, kind, m))
    for role, pinned in head.models.items():
        if role not in manifests:
            raw = verify.find_manifest(source, pinned.name, pinned.version, pinned.manifest_sha256)
            manifests[role] = manifest.parse(raw)
    check_dependencies(head, manifests)
    pr = github.pull_request(number)
    if head_sha is not None and not secrets.compare_digest(head_sha, pr.head_sha):
        raise Refused("not_approved", detail_code="head_moved")
    check_approvals(pr, reviewers, changes)
    return {c.role: c.kind for c in changes}


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
    verifier over the base's pin, its role from its own manifest, no gate or sign-off."""

    def verify_base(text: str) -> None:
        name, version, sha = parse_base_pin(text)
        verify.check_environment(policy)
        m = manifest.parse(verify.find_manifest(source, name, version, sha))
        verify.verify_pin(
            source,
            m.role,
            _BasePin(name, version, sha),
            policy,
            cache_dir,
            require_signoff=False,
            require_gate=False,
        )

    return verify_base


__all__ = [
    "CLINICAL_NOTE",
    "FIXTURE_PREFIX",
    "LABEL",
    "MAX_REVIEW_PAGES",
    "ROLES",
    "SCHEMA",
    "Change",
    "GitHubApi",
    "GitHubClient",
    "Lock",
    "Pin",
    "PullRequest",
    "Review",
    "Reviewers",
    "approvals",
    "base_verifier",
    "changed_roles",
    "check_approvals",
    "check_dependencies",
    "check_evaluation",
    "check_pr_number",
    "check_production",
    "empty",
    "find_signed_manifest",
    "lock_check",
    "parse",
    "parse_base_pin",
    "parse_reviewers",
    "pin",
    "read",
    "read_reviewers",
    "to_bytes",
    "write",
]
