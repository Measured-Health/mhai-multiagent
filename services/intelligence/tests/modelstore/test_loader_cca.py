"""The pinned answer model at start-up (own answer model plan, PR 3, Task 9): the worker
verifies the ``cca`` pin (hash, signature, gate and every file, the existing path),
writes ``active.json`` for the ``llama`` sidecar, waits for the sidecar to serve the
verified ``<name>@<version>#<sha12>``, and asks the warm-up question (liveness only).

A pin is verified offline from the mirror ``model-fetch`` writes (:class:`DirReadStore`),
exactly as from the store. Every model, key and answer is synthetic.
"""

from __future__ import annotations

import hashlib
import json
import os
import stat
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.answers import gate as answer_gate
from intelligence.doceval.answers import llama_runtime
from intelligence.modelstore import (
    canonical,
    fixtures,
    loader,
    lock,
    manifest,
    mirror,
    signing,
    store,
    verify,
)
from intelligence.modelstore.reasons import Refused
from tests.modelstore.fakes import gguf as g
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_verify import (
    RUN,
    Keys,
    MemSigner,
    Published,
    _manifest_bytes,
    _policy,
    _put,
)

NAME = "mhai-cca-synthetic"
VERSION = "1.0.0"
GATE = "answer"


@pytest.fixture
def keys() -> Keys:
    m = MemSigner()
    return Keys(m, _policy(m))


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)


def warmup_doc(**over: Any) -> bytes:
    doc: dict[str, Any] = {
        "question_sha256": hashlib.sha256(answer_gate.WARMUP_QUESTION.encode()).hexdigest(),
        "engine": llama_runtime.ENGINE,
        "engine_version": llama_runtime.ENGINE_VERSION,
        "answered": True,
    }
    doc.update(over)
    return canonical.dumps(doc)


def _cca_run(root: Path, seed: int = 3) -> Path:
    run = fixtures.make_run(root, "good", seed, role="cca", name=NAME)
    kvs = [*g.tiny_kvs(), g.kv_string(b"general.name", b"mhai-synthetic-cca")]
    (run / "model-q4_k_m.gguf").write_bytes(g.gguf(kvs, g.TINY_INFOS, g.TINY_DATA))
    text = g.TINY_TEMPLATE.decode("utf-8")
    (run / "chat_template.json").write_text(json.dumps({"chat_template": text}))
    return run


def publish_cca(
    target: Any, tmp_path: Path, keys: Keys, *, warmup: bytes | None = None, seed: int = 3
) -> Published:
    run_dir = _cca_run(tmp_path / f"run-cca-{seed}", seed)
    records = []
    for p in sorted(os.listdir(run_dir)):
        body = (run_dir / p).read_bytes()
        vid = _put(target, store.run_file_key(NAME, VERSION, RUN, p), body)
        records.append(manifest.FileRecord(p, len(body), hashlib.sha256(body).hexdigest(), vid))
    docs = {}
    for d in manifest.DOCUMENTS:
        body = (warmup or warmup_doc()) if d == "warmup.json" else f'{{"synthetic":"{d}"}}'.encode()
        vid = _put(target, store.run_doc_key(NAME, VERSION, RUN, d), body)
        docs[d] = manifest.DocumentRecord(hashlib.sha256(body).hexdigest(), vid)
    raw = _manifest_bytes(
        records, docs, name=NAME, version=VERSION, role="cca", gates={GATE: "pass"}
    )
    _put(target, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    _put(target, store.manifest_key(NAME, VERSION), raw)
    return Published(raw, manifest.sha256(raw), manifest.parse(raw), run_dir)


def _settings(tmp_path: Path, pub: Published) -> loader.LoadSettings:
    cache = tmp_path / "cache"
    cache.mkdir(mode=0o700, exist_ok=True)
    path = tmp_path / "models.lock"
    lock.write(path, lock.Lock({"cca": lock.Pin(NAME, VERSION, pub.sha, GATE)}))
    return loader.LoadSettings.from_lock(("cca",), path, cache, start_timeout_s=1.0)


class FakeSidecar:
    """The llama sidecar as the loader sees it: serves ``alias`` (None: whatever
    active.json names, as the real entrypoint would) and answers ``answer``."""

    def __init__(self, cache: Path, alias: str | None = None, answer: str = "Metformin.") -> None:
        self.cache = cache
        self.alias = alias
        self.answer = answer
        self.asked: list[tuple[str, str, str]] = []
        self.waited: list[tuple[str, float]] = []

    def wait_for_alias(self, expected: str, timeout_s: float) -> None:
        self.waited.append((expected, timeout_s))
        served = self.alias
        if served is None:
            active = json.loads((self.cache / loader.ACTIVE_FILE).read_text())
            served = active["version_string"]
        if served != expected:
            raise _Timeout()

    def ask(self, alias: str, system: str, user: str) -> str:
        self.asked.append((alias, system, user))
        return self.answer


class _Timeout(Exception):
    detail = "start_timeout"


def _load(settings: loader.LoadSettings, source: Any, keys: Keys, side: Any) -> Any:
    return loader.PinnedModels.load(settings, source, keys.policy, sidecar=side)


def test_cca_pin_writes_active_json_and_serves_its_alias(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    side = FakeSidecar(settings.cache_dir)
    pinned = _load(settings, mem, keys, side)
    vs = verify.version_string(NAME, VERSION, pub.sha)
    assert pinned.versions() == {"cca": vs}
    raw = (settings.cache_dir / loader.ACTIVE_FILE).read_bytes()
    gguf = next(f for f in pub.m.files if f.path.endswith(".gguf"))
    assert json.loads(raw) == {
        "gguf": f"{NAME}/{VERSION}-{pub.sha[:12]}/{gguf.path}",
        "sha256": gguf.sha256,
        "version_string": vs,
    }
    assert canonical.dumps(json.loads(raw)) == raw  # canonical, so the entrypoint's match holds
    # The named file is the verified one, and its hash is the manifest's.
    served = settings.cache_dir / json.loads(raw)["gguf"]
    assert hashlib.sha256(served.read_bytes()).hexdigest() == gguf.sha256
    mode = stat.S_IMODE(os.stat(settings.cache_dir / loader.ACTIVE_FILE).st_mode)
    assert mode & 0o077 == 0
    assert not [p for p in os.listdir(settings.cache_dir) if p.startswith(".active")]
    assert side.waited == [(vs, 1.0)]
    assert side.asked == [(vs, answer_gate.WARMUP_SYSTEM, answer_gate.WARMUP_QUESTION)]


def test_tampered_gguf_refused_and_active_json_not_written(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    gguf = next(f for f in pub.m.files if f.path.endswith(".gguf"))
    key = store.run_file_key(NAME, VERSION, RUN, gguf.path)
    [(vid, body)] = mem.objects[key]
    mem.objects[key] = [(vid, body[:-1] + bytes([body[-1] ^ 1]))]  # one flipped bit
    settings = _settings(tmp_path, pub)
    side = FakeSidecar(settings.cache_dir)
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mem, keys, side)
    assert e.value.role == "cca" and e.value.reason == "hash_mismatch"
    assert not (settings.cache_dir / loader.ACTIVE_FILE).exists()
    assert side.waited == [] and side.asked == []


def test_a_stale_active_json_is_replaced(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    (settings.cache_dir / loader.ACTIVE_FILE).write_text('{"gguf":"old"}')
    _load(settings, mem, keys, FakeSidecar(settings.cache_dir))
    assert json.loads((settings.cache_dir / loader.ACTIVE_FILE).read_text())["sha256"]


def test_sidecar_serving_another_alias_is_not_ready(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    side = FakeSidecar(settings.cache_dir, alias="mhai-cca-synthetic@0.9.0#ba9876543210")
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mem, keys, side)
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "start_timeout")
    assert side.asked == []


def test_no_sidecar_is_no_runtime(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mem, keys, None)
    assert e.value.reason == "no_runtime"


@pytest.mark.parametrize("answer", ["", "   "])
def test_warmup_without_an_answer_is_not_ready(tmp_path: Path, keys: Keys, answer: str) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mem, keys, FakeSidecar(settings.cache_dir, answer=answer))
    assert (e.value.reason, e.value.detail_code) == ("warmup_mismatch", "no_answer")


@pytest.mark.parametrize(
    "doc",
    [
        warmup_doc(engine_version="1.0.0+000000000000"),
        warmup_doc(engine="vllm"),
        warmup_doc(answered=False),
        warmup_doc(question_sha256="0" * 64),
        canonical.dumps({"page_sha256": "0" * 64}),
        b"not json",
    ],
)
def test_warmup_document_checked(tmp_path: Path, keys: Keys, doc: bytes) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys, warmup=doc)
    settings = _settings(tmp_path, pub)
    side = FakeSidecar(settings.cache_dir)
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mem, keys, side)
    assert e.value.reason in ("warmup_mismatch", "json_invalid", "not_canonical")
    assert side.asked == []


# ---------------------------------------------------------------- the offline mirror


def test_verifies_offline_from_the_mirror(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    root = tmp_path / "mirror"
    root.mkdir(mode=0o700)
    lk = lock.read(tmp_path / "models.lock")
    copied = mirror.fetch_pinned(mem, lk, root)
    assert copied["cca"] > 0
    pinned = _load(settings, mirror.DirReadStore(root), keys, FakeSidecar(settings.cache_dir))
    assert pinned.versions() == {"cca": verify.version_string(NAME, VERSION, pub.sha)}
    # A second fetch of the same pin changes nothing and copies nothing new.
    assert mirror.fetch_pinned(mem, lk, root)["cca"] == 0


def test_mirror_missing_the_signature_is_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    root = tmp_path / "mirror"
    root.mkdir(mode=0o700)
    mirror.fetch_pinned(mem, lock.read(tmp_path / "models.lock"), root)
    sig_dir = root / store.manifest_sig_key(NAME, VERSION).rsplit("/", 1)[0]
    for p in sig_dir.iterdir():
        if p.name.startswith("manifest.sig@"):
            p.unlink()
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mirror.DirReadStore(root), keys, FakeSidecar(settings.cache_dir))
    assert e.value.reason == "signature_invalid"


def test_mirror_with_a_changed_file_is_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish_cca(mem, tmp_path, keys)
    settings = _settings(tmp_path, pub)
    root = tmp_path / "mirror"
    root.mkdir(mode=0o700)
    mirror.fetch_pinned(mem, lock.read(tmp_path / "models.lock"), root)
    gguf = next(f for f in pub.m.files if f.path.endswith(".gguf"))
    key = store.run_file_key(NAME, VERSION, RUN, gguf.path)
    p = root / f"{key}@{gguf.store_version_id}"
    data = bytearray(p.read_bytes())
    data[-1] ^= 1
    p.chmod(0o600)
    p.write_bytes(bytes(data))
    side = FakeSidecar(settings.cache_dir)
    with pytest.raises(loader.PinRefused) as e:
        _load(settings, mirror.DirReadStore(root), keys, side)
    assert e.value.reason == "hash_mismatch"
    assert not (settings.cache_dir / loader.ACTIVE_FILE).exists()


def test_dir_read_store_refuses_links_and_bad_names(tmp_path: Path) -> None:
    root = tmp_path / "mirror"
    root.mkdir()
    d = root / "models" / NAME / VERSION
    d.mkdir(parents=True)
    (tmp_path / "outside").write_bytes(b"x")
    (d / "manifest.json@v1").symlink_to(tmp_path / "outside")
    src = mirror.DirReadStore(root)
    with pytest.raises(Refused):
        src.get(store.manifest_key(NAME, VERSION), "v1", max_bytes=10)
    with pytest.raises(Refused):
        src.get("models/../etc/passwd", "v1", max_bytes=10)
    with pytest.raises(Refused):
        src.get(store.manifest_key(NAME, VERSION), "../v1", max_bytes=10)
    assert src.versions(store.manifest_sig_key(NAME, VERSION)) == []


def test_fetch_pinned_takes_the_reader_credential_from_files_only(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    from intelligence.modelstore import cli

    path = tmp_path / "models.lock"
    lock.write(path, lock.Lock({"cca": lock.Pin(NAME, VERSION, "ab" * 32, GATE)}))
    root = tmp_path / "mirror"
    root.mkdir()
    for var in ("MODEL_STORE_READER_ACCESS_KEY_FILE", "MODEL_STORE_READER_SECRET_KEY_FILE"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("MODEL_STORE_READER_ACCESS_KEY", "SYNTHETICREADERKEY01")
    argv = ["fetch-pinned", "--lock", str(path), "--to", str(root)]
    assert cli.main(argv) == 2
    assert json.loads(capsys.readouterr().out) == {
        "refused": "not_allowed_here",
        "detail": "credential_env",
    }
    monkeypatch.delenv("MODEL_STORE_READER_ACCESS_KEY")
    assert cli.main(argv) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "reader_credentials"
    link = tmp_path / "link"
    link.symlink_to(root)
    assert cli.main(["fetch-pinned", "--lock", str(path), "--to", str(link)]) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "mirror"
