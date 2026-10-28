"""The run_query job (spec §8.2, §9.2-9.4, plan Task 11).

The worker's consumer loop hands RunQuery.handle a job that passed every
check of spec §7.3. In order:

  1. No database, or a query_id that isn't a UUID: no claim. Unavailable
     is Nak'd (or, on the last delivery, Termed with a dead letter); a bad
     id is permanent: Term and a dead letter invalid_request.
  2. Claim, under with_hospital(verified): queued → running, or re-claim a
     running row whose lease ran out; attempts + 1 and a lease. No row:
     RLS shows none (another hospital's id, or none) → not_found, Term and
     a dead letter, nothing written (F27); a terminal row → ack; a running
     row with a live lease → Nak until the lease ends (F1: a copy killed
     mid-job is then re-claimed, never acked away). Over the attempts cap:
     failed, Term, a dead letter.
  3. Stages on worker threads (a bounded executor: the job's slot is held
     until each thread really ends, T9 minor), each bounded by the token's
     exp: masking, retrieval and generation (one call, departure 9),
     verification. While a stage runs and between stages: +WIP every 20 s
     with a fresh lease. After each stage a checkpoint writes progress and
     the stage, only while this claim owns the row; a row that is
     cancelled, failed or done is acked and left alone, one re-claimed by
     another delivery is left to it.
  4. Complete in one transaction (jobs_repo.complete): the decision (unique
     query_id), the evaluation, the query.completed outbox fact, status
     completed, only while this claim owns the row (the check before
     writing). Then ack. A unique violation: another delivery finished it,
     ack.
  5. A failure: retryable (unavailable, internal) before the last delivery
     → end the lease and Nak with 10 s, 30 s, 2 min, 5 min by delivery.
     Permanent, or the last delivery → failed with the code and a
     query.failed fact, Term and a dead letter.
  6. Past exp mid-run: failed / deadline_exceeded with query.failed, Term.
     The abandoned stage's result is never written.

The patient id for retrieval comes from the claimed row; none exists yet
(departure 7), so retrieval sees shared guidelines only. Logs carry ids,
codes and counts only: never the question, the patient data or the answer.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
import time
import uuid
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from intelligence.boundary import jobs_repo, kb_repo
from intelligence.boundary.db import is_database_error, is_unique_violation
from intelligence.boundary.jobs import JobContext
from intelligence.boundary.models import (
    DECISIONS,
    Generation,
    ModelClient,
    ModelError,
    Verification,
    trust_band,
)
from intelligence.boundary.pb import envelope_pb2, pipeline_pb2
from intelligence.boundary.verify import Verified

if TYPE_CHECKING:
    from nats.aio.msg import Msg

    from intelligence.boundary.db import Database

log = logging.getLogger("intelligence.boundary.run_query")

TYPE = "run_query"
RETRYABLE = frozenset({"unavailable", "internal", "overloaded"})
MAX_DEPTH = 16


@dataclass(frozen=True)
class JobSettings:
    """The job's timings (spec §9.2). Tests shorten them; production uses
    the defaults."""

    lease_s: float = 90.0  # the claim's lease, renewed with each +WIP
    wip_every: float = 20.0  # +WIP at least this often while a job runs
    tick: float = 1.0  # how often a running stage looks at the clock
    nak_delays: tuple[float, ...] = (10.0, 30.0, 120.0, 300.0)  # by delivery
    max_deliver: int = 5  # the pipeline consumer's MaxDeliver
    max_attempts: int = 5  # claims of one row, across messages


class _Stop(Exception):
    """Stop the job: ack (the row is terminal) or leave the message alone
    (another delivery owns the row)."""

    def __init__(self, ack: bool) -> None:
        super().__init__("stop")
        self.ack = ack


class _Deadline(Exception):
    """The token's exp passed."""


class _NotOurs(Exception):
    """The completion found the row no longer ours (rolled back)."""


@dataclass
class _Job:
    v: Verified = field(repr=False)
    msg: Msg = field(repr=False)
    ctx: JobContext = field(repr=False)
    delivered: int
    qid: str = ""
    attempts: int = 0
    last_wip: float = 0.0
    committed: bool = False
    abandoned: bool = False
    settled: bool = False


def _delivered(msg: Msg) -> int:
    try:
        return max(1, int(msg.metadata.num_delivered))
    except Exception:
        return 1


def _retrieve(fut: asyncio.Future[Any]) -> None:
    # An abandoned stage may still end in an exception: read it, so asyncio
    # doesn't log it (its text could carry model input).
    if not fut.cancelled():
        fut.exception()


def _mask_tree(mask: Callable[[str], str], value: Any, depth: int = 0) -> Any:
    """Every string in patient data, masked; anything too deep dropped."""
    if depth > MAX_DEPTH:
        return None
    if isinstance(value, str):
        return mask(value)
    if isinstance(value, dict):
        return {str(k): _mask_tree(mask, v, depth + 1) for k, v in value.items()}
    if isinstance(value, list):
        return [_mask_tree(mask, v, depth + 1) for v in value]
    if isinstance(value, int | float | bool) or value is None:
        return value
    return None


class RunQuery:
    """The pipeline's run_query handler. One per worker process."""

    def __init__(
        self,
        db: Database | None,
        models: ModelClient,
        settings: JobSettings | None = None,
        *,
        clock: Callable[[], float] = time.time,
        wip_clock: Callable[[], float] = time.monotonic,
        after_commit: Callable[[], None] | None = None,
        max_concurrency: int = 4,
    ) -> None:
        self._db = db
        self._models = models
        self._s = settings or JobSettings()
        self._clock = clock
        self._wip_clock = wip_clock
        self._after_commit = after_commit  # a test seam: a crash after the commit
        # The worker's job slots bound the work; the executor is sized to
        # match, so a stage never waits for a thread behind abandoned ones
        # the slots already count.
        self._executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=max(1, max_concurrency), thread_name_prefix="run_query"
        )

    def close(self) -> None:
        self._executor.shutdown(wait=False, cancel_futures=True)

    async def ready(self) -> bool:
        """Jobs may be taken: there is a database and it answers. Anything
        else (no database, an error) is not ready: fail closed."""
        if self._db is None:
            return False
        try:
            return (await self._db.ready()) is True
        except Exception:
            return False

    # --- the job ------------------------------------------------------------------

    async def handle(self, verified: Verified, msg: Msg, ctx: JobContext) -> None:
        """Run one delivery of a run_query job to exactly one outcome: ack,
        Nak, Term, or (after a crash past the commit) nothing, which the
        redelivery acks. Never raises but for cancellation."""
        job = _Job(v=verified, msg=msg, ctx=ctx, delivered=_delivered(msg))
        try:
            await self._run(job)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.error(
                "run_query failed query_id=%s attempts=%d error=%s committed=%s",
                job.qid or "-",
                job.attempts,
                type(e).__name__,
                job.committed,
            )
            if job.committed or job.settled:
                return  # the redelivery finds the row completed and acks
            try:
                await self._failure(job, "unavailable" if is_database_error(e) else "internal")
            except Exception as e2:
                log.error("run_query failure handling failed error=%s", type(e2).__name__)

    async def _run(self, job: _Job) -> None:
        v = job.v
        message = v.message
        if not isinstance(message, pipeline_pb2.RunQueryJob):
            await self._permanent(job, "invalid_request")
            return
        try:
            job.qid = str(uuid.UUID(message.query_id))
        except ValueError:
            await self._permanent(job, "invalid_request")
            return
        if self._db is None:
            await self._failure(job, "unavailable")  # never claims without a database
            return
        if self._clock() >= v.exp:
            await self._deadline(job)
            return
        claim = await self._claim(job)
        if claim is None:
            return
        try:
            await self._pipeline(job, claim)
        except _Stop as s:
            if s.ack:
                await self._settle(job, "ack")
        except _Deadline:
            await self._deadline(job)
        except ModelError as e:
            log.warning(
                "run_query model failed query_id=%s attempts=%d code=%s",
                job.qid,
                job.attempts,
                e.code,
            )
            await self._failure(job, e.code)

    async def _claim(self, job: _Job) -> jobs_repo.Claim | None:
        assert self._db is not None
        try:
            async with self._db.with_hospital(job.v) as cur:
                claim = await jobs_repo.claim(cur, job.qid, self._s.lease_s)
                state = None if claim is not None else await jobs_repo.state(cur, job.qid)
        except Exception as e:
            if not is_database_error(e):
                raise
            log.warning("run_query claim failed query_id=%s error=%s", job.qid, type(e).__name__)
            await self._failure(job, "unavailable")
            return None
        if claim is not None:
            job.attempts = claim.attempts
            job.last_wip = self._wip_clock()
            if claim.attempts > self._s.max_attempts:
                log.warning("run_query over the attempts cap query_id=%s", job.qid)
                await self._give_up(job, "internal")
                return None
            log.info("run_query claimed query_id=%s attempts=%d", job.qid, claim.attempts)
            return claim
        if state is None:
            # Not this hospital's (or no such) query: RLS shows nothing (F27).
            log.warning("run_query not found query_id=%s", job.qid)
            await self._permanent(job, "not_found")
        elif state.status in jobs_repo.TERMINAL:
            await self._settle(job, "ack")  # done, failed or cancelled already
        elif state.status == "running":
            # Another copy holds a live lease (F1): come back when it ends,
            # so a copy killed mid-job is re-claimed rather than lost.
            await self._settle(job, "nak", state.lease_left + 1.0)
        else:
            await self._settle(job, "nak", 1.0)  # queued: raced a claim
        return None

    async def _pipeline(self, job: _Job, claim: jobs_repo.Claim) -> None:
        patient = claim.patient_data if isinstance(claim.patient_data, dict) else {}
        question, context = await self._stage(job, self._mask_all, claim.question, patient)
        await self._checkpoint(job, 0.25, "generation")
        chunks = await self._chunks(job)
        generation = await self._stage(job, self._models.generate, question, context, None, chunks)
        await self._checkpoint(job, 0.6, "hga_verification")
        verification = await self._stage(job, self._models.verify, generation)
        await self._checkpoint(job, 0.9, "decision")
        await self._complete(job, self._decision(generation, verification))

    def _mask_all(self, question: str, patient: Mapping[str, Any]) -> tuple[str, dict[str, Any]]:
        masked = _mask_tree(self._models.mask, dict(patient))
        return self._models.mask(question), masked if isinstance(masked, dict) else {}

    async def _chunks(self, job: _Job) -> list[Any]:
        assert self._db is not None
        # Departure 7: no patient id on the row yet, so None: shared
        # guidelines only. The id would come from the claimed row, never
        # from the message.
        async with self._db.with_hospital(job.v) as cur:
            return await kb_repo.load_chunks(cur, None)

    # --- stages, heartbeats and checkpoints ---------------------------------------

    async def _stage(self, job: _Job, fn: Callable[..., Any], *args: Any) -> Any:
        """fn(*args) on a worker thread, until the token's exp. The slot
        stays held until the thread ends, even if we stop waiting."""
        loop = asyncio.get_running_loop()
        remaining = job.v.exp - self._clock()
        if remaining <= 0:
            raise _Deadline
        cfut = self._executor.submit(fn, *args)
        job.ctx.slot.hold(cfut)
        afut = asyncio.wrap_future(cfut)
        afut.add_done_callback(_retrieve)
        end = loop.time() + remaining
        while True:
            left = end - loop.time()
            if left <= 0:
                job.abandoned = True  # its result, if it ever comes, is dropped
                log.warning("run_query deadline query_id=%s attempts=%d", job.qid, job.attempts)
                raise _Deadline
            done, _ = await asyncio.wait({afut}, timeout=min(self._s.tick, left))
            if done:
                return afut.result()
            await self._heartbeat(job)

    async def _heartbeat(self, job: _Job) -> None:
        """+WIP and a fresh lease, every wip_every seconds."""
        now = self._wip_clock()
        if now - job.last_wip < self._s.wip_every:
            return
        job.last_wip = now
        try:
            await job.msg.in_progress()
        except Exception as e:
            log.warning("run_query +WIP failed query_id=%s error=%s", job.qid, type(e).__name__)
        assert self._db is not None
        try:
            async with self._db.with_hospital(job.v) as cur:
                ours = await jobs_repo.extend_lease(cur, job.qid, job.attempts, self._s.lease_s)
        except Exception as e:
            if not is_database_error(e):
                raise
            log.warning("run_query lease failed query_id=%s error=%s", job.qid, type(e).__name__)
            return  # the next checkpoint decides
        if not ours:
            await self._lost(job)

    async def _checkpoint(self, job: _Job, progress: float, stage: str) -> None:
        """Between stages (spec §9.4): still ours, not cancelled? Progress,
        the stage and a fresh lease in one guarded update."""
        if self._clock() >= job.v.exp:
            raise _Deadline
        await self._heartbeat(job)
        assert self._db is not None
        async with self._db.with_hospital(job.v) as cur:
            ours = await jobs_repo.checkpoint(
                cur, job.qid, job.attempts, progress, stage, self._s.lease_s
            )
        if not ours:
            await self._lost(job)

    async def _lost(self, job: _Job) -> None:
        """The claim no longer owns the row. Terminal (cancelled, failed by
        the sweeper, completed): ack. Re-claimed by another delivery: leave
        the message to it. Always raises _Stop."""
        assert self._db is not None
        try:
            async with self._db.with_hospital(job.v) as cur:
                state = await jobs_repo.state(cur, job.qid)
        except Exception as e:
            if not is_database_error(e):
                raise
            raise _Stop(ack=False) from None
        status = state.status if state is not None else "gone"
        log.info(
            "run_query stopped query_id=%s attempts=%d status=%s", job.qid, job.attempts, status
        )
        raise _Stop(ack=state is None or state.status in jobs_repo.TERMINAL)

    # --- outcomes -------------------------------------------------------------------

    def _decision(self, g: Generation, v: Verification) -> jobs_repo.Decision:
        """The row to write. Fail closed: anything unverified, or without a
        score, is ESCALATE and degraded; no score is invented."""
        failed = tuple(dict.fromkeys((*g.failed_agents, *v.failed_agents)))
        verified = bool(v.verified and g.available and v.trust_score is not None)
        decision = v.decision if v.decision in DECISIONS else "ESCALATE"
        if not verified:
            decision = "ESCALATE"
        try:
            versions = dict(self._models.model_versions())
        except Exception:
            versions = {}
        return jobs_repo.Decision(
            response_text=g.response if g.available else "",
            citations=g.cited_chunk_ids,
            trust_score=v.trust_score if verified else None,
            decision=decision,
            trust_band=trust_band(v) if verified else None,
            verified=verified,
            degraded=bool(v.degraded or failed or not verified),
            failed_agents=failed,
            model_versions=versions,
            guardrail_report={
                "failed_agents": list(failed),
                "flags": [list(f) for f in v.flags],
                "model_used": g.model_used,
            },
            retrieval_ms=g.retrieval_ms,
            generation_ms=g.generation_ms,
            scores=dict(v.scores),
            flag_penalty=v.flag_penalty,
            weights=dict(v.weights),
            flags=v.flags,
        )

    async def _complete(self, job: _Job, d: jobs_repo.Decision) -> None:
        if job.abandoned:  # belt and braces: an abandoned run never writes
            return
        if self._clock() >= job.v.exp:
            raise _Deadline
        assert self._db is not None
        try:
            async with self._db.with_hospital(job.v) as cur:
                if not await jobs_repo.complete(cur, job.qid, job.attempts, d):
                    raise _NotOurs
        except _NotOurs:
            await self._lost(job)  # cancelled just before the write, or re-claimed
            return
        except Exception as e:
            if not is_unique_violation(e):
                raise
            log.info("run_query already completed query_id=%s", job.qid)
            await self._settle(job, "ack")
            return
        job.committed = True
        log.info(
            "run_query completed query_id=%s attempts=%d decision=%s",
            job.qid,
            job.attempts,
            d.decision,
        )
        if self._after_commit is not None:
            self._after_commit()
        await self._settle(job, "ack")

    async def _failure(self, job: _Job, code: str) -> None:
        """Retry (Nak with the schedule) or, permanent or last, give up."""
        last = job.delivered >= self._s.max_deliver or job.attempts >= self._s.max_attempts
        if code in RETRYABLE and not last:
            if job.attempts and self._db is not None:
                try:
                    async with self._db.with_hospital(job.v) as cur:
                        await jobs_repo.release_lease(cur, job.qid, job.attempts)
                except Exception as e:
                    if not is_database_error(e):
                        raise
            delays = self._s.nak_delays
            delay = delays[min(job.delivered, len(delays)) - 1] if delays else 0.0
            await self._settle(job, "nak", delay)
            return
        await self._give_up(job, code)

    async def _give_up(self, job: _Job, code: str) -> None:
        """failed with code and query.failed (if this claim owns the row),
        Term, and a dead letter."""
        await self._mark_failed(job, code)
        await self._dead_letter(job, code)
        await self._settle(job, "term")

    async def _permanent(self, job: _Job, code: str) -> None:
        """No claim was made: Term and a dead letter; core marks the row."""
        await self._dead_letter(job, code)
        await self._settle(job, "term")

    async def _deadline(self, job: _Job) -> None:
        """Past exp: failed / deadline_exceeded and Term (spec §9.2). If the
        row couldn't be marked here, a dead letter lets core do it."""
        written = await self._mark_failed(job, "deadline_exceeded")
        if not written:
            await self._dead_letter(job, "deadline_exceeded")
        await self._settle(job, "term")

    async def _mark_failed(self, job: _Job, code: str) -> bool:
        if not job.attempts or self._db is None:
            return False
        try:
            async with self._db.with_hospital(job.v) as cur:
                written = await jobs_repo.fail(cur, job.qid, job.attempts, code)
        except Exception as e:
            if not is_database_error(e):
                raise
            log.warning(
                "run_query fail write failed query_id=%s error=%s", job.qid, type(e).__name__
            )
            return False
        log.info(
            "run_query failed query_id=%s attempts=%d code=%s written=%s",
            job.qid,
            job.attempts,
            code,
            written,
        )
        return written

    async def _dead_letter(self, job: _Job, code: str) -> None:
        await job.ctx.dead_letter(
            envelope_pb2.DeadLetter(
                job_id=job.qid,
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
                "run_query %s failed query_id=%s error=%s", how, job.qid or "-", type(e).__name__
            )
