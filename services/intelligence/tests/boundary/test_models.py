"""boundary/models.py, boundary/kb_repo.py and the job slot: unit tests, no
Docker and no model weights. Every question, chunk and id is synthetic.
"""

from __future__ import annotations

import asyncio
import logging
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
    # Constant SQL with row bounds and no hospital filter: RLS decides which
    # hospital's rows (and the shared ones) come back.
    assert [params for _, params in cur.executed] == [
        (kb_repo.MAX_SHARED_CHUNKS,),
        (kb_repo.MAX_HOSPITAL_CHUNKS,),
    ]
    for query, _ in cur.executed:
        assert "LIMIT %s" in query and "mhai_current_hospital" not in query


class RoutingCursor:
    """The with_hospital cursor, answering each of kb_repo's reads with the
    rows RLS would show for it."""

    def __init__(
        self,
        shared: list[tuple[Any, ...]] | None = None,
        library: list[tuple[Any, ...]] | None = None,
        hospital: list[tuple[Any, ...]] | None = None,
    ) -> None:
        self.answers = {
            kb_repo._LOAD_SHARED: shared or [],
            kb_repo._LOAD_LIBRARY: library or [],
            kb_repo._LOAD_HOSPITAL: hospital or [],
        }
        self.executed: list[tuple[str, Any]] = []
        self._next: list[tuple[Any, ...]] = []

    async def execute(self, query: str, params: Any = None) -> RoutingCursor:
        self.executed.append((query, params))
        self._next = self.answers[query]
        return self

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self._next


def _library_row(n: int, content: str = "synthetic reference text", **meta: Any) -> tuple[Any, ...]:
    vector = [0.0] * 8
    vector[n % 8] = 1.0
    metadata = {"kind": "drug_label", "title": f"Synthetic drug {n}", **meta}
    embedder = "sentence-transformers/synthetic-embedder@" + "0" * 40
    return (f"KB#{1000000 + n}", True, content, "guideline", None, None, metadata, vector, embedder)


def _patient_row(n: int, patient: str = "P-SYN-1") -> tuple[Any, ...]:
    return (f"KB#{n:04d}", False, f"Synthetic lab {n}.", "lab", patient, None, {})


async def test_hospital_rows_are_never_crowded_out_by_shared_rows() -> None:
    """The old read (shared first, one LIMIT 5000) gave a hospital nothing
    once 5,000 shared rows existed. Each kind now has its own bound."""
    shared = [_row(f"KB#{i:04d}", True) for i in range(1, kb_repo.MAX_SHARED_CHUNKS + 1)]
    library = [_library_row(i) for i in range(kb_repo.LIBRARY_CANDIDATES)]
    hospital = [_patient_row(i) for i in range(6000, 6010)]
    cur = RoutingCursor(shared=shared, library=library, hospital=hospital)
    chunks = await kb_repo.load_chunks(cur, "P-SYN-1", question="synthetic question")  # type: ignore[arg-type]
    ids = [c.chunk_id for c in chunks]
    assert set(ids) >= {f"KB#{i:04d}" for i in range(6000, 6010)}
    assert len(ids) == kb_repo.MAX_SHARED_CHUNKS + kb_repo.LIBRARY_CANDIDATES + 10
    queries = [q for q, _ in cur.executed]
    assert queries == [kb_repo._LOAD_SHARED, kb_repo._LOAD_LIBRARY, kb_repo._LOAD_HOSPITAL]
    assert cur.executed[1][1] == ("synthetic question", kb_repo.LIBRARY_CANDIDATES)
    assert cur.executed[2][1] == (kb_repo.MAX_HOSPITAL_CHUNKS,)
    assert "hospital_id IS NOT NULL" in kb_repo._LOAD_HOSPITAL
    assert all("NULLS FIRST" not in q for q in queries)


async def test_library_is_read_only_with_a_question() -> None:
    for question in ("", "   "):
        cur = RoutingCursor(library=[_library_row(1)])
        assert await kb_repo.load_chunks(cur, None, question=question) == []  # type: ignore[arg-type]
        assert kb_repo._LOAD_LIBRARY not in [q for q, _ in cur.executed]
    cur = RoutingCursor(library=[_library_row(1)])
    long = "synthetic " * 1000
    await kb_repo.load_chunks(cur, None, question=long)  # type: ignore[arg-type]
    (text, _bound) = cur.executed[1][1]
    assert text == long[: kb_repo.MAX_QUESTION_CHARS]


async def test_library_rows_are_sealed_reference_guidelines_with_their_vectors() -> None:
    cur = RoutingCursor(library=[_library_row(3, url="https://example.org/synthetic")])
    (chunk,) = await kb_repo.load_chunks(cur, None, question="synthetic")  # type: ignore[arg-type]
    assert chunk.chunk_id == "KB#1000003"
    assert chunk.source_patient_id == REFERENCE_SOURCE and chunk.source_subject_id == ""
    assert chunk.chunk_type == "guideline"
    assert chunk.metadata["title"] == "Synthetic drug 3"
    assert chunk.metadata["url"] == "https://example.org/synthetic"
    from intelligence.boundary.cloud.gate import shared_chunk_of

    assert shared_chunk_of(chunk).chunk_id == "KB#1000003"
    assert chunk.embedding is not None and chunk.embedding.dtype.name == "float32"
    assert list(chunk.embedding) == [0, 0, 0, 1, 0, 0, 0, 0]
    assert chunk.embedder.startswith("sentence-transformers/synthetic-embedder@")


async def test_a_bad_vector_is_dropped_not_used() -> None:
    for bad in ([], [float("nan")] * 4, "1,2,3", [[1.0, 2.0]], [0.1] * 5000):
        row = _library_row(1)
        row = (*row[:7], bad, row[8])
        (chunk,) = await kb_repo.load_chunks(  # type: ignore[arg-type]
            RoutingCursor(library=[row]), None, question="synthetic"
        )
        assert chunk.embedding is None and chunk.embedder == "", bad


async def test_shared_rows_are_never_patient_scoped() -> None:
    """A shared row is a reference row whatever its source columns say
    (00002's kb_shared_has_no_patient keeps them null), and only a
    guideline is shown."""
    posing = (*_library_row(1)[:4], "P-SYN-1", "P-SYN-1", *_library_row(1)[6:])
    lab = (*_library_row(2)[:3], "lab", *_library_row(2)[4:])
    cur = RoutingCursor(library=[posing, lab])
    for patient in ("P-SYN-1", None):
        chunks = await kb_repo.load_chunks(cur, patient, question="synthetic")  # type: ignore[arg-type]
        assert [c.chunk_id for c in chunks] == ["KB#1000001"]
        assert chunks[0].source_patient_id == REFERENCE_SOURCE
        assert chunks[0].source_subject_id == ""


async def test_a_hospital_row_cannot_take_a_library_id_or_pose_as_shared() -> None:
    hospital = [
        ("KB#1000001", False, "Synthetic posing row.", "guideline", "P-SYN-1", None, {}),
        _patient_row(7),
    ]
    # A read answering with the wrong kind of row: each read keeps its own.
    shared_from_hospital_read = _row("KB#0099", True)
    cur = RoutingCursor(
        library=[_library_row(1), _patient_row(8)],
        hospital=[*hospital, shared_from_hospital_read],
    )
    chunks = await kb_repo.load_chunks(cur, "P-SYN-1", question="synthetic")  # type: ignore[arg-type]
    by_id = {c.chunk_id: c for c in chunks}
    assert set(by_id) == {"KB#1000001", "KB#0007"}
    assert by_id["KB#1000001"].source_patient_id == REFERENCE_SOURCE
    assert by_id["KB#1000001"].content == "synthetic reference text"


async def test_library_sql_is_constant_and_parameterised() -> None:
    q = kb_repo._LOAD_LIBRARY
    assert "%s" in q and "{" not in q
    assert "l.active" in q and "k.hospital_id IS NULL" in q and "LIMIT %s" in q
    assert "plainto_tsquery('english', %s)" in q


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


def test_orchestrator_build_failure_escalates() -> None:
    """A model that fails to load (the per-job build) escalates like any
    other model failure, never raises out of generate or passes."""

    def build(chunks: Any) -> Any:
        raise OSError("synthetic: model weights missing")

    client = OrchestratorModelClient(build)
    gen = client.generate(QUESTION, {}, None, [_chunk()])
    assert not gen.available and gen.failed_agents == ("CCA",)
    v = client.verify(gen)
    assert v.decision == "ESCALATE" and v.degraded and v.trust_score is None


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


# --- the cloud route's seams (plan Task 9) ------------------------------------------------


def test_generation_cloud_fields_default_to_local() -> None:
    g = Generation()
    assert dict(g.versions) == {}
    assert g.egress == "none"
    assert g.cloud_outcome is None
    assert (g.tokens_in, g.tokens_out) == (0, 0)


def test_local_clients_accept_and_ignore_call() -> None:
    stub = StubModelClient()
    g = stub.generate(QUESTION, {}, None, [_chunk()], call=object())  # type: ignore[arg-type]
    assert g.available is True and g.cloud_outcome is None and g.egress == "none"
    cca = StubCCA()
    client = _orchestrator_client(cca, PassingHGA())
    g = client.generate(QUESTION, {}, None, [_chunk()], call=object())  # type: ignore[arg-type]
    assert g.available is True and g.cloud_outcome is None


def test_verify_with_is_the_local_verification() -> None:
    from intelligence.boundary.models import _verify_with

    hga = PassingHGA()
    client = _orchestrator_client(StubCCA(), hga)
    g = client.generate(QUESTION, {}, None, [_chunk()])
    via_client = client.verify(g)
    via_function = _verify_with(g.handle, g)
    assert via_client == via_function
    assert via_function.verified is True and via_function.trust_score == 0.91


def test_local_parts_is_default_orchestrators(monkeypatch: pytest.MonkeyPatch) -> None:
    from intelligence.boundary import models

    sentinel = (lambda chunks: None, {"cca": "g", "dense": "d", "nli": "n"})
    monkeypatch.setattr(models, "local_parts", lambda *a, **k: sentinel)
    client = models.default_orchestrator()
    assert client.model_versions() == {"cca": "g", "dense": "d", "nli": "n"}


# --- the router (plan Task 10) -----------------------------------------------------------


class _Closable(StubModelClient):
    def __init__(self, transport: object) -> None:
        super().__init__()
        self.transport = transport
        self.closed = 0

    def close(self) -> None:
        self.closed += 1


def test_router_picks_one_client_per_job() -> None:
    from intelligence.boundary.models import ModelRouter

    local = StubModelClient()
    shared = object()
    anthropic, openai = _Closable(shared), _Closable(shared)
    router = ModelRouter(local, {"anthropic": anthropic, "openai": openai})
    assert router.client_for("local") == (local, "local")
    assert router.client_for("cloud:anthropic") == (anthropic, "anthropic")
    assert router.client_for("cloud:openai") == (openai, "openai")
    router.close()  # the one shared transport is closed once
    assert anthropic.closed + openai.closed == 1


@pytest.mark.parametrize(
    ("route", "provider"),
    [
        ("cloud:xai", "xai"),
        ("cloud:gemini", "gemini"),
        ("cloud:mistral", "unknown"),
        ("", "unknown"),
        ("LOCAL", "unknown"),
    ],
)
def test_router_unknown_route_is_unconfigured(
    route: str, provider: str, caplog: pytest.LogCaptureFixture
) -> None:
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader

    from intelligence.boundary import telemetry
    from intelligence.boundary.models import ModelRouter, _Unconfigured

    local = StubModelClient()
    router = ModelRouter(local, {"anthropic": StubModelClient()})
    client, got = router.client_for(route)
    assert type(client) is _Unconfigured and got == provider
    assert client.mask("x") == StubModelClient.MASKED  # the local masker
    reader = InMemoryMetricReader()
    stop = telemetry.setup(metric_reader=reader)
    try:
        with caplog.at_level(logging.ERROR):
            g = client.generate("q", {}, None, [_chunk()], call=None)
        data = reader.get_metrics_data()
    finally:
        stop()
    assert g.available is False and g.cloud_outcome == "unconfigured"
    assert g.failed_agents == ("CCA",) and dict(g.versions) == {} and g.egress == "none"
    assert local.calls["generate"] == 0  # no local fallback
    v = client.verify(g)
    assert v.decision == "ESCALATE" and v.trust_score is None and v.verified is False
    assert "cca" not in client.model_versions()
    assert [r.getMessage() for r in caplog.records] == [f"cloud.unconfigured provider={provider}"]
    pts = [
        p
        for rm in data.resource_metrics
        for sm in rm.scope_metrics
        for m in sm.metrics
        if m.name == "mhai_cloud_alerts_total"
        for p in m.data.data_points
    ]
    assert [(p.attributes["kind"], p.value) for p in pts] == [("unconfigured", 1)]


def test_runquery_accepts_plain_client() -> None:
    from intelligence.boundary.jobs.run_query import RunQuery
    from intelligence.boundary.models import ModelRouter

    stub = StubModelClient()
    rq = RunQuery(None, stub)
    try:
        assert isinstance(rq._router, ModelRouter)
        assert rq._router.client_for("local") == (stub, "local")
        # A plain client has no cloud client: a cloud row is unconfigured.
        assert rq._router.client_for("cloud:anthropic")[0] is not stub
    finally:
        rq.close()


def test_build_model_router_without_cloud_is_the_local_client() -> None:
    from intelligence.boundary.models import ModelRouter, build_model_router

    s = WorkerSettings.from_env({**BASE_ENV, "INTELLIGENCE_MODEL_CLIENT": "stub"})
    router = build_model_router(s)
    assert isinstance(router, ModelRouter) and isinstance(router.local, StubModelClient)
    assert type(router.client_for("cloud:anthropic")[0]).__name__ == "_Unconfigured"


def test_stub_parts_retrieve_shared_only_and_verify_like_the_stub() -> None:
    from intelligence.boundary.models import _verify_with, stub_parts

    build, versions = stub_parts()
    assert versions == {"dense": "stub", "nli": "stub"}
    patient = KBChunk("KB#0009", "synthetic lab fluids", "lab", "P-SYN-1", "")
    orch = build([_chunk(), patient])
    results = orch.rag_cca.retriever.retrieve("fluids guideline", top_k=5, patient_id=None)
    assert [r.chunk.chunk_id for r in results] == ["KB#0001"]
    g = Generation(question="q", response="r", available=True, handle=orch)
    v = _verify_with(orch, g)
    assert v.verified is True and v.decision == "PASS" and v.trust_score == 0.9


def test_plain_tree_drops_the_tag_and_copies() -> None:
    from intelligence.boundary.cloud.gate import TaggingMasker
    from intelligence.boundary.jobs.run_query import _plain_tree

    m = TaggingMasker()
    tree = {"a": m.mask("x"), "b": [m.mask("y"), 1, None, {"c": 2.5}], "d": True}
    plain = _plain_tree(tree)
    assert plain == {"a": "x", "b": ["y", 1, None, {"c": 2.5}], "d": True}
    assert type(plain["a"]) is str and type(plain["b"][0]) is str
    assert plain["b"] is not tree["b"] and plain["b"][3] is not tree["b"][3]
