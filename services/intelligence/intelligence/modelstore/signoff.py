"""``signoff.json`` (``mhai.models.signoff/1``): the record that a pinned version was approved.

Written only by the sign-off workflow after a merge (Task 21), next to ``signoff.sig``
(purpose ``signoff``, the sign-off signer). This module holds its strict parser, which the
load-time verifier (Task 22) uses; Task 21 adds the writer here.

The shape (canonical JSON, integers and strings only, exactly these keys)::

    schema, name, version, role, gate,
    manifest_sha256, evaluation_sha256,
    pr, merge_commit,
    clinical_reviewers: [login, ...], platform_reviewers: [login, ...]   sorted, distinct
    promotion_actor, trained_by, at

Anything else is ``not_approved``: a sign-off that can't be read is no sign-off.
"""

import re
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from intelligence.doceval.report import GATES
from intelligence.modelstore import canonical, names
from intelligence.modelstore.reasons import Refused

SCHEMA = "mhai.models.signoff/1"
MAX_BYTES = 64 * 1024
MAX_REVIEWERS = 16

_KEYS = frozenset(
    {
        "schema",
        "name",
        "version",
        "role",
        "gate",
        "manifest_sha256",
        "evaluation_sha256",
        "pr",
        "merge_commit",
        "clinical_reviewers",
        "platform_reviewers",
        "promotion_actor",
        "trained_by",
        "at",
    }
)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_COMMIT = re.compile(r"[0-9a-f]{40}", re.ASCII)
_LOGIN = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}", re.ASCII)
_TIMESTAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", re.ASCII)
_MAX_PR = 2**31 - 1


def _invalid(detail: str) -> Refused:
    return Refused("not_approved", detail_code=detail)


def _match(value: object, pattern: re.Pattern[str]) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _named(check: Any, value: object, detail: str) -> None:
    failed = False
    try:
        check(value)
    except Refused:
        failed = True
    if failed:
        raise _invalid(detail)


def _logins(value: object, detail: str) -> tuple[str, ...]:
    if (
        not isinstance(value, list)
        or not 1 <= len(value) <= MAX_REVIEWERS
        or not all(_match(v, _LOGIN) for v in value)
        or any(a >= b for a, b in zip(value, value[1:], strict=False))
    ):
        raise _invalid(detail)
    return tuple(value)


def _timestamp(value: object) -> str:
    if not _match(value, _TIMESTAMP):
        raise _invalid("at")
    failed = False
    try:
        datetime.strptime(str(value), "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        failed = True
    if failed:
        raise _invalid("at")
    return str(value)


@dataclass(frozen=True, slots=True)
class Signoff:
    name: str
    version: str
    role: str
    gate: str
    manifest_sha256: str
    evaluation_sha256: str
    pr: int
    merge_commit: str
    clinical_reviewers: tuple[str, ...]
    platform_reviewers: tuple[str, ...]
    promotion_actor: str
    trained_by: str
    at: str


def parse(b: bytes) -> Signoff:
    """A sign-off from its stored bytes: strict canonical JSON, exactly the keys above,
    every value to its pattern; ``not_approved`` otherwise."""
    failed = False
    try:
        obj = canonical.loads_strict(bytes(b), MAX_BYTES)
    except (Refused, TypeError):
        failed = True
    if failed:
        raise _invalid("json")
    if canonical.dumps(obj) != bytes(b):
        raise _invalid("not_canonical")
    if not isinstance(obj, dict) or set(obj) != _KEYS:
        raise _invalid("keys")
    if obj["schema"] != SCHEMA:
        raise _invalid("schema")
    _named(names.check_name, obj["name"], "name")
    _named(names.check_version, obj["version"], "version")
    _named(names.check_role, obj["role"], "role")
    if obj["gate"] not in GATES:
        raise _invalid("gate")
    for key in ("manifest_sha256", "evaluation_sha256"):
        if not _match(obj[key], _SHA256):
            raise _invalid(key)
    pr = obj["pr"]
    if not isinstance(pr, int) or isinstance(pr, bool) or not 1 <= pr <= _MAX_PR:
        raise _invalid("pr")
    if not _match(obj["merge_commit"], _COMMIT):
        raise _invalid("merge_commit")
    for key in ("promotion_actor", "trained_by"):
        if not _match(obj[key], _LOGIN):
            raise _invalid(key)
    return Signoff(
        name=obj["name"],
        version=obj["version"],
        role=obj["role"],
        gate=obj["gate"],
        manifest_sha256=obj["manifest_sha256"],
        evaluation_sha256=obj["evaluation_sha256"],
        pr=pr,
        merge_commit=obj["merge_commit"],
        clinical_reviewers=_logins(obj["clinical_reviewers"], "clinical_reviewers"),
        platform_reviewers=_logins(obj["platform_reviewers"], "platform_reviewers"),
        promotion_actor=obj["promotion_actor"],
        trained_by=obj["trained_by"],
        at=_timestamp(obj["at"]),
    )


def to_bytes(s: Signoff) -> bytes:
    """The canonical bytes of ``s`` (what is stored and signed). Parsed back strictly."""
    obj = {
        "schema": SCHEMA,
        "name": s.name,
        "version": s.version,
        "role": s.role,
        "gate": s.gate,
        "manifest_sha256": s.manifest_sha256,
        "evaluation_sha256": s.evaluation_sha256,
        "pr": s.pr,
        "merge_commit": s.merge_commit,
        "clinical_reviewers": list(s.clinical_reviewers),
        "platform_reviewers": list(s.platform_reviewers),
        "promotion_actor": s.promotion_actor,
        "trained_by": s.trained_by,
        "at": s.at,
    }
    b = canonical.dumps(obj)
    parse(b)
    return b


__all__ = ["MAX_BYTES", "SCHEMA", "Signoff", "parse", "to_bytes"]
