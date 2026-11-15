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

**Files over 5 GiB** (one PUT's limit; Gemma 4's per-layer embedding alone is 5.6 GB) go
up as a multipart upload, and only when the store was built with ``conditional_multipart``
(``MODEL_STORE_CONDITIONAL_MULTIPART=1``, set once ``mhai-models store-probe-multipart``
has shown that the store honours ``If-None-Match`` on completion; MinIO and AWS S3 do).
Without it such a file is ``too_large`` (``file_too_large_for_store``), never an
unconditional write. :meth:`S3Store.put_new` then:

1. refuses a key that already exists before sending anything;
2. sends each part with its SHA-256 (``ChecksumSHA256``), or with ``Content-MD5`` if the
   store did not take SHA-256 for the upload, so the store refuses a part changed on the
   way;
3. hashes the bytes as it sends them: the whole file's SHA-256 must be the one the caller
   checked (the run's files hash) before anything is completed;
4. completes with ``If-None-Match: *``, so an object that appeared meanwhile is refused;
5. reads back what the store kept: its size and its checksum of the parts' SHA-256s
   (compared with the one computed from the parts sent) or, when it keeps none, the whole
   object again; and the key must have exactly that one version.

Any failure before completion aborts the upload (``AbortMultipartUpload``). An upload a
killed job leaves behind is aborted by ``mhai-models store-abort-stale-uploads`` or by the
bucket's lifecycle rule (deploy/model-store/README.md). The store keeps no SHA-256 of a
multipart object as a whole: :meth:`S3Store.head` refuses it (``checksum``, as for any
object without one), and :meth:`S3Store.download` hashes the full object, which every
caller compares with the manifest (or verifies as a signature).

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
import secrets
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
# The version id of an object in a bucket without versioning (S3's own spelling).
NULL_VERSION = "null"
MAX_PUT_BYTES = 5 * 1024**3  # one S3 PUT; larger objects need a conditional multipart upload
MAX_OBJECT_BYTES = 50 * 1024**3  # a multipart upload (files.Limits.max_file_bytes)
PART_BYTES = 64 * 1024**2  # 50 GiB is 800 parts
MIN_PART_BYTES = 5 * 1024**2  # S3's floor for every part but the last
MAX_PARTS = 10_000  # S3's ceiling
MIN_STALE_AGE_S = 6 * 3600  # an upload younger than this may still be running
DEFAULT_STALE_AGE_S = 24 * 3600
NO_MULTIPART = "file_too_large_for_store"  # promote.FILE_TOO_LARGE
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
_SHA256_HEX = re.compile(r"[0-9a-f]{64}", re.ASCII)
# The store's checksum of a multipart object: base64 SHA-256 of its parts' SHA-256s, "-N".
_COMPOSITE = re.compile(r"[A-Za-z0-9+/]{43}=-[1-9][0-9]{0,4}", re.ASCII)
_MULTIPART_ETAG = re.compile(r'"?[0-9a-fA-F]{32}-[1-9][0-9]{0,4}"?', re.ASCII)
_ETAG = re.compile(r'"?[A-Za-z0-9-]{1,128}"?', re.ASCII)
_UPLOAD_ID = re.compile(r"[A-Za-z0-9._~+/=-]{1,1024}", re.ASCII)
_SDK_LOGGERS = ("boto3", "botocore", "s3transfer", "urllib3")

_NOT_FOUND = frozenset({"NoSuchKey", "NoSuchVersion", "NotFound", "404"})
_PRECONDITION = frozenset({"PreconditionFailed", "412"})
_DENIED = frozenset({"AccessDenied", "403", "InvalidAccessKeyId", "SignatureDoesNotMatch"})
_CHECKSUM = frozenset({"XAmzContentChecksumMismatch", "BadDigest", "InvalidDigest"})


log = logging.getLogger(__name__)


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

    def put_new(self, key: str, data: bytes | Path, *, sha256: str | None = None) -> str: ...

    def download(
        self, key: str, *, version_id: str, out: BinaryIO, max_bytes: int
    ) -> tuple[int, str]: ...

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


def _whole_sha256(resp: Mapping[str, Any]) -> str | None:
    """The SHA-256 the store keeps of the whole object (lowercase hex), or None for a
    multipart object, of which it keeps none (it keeps a checksum of the parts'
    checksums, or nothing and a multipart ETag). Anything else without a SHA-256 is
    refused (``checksum``), as before."""
    value = resp.get("ChecksumSHA256")
    if isinstance(value, str) and _COMPOSITE.fullmatch(value) is not None:
        return None
    etag = resp.get("ETag")
    if value is None and isinstance(etag, str) and _MULTIPART_ETAG.fullmatch(etag) is not None:
        return None
    return _hex_from_b64(value)


def _composite(digests: list[bytes]) -> str:
    """What the store reports for a multipart upload with SHA-256 part checksums."""
    whole = hashlib.sha256(b"".join(digests)).digest()
    return f"{base64.b64encode(whole).decode('ascii')}-{len(digests)}"


def _check_sha256_hex(value: object) -> str:
    if not isinstance(value, str) or _SHA256_HEX.fullmatch(value) is None:
        raise Refused("hash_mismatch", detail_code="expected")
    return value


def _returned(value: object, pattern: re.Pattern[str], detail: str) -> str:
    """A string the store returned (an upload id, an ETag), checked before it is sent
    back in another call."""
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise _unavailable(detail)
    return value


def _stale_upload(up: object, cutoff: datetime) -> tuple[str, str] | None:
    """The key and upload id of a listed upload started before ``cutoff``, or None (a
    newer one, or one whose key or id is not ours to send back)."""
    if not isinstance(up, Mapping):
        return None
    started = up.get("Initiated")
    if not isinstance(started, datetime) or started.tzinfo is None or started >= cutoff:
        return None
    bad = False
    try:
        key = _check_key(up.get("Key"))
        upload_id = _returned(up.get("UploadId"), _UPLOAD_ID, "upload_id")
    except Refused:
        bad = True
    if bad:
        return None
    return key, upload_id


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


def _read_exact(f: BinaryIO, n: int) -> bytes:
    """``n`` bytes of ``f``; fewer (the file shrank) is ``size_mismatch``."""
    chunks: list[bytes] = []
    left = n
    failed = False
    try:
        while left:
            block = f.read(left)
            if not block:
                break
            chunks.append(block)
            left -= len(block)
    except OSError:
        failed = True
    if failed:
        raise Refused("file_not_allowed", detail_code="read")
    if left:
        raise Refused("size_mismatch")
    return b"".join(chunks)


def _has_more(f: BinaryIO) -> bool:
    """True when ``f`` still has bytes (the file grew while it was being sent)."""
    failed = False
    try:
        more = f.read(1)
    except OSError:
        failed = True
    if failed:
        raise Refused("file_not_allowed", detail_code="read")
    return bool(more)


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

    __slots__ = ("_bucket", "_client", "_multipart", "_part_bytes", "_threshold")

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
        conditional_multipart: bool = False,
        part_bytes: int = PART_BYTES,
        multipart_threshold: int | None = None,
    ) -> None:
        """``conditional_multipart``: the store honours ``If-None-Match`` on
        ``CompleteMultipartUpload`` (``store-probe-multipart``), so a file over
        ``multipart_threshold`` (one PUT's 5 GiB unless a test lowers it) goes up in parts
        of ``part_bytes``. Without it such a file is refused."""
        url = check_endpoint(endpoint)
        check_bucket(bucket)
        check_region(region)
        if not isinstance(credentials, Credentials):
            raise _unavailable("credentials")
        threshold = MAX_PUT_BYTES if multipart_threshold is None else multipart_threshold
        if (
            not isinstance(conditional_multipart, bool)
            or not isinstance(part_bytes, int)
            or isinstance(part_bytes, bool)
            or not MIN_PART_BYTES <= part_bytes <= MAX_PUT_BYTES
            or not isinstance(threshold, int)
            or isinstance(threshold, bool)
            or not 0 <= threshold <= MAX_PUT_BYTES
        ):
            raise _unavailable("multipart_settings")
        self._multipart = conditional_multipart
        self._part_bytes = part_bytes
        self._threshold = threshold
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

    @property
    def max_object_bytes(self) -> int:
        """The largest object :meth:`put_new` takes: 50 GiB with conditional multipart,
        else one PUT's 5 GiB."""
        return MAX_OBJECT_BYTES if self._multipart else MAX_PUT_BYTES

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

    def put_new(self, key: str, data: bytes | Path, *, sha256: str | None = None) -> str:
        """Create ``key`` once and return its version id (:data:`NULL_VERSION` in a bucket
        without versioning). An existing key is ``version_exists`` (P7). A ``Path`` is read
        without following a link and streamed; if it changes between hashing and upload,
        the store refuses it (``hash_mismatch``). ``sha256`` (lowercase hex), when given,
        is what the bytes must hash to (``hash_mismatch`` before anything is completed);
        a file over the multipart threshold needs it (see the module docstring)."""
        _check_key(key)
        expect = None if sha256 is None else _check_sha256_hex(sha256)
        if isinstance(data, (bytes, bytearray)):
            body = bytes(data)
            if len(body) > MAX_PUT_BYTES:
                raise Refused("too_large")
            digest = hashlib.sha256(body).digest()
            if expect is not None and not secrets.compare_digest(digest.hex(), expect):
                raise Refused("hash_mismatch", detail_code="expected")
            return self._put(key, io.BytesIO(body), digest)
        if not isinstance(data, Path):
            raise Refused("file_not_allowed", detail_code="data")
        with _open_regular(data) as (f, size):
            if size > MAX_OBJECT_BYTES:
                raise Refused("too_large")
            if self._multipart and size > self._threshold:
                if expect is None:
                    raise Refused("hash_mismatch", detail_code="expected")
                return self._put_multipart(key, f, size, expect)
            if size > MAX_PUT_BYTES:
                raise Refused("too_large", detail_code=NO_MULTIPART)
            digest, n = _sha256_stream(f)
            if n != size:
                raise Refused("size_mismatch")
            if expect is not None and not secrets.compare_digest(digest.hex(), expect):
                raise Refused("hash_mismatch", detail_code="expected")
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

    # ------------------------------------------------------------ multipart

    def _check_absent(self, key: str) -> None:
        """Nothing at ``key`` yet, so no parts are sent for a key that is taken. (The
        conditional completion is what guarantees write-once; this only saves the
        upload.)"""
        exists = True
        try:
            self._call(self._client.head_object, Key=key)
        except Refused as exc:
            if exc.reason != "not_found":
                raise
            exists = False
        if exists:
            raise Refused("version_exists", detail_code="not_ours")

    def _put_multipart(self, key: str, f: BinaryIO, size: int, expect: str) -> str:
        count = -(-size // self._part_bytes)
        if count > MAX_PARTS:
            raise Refused("too_large", detail_code="parts")
        self._check_absent(key)
        up = self._call(self._client.create_multipart_upload, Key=key, ChecksumAlgorithm="SHA256")
        upload_id = _returned(up.get("UploadId"), _UPLOAD_ID, "upload_id")
        sha_parts = up.get("ChecksumAlgorithm") == "SHA256"
        vid: str | None = None
        composite: str | None = None
        try:
            parts, digests = self._send_parts(key, upload_id, f, size, expect, sha_parts)
            composite = _composite(digests) if sha_parts else None
            vid = self._complete(key, upload_id, parts, composite)
        finally:
            if vid is None:  # any failure, KeyboardInterrupt included
                self._abort(key, upload_id)
        self._check_multipart_stored(key, vid, size, expect, composite)
        log.info("store multipart upload complete: parts=%d", count)
        return vid

    def _send_parts(
        self, key: str, upload_id: str, f: BinaryIO, size: int, expect: str, sha_parts: bool
    ) -> tuple[list[dict[str, Any]], list[bytes]]:
        """Every part, each with its checksum. The whole file's SHA-256, over exactly the
        bytes sent, must be ``expect`` (``hash_mismatch``: nothing is completed)."""
        whole = hashlib.sha256()
        parts: list[dict[str, Any]] = []
        digests: list[bytes] = []
        sent = 0
        number = 0
        while sent < size:
            number += 1
            body = _read_exact(f, min(self._part_bytes, size - sent))
            sent += len(body)
            whole.update(body)
            digest = hashlib.sha256(body).digest()
            digests.append(digest)
            kw: dict[str, Any] = {"Key": key, "UploadId": upload_id, "PartNumber": number}
            entry: dict[str, Any] = {}
            if sha_parts:
                checksum = base64.b64encode(digest).decode("ascii")
                resp = self._call(
                    self._client.upload_part,
                    Body=body,
                    ChecksumAlgorithm="SHA256",
                    ChecksumSHA256=checksum,
                    **kw,
                )
                if resp.get("ChecksumSHA256") != checksum:
                    raise Refused("hash_mismatch", detail_code="part_checksum")
                entry["ChecksumSHA256"] = checksum
            else:
                md5 = hashlib.md5(body, usedforsecurity=False).digest()
                resp = self._call(
                    self._client.upload_part,
                    Body=body,
                    ContentMD5=base64.b64encode(md5).decode("ascii"),
                    **kw,
                )
            etag = _returned(resp.get("ETag"), _ETAG, "etag")
            parts.append({"PartNumber": number, "ETag": etag, **entry})
        if _has_more(f):
            raise Refused("size_mismatch")
        if not secrets.compare_digest(whole.hexdigest(), expect):
            raise Refused("hash_mismatch", detail_code="expected")
        return parts, digests

    def _complete(
        self, key: str, upload_id: str, parts: list[dict[str, Any]], composite: str | None
    ) -> str:
        """``CompleteMultipartUpload`` with ``If-None-Match: *``. A refusal whose object
        is exactly ours (a retried completion whose first attempt succeeded: 412, or no
        such upload) is adopted; anything else is ``version_exists``."""
        failure = False
        resp: Any = None
        try:
            resp = self._call(
                self._client.complete_multipart_upload,
                precondition="version_exists",
                Key=key,
                UploadId=upload_id,
                MultipartUpload={"Parts": parts},
                IfNoneMatch="*",
            )
        except Refused as exc:
            if exc.reason not in ("version_exists", "not_found"):
                raise
            failure = True
        if not failure:
            got = resp.get("ChecksumSHA256")
            if composite is not None and got is not None and got != composite:
                raise Refused("hash_mismatch", detail_code="stored_checksum")
            return _returned_version_id(resp.get("VersionId"))
        found = self.versions(key)
        if len(found) != 1 or composite is None:
            raise Refused("version_exists", detail_code="not_ours")
        resp = self._call(
            self._client.head_object, Key=key, ChecksumMode="ENABLED", **_version_kw(found[0])
        )
        if resp.get("ChecksumSHA256") != composite:
            raise Refused("version_exists", detail_code="not_ours")
        log.info("store multipart completion retried and found ours")
        return found[0]

    def _abort(self, key: str, upload_id: str) -> None:
        """Best effort: a failed abort is logged (without the key) and left to
        ``store-abort-stale-uploads`` or the bucket's lifecycle rule."""
        failed = False
        try:
            self._call(self._client.abort_multipart_upload, Key=key, UploadId=upload_id)
        except Refused as exc:
            failed = exc.reason != "not_found"
        if failed:
            log.warning("store multipart abort failed; the stale-upload cleanup removes it")

    def _check_multipart_stored(
        self, key: str, vid: str, size: int, expect: str, composite: str | None
    ) -> None:
        """The stored version is exactly what was hashed, and the key's only version."""
        resp = self._call(
            self._client.head_object, Key=key, ChecksumMode="ENABLED", **_version_kw(vid)
        )
        if resp.get("ContentLength") != size:
            raise Refused("hash_mismatch", detail_code="stored")
        if _returned_version_id(resp.get("VersionId")) != vid:
            raise Refused("hash_mismatch", detail_code="stored")
        got = resp.get("ChecksumSHA256")
        if composite is not None and isinstance(got, str) and _COMPOSITE.fullmatch(got):
            if not secrets.compare_digest(got, composite):
                raise Refused("hash_mismatch", detail_code="stored")
        else:
            # No checksum of the parts kept: read the whole object back.
            with open(os.devnull, "wb", buffering=0) as sink:
                n, sha = self.download(key, version_id=vid, out=sink, max_bytes=size)
            if n != size or not secrets.compare_digest(sha, expect):
                raise Refused("hash_mismatch", detail_code="stored")
        if self.versions(key) != [vid]:
            raise Refused("version_exists", detail_code="not_ours")

    # ------------------------------------------------------------ operators

    def probe_conditional_multipart(self) -> bool:
        """Whether the store refuses a conditional multipart completion on a key that
        exists (True) or completes it anyway (False: never set
        ``MODEL_STORE_CONDITIONAL_MULTIPART`` for it). Writes one small object, and on a
        store that fails the probe a second version of it, under
        ``models/_store-probe/<random>/``; nothing else."""
        key = f"models/_store-probe/{secrets.token_hex(8)}/probe"
        self.put_new(key, b"probe: the first object")
        up = self._call(self._client.create_multipart_upload, Key=key, ChecksumAlgorithm="SHA256")
        upload_id = _returned(up.get("UploadId"), _UPLOAD_ID, "upload_id")
        body = b"probe: an overwrite the store must refuse"
        checksum = base64.b64encode(hashlib.sha256(body).digest()).decode("ascii")
        honoured = False
        try:
            resp = self._call(
                self._client.upload_part,
                Key=key,
                UploadId=upload_id,
                PartNumber=1,
                Body=body,
                ChecksumAlgorithm="SHA256",
                ChecksumSHA256=checksum,
            )
            etag = _returned(resp.get("ETag"), _ETAG, "etag")
            part = {"PartNumber": 1, "ETag": etag, "ChecksumSHA256": checksum}
            try:
                self._call(
                    self._client.complete_multipart_upload,
                    precondition="version_exists",
                    Key=key,
                    UploadId=upload_id,
                    MultipartUpload={"Parts": [part]},
                    IfNoneMatch="*",
                )
            except Refused as exc:
                if exc.reason != "version_exists":
                    raise
                honoured = True
        finally:
            self._abort(key, upload_id)
        log.info("store probe: conditional multipart honoured=%s", honoured)
        return honoured

    def abort_stale_uploads(
        self,
        older_than_s: int = DEFAULT_STALE_AGE_S,
        *,
        now: datetime | None = None,
        prefixes: tuple[str, ...] = ("models/", "eval/"),
    ) -> int:
        """Abort every incomplete multipart upload under ``prefixes`` (each a layout prefix
        or a key) started more than ``older_than_s`` seconds ago (at least
        :data:`MIN_STALE_AGE_S`, so a running promotion is never cut off), and return how
        many. No object is touched: an abort only drops parts never completed.

        MinIO lists uploads for an exact key only, so there this finds nothing under
        ``models/``; MinIO drops stale uploads itself (its ``stale_uploads_expiry``,
        24 hours by default). AWS S3 lists by prefix."""
        if (
            not isinstance(older_than_s, int)
            or isinstance(older_than_s, bool)
            or older_than_s < MIN_STALE_AGE_S
        ):
            raise Refused("not_allowed_here", detail_code="stale_age")
        for prefix in prefixes:
            if not (isinstance(prefix, str) and prefix.endswith("/")):
                _check_key(prefix)
            else:
                _check_prefix(prefix)
        cutoff = (now or datetime.now(UTC)) - timedelta(seconds=older_than_s)
        aborted = 0
        seen = 0
        for prefix in prefixes:
            kw: dict[str, Any] = {"Prefix": prefix}
            while True:
                resp = self._call(self._client.list_multipart_uploads, **kw)
                for up in resp.get("Uploads") or []:
                    seen += 1
                    if seen > MAX_LIST:
                        raise Refused("too_many_files")
                    stale = _stale_upload(up, cutoff)
                    if stale is None or not stale[0].startswith(prefix):
                        continue
                    key, upload_id = stale
                    try:
                        self._call(self._client.abort_multipart_upload, Key=key, UploadId=upload_id)
                    except Refused as exc:
                        if exc.reason != "not_found":  # finished or aborted meanwhile
                            raise
                        continue
                    aborted += 1
                if not resp.get("IsTruncated"):
                    break
                kw = {
                    "Prefix": prefix,
                    "KeyMarker": resp.get("NextKeyMarker"),
                    "UploadIdMarker": resp.get("NextUploadIdMarker"),
                }
        log.info("store stale uploads aborted: count=%d", aborted)
        return aborted

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
        store keeps with it is ``hash_mismatch``. A multipart object, of which the store
        keeps no whole SHA-256, is hashed in full all the same. The caller compares the
        result with what the manifest recorded (or verifies it as a signature)."""
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
            expected = _whole_sha256(resp)
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
        if expected is not None and sha != expected:
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


def object_cap(target: object | None) -> int:
    """The largest file ``target`` takes: an :class:`S3Store`'s ``max_object_bytes``; any
    other store one PUT's 5 GiB; no store at all (a dry run, the export step) the
    multipart bound, since the store it goes to is not known yet."""
    if target is None:
        return MAX_OBJECT_BYTES
    if isinstance(target, S3Store):
        return target.max_object_bytes
    return MAX_PUT_BYTES


__all__ = [
    "DEFAULT_STALE_AGE_S",
    "LOOPBACK_HOSTS",
    "MAX_OBJECT_BYTES",
    "MAX_PUT_BYTES",
    "MIN_STALE_AGE_S",
    "NULL_VERSION",
    "PART_BYTES",
    "Credentials",
    "ObjectInfo",
    "ObjectStore",
    "S3Store",
    "check_bucket",
    "check_endpoint",
    "check_region",
    "claim_key",
    "eval_file_key",
    "object_cap",
    "manifest_key",
    "manifest_sig_key",
    "run_doc_key",
    "run_file_key",
    "testset_claim_key",
    "testset_key",
    "testset_sig_key",
]
