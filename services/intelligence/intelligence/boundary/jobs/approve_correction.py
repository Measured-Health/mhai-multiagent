"""The approve_correction job (Plan 4 spec §4.7, §10.6): the run_query
skeleton, minus the model.

Core publishes it after a second clinician approves a correction (its own
transaction, its SecondPerson check, the database's two-person rules). The
worker's consumer loop (the pipeline_learning durable) hands
ApproveCorrection.handle a job that passed every check of spec §7.3. In
order:

  1. Not an ApproveCorrectionJob, or ids that aren't UUIDs: no claim, Term
     and a dead letter invalid_request. No database: unavailable (Nak, or
     on the last delivery Term and a dead letter). Past exp: Term and a
     dead letter deadline_exceeded.
  2. Claim under with_hospital(verified), in one statement
     (learning_repo.claim): only a jobs row of this type naming this
     correction. No row → not_found, Term and a dead letter, nothing
     written and nothing else read (C9); a terminal row → ack; a live lease
     → Nak until it ends; queued (a raced claim) → Nak. Over the attempts
     cap: failed, Term, a dead letter.
  3. In one transaction: hold the row (only while this claim owns it),
     read the correction's status and query, the decision and the latest
     evaluation (never the free text), compute the buffer entry through
     the LearningClient, then the buffer row, the correction.applied fact
     and status completed. Ack after the commit. A correction that isn't
     approved (core or the database went wrong; it alerts): failed
     invalid_request with correction.apply_failed, Term, a dead letter.
  4. A failure: retryable before the last delivery → end the lease and
     Nak with the schedule; permanent, or the last delivery → failed with
     correction.apply_failed, Term and a dead letter. The correction stays
     approved: a failed job never undoes the human decision.

The hospital is the verified token's, never the message's. Nothing here
checks who approved (buffer_needs_approval does, in the database), reads
the reason or the corrected diagnosis, writes guardrail weights or the
knowledge base, or acts on the operation's second_person flag (core's).
Logs and dead letters carry ids and codes only.
"""

from __future__ import annotations

import asyncio
import logging
import re
import time
import uuid
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from intelligence.boundary import learning_repo, telemetry
from intelligence.boundary.db import is_database_error
from intelligence.boundary.jobs import JobContext
from intelligence.boundary.jobs.run_query import RETRYABLE, JobSettings
from intelligence.boundary.learning import LearningClient
from intelligence.boundary.pb import envelope_pb2, learning_pb2
from intelligence.boundary.verify import Verified

if TYPE_CHECKING:
    from nats.aio.msg import Msg

    from intelligence.boundary.db import Database

log = logging.getLogger("intelligence.boundary.approve_correction")

TYPE = learning_repo.TYPE
PERSONA_UPDATES = frozenset({"skipped", "applied"})
# core's factschema: model names and versions, at most 16.
_VERSION_KEY = re.compile(r"[A-Za-z0-9_.:/\-]{1,64}")
_VERSION_VAL = re.compile(r"[A-Za-z0-9_.:/@+\-]{1,128}")
_MAX_VERSIONS = 16


@dataclass
class _Job:
    v: Verified = field(repr=False)
    msg: Msg = field(repr=False)
    ctx: JobContext = field(repr=False)
    delivered: int
    jid: str = ""
    cid: str = ""
    # The attempts this claim may reach: max_attempts per platform retry
    # (attempts never goes down; each retry is a fresh budget).
    cap: int = 0
    attempts: int = 0
    committed: bool = False
    settled: bool = False


def _delivered(msg: Msg) -> int:
    try:
        return max(1, int(msg.metadata.num_delivered))
    except Exception:
        return 1


def _fact_versions(raw: Mapping[str, Any]) -> dict[str, str]:
    """The learning client's versions as the correction.applied fact may
    carry them (core refuses the whole payload otherwise)."""
    out: dict[str, str] = {}
    for k, v in raw.items():
        if len(out) >= _MAX_VERSIONS:
            break
        if (
            isinstance(k, str)
            and isinstance(v, str)
            and _VERSION_KEY.fullmatch(k)
            and _VERSION_VAL.fullmatch(v)
        ):
            out[k] = v
    return out


class ApproveCorrection:
    """The pipeline's approve_correction handler. One per worker process."""

    def __init__(
        self,
        db: Database | None,
        learning: LearningClient,
        settings: JobSettings | None = None,
        *,
        clock: Callable[[], float] = time.time,
        after_commit: Callable[[], None] | None = None,
    ) -> None:
        self._db = db
        self._learning = learning
        self._s = settings or JobSettings()
        self._clock = clock
        self._after_commit = after_commit  # a test seam: a crash after the commit

    async def ready(self) -> bool:
        """Jobs may be taken: there is a database and it answers."""
        if self._db is None:
            return False
        try:
            return (await self._db.ready()) is True
        except Exception:
            return False

    async def handle(self, verified: Verified, msg: Msg, ctx: JobContext) -> None:
        """Run one delivery to exactly one outcome: ack, Nak, Term, or
        (after a crash past the commit) nothing, which the redelivery acks.
        Never raises but for cancellation."""
        job = _Job(v=verified, msg=msg, ctx=ctx, delivered=_delivered(msg))
        try:
            await self._run(job)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.error(
                "approve_correction failed job_id=%s attempts=%d error=%s committed=%s",
                job.jid or "-",
                job.attempts,
                type(e).__name__,
                job.committed,
            )
            if job.committed or job.settled:
                return  # the redelivery finds the row completed and acks
            try:
                await self._failure(job, "unavailable" if is_database_error(e) else "internal")
            except Exception as e2:
                log.error("approve_correction failure handling failed error=%s", type(e2).__name__)

    async def _run(self, job: _Job) -> None:
        message = job.v.message
        if not isinstance(message, learning_pb2.ApproveCorrectionJob):
            await self._permanent(job, "invalid_request")
            return
        try:
            job.jid = str(uuid.UUID(message.job_id))
            job.cid = str(uuid.UUID(message.correction_id))
        except ValueError:
            job.jid = job.cid = ""
            await self._permanent(job, "invalid_request")
            return
        telemetry.annotate(job_id=job.jid)
        if self._db is None:
            await self._failure(job, "unavailable")  # never claims without a database
            return
        if self._clock() >= job.v.exp:
            await self._permanent(job, "deadline_exceeded")
            return
        if not await self._claim(job):
            return
        try:
            await self._apply(job)
        except learning_repo.BufferRefused:
            # buffer_needs_approval (or the grant) refused the row: the
            # correction isn't approved by a second person. Permanent, never
            # retried; error level, it alerts (core or the database is
            # wrong). The transaction rolled back; the failure is its own.
            log.error(
                "approve_correction buffer row refused job_id=%s correction_id=%s",
                job.jid,
                job.cid,
            )
            await self._give_up(job, "invalid_request")

    def _last(self, job: _Job) -> bool:
        """This is the consumer's last delivery: a Nak would never come back."""
        return job.delivered >= self._s.max_deliver

    async def _claim(self, job: _Job) -> bool:
        assert self._db is not None
        try:
            async with self._db.with_hospital(job.v) as cur:
                c = await learning_repo.claim(cur, job.jid, job.cid)
        except Exception as e:
            if not is_database_error(e):
                raise
            log.warning(
                "approve_correction claim failed job_id=%s error=%s", job.jid, type(e).__name__
            )
            await self._failure(job, "unavailable")
            return False
        if c.attempts is not None:
            job.attempts = c.attempts
            job.cap = self._s.max_attempts * (c.retry_count + 1)
            telemetry.annotate(attempt=c.attempts)
            if c.attempts > job.cap:
                log.warning("approve_correction over the attempts cap job_id=%s", job.jid)
                await self._give_up(job, "internal")
                return False
            log.info("approve_correction claimed job_id=%s attempts=%d", job.jid, c.attempts)
            return True
        if c.status is None:
            # No job of this hospital names this correction: RLS shows none
            # (another hospital's ids, or none at all). Nothing written.
            log.warning("approve_correction not found job_id=%s", job.jid)
            await self._permanent(job, "not_found")
        elif c.status in learning_repo.TERMINAL:
            await self._settle(job, "ack")  # done, failed or cancelled already
        elif self._last(job):
            log.warning("approve_correction last delivery unclaimed job_id=%s", job.jid)
            await self._permanent(job, "unavailable")
        elif c.status == "running":
            await self._settle(job, "nak", c.lease_left + 1.0)  # another copy holds it
        else:
            await self._settle(job, "nak", 1.0)  # queued: raced a claim
        return False

    async def _apply(self, job: _Job) -> None:
        """Steps 3 and 4 of spec §4.7, in one transaction."""
        assert self._db is not None
        if self._clock() >= job.v.exp:
            await self._give_up(job, "deadline_exceeded")
            return
        code = ""
        async with self._db.with_hospital(job.v) as cur:
            if not await learning_repo.ours(cur, job.jid, job.attempts):
                # Cancelled, failed by the sweeper, or re-claimed by another
                # delivery. Not settled here on purpose: which of those it
                # is would take another read, and acking a message another
                # delivery now owns could drop it if that one later Naks.
                # JetStream redelivers this one after ack_wait, and that
                # delivery's claim sorts it out (terminal: ack; held: Nak).
                log.info("approve_correction no longer ours job_id=%s", job.jid)
                job.settled = True
                return
            loaded = await learning_repo.load(cur, job.cid)
            if loaded is None:
                code = "not_found"
            elif loaded.status != "approved":
                # Core published a job for a correction that isn't approved,
                # or the row changed under it: core or the database went
                # wrong. Error level: it alerts.
                log.error(
                    "approve_correction correction not approved job_id=%s correction_id=%s",
                    job.jid,
                    job.cid,
                )
                code = "invalid_request"
            elif loaded.decision is None:
                log.error("approve_correction no decision job_id=%s", job.jid)
                code = "invalid_request"
            if code:
                await learning_repo.fail(cur, job.jid, job.cid, job.attempts, code)
            else:
                assert loaded is not None and loaded.decision is not None
                entry = self._learning.buffer_entry(loaded.scores, loaded.decision)
                persona = self._learning.persona_update(job.cid)
                if persona not in PERSONA_UPDATES:
                    raise ValueError("unknown persona update")
                applied = await learning_repo.complete(
                    cur,
                    job.jid,
                    job.cid,
                    job.attempts,
                    entry,
                    loaded.model_versions,
                    _fact_versions(self._learning.model_versions()),
                    persona,
                )
                if not applied:
                    log.warning(
                        "approve_correction already applied job_id=%s correction_id=%s",
                        job.jid,
                        job.cid,
                    )
        if code:
            log.info(
                "approve_correction failed job_id=%s attempts=%d code=%s",
                job.jid,
                job.attempts,
                code,
            )
            await self._dead_letter(job, code)
            await self._settle(job, "term")
            return
        job.committed = True
        log.info("approve_correction completed job_id=%s attempts=%d", job.jid, job.attempts)
        if self._after_commit is not None:
            self._after_commit()
        await self._settle(job, "ack")

    # --- outcomes -------------------------------------------------------------------

    async def _failure(self, job: _Job, code: str) -> None:
        """Retry (Nak with the schedule) or, permanent or last, give up."""
        last = self._last(job) or (job.cap > 0 and job.attempts >= job.cap)
        if code in RETRYABLE and not last:
            if job.attempts and self._db is not None:
                try:
                    async with self._db.with_hospital(job.v) as cur:
                        await learning_repo.release_lease(cur, job.jid, job.attempts)
                except Exception as e:
                    if not is_database_error(e):
                        raise
            delays = self._s.nak_delays
            delay = delays[min(job.delivered, len(delays)) - 1] if delays else 0.0
            await self._settle(job, "nak", delay)
            return
        await self._give_up(job, code)

    async def _give_up(self, job: _Job, code: str) -> None:
        """failed with code and correction.apply_failed (if this claim owns
        the row), a dead letter, Term."""
        await self._mark_failed(job, code)
        await self._dead_letter(job, code)
        await self._settle(job, "term")

    async def _permanent(self, job: _Job, code: str) -> None:
        """No claim was made: a dead letter and Term; core marks the row."""
        await self._dead_letter(job, code)
        await self._settle(job, "term")

    async def _mark_failed(self, job: _Job, code: str) -> bool:
        if not job.attempts or self._db is None:
            return False
        try:
            async with self._db.with_hospital(job.v) as cur:
                written = await learning_repo.fail(cur, job.jid, job.cid, job.attempts, code)
        except Exception as e:
            if not is_database_error(e):
                raise
            log.warning(
                "approve_correction fail write failed job_id=%s error=%s",
                job.jid,
                type(e).__name__,
            )
            return False
        log.info(
            "approve_correction failed job_id=%s attempts=%d code=%s written=%s",
            job.jid,
            job.attempts,
            code,
            written,
        )
        return written

    async def _dead_letter(self, job: _Job, code: str) -> None:
        await job.ctx.dead_letter(
            envelope_pb2.DeadLetter(
                job_id=job.jid,
                type=TYPE,
                subject_hospital=job.v.hid,
                attempts=job.attempts or job.delivered,
                code=code,
                reason=code,
                kid=job.v.kid,
            )
        )

    async def _settle(self, job: _Job, how: str, delay: float = 0.0) -> None:
        """The message's one outcome. A failure to send it is logged: the
        message then comes back after ack_wait, and the claim sorts it out."""
        if job.settled:
            return
        if how == "nak" and self._last(job):
            # Never Nak the last delivery: JetStream wouldn't redeliver it.
            log.error("approve_correction refused to Nak the last delivery job_id=%s", job.jid)
            await self._mark_failed(job, "unavailable")
            await self._dead_letter(job, "unavailable")
            how = "term"
        job.settled = True
        try:
            if how == "ack":
                await job.msg.ack()
            elif how == "nak":
                await job.msg.nak(delay=max(0.0, delay))
            else:
                await job.msg.term()
        except Exception as e:
            log.warning(
                "approve_correction %s failed job_id=%s error=%s",
                how,
                job.jid or "-",
                type(e).__name__,
            )
