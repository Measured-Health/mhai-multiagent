"""``mhai-models``: the model store's command line.

Commands::

    mhai-models dev-keygen OUT_DIR
    mhai-models testset-build --suite clinical_document --version 1.0.0 [--scale S]

TODO(Task 19/20/21): check, promote, verify, pin, lock-check, signoff, rollback,
drive-check, make-fixture and store-init.

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

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout), 3 a file
could not be read or written, 4 (``testset-build`` only) the split is locked and its lock
line printed, but the scratch copy could not be removed (``{"warning":
"scratch_not_removed"}`` on stderr, no path): remove the scratch root by hand.
"""

import argparse
import math
import os
import re
import sys
from collections.abc import Sequence

from intelligence.modelstore import canonical, names, signing, store, testsets
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
MAX_SCALE = 10.0


def _print(obj: dict[str, str]) -> None:
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


def _retention_days() -> int:
    raw = _env(TESTSET_RETENTION_ENV, "not_allowed_here")
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
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    code: str | None = None
    try:
        result: int = args.func(args)
        return result
    except Refused as exc:
        code = exc.reason
    except OSError:
        code = None
    if code is not None:
        _print({"refused": code})
        return 2
    _print({"error": "io"})
    return 3


if __name__ == "__main__":
    sys.exit(main())
