"""Every abuse case in the cloud plan has its named test, and TODO.md keeps
the human gate before real patient data (plan Task 16).

The plan's "Abuse cases → tests" table is the source: fix a test's name to
match it, never the table. Python tests are found by `def <name>(` under
services/intelligence/tests, Go tests by `func <name>(` under
services/core, and files by name anywhere in the repository."""

from __future__ import annotations

import re
from functools import cache
from pathlib import Path

import pytest

pytestmark = pytest.mark.disable_socket

REPO = Path(__file__).resolve().parents[5]
PLAN = REPO / "docs" / "plans" / "2026-11-06-cloud-model-providers.md"
TODO = REPO / "TODO.md"
PY_TESTS = REPO / "services" / "intelligence" / "tests"
GO = REPO / "services" / "core"


def _table() -> dict[str, list[str]]:
    """Case id -> the names in its row's test column."""
    text = PLAN.read_text("utf-8")
    section = text.split("## Abuse cases → tests", 1)[1].split("\n## ", 1)[0]
    rows: dict[str, list[str]] = {}
    for line in section.splitlines():
        m = re.match(r"\| ([AK]\d+) [^|]*\| (.+) \|$", line)
        if m:
            rows[m.group(1)] = re.findall(r"`([^`]+)`", m.group(2))
    return rows


@cache
def _py_defs() -> frozenset[str]:
    names: set[str] = set()
    for f in PY_TESTS.rglob("*.py"):
        names.update(re.findall(r"^\s*(?:async )?def (test_\w+)\(", f.read_text("utf-8"), re.M))
    return frozenset(names)


@cache
def _go_funcs() -> frozenset[str]:
    names: set[str] = set()
    for f in GO.rglob("*_test.go"):
        names.update(re.findall(r"^func (Test\w+)\(", f.read_text("utf-8"), re.M))
    return frozenset(names)


@cache
def _files() -> frozenset[str]:
    skip = {".git", ".venv", "node_modules", "__pycache__"}
    out: set[str] = set()
    for f in REPO.rglob("*"):
        if f.is_file() and not skip & set(f.relative_to(REPO).parts):
            out.add(f.name)
    return frozenset(out)


def _exists(name: str) -> bool:
    base = name.split("[", 1)[0]
    if base.endswith((".py", ".sh")):
        return base in _files()
    if base.startswith("Test"):
        return base in _go_funcs()
    if base.startswith("test_"):
        return base in _py_defs()
    raise AssertionError(f"not a test name: {name}")


def test_table_lists_every_case() -> None:
    rows = _table()
    assert sorted(k for k in rows if k[0] == "A") == sorted(f"A{i}" for i in range(1, 19))
    assert sorted(k for k in rows if k[0] == "K") == sorted(f"K{i}" for i in range(1, 19))
    for case, names in rows.items():
        assert names, f"{case} names no test"


def test_every_abuse_case_has_a_test() -> None:
    missing = [
        f"{case}: {name}" for case, names in _table().items() for name in names if not _exists(name)
    ]
    assert missing == []


def test_the_checker_finds_and_misses() -> None:
    """The lookups work, so an empty miss list means something."""
    assert _exists("test_every_abuse_case_has_a_test")
    assert _exists("TestEgressProxyPinnedByDigest")
    assert _exists("test-egress.sh") and _exists("test_property.py")
    assert _exists("test_property[<provider>]")
    assert not _exists("test_no_such_abuse_case_test")
    assert not _exists("TestNoSuchAbuseCase")
    assert not _exists("no-such-file.sh")


def _todo() -> str:
    return " ".join(TODO.read_text("utf-8").split())


def test_todo_keeps_the_real_data_gate() -> None:
    """The human gate before real patient data (design §13.6; rulings C5,
    C8): its heading, its four numbered items and the untitled-names entry.
    A later edit that drops any of them fails here. No agent marks them
    done: they are people's sign-offs."""
    todo = _todo()
    for phrase in (
        "**Cloud models: before any real patient data (human gate, design §13.6).**",
        "(1) The masker upgrade (Nigerian identifiers and a local NER for names, including names "
        "without a title), measured on a synthetic Nigerian PII set at recall ≥ 0.99 for names and "
        "≥ 0.999 for numeric identifiers, signed off by the privacy lead.",
        "(2) The legal check:",
        "(3) The clinical evaluation of cloud answers on the synthetic evaluation set",
        "(4) Core supplies keyed hashes of the patient's name tokens with each job",
        "Until all four are done, cloud generation runs on demo and synthetic data only. "
        "No code flag represents this gate.",
        "**Cloud models: names without a title reach the provider today (ruling C8).**",
        "`test_untitled_name_on_core_shape_refused`",
        "No real patient data goes to a cloud provider until both the masker upgrade",
    ):
        assert phrase in todo, phrase


def test_todo_keeps_the_other_cloud_items() -> None:
    todo = _todo()
    for phrase in (
        "**Cloud models: stale budget reservations.**",
        "`mhai_cloud_settle_failures_total`",
        "**Cloud models: measured over-blocking and residuals for the H2 evaluation (ruling C4).**",
        "**Cloud models: egress isolation (design §16 question 6).**",
        "**Cloud models: the live smoke and recorded fixtures**",
    ):
        assert phrase in todo, phrase
