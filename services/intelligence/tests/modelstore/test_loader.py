"""The pinned models at start-up (plan Task 23): every required role verified with
:func:`~intelligence.modelstore.verify.verify_for_load`, warmed up from the warm-up
document the manifest recorded, and reported as ``<name>@<version>#<hash12>``.

Every model is the tiny synthetic extractor (``fixtures.make_run``); keys are made in
memory; the store is the in-memory fake, and MinIO for the ``store`` test.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import time
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore import (
    canonical,
    fixtures,
    loader,
    lock,
    manifest,
    promote,
    signing,
    store,
    verify,
)
from intelligence.modelstore.reasons import Refused
from tests.modelstore.conftest import MinioStore
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_verify import (
    GATE,
    RUN,
    Keys,
    MemSigner,
    Published,
    Spy,
    _manifest_bytes,
    _policy,
    _put,
    write_signoff,
)

NAME = fixtures.MODEL_NAME
ROLE = fixtures.ROLE
VERSION = "0.1.0"
PINNED = re.compile(r"[a-z0-9][a-z0-9-]{1,62}@\d+\.\d+\.\d+#[0-9a-f]{12}")


@pytest.fixture
def keys() -> Keys:
    m, s = MemSigner(), MemSigner()
    return Keys(m, s, _policy(m, s))


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)


def _warmup_doc(run_dir: Path, version: str) -> bytes:
    """``warmup.json`` exactly as promote writes it for this run."""
    chosen = promote.choose_extractor(run_dir, NAME, ROLE, version, None)
    return promote.warmup(chosen, promote.WARMUP_PAGE)


def publish(
    target: Any,
    tmp_path: Path,
    keys: Keys,
    *,
    version: str = VERSION,
    seed: int = 7,
    warmup: bytes | None = None,
    architecture: str | None = None,
    with_signoff: bool = True,
) -> Published:
    """A promoted version as promote writes it, with a real ``warmup.json``."""
    run_dir = fixtures.make_run(tmp_path / f"run-{version}-{seed}", "good", seed, name=NAME)
    warm = _warmup_doc(run_dir, version) if warmup is None else warmup
    if architecture is not None:
        cfg = json.loads((run_dir / "config.json").read_text())
        cfg["architectures"] = [architecture]
        (run_dir / "config.json").write_text(json.dumps(cfg))
    records = []
    for p in sorted(os.listdir(run_dir)):
        body = (run_dir / p).read_bytes()
        vid = _put(target, store.run_file_key(NAME, version, RUN, p), body)
        records.append(manifest.FileRecord(p, len(body), hashlib.sha256(body).hexdigest(), vid))
    docs = {}
    for d in manifest.DOCUMENTS:
        body = warm if d == "warmup.json" else f'{{"synthetic":"{d}"}}'.encode()
        vid = _put(target, store.run_doc_key(NAME, version, RUN, d), body)
        docs[d] = manifest.DocumentRecord(hashlib.sha256(body).hexdigest(), vid)
    raw = _manifest_bytes(records, docs, version=version)
    _put(target, store.manifest_sig_key(NAME, version), keys.manifest.sign("manifest", raw))
    _put(target, store.manifest_key(NAME, version), raw)
    pub = Published(raw, manifest.sha256(raw), manifest.parse(raw), run_dir)
    if with_signoff:
        write_signoff(target, pub, keys.signoff)
    return pub


def _lock_file(tmp_path: Path, pins: dict[str, Published], name: str = "models.lock") -> Path:
    path = tmp_path / name
    lock.write(
        path,
        lock.Lock({r: lock.Pin(p.m.name, p.m.version, p.sha, GATE) for r, p in pins.items()}),
    )
    return path


def _settings(tmp_path: Path, pins: dict[str, Published], roles: tuple[str, ...] = (ROLE,)) -> Any:
    cache = tmp_path / "cache"
    cache.mkdir(mode=0o700, exist_ok=True)
    return loader.LoadSettings.from_lock(roles, _lock_file(tmp_path, pins), cache)


def _refused(settings: Any, source: Any, policy: signing.TrustPolicy) -> loader.PinRefused:
    with pytest.raises(loader.PinRefused) as e:
        loader.PinnedModels.load(settings, source, policy)
    return e.value


# ---------------------------------------------------------------- versions


def test_versions_string_format(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    pinned = loader.PinnedModels.load(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    versions = pinned.versions()
    assert versions == {ROLE: f"{NAME}@{VERSION}#{pub.sha[:12]}"}
    assert PINNED.fullmatch(versions[ROLE])
    assert versions[ROLE].split("#")[1] == pub.sha[:12] and len(pub.sha[:12]) == 12
    # The verifier's own string, not a second spelling of it.
    assert versions[ROLE] == verify.version_string(NAME, VERSION, pub.sha)
    path = pinned.path(ROLE)
    assert sorted(os.listdir(path)) == [f.path for f in pub.m.files]
    with pytest.raises(KeyError):
        pinned.path("nli")


def test_versions_from_verified_not_lock(tmp_path: Path, keys: Keys) -> None:
    """The lock is read once, at start-up: rewriting it afterwards changes neither what a
    retry verifies nor what ``versions()`` reports."""
    mem = MemoryStore()
    first = publish(mem, tmp_path, keys)
    second = publish(mem, tmp_path, keys, version="0.2.0", seed=8)
    settings = _settings(tmp_path, {ROLE: first})
    pinned = loader.PinnedModels.load(settings, mem, keys.policy)
    want = {ROLE: f"{NAME}@{VERSION}#{first.sha[:12]}"}
    assert pinned.versions() == want

    _lock_file(tmp_path, {ROLE: second})  # the same path, a new pin, after start
    assert pinned.versions() == want
    again = loader.PinnedModels.load(settings, mem, keys.policy)
    assert again.versions() == want


def test_lock_without_a_required_role_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = tmp_path / "cache"
    cache.mkdir(mode=0o700)
    with pytest.raises(Refused) as e:
        loader.LoadSettings.from_lock(("extract", "nli"), _lock_file(tmp_path, {ROLE: pub}), cache)
    assert e.value.reason == "lock_invalid"
    with pytest.raises(Refused):
        loader.LoadSettings.from_lock(("Bad-Role",), _lock_file(tmp_path, {ROLE: pub}), cache)


# ---------------------------------------------------------------- refusals


def test_tampered_pin_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    spy = Spy(mem)
    spy.corrupt.add("model.safetensors")
    e = _refused(_settings(tmp_path, {ROLE: pub}), spy, keys.policy)
    assert (e.role, e.reason) == (ROLE, "hash_mismatch")


def test_no_fallback_to_other_version(tmp_path: Path, keys: Keys) -> None:
    """The store has 0.1.0 and 0.2.0; the lock pins a tampered 0.2.0 (its hash is not any
    stored manifest's). Nothing loads, and nothing of 0.1.0 is read or cached."""
    mem = MemoryStore()
    publish(mem, tmp_path, keys)
    newer = publish(mem, tmp_path, keys, version="0.2.0", seed=8)
    tampered = Published(newer.raw, "0" * 64, newer.m, newer.run_dir)
    spy = Spy(mem)
    settings = _settings(tmp_path, {ROLE: tampered})
    for _ in range(3):  # every retry: the same pin, never another version
        e = _refused(settings, spy, keys.policy)
        assert (e.role, e.reason) == (ROLE, "lock_mismatch")
    assert not [r for r in spy.reads if f"/{VERSION}/" in r[1]]
    assert not (tmp_path / "cache" / NAME).exists() or not [
        d for d in os.listdir(tmp_path / "cache" / NAME) if d.startswith(VERSION)
    ]


def test_no_signoff_refused_in_production(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys, with_signoff=False)
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert (e.role, e.reason) == (ROLE, "not_approved")
    write_signoff(mem, pub, keys.signoff)
    pinned = loader.PinnedModels.load(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert pinned.versions()[ROLE].endswith(pub.sha[:12])


def test_warmup_mismatch_not_ready(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    run_dir = fixtures.make_run(tmp_path / "probe", "good", 7, name=NAME)
    good = json.loads(_warmup_doc(run_dir, VERSION))
    bad = canonical.dumps({**good, "output_sha256": "ab" * 32})
    pub = publish(mem, tmp_path, keys, warmup=bad)
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert (e.role, e.reason, e.detail_code) == (ROLE, "warmup_mismatch", "output")


def test_warmup_page_mismatch_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    run_dir = fixtures.make_run(tmp_path / "probe", "good", 7, name=NAME)
    good = json.loads(_warmup_doc(run_dir, VERSION))
    pub = publish(mem, tmp_path, keys, warmup=canonical.dumps({**good, "page_sha256": "cd" * 32}))
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert (e.reason, e.detail_code) == ("warmup_mismatch", "page")


@pytest.mark.parametrize(
    "body",
    [
        b'{"engine":"numpy"}',
        b'{"engine": "numpy", "engine_version": null, "output_sha256": "x", "page_sha256": "y"}',
        b"not json",
        b'{"engine":"numpy","engine_version":null,"extra":1,"output_sha256":"'
        + b"a" * 64
        + b'","page_sha256":"'
        + b"b" * 64
        + b'"}',
    ],
)
def test_warmup_document_parsed_strictly(tmp_path: Path, keys: Keys, body: bytes) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys, warmup=body)
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert e.reason in ("warmup_mismatch", "not_canonical", "json_invalid")


def test_warmup_read_by_recorded_version(tmp_path: Path, keys: Keys) -> None:
    """A newer, wrong ``warmup.json`` written over the key is ignored: the document is
    read by the version id the verified manifest recorded, never latest."""
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    key = store.run_doc_key(NAME, VERSION, RUN, "warmup.json")
    mem.shadow(key, b'{"engine":"numpy","output_sha256":"' + b"0" * 64 + b'"}')
    spy = Spy(mem)
    loader.PinnedModels.load(_settings(tmp_path, {ROLE: pub}), spy, keys.policy)
    reads = [r for r in spy.reads if r[1] == key]
    assert reads == [("get", key, pub.m.documents["warmup.json"].store_version_id)]


def test_vlm_role_has_no_runtime_in_phase_0(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys, architecture=sorted(promote.KNOWN_VLM_ARCHITECTURES)[0])
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert (e.role, e.reason) == (ROLE, "no_runtime")


def test_refusal_logs_ids_only(
    tmp_path: Path, keys: Keys, caplog: pytest.LogCaptureFixture
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    caplog.set_level(logging.DEBUG)
    pinned = loader.PinnedModels.load(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    text = caplog.text
    assert "model.warmed_up" in text and pub.sha in text
    assert str(tmp_path) not in text and str(pinned.path(ROLE)) not in text


# ---------------------------------------------------------------- MinIO


@pytest.mark.store
def test_pinned_models_load_from_minio(tmp_path: Path, keys: Keys, minio_store: MinioStore) -> None:
    root = minio_store.store()
    version = f"0.{int(time.time()) % 100000}.{os.getpid() % 1000}"
    pub = publish(root, tmp_path, keys, version=version)
    reader = minio_store.store("reader")
    pinned = loader.PinnedModels.load(_settings(tmp_path, {ROLE: pub}), reader, keys.policy)
    assert pinned.versions() == {ROLE: f"{NAME}@{version}#{pub.sha[:12]}"}


def test_warmup_engine_version_compared(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    run_dir = fixtures.make_run(tmp_path / "probe", "good", 7, name=NAME)
    good = json.loads(_warmup_doc(run_dir, VERSION))
    assert good["engine_version"]  # promote records numpy's version
    pub = publish(mem, tmp_path, keys, warmup=canonical.dumps({**good, "engine_version": "0.0.1"}))
    e = _refused(_settings(tmp_path, {ROLE: pub}), mem, keys.policy)
    assert (e.reason, e.detail_code) == ("warmup_mismatch", "engine_version")


def test_missing_warmup_page_has_its_own_code(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = tmp_path / "cache"
    cache.mkdir(mode=0o700)
    settings = loader.LoadSettings.from_lock(
        (ROLE,), _lock_file(tmp_path, {ROLE: pub}), cache, warmup_page=tmp_path / "none.png"
    )
    e = _refused(settings, mem, keys.policy)
    assert e.reason == "warmup_page_missing"
