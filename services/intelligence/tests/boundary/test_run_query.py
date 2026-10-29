"""The run_query job against live NATS 2.10 (JetStream, the real nats.conf)
and Postgres 16 migrated by core (spec §8.2, §9.2-9.4, §12). Marked
`boundary`: needs Docker. Models are the stub client: no weights load.

Core is played by a gateway connection: it makes the JOBS and DEAD streams
and their consumers as bus.EnsureStreams does (ack_wait shortened where a
test waits for a redelivery), publishes jobs with the test-only signer's
tokens, and reads dead letters through core_dead. Every id, question and
password here is synthetic.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import threading
import time
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import nats
import psycopg
import pytest
from nats.aio.client import Client
from nats.js import api

from intelligence.boundary import envelope, jobs_repo
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.db import Database, DatabaseError
from intelligence.boundary.jobs.run_query import JobSettings, RunQuery
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.models import ModelClient, StubModelClient
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import envelope_pb2, pipeline_pb2
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
from tests.boundary.tokens import JOB_SCHEMA, RID, Signer

pytestmark = pytest.mark.boundary

HOSPITAL_A = "a11a11a1-0000-4000-8000-0000000000a1"
HOSPITAL_B = "b11b11b1-0000-4000-8000-0000000000b1"
HOSPITAL_C = "c11c11c1-0000-4000-8000-0000000000c1"  # deactivated
MARKER = "SYNTHETIC-PHI-MARKER-7f3a"
FAST_NAKS = (0.2, 0.2, 0.2, 0.2)


# --- core's side, played by the test ----------------------------------------------


async def _connect(server: NatsServer, user: str, inbox: str) -> Client:
    return await nats.connect(
        server.url(user), inbox_prefix=inbox, allow_reconnect=False, connect_timeout=5
    )


async def ensure_streams(js: Any, ack_wait: float, max_deliver: int = 5) -> None:
    """bus.EnsureStreams' JOBS, DEAD, pipeline and core_dead (values from
    streams.go), with ack_wait as the test needs."""
    await js.add_stream(
        api.StreamConfig(
            name="JOBS",
            subjects=["jobs.v1.>"],
            storage=api.StorageType.FILE,
            retention=api.RetentionPolicy.WORK_QUEUE,
            discard=api.DiscardPolicy.NEW,
            max_msgs_per_subject=200,
            max_msg_size=72 << 10,
            max_bytes=256 << 20,
            duplicate_window=120,
        )
    )
    await js.add_stream(
        api.StreamConfig(
            name="DEAD",
            subjects=["dead.v1.>"],
            storage=api.StorageType.FILE,
            retention=api.RetentionPolicy.LIMITS,
            discard=api.DiscardPolicy.OLD,
            max_msgs_per_subject=10000,
            max_msg_size=4 << 10,
            max_bytes=64 << 20,
        )
    )
    await js.add_consumer(
        "JOBS",
        api.ConsumerConfig(
            durable_name="pipeline",
            filter_subject="jobs.v1.*.run_query",
            ack_policy=api.AckPolicy.EXPLICIT,
            ack_wait=ack_wait,
            max_deliver=max_deliver,
            max_ack_pending=8,
        ),
    )
    await js.add_consumer(
        "DEAD",
        api.ConsumerConfig(
            durable_name="core_dead", ack_policy=api.AckPolicy.EXPLICIT, ack_wait=30
        ),
    )


@dataclass
class Env:
    server: NatsServer
    postgres: Postgres
    core: Client
    js: Any
    keys_file: Path
    signer: Signer
    db: Database
    workers: list[Worker] = field(default_factory=list)
    gates: list[threading.Event] = field(default_factory=list)
    _dead: Any = None

    # jobs

    def job(
        self, qid: str, hid: str = HOSPITAL_A, *, ttl: int = 600, iat: int | None = None
    ) -> tuple[str, dict[str, str], bytes]:
        body = pipeline_pb2.RunQueryJob(query_id=qid).SerializeToString(deterministic=True)
        subject = f"jobs.v1.{hid}.run_query"
        tok = self.signer.mint_job(
            body,
            hid=hid,
            nsub=subject,
            jti=uuid.uuid4().hex,
            iat=int(time.time()) if iat is None else iat,
            ttl=ttl,
        )
        headers = {envelope.TOKEN: tok, envelope.SCHEMA: JOB_SCHEMA, envelope.REQUEST_ID: RID}
        return subject, headers, body

    async def publish(
        self, qid: str, hid: str = HOSPITAL_A, *, msg_id: str | None = None, **kw: Any
    ) -> tuple[str, dict[str, str], bytes]:
        subject, headers, body = self.job(qid, hid, **kw)
        await self.send(subject, headers, body, msg_id=msg_id if msg_id is not None else qid)
        return subject, headers, body

    async def send(
        self, subject: str, headers: dict[str, str], body: bytes, *, msg_id: str | None = None
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
        return await self.js.consumer_info("JOBS", "pipeline")

    async def settled(self, timeout: float = 10.0) -> bool:
        """The job left the queue: nothing pending, nothing unacked."""
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            ci = await self.consumer()
            if ci.num_ack_pending == 0 and ci.num_pending == 0:
                return True
            await asyncio.sleep(0.1)
        return False

    # workers

    async def start(
        self,
        models: ModelClient,
        *,
        settings: JobSettings | None = None,
        max_concurrency: int = 2,
        after_commit: Callable[[], None] | None = None,
        wip_clock: Callable[[], float] | None = None,
        database: Database | None | bool = True,
    ) -> tuple[Worker, RunQuery]:
        ws = WorkerSettings(
            service="pipeline",
            nats_url=self.server.url("pipeline"),
            trusted_keys_path=str(self.keys_file),
            max_concurrency=max_concurrency,
            development=True,
        )
        verifier = Verifier(
            TrustedKeys.from_encoded(self.signer.encoded_keys()),
            "pipeline",
            Operations.load(development=True),
            JtiSet(time.time),
            time.time,
        )
        db = self.db if database is True else (database or None)
        kw: dict[str, Any] = {}
        if wip_clock is not None:
            kw["wip_clock"] = wip_clock
        runner = RunQuery(
            db,
            models,
            settings or JobSettings(tick=0.05, nak_delays=FAST_NAKS),
            clock=time.time,
            after_commit=after_commit,
            max_concurrency=max_concurrency,
            **kw,
        )
        w = Worker(
            ws,
            pipeline.handlers(),
            verifier,
            jobs={"run_query": runner.handle},
            jobs_ready=runner.ready,
        )
        w.fetch_timeout = 0.5
        await w.start()
        self.workers.append(w)
        return w, runner

    def gate(self) -> threading.Event:
        g = threading.Event()
        self.gates.append(g)
        return g


@pytest.fixture
async def database(postgres: Postgres) -> AsyncIterator[Database]:
    db = Database(
        postgres.intelligence_url,
        user=INTELLIGENCE_DB_USER,
        password=INTELLIGENCE_DB_PASSWORD,
        max_size=6,
    )
    await db.open()
    try:
        yield db
    finally:
        await db.close()


@pytest.fixture
def keys_file(tmp_path: Path, signer: Signer) -> Path:
    p = tmp_path / "trusted.json"
    p.write_text(json.dumps({"keys": signer.encoded_keys()}))
    return p


MakeEnv = Callable[..., Awaitable[Env]]


@pytest.fixture
async def make_env(
    fresh_nats: NatsServer,
    postgres: Postgres,
    database: Database,
    keys_file: Path,
    signer: Signer,
) -> AsyncIterator[MakeEnv]:
    envs: list[Env] = []

    async def make(ack_wait: float = 30.0, max_deliver: int = 5) -> Env:
        core = await _connect(fresh_nats, "gateway", "_INBOX_gateway")
        js = core.jetstream()
        await ensure_streams(js, ack_wait, max_deliver)
        e = Env(fresh_nats, postgres, core, js, keys_file, signer, database)
        envs.append(e)
        return e

    yield make
    for e in envs:
        for g in e.gates:
            g.set()  # free any stub thread still blocked
        for w in e.workers:
            await w.abort()
        await e.core.close()


# --- Postgres, as the superuser (test-only) ------------------------------------------


async def su(pg: Postgres) -> psycopg.AsyncConnection[Any]:
    return await psycopg.AsyncConnection.connect(pg.url, autocommit=True)


async def seed_query(pg: Postgres, hid: str = HOSPITAL_A, question: str = "") -> str:
    qid = str(uuid.uuid4())
    async with await su(pg) as conn:
        for h, code in ((HOSPITAL_A, "T11A"), (HOSPITAL_B, "T11B")):
            await conn.execute(
                "INSERT INTO hospitals (id, name, short_code, status, time_zone) "
                "VALUES (%s, %s, %s, 'active', 'UTC') ON CONFLICT (id) DO NOTHING",
                (h, "Synthetic Hospital " + code, code),
            )
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) "
            "VALUES ('KB#0001', NULL, 'Synthetic shared guideline.', 'guideline') "
            "ON CONFLICT DO NOTHING"
        )
        # Chunks no question here may use: a patient's own record (no patient
        # id yet, departure 7), a hospital row posing as reference, and
        # another hospital's guideline (RLS).
        for chunk_id, h, ctype, source in (
            ("KB#A-PAT", HOSPITAL_A, "lab", "P-SYN-1"),
            ("KB#A-REF", HOSPITAL_A, "guideline", "reference"),
            ("KB#B-GUIDE", HOSPITAL_B, "guideline", None),
        ):
            await conn.execute(
                "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, "
                "source_patient_id) VALUES (%s, %s, 'Synthetic chunk.', %s, %s) "
                "ON CONFLICT DO NOTHING",
                (chunk_id, h, ctype, source),
            )
        await conn.execute(
            "INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, "
            "patient_data_json, request_id) VALUES (%s, %s, %s, 'routine', %s, %s)",
            (
                qid,
                hid,
                question or "Synthetic question for a synthetic patient",
                json.dumps({"note": "synthetic", "marker": MARKER}),
                RID,
            ),
        )
    return qid


async def row(pg: Postgres, qid: str) -> dict[str, Any]:
    async with await su(pg) as conn:
        cur = await conn.execute(
            "SELECT status, error, attempts, started_at, completed_at, progress, current_stage, "
            "lease_until FROM clinical_queries WHERE id = %s",
            (qid,),
        )
        r = await cur.fetchone()
        assert r is not None
        keys = (
            "status",
            "error",
            "attempts",
            "started_at",
            "completed_at",
            "progress",
            "current_stage",
            "lease_until",
        )
        return dict(zip(keys, r, strict=True))


async def count(pg: Postgres, table: str, qid: str, action: str | None = None) -> int:
    q = {
        "decisions": "SELECT count(*) FROM clinical_decisions WHERE query_id = %s",
        "hga": "SELECT count(*) FROM hga_evaluations WHERE query_id = %s",
        "outbox": "SELECT count(*) FROM intelligence_audit_outbox WHERE query_id = %s",
    }[table]
    params: tuple[Any, ...] = (qid,)
    if action is not None:
        q += " AND action = %s"
        params = (qid, action)
    async with await su(pg) as conn:
        cur = await conn.execute(q, params)
        r = await cur.fetchone()
        assert r is not None
        return int(r[0])


async def outbox(pg: Postgres, qid: str) -> list[tuple[str, str, dict[str, Any]]]:
    async with await su(pg) as conn:
        cur = await conn.execute(
            "SELECT action, hospital_id::text, payload FROM intelligence_audit_outbox "
            "WHERE query_id = %s ORDER BY id",
            (qid,),
        )
        return [(a, h, p) for a, h, p in await cur.fetchall()]


async def until(pred: Callable[[], Awaitable[bool]], timeout: float = 15.0) -> bool:
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if await pred():
            return True
        await asyncio.sleep(0.05)
    return await pred()


async def status_is(pg: Postgres, qid: str, *want: str) -> bool:
    return (await row(pg, qid))["status"] in want


def started(stub: StubModelClient, stage: str) -> Callable[[], Awaitable[bool]]:
    async def p() -> bool:
        return stub.started[stage].is_set()

    return p


# --- the tests ---------------------------------------------------------------------


async def test_job_runs_to_completed(
    make_env: MakeEnv, postgres: Postgres, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    env = await make_env()
    qid = await seed_query(postgres, question=f"Synthetic question {MARKER}")
    stub = StubModelClient()
    await env.start(stub)
    await env.publish(qid)
    assert await until(lambda: status_is(postgres, qid, "completed"))
    r = await row(postgres, qid)
    assert r["attempts"] == 1 and r["progress"] == pytest.approx(1.0)
    assert r["completed_at"] is not None and r["error"] is None
    assert await count(postgres, "decisions", qid) == 1
    assert await count(postgres, "hga", qid) == 1
    facts = await outbox(postgres, qid)
    assert len(facts) == 1
    action, hid, payload = facts[0]
    assert (action, hid) == ("query.completed", HOSPITAL_A)
    assert set(payload) == {
        "query_id",
        "decision",
        "trust_band",
        "chunk_ids",
        "model_versions",
        "attempts",
    }
    assert payload["query_id"] == qid and payload["decision"] == "PASS"
    assert payload["attempts"] == 1
    # Retrieval saw shared guidelines only, and the cited id is one of them.
    async with await su(postgres) as conn:
        cur = await conn.execute(
            "SELECT chunk_id FROM kb_chunks WHERE hospital_id IS NULL AND chunk_type = 'guideline'"
        )
        shared = {r[0] for r in await cur.fetchall()}
    (seen,) = stub.seen_chunk_ids
    assert "KB#0001" in seen and set(seen) <= shared
    assert not {"KB#A-PAT", "KB#A-REF", "KB#B-GUIDE"} & set(seen)
    assert len(payload["chunk_ids"]) == 1 and set(payload["chunk_ids"]) <= shared
    assert payload["model_versions"] == {"stub": "1"}
    assert await env.settled()
    async with await su(postgres) as conn:
        cur = await conn.execute(
            "SELECT hga_decision, verified, degraded, trust_band, model_versions, "
            "hospital_id::text FROM clinical_decisions WHERE query_id = %s",
            (qid,),
        )
        assert await cur.fetchone() == ("PASS", True, False, "high", {"stub": "1"}, HOSPITAL_A)
    # The question went to the models masked (the stub's mask replaces it).
    assert stub.seen_questions == [StubModelClient.MASKED]
    # No question text, patient data or answer in any log line (spec §10.3).
    assert MARKER not in caplog.text
    assert StubModelClient.ANSWER not in caplog.text


async def test_redelivery_after_completion_acks_without_second_decision(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A4: the same message again after completion is acked; still one
    decision and one outbox fact."""
    env = await make_env()
    qid = await seed_query(postgres)
    stub = StubModelClient()
    await env.start(stub)
    subject, headers, body = await env.publish(qid)
    assert await until(lambda: status_is(postgres, qid, "completed"))
    assert await env.settled()
    await env.send(subject, headers, body, msg_id=None)  # past the dedupe: a new delivery
    assert await env.settled()
    await asyncio.sleep(0.3)
    assert stub.calls["generate"] == 1
    assert await count(postgres, "decisions", qid) == 1
    assert await count(postgres, "outbox", qid, "query.completed") == 1
    assert (await row(postgres, qid))["attempts"] == 1


async def test_killed_worker_job_redelivered_to_other_copy(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """Two copies on the one durable consumer. The first is killed mid-job
    with no ack; after ack_wait JetStream redelivers to the other, which
    finds the dead copy's lease still live, Naks until it ends (F1), then
    claims and completes. Nothing is lost: one decision."""
    env = await make_env(ack_wait=2)
    qid = await seed_query(postgres)
    gate = env.gate()
    dying = StubModelClient(block={"generate": gate})
    settings = JobSettings(lease_s=4, tick=0.05, nak_delays=FAST_NAKS)
    w1, _ = await env.start(dying, settings=settings)
    await env.publish(qid)
    assert await until(started(dying, "generate"))
    assert (await row(postgres, qid))["attempts"] == 1
    await w1.abort()  # killed: no ack, no Nak, no further write
    survivor = StubModelClient()
    await env.start(survivor, settings=settings)
    assert await until(lambda: status_is(postgres, qid, "completed"), timeout=30)
    r = await row(postgres, qid)
    assert r["attempts"] == 2
    assert survivor.calls["generate"] == 1
    assert await count(postgres, "decisions", qid) == 1
    assert await count(postgres, "outbox", qid, "query.completed") == 1
    assert await env.settled()
    gate.set()  # the dead copy's thread ends: it must write nothing
    await asyncio.sleep(0.5)
    assert await count(postgres, "decisions", qid) == 1


async def test_two_workers_race_one_decision(make_env: MakeEnv, postgres: Postgres) -> None:
    """Two workers hold the same message at once (the first one's lease
    expired, so the redelivery re-claims). Both finish their models; only
    the claim holder writes. Exactly one decision and one fact."""
    env = await make_env(ack_wait=2)
    qid = await seed_query(postgres)
    gate_a, gate_b = env.gate(), env.gate()
    a = StubModelClient(block={"verify": gate_a})
    b = StubModelClient(block={"verify": gate_b})
    settings = JobSettings(tick=0.05, nak_delays=FAST_NAKS)
    # One slot: A can't take its own redelivery while it holds the job.
    await env.start(a, settings=settings, max_concurrency=1)
    await env.publish(qid)
    assert await until(started(a, "verify"))
    async with await su(postgres) as conn:  # A's lease runs out
        await conn.execute(
            "UPDATE clinical_queries SET lease_until = now() - interval '1 s' WHERE id = %s",
            (qid,),
        )
    await env.start(b, settings=settings)
    assert await until(started(b, "verify"), timeout=20)
    assert (await row(postgres, qid))["attempts"] == 2
    gate_a.set()
    gate_b.set()  # both race to complete
    assert await until(lambda: status_is(postgres, qid, "completed"))
    await asyncio.sleep(1.0)
    assert await count(postgres, "decisions", qid) == 1
    assert await count(postgres, "outbox", qid, "query.completed") == 1
    assert (await outbox(postgres, qid))[0][2]["attempts"] == 2
    assert await env.settled()


async def test_deactivated_hospital_is_never_claimed(make_env: MakeEnv, postgres: Postgres) -> None:
    """Final review I-3: a question whose row committed after its hospital's
    deactivation trigger ran (00009 couldn't see the row, so couldn't cancel
    it) is never claimed. The row stays queued (core's dead-letter consumer
    or deadline sweep ends it), no model runs, nothing is written; the last
    delivery dead-letters it."""
    env = await make_env(max_deliver=1)
    async with await su(postgres) as conn:
        await conn.execute(
            "INSERT INTO hospitals (id, name, short_code, status, time_zone) "
            "VALUES (%s, 'Synthetic Hospital T11C', 'T11C', 'deactivated', 'UTC') "
            "ON CONFLICT (id) DO NOTHING",
            (HOSPITAL_C,),
        )
    qid = await seed_query(postgres, hid=HOSPITAL_C)
    stub = StubModelClient()
    await env.start(stub, settings=LAST)
    await env.publish(qid, hid=HOSPITAL_C)
    dead = await env.dead_letters(1)
    assert len(dead) == 1 and dead[0][1].job_id == qid
    r = await row(postgres, qid)
    assert (r["status"], r["attempts"], r["started_at"]) == ("queued", 0, None)
    assert stub.calls == {}
    assert await count(postgres, "decisions", qid) == 0
    assert await count(postgres, "outbox", qid) == 0
    assert await env.settled()


async def test_foreign_query_id_is_not_found_and_reads_nothing(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A9 (F27): a job for hospital A naming B's query: no claim, nothing
    written to B's row, Term and one dead letter not_found. Postgres logs
    only the claim and the status read for that id, as mhai_worker with
    hospital A."""
    env = await make_env()
    qb = await seed_query(postgres, hid=HOSPITAL_B)
    before = await row(postgres, qb)
    stub = StubModelClient()
    async with await su(postgres) as conn:
        await conn.execute("ALTER SYSTEM SET log_statement = 'all'")
        await conn.execute("ALTER SYSTEM SET log_line_prefix = 'pid=%p '")
        await conn.execute("SELECT pg_reload_conf()")
    await asyncio.sleep(0.5)
    mark = len(_pg_log(postgres))
    try:
        await env.start(stub)
        await env.publish(qb, hid=HOSPITAL_A)
        dead = await env.dead_letters(1)
        await asyncio.sleep(0.3)
    finally:
        async with await su(postgres) as conn:
            await conn.execute("ALTER SYSTEM RESET log_statement")
            await conn.execute("ALTER SYSTEM RESET log_line_prefix")
            await conn.execute("SELECT pg_reload_conf()")
    assert len(dead) == 1
    subject, d = dead[0]
    assert subject == f"dead.v1.{HOSPITAL_A}.run_query"
    assert (d.job_id, d.type, d.code, d.subject_hospital) == (
        qb,
        "run_query",
        "not_found",
        HOSPITAL_A,
    )
    assert d.attempts == 1
    assert await row(postgres, qb) == before  # status, attempts, started_at unchanged
    assert await count(postgres, "outbox", qb) == 0
    assert stub.calls == {}
    assert await env.settled()

    entries = _statements(_pg_log(postgres)[mark:])
    touching = [e for e in entries if qb in e.params]
    assert touching, "the claim was never logged"
    for e in touching:
        assert e.sql.startswith(
            ("UPDATE clinical_queries SET status = 'running'", "SELECT status")
        ), e.sql
        assert e.hospital == HOSPITAL_A and e.role_set, e
    assert any(e.sql.startswith("UPDATE") for e in touching)
    assert not any(qb in e.sql for e in entries)  # parameters only, never in the SQL


async def test_completion_commit_writes_outbox_atomically(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A13 (worker side): the worker dies right after the commit, before
    the ack. The outbox fact is there; the redelivery acks without a
    second decision."""
    env = await make_env(ack_wait=2)
    qid = await seed_query(postgres)
    crashed: list[int] = []

    def crash() -> None:
        if not crashed:
            crashed.append(1)
            raise RuntimeError("synthetic crash between commit and ack")

    stub = StubModelClient()
    await env.start(stub, after_commit=crash)
    await env.publish(qid)
    assert await until(lambda: status_is(postgres, qid, "completed"))
    assert await count(postgres, "outbox", qid, "query.completed") == 1
    assert crashed == [1]
    assert await env.settled(timeout=15)  # the redelivery is acked
    ci = await env.consumer()
    assert ci.num_redelivered >= 1 or ci.delivered.consumer_seq >= 2
    assert stub.calls["generate"] == 1
    assert await count(postgres, "decisions", qid) == 1
    assert await count(postgres, "outbox", qid) == 1


async def test_poison_job_dead_letters_after_five_deliveries(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A20: a model that is always unavailable: five deliveries (the Nak
    delays shortened), then Term, one dead letter, failed, query.failed."""
    env = await make_env()
    qid = await seed_query(postgres)
    stub = StubModelClient(fail={"generate": "unavailable"})
    await env.start(stub)
    await env.publish(qid)
    dead = await env.dead_letters(1, timeout=20)
    assert len(dead) == 1
    _, d = dead[0]
    assert (d.job_id, d.code, d.attempts) == (qid, "unavailable", 5)
    assert stub.calls["generate"] == 5
    r = await row(postgres, qid)
    assert (r["status"], r["error"], r["attempts"]) == ("failed", "unavailable", 5)
    facts = await outbox(postgres, qid)
    assert [(a, p) for a, _, p in facts] == [
        ("query.failed", {"query_id": qid, "error": "unavailable", "attempts": 5})
    ]
    assert await count(postgres, "decisions", qid) == 0
    assert await env.settled()
    await asyncio.sleep(0.5)
    assert stub.calls["generate"] == 5
    assert len(await env.dead_letters(1, timeout=1)) == 0


async def test_cancelled_between_stages_stops(make_env: MakeEnv, postgres: Postgres) -> None:
    """A21 (worker side): cancelled while generation runs: no decision, no
    query.completed, the message acked."""
    env = await make_env()
    qid = await seed_query(postgres)
    gate = env.gate()
    stub = StubModelClient(block={"generate": gate})
    await env.start(stub)
    await env.publish(qid)
    assert await until(started(stub, "generate"))
    async with await su(postgres) as conn:
        await conn.execute(
            "UPDATE clinical_queries SET status = 'cancelled', cancel_reason = 'user' WHERE id = %s",
            (qid,),
        )
    gate.set()
    assert await env.settled()
    await asyncio.sleep(0.3)
    assert stub.calls["verify"] == 0
    assert (await row(postgres, qid))["status"] == "cancelled"
    assert await count(postgres, "decisions", qid) == 0
    assert await count(postgres, "outbox", qid) == 0


class CountingDatabase:
    """A Database stand-in that counts every use (a refused job must make none)."""

    def __init__(self, inner: Database) -> None:
        self.inner = inner
        self.uses = 0
        self.ready_checks = 0

    def with_hospital(self, v: Any) -> Any:
        self.uses += 1
        return self.inner.with_hospital(v)

    async def ready(self) -> bool:
        self.ready_checks += 1  # answered here, so the pool stays untouched
        return True


async def test_refused_job_touches_no_database(
    make_env: MakeEnv, postgres: Postgres, database: Database
) -> None:
    """Departure 5: a tampered token is Termed with one dead letter carrying
    the reason, and no connection is checked out."""
    env = await make_env()
    qid = await seed_query(postgres)
    counting = CountingDatabase(database)
    stub = StubModelClient()
    await env.start(stub, database=counting)  # type: ignore[arg-type]
    stats_before = database._pool.get_stats().get("requests_num", 0)
    subject, headers, body = env.job(qid)
    tok = headers[envelope.TOKEN]
    head, payload, sig = tok.split(".")
    # A middle character: the last one carries only partial bits, and changing
    # it can make the segment non-canonical ("malformed", not "signature").
    mid = len(sig) // 2
    bad = sig[:mid] + ("A" if sig[mid] != "A" else "B") + sig[mid + 1 :]
    headers[envelope.TOKEN] = ".".join((head, payload, bad))
    await env.send(subject, headers, body, msg_id=qid)
    dead = await env.dead_letters(1)
    assert len(dead) == 1
    dsubject, d = dead[0]
    assert dsubject == f"dead.v1.{HOSPITAL_A}.run_query"
    assert (d.code, d.reason, d.type, d.subject_hospital) == (
        "unauthorized",
        "signature",
        "run_query",
        HOSPITAL_A,
    )
    assert d.job_id == qid and d.attempts == 1
    assert await env.settled()
    assert counting.uses == 0 and counting.ready_checks >= 1
    # requests_num counts every pool checkout.
    assert database._pool.get_stats().get("requests_num", 0) == stats_before
    assert stub.calls == {}
    assert (await row(postgres, qid))["status"] == "queued"


async def test_deadline_marks_failed(
    make_env: MakeEnv, postgres: Postgres, caplog: pytest.LogCaptureFixture
) -> None:
    """F6, both ways. A job token that expired in the queue: Term and a
    dead letter deadline_exceeded with no alert and no database (core marks
    the row). A deadline that passes mid-run: failed / deadline_exceeded,
    query.failed, Term, and the model thread keeps the slot until it ends."""
    caplog.set_level(logging.INFO)
    env = await make_env()
    # 1. Expired in the queue.
    q1 = await seed_query(postgres)
    stub1 = StubModelClient()
    w, _ = await env.start(stub1, max_concurrency=1)
    now = int(time.time())
    await env.publish(q1, ttl=60, iat=now - 120)
    dead = await env.dead_letters(1)
    assert len(dead) == 1
    _, d = dead[0]
    assert (d.job_id, d.code, d.reason) == (q1, "deadline_exceeded", "expired")
    assert stub1.calls == {}
    assert (await row(postgres, q1))["status"] == "queued"
    assert not [
        r for r in caplog.records if r.levelno >= logging.WARNING and "expired" in r.getMessage()
    ]
    assert await env.settled()
    await w.abort()

    # 2. Past exp mid-run.
    q2 = await seed_query(postgres)
    gate = env.gate()
    stub2 = StubModelClient(block={"generate": gate})
    w2, _ = await env.start(stub2, max_concurrency=1)
    await env.publish(q2, ttl=3)
    assert await until(started(stub2, "generate"))
    assert await until(lambda: status_is(postgres, q2, "failed"), timeout=10)
    r = await row(postgres, q2)
    assert r["error"] == "deadline_exceeded"
    facts = await outbox(postgres, q2)
    assert [(a, p["error"]) for a, _, p in facts] == [("query.failed", "deadline_exceeded")]
    assert await env.settled()
    assert await count(postgres, "decisions", q2) == 0
    # The abandoned thread still holds the only slot (T9 minor): the next
    # job waits in the queue ...
    q3 = await seed_query(postgres)
    await env.publish(q3)
    await asyncio.sleep(1.5)
    assert (await row(postgres, q3))["status"] == "queued"
    assert stub2.calls["generate"] == 1  # q2's alone: q3 never started
    gate.set()
    # ... until the thread really ends. The abandoned result is never written.
    assert await until(lambda: status_is(postgres, q3, "completed"))
    assert await count(postgres, "decisions", q2) == 0
    assert (await row(postgres, q2))["status"] == "failed"
    assert stub2.calls["verify"] == 1  # q3's only


class FakeClock:
    def __init__(self) -> None:
        self.t = 1000.0

    def __call__(self) -> float:
        return self.t


async def test_wip_sent_on_long_stage(make_env: MakeEnv, postgres: Postgres) -> None:
    """A 25 s stage (on a test clock) sends +WIP, so JetStream doesn't
    redeliver a job that is still running."""
    env = await make_env()
    qid = await seed_query(postgres)
    gate = env.gate()
    stub = StubModelClient(block={"generate": gate})
    clock = FakeClock()
    w, _ = await env.start(stub, wip_clock=clock)
    sent: list[tuple[str, bytes]] = []
    nc = w.connection
    real = nc.publish

    async def rec(subject: str, payload: bytes = b"", reply: str = "", headers: Any = None) -> None:
        sent.append((subject, payload))
        await real(subject, payload, reply=reply, headers=headers)

    nc.publish = rec  # type: ignore[method-assign]
    await env.publish(qid)
    assert await until(started(stub, "generate"))
    await asyncio.sleep(0.3)
    assert not [p for _, p in sent if p == b"+WPI"]
    lease_before = (await row(postgres, qid))["lease_until"]
    clock.t += 25
    end = time.monotonic() + 5
    while not [p for _, p in sent if p == b"+WPI"] and time.monotonic() < end:
        await asyncio.sleep(0.05)
    wips = [s for s, p in sent if p == b"+WPI"]
    assert wips and all(s.startswith("$JS.ACK.JOBS.pipeline.") for s in wips)
    assert (await row(postgres, qid))["lease_until"] > lease_before  # the lease is extended too
    gate.set()
    assert await until(lambda: status_is(postgres, qid, "completed"))


async def test_no_database_runs_nothing(make_env: MakeEnv, postgres: Postgres) -> None:
    """T10 note: without a database the worker takes no job (fails closed);
    the message waits in the queue."""
    env = await make_env()
    qid = await seed_query(postgres)
    stub = StubModelClient()
    await env.start(stub, database=None)
    await env.publish(qid)
    await asyncio.sleep(1.5)
    ci = await env.consumer()
    assert ci.num_pending == 1 and ci.num_ack_pending == 0
    assert stub.calls == {}
    assert (await row(postgres, qid))["status"] == "queued"


async def test_permanent_bad_query_id_dead_letters_without_database(
    make_env: MakeEnv, postgres: Postgres, database: Database
) -> None:
    """A verified job whose query_id is not a UUID: invalid_request, Term
    and a dead letter, and no query runs."""
    env = await make_env()
    counting = CountingDatabase(database)
    await env.start(StubModelClient(), database=counting)  # type: ignore[arg-type]
    await env.publish("not-a-uuid", msg_id="bad-id-1")
    dead = await env.dead_letters(1)
    assert len(dead) == 1
    assert (dead[0][1].code, dead[0][1].job_id) == ("invalid_request", "")
    assert counting.uses == 0
    assert await env.settled()


# --- Postgres statement log ------------------------------------------------------------


def _pg_log(pg: Postgres) -> list[str]:
    out, err = pg.container.get_logs()
    return (bytes(out) + bytes(err)).decode(errors="replace").splitlines()


@dataclass
class Stmt:
    pid: str
    sql: str
    params: str = ""
    hospital: str = ""
    role_set: bool = False


_LINE = re.compile(r"pid=(\d+) (?:\S+ )?(LOG|DETAIL):\s+(.*)$")


def _statements(lines: list[str]) -> list[Stmt]:
    """Each logged statement with its parameters, and the hospital and role
    its transaction set before it (per backend pid)."""
    out: list[Stmt] = []
    last: dict[str, Stmt] = {}
    hospital: dict[str, str] = {}
    role: dict[str, bool] = {}
    for line in lines:
        m = _LINE.search(line)
        if m is None:
            continue
        pid, kind, text = m.groups()
        if kind == "LOG":
            sm = re.match(r"(?:statement|execute [^:]*): (.*)$", text)
            if sm is None:
                continue
            sql = sm.group(1).strip()
            if sql.upper().startswith("BEGIN"):
                hospital[pid], role[pid] = "", False
            if "SET LOCAL ROLE mhai_worker" in sql:
                role[pid] = True
            s = Stmt(pid, sql, hospital=hospital.get(pid, ""), role_set=role.get(pid, False))
            out.append(s)
            last[pid] = s
        elif kind == "DETAIL" and pid in last:
            s = last[pid]
            s.params = text
            if "set_config('mhai.hospital_id'" in s.sql:
                hm = re.search(r"\$1 = '([0-9a-f-]{36})'", text)
                hospital[pid] = hm.group(1) if hm else ""
                s.hospital = hospital[pid]
    return out


# --- fix round 1: the last delivery is never Nak'd; heartbeats -----------------------


def spy_publishes(w: Worker, fail: bytes | None = None) -> list[tuple[str, bytes]]:
    """Every (subject, payload) the worker publishes; payloads starting with
    fail raise instead of going out (a lost +WIP, say)."""
    sent: list[tuple[str, bytes]] = []
    nc = w.connection
    real = nc.publish

    async def rec(subject: str, payload: bytes = b"", reply: str = "", headers: Any = None) -> None:
        sent.append((subject, payload))
        if fail is not None and payload.startswith(fail):
            raise ConnectionError("synthetic publish failure")
        await real(subject, payload, reply=reply, headers=headers)

    nc.publish = rec  # type: ignore[method-assign]
    return sent


def naks(sent: list[tuple[str, bytes]]) -> list[bytes]:
    return [p for _, p in sent if p.startswith(b"-NAK")]


LAST = JobSettings(max_deliver=1, tick=0.05, wip_every=1.0, nak_delays=FAST_NAKS)


async def hold_lease(pg: Postgres, qid: str, seconds: int) -> None:
    """A dead copy's claim: running, attempts 1, a lease still live."""
    async with await su(pg) as conn:
        await conn.execute(
            "UPDATE clinical_queries SET status = 'running', attempts = 1, started_at = now(), "
            "lease_until = now() + make_interval(secs => %s) WHERE id = %s",
            (seconds, qid),
        )


async def test_last_delivery_waits_out_a_live_lease_then_claims(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """On the last delivery a live lease is waited out here (with +WIP),
    never Nak'd: then the claim is retried and the job completes."""
    env = await make_env(max_deliver=1)
    qid = await seed_query(postgres)
    await hold_lease(postgres, qid, 3)
    stub = StubModelClient()
    w, _ = await env.start(stub, settings=LAST)
    sent = spy_publishes(w)
    await env.publish(qid)
    assert await until(lambda: status_is(postgres, qid, "completed"), timeout=20)
    assert (await row(postgres, qid))["attempts"] == 2
    assert naks(sent) == []
    assert [p for _, p in sent if p == b"+WPI"], "the wait sent no +WIP"
    assert await count(postgres, "decisions", qid) == 1
    assert await env.settled()


async def test_last_delivery_lease_past_exp_dead_letters(
    make_env: MakeEnv, postgres: Postgres
) -> None:
    """A live lease that outlasts the token on the last delivery: a dead
    letter unavailable and Term at once, never a Nak; the holder's row is
    left alone."""
    env = await make_env(max_deliver=1)
    qid = await seed_query(postgres)
    await hold_lease(postgres, qid, 120)
    stub = StubModelClient()
    w, _ = await env.start(stub, settings=LAST)
    sent = spy_publishes(w)
    await env.publish(qid, ttl=10)
    dead = await env.dead_letters(1)
    assert [(d.job_id, d.code) for _, d in dead] == [(qid, "unavailable")]
    assert naks(sent) == []
    assert await env.settled()
    r = await row(postgres, qid)
    assert (r["status"], r["attempts"]) == ("running", 1)
    assert stub.calls == {}
    assert await count(postgres, "outbox", qid) == 0


async def test_last_delivery_queued_race_is_claimed(
    make_env: MakeEnv, postgres: Postgres, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The last delivery's claim loses a race on a queued row: it claims
    again at once instead of Nak'ing."""
    env = await make_env(max_deliver=1)
    qid = await seed_query(postgres)
    real = jobs_repo.claim
    calls: list[int] = []

    async def racing(cur: Any, query_id: str, lease_s: float) -> Any:
        calls.append(1)
        if len(calls) == 1:
            return None  # as if another claim had the row for a moment
        return await real(cur, query_id, lease_s)

    monkeypatch.setattr(jobs_repo, "claim", racing)
    stub = StubModelClient()
    w, _ = await env.start(stub, settings=LAST)
    sent = spy_publishes(w)
    await env.publish(qid)
    assert await until(lambda: status_is(postgres, qid, "completed"))
    assert len(calls) == 2 and naks(sent) == []
    assert await env.settled()


async def test_last_delivery_database_error_dead_letters(
    make_env: MakeEnv, postgres: Postgres, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The claim is lost and the database can't say whose the row is, on
    the last delivery: a dead letter unavailable and Term, not silence."""
    env = await make_env(max_deliver=1)
    qid = await seed_query(postgres)

    async def not_ours(*_args: Any, **_kw: Any) -> bool:
        return False

    async def down(*_args: Any, **_kw: Any) -> Any:
        raise DatabaseError("synthetic: database unavailable")

    monkeypatch.setattr(jobs_repo, "checkpoint", not_ours)
    monkeypatch.setattr(jobs_repo, "state", down)
    stub = StubModelClient()
    w, _ = await env.start(stub, settings=LAST)
    sent = spy_publishes(w)
    await env.publish(qid)
    dead = await env.dead_letters(1)
    assert [(d.job_id, d.code) for _, d in dead] == [(qid, "unavailable")]
    assert naks(sent) == []
    assert await env.settled()
    assert stub.calls["generate"] == 0
    assert await count(postgres, "decisions", qid) == 0


async def test_failed_heartbeats_stop_the_job(make_env: MakeEnv, postgres: Postgres) -> None:
    """A +WIP that can't be sent doesn't renew the lease (so a redelivery
    can re-claim), and three in a row end the job, writing nothing."""
    env = await make_env()
    qid = await seed_query(postgres)
    gate = env.gate()
    stub = StubModelClient(block={"generate": gate})
    clock = FakeClock()
    w, _ = await env.start(stub, wip_clock=clock)
    sent = spy_publishes(w, fail=b"+WPI")
    await env.publish(qid)
    assert await until(started(stub, "generate"))
    lease = (await row(postgres, qid))["lease_until"]

    def wips() -> int:
        return len([p for _, p in sent if p == b"+WPI"])

    for i in range(3):
        clock.t += 25
        end = time.monotonic() + 5
        while wips() < i + 1 and time.monotonic() < end:
            await asyncio.sleep(0.05)
        assert wips() == i + 1
        await asyncio.sleep(0.2)
        assert (await row(postgres, qid))["lease_until"] == lease  # never renewed
    gate.set()
    await asyncio.sleep(1.0)
    assert stub.calls["verify"] == 0
    r = await row(postgres, qid)
    assert (r["status"], r["attempts"]) == ("running", 1)  # left to a re-claim
    assert await count(postgres, "decisions", qid) == 0
    ci = await env.consumer()
    assert ci.num_ack_pending == 1  # not acked, Nak'd or Termed: redelivered later
    assert naks(sent) == []
