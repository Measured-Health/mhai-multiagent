"""The model store: an S3 bucket with versioning and compliance-mode object lock.

**Layout** (one bucket, ``MODEL_STORE_BUCKET``)::

    models/<name>/<version>/claim.json                       reserved first (conditional create)
    models/<name>/<version>/runs/<run_id>/files/<path>       weights, config, tokenizer
    models/<name>/<version>/runs/<run_id>/docs/<document>    the six documents of the manifest
    models/<name>/<version>/manifest.sig                     signature (purpose manifest)
    models/<name>/<version>/manifest.json                    written last: the version exists once this does
    models/<name>/<version>/signoff.json, signoff.sig        written by the sign-off workflow only
    eval/<suite>/<set_version>/claim.json                    reserved first (conditional create)
    eval/<suite>/<set_version>/files/<path>, testset.json, testset.sig   the locked test split

Keys are only ever built by the layout functions below, from checked names; a key or a
version id that came from outside is checked against the same patterns before any call.

**Immutability.** :meth:`ObjectStore.put_new` is a conditional create
(``If-None-Match: *``): a key that already exists is ``version_exists`` (P7). Every put
sets ``ObjectLockMode=COMPLIANCE`` and ``ObjectLockRetainUntilDate`` and sends its own
SHA-256 (``ChecksumAlgorithm=SHA256`` with ``ChecksumSHA256``), so the store refuses a body
that changed on the way. Nothing here deletes or overwrites: there is no delete method,
and retention is only ever extended (compliance mode refuses shortening, even for the
store's root user).

**Shadowing.** In a versioned bucket a second, unconditional PUT to a key (by a holder of
write credentials outside this code) makes a newer version that object lock doesn't stop.
So readers never trust "latest": the manifest records each object's ``store_version_id``
and the verifier asks for that version (Task 22).

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
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any, BinaryIO, Protocol
from urllib.parse import urlsplit

from intelligence.modelstore import files, names
from intelligence.modelstore.manifest import DOCUMENTS
from intelligence.modelstore.reasons import Refused

LOOPBACK_HOSTS = frozenset({"127.0.0.1", "::1", "localhost"})
LOCK_MODE = "COMPLIANCE"
# The longest lock any call may set or reach, in days: MODEL_STORE_MAX_RETENTION_DAYS, the
# same cap the store's policies enforce (deploy/minio/policies). The design states no
# period; the compliance lead sets it (TODO.md). A compliance lock can never be undone, so
# an uncapped one could keep junk forever.
DEFAULT_MAX_RETENTION_DAYS = 3650
MAX_RETENTION_DAYS_ENV = "MODEL_STORE_MAX_RETENTION_DAYS"
_RETENTION_CEILING = 36500
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


def signoff_key(name: str, version: str) -> str:
    return f"{_version_dir(name, version)}/signoff.json"


def signoff_sig_key(name: str, version: str) -> str:
    return f"{_version_dir(name, version)}/signoff.sig"


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
    """One stored object version: its id, size, SHA-256 (lowercase hex) and lock."""

    version_id: str
    size: int
    sha256: str
    lock_mode: str | None
    retain_until: datetime | None


class ObjectStore(Protocol):
    """Write-once, versioned, locked object storage. There is no delete method."""

    def put_new(self, key: str, data: bytes | Path, retention_days: int) -> str: ...

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes: ...

    def versions(self, key: str) -> list[str]: ...

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo: ...

    def list(self, prefix: str) -> list[str]: ...

    def extend_retention(self, key: str, version_id: str, days: int) -> datetime: ...


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


def _check_days(days: object, cap: int) -> int:
    if not isinstance(days, int) or isinstance(days, bool) or not 1 <= days <= cap:
        raise Refused("not_allowed_here", detail_code="retention_days")
    return days


def retention_cap_from_env() -> int:
    """``MODEL_STORE_MAX_RETENTION_DAYS`` (default :data:`DEFAULT_MAX_RETENTION_DAYS`), a
    whole number of days from 1 to 36,500; anything else is ``not_allowed_here``."""
    raw = os.environ.get(MAX_RETENTION_DAYS_ENV)
    if raw is None:
        return DEFAULT_MAX_RETENTION_DAYS
    if re.fullmatch(r"[1-9][0-9]{0,4}", raw, re.ASCII) is None or int(raw) > _RETENTION_CEILING:
        raise Refused("not_allowed_here", detail_code="max_retention_days")
    return int(raw)


def _check_version_id(version_id: object) -> str:
    if not isinstance(version_id, str):
        raise Refused("invalid_name")
    return names.check_store_version_id(version_id)


def _returned_version_id(value: object) -> str:
    """A version id the store returned: it must be a real one, never missing or ``null``
    (an unversioned bucket)."""
    failed = False
    try:
        _check_version_id(value)
    except Refused:
        failed = True
    if failed:
        raise _unavailable("version_id")
    return str(value)


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

    __slots__ = ("_bucket", "_client", "_max_days")

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
        max_retention_days: int | None = None,
    ) -> None:
        url = check_endpoint(endpoint)
        cap = retention_cap_from_env() if max_retention_days is None else max_retention_days
        if not isinstance(cap, int) or isinstance(cap, bool) or not 1 <= cap <= _RETENTION_CEILING:
            raise Refused("not_allowed_here", detail_code="max_retention_days")
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
        self._max_days = cap

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

    def put_new(self, key: str, data: bytes | Path, retention_days: int) -> str:
        """Create ``key`` once, locked in compliance mode for ``retention_days``; return
        its version id. An existing key is ``version_exists`` (P7). A ``Path`` is read
        without following a link and streamed; if it changes between hashing and upload,
        the store refuses it (``hash_mismatch``)."""
        _check_key(key)
        days = _check_days(retention_days, self._max_days)
        until = datetime.now(UTC).replace(microsecond=0) + timedelta(days=days)
        if isinstance(data, (bytes, bytearray)):
            body = bytes(data)
            if len(body) > MAX_PUT_BYTES:
                raise Refused("too_large")
            return self._put(key, io.BytesIO(body), hashlib.sha256(body).digest(), until)
        if not isinstance(data, Path):
            raise Refused("file_not_allowed", detail_code="data")
        with _open_regular(data) as (f, size):
            if size > MAX_PUT_BYTES:
                raise Refused("too_large")
            digest, n = _sha256_stream(f)
            if n != size:
                raise Refused("size_mismatch")
            f.seek(0)
            return self._put(key, f, digest, until)

    def _put(self, key: str, body: BinaryIO, digest: bytes, until: datetime) -> str:
        checksum = base64.b64encode(digest).decode("ascii")
        resp = self._call(
            self._client.put_object,
            precondition="version_exists",
            Key=key,
            Body=body,
            IfNoneMatch="*",
            ObjectLockMode=LOCK_MODE,
            ObjectLockRetainUntilDate=until,
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
        kw: dict[str, Any] = {"Key": key, "ChecksumMode": "ENABLED"}
        if version_id is not None:
            kw["VersionId"] = _check_version_id(version_id)
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
        required. More than ``max_bytes`` (announced or read) is ``too_large`` and reading
        stops there; a short body is ``size_mismatch``; a body that isn't the SHA-256 the
        store keeps with it is ``hash_mismatch``. The caller compares the result with what
        the manifest recorded."""
        _check_key(key)
        vid = _check_version_id(version_id)
        if not isinstance(max_bytes, int) or isinstance(max_bytes, bool) or max_bytes < 0:
            raise Refused("too_large", detail_code="max_bytes")
        resp = self._call(self._client.get_object, Key=key, VersionId=vid, ChecksumMode="ENABLED")
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
        kw: dict[str, Any] = {"Key": key, "ChecksumMode": "ENABLED"}
        if version_id is not None:
            kw["VersionId"] = _check_version_id(version_id)
        resp = self._call(self._client.head_object, **kw)
        size = resp.get("ContentLength")
        if not isinstance(size, int) or size < 0:
            raise _unavailable("size")
        mode = resp.get("ObjectLockMode")
        until = resp.get("ObjectLockRetainUntilDate")
        return ObjectInfo(
            version_id=_returned_version_id(resp.get("VersionId")),
            size=size,
            sha256=_hex_from_b64(resp.get("ChecksumSHA256")),
            lock_mode=mode if isinstance(mode, str) else None,
            retain_until=until if isinstance(until, datetime) else None,
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

    def extend_retention(self, key: str, version_id: str, days: int) -> datetime:
        """Keep ``version_id`` of ``key`` locked until ``days`` from now. It only ever
        lengthens: a date not later than the current one is ``not_allowed_here`` and no
        call is made (compliance mode would refuse it anyway). ``days`` over this store's
        cap (``MODEL_STORE_MAX_RETENTION_DAYS``) is ``not_allowed_here`` too; the store's
        policies enforce the same cap. Returns the new date."""
        _check_key(key)
        vid = _check_version_id(version_id)
        n = _check_days(days, self._max_days)
        until = datetime.now(UTC).replace(microsecond=0) + timedelta(days=n)
        current = self._call(self._client.get_object_retention, Key=key, VersionId=vid)
        retention = current.get("Retention") or {}
        mode = retention.get("Mode")
        held = retention.get("RetainUntilDate")
        if mode != LOCK_MODE or not isinstance(held, datetime):
            raise _unavailable("not_locked")
        if until <= held:
            raise Refused("not_allowed_here", detail_code="retention_not_longer")
        self._call(
            self._client.put_object_retention,
            Key=key,
            VersionId=vid,
            Retention={"Mode": LOCK_MODE, "RetainUntilDate": until},
        )
        return until


__all__ = [
    "DEFAULT_MAX_RETENTION_DAYS",
    "LOCK_MODE",
    "LOOPBACK_HOSTS",
    "Credentials",
    "ObjectInfo",
    "ObjectStore",
    "S3Store",
    "check_bucket",
    "check_endpoint",
    "check_region",
    "retention_cap_from_env",
    "claim_key",
    "eval_file_key",
    "manifest_key",
    "manifest_sig_key",
    "run_doc_key",
    "run_file_key",
    "signoff_key",
    "signoff_sig_key",
    "testset_claim_key",
    "testset_key",
    "testset_sig_key",
]
