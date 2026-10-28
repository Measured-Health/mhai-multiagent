"""Strict JSON, as core's reqtoken.strictObject reads it.

Python's json module keeps the last of a repeated key, reads NaN and
Infinity, and keeps lone surrogates from \\u escapes. Core refuses the first
two; the third Go would turn into U+FFFD, and here it is refused (fails
closed: core never writes one). Callers check keys, types and null values
themselves.
"""

from __future__ import annotations

import json
from typing import Any


class StrictJSONError(ValueError):
    """The bytes are not strict JSON. Holds no part of the input."""


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for k, v in pairs:
        if k in out:
            raise StrictJSONError("repeated key")
        out[k] = v
    return out


def _no_constant(_: str) -> Any:
    raise StrictJSONError("NaN or Infinity")


def _has_surrogate(v: Any) -> bool:
    if isinstance(v, str):
        return any("\ud800" <= c <= "\udfff" for c in v)
    if isinstance(v, list):
        return any(_has_surrogate(x) for x in v)
    if isinstance(v, dict):
        return any(_has_surrogate(k) or _has_surrogate(x) for k, x in v.items())
    return False


def loads(raw: bytes) -> Any:
    """Parse raw (UTF-8, one JSON value, nothing after it)."""
    try:
        text = raw.decode("utf-8")
        value = json.loads(text, object_pairs_hook=_pairs, parse_constant=_no_constant)
    except (UnicodeDecodeError, ValueError, RecursionError) as e:
        raise StrictJSONError("not strict JSON") from e
    if _has_surrogate(value):
        raise StrictJSONError("lone surrogate")
    return value


def is_int(v: Any) -> bool:
    """A JSON integer: an int that isn't a bool (json reads 1.0 as float)."""
    return isinstance(v, int) and not isinstance(v, bool)


def is_str_list(v: Any) -> bool:
    return isinstance(v, list) and all(isinstance(x, str) for x in v)
