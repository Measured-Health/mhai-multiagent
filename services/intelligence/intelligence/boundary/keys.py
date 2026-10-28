"""The trusted request-token keys (spec §7.5).

Read from the mounted file INTELLIGENCE_TRUSTED_KEYS_PATH and nowhere else:
never from NATS, so a compromised NATS can't add a key. The format is core's
reqtoken.WriteTrusted: {"keys": {"<kid>": "<base64url 32-byte public key>"}},
one or two keys, each under the kid its key gives (the first 16 hex
characters of sha256 of the key). Errors and logs never name the path or a
key.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import signal
import threading
from collections.abc import Mapping
from pathlib import Path
from types import MappingProxyType
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
    """The current trusted set.

    Reads take no lock: the set is one immutable mapping, replaced whole by
    a single reference assignment, so a verification never waits on a
    reload. A lock only serialises reloads, and is never waited on: a
    reload asked for while one runs is queued and picked up by the running
    one, so nothing can deadlock (spec §7.5).
    """

    def __init__(self, keys: Mapping[str, Ed25519PublicKey]) -> None:
        if not 0 < len(keys) <= MAX_TRUSTED:
            raise TrustedKeysError()
        self._keys: Mapping[str, Ed25519PublicKey] = MappingProxyType(dict(keys))
        self._reload_lock = threading.Lock()
        self._pending = False
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
        return self._keys.get(kid)

    def kids(self) -> frozenset[str]:
        return frozenset(self._keys)

    def reload(self, path: str | Path) -> bool:
        """Re-read the file. A bad file keeps the current set (logged, counted).

        Returns True if this call installed a new set. If a reload is already
        running, this one is queued for it and returns False at once."""
        self._pending = True
        installed = False
        while self._pending:
            if not self._reload_lock.acquire(blocking=False):
                return installed
            try:
                while self._pending:
                    self._pending = False
                    installed = self._reload_once(path)
            finally:
                self._reload_lock.release()
        return installed

    def _reload_once(self, path: str | Path) -> bool:
        try:
            keys = _read(path)
        except TrustedKeysError:
            self.reload_failures += 1
            log.warning(
                "trusted keys reload refused; keeping the current %d key(s)", len(self._keys)
            )
            return False
        self._keys = MappingProxyType(keys)
        log.info("trusted keys reloaded: %d key(s)", len(keys))
        return True


def install_sighup_reload(
    trusted: TrustedKeys, path: str | Path, loop: asyncio.AbstractEventLoop
) -> None:
    """Re-read path into trusted on SIGHUP, as a callback on loop.

    loop.add_signal_handler runs the reload from the event loop, not inside
    a signal handler, so it never interrupts a verification. Undo with
    loop.remove_signal_handler(signal.SIGHUP)."""
    loop.add_signal_handler(signal.SIGHUP, trusted.reload, path)
