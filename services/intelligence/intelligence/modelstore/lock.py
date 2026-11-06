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
:func:`lock_check` compares the lock on ``main`` with the PR's lock. Which data it trusts
is the whole point, so:

* **Only the head lock is data from the PR.** It is the bytes of ``models.lock`` at the
  PR's head commit ``head_sha`` (the workflow writes them with ``git show
  <head sha>:models.lock``, never from a merge checkout), and the check refuses
  (``not_approved``/``head_moved``) unless the PR's current head is that commit, so the
  content checked and the approvals counted belong to one commit.
* **Everything else comes from the current tip of ``main``**, read at check time, never
  from the PR's base commit and never from the PR's own copies: the base lock,
  ``reviewers.json``, ``models/testsets.lock`` and the thresholds files. A reviewer
  removed from ``main`` stops counting at once, also for PRs opened before; a PR that
  lowers a threshold or adds a test set in the same change as a pin is checked against
  ``main``'s, and refused. Task 24's ``model-lock.yml`` runs ``mhai-models lock-check``
  from a checkout of ``main``'s tip (its code and its data, ``--base-dir``) and takes only
  the head lock from the PR.
* **The PR's base branch must be** ``main`` (``base_not_main``): a PR into another branch
  could carry a reviewer list nobody reviewed.
* **The lock travels alone** (D35, D35b, :func:`check_lock_change_alone`).
  ``pull_request`` and ``push`` workflows run the workflow file from the PR or the merge
  commit, and the lock is judged by code all over the repository, so a PR that changes
  ``models.lock`` or ``models/**`` may change only ``models.lock``, ``models/**``,
  ``docs/**`` and ``CHANGELOG.md`` (an allow-list); anything else is
  ``not_allowed_here``/``code_changed_with_lock``. The changed files come from git in
  ``--base-dir``'s repository (``git diff --no-renames <main tip>...<head sha>``: from the
  merge base to the head; a rename counts both paths), never from a list the PR
  supplies. The sign-off applies the same rule to the merge (``git diff <base sha>
  <merge commit>``).

  **What this can't do (D37).** Main's *code* runs, but the *workflow file* is the PR's:
  ``pull_request`` runs ``model-lock.yml`` from the PR's merge ref and ``push`` runs
  ``model-signoff.yml`` from the merge commit. A PR that edits ``model-lock.yml`` (in the
  same PR as the lock change, or in an earlier one) can skip this check and still report
  a green ``lock-check``; the sign-off is the backstop only while ``model-signoff.yml`` is
  unedited, and a PR that edits it too gets its own copy run with the sign-off identity.
  The controls for that are people and settings, not code: code-owner review of
  ``.github/``, approval of the most recent push, and a required reviewer on the
  ``model-signoff`` environment (TODO(H4) in ``TODO.md``).

Every head entry must pass :func:`check_production`. For each changed role:

* **new version:** verified as for ``pin`` (signature, hashes, gate, evaluation); the PR
  carries the ``clinical-review`` label (``label_missing``); its approvals on the head
  commit include a clinical and a platform reviewer from the **base** ``reviewers.json``,
  two different people, neither the PR's author nor the manifest's ``promotion.actor`` or
  ``trained_by`` (``separation_of_duties`` when only those rules stand in the way,
  ``not_approved`` otherwise; P15);
* **rollback** (the version already has a valid sign-off for this role, P16): verified,
  no new evaluation; the label, one platform approval (not the restored version's
  promoter or trainer, nor the author) and a clinical note: a line of the PR body that
  starts with ``Clinical note:`` followed by at least 20 letters or digits, outside
  fenced code blocks and HTML comments (:func:`has_clinical_note`). A version older than the base's pin of the same name without a sign-off is
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
(``pull-requests: read``): the pull request and its reviews, following the ``Link``
header's ``rel="next"`` only when it is the page just requested with ``page`` one
higher (the same path, or GitHub's ``/repositories/<id>`` form with this repository's id;
anything else is ``github_unavailable``/``pagination``), at most
:data:`MAX_REVIEW_PAGES` pages: a next page past the cap is a refusal. No redirect is
followed. The token is never in a log, an
error or a reason; failures are ``github_unavailable`` with no exception chained.

Shared with the sign-off
------------------------
:func:`assess_changes` is the store half of ``lock-check`` (production rules, verify,
classify, evaluation, dependencies) and :func:`check_approvals` the approval half; the
sign-off (:mod:`intelligence.modelstore.signoff`, Task 21) re-runs both on the merged PR,
never a copy of their rules. :meth:`GitHubClient.pulls_for_commit` finds that PR.

``rollback``
------------
:func:`rollback` pins a role back to the latest older version of its pinned name (semver
order) whose sign-off for this role verifies through the loader's own check
(:func:`intelligence.modelstore.verify.verify_pin` with ``require_signoff=True``), or to
``--to``, which must be one of those (P16); the lock is written with :func:`write`.

A promoted base model
---------------------
:func:`base_verifier` is what ``training.check`` calls for a base named by its pin
``<name>@<version>#<manifest sha256>``: Task 22's verifier, the role taken from that
manifest, no gate and no sign-off required. A base that fails is ``base_unverified``, the
inner reason as its detail code (a store outage stays ``store_unavailable``).
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import unicodedata
from collections.abc import Callable, Iterable, Iterator, Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Any, Protocol
from urllib.parse import parse_qsl, urlsplit

from intelligence.doceval.report import GATES, PRODUCTION_GATES, THRESHOLDS_DIR
from intelligence.doceval.report import load_thresholds as _load_thresholds
from intelligence.doceval.report import parse_report as _parse_report
from intelligence.modelstore import canonical, files, manifest, names, signing, store, verify
from intelligence.modelstore import signoff as signoff_record
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
CLINICAL_NOTE_MIN_CHARS = 20
MAIN_BRANCH = "main"
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
    ``thresholds_dir`` are the trusted copies (``main``'s for ``lock-check``)."""
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
    verified = verify.verify_pin(source, role, entry, policy, cache_dir, require_signoff=False)
    check_evaluation(source, verified.manifest, gate, testsets_lock, thresholds_dir)
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
    base_ref: str
    # Set when the PR is merged (the sign-off, Task 21): ``merge_commit_sha`` is the commit
    # on the base branch, ``merged_at`` GitHub's ``YYYY-MM-DDTHH:MM:SSZ``.
    merged: bool = False
    merge_commit_sha: str | None = None
    merged_at: str | None = None


class GitHubApi(Protocol):
    def pull_request(self, number: int) -> PullRequest: ...


class MergedPullRequests(GitHubApi, Protocol):
    """What the sign-off needs too: the pull requests GitHub associates with a commit, and
    a commit's parents."""

    def pulls_for_commit(self, sha: str) -> tuple[int, ...]: ...

    def commit_parents(self, sha: str) -> tuple[str, ...]: ...


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
_REF = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/-]{0,254}", re.ASCII)
_TIMESTAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", re.ASCII)
_NEXT = re.compile(r'<([^<>\s]{1,2048})>\s*;(?:[^,]*;)?\s*rel="next"', re.ASCII)
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
        self._repo_path = f"/repos/{repository}"
        self._base = f"{api_url.rstrip('/')}{self._repo_path}"

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
        return self._get(f"{self._base}{path}", params)[0]

    def _get(self, url: str, params: Mapping[str, str] | None = None) -> tuple[Any, str | None]:
        """The parsed JSON at ``url`` and the response's ``Link`` header (or None)."""
        resp: Any = None
        try:
            resp = self._session.get(
                url,
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
        link: str | None = None
        try:
            got = resp.headers.get("Link")
            link = got if isinstance(got, str) else None
        except Exception:
            link = None
        obj: Any = None
        bad = False
        try:
            obj = json.loads(bytes(body))
        except ValueError:
            bad = True
        if bad:
            raise _unavailable("response")
        return obj, link

    def _next(
        self, link: str | None, path: str, page: int, number: int, repo_id: int | None
    ) -> tuple[str, dict[str, str]] | None:
        """The ``rel="next"`` page after ``page`` of this PR's reviews, as (URL, params), or
        None. The next link must be the path just requested (or GitHub's
        ``/repositories/<id>/...`` form of it, with the id of this repository from the pull
        request), on the same scheme and host, with ``per_page`` unchanged and ``page``
        exactly one more; anything else is ``github_unavailable``/``pagination``: no page is
        skipped and the token never leaves for a URL we didn't expect."""
        if link is None:
            return None
        found = _NEXT.search(link)
        if found is None:
            return None
        failed = False
        try:
            nxt, ours = urlsplit(found.group(1)), urlsplit(self._base)
            query = parse_qsl(nxt.query, keep_blank_values=True, strict_parsing=True)
        except ValueError:
            failed = True
        if failed:
            raise _unavailable("pagination")
        api_path = ours.path[: -len(self._repo_path)]
        allowed = {path}
        if repo_id is not None:
            allowed.add(f"{api_path}/repositories/{repo_id}/pulls/{number}/reviews")
        expected = {"per_page": str(PER_PAGE), "page": str(page + 1)}
        if (
            nxt.scheme != ours.scheme
            or nxt.netloc != ours.netloc
            or nxt.fragment
            or nxt.path not in allowed
            or sorted(query) != sorted(expected.items())
        ):
            raise _unavailable("pagination")
        return f"{nxt.scheme}://{nxt.netloc}{nxt.path}", expected

    def _reviews(self, number: int, repo_id: int | None) -> tuple[Review, ...]:
        out: list[Review] = []
        url = f"{self._base}/pulls/{number}/reviews"
        page = 1
        params = {"per_page": str(PER_PAGE), "page": str(page)}
        for _ in range(MAX_REVIEW_PAGES):
            items, link = self._get(url, params)
            if not isinstance(items, list) or len(items) > PER_PAGE:
                raise _unavailable("response")
            for item in items:
                review = _review(item)
                if review is not None:
                    out.append(review)
            nxt = self._next(link, urlsplit(url).path, page, number, repo_id)
            if nxt is None:
                return tuple(out)
            url, params = nxt
            page += 1
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
        base = obj.get("base")
        base_ref = base.get("ref") if isinstance(base, dict) else None
        if not isinstance(base_ref, str) or _REF.fullmatch(base_ref) is None:
            raise _unavailable("response")
        repo = base.get("repo") if isinstance(base, dict) else None
        rid = repo.get("id") if isinstance(repo, dict) else None
        repo_id = rid if isinstance(rid, int) and not isinstance(rid, bool) and rid > 0 else None
        reviews = self._reviews(number, repo_id)
        merged = obj.get("merged") is True
        mc = obj.get("merge_commit_sha")
        merge_commit = mc if isinstance(mc, str) and _COMMIT.fullmatch(mc) is not None else None
        at = obj.get("merged_at")
        merged_at = at if isinstance(at, str) and _TIMESTAMP.fullmatch(at) is not None else None
        return PullRequest(
            number, author, sha, labels, body, reviews, base_ref, merged, merge_commit, merged_at
        )

    def commit_parents(self, sha: str) -> tuple[str, ...]:
        """The parents of commit ``sha`` in order (``GET /git/commits/{sha}``, the git
        object only, never the diff, so a large merge isn't refused for its size): the
        first is the branch the commit was made on, the second (for a merge) the merged
        head."""
        if not isinstance(sha, str) or _COMMIT.fullmatch(sha) is None:
            raise Refused("invalid_name", detail_code="commit")
        obj = self._json(f"/git/commits/{sha}")
        parents = obj.get("parents") if isinstance(obj, dict) else None
        if not isinstance(obj, dict) or obj.get("sha") != sha or not isinstance(parents, list):
            raise _unavailable("response")
        if len(parents) > 8:
            raise _unavailable("response")
        out: list[str] = []
        for p in parents:
            psha = p.get("sha") if isinstance(p, dict) else None
            if not isinstance(psha, str) or _COMMIT.fullmatch(psha) is None:
                raise _unavailable("response")
            out.append(psha)
        return tuple(out)

    def pulls_for_commit(self, sha: str) -> tuple[int, ...]:
        """The numbers of the pull requests GitHub associates with commit ``sha``
        (``GET /commits/{sha}/pulls``), one page of at most :data:`PER_PAGE`. A second page
        means more than one, which the sign-off refuses anyway: it is
        ``not_approved``/``many_prs`` here."""
        if not isinstance(sha, str) or _COMMIT.fullmatch(sha) is None:
            raise Refused("invalid_name", detail_code="commit")
        items, link = self._get(
            f"{self._base}/commits/{sha}/pulls", {"per_page": str(PER_PAGE), "page": "1"}
        )
        if not isinstance(items, list) or len(items) > PER_PAGE:
            raise _unavailable("response")
        if link is not None and _NEXT.search(link) is not None:
            raise Refused("not_approved", detail_code="many_prs")
        out: list[int] = []
        for item in items:
            n = item.get("number") if isinstance(item, dict) else None
            if not isinstance(n, int) or isinstance(n, bool) or not 1 <= n <= MAX_PR:
                raise _unavailable("response")
            out.append(n)
        return tuple(out)


# ---------------------------------------------------------------- the rehearsal's GitHub

FAKE_MAX_BYTES = 1024 * 1024


class _FileResponse:
    def __init__(self, status_code: int, body: bytes) -> None:
        self.status_code = status_code
        self.headers: dict[str, str] = {}
        self._body = body

    def iter_content(self, size: int) -> Iterator[bytes]:
        for i in range(0, len(self._body), size):
            yield self._body[i : i + size]

    def close(self) -> None:
        return None


class FileSession:
    """GitHub's answers from a file, for Task 24's CI rehearsal only (``mhai-models``
    takes it from ``MHAI_MODELS_GITHUB_FAKE``, in development only, both switches). The
    file is ``{"repository": "<owner>/<repo>", "responses": {"<path>": <JSON body>}}``,
    each path the part after ``/repos/<owner>/<repo>`` (``/pulls/1``,
    ``/pulls/1/reviews``, ``/commits/<sha>/pulls``, ``/git/commits/<sha>``); anything
    else is a 404. No network, no token: :class:`GitHubClient` parses these answers as it
    parses GitHub's, so the rehearsal runs the same checks."""

    def __init__(self, responses: Mapping[str, Any], repository: str) -> None:
        self._responses = dict(responses)
        self._prefix = f"{API_URL}/repos/{repository}"

    @classmethod
    def client(cls, path: Path | str) -> GitHubClient:
        failed = False
        obj: Any = None
        try:
            obj = json.loads(files.read_regular(path, FAKE_MAX_BYTES))
        except (Refused, ValueError):
            failed = True
        if failed or not isinstance(obj, dict) or not isinstance(obj.get("responses"), dict):
            raise Refused("not_allowed_here", detail_code="github_fake")
        repository = obj.get("repository")
        if not isinstance(repository, str) or _REPO.fullmatch(repository) is None:
            raise Refused("not_allowed_here", detail_code="github_fake")
        return GitHubClient(cls(obj["responses"], repository), repository)

    def get(
        self,
        url: str,
        *,
        params: Mapping[str, str],
        timeout: Any,
        stream: bool,
        allow_redirects: bool,
    ) -> _FileResponse:
        if not url.startswith(self._prefix):
            return _FileResponse(404, b"")
        found = self._responses.get(url[len(self._prefix) :])
        if found is None:
            return _FileResponse(404, b"")
        return _FileResponse(200, json.dumps(found).encode())


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


# Only complete comments are removed; anything else that starts raw HTML makes the body
# fail closed (see has_clinical_note).
_COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)
_RAW_HTML = re.compile(r"<[A-Za-z?!/]")
# CommonMark's line endings only: never str.splitlines' \v, \f, \x1c-\x1e, \x85, U+2028/9.
_LINES = re.compile(r"\r\n|\r|\n")
# A link reference definition, ``[label]: url "title"``: never rendered, so text in it is
# hidden (D34).
_LINK_DEFINITION = re.compile(r" {0,3}\[[^\]]*\]:")
_FENCE = re.compile(r" {0,3}(`{3,}|~{3,})(.*)", re.DOTALL)
# CommonMark: a closing fence carries no info string, only trailing spaces or tabs.
_CLOSER = re.compile(r" {0,3}(`{3,}|~{3,})[ \t]*")
# Letters that render as nothing: the Hangul fillers (category Lo). They are the only
# default-ignorable code points in categories L and N; the rest are Cf or Mn, never counted.
_FILLERS = frozenset({"\u115f", "\u1160", "\u3164", "\uffa0"})


def _counts(ch: str) -> bool:
    return unicodedata.category(ch)[0] in "LN" and ch not in _FILLERS


def has_clinical_note(body: str) -> bool:
    """A line that starts with ``Clinical note:`` and has at least
    :data:`CLINICAL_NOTE_MIN_CHARS` letters or digits after it on that line, outside HTML
    comments and fenced code blocks (an unclosed fence runs to the end; a closing fence
    carries no info string, as in CommonMark).

    It is read as GitHub renders it, and fails closed wherever the two could differ:

    * lines end only at ``\n``, ``\r\n`` or a lone ``\r`` (never at ``\v``, ``\f``,
      ``\x85`` or the Unicode separators, which CommonMark keeps inside a line);
    * comment and fence boundaries are found on the original text, so a format character
      (category Cf) inside ``-->`` or a fence marker breaks that marker;
    * only complete ``<!-- -->`` comments are removed; any other raw HTML left anywhere
      (``<?``, ``<!``, CDATA, an unclosed comment, a tag, which may hide text in an
      attribute) means no note;
    * a backtick fence whose info string contains a backtick isn't a fence in
      CommonMark: no note;
    * a link reference definition (``[label]: ...``, at most three spaces in, after Cf
      removal) anywhere in the body hides its text: no note (D34);
    * each candidate line has its Cf characters removed before the prefix check, and only
      letters and digits (categories L and N) count, never the Hangul fillers, which
      render as nothing.

    Indented, quoted or inline mentions don't count.

    Residual risk: this is not a full GFM renderer, so some markup may still hide text
    that is counted here. The note is a human attestation, shown on the PR to the
    platform approver whose approval the rollback also needs; this check makes it
    present, not true."""
    if not isinstance(body, str):
        return False
    for line in _LINES.split(body):
        if _LINK_DEFINITION.match("".join(ch for ch in line if unicodedata.category(ch) != "Cf")):
            return False
    text = _COMMENT.sub("", body)
    if _RAW_HTML.search(text):
        return False
    fence: str | None = None
    for line in _LINES.split(text):
        if fence is not None:
            c = _CLOSER.fullmatch(line)
            if c is not None and c.group(1)[0] == fence[0] and len(c.group(1)) >= len(fence):
                fence = None
            continue
        m = _FENCE.match(line)
        if m is not None:
            if m.group(1)[0] == "`" and "`" in m.group(2):
                return False
            fence = m.group(1)
            continue
        line = "".join(ch for ch in line if unicodedata.category(ch) != "Cf")
        if line.startswith(CLINICAL_NOTE):
            rest = line[len(CLINICAL_NOTE) :]
            if sum(1 for ch in rest if _counts(ch)) >= CLINICAL_NOTE_MIN_CHARS:
                return True
    return False


def check_approvals(pr: PullRequest, reviewers: Reviewers, changes: Iterable[Change]) -> None:
    changes = list(changes)
    kinds = {c.kind for c in changes}
    if kinds & {"new", "rollback"} and LABEL not in pr.labels:
        raise Refused("label_missing")
    if "rollback" in kinds and not has_clinical_note(pr.body):
        raise Refused("not_approved", detail_code="clinical_note")
    excluded = {pr.author}
    for c in changes:
        # P15: nobody approves a version they promoted or trained, new or restored
        if c.manifest is not None:
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

# D35, D35b: ``pull_request`` and ``push`` workflows run the workflow file of the PR or of
# the merge commit, and a lock is judged by code all over the repository (the model store,
# doceval's reports and thresholds, the boundary, CI's scripts and actions), so a change to
# the lock may change nothing but the lock data and documentation: an allow-list, not a
# list of what to guard. Paths are relative to the repository's top level, exactly as
# ``git diff --name-only --no-renames`` prints them (a rename lists both of its paths).
LOCK_FILES: frozenset[str] = frozenset({"models.lock"})
LOCK_PREFIXES: tuple[str, ...] = ("models/",)
ALLOWED_WITH_LOCK_FILES: frozenset[str] = frozenset({"models.lock", "CHANGELOG.md"})
ALLOWED_WITH_LOCK_PREFIXES: tuple[str, ...] = ("models/", "docs/")


def _is_lock_path(path: str) -> bool:
    return path in LOCK_FILES or path.startswith(LOCK_PREFIXES)


def _allowed_with_lock(path: str) -> bool:
    if path in ALLOWED_WITH_LOCK_FILES:
        return True
    if not path.startswith(ALLOWED_WITH_LOCK_PREFIXES):
        return False
    # git prints normalised paths; anything else (``..``, ``.``, empty parts, a
    # backslash) is not one, and is refused rather than interpreted.
    parts = path.split("/")
    return "\\" not in path and all(p not in ("", ".", "..") for p in parts)


def check_lock_change_alone(changed_paths: Iterable[str]) -> None:
    """D35, D35b: a change to ``models.lock`` or ``models/**`` may change only
    ``models.lock``, ``models/**``, ``docs/**`` and ``CHANGELOG.md``; any other changed
    path (code, workflows, CI scripts and actions, dependencies) is
    ``not_allowed_here``/``code_changed_with_lock``. ``lock-check`` and the sign-off both
    call this, never a copy of it.

    ``changed_paths`` is the list of changed files the caller worked out from git (never
    a list the PR supplies), with renames as both paths; a bare string is a programming
    error, not a list of paths."""
    if isinstance(changed_paths, (str, bytes)):
        raise TypeError("changed_paths must be a collection of paths, not one string")
    paths = list(changed_paths)
    if not all(isinstance(p, str) for p in paths):
        raise TypeError("changed_paths must hold strings")
    if any(_is_lock_path(p) for p in paths) and not all(_allowed_with_lock(p) for p in paths):
        raise Refused("not_allowed_here", detail_code="code_changed_with_lock")


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


def assess_changes(
    base: Lock,
    head: Lock,
    *,
    source: verify.ReadStore,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    thresholds_dir: Path | str,
    is_own: Callable[[signoff_record.Signoff], bool] | None = None,
    rehearsal: bool = False,
) -> tuple[list[Change], dict[str, manifest.Manifest]]:
    """The store half of ``lock-check`` (and of the sign-off, Task 21): every head entry
    passes :func:`check_production`; each changed role is verified and classified as
    ``new``, ``rollback`` or ``removed`` (see the module docstring), a new one with its
    evaluation checked; the dependencies agree. Returns the changes and every head role's
    manifest.

    ``is_own`` is the sign-off's: a valid sign-off it accepts (the one this very merge
    already wrote, on a re-run) keeps the version ``new`` instead of making it a
    rollback. ``rehearsal`` relaxes :func:`check_production` (development only)."""
    for role, pinned in head.models.items():
        check_production(role, pinned, rehearsal=rehearsal)
    changes: list[Change] = []
    manifests: dict[str, manifest.Manifest] = {}
    for role in changed_roles(base, head):
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
            found = verify.find_signoff(source, role, entry, m, policy)
        except Refused as e:
            if e.reason != "not_approved":
                raise
            kind = "new"
        else:
            if is_own is not None and is_own(found.record):
                kind = "new"
        if kind == "new":
            old = base.models.get(role)
            if (
                old is not None
                and old.name == entry.name
                and names.Version.parse(entry.version) < names.Version.parse(old.version)
            ):
                raise Refused("not_approved", detail_code="never_approved")
            check_evaluation(source, m, entry.gate, testsets_lock, thresholds_dir)
        changes.append(Change(role, kind, m))
    for role, pinned in head.models.items():
        if role not in manifests:
            raw = verify.find_manifest(source, pinned.name, pinned.version, pinned.manifest_sha256)
            manifests[role] = manifest.parse(raw)
    check_dependencies(head, manifests)
    return changes, manifests


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
    thresholds_dir: Path | str,
    head_sha: str,
    changed_paths: Iterable[str],
    rehearsal: bool = False,
) -> dict[str, str]:
    """Check a PR's change to ``models.lock``; returns ``{role: new|rollback|removed}``.

    ``base_raw``, ``reviewers``, ``testsets_lock`` and ``thresholds_dir`` are ``main``'s
    current copies, never the PR's (see the module docstring). ``head_raw`` is the lock at
    ``head_sha``; a PR whose head is no longer ``head_sha`` is ``not_approved``/
    ``head_moved`` (the new head gets its own check), and one whose base branch isn't
    ``main`` is ``base_not_main``. ``changed_paths`` are the files the PR changes, from git
    (:func:`check_lock_change_alone`, D35). ``rehearsal`` (development only) allows
    fixture names and the rehearsal gate (:func:`check_production`). Raises
    :class:`Refused` on any failure."""
    number = check_pr_number(number)
    if not isinstance(head_sha, str) or _COMMIT.fullmatch(head_sha) is None:
        raise Refused("invalid_name", detail_code="head_sha")
    check_lock_change_alone(changed_paths)
    check_rehearsal(rehearsal)
    if not isinstance(reviewers, Reviewers):
        raise Refused("policy_invalid", detail_code="reviewers")
    base, head = parse(base_raw), parse(head_raw)
    for role, pinned in head.models.items():
        check_production(role, pinned, rehearsal=rehearsal)
    if not changed_roles(base, head):
        return {}
    changes, _ = assess_changes(
        base,
        head,
        source=source,
        policy=policy,
        cache_dir=cache_dir,
        testsets_lock=testsets_lock,
        thresholds_dir=thresholds_dir,
        rehearsal=rehearsal,
    )
    pr = github.pull_request(number)
    if pr.base_ref != MAIN_BRANCH:
        raise Refused("base_not_main")
    if not secrets.compare_digest(head_sha, pr.head_sha):
        raise Refused("not_approved", detail_code="head_moved")
    check_approvals(pr, reviewers, changes)
    return {c.role: c.kind for c in changes}


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
    older than ``current`` (semver order, never string order) whose sign-off for this
    role verifies through the loader's own check, :func:`verify.verify_pin` with
    ``require_signoff=True``, and passes :func:`check_production`. Being in the store, or
    having a ``signoff.json``, is not enough (P16). The entry's hash and gate are the
    sign-off's. ``only`` looks at that one version, ``first`` stops at the latest
    candidate (each check downloads and hashes a whole version). ``rehearsal``
    (development only) relaxes :func:`check_production`."""
    names.check_role(role)
    check_rehearsal(rehearsal)
    verify.check_environment(policy)
    name = current.name
    now = names.Version.parse(current.version)
    prefix = f"models/{name}/"
    versions: set[str] = set()
    for key in source.list(prefix):
        parts = key[len(prefix) :].split("/")
        if len(parts) == 2 and parts[1] == "signoff.json" and _ok(names.check_version, parts[0]):
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
        entry = _signed_off_entry(source, role, name, version, policy, cache_dir, rehearsal)
        if entry is not None:
            out.append(entry)
            if first:
                break
    return out


def _signed_off_entry(
    source: ListStore,
    role: str,
    name: str,
    version: str,
    policy: signing.TrustPolicy,
    cache_dir: Path | str,
    rehearsal: bool = False,
) -> Pin | None:
    """The pin a verified sign-off of ``name@version`` for ``role`` names, or None. A
    store outage propagates; any other refusal means "not a candidate"."""
    key = store.signoff_key(name, version)
    for vid in list(reversed(source.versions(key)))[: verify.MAX_VERSIONS]:
        raw = _read_version(source, key, vid, signoff_record.MAX_BYTES)
        if raw is None:
            continue
        try:
            s = signoff_record.parse(raw)
            if s.role != role or (s.name, s.version) != (name, version):
                continue
            entry = Pin(name, version, s.manifest_sha256, s.gate)
            check_production(role, entry, rehearsal=rehearsal)
            verify.verify_pin(source, role, entry, policy, cache_dir, require_signoff=True)
        except Refused as e:
            if e.reason == "store_unavailable":
                raise
            continue
        return entry
    return None


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
    :func:`rollback_candidates` (or ``to``, which must be one of them: ``not_approved``/
    ``not_a_candidate`` otherwise, P16) and write the lock with :func:`write`. A role
    the lock doesn't pin is ``lock_invalid``/``role``; no candidate at all is
    ``not_approved``/``no_candidate``. The PR it goes in follows ``lock-check``'s rollback
    rule. ``rehearsal`` (development only) relaxes :func:`check_production`."""
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
        raise Refused("not_approved", detail_code=detail)
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
    verifier over the base's pin, its role from its own manifest, no gate or sign-off."""

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
                require_signoff=False,
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
    "CLINICAL_NOTE",
    "FIXTURE_PREFIX",
    "LABEL",
    "MAIN_BRANCH",
    "ListStore",
    "MergedPullRequests",
    "RollbackResult",
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
    "assess_changes",
    "base_verifier",
    "changed_roles",
    "check_approvals",
    "check_dependencies",
    "check_evaluation",
    "check_pr_number",
    "check_production",
    "check_rehearsal",
    "has_clinical_note",
    "empty",
    "find_signed_manifest",
    "lock_check",
    "parse",
    "parse_base_pin",
    "parse_reviewers",
    "pin",
    "read",
    "read_reviewers",
    "rollback",
    "rollback_candidates",
    "thresholds_sha256",
    "to_bytes",
    "write",
]
