"""The approve_correction job against live NATS 2.10 (JetStream, the real
nats.conf) and Postgres 16 migrated by core (Plan 4 spec §4.7, §4.9,
§10.5, §11.5). Marked `boundary`: needs Docker. The learning client is the
stub; no model runs.

Core is played by the test: it seeds a correction approved by a second
clinician (through the database's own triggers, as core's transaction
does), inserts the jobs row and publishes jobs.v1.<hid>.approve_correction
with the test-only signer's token. Every id, name and text here is
synthetic.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable, Iterator
from contextlib import asynccontextmanager, contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import nats
import psycopg
import pytest
from nats.aio.client import Client

from intelligence.boundary import envelope
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.db import Database
from intelligence.boundary.jobs.approve_correction import ApproveCorrection
from intelligence.boundary.jobs.run_query import JobSettings, RunQuery
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.learning import LearningClient, StubLearningClient
from intelligence.boundary.models import StubModelClient
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import envelope_pb2, learning_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import pipeline
from intelligence.boundary.verify import Verifier
from intelligence.boundary.worker import Worker
from tests.boundary.conftest import (
    INTELLIGENCE_DB_PASSWORD,
    INTELLIGENCE_DB_USER,
    NatsServer,
    Postgres,
)
from tests.boundary.test_run_query import (
    FAST_NAKS,
    HOSPITAL_A,
    HOSPITAL_B,
    MARKER,
    _pg_log,
    _statements,
    ensure_streams,
    seed_query,
    su,
    until,
)
from tests.boundary.tokens import RID, Signer

pytestmark = pytest.mark.boundary

SCHEMA = "mhai.intelligence.v1.ApproveCorrectionJob"
CONSUMER = "pipeline_learning"


# --- core's side, played by the test ----------------------------------------------


@dataclass(frozen=True)
class Correction:
    hid: str
    query_id: str
    correction_id: str
    job_id: str
    submitter: str
    reviewer: str


async def _user(conn: psycopg.AsyncConnection[Any], hid: str) -> str:
    uid = str(uuid.uuid4())
    await conn.execute(
        "INSERT INTO users (id, hospital_id, email, display_name) VALUES (%s, %s, %s, %s)",
        (uid, hid, f"syn-{uid}@example.test", "Synthetic Clinician"),
    )
    await conn.execute("INSERT INTO user_roles (user_id, role) VALUES (%s, 'clinician')", (uid,))
    return uid


async def seed_correction(
    pg: Postgres, hid: str = HOSPITAL_A, *, approve: bool = True, job: bool = True
) -> Correction:
    """A completed question of hid with its decision and evaluation, a
    correction by one clinician approved by another (the decider and
    transition triggers run, as in core's transaction), and its queued
    jobs row. The free text carries the synthetic marker."""
    qid = await seed_query(pg, hid=hid)
    cid, jid = str(uuid.uuid4()), str(uuid.uuid4())
    async with await su(pg) as conn:
        submitter = await _user(conn, hid)
        reviewer = await _user(conn, hid)
        await conn.execute(
            "INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text, "
            "hga_decision, model_versions, masked_question) "
            "VALUES (%s, %s, %s, %s, 'REGENERATE', %s, %s)",
            (
                str(uuid.uuid4()),
                hid,
                qid,
                f"Synthetic answer {MARKER}",
                json.dumps({"stub": "1"}),
                "Synthetic masked question.",
            ),
        )
        await conn.execute(
            "INSERT INTO hga_evaluations (hospital_id, query_id, target_agent, trust, decision, "
            "nli_score, coherence_score, numerical_score, se_score, ts) "
            "VALUES (%s, %s, 'CCA', 0.2, 'ESCALATE', 0.1, 0.1, 0.1, 0.1, now() - interval '1 h')",
            (hid, qid),
        )
        await conn.execute(
            "INSERT INTO hga_evaluations (hospital_id, query_id, target_agent, trust, decision, "
            "nli_score, coherence_score, numerical_score, se_score) "
            "VALUES (%s, %s, 'CCA', 0.8, 'REGENERATE', 0.9, 0.7, 1.0, 0.6)",
            (hid, qid),
        )
        await conn.execute(
            "INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, "
            "status, reason, corrected_diagnosis, request_id) "
            "VALUES (%s, %s, %s, %s, 'correct', 'pending_review', %s, %s, %s)",
            (
                cid,
                hid,
                qid,
                submitter,
                f"Synthetic reason {MARKER}",
                f"Synthetic diagnosis {MARKER}",
                RID,
            ),
        )
        if approve:
            async with conn.transaction():
                await conn.execute("SELECT set_config('mhai.user_id', %s, true)", (reviewer,))
                await conn.execute(
                    "UPDATE feedback_events SET status = 'approved', decided_by = %s, "
                    "decided_at = now() WHERE id = %s",
                    (reviewer, cid),
                )
        if job:
            await insert_job(conn, hid, jid, cid, reviewer)
    return Correction(hid, qid, cid, jid, submitter, reviewer)


async def insert_job(
    conn: psycopg.AsyncConnection[Any], hid: str, jid: str, cid: str, creator: str
) -> None:
    await conn.execute(
        "INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, request_id, "
        "deadline_at) VALUES (%s, %s, 'approve_correction', %s, %s, %s, "
        "now() + interval '5 minutes')",
        (jid, hid, cid, creator, RID),
    )


async def force_pending(pg: Postgres, cid: str) -> None:
    """As the superuser, around the transition trigger (it would refuse):
    the approved correction back to pending_review."""
    async with await su(pg) as conn, conn.transaction():
        await conn.execute("SET LOCAL session_replication_role = replica")
        await conn.execute(
            "UPDATE feedback_events SET status = 'pending_review', decided_by = NULL, "
            "decided_at = NULL WHERE id = %s",
            (cid,),
        )


async def job_row(pg: Postgres, jid: str) -> dict[str, Any]:
    async with await su(pg) as conn:
        cur = await conn.execute(
            "SELECT status, error, attempts, lease_until, completed_at, failed_at "
            "FROM jobs WHERE id = %s",
            (jid,),
        )
        r = await cur.fetchone()
    assert r is not None
    keys = ("status", "error", "attempts", "lease_until", "completed_at", "failed_at")
    return dict(zip(keys, r, strict=True))


async def buffer_rows(pg: Postgres, cid: str) -> list[tuple[Any, ...]]:
    async with await su(pg) as conn:
        cur = await conn.execute(
            "SELECT hospital_id::text, nli_score, coherence_score, numerical_score, se_score, "
            "system_decision, model_versions, cleared_at, reset_id FROM feedback_buffer "
            "WHERE correction_id = %s",
            (cid,),
        )
        return list(await cur.fetchall())


async def facts(pg: Postgres, jid: str) -> list[tuple[Any, ...]]:
    async with await su(pg) as conn:
        cur = await conn.execute(
            "SELECT action, hospital_id::text, query_id, entity, entity_id, payload "
            "FROM intelligence_audit_outbox WHERE job_id = %s ORDER BY id",
            (jid,),
        )
        return list(await cur.fetchall())


async def correction_status(pg: Postgres, cid: str) -> str:
    async with await su(pg) as conn:
        cur = await conn.execute("SELECT status FROM feedback_events WHERE id = %s", (cid,))
        r = await cur.fetchone()
    assert r is not None
    return str(r[0])


async def job_is(pg: Postgres, jid: str, *want: str) -> bool:
    return (await job_row(pg, jid))["status"] in want


@dataclass
class Env:
    server: NatsServer
    core: Client
    js: Any
    keys_file: Path
    signer: Signer
    db: Database
    workers: list[Worker] = field(default_factory=list)
    _dead: Any = None

    def job(self, c: Correction, hid: str | None = None) -> tuple[str, dict[str, str], bytes]:
        hid = hid or c.hid
        body = learning_pb2.ApproveCorrectionJob(
            job_id=c.job_id, correction_id=c.correction_id
        ).SerializeToString(deterministic=True)
        subject = f"jobs.v1.{hid}.approve_correction"
        tok = self.signer.mint_job(
            body,
            hid=hid,
            sub=c.reviewer,
            nsub=subject,
            jti=uuid.uuid4().hex,
            iat=int(time.time()),
            ttl=300,
        )
        return (
            subject,
            {envelope.TOKEN: tok, envelope.SCHEMA: SCHEMA, envelope.REQUEST_ID: RID},
            body,
        )

    async def publish(
        self, c: Correction, hid: str | None = None, *, msg_id: str | None = ""
    ) -> tuple[str, dict[str, str], bytes]:
        subject, headers, body = self.job(c, hid)
        await self.send(subject, headers, body, msg_id=c.job_id if msg_id == "" else msg_id)
        return subject, headers, body

    async def send(
        self, subject: str, headers: dict[str, str], body: bytes, *, msg_id: str | None
    ) -> None:
        h = dict(headers)
        if msg_id:
            h["Nats-Msg-Id"] = msg_id
        await self.js.publish(subject, body, headers=h, stream="JOBS")

    async def dead_letters(self, want: int = 1, timeout: float = 10.0) -> list[Any]:
        if self._dead is None:
            self._dead = await self.js.pull_subscribe_bind(durable="core_dead", stream="DEAD")
        out: list[Any] = []
        end = time.monotonic() + timeout
        while len(out) < want and time.monotonic() < end:
            try:
                msgs = await self._dead.fetch(want - len(out), timeout=0.5)
            except nats.errors.TimeoutError:
                continue
            for m in msgs:
                d = envelope_pb2.DeadLetter()
                d.ParseFromString(m.data)
                out.append((m.subject, d))
                await m.ack()
        return out

    async def consumer(self) -> Any:
        return await self.js.consumer_info("JOBS", CONSUMER)

    async def settled(self, timeout: float = 10.0) -> bool:
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            ci = await self.consumer()
            if ci.num_ack_pending == 0 and ci.num_pending == 0:
                return True
            await asyncio.sleep(0.1)
        return False

    async def start(
        self,
        learning: LearningClient,
        *,
        after_commit: Callable[[], None] | None = None,
        with_run_query: bool = False,
    ) -> ApproveCorrection:
        ws = WorkerSettings(
            service="pipeline",
            nats_url=self.server.url("pipeline"),
            trusted_keys_path=str(self.keys_file),
            max_concurrency=2,
            development=True,
        )
        verifier = Verifier(
            TrustedKeys.from_encoded(self.signer.encoded_keys()),
            "pipeline",
            Operations.load(development=True),
            JtiSet(time.time),
            time.time,
        )
        settings = JobSettings(tick=0.05, nak_delays=FAST_NAKS)
        runner = ApproveCorrection(
            self.db, learning, settings, clock=time.time, after_commit=after_commit
        )
        jobs = {"approve_correction": runner.handle}
        if with_run_query:
            rq = RunQuery(self.db, StubModelClient(), settings, max_concurrency=2)
            jobs["run_query"] = rq.handle
        w = Worker(ws, pipeline.handlers(), verifier, jobs=jobs, jobs_ready=runner.ready)
        w.fetch_timeout = 0.5
        await w.start()
        self.workers.append(w)
        return runner


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


MakeEnv = Callable[..., Awaitable[Env]]


@pytest.fixture
async def make_env(
    fresh_nats: NatsServer,
    database: Database,
    tmp_path: Path,
    signer: Signer,
) -> AsyncIterator[MakeEnv]:
    envs: list[Env] = []
    keys_file = tmp_path / "trusted.json"
    keys_file.write_text(json.dumps({"keys": signer.encoded_keys()}))

    async def make(ack_wait: float = 30.0, max_deliver: int = 5) -> Env:
        core = await nats.connect(
            fresh_nats.url("gateway"),
            inbox_prefix="_INBOX_gateway",
            allow_reconnect=False,
            connect_timeout=5,
        )
        js = core.jetstream()
        await ensure_streams(js, ack_wait, max_deliver)
        e = Env(fresh_nats, core, js, keys_file, signer, database)
        envs.append(e)
        return e

    yield make
    for e in envs:
        for w in e.workers:
            await w.abort()
        await e.core.close()


@contextmanager
def statement_log(pg: Postgres) -> Iterator[list[int]]:
    """Postgres logs every statement while the block runs; yields [mark],
    the log length before it, so the caller reads only the block's lines."""
    with psycopg.connect(pg.url, autocommit=True) as conn:
        conn.execute("ALTER SYSTEM SET log_statement = 'all'")
        conn.execute("ALTER SYSTEM SET log_line_prefix = 'pid=%p '")
        conn.execute("SELECT pg_reload_conf()")
    time.sleep(0.5)
    mark = [len(_pg_log(pg))]
    try:
        yield mark
    finally:
        with psycopg.connect(pg.url, autocommit=True) as conn:
            conn.execute("ALTER SYSTEM RESET log_statement")
            conn.execute("ALTER SYSTEM RESET log_line_prefix")
            conn.execute("SELECT pg_reload_conf()")


@asynccontextmanager
async def as_worker(pg: Postgres, hid: str) -> AsyncIterator[psycopg.AsyncConnection[Any]]:
    """A transaction as mhai_worker with hid set, the way with_hospital sets
    it, but from the superuser's login (for statements the job never runs)."""
    async with await psycopg.AsyncConnection.connect(pg.url) as conn:
        async with conn.transaction():
            await conn.execute("SET LOCAL ROLE mhai_worker")
            await conn.execute("SELECT set_config('mhai.hospital_id', %s, true)", (hid,))
            yield conn


# --- the tests ---------------------------------------------------------------------


async def test_approved_correction_writes_one_buffer_row(
    make_env: MakeEnv, postgres: Postgres, caplog: pytest.LogCaptureFixture
) -> None:
    """The happy path, with run_query's consumer bound beside it: one
    buffer row of the correction's hospital (the latest evaluation's
    scores, the decision's system decision and model versions), one
    correction.applied fact against the correction with persona_update
    skipped, the job completed and acked. The correction stays approved."""
    caplog.set_level(logging.DEBUG)
    env = await make_env()
    c = await seed_correction(postgres)
    stub = StubLearningClient()
    await env.start(stub, with_run_query=True)
    await env.publish(c)
    assert await until(lambda: job_is(postgres, c.job_id, "completed"))
    j = await job_row(postgres, c.job_id)
    assert (j["attempts"], j["error"], j["lease_until"]) == (1, None, None)
    assert j["completed_at"] is not None and j["failed_at"] is None
    rows = await buffer_rows(postgres, c.correction_id)
    assert len(rows) == 1
    hid, nli, coh, num, se, decision, versions, cleared, reset = rows[0]
    assert hid == HOSPITAL_A
    assert (nli, coh, num, se) == pytest.approx((0.9, 0.7, 1.0, 0.6))
    assert (decision, versions, cleared, reset) == ("REGENERATE", {"stub": "1"}, None, None)
    out = await facts(postgres, c.job_id)
    assert len(out) == 1
    action, fhid, query_id, entity, entity_id, payload = out[0]
    assert (action, fhid, query_id) == ("correction.applied", HOSPITAL_A, None)
    assert (entity, entity_id) == ("correction", c.correction_id)
    assert payload == {
        "correction_id": c.correction_id,
        "attempts": 1,
        "model_versions": {"stub_learning": "1"},
        "persona_update": "skipped",
    }
    assert stub.calls["buffer_entry"] == 1 and stub.calls["persona_update"] == 1
    assert await correction_status(postgres, c.correction_id) == "approved"
    assert await env.settled()
    assert MARKER not in caplog.text


async def test_redelivery_after_completion_acks_without_second_row(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A4's twin: the same job again after completion is acked; still one
    buffer row, one fact, one claim."""
    env = await make_env()
    c = await seed_correction(postgres)
    stub = StubLearningClient()
    await env.start(stub)
    subject, headers, body = await env.publish(c)
    assert await until(lambda: job_is(postgres, c.job_id, "completed"))
    assert await env.settled()
    await env.send(subject, headers, body, msg_id=None)  # past the dedupe: a new delivery
    assert await env.settled()
    await asyncio.sleep(0.3)
    assert stub.calls["buffer_entry"] == 1
    assert len(await buffer_rows(postgres, c.correction_id)) == 1
    assert [f[0] for f in await facts(postgres, c.job_id)] == ["correction.applied"]
    assert (await job_row(postgres, c.job_id))["attempts"] == 1


async def test_second_job_for_applied_correction_adds_no_row(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A second jobs row for a correction already in the buffer (23505 on
    correction_id): already applied. Its insert alone rolls back; the job
    completes; still one buffer row."""
    env = await make_env()
    c = await seed_correction(postgres)
    await env.start(StubLearningClient())
    await env.publish(c)
    assert await until(lambda: job_is(postgres, c.job_id, "completed"))
    again = Correction(
        c.hid, c.query_id, c.correction_id, str(uuid.uuid4()), c.submitter, c.reviewer
    )
    async with await su(postgres) as conn:
        await insert_job(conn, c.hid, again.job_id, c.correction_id, c.reviewer)
    await env.publish(again)
    assert await until(lambda: job_is(postgres, again.job_id, "completed"))
    assert await env.settled()
    assert len(await buffer_rows(postgres, c.correction_id)) == 1
    assert [f[0] for f in await facts(postgres, again.job_id)] == ["correction.applied"]


async def test_commit_then_crash_still_audited(make_env: MakeEnv, postgres: Postgres) -> None:
    """A13's twin (C23, worker side): the worker dies right after the
    commit, before the ack. The fact is there with the row; the redelivery
    acks without a second row."""
    env = await make_env(ack_wait=2)
    c = await seed_correction(postgres)
    crashed: list[int] = []

    def crash() -> None:
        if not crashed:
            crashed.append(1)
            raise RuntimeError("synthetic crash between commit and ack")

    stub = StubLearningClient()
    await env.start(stub, after_commit=crash)
    await env.publish(c)
    assert await until(lambda: job_is(postgres, c.job_id, "completed"))
    assert [f[0] for f in await facts(postgres, c.job_id)] == ["correction.applied"]
    assert crashed == [1]
    assert await env.settled(timeout=15)  # the redelivery is acked
    ci = await env.consumer()
    assert ci.num_redelivered >= 1 or ci.delivered.consumer_seq >= 2
    assert stub.calls["buffer_entry"] == 1
    assert len(await buffer_rows(postgres, c.correction_id)) == 1
    assert len(await facts(postgres, c.job_id)) == 1


async def test_poison_job_dead_letters_after_five(make_env: MakeEnv, postgres: Postgres) -> None:
    """A20's twin: a learning client that always fails. Five deliveries
    (each rolled back, nothing half-written), then failed internal with
    correction.apply_failed, Term, one dead letter. The correction stays
    approved."""
    env = await make_env()
    c = await seed_correction(postgres)
    stub = StubLearningClient(raises={"buffer_entry": RuntimeError("synthetic")})
    await env.start(stub)
    await env.publish(c)
    dead = await env.dead_letters(1, timeout=20)
    assert len(dead) == 1
    subject, d = dead[0]
    assert subject == f"dead.v1.{HOSPITAL_A}.approve_correction"
    assert (d.job_id, d.type, d.code, d.reason, d.attempts) == (
        c.job_id,
        "approve_correction",
        "internal",
        "internal",
        5,
    )
    assert stub.calls["buffer_entry"] == 5
    j = await job_row(postgres, c.job_id)
    assert (j["status"], j["error"], j["attempts"]) == ("failed", "internal", 5)
    assert j["failed_at"] is not None
    out = await facts(postgres, c.job_id)
    assert [(f[0], f[3], f[4], f[5]) for f in out] == [
        (
            "correction.apply_failed",
            "correction",
            c.correction_id,
            {"correction_id": c.correction_id, "error": "internal", "attempts": 5},
        )
    ]
    assert await buffer_rows(postgres, c.correction_id) == []
    assert await correction_status(postgres, c.correction_id) == "approved"
    assert await env.settled()
    await asyncio.sleep(0.5)
    assert stub.calls["buffer_entry"] == 5
    assert len(await env.dead_letters(1, timeout=1)) == 0


async def test_foreign_correction_is_not_found_and_reads_nothing(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """C9: a job for hospital A naming hospital B's job and correction:
    not_found, Term and one dead letter; no buffer row, B's job untouched.
    Postgres logs only the claim for those ids, as mhai_worker with A."""
    env = await make_env()
    b = await seed_correction(postgres, hid=HOSPITAL_B)
    before = await job_row(postgres, b.job_id)
    stub = StubLearningClient()
    with statement_log(postgres) as mark:
        await env.start(stub)
        await env.publish(b, hid=HOSPITAL_A)
        dead = await env.dead_letters(1)
        await asyncio.sleep(0.3)
        lines = _pg_log(postgres)[mark[0] :]
    assert len(dead) == 1
    subject, d = dead[0]
    assert subject == f"dead.v1.{HOSPITAL_A}.approve_correction"
    assert (d.job_id, d.code, d.subject_hospital, d.attempts) == (
        b.job_id,
        "not_found",
        HOSPITAL_A,
        1,
    )
    assert await job_row(postgres, b.job_id) == before
    assert await buffer_rows(postgres, b.correction_id) == []
    assert await facts(postgres, b.job_id) == []
    assert stub.calls == {}
    assert await env.settled()

    entries = _statements(lines)
    touching = [e for e in entries if b.job_id in e.params or b.correction_id in e.params]
    assert len(touching) == 1, [e.sql for e in touching]
    (claim,) = touching
    assert claim.sql.startswith("WITH claimed AS ("), claim.sql
    assert claim.hospital == HOSPITAL_A and claim.role_set, claim
    assert not any(b.job_id in e.sql or b.correction_id in e.sql for e in entries)


async def test_unapproved_correction_terminated(make_env: MakeEnv, postgres: Postgres) -> None:
    """C8 (worker side): the correction is forced back to pending_review
    before delivery. Term with invalid_request, a dead letter, failed with
    correction.apply_failed, and no buffer row."""
    env = await make_env()
    c = await seed_correction(postgres)
    await force_pending(postgres, c.correction_id)
    stub = StubLearningClient()
    await env.start(stub)
    await env.publish(c)
    dead = await env.dead_letters(1)
    assert len(dead) == 1
    _, d = dead[0]
    assert (d.job_id, d.code, d.attempts) == (c.job_id, "invalid_request", 1)
    j = await job_row(postgres, c.job_id)
    assert (j["status"], j["error"]) == ("failed", "invalid_request")
    out = await facts(postgres, c.job_id)
    assert [(f[0], f[5]) for f in out] == [
        (
            "correction.apply_failed",
            {"correction_id": c.correction_id, "error": "invalid_request", "attempts": 1},
        )
    ]
    assert await buffer_rows(postgres, c.correction_id) == []
    assert stub.calls["buffer_entry"] == 0
    assert await env.settled()


@pytest.mark.parametrize("state", ["pending_review", "rejected", "self_approved"])
async def test_buffer_needs_approval_refuses_a_direct_insert(
    postgres: Postgres, state: str
) -> None:
    """C8 (database side): past the handler's check, as mhai_worker with
    the right hospital, a buffer row for a correction that is pending, or
    rejected, or approved by its own submitter (forced as superuser) is
    refused by the RESTRICTIVE buffer_needs_approval."""
    c = await seed_correction(postgres, approve=False, job=False)
    async with await su(postgres) as conn:
        if state == "rejected":
            async with conn.transaction():
                await conn.execute("SELECT set_config('mhai.user_id', %s, true)", (c.reviewer,))
                await conn.execute(
                    "UPDATE feedback_events SET status = 'rejected', decided_by = %s, "
                    "decided_at = now(), rejection_code = 'unsafe' WHERE id = %s",
                    (c.reviewer, c.correction_id),
                )
        elif state == "self_approved":
            async with conn.transaction():
                await conn.execute("SET LOCAL session_replication_role = replica")
                await conn.execute(
                    "ALTER TABLE feedback_events DROP CONSTRAINT feedback_not_self_decided"
                )
                await conn.execute(
                    "UPDATE feedback_events SET status = 'approved', decided_by = submitted_by, "
                    "decided_at = now() WHERE id = %s",
                    (c.correction_id,),
                )
                # The CHECK back, NOT VALID: it holds new rows, not this one.
                await conn.execute(
                    "ALTER TABLE feedback_events ADD CONSTRAINT feedback_not_self_decided "
                    "CHECK (decided_by IS NULL OR decided_by <> submitted_by) NOT VALID"
                )
    with pytest.raises(psycopg.errors.InsufficientPrivilege):
        async with as_worker(postgres, HOSPITAL_A) as w:
            await w.execute(
                "INSERT INTO feedback_buffer (hospital_id, correction_id, system_decision) "
                "VALUES (mhai_current_hospital(), %s, 'PASS')",
                (c.correction_id,),
            )
    assert await buffer_rows(postgres, c.correction_id) == []
    if state == "self_approved":
        async with await su(postgres) as conn:
            # Leave the schema as migrated: the forced row goes, the CHECK
            # is validated again.
            await conn.execute("DELETE FROM feedback_events WHERE id = %s", (c.correction_id,))
            await conn.execute(
                "ALTER TABLE feedback_events VALIDATE CONSTRAINT feedback_not_self_decided"
            )


async def test_worker_never_selects_free_text(make_env: MakeEnv, postgres: Postgres) -> None:
    """C16 (worker side): during a whole run, no statement names the
    correction's free text or the masked consultation, and the synthetic
    marker in the free text appears in no statement or parameter."""
    env = await make_env()
    c = await seed_correction(postgres)
    with statement_log(postgres) as mark:
        await env.start(StubLearningClient())
        await env.publish(c)
        assert await until(lambda: job_is(postgres, c.job_id, "completed"))
        assert await env.settled()
        lines = _pg_log(postgres)[mark[0] :]
    entries = _statements(lines)
    worker = [e for e in entries if e.role_set]
    assert any(e.sql.startswith("WITH claimed AS (") for e in worker)
    assert any("feedback_events" in e.sql for e in worker)
    text = "\n".join(lines)
    for column in ("reason", "corrected_diagnosis", "rejection_note", "cancel_note"):
        assert not re.search(rf"\b{column}\b", text), column
    for column in ("masked_question", "masked_patient_json", "response_text"):
        assert column not in "\n".join(e.sql for e in worker), column
    assert MARKER not in text
    assert len(await buffer_rows(postgres, c.correction_id)) == 1
