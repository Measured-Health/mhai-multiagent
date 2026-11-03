"""The approve_correction job's SQL (Plan 4 spec §4.7, §10.6): the claim on
the jobs table, the reads, and the completion into feedback_buffer, run on
the cursor `Database.with_hospital` yields.

Like jobs_repo, this module never opens a connection or a cursor
(test_no_direct_db holds it to that): the role (mhai_worker) and the
hospital (from the verified token) are already set, RLS hides every other
hospital's row, and every statement is a parameterised literal.

What it reads is what mhai_worker's grants allow and no more: of the
correction, its query and status (never the free text: 00011 grants the
worker seven non-text columns); of the decision, the system's decision and
the model versions; of the evaluation, the four expert scores, the trust
and the decision. Of the jobs row, nothing beyond the status columns.

It never checks who approved. buffer_needs_approval (00013, RESTRICTIVE)
refuses a buffer row whose correction isn't approved by someone other than
its submitter, whatever this code does. The worker has no SELECT on
feedback_buffer, so the insert is plain: no RETURNING, no ON CONFLICT.

Ownership of a running job is its attempts, as in jobs_repo: every write
after the claim is guarded by status = 'running' AND attempts = <the
claim's>, so a copy whose lease ran out (and was re-claimed), or whose row
the sweeper failed or the platform cancelled, changes nothing.
"""

from __future__ import annotations

import json
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from intelligence.boundary.db import Cursor, is_insufficient_privilege, is_unique_violation
from intelligence.boundary.learning import BufferEntry, Scores

TYPE = "approve_correction"
TERMINAL = frozenset({"completed", "failed", "cancelled"})
CORRECTION_APPLIED = "correction.applied"
CORRECTION_APPLY_FAILED = "correction.apply_failed"

# The claim and the row's state in one statement, so a job naming another
# hospital's correction (or none) logs this one statement and nothing else
# (C9). The outer SELECT sees the row as it was before the claim: its
# status matters only when nothing was claimed.
_CLAIM = """WITH claimed AS (
    UPDATE jobs SET status = 'running', attempts = attempts + 1,
        lease_until = now() + interval '60 s'
    WHERE id = %s AND type = 'approve_correction' AND subject_id = %s
        AND (status = 'queued' OR (status = 'running' AND lease_until < now()))
    RETURNING attempts, retry_count
)
SELECT (SELECT attempts FROM claimed), (SELECT retry_count FROM claimed), j.status,
    greatest(0, extract(epoch FROM (j.lease_until - now())))::float8
FROM jobs j WHERE j.id = %s AND j.type = 'approve_correction' AND j.subject_id = %s"""

# Still ours: locks the row and renews the lease, or finds nothing.
_OURS = """UPDATE jobs SET lease_until = now() + interval '60 s'
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_RELEASE = """UPDATE jobs SET lease_until = now()
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_CORRECTION = """SELECT query_id, status FROM feedback_events
WHERE id = %s AND action = 'correct'"""

_DECISION = """SELECT hga_decision, model_versions FROM clinical_decisions
WHERE query_id = %s"""

_SCORES = """SELECT nli_score, coherence_score, numerical_score, se_score, trust, decision
FROM hga_evaluations WHERE query_id = %s
ORDER BY ts DESC, id DESC LIMIT 1"""

_SAVEPOINT = "SAVEPOINT feedback_buffer_row"
_ROLLBACK_SAVEPOINT = "ROLLBACK TO SAVEPOINT feedback_buffer_row"
_RELEASE_SAVEPOINT = "RELEASE SAVEPOINT feedback_buffer_row"

_INSERT_BUFFER = """INSERT INTO feedback_buffer (hospital_id, correction_id, nli_score,
    coherence_score, numerical_score, se_score, system_decision, model_versions)
VALUES (mhai_current_hospital(), %s, %s, %s, %s, %s, %s, %s::jsonb)"""

_INSERT_FACT = """INSERT INTO intelligence_audit_outbox (hospital_id, job_id, action, entity,
    entity_id, payload)
VALUES (mhai_current_hospital(), %s, %s, 'correction', %s, %s::jsonb)"""

_COMPLETE = """UPDATE jobs SET status = 'completed', completed_at = now(), lease_until = NULL
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING id"""

_FAIL = """UPDATE jobs SET status = 'failed', error = %s, failed_at = now(), completed_at = now(),
    lease_until = NULL
WHERE id = %s AND status = 'running' AND attempts = %s
RETURNING attempts"""

# feedback_buffer.model_versions: an object of at most 4096 bytes (00013).
_MAX_VERSIONS = 16
_MAX_VERSIONS_BYTES = 4000


class BufferRefused(Exception):
    """The database refused the buffer row (42501: buffer_needs_approval or
    the grant). Permanent: the correction isn't approved by a second
    person, whatever the handler read a moment before."""


def _uuid(v: str) -> str:
    return str(uuid.UUID(v))


@dataclass(frozen=True)
class Claim:
    """The claim's outcome: attempts when this delivery claimed the row;
    otherwise the row's status and lease (status None: RLS shows no such
    job of this type and subject)."""

    attempts: int | None
    status: str | None
    lease_left: float = 0.0
    # The platform's retries of this row (spec §6.2): each retry gives the
    # job a fresh attempts budget, as attempts itself never goes down.
    retry_count: int = 0


@dataclass(frozen=True)
class Loaded:
    """What the job reads. status is the correction's; decision is None
    when the query has no decision row."""

    query_id: str
    status: str
    decision: str | None
    model_versions: Mapping[str, str]
    scores: Scores


async def claim(cur: Cursor, job_id: str, correction_id: str) -> Claim:
    """Claim the job (spec §4.7 step 2): queued → running, or re-claim a
    running row whose lease ran out; attempts + 1 and a 60 s lease. Only a
    jobs row of this type naming this correction: a job id and a subject
    that don't belong together claim nothing."""
    jid, cid = _uuid(job_id), _uuid(correction_id)
    await cur.execute(_CLAIM, (jid, cid, jid, cid))
    row = await cur.fetchone()
    if row is None:
        return Claim(None, None)
    attempts, retry_count, status, lease_left = row
    return Claim(
        int(attempts) if attempts is not None else None,
        str(status),
        float(lease_left or 0.0),
        int(retry_count or 0),
    )


async def ours(cur: Cursor, job_id: str, attempts: int) -> bool:
    """Lock the row and renew the lease, only while this claim owns it."""
    await cur.execute(_OURS, (_uuid(job_id), int(attempts)))
    return await cur.fetchone() is not None


async def release_lease(cur: Cursor, job_id: str, attempts: int) -> bool:
    """End this claim's lease now, so a Nak'd redelivery re-claims at once."""
    await cur.execute(_RELEASE, (_uuid(job_id), int(attempts)))
    return await cur.fetchone() is not None


def _versions(raw: Any) -> dict[str, str]:
    """The decision's model_versions as the buffer stores them: string
    pairs only, bounded (00013's CHECK)."""
    if not isinstance(raw, Mapping):
        return {}
    out: dict[str, str] = {}
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and len(out) < _MAX_VERSIONS:
            out[k] = v
    if len(json.dumps(out).encode()) > _MAX_VERSIONS_BYTES:
        return {}
    return out


def _real(v: Any) -> float | None:
    return float(v) if isinstance(v, int | float) and not isinstance(v, bool) else None


async def load(cur: Cursor, correction_id: str) -> Loaded | None:
    """The correction (None when RLS shows no such correction), its
    decision and the latest evaluation of its query."""
    await cur.execute(_CORRECTION, (_uuid(correction_id),))
    row = await cur.fetchone()
    if row is None:
        return None
    query_id, status = str(row[0]), str(row[1])
    await cur.execute(_DECISION, (query_id,))
    d = await cur.fetchone()
    await cur.execute(_SCORES, (query_id,))
    s = await cur.fetchone()
    scores = (
        Scores(
            nli=_real(s[0]),
            coherence=_real(s[1]),
            numerical=_real(s[2]),
            se=_real(s[3]),
            trust=_real(s[4]),
            decision=str(s[5]) if s[5] is not None else None,
        )
        if s is not None
        else Scores()
    )
    return Loaded(
        query_id=query_id,
        status=status,
        decision=str(d[0]) if d is not None and d[0] is not None else None,
        model_versions=_versions(d[1]) if d is not None else {},
        scores=scores,
    )


async def complete(
    cur: Cursor,
    job_id: str,
    correction_id: str,
    attempts: int,
    entry: BufferEntry,
    buffer_versions: Mapping[str, str],
    fact_versions: Mapping[str, str],
    persona_update: str,
) -> bool:
    """The completion (spec §4.7 step 5), in the caller's one transaction:
    the buffer row, the correction.applied fact, status completed. The
    caller holds the row (ours()). A buffer row already there for this
    correction (23505: another job of the same correction applied it) is
    "already applied": its insert alone is rolled back and the job still
    completes. Returns False in that case (already applied), else True.
    A refusal by the database (42501) raises BufferRefused."""
    jid, cid = _uuid(job_id), _uuid(correction_id)
    await cur.execute(_SAVEPOINT)
    try:
        await cur.execute(
            _INSERT_BUFFER,
            (
                cid,
                entry.nli,
                entry.coherence,
                entry.numerical,
                entry.se,
                entry.system_decision,
                json.dumps(dict(buffer_versions)),
            ),
        )
    except Exception as e:
        if is_insufficient_privilege(e):
            raise BufferRefused("feedback_buffer: refused") from None
        if not is_unique_violation(e):
            raise
        await cur.execute(_ROLLBACK_SAVEPOINT)
        applied = False
    else:
        applied = True
    await cur.execute(_RELEASE_SAVEPOINT)
    payload = {
        "correction_id": cid,
        "attempts": int(attempts),
        "model_versions": dict(fact_versions),
        "persona_update": persona_update,
    }
    await cur.execute(_INSERT_FACT, (jid, CORRECTION_APPLIED, cid, json.dumps(payload)))
    await cur.execute(_COMPLETE, (jid, int(attempts)))
    if await cur.fetchone() is None:  # can't happen while ours() holds the row lock
        raise RuntimeError("completion lost the row")
    return applied


async def fail(cur: Cursor, job_id: str, correction_id: str, attempts: int, code: str) -> bool:
    """status failed with code (a closed-set code, never text) and the
    correction.apply_failed fact, only while this claim owns the row. The
    correction stays approved: a failed job never undoes the decision."""
    jid, cid = _uuid(job_id), _uuid(correction_id)
    await cur.execute(_FAIL, (code, jid, int(attempts)))
    if await cur.fetchone() is None:
        return False
    payload = {"correction_id": cid, "error": code, "attempts": int(attempts)}
    await cur.execute(_INSERT_FACT, (jid, CORRECTION_APPLY_FAILED, cid, json.dumps(payload)))
    return True
