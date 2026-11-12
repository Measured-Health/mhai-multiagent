"""The model store: a plain S3-compatible bucket, versioned if the store offers it.

**Layout** (one bucket, ``MODEL_STORE_BUCKET``)::

    models/<name>/<version>/claim.json                       reserved first (conditional create)
    models/<name>/<version>/runs/<run_id>/files/<path>       weights, config, tokenizer
    models/<name>/<version>/runs/<run_id>/docs/<document>    the six documents of the manifest
    models/<name>/<version>/manifest.sig                     signature (purpose manifest)
    models/<name>/<version>/manifest.json                    written last: the version exists once this does
    eval/<suite>/<set_version>/claim.json                    reserved first (conditional create)
    eval/<suite>/<set_version>/files/<path>, testset.json, testset.sig   the locked test split

Keys are only ever built by the layout functions below, from checked names; a key or a
version id that came from outside is checked against the same patterns before any call.

**Write once.** :meth:`ObjectStore.put_new` is a conditional create
(``If-None-Match: *``): a key that already exists is ``version_exists`` (P7). Every put
sends its own SHA-256 (``ChecksumAlgorithm=SHA256`` with ``ChecksumSHA256``), so the store
refuses a body that changed on the way. Nothing here deletes or overwrites: there is no
delete method. Object lock and retention are not used (TODO.md, "Before scale").

**Versioning is optional.** In a versioned bucket every put returns a version id, and the
manifest records it (``store_version_id``); readers ask for that version. In a bucket
without versioning the store returns none, and :data:`NULL_VERSION` (``null``, S3's own
name for it) is recorded and read as "the one object at this key". Either way the bytes
are never trusted for their key: the lock pins the manifest's SHA-256, the manifest pins
every file's SHA-256 and size, and the verifier checks both (Task 22). A second,
unconditional PUT by a holder of write credentials outside this code (a newer version, or
an overwrite in an unversioned bucket) is therefore a hash mismatch, never a different
model.

``version_id=None`` ("latest") on :meth:`ObjectStore.get` and :meth:`ObjectStore.head` is
for tests and operators' checks only: promote, the test-split fetch and the load-time
verifier (:mod:`intelligence.modelstore.verify`) always name a version, and
:meth:`S3Store.download` has no "latest" at all (Task 16 M4).

**Transport and errors.** TLS is required unless the endpoint host is exactly
``127.0.0.1``, ``::1`` or ``localhost``. Every failure is a :class:`Refused` with a fixed
code (``store_unavailable``, ``not_found``, ``version_exists``, ``hash_mismatch``,
``too_large``) raised outside the ``except`` block, so neither its message nor its
exception chain carries a URL, key, bucket, signed URL or credential. The SDK's own
loggers are held at ``ERROR`` so their debug output (which has URLs and headers) never
reaches the logs.
"""

import base64
import hashlib
import io
import logging
import os
import re
import stat
from collections.abc import Callable, Iterator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, BinaryIO, Protocol
from urllib.parse import urlsplit

from intelligence.modelstore import files, names
from intelligence.modelstore.manifest import DOCUMENTS
from intelligence.modelstore.reasons import Refused

LOOPBACK_HOSTS = frozenset({"127.0.0.1", "::1", "localhost"})
# The version id of an object in a bucket without versioning (S3's own spelling).
NULL_VERSION = "null"
MAX_PUT_BYTES = 5 * 1024**3  # one S3 PUT; larger objects need multipart (not in phase 0)
MAX_KEY_CHARS = 1024
MAX_LIST = 10_000
MAX_EVAL_PATH_PARTS = 8
_BLOCK = 8 * 1024 * 1024
_CLOEXEC = getattr(os, "O_CLOEXEC", 0)

_SEGMENT = r"[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}"
_KEY = re.compile(rf"(?:models|eval)(?:/{_SEGMENT})+", re.ASCII)
_PREFIX = re.compile(rf"(?:models|eval)/(?:{_SEGMENT}/)*", re.ASCII)
_BUCKET = re.compile(r"[a-z0-9][a-z0-9-]{1,61}[a-z0-9]", re.ASCII)
_REGION = re.compile(r"[a-z]{2}(?:-[a-z0-9]+){1,4}", re.ASCII)
_SUITE = re.compile(r"[a-z][a-z0-9_]{0,62}", re.ASCII)
_SHA256_B64 = re.compile(r"[A-Za-z0-9+/]{43}=", re.ASCII)
_SDK_LOGGERS = ("boto3", "botocore", "s3transfer", "urllib3")

_NOT_FOUND = frozenset({"NoSuchKey", "NoSuchVersion", "NotFound", "404"})
_PRECONDITION = frozenset({"PreconditionFailed", "412"})
_DENIED = frozenset({"AccessDenied", "403", "InvalidAccessKeyId", "SignatureDoesNotMatch"})
_CHECKSUM = frozenset({"XAmzContentChecksumMismatch", "BadDigest", "InvalidDigest"})


def _unavailable(detail: str) -> Refused:
    return Refused("store_unavailable", detail_code=detail)


# ---------------------------------------------------------------- the layout


def _check_key(key: object) -> str:
    """A key in the layout's character set (no ``.``/``..`` segment, no empty segment,
    at most 1,024 characters), or ``invalid_name``."""
    if (
        not isinstance(key, str)
        or len(key) > MAX_KEY_CHARS
        or _KEY.fullmatch(key) is None
        or any(part in (".", "..") for part in key.split("/"))
    ):
        raise Refused("invalid_name")
    return key


def _check_prefix(prefix: object) -> str:
    if (
        not isinstance(prefix, str)
        or len(prefix) > MAX_KEY_CHARS
        or _PREFIX.fullmatch(prefix) is None
        or any(part in (".", "..") for part in prefix.split("/"))
    ):
        raise Refused("invalid_name")
    return prefix


def _version_dir(name: str, version: str) -> str:
    return f"models/{names.check_name(name)}/{names.check_version(version)}"


def _file_name(path: str) -> str:
    failed = False
    try:
        files.check_file_name(path)
    except Refused:
        failed = True
    if failed:
        raise Refused("invalid_name")
    return path


def claim_key(name: str, version: str) -> str:
    return f"{_version_dir(name, version)}/claim.json"


def run_file_key(name: str, version: str, run_id: str, path: str) -> str:
    """A run's weights, config or tokenizer file (an allow-listed file name)."""
    run = names.check_run_id(run_id)
    return f"{_version_dir(name, version)}/runs/{run}/files/{_file_name(path)}"


def run_doc_key(name: str, version: str, run_id: str, document: str) -> str:
    """One of the manifest's six documents."""
    if document not in DOCUMENTS:
        raise Refused("invalid_name")
    run = names.check_run_id(run_id)
    return f"{_version_dir(name, version)}/runs/{run}/docs/{document}"


def manifest_key(name: str, version: str) -> str:
    return f"{_version_dir(name, version)}/manifest.json"


def manifest_sig_key(name: str, version: str) -> str:
    return f"{_version_dir(name, version)}/manifest.sig"


def _eval_dir(suite: str, set_version: str) -> str:
    if not isinstance(suite, str) or _SUITE.fullmatch(suite) is None:
        raise Refused("invalid_name")
    return f"eval/{suite}/{names.check_version(set_version)}"


def eval_file_key(suite: str, set_version: str, path: str) -> str:
    """A file of the locked test split: a relative path of at most
    :data:`MAX_EVAL_PATH_PARTS` plain segments."""
    if not isinstance(path, str):
        raise Refused("invalid_name")
    parts = path.split("/")
    if len(parts) > MAX_EVAL_PATH_PARTS:
        raise Refused("invalid_name")
    return _check_key(f"{_eval_dir(suite, set_version)}/files/{path}")


def testset_claim_key(suite: str, set_version: str) -> str:
    """Reserved before a test split is built, so a second build of the same suite and
    version stops before it generates or uploads anything."""
    return f"{_eval_dir(suite, set_version)}/claim.json"


def testset_key(suite: str, set_version: str) -> str:
    return f"{_eval_dir(suite, set_version)}/testset.json"


def testset_sig_key(suite: str, set_version: str) -> str:
    return f"{_eval_dir(suite, set_version)}/testset.sig"


# ---------------------------------------------------------------- the protocol


@dataclass(frozen=True, slots=True)
class ObjectInfo:
    """One stored object version: its id (:data:`NULL_VERSION` without versioning), size
    and SHA-256 (lowercase hex)."""

    version_id: str
    size: int
    sha256: str


class ObjectStore(Protocol):
    """Write-once object storage. There is no delete method."""

    def put_new(self, key: str, data: bytes | Path) -> str: ...

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes: ...

    def versions(self, key: str) -> list[str]: ...

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo: ...

    def list(self, prefix: str) -> list[str]: ...


# ---------------------------------------------------------------- S3


@dataclass(frozen=True, slots=True)
class Credentials:
    """An access key and its secret. The secret is never in ``repr``."""

    access_key: str
    secret_key: str = field(repr=False)

    def __post_init__(self) -> None:
        for value in (self.access_key, self.secret_key):
            if not isinstance(value, str) or not value or not value.isascii():
                raise _unavailable("credentials")

    def __reduce__(self) -> Any:
        raise TypeError("Credentials cannot be pickled")


def check_endpoint(endpoint: str) -> str:
    """``https://host[:port]``, or ``http://`` only for an exact loopback host. Anything
    else (userinfo, a path, a query, another scheme, plain HTTP to another host) is
    ``store_unavailable``."""
    if not isinstance(endpoint, str) or not endpoint.isascii() or len(endpoint) > 512:
        raise _unavailable("endpoint")
    failed = False
    try:
        parts = urlsplit(endpoint)
        host = parts.hostname
        _ = parts.port
    except ValueError:
        failed = True
    if failed:
        raise _unavailable("endpoint")
    if (
        parts.scheme not in ("https", "http")
        or not host
        or parts.username is not None
        or parts.password is not None
        or parts.path not in ("", "/")
        or parts.query
        or parts.fragment
    ):
        raise _unavailable("endpoint")
    if parts.scheme == "http" and host not in LOOPBACK_HOSTS:
        raise _unavailable("tls_required")
    return f"{parts.scheme}://{parts.netloc}"


def check_bucket(bucket: object) -> str:
    """An S3 bucket name (lowercase, digits, inner hyphens; 3-63), else
    ``store_unavailable``."""
    if not isinstance(bucket, str) or _BUCKET.fullmatch(bucket) is None:
        raise _unavailable("bucket")
    return bucket


def check_region(region: object) -> str:
    """An S3 region name such as ``us-east-1``, else ``store_unavailable``."""
    if not isinstance(region, str) or _REGION.fullmatch(region) is None:
        raise _unavailable("region")
    return region


def _is_loopback(url: str) -> bool:
    return urlsplit(url).hostname in LOOPBACK_HOSTS


def _quiet_sdk_loggers() -> None:
    """Hold the SDK's loggers at ``ERROR``: their debug and warning output carries
    endpoints, keys and signed request details."""
    for name in _SDK_LOGGERS:
        logger = logging.getLogger(name)
        if logger.level < logging.ERROR:
            logger.setLevel(logging.ERROR)


def _check_version_id(version_id: object) -> str:
    if not isinstance(version_id, str):
        raise Refused("invalid_name")
    return names.check_store_version_id(version_id)


def _returned_version_id(value: object) -> str:
    """A version id the store returned; none at all (a bucket without versioning) is
    :data:`NULL_VERSION`."""
    if value is None:
        return NULL_VERSION
    failed = False
    try:
        _check_version_id(value)
    except Refused:
        failed = True
    if failed:
        raise _unavailable("version_id")
    return str(value)


def _version_kw(version_id: str | None) -> dict[str, str]:
    """The ``VersionId`` argument for a read: none for "latest" and for
    :data:`NULL_VERSION` (the one object of an unversioned bucket; its hash is checked by
    the caller like any other)."""
    if version_id is None:
        return {}
    vid = _check_version_id(version_id)
    return {} if vid == NULL_VERSION else {"VersionId": vid}


def _hex_from_b64(value: object) -> str:
    """The store's base64 SHA-256 as lowercase hex; missing or composite is refused."""
    if not isinstance(value, str) or _SHA256_B64.fullmatch(value) is None:
        raise _unavailable("checksum")
    return base64.b64decode(value).hex()


@contextmanager
def _open_regular(path: Path) -> Iterator[tuple[BinaryIO, int]]:
    """A regular, singly linked file opened without following a link, and its size."""
    fd = -1
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC)
    except OSError:
        pass
    if fd < 0:
        raise Refused("missing_file", detail_code="open")
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_nlink != 1:
            raise Refused("file_not_allowed", detail_code="not_regular")
        f = os.fdopen(os.dup(fd), "rb")
    finally:
        os.close(fd)
    with f:
        yield f, st.st_size


def _sha256_stream(f: BinaryIO) -> tuple[bytes, int]:
    h = hashlib.sha256()
    n = 0
    while block := f.read(_BLOCK):
        h.update(block)
        n += len(block)
    return h.digest(), n


class S3Store:
    """:class:`ObjectStore` over the S3 API (boto3): MinIO in tests and compose, the
    cloud store in production (Task 27)."""

    __slots__ = ("_bucket", "_client")

    def __init__(
        self,
        endpoint: str,
        bucket: str,
        credentials: Credentials,
        region: str = "us-east-1",
        *,
        connect_timeout: float = 10.0,
        read_timeout: float = 120.0,
        max_attempts: int = 5,
    ) -> None:
        url = check_endpoint(endpoint)
        check_bucket(bucket)
        check_region(region)
        if not isinstance(credentials, Credentials):
            raise _unavailable("credentials")
        # The SDK loggers are clamped here, when a store is built, and process-wide (the
        # four parent loggers; their children inherit the level). Pinned by
        # test_sdk_loggers_clamped_at_construction.
        _quiet_sdk_loggers()
        import boto3
        from botocore.config import Config

        config = Config(
            signature_version="s3v4",
            s3={"addressing_style": "path"},
            retries={"mode": "adaptive", "max_attempts": max_attempts},
            connect_timeout=connect_timeout,
            read_timeout=read_timeout,
            request_checksum_calculation="when_required",
            response_checksum_validation="when_supported",
            # A loopback endpoint never goes through a proxy: HTTP_PROXY would otherwise
            # carry its plain-HTTP, signed requests off the machine. For a remote (TLS)
            # endpoint botocore honours HTTPS_PROXY and NO_PROXY as usual; the proxy only
            # sees a CONNECT tunnel, and the certificate is still verified end to end.
            proxies={} if _is_loopback(url) else None,
        )
        failed = False
        try:
            session = boto3.session.Session()
            self._client: Any = session.client(
                "s3",
                endpoint_url=url,
                region_name=region,
                aws_access_key_id=credentials.access_key,
                aws_secret_access_key=credentials.secret_key,
                config=config,
                verify=True,
            )
        except Exception:
            failed = True
        if failed:
            raise _unavailable("client")
        self._bucket = bucket

    def __repr__(self) -> str:
        return "S3Store()"

    def __reduce__(self) -> Any:
        raise TypeError("S3Store cannot be pickled")

    # ------------------------------------------------------------ calls

    def _call(self, fn: Callable[..., Any], *, precondition: str | None = None, **kw: Any) -> Any:
        """One SDK call. Every failure becomes a fixed :class:`Refused` raised after the
        ``except`` block, so the SDK exception (which has the URL, bucket and key) is in
        neither the message nor the chain."""
        from botocore.exceptions import BotoCoreError, ClientError

        reason, detail = "store_unavailable", "error"
        try:
            return fn(Bucket=self._bucket, **kw)
        except ClientError as exc:
            code = str(exc.response.get("Error", {}).get("Code", ""))
            status = str(exc.response.get("ResponseMetadata", {}).get("HTTPStatusCode", ""))
            if code in _NOT_FOUND or status == "404":
                reason, detail = "not_found", "missing"
            elif precondition is not None and (code in _PRECONDITION or status == "412"):
                reason, detail = precondition, "exists"
            elif code in _DENIED or status == "403":
                reason, detail = "store_unavailable", "denied"
            elif code in _CHECKSUM:
                reason, detail = "hash_mismatch", "checksum"
            else:
                reason, detail = "store_unavailable", "error"
        except BotoCoreError:
            reason, detail = "store_unavailable", "transport"
        except (OSError, ValueError):
            reason, detail = "store_unavailable", "transport"
        raise Refused(reason, detail_code=detail)

    # ------------------------------------------------------------ the protocol

    def put_new(self, key: str, data: bytes | Path) -> str:
        """Create ``key`` once and return its version id (:data:`NULL_VERSION` in a bucket
        without versioning). An existing key is ``version_exists`` (P7). A ``Path`` is read
        without following a link and streamed; if it changes between hashing and upload,
        the store refuses it (``hash_mismatch``)."""
        _check_key(key)
        if isinstance(data, (bytes, bytearray)):
            body = bytes(data)
            if len(body) > MAX_PUT_BYTES:
                raise Refused("too_large")
            return self._put(key, io.BytesIO(body), hashlib.sha256(body).digest())
        if not isinstance(data, Path):
            raise Refused("file_not_allowed", detail_code="data")
        with _open_regular(data) as (f, size):
            if size > MAX_PUT_BYTES:
                raise Refused("too_large")
            digest, n = _sha256_stream(f)
            if n != size:
                raise Refused("size_mismatch")
            f.seek(0)
            return self._put(key, f, digest)

    def _put(self, key: str, body: BinaryIO, digest: bytes) -> str:
        checksum = base64.b64encode(digest).decode("ascii")
        resp = self._call(
            self._client.put_object,
            precondition="version_exists",
            Key=key,
            Body=body,
            IfNoneMatch="*",
            ChecksumAlgorithm="SHA256",
            ChecksumSHA256=checksum,
        )
        if resp.get("ChecksumSHA256") != checksum:
            raise Refused("hash_mismatch", detail_code="stored_checksum")
        return _returned_version_id(resp.get("VersionId"))

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        """The bytes of ``key`` (of ``version_id`` if given), at most ``max_bytes``
        (``too_large`` otherwise), checked against the SHA-256 stored with them."""
        _check_key(key)
        if not isinstance(max_bytes, int) or isinstance(max_bytes, bool) or max_bytes < 0:
            raise Refused("too_large", detail_code="max_bytes")
        kw: dict[str, Any] = {"Key": key, "ChecksumMode": "ENABLED", **_version_kw(version_id)}
        resp = self._call(self._client.get_object, **kw)
        stream = resp.get("Body")
        try:
            length = resp.get("ContentLength")
            if not isinstance(length, int) or length > max_bytes:
                raise Refused("too_large")
            expected = _hex_from_b64(resp.get("ChecksumSHA256"))
            data = self._read(stream, max_bytes + 1)
        finally:
            if stream is not None:
                stream.close()
        if len(data) > max_bytes:
            raise Refused("too_large")
        if len(data) != length:
            raise Refused("size_mismatch")
        if hashlib.sha256(data).hexdigest() != expected:
            raise Refused("hash_mismatch")
        return data

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]:
        """Stream exactly ``version_id`` of ``key`` into ``out`` in 8 MB blocks and return
        its size and SHA-256 (lowercase hex). There is no "latest": ``version_id`` is
        required (:data:`NULL_VERSION` in a bucket without versioning). More than ``max_bytes`` (announced or read) is ``too_large`` and reading
        stops there; a short body is ``size_mismatch``; a body that isn't the SHA-256 the
        store keeps with it is ``hash_mismatch``. The caller compares the result with what
        the manifest recorded."""
        _check_key(key)
        version = _version_kw(_check_version_id(version_id))
        if not isinstance(max_bytes, int) or isinstance(max_bytes, bool) or max_bytes < 0:
            raise Refused("too_large", detail_code="max_bytes")
        resp = self._call(self._client.get_object, Key=key, ChecksumMode="ENABLED", **version)
        stream = resp.get("Body")
        try:
            length = resp.get("ContentLength")
            if not isinstance(length, int) or length > max_bytes:
                raise Refused("too_large")
            expected = _hex_from_b64(resp.get("ChecksumSHA256"))
            digest = hashlib.sha256()
            n = 0
            while n <= max_bytes:
                block = self._read(stream, min(_BLOCK, max_bytes + 1 - n))
                if not block:
                    break
                n += len(block)
                if n > max_bytes:
                    raise Refused("too_large")
                digest.update(block)
                self._write(out, block)
        finally:
            if stream is not None:
                stream.close()
        if n != length:
            raise Refused("size_mismatch")
        sha = digest.hexdigest()
        if sha != expected:
            raise Refused("hash_mismatch")
        return n, sha

    @staticmethod
    def _write(out: BinaryIO, block: bytes) -> None:
        failed = False
        view = memoryview(block)
        try:
            while view:  # an unbuffered file may take part of a block
                n = out.write(view)
                if not n:
                    failed = True
                    break
                view = view[n:]
        except (OSError, ValueError):
            failed = True
        if failed:
            raise Refused("file_not_allowed", detail_code="write")

    def _read(self, stream: Any, limit: int) -> bytes:
        from botocore.exceptions import BotoCoreError

        failed = False
        try:
            out: bytes = stream.read(limit)
        except (BotoCoreError, OSError, ValueError):
            failed = True
        if failed:
            raise _unavailable("read")
        return out

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo:
        _check_key(key)
        kw: dict[str, Any] = {"Key": key, "ChecksumMode": "ENABLED", **_version_kw(version_id)}
        resp = self._call(self._client.head_object, **kw)
        size = resp.get("ContentLength")
        if not isinstance(size, int) or size < 0:
            raise _unavailable("size")
        return ObjectInfo(
            version_id=_returned_version_id(resp.get("VersionId")),
            size=size,
            sha256=_hex_from_b64(resp.get("ChecksumSHA256")),
        )

    def versions(self, key: str) -> list[str]:
        """Every stored version id of exactly ``key``, newest first."""
        _check_key(key)
        out: list[str] = []
        kw: dict[str, Any] = {"Prefix": key}
        while True:
            resp = self._call(self._client.list_object_versions, **kw)
            for v in resp.get("Versions") or []:
                if isinstance(v, Mapping) and v.get("Key") == key:
                    out.append(_returned_version_id(v.get("VersionId")))
                    if len(out) > MAX_LIST:
                        raise Refused("too_many_files")
            if not resp.get("IsTruncated"):
                return out
            kw = {
                "Prefix": key,
                "KeyMarker": resp.get("NextKeyMarker"),
                "VersionIdMarker": resp.get("NextVersionIdMarker"),
            }

    def list(self, prefix: str) -> list[str]:
        """The keys under ``prefix`` (which ends with ``/``), sorted; a key outside the
        layout's character set is ``store_unavailable``."""
        _check_prefix(prefix)
        out: list[str] = []
        kw: dict[str, Any] = {"Prefix": prefix}
        while True:
            resp = self._call(self._client.list_objects_v2, **kw)
            for obj in resp.get("Contents") or []:
                key = obj.get("Key") if isinstance(obj, Mapping) else None
                failed = False
                try:
                    _check_key(key)
                except Refused:
                    failed = True
                if failed:
                    raise _unavailable("unexpected_key")
                out.append(str(key))
                if len(out) > MAX_LIST:
                    raise Refused("too_many_files")
            token = resp.get("NextContinuationToken")
            if not resp.get("IsTruncated") or not isinstance(token, str):
                return sorted(out)
            kw = {"Prefix": prefix, "ContinuationToken": token}


__all__ = [
    "LOOPBACK_HOSTS",
    "NULL_VERSION",
    "Credentials",
    "ObjectInfo",
    "ObjectStore",
    "S3Store",
    "check_bucket",
    "check_endpoint",
    "check_region",
    "claim_key",
    "eval_file_key",
    "manifest_key",
    "manifest_sig_key",
    "run_doc_key",
    "run_file_key",
    "testset_claim_key",
    "testset_key",
    "testset_sig_key",
]
