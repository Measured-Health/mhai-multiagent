"""The conditional multipart upload: files over one PUT's 5 GiB (TODO Task 27).

Write-once stays write-once (``If-None-Match: *`` on completion; a store without it refuses
the file), every part carries its SHA-256, the whole file's SHA-256 is checked before
completion, what the store kept is read back, and any failure aborts the upload.

The ``store`` tests run on MinIO (conftest.py) with a 5 MiB part size and threshold, so a
few MiB go through exactly the path a 5.6 GB tensor does. The one over 5 GiB itself runs
with ``MHAI_STORE_LARGE=1`` (a sparse file; MinIO stores 5 GiB), on the build server.
"""

from __future__ import annotations

import base64
import hashlib
import io
import os
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import SimpleNamespace
from typing import Any, BinaryIO

import pytest
from botocore.exceptions import ClientError, EndpointConnectionError

from intelligence.modelstore import cli, export, files, manifest, promote, store, verify
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import Credentials, S3Store
from tests.modelstore.conftest import MinioStore
from tests.modelstore.fakes.memory_store import MemoryStore

CREDS = Credentials("synthetic-access", "synthetic-secret-not-real")
NOWHERE = "http://127.0.0.1:9"  # nothing listens: a call would fail as unavailable
PART = 5 * 1024**2  # store.MIN_PART_BYTES
GIB = 1024**3


def _name() -> str:
    return f"t{os.urandom(6).hex()}"


def _refused(fn: Any, *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _file(tmp_path: Path, size: int, name: str = "model.safetensors") -> tuple[Path, str]:
    path = tmp_path / name
    data = os.urandom(size)
    path.write_bytes(data)
    return path, hashlib.sha256(data).hexdigest()


# ---------------------------------------------------------------- caps (no Docker)


def test_caps_agree() -> None:
    assert store.MAX_PUT_BYTES == 5 * GIB
    assert store.MAX_OBJECT_BYTES == 50 * GIB == files.Limits().max_file_bytes
    assert files.Limits().max_total_bytes == 200 * GIB  # the run cap is unchanged
    assert -(-store.MAX_OBJECT_BYTES // store.PART_BYTES) <= store.MAX_PARTS
    # The export step mirrors the store without importing it.
    assert export.MAX_STORE_FILE_BYTES == store.MAX_OBJECT_BYTES
    assert export.ONE_PUT_BYTES == store.MAX_PUT_BYTES
    assert store.NO_MULTIPART == promote.FILE_TOO_LARGE == export.STORE_CAP_DETAIL


def test_object_cap() -> None:
    assert store.object_cap(None) == store.MAX_OBJECT_BYTES
    assert store.object_cap(S3Store(NOWHERE, "mhai-models", CREDS)) == store.MAX_PUT_BYTES
    on = S3Store(NOWHERE, "mhai-models", CREDS, conditional_multipart=True)
    assert store.object_cap(on) == store.MAX_OBJECT_BYTES
    assert store.object_cap(MemoryStore()) == store.MAX_PUT_BYTES


@pytest.mark.parametrize(
    "kw",
    [
        {"part_bytes": PART - 1},
        {"part_bytes": 5 * GIB + 1},
        {"part_bytes": True},
        {"multipart_threshold": 5 * GIB + 1},
        {"multipart_threshold": -1},
        {"conditional_multipart": 1},
    ],
)
def test_multipart_settings_checked(kw: dict[str, Any]) -> None:
    err = _refused(S3Store, NOWHERE, "mhai-models", CREDS, **kw)
    assert (err.reason, err.detail_code) == ("store_unavailable", "multipart_settings")


def test_without_conditional_multipart_a_big_file_is_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    s = S3Store(NOWHERE, "mhai-models", CREDS)  # conditional_multipart off (the default)
    monkeypatch.setattr(store, "MAX_PUT_BYTES", 16)
    path, sha = _file(tmp_path, 32)
    err = _refused(s.put_new, store.claim_key(_name(), "1.0.0"), path, sha256=sha)
    # Refused before any call (nothing listens at NOWHERE): never an unconditional write.
    assert (err.reason, err.detail_code) == ("too_large", "file_too_large_for_store")


def test_over_the_multipart_bound_is_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    s = S3Store(NOWHERE, "mhai-models", CREDS, conditional_multipart=True)
    monkeypatch.setattr(store, "MAX_OBJECT_BYTES", 16)
    path, sha = _file(tmp_path, 32)
    assert _refused(s.put_new, store.claim_key(_name(), "1.0.0"), path, sha256=sha).reason == (
        "too_large"
    )


def test_multipart_needs_the_expected_sha256(tmp_path: Path) -> None:
    s = S3Store(NOWHERE, "mhai-models", CREDS, conditional_multipart=True, multipart_threshold=0)
    path, _ = _file(tmp_path, 32)
    key = store.claim_key(_name(), "1.0.0")
    for bad in (None, "ABC", "0" * 63, b"0" * 64):
        err = _refused(s.put_new, key, path, sha256=bad)
        assert (err.reason, err.detail_code) == ("hash_mismatch", "expected")


def test_single_put_checks_the_expected_sha256_first(tmp_path: Path) -> None:
    s = S3Store(NOWHERE, "mhai-models", CREDS)
    key = store.claim_key(_name(), "1.0.0")
    err = _refused(s.put_new, key, b"bytes", sha256=hashlib.sha256(b"other").hexdigest())
    assert (err.reason, err.detail_code) == ("hash_mismatch", "expected")
    path, _ = _file(tmp_path, 32)
    err = _refused(s.put_new, key, path, sha256=hashlib.sha256(b"other").hexdigest())
    assert (err.reason, err.detail_code) == ("hash_mismatch", "expected")


class _Body:
    def __init__(self, data: bytes) -> None:
        self.stream = io.BytesIO(data)

    def read(self, n: int = -1) -> bytes:
        return self.stream.read(n)

    def close(self) -> None:
        pass


def _download(data: bytes, **resp: Any) -> Any:
    class Client:
        def get_object(self, **kw: Any) -> dict[str, Any]:
            return {"Body": _Body(data), "ContentLength": len(data), **resp}

    s = object.__new__(S3Store)
    object.__setattr__(s, "_client", Client())
    object.__setattr__(s, "_bucket", "mhai-models-test")
    key = store.run_file_key("fixture-model", "1.0.0", "run_1", "model.safetensors")
    try:
        return s.download(key, version_id="v-1", out=io.BytesIO(), max_bytes=len(data))
    except Refused as e:
        return e


def test_download_hashes_a_multipart_object_in_full() -> None:
    data = b"synthetic weights" * 100
    want = (len(data), hashlib.sha256(data).hexdigest())
    composite = base64.b64encode(hashlib.sha256(b"parts").digest()).decode() + "-3"
    # The store keeps a checksum of the parts' checksums, or none and a multipart ETag:
    # the object is hashed all the same, and the caller compares that with the manifest.
    assert _download(data, ChecksumSHA256=composite) == want
    assert _download(data, ETag='"' + "a" * 32 + '-3"') == want
    # A single-PUT object still needs its stored SHA-256, and must match it.
    err = _download(data, ETag='"' + "a" * 32 + '"')
    assert (err.reason, err.detail_code) == ("store_unavailable", "checksum")
    other = base64.b64encode(hashlib.sha256(b"other").digest()).decode()
    assert _download(data, ChecksumSHA256=other).reason == "hash_mismatch"
    assert _download(data, ChecksumSHA256="not-a-checksum-3").detail_code == "checksum"


def test_head_still_refuses_a_composite_checksum() -> None:
    composite = base64.b64encode(hashlib.sha256(b"parts").digest()).decode() + "-2"

    class Client:
        def head_object(self, **kw: Any) -> dict[str, Any]:
            return {"ContentLength": 3, "ChecksumSHA256": composite, "VersionId": "v-1"}

    s = object.__new__(S3Store)
    object.__setattr__(s, "_client", Client())
    object.__setattr__(s, "_bucket", "mhai-models-test")
    err = _refused(s.head, store.manifest_key("fixture-model", "1.0.0"), "v-1")
    # The readers (lock-check, the verifier, the test split) skip such a version as one
    # the store cannot vouch for, as before.
    assert (err.reason, err.detail_code) == ("store_unavailable", "checksum")


class _Multipart(MemoryStore):
    """A store that keeps no whole SHA-256 (head refuses ``checksum``), as for a multipart
    object, and can serve other bytes than were put."""

    def __init__(self, serve: bytes | None = None) -> None:
        super().__init__()
        self.serve = serve

    def head(self, key: str, version_id: str | None = None) -> store.ObjectInfo:
        raise Refused("store_unavailable", detail_code="checksum")

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]:
        if self.serve is None:
            return super().download(key, version_id=version_id, out=out, max_bytes=max_bytes)
        out.write(self.serve)
        return len(self.serve), hashlib.sha256(self.serve).hexdigest()


def test_put_once_reads_a_multipart_object_back_in_full(tmp_path: Path) -> None:
    path, sha = _file(tmp_path, 64)
    size = path.stat().st_size
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    good = _Multipart()
    vid = promote.put_once(good, key, path, size, sha)
    assert ("download", key) in good.calls
    assert good.versions(key) == [vid]
    bad = _Multipart(serve=b"x" * size)  # the store holds other bytes than were sent
    err = _refused(promote.put_once, bad, key, path, size, sha)
    assert (err.reason, err.detail_code) == ("hash_mismatch", "stored")


def test_put_once_never_adopts_what_multipart_says_is_not_ours(tmp_path: Path) -> None:
    class NotOurs(MemoryStore):
        def put_new(self, key: str, data: bytes | Path, *, sha256: str | None = None) -> str:
            raise Refused("version_exists", detail_code="not_ours")

    path, sha = _file(tmp_path, 64)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    mem = NotOurs()
    mem.objects[key] = [("v-1", path.read_bytes())]  # the same bytes, someone else's object
    err = _refused(promote.put_once, mem, key, path, 64, sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")


def test_check_files_takes_the_stores_cap(tmp_path: Path) -> None:
    from intelligence.modelstore import fixtures

    run = tmp_path / "run"
    fixtures.make_run(run, "good", 0)
    biggest = max(p.stat().st_size for p in run.iterdir())
    promote.check_files(run, max_file_bytes=biggest)
    err = _refused(promote.check_files, run, max_file_bytes=biggest - 1)
    assert (err.reason, err.detail_code) == ("too_large", "file_too_large_for_store")


@pytest.mark.parametrize(("value", "on"), [("", False), ("0", False), ("1", True)])
def test_cli_multipart_setting(value: str, on: bool, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(cli.MULTIPART_ENV, value)
    assert cli._conditional_multipart() is on


@pytest.mark.parametrize("value", ["yes", "true", "on", " 1", "2"])
def test_cli_multipart_setting_typo_refused(value: str, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(cli.MULTIPART_ENV, value)
    err = _refused(cli._conditional_multipart)
    assert (err.reason, err.detail_code) == ("store_unavailable", "conditional_multipart")


def test_cli_only_the_writer_uploads_in_parts(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(cli.MULTIPART_ENV, "1")
    monkeypatch.setenv(cli.ENDPOINT_ENV, NOWHERE)
    monkeypatch.setenv(cli.BUCKET_ENV, "mhai-models")
    for role in ("WRITER", "READER"):
        monkeypatch.setenv(f"MODEL_STORE_{role}_ACCESS_KEY", "synthetic-access")
        monkeypatch.setenv(f"MODEL_STORE_{role}_SECRET_KEY", "synthetic-secret-not-real")
    writer = cli._s3(cli.WRITER_ACCESS_ENV, cli.WRITER_SECRET_ENV)
    reader = cli._s3(cli.READER_ACCESS_ENV, cli.READER_SECRET_ENV)
    assert writer is not None and writer.max_object_bytes == store.MAX_OBJECT_BYTES
    assert reader is not None and reader.max_object_bytes == store.MAX_PUT_BYTES


def test_cli_stale_age_bounds(monkeypatch: pytest.MonkeyPatch) -> None:
    for hours in ("0", "-1", "x", "99999999"):
        assert cli.main(["store-abort-stale-uploads", "--older-than-hours", hours]) == 2
    # Below the store's floor (6 hours) the store refuses too, before any call.
    s = S3Store(NOWHERE, "mhai-models", CREDS)
    err = _refused(s.abort_stale_uploads, 3600)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "stale_age")
    for bad in ("", "other/", "models/../x/", "models"):
        assert _refused(s.abort_stale_uploads, prefixes=(bad,)).reason == "invalid_name"


# ---------------------------------------------------------------- against MinIO


def _writer(m: MinioStore, **kw: Any) -> S3Store:
    """The writer with conditional multipart and 5 MiB parts above 5 MiB (a test override:
    the same path as a 5.6 GB file, in a few MiB)."""
    settings: dict[str, Any] = {
        "conditional_multipart": True,
        "part_bytes": PART,
        "multipart_threshold": PART,
    }
    settings.update(kw)
    return S3Store(m.endpoint, m.bucket, m.users["writer"], max_attempts=1, **settings)


def _uploads(m: MinioStore, key: str, *, exact: bool = True) -> list[Any]:
    resp = m.client("root").list_multipart_uploads(Bucket=m.bucket, Prefix=key)
    return [u for u in resp.get("Uploads") or [] if not exact or u.get("Key") == key]


def _absent(m: MinioStore, key: str) -> None:
    err = _refused(m.store("reader").head, key)
    assert err.reason == "not_found"
    assert m.store("root").versions(key) == []
    assert _uploads(m, key) == []


def _sha_of(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while block := f.read(8 * 1024 * 1024):
            h.update(block)
    return h.hexdigest()


@pytest.mark.store
def test_multipart_round_trip(minio_store: MinioStore, tmp_path: Path) -> None:
    path, sha = _file(tmp_path, 2 * PART + 12_345)  # three parts, the last short
    size = path.stat().st_size
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    vid = _writer(minio_store).put_new(key, path, sha256=sha)
    reader = minio_store.store("reader")
    assert reader.versions(key) == [vid]
    with open(os.devnull, "wb") as sink:
        assert reader.download(key, version_id=vid, out=sink, max_bytes=size) == (size, sha)
    assert _uploads(minio_store, key) == []
    # promote.put_once on the same path: the version is read back in full.
    key2 = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    vid2 = promote.put_once(_writer(minio_store), key2, path, size, sha)
    assert reader.versions(key2) == [vid2]


@pytest.mark.store
@pytest.mark.slow
@pytest.mark.skipif(
    os.environ.get("MHAI_STORE_LARGE") != "1", reason="5 GiB to MinIO: MHAI_STORE_LARGE=1"
)
def test_multipart_over_5_gib(minio_store: MinioStore, tmp_path: Path) -> None:
    # A sparse file (no disk on this side): one tensor over 5 GiB, as Gemma 4's per-layer
    # embedding is, with the production part size and threshold.
    path = tmp_path / "model.safetensors"
    size = 5 * GIB + 3 * 1024 * 1024 + 17
    with open(path, "wb") as f:
        f.truncate(size)
    with open(path, "r+b") as f:  # a few real bytes, so a misplaced part shows
        f.seek(5 * GIB + 1)
        f.write(b"synthetic tail")
    sha = _sha_of(path)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    writer = S3Store(
        minio_store.endpoint,
        minio_store.bucket,
        minio_store.users["writer"],
        max_attempts=1,
        conditional_multipart=True,
        read_timeout=600.0,
    )
    vid = promote.put_once(writer, key, path, size, sha)
    with open(os.devnull, "wb") as sink:
        got = minio_store.store("reader").download(key, version_id=vid, out=sink, max_bytes=size)
    assert got == (size, sha)
    assert _uploads(minio_store, key) == []


@pytest.mark.store
def test_existing_object_refused_before_any_part(minio_store: MinioStore, tmp_path: Path) -> None:
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    first = minio_store.store("writer").put_new(key, b"the first object")
    path, sha = _file(tmp_path, PART + 1)
    err = _refused(_writer(minio_store).put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")
    assert minio_store.store("root").versions(key) == [first]
    assert _uploads(minio_store, key) == []


@pytest.mark.store
def test_completion_over_an_existing_object_refused(
    minio_store: MinioStore, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # The key is taken after the early check (a race): the conditional completion is
    # what refuses it, the upload is aborted and the object is untouched.
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    first = minio_store.store("writer").put_new(key, b"the first object")
    monkeypatch.setattr(S3Store, "_check_absent", lambda self, key: None)
    path, sha = _file(tmp_path, PART + 1)
    err = _refused(_writer(minio_store).put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")
    root = minio_store.store("root")
    assert root.versions(key) == [first]
    assert root.get(key, first, max_bytes=100) == b"the first object"
    assert _uploads(minio_store, key) == []


def _tamper_part(s: S3Store, number: int) -> None:
    """Flip a byte of part ``number`` on the way, keeping the checksum sent for it."""
    real = s._client.upload_part

    def upload_part(**kw: Any) -> Any:
        if kw["PartNumber"] == number:
            body = bytearray(kw["Body"])
            body[len(body) // 2] ^= 0x01
            kw["Body"] = bytes(body)
        return real(**kw)

    s._client.upload_part = upload_part


@pytest.mark.store
def test_tampered_part_refused(minio_store: MinioStore, tmp_path: Path) -> None:
    path, sha = _file(tmp_path, 2 * PART + 1)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    s = _writer(minio_store)
    _tamper_part(s, 2)
    err = _refused(s.put_new, key, path, sha256=sha)
    assert err.reason == "hash_mismatch"
    _absent(minio_store, key)


@pytest.mark.store
def test_bytes_other_than_the_files_hash_never_complete(
    minio_store: MinioStore, tmp_path: Path
) -> None:
    # The file is not what the run's files hash says (changed after the check): every
    # part goes up intact, but the whole SHA-256 is wrong, so nothing is completed.
    path, _ = _file(tmp_path, PART + 1)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    err = _refused(
        _writer(minio_store).put_new, key, path, sha256=hashlib.sha256(b"other").hexdigest()
    )
    assert (err.reason, err.detail_code) == ("hash_mismatch", "expected")
    _absent(minio_store, key)


@pytest.mark.store
def test_failure_mid_upload_aborts_and_leaves_no_object(
    minio_store: MinioStore, tmp_path: Path
) -> None:
    path, sha = _file(tmp_path, 2 * PART + 1)
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    s = _writer(minio_store)
    real = s._client.upload_part

    def upload_part(**kw: Any) -> Any:
        if kw["PartNumber"] == 3:
            raise EndpointConnectionError(endpoint_url=minio_store.endpoint)
        return real(**kw)

    s._client.upload_part = upload_part
    err = _refused(s.put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("store_unavailable", "transport")
    assert err.__cause__ is None and err.__context__ is None
    _absent(minio_store, key)


@pytest.mark.store
def test_verify_on_load_catches_a_mismatch(minio_store: MinioStore, tmp_path: Path) -> None:
    path, sha = _file(tmp_path, PART + 99)
    size = path.stat().st_size
    name = _name()
    run_file = "model.safetensors"
    key = store.run_file_key(name, "1.0.0", "run_1", run_file)
    vid = _writer(minio_store).put_new(key, path, sha256=sha)
    m: Any = SimpleNamespace(name=name, version="1.0.0", run=SimpleNamespace(id="run_1"))
    reader = minio_store.store("reader")

    def load(rec: manifest.FileRecord, into: str) -> None:
        d = tmp_path / into
        d.mkdir()
        dfd = os.open(d, os.O_RDONLY | os.O_DIRECTORY)
        try:
            verify._download_one(reader, m, rec, dfd)
        finally:
            os.close(dfd)

    load(manifest.FileRecord(run_file, size, sha, vid), "good")
    assert _sha_of(tmp_path / "good" / run_file) == sha
    # A manifest whose SHA-256 is not the stored object's: refused, all bytes hashed.
    other = hashlib.sha256(b"other").hexdigest()
    err = _refused(load, manifest.FileRecord(run_file, size, other, vid), "bad")
    assert err.reason == "hash_mismatch"
    # Someone with the root credential overwrites the key unconditionally, in parts:
    # the new version is not the one the manifest names, and naming it is refused.
    raw = minio_store.client("root")
    up = raw.create_multipart_upload(Bucket=minio_store.bucket, Key=key)
    part = raw.upload_part(
        Bucket=minio_store.bucket,
        Key=key,
        UploadId=up["UploadId"],
        PartNumber=1,
        Body=b"y" * size,
    )
    done = raw.complete_multipart_upload(
        Bucket=minio_store.bucket,
        Key=key,
        UploadId=up["UploadId"],
        MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": part["ETag"]}]},
    )
    load(manifest.FileRecord(run_file, size, sha, vid), "still-good")
    err = _refused(load, manifest.FileRecord(run_file, size, sha, done["VersionId"]), "shadow")
    assert err.reason == "hash_mismatch"


@pytest.mark.store
def test_reader_cannot_abort_or_list_uploads(minio_store: MinioStore) -> None:
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    up = minio_store.client("writer").create_multipart_upload(Bucket=minio_store.bucket, Key=key)
    reader = minio_store.client("reader")
    for call, kw in (
        (reader.abort_multipart_upload, {"Key": key, "UploadId": up["UploadId"]}),
        (reader.list_multipart_uploads, {"Prefix": "models/"}),
    ):
        with pytest.raises(ClientError) as e:
            call(Bucket=minio_store.bucket, **kw)
        assert e.value.response["Error"]["Code"] == "AccessDenied"
    assert len(_uploads(minio_store, key)) == 1
    minio_store.client("writer").abort_multipart_upload(
        Bucket=minio_store.bucket, Key=key, UploadId=up["UploadId"]
    )


@pytest.mark.store
def test_stale_uploads_aborted_and_fresh_ones_kept(minio_store: MinioStore) -> None:
    key = store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")
    raw = minio_store.client("writer")
    raw.create_multipart_upload(Bucket=minio_store.bucket, Key=key)
    writer = minio_store.store("writer")
    # MinIO lists uploads for an exact key only (it expires stale ones itself), so the
    # test names the key; on AWS S3 the default prefixes models/ and eval/ find it.
    assert writer.abort_stale_uploads(prefixes=(key,)) == 0  # started just now: kept
    assert len(_uploads(minio_store, key)) == 1
    later = datetime.now(UTC) + timedelta(hours=25)
    assert writer.abort_stale_uploads(now=later, prefixes=(key,)) == 1
    assert _uploads(minio_store, key) == []
    assert minio_store.store("root").versions(key) == []  # no object was ever made


@pytest.mark.store
def test_probe_says_minio_honours_conditional_completion(minio_store: MinioStore) -> None:
    assert minio_store.store("writer").probe_conditional_multipart() is True
    assert minio_store.store("root").list("models/_store-probe/")
    assert _uploads(minio_store, "models/_store-probe/", exact=False) == []


# ---------------------------------------------------------------- the decisions, stubbed S3


def _client_error(code: str, status: int) -> ClientError:
    return ClientError(
        {"Error": {"Code": code}, "ResponseMetadata": {"HTTPStatusCode": status}}, "op"
    )


def _b64sha(data: bytes) -> str:
    return base64.b64encode(hashlib.sha256(data).digest()).decode("ascii")


class _S3:
    """An S3 client stub for the multipart calls: versioned objects, one upload at a time,
    and switches for each way a store can misbehave."""

    def __init__(self, *, sha_parts: bool = True) -> None:
        self.objects: dict[str, list[dict[str, Any]]] = {}
        self.sha_parts = sha_parts
        self.parts: dict[int, bytes] = {}
        self.calls: list[str] = []
        self.aborted: list[str] = []
        self.complete_kw: dict[str, Any] = {}
        self.upload_id = "up-1"
        # switches
        self.part_hook: Any = None  # called (number, kw) before a part is kept
        self.echo_wrong = False
        self.complete_error: ClientError | None = None
        self.store_then_412 = False  # completed, then the reply is a 412 (a retry)
        self.complete_checksum: str | None = None  # what complete reports, if set
        self.abort_error: ClientError | None = None
        self.serve: bytes | None = None  # other bytes than were kept
        self.head_size_delta = 0
        self.extra_version = False
        self.uploads_pages: list[dict[str, Any]] = []

    def _new(self, key: str, body: bytes, checksum: str | None, multipart: bool) -> str:
        vid = f"v{sum(len(v) for v in self.objects.values()) + 1}"
        self.objects.setdefault(key, []).append(
            {"vid": vid, "body": body, "checksum": checksum, "multipart": multipart}
        )
        return vid

    def _find(self, key: str, vid: str | None) -> dict[str, Any]:
        for o in reversed(self.objects.get(key, [])):
            if vid is None or o["vid"] == vid:
                return o
        raise _client_error("NoSuchKey", 404)

    def head_object(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("head")
        o = self._find(kw["Key"], kw.get("VersionId"))
        resp = {"ContentLength": len(o["body"]) + self.head_size_delta, "VersionId": o["vid"]}
        if o["checksum"] is not None:
            resp["ChecksumSHA256"] = o["checksum"]
        return resp

    def create_multipart_upload(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("create")
        self.parts = {}
        resp: dict[str, Any] = {"UploadId": self.upload_id}
        if self.sha_parts:
            resp["ChecksumAlgorithm"] = "SHA256"
        return resp

    def upload_part(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("part")
        if self.part_hook is not None:
            self.part_hook(kw["PartNumber"], kw)
        body = kw["Body"]
        if "ChecksumSHA256" in kw and kw["ChecksumSHA256"] != _b64sha(body):
            raise _client_error("BadDigest", 400)
        if "ContentMD5" in kw:
            md5 = base64.b64encode(hashlib.md5(body, usedforsecurity=False).digest())
            if kw["ContentMD5"] != md5.decode("ascii"):
                raise _client_error("BadDigest", 400)
        self.parts[kw["PartNumber"]] = body
        etag = hashlib.md5(body, usedforsecurity=False).hexdigest()
        resp: dict[str, Any] = {"ETag": f'"{etag}"'}
        if "ChecksumSHA256" in kw:
            resp["ChecksumSHA256"] = _b64sha(b"x") if self.echo_wrong else kw["ChecksumSHA256"]
        return resp

    def _composite(self) -> str | None:
        if not self.sha_parts:
            return None
        digests = [hashlib.sha256(self.parts[n]).digest() for n in sorted(self.parts)]
        return store._composite(digests)

    def complete_multipart_upload(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("complete")
        self.complete_kw = kw
        if self.complete_error is not None:
            raise self.complete_error
        key = kw["Key"]
        if kw.get("IfNoneMatch") == "*" and self.objects.get(key) and not self.store_then_412:
            raise _client_error("PreconditionFailed", 412)
        body = b"".join(self.parts[n] for n in sorted(self.parts))
        vid = self._new(key, body, self._composite(), True)
        if self.extra_version:
            self._new(key, body, self._composite(), True)
        if self.store_then_412:
            raise _client_error("PreconditionFailed", 412)
        resp: dict[str, Any] = {"VersionId": vid}
        checksum = self.complete_checksum or self._composite()
        if checksum is not None:
            resp["ChecksumSHA256"] = checksum
        return resp

    def abort_multipart_upload(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("abort")
        if self.abort_error is not None:
            raise self.abort_error
        self.aborted.append(kw["UploadId"])
        return {}

    def list_object_versions(self, **kw: Any) -> dict[str, Any]:
        found = [
            {"Key": k, "VersionId": o["vid"]}
            for k, objs in self.objects.items()
            if k.startswith(kw["Prefix"])
            for o in reversed(objs)
        ]
        return {"Versions": found, "IsTruncated": False}

    def get_object(self, **kw: Any) -> dict[str, Any]:
        o = self._find(kw["Key"], kw.get("VersionId"))
        body = o["body"] if self.serve is None else self.serve
        resp = {"Body": _Body(body), "ContentLength": len(body)}
        if o["multipart"]:
            resp["ETag"] = '"' + "a" * 32 + f'-{max(1, len(self.parts))}"'
            if o["checksum"] is not None:
                resp["ChecksumSHA256"] = o["checksum"]
        elif o["checksum"] is not None:
            resp["ChecksumSHA256"] = o["checksum"]
        return resp

    def put_object(self, **kw: Any) -> dict[str, Any]:
        key = kw["Key"]
        if kw.get("IfNoneMatch") == "*" and self.objects.get(key):
            raise _client_error("PreconditionFailed", 412)
        body = kw["Body"].read()
        vid = self._new(key, body, _b64sha(body), False)
        return {"VersionId": vid, "ChecksumSHA256": _b64sha(body)}

    def list_multipart_uploads(self, **kw: Any) -> dict[str, Any]:
        self.calls.append("list_uploads")
        return self.uploads_pages.pop(0) if self.uploads_pages else {"Uploads": []}


def _stubbed(client: _S3, **kw: Any) -> S3Store:
    settings: dict[str, Any] = {
        "conditional_multipart": True,
        "part_bytes": PART,
        "multipart_threshold": PART,
    }
    settings.update(kw)
    s = S3Store(NOWHERE, "mhai-models", CREDS, **settings)
    s._client = client
    return s


def _key() -> str:
    return store.run_file_key(_name(), "1.0.0", "run_1", "model.safetensors")


@pytest.fixture
def big(tmp_path: Path) -> tuple[Path, str]:
    return _file(tmp_path, 2 * PART + 7)  # three parts


def test_stub_sha_parts_complete_conditionally(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3()
    key = _key()
    vid = _stubbed(c).put_new(key, path, sha256=sha)
    assert c.calls[:2] == ["head", "create"] and c.calls.count("part") == 3
    assert c.complete_kw["IfNoneMatch"] == "*"
    assert all("ChecksumSHA256" in p for p in c.complete_kw["MultipartUpload"]["Parts"])
    assert c.aborted == [] and c.objects[key][0]["vid"] == vid
    assert c.objects[key][0]["body"] == path.read_bytes()


def test_stub_md5_fallback_reads_the_object_back(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3(sha_parts=False)
    key = _key()
    seen: list[dict[str, Any]] = []
    c.part_hook = lambda n, kw: seen.append(dict(kw))
    vid = _stubbed(c).put_new(key, path, sha256=sha)
    assert all("ContentMD5" in kw and "ChecksumSHA256" not in kw for kw in seen)
    assert c.objects[key][0]["vid"] == vid
    # The same, but the store holds other bytes: the read-back refuses them.
    c2 = _S3(sha_parts=False)
    c2.serve = b"y" * path.stat().st_size
    err = _refused(_stubbed(c2).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("hash_mismatch", "stored")


def test_stub_existing_key_refused_before_any_part(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3()
    key = _key()
    c._new(key, b"first", _b64sha(b"first"), False)
    err = _refused(_stubbed(c).put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")
    assert "create" not in c.calls


def test_stub_completion_refused_aborts(
    big: tuple[Path, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    path, sha = big
    c = _S3()
    key = _key()
    monkeypatch.setattr(S3Store, "_check_absent", lambda self, key: None)
    c._new(key, b"first", _b64sha(b"first"), False)
    err = _refused(_stubbed(c).put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")
    assert c.aborted == ["up-1"] and len(c.objects[key]) == 1


def test_stub_retried_completion_that_is_ours_is_adopted(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3()
    c.store_then_412 = True
    key = _key()
    vid = _stubbed(c).put_new(key, path, sha256=sha)
    assert vid == c.objects[key][0]["vid"]


@pytest.mark.parametrize("error", ["NoSuchUpload", "other"])
def test_stub_completion_failures(big: tuple[Path, str], error: str) -> None:
    path, sha = big
    c = _S3()
    c.complete_error = (
        _client_error("NoSuchUpload", 404) if error == "NoSuchUpload" else _client_error("X", 500)
    )
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    if error == "NoSuchUpload":
        assert (err.reason, err.detail_code) == ("version_exists", "not_ours")
    else:
        assert (err.reason, err.detail_code) == ("store_unavailable", "error")
    assert c.aborted == ["up-1"]


def test_stub_md5_mode_never_adopts_on_412(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3(sha_parts=False)
    c.store_then_412 = True
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("version_exists", "not_ours")


def test_stub_part_checksum_echo_and_tamper(big: tuple[Path, str]) -> None:
    path, sha = big
    c = _S3()
    c.echo_wrong = True
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("hash_mismatch", "part_checksum")
    assert c.aborted == ["up-1"] and "complete" not in c.calls

    def flip(n: int, kw: dict[str, Any]) -> None:
        if n == 2:
            kw["Body"] = b"z" + kw["Body"][1:]

    c2 = _S3()
    c2.part_hook = flip
    err = _refused(_stubbed(c2).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("hash_mismatch", "checksum")
    assert c2.aborted == ["up-1"] and "complete" not in c2.calls


def test_stub_part_transport_error_aborts(
    big: tuple[Path, str], caplog: pytest.LogCaptureFixture
) -> None:
    path, sha = big
    c = _S3()

    def boom(n: int, kw: dict[str, Any]) -> None:
        if n == 2:
            raise EndpointConnectionError(endpoint_url=NOWHERE)

    c.part_hook = boom
    c.abort_error = _client_error("AccessDenied", 403)  # the abort fails too
    key = _key()
    err = _refused(_stubbed(c).put_new, key, path, sha256=sha)
    assert (err.reason, err.detail_code) == ("store_unavailable", "transport")
    assert c.calls[-1] == "abort" and key not in c.objects
    assert "abort failed" in caplog.text and key not in caplog.text


def test_stub_whole_sha256_checked_before_completion(big: tuple[Path, str]) -> None:
    path, _ = big
    c = _S3()
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=hashlib.sha256(b"o").hexdigest())
    assert (err.reason, err.detail_code) == ("hash_mismatch", "expected")
    assert "complete" not in c.calls and c.aborted == ["up-1"]


@pytest.mark.parametrize("change", ["shrink", "grow"])
def test_stub_file_changed_while_sent(big: tuple[Path, str], change: str) -> None:
    path, sha = big
    c = _S3()

    def edit(n: int, kw: dict[str, Any]) -> None:
        if change == "shrink" and n == 1:
            os.truncate(path, PART + 1)
        if change == "grow" and n == 3:
            with open(path, "ab") as f:
                f.write(b"more")

    c.part_hook = edit
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    assert err.reason == "size_mismatch"
    assert "complete" not in c.calls and c.aborted == ["up-1"]


@pytest.mark.parametrize("fault", ["size", "versions", "complete_checksum"])
def test_stub_what_was_stored_is_checked(big: tuple[Path, str], fault: str) -> None:
    path, sha = big
    c = _S3()
    if fault == "size":
        c.head_size_delta = 1
    elif fault == "versions":
        c.extra_version = True
    else:
        c.complete_checksum = _b64sha(b"other") + "-3"
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    want = {
        "size": ("hash_mismatch", "stored"),
        "versions": ("version_exists", "not_ours"),
        "complete_checksum": ("hash_mismatch", "stored_checksum"),
    }[fault]
    assert (err.reason, err.detail_code) == want


def test_stub_parts_and_ids_bounded(big: tuple[Path, str], monkeypatch: pytest.MonkeyPatch) -> None:
    path, sha = big
    c = _S3()
    monkeypatch.setattr(store, "MAX_PARTS", 2)
    err = _refused(_stubbed(c).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("too_large", "parts")
    assert "create" not in c.calls
    monkeypatch.setattr(store, "MAX_PARTS", 10_000)
    c2 = _S3()
    c2.upload_id = "bad id\n"
    err = _refused(_stubbed(c2).put_new, _key(), path, sha256=sha)
    assert (err.reason, err.detail_code) == ("store_unavailable", "upload_id")


def test_stub_small_file_takes_one_conditional_put(tmp_path: Path) -> None:
    path, sha = _file(tmp_path, 100)
    c = _S3()
    key = _key()
    _stubbed(c).put_new(key, path, sha256=sha)
    assert "create" not in c.calls and not c.objects[key][0]["multipart"]


@pytest.mark.parametrize("outcome", ["honoured", "ignored", "error"])
def test_stub_probe(outcome: str) -> None:
    c = _S3()
    if outcome == "ignored":
        c.store_then_412 = False
        real = c.complete_multipart_upload

        def ignore(**kw: Any) -> dict[str, Any]:
            kw.pop("IfNoneMatch")  # a store that drops the condition
            return real(**kw)

        c.complete_multipart_upload = ignore  # type: ignore[method-assign]
    if outcome == "error":
        c.complete_error = _client_error("NotImplemented", 501)
    s = _stubbed(c, conditional_multipart=False)
    if outcome == "error":
        assert _refused(s.probe_conditional_multipart).reason == "store_unavailable"
    else:
        assert s.probe_conditional_multipart() is (outcome == "honoured")
    assert c.calls[-1] == "abort"
    assert all(k.startswith("models/_store-probe/") for k in c.objects)


def test_stub_abort_stale_uploads(monkeypatch: pytest.MonkeyPatch) -> None:
    now = datetime.now(UTC)
    old, fresh = now - timedelta(days=2), now - timedelta(hours=1)
    k1, k2, k3 = _key(), _key(), _key()
    c = _S3()
    c.uploads_pages = [
        {
            "Uploads": [
                {"Key": k1, "UploadId": "u1", "Initiated": old},
                {"Key": k2, "UploadId": "u2", "Initiated": fresh},
                {"Key": "models/../x", "UploadId": "u3", "Initiated": old},
                {"Key": k3, "UploadId": "bad id\n", "Initiated": old},
                {"Key": k3, "UploadId": "u4", "Initiated": "yesterday"},
                "not a mapping",
            ],
            "IsTruncated": True,
            "NextKeyMarker": k1,
            "NextUploadIdMarker": "u1",
        },
        {"Uploads": [{"Key": k3, "UploadId": "u5", "Initiated": old}], "IsTruncated": False},
        {"Uploads": [], "IsTruncated": False},  # eval/
    ]
    s = _stubbed(c)
    assert s.abort_stale_uploads(now=now) == 2
    assert c.aborted == ["u1", "u5"]
    # One finished meanwhile (NoSuchUpload): skipped; any other failure stops the run.
    c.uploads_pages = [{"Uploads": [{"Key": k1, "UploadId": "u6", "Initiated": old}]}]
    c.abort_error = _client_error("NoSuchUpload", 404)
    assert s.abort_stale_uploads(now=now, prefixes=("models/",)) == 0
    c.uploads_pages = [{"Uploads": [{"Key": k1, "UploadId": "u7", "Initiated": old}]}]
    c.abort_error = _client_error("AccessDenied", 403)
    assert _refused(s.abort_stale_uploads, now=now, prefixes=("models/",)).reason == (
        "store_unavailable"
    )
    monkeypatch.setattr(store, "MAX_LIST", 1)
    c.abort_error = None
    c.uploads_pages = [{"Uploads": [{"Key": k1, "UploadId": "u8", "Initiated": fresh}] * 2}]
    assert _refused(s.abort_stale_uploads, now=now).reason == "too_many_files"


def test_stub_put_once_head_failure_propagates(tmp_path: Path) -> None:
    class Denied(MemoryStore):
        def head(self, key: str, version_id: str | None = None) -> store.ObjectInfo:
            raise Refused("store_unavailable", detail_code="denied")

    path, sha = _file(tmp_path, 64)
    err = _refused(promote.put_once, Denied(), _key(), path, 64, sha)
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")


class _FakeWriter:
    def __init__(self, honoured: bool = True) -> None:
        self.honoured = honoured
        self.ages: list[int] = []

    def probe_conditional_multipart(self) -> bool:
        return self.honoured

    def abort_stale_uploads(self, older_than_s: int) -> int:
        self.ages.append(older_than_s)
        return 3


def test_cli_store_commands(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    w = _FakeWriter()
    monkeypatch.setattr(cli, "_writer", lambda: w)
    assert cli.main(["store-probe-multipart"]) == 0
    assert '"conditional_multipart":true' in capsys.readouterr().out
    w.honoured = False
    assert cli.main(["store-probe-multipart"]) == 2
    assert '"conditional_multipart":false' in capsys.readouterr().out
    assert cli.main(["store-abort-stale-uploads"]) == 0
    assert cli.main(["store-abort-stale-uploads", "--older-than-hours", "48"]) == 0
    assert w.ages == [24 * 3600, 48 * 3600]
    assert '"stale_uploads_aborted":3' in capsys.readouterr().out


def test_cli_store_commands_need_the_writer(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    for name in (cli.WRITER_ACCESS_ENV, cli.WRITER_SECRET_ENV):
        monkeypatch.delenv(name, raising=False)
    assert cli.main(["store-probe-multipart"]) == 2
    out = capsys.readouterr().out
    assert '"refused":"not_allowed_here"' in out and "writer_credentials" in out
