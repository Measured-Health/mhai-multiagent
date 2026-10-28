"""The operations table (spec §8.1), read from the generated operations.json.

Python writes no permission rule: every allow comes from a field core's
permgen wrote, and every field is required, so nothing is inferred from a
missing one. The file is checked strictly at start-up and a bad one stops
the worker. Test-only operations are left out unless APP_ENV names
development (an unset APP_ENV is production, as core's CORE_ENV).
"""

from __future__ import annotations

import re
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from importlib import resources
from pathlib import Path
from typing import Any

from intelligence.boundary import _strictjson, config

MAX_FILE = 1 << 20
MAX_TIMEOUT_MS = {"req": 60_000, "job": 24 * 3600 * 1000}

_NAME = re.compile(r"[a-z0-9_]+")
_FIELDS: dict[str, type] = {
    "name": str,
    "service": str,
    "op": str,
    "kind": str,
    "any_role": list,
    "svc_core": bool,
    "needs_hospital": bool,
    "bff_direct": bool,
    "timeout_ms": int,
    "request_schema": str,
    "response_schema": str,
    "test_only": bool,
}


class OperationsError(Exception):
    """operations.json (or a handler list) is not what the worker can run."""


@dataclass(frozen=True)
class Operation:
    name: str
    service: str
    op: str
    kind: str
    any_role: tuple[str, ...]
    svc_core: bool
    needs_hospital: bool
    bff_direct: bool
    timeout_ms: int
    request_schema: str
    response_schema: str
    test_only: bool


def _packaged(name: str) -> bytes:
    return (resources.files("intelligence.boundary") / name).read_bytes()


def clinical_roles() -> frozenset[str]:
    """The roles an operation may name, from the generated catalogue.json."""
    doc = _strictjson.loads(_packaged("catalogue.json"))
    roles = doc.get("clinical_roles") if isinstance(doc, dict) else None
    if not _strictjson.is_str_list(roles) or not roles:
        raise OperationsError("catalogue.json has no clinical_roles")
    return frozenset(roles)


def _check_type(value: Any, want: type) -> bool:
    if want is int:
        return _strictjson.is_int(value)
    return type(value) is want


def _row(raw: Any, roles: frozenset[str]) -> Operation:
    if not isinstance(raw, dict) or set(raw) != set(_FIELDS):
        raise OperationsError("an operation has missing or unknown fields")
    for field, want in _FIELDS.items():
        if not _check_type(raw[field], want):
            raise OperationsError(f"operation field {field} has the wrong type")
    name, service, op, kind = raw["name"], raw["service"], raw["op"], raw["kind"]
    if not (_NAME.fullmatch(service) and _NAME.fullmatch(op)) or name != f"{service}.{op}":
        raise OperationsError("an operation name is not service.op")
    if kind not in MAX_TIMEOUT_MS:
        raise OperationsError(f"operation {name} has an unknown kind")
    if not 0 < raw["timeout_ms"] <= MAX_TIMEOUT_MS[kind]:
        raise OperationsError(f"operation {name} timeout is out of range")
    any_role = raw["any_role"]
    if not _strictjson.is_str_list(any_role) or len(set(any_role)) != len(any_role):
        raise OperationsError(f"operation {name} any_role is not a list of distinct roles")
    if not set(any_role) <= roles:
        raise OperationsError(f"operation {name} names a role that is not a clinical role")
    if not raw["request_schema"]:
        raise OperationsError(f"operation {name} has no request schema")
    if raw["bff_direct"] and (kind != "req" or not any_role):
        raise OperationsError(f"operation {name}: bff_direct needs request/reply and a role")
    return Operation(
        name=name,
        service=service,
        op=op,
        kind=kind,
        any_role=tuple(any_role),
        svc_core=raw["svc_core"],
        needs_hospital=raw["needs_hospital"],
        bff_direct=raw["bff_direct"],
        timeout_ms=raw["timeout_ms"],
        request_schema=raw["request_schema"],
        response_schema=raw["response_schema"],
        test_only=raw["test_only"],
    )


class Operations:
    """The loaded table. Build it with Operations.load."""

    def __init__(self, ops: Iterable[Operation]) -> None:
        self._by_name: dict[str, Operation] = {}
        self._jobs: dict[str, Operation] = {}
        for o in ops:
            if o.name in self._by_name:
                raise OperationsError(f"duplicate operation {o.name}")
            self._by_name[o.name] = o
            if o.kind == "job":
                # jobs.v1.<hid>.<op> names no service, so a job op is unique.
                if o.op in self._jobs:
                    raise OperationsError(f"two job operations named {o.op}")
                self._jobs[o.op] = o

    @classmethod
    def load(cls, path: str | Path | None = None, *, development: bool | None = None) -> Operations:
        """Read operations.json (the packaged copy by default), strictly."""
        if development is None:
            development = config.is_development()
        try:
            if path is None:
                raw = _packaged("operations.json")
            else:
                with open(path, "rb") as f:
                    raw = f.read(MAX_FILE + 1)
        except OSError:
            raise OperationsError("cannot read operations.json") from None
        if len(raw) > MAX_FILE:
            raise OperationsError("operations.json is too large")
        try:
            rows = _strictjson.loads(raw)
        except _strictjson.StrictJSONError:
            raise OperationsError("operations.json is not strict JSON") from None
        if not isinstance(rows, list) or not rows:
            raise OperationsError("operations.json is not a non-empty list")
        roles = clinical_roles()
        ops = [_row(r, roles) for r in rows]
        # Validate everything, then leave test-only rows out outside development.
        table = cls(ops)
        if development:
            return table
        return cls(o for o in ops if not o.test_only)

    def __iter__(self) -> Iterator[Operation]:
        return iter(self._by_name.values())

    def by_name(self, name: str) -> Operation | None:
        return self._by_name.get(name)

    def for_subject(self, subject: str) -> Operation | None:
        """The operation a subject names: inference.v1.<service>.<op> for
        request/reply, jobs.v1.<hid|global>.<op> for jobs. None otherwise."""
        seg = subject.split(".")
        if len(seg) != 4 or seg[1] != "v1":
            return None
        if seg[0] == "inference":
            o = self._by_name.get(f"{seg[2]}.{seg[3]}")
            return o if o is not None and o.kind == "req" else None
        if seg[0] == "jobs":
            return self._jobs.get(seg[3])
        return None

    def require(self, service: str, handler_names: Iterable[str]) -> None:
        """Refuse to start if a handler (an op name of service) has no entry."""
        for h in handler_names:
            o = self._by_name.get(f"{service}.{h}")
            if o is None or o.service != service:
                raise OperationsError(f"handler {service}.{h} has no operation entry")
