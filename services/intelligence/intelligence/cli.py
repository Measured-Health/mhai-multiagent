"""Command-line interface for MHAI."""
import argparse
import sys
from intelligence.config.settings import get_config
from intelligence.utils.logging import setup_logging

def _build_parser():
    parser = argparse.ArgumentParser(prog="intelligence",
        description="5-agent hallucination-resistant clinical AI system.")
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="Process a clinical query.")
    run.add_argument("--query", type=str, required=True, help="Clinical question.")
    run.add_argument("--urgency", choices=["routine", "urgent", "emergency"], default="routine")
    run.add_argument("--log-level", default=None)

    worker = sub.add_parser("worker", help="Serve one service over NATS (spec section 4).")
    worker.add_argument("--service", choices=["pipeline", "echo"], required=True)
    worker.add_argument("--log-level", default=None)
    return parser


def _worker(args):
    """intelligence worker --service pipeline|echo: refuses to start on any
    bad setting (the error names variables, never values), then serves until
    SIGTERM and exits 0 once drained."""
    import asyncio
    import logging

    from intelligence.boundary import worker
    from intelligence.boundary.config import ConfigError, WorkerSettings
    from intelligence.boundary.db import DatabaseError
    from intelligence.boundary.keys import TrustedKeysError
    from intelligence.boundary.operations import OperationsError

    setup_logging(args.log_level)
    try:
        settings = WorkerSettings.from_env(service=args.service)
    except ConfigError as e:
        print(f"intelligence worker: {e}", file=sys.stderr)
        return 2
    try:
        asyncio.run(worker.run_service(settings))
    except (TrustedKeysError, OperationsError, DatabaseError) as e:
        # These messages name files, variables and roles, never credentials.
        print(f"intelligence worker: {e}", file=sys.stderr)
        return 2
    except Exception as e:
        # The type only: a connection error can carry the URL.
        logging.getLogger("intelligence.worker").error("worker stopped: %s", type(e).__name__)
        return 1
    return 0

def main(argv=None):
    args = _build_parser().parse_args(argv if argv is not None else sys.argv[1:])
    if args.command == "run":
        setup_logging(args.log_level)
        config = get_config()
        print(f"MHAI Clinical Query Pipeline")
        print(f"  Query: {args.query}")
        print(f"  Urgency: {args.urgency}")
        print(f"  NLI Model: {config.NLI_MODEL}")
        print(f"  CCA Model: {config.CCA_MODEL}")
        return 0
    if args.command == "worker":
        return _worker(args)
    return 1

if __name__ == "__main__":
    raise SystemExit(main())
