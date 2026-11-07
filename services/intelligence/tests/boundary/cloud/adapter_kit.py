"""What the three adapter suites share (plan Tasks 11 to 13).

Each adapter is tested with its real SDK over fake transports: a transport
that answers from a synthetic fixture (tests/fixtures/cloud/<provider>/),
one that echoes the request back in a 401, and one that answers ok citing
the first chunk it was sent. Nothing opens a socket and no real key is
used: the key is FAKE_KEY. Every name, id and text is synthetic.

The checks themselves live here, as functions taking the provider's name
and its adapter factory, so the three suites run the same ones.
"""

from __future__ import annotations

import json
import logging
import random
import re
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx2
import pytest
from opentelemetry.sdk.trace import TracerProvider

from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud import retry, scan, vocab
from intelligence.boundary.cloud.apikey import ApiKey, quiet_sdk_loggers
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS, estimate_tokens
from intelligence.boundary.cloud.client import CloudModelClient, CloudReply
from intelligence.boundary.cloud.gate import (
    CloudContext,
    CloudPrompt,
    EgressRefused,
    TaggingMasker,
    shared_chunk_of,
)
from intelligence.boundary.cloud.retry import RetryPolicy
from intelligence.boundary.cloud.scan import Identifiers, ScanHook, scan_body
from intelligence.boundary.cloud.settings import ProviderSettings
from intelligence.boundary.models import Generation
from tests.boundary.cloud import test_client as tc
from tests.boundary.cloud.conftest import FAKE_KEY, MODEL, CapturingTransport
from tests.boundary.cloud.test_scan import SPLIT_NAMES

FIXTURES = Path(__file__).resolve().parents[2] / "fixtures" / "cloud"
QUESTION = tc.QUESTION
NAME = tc.NAME
IDS = Identifiers(frozenset({"adaeze okafor", "adaeze", "okafor"}))
Factory = Callable[[str], Any]

# The headers an SDK may send. Anything else (a trace header, a beta flag,
# a header an SDK update adds) fails the request test.
ALLOWED_HEADERS = re.compile(
    r"accept|accept-encoding|connection|content-length|content-type|host|user-agent"
    r"|x-stainless-[a-z-]+|anthropic-version|x-api-key|authorization"
)
CHUNK_IDS = re.compile(r"<<chunk (KB#[0-9]{4})>>")


# --- fixtures and transports -------------------------------------------------------------


def load(provider: str, name: str) -> dict[str, Any]:
    out: dict[str, Any] = json.loads((FIXTURES / provider / f"{name}.json").read_text("utf-8"))
    return out


def fixture_names(provider: str) -> list[str]:
    return sorted(p.stem for p in (FIXTURES / provider).glob("*.json"))


def respond(fx: dict[str, Any], request: httpx2.Request) -> httpx2.Response:
    return httpx2.Response(
        int(fx["status"]),
        headers=dict(fx.get("headers", {})),
        content=json.dumps(fx["body"]).encode("utf-8"),
        request=request,
    )


class FixtureTransport(CapturingTransport):
    """Answers every request with a fixture (the last one repeats) and
    records each request's URL."""

    def __init__(self, *fixtures: dict[str, Any]) -> None:
        super().__init__()
        self.fixtures = list(fixtures)
        self.urls: list[str] = []

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        self.urls.append(str(request.url))
        fx = self.fixtures.pop(0) if len(self.fixtures) > 1 else self.fixtures[0]
        return respond(fx, request)


class EchoTransport(CapturingTransport):
    """A 401 whose error message echoes the whole request: its body and
    its key header, as a careless provider might."""

    def __init__(self, provider: str) -> None:
        super().__init__()
        self.provider = provider

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        fx = load(self.provider, "unauthorized_401")
        echo = request.content.decode("utf-8") + " " + json.dumps(dict(request.headers))
        body = fx["body"]
        if "error" in body and isinstance(body["error"], dict):
            body["error"]["message"] = echo
        else:
            body["message"] = echo
        return respond(fx, request)


def ok_reply(provider: str, first: str | None) -> dict[str, Any]:
    """The ok fixture, citing first (or insufficient evidence with none)."""
    fx = load(provider, "ok")
    if first is None:
        text = json.dumps(
            {"answer": "Not in the chunks.", "citations": [], "insufficient_evidence": True}
        )
        old = json.dumps(
            {
                "answer": "Synthetic answer: give oral fluids [KB#0001].",
                "citations": ["KB#0001"],
                "insufficient_evidence": False,
            }
        )
        dumped = json.dumps(fx["body"]).replace(json.dumps(old)[1:-1], json.dumps(text)[1:-1])
    else:
        dumped = json.dumps(fx["body"]).replace("KB#0001", first)
    fx["body"] = json.loads(dumped)
    return fx


class ReplyingTransport(CapturingTransport):
    """Answers ok, citing the first chunk the request sent: what a provider
    would do, so whole jobs complete with the real adapter."""

    def __init__(self, provider: str) -> None:
        super().__init__()
        self.provider = provider

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        found = CHUNK_IDS.findall(request.content.decode("utf-8"))
        return respond(ok_reply(self.provider, found[0] if found else None), request)


# --- prompts, rendering and the client ------------------------------------------------------


def prompt(
    question: str = QUESTION,
    n: int = 3,
    chunk_text: str = tc.CHUNK_TEXT,
    category: str | None = "fever",
    score: float | None = 0.4,
) -> CloudPrompt:
    m = TaggingMasker()
    shared = tuple(shared_chunk_of(c) for c in tc.shared_chunks(n, chunk_text))
    ctx = CloudContext(None if category is None else m.mask(category), score)
    return CloudPrompt(m.mask(question), ctx, shared, m)


def render(
    provider: str, factory: Factory, p: CloudPrompt, model: str = MODEL
) -> tuple[httpx2.Request, CloudReply]:
    """The exact request the adapter's SDK sends for p, and the reply the
    adapter made of the ok fixture."""
    seen: list[httpx2.Request] = []

    def handler(request: httpx2.Request) -> httpx2.Response:
        request.read()
        seen.append(request)
        found = CHUNK_IDS.findall(request.content.decode("utf-8"))
        return respond(ok_reply(provider, found[0] if found else None), request)

    adapter = factory(model)
    with httpx2.Client(
        transport=httpx2.MockTransport(handler), trust_env=False, follow_redirects=False
    ) as http:
        sdk = adapter.make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider])
        reply = adapter.call(sdk, p, model, 30.0)
    assert len(seen) == 1, "the adapter sent exactly one request"
    return seen[0], reply


def body_of(provider: str, factory: Factory, p: CloudPrompt) -> bytes:
    return bytes(render(provider, factory, p)[0].content)


def make_client(
    provider: str, factory: Factory, transport: CapturingTransport, **kw: Any
) -> CloudModelClient:
    settings = ProviderSettings(provider, MODEL, ApiKey(FAKE_KEY), None, vocab.BASE_URLS[provider])
    kw.setdefault("sleep", lambda s: None)
    kw.setdefault("policy", RetryPolicy(rng=random.Random(1)))
    return CloudModelClient(
        settings,
        factory(MODEL),
        tc.Builder(),
        {"cca": "local-gen", "dense": "dense-1", "nli": "nli-1"},
        transport,
        **kw,
    )


def generate(
    c: CloudModelClient, question: str = QUESTION, ids: Identifiers = IDS
) -> tuple[Generation, tc.FakeBudget]:
    call, budget = tc.call_for(ids=ids)
    return tc.run(c, call, question), budget


# --- the shared checks ---------------------------------------------------------------------


def check_headers(request: httpx2.Request) -> None:
    names = {k.lower() for k in request.headers}
    assert all(ALLOWED_HEADERS.fullmatch(n) for n in names), sorted(names)
    assert not names & set(scan.TRACE_HEADERS)


def check_text_blocks_only(provider: str, doc: dict[str, Any], p: CloudPrompt) -> None:
    """The user content is the prompt's blocks, in order, as text parts
    with exactly the keys type and text: no image, document or file part
    can come out of any CloudPrompt."""
    pointer = scan.BLOCK_ARRAYS[provider].strip("/").split("/")
    node: Any = doc
    for part in pointer:
        node = node[int(part)] if isinstance(node, list) else node[part]
    assert isinstance(node, list)
    assert [b["text"] for b in node] == list(p.blocks())
    for b in node:
        assert set(b) == {"type", "text"} and b["type"] == scan.BLOCK_TYPES[provider]


def check_fixture_outcome(provider: str, factory: Factory, name: str, expect: str) -> None:
    transport = FixtureTransport(load(provider, name))
    c = make_client(provider, factory, transport)
    g, budget = generate(c)
    assert g.cloud_outcome == expect, (name, g.cloud_outcome)
    assert g.cloud_outcome in vocab.OUTCOMES
    attempts = 3 if retry.classify(int(load(provider, name)["status"])) == retry.RETRY else 1
    assert len(transport.bodies) == attempts, name
    assert len(budget.settled) == attempts
    if expect == "ok":
        assert g.available is True and g.cited_chunk_ids == ("KB#0001",)
        assert g.versions["cca"].startswith(f"{provider}/{MODEL}@")
        assert g.egress == provider and g.tokens_in > 0 and g.tokens_out > 0
    else:
        assert g.available is False and g.failed_agents == ("CCA",)
        assert g.versions == {} and g.egress == provider


def check_redirect_not_followed(provider: str, factory: Factory) -> None:
    fx = load(provider, "redirect_301")
    transport = FixtureTransport(fx)
    c = make_client(provider, factory, transport)
    g, budget = generate(c)
    assert g.available is False and g.cloud_outcome == "unavailable"
    assert len(transport.urls) == 1, "the redirect was followed or retried"
    assert transport.urls[0].startswith(vocab.BASE_URLS[provider])
    assert fx["headers"]["location"] not in transport.urls
    assert len(budget.settled) == 1


def check_error_logged_by_type_and_status_only(
    provider: str, factory: Factory, caplog: pytest.LogCaptureFixture
) -> None:
    quiet_sdk_loggers()  # as the worker does at start
    caplog.set_level(logging.DEBUG)
    transport = EchoTransport(provider)
    c = make_client(provider, factory, transport)
    g, _ = generate(c)
    assert g.cloud_outcome == "auth_failed"
    # The provider really did echo the request back.
    assert len(transport.bodies) == 1
    assert "cloud.auth_failed" in caplog.text and "status=401" in caplog.text
    for secret in (FAKE_KEY, "first-line fluid", tc.CHUNK_TEXT[:30]):
        assert secret not in caplog.text
        assert secret not in repr(g)
    # The adapter's own reply holds nothing of the echoed error either.
    with httpx2.Client(transport=EchoTransport(provider), trust_env=False) as http:
        adapter = factory(MODEL)
        sdk = adapter.make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider])
        reply = adapter.call(sdk, prompt(), MODEL, 30.0)
    assert reply.outcome == "auth_failed" and reply.status == 401 and reply.answer is None
    assert FAKE_KEY not in repr(reply) and "first-line" not in repr(reply)


def check_hook_refusal(
    provider: str, factory: Factory, monkeypatch: pytest.MonkeyPatch, wrapped: bool
) -> None:
    """The scan hook's refusal, through the real SDK: egress_blocked, never
    unavailable, and never retried. The dry render's pre-scan is skipped
    here, so the hook is what refuses."""
    adapter = factory(MODEL)
    hook = ScanHook(IDS, adapter.shape)
    transport = CapturingTransport()
    with httpx2.Client(
        transport=transport, event_hooks={"request": [hook]}, trust_env=False
    ) as http:
        sdk = adapter.make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider])
        with pytest.raises(Exception) as caught:
            adapter.call(sdk, prompt(f"{NAME} has a fever"), MODEL, 30.0)
    assert hook.verdict == "known_identifier" and transport.bodies == []
    # anthropic wraps the hook's EgressRefused in APIConnectionError, which
    # alone would read as retryable; openai lets it through.
    assert (type(caught.value).__name__ == "APIConnectionError") is wrapped
    assert retry.classify(caught.value) == "egress_blocked"

    monkeypatch.setattr(client_mod, "scan_body", lambda *a: None)
    transport = CapturingTransport()
    c = make_client(provider, factory, transport)
    g, budget = generate(c, f"{NAME} has a fever")
    assert g.cloud_outcome == "egress_blocked" and g.egress == "none"
    assert transport.bodies == []
    assert len(budget.reserved) == 1 and budget.settled == [("r1", 0)]


def check_no_socket_opened(provider: str, factory: Factory) -> None:
    """A plain str, a dict or a look-alike never reaches a transport: the
    adapter raises first. The transport is a real one: pytest-socket would
    refuse a connection, and none is attempted."""
    p = prompt()
    with httpx2.Client(transport=httpx2.HTTPTransport(trust_env=False), trust_env=False) as http:
        adapter = factory(MODEL)
        sdk = adapter.make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider])
        for bad in (
            QUESTION,
            {"question": QUESTION},
            list(p.blocks()),
            str(p.question),
            p.blocks(),
        ):
            with pytest.raises(TypeError):
                adapter.call(sdk, bad, MODEL, 30.0)


def check_sdk_encoded_bodies_blocked(provider: str, factory: Factory) -> None:
    adapter = factory(MODEL)
    for name in SPLIT_NAMES:
        raw = body_of(provider, factory, prompt(f"{name} has had a fever for 3 days"))
        assert NAME.encode() not in raw  # the encoder really hides it
        assert scan_body(raw, IDS, adapter.shape) == "known_identifier", repr(name)


def _pointer(doc: Any, ptr: str) -> Any:
    node = doc
    for part in ptr.strip("/").split("/"):
        part = part.replace("~1", "/").replace("~0", "~")
        node = node[int(part)] if isinstance(node, list) else node[part]
    return node


def check_shape_covers_every_emitted_path(provider: str, factory: Factory) -> None:
    adapter = factory(MODEL)
    texts = scan.block_pointers(provider)
    for n in (0, 1, 5):
        p = prompt(n=n)
        raw = body_of(provider, factory, p)
        doc = json.loads(raw)
        assert scan.leaf_pointers(doc) <= adapter.shape.allowed_paths
        assert adapter.shape.patient_paths == frozenset(texts[:2])
        assert _pointer(doc, texts[0]) == str(p.question)
        assert _pointer(doc, texts[1]) == p.context.render()
        for i, c in enumerate(p.chunks):
            assert _pointer(doc, texts[2 + i]).startswith(f"<<chunk {c.chunk_id}>>")
        assert scan_body(raw, IDS, adapter.shape) is None
    dose = body_of(provider, factory, prompt(chunk_text="Give 200000 IU of vitamin A once."))
    assert scan_body(dose, IDS, adapter.shape) is None
    named = body_of(provider, factory, prompt(f"{NAME} has a fever"))
    assert scan_body(named, IDS, adapter.shape) == "known_identifier"


def _mutations(doc: Any) -> list[Any]:
    """doc with a name put in each place in turn: appended to every string,
    in place of every other leaf, and as a new key (with the name as its
    value) in every object."""
    out: list[Any] = []
    for ptr, leaf in scan.walk(doc):
        copy = json.loads(json.dumps(doc))
        parts = ptr.strip("/").split("/") if ptr else []
        parent: Any = copy
        for part in parts[:-1]:
            part = part.replace("~1", "/").replace("~0", "~")
            parent = parent[int(part)] if isinstance(parent, list) else parent[part]
        last = parts[-1].replace("~1", "/").replace("~0", "~")
        key: Any = int(last) if isinstance(parent, list) else last
        parent[key] = f"{leaf} {NAME}" if isinstance(leaf, str) else NAME
        out.append(copy)

    def objects(node: Any) -> list[Any]:
        if isinstance(node, dict):
            return [node] + [o for v in node.values() for o in objects(v)]
        if isinstance(node, list):
            return [o for v in node for o in objects(v)]
        return []

    for i in range(len(objects(doc))):
        copy = json.loads(json.dumps(doc))
        objects(copy)[i][NAME] = NAME
        out.append(copy)
    return out


def check_exact_request_scanned(provider: str, factory: Factory) -> None:
    """The exact request the SDK sends passes the scan, and the name put
    anywhere in it (any value, any new key, a header or the URL) is
    refused."""
    adapter = factory(MODEL)
    request, _ = render(provider, factory, prompt())
    raw = bytes(request.content)
    assert scan_body(raw, IDS, adapter.shape) is None
    hook = ScanHook(IDS, adapter.shape)
    hook(request)
    assert hook.sent is True and hook.verdict is None

    doc = json.loads(raw)
    mutated = _mutations(doc)
    assert len(mutated) > len(list(scan.walk(doc)))
    for m in mutated:
        for ensure_ascii in (True, False):
            body = json.dumps(m, ensure_ascii=ensure_ascii).encode("utf-8")
            assert scan_body(body, IDS, adapter.shape) is not None, m

    def rebuilt(url: str, headers: dict[str, str]) -> httpx2.Request:
        return httpx2.Request("POST", url, headers=headers, content=raw)

    base = dict(request.headers)
    for r in (
        rebuilt(f"{request.url}?q=Adaeze%20Okafor", base),
        rebuilt(str(request.url).replace("/v1", "/v1/Adaeze-Okafor"), base),
        rebuilt(str(request.url), {**base, "x-stainless-note": NAME}),
    ):
        h = ScanHook(IDS, adapter.shape)
        with pytest.raises(EgressRefused):
            h(r)
        assert h.verdict == "known_identifier"


def check_no_trace_context(provider: str, factory: Factory) -> None:
    """With a real span current, the SDK adds no trace context (the
    propagator would inject the current span's, whichever provider made
    it)."""
    tracer = TracerProvider().get_tracer("test")
    with tracer.start_as_current_span("job"):
        request, _ = render(provider, factory, prompt())
    check_headers(request)


def check_reserved_cap_is_the_sent_cap(provider: str, factory: Factory, cap_key: str) -> None:
    transport = FixtureTransport(load(provider, "ok"))
    c = make_client(provider, factory, transport)
    g, budget = generate(c)
    assert g.cloud_outcome == "ok"
    sent = transport.bodies[0]
    assert json.loads(sent)[cap_key] == MAX_OUTPUT_TOKENS
    assert budget.reserved == [(provider, estimate_tokens(len(sent), MAX_OUTPUT_TOKENS))]


def check_registered_with_retries_off(provider: str, factory: Factory) -> None:
    assert provider in client_mod.ADAPTERS
    with httpx2.Client(transport=CapturingTransport(), trust_env=False) as http:
        sdk = client_mod.ADAPTERS[provider](MODEL).make_sdk(
            http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider]
        )
        assert sdk.max_retries == 0
        sdk2 = factory(MODEL).make_sdk(http, ApiKey(FAKE_KEY), vocab.BASE_URLS[provider])
        assert sdk2.max_retries == 0
