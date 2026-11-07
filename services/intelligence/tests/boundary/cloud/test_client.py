"""CloudModelClient (design sections 3 to 11; plan Task 9).

Local retrieval and verification around a gated, scanned, budgeted cloud
draft. FakeAdapter POSTs the real rendering of the CloudPrompt through the
client it is given, so the scan hook and the capturing transport see real
bytes. No socket is opened. Every name, id and text here is synthetic.
"""

from __future__ import annotations

import json
import logging
import ssl
import threading
from collections.abc import Iterator, Sequence
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import certifi
import httpx2
import pytest
from opentelemetry import propagate
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider

from intelligence.boundary import jobs_repo, kb_repo, telemetry
from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud import scan, vocab
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS, Reservation
from intelligence.boundary.cloud.client import (
    CloudCall,
    CloudModelClient,
    CloudReply,
    build_cloud_clients,
    tls_context,
)
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT, MaskedText
from intelligence.boundary.cloud.retry import RETRY, RetryPolicy
from intelligence.boundary.cloud.scan import Identifiers, ScanHook
from intelligence.boundary.cloud.settings import CloudSettings, ProviderSettings
from intelligence.boundary.config import ConfigError
from intelligence.boundary.models import Generation
from intelligence.knowledge_base.kb_builder import KBChunk, RetrievalResult
from tests.boundary.cloud.conftest import (
    BASE_URL,
    FAKE_KEY,
    MODEL,
    CapturingTransport,
    FakeAdapter,
    FakeConnectionError,
    ScriptedTransport,
)

pytestmark = pytest.mark.disable_socket

QUESTION = "Synthetic question: what is the first-line fluid for a synthetic fever?"
NAME = "Adaeze Okafor"
CHUNK_TEXT = "Synthetic guideline text: give oral fluids and review in 24 hours."


# --- fakes -----------------------------------------------------------------------------


class FakeRetriever:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.chunks = list(chunks)
        self.calls: list[dict[str, Any]] = []

    def retrieve(
        self, query: str, top_k: int = 5, patient_id: str | None = None
    ) -> list[RetrievalResult]:
        self.calls.append({"query": query, "top_k": top_k, "patient_id": patient_id})
        return [RetrievalResult(c, 1.0, "fake", i) for i, c in enumerate(self.chunks[:top_k])]


class NoGenerator:
    """Anything that would generate locally fails the test."""

    def __getattr__(self, name: str) -> Any:
        raise AssertionError(f"the local generator was used: {name}")


class FakeCCA:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.retriever = FakeRetriever(chunks)
        self.generator = NoGenerator()

    def generate_with_rag(self, *args: Any, **kwargs: Any) -> Any:
        raise AssertionError("generate_with_rag was called on the cloud path")


class PassingHGA:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        self.calls.append({"response_text": response_text, **kwargs})
        return {
            "trust_score": 0.91,
            "decision": "pass",
            "nli_score": 0.9,
            "flags": [],
            "weights": {"alpha": 0.35},
        }


class Builder:
    def __init__(self) -> None:
        self.built: list[Any] = []

    def __call__(self, chunks: Sequence[KBChunk]) -> Any:
        from intelligence.agents.orchestrator import DynamicOrchestratorAgent

        orch = DynamicOrchestratorAgent(rag_cca=FakeCCA(chunks), hga=PassingHGA())
        self.built.append(orch)
        return orch


class FakeBudget:
    def __init__(self, refuse: bool = False) -> None:
        self.refuse = refuse
        self.reserved: list[tuple[str, int]] = []
        self.settled: list[tuple[str, int | None]] = []
        self.lock = threading.Lock()

    def reserve(self, provider: str, tokens: int) -> Reservation | None:
        with self.lock:
            self.reserved.append((provider, tokens))
            if self.refuse:
                return None
            return Reservation(f"r{len(self.reserved)}", tokens, 10)

    def settle(self, r: Reservation, used: int | None) -> None:
        with self.lock:
            self.settled.append((r.id, used))


class Clock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


def shared_chunks(n: int = 3, text: str = CHUNK_TEXT) -> list[KBChunk]:
    return [
        kb_repo._chunk((f"KB#{i:04d}", True, f"{text} ({i})", "guideline", None, None, {}))
        for i in range(1, n + 1)
    ]


def provider() -> ProviderSettings:
    return ProviderSettings("anthropic", MODEL, ApiKey(FAKE_KEY), None, BASE_URL)


def make(
    adapter: FakeAdapter | None = None,
    transport: CapturingTransport | None = None,
    **kw: Any,
) -> tuple[CloudModelClient, FakeAdapter, CapturingTransport, Builder, list[float]]:
    adapter = adapter or FakeAdapter()
    transport = transport or CapturingTransport()
    builder = Builder()
    sleeps: list[float] = []
    kw.setdefault("sleep", sleeps.append)
    kw.setdefault("policy", RetryPolicy(rng=__import__("random").Random(1)))
    c = CloudModelClient(
        provider(),
        adapter,
        builder,
        {"cca": "local-gen", "dense": "dense-1", "nli": "nli-1"},
        transport,
        **kw,
    )
    return c, adapter, transport, builder, sleeps


def call_for(
    budget: FakeBudget | None = None,
    ids: Identifiers | None = None,
    deadline: float = 1e12,
) -> tuple[CloudCall, FakeBudget]:
    budget = budget or FakeBudget()
    ids = ids if ids is not None else Identifiers(frozenset({"adaeze okafor", "adaeze", "okafor"}))
    return CloudCall(ids, budget, deadline), budget


def run(
    c: CloudModelClient,
    call: CloudCall | None,
    question: str = QUESTION,
    context: dict[str, Any] | None = None,
    chunks: list[KBChunk] | None = None,
) -> Generation:
    masked = c.mask(question)
    if context is None:
        context = {"disease_category": c.mask("fever"), "risk_score": 0.4}
    return c.generate(
        masked, context, None, chunks if chunks is not None else shared_chunks(), call=call
    )


def doc(body: bytes) -> dict[str, Any]:
    out: dict[str, Any] = json.loads(body)
    return out


@pytest.fixture
def metrics() -> Iterator[InMemoryMetricReader]:
    reader = InMemoryMetricReader()
    stop = telemetry.setup(metric_reader=reader)
    yield reader
    stop()


def points(reader: InMemoryMetricReader, name: str) -> list[Any]:
    data = reader.get_metrics_data()
    out: list[Any] = []
    for rm in data.resource_metrics if data else ():
        for sm in rm.scope_metrics:
            for m in sm.metrics:
                if m.name == name:
                    out += list(m.data.data_points)
    return out


# --- the answer ---------------------------------------------------------------------


def test_ok_reply_is_verified_locally() -> None:
    c, adapter, transport, builder, _ = make()
    call, budget = call_for()
    g = run(c, call)
    assert g.available is True
    assert g.cloud_outcome == "ok"
    assert g.egress == "anthropic"
    assert g.response == "Synthetic answer [KB#0001]."
    assert g.cited_chunk_ids == ("KB#0001",)
    assert g.retrieved_chunk_ids == ("KB#0001", "KB#0002", "KB#0003")
    assert g.model_used == f"anthropic/{MODEL}"
    assert dict(g.versions) == {
        "cca": f"anthropic/{MODEL}@synthetic-2026-11-01",
        "cca_route": "cloud",
    }
    assert (g.tokens_in, g.tokens_out) == (120, 40)
    assert g.patient_id is None
    assert c.model_versions() == {"dense": "dense-1", "nli": "nli-1"}
    v = c.verify(g)
    assert v.verified is True and v.decision == "PASS" and v.trust_score == 0.91
    hga = builder.built[0].hga
    assert hga.calls[0]["response_text"] == "Synthetic answer [KB#0001]."
    assert hga.calls[0]["patient_id"] is None
    # One body sent, one reservation, settled with the reported use.
    assert len(transport.bodies) == 1
    assert budget.settled == [("r1", 160)]


def test_never_calls_generate_with_rag_or_the_generator() -> None:
    c, _, _, builder, _ = make()
    call, _ = call_for()
    g = run(c, call)
    assert g.available is True  # FakeCCA would have failed the test otherwise
    retriever = builder.built[0].rag_cca.retriever
    assert retriever.calls == [{"query": c.mask(QUESTION), "top_k": 5, "patient_id": None}]


FAILURES = [o for o in vocab.OUTCOMES if o != "ok"]


@pytest.mark.parametrize("outcome", FAILURES)
def test_every_failure_escalates_without_score(outcome: str) -> None:
    # A12: every outcome but ok escalates, with no score and no versions.
    c, _, _, _, _ = make(FakeAdapter([outcome]))
    call, budget = call_for()
    g = run(c, call)
    assert g.available is False
    assert g.failed_agents == ("CCA",)
    assert g.cloud_outcome == outcome
    assert dict(g.versions) == {}
    assert g.response == ""
    v = c.verify(g)
    assert v.decision == "ESCALATE"
    assert v.verified is False
    assert v.trust_score is None
    assert v.degraded is True
    assert len(budget.reserved) == len(budget.settled)


def test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried() -> None:
    # Review Focus 2 and K11: the SDK wraps the hook's refusal in its own
    # connection error; the hook's verdict wins and nothing is retried.
    adapter = FakeAdapter(wrap=True, headers=lambda: {"traceparent": "00-" + "1" * 32 + "-01"})
    c, adapter, transport, _, sleeps = make(adapter)
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "egress_blocked"
    assert g.egress == "none"
    assert transport.bodies == []
    assert len(budget.reserved) == 1  # one attempt only
    assert budget.settled == [("r1", 0)]  # nothing sent
    assert sleeps == []


def test_retries_only_retryable_then_unavailable() -> None:
    adapter = FakeAdapter([RETRY, RETRY, RETRY], status=503)
    c, adapter, transport, _, sleeps = make(adapter)
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "unavailable"
    assert g.egress == "anthropic"
    assert len(transport.bodies) == 3
    assert len(budget.reserved) == 3
    assert [r for r, _ in budget.settled] == ["r1", "r2", "r3"]
    assert len(sleeps) == 2
    assert 0 <= sleeps[0] <= 1.0 and 0 <= sleeps[1] <= 3.0


@pytest.mark.parametrize("final", ["refused", "truncated", "malformed", "auth_failed"])
def test_final_outcomes_are_not_retried(final: str) -> None:
    c, _, transport, _, sleeps = make(FakeAdapter([final, "ok"]))
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == final
    assert len(transport.bodies) == 1 and len(budget.reserved) == 1 and sleeps == []


def test_retry_then_success() -> None:
    c, _, transport, _, _ = make(FakeAdapter([RETRY, "ok"]))
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "ok"
    assert len(transport.bodies) == 2
    assert len(budget.settled) == 2


def test_reservation_settled_on_every_failure_path() -> None:
    # Review Focus 4: an exception in adapter.call, a timeout, a malformed
    # reply: each reservation settled exactly once.
    clock = Clock()

    class Boom(FakeAdapter):
        def call(self, sdk: Any, prompt: Any, model: str, timeout: float) -> Any:
            out = super().call(sdk, prompt, model, timeout)
            if self.sends:
                raise RuntimeError("synthetic adapter failure")
            return out

    c, _, _, _, _ = make(Boom())
    call, budget = call_for()
    assert run(c, call).cloud_outcome == "unavailable"
    assert budget.settled == [("r1", None)]  # sent, reply unknown: all of it

    class Slow(FakeAdapter):
        def call(self, sdk: Any, prompt: Any, model: str, timeout: float) -> Any:
            out = super().call(sdk, prompt, model, timeout)
            if self.sends:
                clock.now += 1000  # the stage's time runs out mid-call
                raise httpx2.ReadTimeout("synthetic")
            return out

    c, _, _, _, _ = make(Slow(), clock=clock)
    call, budget = call_for(deadline=clock.now + 100)
    assert run(c, call).cloud_outcome == "timeout"
    assert budget.settled == [("r1", None)]

    c, _, _, _, _ = make(FakeAdapter(["malformed"]))
    call, budget = call_for()
    assert run(c, call).cloud_outcome == "malformed"
    assert budget.settled == [("r1", 100)]


def test_budget_exhausted_sends_nothing() -> None:
    c, _, transport, _, _ = make()
    call, budget = call_for(FakeBudget(refuse=True))
    g = run(c, call)
    assert g.cloud_outcome == "budget_exhausted"
    assert g.egress == "none"
    assert transport.bodies == []
    assert budget.settled == []


def test_reservation_is_the_body_bytes_plus_the_output_cap() -> None:
    # Ruling C7: UTF-8 bytes of the exact body, plus the cap the body sends.
    c, _, transport, _, _ = make()
    call, budget = call_for()
    run(c, call)
    body = transport.bodies[0]
    assert budget.reserved == [("anthropic", len(body) + MAX_OUTPUT_TOKENS)]
    assert doc(body)["max_tokens"] == MAX_OUTPUT_TOKENS


@pytest.mark.parametrize("model_id", ["x y", "<script>", "", "a" * 200, "id\n2"])
def test_unsafe_model_id_is_malformed(model_id: str) -> None:
    from intelligence.boundary.cloud.reply import validate_answer

    answer = validate_answer(
        {"answer": "A [KB#0001].", "citations": ["KB#0001"], "insufficient_evidence": False},
        frozenset({"KB#0001"}),
    )
    c, _, _, _, _ = make(FakeAdapter([CloudReply(answer, model_id, 1, 1, "ok")]))
    call, _ = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "malformed"
    assert dict(g.versions) == {}


def test_citation_outside_the_sent_chunks_is_malformed() -> None:
    from intelligence.boundary.cloud.reply import validate_answer

    answer = validate_answer(
        {"answer": "A [KB#0009].", "citations": ["KB#0009"], "insufficient_evidence": False},
        frozenset({"KB#0009"}),
    )
    c, _, _, _, _ = make(FakeAdapter([CloudReply(answer, "m-1", 1, 1, "ok")]))
    call, _ = call_for()
    assert run(c, call).cloud_outcome == "malformed"


def test_insufficient_evidence_is_insufficient() -> None:
    c, _, _, _, _ = make(FakeAdapter(["insufficient"]))
    call, _ = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "insufficient" and g.available is False
    assert g.egress == "anthropic"


def test_patient_id_or_no_call_refused() -> None:
    c, adapter, transport, builder, _ = make()
    call, budget = call_for()
    masked = c.mask(QUESTION)
    ctx = {"risk_score": 0.4}
    assert c.generate(masked, ctx, None, shared_chunks()).cloud_outcome == "egress_blocked"
    g = c.generate(masked, ctx, "patient-1", shared_chunks(), call=call)
    assert g.cloud_outcome == "egress_blocked"
    assert builder.built == [] and adapter.calls == 0 and transport.bodies == []
    assert budget.reserved == []


def test_unmasked_question_or_unsealed_chunk_refused() -> None:
    c, adapter, transport, _, _ = make()
    call, _ = call_for()
    g = c.generate(QUESTION, {}, None, shared_chunks(), call=call)  # a plain str
    assert g.cloud_outcome == "egress_blocked"
    hospital = KBChunk("KB#0001", "hospital text", "guideline", "reference", "")
    g = run(c, call, chunks=[hospital])
    assert g.cloud_outcome == "egress_blocked"
    assert transport.bodies == [] and adapter.sends == 0


def test_prompt_holds_only_allowed_parts() -> None:
    c, _, transport, _, _ = make()
    call, _ = call_for()
    context = {
        "disease_category": c.mask("fever"),
        "risk_score": 0.4,
        "name": c.mask("SYNTHETICNAMEFIELD"),
        "mrn": 284817,
        "history": c.mask("SYNTHETICHISTORYTEXT"),
    }
    run(c, call, context=context, chunks=shared_chunks(7))
    body = doc(transport.bodies[0])
    assert set(body) == {"model", "max_tokens", "system", "messages", "output_config"}
    assert body["system"] == SYSTEM_PROMPT
    texts = [b["text"] for b in body["messages"][0]["content"]]
    assert texts[0] == str(c.mask(QUESTION))
    assert texts[1] == "Category: fever\nRisk: 0.4"
    assert len(texts) == 2 + 5  # at most five chunks
    for i, t in enumerate(texts[2:], start=1):
        assert t.startswith(f"<<chunk KB#{i:04d}>>\n") and t.endswith("\n<<end>>")
    raw = transport.bodies[0].decode()
    for absent in ("SYNTHETICNAMEFIELD", "SYNTHETICHISTORYTEXT", "284817"):
        assert absent not in raw


def test_per_call_state_not_on_client() -> None:
    # K10: concurrent calls with different identifiers block on their own only.
    c, _, _, _, _ = make()
    question = f"Synthetic question about {NAME} and fluids"
    blocked = Identifiers(frozenset({"okafor"}))
    other = Identifiers(frozenset({"chukwuemeka"}))

    def one(i: int) -> str | None:
        ids = blocked if i % 2 else other
        g = run(c, CloudCall(ids, FakeBudget(), 1e12), question=question)
        return g.cloud_outcome

    with ThreadPoolExecutor(8) as pool:
        out = list(pool.map(one, range(32)))
    assert all(o == "egress_blocked" for o in out[1::2])
    assert all(o == "ok" for o in out[0::2])
    assert "identifiers" not in vars(c) and not any(
        isinstance(v, Identifiers | CloudCall) for v in vars(c).values()
    )


def test_egress_none_when_nothing_sent() -> None:
    for adapter, budget in (
        (FakeAdapter(), FakeBudget(refuse=True)),
        (FakeAdapter(wrap=True, headers=lambda: {"baggage": "x=1"}), FakeBudget()),
    ):
        c, _, transport, _, _ = make(adapter)
        g = run(c, CloudCall(Identifiers(frozenset()), budget, 1e12))
        assert g.egress == "none" and transport.bodies == []


def test_dry_render_sends_nothing_and_prescans() -> None:
    c, adapter, transport, _, _ = make()
    call, budget = call_for()
    g = run(c, call, question=f"Is {NAME} febrile?")
    assert g.cloud_outcome == "egress_blocked"
    assert adapter.calls == 1  # the dry render only
    assert transport.bodies == []
    assert budget.reserved == []  # refused before any reservation


def test_render_failed_is_egress_blocked() -> None:
    class NoPost(FakeAdapter):
        def call(self, sdk: Any, prompt: Any, model: str, timeout: float) -> Any:
            raise RuntimeError("never builds a request")

    c, _, transport, _, _ = make(NoPost())
    call, budget = call_for()
    assert run(c, call).cloud_outcome == "egress_blocked"
    assert budget.reserved == [] and transport.bodies == []


def test_scan_hook_is_the_only_request_hook() -> None:
    c, adapter, _, _, _ = make()
    call, _ = call_for()
    run(c, call)
    render_hooks, send_hooks = adapter.hooks
    assert render_hooks == []
    assert len(send_hooks) == 1 and type(send_hooks[0]) is ScanHook


def test_shared_transport_survives_attempts() -> None:
    # M8: closing each attempt's client never closes the shared pool.
    c, _, transport, _, _ = make(FakeAdapter([RETRY, RETRY, "ok", "ok"]))
    call, _ = call_for()
    assert run(c, call).cloud_outcome == "ok"
    assert transport.closed is False
    assert run(c, call).cloud_outcome == "ok"
    assert len(transport.bodies) == 4
    c.close()
    assert transport.closed is True


def test_auth_failed_alerts(
    caplog: pytest.LogCaptureFixture, metrics: InMemoryMetricReader
) -> None:
    c, _, _, _, _ = make(FakeAdapter(["auth_failed"], status=401))
    call, _ = call_for()
    with caplog.at_level(logging.DEBUG):
        g = run(c, call)
    assert g.cloud_outcome == "auth_failed"
    records = [r for r in caplog.records if "cloud.auth_failed" in r.getMessage()]
    assert len(records) == 1 and records[0].levelno == logging.ERROR
    assert records[0].getMessage() == "cloud.auth_failed provider=anthropic status=401"
    for r in caplog.records:
        assert FAKE_KEY not in r.getMessage() and QUESTION not in r.getMessage()
    pts = points(metrics, "mhai_cloud_alerts_total")
    assert [(p.attributes["provider"], p.attributes["kind"], p.value) for p in pts] == [
        ("anthropic", "auth_failed", 1)
    ]


def test_context_dropped_is_counted(metrics: InMemoryMetricReader) -> None:
    c, _, _, _, _ = make()
    call, _ = call_for()
    run(c, call, context={"risk_score": 0.4, "history": c.mask("x"), "allergies": []})
    assert sum(p.value for p in points(metrics, "mhai_cloud_context_dropped_total")) == 2


def test_fake_adapter_bodies_are_captured() -> None:
    c, adapter, transport, _, _ = make(FakeAdapter([RETRY, "ok"]))
    call, _ = call_for()
    run(c, call)
    assert adapter.sends == 2 and len(transport.bodies) == 2
    assert transport.bodies[0] == transport.bodies[1]
    assert scan.scan_body(transport.bodies[0], call.identifiers, adapter.shape) is None


def test_worst_case_question_length(capsys: pytest.CaptureFixture[str]) -> None:
    # A question of MAX_MASKED_QUESTION characters, all non-ASCII, with five
    # 2,000-character chunks: json's ensure_ascii writes each as \uXXXX, so
    # the body is over MAX_BODY_BYTES and is refused (too_large), never sent.
    word = "\u00e9\u00e8\u00ea\u00eb\u00e0\u00e2\u00ee\u00ef\u00f4\u00fb\u00e7\u00a0"
    long = (word * jobs_repo.MAX_MASKED_QUESTION)[: jobs_repo.MAX_MASKED_QUESTION]
    chunks = shared_chunks(5, text="g" * 1990)
    assert all(len(c.content) <= 2000 for c in chunks)
    c, _, transport, _, _ = make()
    call, budget = call_for(ids=Identifiers(frozenset()))
    assert run(c, call, question=long, chunks=chunks).cloud_outcome == "egress_blocked"
    assert transport.bodies == [] and budget.reserved == []
    # The longest such question still sent (recorded, not a limit).
    lo, hi = 0, len(long)
    while lo < hi:
        mid = (lo + hi + 1) // 2
        c2, _, _, _, _ = make()
        call2, _ = call_for(ids=Identifiers(frozenset()))
        if run(c2, call2, question=long[:mid].strip() or "x", chunks=chunks).available:
            lo = mid
        else:
            hi = mid - 1
    assert 1000 < lo < jobs_repo.MAX_MASKED_QUESTION
    with capsys.disabled():
        print(f"\nlongest non-ASCII question still sent with five full chunks: {lo} chars")


def test_tls_context_ignores_ssl_cert_env(monkeypatch: pytest.MonkeyPatch) -> None:
    base = tls_context().get_ca_certs()
    monkeypatch.setenv("SSL_CERT_FILE", certifi.where())
    monkeypatch.setenv("SSL_CERT_DIR", "/nonexistent-ca-dir")
    # The variable would matter to a default context: it loads that bundle.
    assert ssl.create_default_context().get_ca_certs() != base
    ctx = tls_context()
    assert ctx.get_ca_certs() == base
    assert ctx.check_hostname is True
    assert ctx.verify_mode == ssl.CERT_REQUIRED
    assert ctx.minimum_version >= ssl.TLSVersion.TLSv1_2

    seen: dict[str, Any] = {}
    real = httpx2.HTTPTransport

    def spy(**kw: Any) -> httpx2.HTTPTransport:
        seen.update(kw)
        return real(**kw)

    monkeypatch.setattr(client_mod.httpx2, "HTTPTransport", spy)
    monkeypatch.setitem(client_mod.ADAPTERS, "anthropic", lambda model: FakeAdapter())
    settings = CloudSettings(providers=(provider(),), proxy_url="http://egress-proxy:3128")
    clients = build_cloud_clients(settings, Builder(), {"cca": "x", "nli": "n"})
    assert set(clients) == {"anthropic"}
    assert seen["trust_env"] is False and seen["retries"] == 0
    assert seen["proxy"] == "http://egress-proxy:3128"
    verify = seen["verify"]
    assert isinstance(verify, ssl.SSLContext) and verify.get_ca_certs() == base
    clients["anthropic"].close()


def test_build_cloud_clients_refuses_a_provider_without_adapter(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(client_mod, "ADAPTERS", {})
    settings = CloudSettings(providers=(provider(),), proxy_url="http://egress-proxy:3128")
    with pytest.raises(ConfigError):
        build_cloud_clients(settings, Builder(), {})
    assert build_cloud_clients(CloudSettings(), Builder(), {}) == {}


def test_no_trace_context_sent() -> None:
    # A real tracer with a span current: nothing adds trace context to the
    # request, and an adapter that did would be refused by the hook.
    tracer = TracerProvider().get_tracer("test")
    c, _, transport, _, _ = make()
    call, _ = call_for()
    with tracer.start_as_current_span("job"):
        assert run(c, call).cloud_outcome == "ok"
    for h in transport.headers:
        assert not {"traceparent", "tracestate", "baggage"} & {k.lower() for k in h}

    def inject() -> dict[str, str]:
        carrier: dict[str, str] = {}
        propagate.inject(carrier)
        return carrier

    c, _, transport, _, _ = make(FakeAdapter(headers=inject, wrap=True))
    with tracer.start_as_current_span("job"):
        g = run(c, call)
    assert g.cloud_outcome == "egress_blocked" and transport.bodies == []


def test_stage_deadline_passed_is_timeout() -> None:
    clock = Clock()
    c, adapter, transport, _, _ = make(clock=clock)
    call, budget = call_for(deadline=clock.now - 1)
    assert run(c, call).cloud_outcome == "timeout"
    assert adapter.calls == 0 and budget.reserved == []


def test_outcomes_stay_in_the_closed_set() -> None:
    for item in ("surprise", RETRY, "ok"):
        script: list[Any] = [item] * 3 if item != "ok" else ["ok"]
        c, _, _, _, _ = make(FakeAdapter(script))
        call, _ = call_for()
        g = run(c, call)
        assert g.cloud_outcome in vocab.OUTCOMES
        assert g.egress in vocab.EGRESS


def test_cloud_call_repr_and_types() -> None:
    call, _ = call_for()
    assert repr(call) == "CloudCall(<per-job>)"
    with pytest.raises(TypeError):
        CloudCall(frozenset({"x"}), FakeBudget(), 1.0)  # type: ignore[arg-type]
    with pytest.raises(TypeError):
        CloudCall(Identifiers(frozenset()), FakeBudget(), float("nan"))


def test_mask_gives_masked_text() -> None:
    c, _, _, _, _ = make()
    assert type(c.mask(QUESTION)) is MaskedText


def test_wrapped_connection_error_is_retried() -> None:
    transport = ScriptedTransport([httpx2.ConnectError("synthetic"), 200])
    c, _, _, _, sleeps = make(FakeAdapter(wrap=True), transport=transport)
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "ok"
    assert len(budget.reserved) == 2 and len(sleeps) == 1
    assert isinstance(FakeConnectionError(), Exception)  # the SDK's own type


# --- PR C hardening -------------------------------------------------------------------


def _hand_built(answer: str, citations: tuple[str, ...]) -> Any:
    """An Answer that never went through validate_answer."""
    from intelligence.boundary.cloud.reply import Answer

    return Answer.model_construct(answer=answer, citations=citations, insufficient_evidence=False)


@pytest.mark.parametrize(
    ("text", "citations"),
    [
        # A citation that was never sent.
        ("A [KB#0009].", ("KB#0009",)),
        # A sent citation, but the text's marker names one that wasn't.
        ("A [KB#0009].", ("KB#0001",)),
        # A sent citation and a link the adapter never refused.
        ("A [KB#0001] https://example.invalid/x", ("KB#0001",)),
    ],
)
def test_adapter_answer_is_rechecked_against_sent_ids(
    text: str, citations: tuple[str, ...]
) -> None:
    # The client never trusts an adapter's Answer as given.
    c, _, _, _, _ = make(FakeAdapter([CloudReply(_hand_built(text, citations), "m-1", 1, 1, "ok")]))
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "malformed"
    assert g.available is False and g.response == ""
    assert len(budget.settled) == 1


def test_ok_reply_uses_the_rechecked_answer() -> None:
    c, _, _, _, _ = make(
        FakeAdapter([CloudReply(_hand_built("A [KB#0002].", ("KB#0002",)), "m-1", 1, 1, "ok")])
    )
    call, _ = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "ok"
    assert g.response == "A [KB#0002]." and g.cited_chunk_ids == ("KB#0002",)


def test_reservation_settled_when_the_scan_hook_cannot_be_built(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def broken(*args: Any, **kwargs: Any) -> Any:
        raise TypeError("synthetic: the hook can't be built")

    c, _, transport, _, sleeps = make()
    monkeypatch.setattr(client_mod, "ScanHook", broken)
    call, budget = call_for()
    g = run(c, call)
    assert g.cloud_outcome == "egress_blocked"
    assert g.egress == "none"
    assert transport.bodies == []
    assert budget.reserved and budget.settled == [("r1", 0)]
    assert sleeps == []


class DripStream(httpx2.SyncByteStream):
    """A body that drips one byte per read, each read one second apart."""

    def __init__(self, clock: Clock, n: int) -> None:
        self.clock = clock
        self.n = n
        self.read = 0

    def __iter__(self) -> Iterator[bytes]:
        for _ in range(self.n):
            self.clock.now += 1.0
            self.read += 1
            yield b" "


class DripTransport(CapturingTransport):
    def __init__(self, clock: Clock, n: int) -> None:
        super().__init__()
        self.clock = clock
        self.n = n
        self.streams: list[DripStream] = []

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        s = DripStream(self.clock, self.n)
        self.streams.append(s)
        return httpx2.Response(200, stream=s, request=request)


def test_a_dripping_reply_stops_at_the_stage_deadline() -> None:
    # Each read is quick, so no per-read timeout fires; the attempt's
    # overall deadline over the whole body does.
    clock = Clock()
    transport = DripTransport(clock, n=10_000)
    c, _, _, _, _ = make(transport=transport, clock=clock)
    call, budget = call_for(deadline=clock.now + 30)
    g = run(c, call)
    assert g.cloud_outcome == "timeout"
    assert g.egress == "anthropic"
    assert len(transport.streams) == 1
    assert transport.streams[0].read <= 31
    assert clock.now <= call.stage_deadline + 1
    assert budget.settled == [("r1", None)]


def test_adapters_turn_sdk_retries_off() -> None:
    # Tasks 11 to 13: retrying is the client's (RetryPolicy, under the
    # budget and the stage deadline); an SDK's own retries would resend a
    # body with no reservation. Every registered adapter's SDK client must
    # have max_retries == 0, and the registry really holds the adapters, so
    # this is not vacuous.
    assert set(client_mod.ADAPTERS) == set(vocab.PROVIDERS)
    with httpx2.Client(transport=CapturingTransport()) as http:
        for name, factory in client_mod.ADAPTERS.items():
            adapter = factory(MODEL)
            assert adapter.name == name
            sdk = adapter.make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[name])
            assert getattr(sdk, "max_retries", None) == 0, name


# --- review I1: a reported use above the reservation ------------------------------


class FixedBudget(FakeBudget):
    """Reserves exactly 1,000 tokens, whatever was asked."""

    def reserve(self, provider: str, tokens: int) -> Reservation | None:
        with self.lock:
            self.reserved.append((provider, tokens))
            return Reservation(f"r{len(self.reserved)}", 1000, 10)


def _used(tokens_in: int, tokens_out: int, outcome: str = "ok") -> CloudReply:
    from intelligence.boundary.cloud.reply import validate_answer

    doc = {"answer": "Synthetic answer [KB#0001].", "citations": ["KB#0001"]}
    answer = validate_answer({**doc, "insufficient_evidence": False}, frozenset({"KB#0001"}))
    ok = outcome == "ok"
    return CloudReply(answer if ok else None, "synthetic-1", tokens_in, tokens_out, outcome)


@pytest.mark.parametrize(
    ("tokens_out", "outcome", "counted", "alerted"),
    [
        (100, "ok", False, False),  # 900 in + 100 out = 1000: within
        (150, "ok", True, False),  # 1050: 5% over, counted only
        (200, "ok", True, False),  # 1100: exactly 10% over, counted only
        (201, "ok", True, True),  # 1101: above 10%, the provider is flagged
        (5000, "truncated", True, True),
    ],
)
def test_usage_above_the_reservation_is_logged_counted_and_flagged(
    caplog: pytest.LogCaptureFixture,
    metrics: InMemoryMetricReader,
    tokens_out: int,
    outcome: str,
    counted: bool,
    alerted: bool,
) -> None:
    # Core settles at most the reservation (00018: least(used, tokens)), so
    # the worker must say when a provider reported more: by outcome code and
    # token counts only.
    c, _, _, _, _ = make(FakeAdapter([_used(900, tokens_out, outcome)]))
    call, budget = call_for(FixedBudget())
    with caplog.at_level(logging.DEBUG):
        g = run(c, call)
    assert g.cloud_outcome == outcome
    assert budget.settled == [("r1", 900 + tokens_out)]
    over = [r for r in caplog.records if "cloud.usage_overrun" in r.getMessage()]
    pts = points(metrics, "mhai_cloud_usage_overrun_total")
    alerts = [
        p
        for p in points(metrics, "mhai_cloud_alerts_total")
        if p.attributes["kind"] == "usage_overrun"
    ]
    if not counted:
        assert over == [] and pts == [] and alerts == []
        return
    assert len(over) == 1
    msg = over[0].getMessage()
    assert msg == (
        f"cloud.usage_overrun provider=anthropic outcome={outcome} "
        f"reserved=1000 used={900 + tokens_out} over={tokens_out - 100}"
    )
    assert over[0].levelno == (logging.ERROR if alerted else logging.WARNING)
    assert [(p.attributes["provider"], p.value) for p in pts] == [("anthropic", 1)]
    assert [(p.attributes["provider"], p.value) for p in alerts] == (
        [("anthropic", 1)] if alerted else []
    )
    for r in caplog.records:
        assert QUESTION not in r.getMessage() and "Synthetic answer" not in r.getMessage()


def test_unknown_use_is_no_overrun(metrics: InMemoryMetricReader) -> None:
    # Without both counts the whole reservation is settled: nothing over.
    c, _, _, _, _ = make(FakeAdapter([CloudReply(None, "", None, 5000, "malformed")]))
    call, budget = call_for(FixedBudget())
    assert run(c, call).cloud_outcome == "malformed"
    assert budget.settled == [("r1", None)]
    assert points(metrics, "mhai_cloud_usage_overrun_total") == []
