"""The Python twin of core's nodirectdb_test.go (spec §8.2, F16).

Only boundary/db.py opens a connection or a cursor, so every query runs
inside with_hospital (SET LOCAL ROLE mhai_worker, the verified hospital).
The repositories (boundary/kb_repo.py, boundary/jobs_repo.py) may run SQL
only on a cursor they are handed as a parameter, and only with SQL text
that is a literal (a string, a module constant bound once to one, or a
psycopg sql.SQL composition of one), so it is always parameterised.
Everything else may not import a database driver, name a connection or
pool class, or call .cursor(), .execute() or .executemany(). Nowhere may
getattr, __import__ or importlib.import_module look up a name that isn't a
literal, or one of those names. Unit test, no Docker.
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
# Names a dynamic lookup must not reach: getattr(cur, "execute") is .execute.
BANNED_ATTRS = CLASSES | SQL_CALLS | {"cursor", "connect", "_pool", "_conn"}
DYNAMIC = {"getattr", "__import__", "import_module"}  # import_module: importlib's


def _module_strings(tree: ast.Module) -> set[str]:
    """Module-level names bound exactly once, to a plain string literal,
    and never declared global (so never rebound inside a function)."""
    bound: dict[str, int] = {}
    literal: set[str] = set()
    for st in tree.body:
        targets: list[ast.expr] = []
        value: ast.expr | None = None
        if isinstance(st, ast.Assign):
            targets, value = list(st.targets), st.value
        elif isinstance(st, ast.AnnAssign | ast.AugAssign):
            targets, value = [st.target], st.value if isinstance(st, ast.AnnAssign) else None
        for t in targets:
            for n in ast.walk(t):
                if isinstance(n, ast.Name):
                    bound[n.id] = bound.get(n.id, 0) + 1
                    if (
                        len(targets) == 1
                        and t is n
                        and isinstance(value, ast.Constant)
                        and isinstance(value.value, str)
                    ):
                        literal.add(n.id)
    rebound = {name for g in ast.walk(tree) if isinstance(g, ast.Global) for name in g.names}
    return {n for n in literal if bound[n] == 1 and n not in rebound}


def _is_sql_composition(node: ast.expr) -> bool:
    """sql.SQL("literal"), optionally .format(...) or .join(...) on it:
    psycopg's safe composition, with the SQL text itself a literal."""
    if (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and node.func.attr in {"format", "join"}
    ):
        return _is_sql_composition(node.func.value)
    return (
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and node.func.attr == "SQL"
        and isinstance(node.func.value, ast.Name)
        and node.func.value.id == "sql"
        and len(node.args) == 1
        and isinstance(node.args[0], ast.Constant)
        and isinstance(node.args[0].value, str)
    )


def _is_safe_query(node: ast.expr, literals: set[str]) -> bool:
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return True
    if isinstance(node, ast.Name) and node.id in literals:
        return True
    return _is_sql_composition(node)


def _params(fn: ast.FunctionDef | ast.AsyncFunctionDef) -> set[str]:
    a = fn.args
    return {p.arg for p in [*a.posonlyargs, *a.args, *a.kwonlyargs]}


def offenders(rel: str, source: str) -> list[str]:
    """What rel (a path under intelligence/) does that only db.py may."""
    if rel == HELPER or rel.startswith(GENERATED):
        return []
    tree = ast.parse(source)
    out: list[str] = []
    literals = _module_strings(tree)

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
                    if alias.name in {"Cursor", "sql", "_pool"}:
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
                elif not node.args or not _is_safe_query(node.args[0], literals):
                    # Parameterised queries only: the SQL text is a literal
                    # (or a module constant, or a sql.SQL composition of
                    # one), never a string built at run time.
                    where(node, f"calls .{f.attr}() with SQL that isn't a literal")
            elif isinstance(f, ast.Attribute) and f.attr == "connect":
                recv = f.value
                if isinstance(recv, ast.Name) and recv.id in DRIVERS | CLASSES:
                    where(node, f"calls {recv.id}.connect()")
            fname = (
                f.id if isinstance(f, ast.Name) else f.attr if isinstance(f, ast.Attribute) else ""
            )
            if fname in DYNAMIC:
                # The name looked up: getattr's second argument, the
                # importers' first.
                pos = 1 if fname == "getattr" else 0
                arg = node.args[pos] if len(node.args) > pos else None
                if not (isinstance(arg, ast.Constant) and isinstance(arg.value, str)):
                    where(node, f"calls {fname}() with a name that isn't a literal")
                elif arg.value in BANNED_ATTRS or arg.value.split(".")[0] in DRIVERS:
                    where(node, f"calls {fname}() for {arg.value}")
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
    # Parameterised SQL only, in the repositories.
    for src in (
        "async def f(cur, t):\n    await cur.execute('SELECT * FROM ' + t)",
        "async def f(cur, t):\n    await cur.execute(f'SELECT * FROM {t}')",
        "async def f(cur, q):\n    await cur.execute(q)",
        "async def f(cur, t):\n    await cur.execute('SELECT %s' % t)",
        "async def f(cur, t):\n    await cur.execute('SELECT {}'.format(t))",
        "async def f(cur, t):\n    await cur.execute(sql.SQL(t))",
        "async def f(cur, t):\n    await cur.execute(sql.SQL('SELECT ' + t))",
        "async def f(cur):\n    await cur.execute()",
        "Q = 'SELECT 1'\nQ = Q + x\nasync def f(cur):\n    await cur.execute(Q)",
        "Q = 'SELECT 1'\nQ += x\nasync def f(cur):\n    await cur.execute(Q)",
        "Q = 'SELECT 1'\ndef g(x):\n    global Q\n    Q = x\nasync def f(cur):\n    await cur.execute(Q)",
    ):
        got = offenders(repo, src)
        assert any("isn't a literal" in g for g in got), (src, got)
    for src in (
        "async def f(cur, x):\n    await cur.execute('SELECT %s', (x,))",
        "Q = 'SELECT 1'\nasync def f(cur):\n    await cur.execute(Q)",
        "Q: str = 'SELECT 1'\nasync def f(cur):\n    await cur.execute(Q)",
        "from intelligence.boundary.db import sql\n"
        "async def f(cur, t):\n"
        "    await cur.execute(sql.SQL('SELECT * FROM {}').format(sql.Identifier(t)))",
    ):
        assert offenders(repo, src) == [], src
    # Dynamic lookups that could reach round the guard, anywhere.
    for src, what in {
        "getattr(cur, name)": "getattr() with a name that isn't a literal",
        "getattr(cur, 'exe' + 'cute')": "getattr() with a name that isn't a literal",
        "getattr(cur, 'execute')": "getattr() for execute",
        "getattr(db, '_pool')": "getattr() for _pool",
        "getattr(mod, 'AsyncConnection')": "getattr() for AsyncConnection",
        "__import__(name)": "__import__() with a name that isn't a literal",
        "__import__('psycopg')": "__import__() for psycopg",
        "import importlib\nimportlib.import_module(name)": "import_module() with a name",
        "from importlib import import_module\nimport_module('psycopg_pool')": "import_module() for",
        "getattr(cur)": "getattr() with a name that isn't a literal",
    }.items():
        got = offenders("agents/x.py", src)
        assert any(what in g for g in got), (src, got)
    assert offenders("agents/x.py", "getattr(obj, 'diagnoses', [])") == []
    assert offenders("agents/x.py", "from intelligence.boundary.db import sql")
    # The helper and generated code are exempt; NATS's connect isn't SQL.
    assert offenders(HELPER, "import psycopg\nx = AsyncConnection") == []
    assert offenders("boundary/worker.py", "async def f(nc):\n    await nc.connect('x')") == []
