"""Our own model on the GPU (the ``own`` route; deploy/README.md, "Our own model on the
GPU"): the settings, the own client against a fake ``llama-server`` on loopback, and the
router.

A job whose route is ``own`` (core's: the hospital's model policy has own_model on) runs
on the own client: the gpu-model service must serve exactly the configured
``<name>@<version>#<sha12>`` before every job, or the job escalates as
``local_unverified`` with nothing generated. Without INTELLIGENCE_OWN_MODEL=on every own
job escalates; it never falls back to the local route or the cloud.

Every question, alias, hash and answer is synthetic.
"""

from __future__ import annotations

import threading
from collections.abc import Iterator
from http.server import ThreadingHTTPServer
from typing import Any

import pytest

from intelligence.agents import answer_profile, prompt_formats
from intelligence.boundary import models
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.models import ModelRouter, StubModelClient, own_client
from intelligence.tools import llama_client
from intelligence.tools.llama_client import OwnModelSettings
from tests.boundary.test_pinned_llama import (
    ANSWER,
    BASE_ENV,
    QUESTION,
    FakePinned,
    PassingHGA,
    Sidecar,
    _chats,
    _chunk,
    _escalated_unverified,
    _handler,
    _settings,
)

SHA = "0123456789abcdef" * 4
ALIAS = "gemma4-e4b-cca@1.0.0#0123456789ab"
OWN_ENV = {
    "INTELLIGENCE_OWN_MODEL": "on",
    "INTELLIGENCE_OWN_MODEL_NAME": "gemma4-e4b-cca",
    "INTELLIGENCE_OWN_MODEL_VERSION": "1.0.0",
    "INTELLIGENCE_OWN_MODEL_SHA256": SHA,
    "INTELLIGENCE_OWN_MODEL_PROFILE": "gemma4-e4b-cca-v1",
}


# ---------------------------------------------------------------- the settings


def _own(env: dict[str, str], development: bool = False) -> tuple[Any, list[str]]:
    errs: list[str] = []
    return llama_client.own_settings_from_env(env, development, errs), errs


def test_off_by_default_and_when_off() -> None:
    assert _own({}) == (None, [])
    # Off ignores the rest: the host's .env keeps the hash while the model is off.
    assert _own({**OWN_ENV, "INTELLIGENCE_OWN_MODEL": "off"}) == (None, [])


def test_true_and_false_as_the_hosts_env_says_them() -> None:
    assert _own({**OWN_ENV, "INTELLIGENCE_OWN_MODEL": "false"}) == (None, [])
    own, errs = _own({**OWN_ENV, "INTELLIGENCE_OWN_MODEL": "true"})
    assert errs == [] and own is not None and own.alias == ALIAS


def test_the_deploy_script_knows_every_profile() -> None:
    import re
    from pathlib import Path

    script = Path(__file__).parents[4] / "deploy" / "gpu-model" / "gpu-model.sh"
    m = re.search(r"^  ([a-z0-9|-]+)\) ;;$", script.read_text(), re.MULTILINE)
    assert m is not None
    assert set(m.group(1).split("|")) == answer_profile.PROFILE_IDS


def test_on_reads_every_setting() -> None:
    own, errs = _own(OWN_ENV)
    assert errs == []
    assert own == OwnModelSettings(
        url="http://gpu-model:8080",
        name="gemma4-e4b-cca",
        version="1.0.0",
        sha256=SHA,
        profile="gemma4-e4b-cca-v1",
    )
    assert own.alias == ALIAS


@pytest.mark.parametrize(
    ("over", "named"),
    [
        ({"INTELLIGENCE_OWN_MODEL": "yes"}, "INTELLIGENCE_OWN_MODEL must"),
        ({"INTELLIGENCE_OWN_MODEL_NAME": ""}, "INTELLIGENCE_OWN_MODEL_NAME"),
        ({"INTELLIGENCE_OWN_MODEL_NAME": "Gemma"}, "INTELLIGENCE_OWN_MODEL_NAME"),
        ({"INTELLIGENCE_OWN_MODEL_NAME": "a@b"}, "INTELLIGENCE_OWN_MODEL_NAME"),
        ({"INTELLIGENCE_OWN_MODEL_VERSION": "1.0"}, "INTELLIGENCE_OWN_MODEL_VERSION"),
        ({"INTELLIGENCE_OWN_MODEL_VERSION": "01.0.0"}, "INTELLIGENCE_OWN_MODEL_VERSION"),
        ({"INTELLIGENCE_OWN_MODEL_SHA256": SHA.upper()}, "INTELLIGENCE_OWN_MODEL_SHA256"),
        ({"INTELLIGENCE_OWN_MODEL_SHA256": SHA[:63]}, "INTELLIGENCE_OWN_MODEL_SHA256"),
        ({"INTELLIGENCE_OWN_MODEL_SHA256": ""}, "INTELLIGENCE_OWN_MODEL_SHA256"),
        ({"INTELLIGENCE_OWN_MODEL_PROFILE": ""}, "INTELLIGENCE_OWN_MODEL_PROFILE"),
        ({"INTELLIGENCE_OWN_MODEL_PROFILE": "gemma"}, "INTELLIGENCE_OWN_MODEL_PROFILE"),
        ({"INTELLIGENCE_OWN_MODEL_URL": "http://llama:8080"}, "INTELLIGENCE_OWN_MODEL_URL"),
        ({"INTELLIGENCE_OWN_MODEL_URL": "https://gpu-model:8080"}, "INTELLIGENCE_OWN_MODEL_URL"),
        ({"INTELLIGENCE_OWN_MODEL_URL": "http://gpu-model:8080/v1"}, "INTELLIGENCE_OWN_MODEL_URL"),
        ({"INTELLIGENCE_OWN_MODEL_URL": "http://u:p@gpu-model:8080"}, "INTELLIGENCE_OWN_MODEL_URL"),
        ({"INTELLIGENCE_OWN_MODEL_URL": "http://model.example:8080"}, "INTELLIGENCE_OWN_MODEL_URL"),
    ],
)
def test_malformed_settings_are_refused_by_name(over: dict[str, str], named: str) -> None:
    own, errs = _own({**OWN_ENV, **over})
    assert own is None
    assert len(errs) == 1 and errs[0].startswith(named)
    for v in over.values():
        if v:
            assert v not in errs[0]  # never the value


@pytest.mark.parametrize(
    "url", ["http://gpu-model:8080", "http://127.0.0.1:8095", "http://gpu-model"]
)
def test_the_url_is_the_service_or_loopback(url: str) -> None:
    own, errs = _own({**OWN_ENV, "INTELLIGENCE_OWN_MODEL_URL": url})
    assert errs == [] and own is not None and own.url == url


def test_another_host_only_in_development() -> None:
    env = {**OWN_ENV, "INTELLIGENCE_OWN_MODEL_URL": "http://model.example:8080"}
    own, errs = _own(env, development=True)
    assert errs == [] and own is not None


def test_worker_settings_carry_it_for_the_pipeline_only() -> None:
    assert WorkerSettings.from_env(BASE_ENV).own_model is None
    s = WorkerSettings.from_env({**BASE_ENV, **OWN_ENV})
    assert s.own_model is not None and s.own_model.alias == ALIAS
    with pytest.raises(ConfigError, match="pipeline service only"):
        WorkerSettings.from_env({**BASE_ENV, **OWN_ENV, "INTELLIGENCE_SERVICE": "echo"})
    with pytest.raises(ConfigError, match="INTELLIGENCE_OWN_MODEL_SHA256"):
        WorkerSettings.from_env({**BASE_ENV, **OWN_ENV, "INTELLIGENCE_OWN_MODEL_SHA256": "x"})


def test_never_with_the_sample_client() -> None:
    env = {
        **BASE_ENV,
        **OWN_ENV,
        "INTELLIGENCE_MODEL_CLIENT": "sample",
        "INTELLIGENCE_SAMPLE_HOSPITALS": "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11",
    }
    with pytest.raises(ConfigError, match="never runs with INTELLIGENCE_MODEL_CLIENT=sample"):
        WorkerSettings.from_env(env)


# ---------------------------------------------------------------- the own client


@pytest.fixture
def gpu() -> Iterator[tuple[Sidecar, str]]:
    """A fake llama-server standing in for the gpu-model service, on loopback."""
    s = Sidecar(aliases=[ALIAS])
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _handler(s))
    httpd.daemon_threads = True
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield s, f"http://127.0.0.1:{httpd.server_address[1]}"
    finally:
        httpd.shutdown()
        httpd.server_close()


class RecordingCCA:
    """The RAG CCA's contract over the own generator, with the profile's options."""

    def __init__(self, generator: Any, profile: answer_profile.AnswerProfile) -> None:
        self.generator = generator
        self.profile = profile

    def generate_with_rag(self, query: str, **kwargs: Any) -> dict[str, Any]:
        asked = prompt_formats.steps_v1(query, [("KB#0001", "synthetic guideline")])
        response = self.generator.generate(asked.system, asked.user, **asked.options)
        return {
            "response": response,
            "citations": ["0001"],
            "retrieved_chunks": [{"id": "KB#0001"}],
            "model_used": self.generator.name,
            "retrieval_latency_ms": 1.0,
            "total_latency_ms": 2.0,
        }


def _own_client(url: str, pinned: Any = None) -> tuple[Any, list[Any]]:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent

    built: list[Any] = []

    def build(
        chunks: Any,
        profile: answer_profile.AnswerProfile = answer_profile.DEFAULT,
        generator_override: Any = None,
    ) -> DynamicOrchestratorAgent:
        built.append((list(chunks), profile, generator_override))
        return DynamicOrchestratorAgent(
            rag_cca=RecordingCCA(generator_override, profile), hga=PassingHGA()
        )

    own, errs = _own({**OWN_ENV, "INTELLIGENCE_OWN_MODEL_URL": url})
    assert errs == [] and own is not None
    source = (lambda: pinned) if pinned is not None else None
    client = own_client(
        own,
        _settings("http://llama:8080"),
        build,
        {"cca": "unpinned:refused", "dense": "d"},
        source,
    )
    return client, built


def test_own_model_answers_with_its_alias_profile_and_format(
    gpu: tuple[Sidecar, str],
) -> None:
    s, url = gpu
    s.aliases = [ALIAS]
    client, built = _own_client(
        url, FakePinned({"cca": "other@1.0.0#aaaaaaaaaaaa", "nli": "nli-x@1.0.0#bbbbbbbbbbbb"})
    )
    g = client.generate(QUESTION, {}, None, [_chunk()])
    assert g.available is True and g.response == ANSWER and g.failed_agents == ()
    assert client.verify(g).decision == "PASS"
    # The alias first, then one completion, to the own model's URL, in its own format.
    assert [r[:2] for r in s.requests] == [("GET", "/v1/models"), ("POST", "/v1/chat/completions")]
    body = _chats(s)[0][2]
    assert body["model"] == ALIAS
    assert body["stop"] == ["NOTE:"] and body["repeat_penalty"] == 1.05
    assert body["messages"][0]["content"].endswith("reply only: INSUFFICIENT EVIDENCE.")
    assert body["temperature"] == 0.0 and body["max_tokens"] == 512
    # Built with the operator's profile and the own generator, never the local one.
    (_, profile, gen) = built[0]
    assert profile is answer_profile.GEMMA4_E4B_CCA_V1
    assert isinstance(gen, llama_client.PinnedLlamaGenerator)
    # cca is the own model's alias, never the local pin's; other pinned roles stay.
    versions = client.model_versions()
    assert versions["cca"] == ALIAS and versions["nli"] == "nli-x@1.0.0#bbbbbbbbbbbb"


def test_another_alias_escalates_without_generating(gpu: tuple[Sidecar, str]) -> None:
    s, url = gpu
    s.aliases = ["gemma4-e4b-cca@1.0.0#ffffffffffff"]  # another GGUF than configured
    client, built = _own_client(url)
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert _chats(s) == [] and built == []


def test_an_unreachable_gpu_model_escalates() -> None:
    from tests.boundary.test_pinned_llama import _closed_port

    client, built = _own_client(_closed_port())
    _escalated_unverified(client, client.generate(QUESTION, {}, None, [_chunk()]))
    assert built == []


def test_an_unknown_profile_is_refused() -> None:
    own = OwnModelSettings("http://gpu-model:8080", "m-1", "1.0.0", SHA, "nope")
    with pytest.raises(ConfigError):
        own_client(own, _settings("http://llama:8080"), lambda chunks: None, {})


# ---------------------------------------------------------------- the router


def test_the_own_route_without_an_own_model_escalates(caplog: pytest.LogCaptureFixture) -> None:
    local = StubModelClient()
    router = ModelRouter(local, {})
    client, provider = router.client_for("own")
    assert provider == "own" and type(client).__name__ == "_OwnOff"
    assert client.mask("x") == StubModelClient.MASKED  # the local masker
    g = client.generate(QUESTION, {}, None, [_chunk()])
    assert g.available is False and g.failed_agents == ("CCA",)
    assert g.cloud_outcome == "local_unverified" and g.egress == "none"
    assert local.calls["generate"] == 0  # never the local route in its place
    v = client.verify(g)
    assert v.decision == "ESCALATE" and v.trust_score is None
    assert "cca" not in client.model_versions()
    assert any("own.off" in r.getMessage() for r in caplog.records)


def test_the_own_route_takes_the_own_client() -> None:
    local, own = StubModelClient(), StubModelClient()
    router = ModelRouter(local, {}, own=own)
    assert router.client_for("own") == (own, "own")
    assert router.client_for("local") == (local, "local")


def test_build_model_router_adds_the_own_client(monkeypatch: pytest.MonkeyPatch) -> None:
    seen: dict[str, Any] = {}

    def fake_parts(*args: Any, **kwargs: Any) -> tuple[Any, dict[str, str]]:
        def build(chunks: Any, profile: Any = None, generator_override: Any = None) -> Any:
            seen["generator"] = generator_override
            return None

        return build, {"cca": "local", "dense": "d"}

    monkeypatch.setattr(models, "local_parts", fake_parts)
    s = WorkerSettings.from_env({**BASE_ENV, **OWN_ENV})
    router = models.build_model_router(s)
    assert router.own is not None
    assert router.client_for("own")[0] is router.own
    assert router.own.model_versions()["cca"] == ALIAS
    assert router.local.model_versions()["cca"] == "local"


def test_build_model_router_without_the_own_model_has_none() -> None:
    s = WorkerSettings.from_env({**BASE_ENV, "INTELLIGENCE_MODEL_CLIENT": "stub"})
    router = models.build_model_router(s)
    assert router.own is None
    assert type(router.client_for("own")[0]).__name__ == "_OwnOff"


def test_the_stub_never_gets_an_own_client() -> None:
    s = WorkerSettings.from_env({**BASE_ENV, **OWN_ENV, "INTELLIGENCE_MODEL_CLIENT": "stub"})
    router = models.build_model_router(s)
    assert router.own is None and isinstance(router.local, StubModelClient)


def test_own_is_a_route() -> None:
    from intelligence.boundary.cloud import vocab
    from intelligence.boundary.jobs.run_query import ON_HOST_ROUTES

    assert "own" in vocab.ROUTES
    assert ON_HOST_ROUTES == frozenset({"local", "own"})
