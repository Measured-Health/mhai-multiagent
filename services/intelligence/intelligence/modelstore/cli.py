"""``mhai-models``: the model store's command line.

Commands::

    mhai-models dev-keygen OUT_DIR
    mhai-models testset-build --suite clinical_document --version 1.0.0 [--scale S]
    mhai-models promote NAME VERSION --source drive|local|hf --run RUN
        [--hf-repo R --hf-revision SHA] [--dry-run] [--expect-files SHA256]
        [--runtime-url URL] [--gate phase1|phase3|rehearsal ...] [--dev-set DIR]
        [--report-dir DIR]
    mhai-models check DIR
    mhai-models make-fixture DIR [--variant V] [--seed N] [--name NAME]
    mhai-models store-init
    mhai-models pin ROLE NAME VERSION --gate phase1|phase3 [--lock PATH [--rehearsal]]
    mhai-models lock-check --base-dir MAIN_CHECKOUT --head HEAD_LOCK --head-sha SHA --pr N
        --testsets MAIN_CHECKOUT/models/testsets.lock
        --thresholds-dir MAIN_CHECKOUT/services/intelligence/intelligence/doceval/thresholds
        [--rehearsal]
    mhai-models signoff --base-dir BASE_CHECKOUT --base-sha BASE_SHA --before OLD_LOCK
        --after NEW_LOCK --commit SHA --head-sha HEAD_SHA --head-lock HEAD_LOCK
        --testsets BASE_CHECKOUT/models/testsets.lock
        --thresholds-dir BASE_CHECKOUT/services/intelligence/intelligence/doceval/thresholds
        [--rehearsal]
    mhai-models rollback ROLE [--to VERSION] [--lock PATH] [--rehearsal]

The CI rehearsal (Task 24, ``scripts/rehearse-promotion.sh``): ``--rehearsal`` on
``lock-check``, ``signoff`` and ``rollback`` allows a ``fixture-`` name and the ``rehearsal``
gate (:func:`intelligence.modelstore.lock.check_production`), and
``MHAI_MODELS_GITHUB_FAKE`` names a file of GitHub's answers
(:class:`intelligence.modelstore.lock.FileSession`) in place of the API. Both are
``not_allowed_here`` (``rehearsal``, ``github_fake``) outside development (both switches),
before any store or GitHub access; the production workflows set neither, and
``test_workflows.py`` checks it. ``make-fixture --name`` names the fixture's model.

``dev-keygen`` makes a development Ed25519 key pair in ``OUT_DIR`` (the private key with
mode 0600) and prints one JSON line with the key id and the public key PEM, never the
private key. It refuses outside ``MHAI_MODELS_ENV=development`` (``not_allowed_here``)
and never overwrites a key (``key_exists``).

``testset-build`` builds the locked test split inside the store
(:func:`intelligence.modelstore.testsets.build_and_lock`) as the eval writer and prints one
line, ``<suite>@<version>#<sha256>``, for ``models/testsets.lock``; nothing else of the split
is printed. Its names are checked before any I/O. Everything else comes from the environment
only: ``MODEL_STORE_ENDPOINT`` (TLS unless loopback), ``MODEL_STORE_BUCKET``,
``MODEL_STORE_REGION`` (optional), ``MODEL_STORE_EVAL_WRITER_ACCESS_KEY`` and
``MODEL_STORE_EVAL_WRITER_SECRET_KEY``, ``MODEL_STORE_TESTSET_RETENTION_DAYS`` (whole days,
at most the store's cap), ``MHAI_MODELS_SIGNER`` (the development key file; development
only), ``MHAI_MODELS_TRUST_POLICY`` (the signer must be trusted for ``testset``) and
``MHAI_MODELS_SCRATCH`` (optional: where the 0700 scratch folder is made).

``promote`` runs :func:`intelligence.modelstore.promote.promote` and prints one JSON summary
(name, version, run, files hash, split, test set, verdicts, failed gates, and the manifest's
SHA-256 when promoted). Its names are checked before any I/O. A dry run reads no store
credentials and writes nothing to the store; with ``--report-dir DIR`` (a dry run only: an
existing folder, not a symlink, holding no ``evaluation.json``, checked before anything
runs) it also writes the run's ``evaluation.json`` there (mode 0600, never overwritten),
the aggregate-only report a promotion would upload, for the workflow's ``evaluation.md``
(``mhai-doceval report``). A promotion reads ``MODEL_STORE_ENDPOINT``,
``MODEL_STORE_BUCKET``, ``MODEL_STORE_REGION``, ``MODEL_STORE_PROMOTER_ACCESS_KEY`` and
``MODEL_STORE_PROMOTER_SECRET_KEY``, ``MODEL_STORE_CANDIDATE_RETENTION_DAYS``,
``MHAI_MODELS_SIGNER``, ``MHAI_MODELS_TRUST_POLICY`` and the workflow run (``GITHUB_RUN_ID``,
``GITHUB_WORKFLOW_REF``, ``GITHUB_SHA``, ``GITHUB_ACTOR``); without any of them it is
``not_allowed_here``. The source: ``local`` reads ``MHAI_MODELS_LOCAL_ROOT``; ``hf`` the
repo and commit given; ``drive`` ``MHAI_MODELS_DRIVE_FOLDER_ID`` (refused as
``source_unavailable`` until the Drive session exists, TODO(Task 28), which also runs
``drive_check`` first). The split: with ``MHAI_EVAL_ROLE=locked``, the locked split named
by ``models/testsets.lock`` (``MHAI_MODELS_TESTSETS_LOCK``), read with
``MODEL_STORE_EVAL_READER_ACCESS_KEY`` and ``MODEL_STORE_EVAL_READER_SECRET_KEY``; otherwise
``--dev-set``, or the dev split built into scratch. The registries are
``models/approved-datasets.json`` and ``models/licences.json`` (``MHAI_MODELS_DATASETS``,
``MHAI_MODELS_LICENCES``).

``check DIR`` runs steps 3 and 4 (file and training manifest checks) on a local run folder
and prints its files hash. ``make-fixture`` writes a tiny synthetic run
(:func:`intelligence.modelstore.fixtures.make_run`). ``store-init`` is for rehearsal only: it
runs ``deploy/minio/init-model-store.sh`` (which needs ``mc``) and refuses unless
``MHAI_MODELS_ENV=development`` and ``MINIO_URL``'s host is ``127.0.0.1`` or ``::1``.

A run whose training manifest names a promoted base model has that base verified
(:func:`intelligence.modelstore.lock.base_verifier`), on a dry run as on a promotion: the
verifier is built on first use only, so a run with an upstream base reads no reader
credentials or trust policy for it. It needs ``MODEL_STORE_READER_ACCESS_KEY``,
``MODEL_STORE_READER_SECRET_KEY`` and ``MHAI_MODELS_TRUST_POLICY``; without them, or when
the base fails, the run is ``base_unverified`` (the inner reason as the detail).

``pin`` (:func:`intelligence.modelstore.lock.pin`) verifies ``NAME@VERSION`` with the
reader's credentials (``MODEL_STORE_ENDPOINT``, ``MODEL_STORE_BUCKET``,
``MODEL_STORE_READER_ACCESS_KEY``, ``MODEL_STORE_READER_SECRET_KEY``) and
``MHAI_MODELS_TRUST_POLICY``, checks its evaluation against ``models/testsets.lock``
(``MHAI_MODELS_TESTSETS_LOCK``) and writes ``models.lock`` (or ``--lock``). It prints
``{"pinned": {role, name, version, manifest_sha256, gate}}``. The production rules
(no ``fixture-`` name, no ``rehearsal`` gate, a known role) always apply unless
``--rehearsal`` is passed, which needs development (both switches) and an explicit
``--lock`` (``not_allowed_here`` otherwise): the decision is the flag, never a path
comparison. It opens no PR. Its trust policy is ``MHAI_MODELS_TRUST_POLICY``, any path:
``pin`` runs from a checkout of main (a person's or the rehearsal's), and whatever it
writes is judged again by ``lock-check`` against main's own policy.

``lock-check`` (:func:`intelligence.modelstore.lock.lock_check`, in ``model-lock.yml``)
trusts the PR for one thing only, the head lock:

* ``--head`` is a file holding ``models.lock`` at the PR's head commit, written by the
  workflow with ``git show "$HEAD_SHA":models.lock``, and ``--head-sha`` is that commit
  (``github.event.pull_request.head.sha``); both are required, and the check refuses if
  the PR's head has moved since.
* ``--base-dir`` is a checkout of the **current tip of main**, made at check time (for
  example ``git fetch origin main && git worktree add main-tip origin/main``), never the
  PR's base commit or its merge checkout. The base lock (``models.lock``) and
  ``models/reviewers.json`` are read from it; ``--testsets`` and ``--thresholds-dir`` are
  required and must be inside it (``not_allowed_here`` otherwise). A reviewer removed on
  main stops counting at once.
* The trust policy (who may sign manifests and sign-offs) is ``models/trust-policy.json``
  in ``--base-dir``, never the PR's: an ``MHAI_MODELS_TRUST_POLICY`` naming any other file
  is ``not_allowed_here``/``trust_policy_outside_base_dir``.
* The workflow runs this command from that same main checkout (its code as well as its
  data), so a PR can't change the checker that judges it.
* The PR's head commit must be fetched into that checkout's repository: the files the PR
  changes are ``git diff --no-renames <main tip>...<head sha>`` there (D35), and a PR
  that changes ``models.lock`` or ``models/**`` may change only those, ``docs/**`` and
  ``CHANGELOG.md`` (D35b; ``not_allowed_here``/``code_changed_with_lock``; git failing is
  ``changed_files_unknown``).

It reads the store as ``pin`` does and the PR with ``GITHUB_TOKEN`` and
``GITHUB_REPOSITORY`` (``GITHUB_API_URL`` optional). It prints ``{"lock_check": "pass",
"roles": {role: new|rollback|removed}}``; a lock with no changed role needs no store or
GitHub access. Verified files go to ``MHAI_MODELS_CACHE`` when set, else to a private
temporary folder removed afterwards.

``signoff`` (:func:`intelligence.modelstore.signoff.sign_off`, in ``model-signoff.yml``
after a push to main) checks that ``--commit`` is a two-parent merge of ``--base-sha``
(the push event's ``before``) and ``--head-sha`` (the merged PR's head), that every role
it changed is ``--head-lock``'s entry (the lock at the head), re-checks the PR's
approvals, extends retention and then writes ``signoff.json`` and ``signoff.sig``
(D32, D33; the full contract for ``model-signoff.yml`` is in
:mod:`intelligence.modelstore.signoff`). ``--base-dir`` is a checkout of ``--base-sha``
(it must be the repository's top level, ``base_dir_not_toplevel``, and its ``HEAD``
``--base-sha``, ``base_dir_not_base_sha``, both checked with ``git rev-parse``), from which
the command also runs; ``--commit`` must be fetched into that repository, since the
files the merge changed are ``git diff <base-sha> <commit>`` there (D35, as for
``lock-check``). Its data files are read with ``git show <base-sha>:<path>``, never
from the working tree, so a dirty checkout changes nothing: its ``models.lock`` must be
``--before`` (``lock_invalid``/``before_mismatch``), and ``models/reviewers.json``,
``models/trust-policy.json``, ``--testsets`` and ``--thresholds-dir`` are read from it
(an ``MHAI_MODELS_TRUST_POLICY`` naming another file is ``not_allowed_here``). ``--after``
is the lock at ``--commit``. It reads ``MODEL_STORE_APPROVED_RETENTION_DAYS`` (required, no
default, pending H2; at most ``MODEL_STORE_MAX_RETENTION_DAYS``), the store as the
``signoff`` role (``MODEL_STORE_SIGNOFF_ACCESS_KEY``, ``MODEL_STORE_SIGNOFF_SECRET_KEY``),
the sign-off signer (``MHAI_MODELS_SIGNER``; TODO(Task 26): the production signer, the
development ``FileSigner`` refuses outside development) and GitHub as ``lock-check``
does. It prints ``{"signoff": {role: signed_off|already_signed_off|removed}, "pr": N,
"retention_extended": N}``; a push that changes no role needs no store, signer or GitHub.

``rollback ROLE`` (:func:`intelligence.modelstore.lock.rollback`) pins ``ROLE`` to the latest
older version of its pinned name with a sign-off for this role that verifies (or
``--to VERSION``, which must be such a version: P16), reading the store as ``pin`` does,
and writes ``models.lock`` (or ``--lock``). It prints ``{"rollback": {"role", "from",
"to"}}``. The PR it goes in follows ``lock-check``'s rollback rule.

A promotion (not ``--dry-run``) outside ``MHAI_MODELS_ENV=development`` must run with
``MHAI_EVAL_ROLE=locked`` (the locked split), must pass ``--expect-files`` and may not pass
``--dev-set``; each is ``not_allowed_here`` otherwise.

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout; for
``promote``, ``check``, ``make-fixture``, ``store-init``, ``pin``, ``lock-check``,
``signoff`` and ``rollback`` with
``"detail"``, a fixed code, when there is one), 3 a file could not be read or written, or
(``promote``, ``check``, ``store-init``, ``pin``, ``lock-check``, ``signoff``, ``rollback``)
the store, the source or
GitHub is unavailable, 4 (``testset-build`` only) the split
is locked and its lock line printed, but the scratch copy could not be removed
(``{"warning": "scratch_not_removed"}`` on stderr, no path): remove the scratch root by hand.
An unexpected error is ``{"error": "internal"}`` and exit 3, with no traceback (unless
``MHAI_MODELS_DEBUG=1`` in development, to stderr).
"""

import argparse
import contextlib
import dataclasses
import math
import os
import re
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Callable, Iterator, Sequence
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from intelligence.doceval.report import GATES
from intelligence.modelstore import (
    canonical,
    lock,
    names,
    promote,
    signing,
    signoff,
    sources,
    store,
    testsets,
    training,
    verify,
)
from intelligence.modelstore.reasons import Refused

ENDPOINT_ENV = "MODEL_STORE_ENDPOINT"
BUCKET_ENV = "MODEL_STORE_BUCKET"
REGION_ENV = "MODEL_STORE_REGION"
EVAL_WRITER_ACCESS_ENV = "MODEL_STORE_EVAL_WRITER_ACCESS_KEY"
EVAL_WRITER_SECRET_ENV = "MODEL_STORE_EVAL_WRITER_SECRET_KEY"
TESTSET_RETENTION_ENV = "MODEL_STORE_TESTSET_RETENTION_DAYS"
SIGNER_ENV = "MHAI_MODELS_SIGNER"
TRUST_POLICY_ENV = "MHAI_MODELS_TRUST_POLICY"
SCRATCH_ENV = "MHAI_MODELS_SCRATCH"
PROMOTER_ACCESS_ENV = "MODEL_STORE_PROMOTER_ACCESS_KEY"
PROMOTER_SECRET_ENV = "MODEL_STORE_PROMOTER_SECRET_KEY"
EVAL_READER_ACCESS_ENV = "MODEL_STORE_EVAL_READER_ACCESS_KEY"
EVAL_READER_SECRET_ENV = "MODEL_STORE_EVAL_READER_SECRET_KEY"
READER_ACCESS_ENV = "MODEL_STORE_READER_ACCESS_KEY"
READER_SECRET_ENV = "MODEL_STORE_READER_SECRET_KEY"
SIGNOFF_ACCESS_ENV = "MODEL_STORE_SIGNOFF_ACCESS_KEY"
SIGNOFF_SECRET_ENV = "MODEL_STORE_SIGNOFF_SECRET_KEY"
APPROVED_RETENTION_ENV = "MODEL_STORE_APPROVED_RETENTION_DAYS"
CACHE_ENV = "MHAI_MODELS_CACHE"
CANDIDATE_RETENTION_ENV = "MODEL_STORE_CANDIDATE_RETENTION_DAYS"
LOCAL_ROOT_ENV = "MHAI_MODELS_LOCAL_ROOT"
DRIVE_FOLDER_ENV = "MHAI_MODELS_DRIVE_FOLDER_ID"
DATASETS_ENV = "MHAI_MODELS_DATASETS"
LICENCES_ENV = "MHAI_MODELS_LICENCES"
TESTSETS_LOCK_ENV = "MHAI_MODELS_TESTSETS_LOCK"
MINIO_URL_ENV = "MINIO_URL"
GITHUB_FAKE_ENV = "MHAI_MODELS_GITHUB_FAKE"
WORKFLOW_ENVS = ("GITHUB_RUN_ID", "GITHUB_WORKFLOW_REF", "GITHUB_SHA", "GITHUB_ACTOR")
MAX_SCALE = 10.0
MODELS_DIR = promote.REPO / "models"
PRODUCTION_LOCK = promote.REPO / "models.lock"
# Inside a checkout of main's tip (lock-check's --base-dir).
BASE_LOCK_PATH = "models.lock"
BASE_REVIEWERS_PATH = "models/reviewers.json"
BASE_TRUST_POLICY_PATH = "models/trust-policy.json"
INIT_SCRIPT = promote.REPO / "deploy" / "minio" / "init-model-store.sh"
UNAVAILABLE = frozenset({"store_unavailable", "source_unavailable", "github_unavailable"})
# store-init: loopback IP literals only (a name such as localhost can resolve anywhere).
STORE_INIT_HOSTS = frozenset({"127.0.0.1", "::1"})
DEBUG_ENV = "MHAI_MODELS_DEBUG"
# Commands whose refusals carry the fixed detail code, and whose store or source being
# unavailable is exit 3.
_DETAILED = frozenset(
    {"promote", "check", "make-fixture", "store-init", "pin", "lock-check", "signoff", "rollback"}
)


def _print(obj: dict[str, Any]) -> None:
    sys.stdout.write(canonical.dumps(obj).decode("ascii") + "\n")


def _dev_keygen(args: argparse.Namespace) -> int:
    kid, pem = signing.generate_dev_key(args.out_dir)
    _print({"key_id": kid, "public_key_pem": pem})
    return 0


def _env(name: str, reason: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise Refused(reason, detail_code="env")
    return value


def _scale(text: str) -> float:
    try:
        value = float(text)
    except ValueError:
        value = math.nan
    if not math.isfinite(value) or not 0 < value <= MAX_SCALE:
        raise Refused("invalid_name", detail_code="scale")
    return value


def _retention_days(name: str = TESTSET_RETENTION_ENV) -> int:
    raw = _env(name, "not_allowed_here")
    if re.fullmatch(r"[1-9][0-9]{0,4}", raw, re.ASCII) is None:
        raise Refused("not_allowed_here", detail_code="retention_days")
    days = int(raw)
    if days > store.retention_cap_from_env():
        raise Refused("not_allowed_here", detail_code="retention_days")
    return days


def _signer() -> signing.Signer:
    # TODO(Task 26): the production signer backend. Phase 0 has only the development
    # FileSigner, which refuses to load or sign outside MHAI_MODELS_ENV=development.
    return signing.FileSigner(_env(SIGNER_ENV, "not_allowed_here"))


def _testset_build(args: argparse.Namespace) -> int:
    # Names first, before any environment, file or network access.
    if args.suite not in testsets.SUITES:
        raise Refused("no_suite")
    version = names.check_version(args.version)
    scale = _scale(args.scale)
    endpoint = store.check_endpoint(_env(ENDPOINT_ENV, "store_unavailable"))
    bucket = _env(BUCKET_ENV, "store_unavailable")
    creds = store.Credentials(
        os.environ.get(EVAL_WRITER_ACCESS_ENV, ""), os.environ.get(EVAL_WRITER_SECRET_ENV, "")
    )
    days = _retention_days()
    target = store.S3Store(endpoint, bucket, creds, os.environ.get(REGION_ENV) or "us-east-1")
    signer = _signer()
    policy = signing.load_policy(_env(TRUST_POLICY_ENV, "policy_invalid"))
    built = testsets.build_and_lock(
        target,
        signer,
        policy,
        suite=args.suite,
        version=version,
        scale=scale,
        retention_days=days,
        scratch_parent=os.environ.get(SCRATCH_ENV) or None,
    )
    sys.stdout.write(built.entry.text + "\n")
    if not built.scratch_removed:
        # The split is locked, but its plaintext scratch copy is still on this disk.
        sys.stderr.write(canonical.dumps({"warning": "scratch_not_removed"}).decode() + "\n")
        return 4
    return 0


# ---------------------------------------------------------------- promote


def _registries() -> tuple[training.Registry, training.Licences]:
    datasets = os.environ.get(DATASETS_ENV) or str(MODELS_DIR / "approved-datasets.json")
    licences = os.environ.get(LICENCES_ENV) or str(MODELS_DIR / "licences.json")
    return training.load_registry(datasets), training.load_licences(licences)


def _s3(access_env: str, secret_env: str) -> store.S3Store | None:
    """The store as one role, or ``None`` when that role's credentials are not here."""
    access, secret = os.environ.get(access_env), os.environ.get(secret_env)
    if not access or not secret:
        return None
    endpoint = store.check_endpoint(_env(ENDPOINT_ENV, "store_unavailable"))
    bucket = _env(BUCKET_ENV, "store_unavailable")
    region = os.environ.get(REGION_ENV) or "us-east-1"
    return store.S3Store(endpoint, bucket, store.Credentials(access, secret), region)


def _workflow() -> promote.WorkflowRun | None:
    values = [os.environ.get(n) for n in WORKFLOW_ENVS]
    if not all(values):
        return None
    run_id, ref, commit, actor = (str(v) for v in values)
    # GITHUB_ACTOR keeps the account's display case; logins are case-insensitive, and the
    # manifest stores the lowercase spelling (names.check_login, ruling D30).
    return promote.WorkflowRun(run_id, ref, commit, actor.lower())


def _lock_entry() -> testsets.LockEntry | None:
    """The newest locked split of the extraction suite in ``models/testsets.lock``."""
    path = os.environ.get(TESTSETS_LOCK_ENV) or str(MODELS_DIR / "testsets.lock")
    suite = promote.SUITES["extract"]
    found = [e for (s, _), e in testsets.read_lock(path).items() if s == suite]
    if not found:
        return None
    return max(found, key=lambda e: names.Version.parse(e.version))


def _split(dev_set: str | None) -> promote.Split:
    reader: store.S3Store | None = None
    entry: testsets.LockEntry | None = None
    policy: signing.TrustPolicy | None = None
    if os.environ.get(promote.EVAL_ROLE_ENV) == promote.LOCKED_ROLE:
        reader = _s3(EVAL_READER_ACCESS_ENV, EVAL_READER_SECRET_ENV)
        entry = _lock_entry()
        if os.environ.get(TRUST_POLICY_ENV):
            policy = signing.load_policy(os.environ[TRUST_POLICY_ENV])
    return promote.select_split(
        os.environ,
        reader=reader,
        lock_entry=entry,
        policy=policy,
        dev_set=Path(dev_set) if dev_set else None,
    )


def _source_for(req: promote.Request) -> Any:
    def make(scratch: sources.Scratch) -> sources.Source:
        if req.source == "local":
            root = os.environ.get(LOCAL_ROOT_ENV)
            if not root:
                raise Refused("source_unavailable", detail_code="local_root")
            from intelligence.modelstore.sources.local import LocalSource

            return LocalSource(root)
        if req.source == "hf":
            from intelligence.modelstore.sources.hf import HFSource

            assert req.hf_repo is not None and req.hf_revision is not None
            return HFSource(req.hf_repo, req.hf_revision, scratch=scratch)
        # TODO(Task 28): the Drive session (HttpDriveApi), and drive_check first.
        raise Refused("source_unavailable", detail_code="drive_session")

    return make


def _reader_store() -> lock.ListStore:
    """The store with the reader's credentials (``not_allowed_here`` without them)."""
    reader = _s3(READER_ACCESS_ENV, READER_SECRET_ENV)
    if reader is None:
        raise Refused("not_allowed_here", detail_code="reader_credentials")
    return reader


def _trust_policy(path: Path | None = None) -> signing.TrustPolicy:
    """``MHAI_MODELS_TRUST_POLICY``, or (lock-check) the given file in main's checkout."""
    if path is None:
        return signing.load_policy(_env(TRUST_POLICY_ENV, "policy_invalid"))
    return signing.load_policy(path)


def _check_policy_env(path: Path) -> None:
    """lock-check reads the trust policy from main's checkout only. An
    ``MHAI_MODELS_TRUST_POLICY`` that names any other file (the PR's copy, which could add
    its own signer) is refused, not silently ignored."""
    given = os.environ.get(TRUST_POLICY_ENV)
    if given and Path(given).resolve() != path.resolve():
        raise Refused("not_allowed_here", detail_code="trust_policy_outside_base_dir")


def _github() -> lock.MergedPullRequests:
    """GitHub with the job's token; or, for the CI rehearsal only, the canned answers in
    ``MHAI_MODELS_GITHUB_FAKE`` (:class:`intelligence.modelstore.lock.FileSession`),
    which is ``not_allowed_here``/``github_fake`` outside development (both switches)."""
    fake = os.environ.get(GITHUB_FAKE_ENV)
    if fake:
        if not verify.is_development():
            raise Refused("not_allowed_here", detail_code="github_fake")
        return lock.FileSession.client(fake)
    return lock.GitHubClient.from_env()


def _testsets_lock(path: str | None = None) -> dict[tuple[str, str], testsets.LockEntry]:
    path = path or os.environ.get(TESTSETS_LOCK_ENV) or str(MODELS_DIR / "testsets.lock")
    return testsets.read_lock(path)


def _remove_cache(root: Path) -> None:
    """Remove a temporary cache (verified directories are read-only: give write back)."""
    for dirpath, dirnames, _ in os.walk(root):
        for d in dirnames:
            p = os.path.join(dirpath, d)
            if not os.path.islink(p):
                os.chmod(p, 0o700)
    shutil.rmtree(root, ignore_errors=True)


@contextlib.contextmanager
def _cache() -> Iterator[Path]:
    """``MHAI_MODELS_CACHE``, or a private temporary folder removed afterwards."""
    given = os.environ.get(CACHE_ENV)
    if given:
        yield Path(given)
        return
    tmp = Path(tempfile.mkdtemp(prefix="mhai-models-cache-", dir=os.environ.get(SCRATCH_ENV)))
    try:
        yield tmp
    finally:
        _remove_cache(tmp)


def _pin(args: argparse.Namespace) -> int:
    role, name, version, gate = args.role, args.name, args.version, args.gate
    names.check_role(role)  # names first, before any environment, file or store access
    names.check_name(name)
    names.check_version(version)
    # P14: the production rules unless an explicit rehearsal, in development, into a lock
    # named explicitly.
    if args.rehearsal and (not verify.is_development() or not args.lock):
        raise Refused("not_allowed_here", detail_code="rehearsal")
    production = not args.rehearsal
    lock_path = Path(args.lock) if args.lock else PRODUCTION_LOCK
    source = _reader_store()
    policy = _trust_policy()
    locked = _testsets_lock()
    with _cache() as cache_dir:
        entry = lock.pin(
            role,
            name,
            version,
            gate,
            source=source,
            policy=policy,
            cache_dir=cache_dir,
            lock_path=lock_path,
            testsets_lock=locked,
            production=production,
        )
    _print({"pinned": {"role": role, **entry.to_obj()}})
    return 0


def _inside(path: str, base_dir: Path) -> Path:
    """``path``, which must be inside ``base_dir`` (main's checkout), never elsewhere."""
    resolved = Path(path).resolve()
    if not resolved.is_relative_to(base_dir):
        raise Refused("not_allowed_here", detail_code="outside_base_dir")
    return resolved


def _lock_check(args: argparse.Namespace) -> int:
    number = lock.check_pr_number(args.pr)
    if not isinstance(args.head_sha, str) or re.fullmatch(r"[0-9a-f]{40}", args.head_sha) is None:
        raise Refused("invalid_name", detail_code="head_sha")
    base_dir = Path(args.base_dir).resolve()
    if not base_dir.is_dir():
        raise Refused("not_allowed_here", detail_code="base_dir")
    testsets_path = _inside(args.testsets, base_dir)
    thresholds_dir = _inside(args.thresholds_dir, base_dir)
    policy_path = base_dir / BASE_TRUST_POLICY_PATH
    _check_policy_env(policy_path)
    rehearsal = bool(args.rehearsal)
    lock.check_rehearsal(rehearsal)
    changed = _pr_changed_files(base_dir, args.head_sha)
    lock.check_lock_change_alone(changed)
    base, head = lock.read(base_dir / BASE_LOCK_PATH), lock.read(args.head)
    reviewers = lock.read_reviewers(base_dir / BASE_REVIEWERS_PATH)
    locked = testsets.read_lock(testsets_path)
    for role, entry in head.models.items():
        lock.check_production(role, entry, rehearsal=rehearsal)
    if not lock.changed_roles(base, head):
        _print({"lock_check": "pass", "roles": {}})
        return 0
    source = _reader_store()
    policy = _trust_policy(policy_path)
    github = _github()
    with _cache() as cache_dir:
        roles = lock.lock_check(
            lock.to_bytes(base),
            lock.to_bytes(head),
            number,
            github=github,
            reviewers=reviewers,
            source=source,
            policy=policy,
            cache_dir=cache_dir,
            testsets_lock=locked,
            thresholds_dir=thresholds_dir,
            head_sha=args.head_sha,
            changed_paths=changed,
            rehearsal=rehearsal,
        )
    _print({"lock_check": "pass", "roles": roles})
    return 0


def _signoff_store() -> signoff.SignoffStore:
    """The store as the ``signoff`` role (``not_allowed_here`` without its credentials)."""
    target = _s3(SIGNOFF_ACCESS_ENV, SIGNOFF_SECRET_ENV)
    if target is None:
        raise Refused("not_allowed_here", detail_code="signoff_credentials")
    return target


def _signoff_signer() -> signing.Signer:
    # TODO(Task 26): the production sign-off signer (a key separate from the promotion
    # one). Phase 0 has only the development FileSigner, which refuses to load or sign
    # outside MHAI_MODELS_ENV=development.
    return _signer()


MAX_BASE_FILE_BYTES = 16 * 1024 * 1024


def _git(path: Path, *args: str) -> bytes | None:
    """``git -C path args`` (no shell, no stdin), its stdout, or None on any failure or
    more than :data:`MAX_BASE_FILE_BYTES`."""
    try:
        done = subprocess.run(
            ["git", "-C", str(path), *args],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            check=False,
            timeout=60,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if done.returncode != 0 or len(done.stdout) > MAX_BASE_FILE_BYTES:
        return None
    return bytes(done.stdout)


def _checkout_head(path: Path) -> str | None:
    """The commit checked out at ``path`` (``git rev-parse``, no shell), or None."""
    raw = _git(path, "rev-parse", "--verify", "HEAD^{commit}")
    out = raw.decode("ascii", "replace").strip() if raw is not None else ""
    return out if re.fullmatch(r"[0-9a-f]{40}", out) else None


def _checkout_root(path: Path) -> Path | None:
    raw = _git(path, "rev-parse", "--show-toplevel")
    if raw is None:
        return None
    return Path(raw.decode("utf-8", "replace").strip()).resolve()


def _changed_files(repo: Path, base: str, head: str, *, from_merge_base: bool) -> list[str]:
    """The files changed between two commits in the repository at ``repo``, from git
    (D35), never from the PR: ``git diff --name-only -z --no-renames`` (a rename lists
    both paths), ``base...head`` (from their merge base to ``head``: what a PR changes)
    when ``from_merge_base``, else ``base head`` (what a merge commit changed). Paths are
    relative to the repository's top level. Any git failure, such as a commit that was
    not fetched, is ``not_allowed_here``/``changed_files_unknown``: fail closed."""
    for value in (base, head):
        if re.fullmatch(r"[0-9a-f]{40}", value) is None:
            raise Refused("invalid_name", detail_code="commit")
    spec = [f"{base}...{head}"] if from_merge_base else [base, head]
    raw = _git(repo, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", *spec, "--")
    if raw is None:
        raise Refused("not_allowed_here", detail_code="changed_files_unknown")
    return [p.decode("utf-8", "surrogateescape") for p in raw.split(b"\0") if p]


def _pr_changed_files(base_dir: Path, head_sha: str) -> list[str]:
    """D35, lock-check: what the PR changes, ``git diff <main tip>...<head sha>`` in main's
    checkout (``HEAD`` is main's tip; the head commit must have been fetched)."""
    main_tip = _checkout_head(base_dir)
    if main_tip is None:
        raise Refused("not_allowed_here", detail_code="changed_files_unknown")
    return _changed_files(base_dir, main_tip, head_sha, from_merge_base=True)


def _merge_changed_files(base_dir: Path, base_sha: str, commit: str) -> list[str]:
    """D35, sign-off: what the merge changed on main, ``git diff <base sha> <commit>`` in
    the base checkout's repository (the merge commit must have been fetched)."""
    return _changed_files(base_dir, base_sha, commit, from_merge_base=False)


@contextlib.contextmanager
def _base_snapshot(base_dir: Path, base_sha: str, rels: Sequence[str]) -> Iterator[Path]:
    """The base data files at ``base_sha``, read with ``git show <sha>:<path>`` from the
    repository at ``base_dir`` (never its working tree, which may be dirty), written into
    a private temporary folder with the same layout, removed afterwards. A file missing at
    that commit is left out, so its reader refuses as for any missing file."""
    tmp = Path(tempfile.mkdtemp(prefix="mhai-models-base-", dir=os.environ.get(SCRATCH_ENV)))
    try:
        for rel in rels:
            if Path(rel).is_absolute() or ".." in Path(rel).parts:
                raise Refused("not_allowed_here", detail_code="outside_base_dir")
            raw = _git(base_dir, "show", f"{base_sha}:{rel}")
            if raw is None:
                continue
            out = tmp / rel
            out.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            out.write_bytes(raw)
        yield tmp
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def _signoff(args: argparse.Namespace) -> int:
    commit, head_sha, base_sha = args.commit, args.head_sha, args.base_sha
    for value, detail in ((commit, "commit"), (head_sha, "head_sha"), (base_sha, "base_sha")):
        if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{40}", value) is None:
            raise Refused("invalid_name", detail_code=detail)
    lock.check_rehearsal(bool(args.rehearsal))
    base_dir = Path(args.base_dir).resolve()
    if not base_dir.is_dir():
        raise Refused("not_allowed_here", detail_code="base_dir")
    # D33: the data that judges the merge is main's before the push, at --base-sha, read
    # from git at that commit (N1): a dirty working tree changes nothing.
    if _checkout_root(base_dir) != base_dir:
        raise Refused("not_allowed_here", detail_code="base_dir_not_toplevel")
    if _checkout_head(base_dir) != base_sha:
        raise Refused("not_allowed_here", detail_code="base_dir_not_base_sha")
    changed = _merge_changed_files(base_dir, base_sha, commit)
    lock.check_lock_change_alone(changed)
    testsets_rel = _inside(args.testsets, base_dir).relative_to(base_dir).as_posix()
    thresholds_rel = _inside(args.thresholds_dir, base_dir).relative_to(base_dir).as_posix()
    _check_policy_env(base_dir / BASE_TRUST_POLICY_PATH)
    before, after = lock.read(args.before), lock.read(args.after)
    head = lock.read(args.head_lock)
    rels = [BASE_LOCK_PATH, BASE_REVIEWERS_PATH, BASE_TRUST_POLICY_PATH, testsets_rel]
    rels += [f"{thresholds_rel}/{gate}.json" for gate in sorted(GATES)]
    with _base_snapshot(base_dir, base_sha, rels) as snap:
        return _signoff_from(args, snap, before, after, head, testsets_rel, thresholds_rel, changed)


def _signoff_from(
    args: argparse.Namespace,
    snap: Path,
    before: lock.Lock,
    after: lock.Lock,
    head: lock.Lock,
    testsets_rel: str,
    thresholds_rel: str,
    changed: Sequence[str],
) -> int:
    """The sign-off over the base data in ``snap`` (see :func:`_base_snapshot`)."""
    commit, head_sha, base_sha = args.commit, args.head_sha, args.base_sha
    if lock.read(snap / BASE_LOCK_PATH) != before:
        raise Refused("lock_invalid", detail_code="before_mismatch")
    reviewers = lock.read_reviewers(snap / BASE_REVIEWERS_PATH)
    locked = testsets.read_lock(snap / testsets_rel)
    thresholds_dir = snap / thresholds_rel
    policy_path = snap / BASE_TRUST_POLICY_PATH
    days = _retention_days(APPROVED_RETENTION_ENV)
    rehearsal = bool(args.rehearsal)
    for role, entry in after.models.items():
        lock.check_production(role, entry, rehearsal=rehearsal)
    if not lock.changed_roles(before, after):
        _print(signoff.Result({}, None, 0).to_obj())
        return 0
    target = _signoff_store()
    policy = _trust_policy(policy_path)
    signer = _signoff_signer()
    github = _github()
    with _cache() as cache_dir:
        result = signoff.sign_off(
            lock.to_bytes(before),
            lock.to_bytes(after),
            commit,
            head_raw=lock.to_bytes(head),
            head_sha=head_sha,
            base_sha=base_sha,
            github=github,
            reviewers=reviewers,
            target=target,
            policy=policy,
            signer=signer,
            cache_dir=cache_dir,
            testsets_lock=locked,
            thresholds_dir=thresholds_dir,
            retention_days=days,
            changed_paths=changed,
            rehearsal=rehearsal,
        )
    _print(result.to_obj())
    return 0


def _rollback(args: argparse.Namespace) -> int:
    names.check_role(args.role)  # names first, before any environment, file or store access
    if args.to is not None:
        names.check_version(args.to)
    lock.check_rehearsal(bool(args.rehearsal))
    lock_path = Path(args.lock) if args.lock else PRODUCTION_LOCK
    source = _reader_store()
    policy = _trust_policy()
    with _cache() as cache_dir:
        done = lock.rollback(
            args.role,
            source=source,
            policy=policy,
            cache_dir=cache_dir,
            lock_path=lock_path,
            to=args.to,
            rehearsal=bool(args.rehearsal),
        )
    out = {"role": done.role, "from": done.previous.to_obj(), "to": done.entry.to_obj()}
    _print({"rollback": out})
    return 0


def _base_verifier(stack: contextlib.ExitStack) -> Callable[[str], None]:
    """The promoted-base verifier, built on its first call only (a run whose base is
    upstream never reads the reader's credentials or the trust policy for it). A dry run
    with a promoted base verifies it too: a base is checked before anything is evaluated."""
    built: list[Callable[[str], None]] = []

    def verify_base(pin: str) -> None:
        if not built:
            if not (os.environ.get(READER_ACCESS_ENV) and os.environ.get(READER_SECRET_ENV)):
                raise Refused("base_unverified", detail_code="reader_credentials")
            if not os.environ.get(TRUST_POLICY_ENV):
                raise Refused("base_unverified", detail_code="policy")
            source, policy = _reader_store(), _trust_policy()
            built.append(lock.base_verifier(source, policy, stack.enter_context(_cache())))
        built[0](pin)

    return verify_base


def _promote(args: argparse.Namespace) -> int:
    req = promote.Request(
        name=args.name,
        version=args.version,
        source=args.source,
        run=args.run,
        hf_repo=args.hf_repo,
        hf_revision=args.hf_revision,
        drive_folder_id=os.environ.get(DRIVE_FOLDER_ENV) if args.source == "drive" else None,
        dry_run=bool(args.dry_run),
        expect_files=args.expect_files,
        runtime_url=args.runtime_url,
        gates=tuple(args.gate or ()),
    )
    promote.check_request(req)  # names first, before any environment, file or network
    report_dir = _check_report_dir(args.report_dir, req.dry_run)
    if args.dev_set and not req.dry_run and not verify.is_development():
        # A promotion is measured on the locked split, never on a set the caller picks.
        raise Refused("not_allowed_here", detail_code="dev_set_promotion")
    registry, licences = _registries()
    with contextlib.ExitStack() as stack:
        ctx = promote.Context(
            registry=registry,
            licences=licences,
            source_for=_source_for(req),
            split=_split(args.dev_set),
            scratch_parent=Path(os.environ[SCRATCH_ENV]) if os.environ.get(SCRATCH_ENV) else None,
            base_verifier=_base_verifier(stack),
        )
        return _run_promote(req, ctx, report_dir)


REPORT_FILE = "evaluation.json"


def _check_report_dir(given: str | None, dry_run: bool) -> Path | None:
    """``--report-dir``: a dry run only, an existing real folder (not a symlink) with no
    ``evaluation.json`` in it yet. Refused (``not_allowed_here``/``report_dir``) before
    anything runs."""
    if given is None:
        return None
    path = Path(given)
    ok = (
        dry_run
        and path.is_dir()
        and not path.is_symlink()
        and not os.path.lexists(path / REPORT_FILE)
    )
    if not ok:
        raise Refused("not_allowed_here", detail_code="report_dir")
    return path


def _write_report(folder: Path, raw: bytes) -> None:
    """The dry run's evaluation.json: created new (never overwritten, never through a
    symlink), mode 0600."""
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
    fd = os.open(folder / REPORT_FILE, flags, 0o600)
    with os.fdopen(fd, "wb") as fh:
        fh.write(raw)


def _run_promote(req: promote.Request, ctx: promote.Context, report_dir: Path | None = None) -> int:
    if not req.dry_run:
        # A dry run never reads store write credentials or the signer.
        signer = _signer() if os.environ.get(SIGNER_ENV) else None
        policy_path = os.environ.get(TRUST_POLICY_ENV)
        ctx = dataclasses.replace(
            ctx,
            store=_s3(PROMOTER_ACCESS_ENV, PROMOTER_SECRET_ENV),
            signer=signer,
            policy=signing.load_policy(policy_path) if policy_path else None,
            workflow=_workflow(),
            retention_days=(
                _retention_days(CANDIDATE_RETENTION_ENV)
                if os.environ.get(CANDIDATE_RETENTION_ENV)
                else None
            ),
        )
    summary = promote.promote(req, ctx)
    if report_dir is not None:
        _write_report(report_dir, summary.documents["evaluation.json"])
    _print(summary.to_obj())
    return 0


def _check(args: argparse.Namespace) -> int:
    registry, licences = _registries()
    checked = promote.check_folder(Path(args.dir), registry, licences)
    tm = checked.training.manifest
    _print(
        {
            "files_sha256": checked.files_sha256,
            "files": len(checked.files),
            "name": tm.name,
            "role": tm.role,
        }
    )
    return 0


def _make_fixture(args: argparse.Namespace) -> int:
    from intelligence.modelstore import fixtures

    name = fixtures.MODEL_NAME if args.name is None else names.check_name(args.name)
    if args.variant not in fixtures.EXPECTED_REASONS:
        raise Refused("fixture_name")
    if re.fullmatch(r"[0-9]{1,10}", args.seed, re.ASCII) is None:
        raise Refused("fixture_name", detail_code="seed")
    fixtures.make_run(Path(args.dir), args.variant, int(args.seed), name=name)
    _print({"fixture": args.variant})
    return 0


def _store_init(args: argparse.Namespace) -> int:
    """Rehearsal only: the init script against a loopback MinIO, in development."""
    if not signing.is_development():
        raise Refused("not_allowed_here", detail_code="development_only")
    url = os.environ.get(MINIO_URL_ENV, "")
    failed = False
    try:
        parts = urlsplit(url)
        host = parts.hostname
        _ = parts.port
    except ValueError:
        failed = True
    if (
        failed
        or parts.scheme not in ("http", "https")
        or host not in STORE_INIT_HOSTS
        or parts.username is not None
        or parts.path not in ("", "/")
        or parts.query
        or parts.fragment
    ):
        raise Refused("not_allowed_here", detail_code="loopback_only")
    if not INIT_SCRIPT.is_file():
        raise Refused("not_found", detail_code="init_script")
    if shutil.which("mc") is None:
        raise Refused("not_allowed_here", detail_code="mc_missing")
    # A fixed script, no shell; it reads the root credentials from this environment.
    done = subprocess.run(
        ["sh", str(INIT_SCRIPT)],
        env=dict(os.environ),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        check=False,
        timeout=600,
    )
    if done.returncode != 0:
        raise Refused("store_unavailable", detail_code="init")
    _print({"store_init": "done"})
    return 0


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mhai-models")
    sub = parser.add_subparsers(dest="command", required=True)
    keygen = sub.add_parser("dev-keygen", help="make a development signing key pair")
    keygen.add_argument("out_dir")
    keygen.set_defaults(func=_dev_keygen)
    build = sub.add_parser("testset-build", help="build, sign and lock the test split")
    build.add_argument("--suite", required=True)
    build.add_argument("--version", required=True)
    build.add_argument("--scale", default="1.0")
    build.set_defaults(func=_testset_build)
    pr = sub.add_parser("promote", help="dry-run or promote a model run")
    pr.add_argument("name")
    pr.add_argument("version")
    pr.add_argument("--source", required=True)
    pr.add_argument("--run", required=True)
    pr.add_argument("--hf-repo", default=None)
    pr.add_argument("--hf-revision", default=None)
    pr.add_argument("--dry-run", action="store_true")
    pr.add_argument("--expect-files", default=None)
    pr.add_argument("--runtime-url", default=None)
    pr.add_argument("--gate", action="append", default=None)
    pr.add_argument("--dev-set", default=None)
    pr.add_argument("--report-dir", default=None)
    pr.set_defaults(func=_promote)
    ck = sub.add_parser("check", help="file and training manifest checks on a run folder")
    ck.add_argument("dir")
    ck.set_defaults(func=_check)
    mf = sub.add_parser("make-fixture", help="write a tiny synthetic run")
    mf.add_argument("dir")
    mf.add_argument("--variant", default="good")
    mf.add_argument("--seed", default="0")
    mf.add_argument("--name", default=None)
    mf.set_defaults(func=_make_fixture)
    si = sub.add_parser("store-init", help="provision a loopback MinIO (rehearsal only)")
    si.set_defaults(func=_store_init)
    pn = sub.add_parser("pin", help="verify a promoted version and pin it in models.lock")
    pn.add_argument("role")
    pn.add_argument("name")
    pn.add_argument("version")
    pn.add_argument("--gate", required=True)
    pn.add_argument("--lock", default=None)
    pn.add_argument("--rehearsal", action="store_true")
    pn.set_defaults(func=_pin)
    lc = sub.add_parser("lock-check", help="check a PR's change to models.lock")
    lc.add_argument("--base-dir", required=True)
    lc.add_argument("--head", required=True)
    lc.add_argument("--head-sha", required=True)
    lc.add_argument("--pr", required=True)
    lc.add_argument("--testsets", required=True)
    lc.add_argument("--thresholds-dir", required=True)
    lc.add_argument("--rehearsal", action="store_true")
    lc.set_defaults(func=_lock_check)
    so = sub.add_parser("signoff", help="sign off the versions a merge to main pinned")
    so.add_argument("--base-dir", required=True)
    so.add_argument("--before", required=True)
    so.add_argument("--after", required=True)
    so.add_argument("--commit", required=True)
    so.add_argument("--base-sha", required=True)
    so.add_argument("--head-sha", required=True)
    so.add_argument("--head-lock", required=True)
    so.add_argument("--testsets", required=True)
    so.add_argument("--thresholds-dir", required=True)
    so.add_argument("--rehearsal", action="store_true")
    so.set_defaults(func=_signoff)
    rb = sub.add_parser("rollback", help="pin a role back to an older signed-off version")
    rb.add_argument("role")
    rb.add_argument("--to", default=None)
    rb.add_argument("--lock", default=None)
    rb.add_argument("--rehearsal", action="store_true")
    rb.set_defaults(func=_rollback)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    """Run one command. Every failure is a fixed code on stdout and an exit code (2 refused,
    3 unavailable, unreadable or unexpected); no traceback or exception text is printed,
    except with ``MHAI_MODELS_DEBUG=1`` in development, where an unexpected error's
    traceback goes to stderr."""
    args = _parser().parse_args(argv)
    detailed = args.command in _DETAILED
    refusal: Refused | None = None
    error: str | None = None
    try:
        result: int = args.func(args)
        return result
    except Refused as exc:
        refusal = exc
    except OSError:
        error = "io"
    except Exception:
        error = "internal"
        if os.environ.get(DEBUG_ENV) == "1" and signing.is_development():
            import traceback

            traceback.print_exc(file=sys.stderr)
    if refusal is not None:
        out: dict[str, Any] = {"refused": refusal.reason}
        if detailed and refusal.detail_code is not None:
            out["detail"] = refusal.detail_code
        _print(out)
        return 3 if detailed and refusal.reason in UNAVAILABLE else 2
    _print({"error": error or "internal"})
    return 3


if __name__ == "__main__":
    sys.exit(main())
