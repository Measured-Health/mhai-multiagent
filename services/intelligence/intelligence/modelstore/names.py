"""Validation of names, versions, roles and run ids.

Inputs are names, never paths. Everything is checked before any I/O, with
ASCII-only full matches (no trailing newline, no Unicode digits).
"""

import re
from dataclasses import dataclass

from intelligence.modelstore.reasons import Refused

_NAME = re.compile(r"[a-z0-9][a-z0-9-]{1,62}", re.ASCII)
_VERSION = re.compile(r"(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})", re.ASCII)
_ROLE = re.compile(r"[a-z][a-z0-9_]{1,31}", re.ASCII)
_RUN_ID = re.compile(r"[A-Za-z0-9_-]{1,64}", re.ASCII)


def _check(value: str, pattern: re.Pattern[str]) -> str:
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise Refused("invalid_name")
    return value


def check_name(s: str) -> str:
    return _check(s, _NAME)


def check_version(s: str) -> str:
    return _check(s, _VERSION)


def check_role(s: str) -> str:
    return _check(s, _ROLE)


def check_run_id(s: str) -> str:
    return _check(s, _RUN_ID)


@dataclass(frozen=True, order=True)
class Version:
    """A semantic version, ordered numerically."""

    major: int
    minor: int
    patch: int

    @classmethod
    def parse(cls, s: str) -> "Version":
        check_version(s)
        major, minor, patch = (int(p) for p in s.split("."))
        return cls(major, minor, patch)

    def __str__(self) -> str:
        return f"{self.major}.{self.minor}.{self.patch}"
