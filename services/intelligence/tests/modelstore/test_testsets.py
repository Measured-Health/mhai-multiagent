"""The locked test split: built once inside the store, signed, pinned in models/testsets.lock.

The tests marked ``store`` build a small split (``SCALE``) into the session's MinIO through
``mhai-models testset-build``, exactly as the workflow does, and fetch it back as the eval
reader. The rest need no Docker: the lock file's strict format, the CLI's checks before any
I/O, the locked manifest's checks, and the build's clean-up on failure (an in-memory store).
Every set is synthetic. Nothing here prints a document id of a test split.
"""

from __future__ import annotations

import base64
import contextlib
import hashlib
import io
import json
import logging
import os
import re
import secrets
import uuid
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import sets
from intelligence.doceval.synth import scanner as synth_scanner
from intelligence.modelstore import canonical, cli, signing, store, testsets
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo
from tests.modelstore.conftest import MinioStore

REPO = Path(__file__).resolve().parents[4]
LOCK_FILE = REPO / "models" / "testsets.lock"
SUITE = "clinical_document"
SCALE = 0.02
DAY = 1
LOCK_LINE = re.compile(r"clinical_document@[0-9]+\.[0-9]+\.[0-9]+#[0-9a-f]{64}")
SHA = "ab" * 32


def _refused(fn: Any, *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _version() -> str:
    # At most five digits in all: the scanner refuses a longer joined digit run anywhere in
    # testset.json, the version included (``long_digits``).
    return f"3.{secrets.randbelow(1000)}.{secrets.randbelow(10)}"


# ---------------------------------------------------------------- the lock file


def test_committed_lock_has_no_entries() -> None:
    raw = LOCK_FILE.read_bytes()
    assert raw.endswith(b"\n")
    assert all(line.startswith(b"#") for line in raw.splitlines())
    assert testsets.parse_lock(raw) == {}
    assert testsets.read_lock(LOCK_FILE) == {}


def test_lock_entry_round_trip() -> None:
    text = f"{SUITE}@1.0.0#{SHA}"
    entry = testsets.LockEntry.parse(text)
    assert (entry.suite, entry.version, entry.sha256) == (SUITE, "1.0.0", SHA)
    assert entry.text == text
    lock = testsets.parse_lock(f"# header\n{text}\n{SUITE}@1.0.1#{'cd' * 32}\n".encode())
    assert lock[(SUITE, "1.0.0")] == entry
    assert sorted(lock) == [(SUITE, "1.0.0"), (SUITE, "1.0.1")]


@pytest.mark.parametrize(
    "raw",
    [
        f"{SUITE}@1.0.0#{SHA}".encode(),  # no final newline
        f"{SUITE}@1.0.0#{SHA}\r\n".encode(),
        f"{SUITE}@1.0.0#{SHA} \n".encode(),
        f"{SUITE}@1.0.0#{SHA}\n\n".encode(),  # blank line
        f"\n{SUITE}@1.0.0#{SHA}\n".encode(),
        f"{SUITE}@1.0.0#{SHA.upper()}\n".encode(),
        f"{SUITE}@1.0#{SHA}\n".encode(),
        f"{SUITE}@01.0.0#{SHA}\n".encode(),
        f"{SUITE}@1.0.0#{SHA[:-1]}\n".encode(),
        f"Clinical@1.0.0#{SHA}\n".encode(),
        f"{SUITE}@1.0.0#{SHA}\n{SUITE}@1.0.0#{'cd' * 32}\n".encode(),  # duplicate
        f"{SUITE}@1.0.0#{SHA}\n{SUITE}@1.0.0#{SHA}\n".encode(),
        f"  {SUITE}@1.0.0#{SHA}\n".encode(),
        "# café\n".encode(),
        b"#\ttab\n",
        b"\xef\xbb\xbf# bom\n",
        b"# ok\n#\x00\n",
    ],
)
def test_lock_parse_strict(raw: bytes) -> None:
    assert _refused(testsets.parse_lock, raw).reason == "lock_invalid"


def test_lock_entry_strict() -> None:
    for text in (f"{SUITE}@1.0.0", f"{SUITE}#{SHA}", f"{SUITE}@1.0.0#{SHA}\n", "", 7):
        assert _refused(testsets.LockEntry.parse, text).reason == "lock_invalid"


def test_lock_read_refuses_a_link(tmp_path: Path) -> None:
    target = tmp_path / "real.lock"
    target.write_bytes(b"# x\n")
    link = tmp_path / "testsets.lock"
    link.symlink_to(target)
    assert _refused(testsets.read_lock, link).reason == "lock_invalid"


# ---------------------------------------------------------------- the CLI, before any I/O


@pytest.fixture
def no_build(monkeypatch: pytest.MonkeyPatch) -> None:
    def fail(*_a: Any, **_kw: Any) -> Any:
        raise AssertionError("the build must not start")

    monkeypatch.setattr(testsets, "build_and_lock", fail)


def _cli_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path, **overrides: str | None) -> None:
    env: dict[str, str | None] = {
        "MODEL_STORE_ENDPOINT": "https://store.example.test",
        "MODEL_STORE_BUCKET": "mhai-models-test",
        "MODEL_STORE_WRITER_ACCESS_KEY": "synthetic-access",
        "MODEL_STORE_WRITER_SECRET_KEY": "synthetic-secret-not-real",
        "MHAI_MODELS_SIGNER": str(tmp_path / "missing.pem"),
        "MHAI_MODELS_TRUST_POLICY": str(tmp_path / "missing-policy.json"),
        "MHAI_MODELS_ENV": "development",
    }
    env.update(overrides)
    for k, v in env.items():
        if v is None:
            monkeypatch.delenv(k, raising=False)
        else:
            monkeypatch.setenv(k, v)


def _run_cli(argv: list[str]) -> tuple[int, str]:
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = cli.main(argv)
    return code, out.getvalue()


def _build_argv(version: str = "1.0.0", *extra: str) -> list[str]:
    return ["testset-build", "--suite", SUITE, "--version", version, *extra]


@pytest.mark.parametrize(
    "endpoint",
    ["http://store.example.test:9000", "http://10.0.0.5:9000", "ftp://store.example.test"],
)
def test_cli_refuses_plain_http_to_a_remote_store(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, no_build: None, endpoint: str
) -> None:
    _cli_env(monkeypatch, tmp_path, MODEL_STORE_ENDPOINT=endpoint)
    code, out = _run_cli(_build_argv())
    assert (code, json.loads(out)) == (2, {"refused": "store_unavailable"})


@pytest.mark.parametrize(
    "argv",
    [
        ["testset-build", "--suite", "Clinical", "--version", "1.0.0"],
        ["testset-build", "--suite", "other_suite", "--version", "1.0.0"],
        ["testset-build", "--suite", SUITE, "--version", "1.0"],
        ["testset-build", "--suite", SUITE, "--version", "../1.0.0"],
        ["testset-build", "--suite", SUITE, "--version", "1.0.0", "--scale", "0"],
        ["testset-build", "--suite", SUITE, "--version", "1.0.0", "--scale", "11"],
        ["testset-build", "--suite", SUITE, "--version", "1.0.0", "--scale", "nan"],
    ],
)
def test_cli_checks_names_before_any_io(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, no_build: None, argv: list[str]
) -> None:
    # The endpoint is refused too, so a check made after reading the environment would
    # show as store_unavailable instead.
    _cli_env(monkeypatch, tmp_path, MODEL_STORE_ENDPOINT="http://store.example.test")
    code, out = _run_cli(argv)
    assert code == 2
    assert json.loads(out)["refused"] in ("invalid_name", "no_suite")


@pytest.mark.parametrize(
    "missing",
    [
        "MODEL_STORE_ENDPOINT",
        "MODEL_STORE_BUCKET",
        "MODEL_STORE_WRITER_ACCESS_KEY",
        "MODEL_STORE_WRITER_SECRET_KEY",
        "MHAI_MODELS_SIGNER",
        "MHAI_MODELS_TRUST_POLICY",
    ],
)
def test_cli_needs_its_settings_from_the_environment(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, no_build: None, missing: str
) -> None:
    _cli_env(monkeypatch, tmp_path, **{missing: None})
    code, out = _run_cli(_build_argv())
    assert code == 2
    assert set(json.loads(out)) == {"refused"}


def test_cli_dev_signer_only_in_development(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, no_build: None
) -> None:
    """A development key signs in production only if the policy lists it, and a policy
    listing a development key is refused in production: nothing is built."""
    monkeypatch.setenv("MHAI_MODELS_ENV", "development")
    keys = _make_keys(tmp_path / "k")
    _cli_env(
        monkeypatch,
        tmp_path,
        MHAI_MODELS_SIGNER=str(keys.key_path),
        MHAI_MODELS_TRUST_POLICY=str(keys.policy_path),
        MHAI_MODELS_ENV="production",
    )
    code, out = _run_cli(_build_argv())
    assert (code, json.loads(out)) == (2, {"refused": "policy_invalid"})


# ---------------------------------------------------------------- signing helpers


@dataclass
class Keys:
    signer: signing.FileSigner
    policy: signing.TrustPolicy
    policy_path: Path
    key_path: Path
    empty_policy: signing.TrustPolicy = field(repr=False)


def _entry_for(signer: signing.FileSigner) -> dict[str, Any]:
    return {
        "alg": "ed25519",
        "key_id": signer.key_id,
        "public_key_pem": signer.public_key_pem(),
        "development": True,
    }


def _make_keys(folder: Path) -> Keys:
    folder.mkdir(mode=0o700, parents=True, exist_ok=True)
    signing.generate_dev_key(folder / "keys")
    key_path = folder / "keys" / signing.DEV_KEY_FILE
    signer = signing.FileSigner(key_path)
    obj = {"schema": signing.TRUST_SCHEMA, "signers": [_entry_for(signer)]}
    policy_path = folder / "trust-policy.json"
    policy_path.write_bytes(canonical.dumps(obj))
    empty = {"schema": signing.TRUST_SCHEMA, "signers": []}
    return Keys(
        signer=signer,
        policy=signing.parse_policy(obj, environment="development"),
        policy_path=policy_path,
        key_path=key_path,
        empty_policy=signing.parse_policy(empty, environment="development"),
    )


@pytest.fixture
def dev_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(signing.ENV_VAR, "development")


# ---------------------------------------------------------------- the locked manifest


def _locked_obj(n: int = 2) -> dict[str, Any]:
    paths = [f"pages/doc{i}/1.png" for i in range(n)]
    return {
        "schema": sets.TESTSET_SCHEMA,
        "name": SUITE,
        "version": "1.0.0",
        "purpose": "test",
        "extraction_schema_version": "1.0.0",
        "locked": True,
        "files": [{"path": p, "size": 3, "sha256": SHA} for p in paths],
        "store_version_ids": {p: str(uuid.uuid4()) for p in paths},
        "slices": {},
        "totals": {},
        "documents": [],
    }


def test_locked_manifest_checks() -> None:
    entry = testsets.LockEntry(SUITE, "1.0.0", SHA)
    good = _locked_obj()
    assert len(testsets.check_locked(canonical.dumps(good), entry)) == 2

    def mutate(fn: Any) -> bytes:
        obj = json.loads(canonical.dumps(good))
        fn(obj)
        return canonical.dumps(obj)

    cases = [
        mutate(lambda o: o["store_version_ids"].popitem()),  # a file without a version
        mutate(lambda o: o["store_version_ids"].update({"pages/extra/1.png": "v1"})),
        mutate(lambda o: o["files"].pop()),  # a version without a file
        mutate(lambda o: o["files"].append(dict(o["files"][0]))),  # duplicate path
        mutate(lambda o: o.update(locked=False)),
        mutate(lambda o: o.pop("locked")),
        mutate(lambda o: o.update(purpose="dev")),
        mutate(lambda o: o.update(name="other")),
        mutate(lambda o: o.update(version="1.0.1")),
        mutate(lambda o: o.update(schema="x")),
        mutate(lambda o: o["files"][0].update(path="../x.png")),
        mutate(lambda o: o["files"][0].update(path="testset.json")),
        mutate(lambda o: o["files"][0].update(size=-1)),
        mutate(lambda o: o["files"][0].update(sha256="AB" * 32)),
        mutate(lambda o: o["files"][0].update(extra=1)),
        mutate(lambda o: o["store_version_ids"].update({o["files"][0]["path"]: "a/b"})),
        canonical.dumps(good) + b" ",  # not canonical
        json.dumps(good, indent=1).encode(),
        b"[]",
    ]
    for raw in cases:
        assert _refused(testsets.check_locked, raw, entry).reason == "testset_mismatch"
    # A bucket without versioning records "null" for every file: accepted.
    unversioned = mutate(lambda o: o["store_version_ids"].update({o["files"][0]["path"]: "null"}))
    assert len(testsets.check_locked(unversioned, entry)) == 2


# ---------------------------------------------------------------- the build's clean-up


class MemoryStore:
    """An in-memory ObjectStore with versions, for the build's failure paths."""

    def __init__(self, fail_after: int | None = None) -> None:
        self.objects: dict[str, list[tuple[str, bytes]]] = {}
        self.fail_after = fail_after
        self.puts = 0

    def put_new(self, key: str, data: bytes | Path) -> str:
        store._check_key(key)
        if key in self.objects:
            raise Refused("version_exists")
        self.puts += 1
        if self.fail_after is not None and self.puts > self.fail_after:
            raise Refused("store_unavailable", detail_code="error")
        body = data if isinstance(data, bytes) else Path(data).read_bytes()
        vid = str(uuid.uuid4())
        self.objects[key] = [(vid, body)]
        return vid

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        for vid, body in self.objects.get(key, []):
            if version_id is None or vid == version_id:
                if len(body) > max_bytes:
                    raise Refused("too_large")
                return body
        raise Refused("not_found")

    def versions(self, key: str) -> list[str]:
        return [vid for vid, _ in reversed(self.objects.get(key, []))]

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo:
        for vid, body in self.objects.get(key, []):
            if version_id is None or vid == version_id:
                return ObjectInfo(vid, len(body), hashlib.sha256(body).hexdigest())
        raise Refused("not_found")

    def list(self, prefix: str) -> list[str]:
        return sorted(k for k in self.objects if k.startswith(prefix))


def _fake_build(
    seen: list[Path], fail: str | None = None, doc_id: str = "docaa", link: bool = False
) -> Any:
    def build(purpose: str, seed: Any, out_dir: Path, scale: float, *, version: str) -> Any:
        assert purpose == "test" and seed is None
        seen.append(Path(out_dir))
        root = Path(out_dir) / "test"
        (root / "pages" / doc_id).mkdir(parents=True)
        page = b"synthetic page"
        if link:  # the listed page is a link: the build's own read refuses it
            (root / "real.png").write_bytes(page)
            (root / "pages" / doc_id / "1.png").symlink_to(root / "real.png")
        else:
            (root / "pages" / doc_id / "1.png").write_bytes(page)
        if fail is not None:
            raise sets.SetError(fail)
        manifest = {
            "schema": sets.TESTSET_SCHEMA,
            "name": SUITE,
            "version": version,
            "purpose": "test",
            "extraction_schema_version": "1.0.0",
            "slices": {},
            "totals": {"documents": 1},
            "documents": [],
            "files": [
                {
                    "path": f"pages/{doc_id}/1.png",
                    "size": len(page),
                    "sha256": hashlib.sha256(page).hexdigest(),
                }
            ],
        }
        raw = canonical.dumps(manifest)
        (root / "testset.json").write_bytes(raw)
        return sets.SetManifest("test", version, root, hashlib.sha256(raw).hexdigest(), {}, 1, 1)

    return build


def _ok_scan(_path: Any) -> synth_scanner.ScanResult:
    return synth_scanner.ScanResult(True, (), 1)


def test_build_failure_removes_scratch(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path)
    scratch = tmp_path / "scratch"
    scratch.mkdir()
    seen: list[Path] = []
    monkeypatch.setattr(testsets.sets, "build", _fake_build(seen, fail="generation_failed"))
    s = MemoryStore()
    err = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert err.reason == "evaluation_failed"
    assert seen and not seen[0].exists()
    assert list(scratch.iterdir()) == []
    # Only the claim was written; no file of the split went up.
    assert list(s.objects) == [store.testset_claim_key(SUITE, "1.0.0")]


def test_scanner_refusal_uploads_nothing(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path)
    scratch = tmp_path / "scratch"
    scratch.mkdir()
    seen: list[Path] = []
    monkeypatch.setattr(testsets.sets, "build", _fake_build(seen))
    finding = synth_scanner.Finding("truth/x.json#/patient/mrn", "mrn_like")
    monkeypatch.setattr(
        testsets.synth_scanner,
        "scan_set",
        lambda _p: synth_scanner.ScanResult(False, (finding,), 1),
    )
    s = MemoryStore()
    err = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert err.reason == "dataset_not_synthetic"
    assert list(scratch.iterdir()) == []
    assert list(s.objects) == [store.testset_claim_key(SUITE, "1.0.0")]


def test_upload_failure_removes_scratch_and_writes_no_manifest(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path)
    scratch = tmp_path / "scratch"
    scratch.mkdir()
    monkeypatch.setattr(testsets.sets, "build", _fake_build([]))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    s = MemoryStore(fail_after=2)  # the claim and the page go up; testset.json fails
    err = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert err.reason == "store_unavailable"
    assert list(scratch.iterdir()) == []
    assert store.testset_key(SUITE, "1.0.0") not in s.objects
    assert store.testset_sig_key(SUITE, "1.0.0") not in s.objects


def test_untrusted_signer_refused_before_anything(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path)
    monkeypatch.setattr(testsets.sets, "build", _fake_build([]))
    s = MemoryStore()
    err = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.empty_policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=tmp_path,
    )
    assert err.reason == "signer_untrusted"
    assert s.objects == {}


def test_memory_build_and_fetch_round_trip(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path)
    monkeypatch.setattr(testsets.sets, "build", _fake_build([]))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    s = MemoryStore()
    built = testsets.build_and_lock(
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=tmp_path,
    )
    order = list(s.objects)
    assert order[0] == store.testset_claim_key(SUITE, "1.0.0")
    assert order[-2:] == [store.testset_key(SUITE, "1.0.0"), store.testset_sig_key(SUITE, "1.0.0")]
    assert set(built.export()) == {"sha256", "extraction_schema_version", "slices", "totals"}
    out = testsets.fetch(s, testsets.LockEntry.parse(built.entry.text), keys.policy, tmp_path / "f")
    assert (out / "pages" / "docaa" / "1.png").read_bytes() == b"synthetic page"
    assert hashlib.sha256((out / "testset.json").read_bytes()).hexdigest() == built.entry.sha256
    assert not (out / "testset.sig").exists()
    # A second build of the same suite and version stops at the claim.
    again = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=tmp_path,
    )
    assert again.reason == "version_exists"


# ---------------------------------------------------------------- against MinIO


@dataclass
class Locked:
    version: str
    entry: testsets.LockEntry
    stdout: str
    logs: str
    seeds: list[bytes]
    scratch_parent: Path
    keys: Keys


class _Capture(logging.Handler):
    def __init__(self) -> None:
        super().__init__(logging.DEBUG)
        self.lines: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.lines.append(self.format(record))


@pytest.fixture(scope="module")
def locked(minio_store: MinioStore, tmp_path_factory: pytest.TempPathFactory) -> Iterator[Locked]:
    """One small split, built through the CLI as the eval writer, with the seed spied on."""
    folder = tmp_path_factory.mktemp("testset")
    version = _version()
    scratch = folder / "scratch"
    scratch.mkdir(mode=0o700)
    seeds: list[bytes] = []
    real = secrets.token_bytes

    def spy(n: int | None = None) -> bytes:
        out = real(n)
        if n == 32:
            seeds.append(out)
        return out

    creds = minio_store.users["writer"]
    handler = _Capture()
    root_logger = logging.getLogger()
    old_level = root_logger.level
    with pytest.MonkeyPatch.context() as mp:
        mp.setenv(signing.ENV_VAR, "development")
        keys = _make_keys(folder)
        mp.setattr(sets.secrets, "token_bytes", spy)
        for k, v in {
            "MODEL_STORE_ENDPOINT": minio_store.endpoint,
            "MODEL_STORE_BUCKET": minio_store.bucket,
            "MODEL_STORE_WRITER_ACCESS_KEY": creds.access_key,
            "MODEL_STORE_WRITER_SECRET_KEY": creds.secret_key,
            "MHAI_MODELS_SIGNER": str(keys.key_path),
            "MHAI_MODELS_TRUST_POLICY": str(keys.policy_path),
            "MHAI_MODELS_SCRATCH": str(scratch),
        }.items():
            mp.setenv(k, v)
        root_logger.addHandler(handler)
        root_logger.setLevel(logging.DEBUG)
        try:
            code, out = _run_cli(_build_argv(version, "--scale", str(SCALE)))
        finally:
            root_logger.removeHandler(handler)
            root_logger.setLevel(old_level)
    assert code == 0, out
    entry = testsets.LockEntry.parse(out.strip())
    yield Locked(version, entry, out, "\n".join(handler.lines), seeds, scratch, keys)


def _locked_manifest(m: MinioStore, x: Locked) -> tuple[bytes, dict[str, Any]]:
    r = m.store("reader")
    key = store.testset_key(SUITE, x.version)
    for vid in r.versions(key):
        raw = r.get(key, vid, max_bytes=64 * 1024 * 1024)
        if hashlib.sha256(raw).hexdigest() == x.entry.sha256:
            return raw, json.loads(raw)
    raise AssertionError("the locked manifest is missing")


@pytest.mark.store
def test_build_upload_lock_and_fetch(
    minio_store: MinioStore, locked: Locked, tmp_path: Path, dev_env: None
) -> None:
    # The CLI prints the lock line and nothing else.
    assert LOCK_LINE.fullmatch(locked.stdout.rstrip("\n"))
    assert locked.stdout.count("\n") == 1
    assert locked.entry.version == locked.version
    raw, obj = _locked_manifest(minio_store, locked)
    assert obj["locked"] is True and obj["purpose"] == "test"
    assert set(obj["store_version_ids"]) == {f["path"] for f in obj["files"]}
    # Every object is stored as the split recorded it.
    root = minio_store.store()
    for f in obj["files"]:
        info = root.head(
            store.eval_file_key(SUITE, locked.version, f["path"]),
            obj["store_version_ids"][f["path"]],
        )
        assert (info.size, info.sha256) == (f["size"], f["sha256"])
    # It fetches back as the reader, and the runner's own checks accept it.
    out = testsets.fetch(minio_store.store("reader"), locked.entry, locked.keys.policy, tmp_path)
    assert (out / "testset.json").read_bytes() == raw
    assert not (out / "testset.sig").exists()
    assert oct(out.stat().st_mode & 0o777) == oct(0o700)
    for f in obj["files"]:
        data = (out / f["path"]).read_bytes()
        assert hashlib.sha256(data).hexdigest() == f["sha256"]
    from intelligence.doceval import runner

    runner._verify(out)
    # Neither stdout nor the logs carry a document id of the split.
    ids = [d["id"] for d in obj["documents"]]
    assert ids
    for doc_id in ids:
        assert doc_id not in locked.stdout
        assert doc_id not in locked.logs
    # A second build of the same version stops at the claim.
    with pytest.MonkeyPatch.context() as mp:
        creds = minio_store.users["writer"]
        _cli_env(
            mp,
            tmp_path,
            MODEL_STORE_ENDPOINT=minio_store.endpoint,
            MODEL_STORE_BUCKET=minio_store.bucket,
            MODEL_STORE_WRITER_ACCESS_KEY=creds.access_key,
            MODEL_STORE_WRITER_SECRET_KEY=creds.secret_key,
            MHAI_MODELS_SIGNER=str(locked.keys.key_path),
            MHAI_MODELS_TRUST_POLICY=str(locked.keys.policy_path),
        )
        code, again = _run_cli(_build_argv(locked.version, "--scale", str(SCALE)))
    assert (code, json.loads(again)) == (2, {"refused": "version_exists"})


@pytest.mark.store
def test_scratch_removed_and_seed_absent(minio_store: MinioStore, locked: Locked) -> None:
    assert list(locked.scratch_parent.iterdir()) == []
    assert len(locked.seeds) >= 1
    forms: list[bytes] = []
    for seed in locked.seeds:
        forms += [seed, seed.hex().encode(), base64.b64encode(seed), base64.b64encode(seed)[:-1]]
    root = minio_store.store()
    keys = root.list(f"eval/{SUITE}/{locked.version}/")
    assert store.testset_key(SUITE, locked.version) in keys
    assert store.testset_sig_key(SUITE, locked.version) in keys
    for key in keys:
        for vid in root.versions(key):
            body = root.get(key, vid, max_bytes=64 * 1024 * 1024)
            for form in forms:
                assert form not in body
    for form in forms:
        assert form.decode("latin-1") not in locked.stdout
        assert form.decode("latin-1") not in locked.logs


def _raw_put(m: MinioStore, role: str, key: str, body: bytes) -> str:
    resp = m.client(role).put_object(
        Bucket=m.bucket,
        Key=key,
        Body=body,
        ChecksumAlgorithm="SHA256",
        ChecksumSHA256=base64.b64encode(hashlib.sha256(body).digest()).decode(),
    )
    return str(resp["VersionId"])


@pytest.mark.store
def test_testset_tampered_file_refused(
    minio_store: MinioStore, locked: Locked, tmp_path: Path, dev_env: None
) -> None:
    raw, obj = _locked_manifest(minio_store, locked)
    page = next(f for f in obj["files"] if f["path"].endswith(".png"))
    key = store.eval_file_key(SUITE, locked.version, page["path"])
    # A holder of the eval writer's credentials shadows one page with a newer version.
    shadow = _raw_put(minio_store, "writer", key, b"not the locked page")
    reader = minio_store.store("reader")
    assert reader.versions(key)[0] == shadow
    # Fetch still takes the recorded version.
    out = testsets.fetch(reader, locked.entry, locked.keys.policy, tmp_path / "a")
    data = (out / page["path"]).read_bytes()
    assert hashlib.sha256(data).hexdigest() == page["sha256"]
    # A signed manifest that points at the shadow version is refused: the file's hash.
    bad = json.loads(raw)
    bad["store_version_ids"][page["path"]] = shadow
    bad_raw = canonical.dumps(bad)
    sig = locked.keys.signer.sign("testset", bad_raw).to_bytes()
    _raw_put(minio_store, "writer", store.testset_key(SUITE, locked.version), bad_raw)
    _raw_put(minio_store, "writer", store.testset_sig_key(SUITE, locked.version), sig)
    bad_entry = testsets.LockEntry(SUITE, locked.version, hashlib.sha256(bad_raw).hexdigest())
    err = _refused(testsets.fetch, reader, bad_entry, locked.keys.policy, tmp_path / "b")
    assert err.reason == "testset_mismatch"
    assert not (tmp_path / "b").exists() or list((tmp_path / "b").iterdir()) == []
    # The real lock entry still fetches: the shadow manifest and signature are skipped.
    out = testsets.fetch(reader, locked.entry, locked.keys.policy, tmp_path / "c")
    assert (out / "testset.json").read_bytes() == raw


@pytest.mark.store
def test_testset_lock_mismatch_refused(
    minio_store: MinioStore, locked: Locked, tmp_path: Path, dev_env: None
) -> None:
    reader = minio_store.store("reader")
    wrong = testsets.LockEntry(SUITE, locked.version, secrets.token_hex(32))
    err = _refused(testsets.fetch, reader, wrong, locked.keys.policy, tmp_path)
    assert err.reason == "testset_mismatch"
    assert list(tmp_path.iterdir()) == []
    # A version that was never built.
    missing = testsets.LockEntry(SUITE, _version(), locked.entry.sha256)
    err = _refused(testsets.fetch, reader, missing, locked.keys.policy, tmp_path)
    assert (err.reason, err.detail_code) == ("testset_mismatch", "missing")
    assert list(tmp_path.iterdir()) == []
    # A trust policy that doesn't trust the signer for testset.
    err = _refused(testsets.fetch, reader, locked.entry, locked.keys.empty_policy, tmp_path)
    assert (err.reason, err.detail_code) == ("testset_mismatch", "signer_untrusted")
    assert list(tmp_path.iterdir()) == []
    # A manifest version signed by a key nobody trusts.
    raw, obj = _locked_manifest(minio_store, locked)
    other = _make_keys(tmp_path.parent / f"other-{secrets.token_hex(4)}")
    obj["totals"] = dict(obj["totals"], documents=obj["totals"]["documents"] + 1)
    forged = canonical.dumps(obj)
    _raw_put(minio_store, "writer", store.testset_key(SUITE, locked.version), forged)
    _raw_put(
        minio_store,
        "writer",
        store.testset_sig_key(SUITE, locked.version),
        other.signer.sign("testset", forged).to_bytes(),
    )
    forged_entry = testsets.LockEntry(SUITE, locked.version, hashlib.sha256(forged).hexdigest())
    err = _refused(testsets.fetch, reader, forged_entry, locked.keys.policy, tmp_path)
    assert err.reason == "testset_mismatch"
    assert list(tmp_path.iterdir()) == []


@pytest.mark.store
def test_reader_cannot_build(
    minio_store: MinioStore, locked: Locked, tmp_path: Path, dev_env: None
) -> None:
    """The reader can't write a split; it can read the locked one."""
    scratch = tmp_path / "build-reader"
    scratch.mkdir()
    err = _refused(
        testsets.build_and_lock,
        minio_store.store("reader"),
        locked.keys.signer,
        locked.keys.policy,
        suite=SUITE,
        version=_version(),
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")
    assert list(scratch.iterdir()) == []


def test_seed_never_reaches_the_build_output(tmp_path: Path) -> None:
    """``sets.build`` refuses a seed for the test purpose; the build passes none."""
    with pytest.raises(sets.SetError):
        sets.build("test", 1, tmp_path, SCALE, version="1.0.0")


def test_scratch_folder_mode(tmp_path: Path) -> None:
    folder = testsets._new_scratch(tmp_path)
    try:
        assert folder.stat().st_mode & 0o777 == 0o700
        assert folder.parent == tmp_path
    finally:
        os.rmdir(folder)


def test_policies_scope_listing_to_the_layout() -> None:
    """Both store users list (and list versions) only under models/ and eval/."""
    folder = REPO / "deploy" / "minio" / "policies"
    assert sorted(p.stem for p in folder.glob("*.json")) == ["reader", "writer"]
    for path in folder.glob("*.json"):
        doc = json.loads(path.read_text())
        listing = [
            st
            for st in doc["Statement"]
            if st["Effect"] == "Allow" and "s3:ListBucketVersions" in st["Action"]
        ]
        assert listing == [
            {
                "Sid": "List",
                "Effect": "Allow",
                "Action": ["s3:ListBucket", "s3:ListBucketVersions"],
                "Resource": ["arn:aws:s3:::@BUCKET@"],
                "Condition": {"StringLike": {"s3:prefix": ["models/*", "eval/*"]}},
            }
        ]


# ---------------------------------------------------------------- fix round 1


PLANTED = "plantedtestsplitidbbbbbbbb"  # stands in for a test-split document id


def _no_planted(err: BaseException) -> None:
    """Neither the refusal, its attributes nor its exception chain carries the id."""
    seen: list[BaseException] = []
    e: BaseException | None = err
    while e is not None and e not in seen:
        seen.append(e)
        e = e.__cause__ or e.__context__
    for x in seen:
        for text in (str(x), repr(x), repr(vars(x)) if hasattr(x, "__dict__") else ""):
            assert PLANTED not in text
        assert PLANTED not in repr(getattr(x, "args", ()))
        assert PLANTED not in str(getattr(x, "filename", ""))


def _memory_locked(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, doc_id: str = "docaa"
) -> tuple[MemoryStore, Keys, Any]:
    keys = _make_keys(tmp_path / "k")
    monkeypatch.setattr(testsets.sets, "build", _fake_build([], doc_id=doc_id))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    s = MemoryStore()
    built = testsets.build_and_lock(
        s,
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=tmp_path,
    )
    return s, keys, built


def _add_version(s: MemoryStore, key: str, body: bytes) -> None:
    s.objects[key].append((str(uuid.uuid4()), body))


def test_same_bytes_and_junk_versions_still_fetch(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, keys, built = _memory_locked(tmp_path, monkeypatch)
    entry = testsets.LockEntry.parse(built.entry.text)
    tkey, skey = store.testset_key(SUITE, "1.0.0"), store.testset_sig_key(SUITE, "1.0.0")
    raw, sig = s.objects[tkey][0][1], s.objects[skey][0][1]
    # Identical re-uploads of both, and junk shadows (more of them than MAX_VERSIONS).
    for _ in range(3):
        _add_version(s, tkey, raw)
        _add_version(s, skey, sig)
    for n in range(testsets.MAX_VERSIONS + 2):
        _add_version(s, tkey, b"junk %d" % n)
        _add_version(s, skey, b"junk %d" % n)
    out = testsets.fetch(s, entry, keys.policy, tmp_path / "f")
    assert (out / "testset.json").read_bytes() == raw


def test_failed_signature_reads_count_toward_the_cap(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, keys, built = _memory_locked(tmp_path, monkeypatch)
    entry = testsets.LockEntry.parse(built.entry.text)
    assert entry.sig_version_id is None
    skey = store.testset_sig_key(SUITE, "1.0.0")
    # Only oversized junk: every read fails, and the reads must stop at the cap.
    big = b"x" * (signing.MAX_SIGNATURE_DOC_BYTES + 1)
    s.objects[skey] = [(str(uuid.uuid4()), big) for _ in range(testsets.MAX_VERSIONS * 3)]
    gets: list[str] = []
    real_get = s.get

    def counting_get(key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        if key == skey:
            gets.append(key)
        return real_get(key, version_id, max_bytes=max_bytes)

    monkeypatch.setattr(s, "get", counting_get)
    err = _refused(testsets.fetch, s, entry, keys.policy, tmp_path / "f")
    assert (err.reason, err.detail_code) == ("testset_mismatch", "signature")
    assert len(gets) == testsets.MAX_VERSIONS


def test_duplicate_signature_reads_count_toward_the_cap(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, keys, built = _memory_locked(tmp_path, monkeypatch)
    entry = testsets.LockEntry.parse(built.entry.text)
    skey = store.testset_sig_key(SUITE, "1.0.0")
    # Many versions with the same small junk bytes: each read counts, duplicate or not.
    s.objects[skey] = [(str(uuid.uuid4()), b"junk") for _ in range(testsets.MAX_VERSIONS * 3)]
    gets: list[str] = []
    real_get = s.get

    def counting_get(key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        if key == skey:
            gets.append(key)
        return real_get(key, version_id, max_bytes=max_bytes)

    monkeypatch.setattr(s, "get", counting_get)
    err = _refused(testsets.fetch, s, entry, keys.policy, tmp_path / "f")
    assert (err.reason, err.detail_code) == ("testset_mismatch", "signature")
    assert len(gets) == testsets.MAX_VERSIONS


def test_read_scratch_refusal_is_fixed(tmp_path: Path) -> None:
    err = _refused(testsets._read_scratch, tmp_path / "missing", 10)
    assert (err.reason, err.detail_code) == ("missing_file", "scratch")
    assert err.__cause__ is None and err.__context__ is None


def test_manifest_purpose_signature_refused_at_fetch(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, keys, built = _memory_locked(tmp_path, monkeypatch)
    entry = testsets.LockEntry.parse(built.entry.text)
    tkey, skey = store.testset_key(SUITE, "1.0.0"), store.testset_sig_key(SUITE, "1.0.0")
    raw = s.objects[tkey][0][1]
    # One key signs manifests and test splits: only the purpose tells them apart.
    both = keys.policy
    doc = keys.signer.sign("manifest", raw)
    relabelled = signing.SignatureDoc("testset", doc.alg, doc.key_id, doc.signature)
    for forged in (doc.to_bytes(), relabelled.to_bytes()):
        s.objects[skey] = [(str(uuid.uuid4()), forged)]
        err = _refused(testsets.fetch, s, entry, both, tmp_path / "f")
        assert (err.reason, err.detail_code) == ("testset_mismatch", "signature")
        assert not (tmp_path / "f").exists() or list((tmp_path / "f").iterdir()) == []


@pytest.mark.parametrize("call", ["open", "mkdir"])
def test_fetch_write_errors_carry_no_path(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch, call: str
) -> None:
    s, keys, built = _memory_locked(tmp_path, monkeypatch, doc_id=PLANTED)
    entry = testsets.LockEntry.parse(built.entry.text)
    real = getattr(os, call)

    def fail(path: Any, *a: Any, **kw: Any) -> Any:
        # Only fetch's own writes fail (mkdir, or an open that creates), not the clean-up.
        creating = call == "mkdir" or bool(a and isinstance(a[0], int) and a[0] & os.O_CREAT)
        if creating and PLANTED in str(path):
            raise OSError(28, "No space left on device", str(path))
        return real(path, *a, **kw)

    monkeypatch.setattr(testsets.os, call, fail)
    err = _refused(testsets.fetch, s, entry, keys.policy, tmp_path / "f")
    assert (err.reason, err.detail_code) == ("file_not_allowed", "write")
    _no_planted(err)
    monkeypatch.setattr(testsets.os, call, real)
    assert list((tmp_path / "f").iterdir()) == []


def test_build_read_refusal_carries_no_document_id(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path / "k")
    scratch = tmp_path / "scratch"
    scratch.mkdir()
    monkeypatch.setattr(testsets.sets, "build", _fake_build([], doc_id=PLANTED, link=True))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    err = _refused(
        testsets.build_and_lock,
        MemoryStore(),
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert err.detail_code == "scratch"
    assert type(err) is Refused
    _no_planted(err)
    assert list(scratch.iterdir()) == []


def test_bad_scratch_or_wrong_key_never_takes_the_claim(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    keys = _make_keys(tmp_path / "k")
    monkeypatch.setattr(testsets.sets, "build", _fake_build([]))
    s = MemoryStore()
    kw: dict[str, Any] = {
        "suite": SUITE,
        "version": "1.0.0",
        "scale": SCALE,
    }
    # A scratch root that doesn't exist.
    err = _refused(
        testsets.build_and_lock,
        s,
        keys.signer,
        keys.policy,
        scratch_parent=tmp_path / "missing",
        **kw,
    )
    assert (err.reason, err.detail_code) == ("not_allowed_here", "scratch")
    assert s.objects == {}
    # A signer that names the trusted key but holds another private key.
    other = _make_keys(tmp_path / "other")

    class Impostor:
        key_id = keys.signer.key_id
        alg = keys.signer.alg

        def sign(self, purpose: str, payload: bytes) -> signing.SignatureDoc:
            doc = other.signer.sign(purpose, payload)
            return signing.SignatureDoc(purpose, doc.alg, keys.signer.key_id, doc.signature)

    err = _refused(
        testsets.build_and_lock, s, Impostor(), keys.policy, scratch_parent=tmp_path, **kw
    )
    assert err.reason == "signature_invalid"
    assert s.objects == {}
    assert sorted(p.name for p in tmp_path.iterdir()) == ["k", "other"]


def test_scratch_not_removed_is_reported(
    tmp_path: Path,
    dev_env: None,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.DEBUG)
    monkeypatch.setattr(testsets.shutil, "rmtree", lambda *_a, **_kw: None)
    scratch = tmp_path / "scratch"
    scratch.mkdir()
    keys = _make_keys(tmp_path / "k")
    monkeypatch.setattr(testsets.sets, "build", _fake_build([], doc_id=PLANTED))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    built = testsets.build_and_lock(
        MemoryStore(),
        keys.signer,
        keys.policy,
        suite=SUITE,
        version="1.0.0",
        scale=SCALE,
        scratch_parent=scratch,
    )
    assert built.scratch_removed is False
    assert "code=scratch_not_removed" in caplog.text
    assert PLANTED not in caplog.text
    assert str(scratch) not in caplog.text


def _memory_cli_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, keys: Keys, s: MemoryStore
) -> None:
    _cli_env(
        monkeypatch,
        tmp_path,
        MHAI_MODELS_SIGNER=str(keys.key_path),
        MHAI_MODELS_TRUST_POLICY=str(keys.policy_path),
        MHAI_MODELS_SCRATCH=str(tmp_path / "scratch"),
    )
    (tmp_path / "scratch").mkdir(exist_ok=True)
    monkeypatch.setattr(cli.store, "S3Store", lambda *_a, **_kw: s)


def test_cli_exit_4_when_scratch_not_removed(
    tmp_path: Path,
    dev_env: None,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    keys = _make_keys(tmp_path / "k")
    _memory_cli_env(monkeypatch, tmp_path, keys, MemoryStore())
    monkeypatch.setattr(testsets.sets, "build", _fake_build([]))
    monkeypatch.setattr(testsets.synth_scanner, "scan_set", _ok_scan)
    monkeypatch.setattr(testsets.shutil, "rmtree", lambda *_a, **_kw: None)
    assert cli.main(_build_argv()) == 4
    out = capsys.readouterr()
    assert LOCK_LINE.fullmatch(out.out.strip())
    assert json.loads(out.err) == {"warning": "scratch_not_removed"}


def test_scanner_failure_logs_no_document_id(
    tmp_path: Path,
    dev_env: None,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The real build of a small test split, refused by the scanner inside sets.build
    with findings that name a (planted) document id: neither the logs nor the CLI's
    output carry it."""
    caplog.set_level(logging.DEBUG)
    keys = _make_keys(tmp_path / "k")
    s = MemoryStore()
    _memory_cli_env(monkeypatch, tmp_path, keys, s)
    findings = (
        synth_scanner.Finding(f"truth/{PLANTED}.json#patient.mrn", "mrn_like"),
        synth_scanner.Finding(f"pages/{PLANTED}/1.png", "orphan_image"),
    )
    monkeypatch.setattr(
        sets.synth_scanner, "scan_set", lambda _p: synth_scanner.ScanResult(False, findings, 2)
    )
    assert cli.main(_build_argv("1.0.0", "--scale", str(SCALE))) == 2
    out = capsys.readouterr()
    assert json.loads(out.out) == {"refused": "evaluation_failed"}
    assert "findings=2" in caplog.text
    for text in (caplog.text, out.out, out.err):
        assert PLANTED not in text
        assert "truth/" not in text and "pages/" not in text
    assert list((tmp_path / "scratch").iterdir()) == []
    assert list(s.objects) == [store.testset_claim_key(SUITE, "1.0.0")]


@pytest.mark.store
@pytest.mark.parametrize(
    "prefix", ["eval/../models/", "eval/%2e%2e/models/", "eval/./../models/", "eval/..%2fmodels/"]
)
def test_reader_listing_cannot_climb_out_of_the_layout(
    minio_store: MinioStore, locked: Locked, prefix: str
) -> None:
    from botocore.exceptions import ClientError

    raw = minio_store.client("reader")
    try:
        resp = raw.list_object_versions(Bucket=minio_store.bucket, Prefix=prefix)
    except ClientError:
        return  # refused outright
    keys = [v["Key"] for v in resp.get("Versions", []) + resp.get("DeleteMarkers", [])]
    keys += [p["Prefix"] for p in resp.get("CommonPrefixes", [])]
    assert not [k for k in keys if not k.startswith(("eval/", "models/"))]


@pytest.mark.store
def test_same_bytes_and_junk_versions_in_minio(
    minio_store: MinioStore, locked: Locked, tmp_path: Path, dev_env: None
) -> None:
    raw, _ = _locked_manifest(minio_store, locked)
    r = minio_store.store("reader")
    tkey = store.testset_key(SUITE, locked.version)
    skey = store.testset_sig_key(SUITE, locked.version)
    sig_vid = r.versions(skey)[-1]  # the oldest: the build's own
    sig = r.get(skey, sig_vid, max_bytes=4096)
    for body in (raw, b"junk manifest"):
        _raw_put(minio_store, "writer", tkey, body)
    for body in (sig, b"junk signature"):
        _raw_put(minio_store, "writer", skey, body)
    out = testsets.fetch(r, locked.entry, locked.keys.policy, tmp_path)
    assert (out / "testset.json").read_bytes() == raw
