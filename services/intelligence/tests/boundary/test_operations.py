"""The operations loader (spec §8.1): strict, fail closed at start-up, and
test-only operations only in development. Python writes no permission rule;
these tests check it reads operations.json exactly."""

from __future__ import annotations

import copy
import json
from importlib import resources
from pathlib import Path
from typing import Any

import pytest

from intelligence.boundary import config, envelope
from intelligence.boundary.operations import Operations, OperationsError

PACKAGED: list[dict[str, Any]] = json.loads(
    (resources.files("intelligence.boundary") / "operations.json").read_text()
)
CATALOGUE: dict[str, Any] = json.loads(
    (resources.files("intelligence.boundary") / "catalogue.json").read_text()
)


def write(tmp_path: Path, rows: object) -> Path:
    p = tmp_path / "operations.json"
    p.write_text(rows if isinstance(rows, str) else json.dumps(rows))
    return p


def row(name: str = "echo.ping") -> dict[str, Any]:
    return copy.deepcopy(next(r for r in PACKAGED if r["name"] == name))


def test_loads_packaged_table() -> None:
    ops = Operations.load(development=True)
    assert {o.name for o in ops} == {r["name"] for r in PACKAGED}
    ping = ops.for_subject("inference.v1.echo.ping")
    assert ping is not None and ping.any_role == ("clinician",) and ping.bff_direct
    job = ops.for_subject("jobs.v1.22222222-2222-4222-8222-222222222222.run_query")
    assert job is not None and job.name == "pipeline.run_query" and job.kind == "job"
    assert ops.for_subject("jobs.v1.global.run_query") is job
    for s in (
        "inference.v1.echo",
        "inference.v1.echo.ping.x",
        "inference.v2.echo.ping",
        "inference.v1.echo.run_query",
        "jobs.v1.x.ping",
        "jobs.v1.global.retention_purge",
        "",
    ):
        assert ops.for_subject(s) is None, s


def test_test_only_refused_unless_development(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("APP_ENV", raising=False)
    assert config.is_development() is False  # unset counts as production
    assert Operations.load().for_subject("inference.v1.echo.ping") is None
    for env in ("production", "local", "dev", ""):
        monkeypatch.setenv("APP_ENV", env)
        assert Operations.load().for_subject("inference.v1.echo.ping") is None, env
    monkeypatch.setenv("APP_ENV", " Development ")
    assert Operations.load().for_subject("inference.v1.echo.ping") is not None
    assert Operations.load(development=False).for_subject("inference.v1.echo.ping") is None


def test_operations_missing_handler_refuses_start() -> None:
    ops = Operations.load(development=True)
    ops.require("pipeline", ["health", "run_query"])
    with pytest.raises(OperationsError):
        ops.require("pipeline", ["health", "nope"])
    with pytest.raises(OperationsError):
        ops.require("pipeline", ["ping"])  # echo's, not pipeline's
    with pytest.raises(OperationsError):
        Operations.load(development=False).require("echo", ["ping"])  # test-only in production


def _mutations() -> list[tuple[str, Any]]:
    def set_(field: str, value: Any) -> Any:
        r = row()
        r[field] = value
        return [r]

    def drop(field: str) -> Any:
        r = row()
        del r[field]
        return [r]

    job_bff = row("pipeline.run_query")
    job_bff["bff_direct"] = True
    return [
        ("not a list", {"echo.ping": row()}),
        ("empty", []),
        ("duplicate name", [row(), row()]),
        ("unknown field", set_("allow_all", True)),
        ("missing field", drop("svc_core")),
        ("missing any_role", drop("any_role")),
        ("unknown role", set_("any_role", ["clinician", "superuser"])),
        ("platform role", set_("any_role", ["platform_admin"])),
        ("role not a string", set_("any_role", [1])),
        ("bool as string", set_("svc_core", "true")),
        ("int as bool", set_("needs_hospital", 1)),
        ("bool as int", set_("timeout_ms", True)),
        ("float timeout", set_("timeout_ms", 5000.0)),
        ("zero timeout", set_("timeout_ms", 0)),
        ("req timeout over 60 s", set_("timeout_ms", 60001)),
        ("bad kind", set_("kind", "stream")),
        ("name not service.op", set_("name", "echo.pong")),
        ("bad service", set_("service", "Echo")),
        ("null", set_("request_schema", None)),
        ("empty schema", set_("request_schema", "")),
        ("bff_direct on a job", [job_bff]),
        ("bff_direct without a role", set_("any_role", [])),
    ]


@pytest.mark.parametrize("label,rows", _mutations(), ids=[m[0] for m in _mutations()])
def test_strict_validation(tmp_path: Path, label: str, rows: Any) -> None:
    with pytest.raises(OperationsError):
        Operations.load(write(tmp_path, rows), development=True)


@pytest.mark.parametrize(
    "raw",
    [
        '[{"name":"echo.ping","name":"echo.ping"}]',
        "[" + json.dumps(row()) + "] trailing",
        "[" + json.dumps(row()).replace('"svc_core"', '"SVC_CORE"') + "]",
        "[" + json.dumps(row()).replace("5000", "NaN") + "]",
        "",
    ],
)
def test_strict_json(tmp_path: Path, raw: str) -> None:
    with pytest.raises(OperationsError):
        Operations.load(write(tmp_path, raw), development=True)


def test_job_op_names_unique(tmp_path: Path) -> None:
    a = row("pipeline.run_query")
    b = copy.deepcopy(a)
    b["name"], b["service"] = "other.run_query", "other"
    with pytest.raises(OperationsError):
        Operations.load(write(tmp_path, [a, b]), development=True)


def test_error_codes_equal_core() -> None:
    """envelope.CODES is core's authz.ErrorCodes(), via the generated catalogue (T4b)."""
    assert set(CATALOGUE) == {"error_codes", "clinical_roles"}
    assert envelope.CODES == frozenset(CATALOGUE["error_codes"])
    assert len(CATALOGUE["error_codes"]) == len(envelope.CODES)


def test_catalogue_is_package_data() -> None:
    import tomllib

    pyproject = Path(__file__).resolve().parents[2] / "pyproject.toml"
    data = tomllib.loads(pyproject.read_text())
    assert "boundary/catalogue.json" in data["tool"]["setuptools"]["package-data"]["intelligence"]


def test_grant_refused_only_for_bff_direct() -> None:
    headers, _ = envelope.reply_error("grant_refused:mfa_required", False, bff_direct=True)
    assert headers[envelope.STATUS] == "grant_refused:mfa_required"
    with pytest.raises(ValueError):
        envelope.reply_error("grant_refused:mfa_required", False)
    for bad in ("grant_refused:", "grant_refused:Bad Code", "grant_refused:a\nb", "teapot"):
        with pytest.raises(ValueError):
            envelope.reply_error(bad, False, bff_direct=True)


def test_reply_helpers() -> None:
    from intelligence.boundary.pb import echo_pb2, envelope_pb2

    meta = envelope_pb2.ResponseMeta(model_versions={"echo": "1"})
    headers, body = envelope.reply_ok(
        meta, echo_pb2.PingResponse(text="synthetic"), request_id="r1"
    )
    assert headers == {
        envelope.STATUS: "ok",
        envelope.SCHEMA: "mhai.intelligence.v1.PingResponse",
        envelope.REQUEST_ID: "r1",
    }
    got = echo_pb2.PingResponse.FromString(body)
    assert got.meta.model_versions["echo"] == "1" and got.text == "synthetic"
    headers, body = envelope.reply_error("unauthorized", False)
    assert headers == {
        envelope.STATUS: "unauthorized",
        envelope.SCHEMA: "mhai.intelligence.v1.Error",
    }
    err = envelope_pb2.Error.FromString(body)
    assert (err.code, err.retryable) == ("unauthorized", False)
    with pytest.raises(ValueError):
        envelope.reply_error("internal_error", True)
