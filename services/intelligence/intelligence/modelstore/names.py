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
# An object store version id. S3's are opaque, URL-ready and at most 1,024 bytes (letters,
# digits, ".", "_" and "-" in practice); MinIO's are UUIDs. ``null`` (an object written
# while versioning was off) is refused: every object we write is a locked version. A
# version id is only ever passed as the ``VersionId`` parameter, never joined into a key,
# a path or a URL.
_STORE_VERSION_ID = re.compile(r"[A-Za-z0-9._-]{1,1024}", re.ASCII)
# A GitHub login, lowercase only. GitHub logins are case-insensitive (``Alice`` and
# ``alice`` are one account), so every login we store or compare is the lowercase
# spelling and anything else is refused: then exact comparison means "same person"
# (ruling D30). Alphanumerics and single inner hyphens, at most 39 characters.
_LOGIN = re.compile(r"[a-z0-9](?:[a-z0-9]|-(?=[a-z0-9])){0,38}", re.ASCII)


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


def is_login(value: object) -> bool:
    """A lowercase GitHub login (see :data:`_LOGIN`)."""
    return isinstance(value, str) and _LOGIN.fullmatch(value) is not None


def check_login(s: str) -> str:
    """A lowercase GitHub login, or ``invalid_name``. Upper case is refused, not folded:
    the writer must store the lowercase spelling."""
    if not is_login(s):
        raise Refused("invalid_name")
    return s


def check_store_version_id(s: str) -> str:
    """An object store version id (see :data:`_STORE_VERSION_ID`), or ``invalid_name``."""
    _check(s, _STORE_VERSION_ID)
    if s == "null":
        raise Refused("invalid_name")
    return s


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
