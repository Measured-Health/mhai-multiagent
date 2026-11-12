"""The model store: conditional create, checksums, versioned or not, two roles' policies.

The tests marked ``store`` run against MinIO provisioned by deploy/minio/init-model-store.sh
(see conftest.py); the rest need no Docker. Every object is synthetic, under a fresh
model name per test, since nothing can be deleted.
"""

import base64
import hashlib
import io
import json
import logging
import os
import re
import secrets
from pathlib import Path
from typing import Any

import pytest
from botocore.exceptions import ClientError

from intelligence.modelstore import store
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import Credentials, S3Store
from tests.modelstore.conftest import ROLES, MinioStore

REPO = Path(__file__).resolve().parents[4]
POLICY_DIR = REPO / "deploy" / "minio" / "policies"
STORE_PY = REPO / "services" / "intelligence" / "intelligence" / "modelstore" / "store.py"
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
CREDS = Credentials("synthetic-access", "synthetic-secret-not-real")


def _name() -> str:
    return f"t{secrets.token_hex(6)}"


def _refused(fn: Any, *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _denied(fn: Any, *args: Any, **kw: Any) -> None:
    err = _refused(fn, *args, **kw)
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")


def _raw_denied(fn: Any, **kw: Any) -> str:
    with pytest.raises(ClientError) as e:
        fn(**kw)
    return str(e.value.response["Error"]["Code"])


def _promote(m: MinioStore, name: str, version: str = "1.0.0") -> dict[str, str]:
    """What a promotion writes: the claim, one run file, one document, the manifest."""
    p = m.store("writer")
    keys = {
        "claim": store.claim_key(name, version),
        "file": store.run_file_key(name, version, "run_1", "config.json"),
        "doc": store.run_doc_key(name, version, "run_1", "model_card.md"),
        "sig": store.manifest_sig_key(name, version),
        "manifest": store.manifest_key(name, version),
    }
    return {k: p.put_new(key, f"synthetic {k}".encode()) for k, key in keys.items()} | {
        f"{k}_key": key for k, key in keys.items()
    }


# ---------------------------------------------------------------- layout (no Docker)


def test_layout_keys() -> None:
    assert store.claim_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/claim.json"
    assert (
        store.run_file_key("mhai-tiny", "1.2.3", "run_1", "model.safetensors")
        == "models/mhai-tiny/1.2.3/runs/run_1/files/model.safetensors"
    )
    assert (
        store.run_doc_key("mhai-tiny", "1.2.3", "run_1", "evaluation.json")
        == "models/mhai-tiny/1.2.3/runs/run_1/docs/evaluation.json"
    )
    assert store.manifest_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/manifest.json"
    assert store.manifest_sig_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/manifest.sig"
    assert not hasattr(store, "signoff_key") and not hasattr(store, "signoff_sig_key")
    assert (
        store.eval_file_key("clinical_document", "1.0.0", "pages/p-001.png")
        == "eval/clinical_document/1.0.0/files/pages/p-001.png"
    )
    assert store.testset_key("clinical_document", "1.0.0").endswith("/1.0.0/testset.json")
    assert store.testset_sig_key("clinical_document", "1.0.0").endswith("/1.0.0/testset.sig")


@pytest.mark.parametrize(
    "build",
    [
        lambda: store.claim_key("../x", "1.0.0"),
        lambda: store.claim_key("mhai-tiny", "1.0"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "../r", "config.json"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "../config.json"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "model.bin"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "a/config.json"),
        lambda: store.run_doc_key("mhai-tiny", "1.0.0", "r", "notes.md"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "../../models/x"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a/./b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a//b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "/abs"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a?b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "/".join("a" * 9)),
        lambda: store.eval_file_key("Clinical", "1.0.0", "a"),
        lambda: store.testset_key("clinical_document", "x"),
    ],
)
def test_layout_refuses_bad_names(build: Any) -> None:
    assert _refused(build).reason == "invalid_name"


def test_keys_and_version_ids_checked_before_any_call() -> None:
    # Port 1 on loopback: nothing listens, so any call would be store_unavailable. A bad
    # key or version id must be refused before that.
    s = S3Store("http://127.0.0.1:1", "mhai-models", CREDS, max_attempts=1)
    for key in ("../x", "models/../eval/x", "models/a//b", "other/x", "models/a?b", "models/"):
        assert _refused(s.get, key, max_bytes=10).reason == "invalid_name"
        assert _refused(s.put_new, key, b"x").reason == "invalid_name"
        assert _refused(s.head, key).reason == "invalid_name"
        assert _refused(s.versions, key).reason == "invalid_name"
    for vid in ("a/b", "", "x?versionId=1", "a+b", 7):
        assert _refused(s.get, "models/a/1.0.0/claim.json", vid, max_bytes=10).reason == (
            "invalid_name"
        )
        assert _refused(s.head, "models/a/1.0.0/claim.json", vid).reason == "invalid_name"
        out = io.BytesIO()
        assert _refused(
            s.download, "models/a/1.0.0/claim.json", version_id=vid, out=out, max_bytes=10
        ).reason == ("invalid_name")
    for prefix in ("models", "../", "models/a", "eval//"):
        assert _refused(s.list, prefix).reason == "invalid_name"


def test_put_new_path_must_be_a_regular_file(tmp_path: Path) -> None:
    s = S3Store("http://127.0.0.1:1", "mhai-models", CREDS, max_attempts=1)
    target = tmp_path / "config.json"
    target.write_bytes(b"{}")
    link = tmp_path / "link.json"
    link.symlink_to(target)
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", link).reason == "missing_file"
    os.mkfifo(tmp_path / "fifo")
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", tmp_path / "fifo").reason == (
        "file_not_allowed"
    )
    hard = tmp_path / "hard.json"
    os.link(target, hard)
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", hard).reason == ("file_not_allowed")
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", "config.json").reason == (
        "file_not_allowed"
    )


def test_tls_required_for_remote_endpoint() -> None:
    for ok in (
        "https://store.example.org",
        "https://store.example.org:9000",
        "http://127.0.0.1:9000",
        "http://[::1]:9000",
        "http://localhost:9000",
        "https://127.0.0.1",
    ):
        assert store.check_endpoint(ok) == ok
        S3Store(ok, "mhai-models", CREDS)
    for plain in (
        "http://store.example.org",
        "http://minio:9000",
        "http://127.0.0.2:9000",
        "http://localhost.:9000",
        "http://10.0.0.5:9000",
        "http://localhost.example.org",
        "http://127.0.0.1.evil.com",
        "http://[::ffff:127.0.0.1]:9000",
        "http://[0:0:0:0:0:0:0:1]:9000",
    ):
        err = _refused(S3Store, plain, "mhai-models", CREDS)
        assert (err.reason, err.detail_code) == ("store_unavailable", "tls_required")
    for bad in (
        "ftp://store.example.org",
        "https://user:pw@store.example.org",
        "http://localhost@evil.example.org",
        "http://evil@localhost:9000",
        "https://store.example.org/bucket",
        "https://store.example.org?x=1",
        "https://store.example.org:99999",
        "store.example.org",
        "https://",
    ):
        err = _refused(S3Store, bad, "mhai-models", CREDS)
        assert err.reason == "store_unavailable"
    assert _refused(S3Store, "https://s.example.org", "Bad_Bucket", CREDS).reason == (
        "store_unavailable"
    )


def test_store_has_no_delete_method() -> None:
    source = STORE_PY.read_text()
    assert "delete_object" not in source
    assert "delete_objects" not in source
    assert "delete_bucket" not in source
    for cls in (store.S3Store, store.ObjectStore):
        assert not [a for a in dir(cls) if "delete" in a.lower() or "remove" in a.lower()]


def test_secrets_not_in_repr() -> None:
    assert "synthetic-secret" not in repr(CREDS)
    s = S3Store("https://store.example.org", "mhai-models", CREDS)
    assert repr(s) == "S3Store()"
    with pytest.raises(TypeError):
        CREDS.__reduce__()


def test_errors_unreachable_have_no_url_or_credentials(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    s = S3Store("http://127.0.0.1:1", "mhai-secret-bucket", CREDS, max_attempts=1)
    key = "models/mhai-secret-name/1.0.0/manifest.json"
    for call in (
        lambda: s.get(key, max_bytes=10),
        lambda: s.put_new(key, b"x"),
        lambda: s.head(key),
        lambda: s.versions(key),
        lambda: s.list("models/"),
    ):
        err = _refused(call)
        _assert_clean(err, ["127.0.0.1:1", "mhai-secret-bucket", "mhai-secret-name", CREDS])
        assert (err.reason, err.detail_code) == ("store_unavailable", "transport")
    _assert_logs_clean(caplog, ["127.0.0.1:1", "mhai-secret-bucket", "mhai-secret-name", CREDS])


def _secrets_of(items: list[Any]) -> list[str]:
    out: list[str] = []
    for item in items:
        if isinstance(item, Credentials):
            out += [item.access_key, item.secret_key]
        else:
            out.append(str(item))
    return out


def _assert_clean(err: Refused, forbidden: list[Any]) -> None:
    assert err.__cause__ is None and err.__context__ is None
    text = str(err) + repr(err) + repr(err.args) + str(err.detail_code)
    for value in _secrets_of(forbidden):
        assert value not in text


def _assert_logs_clean(caplog: pytest.LogCaptureFixture, forbidden: list[Any]) -> None:
    logged = "\n".join(r.getMessage() + str(r.args) + str(r.exc_text or "") for r in caplog.records)
    for value in _secrets_of(forbidden):
        assert value not in logged


class _Body:
    """A response body that serves ``data`` and counts what was read."""

    def __init__(self, data: bytes) -> None:
        self.data = data
        self.read_total = 0
        self.closed = False

    def read(self, n: int = -1) -> bytes:
        rest = self.data[self.read_total :]
        chunk = rest if n < 0 else rest[:n]
        self.read_total += len(chunk)
        return chunk

    def close(self) -> None:
        self.closed = True


class _Client:
    def __init__(self, body: _Body, length: int, sha_of: bytes) -> None:
        self.body, self.length, self.sha_of = body, length, sha_of
        self.calls: list[dict[str, Any]] = []

    def get_object(self, **kw: Any) -> dict[str, Any]:
        self.calls.append(kw)
        digest = hashlib.sha256(self.sha_of).digest()
        return {
            "Body": self.body,
            "ContentLength": self.length,
            "ChecksumSHA256": base64.b64encode(digest).decode("ascii"),
        }


def _fake_s3(client: _Client) -> S3Store:
    s = object.__new__(S3Store)
    object.__setattr__(s, "_client", client)
    object.__setattr__(s, "_bucket", "mhai-models-test")
    return s


def _download(
    data: bytes, length: int, max_bytes: int, sha_of: bytes | None = None, vid: str = "v-1"
) -> Any:
    body = _Body(data)
    client = _Client(body, length, data if sha_of is None else sha_of)
    out = io.BytesIO()
    key = store.manifest_key("fixture-model", "1.0.0")
    try:
        result: Any = _fake_s3(client).download(key, version_id=vid, out=out, max_bytes=max_bytes)
    except Refused as e:
        result = e
    return result, out.getvalue(), body, client


def test_download_names_the_version_and_stops_at_max_bytes() -> None:
    data = b"0123456789" * 3
    (n, sha), written, body, client = _download(data, len(data), len(data))
    assert (n, sha, written) == (len(data), hashlib.sha256(data).hexdigest(), data)
    assert client.calls[0]["VersionId"] == "v-1" and body.closed
    # The store announces more than allowed: refused before any byte is read.
    err, written, body, _ = _download(data, len(data), 10)
    assert err.reason == "too_large" and written == b"" and body.read_total == 0
    # The store announces an allowed size but sends more: reading stops one byte past the
    # cap, and no more than the cap reaches the file.
    err, written, body, _ = _download(data, 10, 10)
    assert err.reason == "too_large"
    assert len(written) <= 10 and body.read_total <= 11 and body.closed
    # A short body, and a body that isn't the stored checksum.
    err, *_ = _download(data[:5], 10, 10)
    assert err.reason == "size_mismatch"
    err, *_ = _download(data, len(data), len(data), sha_of=b"other bytes")
    assert err.reason == "hash_mismatch"
    with pytest.raises(TypeError):
        _fake_s3(client).download(  # type: ignore[call-arg]
            store.manifest_key("fixture-model", "1.0.0"), out=io.BytesIO(), max_bytes=1
        )


def test_null_version_reads_the_one_object() -> None:
    """A bucket without versioning: the recorded id is ``null`` and the read names no
    version (S3's "the one object"); the hash is still checked."""
    data = b"synthetic manifest"
    (n, sha), written, _, client = _download(data, len(data), 100, vid=store.NULL_VERSION)
    assert (n, sha, written) == (len(data), hashlib.sha256(data).hexdigest(), data)
    assert "VersionId" not in client.calls[0]
    err, *_ = _download(data, len(data), 100, sha_of=b"overwritten", vid=store.NULL_VERSION)
    assert err.reason == "hash_mismatch"
    # get with "null" and with no version ("latest") both name none; a real id is sent.
    for vid, sent in ((store.NULL_VERSION, None), (None, None), ("v-2", "v-2")):
        client = _Client(_Body(data), len(data), data)
        assert _fake_s3(client).get(store.manifest_key("m-x", "1.0.0"), vid, max_bytes=100) == data
        assert client.calls[0].get("VersionId") == sent
    # A store that returns no version id (no versioning) reports "null"; a malformed one
    # is refused.
    assert store._returned_version_id(None) == store.NULL_VERSION == "null"
    assert store._returned_version_id("abc-1") == "abc-1"
    assert _refused(store._returned_version_id, "a/b").reason == "store_unavailable"
    assert store._version_kw(None) == {} and store._version_kw("null") == {}
    assert store._version_kw("v-3") == {"VersionId": "v-3"}


def test_no_lock_or_retention_left() -> None:
    source = STORE_PY.read_text()
    for gone in ("ObjectLock", "Retention", "retention", "COMPLIANCE"):
        assert gone not in source.replace("Object lock and retention are not used", "")
    assert not hasattr(store.S3Store, "extend_retention")
    assert not hasattr(store.ObjectStore, "extend_retention")


def test_policies_load_and_deny_deletes() -> None:
    files = sorted(p.stem for p in POLICY_DIR.glob("*.json"))
    assert files == sorted(ROLES) == ["reader", "writer"]
    for role in ROLES:
        doc = json.loads((POLICY_DIR / f"{role}.json").read_text())
        assert doc["Version"] == "2012-10-17"
        statements = doc["Statement"]
        allowed: set[str] = set()
        for st in statements:
            assert set(st) <= {"Sid", "Effect", "Action", "Resource", "Condition"}
            assert st["Effect"] in ("Allow", "Deny")
            for r in st["Resource"]:
                assert r.startswith("arn:aws:s3:::@BUCKET@")
            if st["Effect"] == "Allow":
                # Nothing is ever allowed wholesale, and nothing that deletes.
                assert "s3:*" not in st["Action"]
                assert not [a for a in st["Action"] if "Delete" in a or "Bypass" in a]
                allowed |= set(st["Action"])
        denied = {a for st in statements if st["Effect"] == "Deny" for a in st["Action"]}
        assert {"s3:DeleteObject", "s3:DeleteObjectVersion"} <= denied
        assert {"s3:PutBucketVersioning", "s3:PutBucketPolicy", "s3:DeleteBucket"} <= denied
        assert not [a for a in allowed if "Retention" in a or "LegalHold" in a]
        assert ("s3:PutObject" in allowed) == (role == "writer")
    reader = json.loads((POLICY_DIR / "reader.json").read_text())
    assert any(
        st["Effect"] == "Deny" and "s3:PutObject" in st["Action"] for st in reader["Statement"]
    )


# ---------------------------------------------------------------- against MinIO


@pytest.mark.store
def test_init_writes_secrets_0600_and_never_echoes_them(minio_store: MinioStore) -> None:
    init = minio_store.init
    assert sorted(init.secrets) == sorted(ROLES)
    assert init.modes["."] == 0o700
    for role in ROLES:
        assert init.modes[role] == 0o600
        assert re.fullmatch(r"[0-9a-f]{40}", init.secrets[role])
    assert len(set(init.secrets.values())) == len(ROLES)
    for value in [*init.secrets.values(), minio_store.root.secret_key]:
        assert value not in init.logs
    # A re-run (compose restarted) keeps the passwords, and every user still signs in.
    again = minio_store.run_init()
    assert again.exit_code == 0
    assert dict(again.secrets) == dict(init.secrets)
    for value in [*again.secrets.values(), minio_store.root.secret_key]:
        assert value not in again.logs
    for role in ROLES:
        minio_store.store(role).list("models/")


@pytest.mark.store
def test_bucket_is_versioned(minio_store: MinioStore) -> None:
    raw = minio_store.client()
    assert raw.get_bucket_versioning(Bucket=minio_store.bucket)["Status"] == "Enabled"


@pytest.mark.store
def test_put_new_refuses_existing_key(minio_store: MinioStore) -> None:
    w = minio_store.store("writer")
    key = store.claim_key(_name(), "1.0.0")
    vid = w.put_new(key, b'{"claim":"first"}')
    err = _refused(w.put_new, key, b'{"claim":"second"}')
    assert err.reason == "version_exists"
    # Root too: the conditional create holds for everyone.
    assert _refused(minio_store.store().put_new, key, b"x").reason == "version_exists"
    assert w.versions(key) == [vid]
    assert w.get(key, max_bytes=100) == b'{"claim":"first"}'


@pytest.mark.store
def test_version_ids_returned_and_fetchable(minio_store: MinioStore, tmp_path: Path) -> None:
    w = minio_store.store("writer")
    name = _name()
    key = store.run_file_key(name, "1.0.0", "run_1", "config.json")
    vid = w.put_new(key, b'{"synthetic": 1}')
    assert UUID.fullmatch(vid)
    info = w.head(key, vid)
    assert info.version_id == vid and info.size == 16
    assert info.sha256 == hashlib.sha256(b'{"synthetic": 1}').hexdigest()
    assert w.get(key, vid, max_bytes=16) == b'{"synthetic": 1}'
    assert _refused(w.get, key, vid, max_bytes=15).reason == "too_large"
    out = io.BytesIO()
    assert w.download(key, version_id=vid, out=out, max_bytes=16) == (16, info.sha256)
    assert _refused(w.download, key, version_id=vid, out=io.BytesIO(), max_bytes=15).reason == (
        "too_large"
    )

    # Shadowing: an unconditional PUT by a holder of root makes a newer version. The one
    # we recorded is still there, unchanged, and is what we ask for.
    minio_store.client().put_object(Bucket=minio_store.bucket, Key=key, Body=b"shadow")
    versions = w.versions(key)
    assert len(versions) == 2 and versions[-1] == vid and versions[0] != vid
    assert w.get(key, vid, max_bytes=100) == b'{"synthetic": 1}'

    # A file is streamed, and listed under its prefix.
    weights = tmp_path / "model.safetensors"
    weights.write_bytes(os.urandom(3000))
    wkey = store.run_file_key(name, "1.0.0", "run_1", "model.safetensors")
    wvid = w.put_new(wkey, weights)
    assert w.get(wkey, wvid, max_bytes=3000) == weights.read_bytes()
    assert w.list(f"models/{name}/1.0.0/runs/run_1/files/") == sorted([key, wkey])

    missing = _refused(w.get, store.claim_key(name, "9.9.9"), max_bytes=10)
    assert missing.reason == "not_found"
    no_such_version = "6d2f9c1e-3b4a-4f5e-9a8b-7c6d5e4f3a2b"  # gitleaks:allow (made up)
    gone = _refused(w.get, key, no_such_version, max_bytes=10)
    assert gone.reason == "not_found"


@pytest.mark.store
def test_bucket_without_versioning(minio_store: MinioStore) -> None:
    """Versioning is optional: ids are "null", reads work, an overwrite never passes as the
    recorded bytes."""
    bucket = f"mhai-plain-{secrets.token_hex(4)}"
    raw = minio_store.client()
    raw.create_bucket(Bucket=bucket)
    assert raw.get_bucket_versioning(Bucket=bucket).get("Status") in (None, "")
    s = S3Store(minio_store.endpoint, bucket, minio_store.root, max_attempts=1)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "config.json")
    body = b'{"synthetic": "plain"}'
    sha = hashlib.sha256(body).hexdigest()
    vid = s.put_new(key, body)
    assert vid == store.NULL_VERSION
    assert _refused(s.put_new, key, b"again").reason == "version_exists"
    info = s.head(key, vid)
    assert (info.version_id, info.size, info.sha256) == (vid, len(body), sha)
    assert s.get(key, vid, max_bytes=100) == body
    out = io.BytesIO()
    assert s.download(key, version_id=vid, out=out, max_bytes=100) == (len(body), sha)
    assert out.getvalue() == body
    assert s.versions(key) == [store.NULL_VERSION]
    # Overwritten outside this code, with a checksum: the bytes read now are not the
    # recorded SHA-256, which every reader compares (the verifier's hash_mismatch).
    raw.put_object(Bucket=bucket, Key=key, Body=b"tampered", ChecksumAlgorithm="SHA256")
    n, got = s.download(key, version_id=vid, out=io.BytesIO(), max_bytes=100)
    assert (n, got) == (8, hashlib.sha256(b"tampered").hexdigest()) and got != sha
    assert s.head(key, vid).sha256 != sha
    # Without a checksum: the store can't vouch for it, so it is refused outright.
    raw.put_object(Bucket=bucket, Key=key, Body=b"tampered again")
    err = _refused(s.download, key, version_id=vid, out=io.BytesIO(), max_bytes=100)
    assert (err.reason, err.detail_code) == ("store_unavailable", "checksum")


@pytest.mark.store
def test_writer_reads_and_writes_models_and_eval(minio_store: MinioStore) -> None:
    w = minio_store.store("writer")
    name = _name()
    ids = _promote(minio_store, name)
    assert w.get(ids["manifest_key"], ids["manifest"], max_bytes=100) == b"synthetic manifest"
    suite_version = f"1.{secrets.randbelow(10**5)}.0"
    ekey = store.eval_file_key("clinical_document", suite_version, "pages/p-001.png")
    evid = w.put_new(ekey, b"synthetic page")
    assert w.get(ekey, evid, max_bytes=100) == b"synthetic page"
    assert ekey in w.list(f"eval/clinical_document/{suite_version}/")
    assert w.versions(ekey) == [evid]


@pytest.mark.store
def test_reader_cannot_write(minio_store: MinioStore) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    suite_version = f"2.{secrets.randbelow(10**5)}.0"
    ekey = store.eval_file_key("clinical_document", suite_version, "a.png")
    evid = minio_store.store("writer").put_new(ekey, b"synthetic page")
    r = minio_store.store("reader")
    assert r.get(ids["manifest_key"], ids["manifest"], max_bytes=100) == b"synthetic manifest"
    assert r.versions(ids["manifest_key"]) == [ids["manifest"]]
    assert store.manifest_key(name, "1.0.0") in r.list(f"models/{name}/1.0.0/")
    # The reader reads the locked split too (the promotion's evaluation, lock-check).
    assert r.get(ekey, evid, max_bytes=100) == b"synthetic page"
    for key in (
        store.claim_key(_name(), "1.0.0"),
        store.manifest_key(name, "2.0.0"),
        store.eval_file_key("clinical_document", "1.0.0", f"{_name()}.png"),
    ):
        _denied(r.put_new, key, b"x")
    raw = minio_store.client("reader")
    _raw_denied(raw.put_object, Bucket=minio_store.bucket, Key=ids["file_key"], Body=b"shadow")
    assert r.versions(ids["file_key"]) == [ids["file"]]


@pytest.mark.store
def test_nobody_can_delete_or_change_the_bucket(minio_store: MinioStore) -> None:
    ids = _promote(minio_store, _name())
    bucket = minio_store.bucket
    for role in ROLES:
        raw = minio_store.client(role)
        for kw in ({}, {"VersionId": ids["file"]}):
            code = _raw_denied(raw.delete_object, Bucket=bucket, Key=ids["file_key"], **kw)
            assert code == "AccessDenied", role
        resp = raw.delete_objects(
            Bucket=bucket, Delete={"Objects": [{"Key": ids["file_key"], "VersionId": ids["file"]}]}
        )
        assert resp.get("Errors") and not resp.get("Deleted"), role
        _raw_denied(
            raw.put_bucket_versioning,
            Bucket=bucket,
            VersioningConfiguration={"Status": "Suspended"},
        )
        _raw_denied(raw.put_bucket_policy, Bucket=bucket, Policy="{}")
        _raw_denied(raw.delete_bucket, Bucket=bucket)
    reader = minio_store.store("reader")
    assert reader.versions(ids["file_key"]) == [ids["file"]]
    assert reader.head(ids["manifest_key"]).version_id == ids["manifest"]
    raw = minio_store.client()
    assert raw.get_bucket_versioning(Bucket=bucket)["Status"] == "Enabled"


@pytest.mark.store
def test_listing_scoped_to_the_layout(minio_store: MinioStore) -> None:
    bucket = minio_store.bucket
    for role in ROLES:
        raw = minio_store.client(role)
        _raw_denied(raw.list_object_versions, Bucket=bucket)
        _raw_denied(raw.list_objects_v2, Bucket=bucket)
        _raw_denied(raw.list_objects_v2, Bucket=bucket, Prefix="other/")
        raw.list_objects_v2(Bucket=bucket, Prefix="models/")
        raw.list_object_versions(Bucket=bucket, Prefix="eval/")


@pytest.mark.store
def test_errors_have_no_url_or_credentials(
    minio_store: MinioStore, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    name = _name()
    ids = _promote(minio_store, name)
    host = minio_store.endpoint.split("://", 1)[1]
    forbidden: list[Any] = [host, minio_store.bucket, name, minio_store.root]
    forbidden += list(minio_store.users.values())
    wrong = Credentials("writer", "not-the-password-0123456789")
    bad = S3Store(minio_store.endpoint, minio_store.bucket, wrong, max_attempts=1)
    err = _refused(bad.get, ids["manifest_key"], max_bytes=100)
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")
    _assert_clean(err, [*forbidden, wrong])
    w = minio_store.store("writer")
    r = minio_store.store("reader")
    for call, reason in (
        (lambda: w.get(store.claim_key(name, "9.9.9"), max_bytes=10), "not_found"),
        (lambda: w.put_new(ids["claim_key"], b"x"), "version_exists"),
        (lambda: r.put_new(store.claim_key(_name(), "1.0.0"), b"x"), "store_unavailable"),
        (lambda: w.head(store.claim_key(name, "9.9.9")), "not_found"),
    ):
        err = _refused(call)
        assert err.reason == reason
        _assert_clean(err, forbidden)
    _assert_logs_clean(caplog, forbidden)


def test_sdk_loggers_clamped_at_construction() -> None:
    saved = {n: logging.getLogger(n).level for n in ("boto3", "botocore", "s3transfer", "urllib3")}
    try:
        for n in saved:
            logging.getLogger(n).setLevel(logging.DEBUG)
        logging.getLogger("urllib3").setLevel(logging.CRITICAL)  # stricter: left alone
        S3Store("https://store.example.org", "mhai-models", CREDS)
        assert logging.getLogger("boto3").level == logging.ERROR
        assert logging.getLogger("botocore").level == logging.ERROR
        assert logging.getLogger("s3transfer").level == logging.ERROR
        assert logging.getLogger("urllib3").level == logging.CRITICAL
        # Process-wide: a child logger inherits it.
        assert logging.getLogger("botocore.endpoint").getEffectiveLevel() == logging.ERROR
    finally:
        for n, level in saved.items():
            logging.getLogger(n).setLevel(level)


def test_no_proxy_for_loopback_only() -> None:
    local = S3Store("http://127.0.0.1:9000", "mhai-models", CREDS)
    remote = S3Store("https://store.example.org", "mhai-models", CREDS)
    assert local._client.meta.config.proxies == {}
    assert remote._client.meta.config.proxies is None  # the environment's, as usual


@pytest.mark.store
def test_loopback_ignores_proxy_environment(
    minio_store: MinioStore, monkeypatch: pytest.MonkeyPatch
) -> None:
    # A proxy that doesn't exist: a request sent through it would fail.
    for var in ("HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY"):
        monkeypatch.setenv(var, "http://127.0.0.1:9")
    for var in ("NO_PROXY", "no_proxy"):
        monkeypatch.delenv(var, raising=False)
    s = minio_store.store("writer")
    key = store.claim_key(_name(), "1.0.0")
    vid = s.put_new(key, b"no proxy")
    assert s.get(key, vid, max_bytes=100) == b"no proxy"


def _multipart(raw: Any, bucket: str, key: str, **complete: Any) -> Any:
    up = raw.create_multipart_upload(Bucket=bucket, Key=key, ChecksumAlgorithm="SHA256")
    part = raw.upload_part(
        Bucket=bucket,
        Key=key,
        UploadId=up["UploadId"],
        PartNumber=1,
        Body=b"multipart",
        ChecksumAlgorithm="SHA256",
    )
    return raw.complete_multipart_upload(
        Bucket=bucket,
        Key=key,
        UploadId=up["UploadId"],
        MultipartUpload={
            "Parts": [
                {"PartNumber": 1, "ETag": part["ETag"], "ChecksumSHA256": part["ChecksumSHA256"]}
            ]
        },
        **complete,
    )


@pytest.mark.store
def test_multipart_only_for_the_writer_and_never_past_if_none_match(
    minio_store: MinioStore,
) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    bucket = minio_store.bucket
    _raw_denied(
        minio_store.client("reader").create_multipart_upload,
        Bucket=bucket,
        Key=store.claim_key(_name(), "1.0.0"),
    )
    # The writer can use multipart, but a conditional completion on an existing key is
    # refused: multipart is no way round If-None-Match.
    raw = minio_store.client("writer")
    new_key = store.run_file_key(name, "1.0.0", "run_2", "model.safetensors")
    done = _multipart(raw, bucket, new_key, IfNoneMatch="*")  # gitleaks:allow (kwarg)
    assert done["VersionId"]
    claim = ids["claim_key"]
    code = _raw_denied(
        _multipart,
        raw=raw,
        bucket=bucket,
        key=claim,
        IfNoneMatch="*",  # gitleaks:allow (a keyword argument)
    )
    assert code == "PreconditionFailed"
    assert minio_store.store("reader").versions(ids["claim_key"]) == [ids["claim"]]
