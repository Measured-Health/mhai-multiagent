"""A1: synthetic identifiers everywhere never reach a request body, and
clean records are still sent (plan Task 10; design section 4.5). Marked
`boundary`: whole jobs run against NATS and Postgres migrated by core.

Hypothesis draws synthetic records (derandomised, one run, at most 50
jobs): random synthetic names, MRNs (string and integer), Nigerian phone
numbers, NINs, addresses and dates, in every field, at nested levels, as
dict keys and in the question. Names go under identifying keys or after a
title, never bare in a free-text field: an untitled name there is the C8
residual (pinned in test_scan.py), which this test doesn't claim to cover.

Each job runs through FakeAdapter, which POSTs the real rendering into a
CapturingTransport, and again through each real adapter (PR D: its SDK
encodes the body, and a transport answers ok citing the first chunk it was
sent). No raw identifier value (normalised by norm_words)
appears in any captured body, decoded as the scan decodes it, and each
job either sent a clean body or ended egress_blocked. Two checks keep it
from passing vacuously: at least 10 bodies are captured, and CLEAN_RECORD
is always sent. Every value is synthetic.
"""

from __future__ import annotations

import json
from typing import Any

import pytest
from hypothesis import HealthCheck, given, settings
from hypothesis import strategies as st

from intelligence.boundary.cloud.scan import IDENTIFYING_KEYS, norm_words, walk
from tests.boundary import test_run_query as rq
from tests.boundary import test_worker as tw
from tests.boundary.cloud import adapter_kit as kit

pytestmark = pytest.mark.boundary

keys_file = tw.keys_file
database = rq.database
make_env = rq.make_env

JOBS = 40
GIVEN = ("Adaeze", "Chukwuemeka", "Ngozi", "Oluwaseun", "Grace", "Ifeoma", "Babatunde")
FAMILY = ("Okafor", "Adeyemi", "Nwachukwu", "Balogun", "Okonkwo", "Eze")
TITLES = ("Mr", "Mrs", "Dr", "Ms")
# An ordinary word used as a name: blocked whenever a chunk sent has the word.
GRACE_CHUNK = "Synthetic guideline: allow a grace period of 24 hours before review."
CLEAN_QUESTION = "What is the first-line fluid for a synthetic fever?"
CLEAN_RECORD: dict[str, Any] = {
    "disease_category": "fever",
    "risk_score": 0.3,
    "name": {"given": ["Zubairu"], "family": "Olatunji-Bassey"},
    "mrn": "MRN-930417",
    "phone": "0803 555 0199",
}
QUESTIONS = (
    "What is the first-line fluid for a synthetic fever?",
    "Which antimalarial is first line for uncomplicated malaria?",
    "When should a febrile child be referred?",
)


@st.composite
def identifier(draw: st.DrawFn) -> tuple[str, Any]:
    """(kind, value): one synthetic identifier."""
    kind = draw(st.sampled_from(("mrn", "mrn_int", "phone", "nin", "address", "dob", "email")))
    n = draw(st.integers(100_000, 999_999))
    if kind == "mrn":
        return kind, f"MRN-{n}"
    if kind == "mrn_int":
        return kind, n
    if kind == "phone":
        prefix = draw(st.sampled_from(("0803", "0806", "0813", "0705", "0901")))
        return kind, f"{prefix} {n // 1000:03d} {n % 10000:04d}"
    if kind == "nin":
        return kind, f"{draw(st.integers(10**10, 10**11 - 1))}"
    if kind == "address":
        street = draw(st.sampled_from(("Adeola Odeku Street", "Herbert Macaulay Way")))
        return kind, f"{n % 200 + 1} {street}, Lagos"
    if kind == "dob":
        return kind, f"{n % 28 + 1:02d}/{n % 12 + 1:02d}/19{n % 90 + 10}"
    return kind, f"patient{n}@example.com"


KEY_FOR = {
    "mrn": "mrn",
    "mrn_int": "mrn",
    "phone": "phone",
    "nin": "nin",
    "address": "address",
    "dob": "birthDate",
    "email": "email",
}


@st.composite
def record(draw: st.DrawFn) -> tuple[str, dict[str, Any], list[Any]]:
    """(question, patient data, the raw identifier values it holds)."""
    given_name = draw(st.sampled_from(GIVEN))
    family = draw(st.sampled_from(FAMILY))
    values: list[Any] = [given_name, family]
    patient: dict[str, Any] = {
        "disease_category": draw(st.sampled_from(("fever", "malaria", "respiratory"))),
        "risk_score": draw(st.sampled_from((0.1, 0.5, 0.9))),
    }
    # The name under an identifying key, at a nested level or not.
    name = {"given": [given_name], "family": family}
    if draw(st.booleans()):
        patient["name"] = name
    else:
        patient["contact"] = {"next_of_kin": {"name": name}}
    for kind, value in draw(st.lists(identifier(), min_size=1, max_size=4)):
        values.append(value)
        where = draw(st.sampled_from(("top", "nested", "list", "key", "category")))
        key = KEY_FOR[kind]
        if where == "top":
            patient[key] = value
        elif where == "nested":
            patient.setdefault("history", {}).setdefault("records", {})[key] = value
        elif where == "list":
            patient.setdefault("encounters", []).append({key: value, "note": "synthetic"})
        elif where == "key":
            patient.setdefault("index", {})[str(value)] = "synthetic"
        elif kind not in ("address",):
            # A pattern-shaped value in the one free-text field that leaves.
            patient["disease_category"] = f"fever {value}"
    question = draw(st.sampled_from(QUESTIONS))
    leak = draw(st.sampled_from(("none", "none", "title", "identifier")))
    if leak == "title":
        question = f"{draw(st.sampled_from(TITLES))} {family}: {question}"
    elif leak == "identifier":
        question = f"{question} Ref {values[-1]}"
    return question, patient, values


def _strings(body: bytes) -> str:
    doc = json.loads(body)
    return " ".join(v for _, v in walk(doc) if isinstance(v, str))


def _leaks(body: bytes, values: list[Any]) -> list[str]:
    text = f" {norm_words(_strings(body))} "
    out = []
    for v in values:
        w = norm_words(str(v))
        if len(w.replace(" ", "")) >= 3 and f" {w} " in text:
            out.append(w)
    return out


# The real adapters (plan Tasks 11 to 13): provider -> its adapter class.
def _adapters() -> dict[str, Any]:
    from intelligence.boundary.cloud.anthropic_adapter import AnthropicAdapter
    from intelligence.boundary.cloud.openai_adapter import OpenAIAdapter
    from intelligence.boundary.cloud.xai_adapter import XaiAdapter

    return {"anthropic": AnthropicAdapter, "openai": OpenAIAdapter, "xai": XaiAdapter}


KINDS = ("fake", "anthropic", "openai", "xai")


def _cloud(kind: str) -> tuple[rq.CloudFake, str]:
    if kind == "fake":
        return rq.cloud_fake(), "anthropic"
    adapter = _adapters()[kind](rq.MODEL)
    return rq.cloud_fake(adapter, provider=kind, transport=kit.ReplyingTransport(kind)), kind


@pytest.mark.parametrize("kind", KINDS)
@settings(
    max_examples=1,
    derandomize=True,
    deadline=None,
    suppress_health_check=list(HealthCheck),
)
@given(data=st.data())
async def test_property_no_identifier_reaches_a_body(
    make_env: rq.MakeEnv, postgres: rq.Postgres, kind: str, data: st.DataObject
) -> None:
    env = await make_env()
    async with await rq.su(postgres) as conn:
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) "
            "VALUES ('KB#0888', NULL, %s, 'guideline') ON CONFLICT DO NOTHING",
            (GRACE_CHUNK,),
        )
    f, provider = _cloud(kind)
    await env.start(f.router, max_concurrency=4)
    try:
        cases = [(CLEAN_QUESTION, CLEAN_RECORD, [])]
        cases += [data.draw(record()) for _ in range(JOBS - 1)]
        sent = blocked = 0
        for question, patient, values in cases:
            before = len(f.transport.bodies)
            qid = await rq.seed_query(
                postgres, provider=provider, question=question, patient=patient
            )
            await env.publish(qid)
            assert await rq.until(lambda q=qid: rq.status_is(postgres, q, "completed"), 30)
            facts = await rq.outbox(postgres, qid)
            outcome = facts[-1][2]["cloud_outcome"]
            new = f.transport.bodies[before:]
            if patient is CLEAN_RECORD:
                assert outcome == "ok" and len(new) == 1, "the clean record must be sent"
                values = _clean_values()
            if new:
                assert outcome == "ok"
                sent += 1
                for body in new:
                    assert _leaks(body, values) == [], (question, values)
            else:
                assert outcome == "egress_blocked", outcome
                blocked += 1
        assert sent >= 10, (sent, blocked)
    finally:
        async with await rq.su(postgres) as conn:
            await conn.execute("DELETE FROM kb_chunks WHERE chunk_id = 'KB#0888'")
            await rq.set_policy(conn, rq.HOSPITAL_A, None)


def _clean_values() -> list[Any]:
    out: list[Any] = []
    for ptr, v in walk(CLEAN_RECORD):
        parts = ptr.strip("/").split("/")
        if any(p.casefold() in IDENTIFYING_KEYS for p in parts):
            out.append(v)
    return out
