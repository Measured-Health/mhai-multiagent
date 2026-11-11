"""``mhai-models``: the model store's command line.

Commands::

    mhai-models keygen OUT_DIR
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
    mhai-models lock-check --base MAIN_LOCK [--head LOCK] [--trust-policy FILE] [--rehearsal]
    mhai-models rollback ROLE [--to VERSION] [--lock PATH] [--rehearsal]

The store is any S3-compatible bucket: ``MODEL_STORE_ENDPOINT`` (TLS unless loopback),
``MODEL_STORE_BUCKET`` and ``MODEL_STORE_REGION`` (optional). Two credentials: the writer
(``MODEL_STORE_WRITER_ACCESS_KEY``, ``MODEL_STORE_WRITER_SECRET_KEY``; ``promote`` and
``testset-build``) and the read-only reader (``MODEL_STORE_READER_ACCESS_KEY``,
``MODEL_STORE_READER_SECRET_KEY``; the locked split, ``pin``, ``lock-check``,
``rollback``, a promoted base, and the AI service). The signer is ``MHAI_MODELS_SIGNER``,
an Ed25519 key file (mode 0600): the repository secret written to a file in the workflow,
or a key file for the CLI. The trust policy is ``MHAI_MODELS_TRUST_POLICY``, by default
``models/trust-policy.json``.

``keygen`` makes the signing key pair in ``OUT_DIR`` (``signing-key.pem``, mode 0600, and
``signing-key.pub.pem``) and prints the entry for ``models/trust-policy.json``, never the
private key. ``dev-keygen`` makes a development key (``dev-signing-key.pem``), only with
``MHAI_MODELS_ENV=development``, and prints its key id and public key. Neither overwrites
a key (``key_exists``).

``testset-build`` builds the locked test split inside the store
(:func:`intelligence.modelstore.testsets.build_and_lock`) as the writer and prints one
line, ``<suite>@<version>#<sha256>``, for ``models/testsets.lock``; nothing else of the
split is printed. Its names are checked before any I/O. ``MHAI_MODELS_SCRATCH`` (optional)
says where the 0700 scratch folder is made.

``promote`` runs :func:`intelligence.modelstore.promote.promote` and prints one JSON summary
(name, version, run, files hash, split, test set, verdicts, failed gates, and the manifest's
SHA-256 when promoted). Its names are checked before any I/O. A dry run reads no store
credentials and writes nothing to the store; with ``--report-dir DIR`` (a dry run only: an
existing folder, not a symlink, holding no ``evaluation.json``, checked before anything
runs) it also writes the run's ``evaluation.json`` there (mode 0600, never overwritten),
the aggregate-only report a promotion would upload, for the workflow's ``evaluation.md``
(``mhai-doceval report``). A promotion needs the writer, the signer, the trust policy and
the workflow run (``GITHUB_RUN_ID``, ``GITHUB_WORKFLOW_REF``, ``GITHUB_SHA``,
``GITHUB_ACTOR``: set by GitHub, or by hand for a run from the CLI); without any of them it
is ``not_allowed_here``. The source: ``local`` reads ``MHAI_MODELS_LOCAL_ROOT``; ``hf`` the
repo and commit given; ``drive`` the folder ``MHAI_MODELS_DRIVE_FOLDER_ID`` with the
access token in ``MHAI_MODELS_DRIVE_TOKEN_FILE`` (``drive_check`` runs first). The split:
with ``MHAI_EVAL_ROLE=locked``, the locked split named by ``models/testsets.lock``
(``MHAI_MODELS_TESTSETS_LOCK``), read as the reader; otherwise ``--dev-set``, or the dev
split built into scratch. The registries are ``models/approved-datasets.json`` and
``models/licences.json`` (``MHAI_MODELS_DATASETS``, ``MHAI_MODELS_LICENCES``).

``check DIR`` runs steps 3 and 4 (file and training manifest checks) on a local run folder
and prints its files hash. ``make-fixture`` writes a tiny synthetic run
(:func:`intelligence.modelstore.fixtures.make_run`). ``store-init`` is for rehearsal only: it
runs ``deploy/minio/init-model-store.sh`` (which needs ``mc``) and refuses unless
``MHAI_MODELS_ENV=development`` and ``MINIO_URL``'s host is ``127.0.0.1`` or ``::1``.

A run whose training manifest names a promoted base model has that base verified
(:func:`intelligence.modelstore.lock.base_verifier`), on a dry run as on a promotion: the
verifier is built on first use only, so a run with an upstream base reads no reader
credentials for it. Without them, or when the base fails, the run is ``base_unverified``
(the inner reason as the detail).

``pin`` (:func:`intelligence.modelstore.lock.pin`) verifies ``NAME@VERSION`` as the reader,
checks its evaluation against ``models/testsets.lock`` (``MHAI_MODELS_TESTSETS_LOCK``) and
writes ``models.lock`` (or ``--lock``). It prints ``{"pinned": {role, name, version,
manifest_sha256, gate}}``. The production rules (no ``fixture-`` name, no ``rehearsal``
gate, a known role) always apply unless ``--rehearsal`` is passed, which needs development
(both switches) and an explicit ``--lock`` (``not_allowed_here`` otherwise). It opens no PR.

``lock-check`` (:func:`intelligence.modelstore.lock.lock_check`, in ``model-lock.yml``)
compares ``--base`` (``main``'s ``models.lock``) with ``--head`` (the PR's, by default
``models.lock``) and verifies every changed role as the reader. ``--trust-policy`` (the
workflow passes ``main``'s copy when ``main`` has one) overrides ``MHAI_MODELS_TRUST_POLICY``.
It prints ``{"lock_check": "pass", "roles": {role: new|rollback|removed}}``; a lock with no
changed role needs no store access. Verified files go to ``MHAI_MODELS_CACHE`` when set,
else to a private temporary folder removed afterwards.

``rollback ROLE`` (:func:`intelligence.modelstore.lock.rollback`) pins ``ROLE`` to the latest
older version of its pinned name that verifies for this role and gate (or ``--to
VERSION``, which must be such a version: P16), reading the store as ``pin`` does, and
writes ``models.lock`` (or ``--lock``). It prints ``{"rollback": {"role", "from", "to"}}``.
The change goes in a normal PR.

``--rehearsal`` on ``pin``, ``lock-check`` and ``rollback`` allows a ``fixture-`` name and
the ``rehearsal`` gate (:func:`intelligence.modelstore.lock.check_production`), in
development only (both switches; ``not_allowed_here``/``rehearsal`` otherwise, before any
store access). The production workflows never pass it, and ``test_workflows.py`` checks it.

A promotion (not ``--dry-run``) outside ``MHAI_MODELS_ENV=development`` must run with
``MHAI_EVAL_ROLE=locked`` (the locked split), must pass ``--expect-files`` and may not pass
``--dev-set``; each is ``not_allowed_here`` otherwise.

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout; for
``promote``, ``check``, ``make-fixture``, ``store-init``, ``pin``, ``lock-check`` and
``rollback`` with ``"detail"``, a fixed code, when there is one), 3 a file could not be
read or written, or (those same commands) the store or the source is unavailable, 4
(``testset-build`` only) the split is locked and its lock line printed, but the scratch
copy could not be removed (``{"warning": "scratch_not_removed"}`` on stderr, no path):
remove the scratch root by hand. An unexpected error is ``{"error": "internal"}`` and exit
3, with no traceback (unless ``MHAI_MODELS_DEBUG=1`` in development, to stderr).
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

from intelligence.modelstore import (
    canonical,
    files,
    lock,
    names,
    promote,
    signing,
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
WRITER_ACCESS_ENV = "MODEL_STORE_WRITER_ACCESS_KEY"
WRITER_SECRET_ENV = "MODEL_STORE_WRITER_SECRET_KEY"
READER_ACCESS_ENV = "MODEL_STORE_READER_ACCESS_KEY"
READER_SECRET_ENV = "MODEL_STORE_READER_SECRET_KEY"
SIGNER_ENV = "MHAI_MODELS_SIGNER"
TRUST_POLICY_ENV = "MHAI_MODELS_TRUST_POLICY"
SCRATCH_ENV = "MHAI_MODELS_SCRATCH"
CACHE_ENV = "MHAI_MODELS_CACHE"
LOCAL_ROOT_ENV = "MHAI_MODELS_LOCAL_ROOT"
DRIVE_FOLDER_ENV = "MHAI_MODELS_DRIVE_FOLDER_ID"
DRIVE_TOKEN_FILE_ENV = "MHAI_MODELS_DRIVE_TOKEN_FILE"
DATASETS_ENV = "MHAI_MODELS_DATASETS"
LICENCES_ENV = "MHAI_MODELS_LICENCES"
TESTSETS_LOCK_ENV = "MHAI_MODELS_TESTSETS_LOCK"
MINIO_URL_ENV = "MINIO_URL"
WORKFLOW_ENVS = ("GITHUB_RUN_ID", "GITHUB_WORKFLOW_REF", "GITHUB_SHA", "GITHUB_ACTOR")
MAX_SCALE = 10.0
MODELS_DIR = promote.REPO / "models"
PRODUCTION_LOCK = promote.REPO / "models.lock"
TRUST_POLICY = MODELS_DIR / "trust-policy.json"
INIT_SCRIPT = promote.REPO / "deploy" / "minio" / "init-model-store.sh"
UNAVAILABLE = frozenset({"store_unavailable", "source_unavailable"})
# store-init: loopback IP literals only (a name such as localhost can resolve anywhere).
STORE_INIT_HOSTS = frozenset({"127.0.0.1", "::1"})
DEBUG_ENV = "MHAI_MODELS_DEBUG"
# Commands whose refusals carry the fixed detail code, and whose store or source being
# unavailable is exit 3.
_DETAILED = frozenset(
    {"promote", "check", "make-fixture", "store-init", "pin", "lock-check", "rollback"}
)


def _print(obj: dict[str, Any]) -> None:
    sys.stdout.write(canonical.dumps(obj).decode("ascii") + "\n")


def _keygen(args: argparse.Namespace) -> int:
    entry = signing.generate_key(args.out_dir, development=False)
    _print({"trust_policy_signer": entry.to_obj()})
    return 0


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


def _signer() -> signing.Signer:
    """The Ed25519 key file ``MHAI_MODELS_SIGNER`` (``not_allowed_here`` when unset)."""
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
        os.environ.get(WRITER_ACCESS_ENV, ""), os.environ.get(WRITER_SECRET_ENV, "")
    )
    target = store.S3Store(endpoint, bucket, creds, os.environ.get(REGION_ENV) or "us-east-1")
    signer = _signer()
    policy = _trust_policy()
    built = testsets.build_and_lock(
        target,
        signer,
        policy,
        suite=args.suite,
        version=version,
        scale=scale,
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
        reader = _s3(READER_ACCESS_ENV, READER_SECRET_ENV)
        entry = _lock_entry()
        policy = _trust_policy()
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
        return _drive_source(req)

    return make


def _reader_store() -> lock.ListStore:
    """The store with the reader's credentials (``not_allowed_here`` without them)."""
    reader = _s3(READER_ACCESS_ENV, READER_SECRET_ENV)
    if reader is None:
        raise Refused("not_allowed_here", detail_code="reader_credentials")
    return reader


def _trust_policy(path: str | None = None) -> signing.TrustPolicy:
    """``path``, else ``MHAI_MODELS_TRUST_POLICY``, else ``models/trust-policy.json``."""
    return signing.load_policy(path or os.environ.get(TRUST_POLICY_ENV) or TRUST_POLICY)


MAX_TOKEN_BYTES = 8 * 1024


class _BearerSession:
    """A ``requests`` session that sends one OAuth access token (read from a file, never
    from the command line) and follows no redirect, for :class:`HttpDriveApi`."""

    def __init__(self, token: str) -> None:
        import requests

        self._session = requests.Session()
        self._session.trust_env = False  # no proxy or netrc from the environment
        self._session.headers["Authorization"] = f"Bearer {token}"

    def get(self, url: str, *, params: Any, stream: bool, timeout: Any) -> Any:
        return self._session.get(
            url, params=params, stream=stream, timeout=timeout, allow_redirects=False
        )

    def __repr__(self) -> str:
        return "_BearerSession()"


def _drive_token() -> str:
    """The access token in ``MHAI_MODELS_DRIVE_TOKEN_FILE`` (a regular file, never through
    a link): printable ASCII with no spaces, else ``source_unavailable``/``drive_token``."""
    path = os.environ.get(DRIVE_TOKEN_FILE_ENV)
    failed = not path
    raw = b""
    if path:
        try:
            raw = files.read_regular(path, MAX_TOKEN_BYTES)
        except Refused:
            failed = True
    token = raw.decode("ascii", "replace").strip()
    if failed or re.fullmatch(r"[\x21-\x7e]{16,4096}", token, re.ASCII) is None:
        raise Refused("source_unavailable", detail_code="drive_token")
    return token


def _drive_source(req: promote.Request) -> sources.Source:
    """The run's Drive folder, read with the access token: ``drive_check`` first (the
    account sees only the shared folder, read-only), then the run."""
    from intelligence.modelstore.sources import drive

    folder = drive.check_id(req.drive_folder_id)
    api = drive.HttpDriveApi(_BearerSession(_drive_token()))
    drive.drive_check(api, folder)
    return drive.DriveSource(api, folder)


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


def _lock_check(args: argparse.Namespace) -> int:
    rehearsal = bool(args.rehearsal)
    lock.check_rehearsal(rehearsal)
    base = lock.read(args.base)
    head = lock.read(args.head or PRODUCTION_LOCK)
    for role, entry in head.models.items():
        lock.check_production(role, entry, rehearsal=rehearsal)
    if not lock.changed_roles(base, head):
        _print({"lock_check": "pass", "roles": {}})
        return 0
    source = _reader_store()
    policy = _trust_policy(args.trust_policy)
    locked = _testsets_lock()
    with _cache() as cache_dir:
        roles = lock.lock_check(
            lock.to_bytes(base),
            lock.to_bytes(head),
            source=source,
            policy=policy,
            cache_dir=cache_dir,
            testsets_lock=locked,
            rehearsal=rehearsal,
        )
    _print({"lock_check": "pass", "roles": roles})
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
        ctx = dataclasses.replace(
            ctx,
            store=_s3(WRITER_ACCESS_ENV, WRITER_SECRET_ENV),
            signer=signer,
            policy=_trust_policy(),
            workflow=_workflow(),
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
    keygen = sub.add_parser("keygen", help="make the signing key pair")
    keygen.add_argument("out_dir")
    keygen.set_defaults(func=_keygen)
    dev_keygen = sub.add_parser("dev-keygen", help="make a development signing key pair")
    dev_keygen.add_argument("out_dir")
    dev_keygen.set_defaults(func=_dev_keygen)
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
    lc.add_argument("--base", required=True)
    lc.add_argument("--head", default=None)
    lc.add_argument("--trust-policy", default=None)
    lc.add_argument("--rehearsal", action="store_true")
    lc.set_defaults(func=_lock_check)
    rb = sub.add_parser("rollback", help="pin a role back to an earlier verified version")
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
