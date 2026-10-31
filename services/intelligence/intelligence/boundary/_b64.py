"""Unpadded, canonical base64url, as core's reqtoken.decodeB64 reads it."""

from __future__ import annotations

import base64
import binascii

_ALPHABET = frozenset(b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")


class B64Error(ValueError):
    """Not unpadded canonical base64url."""


def encode(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def decode(s: str) -> bytes:
    """Refuse any byte outside [A-Za-z0-9_-] first (padding, CR, LF, spaces,
    + and / included), then decode, refusing non-canonical trailing bits."""
    try:
        data = s.encode("ascii")
    except UnicodeEncodeError as e:
        raise B64Error("not base64url") from e
    if not _ALPHABET.issuperset(data) or len(data) % 4 == 1:
        raise B64Error("not base64url")
    try:
        raw = base64.urlsafe_b64decode(data + b"=" * (-len(data) % 4))
    except (binascii.Error, ValueError) as e:
        raise B64Error("not base64url") from e
    if encode(raw) != s:
        raise B64Error("non-canonical base64url")
    return raw
