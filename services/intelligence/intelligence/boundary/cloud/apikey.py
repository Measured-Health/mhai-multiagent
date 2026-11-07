"""Provider API keys: read from files only, held redacted, kept out of logs.

A key reaches memory through load_api_key and nowhere else (never the
environment: settings.SDK_ENV_REFUSED). It is held as ApiKey, whose repr
and str are redacted and which can't be pickled or copied. RedactingFilter
is the backstop on every root handler: anything shaped like a provider key,
and each loaded key's exact value, becomes [REDACTED] in the message, its
arguments and the exception text (design section 7; abuse case A10).
"""

from __future__ import annotations

import logging
import re
import threading
from collections.abc import Iterable
from typing import Any, NoReturn, final

from intelligence.boundary import config

MIN_KEY_LEN = 20
REDACTED = "[REDACTED]"
# The SDKs' own loggers, and the HTTP stack under them: at DEBUG they log
# requests. Set to WARNING at start (design section 7).
SDK_LOGGERS = ("openai", "anthropic", "httpx", "httpcore", "httpx2", "httpcore2")

# Provider key shapes. The lookbehind keeps words such as "risk-assessment"
# whole; a key is preceded by a space, a quote, "=" or ":" in practice.
_KEY_SHAPES = re.compile(r"(?<![A-Za-z0-9_-])(?:sk-ant-|sk-|xai-)[A-Za-z0-9_-]{8,}")


@final
class ApiKey:
    """A provider key. repr, str and format show ApiKey(<redacted>); only
    reveal() gives the value, at the point of use. It refuses pickling and
    copying, so it can't land in a cache, a queue or a dead letter."""

    __slots__ = ("_value",)
    _value: str

    def __init__(self, value: str) -> None:
        if not isinstance(value, str):
            raise TypeError("ApiKey takes a str")
        object.__setattr__(self, "_value", value)

    def reveal(self) -> str:
        return self._value

    def __repr__(self) -> str:
        return "ApiKey(<redacted>)"

    __str__ = __repr__

    def __format__(self, spec: str) -> str:
        return repr(self)

    def __setattr__(self, name: str, value: object) -> NoReturn:
        raise AttributeError("ApiKey is immutable")

    def __delattr__(self, name: str) -> NoReturn:
        raise AttributeError("ApiKey is immutable")

    def __reduce__(self) -> NoReturn:
        raise TypeError("ApiKey can't be pickled")

    def __reduce_ex__(self, protocol: Any) -> NoReturn:
        raise TypeError("ApiKey can't be pickled")

    def __copy__(self) -> NoReturn:
        raise TypeError("ApiKey can't be copied")

    def __deepcopy__(self, memo: Any) -> NoReturn:
        raise TypeError("ApiKey can't be copied")


def load_api_key(path: str, name: str, development: bool, errs: list[str]) -> ApiKey | None:
    """The key in the file at path, or None with an error naming name (the
    variable) and never the value. The file must be a regular file that
    others can't read (checked with fstat on the descriptor that is read)
    and, outside development, that group and others can't write. The key
    must not be empty, a placeholder, contain whitespace or a character
    outside printable ASCII, or be shorter than MIN_KEY_LEN."""
    raw = config._read_secret_file(path, name, development, errs, refuse_others_read=True)
    if raw is None:
        return None
    problem = None
    if not raw:
        problem = "is empty"
    elif config.is_placeholder(raw):
        problem = "is a placeholder"
    elif any(c.isspace() for c in raw):
        problem = "contains whitespace"
    elif not (raw.isascii() and raw.isprintable()):
        problem = "contains a character a key can't have"
    elif len(raw) < MIN_KEY_LEN:
        problem = f"is too short (at least {MIN_KEY_LEN} characters)"
    if problem is not None:
        errs.append(f"{name} {problem}")
        return None
    return ApiKey(raw)


class RedactingFilter(logging.Filter):
    """Rewrites a record so that provider-shaped keys and each known key's
    exact value become [REDACTED]: the message with its arguments merged
    in (args then None), the exception text (rendered here, exc_info then
    None) and the stack text. It never drops a record and never raises."""

    def __init__(self, keys: Iterable[ApiKey] = ()) -> None:
        super().__init__()
        self._lock = threading.Lock()
        self._exact: tuple[str, ...] = ()
        self.add_keys(keys)

    def add_keys(self, keys: Iterable[ApiKey]) -> None:
        with self._lock:
            values = set(self._exact) | {k.reveal() for k in keys if k.reveal()}
            # Longest first, so a key that contains another is replaced whole.
            self._exact = tuple(sorted(values, key=len, reverse=True))

    def redact(self, text: str) -> str:
        for value in self._exact:
            text = text.replace(value, REDACTED)
        return _KEY_SHAPES.sub(REDACTED, text)

    def filter(self, record: logging.LogRecord) -> bool:
        try:
            message = record.getMessage()
        except Exception:
            # A bad call: keep what was passed, redacted, rather than let the
            # handler print the raw arguments in its error report.
            args = record.args if isinstance(record.args, tuple) else (record.args,)
            message = " ".join([str(record.msg), *(str(a) for a in args)])
        record.msg = self.redact(message)
        record.args = None
        if record.exc_info:
            if not record.exc_text:
                record.exc_text = logging.Formatter().formatException(record.exc_info)
            record.exc_info = None
        if record.exc_text:
            record.exc_text = self.redact(record.exc_text)
        if record.stack_info:
            record.stack_info = self.redact(record.stack_info)
        return True


_FILTER = RedactingFilter()


def install_redaction(keys: Iterable[ApiKey] = ()) -> RedactingFilter:
    """Put the one shared RedactingFilter on every root handler (and on
    logging's last-resort handler when the root has none), adding keys to
    the exact values it removes. Calling it again adds keys, never a second
    filter."""
    _FILTER.add_keys(keys)
    handlers = list(logging.getLogger().handlers)
    if logging.lastResort is not None:
        handlers.append(logging.lastResort)
    for h in handlers:
        if _FILTER not in h.filters:
            h.addFilter(_FILTER)
    return _FILTER


def quiet_sdk_loggers() -> None:
    """SDK and HTTP debug logging off: those loggers log requests."""
    for name in SDK_LOGGERS:
        logging.getLogger(name).setLevel(logging.WARNING)
