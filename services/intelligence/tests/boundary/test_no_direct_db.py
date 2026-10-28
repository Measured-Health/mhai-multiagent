"""The Python twin of core's nodirectdb_test.go (spec §8.2, F16).

Only boundary/db.py opens a connection or a cursor, so every query runs
inside with_hospital (SET LOCAL ROLE mhai_worker, the verified hospital).
The repositories (boundary/kb_repo.py, boundary/jobs_repo.py) may run SQL
only on a cursor they are handed as a parameter. Everything else may not
import a database driver, name a connection or pool class, or call
.cursor(), .execute() or .executemany(). Unit test, no Docker.
"""

from __future__ import annotations

import ast
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2] / "intelligence"
HELPER = "boundary/db.py"
REPOS = {"boundary/kb_repo.py", "boundary/jobs_repo.py"}
GENERATED = "boundary/pb/"

DRIVERS = {"psycopg", "psycopg_pool", "psycopg2", "asyncpg", "sqlalchemy", "sqlite3", "pg8000"}
CLASSES = {
    "Connection",
    "AsyncConnection",
    "ConnectionPool",
    "AsyncConnectionPool",
    "NullConnectionPool",
    "AsyncNullConnectionPool",
}
SQL_CALLS = {"execute", "executemany"}


def _params(fn: ast.FunctionDef | ast.AsyncFunctionDef) -> set[str]:
    a = fn.args
    return {p.arg for p in [*a.posonlyargs, *a.args, *a.kwonlyargs]}


def offenders(rel: str, source: str) -> list[str]:
    """What rel (a path under intelligence/) does that only db.py may."""
    if rel == HELPER or rel.startswith(GENERATED):
        return []
    tree = ast.parse(source)
    out: list[str] = []

    def where(node: ast.AST, what: str) -> None:
        out.append(f"{rel}:{getattr(node, 'lineno', 0)}: {what}")

    # Each node's enclosing function's parameters, for the repositories.
    params_of: dict[int, set[str]] = {}
    for fn in ast.walk(tree):
        if isinstance(fn, ast.FunctionDef | ast.AsyncFunctionDef):
            ps = _params(fn)
            for n in ast.walk(fn):
                params_of[id(n)] = params_of.get(id(n), set()) | ps

    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name.split(".")[0] in DRIVERS:
                    where(node, f"imports {alias.name}")
        elif isinstance(node, ast.ImportFrom):
            mod = (node.module or "").split(".")[0]
            if mod in DRIVERS:
                where(node, f"imports from {node.module}")
            for alias in node.names:
                if alias.name in CLASSES or alias.name == "connect":
                    where(node, f"imports {alias.name}")
            if node.module == "intelligence.boundary.db" and rel not in REPOS:
                for alias in node.names:
                    if alias.name in {"Cursor", "_pool"}:
                        where(node, f"imports {alias.name} from the helper")
        elif isinstance(node, ast.Name) and node.id in CLASSES:
            where(node, f"names {node.id}")
        elif isinstance(node, ast.Attribute):
            if node.attr in CLASSES:
                where(node, f"names .{node.attr}")
            elif node.attr in {"_pool", "_conn"}:
                where(node, f"reaches into .{node.attr}")
        elif isinstance(node, ast.Call):
            f = node.func
            if isinstance(f, ast.Attribute) and f.attr == "cursor":
                where(node, "calls .cursor()")
            elif isinstance(f, ast.Attribute) and f.attr in SQL_CALLS:
                recv = f.value
                ok = (
                    rel in REPOS
                    and isinstance(recv, ast.Name)
                    and recv.id in params_of.get(id(node), set())
                )
                if not ok:
                    where(node, f"calls .{f.attr}()")
            elif isinstance(f, ast.Attribute) and f.attr == "connect":
                recv = f.value
                if isinstance(recv, ast.Name) and recv.id in DRIVERS | CLASSES:
                    where(node, f"calls {recv.id}.connect()")
    return out


def test_no_direct_db() -> None:
    found: list[str] = []
    for p in sorted(ROOT.rglob("*.py")):
        rel = p.relative_to(ROOT).as_posix()
        found.extend(offenders(rel, p.read_text(encoding="utf-8")))
    assert found == []


def test_the_helper_and_repositories_exist() -> None:
    assert (ROOT / HELPER).is_file()
    assert (ROOT / "boundary/jobs_repo.py").is_file()


def test_guard_catches_each_kind() -> None:
    """The guard isn't vacuous: each way round the helper is caught."""
    bad = {
        "import psycopg": "imports psycopg",
        "from psycopg_pool import AsyncConnectionPool": "imports from psycopg_pool",
        "import sqlite3": "imports sqlite3",
        "from sqlalchemy import create_engine": "imports from sqlalchemy",
        "x = AsyncConnection": "names AsyncConnection",
        "x = psycopg.AsyncConnection": "names .AsyncConnection",
        "async def f(db):\n    db._pool": "reaches into ._pool",
        "async def f(conn):\n    conn.cursor()": "calls .cursor()",
        "async def f(cur):\n    await cur.execute('SELECT 1')": "calls .execute()",
        "async def f(cur):\n    await cur.executemany('SELECT 1', [])": "calls .executemany()",
        "from intelligence.boundary.db import Cursor": "imports Cursor from the helper",
    }
    for src, what in bad.items():
        got = offenders("agents/x.py", src)
        assert any(what in g for g in got), (src, got)
    # The repositories: SQL only on a cursor they were handed.
    repo = "boundary/jobs_repo.py"
    assert offenders(repo, "async def f(cur):\n    await cur.execute('SELECT 1')") == []
    assert offenders(repo, "from intelligence.boundary.db import Cursor") == []
    assert offenders(repo, "CUR = None\nasync def f():\n    await CUR.execute('SELECT 1')")
    assert offenders(repo, "async def f(conn):\n    conn.cursor()")
    assert offenders(repo, "import psycopg")
    # The helper and generated code are exempt; NATS's connect isn't SQL.
    assert offenders(HELPER, "import psycopg\nx = AsyncConnection") == []
    assert offenders("boundary/worker.py", "async def f(nc):\n    await nc.connect('x')") == []
