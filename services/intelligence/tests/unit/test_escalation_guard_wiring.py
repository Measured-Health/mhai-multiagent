"""The escalation guard on every answer route (ADR 0017): local, cloud, the decision row,
the query.completed fact and the worker's start-up.

The failure it closes: with the citation tags stripped before NLI (#185), a reply that
recites its on-topic chunk is fully grounded, so a red-flag question passed with a high
trust score. Here that reply still gets NLI 1.0 and still escalates. Every text is
synthetic; the NLI model is the tag-sensitive fake of test_nli_citation_tags.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

import pytest

from intelligence.agents import answer_profile
from intelligence.boundary import jobs_repo
from intelligence.boundary import models as models_mod
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.client import CloudCall, CloudModelClient, CloudReply
from intelligence.boundary.cloud.reply import validate_answer
from intelligence.boundary.cloud.scan import Identifiers
from intelligence.boundary.cloud.settings import ProviderSettings
from intelligence.boundary.jobs.run_query import RunQuery
from intelligence.boundary.models import Generation, Verification, _verify_with
from intelligence.guardrails import escalation_guard as eg
from intelligence.knowledge_base import kb_builder
from tests.boundary.cloud.conftest import BASE_URL, FAKE_KEY, MODEL, CapturingTransport, FakeAdapter
from tests.boundary.cloud.test_client import FakeBudget
from tests.unit.test_nli_citation_tags import (
    CLAIM,
    MALARIA,
    URTI,
    TagSensitiveNLI,
    _Generator,
    _Retriever,
    _sealed,
    _shared,
)

RECITAL = f"{CLAIM} [KB#0002]."
ANSWERABLE = "Synthetic question: how is malaria confirmed?"
RED_FLAG = "Synthetic question: malaria with convulsions and now unresponsive, how is it confirmed?"
DOSE = "Synthetic question: what artesunate dose should he get?"


@pytest.fixture
def build(monkeypatch: pytest.MonkeyPatch) -> Any:
    def factory(*args: Any, **kwargs: Any) -> Any:
        return TagSensitiveNLI

    monkeypatch.setattr(models_mod, "nli_factory", factory)
    monkeypatch.setattr(kb_builder, "HybridRetriever", _Retriever)
    b, versions = models_mod.local_parts(generator=_Generator())
    b.versions = versions  # type: ignore[attr-defined]
    return b


def _local(build: Any, question: str, patient: dict[str, Any] | None = None) -> Verification:
    chunks = [_shared("KB#0002", MALARIA), _shared("KB#0005", URTI)]
    orch = build(chunks, profile=answer_profile.DEFAULT)
    gen = Generation(
        question=question,
        response=RECITAL,
        available=True,
        patient_context=patient or {},
        chunk_texts=models_mod._chunk_texts(chunks),
    )
    return _verify_with(orch, gen)


# ---------------------------------------------------------------- the local route


def test_an_answerable_recital_still_passes(build: Any) -> None:
    v = _local(build, ANSWERABLE)
    assert v.decision == "PASS"
    assert v.guard_reasons == () and v.guard_rules == ()


def test_a_grounded_recital_to_a_red_flag_question_escalates(build: Any) -> None:
    v = _local(build, RED_FLAG)
    assert v.scores["nli"] == 1.0  # fully grounded: NLI can't see the problem
    assert v.decision == "ESCALATE"
    assert v.guard_reasons == ("red_flag",)
    assert "consciousness-altered" in v.guard_rules
    assert v.verified is True and v.trust_score is not None  # scores kept for review
    assert models_mod.trust_band(v) == "low"


def test_a_red_flag_in_the_patient_data_escalates(build: Any) -> None:
    v = _local(build, ANSWERABLE, {"age": 4, "symptoms": ["vomits everything"]})
    assert v.decision == "ESCALATE" and v.guard_reasons == ("red_flag",)


def test_missing_information_escalates(build: Any) -> None:
    v = _local(build, "Synthetic question: is this patient's malaria severe?")
    assert v.decision == "ESCALATE"
    assert "missing_information" in v.guard_reasons


def test_a_dose_the_chunks_do_not_give_escalates(build: Any) -> None:
    v = _local(build, DOSE)
    assert v.decision == "ESCALATE" and v.guard_reasons == ("out_of_scope",)


def test_an_off_question_recital_is_downgraded(build: Any) -> None:
    v = _local(build, "Synthetic question: what diet suits gout?")
    assert v.decision == "REGENERATE"
    assert v.guard_reasons == ("recital",)


def test_a_failing_recital_check_is_a_recital(build: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*a: Any, **k: Any) -> bool:
        raise RuntimeError("synthetic")

    monkeypatch.setattr(eg, "is_recital", boom)
    v = _local(build, ANSWERABLE)
    assert v.decision == "REGENERATE" and v.guard_reasons == ("recital",)


def test_a_guard_that_cannot_load_escalates(build: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    def refused() -> Any:
        raise eg.RuleSetError("sha256")

    monkeypatch.setattr(eg, "default", refused)
    v = _local(build, ANSWERABLE)
    assert v.decision == "ESCALATE"
    assert v.guard_reasons == ("guard_unavailable",)


def test_the_guard_reasons_survive_an_hga_failure() -> None:
    class Orch:
        hga = None

    gen = Generation(question=RED_FLAG, response=RECITAL, available=True)
    v = _verify_with(Orch(), gen)
    assert v.decision == "ESCALATE" and v.failed_agents == ("HGA",)
    assert v.guard_reasons == ("red_flag",)


def test_the_local_client_hands_the_guard_its_inputs(build: Any) -> None:
    class CCA:
        def generate_with_rag(self, *a: Any, **k: Any) -> dict[str, Any]:
            return {
                "response": RECITAL,
                "citations": ["0002"],
                "retrieved_chunks": [{"id": "KB#0002"}],
            }

    from intelligence.agents.orchestrator import DynamicOrchestratorAgent

    def b(chunks: Any) -> Any:
        orch = build(chunks)
        return DynamicOrchestratorAgent(rag_cca=CCA(), hga=orch.hga)

    client = models_mod.OrchestratorModelClient(b, masker=object())
    patient = {"age": 70, "symptoms": ["chest pain radiating to the jaw"]}
    gen = client.generate(ANSWERABLE, patient, None, [_shared("KB#0002", MALARIA)])
    assert gen.patient_context == patient and gen.chunk_texts == {"KB#0002": MALARIA}
    v = client.verify(gen)
    assert v.decision == "ESCALATE" and v.guard_reasons == ("red_flag",)


def test_the_rule_set_version_is_recorded(build: Any) -> None:
    assert build.versions["escalation_guard"] == eg.default().version_string
    assert models_mod._SAFE_VERSION.fullmatch(build.versions["escalation_guard"])


def test_local_parts_refuses_a_rule_set_that_cannot_load(monkeypatch: pytest.MonkeyPatch) -> None:
    def refused() -> Any:
        raise eg.RuleSetError("sha256")

    monkeypatch.setattr(eg, "default", refused)
    with pytest.raises(eg.RuleSetError):
        models_mod.local_parts(generator=_Generator())


# ---------------------------------------------------------------- the cloud route


def _cloud(build: Any, question: str, patient: dict[str, Any]) -> Verification:
    chunks = [_sealed("KB#0002", MALARIA), _sealed("KB#0005", URTI)]
    sent = frozenset(c.chunk_id for c in chunks)
    answer = validate_answer(
        {"answer": RECITAL, "citations": ["KB#0002"], "insufficient_evidence": False}, sent
    )
    adapter = FakeAdapter([CloudReply(answer, "synthetic-2026-11-01", 120, 40, "ok")])
    client = CloudModelClient(
        ProviderSettings("anthropic", MODEL, ApiKey(FAKE_KEY), None, BASE_URL),
        adapter,
        build,
        {"cca": "local-gen", "dense": "dense-1", "nli": "nli-1"},
        CapturingTransport(),
        sleep=lambda s: None,
    )
    call = CloudCall(Identifiers(frozenset()), FakeBudget(), 1e12)
    gen = client.generate(client.mask(question), patient, None, chunks, call=call)
    assert gen.cloud_outcome == "ok"
    assert set(gen.chunk_texts) == {"KB#0002", "KB#0005"}
    return client.verify(gen)


@pytest.mark.disable_socket
def test_the_cloud_route_passes_an_answerable_recital(build: Any) -> None:
    v = _cloud(build, ANSWERABLE, {})
    assert v.decision == "PASS" and v.guard_reasons == ()


@pytest.mark.disable_socket
def test_the_cloud_route_escalates_a_red_flag_recital(build: Any) -> None:
    v = _cloud(build, RED_FLAG, {})
    assert v.scores["nli"] == 1.0
    assert v.decision == "ESCALATE" and v.guard_reasons == ("red_flag",)


@pytest.mark.disable_socket
def test_the_cloud_route_reads_the_masked_patient_data(build: Any) -> None:
    v = _cloud(build, ANSWERABLE, {"age": 30, "symptoms": ["took a handful of tablets"]})
    assert v.decision == "ESCALATE" and v.guard_reasons == ("red_flag",)


@pytest.mark.disable_socket
def test_the_cloud_route_escalates_an_out_of_scope_dose(build: Any) -> None:
    v = _cloud(build, DOSE, {})
    assert v.decision == "ESCALATE" and v.guard_reasons == ("out_of_scope",)


# ---------------------------------------------------------------- the record


def _decision(v: Verification) -> jobs_repo.Decision:
    runner = RunQuery.__new__(RunQuery)
    client = models_mod.StubModelClient()
    g = Generation(question="q", response="r", available=True, model_used="m")
    return runner._decision(client, "local", g, v)


def test_the_decision_row_carries_the_codes() -> None:
    v = Verification(
        decision="ESCALATE",
        trust_score=0.9,
        verified=True,
        degraded=False,
        guard_reasons=("red_flag", "out_of_scope"),
        guard_rules=("stroke-signs", "dose-scope"),
    )
    d = _decision(v)
    assert d.decision == "ESCALATE"
    assert d.guardrail_report["guard_reasons"] == ["red_flag", "out_of_scope"]
    assert d.guardrail_report["guard_rules"] == ["stroke-signs", "dose-scope"]
    assert d.guard_reasons == ("red_flag", "out_of_scope")


def test_a_decision_without_guard_reasons_records_none() -> None:
    d = _decision(Verification(decision="PASS", trust_score=0.9, verified=True, degraded=False))
    assert d.guardrail_report["guard_reasons"] == [] and d.guard_reasons == ()


class _Cursor:
    def __init__(self) -> None:
        self.calls: list[tuple[str, Any]] = []

    async def execute(self, query: str, params: Any = None) -> None:
        self.calls.append((query, params))

    async def fetchone(self) -> Any:
        return (1,)


def _fact(guard_reasons: tuple[str, ...]) -> dict[str, Any]:
    d = jobs_repo.Decision(
        response_text="synthetic",
        citations=(),
        trust_score=0.9,
        decision="ESCALATE",
        trust_band="low",
        verified=True,
        degraded=False,
        failed_agents=(),
        model_versions={},
        guardrail_report={},
        retrieval_ms=None,
        generation_ms=None,
        scores={},
        flag_penalty=None,
        weights={},
        flags=(),
        guard_reasons=guard_reasons,
    )
    cur = _Cursor()
    qid = "00000000-0000-4000-8000-000000000001"
    assert asyncio.run(jobs_repo.complete(cur, qid, 1, d))  # type: ignore[arg-type]
    (fact,) = [p for q, p in cur.calls if q == jobs_repo._INSERT_FACT]
    payload: dict[str, Any] = json.loads(fact[3])
    return payload


def test_the_completion_fact_carries_the_reason_codes() -> None:
    assert _fact(("red_flag", "missing_information"))["guard_reasons"] == [
        "red_flag",
        "missing_information",
    ]
    assert "guard_reasons" not in _fact(())


# ---------------------------------------------------------------- start-up


def test_the_pipeline_worker_refuses_to_start(monkeypatch: pytest.MonkeyPatch) -> None:
    from intelligence.boundary import worker
    from intelligence.boundary.config import WorkerSettings

    def refused() -> Any:
        raise eg.RuleSetError("sha256")

    def never(*a: Any, **k: Any) -> Any:
        raise AssertionError("got past the rule set")

    monkeypatch.setattr(eg, "default", refused)
    monkeypatch.setattr(worker, "build_verifier", never)
    settings = WorkerSettings.from_env(
        {
            "APP_ENV": "development",
            "INTELLIGENCE_SERVICE": "pipeline",
            "NATS_URL": "nats://127.0.0.1:4222",
            "INTELLIGENCE_TRUSTED_KEYS_PATH": "/nonexistent/trusted.json",
        },
        service="pipeline",
    )
    with pytest.raises(eg.RuleSetError):
        asyncio.run(worker.run_service(settings))


def test_the_cli_exits_2_on_a_refused_rule_set(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    import argparse

    from intelligence import cli
    from intelligence.boundary import worker
    from intelligence.boundary.config import WorkerSettings

    async def refused(settings: Any) -> None:
        raise eg.RuleSetError("sha256")

    monkeypatch.setattr(worker, "run_service", refused)
    monkeypatch.setattr(WorkerSettings, "from_env", classmethod(lambda cls, *a, **k: object()))
    assert cli._worker(argparse.Namespace(service="pipeline", log_level="WARNING")) == 2
    assert "escalation guard rule set refused: sha256" in capsys.readouterr().err


def test_inputs_the_guard_cannot_read_escalate(build: Any) -> None:
    chunks = [_shared("KB#0002", MALARIA)]
    orch = build(chunks)
    gen = Generation(
        question=ANSWERABLE,
        response=RECITAL,
        available=True,
        chunk_texts=None,  # type: ignore[arg-type]
    )
    v = _verify_with(orch, gen)
    assert v.decision == "ESCALATE" and v.guard_reasons == ("guard_unavailable",)
