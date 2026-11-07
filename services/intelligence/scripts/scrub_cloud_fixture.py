"""Scrub a recorded cloud provider reply into a test fixture, or check
fixtures (plan Task 15; threat C19).

    python scripts/scrub_cloud_fixture.py <in> <out>
    python scripts/scrub_cloud_fixture.py --check <file> [<file> ...]

A fixture is {"status", "headers", "body"}, as the adapter tests load it.
Scrubbing keeps only the headers on HEADERS (request ids replaced), gives
every id field a fixed SYNTHETIC value (keeping the provider's prefix, such
as msg_ or chatcmpl-), and sets every timing field to a fixed time. It
writes nothing unless the result passes the check.

The check fails on: a top-level field other than status, headers and body;
a header outside HEADERS; an id field or request-id header whose value
isn't SYNTHETIC; an organisation, project or user field; a timing field
other than the fixed time; and any string, anywhere, shaped like a provider
key, a bearer token or a real organisation or project id. Problems name the
file and the rule, never the value. CI runs it on every committed fixture
(tests/boundary/cloud/test_smoke_guard.py).
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path
from typing import Any

SYNTHETIC = "SYNTHETIC0001"
FIXED_TIME = 1762473600  # 2025-11-07T00:00:00Z, as the synthetic fixtures

# Kept as they are: what the adapters read (content type, the xAI retention
# header, Retry-After, a redirect's Location).
KEEP_HEADERS = frozenset({"content-type", "x-zero-data-retention", "retry-after", "location"})
# Kept with a synthetic value.
ID_HEADERS = frozenset({"request-id", "x-request-id"})
HEADERS = KEEP_HEADERS | ID_HEADERS

# Body keys whose value identifies a response, request or account.
_ID_KEY = re.compile(r"^(id|.*_id|.*Id|system_fingerprint)$")
_TIME_KEY = re.compile(r"^(created|.*_at|.*_time|.*_ms|.*_ts)$")
_ACCOUNT_KEYS = frozenset(
    {"organization", "organisation", "project", "user", "workspace", "safety_identifier"}
)
_PREFIX = re.compile(r"^([a-z]{2,10}[_-])")

# Strings that must never be in a fixture.
_KEY_SHAPE = re.compile(r"(?<![A-Za-z0-9_-])(?:sk-ant-|sk-|xai-)[A-Za-z0-9_-]{8,}")
_BEARER = re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/-]{8,}")
_ACCOUNT_ID = re.compile(r"(?<![A-Za-z0-9])(?:org|proj|user|ws|wrkspc)[-_][A-Za-z0-9]{8,}")


def _synthetic_id(value: Any) -> Any:
    if value is None:
        return None
    m = _PREFIX.match(str(value))
    return (m.group(1) if m else "") + SYNTHETIC


def _scrub_body(node: Any) -> Any:
    if isinstance(node, dict):
        out: dict[str, Any] = {}
        for k, v in node.items():
            if k in _ACCOUNT_KEYS:
                continue
            if _ID_KEY.match(k) and not isinstance(v, dict | list):
                out[k] = _synthetic_id(v)
            elif (
                _TIME_KEY.match(k) and isinstance(v, int | float | str) and not isinstance(v, bool)
            ):
                out[k] = FIXED_TIME
            else:
                out[k] = _scrub_body(v)
        return out
    if isinstance(node, list):
        return [_scrub_body(v) for v in node]
    return node


def scrub(raw: dict[str, Any]) -> dict[str, Any]:
    """The fixture for a recorded reply. It may still fail check() (a key
    inside the text, say): the caller must check before writing."""
    headers: dict[str, str] = {}
    for k, v in dict(raw.get("headers", {})).items():
        name = str(k).lower()
        if name in KEEP_HEADERS:
            headers[name] = str(v)
        elif name in ID_HEADERS:
            headers[name] = _synthetic_id(v) if str(v) else ""
    return {"status": raw.get("status"), "headers": headers, "body": _scrub_body(raw.get("body"))}


def _strings(node: Any) -> list[str]:
    if isinstance(node, dict):
        return [s for k, v in node.items() for s in (str(k), *_strings(v))]
    if isinstance(node, list):
        return [s for v in node for s in _strings(v)]
    return [node] if isinstance(node, str) else []


def _check_body(node: Any, problems: list[str]) -> None:
    if isinstance(node, dict):
        for k, v in node.items():
            if k in _ACCOUNT_KEYS:
                if v is not None:
                    problems.append(f"account field {k}")
            elif _ID_KEY.match(k) and not isinstance(v, dict | list):
                if v is not None and "SYNTHETIC" not in str(v):
                    problems.append(f"id field {k} is not SYNTHETIC")
            elif (
                _TIME_KEY.match(k) and isinstance(v, int | float | str) and not isinstance(v, bool)
            ):
                if v != FIXED_TIME:
                    problems.append(f"timing field {k} is not the fixed time")
            else:
                _check_body(v, problems)
    elif isinstance(node, list):
        for v in node:
            _check_body(v, problems)


def check(fx: Any) -> list[str]:
    """The rules fx breaks (empty when it is a clean fixture)."""
    if not isinstance(fx, dict):
        return ["not a {status, headers, body} object"]
    problems: list[str] = []
    if set(fx) != {"status", "headers", "body"}:
        problems.append("top-level fields are not exactly status, headers, body")
    if type(fx.get("status")) is not int:
        problems.append("status is not an integer")
    headers = fx.get("headers", {})
    if not isinstance(headers, dict):
        problems.append("headers is not an object")
        headers = {}
    for k, v in headers.items():
        name = str(k).lower()
        if name != k or name not in HEADERS:
            problems.append(f"header {k!s:.40} is not on the list")
        elif name in ID_HEADERS and v and "SYNTHETIC" not in str(v):
            problems.append(f"header {name} is not SYNTHETIC")
    _check_body(fx.get("body"), problems)
    for s in _strings(fx):
        if _KEY_SHAPE.search(s):
            problems.append("a string is shaped like a provider key")
        if _BEARER.search(s):
            problems.append("a string holds a bearer token")
        m = _ACCOUNT_ID.search(s)
        if m and "SYNTHETIC" not in m.group(0):
            problems.append("a string holds an organisation, project or user id")
    return sorted(set(problems))


def _load(path: Path) -> Any:
    return json.loads(path.read_text("utf-8"))


def main(argv: list[str]) -> int:
    if argv[:1] == ["--check"] and len(argv) > 1:
        failed = 0
        for name in argv[1:]:
            try:
                problems = check(_load(Path(name)))
            except (OSError, ValueError):
                problems = ["not readable JSON"]
            for p in problems:
                print(f"{name}: {p}")
            failed += bool(problems)
        if failed:
            print(f"{failed} fixture(s) failed the scrubber check", file=sys.stderr)
        return 1 if failed else 0
    if len(argv) == 2 and not argv[0].startswith("-"):
        src, dst = Path(argv[0]), Path(argv[1])
        try:
            fx = scrub(_load(src))
        except (OSError, ValueError, AttributeError):
            print(f"{src}: not a readable recorded reply", file=sys.stderr)
            return 1
        problems = check(fx)
        if problems:
            for p in problems:
                print(f"{src}: {p} (nothing written)", file=sys.stderr)
            return 1
        dst.write_text(json.dumps(fx, indent=2, ensure_ascii=False) + "\n", "utf-8")
        return 0
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
