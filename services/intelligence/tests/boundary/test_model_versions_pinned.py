"""The pipeline's pinned models (plan Task 23), without Docker: the settings, readiness
until every required role verifies, retries with a capped backoff, the log line and
metric of a refusal, ``model_versions()`` with the pinned strings, and the NLI model
loaded from its verified directory, offline.

Every model is the tiny synthetic extractor; every key, credential and id is synthetic.
"""

from __future__ import annotations

import asyncio
import os
import stat
import sys
import types
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from intelligence.boundary import models, telemetry
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import pipeline
from intelligence.boundary.verify import Verifier
from intelligence.boundary.worker import PinnedModelGate, Worker, all_ready
from intelligence.modelstore import fixtures, loader, signing, verify
from tests.boundary.tokens import Signer
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_loader import _lock_file, _settings, publish
from tests.modelstore.test_verify import Keys, MemSigner, Spy, _policy, write_signoff

ROLE = fixtures.ROLE
# The same examples as TestModelVersionAllowsHash (services/core/internal/intel).
GOOD_VERSIONS = (
    "doc-extract-tiny@0.1.0#3f9a1c2b4d5e",
    "mhai-tiny-extractor@12.0.345#0123456789ab",
    "cross-encoder/nli-deberta-v3-base",
    "v1.2@sha+x/y",
)
BAD_VERSIONS = (
    "doc-extract-tiny@0.1.0#3f9a1c2b4d5",
    "doc-extract-tiny@0.1.0#3f9a1c2b4d5e0",
    "doc-extract-tiny@0.1.0#3F9A1C2B4D5E",
    "doc-extract-tiny#3f9a1c2b4d5e",
    "doc-extract-tiny@0.1#3f9a1c2b4d5e",
    "doc-extract-tiny@01.1.0#3f9a1c2b4d5e",
    "Doc-Extract@0.1.0#3f9a1c2b4d5e",
    "doc-extract-tiny@0.1.0#3f9a1c2b4d5e#",
    "x#y",
    "#3f9a1c2b4d5e",
    "doc-extract-tiny@0.1.0 #3f9a1c2b4d5e",
)


@pytest.fixture
def keys() -> Keys:
    m, s = MemSigner(), MemSigner()
    return Keys(m, s, _policy(m, s))


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)


# ---------------------------------------------------------------- versions


def test_safe_version_keeps_hash_sign() -> None:
    cleaned = models._clean_versions({f"m{i}": v for i, v in enumerate(GOOD_VERSIONS)})
    assert sorted(cleaned.values()) == sorted(GOOD_VERSIONS)
    for v in BAD_VERSIONS:
        assert models._clean_versions({"extract": v}) == {}, v


class FakePinned:
    def __init__(self, versions: dict[str, str], paths: dict[str, Path] | None = None) -> None:
        self._versions = versions
        self._paths = paths or {}

    def versions(self) -> dict[str, str]:
        return dict(self._versions)

    def path(self, role: str) -> Path:
        return self._paths[role]


def _client(pinned: Any) -> models.OrchestratorModelClient:
    def build(chunks: Any) -> Any:
        raise AssertionError("not built")

    return models.OrchestratorModelClient(
        build,
        masker=object(),
        versions={"cca": "qwen2.5:7b-instruct", "dense": "all-MiniLM-L6-v2", "nli": "x/nli"},
        pinned=pinned,
    )


def test_model_versions_overlays_pinned_roles() -> None:
    pin = "mhai-tiny-extractor@0.1.0#0123456789ab"
    nli = "nli-small@1.0.0#abcdefabcdef"
    holder: dict[str, Any] = {"p": None}
    client = _client(lambda: holder["p"])
    legacy = {"cca": "qwen2.5:7b-instruct", "dense": "all-MiniLM-L6-v2", "nli": "x/nli"}
    assert client.model_versions() == legacy
    holder["p"] = FakePinned({"extract": pin, "nli": nli})
    assert client.model_versions() == {**legacy, "extract": pin, "nli": nli}


def test_versions_from_verified_not_lock(tmp_path: Path, keys: Keys) -> None:
    """model_versions() reports what was verified: a lock rewritten on disk after start
    changes nothing until restart."""
    mem = MemoryStore()
    first = publish(mem, tmp_path, keys)
    second = publish(mem, tmp_path, keys, version="0.2.0", seed=8)
    settings = _settings(tmp_path, {ROLE: first})
    gate = PinnedModelGate(lambda: loader.PinnedModels.load(settings, mem, keys.policy), (ROLE,))
    client = _client(lambda: gate.models)
    assert gate.attempt() is True
    want = f"{fixtures.MODEL_NAME}@0.1.0#{first.sha[:12]}"
    assert client.model_versions()[ROLE] == want
    _lock_file(tmp_path, {ROLE: second})
    assert gate.attempt() is True  # a retry verifies the pin read at start
    assert client.model_versions()[ROLE] == want


# ---------------------------------------------------------------- readiness


class FakePsub:
    def __init__(self) -> None:
        self.fetches = 0

    async def fetch(self, n: int, timeout: float) -> list[Any]:
        self.fetches += 1
        return []


def _pipeline_worker(jobs_ready: Any) -> Worker:
    signer = Signer()
    ops = Operations.load(development=True)
    clock = __import__("time").time
    v = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()), "pipeline", ops, JtiSet(clock), clock
    )
    settings = WorkerSettings(
        service="pipeline",
        nats_url="nats://127.0.0.1:4222",
        trusted_keys_path="/nonexistent",
        development=True,
    )

    async def job(*_a: Any) -> None:
        raise AssertionError("no job may run")

    return Worker(
        settings,
        pipeline.handlers(models_ready=jobs_ready),
        v,
        jobs={"run_query": job, "approve_correction": job},
        jobs_ready=jobs_ready,
    )


def test_unverified_pin_not_ready_and_no_jobs(tmp_path: Path, keys: Keys) -> None:
    """B18: a tampered pin. Health says models_ready false and the job consumer never
    fetches, even with a database that answers."""
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    spy = Spy(mem)
    spy.corrupt.add("model.safetensors")
    settings = _settings(tmp_path, {ROLE: pub})
    gate = PinnedModelGate(lambda: loader.PinnedModels.load(settings, spy, keys.policy), (ROLE,))
    assert gate.attempt() is False
    assert gate.ready() is False and gate.models is None
    assert gate.failures == {(ROLE, "hash_mismatch"): 1}

    async def database_ready() -> bool:
        return True

    ready = all_ready(database_ready, gate.ready)

    async def go() -> None:
        health = pipeline.health_handler(database_ready, ready)
        _, out = await health(None, pipeline_pb2.HealthRequest())  # type: ignore[arg-type]
        assert isinstance(out, pipeline_pb2.HealthResponse)
        assert (out.models_ready, out.database_ready) == (False, True)
        w = _pipeline_worker(ready)
        psub = FakePsub()
        task = asyncio.get_running_loop().create_task(
            w._consume("pipeline", psub, asyncio.Semaphore(1))
        )
        await asyncio.sleep(0.05)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert psub.fetches == 0
        assert w.refusals["jobs_not_ready"] == 1

    asyncio.run(go())


def test_all_ready_fails_closed() -> None:
    def boom() -> bool:
        raise RuntimeError("synthetic")

    async def yes() -> bool:
        return True

    async def go() -> None:
        assert await all_ready(yes, lambda: True)() is True
        assert await all_ready(yes, lambda: False)() is False
        assert await all_ready(yes, boom)() is False
        assert await all_ready(lambda: 1)() is False  # only True is ready

    asyncio.run(go())


@pytest.fixture
def metrics() -> Iterator[InMemoryMetricReader]:
    reader = InMemoryMetricReader()
    stop = telemetry.setup(metric_reader=reader)
    try:
        yield reader
    finally:
        stop()


def _failures(reader: InMemoryMetricReader) -> dict[tuple[str, str], int]:
    out: dict[tuple[str, str], int] = {}
    data = reader.get_metrics_data()
    for rm in data.resource_metrics if data else ():
        for sm in rm.scope_metrics:
            for m in sm.metrics:
                if m.name == "mhai_model_verify_failures_total":
                    for p in m.data.data_points:
                        out[(p.attributes["role"], p.attributes["reason"])] = int(p.value)
    return out


def test_ready_after_signoff_appears(
    tmp_path: Path,
    keys: Keys,
    caplog: pytest.LogCaptureFixture,
    metrics: InMemoryMetricReader,
) -> None:
    """A deploy that arrives before its sign-off: refused (not_approved), retried with
    backoff, each retry a full verification of the same pin, ready once the sign-off
    exists. No job slot is taken meanwhile."""
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys, with_signoff=False)
    settings = _settings(tmp_path, {ROLE: pub})
    spy = Spy(mem)
    delays: list[float] = []

    async def sleep(s: float) -> None:
        delays.append(s)
        if len(delays) == 3:
            write_signoff(mem, pub, keys.signoff)

    gate = PinnedModelGate(
        lambda: loader.PinnedModels.load(settings, spy, keys.policy),
        (ROLE,),
        first_delay=2.0,
        max_delay=5.0,
        sleep=sleep,
    )
    caplog.set_level("INFO")
    asyncio.run(gate.run())
    assert gate.ready() is True
    assert delays == [2.0, 4.0, 5.0]
    assert gate.failures == {(ROLE, "not_approved"): 3}
    assert gate.models is not None
    assert gate.models.versions() == {ROLE: f"{fixtures.MODEL_NAME}@0.1.0#{pub.sha[:12]}"}
    # Every attempt read the pinned manifest again (a full verification), and only it.
    manifests = [r for r in spy.reads if r[1].endswith("/manifest.json")]
    assert all("/0.1.0/" in r[1] for r in manifests)
    assert len([r for r in manifests if r[0] == "versions"]) == 4
    failed = [r.getMessage() for r in caplog.records if "model.verify_failed" in r.getMessage()]
    assert len(failed) == 3
    assert all(m.startswith(f"model.verify_failed role={ROLE} reason=not_approved") for m in failed)
    loaded = [r.getMessage() for r in caplog.records if r.getMessage().startswith("model.loaded")]
    assert len(loaded) == 1
    assert pub.sha in loaded[0] and fixtures.MODEL_NAME in loaded[0] and "0.1.0" in loaded[0]
    assert _failures(metrics) == {(ROLE, "not_approved"): 3}


def test_metric_labels_come_from_closed_sets(metrics: InMemoryMetricReader) -> None:
    def load() -> Any:
        raise loader.PinRefused("someone_elses_role", "internal")

    gate = PinnedModelGate(load, (ROLE,))
    assert gate.attempt() is False

    def load2() -> Any:
        raise RuntimeError("synthetic: /etc/secret-path")

    gate2 = PinnedModelGate(load2, (ROLE,))
    assert gate2.attempt() is False
    assert _failures(metrics) == {("other", "internal"): 1, ("all", "internal"): 1}


# ---------------------------------------------------------------- NLI


def test_nli_gets_verified_path_offline(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    import intelligence.agents.hga_nli as hga_nli

    seen: list[dict[str, Any]] = []

    class Recorder:
        def __init__(self, **kw: Any) -> None:
            seen.append(kw)

    monkeypatch.setattr(hga_nli, "NLIEntailmentVerifier", Recorder)
    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)
    path = tmp_path / "verified-nli"
    path.mkdir()
    pinned = FakePinned({"nli": "nli-small@1.0.0#abcdefabcdef"}, {"nli": path})
    holder: dict[str, Any] = {"p": None}
    nli = models.nli_factory("cross-encoder/legacy", lambda: holder["p"], pinned_nli=True)
    with pytest.raises(RuntimeError):
        nli()  # pinned but not yet verified: never the legacy model, never the hub
    assert seen == []
    holder["p"] = pinned
    first = nli()
    assert nli() is first  # shared
    assert seen == [
        {"model_name": str(path), "device": "cpu", "local_files_only": True, "require_model": True}
    ]
    assert os.environ["HF_HUB_OFFLINE"] == "1"

    # Not pinned: the configured model, as before.
    seen.clear()
    legacy = models.nli_factory("cross-encoder/legacy", lambda: None, pinned_nli=False)
    legacy()
    assert seen == [{"model_name": "cross-encoder/legacy", "device": "cpu"}]


def test_nli_loader_passes_safetensors_and_local_only(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, str, dict[str, Any]]] = []

    class FakeModel:
        config = types.SimpleNamespace(id2label={0: "contradiction", 1: "neutral", 2: "entailment"})

        def to(self, device: str) -> FakeModel:
            return self

        def eval(self) -> None:
            return None

    class AutoTokenizer:
        @staticmethod
        def from_pretrained(name: str, **kw: Any) -> object:
            calls.append(("tokenizer", name, kw))
            return object()

    class AutoModelForSequenceClassification:
        @staticmethod
        def from_pretrained(name: str, **kw: Any) -> FakeModel:
            calls.append(("model", name, kw))
            return FakeModel()

    transformers = types.ModuleType("transformers")
    transformers.AutoTokenizer = AutoTokenizer  # type: ignore[attr-defined]
    transformers.AutoModelForSequenceClassification = AutoModelForSequenceClassification  # type: ignore[attr-defined]
    torch = types.ModuleType("torch")
    torch.cuda = types.SimpleNamespace(is_available=lambda: False)  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "transformers", transformers)
    monkeypatch.setitem(sys.modules, "torch", torch)
    from intelligence.agents.hga_nli import NLIEntailmentVerifier

    v = NLIEntailmentVerifier(
        model_name="/verified/nli", device="cpu", local_files_only=True, require_model=True
    )
    assert v._loaded is True
    assert calls == [
        ("tokenizer", "/verified/nli", {"local_files_only": True}),
        ("model", "/verified/nli", {"local_files_only": True, "use_safetensors": True}),
    ]

    def broken(name: str, **kw: Any) -> Any:
        raise OSError("synthetic: no safetensors file")

    monkeypatch.setattr(AutoModelForSequenceClassification, "from_pretrained", broken)
    with pytest.raises(RuntimeError):
        NLIEntailmentVerifier(model_name="/verified/nli", device="cpu", require_model=True)
    # The legacy path keeps its rule-based fallback.
    assert NLIEntailmentVerifier(model_name="x", device="cpu")._loaded is False


# ---------------------------------------------------------------- settings


BASE = {
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/keys",
    "APP_ENV": "development",
}


def _secret(tmp_path: Path, name: str, value: str, mode: int = 0o600) -> str:
    p = tmp_path / name
    p.write_text(value)
    os.chmod(p, mode)
    return str(p)


def _store_env(tmp_path: Path, **over: str) -> dict[str, str]:
    env = {
        **BASE,
        "INTELLIGENCE_MODEL_ROLES": "extract",
        "INTELLIGENCE_MODELS_LOCK": str(tmp_path / "models.lock"),
        "INTELLIGENCE_MODEL_CACHE": str(tmp_path / "cache"),
        "MODEL_STORE_ENDPOINT": "https://store.synthetic.test",
        "MODEL_STORE_BUCKET": "mhai-models",
        "MODEL_STORE_READER_ACCESS_KEY_FILE": _secret(tmp_path, "ak", "SYNTHETICREADERKEY01"),
        "MODEL_STORE_READER_SECRET_KEY_FILE": _secret(
            tmp_path, "sk", "synthetic-reader-secret-0123456789abcdef"
        ),
        "MHAI_MODELS_TRUST_POLICY": str(tmp_path / "trust-policy.json"),
    }
    env.update(over)
    return env


def test_store_settings_from_env(tmp_path: Path) -> None:
    s = WorkerSettings.from_env(_store_env(tmp_path))
    assert s.model_roles == ("extract",)
    assert s.model_store_access_key == "SYNTHETICREADERKEY01"
    assert "synthetic-reader-secret" not in repr(s) and "SYNTHETICREADERKEY01" not in repr(s)
    assert s.model_store_endpoint == "https://store.synthetic.test"


def test_no_roles_needs_no_store_settings() -> None:
    for roles in (None, "", "  "):
        env = dict(BASE)
        if roles is not None:
            env["INTELLIGENCE_MODEL_ROLES"] = roles
        s = WorkerSettings.from_env(env)
        assert s.model_roles == ()


@pytest.mark.parametrize(
    "value",
    [
        "",
        "changeme",
        "minioadmin",
        "placeholder",
        "example",
        "my-change-me-key",
        "AKIAEXAMPLEKEY",
        "MinioAdmin123",
    ],
)
def test_placeholder_store_credentials_refused(tmp_path: Path, value: str) -> None:
    for var in ("MODEL_STORE_READER_ACCESS_KEY_FILE", "MODEL_STORE_READER_SECRET_KEY_FILE"):
        env = _store_env(tmp_path, **{var: _secret(tmp_path, "bad", value)})
        with pytest.raises(ConfigError) as e:
            WorkerSettings.from_env(env)
        assert var in str(e.value)
        if value:
            assert value not in str(e.value)


def test_store_credentials_only_from_safe_files(tmp_path: Path) -> None:
    good = "synthetic-reader-secret-0123456789abcdef"
    loose = _secret(tmp_path, "loose", good, 0o640)
    link = tmp_path / "link"
    link.symlink_to(_secret(tmp_path, "target", good))
    fifo = tmp_path / "fifo"
    os.mkfifo(fifo)
    assert stat.S_ISFIFO(os.stat(fifo).st_mode)
    for path in (loose, str(link), str(fifo), str(tmp_path / "missing")):
        env = _store_env(tmp_path, MODEL_STORE_READER_SECRET_KEY_FILE=path)
        with pytest.raises(ConfigError, match="MODEL_STORE_READER_SECRET_KEY_FILE"):
            WorkerSettings.from_env(env)
    for var in ("MODEL_STORE_READER_ACCESS_KEY", "MODEL_STORE_READER_SECRET_KEY"):
        with pytest.raises(ConfigError, match="_FILE"):
            WorkerSettings.from_env(_store_env(tmp_path, **{var: good}))


@pytest.mark.parametrize(
    "over,name",
    [
        ({"INTELLIGENCE_MODEL_ROLES": "Extract"}, "INTELLIGENCE_MODEL_ROLES"),
        ({"INTELLIGENCE_MODEL_ROLES": "extract,,nli"}, "INTELLIGENCE_MODEL_ROLES"),
        ({"INTELLIGENCE_MODEL_ROLES": "extract,extract"}, "INTELLIGENCE_MODEL_ROLES"),
        (
            {"INTELLIGENCE_MODEL_ROLES": ",".join(f"r{i:02d}" for i in range(17))},
            "INTELLIGENCE_MODEL_ROLES",
        ),
        ({"INTELLIGENCE_SERVICE": "echo"}, "INTELLIGENCE_MODEL_ROLES"),
        ({"INTELLIGENCE_MODELS_LOCK": ""}, "INTELLIGENCE_MODELS_LOCK"),
        ({"INTELLIGENCE_MODEL_CACHE": "relative/cache"}, "INTELLIGENCE_MODEL_CACHE"),
        ({"MODEL_STORE_ENDPOINT": "http://store.synthetic.test"}, "MODEL_STORE_ENDPOINT"),
        ({"MODEL_STORE_ENDPOINT": "https://user:pw@store.synthetic.test"}, "MODEL_STORE_ENDPOINT"),
        ({"MODEL_STORE_ENDPOINT": ""}, "MODEL_STORE_ENDPOINT"),
        ({"MODEL_STORE_BUCKET": "Bad_Bucket"}, "MODEL_STORE_BUCKET"),
        ({"MODEL_STORE_REGION": "nowhere"}, "MODEL_STORE_REGION"),
        ({"MHAI_MODELS_TRUST_POLICY": ""}, "MHAI_MODELS_TRUST_POLICY"),
        ({"APP_ENV": "production", "MHAI_MODELS_ENV": "development"}, "MHAI_MODELS_ENV"),
    ],
)
def test_malformed_model_settings_refused(tmp_path: Path, over: dict[str, str], name: str) -> None:
    with pytest.raises(ConfigError, match=name):
        WorkerSettings.from_env(_store_env(tmp_path, **over))


def test_loopback_store_may_use_plain_http(tmp_path: Path) -> None:
    s = WorkerSettings.from_env(_store_env(tmp_path, MODEL_STORE_ENDPOINT="http://127.0.0.1:9000"))
    assert s.model_store_endpoint == "http://127.0.0.1:9000"


def test_malformed_store_setting_refused_without_roles(tmp_path: Path) -> None:
    env = {**BASE, "MODEL_STORE_ENDPOINT": "http://store.synthetic.test"}
    with pytest.raises(ConfigError, match="MODEL_STORE_ENDPOINT"):
        WorkerSettings.from_env(env)


def test_model_gate_set_up_once_at_start(tmp_path: Path, keys: Keys) -> None:
    from intelligence.boundary.worker import build_model_gate
    from tests.modelstore.conftest import REPO

    assert build_model_gate(WorkerSettings.from_env(dict(BASE))) is None
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    env = _store_env(
        tmp_path,
        INTELLIGENCE_MODELS_LOCK=str(_lock_file(tmp_path, {ROLE: pub})),
        MHAI_MODELS_TRUST_POLICY=str(REPO / "models" / "trust-policy.json"),
    )
    gate = build_model_gate(WorkerSettings.from_env(env))
    assert gate is not None and gate.roles == (ROLE,) and not gate.ready()
    # A lock that doesn't pin a required role stops the start.
    missing = _store_env(
        tmp_path,
        INTELLIGENCE_MODEL_ROLES="extract,nli",
        INTELLIGENCE_MODELS_LOCK=env["INTELLIGENCE_MODELS_LOCK"],
        MHAI_MODELS_TRUST_POLICY=env["MHAI_MODELS_TRUST_POLICY"],
    )
    with pytest.raises(ConfigError, match="lock_invalid"):
        build_model_gate(WorkerSettings.from_env(missing))
    bad_policy = {**env, "MHAI_MODELS_TRUST_POLICY": str(tmp_path / "missing.json")}
    with pytest.raises(ConfigError, match="policy_invalid"):
        build_model_gate(WorkerSettings.from_env(bad_policy))
