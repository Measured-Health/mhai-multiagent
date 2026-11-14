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
