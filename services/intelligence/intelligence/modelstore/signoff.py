"""``signoff.json`` (``mhai.models.signoff/1``): the record that a pinned version was
approved, and the sign-off that writes it (plan Task 21, design section 7.6).

The record
----------
Canonical JSON, integers and strings only, exactly these keys::

    schema, name, version, role, gate,
    manifest_sha256, evaluation_sha256,
    pr, merge_commit,
    clinical_reviewers: [login, ...], platform_reviewers: [login, ...]   sorted, distinct
    promotion_actor, trained_by, at

Every login is lowercase only (:func:`intelligence.modelstore.names.check_login`): GitHub
logins are case-insensitive, so ``Alice`` would be the same person as ``alice``. ``at``
is the PR's ``merged_at`` (``YYYY-MM-DDTHH:MM:SSZ``): when the approved change reached
``main``. Taking it from the merge, not the clock, makes a re-run build the same bytes.
:func:`parse` is strict (the load-time verifier, Task 22, uses it); anything else is
``not_approved``: a sign-off that can't be read is no sign-off. :func:`build` makes the
bytes and refuses a record in which a reviewer is the promoter or the trainer, or one
person is both the clinical and the platform reviewer (the verifier's own rule).

The sign-off
------------
:func:`sign_off` is ``mhai-models signoff`` (in ``model-signoff.yml``, after a push to
``main``). P17: a lock that reached ``main`` without its approvals (an admin merge, a
direct push) gets no sign-off. In order; no sign-off is written until every check has
passed for every role and every object's retention is extended:

1. **Inputs.** The commit, head and base shas are 40 hex; the retention is a whole number
   of days; the environment and policy pass
   :func:`intelligence.modelstore.verify.check_environment`; the signer's key is in the
   policy's ``signoff`` set and not in its ``manifest`` set (``signer_untrusted``).
   TODO(Task 26): the production signer; phase 0 has only the development
   ``FileSigner``, which refuses outside ``MHAI_MODELS_ENV=development``. The files the
   merge changed (``git diff --no-renames <base sha> <commit>``, worked out by the CLI
   from git) may, when they include ``models.lock`` or ``models/**``, be only those,
   ``docs/**`` and ``CHANGELOG.md`` (D35, D35b,
   :func:`intelligence.modelstore.lock.check_lock_change_alone`:
   ``not_allowed_here``/``code_changed_with_lock``). A push that changes no role stops
   here (exit 0): there is nothing to sign.
2. **The merge commit** (D33, :func:`check_merge`). ``--commit`` must have exactly two
   parents as GitHub reports them (``GET /git/commits/{sha}``), else
   ``not_approved``/``not_a_merge_commit``: a squash or rebase merge, a direct push or a
   batched push has one parent, and its first parent could be one of the PR's own
   commits, whose ``reviewers.json`` or trust policy the PR controls. The first parent
   must be ``--base-sha`` (``base_mismatch``), which is ``main`` before the push; the
   second must be ``--head-sha`` (``head_mismatch``). TODO(H4): branch protection on
   ``main`` allows merge commits only (no squash, no rebase, no merge queue batching)
   and requires branches to be up to date; otherwise a model change can't be signed off.
3. **The merged PR.** GitHub's pull requests for ``--commit`` (``GET
   /commits/{sha}/pulls``): none, or more than one, is ``not_approved`` (``no_pr``,
   ``many_prs``); the one must be merged, into ``main``, with ``merge_commit_sha`` exactly
   ``--commit`` (``not_merged``, ``not_main``), and its head must be ``--head-sha``
   (``head_mismatch``).
4. **The merge is what was approved** (D32, :func:`check_merge_matches_head`): every
   role the merge changed holds exactly the entry of ``--head-lock``, the lock at the
   PR's head, which is what the reviewers approved; otherwise
   ``not_approved``/``merge_differs_from_head`` (a hand-edited merge). Checked before any
   store read.
5. **The store half of lock-check**, :func:`intelligence.modelstore.lock.assess_changes`:
   each changed role verified and classified ``new``, ``rollback`` or ``removed``, a new
   one with its evaluation re-checked against the thresholds and ``testsets.lock`` at
   ``--base-sha``. A version this very merge already signed off (a re-run) stays ``new``.
6. **The approvals**, :func:`intelligence.modelstore.lock.check_approvals` on that PR,
   with ``reviewers.json`` at ``--base-sha``: the same function ``lock-check`` runs,
   never a copy of its rules.
7. **The records.** For each new version: the approving reviewers on the PR's head
   commit, lowercase, minus the PR's author, the promoter and the trainer, a person who
   is on both lists counted once (:func:`choose_reviewers`); the record built and signed
   (purpose ``signoff``), the signature verified against the policy. Then the state of
   ``signoff.json`` and ``signoff.sig`` in the store:

   * a sign-off that verifies for this pin is already there (a re-run, or a rollback to
     a version signed off before): nothing to write;
   * neither key exists: both are written;
   * ``signoff.json`` exists without ``signoff.sig`` (a crash between the two puts):
     **adopted only if it is the key's one version and byte for byte the record built
     now** (``at`` is the merge time, so a re-run builds the same bytes); then only the
     signature is written. Refusing would leave the version unsignable forever (the key
     can't be written twice), and adopting anything else would sign bytes this run didn't
     check;
   * anything else (another record, a second version, a signature that doesn't verify):
     ``not_approved``/``signoff_conflict``.

   A changed role that ends up neither signed off nor removed is
   ``not_approved``/``not_signed_off``: never a silent exit 0.
8. **Retention first.** Every object of each pinned version (new or rollback), by its
   recorded version id, never "latest": each file, each document, ``manifest.json``,
   ``manifest.sig`` and whichever of ``signoff.json`` and ``signoff.sig`` already exist,
   extended to ``MODEL_STORE_APPROVED_RETENTION_DAYS`` from now (no default, pending H2;
   at most ``MODEL_STORE_MAX_RETENTION_DAYS``). Only ever longer: an object already
   locked longer is left as it is. A failure part-way (exit 3) leaves no new sign-off, so
   "a sign-off exists" always means "its version is retained"; a re-run starts again.
9. **Writes**, with the ``signoff`` store role: ``signoff.json`` then ``signoff.sig``,
   each a conditional create (:func:`intelligence.modelstore.promote.put_once`), locked
   for the approved retention as they are created.

A refusal raises :class:`~intelligence.modelstore.reasons.Refused`; the CLI prints one
JSON summary and exits 2 (3 when the store or GitHub is unavailable), and the workflow
writes it to the job summary (Task 24). The loader then refuses that pin (Task 22) and
the service stays not ready on it. A record adopted or found that isn't locked in
compliance mode fails the retention step (``store_unavailable``/``not_locked``, exit 3)
and needs a person.

What Task 24's ``model-signoff.yml`` passes (D32, D33):

* ``--commit "$GITHUB_SHA"`` (the push's ``after``), fetched into ``--base-dir``'s
  repository: the CLI diffs ``--base-sha`` against it there (D35);
* ``--base-sha`` = the push event's ``before`` (``github.event.before``): ``main`` before
  the push, which must be the merge commit's first parent;
* ``--base-dir``: a checkout of ``--base-sha``, from which the command also runs (its
  code and data, as lock-check does, D31); the CLI checks that its ``HEAD`` is
  ``--base-sha``. ``models/reviewers.json``, ``models/trust-policy.json``,
  ``--testsets`` and ``--thresholds-dir`` are read from it;
* ``--before`` = ``git show "$BASE_SHA":models.lock`` (must equal ``--base-dir``'s) and
  ``--after`` = ``git show "$GITHUB_SHA":models.lock``, written to files;
* ``--head-sha``: the merged PR's head (``.[0].head.sha`` of ``GET
  /commits/$GITHUB_SHA/pulls``, which is also the merge commit's second parent), and
  ``--head-lock`` = ``git show "$HEAD_SHA":models.lock`` after ``git fetch origin
  "refs/pull/$N/head"`` (the branch may be deleted); a missing file fails the job;
* the environment: ``GITHUB_TOKEN`` (``contents: read``, ``pull-requests: read``),
  ``GITHUB_REPOSITORY``, the store as the ``signoff`` role (``MODEL_STORE_ENDPOINT``,
  ``MODEL_STORE_BUCKET``, ``MODEL_STORE_SIGNOFF_ACCESS_KEY``,
  ``MODEL_STORE_SIGNOFF_SECRET_KEY``), ``MODEL_STORE_APPROVED_RETENTION_DAYS`` and the
  sign-off signer (``MHAI_MODELS_SIGNER`` until Task 26).
"""

from __future__ import annotations

import hashlib
import logging
import re
import secrets
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import TYPE_CHECKING, Any, BinaryIO, Protocol

from intelligence.doceval.report import GATES
from intelligence.modelstore import canonical, manifest, names, signing, store, verify
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo

# ``verify`` imports this module for the record; nothing of ``verify`` is used here at
# import time, so either may be imported first. ``lock`` (which imports both) is imported
# where it is used.
if TYPE_CHECKING:
    from intelligence.modelstore import lock
    from intelligence.modelstore.testsets import LockEntry as TestsetEntry

log = logging.getLogger(__name__)

SCHEMA = "mhai.models.signoff/1"
MAX_BYTES = 64 * 1024
MAX_REVIEWERS = 16

_KEYS = frozenset(
    {
        "schema",
        "name",
        "version",
        "role",
        "gate",
        "manifest_sha256",
        "evaluation_sha256",
        "pr",
        "merge_commit",
        "clinical_reviewers",
        "platform_reviewers",
        "promotion_actor",
        "trained_by",
        "at",
    }
)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_COMMIT = re.compile(r"[0-9a-f]{40}", re.ASCII)
_TIMESTAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", re.ASCII)
_MAX_PR = 2**31 - 1


def _invalid(detail: str) -> Refused:
    return Refused("not_approved", detail_code=detail)


def _match(value: object, pattern: re.Pattern[str]) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _named(check: Any, value: object, detail: str) -> None:
    failed = False
    try:
        check(value)
    except Refused:
        failed = True
    if failed:
        raise _invalid(detail)


def _logins(value: object, detail: str) -> tuple[str, ...]:
    if (
        not isinstance(value, list)
        or not 1 <= len(value) <= MAX_REVIEWERS
        or not all(names.is_login(v) for v in value)
        or any(a >= b for a, b in zip(value, value[1:], strict=False))
    ):
        raise _invalid(detail)
    return tuple(value)


def _timestamp(value: object) -> str:
    if not _match(value, _TIMESTAMP):
        raise _invalid("at")
    failed = False
    try:
        datetime.strptime(str(value), "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        failed = True
    if failed:
        raise _invalid("at")
    return str(value)


@dataclass(frozen=True, slots=True)
class Signoff:
    name: str
    version: str
    role: str
    gate: str
    manifest_sha256: str
    evaluation_sha256: str
    pr: int
    merge_commit: str
    clinical_reviewers: tuple[str, ...]
    platform_reviewers: tuple[str, ...]
    promotion_actor: str
    trained_by: str
    at: str


def parse(b: bytes) -> Signoff:
    """A sign-off from its stored bytes: strict canonical JSON, exactly the keys above,
    every value to its pattern; ``not_approved`` otherwise."""
    failed = False
    try:
        obj = canonical.loads_strict(bytes(b), MAX_BYTES)
    except (Refused, TypeError):
        failed = True
    if failed:
        raise _invalid("json")
    if canonical.dumps(obj) != bytes(b):
        raise _invalid("not_canonical")
    if not isinstance(obj, dict) or set(obj) != _KEYS:
        raise _invalid("keys")
    if obj["schema"] != SCHEMA:
        raise _invalid("schema")
    _named(names.check_name, obj["name"], "name")
    _named(names.check_version, obj["version"], "version")
    _named(names.check_role, obj["role"], "role")
    if obj["gate"] not in GATES:
        raise _invalid("gate")
    for key in ("manifest_sha256", "evaluation_sha256"):
        if not _match(obj[key], _SHA256):
            raise _invalid(key)
    pr = obj["pr"]
    if not isinstance(pr, int) or isinstance(pr, bool) or not 1 <= pr <= _MAX_PR:
        raise _invalid("pr")
    if not _match(obj["merge_commit"], _COMMIT):
        raise _invalid("merge_commit")
    for key in ("promotion_actor", "trained_by"):
        if not names.is_login(obj[key]):
            raise _invalid(key)
    return Signoff(
        name=obj["name"],
        version=obj["version"],
        role=obj["role"],
        gate=obj["gate"],
        manifest_sha256=obj["manifest_sha256"],
        evaluation_sha256=obj["evaluation_sha256"],
        pr=pr,
        merge_commit=obj["merge_commit"],
        clinical_reviewers=_logins(obj["clinical_reviewers"], "clinical_reviewers"),
        platform_reviewers=_logins(obj["platform_reviewers"], "platform_reviewers"),
        promotion_actor=obj["promotion_actor"],
        trained_by=obj["trained_by"],
        at=_timestamp(obj["at"]),
    )


def to_bytes(s: Signoff) -> bytes:
    """The canonical bytes of ``s`` (what is stored and signed). Parsed back strictly."""
    obj = {
        "schema": SCHEMA,
        "name": s.name,
        "version": s.version,
        "role": s.role,
        "gate": s.gate,
        "manifest_sha256": s.manifest_sha256,
        "evaluation_sha256": s.evaluation_sha256,
        "pr": s.pr,
        "merge_commit": s.merge_commit,
        "clinical_reviewers": list(s.clinical_reviewers),
        "platform_reviewers": list(s.platform_reviewers),
        "promotion_actor": s.promotion_actor,
        "trained_by": s.trained_by,
        "at": s.at,
    }
    b = canonical.dumps(obj)
    parse(b)
    return b


def build(
    *,
    name: str,
    version: str,
    role: str,
    gate: str,
    manifest_sha256: str,
    evaluation_sha256: str,
    pr: int,
    merge_commit: str,
    clinical_reviewers: Iterable[str],
    platform_reviewers: Iterable[str],
    promotion_actor: str,
    trained_by: str,
    at: str,
) -> bytes:
    """The canonical bytes of a sign-off. The reviewer lists are sorted; a reviewer who
    is the promoter or the trainer, or one person on both lists, is
    ``not_approved``/``separation`` (the load-time verifier would refuse it anyway)."""
    clinical, platform = sorted(set(clinical_reviewers)), sorted(set(platform_reviewers))
    if set(clinical) & set(platform) or (set(clinical) | set(platform)) & {
        promotion_actor,
        trained_by,
    }:
        raise _invalid("separation")
    return to_bytes(
        Signoff(
            name=name,
            version=version,
            role=role,
            gate=gate,
            manifest_sha256=manifest_sha256,
            evaluation_sha256=evaluation_sha256,
            pr=pr,
            merge_commit=merge_commit,
            clinical_reviewers=tuple(clinical),
            platform_reviewers=tuple(platform),
            promotion_actor=promotion_actor,
            trained_by=trained_by,
            at=at,
        )
    )


# ---------------------------------------------------------------- the sign-off


class SignoffStore(Protocol):
    """The calls the sign-off makes, as the store's ``signoff`` role: the verifier's
    reads (:class:`intelligence.modelstore.verify.ReadStore`), conditional create and
    retention extension."""

    def versions(self, key: str) -> list[str]: ...

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo: ...

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes: ...

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]: ...

    def put_new(self, key: str, data: bytes | Path, retention_days: int) -> str: ...

    def extend_retention(self, key: str, version_id: str, days: int) -> datetime: ...


_COMMIT_SHA = re.compile(r"[0-9a-f]{40}", re.ASCII)


def check_signer(signer: signing.Signer, policy: signing.TrustPolicy) -> None:
    """The sign-off signer: trusted for ``signoff``, never a ``manifest`` key (the two
    workflows' signers are separate; Task 15)."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    ident = (signer.key_id, signer.alg)
    if ident not in {(k.key_id, k.alg) for k in policy.signers["signoff"]}:
        raise Refused("signer_untrusted", detail_code="signoff")
    if signer.key_id in {k.key_id for k in policy.signers["manifest"]}:
        raise Refused("signer_untrusted", detail_code="manifest_key")


def merged_pr(github: lock.MergedPullRequests, commit: str) -> lock.PullRequest:
    """The one PR merged into ``main`` at exactly ``commit`` (step 2)."""
    from intelligence.modelstore import lock

    numbers = github.pulls_for_commit(commit)
    if not numbers:
        raise _invalid("no_pr")
    if len(numbers) > 1:
        raise _invalid("many_prs")
    pr = github.pull_request(numbers[0])
    if (
        not pr.merged
        or pr.merge_commit_sha is None
        or pr.merged_at is None
        or not secrets.compare_digest(pr.merge_commit_sha, commit)
    ):
        raise _invalid("not_merged")
    if pr.base_ref != lock.MAIN_BRANCH:
        raise _invalid("not_main")
    return pr


def choose_reviewers(
    pr: lock.PullRequest, reviewers: lock.Reviewers, m: manifest.Manifest
) -> tuple[tuple[str, ...], tuple[str, ...]]:
    """The clinical and platform reviewers to record: approvals on the head commit by
    people on each list, never the PR's author, the promoter or the trainer. Someone on
    both lists is recorded once, where a list would otherwise be empty (else as
    clinical), so nobody is both. ``check_approvals`` has already passed, so both lists
    end up non-empty; if not, ``not_approved``/``approvals``."""
    from intelligence.modelstore import lock

    strict = lock.approvals(pr) - {pr.author, m.promotion.actor, m.trained_by}
    clinical_all, platform_all = strict & reviewers.clinical, strict & reviewers.platform
    both = clinical_all & platform_all
    clinical, platform = set(clinical_all - both), set(platform_all - both)
    for login in sorted(both):
        if not clinical:
            clinical.add(login)
        elif not platform:
            platform.add(login)
        else:
            clinical.add(login)
    if not clinical or not platform:
        raise _invalid("approvals")
    return tuple(sorted(clinical)[:MAX_REVIEWERS]), tuple(sorted(platform)[:MAX_REVIEWERS])


@dataclass(frozen=True, slots=True)
class _Plan:
    """One new version's sign-off: what to write, or the versions already there."""

    role: str
    entry: lock.Pin
    m: manifest.Manifest
    body: bytes
    sig: bytes | None
    json_vid: str | None
    sig_vid: str | None


@dataclass(frozen=True, slots=True)
class Result:
    """What :func:`sign_off` did: per role ``signed_off``, ``already_signed_off``
    (nothing written: a re-run or a rollback) or ``removed``; the PR; how many object
    versions had their retention lengthened."""

    roles: Mapping[str, str]
    pr: int | None
    retention_extended: int

    def to_obj(self) -> dict[str, Any]:
        return {
            "signoff": dict(self.roles),
            "pr": self.pr,
            "retention_extended": self.retention_extended,
        }


def _conflict() -> Refused:
    return _invalid("signoff_conflict")


def _plan_new(
    source: SignoffStore,
    role: str,
    entry: lock.Pin,
    m: manifest.Manifest,
    body: bytes,
    signer: signing.Signer,
    policy: signing.TrustPolicy,
) -> _Plan:
    """Step 5 for one new version (see the module docstring)."""
    try:
        found = verify.find_signoff(source, role, entry, m, policy)
    except Refused as e:
        if e.reason != "not_approved":
            raise
    else:
        return _Plan(role, entry, m, body, None, found.version_id, found.sig_version_id)
    json_key = store.signoff_key(m.name, m.version)
    sig_key = store.signoff_sig_key(m.name, m.version)
    json_vids = source.versions(json_key)
    json_vid: str | None = None
    if json_vids:
        if len(json_vids) != 1:
            raise _conflict()
        there = source.get(json_key, json_vids[0], max_bytes=MAX_BYTES)
        if not secrets.compare_digest(there, body):
            raise _conflict()
        json_vid = json_vids[0]
    if source.versions(sig_key):
        # With no signoff.json there is nothing a signature could be ours for; with ours
        # adopted, a signature that verified would have been found above.
        raise _conflict()
    doc = signer.sign("signoff", body)
    signing.Verifier().verify("signoff", body, doc, policy)
    return _Plan(role, entry, m, body, doc.to_bytes(), json_vid, None)


def _write(target: SignoffStore, plan: _Plan, days: int) -> tuple[str, str]:
    """Step 6: ``signoff.json`` (unless adopted), then ``signoff.sig``."""
    from intelligence.modelstore.promote import put_once

    name, version = plan.m.name, plan.m.version
    json_vid = plan.json_vid
    if json_vid is None:
        json_vid = put_once(
            target,  # type: ignore[arg-type]
            store.signoff_key(name, version),
            plan.body,
            len(plan.body),
            hashlib.sha256(plan.body).hexdigest(),
            days,
        )
    sig_vid = plan.sig_vid
    if sig_vid is None:
        assert plan.sig is not None
        sig_vid = put_once(
            target,  # type: ignore[arg-type]
            store.signoff_sig_key(name, version),
            plan.sig,
            len(plan.sig),
            hashlib.sha256(plan.sig).hexdigest(),
            days,
        )
    log.info(
        "signoff written: role=%s name=%s version=%s pr=%d",
        plan.role,
        name,
        version,
        parse(plan.body).pr,
    )
    return json_vid, sig_vid


def version_objects(
    source: verify.ReadStore,
    m: manifest.Manifest,
    manifest_sha256: str,
    policy: signing.TrustPolicy,
    signoff_vids: tuple[str | None, str | None] = (None, None),
) -> list[tuple[str, str]]:
    """Every object of the version as ``(key, version id)``: each file and document by
    its recorded id, the manifest version with the pinned hash, the ``manifest.sig``
    version that verifies over it, and those of the sign-off's two versions that already
    exist (``None``: not written yet)."""
    out = [
        (store.run_file_key(m.name, m.version, m.run.id, f.path), f.store_version_id)
        for f in m.files
    ]
    out += [
        (store.run_doc_key(m.name, m.version, m.run.id, d), rec.store_version_id)
        for d, rec in sorted(m.documents.items())
    ]
    manifest_vid, raw = verify.find_manifest_version(source, m.name, m.version, manifest_sha256)
    sig_vid = verify.manifest_signature_version(source, m.name, m.version, raw, policy)
    out.append((store.manifest_key(m.name, m.version), manifest_vid))
    out.append((store.manifest_sig_key(m.name, m.version), sig_vid))
    for key, vid in (
        (store.signoff_key(m.name, m.version), signoff_vids[0]),
        (store.signoff_sig_key(m.name, m.version), signoff_vids[1]),
    ):
        if vid is not None:
            out.append((key, vid))
    return out


def extend_retention(target: SignoffStore, objects: Iterable[tuple[str, str]], days: int) -> int:
    """Step 7: lengthen each ``(key, version id)`` to ``days`` from now; one already locked
    at least that long is left alone (never shortened). Returns how many were extended."""
    n = 0
    for key, vid in objects:
        try:
            target.extend_retention(key, vid, days)
        except Refused as e:
            if (e.reason, e.detail_code) == ("not_allowed_here", "retention_not_longer"):
                continue
            raise
        n += 1
    return n


def _check_days(days: object) -> int:
    if not isinstance(days, int) or isinstance(days, bool) or days < 1:
        raise Refused("not_allowed_here", detail_code="retention_days")
    return days


def check_merge(
    github: lock.MergedPullRequests, commit: str, base_sha: str, head_sha: str
) -> lock.PullRequest:
    """Steps 2 and 3: ``commit`` is a merge commit with exactly two parents, the first
    ``base_sha`` (``main`` before the push) and the second ``head_sha``; it is the merge
    of exactly one PR into ``main`` (:func:`merged_pr`), whose head is ``head_sha``.
    Anything else is ``not_approved``."""
    parents = github.commit_parents(commit)
    if len(parents) != 2:
        raise _invalid("not_a_merge_commit")
    if not secrets.compare_digest(parents[0], base_sha):
        raise _invalid("base_mismatch")
    pr = merged_pr(github, commit)
    if not secrets.compare_digest(pr.head_sha, head_sha) or not secrets.compare_digest(
        parents[1], head_sha
    ):
        raise _invalid("head_mismatch")
    return pr


def check_merge_matches_head(before: lock.Lock, after: lock.Lock, head: lock.Lock) -> None:
    """D32: every role the merge changed holds exactly the head lock's entry (the lock the
    reviewers approved), or ``not_approved``/``merge_differs_from_head``."""
    from intelligence.modelstore import lock

    for role in lock.changed_roles(before, after):
        if after.models.get(role) != head.models.get(role):
            raise _invalid("merge_differs_from_head")


def sign_off(
    before_raw: bytes,
    after_raw: bytes,
    commit: str,
    *,
    head_raw: bytes,
    head_sha: str,
    base_sha: str,
    github: lock.MergedPullRequests,
    reviewers: lock.Reviewers,
    target: SignoffStore,
    policy: signing.TrustPolicy,
    signer: signing.Signer,
    cache_dir: Path | str,
    testsets_lock: Mapping[tuple[str, str], TestsetEntry],
    thresholds_dir: Path | str,
    retention_days: int,
    changed_paths: Iterable[str],
) -> Result:
    """Sign off every version newly pinned by the merge ``commit`` (see the module
    docstring). ``before_raw`` is the lock at ``base_sha`` (``main`` before the push,
    which must be the commit's first parent), ``after_raw`` at the commit, ``head_raw`` at
    ``head_sha`` (the merged PR's head, which the reviewers approved); ``reviewers``,
    ``testsets_lock``, ``thresholds_dir`` and ``policy`` are ``main``'s at ``base_sha``.
    ``changed_paths`` are the files the merge changed (``git diff <base sha> <commit>``),
    checked by :func:`intelligence.modelstore.lock.check_lock_change_alone` (D35).
    Raises :class:`Refused`; no sign-off is written unless every role passed every check
    and every object's retention was extended."""
    from intelligence.modelstore import lock

    for value, detail in ((commit, "commit"), (head_sha, "head_sha"), (base_sha, "base_sha")):
        if not isinstance(value, str) or _COMMIT_SHA.fullmatch(value) is None:
            raise Refused("invalid_name", detail_code=detail)
    lock.check_lock_change_alone(changed_paths)
    days = _check_days(retention_days)
    if not isinstance(reviewers, lock.Reviewers):
        raise Refused("policy_invalid", detail_code="reviewers")
    verify.check_environment(policy)
    check_signer(signer, policy)
    before, after, head = lock.parse(before_raw), lock.parse(after_raw), lock.parse(head_raw)
    for role, pinned in after.models.items():
        lock.check_production(role, pinned)
    changed = lock.changed_roles(before, after)
    if not changed:
        return Result({}, None, 0)
    pr = check_merge(github, commit, base_sha, head_sha)
    assert pr.merged_at is not None
    check_merge_matches_head(before, after, head)  # before any store read

    def is_own(s: Signoff) -> bool:
        return s.pr == pr.number and secrets.compare_digest(s.merge_commit, commit)

    changes, _ = lock.assess_changes(
        before,
        after,
        source=target,
        policy=policy,
        cache_dir=cache_dir,
        testsets_lock=testsets_lock,
        thresholds_dir=thresholds_dir,
        is_own=is_own,
    )
    lock.check_approvals(pr, reviewers, changes)

    plans: list[_Plan] = []
    roles: dict[str, str] = {c.role: "removed" for c in changes if c.kind == "removed"}
    retained: list[tuple[str, str]] = []
    for c in changes:
        if c.kind == "removed":
            continue
        entry = after.models[c.role]
        assert c.manifest is not None
        m = c.manifest
        if c.kind == "rollback":
            found = verify.find_signoff(target, c.role, entry, m, policy)
            roles[c.role] = "already_signed_off"
            retained += version_objects(
                target, m, entry.manifest_sha256, policy, (found.version_id, found.sig_version_id)
            )
            continue
        clinical, platform = choose_reviewers(pr, reviewers, m)
        body = build(
            name=m.name,
            version=m.version,
            role=c.role,
            gate=entry.gate,
            manifest_sha256=entry.manifest_sha256,
            evaluation_sha256=m.evaluation.report_sha256,
            pr=pr.number,
            merge_commit=commit,
            clinical_reviewers=clinical,
            platform_reviewers=platform,
            promotion_actor=m.promotion.actor,
            trained_by=m.trained_by,
            at=pr.merged_at,
        )
        plan = _plan_new(target, c.role, entry, m, body, signer, policy)
        plans.append(plan)
        retained += version_objects(
            target, m, entry.manifest_sha256, policy, (plan.json_vid, plan.sig_vid)
        )
    if set(roles) | {p.role for p in plans} != set(changed):  # never a silent success
        raise _invalid("not_signed_off")

    # Every check passed for every role. Retention first (M2): a sign-off that exists
    # always means its version is kept; a failure here (exit 3) writes no record, and a
    # re-run starts again from the top.
    extended = extend_retention(target, retained, days)
    for plan in plans:
        done = plan.json_vid is not None and plan.sig_vid is not None
        if not done:
            _write(target, plan, days)  # created with the approved retention
        roles[plan.role] = "already_signed_off" if done else "signed_off"
    log.info(
        "signoff done: pr=%d roles=%d objects=%d extended=%d",
        pr.number,
        len(roles),
        len(retained),
        extended,
    )
    return Result(roles, pr.number, extended)


__all__ = [
    "MAX_BYTES",
    "SCHEMA",
    "Result",
    "Signoff",
    "SignoffStore",
    "build",
    "check_merge",
    "check_merge_matches_head",
    "check_signer",
    "choose_reviewers",
    "extend_retention",
    "merged_pr",
    "parse",
    "sign_off",
    "to_bytes",
    "version_objects",
]
