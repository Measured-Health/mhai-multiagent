"""The pipeline's only door to Postgres (spec §8.2).

Every query runs inside `Database.with_hospital(verified)`:

    BEGIN; SET LOCAL ROLE mhai_worker;
    SELECT set_config('mhai.hospital_id', <verified.hid>, true); ...; COMMIT

The hospital comes only from a `Verified` (which only the verifier makes),
never from a string a caller passes. Row-level security is the second wall.

The login (INTELLIGENCE_DB_USER, default intelligence_app) is a member of
mhai_worker_app only, which holds mhai_worker WITH INHERIT FALSE, so outside
`with_hospital` the login can read nothing. `check_login()` refuses to start
on any login that could see past that, the twin of core's db.CheckLoginRole.

Only this module opens a connection or a cursor; `tests/boundary/
test_no_direct_db.py` holds every other module to that. The repositories
(`kb_repo.py`, `jobs_repo.py`) run parameterised SQL on the cursor this
module yields, and nothing else.

Errors and logs carry no credentials, no SQL parameters and no row data:
driver errors are logged by type only.
"""

from __future__ import annotations

import logging
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

import psycopg
from psycopg import AsyncConnection, AsyncCursor
from psycopg_pool import AsyncConnectionPool

from intelligence.boundary.config import DEFAULT_DB_USER, parse_db_url
from intelligence.boundary.verify import Verified

log = logging.getLogger("intelligence.boundary.db")

# The cursor with_hospital yields: the type the repositories take.
Cursor = AsyncCursor[Any]

WORKER_ROLE = "mhai_worker"
LOGIN_ROLE = "mhai_worker_app"

_SET_ROLE = "SET LOCAL ROLE mhai_worker"
_SET_HOSPITAL = "SELECT set_config('mhai.hospital_id', %s, true)"

# Run as the login itself, outside any SET ROLE. Postgres 16.
_CHECK_LOGIN = r"""
SELECT r.rolname, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb, r.rolreplication,
       ARRAY(SELECT g.rolname::text FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
             WHERE m.member = r.oid ORDER BY 1),
       ARRAY(SELECT g.rolname::text FROM pg_roles g
             WHERE g.oid <> r.oid AND g.rolname <> %(login_role)s
               AND pg_has_role(r.oid, g.oid, 'USAGE') ORDER BY 1),
       CASE WHEN EXISTS (SELECT 1 FROM pg_roles w WHERE w.rolname = %(worker)s)
            THEN pg_has_role(r.oid, %(worker)s, 'SET') ELSE false END,
       pg_has_role(r.oid, (SELECT d.datdba FROM pg_database d WHERE d.datname = current_database()),
                   'MEMBER'),
       EXISTS (SELECT 1 FROM pg_namespace n
               WHERE pg_has_role(r.oid, n.nspowner, 'MEMBER')
                 AND n.nspname NOT LIKE 'pg\_%%' AND n.nspname <> 'information_schema'),
       EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
               WHERE pg_has_role(r.oid, c.relowner, 'MEMBER')
                 AND n.nspname NOT LIKE 'pg\_%%' AND n.nspname <> 'information_schema'),
       current_user::text,
       COALESCE(current_setting('mhai.hospital_id', true), '')
FROM pg_roles r WHERE r.rolname = session_user
"""


class DatabaseError(Exception):
    """The database can't be used. The message names no credential, query
    parameter or row."""


class NoHospital(DatabaseError):
    """with_hospital was given a Verified without a usable hospital id."""


class LoginRefused(DatabaseError):
    """check_login found a login that row-level security can't hold."""


def connect_kwargs(
    url: str, *, user: str | None = None, password: str | None = None
) -> dict[str, Any]:
    """libpq keywords for url, with the credentials passed separately
    (never inside a connection string). user and password, when given,
    win over the URL's."""
    u = parse_db_url(url)
    kw: dict[str, Any] = {"host": u.host, "dbname": u.dbname}
    if u.port is not None:
        kw["port"] = u.port
    kw.update(dict(u.params))
    kw["user"] = user or u.user or DEFAULT_DB_USER
    pw = password if password is not None else u.password
    if pw is not None:
        kw["password"] = pw
    return kw


async def _reset(conn: AsyncConnection[Any]) -> None:
    # A query may have run a session-level SET ROLE or set_config(..., false)
    # inside with_hospital; nothing it did outlives its transaction.
    await conn.execute("DISCARD ALL")


class Database:
    """A psycopg 3 connection pool for the pipeline's login."""

    def __init__(
        self,
        url: str,
        *,
        user: str | None = None,
        password: str | None = None,
        min_size: int = 1,
        max_size: int = 4,
        timeout: float = 10.0,
    ) -> None:
        kwargs = connect_kwargs(url, user=user, password=password)
        # autocommit: with_hospital opens every transaction itself (BEGIN),
        # and a statement outside it runs as the bare login, which holds
        # nothing.
        kwargs["autocommit"] = True
        self._timeout = timeout
        self._pool: AsyncConnectionPool[AsyncConnection[Any]] = AsyncConnectionPool(
            kwargs=kwargs,
            min_size=min_size,
            max_size=max_size,
            open=False,
            reset=_reset,
            check=AsyncConnectionPool.check_connection,
            timeout=timeout,
            name="intelligence",
        )
        self._open = False

    def __repr__(self) -> str:
        return "Database()"

    async def open(self) -> None:
        """Connect and run check_login. Refuses to go on (and closes) on any
        failure."""
        try:
            await self._pool.open(wait=True, timeout=self._timeout)
        except Exception as e:
            await self._pool.close()
            log.error("database open failed error=%s", type(e).__name__)
            raise DatabaseError("database: can't connect") from None
        try:
            await self.check_login()
        except BaseException:
            await self._pool.close()
            raise
        self._open = True

    async def close(self) -> None:
        self._open = False
        await self._pool.close()

    async def check_login(self) -> None:
        """Refuse a login that row-level security can't hold: a superuser,
        BYPASSRLS, CREATEROLE, CREATEDB or REPLICATION; a member of anything
        but mhai_worker_app; one that inherits any role's privileges (only
        SET LOCAL ROLE mhai_worker inside with_hospital may reach them); one
        that can't SET ROLE mhai_worker; one that starts in another role or
        with a hospital set (a role default or PGOPTIONS); an owner of the
        database, a schema or a table."""
        try:
            async with self._pool.connection() as conn:
                cur = await conn.execute(
                    _CHECK_LOGIN, {"login_role": LOGIN_ROLE, "worker": WORKER_ROLE}
                )
                row = await cur.fetchone()
        except psycopg.Error as e:
            log.error("database login check failed error=%s", type(e).__name__)
            raise DatabaseError("database: login check failed") from None
        if row is None:
            raise LoginRefused("database: login check found no role")
        (name, sup, bypass, createrole, createdb, replication, direct, inherited) = row[:8]
        can_set, owns_db, owns_schema, owns_rel, current, hospital = row[8:]
        problems: list[str] = []
        for flag, what in (
            (sup, "it is a superuser"),
            (bypass, "it has BYPASSRLS"),
            (createrole, "it has CREATEROLE"),
            (createdb, "it has CREATEDB"),
            (replication, "it has REPLICATION"),
        ):
            if flag:
                problems.append(what)
        if list(direct) != [LOGIN_ROLE]:
            others = [g for g in direct if g != LOGIN_ROLE]
            if LOGIN_ROLE not in direct:
                problems.append(f"it is not a member of {LOGIN_ROLE}")
            if others:
                problems.append("it is also a member of " + ", ".join(others))
        if inherited and not sup:
            problems.append("it inherits the privileges of " + ", ".join(inherited))
        if not can_set and not sup:
            problems.append(f"it can't SET ROLE {WORKER_ROLE}")
        if current != name:
            # A role-level default (ALTER ROLE ... SET role) or PGOPTIONS:
            # every connection would start in that role, outside
            # with_hospital, and DISCARD ALL would reset to it.
            problems.append(f"it starts as {current}, not as itself")
        if hospital:
            problems.append("it starts with a hospital set")
        if owns_db:
            problems.append("it owns the database")
        if owns_schema:
            problems.append("it owns a schema")
        if owns_rel:
            problems.append("it owns a table or other relation")
        if problems:
            raise LoginRefused(
                f"database: refusing login {name!r}: "
                + "; ".join(problems)
                + f"; use a login that is only a member of {LOGIN_ROLE} (core migrate up makes one)"
            )

    async def ready(self) -> bool:
        """For pipeline.health's database_ready: the pool answers."""
        if not self._open:
            return False
        try:
            async with self._pool.connection(timeout=2.0) as conn:
                await conn.execute("SELECT 1")
        except Exception as e:
            log.warning("database not ready error=%s", type(e).__name__)
            return False
        return True

    @asynccontextmanager
    async def with_hospital(self, verified: Verified) -> AsyncIterator[Cursor]:
        """A cursor in one transaction as mhai_worker, scoped to the
        verified token's hospital. Commits if the block ends normally, rolls
        back if it raises. Parameterised queries only."""
        if type(verified) is not Verified:
            raise TypeError("with_hospital takes a Verified, from the verifier only")
        hid = verified.hid
        if not isinstance(hid, str) or not hid:
            raise NoHospital("database: no hospital")
        try:
            hospital = str(uuid.UUID(hid))
        except ValueError:
            raise NoHospital("database: no hospital") from None
        if hospital == str(uuid.UUID(int=0)):
            raise NoHospital("database: no hospital")
        async with self._pool.connection() as conn, conn.transaction():
            cur = conn.cursor()
            try:
                await cur.execute(_SET_ROLE)
                await cur.execute(_SET_HOSPITAL, (hospital,))
                yield cur
            finally:
                await cur.close()
