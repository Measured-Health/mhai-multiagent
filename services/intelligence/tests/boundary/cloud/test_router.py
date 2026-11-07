"""The job on the cloud route, against live NATS and Postgres migrated by
core (plan Task 10; design sections 5.2, 8, 11, 12). Marked `boundary`.

The route is core's: the hospital's policy, read by core's trigger at
insert and re-checked by core's mhai_effective_model_route at the claim.
The cloud client is a CloudModelClient over FakeAdapter and the stub's
local parts; every request is captured, nothing leaves. Every id, name and
text here is synthetic.
"""

from __future__ import annotations

import asyncio
import json
import logging
import time
import uuid
from typing import Any

import pytest
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from intelligence.boundary.cloud import vocab
from intelligence.boundary.cloud.client import CloudReply
from intelligence.boundary.cloud.reply import validate_answer
from intelligence.boundary.cloud.retry import RETRY
from intelligence.boundary.models import ModelRouter, StubModelClient
from intelligence.boundary.pb import pipeline_pb2
from tests.boundary import test_run_query as rq
from tests.boundary import test_telemetry as tt
from tests.boundary import test_worker as tw
from tests.boundary.cloud.conftest import FakeAdapter

pytestmark = pytest.mark.boundary

keys_file = tw.keys_file
database = rq.database
make_env = rq.make_env
exporters = tt.exporters

HOSPITAL_D = "d11d11d1-0000-4000-8000-0000000000d1"  # the budget race's own


async def decision(pg: rq.Postgres, qid: str) -> dict[str, Any]:
    async with await rq.su(pg) as conn:
        cur = await conn.execute(
            "SELECT hga_decision, trust_score, verified, degraded, model_versions, "
            "guardrail_report_json, masked_question, masked_patient_json, response_text "
            "FROM clinical_decisions WHERE query_id = %s",
            (qid,),
        )
        r = await cur.fetchone()
    assert r is not None
    keys = (
        "decision",
        "trust_score",
        "verified",
        "degraded",
        "model_versions",
        "report",
        "masked_question",
        "masked_patient",
        "response_text",
    )
    return dict(zip(keys, r, strict=True))


async def fact(pg: rq.Postgres, qid: str) -> dict[str, Any]:
    facts = await rq.outbox(pg, qid)
    assert [a for a, _, _ in facts] == ["query.completed"]
    return facts[0][2]


async def model_route(pg: rq.Postgres, qid: str) -> str:
    async with await rq.su(pg) as conn:
        cur = await conn.execute("SELECT model_route FROM clinical_queries WHERE id = %s", (qid,))
        r = await cur.fetchone()
    assert r is not None
    return str(r[0])


async def run_one(
    env: rq.Env, pg: rq.Postgres, qid: str, timeout: float = 20.0, hid: str = rq.HOSPITAL_A
) -> None:
    await env.publish(qid, hid=hid)
    assert await rq.until(lambda: rq.status_is(pg, qid, "completed", "failed"), timeout=timeout)
    assert (await rq.row(pg, qid))["status"] == "completed"


# --- the route ------------------------------------------------------------------------------


async def test_cloud_answer_records_provider(make_env: rq.MakeEnv, postgres: rq.Postgres) -> None:
    # A16: the answer records the provider; the fact records egress and outcome.
    env = await make_env()
    qid = await rq.seed_query(postgres, provider="anthropic")
    assert await model_route(postgres, qid) == "cloud:anthropic"
    f = rq.cloud_fake()
    await env.start(f.router)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["decision"] == "PASS" and d["verified"] is True and d["trust_score"] == 0.9
    assert d["model_versions"]["cca"].startswith("anthropic/")
    assert d["model_versions"]["cca_route"] == "cloud"
    assert d["model_versions"]["dense"] == "stub" and d["model_versions"]["nli"] == "stub"
    assert d["report"]["cloud_outcome"] == "ok"
    assert d["report"]["tokens_in"] == 120 and d["report"]["tokens_out"] == 40
    p = await fact(postgres, qid)
    assert p["egress"] == "anthropic" and p["cloud_outcome"] == "ok"
    assert p["egress"] in vocab.EGRESS and p["cloud_outcome"] in vocab.OUTCOMES
    assert p["model_versions"] == d["model_versions"]
    # Core's fact schema (internal/intel/factschema.go) takes these keys.
    assert set(p) == {
        "query_id",
        "decision",
        "trust_band",
        "chunk_ids",
        "model_versions",
        "attempts",
        "egress",
        "cloud_outcome",
    }
    assert len(f.transport.bodies) == 1 and f.adapter.sends == 1
    assert await env.settled()


async def test_local_answer_records_route_local(
    make_env: rq.MakeEnv, postgres: rq.Postgres
) -> None:
    env = await make_env()
    qid = await rq.seed_query(postgres)
    assert await model_route(postgres, qid) == "local"
    f = rq.cloud_fake()
    await env.start(f.router)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["model_versions"] == {"stub": "1", "cca_route": "local"}
    assert "cloud_outcome" not in d["report"]
    p = await fact(postgres, qid)
    assert p["egress"] == "none" and "cloud_outcome" not in p
    assert f.adapter.calls == 0 and f.transport.bodies == []


async def test_route_changed_between_publish_and_claim_runs_local(
    make_env: rq.MakeEnv, postgres: rq.Postgres
) -> None:
    # A14 and Review Focus 5: inserted as cloud:anthropic, the hospital is on
    # openai by the claim: neither provider; it runs local and says so.
    env = await make_env()
    qid = await rq.seed_query(postgres, provider="anthropic")
    async with await rq.su(postgres) as conn:
        await rq.set_policy(conn, rq.HOSPITAL_A, "openai")
    f = rq.cloud_fake()
    await env.start(f.router)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["model_versions"]["cca_route"] == "local"
    assert (await fact(postgres, qid))["egress"] == "none"
    assert f.adapter.calls == 0 and f.transport.bodies == []
    async with await rq.su(postgres) as conn:
        await rq.set_policy(conn, rq.HOSPITAL_A, None)


async def test_message_cannot_choose_route(make_env: rq.MakeEnv, postgres: rq.Postgres) -> None:
    # K13: the route is the row's, set by core's trigger. An insert naming a
    # route is overwritten, and a message carrying extra fields changes nothing.
    env = await make_env()
    qid = await rq.seed_query(postgres)
    async with await rq.su(postgres) as conn:
        await conn.execute(
            "UPDATE clinical_queries SET model_route = 'cloud:anthropic' WHERE id = %s", (qid,)
        )
    f = rq.cloud_fake()
    await env.start(f.router)
    body = pipeline_pb2.RunQueryJob(query_id=qid).SerializeToString(deterministic=True)
    # An unknown field 15 ("cloud:anthropic"), signed with the job.
    extra = body + bytes([0x7A, 15]) + b"cloud:anthropic"
    subject, headers, _ = env.job(qid)
    headers[rq.envelope.TOKEN] = env.signer.mint_job(
        extra,
        hid=rq.HOSPITAL_A,
        nsub=subject,
        jti=uuid.uuid4().hex,
        iat=int(time.time()),
        ttl=600,
    )
    await env.send(subject, headers, extra, msg_id=qid)
    # Whatever the worker made of it (completed on the row's route, or
    # refused), no cloud request was ever built.
    if not await rq.until(lambda: rq.status_is(postgres, qid, "completed"), timeout=10):
        assert len(await env.dead_letters(1)) == 1
    assert f.adapter.calls == 0 and f.transport.bodies == []

    # An insert that names a route is overwritten by core's trigger.
    async with await rq.su(postgres) as conn:
        qid2 = str(uuid.uuid4())
        await conn.execute(
            "INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, "
            "patient_data_json, request_id, model_route) "
            "VALUES (%s, %s, 'Synthetic question', 'routine', '{}', %s, 'cloud:anthropic')",
            (qid2, rq.HOSPITAL_A, rq.RID),
        )
    assert await model_route(postgres, qid2) == "local"
    await run_one(env, postgres, qid2)
    assert f.adapter.calls == 0 and f.transport.bodies == []


# --- failures ---------------------------------------------------------------------------------


def _marker_reply() -> CloudReply:
    answer = validate_answer(
        {
            "answer": f"Synthetic answer {rq.MARKER} [KB#0001].",
            "citations": ["KB#0001"],
            "insufficient_evidence": False,
        },
        frozenset({"KB#0001"}),
    )
    return CloudReply(answer, "synthetic-2026-11-01", 10, 10, "ok")


def _unmetered_reply() -> CloudReply:
    answer = validate_answer(
        {
            "answer": "Synthetic [KB#0001].",
            "citations": ["KB#0001"],
            "insufficient_evidence": False,
        },
        frozenset({"KB#0001"}),
    )
    return CloudReply(answer, "synthetic-2026-11-01", None, None, "ok")


FAILURES: list[tuple[str, list[Any]]] = [
    ("refused", ["refused"]),
    ("truncated", ["truncated"]),
    ("malformed", ["malformed"]),
    ("auth_failed", ["auth_failed"]),
    ("retention_unconfirmed", ["retention_unconfirmed"]),
    ("insufficient", ["insufficient"]),
    ("unavailable", [RETRY, RETRY, RETRY]),
    ("timeout", ["timeout"]),
]


@pytest.mark.parametrize(("outcome", "script"), FAILURES, ids=[o for o, _ in FAILURES])
async def test_cloud_failure_completes_escalated(
    make_env: rq.MakeEnv, postgres: rq.Postgres, outcome: str, script: list[Any]
) -> None:
    # A12: completed, ESCALATE, no score, the outcome in the report and the
    # fact, no Nak, no cca_route (so core shows no generated_by).
    env = await make_env()
    qid = await rq.seed_query(postgres, provider="anthropic")
    f = rq.cloud_fake(FakeAdapter(script))
    w, _ = await env.start(f.router)
    sent = rq.spy_publishes(w)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["decision"] == "ESCALATE" and d["trust_score"] is None
    assert d["verified"] is False and d["degraded"] is True
    assert d["report"]["cloud_outcome"] == outcome
    assert "cca_route" not in d["model_versions"] and "cca" not in d["model_versions"]
    assert d["response_text"] == ""
    p = await fact(postgres, qid)
    assert p["cloud_outcome"] == outcome and p["egress"] == "anthropic"
    assert rq.naks(sent) == []
    assert await env.settled()


async def test_budget_exhausted_and_egress_blocked_complete_escalated(
    make_env: rq.MakeEnv, postgres: rq.Postgres
) -> None:
    env = await make_env()
    f = rq.cloud_fake()
    await env.start(f.router)
    # A name under an identifying key, then in the question: refused before
    # any reservation; egress none.
    qid = await rq.seed_query(
        postgres,
        provider="anthropic",
        question="Is Adaeze Okafor febrile today?",
        patient={"name": {"given": ["Adaeze"], "family": "Okafor"}, "disease_category": "fever"},
    )
    await run_one(env, postgres, qid)
    p = await fact(postgres, qid)
    assert (p["cloud_outcome"], p["egress"]) == ("egress_blocked", "none")
    assert f.transport.bodies == []
    # A budget of one token.
    qid2 = await rq.seed_query(postgres, provider="anthropic")
    async with await rq.su(postgres) as conn:
        await conn.execute(
            "UPDATE hospital_model_policy SET monthly_token_budget = 1 WHERE hospital_id = %s",
            (rq.HOSPITAL_A,),
        )
    try:
        await run_one(env, postgres, qid2)
        p = await fact(postgres, qid2)
        assert (p["cloud_outcome"], p["egress"]) == ("budget_exhausted", "none")
        assert f.transport.bodies == []
        assert (await decision(postgres, qid2))["decision"] == "ESCALATE"
    finally:
        async with await rq.su(postgres) as conn:
            await rq.set_policy(conn, rq.HOSPITAL_A, None)


async def test_unconfigured_provider_escalates_and_alerts(
    make_env: rq.MakeEnv, postgres: rq.Postgres, caplog: pytest.LogCaptureFixture
) -> None:
    # Core's policy names a provider this worker hasn't enabled.
    caplog.set_level(logging.INFO)
    env = await make_env()
    qid = await rq.seed_query(postgres, provider="xai")
    f = rq.cloud_fake()
    await env.start(f.router)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["decision"] == "ESCALATE" and d["trust_score"] is None
    assert d["report"]["cloud_outcome"] == "unconfigured"
    p = await fact(postgres, qid)
    assert (p["cloud_outcome"], p["egress"]) == ("unconfigured", "none")
    assert any(
        r.levelno == logging.ERROR and r.getMessage() == "cloud.unconfigured provider=xai"
        for r in caplog.records
    )
    assert f.adapter.calls == 0
    async with await rq.su(postgres) as conn:
        await rq.set_policy(conn, rq.HOSPITAL_A, None)


async def test_unconfigured_and_failed_cloud_have_no_route(
    make_env: rq.MakeEnv, postgres: rq.Postgres
) -> None:
    env = await make_env()
    plain = StubModelClient()
    await env.start(ModelRouter(plain, {}))  # no cloud client at all
    qid = await rq.seed_query(postgres, provider="anthropic")
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert "cca_route" not in d["model_versions"]
    assert d["model_versions"] == {"stub": "1"}  # the local client's, without cca
    assert plain.calls["generate"] == 0
    async with await rq.su(postgres) as conn:
        await rq.set_policy(conn, rq.HOSPITAL_A, None)


async def test_reviewer_copy_is_plain_and_masked(
    make_env: rq.MakeEnv, postgres: rq.Postgres
) -> None:
    env = await make_env()
    qid = await rq.seed_query(
        postgres,
        provider="anthropic",
        patient={"disease_category": "fever", "risk_score": 0.4, "note": "synthetic note"},
    )
    f = rq.cloud_fake()
    await env.start(f.router)
    await run_one(env, postgres, qid)
    d = await decision(postgres, qid)
    assert d["masked_patient"] == {
        "disease_category": "fever",
        "risk_score": 0.4,
        "note": "synthetic note",
    }
    assert d["masked_question"] == "Synthetic question for a synthetic patient"
    # Only the two allow-listed fields left; the note never did.
    body = json.loads(f.transport.bodies[0])
    texts = [b["text"] for b in body["messages"][0]["content"]]
    assert texts[1] == "Category: fever\nRisk: 0.4"
    assert "synthetic note" not in f.transport.bodies[0].decode()
    async with await rq.su(postgres) as conn:
        await rq.set_policy(conn, rq.HOSPITAL_A, None)


async def test_no_text_in_logs_spans_dead_letters_on_cloud_path(
    make_env: rq.MakeEnv,
    postgres: rq.Postgres,
    caplog: pytest.LogCaptureFixture,
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    # A18: the marker in the question, the patient data, a chunk's text and
    # the reply, over ok and every failure outcome, and a dead-lettered job.
    spans, _ = exporters
    caplog.set_level(logging.DEBUG)
    env = await make_env(max_deliver=1)
    async with await rq.su(postgres) as conn:
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) "
            "VALUES ('KB#0777', NULL, %s, 'guideline') ON CONFLICT DO NOTHING",
            (f"Synthetic shared guideline {rq.MARKER}.",),
        )
    script: list[Any] = [_marker_reply()] + [s for _, ss in FAILURES for s in ss]
    f = rq.cloud_fake(FakeAdapter(script))
    await env.start(f.router, settings=rq.LAST)
    patient = {"disease_category": "fever", "note": f"note {rq.MARKER}"}
    qids = []
    for _ in range(1 + len(FAILURES)):
        qid = await rq.seed_query(
            postgres, provider="anthropic", question=f"Synthetic {rq.MARKER}", patient=patient
        )
        await run_one(env, postgres, qid)
        qids.append(qid)
    # A job that dies on the last delivery: a dead letter.
    bad = rq.cloud_fake(fail={"generate": "internal"})
    await env.start(bad.router, settings=rq.LAST)
    for w in env.workers[:-1]:
        await w.abort()
    qid = await rq.seed_query(
        postgres, provider="anthropic", question=f"Synthetic {rq.MARKER}", patient=patient
    )
    await env.publish(qid)
    dead = await env.dead_letters(1, timeout=20)
    assert len(dead) == 1
    await asyncio.sleep(0.3)
    tt.assert_no_secret(
        spans.get_finished_spans(),
        caplog.text,
        [rq.MARKER],
        [d.SerializeToString() for _, d in dead],
    )
    outcomes = [(await fact(postgres, q))["cloud_outcome"] for q in qids]
    assert outcomes[0] == "ok" and set(outcomes[1:]) == {o for o, _ in FAILURES}
    async with await rq.su(postgres) as conn:
        await conn.execute("DELETE FROM kb_chunks WHERE chunk_id = 'KB#0777'")
        await rq.set_policy(conn, rq.HOSPITAL_A, None)


# --- the budget race (A11) ------------------------------------------------------------------


async def test_budget_race_through_jobs(make_env: rq.MakeEnv, postgres: rq.Postgres) -> None:
    # 20 jobs against a budget for 5: at most 5 adapter calls, the rest
    # budget_exhausted. The hospital is the test's own, so the month's
    # ledger starts empty.
    env = await make_env()
    async with await rq.su(postgres) as conn:
        await conn.execute(
            "INSERT INTO hospitals (id, name, short_code, status, time_zone) "
            "VALUES (%s, 'Synthetic Hospital T11D', 'T11D', 'active', 'UTC') "
            "ON CONFLICT (id) DO NOTHING",
            (HOSPITAL_D,),
        )
    # One job first, to learn one attempt's estimate (body bytes + cap).
    probe = await rq.seed_query(postgres, hid=HOSPITAL_D, provider="anthropic")
    # Replies with no reported use: each settle counts the whole reservation,
    # so the budget really holds five attempts.
    f = rq.cloud_fake(FakeAdapter([_unmetered_reply() for _ in range(25)]))
    await env.start(f.router, max_concurrency=8)
    await run_one(env, postgres, probe, hid=HOSPITAL_D)
    estimate = len(f.transport.bodies[0]) + 1500
    async with await rq.su(postgres) as conn:
        await conn.execute("DELETE FROM cloud_usage WHERE hospital_id = %s", (HOSPITAL_D,))
        await conn.execute(
            "UPDATE hospital_model_policy SET monthly_token_budget = %s WHERE hospital_id = %s",
            (estimate * 5 + estimate // 2, HOSPITAL_D),
        )
    sends_before = f.adapter.sends
    qids = [await rq.seed_query(postgres, hid=HOSPITAL_D, provider="anthropic") for _ in range(20)]
    for q in qids:
        await env.publish(q, hid=HOSPITAL_D)
    for q in qids:
        assert await rq.until(lambda q=q: rq.status_is(postgres, q, "completed"), timeout=60)
    outcomes = [(await fact(postgres, q))["cloud_outcome"] for q in qids]
    sends = f.adapter.sends - sends_before
    assert sends <= 5
    assert outcomes.count("ok") == sends
    assert outcomes.count("budget_exhausted") == 20 - sends
    assert sends >= 1
