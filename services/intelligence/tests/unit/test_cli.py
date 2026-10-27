"""Tests for CLI."""
from intelligence.cli import _build_parser


def test_run_subcommand():
    args = _build_parser().parse_args(["run", "--query", "test", "--urgency", "routine"])
    assert args.command == "run"
    assert args.query == "test"
    assert args.urgency == "routine"
