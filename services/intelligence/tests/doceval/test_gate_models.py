"""The answer gate's retriever and NLI models: pinned to exact revisions in
``models/answer-gate-models.lock`` and verified file by file before the gate runs. A
mismatch refuses the gate. Tiny synthetic files only; no download."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

import pytest

from intelligence.doceval.answers import gate, gate_models
from intelligence.modelstore import files
from intelligence.modelstore.reasons import Refused
from tests.doceval.answer_fakes import server_log, write_fake_llama

FILES = {
    "dense": {"config.json": b'{"synthetic": "dense"}', "1_Pooling/config.json": b"{}"},
    "nli": {"config.json": b'{"synthetic": "nli"}', "model.safetensors": b"synthetic weights"},
}


def _lock(tmp_path: Path) -> Path:
    obj = {
        "schema": gate_models.SCHEMA,
        "models": {
            "dense": {
                "repo": "sentence-transformers/all-MiniLM-L6-v2",
                "revision": "1" * 40,
                "files": {p: hashlib.sha256(b).hexdigest() for p, b in FILES["dense"].items()},
            },
            "nli": {
                "repo": "cross-encoder/nli-deberta-v3-base",
                "revision": "2" * 40,
                "files": {p: hashlib.sha256(b).hexdigest() for p, b in FILES["nli"].items()},
            },
        },
    }
    path = tmp_path / "answer-gate-models.lock"
    path.write_text(json.dumps(obj, sort_keys=True, separators=(",", ":")) + "\n")
    return path


def _models(root: Path) -> Path:
    for role, fs in FILES.items():
        for rel, body in fs.items():
            p = root / role / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(body)
    (root / "dense" / ".cache" / "huggingface").mkdir(parents=True)  # hf's own bookkeeping
    return root


def test_committed_lock_pins_exact_revisions_of_the_configured_models(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    lock = gate_models.load_lock()
    assert set(lock) == {"dense", "nli"}
    for m in lock.values():
        assert len(m.revision) == 40 and all(c in "0123456789abcdef" for c in m.revision)
        assert all(not p.endswith((".bin", ".pt", ".h5", ".onnx", ".ot")) for p in m.files)
        assert any(p.endswith(".safetensors") for p in m.files)
    monkeypatch.delenv("DENSE_MODEL", raising=False)
    monkeypatch.delenv("NLI_MODEL", raising=False)
    gate_models.check_config(lock)  # the worker's defaults are the pinned repos
    monkeypatch.setenv("NLI_MODEL", "someone/other-nli")
    with pytest.raises(Refused) as e:
        gate_models.check_config(lock)
    assert (e.value.reason, e.value.detail_code) == ("not_allowed_here", "gate_models_config")


@pytest.mark.parametrize(
    "change",
    [
        lambda o: o["models"]["nli"].update(revision="main"),
        lambda o: o["models"]["nli"]["files"].update({"../x": "0" * 64}),
        lambda o: o["models"]["nli"]["files"].update({"config.json": "zz"}),
        lambda o: o["models"].pop("dense"),
        lambda o: o.update(extra=1),
    ],
)
def test_lock_refuses_a_floating_revision_or_bad_entry(tmp_path: Path, change: object) -> None:
    path = _lock(tmp_path)
    obj = json.loads(path.read_text())
    change(obj)  # type: ignore[operator]
    path.write_text(json.dumps(obj, sort_keys=True, separators=(",", ":")) + "\n")
    with pytest.raises(Refused) as e:
        gate_models.load_lock(path)
    assert e.value.reason == "lock_invalid"


def test_verified_files_give_the_paths(tmp_path: Path) -> None:
    lock = gate_models.load_lock(_lock(tmp_path))
    root = _models(tmp_path / "models")
    assert gate_models.verify(lock, root) == {"dense": root / "dense", "nli": root / "nli"}


@pytest.mark.parametrize(
    ("tamper", "reason"),
    [
        (
            lambda r: (r / "nli" / "model.safetensors").write_bytes(b"synthetic weightz"),
            "hash_mismatch",
        ),
        (lambda r: (r / "dense" / "config.json").unlink(), "missing_file"),
        (lambda r: (r / "nli" / "pytorch_model.bin").write_bytes(b"x"), "extra_file"),
        (
            lambda r: (
                (r / "nli" / "config.json").unlink(),
                os.symlink(r / "dense" / "config.json", r / "nli" / "config.json"),
            ),
            "file_not_allowed",
        ),
    ],
)
def test_any_difference_refuses(tmp_path: Path, tamper: object, reason: str) -> None:
    lock = gate_models.load_lock(_lock(tmp_path))
    root = _models(tmp_path / "models")
    tamper(root)  # type: ignore[operator]
    with pytest.raises(Refused) as e:
        gate_models.verify(lock, root)
    assert e.value.reason == reason


def test_a_hash_mismatch_refuses_the_gate_before_anything_starts(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(gate_models, "LOCK_PATH", _lock(tmp_path))
    root = _models(tmp_path / "models")
    (root / "nli" / "model.safetensors").write_bytes(b"swapped upstream")
    monkeypatch.setenv(gate_models.DIR_ENV, str(root))
    monkeypatch.delenv("DENSE_MODEL", raising=False)
    monkeypatch.delenv("NLI_MODEL", raising=False)
    server, log = write_fake_llama(tmp_path / "bin")
    from intelligence.doceval import report
    from intelligence.doceval.answers import llama_runtime as lr

    settings = lr.Settings(server, 1 << 30, 2, 2048, 10.0, 10.0, 64)
    entry = files.FileEntry("model-q4_k_m.gguf", 1, "0" * 64)
    dev = Path(__file__).resolve().parents[4] / "testdata" / "answers" / "dev"
    with pytest.raises(Refused) as e:
        gate.evaluate_gguf_run(
            tmp_path,
            [entry],
            dev,
            report.packaged_thresholds("answer"),
            settings=settings,
            home=tmp_path / "home",
            split="dev",
        )
    assert (e.value.reason, e.value.detail_code) == ("hash_mismatch", "gate_models")
    assert server_log(log) == []  # refused before the server or any model started
    monkeypatch.delenv(gate_models.DIR_ENV)
    with pytest.raises(Refused) as e:
        gate_models.paths_from_env()
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "gate_models")
