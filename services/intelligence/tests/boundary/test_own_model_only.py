"""The pipeline with our own model only (deploy/README.md, "Answering with our own model
only"): deploy/compose.yml's pipeline settings with the .env enable-pilot.sh --provider
none writes (GPU_MODEL_ENABLED=true, the GPU_MODEL_* settings, no PILOT_CLOUD_PROVIDER).

The worker starts in production with no cloud provider and the own route; a question on
a cloud route escalates as unconfigured (nothing is sent), and the own route takes our own
model. Every value here is synthetic.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import pytest
import yaml

from intelligence.boundary import models
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.models import StubModelClient

REPO = Path(__file__).parents[4]
SHA = "0123456789abcdef" * 4
ALIAS = "gemma4-e4b-cca@1.0.0#0123456789ab"
# The host's .env after enable-pilot.sh --provider none (none of these is a secret).
HOST_ENV = {
    "GPU_MODEL_ENABLED": "true",
    "GPU_MODEL_NAME": "gemma4-e4b-cca",
    "GPU_MODEL_VERSION": "1.0.0",
    "GPU_MODEL_SHA256": SHA,
    "GPU_MODEL_PROFILE": "gemma4-e4b-cca-v1",
    "PIPELINE_ENABLED": "true",
    "CORE_OWN_MODEL_ALLOWED": "true",
}
_VAR = re.compile(r"\$\{([A-Z0-9_]+)(?::-([^}]*))?\}")


def _interpolate(value: str, env: dict[str, str]) -> str:
    """Compose's ${VAR} and ${VAR:-default}, as `docker compose config` does."""
    return _VAR.sub(lambda m: env.get(m.group(1)) or (m.group(2) or ""), value)


@pytest.fixture
def own_only(tmp_path: Path) -> dict[str, str]:
    compose = yaml.safe_load((REPO / "deploy" / "compose.yml").read_text(encoding="utf-8"))
    raw: dict[str, Any] = compose["services"]["pipeline"]["environment"]
    env = {k: _interpolate(str(v), HOST_ENV) for k, v in raw.items()}
    pw = tmp_path / "intelligence-db-password"
    pw.write_text("test-only-file-password-0042\n")
    pw.chmod(0o400)
    env["INTELLIGENCE_DB_PASSWORD_FILE"] = str(pw)
    # The service's command, `worker --service pipeline`.
    assert compose["services"]["pipeline"]["command"] == ["worker", "--service", "pipeline"]
    env["INTELLIGENCE_SERVICE"] = "pipeline"
    return env


def test_the_compose_settings_have_no_cloud_and_the_own_model(own_only: dict[str, str]) -> None:
    assert own_only["APP_ENV"] == "production"
    assert own_only["INTELLIGENCE_CLOUD_PROVIDERS"] == ""
    for p in ("ANTHROPIC", "GEMINI"):
        assert own_only[f"INTELLIGENCE_CLOUD_{p}_KEY_FILE"] == ""
    assert own_only["INTELLIGENCE_OWN_MODEL"] == "true"


def test_the_worker_starts_with_the_own_route_only(own_only: dict[str, str]) -> None:
    s = WorkerSettings.from_env(own_only)
    assert s.development is False
    assert s.cloud.providers == ()
    assert s.own_model is not None and s.own_model.alias == ALIAS
    assert s.own_model.url == "http://gpu-model:8080"
    assert s.model_client == "orchestrator"


def test_a_cloud_route_escalates_and_the_own_route_takes_our_model(
    own_only: dict[str, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    def fake_parts(*args: Any, **kwargs: Any) -> tuple[Any, dict[str, str]]:
        return (lambda chunks, profile=None, generator_override=None: None), {"dense": "d"}

    monkeypatch.setattr(models, "local_parts", fake_parts)
    router = models.build_model_router(WorkerSettings.from_env(own_only))
    assert router.own is not None
    assert router.client_for("own") == (router.own, "own")
    assert router.own.model_versions()["cca"] == ALIAS
    for route, provider in (("cloud:anthropic", "anthropic"), ("cloud:gemini", "gemini")):
        client, got = router.client_for(route)
        assert got == provider and type(client).__name__ == "_Unconfigured"
    # Unconfigured: masked locally, nothing generated, escalated.
    client, _ = router.client_for("cloud:anthropic")
    g = client.generate("synthetic question", {}, None, [])
    assert g.available is False and g.cloud_outcome == "unconfigured"
    assert client.verify(g).decision == "ESCALATE"


def test_without_the_gpu_model_the_worker_has_no_route_but_starts(
    own_only: dict[str, str],
) -> None:
    """GPU_MODEL_ENABLED off: the settings still start (pipeline.sh refuses the deploy
    first, for want of a route), and the own route escalates."""
    env = {**own_only, "INTELLIGENCE_OWN_MODEL": "false"}
    s = WorkerSettings.from_env(env)
    assert s.own_model is None and s.cloud.providers == ()
    router = models.ModelRouter(StubModelClient(), {})
    assert type(router.client_for("own")[0]).__name__ == "_OwnOff"
