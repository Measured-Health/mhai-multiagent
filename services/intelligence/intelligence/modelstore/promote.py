"""The promote pipeline: ``mhai-models promote`` (dry run and promotion, design section 7.1).

The steps, in order, each a function of its own:

1. :func:`check_request` and :func:`check_inputs`: names, versions, the source's options,
   the runtime URL and the gates, before any I/O. Not a dry run: the promoter's store, the
   signer (trusted for ``manifest``), the trust policy, the workflow run and the retention
   must all be present (``not_allowed_here``), and nothing is touched until they are.
2. :func:`reserve` (not a dry run): ``models/<name>/<version>/claim.json`` with a
   conditional create, before any download. A taken version stops here
   (``version_exists``, P7: reserve, then check).
3. :func:`fetch_run`: the run's files into a fresh 0700 scratch folder
   (:func:`intelligence.modelstore.sources.scratch`, removed on every exit path), then
   :func:`files_hash` over them. With ``--expect-files`` (the dry run's printed hash, passed
   on by the workflow) the hash must match (``hash_mismatch``, P26): the files the approver
   saw are the files promoted.
4. :func:`check_files` (Task 13's ``check_run`` on the scratch copy, which must equal what
   was fetched; one PUT caps a file at 5 GiB) and :func:`check_training` (Task 14's
   ``training.load`` and ``training.check``; the manifest names this model).
5. Evaluation. The suite comes from the role (:func:`suite_for`). The extractor comes only
   from ``config.json``'s architecture (:func:`choose_extractor`), never from a name the
   run declares for itself: ``MhaiTinyExtractor`` is the tiny extractor, a known VLM
   architecture is :class:`~intelligence.doceval.openai_compat.OpenAICompatExtractor` at
   ``--runtime-url``, anything else is ``no_runtime``. No doceval stub can be chosen, and
   :func:`evaluate` refuses one anyway. This module, not the caller or the model, sets
   each extractor's engine and hardware. The split (:func:`select_split`): with
   ``MHAI_EVAL_ROLE=locked`` and the eval reader's credentials, the locked split through
   ``testsets.fetch`` (Task 17); anywhere else the dev split, and the report says so.
6. :func:`warmup`: the extractor on ``testdata/models/warmup/page.png``, recorded as
   ``warmup.json`` ``{page_sha256, output_sha256, engine, engine_version}``.
7. :func:`make_documents`: ``evaluation.json``, ``evaluation.md``, ``model_card.md``,
   ``data_statement.md``, ``training_manifest.json`` and ``warmup.json``.
8. Not a dry run: :func:`upload_run` puts every file and document under
   ``runs/<run_id>/``, :func:`write_manifest` builds the manifest with every
   ``store_version_id`` (``manifest.build``: the only bytes ever signed), signs it, checks
   the signature against the policy, and writes ``manifest.sig`` and then
   ``manifest.json`` last. A crash before ``manifest.json`` leaves a claim and orphan
   objects but no version: the loader needs a manifest (P8).

A dry run never builds a manifest and never touches the store, even when one is in reach.
Promotion stores a candidate whether or not it passes its gates; the gates are enforced at
pin time (Task 20), and the summary says which failed.

**The split a promotion is measured on.** Outside development a promotion (not a dry
run) needs the locked split (``MHAI_EVAL_ROLE=locked`` with the eval reader's credentials)
and ``--expect-files``, or it is ``not_allowed_here`` before the claim. A development
rehearsal may promote on the dev split; the manifest's ``evaluation.split`` and the summary
then say ``dev``, and pin (Task 20) refuses it.

**Every PUT is read back** (:func:`put_once`): each scratch file is streamed again and must
still have the size and SHA-256 Task 13 recorded, and after the PUT the store's own
checksum and size of the stored version must match too (``hash_mismatch``), so nothing that
changed between the check and the upload is ever named by a manifest. A PUT that succeeded
on the server whose reply was lost is retried by the SDK and then reports
``version_exists``. The object is ours if the key has exactly one version with our size and
SHA-256 (for the claim, the same bytes: the same run id, actor and a fresh nonce), locked in
compliance mode at least as long as we asked; then we carry on with its version id.
Anything else is ``version_exists``.

**The VLM runtime** (phase 0 contract): ``--runtime-url`` must be a loopback IP literal
(``127.0.0.0/8`` or ``::1``), where the workflow has started vLLM itself from the verified
files fetched into this job's scratch. Phase 0 cannot prove that the server serves those
files, so a promotion (not a dry run) with ``--runtime-url`` is ``no_runtime``.
TODO(GPU task): a hash handshake in which the server proves it serves the fetched files and
reports its engine version; then allow it for a promotion.

**Logs** carry names, versions, run ids, counts, hashes and reason codes only: never file
contents, page text, ground truth, credentials, paths or URLs.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import logging
import os
import re
import secrets
from collections.abc import Callable, Iterator, Mapping, Sequence
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from intelligence.doceval import extractors, report, runner
from intelligence.modelstore import (
    canonical,
    documents,
    files,
    manifest,
    names,
    signing,
    sources,
    store,
    testsets,
    training,
)
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectStore

log = logging.getLogger(__name__)

REPO = Path(__file__).resolve().parents[4]
WARMUP_PAGE = REPO / "testdata" / "models" / "warmup" / "page.png"
EVAL_ROLE_ENV = "MHAI_EVAL_ROLE"
LOCKED_ROLE = "locked"
CLAIM_SCHEMA = "mhai.models.claim/1"
SOURCES = manifest.SOURCES
SUITES: Mapping[str, str] = {"extract": "clinical_document"}
TINY_ARCHITECTURE = "MhaiTinyExtractor"  # doceval.tiny.ARCHITECTURE
# Vision-language architectures served by vLLM that the extraction role may use. The
# list is closed: a new one is a reviewed change.
KNOWN_VLM_ARCHITECTURES: frozenset[str] = frozenset(
    {
        "Idefics3ForConditionalGeneration",
        "LlavaForConditionalGeneration",
        "LlavaNextForConditionalGeneration",
        "PaliGemmaForConditionalGeneration",
        "Qwen2VLForConditionalGeneration",
        "Qwen2_5_VLForConditionalGeneration",
    }
)
VLM_ENGINE = "vllm"
# A fixture or rehearsal model's name; never promoted or pinned outside development (P14).
FIXTURE_PREFIX = "fixture-"
VLM_HARDWARE = "unknown"  # TODO(GPU task): the runner reports the GPU it ran on
MAX_WARMUP_PAGE_BYTES = 8 * 1024 * 1024
MAX_SIGNATURE_BYTES = signing.MAX_SIGNATURE_DOC_BYTES
DEFAULT_DEADLINE_S = 60.0
FILE_TOO_LARGE = "file_too_large_for_store"

_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_HF_REPO = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,95}/[A-Za-z0-9][A-Za-z0-9_.-]{0,95}", re.ASCII)
_DRIVE_ID = re.compile(r"[A-Za-z0-9_-]{10,128}", re.ASCII)
_STOPS = {"timeouts": "timeouts", "hostile_scoring": "hostile_scoring"}


def _invalid(detail: str) -> Refused:
    return Refused("invalid_name", detail_code=detail)


# ---------------------------------------------------------------- the inputs


@dataclass(frozen=True, slots=True)
class Request:
    """What the operator (the workflow) asked for. Names only, never paths."""

    name: str
    version: str
    source: str
    run: str
    hf_repo: str | None = None
    hf_revision: str | None = None
    drive_folder_id: str | None = None
    dry_run: bool = True
    expect_files: str | None = None
    runtime_url: str | None = None
    gates: tuple[str, ...] = ()  # empty: every gate


@dataclass(frozen=True, slots=True)
class WorkflowRun:
    """The GitHub workflow run that promotes: from the runner's own environment."""

    workflow_run_id: str
    workflow_ref: str
    commit: str
    actor: str

    def promotion(self, at: str) -> manifest.Promotion:
        return manifest.Promotion(
            self.workflow_run_id, self.workflow_ref, self.commit, self.actor, at
        )


@dataclass(frozen=True, slots=True)
class DevSplit:
    """The dev split: a built set at ``path``, or built into scratch at ``scale``."""

    path: Path | None = None
    scale: float = 1.0


@dataclass(frozen=True, slots=True)
class LockedSplit:
    """The locked test split, read as the eval reader (Task 17)."""

    reader: ObjectStore
    entry: testsets.LockEntry
    policy: signing.TrustPolicy


Split = DevSplit | LockedSplit


@dataclass(frozen=True)
class Context:
    """Everything the pipeline uses, made by the CLI from the environment (or by a test).

    ``source_for`` makes the run's source once the scratch folder exists (an HF source
    keeps its cache there). ``store``, ``signer``, ``policy``, ``workflow`` and
    ``retention_days`` are for a promotion only; a dry run never reads them.
    ``base_verifier`` verifies a promoted base model's pin
    (:func:`intelligence.modelstore.lock.base_verifier`); without one a promoted base is
    refused (``base_unverified``)."""

    registry: training.Registry
    licences: training.Licences
    source_for: Callable[[sources.Scratch], sources.Source]
    split: Split
    store: ObjectStore | None = field(default=None, repr=False)
    signer: signing.Signer | None = field(default=None, repr=False)
    policy: signing.TrustPolicy | None = field(default=None, repr=False)
    workflow: WorkflowRun | None = None
    retention_days: int | None = None
    scratch_parent: Path | None = None
    warmup_page: Path = WARMUP_PAGE
    deadline_s: float = DEFAULT_DEADLINE_S
    base_verifier: Callable[[str], None] | None = field(default=None, repr=False)


def _is_loopback_literal(host: str) -> bool:
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def check_runtime_url(url: str) -> str:
    """``http(s)://<loopback IP literal>[:port][/path]``, nothing else (no user info,
    query or fragment, no host name: ``localhost`` can resolve anywhere)."""
    ok = isinstance(url, str) and url.isascii() and len(url) <= 512
    if ok:
        try:
            parts = urlsplit(url)
            _ = parts.port
            host = parts.hostname or ""
        except ValueError:
            ok = False
    if ok:
        ok = (
            parts.scheme in ("http", "https")
            and _is_loopback_literal(host)
            and parts.username is None
            and parts.password is None
            and not parts.query
            and not parts.fragment
            and "@" not in parts.netloc
        )
    if not ok:
        raise Refused("not_allowed_here", detail_code="runtime_url")
    return url


def check_request(req: Request) -> None:
    """Step 1, the part that needs nothing but the request: run before any I/O."""
    if not isinstance(req, Request):
        raise _invalid("request")
    names.check_name(req.name)
    names.check_version(req.version)
    names.check_run_id(req.run)
    if req.source not in SOURCES:
        raise _invalid("source")
    if req.source == "hf":
        if req.hf_repo is None or req.hf_revision is None:
            raise _invalid("hf_options")
        if not isinstance(req.hf_repo, str) or _HF_REPO.fullmatch(req.hf_repo) is None:
            raise _invalid("hf_repo")
        if ".." in req.hf_repo:
            raise _invalid("hf_repo")
        if not isinstance(req.hf_revision, str) or not training.is_commit(req.hf_revision):
            raise Refused("revision_unpinned")  # P23: a branch or a tag can move
    elif req.hf_repo is not None or req.hf_revision is not None:
        raise _invalid("hf_options")
    if req.source == "drive":
        folder = req.drive_folder_id
        if not isinstance(folder, str) or _DRIVE_ID.fullmatch(folder) is None:
            raise _invalid("drive_folder")
    elif req.drive_folder_id is not None:
        raise _invalid("drive_folder")
    if not isinstance(req.dry_run, bool):
        raise _invalid("dry_run")
    if req.expect_files is not None and (
        not isinstance(req.expect_files, str) or _SHA256.fullmatch(req.expect_files) is None
    ):
        raise _invalid("expect_files")
    if not isinstance(req.gates, tuple) or any(g not in report.GATES for g in req.gates):
        raise _invalid("gate")
    if len(set(req.gates)) != len(req.gates):
        raise _invalid("gate")
    if req.runtime_url is not None:
        check_runtime_url(req.runtime_url)
        if not req.dry_run:
            # TODO(GPU task): the server proves it serves the fetched files (a hash
            # handshake) and reports its engine version; until then no VLM is promoted.
            raise Refused("no_runtime", detail_code="runtime_unverified")


def _source_ref(req: Request) -> str:
    if req.source == "hf":
        return f"{req.hf_repo}@{req.hf_revision}"
    if req.source == "drive":
        assert req.drive_folder_id is not None
        return req.drive_folder_id
    return manifest.LOCAL_SOURCE_REF


def _check_signer(signer: signing.Signer, policy: signing.TrustPolicy) -> None:
    """The signer must be one the policy trusts for ``manifest``. Nothing is signed here:
    the only bytes ever signed for ``manifest`` are :func:`manifest.build`'s output."""
    if not isinstance(policy, signing.TrustPolicy):
        raise Refused("policy_invalid", detail_code="type")
    trusted = {(k.key_id, k.alg) for k in policy.signers["manifest"]}
    if (signer.key_id, signer.alg) not in trusted:
        raise Refused("signer_untrusted")


def check_inputs(req: Request, ctx: Context) -> None:
    """Step 1: the request, and for a promotion everything it needs to write."""
    check_request(req)
    if not isinstance(ctx, Context):
        raise Refused("not_allowed_here", detail_code="context")
    if req.dry_run:
        return
    rehearsal = signing.is_development()
    if not rehearsal and report.REHEARSAL_GATE in req.gates:
        # P14: nothing measured only against the rehearsal bar is signed outside a
        # development rehearsal, so no such version exists in a production store.
        raise Refused("not_allowed_here", detail_code="rehearsal_gate")
    if not rehearsal and req.name.startswith(FIXTURE_PREFIX):
        raise Refused("fixture_name")
    if not rehearsal and not isinstance(ctx.split, LockedSplit):
        # The gates signed into a manifest are measured on the locked split; only a
        # development rehearsal may promote on the dev split (recorded as such).
        raise Refused("not_allowed_here", detail_code="locked_split_required")
    if not rehearsal and req.expect_files is None:
        # P26: the files promoted are the files the approver saw in the dry run.
        raise Refused("not_allowed_here", detail_code="expect_files_required")
    for value, detail in (
        (ctx.store, "store"),
        (ctx.signer, "signer"),
        (ctx.policy, "policy"),
        (ctx.workflow, "workflow"),
    ):
        if value is None:
            raise Refused("not_allowed_here", detail_code=detail)
    days = ctx.retention_days
    if not isinstance(days, int) or isinstance(days, bool) or days < 1:
        raise Refused("not_allowed_here", detail_code="retention_days")
    assert ctx.signer is not None and ctx.policy is not None and ctx.workflow is not None
    _check_signer(ctx.signer, ctx.policy)
    ctx.workflow.promotion("2000-01-01T00:00:00Z")  # every field held to its pattern now


# ---------------------------------------------------------------- step 2: reserve


@dataclass(frozen=True, slots=True)
class Claim:
    key: str
    version_id: str


def _check_stored(
    info: store.ObjectInfo, version_id: str, size: int, sha256: str, until: datetime
) -> bool:
    """The stored version is exactly ours: its id, size and SHA-256 (the store's own
    checksum of what it received), locked in compliance mode at least until ``until``."""
    held = info.retain_until
    return (
        info.version_id == version_id
        and info.size == size
        and secrets.compare_digest(info.sha256, sha256)
        and info.lock_mode == store.LOCK_MODE
        and isinstance(held, datetime)
        and held.tzinfo is not None
        and held >= until
    )


def put_once(
    target: ObjectStore, key: str, data: bytes | Path, size: int, sha256: str, days: int
) -> str:
    """``put_new``, then the stored version read back: the store's SHA-256 and size must be
    the ones checked (``hash_mismatch`` otherwise: bytes changed between the check and the
    upload) and it must be locked in compliance mode for at least ``days``.

    If ``put_new`` reports ``version_exists``, the object there may be ours (a retried PUT
    whose first attempt succeeded): it is adopted only if it is the key's one version, with
    our size and SHA-256, compliance-locked at least as long as we asked. Anything else is
    ``version_exists``."""
    until = datetime.now(UTC).replace(microsecond=0) + timedelta(days=days)
    vid: str | None = None
    try:
        vid = target.put_new(key, data, days)
    except Refused as exc:
        if exc.reason != "version_exists":
            raise
    if vid is not None:
        if not _check_stored(target.head(key, vid), vid, size, sha256, until):
            raise Refused("hash_mismatch", detail_code="stored")
        return vid
    found = target.versions(key)
    if len(found) != 1:
        raise Refused("version_exists", detail_code="not_ours")
    if not _check_stored(target.head(key, found[0]), found[0], size, sha256, until):
        raise Refused("version_exists", detail_code="not_ours")
    log.info("promote put retried and found ours: versions=1 sha256=%s", sha256)
    return found[0]


def claim_bytes(run_id: str, actor: str) -> bytes:
    """The claim: the run, the actor and a fresh nonce, so a second run with the same ids
    never mistakes another run's claim for its own retried one."""
    return canonical.dumps(
        {
            "schema": CLAIM_SCHEMA,
            "run_id": names.check_run_id(run_id),
            "actor": actor,
            "nonce": secrets.token_hex(16),
        }
    )


def reserve(
    target: ObjectStore, name: str, version: str, run_id: str, actor: str, days: int
) -> Claim:
    """Step 2: the conditional create of the version's claim, before anything else."""
    key = store.claim_key(name, version)
    body = claim_bytes(run_id, actor)
    vid = put_once(target, key, body, len(body), hashlib.sha256(body).hexdigest(), days)
    log.info("promote reserved: name=%s version=%s run=%s", name, version, run_id)
    return Claim(key, vid)


# ---------------------------------------------------------------- step 3: fetch


def files_hash(entries: Sequence[files.FileEntry]) -> str:
    """The files hash (``--expect-files``): SHA-256 of the canonical JSON of the sorted
    ``[{path, size, sha256}]``."""
    rows = sorted(
        ({"path": e.path, "size": e.size, "sha256": e.sha256} for e in entries),
        key=lambda r: str(r["path"]),
    )
    return hashlib.sha256(canonical.dumps(rows)).hexdigest()


def fetch_run(source: sources.Source, name: str, run: str, dest: Path) -> list[files.FileEntry]:
    """Step 3: list the run and fetch each file into ``dest`` (the scratch run folder)."""
    listed = source.list_run(name, run)
    if not listed:
        raise Refused("missing_file", detail_code="empty_run")
    out: list[files.FileEntry] = []
    for f in listed:
        sha = source.fetch(f, dest / f.name, f.size)
        out.append(files.FileEntry(f.name, f.size, sha))
    out.sort(key=lambda e: e.path)
    log.info("promote fetched: name=%s run=%s files=%d", name, run, len(out))
    return out


def check_expected(entries: Sequence[files.FileEntry], expect: str | None) -> str:
    """The files hash, which must equal ``expect`` when one is given (P26)."""
    got = files_hash(entries)
    if expect is not None and not secrets.compare_digest(got, expect):
        raise Refused("hash_mismatch", detail_code="expect_files")
    return got


# ---------------------------------------------------------------- step 4: checks


def check_files(
    run_dir: Path, fetched: Sequence[files.FileEntry] | None = None
) -> list[files.FileEntry]:
    """Step 4a: Task 13's checks on the run folder; with ``fetched``, the checked files must
    be exactly the fetched ones. A file larger than one PUT is refused (TODO(Task 27): a
    conditional multipart upload)."""
    entries = files.check_run(run_dir)
    if fetched is not None:
        got = [(e.path, e.size, e.sha256) for e in entries]
        if got != [(e.path, e.size, e.sha256) for e in sorted(fetched, key=lambda e: e.path)]:
            raise Refused("hash_mismatch", detail_code="fetched")
    for e in entries:
        if e.size > store.MAX_PUT_BYTES:
            raise Refused("too_large", detail_code=FILE_TOO_LARGE)
    return entries


def check_training(
    run_dir: Path,
    name: str,
    registry: training.Registry,
    licences: training.Licences,
    *,
    verify_base: Callable[[str], None] = training.unverified_pin,
) -> training.TrainingCheck:
    """Step 4b: the training manifest, through ``training.load`` and ``training.check``;
    it must name the model being promoted (``identity_mismatch``). A promoted base is
    checked by ``verify_base`` (by default refused)."""
    tm = training.load_run(run_dir)
    if tm.name != name:
        raise Refused("identity_mismatch", detail_code="training_manifest_name")
    return training.check(tm, registry, licences, verify_pin=verify_base)


@dataclass(frozen=True, slots=True)
class FolderCheck:
    files: tuple[files.FileEntry, ...]
    files_sha256: str
    training: training.TrainingCheck


def check_folder(
    run_dir: Path | str, registry: training.Registry, licences: training.Licences
) -> FolderCheck:
    """``mhai-models check DIR``: steps 3 and 4 on a local folder (no source, no copy:
    ``check_run`` opens nothing through a link). The folder's training manifest names
    the model."""
    entries = check_files(Path(run_dir))
    tm = training.load_run(Path(run_dir))
    tc = training.check(tm, registry, licences)
    return FolderCheck(tuple(entries), files_hash(entries), tc)


# ---------------------------------------------------------------- step 5: evaluation


def suite_for(role: str) -> str:
    """The evaluation suite for a role; a role without one is ``no_suite``."""
    suite = SUITES.get(role)
    if suite is None:
        raise Refused("no_suite")
    return suite


@dataclass(frozen=True)
class Chosen:
    """The extractor picked for a run, with the engine this module records for it."""

    extractor: extractors.Extractor
    engine: str
    engine_version: str | None


def _architecture(run_dir: Path) -> str:
    failed = False
    try:
        config = files.check_json(run_dir / "config.json", "config.json")
    except Refused:
        failed = True
    if failed:
        raise Refused("no_runtime", detail_code="config")
    arch = config.get("architectures")
    if not isinstance(arch, list) or len(arch) != 1 or not isinstance(arch[0], str):
        raise Refused("no_runtime", detail_code="architecture")
    return arch[0]


def _numpy_version() -> str:
    import numpy

    return str(numpy.__version__)


def choose_extractor(
    run_dir: Path, name: str, role: str, version: str, runtime_url: str | None
) -> Chosen:
    """Step 5's extractor, from ``config.json``'s architecture only."""
    arch = _architecture(run_dir)
    model_versions = {names.check_role(role): names.check_version(version)}
    chosen: Chosen
    if arch == TINY_ARCHITECTURE:
        from intelligence.doceval import tiny

        failed = False
        try:
            ex = tiny.TinyExtractor(run_dir, model_versions=model_versions)
        except (tiny.TinyInvalid, Refused, OSError, ValueError):
            failed = True
        if failed:
            raise Refused("no_runtime", detail_code="tiny_invalid")
        chosen = Chosen(ex, "numpy", _numpy_version())
    elif arch in KNOWN_VLM_ARCHITECTURES:
        if runtime_url is None:
            raise Refused("no_runtime", detail_code="runtime_url")
        from intelligence.doceval import openai_compat

        url = check_runtime_url(runtime_url)
        failed = False
        try:
            vlm = openai_compat.OpenAICompatExtractor(
                url,
                names.check_name(name),
                model_versions=model_versions,
                engine=VLM_ENGINE,
                hardware=VLM_HARDWARE,
            )
        except (ValueError, OSError):
            failed = True
        if failed:
            raise Refused("no_runtime", detail_code="runtime_config")
        chosen = Chosen(vlm, VLM_ENGINE, None)
    else:
        raise Refused("no_runtime", detail_code="architecture")
    _not_a_stub(chosen.extractor)
    return chosen


def _not_a_stub(ex: extractors.Extractor) -> extractors.ExtractorInfo:
    failed = False
    try:
        info = extractors.check_info(ex.info())
    except ValueError:
        failed = True
    if failed:
        raise Refused("not_allowed_here", detail_code="extractor_info")
    if report.is_stub(info):
        raise Refused("not_allowed_here", detail_code="stub_extractor")
    return info


def select_split(
    env: Mapping[str, str],
    *,
    reader: ObjectStore | None,
    lock_entry: testsets.LockEntry | None,
    policy: signing.TrustPolicy | None,
    dev_set: Path | None,
    dev_scale: float = 1.0,
) -> Split:
    """The evaluation split. ``MHAI_EVAL_ROLE=locked`` (the ``model-eval`` and
    ``model-promotion`` environments) needs the eval reader, the lock entry and the trust
    policy, or it is refused (P19); unset, it is the dev split; any other value is
    refused."""
    role = env.get(EVAL_ROLE_ENV)
    if role is None or role == "":
        return DevSplit(dev_set, dev_scale)
    if role != LOCKED_ROLE:
        raise Refused("not_allowed_here", detail_code="eval_role")
    if reader is None:
        raise Refused("not_allowed_here", detail_code="eval_reader")
    if lock_entry is None:
        raise Refused("not_allowed_here", detail_code="testset_lock")
    if policy is None:
        raise Refused("not_allowed_here", detail_code="trust_policy")
    return LockedSplit(reader, lock_entry, policy)


@dataclass(frozen=True, slots=True)
class SplitDir:
    path: Path
    name: str  # dev or test


def prepare_split(split: Split, scratch_root: Path) -> SplitDir:
    """The split's folder, checked: a dev split must say ``dev`` and carry no lock; the
    locked split comes from the store and must be the locked ``test`` split."""
    if isinstance(split, LockedSplit):
        path = testsets.fetch(split.reader, split.entry, split.policy, scratch_root / "eval")
        meta = report.testset_meta(path)
        if meta["purpose"] != "test" or not secrets.compare_digest(
            str(meta["sha256"]), split.entry.sha256
        ):
            raise Refused("testset_mismatch", detail_code="locked_split")
        return SplitDir(path, "test")
    if not isinstance(split, DevSplit):
        raise Refused("not_allowed_here", detail_code="split")
    if split.path is None:
        from intelligence.doceval import sets

        failed = False
        try:
            sets.build("dev", None, scratch_root / "dev", split.scale, version="1.0.0")
        except sets.SetError:
            failed = True
        if failed:
            raise Refused("evaluation_failed", detail_code="dev_split")
        path = scratch_root / "dev" / "dev"
    else:
        path = Path(split.path)
    meta = report.testset_meta(path)
    if meta["purpose"] != "dev" or meta["locked"]:
        raise Refused("not_allowed_here", detail_code="split")
    return SplitDir(path, "dev")


DOCEVAL_LOGGER = "intelligence.doceval"


class _WarningsOnly(logging.Filter):
    def filter(self, record: logging.LogRecord) -> bool:
        return record.levelno >= logging.WARNING


@contextmanager
def _quiet_doceval() -> Iterator[None]:
    """No ``intelligence.doceval`` logger, nor any logger below it (whatever its own level
    or handlers), emits below WARNING while this is held: the runner's DEBUG and INFO
    lines carry document ids (of the locked split, in an eval environment). A logger's
    filter applies to every record made on that logger, before any handler sees it, so
    one is put on each logger of the subtree. A logger created while this is held (a
    doceval module imported during the run) has no filter, so the parent's level is also
    held at WARNING: such a logger inherits it unless it sets a level of its own."""
    quiet = _WarningsOnly()
    names_now = [
        n
        for n in list(logging.root.manager.loggerDict)
        if n == DOCEVAL_LOGGER or n.startswith(DOCEVAL_LOGGER + ".")
    ]
    parent = logging.getLogger(DOCEVAL_LOGGER)
    saved_level = parent.level
    loggers = [logging.getLogger(n) for n in {DOCEVAL_LOGGER, *names_now}]
    for lg in loggers:
        lg.addFilter(quiet)
    parent.setLevel(logging.WARNING)
    try:
        yield
    finally:
        parent.setLevel(saved_level)
        for lg in loggers:
            lg.removeFilter(quiet)


def promotion_gates(req: Request) -> tuple[str, ...]:
    """The gates to measure: those asked for; with none, every gate, except that a
    promotion outside development is measured on the production gates only (P14)."""
    if req.gates:
        return tuple(req.gates)
    if not req.dry_run and not signing.is_development():
        return tuple(g for g in report.GATES if g in report.PRODUCTION_GATES)
    return tuple(report.GATES)


def gate_thresholds(gates: Sequence[str]) -> tuple[report.Thresholds, ...]:
    return tuple(report.packaged_thresholds(g) for g in (gates or report.GATES))


def evaluate(
    chosen: Chosen,
    set_dir: Path,
    gates: Sequence[report.Thresholds],
    notebook_metrics: Mapping[str, Any] | None,
    *,
    deadline_s: float = DEFAULT_DEADLINE_S,
) -> dict[str, Any]:
    """Step 5: run the extractor over the split and build the report (aggregates only)."""
    info = _not_a_stub(chosen.extractor)
    report.check_gates(info, gates)
    meta = report.testset_meta(set_dir)
    with _quiet_doceval():
        stopped: str | None = None
        try:
            result = runner.run(chosen.extractor, set_dir, per_page_deadline_s=deadline_s)
        except Refused as exc:
            if exc.reason != "evaluation_failed" or exc.detail_code not in _STOPS:
                raise
            stopped = _STOPS[exc.detail_code]
    if stopped is not None:
        return report.stopped_report(
            info, testset=meta, reason=stopped, gates=gates, engine_version=chosen.engine_version
        )
    if result.testset_sha256 != meta["sha256"] or result.purpose != meta["purpose"]:
        raise Refused("testset_mismatch")  # the set changed under the run
    return report.build_report(
        result,
        gates,
        testset_name=str(meta["name"]),
        engine_version=chosen.engine_version,
        notebook_metrics=notebook_metrics,
    )


# ---------------------------------------------------------------- step 6: warm-up


def _output_bytes(document: Any) -> bytes:
    if isinstance(document, (bytes, bytearray, str)):
        raw = document.encode("utf-8") if isinstance(document, str) else bytes(document)
        failed = False
        try:
            document = json.loads(raw)
        except ValueError:
            failed = True
        if failed:
            raise Refused("warmup_mismatch", detail_code="output")
    if not isinstance(document, Mapping):
        raise Refused("warmup_mismatch", detail_code="output")
    from intelligence.doceval import schema

    failed = False
    try:
        schema.validate_stored(document)
        text = json.dumps(
            document, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False
        )
    except Exception:  # the schema validator's own errors: a fixed code only
        failed = True
    if failed:
        raise Refused("warmup_mismatch", detail_code="output")
    return text.encode("ascii")


def warmup(chosen: Chosen, page: Path, *, deadline_s: float = DEFAULT_DEADLINE_S) -> bytes:
    """Step 6: ``warmup.json``, the extractor's output on the fixed warm-up page."""
    failed = False
    try:
        png = files.read_regular(page, MAX_WARMUP_PAGE_BYTES)
    except Refused:
        failed = True
    if failed:
        raise Refused("warmup_mismatch", detail_code="page")
    with _quiet_doceval():
        result = chosen.extractor.extract([png], deadline_s=deadline_s)
    if not isinstance(result, extractors.Extracted):
        raise Refused("warmup_mismatch", detail_code="not_extracted")
    out = _output_bytes(result.document)
    return canonical.dumps(
        {
            "page_sha256": hashlib.sha256(png).hexdigest(),
            "output_sha256": hashlib.sha256(out).hexdigest(),
            "engine": chosen.engine,
            "engine_version": chosen.engine_version,
        }
    )


# ---------------------------------------------------------------- step 7: documents


def make_documents(
    rep: Mapping[str, Any],
    checked: training.TrainingCheck,
    registry: training.Registry,
    licences: training.Licences,
    *,
    training_manifest: bytes,
    warmup_json: bytes,
) -> dict[str, bytes]:
    """Step 7: the six documents of the manifest, by name."""
    docs = {
        "evaluation.json": report.dumps(rep),
        "evaluation.md": report.render_markdown(rep).encode("utf-8"),
        "model_card.md": documents.model_card(checked, rep, licences).encode("utf-8"),
        "data_statement.md": documents.data_statement(checked.manifest, registry).encode("utf-8"),
        "training_manifest.json": bytes(training_manifest),
        "warmup.json": bytes(warmup_json),
    }
    if set(docs) != set(manifest.DOCUMENTS):
        raise Refused("manifest_invalid", detail_code="documents")
    return docs


def _training_bytes(run_dir: Path, entries: Sequence[files.FileEntry]) -> bytes:
    raw = files.read_regular(run_dir / training.FILE_NAME, files.JSON_CAP)
    listed = {e.path: e.sha256 for e in entries}
    if listed.get(training.FILE_NAME) != hashlib.sha256(raw).hexdigest():
        raise Refused("hash_mismatch", detail_code="training_manifest")
    return raw


# ---------------------------------------------------------------- step 8: write


def _rehash(run_dir: Path, entry: files.FileEntry) -> None:
    """Stream the scratch file again: its size and SHA-256 must still be what Task 13's
    check recorded, or nothing more is uploaded (``hash_mismatch``). ``put_once`` then
    compares the store's own checksum of what it received."""
    failed = False
    digest = hashlib.sha256()
    n = 0
    try:
        with open(run_dir / entry.path, "rb", opener=_nofollow) as fh:
            while block := fh.read(files.BLOCK_SIZE):
                digest.update(block)
                n += len(block)
    except OSError:
        failed = True
    if failed:
        raise Refused("hash_mismatch", detail_code="scratch_changed")
    if n != entry.size or not secrets.compare_digest(digest.hexdigest(), entry.sha256):
        raise Refused("hash_mismatch", detail_code="scratch_changed")


def _nofollow(path: str, flags: int) -> int:
    return os.open(path, flags | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))


def upload_run(
    target: ObjectStore,
    name: str,
    version: str,
    run_id: str,
    run_dir: Path,
    entries: Sequence[files.FileEntry],
    docs: Mapping[str, bytes],
    days: int,
) -> tuple[list[manifest.FileRecord], dict[str, manifest.DocumentRecord]]:
    """Step 8a: every file and document under ``runs/<run_id>/``, each with ``put_new``."""
    records: list[manifest.FileRecord] = []
    for e in sorted(entries, key=lambda e: e.path):
        _rehash(run_dir, e)
        key = store.run_file_key(name, version, run_id, e.path)
        vid = put_once(target, key, run_dir / e.path, e.size, e.sha256, days)
        records.append(manifest.FileRecord(e.path, e.size, e.sha256, vid))
    doc_records: dict[str, manifest.DocumentRecord] = {}
    for doc in sorted(docs):
        body = docs[doc]
        sha = hashlib.sha256(body).hexdigest()
        key = store.run_doc_key(name, version, run_id, doc)
        doc_records[doc] = manifest.DocumentRecord(
            sha, put_once(target, key, body, len(body), sha, days)
        )
    log.info(
        "promote uploaded: name=%s version=%s run=%s files=%d documents=%d",
        name,
        version,
        run_id,
        len(records),
        len(doc_records),
    )
    return records, doc_records


def write_manifest(
    target: ObjectStore,
    signer: signing.Signer,
    policy: signing.TrustPolicy,
    name: str,
    version: str,
    body: bytes,
    days: int,
) -> str:
    """Step 8b: sign ``body`` (which must be :func:`manifest.build` output: it is parsed
    back strictly first), check the signature against the policy, then write
    ``manifest.sig`` and ``manifest.json`` last. Returns the manifest's SHA-256."""
    m = manifest.parse(body)
    if (m.name, m.version) != (name, version):
        raise Refused("identity_mismatch", detail_code="manifest")
    sig = signer.sign("manifest", body)
    signing.Verifier().verify("manifest", body, sig, policy)
    sig_bytes = sig.to_bytes()
    put_once(
        target,
        store.manifest_sig_key(name, version),
        sig_bytes,
        len(sig_bytes),
        hashlib.sha256(sig_bytes).hexdigest(),
        days,
    )
    sha = manifest.sha256(body)
    put_once(target, store.manifest_key(name, version), body, len(body), sha, days)
    log.info("promote manifest written: name=%s version=%s sha256=%s", name, version, sha)
    return sha


# ---------------------------------------------------------------- the pipeline


@dataclass(frozen=True)
class Summary:
    """What ``promote`` prints (:meth:`to_obj`), plus the documents (never printed)."""

    name: str
    version: str
    run: str
    dry_run: bool
    files_sha256: str
    split: str
    testset: str
    verdicts: Mapping[str, str]
    manifest_sha256: str | None
    documents: Mapping[str, bytes] = field(default_factory=dict, repr=False)

    def to_obj(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "version": self.version,
            "run": self.run,
            "dry_run": self.dry_run,
            "files_sha256": self.files_sha256,
            "split": self.split,
            "testset": self.testset,
            "verdicts": dict(sorted(self.verdicts.items())),
            "gates_failed": sorted(g for g, v in self.verdicts.items() if v != "pass"),
            "manifest_sha256": self.manifest_sha256,
        }


def _now() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def promote(req: Request, ctx: Context) -> Summary:
    """Run the eight steps for ``req``; see the module docstring. Raises :class:`Refused`
    with a fixed code on any failure; the scratch folder is removed on every path."""
    check_inputs(req, ctx)  # 1
    log.info(
        "promote started: name=%s version=%s run=%s source=%s dry_run=%s",
        req.name,
        req.version,
        req.run,
        req.source,
        req.dry_run,
    )
    target = None if req.dry_run else ctx.store
    if target is not None:  # 2
        assert ctx.workflow is not None and ctx.retention_days is not None
        reserve(target, req.name, req.version, req.run, ctx.workflow.actor, ctx.retention_days)
    gates = gate_thresholds(promotion_gates(req))
    with sources.scratch(ctx.scratch_parent) as sc:
        source = ctx.source_for(sc)  # 3
        fetched = fetch_run(source, req.name, req.run, sc.run)
        files_sha = check_expected(fetched, req.expect_files)
        entries = check_files(sc.run, fetched)  # 4
        checked = check_training(
            sc.run,
            req.name,
            ctx.registry,
            ctx.licences,
            verify_base=ctx.base_verifier or training.unverified_pin,
        )
        role = checked.manifest.role
        suite_for(role)  # 5
        chosen = choose_extractor(sc.run, req.name, role, req.version, req.runtime_url)
        if target is not None and chosen.engine_version is None:
            # TODO(GPU task): the runtime reports its version in the hash handshake.
            raise Refused("no_runtime", detail_code="runtime_unverified")
        split = prepare_split(ctx.split, sc.root)
        if target is not None and split.name != "test" and not signing.is_development():
            raise Refused("not_allowed_here", detail_code="locked_split_required")
        notebook = training.report_metrics(checked.manifest)
        rep = evaluate(chosen, split.path, gates, notebook, deadline_s=ctx.deadline_s)
        warm = warmup(chosen, ctx.warmup_page, deadline_s=ctx.deadline_s)  # 6
        docs = make_documents(  # 7
            rep,
            checked,
            ctx.registry,
            ctx.licences,
            training_manifest=_training_bytes(sc.run, entries),
            warmup_json=warm,
        )
        verdicts = {g: str(v["verdict"]) for g, v in rep["verdicts"].items()}
        ts = rep["testset"]
        testset = f"{ts['name']}@{ts['version']}#{ts['sha256']}"
        manifest_sha: str | None = None
        if target is not None:  # 8
            assert ctx.signer is not None and ctx.policy is not None
            assert ctx.workflow is not None and ctx.retention_days is not None
            days = ctx.retention_days
            file_records, doc_records = upload_run(
                target, req.name, req.version, req.run, sc.run, entries, docs, days
            )
            body = manifest.build(
                name=req.name,
                version=req.version,
                role=role,
                run=manifest.Run(req.run, req.source, _source_ref(req)),
                files=file_records,
                documents=doc_records,
                base_model=checked.manifest.base,
                dependencies=(),
                evaluation=manifest.Evaluation(
                    hashlib.sha256(docs["evaluation.json"]).hexdigest(),
                    testset,
                    {g: "pass" if v == "pass" else "fail" for g, v in verdicts.items()},
                    split.name,
                ),
                runtime=manifest.Runtime(chosen.engine, str(chosen.engine_version), None),
                promotion=ctx.workflow.promotion(_now()),
                trained_by=checked.manifest.trained_by,
            )
            manifest_sha = write_manifest(
                target, ctx.signer, ctx.policy, req.name, req.version, body, days
            )
    summary = Summary(
        name=req.name,
        version=req.version,
        run=req.run,
        dry_run=req.dry_run,
        files_sha256=files_sha,
        split=split.name,
        testset=testset,
        verdicts=verdicts,
        manifest_sha256=manifest_sha,
        documents=docs,
    )
    log.info(
        "promote done: name=%s version=%s split=%s gates_failed=%d promoted=%s",
        req.name,
        req.version,
        split.name,
        len(summary.to_obj()["gates_failed"]),
        manifest_sha is not None,
    )
    return summary


__all__ = [
    "CLAIM_SCHEMA",
    "EVAL_ROLE_ENV",
    "KNOWN_VLM_ARCHITECTURES",
    "LOCKED_ROLE",
    "SUITES",
    "WARMUP_PAGE",
    "Chosen",
    "Claim",
    "Context",
    "DevSplit",
    "FolderCheck",
    "LockedSplit",
    "Request",
    "Split",
    "SplitDir",
    "Summary",
    "WorkflowRun",
    "check_expected",
    "check_files",
    "check_folder",
    "check_inputs",
    "check_request",
    "check_runtime_url",
    "check_training",
    "choose_extractor",
    "claim_bytes",
    "evaluate",
    "fetch_run",
    "files_hash",
    "gate_thresholds",
    "make_documents",
    "prepare_split",
    "promote",
    "put_once",
    "reserve",
    "select_split",
    "suite_for",
    "upload_run",
    "warmup",
    "write_manifest",
]
