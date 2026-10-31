"""Tests for CLI."""
from intelligence.cli import _build_parser


def test_run_subcommand():
    args = _build_parser().parse_args(["run", "--query", "test", "--urgency", "routine"])
    assert args.command == "run"
    assert args.query == "test"
    assert args.urgency == "routine"


def test_worker_subcommand():
    args = _build_parser().parse_args(["worker", "--service", "pipeline"])
    assert args.command == "worker"
    assert args.service == "pipeline"


def test_worker_subcommand_takes_only_known_services(capsys):
    import pytest

    with pytest.raises(SystemExit):
        _build_parser().parse_args(["worker", "--service", "nli"])


def test_cli_worker_refuses_echo_in_production(monkeypatch, capsys):
    """echo is test and smoke only: with APP_ENV unset (production) or
    production, `intelligence worker --service echo` exits non-zero before
    it connects to anything."""
    from intelligence import cli
    from intelligence.boundary import worker

    async def never(*_a, **_k):
        raise AssertionError("connected")

    monkeypatch.setattr(worker, "run_service", never)
    monkeypatch.setenv("NATS_URL", "nats://echo:test-only-pw@127.0.0.1:4222")
    monkeypatch.setenv("INTELLIGENCE_TRUSTED_KEYS_PATH", "/nonexistent")
    for env in (None, "production"):
        if env is None:
            monkeypatch.delenv("APP_ENV", raising=False)
        else:
            monkeypatch.setenv("APP_ENV", env)
        assert cli.main(["worker", "--service", "echo"]) == 2
        err = capsys.readouterr().err
        assert "APP_ENV" in err and "test-only-pw" not in err
