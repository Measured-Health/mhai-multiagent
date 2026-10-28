"""boundary/models.py, boundary/kb_repo.py and the job slot: unit tests, no
Docker and no model weights. Every question, chunk and id is synthetic.
"""

from __future__ import annotations

import asyncio
import threading
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import pytest

from intelligence.boundary import kb_repo
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.jobs import Slot
from intelligence.boundary.models import (
    Generation,
    ModelClient,
    ModelError,
    OrchestratorModelClient,
    StubModelClient,
    Verification,
    build_model_client,
    trust_band,
)
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

QUESTION = "Synthetic question about a synthetic guideline"
BASE_ENV = {
    "APP_ENV": "development",
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/nonexistent/trusted.json",
}


# --- kb_repo --------------------------------------------------------------------


class FakeCursor:
    """Plays the with_hospital cursor: returns the rows RLS would show."""

    def __init__(self, rows: list[tuple[Any, ...]]) -> None:
        self.rows = rows
        self.executed: list[tuple[str, Any]] = []

    async def execute(self, query: str, params: Any = None) -> FakeCursor:
        self.executed.append((query, params))
        return self

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self.rows


def _row(
    chunk_id: str,
    shared: bool,
    chunk_type: str = "guideline",
    source: str | None = None,
    content: str = "synthetic content",
) -> tuple[Any, ...]:
    # (chunk_id, shared, content, chunk_type, source_patient_id, source_subject_id, metadata)
    return (chunk_id, shared, content, chunk_type, source, None, {"topic": "synthetic"})


SHARED_GUIDELINE = _row("KB#0001", True)
SHARED_NOT_GUIDELINE = _row("KB#0002", True, chunk_type="lab")
OWN_PATIENT = _row("KB#0003", False, chunk_type="lab", source="P-SYN-1")
OTHER_PATIENT = _row("KB#0004", False, chunk_type="lab", source="P-SYN-2")
# A hospital's own row claiming the reserved source: never a shared guideline.
HOSPITAL_CLAIMS_REFERENCE = _row("KB#0005", False, source=REFERENCE_SOURCE)
HOSPITAL_NO_PATIENT = _row("KB#0006", False, source=None)
ALL_ROWS = [
    SHARED_GUIDELINE,
    SHARED_NOT_GUIDELINE,
    OWN_PATIENT,
    OTHER_PATIENT,
    HOSPITAL_CLAIMS_REFERENCE,
    HOSPITAL_NO_PATIENT,
]


async def test_shared_chunks_visible_patient_chunks_not() -> None:
    cur = FakeCursor(ALL_ROWS)
    chunks = await kb_repo.load_chunks(cur, "P-SYN-1")  # type: ignore[arg-type]
    by_id = {c.chunk_id: c for c in chunks}
    assert set(by_id) == {"KB#0001", "KB#0003"}
    assert by_id["KB#0001"].source_patient_id == REFERENCE_SOURCE
    assert all(isinstance(c, KBChunk) for c in chunks)
    # The SQL is a constant with a row bound and no hospital filter: RLS
    # decides which hospital's rows (and the shared ones) come back.
    (query, params), *_ = cur.executed
    assert "hospital_id IS NULL" in query and "LIMIT" in query
    assert params == (kb_repo.MAX_CHUNKS,)


async def test_no_patient_sees_shared_guidelines_only() -> None:
    """Departure 7: no patient id yet, so only shared guidelines."""
    for patient in (None, "", REFERENCE_SOURCE, "unassigned"):
        chunks = await kb_repo.load_chunks(FakeCursor(ALL_ROWS), patient)  # type: ignore[arg-type]
        assert [c.chunk_id for c in chunks] == ["KB#0001"], patient


async def test_load_chunks_requires_patient_id_argument() -> None:
    with pytest.raises(TypeError):
        await kb_repo.load_chunks(FakeCursor(ALL_ROWS))  # type: ignore[call-arg]


# --- the orchestrator client -------------------------------------------------------


class FailingCCA:
    def generate_with_rag(self, *args: Any, **kwargs: Any) -> Any:
        raise RuntimeError("synthetic CCA failure")


class StubCCA:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def generate_with_rag(
        self, query: str, top_k: int = 5, triage_context: Any = None, patient_id: Any = None
    ) -> dict[str, Any]:
        self.calls.append({"query": query, "patient_id": patient_id})
        return {
            "response": "Synthetic answer [KB#0001] [KB#9999].",
            "citations": ["0001", "9999"],
            "n_citations": 2,
            "retrieved_chunks": [{"id": "KB#0001"}],
            "retrieval_latency_ms": 1.5,
            "total_latency_ms": 4.0,
            "model_used": "rule_based",
        }


class FailingHGA:
    def verify(self, *args: Any, **kwargs: Any) -> Any:
        raise RuntimeError("synthetic HGA failure")


class PassingHGA:
    def __init__(self) -> None:
        self.kwargs: dict[str, Any] = {}

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        self.kwargs = kwargs
        return {
            "trust_score": 0.91,
            "decision": "pass",
            "nli_score": 0.9,
            "coherence_score": 1.0,
            "numerical_score": 0.85,
            "semantic_entropy_score": 0.7,
            "flag_penalty": 0.0,
            "flags": [{"type": "uncited", "severity": "warning", "text": "SECRET CLAIM TEXT"}],
            "weights": {"alpha": 0.35},
        }


def _orchestrator_client(cca: Any, hga: Any) -> OrchestratorModelClient:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent

    built: list[list[KBChunk]] = []

    def build(chunks: list[KBChunk]) -> DynamicOrchestratorAgent:
        built.append(chunks)
        return DynamicOrchestratorAgent(rag_cca=cca, hga=hga)

    client = OrchestratorModelClient(build, versions={"cca": "synthetic-1"})
    client.built = built  # type: ignore[attr-defined]
    return client


def _chunk() -> KBChunk:
    return KBChunk("KB#0001", "synthetic guideline", "guideline", REFERENCE_SOURCE, "")


def test_orchestrator_failure_escalates() -> None:
    """ADR 0005: a CCA that raises gives an escalated, degraded result with
    no score, never a pass."""
    client = _orchestrator_client(FailingCCA(), PassingHGA())
    gen = client.generate(QUESTION, {}, None, [_chunk()])
    assert not gen.available and gen.failed_agents == ("CCA",)
    v = client.verify(gen)
    assert v.decision == "ESCALATE"
    assert v.degraded and not v.verified
    assert v.trust_score is None and trust_band(v) is None
    assert v.failed_agents == ("CCA",)


def test_orchestrator_hga_failure_escalates() -> None:
    client = _orchestrator_client(StubCCA(), FailingHGA())
    v = client.verify(client.generate(QUESTION, {}, None, [_chunk()]))
    assert v.decision == "ESCALATE" and v.degraded and not v.verified
    assert v.trust_score is None and v.failed_agents == ("HGA",)


def test_orchestrator_missing_agents_escalate() -> None:
    client = _orchestrator_client(None, None)
    v = client.verify(client.generate(QUESTION, {}, None, []))
    assert v.decision == "ESCALATE" and v.degraded and v.trust_score is None


def test_orchestrator_passes_per_call_thresholds_and_keeps_ids_only() -> None:
    cca, hga = StubCCA(), PassingHGA()
    client = _orchestrator_client(cca, hga)
    gen = client.generate(QUESTION, {"disease_category": "synthetic"}, None, [_chunk()])
    assert cca.calls == [{"query": QUESTION, "patient_id": None}]
    assert client.built == [[_chunk()]]  # type: ignore[attr-defined]
    # A cited id that wasn't retrieved is not a citation of a known chunk.
    assert gen.cited_chunk_ids == ("KB#0001",)
    v = client.verify(gen)
    assert v.decision == "PASS" and v.verified and not v.degraded
    assert v.trust_score == pytest.approx(0.91)
    assert trust_band(v) == "high"
    assert {"pass_threshold", "escalate_threshold"} <= set(hga.kwargs)
    # Flags keep their type and severity, never the claim text.
    assert v.flags == (("uncited", "warning"),)
    assert "SECRET" not in repr(v)


def test_orchestrator_odd_decision_escalates() -> None:
    class OddHGA(PassingHGA):
        def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
            out = super().verify(response_text, **kwargs)
            out["decision"] = "review"
            return out

    client = _orchestrator_client(StubCCA(), OddHGA())
    v = client.verify(client.generate(QUESTION, {}, None, [_chunk()]))
    assert v.decision == "ESCALATE"


def test_orchestrator_masks_with_pii_masker() -> None:
    client = _orchestrator_client(StubCCA(), PassingHGA())
    assert "555-0100" not in client.mask("Synthetic call 555-555-0100 today")


def test_generation_repr_has_no_text() -> None:
    gen = Generation(question="QUESTION-MARKER", response="ANSWER-MARKER")
    assert "MARKER" not in repr(gen) and "MARKER" not in str(gen)


# --- the stub client -----------------------------------------------------------------


def test_stub_returns_fixed_synthetic_answers() -> None:
    stub = StubModelClient()
    assert isinstance(stub, ModelClient)
    masked = stub.mask("anything at all")
    assert masked == StubModelClient.MASKED
    gen = stub.generate(masked, {}, None, [_chunk()])
    assert gen.available and gen.cited_chunk_ids == ("KB#0001",)
    v = stub.verify(gen)
    assert isinstance(v, Verification) and v.decision == "PASS" and v.verified
    assert stub.model_versions() == {"stub": "1"}
    assert stub.calls == {"mask": 1, "generate": 1, "verify": 1}


def test_stub_fails_raises_or_blocks_per_stage() -> None:
    with pytest.raises(ModelError) as e:
        StubModelClient(fail={"generate": "unavailable"}).generate("q", {}, None, [])
    assert e.value.code == "unavailable" and e.value.retryable
    with pytest.raises(ZeroDivisionError):
        StubModelClient(raises={"verify": ZeroDivisionError()}).verify(Generation())
    gate = threading.Event()
    stub = StubModelClient(block={"mask": gate})
    t = threading.Thread(target=stub.mask, args=("q",))
    t.start()
    assert stub.started["mask"].wait(2)
    assert t.is_alive()
    gate.set()
    t.join(2)
    assert not t.is_alive()


def test_model_error_codes_are_closed() -> None:
    assert ModelError("internal").retryable and ModelError("unavailable").retryable
    assert not ModelError("invalid_request").retryable
    with pytest.raises(ValueError):
        ModelError("some free text")


# --- INTELLIGENCE_MODEL_CLIENT (F14) -----------------------------------------------


def test_model_client_defaults_to_orchestrator() -> None:
    assert WorkerSettings.from_env(BASE_ENV).model_client == "orchestrator"


def test_stub_model_client_only_in_development() -> None:
    env = dict(BASE_ENV, INTELLIGENCE_MODEL_CLIENT="stub")
    assert WorkerSettings.from_env(env).model_client == "stub"
    for app_env in ("production", "staging", None):
        e = {k: v for k, v in env.items() if k != "APP_ENV"}
        if app_env is not None:
            e["APP_ENV"] = app_env
        with pytest.raises(ConfigError) as err:
            WorkerSettings.from_env(e)
        assert "INTELLIGENCE_MODEL_CLIENT" in str(err.value)


def test_unknown_model_client_refused() -> None:
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env(dict(BASE_ENV, INTELLIGENCE_MODEL_CLIENT="fake"))
    assert "INTELLIGENCE_MODEL_CLIENT" in str(err.value) and "fake" not in str(err.value)


def test_build_model_client_refuses_stub_outside_development() -> None:
    s = WorkerSettings(
        service="pipeline", nats_url="nats://x", trusted_keys_path="k", model_client="stub"
    )
    with pytest.raises(ConfigError):
        build_model_client(s)
    dev = WorkerSettings(
        service="pipeline",
        nats_url="nats://x",
        trusted_keys_path="k",
        model_client="stub",
        development=True,
    )
    assert isinstance(build_model_client(dev), StubModelClient)


# --- the job slot (T9 deferred minor) ------------------------------------------------


async def test_slot_held_until_the_thread_finishes() -> None:
    """asyncio can't stop a thread: a stage abandoned at the deadline keeps
    the worker's slot until its thread really ends."""
    sem = asyncio.Semaphore(1)
    await sem.acquire()
    slot = Slot(sem)
    gate = threading.Event()
    with ThreadPoolExecutor(1) as pool:
        fut = pool.submit(gate.wait, 5)
        slot.hold(fut)
        slot.release()  # the job's task is done (it abandoned the stage)
        await asyncio.sleep(0.05)
        assert sem.locked(), "slot released while the thread still runs"
        gate.set()
        for _ in range(100):
            if not sem.locked():
                break
            await asyncio.sleep(0.01)
        assert not sem.locked()
    slot.release()  # a second release is a no-op, never an extra slot
    assert sem._value == 1  # type: ignore[attr-defined]
