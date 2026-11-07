"""The budget port against Postgres (plan Task 7: A11, K4, K5; ruling C1).

The rules are core's (migration 00018: mhai_cloud_reserve and
mhai_cloud_settle). These tests drive them through DbBudget, as the
pipeline's login under with_hospital, from worker threads, against the
Postgres core's own `migrate up` built (conftest.postgres). Each test makes
its own synthetic hospitals, so ledgers never meet. Needs Docker; never
loads a model or calls a provider.
"""

from __future__ import annotations

import asyncio
import uuid
from collections.abc import AsyncIterator
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import psycopg
import pytest

from intelligence.boundary.cloud.budget import DbBudget, Reservation
from intelligence.boundary.db import Database
from tests.boundary.conftest import INTELLIGENCE_DB_PASSWORD, INTELLIGENCE_DB_USER, Postgres
from tests.boundary.test_db import verified_for

pytestmark = pytest.mark.boundary

TOKENS = 2000


async def _su(pg: Postgres) -> psycopg.AsyncConnection[Any]:
    return await psycopg.AsyncConnection.connect(pg.url, autocommit=True)


async def _hospital(pg: Postgres, provider: str | None, budget: int, status: str = "active") -> str:
    """A fresh synthetic hospital with a policy naming provider."""
    hid = str(uuid.uuid4())
    code = "CB" + uuid.uuid4().hex[:6].upper()
    async with await _su(pg) as conn:
        await conn.execute(
            "INSERT INTO hospitals (id, name, short_code, status, time_zone) "
            "VALUES (%s, %s, %s, %s, 'Africa/Lagos')",
            (hid, "Synthetic Budget Hospital " + code, code, status),
        )
        await conn.execute(
            "INSERT INTO hospital_model_policy (hospital_id, cloud_provider, agreement_ref, "
            "monthly_token_budget) VALUES (%s, %s, %s, %s)",
            (hid, provider, "DEMO-SYNTHETIC" if provider else None, budget),
        )
    return hid


async def _set_provider(pg: Postgres, hid: str, provider: str | None) -> None:
    async with await _su(pg) as conn:
        await conn.execute(
            "UPDATE hospital_model_policy SET cloud_provider = %s, agreement_ref = %s "
            "WHERE hospital_id = %s",
            (provider, "DEMO-SYNTHETIC" if provider else None, hid),
        )


async def _ledger(pg: Postgres, hid: str) -> tuple[int, int, int]:
    """(used, reserved, open reservations) of hid this month."""
    async with await _su(pg) as conn:
        cur = await conn.execute(
            "SELECT coalesce(sum(used), 0)::bigint, coalesce(sum(reserved), 0)::bigint "
            "FROM cloud_usage WHERE hospital_id = %s",
            (hid,),
        )
        row = await cur.fetchone()
        cur = await conn.execute(
            "SELECT count(*) FROM cloud_reservations WHERE hospital_id = %s", (hid,)
        )
        n = await cur.fetchone()
    assert row is not None and n is not None
    return int(row[0]), int(row[1]), int(n[0])


@pytest.fixture
async def database(postgres: Postgres) -> AsyncIterator[Database]:
    db = Database(
        postgres.intelligence_url,
        user=INTELLIGENCE_DB_USER,
        password=INTELLIGENCE_DB_PASSWORD,
        max_size=4,
    )
    await db.open()
    try:
        yield db
    finally:
        await db.close()


def _port(db: Database, hid: str) -> DbBudget:
    return DbBudget(db, verified_for(hid), asyncio.get_running_loop(), timeout=30.0)


async def _in_thread(fn: Any, *args: Any) -> Any:
    return await asyncio.to_thread(fn, *args)


async def test_budget_race_through_the_port(postgres: Postgres, database: Database) -> None:
    """A11: 20 threads against a budget for 5 reservations: exactly 5."""
    hid = await _hospital(postgres, "anthropic", 5 * TOKENS)
    port = _port(database, hid)
    loop = asyncio.get_running_loop()
    with ThreadPoolExecutor(max_workers=20) as pool:
        got = await asyncio.gather(
            *(loop.run_in_executor(pool, port.reserve, "anthropic", TOKENS) for _ in range(20))
        )
    won = [r for r in got if r is not None]
    assert len(won) == 5
    assert len({r.id for r in won}) == 5
    assert all(isinstance(r, Reservation) and r.tokens == TOKENS for r in won)
    assert await _ledger(postgres, hid) == (0, 5 * TOKENS, 5)


async def test_budget_uses_token_hospital_only(postgres: Postgres, database: Database) -> None:
    """The port has no hospital parameter: it reserves and settles for the
    verified token's hospital, and another hospital's ledger is untouched,
    even when handed that hospital's reservation."""
    a = await _hospital(postgres, "openai", 10 * TOKENS)
    b = await _hospital(postgres, "openai", 10 * TOKENS)
    pa, pb = _port(database, a), _port(database, b)
    rb = await _in_thread(pb.reserve, "openai", TOKENS)
    assert rb is not None
    before_b = await _ledger(postgres, b)
    ra = await _in_thread(pa.reserve, "openai", TOKENS)
    assert ra is not None
    await _in_thread(pa.settle, rb, 1)  # B's reservation, through A's port
    assert await _ledger(postgres, b) == before_b == (0, TOKENS, 1)
    assert await _ledger(postgres, a) == (0, TOKENS, 1)


async def test_settle_twice_is_a_no_op(postgres: Postgres, database: Database) -> None:
    """K5: a settle consumes its reservation; a second one changes nothing.
    An unknown reply (used=None) counts the whole reservation."""
    hid = await _hospital(postgres, "xai", 10 * TOKENS)
    port = _port(database, hid)
    r1 = await _in_thread(port.reserve, "xai", TOKENS)
    r2 = await _in_thread(port.reserve, "xai", TOKENS)
    assert r1 is not None and r2 is not None
    await _in_thread(port.settle, r1, 300)
    assert await _ledger(postgres, hid) == (300, TOKENS, 1)
    await _in_thread(port.settle, r1, 300)
    await _in_thread(port.settle, r1, None)
    assert await _ledger(postgres, hid) == (300, TOKENS, 1)
    await _in_thread(port.settle, r2, None)
    assert await _ledger(postgres, hid) == (300 + TOKENS, 0, 0)


async def test_policy_switch_refuses_the_old_provider(
    postgres: Postgres, database: Database
) -> None:
    """Ruling C1: the reservation names its provider; after the policy moves
    to another provider, or off, the old one gets nothing."""
    hid = await _hospital(postgres, "openai", 10 * TOKENS)
    port = _port(database, hid)
    assert await _in_thread(port.reserve, "anthropic", TOKENS) is None
    assert await _in_thread(port.reserve, "openai", TOKENS) is not None
    await _set_provider(postgres, hid, "anthropic")
    assert await _in_thread(port.reserve, "openai", TOKENS) is None
    assert await _in_thread(port.reserve, "anthropic", TOKENS) is not None
    await _set_provider(postgres, hid, None)
    for p in ("openai", "anthropic", "xai"):
        assert await _in_thread(port.reserve, p, TOKENS) is None
    assert await _ledger(postgres, hid) == (0, 2 * TOKENS, 2)


async def test_inactive_hospital_gets_no_reservation(
    postgres: Postgres, database: Database
) -> None:
    hid = await _hospital(postgres, "anthropic", 10 * TOKENS)
    port = _port(database, hid)
    async with await _su(postgres) as conn:
        await conn.execute("UPDATE hospitals SET status = 'deactivated' WHERE id = %s", (hid,))
    assert await _in_thread(port.reserve, "anthropic", TOKENS) is None
    assert await _ledger(postgres, hid) == (0, 0, 0)


async def test_db_error_from_core_fails_closed(postgres: Postgres, database: Database) -> None:
    """Core refuses out-of-range tokens with an error: no reservation, and
    the transaction rolls back."""
    hid = await _hospital(postgres, "anthropic", 10**9)
    port = _port(database, hid)
    assert await _in_thread(port.reserve, "anthropic", 100_001) is None
    assert await _in_thread(port.reserve, "anthropic", 0) is None
    assert await _ledger(postgres, hid) == (0, 0, 0)
