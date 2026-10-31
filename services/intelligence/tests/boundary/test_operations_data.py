"""operations.json ships inside the package (F13): it must load from the
installed package, not from the source tree."""

import json
import tomllib
from importlib import resources
from pathlib import Path

REQUIRED = {
    "name",
    "service",
    "op",
    "kind",
    "any_role",
    "svc_core",
    "needs_hospital",
    "bff_direct",
    "timeout_ms",
    "request_schema",
    "response_schema",
    "test_only",
}


def test_operations_json_is_package_data() -> None:
    raw = (resources.files("intelligence.boundary") / "operations.json").read_text()
    rows = json.loads(raw)
    names = {r["name"] for r in rows}
    assert {"pipeline.run_query", "pipeline.health", "echo.ping"} <= names
    for r in rows:
        assert set(r) == REQUIRED
    assert [r["name"] for r in rows if r["bff_direct"]] == ["echo.ping"]


def test_pyproject_lists_operations_json_as_package_data() -> None:
    pyproject = Path(__file__).resolve().parents[2] / "pyproject.toml"
    data = tomllib.loads(pyproject.read_text())
    assert "boundary/operations.json" in data["tool"]["setuptools"]["package-data"]["intelligence"]
