"""Command-line interface for MHAI."""
import argparse
import sys
from mhai.config.settings import get_config
from mhai.utils.logging import setup_logging

def _build_parser():
    parser = argparse.ArgumentParser(prog="mhai",
        description="5-agent hallucination-resistant clinical AI system.")
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="Process a clinical query.")
    run.add_argument("--query", type=str, required=True, help="Clinical question.")
    run.add_argument("--urgency", choices=["routine", "urgent", "emergency"], default="routine")
    run.add_argument("--log-level", default=None)
    return parser

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
    return 1

if __name__ == "__main__":
    raise SystemExit(main())
