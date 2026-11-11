"""Load-time verification (plan Task 22): lock hash, identity, signature, every file,
the exact set, the production rules at load, and a cache that is never trusted. There is
no sign-off record: a pin reaches the lock only through a reviewed PR.

Every model here is a tiny synthetic safetensors run (``fixtures.make_run``); keys are made
in memory; the store is the in-memory fake, and MinIO for the ``store`` test.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import stat
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, BinaryIO

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519

from intelligence.modelstore import (
    canonical,
    fixtures,
    manifest,
    signing,
    store,
    training,
    verify,
)
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo
from tests.modelstore.conftest import MinioStore
from tests.modelstore.fakes.memory_store import MemoryStore

NAME = fixtures.MODEL_NAME
ROLE = fixtures.ROLE
VERSION = "0.1.0"
RUN = "run-verify-1"
GATE = "phase1"  # a production gate: production loads refuse the rehearsal gate (P14)
ACTOR = "promoter-bot"
TRAINER = fixtures.TRAINED_BY
MARKER = "Zqxverifymarkerwv"


# ---------------------------------------------------------------- keys and policies


class MemSigner:
    """An Ed25519 key in memory."""

    def __init__(self) -> None:
        self._key = ed25519.Ed25519PrivateKey.generate()
        self.key_id = signing.key_id(self._key.public_key())
        self.alg = "ed25519"

    def pem(self) -> str:
        return (
            self._key.public_key()
            .public_bytes(
                serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
            )
            .decode("ascii")
        )

    def trusted(self, development: bool = False) -> signing.TrustedKey:
        return signing.TrustedKey("ed25519", self.key_id, self.pem(), development)

    def sign(self, purpose: str, payload: bytes) -> bytes:
        sig = self._key.sign(signing.message(purpose, payload))
        return signing.SignatureDoc(purpose, self.alg, self.key_id, sig).to_bytes()


@dataclass(frozen=True)
class Keys:
    manifest: MemSigner
    policy: signing.TrustPolicy


def _policy(m: MemSigner, *, development: bool = False) -> signing.TrustPolicy:
    """The trust policy: one signer, for every purpose (the purpose is in the message)."""
    return signing.TrustPolicy((m.trusted(development),))


@pytest.fixture
def keys() -> Keys:
    m = MemSigner()
    return Keys(m, _policy(m))


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)


# ---------------------------------------------------------------- publishing


@dataclass(frozen=True)
class Pin:
    name: str
    version: str
    manifest_sha256: str
    gate: str = GATE


@dataclass(frozen=True)
class Published:
    raw: bytes
    sha: str
    m: manifest.Manifest
    run_dir: Path

    @property
    def pin(self) -> Pin:
        return Pin(self.m.name, self.m.version, self.sha)


def _hex(seed: str) -> str:
    return hashlib.sha256(seed.encode()).hexdigest()


def _docs() -> dict[str, bytes]:
    return {d: f'{{"synthetic":"{d}"}}'.encode() for d in manifest.DOCUMENTS}


def _put(target: Any, key: str, body: bytes | Path) -> str:
    return str(target.put_new(key, body))


def _upload(
    target: Any,
    tmp_path: Path,
    *,
    name: str = NAME,
    version: str = VERSION,
    role: str = ROLE,
    seed: int = 7,
) -> tuple[list[manifest.FileRecord], dict[str, manifest.DocumentRecord], Path]:
    run_dir = fixtures.make_run(tmp_path / f"run-{name}-{version}-{seed}", "good", seed, name=name)
    records = []
    for p in sorted(os.listdir(run_dir)):
        body = (run_dir / p).read_bytes()
        vid = _put(target, store.run_file_key(name, version, RUN, p), body)
        records.append(manifest.FileRecord(p, len(body), hashlib.sha256(body).hexdigest(), vid))
    docs = {}
    for d, body in _docs().items():
        vid = _put(target, store.run_doc_key(name, version, RUN, d), body)
        docs[d] = manifest.DocumentRecord(hashlib.sha256(body).hexdigest(), vid)
    return records, docs, run_dir


def _manifest_bytes(
    records: list[manifest.FileRecord],
    docs: dict[str, manifest.DocumentRecord],
    *,
    name: str = NAME,
    version: str = VERSION,
    role: str = ROLE,
    gates: dict[str, str] | None = None,
) -> bytes:
    return manifest.build(
        name=name,
        version=version,
        role=role,
        run=manifest.Run(RUN, "local", manifest.LOCAL_SOURCE_REF),
        files=records,
        documents=docs,
        base_model=training.UpstreamBase(fixtures.BASE_REPO, "ab" * 20, fixtures.LICENCE),
        dependencies=[],
        evaluation=manifest.Evaluation(
            docs["evaluation.json"].sha256,
            f"clinical_document@1.0.0#{_hex('set')}",
            gates or {GATE: "pass"},
            "dev",
        ),
        runtime=manifest.Runtime("numpy", "2.1", None),
        promotion=manifest.Promotion(
            "4242",
            "Measured-Health/mhai-multiagent/.github/workflows/model-promote.yml@refs/heads/main",
            "cd" * 20,
            ACTOR,
            "2026-11-06T00:00:00Z",
        ),
        trained_by=TRAINER,
    )


def publish(
    target: Any,
    tmp_path: Path,
    keys: Keys,
    *,
    name: str = NAME,
    version: str = VERSION,
    role: str = ROLE,
    seed: int = 7,
    sign: bool = True,
) -> Published:
    """A promoted version as promote writes it: files, documents, manifest.sig, then
    manifest.json."""
    records, docs, run_dir = _upload(
        target, tmp_path, name=name, version=version, role=role, seed=seed
    )
    raw = _manifest_bytes(records, docs, name=name, version=version, role=role)
    if sign:
        _put(target, store.manifest_sig_key(name, version), keys.manifest.sign("manifest", raw))
    _put(target, store.manifest_key(name, version), raw)
    m = manifest.parse(raw)
    return Published(raw, manifest.sha256(raw), m, run_dir)


def _cache(tmp_path: Path) -> Path:
    c = tmp_path / "cache"
    c.mkdir(mode=0o700, parents=True, exist_ok=True)
    return c


def _verify(src: Any, pub: Published, keys: Keys, cache: Path, **kw: Any) -> verify.VerifiedModel:
    return verify.verify_pin(
        src, kw.pop("role", ROLE), kw.pop("entry", pub.pin), keys.policy, cache, **kw
    )


def _refused(fn: Callable[..., Any], *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _pin_dirs(cache: Path, pub: Published) -> list[str]:
    d = cache / pub.m.name
    if not d.exists():
        return []
    return sorted(p for p in os.listdir(d) if not p.endswith(".lock"))


def _final(cache: Path, pub: Published) -> Path:
    return cache / pub.m.name / f"{pub.m.version}-{pub.sha[:12]}"


class Spy:
    """Wraps a store: records every read with its version id, and can corrupt a
    download, refuse versions the store cannot vouch for, or slow downloads down."""

    def __init__(self, inner: Any) -> None:
        self.inner = inner
        self.reads: list[tuple[str, str, str | None]] = []
        self.corrupt: set[str] = set()
        self.no_checksum: set[str] = set()
        self.delay = 0.0
        self.downloads = 0
        self.max_bytes: dict[str, int] = {}

    def _vouch(self, vid: str | None) -> None:
        if vid in self.no_checksum:
            raise Refused("store_unavailable", detail_code="checksum")

    def versions(self, key: str) -> list[str]:
        self.reads.append(("versions", key, None))
        return list(self.inner.versions(key))

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo:
        self.reads.append(("head", key, version_id))
        self._vouch(version_id)
        info: ObjectInfo = self.inner.head(key, version_id)
        return info

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        self.reads.append(("get", key, version_id))
        self._vouch(version_id)
        return bytes(self.inner.get(key, version_id, max_bytes=max_bytes))

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]:
        self.reads.append(("download", key, version_id))
        self.max_bytes[key] = max_bytes
        self.downloads += 1
        if self.delay:
            time.sleep(self.delay)
        if key.rsplit("/", 1)[-1] in self.corrupt:
            body = bytearray(self.inner.get(key, version_id, max_bytes=max_bytes))
            body[len(body) // 2] ^= 0x01
            out.write(bytes(body))
            return len(body), hashlib.sha256(bytes(body)).hexdigest()
        n, sha = self.inner.download(key, version_id=version_id, out=out, max_bytes=max_bytes)
        return int(n), str(sha)


def _tamper(path: Path) -> None:
    os.chmod(path.parent, 0o700)
    os.chmod(path, 0o600)
    body = bytearray(path.read_bytes())
    body[len(body) // 2] ^= 0x01
    path.write_bytes(bytes(body))
    os.chmod(path, 0o400)
    os.chmod(path.parent, 0o500)


def _weights(pub: Published) -> str:
    return next(f.path for f in pub.m.files if f.path.endswith(".safetensors"))


# ---------------------------------------------------------------- the good path


def test_good_pin_verifies(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    v = _verify(mem, pub, keys, cache)
    assert (v.role, v.name, v.version, v.manifest_sha256) == (ROLE, NAME, VERSION, pub.sha)
    assert v.version_string == f"{NAME}@{VERSION}#{pub.sha[:12]}"
    assert v.path == _final(cache, pub)
    assert v.manifest == pub.m
    # Exactly the manifest's files, byte for byte; no manifest or documents in it.
    assert sorted(os.listdir(v.path)) == [f.path for f in pub.m.files]
    for f in pub.m.files:
        assert (v.path / f.path).read_bytes() == (pub.run_dir / f.path).read_bytes()
        assert stat.S_IMODE(os.lstat(v.path / f.path).st_mode) == 0o400
    assert stat.S_IMODE(os.lstat(v.path).st_mode) == 0o500
    assert _pin_dirs(cache, pub) == [v.path.name]  # no .partial left
    # Again: the same pin verifies from the cache, which is re-checked.
    again = _verify(mem, pub, keys, cache)
    assert again.path == v.path


def test_version_string_format() -> None:
    sha = "3f9a" + "0" * 60
    assert (
        verify.version_string("doc-extract-tiny", "0.1.0", sha)
        == "doc-extract-tiny@0.1.0#3f9a00000000"
    )
    assert (
        _refused(verify.version_string, "doc-extract-tiny", "0.1.0", "XYZ").reason == "lock_invalid"
    )


# ---------------------------------------------------------------- B18: every byte


def test_one_byte_changed_refused(
    tmp_path: Path, keys: Keys, caplog: pytest.LogCaptureFixture
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    weights = _weights(pub)
    original = (pub.run_dir / weights).read_bytes()

    # A newer store version under a file key (a shadow PUT) is never read: the manifest's
    # recorded version id is.
    key = store.run_file_key(NAME, VERSION, RUN, weights)
    mem.shadow(key, original[:-1] + bytes([original[-1] ^ 1]))
    v = _verify(mem, pub, keys, cache)
    assert (v.path / weights).read_bytes() == original

    # The cached file edited after the first verify: found on the next start, deleted,
    # and downloaded again (the edited bytes are never handed out).
    _tamper(v.path / weights)
    with caplog.at_level(logging.WARNING, logger=verify.__name__):
        again = _verify(mem, pub, keys, cache)
    assert (again.path / weights).read_bytes() == original
    assert any("model.cache_mismatch" in r.getMessage() for r in caplog.records)

    # A store that hands back a changed byte: refused, and nothing is left behind.
    spy = Spy(mem)
    spy.corrupt.add(weights)
    fresh_cache = _cache(tmp_path / "fresh")
    assert _refused(_verify, spy, pub, keys, fresh_cache).reason == "hash_mismatch"
    assert _pin_dirs(fresh_cache, pub) == []

    # The cache changed and the store's copy changed too: a second mismatch is refused,
    # and the pin's directory is gone.
    _tamper(again.path / weights)
    assert _refused(_verify, spy, pub, keys, cache).reason == "hash_mismatch"
    assert _pin_dirs(cache, pub) == []


def test_download_stops_at_recorded_size(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    weights = _weights(pub)
    # A manifest that records one byte fewer than the stored object (signed: the
    # promoter's own mistake); the download stops at the recorded size.
    files = [replace(f, size=f.size - 1) if f.path == weights else f for f in pub.m.files]
    for f in pub.m.files:
        body = (pub.run_dir / f.path).read_bytes()
        vid = _put(mem, store.run_file_key(NAME, "0.1.1", RUN, f.path), body)
        files = [replace(x, store_version_id=vid) if x.path == f.path else x for x in files]
    raw = _manifest_bytes(files, dict(pub.m.documents), version="0.1.1")
    _put(mem, store.manifest_sig_key(NAME, "0.1.1"), keys.manifest.sign("manifest", raw))
    _put(mem, store.manifest_key(NAME, "0.1.1"), raw)
    pin = Pin(NAME, "0.1.1", manifest.sha256(raw))
    cache = _cache(tmp_path)
    err = _refused(verify.verify_pin, mem, ROLE, pin, keys.policy, cache)
    assert err.reason == "size_mismatch"
    assert os.listdir(cache / NAME) == [f".0.1.1-{pin.manifest_sha256[:12]}.lock"]


# ---------------------------------------------------------------- B19: signature and lock


def test_unsigned_or_wrong_signer_refused(tmp_path: Path, keys: Keys) -> None:
    cache = _cache(tmp_path)
    mem = MemoryStore()
    unsigned = publish(mem, tmp_path, keys, sign=False)
    assert _refused(_verify, mem, unsigned, keys, cache).reason == "signature_invalid"

    mem = MemoryStore()
    stranger = MemSigner()
    records, docs, _ = _upload(mem, tmp_path / "s")
    raw = _manifest_bytes(records, docs)
    _put(mem, store.manifest_sig_key(NAME, VERSION), stranger.sign("manifest", raw))
    _put(mem, store.manifest_key(NAME, VERSION), raw)
    pin = Pin(NAME, VERSION, manifest.sha256(raw))
    err = _refused(verify.verify_pin, mem, ROLE, pin, keys.policy, cache)
    assert err.reason == "signer_untrusted"

    # A signature for another purpose over the same bytes, by the trusted key, is not a
    # manifest signature (the purpose is bound in the signed message).
    for purpose, signer in (("testset", keys.manifest),):
        mem = MemoryStore()
        records, docs, _ = _upload(mem, tmp_path / purpose)
        raw = _manifest_bytes(records, docs)
        _put(mem, store.manifest_sig_key(NAME, VERSION), signer.sign(purpose, raw))
        _put(mem, store.manifest_key(NAME, VERSION), raw)
        pin = Pin(NAME, VERSION, manifest.sha256(raw))
        err = _refused(verify.verify_pin, mem, ROLE, pin, keys.policy, cache)
        assert err.reason == "signature_invalid"
    assert _pin_dirs(cache, unsigned) == []


def test_good_signature_found_among_shadow_signatures(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    key = store.manifest_sig_key(NAME, VERSION)
    mem.shadow(key, b"junk")
    mem.shadow(key, MemSigner().sign("manifest", pub.raw))
    assert _verify(mem, pub, keys, _cache(tmp_path)).manifest_sha256 == pub.sha


def test_lock_hash_differs_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    other = Pin(NAME, VERSION, _hex("another manifest"))
    err = _refused(verify.verify_pin, mem, ROLE, other, keys.policy, cache)
    assert err.reason == "lock_mismatch"
    bad = Pin(NAME, VERSION, "not-a-hash")
    assert _refused(_verify, mem, pub, keys, cache, entry=bad).reason == "lock_invalid"


def test_shadowing_manifest_version_ignored(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    key = store.manifest_key(NAME, VERSION)
    # A newer, validly signed manifest with other files (a holder of write credentials
    # outside the workflow): not the lock's hash, so never read.
    records, docs, _ = _upload(mem, tmp_path / "other", version="9.9.9", seed=8)
    other = _manifest_bytes(records, docs)
    mem.shadow(key, other)
    mem.shadow(store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", other))
    mem.shadow(key, pub.raw)  # a copy with the same bytes: one candidate
    spy = Spy(mem)
    vids = mem.versions(key)  # newest first
    spy.no_checksum.add(vids[1])  # the shadow can't even be vouched for: skipped
    v = _verify(spy, pub, keys, _cache(tmp_path))
    assert v.manifest_sha256 == pub.sha
    # Only the oldest (the promoted) version of the manifest was read.
    got = [vid for op, k, vid in spy.reads if op == "get" and k == key]
    assert got == [vids[-1]]
    shadow_vid = mem.versions(key)[1]
    assert ("get", key, shadow_vid) not in spy.reads


def test_checksumless_versions_skipped_not_fatal(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    key = store.manifest_key(NAME, VERSION)
    junk = mem.shadow(key, b"shadow without a checksum")  # older than the real one
    records, docs, run_dir = _upload(mem, tmp_path)
    raw = _manifest_bytes(records, docs)
    _put(mem, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    mem.objects[key].append(("real-" + junk, raw))
    pin = Pin(NAME, VERSION, manifest.sha256(raw))
    spy = Spy(mem)
    spy.no_checksum.add(junk)
    v = verify.verify_pin(spy, ROLE, pin, keys.policy, _cache(tmp_path))
    assert v.manifest_sha256 == pin.manifest_sha256

    # A store failure that is not about one version still stops verification.
    class Down(Spy):
        def versions(self, key: str) -> list[str]:
            raise Refused("store_unavailable", detail_code="denied")

    err = _refused(
        verify.verify_pin,
        Down(mem),
        ROLE,
        pin,
        keys.policy,
        _cache(tmp_path),
    )
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")


def test_store_reads_always_name_a_version(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    spy = Spy(mem)
    cache = _cache(tmp_path)
    _verify(spy, pub, keys, cache)
    _verify(spy, pub, keys, cache)
    reads = [r for r in spy.reads if r[0] != "versions"]
    assert reads and all(vid is not None for _, _, vid in reads)
    # File downloads use exactly the recorded ids.
    recorded = {
        store.run_file_key(NAME, VERSION, RUN, f.path): f.store_version_id for f in pub.m.files
    }
    assert {(k, vid) for op, k, vid in spy.reads if op == "download"} == set(recorded.items())
    # ... and stop at each file's recorded size.
    sizes = {store.run_file_key(NAME, VERSION, RUN, f.path): f.size for f in pub.m.files}
    assert spy.max_bytes == sizes
    # The store prefix is never listed.
    assert not [c for c in mem.calls if c[0] == "list"]


# ---------------------------------------------------------------- P9: identity


def test_manifest_for_other_name_version_or_role_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    assert _refused(_verify, mem, pub, keys, cache, role="ocr").reason == "identity_mismatch"

    # A genuine, signed manifest for 0.1.0 copied under 0.2.0's key.
    _put(mem, store.manifest_key(NAME, "0.2.0"), pub.raw)
    _put(mem, store.manifest_sig_key(NAME, "0.2.0"), keys.manifest.sign("manifest", pub.raw))
    moved = Pin(NAME, "0.2.0", pub.sha)
    err = _refused(verify.verify_pin, mem, ROLE, moved, keys.policy, cache)
    assert err.reason == "identity_mismatch"

    # And under another name's key.
    _put(mem, store.manifest_key("other-model", VERSION), pub.raw)
    renamed = Pin("other-model", VERSION, pub.sha)
    err = _refused(verify.verify_pin, mem, ROLE, renamed, keys.policy, cache)
    assert err.reason == "identity_mismatch"
    assert _pin_dirs(cache, pub) == []


# ---------------------------------------------------------------- P10: the exact set


def test_extra_or_missing_file_refused(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Missing: the manifest records a version id the store doesn't have.
    mem = MemoryStore()
    records, docs, _ = _upload(mem, tmp_path / "m")
    records = [
        replace(r, store_version_id="no-such-version") if r.path == "config.json" else r
        for r in records
    ]
    raw = _manifest_bytes(records, docs)
    _put(mem, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    _put(mem, store.manifest_key(NAME, VERSION), raw)
    pin = Pin(NAME, VERSION, manifest.sha256(raw))
    cache = _cache(tmp_path)
    err = _refused(verify.verify_pin, mem, ROLE, pin, keys.policy, cache)
    assert err.reason == "missing_file"
    assert os.listdir(cache / NAME) == [f".{VERSION}-{pin.manifest_sha256[:12]}.lock"]

    # Extra: a file that appears beside the downloads (an allow-listed name, so only the
    # exact-set check can catch it).
    mem = MemoryStore()
    pub = publish(mem, tmp_path / "x", keys)
    real = verify._download

    def with_extra(source: Any, m: Any, name_fd: int, partial: str) -> None:
        real(source, m, name_fd, partial)
        fd = os.open(
            f"{partial}/chat_template.json", os.O_WRONLY | os.O_CREAT, 0o400, dir_fd=name_fd
        )
        os.write(fd, b"{}")
        os.close(fd)

    monkeypatch.setattr(verify, "_download", with_extra)
    cache = _cache(tmp_path / "x")
    assert _refused(_verify, mem, pub, keys, cache).reason == "extra_file"
    assert _pin_dirs(cache, pub) == []
    monkeypatch.setattr(verify, "_download", real)

    # In the cache, an extra file or a link is a mismatch: the directory is rebuilt.
    v = _verify(mem, pub, keys, cache)
    os.chmod(v.path, 0o700)
    (v.path / "chat_template.json").write_bytes(b"{}")
    os.symlink("config.json", v.path / "special_tokens_map.json")
    os.chmod(v.path, 0o500)
    again = _verify(mem, pub, keys, cache)
    assert sorted(os.listdir(again.path)) == [f.path for f in pub.m.files]


def test_partial_upload_not_found(tmp_path: Path, keys: Keys) -> None:
    # A promotion that stopped before its manifest: claim and files, no version.
    mem = MemoryStore()
    _put(mem, store.claim_key(NAME, VERSION), b'{"claim":"synthetic"}')
    records, docs, _ = _upload(mem, tmp_path)
    raw = _manifest_bytes(records, docs)
    _put(mem, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    pin = Pin(NAME, VERSION, manifest.sha256(raw))
    cache = _cache(tmp_path)
    err = _refused(verify.verify_pin, mem, ROLE, pin, keys.policy, cache)
    assert err.reason == "not_found"
    assert not (cache / NAME).exists()


# ---------------------------------------------------------------- P12: environment


def test_dev_policy_refused_in_production(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    dev_keys = Keys(keys.manifest, _policy(keys.manifest, development=True))
    cache = _cache(tmp_path)
    err = _refused(_verify, mem, pub, dev_keys, cache)
    assert (err.reason, err.detail_code) == ("policy_invalid", "development_signer_in_production")
    for value in ("production", "prod", "staging", ""):
        monkeypatch.setenv(signing.ENV_VAR, "development")
        monkeypatch.setenv(verify.APP_ENV, value)
        err = _refused(_verify, mem, pub, dev_keys, cache)
        assert (err.reason, err.detail_code) == ("not_allowed_here", "development_in_production")
    monkeypatch.setenv(verify.APP_ENV, "development")
    assert _verify(mem, pub, dev_keys, cache).path.is_dir()
    # MHAI_MODELS_ENV=development with no APP_ENV: production for the service, so for the
    # verifier too.
    monkeypatch.delenv(verify.APP_ENV)
    assert verify.is_development() is False
    err = _refused(_verify, mem, pub, dev_keys, cache)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "development_in_production")
    err = _refused(verify.verify_for_load, mem, ROLE, pub.pin, dev_keys.policy, cache)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "development_in_production")
    # And with MHAI_MODELS_ENV unset too: a development signer is refused, and the cache
    # verified under development is removed (definitive).
    monkeypatch.delenv(signing.ENV_VAR)
    err = _refused(verify.verify_for_load, mem, ROLE, pub.pin, dev_keys.policy, cache)
    assert (err.reason, err.detail_code) == ("policy_invalid", "development_signer_in_production")
    assert _pin_dirs(cache, pub) == []


# ---------------------------------------------------------------- P11: the cache


def test_cache_rehashed_every_start(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    spy = Spy(mem)
    checked: list[str] = []
    real = verify.files.check_run

    def counting(path: Any, *a: Any, **kw: Any) -> Any:
        checked.append(Path(path).name)
        return real(path, *a, **kw)

    monkeypatch.setattr(verify.files, "check_run", counting)
    v = _verify(spy, pub, keys, cache)
    first = spy.downloads
    assert first == len(pub.m.files)
    for _ in range(2):
        _verify(spy, pub, keys, cache)
    assert spy.downloads == first  # served from the cache ...
    assert checked.count(v.path.name) == 1 + 2  # ... re-hashed in full each time
    # A mode that drifted is a mismatch too: rebuilt.
    os.chmod(v.path / "config.json", 0o600)
    _verify(spy, pub, keys, cache)
    assert spy.downloads == 2 * first
    assert stat.S_IMODE(os.lstat(v.path / "config.json").st_mode) == 0o400


def test_cache_dir_must_be_private(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    real = _cache(tmp_path)
    link = tmp_path / "cache-link"
    os.symlink(real, link)
    err = _refused(_verify, mem, pub, keys, link)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_dir")
    open_dir = tmp_path / "cache-open"
    open_dir.mkdir()
    os.chmod(open_dir, 0o777)
    err = _refused(_verify, mem, pub, keys, open_dir)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_dir_unsafe")
    os.chmod(open_dir, 0o700)
    (open_dir / NAME).mkdir(mode=0o700)
    os.chmod(open_dir / NAME, 0o770)
    err = _refused(_verify, mem, pub, keys, open_dir)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_dir_unsafe")
    # Missing: made 0700.
    made = tmp_path / "cache-new"
    assert _verify(mem, pub, keys, made).path.is_dir()
    assert stat.S_IMODE(os.lstat(made).st_mode) == 0o700


def test_removal_stays_inside_the_cache(tmp_path: Path, keys: Keys) -> None:
    # A link planted where the .partial directory goes is removed as a link; what it
    # points at, outside the cache, is untouched.
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "keep.txt").write_bytes(b"keep")
    (cache / NAME).mkdir(mode=0o700)
    final = _final(cache, pub)
    os.symlink(outside, final.parent / (final.name + ".partial"))
    os.symlink(outside, final)
    v = _verify(mem, pub, keys, cache)
    assert not v.path.is_symlink() and v.path.is_dir()
    assert (outside / "keep.txt").read_bytes() == b"keep"


def test_concurrent_verifies_of_one_pin(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    spy = Spy(mem)
    spy.delay = 0.02
    results: list[verify.VerifiedModel] = []
    errors: list[BaseException] = []

    def run() -> None:
        try:
            results.append(_verify(spy, pub, keys, cache))
        except BaseException as e:  # noqa: BLE001 - reported below
            errors.append(e)

    threads = [threading.Thread(target=run) for _ in range(3)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)
    assert not errors and len(results) == 3
    assert {r.path for r in results} == {_final(cache, pub)}
    assert sorted(os.listdir(_final(cache, pub))) == [f.path for f in pub.m.files]
    assert _pin_dirs(cache, pub) == [_final(cache, pub).name]
    assert spy.downloads == len(pub.m.files)  # one built it; the others re-checked it


# ---------------------------------------------------------------- logs


def test_refusal_logs_ids_only(
    tmp_path: Path, keys: Keys, caplog: pytest.LogCaptureFixture
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path / MARKER)
    spy = Spy(mem)
    spy.corrupt.add(_weights(pub))
    link = tmp_path / "cache-link"
    os.symlink(cache, link)

    class Down(Spy):
        def versions(self, key: str) -> list[str]:
            raise Refused("store_unavailable", detail_code="denied")

    with caplog.at_level(logging.DEBUG):
        errors = [
            _refused(_verify, mem, pub, Keys(MemSigner(), _policy(MemSigner())), cache),
            _refused(_verify, spy, pub, keys, _cache(tmp_path / "b")),
            _refused(_verify, mem, pub, keys, cache, entry=Pin(NAME, VERSION, _hex("x"))),
            _refused(_verify, mem, pub, keys, link),  # cache_dir
            _refused(_verify, Down(mem), pub, keys, cache),  # store_unavailable
            _refused(_verify, mem, pub, keys, cache, role="ocr"),  # identity
        ]
    # Every refusal is a fresh, fixed code: no exception chain (an OSError or SDK error
    # in __context__ would carry a path or a URL).
    for err in errors:
        assert err.__cause__ is None and err.__context__ is None, err.reason
    text = "\n".join(r.getMessage() for r in caplog.records)
    assert "model.verify_refused" in text
    assert "reason=signer_untrusted" in text and "reason=hash_mismatch" in text
    assert "reason=lock_mismatch" in text
    assert MARKER not in text and str(tmp_path) not in text
    assert "models/" not in text and "http" not in text
    for f in pub.m.files:
        assert f.store_version_id not in text


# ---------------------------------------------------------------- MinIO


@pytest.mark.store
def test_good_pin_verifies_on_minio(tmp_path: Path, keys: Keys, minio_store: MinioStore) -> None:
    root = minio_store.store()
    version = f"0.{int(time.time()) % 100000}.{os.getpid() % 1000}"
    pub = publish(root, tmp_path, keys, version=version)
    reader = minio_store.store("reader")
    cache = _cache(tmp_path)
    v = verify.verify_pin(reader, ROLE, pub.pin, keys.policy, cache)
    assert sorted(os.listdir(v.path)) == [f.path for f in pub.m.files]
    for f in pub.m.files:
        assert (v.path / f.path).read_bytes() == (pub.run_dir / f.path).read_bytes()

    # Shadows written with raw PUTs (no conditional create, no checksum) under the
    # manifest and a file key: ignored, and the store's prefix is never listed.
    raw = minio_store.client()
    weights = _weights(pub)
    raw.put_object(
        Bucket=minio_store.bucket, Key=store.manifest_key(NAME, version), Body=b'{"x":1}'
    )
    raw.put_object(
        Bucket=minio_store.bucket, Key=store.run_file_key(NAME, version, RUN, weights), Body=b"x"
    )
    raw.put_object(
        Bucket=minio_store.bucket,
        Key=f"models/{NAME}/{version}/runs/{RUN}/files/stray.json",
        Body=b"{}",
    )
    fresh = _cache(tmp_path / "fresh")
    v2 = verify.verify_pin(reader, ROLE, pub.pin, keys.policy, fresh)
    assert (v2.path / weights).read_bytes() == (pub.run_dir / weights).read_bytes()
    assert sorted(os.listdir(v2.path)) == [f.path for f in pub.m.files]


# ---------------------------------------------------------------- review fixes


def test_gate_must_pass_in_the_manifest(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    mem = MemoryStore()
    records, docs, _ = _upload(mem, tmp_path)
    raw = _manifest_bytes(records, docs, gates={"rehearsal": "fail", "phase1": "pass"})
    _put(mem, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    _put(mem, store.manifest_key(NAME, VERSION), raw)
    sha = manifest.sha256(raw)
    cache = _cache(tmp_path)
    # Development (no production rules) still refuses a pin whose own gate result is not
    # pass.
    monkeypatch.setenv(signing.ENV_VAR, "development")
    monkeypatch.setenv(verify.APP_ENV, "development")
    for gate in ("rehearsal", "phase3"):
        err = _refused(
            verify.verify_for_load, mem, ROLE, Pin(NAME, VERSION, sha, gate), keys.policy, cache
        )
        assert err.reason == "gate_failed"
    err = _refused(
        verify.verify_for_load, mem, ROLE, Pin(NAME, VERSION, sha, "nope"), keys.policy, cache
    )
    assert (err.reason, err.detail_code) == ("lock_invalid", "gate")
    assert verify.verify_for_load(mem, ROLE, Pin(NAME, VERSION, sha, "phase1"), keys.policy, cache)


def _junk_versions(mem: MemoryStore, key: str, count: int) -> list[str]:
    """``count`` versions placed older than everything stored under ``key``."""
    junk = [(f"junk-{i:03d}", f'{{"junk":{i}}}'.encode()) for i in range(count)]
    mem.objects[key] = junk + mem.objects.get(key, [])
    return [vid for vid, _ in junk]


def test_version_flood_is_told_apart(tmp_path: Path, keys: Keys) -> None:
    # 40 readable junk versions older than the real manifest: past the cap, refused with
    # a code an operator can tell from a wrong lock.
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    _junk_versions(mem, store.manifest_key(NAME, VERSION), 40)
    err = _refused(_verify, mem, pub, keys, _cache(tmp_path))
    assert (err.reason, err.detail_code) == ("lock_mismatch", "too_many_versions")
    # The same flood under manifest.sig.
    for key, reason in ((store.manifest_sig_key(NAME, VERSION), "signature_invalid"),):
        mem = MemoryStore()
        pub = publish(mem, tmp_path / reason, keys)
        _junk_versions(mem, key, 40)
        err = _refused(_verify, mem, pub, keys, _cache(tmp_path / reason))
        assert (err.reason, err.detail_code) == (reason, "too_many_versions")
    # Versions the store can't vouch for (no checksum) are skipped before the cap.
    mem = MemoryStore()
    pub = publish(mem, tmp_path / "unvouched", keys)
    spy = Spy(mem)
    spy.no_checksum.update(_junk_versions(mem, store.manifest_key(NAME, VERSION), 40))
    spy.no_checksum.update(_junk_versions(mem, store.manifest_sig_key(NAME, VERSION), 40))
    assert _verify(spy, pub, keys, _cache(tmp_path / "unvouched")).manifest_sha256 == pub.sha


def test_missing_local_file(tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    # A cached directory with a file gone: rebuilt.
    v = _verify(mem, pub, keys, cache)
    os.chmod(v.path, 0o700)
    os.unlink(v.path / "tokenizer.json")
    os.chmod(v.path, 0o500)
    again = _verify(mem, pub, keys, cache)
    assert sorted(os.listdir(again.path)) == [f.path for f in pub.m.files]
    # A download that leaves a file out: refused, nothing left.
    real = verify._download

    def leaves_one_out(source: Any, m: Any, name_fd: int, partial: str) -> None:
        real(source, m, name_fd, partial)
        os.unlink(f"{partial}/tokenizer.json", dir_fd=name_fd)

    monkeypatch.setattr(verify, "_download", leaves_one_out)
    fresh = _cache(tmp_path / "fresh")
    assert _refused(_verify, mem, pub, keys, fresh).reason == "missing_file"
    assert _pin_dirs(fresh, pub) == []


def test_definitive_refusal_removes_earlier_cache(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    v = _verify(mem, pub, keys, cache)

    # A passing store failure in steps 2-4 keeps yesterday's verified directory.
    class Down(Spy):
        def versions(self, key: str) -> list[str]:
            raise Refused("store_unavailable", detail_code="transport")

    assert _refused(_verify, Down(mem), pub, keys, cache).reason == "store_unavailable"
    assert v.path.is_dir()
    # The manifest key rotated out of the policy: definitive, the directory goes.
    rotated = Keys(MemSigner(), _policy(MemSigner()))
    assert _refused(_verify, mem, pub, rotated, cache).reason == "signer_untrusted"
    assert _pin_dirs(cache, pub) == []
    # A lock that no longer matches anything stored: definitive too.
    _verify(mem, pub, keys, cache)
    entry = Pin(NAME, VERSION, pub.sha, GATE)
    mem.objects[store.manifest_key(NAME, VERSION)] = []
    mem.shadow(store.manifest_key(NAME, VERSION), b'{"other":1}')
    assert _refused(_verify, mem, pub, keys, cache, entry=entry).reason == "lock_mismatch"
    assert _pin_dirs(cache, pub) == []


def test_transient_error_keeps_a_verified_directory(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    v = _verify(mem, pub, keys, cache)
    before = {f.path: (v.path / f.path).read_bytes() for f in pub.m.files}

    class SignatureDown(Spy):
        def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
            if key == store.manifest_sig_key(NAME, VERSION):
                raise Refused("store_unavailable", detail_code="transport")
            return super().get(key, version_id, max_bytes=max_bytes)

    # Another process using the directory must not lose it to this one's outage.
    assert _refused(_verify, SignatureDown(mem), pub, keys, cache).reason == "store_unavailable"
    assert {p: (v.path / p).read_bytes() for p in before} == before
    assert _pin_dirs(cache, pub) == [v.path.name]
    assert _verify(mem, pub, keys, cache).path == v.path


def test_cache_parents_must_be_safe(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    shared = tmp_path / "shared"
    shared.mkdir()
    os.chmod(shared, 0o777)  # anyone can swap what is below it
    err = _refused(_verify, mem, pub, keys, shared / "cache")
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_parent_unsafe")
    os.chmod(shared, 0o1777)  # sticky, like /tmp: fine
    assert _verify(mem, pub, keys, shared / "cache").path.is_dir()
    if os.geteuid() == 0:  # only root can hand a directory to someone else
        other = tmp_path / "other"
        other.mkdir(mode=0o755)
        os.chown(other, 54321, -1)
        err = _refused(_verify, mem, pub, keys, other / "cache")
        assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_parent_unsafe")


def test_close_error_is_a_fixed_refusal(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    real = os.fdopen

    class FailingClose:
        def __init__(self, inner: Any) -> None:
            self.inner = inner

        def write(self, b: Any) -> Any:
            return self.inner.write(b)

        def fileno(self) -> int:
            return int(self.inner.fileno())

        def close(self) -> None:
            self.inner.close()
            raise OSError(5, "I/O error at close")

    def fdopen(fd: int, mode: str = "r", *a: Any, **kw: Any) -> Any:
        f = real(fd, mode, *a, **kw)
        return FailingClose(f) if mode == "wb" else f

    monkeypatch.setattr(verify.os, "fdopen", fdopen)
    cache = _cache(tmp_path)
    err = _refused(_verify, mem, pub, keys, cache)
    assert (err.reason, err.detail_code) == ("file_not_allowed", "partial_close")
    assert err.__context__ is None and err.__cause__ is None
    assert _pin_dirs(cache, pub) == []


def _verify_in_child(mem: MemoryStore, pub: Published, keys: Keys, cache: Path) -> None:
    spy = Spy(mem)
    spy.delay = 0.05
    try:
        _verify(spy, pub, keys, cache)
    except BaseException:  # noqa: BLE001 - the exit code is the result
        os._exit(1)
    os._exit(0)


def test_concurrent_verifies_in_processes(tmp_path: Path, keys: Keys) -> None:
    import multiprocessing

    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    ctx = multiprocessing.get_context("fork")
    procs = [ctx.Process(target=_verify_in_child, args=(mem, pub, keys, cache)) for _ in range(3)]
    for p in procs:
        p.start()
    for p in procs:
        p.join(timeout=120)
    assert [p.exitcode for p in procs] == [0, 0, 0]
    assert _pin_dirs(cache, pub) == [_final(cache, pub).name]
    assert _verify(mem, pub, keys, cache).path == _final(cache, pub)


# ---------------------------------------------------------------- round 2 (D30)


def test_logins_are_lowercase_only(tmp_path: Path, keys: Keys) -> None:
    from intelligence.modelstore import names

    assert names.check_login("alice") == "alice"
    for bad in ("Alice", "ALICE", "alice-", "-alice", "al--ice", "", "a" * 40):
        assert _refused(names.check_login, bad).reason == "invalid_name"
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    # The training manifest's trainer and the manifest's promoter and trainer too.
    tm = json.loads((pub.run_dir / training.FILE_NAME).read_text(encoding="utf-8"))
    tm["trained_by"] = "Alice"
    err = _refused(training.parse, tm)
    assert (err.reason, err.detail_code) == ("manifest_invalid", "trained_by")
    obj = manifest.to_obj(pub.m)
    for path, value in ((("promotion", "actor"), "Promoter-Bot"), (("trained_by",), "Alice")):
        o = canonical.loads_strict(canonical.dumps(obj), manifest.MAX_BYTES)
        target = o
        for k in path[:-1]:
            target = target[k]
        target[path[-1]] = value
        assert _refused(manifest.parse, canonical.dumps(o)).reason == "manifest_invalid"
    # The fixtures and the committed example are lowercase.
    assert names.is_login(fixtures.TRAINED_BY)
    example = Path(__file__).resolve().parents[4] / "docs/models/training_manifest.example.json"
    assert names.is_login(json.loads(example.read_text(encoding="utf-8"))["trained_by"])


def test_workflow_actor_is_stored_lowercase(monkeypatch: pytest.MonkeyPatch) -> None:
    from intelligence.modelstore import cli

    for name, value in zip(
        cli.WORKFLOW_ENVS,
        (
            "4242",
            "o/r/.github/workflows/model-promote.yml@refs/heads/main",
            "ab" * 20,
            "Promoter-Bot",
        ),
        strict=True,
    ):
        monkeypatch.setenv(name, value)
    run = cli._workflow()
    assert run is not None and run.actor == "promoter-bot"


def test_flood_of_unreadable_versions_is_capped(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    spy = Spy(mem)
    junk = _junk_versions(mem, store.manifest_key(NAME, VERSION), verify.MAX_VERSION_READS + 10)
    spy.no_checksum.update(junk)
    err = _refused(_verify, spy, pub, keys, _cache(tmp_path))
    assert (err.reason, err.detail_code) == ("lock_mismatch", "too_many_versions")
    heads = [r for r in spy.reads if r[0] == "head"]
    assert len(heads) == verify.MAX_VERSION_READS  # reading stopped at the cap


@pytest.mark.parametrize("failure", ["checksum", "hash_mismatch", "not_found", "transport"])
def test_transient_read_of_the_lock_version_keeps_the_cache(
    tmp_path: Path, keys: Keys, failure: str
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    v = _verify(mem, pub, keys, cache)
    key = store.manifest_key(NAME, VERSION)

    class Flaky(Spy):
        def get(self, k: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
            if k == key:
                if failure in ("checksum", "transport"):
                    raise Refused("store_unavailable", detail_code=failure)
                raise Refused(failure)
            return super().get(k, version_id, max_bytes=max_bytes)

    err = _refused(_verify, Flaky(mem), pub, keys, cache)
    assert err.reason == "store_unavailable"
    assert _pin_dirs(cache, pub) == [v.path.name]  # not purged


def test_cache_path_with_a_link_is_refused(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    real = tmp_path / "real"
    real.mkdir(mode=0o700)
    link = tmp_path / "via-link"
    os.symlink(real, link)
    err = _refused(_verify, mem, pub, keys, link / "cache")
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_parent_link")
    err = _refused(_verify, mem, pub, keys, real / ".." / "real" / "cache")
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_dir")
    assert _verify(mem, pub, keys, real / "cache").path.is_dir()


def test_stale_partial_that_cannot_be_removed_keeps_the_final(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    v = _verify(mem, pub, keys, cache)
    before = {f.path: (v.path / f.path).read_bytes() for f in pub.m.files}
    real = verify._remove

    def stuck(parent_fd: int, name: str) -> bool:
        if name.endswith(verify.PARTIAL_SUFFIX):
            return False
        return real(parent_fd, name)

    monkeypatch.setattr(verify, "_remove", stuck)
    err = _refused(_verify, mem, pub, keys, cache)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "cache_not_removed")
    assert {p: (v.path / p).read_bytes() for p in before} == before


# ---------------------------------------------------------------- P14 at load


@pytest.mark.parametrize("case", ["rehearsal_gate", "fixture_name"])
def test_production_load_refuses_rehearsal_gate_and_fixture_names(
    case: str, tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Outside development the load refuses a pin that lock.check_production refuses (the
    rehearsal gate, a fixture- name), before anything is fetched, even with a trusted
    signature: lock-check is not the only P14 control."""
    mem = MemoryStore()
    name, gate = ("fixture-tiny", GATE) if case == "fixture_name" else (NAME, "rehearsal")
    records, docs, run_dir = _upload(mem, tmp_path, name=name)
    raw = _manifest_bytes(records, docs, name=name, gates={gate: "pass"})
    _put(mem, store.manifest_sig_key(name, VERSION), keys.manifest.sign("manifest", raw))
    _put(mem, store.manifest_key(name, VERSION), raw)
    pub = Published(raw, manifest.sha256(raw), manifest.parse(raw), run_dir)
    pin = Pin(name, VERSION, pub.sha, gate)
    # The verifier itself accepts it (signed, every file): only the production rule refuses.
    assert verify.verify_pin(mem, ROLE, pin, keys.policy, _cache(tmp_path / "p")).path.is_dir()
    cache = _cache(tmp_path / "load")
    spy = Spy(mem)
    assert verify.is_development() is False
    err = _refused(verify.verify_for_load, spy, ROLE, pin, keys.policy, cache)
    if case == "fixture_name":
        assert (err.reason, err.detail_code) == ("fixture_name", None)
    else:
        assert (err.reason, err.detail_code) == ("gate_failed", "not_production")
    assert spy.reads == [] and spy.downloads == 0 and os.listdir(cache) == []
    # In development (both switches) the same pin loads, as the CI rehearsal needs.
    monkeypatch.setenv(signing.ENV_VAR, "development")
    monkeypatch.setenv(verify.APP_ENV, "development")
    assert verify.verify_for_load(mem, ROLE, pin, keys.policy, cache).path.is_dir()


# ---------------------------------------------------------------- the load: fail closed


def test_production_load_needs_no_signoff_record(tmp_path: Path, keys: Keys) -> None:
    """The lean flow: a signed, pinned version with a production gate loads in
    production with nothing beside the manifest and its signature; the PR's approval
    is the sign-off."""
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    assert verify.is_development() is False
    v = verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, _cache(tmp_path))
    assert sorted(os.listdir(v.path)) == [f.path for f in pub.m.files]
    assert not [k for k in mem.objects if "signoff" in k]


def test_load_entry_point_has_no_switch(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    cache = _cache(tmp_path)
    for kw in ({"require_signoff": False}, {"require_gate": False}, {"production": False}):
        with pytest.raises(TypeError):
            verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, cache, **kw)  # type: ignore[call-arg]
    assert not hasattr(verify, "requires_signoff")


def test_bad_signature_bytes_refused_at_load(tmp_path: Path, keys: Keys) -> None:
    """A signature document by the trusted key whose signature bytes don't verify over the
    manifest (one bit flipped), or that signs other bytes: refused, nothing cached."""
    cache = _cache(tmp_path)
    for case in ("flipped", "other_bytes"):
        mem = MemoryStore()
        pub = publish(mem, tmp_path / case, keys, sign=False)
        if case == "flipped":
            doc = signing.parse_signature(keys.manifest.sign("manifest", pub.raw))
            bad = bytearray(doc.signature)
            bad[0] ^= 0x01
            sig = signing.SignatureDoc("manifest", "ed25519", doc.key_id, bytes(bad)).to_bytes()
        else:
            sig = keys.manifest.sign("manifest", pub.raw + b" ")
        _put(mem, store.manifest_sig_key(NAME, VERSION), sig)
        err = _refused(verify.verify_for_load, mem, ROLE, pub.pin, keys.policy, cache)
        assert err.reason == "signature_invalid", case
        assert _pin_dirs(cache, pub) == []


def test_untrusted_signer_and_testset_purpose_refused_at_load(tmp_path: Path, keys: Keys) -> None:
    cache = _cache(tmp_path)
    for case, signer, purpose, reason in (
        ("stranger", MemSigner(), "manifest", "signer_untrusted"),
        ("testset", keys.manifest, "testset", "signature_invalid"),
    ):
        mem = MemoryStore()
        pub = publish(mem, tmp_path / case, keys, sign=False)
        _put(mem, store.manifest_sig_key(NAME, VERSION), signer.sign(purpose, pub.raw))
        err = _refused(verify.verify_for_load, mem, ROLE, pub.pin, keys.policy, cache)
        assert err.reason == reason, case
        assert _pin_dirs(cache, pub) == []


def test_gate_fail_refused_at_load(tmp_path: Path, keys: Keys) -> None:
    mem = MemoryStore()
    records, docs, _ = _upload(mem, tmp_path)
    raw = _manifest_bytes(records, docs, gates={GATE: "fail"})
    _put(mem, store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", raw))
    _put(mem, store.manifest_key(NAME, VERSION), raw)
    pin = Pin(NAME, VERSION, manifest.sha256(raw))
    spy = Spy(mem)
    err = _refused(verify.verify_for_load, spy, ROLE, pin, keys.policy, _cache(tmp_path))
    assert err.reason == "gate_failed"
    assert spy.downloads == 0


def test_hash_mismatch_refused_at_load(tmp_path: Path, keys: Keys) -> None:
    """The store hands back a changed weights byte: the load is refused, nothing is
    cached; a cached copy edited after the load is found at the next start and fetched
    again, never handed out."""
    mem = MemoryStore()
    pub = publish(mem, tmp_path, keys)
    weights = _weights(pub)
    spy = Spy(mem)
    spy.corrupt.add(weights)
    cache = _cache(tmp_path)
    err = _refused(verify.verify_for_load, spy, ROLE, pub.pin, keys.policy, cache)
    assert err.reason == "hash_mismatch"
    assert _pin_dirs(cache, pub) == []
    v = verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, cache)
    _tamper(v.path / weights)
    again = verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, cache)
    assert (again.path / weights).read_bytes() == (pub.run_dir / weights).read_bytes()


# ---------------------------------------------------------------- a bucket without versioning


def test_unversioned_store_verifies_and_refuses_overwrites(tmp_path: Path, keys: Keys) -> None:
    """Versioning is optional: every object is version ``null``, and the hashes are the
    guard. An overwrite of a weights file, of the manifest or of its signature (a write
    outside the workflow) is refused, never loaded."""
    mem = MemoryStore(versioned=False)
    pub = publish(mem, tmp_path, keys)
    assert {f.store_version_id for f in pub.m.files} == {store.NULL_VERSION}
    cache = _cache(tmp_path)
    v = verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, cache)
    weights = _weights(pub)
    original = (pub.run_dir / weights).read_bytes()
    assert (v.path / weights).read_bytes() == original

    # The cached copy edited: refetched from the store and still the signed bytes.
    _tamper(v.path / weights)
    assert (
        verify.verify_for_load(mem, ROLE, pub.pin, keys.policy, cache).path / weights
    ).read_bytes() == original

    # The weights overwritten in the store, and the cache edited too: refused, and the
    # pin's directory is gone.
    key = store.run_file_key(NAME, VERSION, RUN, weights)
    mem.shadow(key, original[:-1] + bytes([original[-1] ^ 1]))
    fresh = _cache(tmp_path / "fresh")
    assert _refused(verify.verify_for_load, mem, ROLE, pub.pin, keys.policy, fresh).reason == (
        "hash_mismatch"
    )
    assert _pin_dirs(fresh, pub) == []
    _tamper(_final(cache, pub) / weights)
    assert _refused(verify.verify_for_load, mem, ROLE, pub.pin, keys.policy, cache).reason == (
        "hash_mismatch"
    )
    assert _pin_dirs(cache, pub) == []

    # The manifest overwritten (even with a valid signature by the trusted key): not the
    # lock's hash, refused.
    mem2 = MemoryStore(versioned=False)
    pub2 = publish(mem2, tmp_path / "m", keys)
    records, docs, _ = _upload(MemoryStore(), tmp_path / "other", seed=8)
    other = _manifest_bytes(records, docs)
    mem2.shadow(store.manifest_key(NAME, VERSION), other)
    mem2.shadow(store.manifest_sig_key(NAME, VERSION), keys.manifest.sign("manifest", other))
    err = _refused(
        verify.verify_for_load, mem2, ROLE, pub2.pin, keys.policy, _cache(tmp_path / "m")
    )
    assert err.reason == "lock_mismatch"

    # The signature overwritten with junk: refused.
    mem3 = MemoryStore(versioned=False)
    pub3 = publish(mem3, tmp_path / "s", keys)
    mem3.shadow(store.manifest_sig_key(NAME, VERSION), b"junk")
    err = _refused(
        verify.verify_for_load, mem3, ROLE, pub3.pin, keys.policy, _cache(tmp_path / "s")
    )
    assert err.reason == "signature_invalid"
