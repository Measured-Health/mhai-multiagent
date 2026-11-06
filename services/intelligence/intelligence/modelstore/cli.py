"""``mhai-models``: the model store's command line.

Commands::

    mhai-models dev-keygen OUT_DIR
    mhai-models testset-build --suite clinical_document --version 1.0.0 [--scale S]
    mhai-models promote NAME VERSION --source drive|local|hf --run RUN
        [--hf-repo R --hf-revision SHA] [--dry-run] [--expect-files SHA256]
        [--runtime-url URL] [--gate phase1|phase3|rehearsal ...] [--dev-set DIR]
    mhai-models check DIR
    mhai-models make-fixture DIR [--variant V] [--seed N]
    mhai-models store-init

TODO(Task 20/21/22): verify, pin, lock-check, signoff, rollback and drive-check.

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
credentials and writes nothing. A promotion reads ``MODEL_STORE_ENDPOINT``,
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

A promotion (not ``--dry-run``) outside ``MHAI_MODELS_ENV=development`` must run with
``MHAI_EVAL_ROLE=locked`` (the locked split), must pass ``--expect-files`` and may not pass
``--dev-set``; each is ``not_allowed_here`` otherwise.

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout; for
``promote``, ``check``, ``make-fixture`` and ``store-init`` with ``"detail"``, a fixed code,
when there is one), 3 a file could not be read or written, or (``promote``, ``check``,
``store-init``) the store or the source is unavailable, 4 (``testset-build`` only) the split
is locked and its lock line printed, but the scratch copy could not be removed
(``{"warning": "scratch_not_removed"}`` on stderr, no path): remove the scratch root by hand.
An unexpected error is ``{"error": "internal"}`` and exit 3, with no traceback (unless
``MHAI_MODELS_DEBUG=1`` in development, to stderr).
"""

import argparse
import dataclasses
import math
import os
import re
import shutil
import subprocess
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from intelligence.modelstore import (
    canonical,
    names,
    promote,
    signing,
    sources,
    store,
    testsets,
    training,
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
CANDIDATE_RETENTION_ENV = "MODEL_STORE_CANDIDATE_RETENTION_DAYS"
LOCAL_ROOT_ENV = "MHAI_MODELS_LOCAL_ROOT"
DRIVE_FOLDER_ENV = "MHAI_MODELS_DRIVE_FOLDER_ID"
DATASETS_ENV = "MHAI_MODELS_DATASETS"
LICENCES_ENV = "MHAI_MODELS_LICENCES"
TESTSETS_LOCK_ENV = "MHAI_MODELS_TESTSETS_LOCK"
MINIO_URL_ENV = "MINIO_URL"
WORKFLOW_ENVS = ("GITHUB_RUN_ID", "GITHUB_WORKFLOW_REF", "GITHUB_SHA", "GITHUB_ACTOR")
MAX_SCALE = 10.0
MODELS_DIR = promote.REPO / "models"
INIT_SCRIPT = promote.REPO / "deploy" / "minio" / "init-model-store.sh"
UNAVAILABLE = frozenset({"store_unavailable", "source_unavailable"})
# store-init: loopback IP literals only (a name such as localhost can resolve anywhere).
STORE_INIT_HOSTS = frozenset({"127.0.0.1", "::1"})
DEBUG_ENV = "MHAI_MODELS_DEBUG"
# Commands whose refusals carry the fixed detail code, and whose store or source being
# unavailable is exit 3.
_DETAILED = frozenset({"promote", "check", "make-fixture", "store-init"})


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
    return promote.WorkflowRun(run_id, ref, commit, actor)


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
    if args.dev_set and not req.dry_run and not signing.is_development():
        # A promotion is measured on the locked split, never on a set the caller picks.
        raise Refused("not_allowed_here", detail_code="dev_set_promotion")
    registry, licences = _registries()
    ctx = promote.Context(
        registry=registry,
        licences=licences,
        source_for=_source_for(req),
        split=_split(args.dev_set),
        scratch_parent=Path(os.environ[SCRATCH_ENV]) if os.environ.get(SCRATCH_ENV) else None,
    )
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

    if args.variant not in fixtures.EXPECTED_REASONS:
        raise Refused("fixture_name")
    if re.fullmatch(r"[0-9]{1,10}", args.seed, re.ASCII) is None:
        raise Refused("fixture_name", detail_code="seed")
    fixtures.make_run(Path(args.dir), args.variant, int(args.seed))
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
    pr.set_defaults(func=_promote)
    ck = sub.add_parser("check", help="file and training manifest checks on a run folder")
    ck.add_argument("dir")
    ck.set_defaults(func=_check)
    mf = sub.add_parser("make-fixture", help="write a tiny synthetic run")
    mf.add_argument("dir")
    mf.add_argument("--variant", default="good")
    mf.add_argument("--seed", default="0")
    mf.set_defaults(func=_make_fixture)
    si = sub.add_parser("store-init", help="provision a loopback MinIO (rehearsal only)")
    si.set_defaults(func=_store_init)
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
