"""The local route's pinned answer model (own answer model plan, PR 3, Task 8):
``PinnedLlamaGenerator`` against a fake ``llama-server`` on loopback.

Before every job the worker asks the sidecar's ``/v1/models`` and requires exactly the
verified ``<name>@<version>#<sha12>``; any mismatch, an unreachable or misbehaving
sidecar, or no verified pin escalates the job with ``failed_agents=("CCA",)`` and the
outcome ``local_unverified``, and nothing is generated. The plain Ollama route is refused
outside development unless ``INTELLIGENCE_LOCAL_UNPINNED=on``.

Every question, alias and answer is synthetic.
"""

from __future__ import annotations

import json
import socket
import threading
import time
from collections.abc import Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

from intelligence.boundary import models
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.models import OrchestratorModelClient
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk
from intelligence.tools import llama_client
from intelligence.tools.llama_client import (
    LlamaSidecar,
    LocalLLMSettings,
    LocalUnverified,
    PinnedLlamaGenerator,
    RefusedGenerator,
)
from intelligence.tools.ollama_client import OllamaGenerator

PINNED = "mhai-cca-synthetic@1.0.0#0123456789ab"
OTHER = "mhai-cca-synthetic@0.9.0#ba9876543210"
QUESTION = "Synthetic question about a synthetic guideline"
ANSWER = "Synthetic answer from the shared guideline [KB#0001]."
BASE_ENV = {
    "APP_ENV": "development",
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/nonexistent/trusted.json",
}


# ---------------------------------------------------------------- the fake sidecar


@dataclass
class Sidecar:
    """What the fake llama-server answers. ``models`` is the /v1/models body (bytes) or
    None for the alias list; ``chat`` the completion's content."""

    aliases: list[str] = field(default_factory=lambda: [PINNED])
    models_status: int = 200
    models_body: bytes | None = None
    models_redirect: bool = False
    chat_status: int = 200
    chat_body: bytes | None = None
    chat_content: Any = ANSWER
    chat_delay: float = 0.0
    chat_redirect: bool = False
    requests: list[tuple[str, str, Any]] = field(default_factory=list)

    def models_reply(self) -> bytes:
        if self.models_body is not None:
            return self.models_body
        return json.dumps(
            {"object": "list", "data": [{"id": a, "object": "model"} for a in self.aliases]}
        ).encode()

    def chat_reply(self) -> bytes:
        if self.chat_body is not None:
            return self.chat_body
        return json.dumps(
            {"choices": [{"message": {"role": "assistant", "content": self.chat_content}}]}
        ).encode()


def _handler(s: Sidecar) -> type[BaseHTTPRequestHandler]:
    class H(BaseHTTPRequestHandler):
        def _send(self, status: int, body: bytes, location: str | None = None) -> None:
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            if location is not None:
                self.send_header("Location", location)
            self.end_headers()
            try:
                self.wfile.write(body)
            except OSError:
                pass

        def do_GET(self) -> None:  # noqa: N802 - http.server's naming
            s.requests.append(("GET", self.path, None))
            if self.path != "/v1/models":
                self._send(404, b"{}")
            elif s.models_redirect:
                self._send(302, b"", "http://127.0.0.1:9/v1/models")
            else:
                self._send(s.models_status, s.models_reply())

        def do_POST(self) -> None:  # noqa: N802
            n = int(self.headers["Content-Length"])
            s.requests.append(("POST", self.path, json.loads(self.rfile.read(n))))
            if s.chat_delay:
                time.sleep(s.chat_delay)
            if s.chat_redirect:
                self._send(307, b"", "http://127.0.0.1:9/v1/chat/completions")
            else:
                self._send(s.chat_status, s.chat_reply())

        def log_message(self, format: str, *args: Any) -> None:
            pass

    return H


@pytest.fixture
def sidecar() -> Iterator[tuple[Sidecar, str]]:
    s = Sidecar()
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _handler(s))
    httpd.daemon_threads = True
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield s, f"http://127.0.0.1:{httpd.server_address[1]}"
    finally:
        httpd.shutdown()
        httpd.server_close()


def _closed_port() -> str:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    return f"http://127.0.0.1:{port}"


# ---------------------------------------------------------------- the client


class GeneratingCCA:
    """The RAG CCA's contract: the generator writes the answer (no retrieval model)."""

    def __init__(self, generator: Any) -> None:
        self.generator = generator

    def generate_with_rag(
        self, query: str, top_k: int = 5, triage_context: Any = None, patient_id: Any = None
    ) -> dict[str, Any]:
        response = self.generator.generate("Synthetic system prompt.", query)
        return {
            "response": response,
            "citations": ["0001"],
            "retrieved_chunks": [{"id": "KB#0001"}],
            "model_used": self.generator.name,
            "retrieval_latency_ms": 1.0,
            "total_latency_ms": 2.0,
        }


class PassingHGA:
    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        return {"trust_score": 0.91, "decision": "pass", "flags": []}


class FakePinned:
    def __init__(self, versions: dict[str, str]) -> None:
        self._versions = versions

    def versions(self) -> dict[str, str]:
        return dict(self._versions)

    def path(self, role: str) -> Any:
        raise KeyError(role)


def _settings(url: str, **kw: Any) -> LocalLLMSettings:
    return LocalLLMSettings(url=url, timeout_s=kw.pop("timeout_s", 5.0), **kw)


def _client(
    url: str, pinned: FakePinned | None = FakePinned({"cca": PINNED}), **kw: Any
) -> tuple[OrchestratorModelClient, list[list[KBChunk]]]:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent

    source = (lambda: pinned) if pinned is not None else (lambda: None)
    gen = models.local_generator(source, ("cca",), _settings(url, **kw), development=False)
    assert isinstance(gen, PinnedLlamaGenerator)
    built: list[list[KBChunk]] = []

    def build(chunks: Any) -> DynamicOrchestratorAgent:
        built.append(list(chunks))
        return DynamicOrchestratorAgent(rag_cca=GeneratingCCA(gen), hga=PassingHGA())

    client = OrchestratorModelClient(
        build,
        versions={"cca": gen.name, "dense": "d", "nli": "n"},
        pinned=source,
        preflight=gen.check,
    )
    return client, built


def _chunk() -> KBChunk:
    return KBChunk("KB#0001", "synthetic guideline", "guideline", REFERENCE_SOURCE, "")


def _chats(s: Sidecar) -> list[Any]:
    return [r for r in s.requests if r[0] == "POST"]


def _escalated_unverified(client: OrchestratorModelClient, g: models.Generation) -> None:
    assert g.available is False
    assert g.failed_agents == ("CCA",)
    assert g.cloud_outcome == "local_unverified"
    assert g.response == "" and g.egress == "none"
    v = client.verify(g)
    assert v.decision == "ESCALATE" and v.verified is False and v.trust_score is None
    assert v.failed_agents == ("CCA",)


def test_alias_match_generates_and_records_the_pinned_version(
    sidecar: tuple[Sidecar, str],
) -> None:
    s, url = sidecar
    client, built = _client(url)
    g = client.generate(QUESTION, {}, None, [_chunk()])
    assert g.available is True and g.response == ANSWER
    assert g.cloud_outcome is None and g.failed_agents == ()
    v = client.verify(g)
    assert v.decision == "PASS" and v.verified is True
    assert client.model_versions()["cca"] == PINNED
    # The alias was checked first, then exactly one completion, against the pinned alias,
    # with the settings' sampling and no streaming.
    assert [r[:2] for r in s.requests] == [("GET", "/v1/models"), ("POST", "/v1/chat/completions")]
    body = _chats(s)[0][2]
    assert body["model"] == PINNED and body["stream"] is False
    assert body["temperature"] == 0.0 and body["top_p"] == 1.0 and body["max_tokens"] == 512
    assert built, "the orchestrator was built once the alias matched"


def test_the_alias_is_checked_before_every_job(sidecar: tuple[Sidecar, str]) -> None:
    s, url = sidecar
    client, _ = _client(url)
    for _ in range(3):
        assert client.generate(QUESTION, {}, None, [_chunk()]).available
    assert [r[1] for r in s.requests if r[0] == "GET"] == ["/v1/models"] * 3
    s.aliases = [OTHER]  # the sidecar was restarted on another model
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))


@pytest.mark.parametrize(
    "change",
    [
        {"aliases": [OTHER]},
        {"aliases": [PINNED, OTHER]},
        {"aliases": []},
        {"aliases": [PINNED.upper()]},
        {"aliases": [PINNED + " "]},
        {"models_status": 500},
        {"models_status": 404},
        {"models_body": b"not json"},
        {"models_body": b'{"data": "mhai-cca-synthetic@1.0.0#0123456789ab"}'},
        {"models_body": b'{"data": [{"name": "mhai-cca-synthetic@1.0.0#0123456789ab"}]}'},
        {"models_body": b"[]"},
        {"models_redirect": True},
    ],
    ids=lambda c: "-".join(f"{k}" for k in c),
)
def test_every_failed_alias_check_escalates_without_generating(
    sidecar: tuple[Sidecar, str], change: dict[str, Any]
) -> None:
    s, url = sidecar
    for k, v in change.items():
        setattr(s, k, v)
    client, built = _client(url)
    g = client.generate(QUESTION, {}, None, [_chunk()])
    _escalated_unverified(client, g)
    assert _chats(s) == [] and built == [], "nothing is generated or built"


def test_an_unreachable_sidecar_escalates() -> None:
    client, built = _client(_closed_port())
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert built == []


def test_no_verified_pin_escalates_without_asking_the_sidecar(
    sidecar: tuple[Sidecar, str],
) -> None:
    s, url = sidecar
    client, built = _client(url, pinned=None)
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert s.requests == [] and built == []
    assert client.model_versions()["cca"] == "llama:unverified"


def test_a_pin_without_cca_escalates(sidecar: tuple[Sidecar, str]) -> None:
    s, url = sidecar
    client, _ = _client(url, pinned=FakePinned({"extract": "x-y@1.0.0#0123456789ab"}))
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert s.requests == []


@pytest.mark.parametrize(
    "change",
    [
        {"chat_status": 500},
        {"chat_status": 503},
        {"chat_body": b"not json"},
        {"chat_body": b'{"choices": []}'},
        {"chat_content": ""},
        {"chat_content": "   "},
        {"chat_content": None},
        {"chat_content": 42},
        {"chat_body": b"x" * (llama_client.MAX_REPLY_BYTES + 1)},
        {"chat_redirect": True},
        {"chat_delay": 2.0},
    ],
    ids=lambda c: "-".join(f"{k}" for k in c),
)
def test_every_failed_generation_escalates(
    sidecar: tuple[Sidecar, str], change: dict[str, Any]
) -> None:
    s, url = sidecar
    for k, v in change.items():
        setattr(s, k, v)
    client, _ = _client(url, timeout_s=0.5)
    g = client.generate(QUESTION, {}, None, [_chunk()])
    assert g.available is False and g.failed_agents == ("CCA",) and g.response == ""
    v = client.verify(g)
    assert v.decision == "ESCALATE" and v.verified is False and v.trust_score is None


def test_proxies_in_the_environment_are_ignored(
    sidecar: tuple[Sidecar, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    s, url = sidecar
    for var in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "all_proxy"):
        monkeypatch.setenv(var, "http://127.0.0.1:9")
    monkeypatch.setenv("NO_PROXY", "")
    client, _ = _client(url)
    assert client.generate(QUESTION, {}, None, [_chunk()]).available is True


def test_wait_for_alias_until_served(sidecar: tuple[Sidecar, str]) -> None:
    s, url = sidecar
    s.aliases = [OTHER]
    side = LlamaSidecar(_settings(url))
    with pytest.raises(LocalUnverified) as e:
        side.wait_for_alias(PINNED, 0.3, interval_s=0.05)
    assert e.value.detail == "start_timeout"
    s.aliases = [PINNED]
    side.wait_for_alias(PINNED, 2.0, interval_s=0.05)
    assert side.ask(PINNED, "Synthetic system.", QUESTION) == ANSWER
    side.close()


def test_errors_carry_codes_only(sidecar: tuple[Sidecar, str]) -> None:
    s, url = sidecar
    s.aliases = ["patient-synthetic-name"]
    gen = PinnedLlamaGenerator(LlamaSidecar(_settings(url)), lambda: PINNED)
    with pytest.raises(LocalUnverified) as e:
        gen.check()
    assert str(e.value) == "alias_mismatch" and "patient" not in repr(e.value)


# ---------------------------------------------------------------- which generator


def test_cca_role_gets_the_pinned_generator() -> None:
    gen = models.local_generator(
        lambda: None, ("cca",), LocalLLMSettings(), development=False, unpinned=True
    )
    assert isinstance(gen, PinnedLlamaGenerator)  # a pin always wins, the switch or not


def test_ollama_refused_in_production_without_the_switch(caplog: pytest.LogCaptureFixture) -> None:
    import logging

    with caplog.at_level(logging.ERROR):
        gen = models.local_generator(lambda: None, (), LocalLLMSettings(), development=False)
    assert isinstance(gen, RefusedGenerator)
    assert any("local.unpinned_refused" in r.getMessage() for r in caplog.records)
    with pytest.raises(LocalUnverified):
        gen.check()
    with pytest.raises(LocalUnverified):
        gen.generate("s", "u")


def test_ollama_allowed_in_development_or_with_the_switch() -> None:
    dev = models.local_generator(lambda: None, (), LocalLLMSettings(), development=True)
    on = models.local_generator(
        lambda: None, (), LocalLLMSettings(), development=False, unpinned=True
    )
    assert isinstance(dev, OllamaGenerator) and isinstance(on, OllamaGenerator)


def test_refused_route_escalates_every_job_as_unverified() -> None:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent

    gen = models.local_generator(lambda: None, (), LocalLLMSettings(), development=False)
    built: list[Any] = []

    def build(chunks: Any) -> DynamicOrchestratorAgent:
        built.append(chunks)
        return DynamicOrchestratorAgent(rag_cca=GeneratingCCA(gen), hga=PassingHGA())

    client = OrchestratorModelClient(build, versions={"cca": gen.name}, preflight=gen.check)
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert built == [] and client.model_versions()["cca"] == "unpinned:refused"


def test_production_clients_get_the_generators_check(monkeypatch: pytest.MonkeyPatch) -> None:
    """build_model_router's and default_orchestrator's local client is built with the
    generator's check as its preflight (the cloud clients share the parts and never
    call it); Ollama has no check, and in development needs none."""
    seen: list[Any] = []

    def fake_parts(*a: Any, **k: Any) -> Any:
        seen.append(k.get("generator"))
        return (lambda chunks: None), {"cca": k["generator"].name}

    monkeypatch.setattr(models, "local_parts", fake_parts)
    s = WorkerSettings.from_env({**BASE_ENV, "INTELLIGENCE_LOCAL_UNPINNED": "on"})
    router = models.build_model_router(s)
    assert isinstance(router.local, OrchestratorModelClient)
    assert isinstance(seen[-1], OllamaGenerator)
    assert router.local._preflight is None  # type: ignore[attr-defined]
    client = models.default_orchestrator(lambda: FakePinned({"cca": PINNED}), ("cca",), s)
    assert isinstance(seen[-1], PinnedLlamaGenerator)
    assert client._preflight == seen[-1].check  # type: ignore[attr-defined]
    assert client.model_versions()["cca"] == PINNED
    prod = WorkerSettings(service="pipeline", nats_url="nats://x", trusted_keys_path="k")
    client = models.default_orchestrator(None, (), prod)
    assert isinstance(seen[-1], RefusedGenerator)
    assert client._preflight == seen[-1].check  # type: ignore[attr-defined]


# ---------------------------------------------------------------- settings


def test_local_llm_settings_defaults() -> None:
    s = WorkerSettings.from_env(dict(BASE_ENV))
    assert s.local_llm == LocalLLMSettings()
    assert s.local_llm.url == "http://llama:8080"
    assert s.local_unpinned is False


@pytest.mark.parametrize(
    "url",
    ["http://llama:8080", "http://llama", "http://127.0.0.1:8080", "http://[::1]:8080"],
)
def test_sidecar_url_allowed(url: str) -> None:
    env = {**BASE_ENV, "APP_ENV": "production"}
    assert llama_client.check_url(url, development=False) == url.rstrip("/")
    assert WorkerSettings.from_env(
        {**BASE_ENV, "INTELLIGENCE_LOCAL_LLM_URL": url}
    ).local_llm.url == url.rstrip("/")
    del env


@pytest.mark.parametrize(
    "url",
    [
        "http://store.synthetic.test:8080",
        "https://llama:8080",
        "http://user:pw@llama:8080",
        "http://llama:8080/v1",
        "http://llama:8080?x=1",
        "http://10.0.0.5:8080",
        "llama:8080",
        "http://llama:0",
        "http://llama:99999",
    ],
)
def test_sidecar_url_refused_outside_development(url: str) -> None:
    with pytest.raises(ValueError):
        llama_client.check_url(url, development=False)


def test_other_hosts_only_in_development() -> None:
    assert llama_client.check_url("http://ollama-dev:8080", development=True)
    with pytest.raises(ValueError):
        llama_client.check_url("http://user:pw@ollama-dev:8080", development=True)


@pytest.mark.parametrize(
    "over,name",
    [
        ({"INTELLIGENCE_LOCAL_LLM_URL": "http://elsewhere.synthetic.test"}, "LOCAL_LLM_URL"),
        ({"INTELLIGENCE_LOCAL_LLM_MAX_TOKENS": "0"}, "LOCAL_LLM_MAX_TOKENS"),
        ({"INTELLIGENCE_LOCAL_LLM_MAX_TOKENS": "lots"}, "LOCAL_LLM_MAX_TOKENS"),
        ({"INTELLIGENCE_LOCAL_LLM_TEMPERATURE": "3"}, "LOCAL_LLM_TEMPERATURE"),
        ({"INTELLIGENCE_LOCAL_LLM_TEMPERATURE": "nan"}, "LOCAL_LLM_TEMPERATURE"),
        ({"INTELLIGENCE_LOCAL_LLM_TOP_P": "0"}, "LOCAL_LLM_TOP_P"),
        ({"INTELLIGENCE_LOCAL_LLM_TIMEOUT": "-1"}, "LOCAL_LLM_TIMEOUT"),
        ({"INTELLIGENCE_LOCAL_LLM_START_TIMEOUT": "0"}, "LOCAL_LLM_START_TIMEOUT"),
        ({"INTELLIGENCE_LOCAL_UNPINNED": "yes"}, "INTELLIGENCE_LOCAL_UNPINNED"),
    ],
)
def test_malformed_local_settings_refused(over: dict[str, str], name: str) -> None:
    env = {**BASE_ENV, "APP_ENV": "production", **over}
    # Production refuses other things too (TLS, NKeys); the local setting is named.
    with pytest.raises(ConfigError, match=name):
        WorkerSettings.from_env(env)


def test_unpinned_switch() -> None:
    for value, want in (("on", True), ("off", False), ("", False)):
        s = WorkerSettings.from_env({**BASE_ENV, "INTELLIGENCE_LOCAL_UNPINNED": value})
        assert s.local_unpinned is want


# ---------------------------------------------------------------- the offline mirror


def _mirror_env(tmp_path: Any, **over: str) -> dict[str, str]:
    from tests.modelstore.conftest import REPO

    (tmp_path / "mirror").mkdir(mode=0o700, exist_ok=True)
    env = {
        **BASE_ENV,
        "INTELLIGENCE_MODEL_ROLES": "cca",
        "INTELLIGENCE_MODELS_LOCK": str(tmp_path / "models.lock"),
        "INTELLIGENCE_MODEL_CACHE": str(tmp_path / "cache"),
        "INTELLIGENCE_MODEL_MIRROR": str(tmp_path / "mirror"),
        "MHAI_MODELS_TRUST_POLICY": str(REPO / "models" / "trust-policy.json"),
    }
    env.update(over)
    return env


def test_mirror_needs_no_store_settings(tmp_path: Any) -> None:
    s = WorkerSettings.from_env(_mirror_env(tmp_path))
    assert s.model_mirror_dir == str(tmp_path / "mirror")
    assert s.model_store_endpoint is None and s.model_store_access_key is None


@pytest.mark.parametrize(
    "over",
    [
        {"MODEL_STORE_ENDPOINT": "https://store.synthetic.test"},
        {"MODEL_STORE_BUCKET": "mhai-models"},
        {"MODEL_STORE_READER_ACCESS_KEY_FILE": "/run/secrets/ak"},
        {"INTELLIGENCE_MODEL_MIRROR": "relative/mirror"},
    ],
)
def test_mirror_and_the_store_are_never_both_set(tmp_path: Any, over: dict[str, str]) -> None:
    """With the mirror the pipeline verifies offline: it is never also given a route or a
    credential for the store (model-fetch is the only container with those)."""
    with pytest.raises(ConfigError, match="INTELLIGENCE_MODEL_MIRROR"):
        WorkerSettings.from_env(_mirror_env(tmp_path, **over))


def test_gate_verifies_from_the_mirror(tmp_path: Any) -> None:
    from intelligence.boundary.worker import build_model_gate
    from intelligence.modelstore import lock

    (tmp_path / "cache").mkdir(mode=0o700)
    lock.write(
        tmp_path / "models.lock",
        lock.Lock({"cca": lock.Pin("mhai-cca-synthetic", "1.0.0", "ab" * 32, "answer")}),
    )
    gate = build_model_gate(WorkerSettings.from_env(_mirror_env(tmp_path)))
    assert gate is not None and gate.roles == ("cca",) and not gate.ready()
