"""Which failures are retried, and how long to wait (design sections 10
and 11; plan Task 8).

classify maps a status, an exception or an adapter's outcome to RETRY or
to a final outcome from the closed set (vocab.OUTCOMES). Only a connection
error, a timeout, 429, 500, 502, 503, 504 and a provider's "overloaded"
(Anthropic 529) are retried. A final outcome is never retried (K12): 400,
401, 403, 404, a refusal, a truncation, a malformed reply, a blocked
egress. Anything classify doesn't know is "unavailable": final, and inside
the set, so nothing outside it reaches core.

A blocked egress anywhere in an exception's chain wins (Review Focus 2):
anthropic wraps the scan hook's EgressRefused in APIConnectionError, which
would otherwise read as a connection error and be retried.

RetryPolicy decides the wait: at most max_extra more attempts, full jitter
(uniform(0, delay)) over 1 s then 3 s, or the provider's Retry-After when
it fits in the time left. When no delay fits, it says stop. Each attempt
reserves its own budget (section 10): that is the client's loop (Task 9),
which asks next_delay only after the failed attempt has settled.
"""

from __future__ import annotations

import math
import random
import re
from dataclasses import dataclass, field
from typing import Final

import anthropic
import httpx2
import openai

from intelligence.boundary.cloud.gate import EgressRefused
from intelligence.boundary.cloud.vocab import OUTCOMES

RETRY: Final = "retry"

_RETRY_STATUS: Final = frozenset({429, 500, 502, 503, 504, 529})
_AUTH_STATUS: Final = frozenset({401, 403})
_MALFORMED_STATUS: Final = frozenset({400, 404, 422})

# Retried transport errors: the connection failed or timed out, or the far
# end broke the protocol. A local protocol error, an unsupported scheme or
# a proxy refusal is our own configuration: final.
_RETRY_EXC: Final = (
    httpx2.NetworkError,
    httpx2.TimeoutException,
    httpx2.RemoteProtocolError,
    TimeoutError,
    ConnectionError,
)
_SDK_STATUS: Final = (openai.APIStatusError, anthropic.APIStatusError)
_SDK_CONNECTION: Final = (openai.APIConnectionError, anthropic.APIConnectionError)
_SDK_VALIDATION: Final = (openai.APIResponseValidationError, anthropic.APIResponseValidationError)
_MAX_CHAIN: Final = 32


def _status(code: int) -> str:
    if code in _RETRY_STATUS:
        return RETRY
    if code in _AUTH_STATUS:
        return "auth_failed"
    if code in _MALFORMED_STATUS:
        return "malformed"
    return "unavailable"  # every other status, a redirect and a success too


def _chain(exc: BaseException) -> list[BaseException]:
    """exc and its causes and contexts, each once, at most _MAX_CHAIN."""
    seen: list[BaseException] = []
    todo: list[BaseException] = [exc]
    while todo and len(seen) < _MAX_CHAIN:
        e = todo.pop()
        if any(e is s for s in seen):
            continue
        seen.append(e)
        for nxt in (e.__context__, e.__cause__):
            if nxt is not None:
                todo.append(nxt)
    return seen


def _exception(exc: BaseException) -> str:
    if any(isinstance(e, EgressRefused) for e in _chain(exc)):
        return "egress_blocked"
    if isinstance(exc, _SDK_STATUS):
        return _status(exc.status_code)
    if isinstance(exc, _SDK_VALIDATION):
        return "malformed"
    if isinstance(exc, _SDK_CONNECTION):
        # The SDKs raise this from the transport's own error; that decides.
        # With none (or a timeout), it is a connection error.
        inner = exc.__cause__
        if inner is None or isinstance(exc, openai.APITimeoutError | anthropic.APITimeoutError):
            return RETRY
        return _exception(inner)
    if isinstance(exc, _RETRY_EXC):
        return RETRY
    return "unavailable"


def classify(exc_or_status: BaseException | int | str) -> str:
    """RETRY, or a final outcome from vocab.OUTCOMES."""
    x = exc_or_status
    if isinstance(x, BaseException):
        return _exception(x)
    if type(x) is int:
        return _status(x)
    if type(x) is str and (x == RETRY or x in OUTCOMES):
        return x
    return "unavailable"


def final_outcome(x: str) -> str:
    """The outcome recorded when retrying stops: RETRY (retries used up) is
    "unavailable", and anything outside the closed set is too."""
    return x if x in OUTCOMES else "unavailable"


def _finite_nonneg(v: float) -> bool:
    return isinstance(v, int | float) and math.isfinite(v) and v >= 0


@dataclass(frozen=True)
class RetryPolicy:
    max_extra: int = 2
    delays: tuple[float, ...] = (1.0, 3.0)
    rng: random.Random = field(default_factory=random.SystemRandom, repr=False, compare=False)

    def __post_init__(self) -> None:
        if type(self.max_extra) is not int or self.max_extra < 0:
            raise ValueError("max_extra")
        if not self.delays or not all(_finite_nonneg(d) for d in self.delays):
            raise ValueError("delays")

    def next_delay(self, attempt: int, retry_after: float | None, left: float) -> float | None:
        """The wait before the next attempt, or None to stop.

        attempt is the number of attempts made so far (1 after the first).
        retry_after is the provider's Retry-After in seconds, if any; left
        is the stage's time left in seconds. Retry-After is used only when
        it fits in left; when it doesn't, stop rather than retry earlier
        than the provider asked. Without it, full jitter, bounded by left.
        """
        if type(attempt) is not int or attempt < 1:
            raise ValueError("attempt")
        if attempt > self.max_extra:
            return None
        if not (isinstance(left, int | float) and left > 0 and math.isfinite(left)):
            return None
        if retry_after is not None and not math.isnan(retry_after) and retry_after >= 0:
            return float(retry_after) if retry_after < left else None
        base = self.delays[min(attempt, len(self.delays)) - 1]
        d = self.rng.uniform(0.0, base)
        if d < left:
            return d
        return self.rng.uniform(0.0, left) * 0.999  # always fits: strictly under left


_DELTA_SECONDS = re.compile(r"[0-9]{1,6}")


def parse_retry_after(value: str | None) -> float | None:
    """A Retry-After header's delta-seconds (ASCII digits only), or None.
    An HTTP date or anything else is ignored: the jitter is used."""
    if value is None:
        return None
    v = value.strip()
    if not _DELTA_SECONDS.fullmatch(v):
        return None
    return float(int(v))
