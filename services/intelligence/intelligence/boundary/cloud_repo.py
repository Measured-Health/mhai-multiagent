"""The cloud token budget's SQL (design section 10; plan Task 7): calls of
core's two functions of migration 00018, and nothing else.

The budget rule is core's: mhai_cloud_reserve(provider, tokens) reserves
in one atomic statement, only for mhai_current_hospital(), only while that
hospital is active and its policy names that provider now, and only under
its monthly budget; mhai_cloud_settle(reservation, used) consumes a
reservation of the caller's own hospital, once. This module copies none of
that: it passes the arguments and reads the result.

It never opens a connection or a cursor (test_no_direct_db holds it to
that): each function takes the cursor `Database.with_hospital` yields, so
the role (mhai_worker) and the hospital (from the verified token) are
already set. Parameterised queries only. Nothing here takes a hospital.
"""

from __future__ import annotations

from intelligence.boundary.db import Cursor

_RESERVE = "SELECT reservation, percent FROM mhai_cloud_reserve(%s::text, %s::bigint)"
_SETTLE = "SELECT mhai_cloud_settle(%s::uuid, %s::bigint)"


async def reserve(cur: Cursor, provider: str, tokens: int) -> tuple[str, int] | None:
    """(reservation id, percent of the budget now taken), or None when core
    reserved nothing (budget, policy or hospital). Core's errors raise."""
    await cur.execute(_RESERVE, (provider, tokens))
    row = await cur.fetchone()
    if row is None or row[0] is None or row[1] is None:
        return None
    return str(row[0]), int(row[1])


async def settle(cur: Cursor, reservation: str, used: int | None) -> bool:
    """True if this call consumed the reservation; False if core found none
    of this hospital's by that id (already settled, or another's)."""
    await cur.execute(_SETTLE, (reservation, used))
    row = await cur.fetchone()
    return bool(row is not None and row[0] is True)
