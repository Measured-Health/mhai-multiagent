"""The job SQL of clinical_queries (F16): the claim, the status updates and
the completion run here, on the cursor `Database.with_hospital` yields.

This module never opens a connection or a cursor (test_no_direct_db holds
it to that): each function takes the cursor, so the role (mhai_worker) and
the hospital (from the verified token) are already set, and RLS hides every
other hospital's row. Parameterised queries only. Plan 3 Task 11 adds the
claim and completion.
"""

from __future__ import annotations

import uuid

from intelligence.boundary.db import Cursor


async def read_status(cur: Cursor, query_id: str) -> str | None:
    """The status of query_id, or None when this hospital has no such
    query (another hospital's id included: RLS hides it, so a forged id
    reads as not found)."""
    qid = str(uuid.UUID(query_id))
    await cur.execute("SELECT status FROM clinical_queries WHERE id = %s", (qid,))
    row = await cur.fetchone()
    return None if row is None else str(row[0])
