"""Promoting and pinning the answer model (role ``cca``, suite ``clinical_answer``): the gate
scores the run's GGUF itself through llama.cpp (security review of #143), here a fake
``llama-server`` (tests.doceval.answer_fakes) that behaves as the GGUF it is given says.
In-memory store, loopback only, synthetic data only."""

from __future__ import annotations

import hashlib
import json
import socket
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import report
from intelligence.doceval.answers import llama_runtime as lr
from intelligence.doceval.answers import report as areport
from intelligence.doceval.answers import schema
from intelligence.modelstore import (
    canonical,
    fixtures,
    lock,
    manifest,
    promote,
    signing,
    sources,
    store,
    testsets,
    training,
    verify,
)
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.sources.local import LocalSource
from tests.doceval.answer_fakes import marker, server_log, test_builder, write_fake_llama
from tests.modelstore.fakes import gguf as g
from tests.modelstore.fakes.memory_store import MemoryStore

REPO = Path(__file__).resolve().parents[4]
DEV = REPO / "testdata" / "answers" / "dev"
NAME = fixtures.MODEL_NAME
RUN = "run-0001"
WORKFLOW = promote.WorkflowRun(
    workflow_run_id="4242",
    workflow_ref="Measured-Health/mhai-multiagent/.github/workflows/model-promote.yml@refs/heads/main",
    commit="ab" * 20,
    actor="promoter-bot",
)


@pytest.fixture(autouse=True)
def _development(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(signing.ENV_VAR, "development")
    monkeypatch.setenv(verify.APP_ENV, "development")
    monkeypatch.delenv(promote.EVAL_ROLE_ENV, raising=False)


@dataclass(frozen=True)
class Keys:
    signer: signing.FileSigner
    policy: signing.TrustPolicy


@pytest.fixture
def keys(tmp_path: Path) -> Keys:
    signing.generate_dev_key(tmp_path / "keys")
    signer = signing.FileSigner(tmp_path / "keys" / signing.DEV_KEY_FILE)
    obj = {
        "schema": signing.TRUST_SCHEMA,
        "signers": [
            {
                "alg": "ed25519",
                "key_id": signer.key_id,
                "public_key_pem": signer.public_key_pem(),
                "development": True,
            }
        ],
    }
    return Keys(signer, signing.parse_policy(obj, environment="development"))


@pytest.fixture(scope="module")
def registries() -> tuple[training.Registry, training.Licences]:
    return (
        training.load_registry(REPO / "models" / "approved-datasets.json"),
        training.load_licences(REPO / "models" / "licences.json"),
    )


def _cca_run(root: Path, mode: str = "good", seed: int = 3, gguf: bool = True) -> Path:
    run = fixtures.make_run(root / "models" / NAME / RUN, "good", seed, role="cca")
    if gguf:
        kvs = [*g.tiny_kvs(), g.kv_string(b"general.name", marker(mode))]
        (run / "model-q4_k_m.gguf").write_bytes(g.gguf(kvs, g.TINY_INFOS, g.TINY_DATA))
        text = g.TINY_TEMPLATE.decode("utf-8")
        (run / "chat_template.json").write_text(json.dumps({"chat_template": text}))
    return root


def _ctx(
    root: Path,
    registries: tuple[training.Registry, training.Licences],
    server: Path,
    split: promote.Split | None = None,
) -> promote.Context:
    def source_for(scratch: sources.Scratch) -> sources.Source:
        return LocalSource(root)

    settings = lr.Settings(
        server=server,
        mem_bytes=1 << 30,
        threads=2,
        ctx=2048,
        start_timeout_s=20.0,
        answer_timeout_s=10.0,
        max_tokens=64,
    )
    return promote.Context(
        registry=registries[0],
        licences=registries[1],
        source_for=source_for,
        split=split or promote.DevSplit(DEV),
        llama=settings,
        answer_builder=test_builder,
    )


def _request(version: str = "1.0.0", **kw: Any) -> promote.Request:
    base = promote.Request(name=NAME, version=version, source="local", run=RUN)
    return replace(base, **kw)


def _dev_items() -> tuple[schema.Item, ...]:
    return schema.load_set(DEV).items


def _dry(
    tmp_path: Path,
    registries: Any,
    mode: str = "good",
    seed: int = 3,
    split: promote.Split | None = None,
) -> Any:
    server, log = write_fake_llama(tmp_path / f"bin-{mode}-{seed}", _dev_items())
    root = _cca_run(tmp_path / f"src-{mode}-{seed}", mode, seed)
    summary = promote.promote(_request(dry_run=True), _ctx(root, registries, server, split))
    return summary, log


# ---------------------------------------------------------------- the suite


def test_cca_has_its_suite_and_is_pinnable() -> None:
    assert promote.SUITES["cca"] == "clinical_answer"
    assert "cca" in lock.ROLES
    assert "clinical_answer" in testsets.SUITES


def test_pass_on_the_dev_split_scores_the_gguf_through_llama(
    tmp_path: Path, registries: Any
) -> None:
    summary, log = _dry(tmp_path, registries)
    assert summary.split == "dev" and summary.verdicts == {"answer": "pass"}
    rep = areport.parse_report(summary.documents["evaluation.json"])
    assert rep["engine"] == {"name": "llama-cpp", "version": lr.ENGINE_VERSION}
    assert rep["testset"]["name"] == "clinical_answer" and rep["sign_off"] == "pilot, unsigned"
    warm = json.loads(summary.documents["warmup.json"])
    assert set(warm) == {"answered", "engine", "engine_version", "question_sha256"}
    assert warm["answered"] is True and warm["engine"] == "llama-cpp"
    (entry,) = server_log(log)
    model = entry["argv"][entry["argv"].index("-m") + 1]
    assert model.endswith("/model-q4_k_m.gguf")
    card = summary.documents["model_card.md"].decode()
    assert "answer" in card.lower() and "`answer`" in card


def test_the_gguf_is_what_is_scored_not_the_safetensors(tmp_path: Path, registries: Any) -> None:
    """Same safetensors, different GGUF: the verdict follows the GGUF. Different
    safetensors, same GGUF: the verdict does not move."""
    good, log = _dry(tmp_path, registries, "good", seed=3)
    unsafe, _ = _dry(tmp_path, registries, "unsafe", seed=3)
    other_weights, _ = _dry(tmp_path, registries, "good", seed=4)
    assert good.verdicts == {"answer": "pass"} and unsafe.verdicts == {"answer": "fail"}
    assert other_weights.verdicts == {"answer": "pass"}
    assert good.files_sha256 != other_weights.files_sha256  # the safetensors differ
    # the server was only ever given the GGUF
    (entry,) = server_log(log)
    assert not any(a.endswith(".safetensors") for a in entry["argv"])


@pytest.mark.parametrize(
    ("mode", "failed"), [("miss", "escalation_recall"), ("unsafe", "unsafe_rate")]
)
def test_one_missed_escalation_or_one_unsafe_statement_fails(
    tmp_path: Path, registries: Any, mode: str, failed: str
) -> None:
    summary, _ = _dry(tmp_path, registries, mode)
    rep = areport.parse_report(summary.documents["evaluation.json"])
    assert rep["verdicts"]["answer"]["failed"] == [f"overall.{failed}"]


def test_the_evaluation_makes_no_connection_off_loopback(
    tmp_path: Path, registries: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    real = socket.socket.connect
    seen: list[Any] = []

    def guarded(self: socket.socket, address: Any) -> Any:
        seen.append(address)
        host = address[0] if isinstance(address, tuple) else address
        if host not in ("127.0.0.1", "::1"):
            raise AssertionError("network access during the evaluation")
        return real(self, address)

    monkeypatch.setattr(socket.socket, "connect", guarded)
    # the dev split built into the job's scratch from the bank (no --dev-set)
    summary, log = _dry(tmp_path, registries, split=promote.DevSplit(None))
    assert summary.verdicts == {"answer": "pass"} and seen
    (entry,) = server_log(log)
    assert "--offline" in entry["argv"]
    assert not {"HTTP_PROXY", "HTTPS_PROXY", "HF_TOKEN"} & set(entry["env"])


def test_a_cca_run_without_a_gguf_has_no_runtime(tmp_path: Path, registries: Any) -> None:
    server, log = write_fake_llama(tmp_path / "bin")
    root = _cca_run(tmp_path / "src", gguf=False)
    with pytest.raises(Refused) as e:
        promote.promote(_request(dry_run=True), _ctx(root, registries, server))
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "no_gguf")
    assert server_log(log) == []


@pytest.mark.parametrize(
    ("kw", "detail"),
    [
        ({"gates": ("phase1",)}, "gate"),
        ({"runtime_url": "http://127.0.0.1:8000/v1"}, "runtime_url"),
    ],
)
def test_cca_takes_only_its_own_gate_and_starts_its_own_runtime(
    tmp_path: Path, registries: Any, kw: dict[str, Any], detail: str
) -> None:
    server, log = write_fake_llama(tmp_path / "bin")
    root = _cca_run(tmp_path / "src")
    with pytest.raises(Refused) as e:
        promote.promote(_request(dry_run=True, **kw), _ctx(root, registries, server))
    assert e.value.detail_code == detail
    assert server_log(log) == []


def test_extract_cannot_take_the_answer_gate() -> None:
    with pytest.raises(Refused) as e:
        promote.promotion_gates(_request(gates=("answer",)), "clinical_document")
    assert (e.value.reason, e.value.detail_code) == ("invalid_name", "gate")
    assert promote.promotion_gates(_request(), "clinical_answer") == ("answer",)


# ---------------------------------------------------------------- promote, then pin


def _locked_split(mem: MemoryStore, keys: Keys, tmp_path: Path) -> testsets.LockEntry:
    built = testsets.build_and_lock(
        mem, keys.signer, keys.policy, suite="clinical_answer", version="1.0.0", scale=1.0
    )
    return built.entry


def _promote_and_pin(tmp_path: Path, registries: Any, keys: Keys, mode: str) -> lock.Pin:
    mem = MemoryStore()
    entry = _locked_split(mem, keys, tmp_path)
    fetched = testsets.fetch(mem, entry, keys.policy, tmp_path / "fetched")
    server, _ = write_fake_llama(tmp_path / "bin", schema.load_set(fetched).items)
    root = _cca_run(tmp_path / "src", mode)
    ctx = replace(
        _ctx(root, registries, server, promote.LockedSplit(mem, entry, keys.policy)),
        store=mem,
        signer=keys.signer,
        policy=keys.policy,
        workflow=WORKFLOW,
    )
    summary = promote.promote(_request("1.0.0", dry_run=False), ctx)
    assert summary.split == "test" and summary.manifest_sha256 is not None
    m = manifest.parse(
        mem.get(
            store.manifest_key(NAME, "1.0.0"),
            mem.versions(store.manifest_key(NAME, "1.0.0"))[0],
            max_bytes=1 << 20,
        )
    )
    gguf = [f for f in m.files if f.path.endswith(".gguf")]
    assert len(gguf) == 1 and m.role == "cca"
    assert m.runtime.engine == "llama-cpp" and m.runtime.engine_version == lr.ENGINE_VERSION
    lock_path = tmp_path / "models.lock"
    lock_path.write_bytes(lock.to_bytes(lock.empty()))
    return lock.pin(
        "cca",
        NAME,
        "1.0.0",
        "answer",
        source=mem,
        policy=keys.policy,
        cache_dir=tmp_path / "cache",
        lock_path=lock_path,
        testsets_lock={
            (entry.suite, entry.version): testsets.LockEntry(
                entry.suite, entry.version, entry.sha256
            )
        },
    )


def test_a_passing_gguf_on_the_locked_split_is_pinnable(
    tmp_path: Path, registries: Any, keys: Keys
) -> None:
    pin = _promote_and_pin(tmp_path, registries, keys, "good")
    assert (pin.name, pin.version, pin.gate) == (NAME, "1.0.0", "answer")
    assert lock.read(tmp_path / "models.lock").models["cca"] == pin


@pytest.mark.parametrize("mode", ["miss", "unsafe"])
def test_a_failing_gguf_is_signed_but_never_pinned(
    tmp_path: Path, registries: Any, keys: Keys, mode: str
) -> None:
    with pytest.raises(Refused) as e:
        _promote_and_pin(tmp_path, registries, keys, mode)
    assert e.value.reason == "gate_failed"
    assert lock.read(tmp_path / "models.lock").models == {}


def test_a_dev_split_promotion_is_never_pinned(tmp_path: Path, registries: Any, keys: Keys) -> None:
    mem = MemoryStore()
    server, _ = write_fake_llama(tmp_path / "bin", _dev_items())
    root = _cca_run(tmp_path / "src")
    ctx = replace(
        _ctx(root, registries, server),
        store=mem,
        signer=keys.signer,
        policy=keys.policy,
        workflow=WORKFLOW,
    )
    promote.promote(_request("1.0.0", dry_run=False), ctx)
    lock_path = tmp_path / "models.lock"
    lock_path.write_bytes(lock.to_bytes(lock.empty()))
    with pytest.raises(Refused) as e:
        lock.pin(
            "cca",
            NAME,
            "1.0.0",
            "answer",
            source=mem,
            policy=keys.policy,
            cache_dir=tmp_path / "cache",
            lock_path=lock_path,
            testsets_lock={},
        )
    assert (e.value.reason, e.value.detail_code) == ("testset_mismatch", "split")


def test_pin_refuses_a_gate_of_another_suite(tmp_path: Path) -> None:
    for role, gate in (("cca", "phase1"), ("extract", "answer")):
        with pytest.raises(Refused) as e:
            lock.pin(
                role,
                NAME,
                "1.0.0",
                gate,
                source=None,  # type: ignore[arg-type]
                policy=None,  # type: ignore[arg-type]
                cache_dir=tmp_path / "cache",
                lock_path=tmp_path / "models.lock",
                testsets_lock={},
            )
        assert (e.value.reason, e.value.detail_code) == ("gate_failed", "suite")
    entry = lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")
    with pytest.raises(Refused) as e:
        lock.check_production("cca", entry)
    assert (e.value.reason, e.value.detail_code) == ("gate_failed", "suite")


def test_thresholds_hash_is_the_committed_answer_file() -> None:
    raw = (report.THRESHOLDS_DIR / "answer.json").read_bytes()
    assert (
        lock.thresholds_sha256(report.THRESHOLDS_DIR, "answer") == hashlib.sha256(raw).hexdigest()
    )
    assert canonical.dumps(canonical.loads_strict(raw, 1 << 20)) == raw
