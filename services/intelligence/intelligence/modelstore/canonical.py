"""Canonical JSON for anything hashed or signed.

UTF-8, sorted keys, ``(",", ":")`` separators, ``ensure_ascii=True``, and no
floats: integers and strings only (plus booleans, null, lists and objects).
The parser is strict: duplicate keys, floats, NaN, a BOM, leading or
trailing bytes, and oversized input are all refused.
"""

import json
import math
from typing import Any

from intelligence.modelstore.reasons import Refused

_DEFAULT_MAX = 16 * 1024 * 1024


def _no_float(_: str) -> Any:
    raise Refused("json_invalid")


def _no_constant(_: str) -> Any:
    raise Refused("json_invalid")


def _finite_float(s: str) -> float:
    value = float(s)
    if not math.isfinite(value):
        raise Refused("json_invalid")
    return value


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            raise Refused("json_invalid")
        out[key] = value
    return out


def _check_value(obj: Any) -> None:
    if obj is None or isinstance(obj, (bool, int, str)):
        return
    if isinstance(obj, list):
        for item in obj:
            _check_value(item)
        return
    if isinstance(obj, dict):
        for key, value in obj.items():
            if not isinstance(key, str):
                raise Refused("json_invalid")
            _check_value(value)
        return
    raise Refused("json_invalid")


_MAX_DEPTH = 64


def _check_depth(obj: Any) -> None:
    """Refuse nesting deeper than ``_MAX_DEPTH`` (iterative, so it cannot recurse)."""
    stack: list[tuple[Any, int]] = [(obj, 1)]
    while stack:
        item, depth = stack.pop()
        if isinstance(item, (list, dict)):
            if depth > _MAX_DEPTH:
                raise Refused("json_invalid")
            children = item.values() if isinstance(item, dict) else item
            stack.extend((child, depth + 1) for child in children)


def dumps(obj: Any) -> bytes:
    try:
        _check_value(obj)
        text = json.dumps(
            obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False
        )
    except (RecursionError, ValueError, TypeError) as exc:
        raise Refused("json_invalid") from exc
    return text.encode("ascii")


def loads_strict(
    b: bytes, max_bytes: int, *, allow_floats: bool = False, trailing_whitespace: bool = False
) -> Any:
    """Parse ``b`` strictly. ``allow_floats`` admits finite floats, and
    ``trailing_whitespace`` JSON whitespace after the value (the newline that
    ``transformers`` and editors write at the end of a file), for files that are checked
    but never hashed as canonical JSON (``config.json`` and the like, hand-edited
    registries); anything hashed or signed keeps the defaults.
    """
    if not isinstance(b, (bytes, bytearray)):
        raise Refused("json_invalid")
    if len(b) > max_bytes:
        raise Refused("too_large")
    if b.startswith(b"\xef\xbb\xbf"):
        raise Refused("json_invalid")
    # The refusal is raised outside the ``except`` block, so no exception in its chain
    # (a JSONDecodeError's ``doc``, a UnicodeDecodeError's ``object``) holds the input.
    failed = False
    try:
        text = bytes(b).decode("utf-8")
        decoder = json.JSONDecoder(
            parse_float=_finite_float if allow_floats else _no_float,
            parse_constant=_no_constant,
            object_pairs_hook=_no_duplicates,
        )
        obj, end = decoder.raw_decode(text, 0)
    except Refused:
        failed = True
    except (UnicodeDecodeError, ValueError, RecursionError):
        failed = True
    if failed:
        raise Refused("json_invalid")
    if end != len(text) and not (trailing_whitespace and text[end:].strip(" \t\n\r") == ""):
        raise Refused("json_invalid")
    _check_depth(obj)
    return obj


def is_canonical(b: bytes) -> bool:
    try:
        return dumps(loads_strict(b, _DEFAULT_MAX)) == bytes(b)
    except Refused:
        return False
