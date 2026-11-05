"""``mhai-models``: the model store's command line.

Commands::

    mhai-models dev-keygen OUT_DIR

TODO(Task 19/20/21): check, promote, verify, pin, lock-check, signoff, rollback,
drive-check, make-fixture and store-init.

``dev-keygen`` makes a development Ed25519 key pair in ``OUT_DIR`` (the private key with
mode 0600) and prints one JSON line with the key id and the public key PEM, never the
private key. It refuses outside ``MHAI_MODELS_ENV=development`` (``not_allowed_here``)
and never overwrites a key (``key_exists``).

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout), 3 a file
could not be read or written.
"""

import argparse
import sys
from collections.abc import Sequence

from intelligence.modelstore import canonical, signing
from intelligence.modelstore.reasons import Refused


def _print(obj: dict[str, str]) -> None:
    sys.stdout.write(canonical.dumps(obj).decode("ascii") + "\n")


def _dev_keygen(args: argparse.Namespace) -> int:
    kid, pem = signing.generate_dev_key(args.out_dir)
    _print({"key_id": kid, "public_key_pem": pem})
    return 0


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mhai-models")
    sub = parser.add_subparsers(dest="command", required=True)
    keygen = sub.add_parser("dev-keygen", help="make a development signing key pair")
    keygen.add_argument("out_dir")
    keygen.set_defaults(func=_dev_keygen)
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
