"""The trusted request-token keys (spec §7.5).

Read from the mounted file INTELLIGENCE_TRUSTED_KEYS_PATH and nowhere else:
never from NATS, so a compromised NATS can't add a key. The format is core's
reqtoken.WriteTrusted: {"keys": {"<kid>": "<base64url 32-byte public key>"}},
one or two keys, each under the kid its key gives (the first 16 hex
characters of sha256 of the key). Errors and logs never name the path or a
key.
"""

from __future__ import annotations

import hashlib
import logging
import signal
import threading
from collections.abc import Callable, Mapping
from pathlib import Path
from types import FrameType
from typing import Any

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from intelligence.boundary import _b64, _strictjson

log = logging.getLogger("intelligence.boundary.keys")

MAX_TRUSTED = 2
MAX_FILE = 16 << 10
PUBLIC_KEY_SIZE = 32


class TrustedKeysError(Exception):
    """The trusted-keys file is missing, empty, over two keys, or holds a bad key."""

    def __init__(self) -> None:
        super().__init__(
            "the trusted-keys file is missing, empty, over two keys, or holds a bad key"
        )


def key_id(pub: bytes) -> str:
    return hashlib.sha256(pub).hexdigest()[:16]


def _parse_encoded(enc: Any) -> dict[str, Ed25519PublicKey]:
    if not isinstance(enc, dict) or not 0 < len(enc) <= MAX_TRUSTED:
        raise TrustedKeysError()
    out: dict[str, Ed25519PublicKey] = {}
    for kid, val in enc.items():
        if not isinstance(kid, str) or not isinstance(val, str):
            raise TrustedKeysError()
        try:
            raw = _b64.decode(val)
        except _b64.B64Error:
            raise TrustedKeysError() from None
        if len(raw) != PUBLIC_KEY_SIZE or kid == "" or kid != key_id(raw):
            raise TrustedKeysError()
        out[kid] = Ed25519PublicKey.from_public_bytes(raw)
    return out


def _read(path: str | Path) -> dict[str, Ed25519PublicKey]:
    try:
        with open(path, "rb") as f:
            raw = f.read(MAX_FILE + 1)
    except OSError:
        raise TrustedKeysError() from None
    if len(raw) > MAX_FILE:
        raise TrustedKeysError()
    try:
        doc = _strictjson.loads(raw)
    except _strictjson.StrictJSONError:
        raise TrustedKeysError() from None
    if not isinstance(doc, dict) or set(doc) != {"keys"}:
        raise TrustedKeysError()
    return _parse_encoded(doc["keys"])


class TrustedKeys:
    """The current trusted set. reload() swaps it whole, or keeps the old one."""

    def __init__(self, keys: Mapping[str, Ed25519PublicKey]) -> None:
        if not 0 < len(keys) <= MAX_TRUSTED:
            raise TrustedKeysError()
        self._keys: dict[str, Ed25519PublicKey] = dict(keys)
        self._lock = threading.Lock()
        self.reload_failures = 0

    @classmethod
    def load(cls, path: str | Path) -> TrustedKeys:
        """Read the file; refuse to start on anything wrong with it."""
        return cls(_read(path))

    @classmethod
    def from_encoded(cls, enc: Mapping[str, str]) -> TrustedKeys:
        """The same rules as load, on the inner {kid: key} object (tests, vectors)."""
        return cls(_parse_encoded(dict(enc)))

    def get(self, kid: str) -> Ed25519PublicKey | None:
        with self._lock:
            return self._keys.get(kid)

    def kids(self) -> frozenset[str]:
        with self._lock:
            return frozenset(self._keys)

    def reload(self, path: str | Path) -> bool:
        """Re-read the file. A bad file keeps the current set (logged, counted)."""
        try:
            keys = _read(path)
        except TrustedKeysError:
            with self._lock:
                self.reload_failures += 1
                count = len(self._keys)
            log.warning("trusted keys reload refused; keeping the current %d key(s)", count)
            return False
        with self._lock:
            self._keys = keys
        log.info("trusted keys reloaded: %d key(s)", len(keys))
        return True


SignalHandler = Callable[[int, FrameType | None], Any] | int | None


def install_sighup_reload(trusted: TrustedKeys, path: str | Path) -> SignalHandler:
    """Re-read path into trusted on SIGHUP. Returns the previous handler."""

    def _on_hup(_signum: int, _frame: FrameType | None) -> None:
        trusted.reload(path)

    return signal.signal(signal.SIGHUP, _on_hup)
