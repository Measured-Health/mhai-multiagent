"""The job SQL of clinical_queries (F16): the claim, the status updates and
the completion run here, on the cursor `Database.with_hospital` yields.

This module never opens a connection or a cursor (test_no_direct_db holds
it to that): each function takes the cursor, so the role (mhai_worker) and
the hospital (from the verified token) are already set, and RLS hides every
other hospital's row. Parameterised queries only.

Ownership of a running row is its `attempts`: every claim bumps it (the
trigger of 00008 stops it ever going down), so `status = 'running' AND
attempts = <the claim's>` holds only for the claim that set it. Every write
after the claim is guarded that way, so a worker whose lease ran out (and
whose job another delivery re-claimed), or whose row core cancelled or the
sweeper failed, changes nothing. The worker_status policy of 00008 adds the
database's own wall: only queued or running rows, only to running,
completed (with a decision row) or failed.

The hospital of every row written here is mhai_current_hospital(), the one
with_hospital set from the verified token; nothing here takes a hospital.
"""

from __future__ import annotations

import json
import uuid
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any

from intelligence.boundary.db import Cursor

TERMINAL = frozenset({"completed", "failed", "cancelled"})
QUERY_COMPLETED = "query.completed"
QUERY_FAILED = "query.failed"

_CLAIM = """UPDATE clinical_queries SET status = 'running', attempts = attempts + 1,
    started_at = coalesce(started_at, now()),
    lease_until = now() + make_interval(secs => %s)
WHERE id = %s AND (status = 'queued' OR (status = 'running' AND lease_until < now()))
RETURNING clinical_question, patient_data_json, urgency, category, attempts"""

_STATE = """SELECT status,
    greatest(0, extract(epoch FROM (lease_until - now())))::float8
FROM clinical_queries WHERE id = %s"""

_CHECKPOINT = """UPDATE clinical_queries SET progress = %s, current_stage = %s,
    lease_until = now() + make_interval(secs => %s)
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_EXTEND = """UPDATE clinical_queries SET lease_until = now() + make_interval(secs => %s)
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_RELEASE = """UPDATE clinical_queries SET lease_until = now()
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_INSERT_DECISION = """INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text,
    citations_json, trust_score, hga_decision, guardrail_report_json, retrieval_ms,
    generation_ms, verified, degraded, failed_agents, model_versions, trust_band)
VALUES (%s, mhai_current_hospital(), %s, %s, %s::jsonb, %s, %s, %s::jsonb, %s, %s, %s, %s,
    %s::text[], %s::jsonb, %s)"""

_INSERT_HGA = """INSERT INTO hga_evaluations (hospital_id, query_id, target_agent, trust, decision,
    nli_score, coherence_score, numerical_score, se_score, flag_penalty, weights_snapshot_json,
    cascade_layers_json)
VALUES (mhai_current_hospital(), %s, %s, %s, %s, %s, %s, %s, %s, %s, %s::jsonb, %s::jsonb)"""

_INSERT_FACT = """INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action, entity,
    entity_id, payload)
VALUES (mhai_current_hospital(), %s, %s, 'query', %s, %s::jsonb)"""

_COMPLETE = """UPDATE clinical_queries SET status = 'completed', progress = 1,
    current_stage = 'decision', completed_at = now(), lease_until = NULL
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_FAIL = """UPDATE clinical_queries SET status = 'failed', error = %s, completed_at = now(),
    lease_until = NULL
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING attempts"""


def _qid(query_id: str) -> str:
    return str(uuid.UUID(query_id))


@dataclass(frozen=True)
class Claim:
    """A claimed row: what the job needs, and the attempts that own it."""

    question: str = field(repr=False)
    patient_data: Any = field(repr=False)
    urgency: str
    category: str | None
    attempts: int


@dataclass(frozen=True)
class State:
    """A row's status, and how long its lease has left (seconds)."""

    status: str
    lease_left: float


@dataclass(frozen=True)
class Decision:
    """The completed answer, as the database stores it. Text fields never
    reach a log (repr=False)."""

    response_text: str = field(repr=False)
    citations: Sequence[str]
    trust_score: float | None
    decision: str
    trust_band: str | None
    verified: bool
    degraded: bool
    failed_agents: Sequence[str]
    model_versions: Mapping[str, str]
    guardrail_report: Mapping[str, Any]
    retrieval_ms: float | None
    generation_ms: float | None
    # hga_evaluations
    scores: Mapping[str, float]
    flag_penalty: float | None
    weights: Mapping[str, float]
    flags: Sequence[Sequence[str]]


async def read_status(cur: Cursor, query_id: str) -> str | None:
    """The status of query_id, or None when this hospital has no such
    query (another hospital's id included: RLS hides it, so a forged id
    reads as not found)."""
    qid = _qid(query_id)
    await cur.execute("SELECT status FROM clinical_queries WHERE id = %s", (qid,))
    row = await cur.fetchone()
    return None if row is None else str(row[0])


async def claim(cur: Cursor, query_id: str, lease_s: float) -> Claim | None:
    """Claim query_id (spec §9.2): queued → running, or re-claim a running
    row whose lease has run out; attempts + 1, a fresh lease. None when
    nothing was claimed (see state())."""
    await cur.execute(_CLAIM, (float(lease_s), _qid(query_id)))
    row = await cur.fetchone()
    if row is None:
        return None
    question, patient, urgency, category, attempts = row
    return Claim(str(question), patient, str(urgency), category, int(attempts))


async def state(cur: Cursor, query_id: str) -> State | None:
    """The row's status and lease, or None when RLS shows no such row."""
    await cur.execute(_STATE, (_qid(query_id),))
    row = await cur.fetchone()
    if row is None:
        return None
    return State(str(row[0]), float(row[1] or 0.0))


async def checkpoint(
    cur: Cursor, query_id: str, attempts: int, progress: float, stage: str, lease_s: float
) -> bool:
    """Between stages: progress, stage and a fresh lease, only while this
    claim still owns the row. False: cancelled, failed, done or re-claimed."""
    await cur.execute(
        _CHECKPOINT, (float(progress), stage, float(lease_s), _qid(query_id), int(attempts))
    )
    return await cur.fetchone() is not None


async def extend_lease(cur: Cursor, query_id: str, attempts: int, lease_s: float) -> bool:
    """A fresh lease (with +WIP), only while this claim owns the row."""
    await cur.execute(_EXTEND, (float(lease_s), _qid(query_id), int(attempts)))
    return await cur.fetchone() is not None


async def release_lease(cur: Cursor, query_id: str, attempts: int) -> bool:
    """End this claim's lease now, so the Nak'd redelivery re-claims at once
    instead of waiting out the lease (each wait would cost a delivery)."""
    await cur.execute(_RELEASE, (_qid(query_id), int(attempts)))
    return await cur.fetchone() is not None


async def complete(cur: Cursor, query_id: str, attempts: int, d: Decision) -> bool:
    """The completion (spec §9.2), all in the caller's one transaction: lock
    the row (only if this claim owns it), the decision (unique query_id),
    the evaluation, the query.completed fact, then status completed. False
    when the claim no longer owns the row: nothing is written. A unique
    violation propagates: the caller rolls back."""
    qid = _qid(query_id)
    await cur.execute(_EXTEND, (60.0, qid, int(attempts)))  # locks the row, or not ours
    if await cur.fetchone() is None:
        return False
    await cur.execute(
        _INSERT_DECISION,
        (
            str(uuid.uuid4()),
            qid,
            d.response_text,
            json.dumps(list(d.citations)),
            d.trust_score,
            d.decision,
            json.dumps(dict(d.guardrail_report)),
            d.retrieval_ms,
            d.generation_ms,
            d.verified,
            d.degraded,
            list(d.failed_agents),
            json.dumps(dict(d.model_versions)),
            d.trust_band,
        ),
    )
    await cur.execute(
        _INSERT_HGA,
        (
            qid,
            "CCA",
            d.trust_score,
            d.decision,
            d.scores.get("nli"),
            d.scores.get("coherence"),
            d.scores.get("numerical"),
            d.scores.get("se"),
            d.flag_penalty,
            json.dumps(dict(d.weights)),
            json.dumps([list(f) for f in d.flags]),
        ),
    )
    payload = {
        "query_id": qid,
        "decision": d.decision,
        "trust_band": d.trust_band,
        "chunk_ids": list(d.citations),
        "model_versions": dict(d.model_versions),
        "attempts": int(attempts),
    }
    await cur.execute(_INSERT_FACT, (qid, QUERY_COMPLETED, qid, json.dumps(payload)))
    await cur.execute(_COMPLETE, (qid, int(attempts)))
    if await cur.fetchone() is None:  # can't happen while we hold the row lock
        raise RuntimeError("completion lost the row")
    return True


async def fail(cur: Cursor, query_id: str, attempts: int, code: str) -> bool:
    """status failed with code (a closed-set code, never text) and the
    query.failed fact, only while this claim owns the row."""
    qid = _qid(query_id)
    await cur.execute(_FAIL, (code, qid, int(attempts)))
    if await cur.fetchone() is None:
        return False
    payload = {"query_id": qid, "error": code, "attempts": int(attempts)}
    await cur.execute(_INSERT_FACT, (qid, QUERY_FAILED, qid, json.dumps(payload)))
    return True
