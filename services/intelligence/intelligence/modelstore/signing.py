"""Domain-separated signatures, the trust policy, and a development-only file signer.

**The signed message** is ``b"mhai.models.<purpose>/1\\n" + payload``, with ``purpose`` one
of :data:`PURPOSES` (``manifest``, ``signoff``, ``testset``). A signature made for one
purpose never verifies as another, and a signature over the bare payload never verifies
at all (P13).

**A signature document** (``mhai.models.signature/1``) is strict canonical JSON:
``{schema, purpose, alg, key_id, signature}``; ``signature`` is standard, padded base64
of the raw Ed25519 signature or of the DER-encoded ECDSA signature (never raw ``r||s``).
``key_id`` is the lowercase hex SHA-256 of the DER SubjectPublicKeyInfo.

**The trust policy** (``models/trust-policy.json``, ``mhai.models.trust/1``) lists, per
purpose, the accepted signers: ``{kind: public_key, alg: ed25519|ecdsa-p256-sha256,
key_id, public_key_pem, development}``. Loading it outside development refuses any
signer marked ``development`` (``policy_invalid``, P12), as does verifying with one; the
``manifest`` and ``signoff`` sets never share a key (the two workflows' signers are
separate). The committed policy trusts nobody until Task 26.

**The file signer** (:class:`FileSigner`) is Ed25519 from a PEM file, for development
and tests only: it refuses to exist, or to sign, unless ``MHAI_MODELS_ENV`` is exactly
``development``. Private key material is never logged, printed or put in an error.
"""

import base64
import binascii
import hashlib
import os
import re
import secrets
import stat
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from types import MappingProxyType
from typing import Any, Protocol

from cryptography.exceptions import InvalidSignature, UnsupportedAlgorithm
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec, ed25519
from cryptography.hazmat.primitives.asymmetric.utils import (
    decode_dss_signature,
    encode_dss_signature,
)

from intelligence.modelstore import canonical, files
from intelligence.modelstore.reasons import Refused

PURPOSES: tuple[str, ...] = ("manifest", "signoff", "testset")
ALGS: tuple[str, ...] = ("ed25519", "ecdsa-p256-sha256")
SIGNATURE_SCHEMA = "mhai.models.signature/1"
TRUST_SCHEMA = "mhai.models.trust/1"
ENV_VAR = "MHAI_MODELS_ENV"
DEVELOPMENT = "development"
DEV_KEY_FILE = "dev-signing-key.pem"
DEV_PUBLIC_KEY_FILE = "dev-signing-key.pub.pem"

MAX_SIGNATURE_DOC_BYTES = 4096
MAX_SIGNATURE_BYTES = 512
MAX_POLICY_BYTES = 1024 * 1024
MAX_SIGNERS = 16
MAX_PEM_CHARS = 4096
MAX_KEY_FILE_BYTES = 16 * 1024

_KEY_ID = re.compile(r"[0-9a-f]{64}", re.ASCII)
_B64 = re.compile(r"(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?", re.ASCII)
_SIGNATURE_KEYS = frozenset({"schema", "purpose", "alg", "key_id", "signature"})
_SIGNER_KEYS = frozenset({"kind", "alg", "key_id", "public_key_pem", "development"})
_CLOEXEC = getattr(os, "O_CLOEXEC", 0)

PublicKey = ed25519.Ed25519PublicKey | ec.EllipticCurvePublicKey


# ---------------------------------------------------------------- environment


def is_development() -> bool:
    """True only when ``MHAI_MODELS_ENV`` is exactly ``development``."""
    return os.environ.get(ENV_VAR) == DEVELOPMENT


def _require_development() -> None:
    if not is_development():
        raise Refused("not_allowed_here", detail_code="not_development")


# ---------------------------------------------------------------- messages and ids


def _check_purpose(purpose: object) -> str:
    if not isinstance(purpose, str) or purpose not in PURPOSES:
        raise Refused("signature_invalid", detail_code="purpose")
    return purpose


def message(purpose: str, payload: bytes) -> bytes:
    """The bytes actually signed: ``b"mhai.models.<purpose>/1\\n" + payload``."""
    _check_purpose(purpose)
    if not isinstance(payload, (bytes, bytearray)):
        raise Refused("signature_invalid", detail_code="payload")
    return b"mhai.models." + purpose.encode("ascii") + b"/1\n" + bytes(payload)


def key_id(public_key: PublicKey) -> str:
    """The lowercase hex SHA-256 of the key's DER SubjectPublicKeyInfo."""
    der = public_key.public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    return hashlib.sha256(der).hexdigest()


def _public_pem(public_key: PublicKey) -> str:
    return public_key.public_bytes(
        serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
    ).decode("ascii")


# ---------------------------------------------------------------- signature documents


def _sig_invalid(detail: str) -> Refused:
    return Refused("signature_invalid", detail_code=detail)


@dataclass(frozen=True, slots=True)
class SignatureDoc:
    """``mhai.models.signature/1``: who signed what, for which purpose."""

    purpose: str
    alg: str
    key_id: str
    signature: bytes = field(repr=False)

    def __post_init__(self) -> None:
        _check_purpose(self.purpose)
        if not isinstance(self.alg, str) or self.alg not in ALGS:
            raise _sig_invalid("alg")
        if not isinstance(self.key_id, str) or _KEY_ID.fullmatch(self.key_id) is None:
            raise _sig_invalid("key_id")
        if (
            not isinstance(self.signature, bytes)
            or not 1 <= len(self.signature) <= MAX_SIGNATURE_BYTES
        ):
            raise _sig_invalid("signature")

    def to_obj(self) -> dict[str, str]:
        return {
            "schema": SIGNATURE_SCHEMA,
            "purpose": self.purpose,
            "alg": self.alg,
            "key_id": self.key_id,
            "signature": base64.b64encode(self.signature).decode("ascii"),
        }

    def to_bytes(self) -> bytes:
        return canonical.dumps(self.to_obj())


def _b64decode(value: object) -> bytes:
    """Standard, padded base64 in its one canonical spelling, or ``signature_invalid``."""
    if not isinstance(value, str) or not value or _B64.fullmatch(value) is None:
        raise _sig_invalid("base64")
    failed = False
    try:
        raw = base64.b64decode(value, validate=True)
    except (binascii.Error, ValueError):
        failed = True
    if failed or base64.b64encode(raw).decode("ascii") != value:
        raise _sig_invalid("base64")
    return raw


def parse_signature(b: bytes) -> SignatureDoc:
    """A signature document from its stored bytes: strict canonical JSON
    (``not_canonical`` otherwise), exactly the five keys, every value checked."""
    failed = False
    try:
        obj = canonical.loads_strict(b, MAX_SIGNATURE_DOC_BYTES)
    except Refused:
        failed = True
    if failed:
        raise _sig_invalid("json")
    if canonical.dumps(obj) != bytes(b):
        raise Refused("not_canonical")
    if not isinstance(obj, dict) or set(obj) != _SIGNATURE_KEYS:
        raise _sig_invalid("keys")
    if obj["schema"] != SIGNATURE_SCHEMA:
        raise _sig_invalid("schema")
    return SignatureDoc(obj["purpose"], obj["alg"], obj["key_id"], _b64decode(obj["signature"]))


# ---------------------------------------------------------------- the trust policy


def _policy(detail: str) -> Refused:
    return Refused("policy_invalid", detail_code=detail)


@dataclass(frozen=True, slots=True)
class TrustedKey:
    """One accepted public-key signer. Construction checks that ``key_id`` is the key's
    id, that the key fits ``alg`` (Ed25519, or ECDSA on P-256), and that the PEM is the
    key's own canonical SubjectPublicKeyInfo PEM."""

    alg: str
    key_id: str
    public_key_pem: str
    development: bool
    public_key: PublicKey = field(init=False, repr=False, compare=False)
    kind: str = "public_key"

    def __post_init__(self) -> None:
        if self.kind != "public_key":
            raise _policy("signer_kind")
        if not isinstance(self.alg, str) or self.alg not in ALGS:
            raise _policy("signer_alg")
        if not isinstance(self.development, bool):
            raise _policy("signer_development")
        if not isinstance(self.key_id, str) or _KEY_ID.fullmatch(self.key_id) is None:
            raise _policy("signer_key_id")
        pem = self.public_key_pem
        if not isinstance(pem, str) or not pem.isascii() or len(pem) > MAX_PEM_CHARS:
            raise _policy("signer_pem")
        failed = False
        try:
            key: Any = serialization.load_pem_public_key(pem.encode("ascii"))
        except (ValueError, TypeError, UnsupportedAlgorithm):
            failed = True
        if failed:
            raise _policy("signer_pem")
        if self.alg == "ed25519":
            ok = isinstance(key, ed25519.Ed25519PublicKey)
        else:
            ok = isinstance(key, ec.EllipticCurvePublicKey) and isinstance(key.curve, ec.SECP256R1)
        if not ok:
            raise _policy("signer_alg_mismatch")
        if _public_pem(key) != pem:
            raise _policy("signer_pem")
        if key_id(key) != self.key_id:
            raise _policy("signer_key_id_mismatch")
        object.__setattr__(self, "public_key", key)


@dataclass(frozen=True, slots=True)
class TrustPolicy:
    """The accepted signers per purpose. Construction requires every purpose, at most
    :data:`MAX_SIGNERS` distinct keys each, and no ``signoff`` key that is also a
    ``manifest`` or ``testset`` key."""

    signers: Mapping[str, tuple[TrustedKey, ...]]

    def __post_init__(self) -> None:
        if not isinstance(self.signers, Mapping) or set(self.signers) != set(PURPOSES):
            raise _policy("purposes")
        for keys in self.signers.values():
            if (
                not isinstance(keys, tuple)
                or len(keys) > MAX_SIGNERS
                or not all(isinstance(k, TrustedKey) for k in keys)
                or len({k.key_id for k in keys}) != len(keys)
            ):
                raise _policy("signers")
        signoff_ids = {k.key_id for k in self.signers["signoff"]}
        if signoff_ids & {k.key_id for k in self.signers["manifest"]}:
            raise _policy("manifest_signoff_shared")
        # The test-split signer lives in model-promotion; a shared key would let that
        # identity sign sign-offs (domain separation can't help the key's holder).
        if signoff_ids & {k.key_id for k in self.signers["testset"]}:
            raise _policy("testset_signoff_shared")
        object.__setattr__(self, "signers", MappingProxyType(dict(self.signers)))

    def has_development_signer(self) -> bool:
        return any(k.development for keys in self.signers.values() for k in keys)


def _signer(obj: object) -> TrustedKey:
    if not isinstance(obj, dict) or set(obj) != _SIGNER_KEYS:
        raise _policy("signer_keys")
    if obj["kind"] != "public_key":
        raise _policy("signer_kind")
    return TrustedKey(obj["alg"], obj["key_id"], obj["public_key_pem"], obj["development"])


def parse_policy(obj: object, *, environment: str | None) -> TrustPolicy:
    """A trust policy from its parsed JSON. ``environment`` is the ``MHAI_MODELS_ENV``
    value it is loaded under: anything but exactly ``development`` is production, where a
    development signer is ``policy_invalid``."""
    if not isinstance(obj, dict) or set(obj) != {"schema", "signers"}:
        raise _policy("keys")
    if obj["schema"] != TRUST_SCHEMA:
        raise _policy("schema")
    signers = obj["signers"]
    if not isinstance(signers, dict) or set(signers) != set(PURPOSES):
        raise _policy("purposes")
    out: dict[str, tuple[TrustedKey, ...]] = {}
    for purpose in PURPOSES:
        entries = signers[purpose]
        if not isinstance(entries, list) or len(entries) > MAX_SIGNERS:
            raise _policy("signers")
        out[purpose] = tuple(_signer(e) for e in entries)
    policy = TrustPolicy(out)
    if environment != DEVELOPMENT and policy.has_development_signer():
        raise _policy("development_signer_in_production")
    return policy


class _FromEnvironment:
    pass


_FROM_ENVIRONMENT = _FromEnvironment()


def load_policy(
    path: Path | str, *, environment: str | None | _FromEnvironment = _FROM_ENVIRONMENT
) -> TrustPolicy:
    """Read ``models/trust-policy.json`` (no links; strict JSON, no floats). By default
    the environment is read from ``MHAI_MODELS_ENV``. Any read failure is
    ``policy_invalid``."""
    env = os.environ.get(ENV_VAR) if isinstance(environment, _FromEnvironment) else environment
    failed = False
    try:
        raw = files.read_regular(path, MAX_POLICY_BYTES)
        obj = canonical.loads_strict(raw, MAX_POLICY_BYTES, trailing_whitespace=True)
    except Refused:
        failed = True
    if failed:
        raise _policy("read")
    return parse_policy(obj, environment=env)


# ---------------------------------------------------------------- signing and verifying


class Signer(Protocol):
    """Signs a payload for one purpose (the development :class:`FileSigner`; the
    production backend in Task 26)."""

    @property
    def key_id(self) -> str: ...

    @property
    def alg(self) -> str: ...

    def sign(self, purpose: str, payload: bytes) -> SignatureDoc: ...


@dataclass(frozen=True, slots=True)
class SignerId:
    """Who a verified signature came from."""

    purpose: str
    key_id: str
    alg: str
    development: bool


def _verify_raw(key: TrustedKey, signature: bytes, msg: bytes) -> bool:
    try:
        if key.alg == "ed25519":
            pub = key.public_key
            if not isinstance(pub, ed25519.Ed25519PublicKey) or len(signature) != 64:
                return False
            pub.verify(signature, msg)
            return True
        pub_ec = key.public_key
        if not isinstance(pub_ec, ec.EllipticCurvePublicKey):
            return False
        # DER only: it must decode, and re-encode to exactly the same bytes. High-S is
        # accepted on purpose: (r, n - s) also verifies, and cloud KMS ECDSA signers
        # (Task 26) don't normalise S, so requiring low S would refuse about half of
        # their valid signatures. A signature is therefore malleable and must never be
        # used as an identifier or a deduplication key; pins and locks name the signed
        # payload's SHA-256 instead.
        r, s = decode_dss_signature(signature)
        if encode_dss_signature(r, s) != signature:
            return False
        pub_ec.verify(signature, msg, ec.ECDSA(hashes.SHA256()))
        return True
    except (InvalidSignature, ValueError, TypeError):
        return False


class Verifier:
    """Verifies a signature document against a trust policy."""

    def verify(
        self, purpose: str, payload: bytes, sig: SignatureDoc | bytes, policy: TrustPolicy
    ) -> SignerId:
        """The signer of ``sig`` over ``message(purpose, payload)``, or :class:`Refused`:

        * ``signature_invalid``: a malformed document, another purpose, or a signature
          that does not verify over the domain-separated message;
        * ``signer_untrusted``: no signer for ``purpose`` has this key id and alg;
        * ``policy_invalid``: not a policy, or a development signer outside development.
        """
        msg = message(purpose, payload)
        if isinstance(sig, (bytes, bytearray)):
            sig = parse_signature(bytes(sig))
        if not isinstance(sig, SignatureDoc):
            raise _sig_invalid("type")
        if not isinstance(policy, TrustPolicy):
            raise _policy("type")
        if sig.purpose != purpose:
            raise _sig_invalid("purpose_mismatch")
        trusted = [k for k in policy.signers[purpose] if (k.key_id, k.alg) == (sig.key_id, sig.alg)]
        if not trusted:
            raise Refused("signer_untrusted")
        key = trusted[0]
        if key.development and not is_development():
            raise _policy("development_signer_in_production")
        if not _verify_raw(key, sig.signature, msg):
            raise _sig_invalid("verify")
        return SignerId(purpose, key.key_id, key.alg, key.development)


# ---------------------------------------------------------------- the development signer


def _key_invalid(detail: str) -> Refused:
    return Refused("key_invalid", detail_code=detail)


def _read_private_key(path: Path | str) -> ed25519.Ed25519PrivateKey:
    """An Ed25519 private key from a regular, singly linked PEM file of mode 0600 or
    stricter, owned by this user, opened without following a link."""
    fd = -1
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC)
    except OSError:
        pass
    if fd < 0:
        raise _key_invalid("open")
    try:
        st = os.fstat(fd)
        if (
            not stat.S_ISREG(st.st_mode)
            or st.st_nlink != 1
            or stat.S_IMODE(st.st_mode) & 0o177
            or st.st_uid != os.geteuid()
            or st.st_size > MAX_KEY_FILE_BYTES
        ):
            raise _key_invalid("file")
        with os.fdopen(os.dup(fd), "rb", buffering=0) as f:
            pem = f.read(MAX_KEY_FILE_BYTES + 1)
    finally:
        os.close(fd)
    if len(pem) > MAX_KEY_FILE_BYTES:
        raise _key_invalid("file")
    # The refusal is raised outside the ``except`` block: no exception in its chain
    # holds the key bytes.
    failed = False
    try:
        key = serialization.load_pem_private_key(pem, password=None)
    except (ValueError, TypeError, UnsupportedAlgorithm):
        failed = True
    del pem
    if failed or not isinstance(key, ed25519.Ed25519PrivateKey):
        raise _key_invalid("not_ed25519")
    return key


class FileSigner:
    """Ed25519 from a PEM file: development and tests only (P12)."""

    __slots__ = ("_key", "_key_id")

    def __init__(self, path: Path | str) -> None:
        _require_development()
        self._key = _read_private_key(path)
        self._key_id = key_id(self._key.public_key())

    @property
    def key_id(self) -> str:
        return self._key_id

    @property
    def alg(self) -> str:
        return "ed25519"

    def public_key_pem(self) -> str:
        return _public_pem(self._key.public_key())

    def sign(self, purpose: str, payload: bytes) -> SignatureDoc:
        _require_development()
        msg = message(purpose, payload)
        return SignatureDoc(purpose, self.alg, self._key_id, self._key.sign(msg))

    def __repr__(self) -> str:
        return f"FileSigner(key_id={self._key_id!r})"

    def __reduce__(self) -> Any:
        raise TypeError("FileSigner cannot be pickled")


def _open_out_dir(out: Path) -> int:
    """Open ``out`` (made with mode 0700 if missing) without following a link. An existing
    directory must belong to this user and be writable by nobody else: ``key_invalid``."""
    failed = False
    try:
        os.mkdir(out, 0o700)
    except FileExistsError:
        pass
    except OSError:
        failed = True
    if failed:
        raise _key_invalid("out_dir")
    dfd = -1
    try:
        dfd = os.open(out, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC)
    except OSError:
        pass
    if dfd < 0:
        raise _key_invalid("out_dir")
    st = os.fstat(dfd)
    if st.st_uid != os.geteuid() or stat.S_IMODE(st.st_mode) & 0o022:
        os.close(dfd)
        raise _key_invalid("out_dir_unsafe")
    return dfd


def generate_dev_key(out_dir: Path | str) -> tuple[str, str]:
    """Make a development Ed25519 key pair in ``out_dir``: :data:`DEV_KEY_FILE` (mode
    0600) and :data:`DEV_PUBLIC_KEY_FILE` (0644). Both files appear, or neither: each is
    written to a fresh temporary name (``O_EXCL``, mode 0600) and then linked into place
    without replacing anything, and if the second fails the first is removed. Never
    overwrites: an existing key file is ``key_exists``. ``out_dir`` is created with mode
    0700 if missing; an existing one owned by someone else, or writable by group or
    others, is ``key_invalid``. Returns the key id and the public key PEM, never the
    private key."""
    _require_development()
    dfd = _open_out_dir(Path(out_dir))
    try:
        for name in (DEV_KEY_FILE, DEV_PUBLIC_KEY_FILE):
            try:
                os.stat(name, dir_fd=dfd, follow_symlinks=False)
            except FileNotFoundError:
                continue
            raise Refused("key_exists")
        key = ed25519.Ed25519PrivateKey.generate()
        pub_pem = _public_pem(key.public_key())
        _write_pair(
            dfd,
            [
                (DEV_KEY_FILE, 0o600, _private_pem(key)),
                (DEV_PUBLIC_KEY_FILE, 0o644, pub_pem.encode()),
            ],
        )
    finally:
        os.close(dfd)
    return key_id(key.public_key()), pub_pem


def _private_pem(key: ed25519.Ed25519PrivateKey) -> bytes:
    return key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )


def _unlink_quietly(name: str, dfd: int) -> None:
    try:
        os.unlink(name, dir_fd=dfd)
    except OSError:
        pass


def _write_pair(dfd: int, items: list[tuple[str, int, bytes]]) -> None:
    """Write every ``(name, mode, content)`` to a temporary name, then link each into
    place (``link`` never replaces an existing name). All the names appear, or none."""
    temps: list[str] = []
    placed: list[str] = []
    ok = False
    try:
        for name, mode, content in items:
            temp = f".{name}.{secrets.token_hex(8)}.tmp"
            temps.append(temp)
            _write_new(dfd, temp, content, mode)
        for (name, _, _), temp in zip(items, temps, strict=True):
            _link_new(dfd, temp, name)
            placed.append(name)
        ok = True
    finally:
        for temp in temps:
            _unlink_quietly(temp, dfd)
        if not ok:
            for name in placed:
                _unlink_quietly(name, dfd)


def _link_new(dfd: int, temp: str, name: str) -> None:
    exists = False
    try:
        os.link(temp, name, src_dir_fd=dfd, dst_dir_fd=dfd, follow_symlinks=False)
    except FileExistsError:
        exists = True
    if exists:
        raise Refused("key_exists")


def _write_new(dfd: int, name: str, content: bytes, final_mode: int) -> None:
    """Create ``name`` (``O_EXCL``, mode 0600), write and sync it, then set
    ``final_mode``."""
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | _CLOEXEC
    fd = -1
    exists = False
    try:
        fd = os.open(name, flags, 0o600, dir_fd=dfd)
    except FileExistsError:
        exists = True
    if exists:
        raise Refused("key_exists")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(os.dup(fd), "wb") as f:
            f.write(content)
            f.flush()
            os.fsync(f.fileno())
        os.fchmod(fd, final_mode)
    finally:
        os.close(fd)


__all__ = [
    "ALGS",
    "DEVELOPMENT",
    "DEV_KEY_FILE",
    "DEV_PUBLIC_KEY_FILE",
    "ENV_VAR",
    "PURPOSES",
    "SIGNATURE_SCHEMA",
    "TRUST_SCHEMA",
    "FileSigner",
    "SignatureDoc",
    "Signer",
    "SignerId",
    "TrustPolicy",
    "TrustedKey",
    "Verifier",
    "generate_dev_key",
    "is_development",
    "key_id",
    "load_policy",
    "message",
    "parse_policy",
    "parse_signature",
]
