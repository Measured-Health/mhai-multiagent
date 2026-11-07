"""Retries (design section 10; plan Task 8; K12).

classify gives "retry" only for a connection error, a timeout, 429, 500,
502, 503, 504 and a provider's "overloaded" (Anthropic 529). Everything
else is a final outcome from the closed set (vocab.OUTCOMES), and a final
outcome is never retried. Backoff is full jitter (1 s, then 3 s), or
Retry-After when it fits in the time left; nothing fits, nothing is retried.
"""

from __future__ import annotations

import math
import random

import anthropic
import httpx2
import openai
import pytest

from intelligence.boundary.cloud import retry, vocab
from intelligence.boundary.cloud.gate import EgressRefused
from intelligence.boundary.cloud.retry import RETRY, RetryPolicy, classify, parse_retry_after

pytestmark = pytest.mark.disable_socket

REQ = httpx2.Request("POST", "https://api.example.invalid/v1/x")


def _status(sdk: object, code: int) -> Exception:
    err = sdk.APIStatusError  # type: ignore[attr-defined]
    return err("status", response=httpx2.Response(code, request=REQ), body=None)  # type: ignore[no-any-return]


STATUS_TABLE = [
    (429, RETRY),
    (500, RETRY),
    (502, RETRY),
    (503, RETRY),
    (504, RETRY),
    (529, RETRY),
    (401, "auth_failed"),
    (403, "auth_failed"),
    (400, "malformed"),
    (404, "malformed"),
    (422, "malformed"),
    (402, "unavailable"),
    (405, "unavailable"),
    (408, "unavailable"),
    (409, "unavailable"),
    (413, "unavailable"),
    (501, "unavailable"),
    (505, "unavailable"),
    (599, "unavailable"),
    # Not an error status at all (a redirect not followed, A17; a success
    # passed by mistake): never "ok", never a retry.
    (200, "unavailable"),
    (301, "unavailable"),
    (302, "unavailable"),
    (0, "unavailable"),
    (-1, "unavailable"),
    (1000, "unavailable"),
]


@pytest.mark.parametrize(("status", "want"), STATUS_TABLE)
def test_classify_table(status: int, want: str) -> None:
    assert classify(status) == want
    # The same status inside each SDK's status error.
    assert classify(_status(openai, status if 100 <= status <= 599 else 500)) == classify(
        status if 100 <= status <= 599 else 500
    )
    assert classify(_status(anthropic, status if 100 <= status <= 599 else 500)) == classify(
        status if 100 <= status <= 599 else 500
    )


def test_classify_exceptions() -> None:
    retryable = [
        httpx2.ConnectError("x"),
        httpx2.ReadError("x"),
        httpx2.WriteError("x"),
        httpx2.ConnectTimeout("x"),
        httpx2.ReadTimeout("x"),
        httpx2.WriteTimeout("x"),
        httpx2.PoolTimeout("x"),
        httpx2.RemoteProtocolError("x"),
        TimeoutError(),
        ConnectionResetError(),
        openai.APIConnectionError(request=REQ),
        openai.APITimeoutError(request=REQ),
        anthropic.APIConnectionError(request=REQ),
        anthropic.APITimeoutError(request=REQ),
    ]
    for e in retryable:
        assert classify(e) == RETRY, type(e).__name__
    final = {
        httpx2.LocalProtocolError("x"): "unavailable",
        httpx2.ProxyError("x"): "unavailable",
        httpx2.UnsupportedProtocol("x"): "unavailable",
        ValueError("x"): "unavailable",
        RuntimeError("x"): "unavailable",
        openai.APIResponseValidationError(httpx2.Response(200, request=REQ), None): "malformed",
        anthropic.APIResponseValidationError(httpx2.Response(200, request=REQ), None): "malformed",
    }
    for e, want in final.items():
        assert classify(e) == want, type(e).__name__


def _wrapped(sdk: object, inner: BaseException) -> Exception:
    """An SDK's connection error raised from inner, as both SDKs do."""
    try:
        try:
            raise inner
        except BaseException as e:
            raise sdk.APIConnectionError(request=REQ) from e  # type: ignore[attr-defined]
    except Exception as outer:
        return outer
    raise AssertionError


def test_wrapped_egress_refusal_is_final() -> None:
    # Review Focus 2 and K11: anthropic wraps the hook's refusal in
    # APIConnectionError; it must not look like a connection error.
    assert classify(EgressRefused("known_identifier")) == "egress_blocked"
    for sdk in (openai, anthropic):
        assert classify(_wrapped(sdk, EgressRefused("shape"))) == "egress_blocked"
    # As __context__ only, and deep in a chain.
    try:
        try:
            raise EgressRefused("gate")
        except EgressRefused:
            raise httpx2.ConnectError("x")  # noqa: B904
    except httpx2.ConnectError as e:
        assert classify(e) == "egress_blocked"
    deep: BaseException = EgressRefused("gate")
    for _ in range(5):
        nxt = httpx2.ConnectError("x")
        nxt.__cause__ = deep
        deep = nxt
    assert classify(deep) == "egress_blocked"


def test_wrapped_cause_decides() -> None:
    # A connection error caused by something final stays final.
    assert classify(_wrapped(openai, httpx2.ProxyError("x"))) == "unavailable"
    assert classify(_wrapped(anthropic, httpx2.ConnectError("x"))) == RETRY


def test_cyclic_exception_chain_terminates() -> None:
    a = httpx2.ConnectError("a")
    b = httpx2.ConnectError("b")
    a.__cause__ = b
    b.__cause__ = a
    assert classify(a) == RETRY


def test_never_retry_on_400_401_403_404_refusal_truncated_malformed_egress() -> None:
    # K12.
    for status in (400, 401, 403, 404):
        assert classify(status) != RETRY
    for outcome in vocab.OUTCOMES:
        assert classify(outcome) == outcome
        assert classify(outcome) != RETRY
    for outcome in ("refused", "truncated", "malformed", "egress_blocked"):
        assert classify(outcome) == outcome
    # An adapter's "retry" passes through; anything outside the set is
    # final and inside it.
    assert classify(RETRY) == RETRY
    assert classify("surprise") == "unavailable"
    assert classify(True) == "unavailable"  # type: ignore[arg-type]
    assert classify(None) == "unavailable"  # type: ignore[arg-type]
    for x in (*range(0, 1000, 7), "x", ValueError()):
        out = classify(x)
        assert out == RETRY or out in vocab.OUTCOMES
    assert RETRY not in vocab.OUTCOMES


def test_final_outcome_is_in_the_closed_set() -> None:
    assert retry.final_outcome(RETRY) == "unavailable"
    assert retry.final_outcome("timeout") == "timeout"
    assert retry.final_outcome("surprise") == "unavailable"
    for o in vocab.OUTCOMES:
        assert retry.final_outcome(o) == o


class _FixedRng(random.Random):
    """uniform returns the given fraction of the range."""

    def __init__(self, frac: float) -> None:
        super().__init__(0)
        self.frac = frac
        self.calls: list[tuple[float, float]] = []

    def uniform(self, a: float, b: float) -> float:
        self.calls.append((a, b))
        return a + (b - a) * self.frac


def test_full_jitter_in_range_and_stops_after_max_extra() -> None:
    rng = _FixedRng(1.0)
    p = RetryPolicy(rng=rng)
    assert p.max_extra == 2 and p.delays == (1.0, 3.0)
    assert p.next_delay(1, None, 100.0) == 1.0
    assert p.next_delay(2, None, 100.0) == 3.0
    assert p.next_delay(3, None, 100.0) is None  # three attempts made: stop
    assert rng.calls == [(0.0, 1.0), (0.0, 3.0)]
    seeded = RetryPolicy(rng=random.Random(7))
    for _ in range(200):
        d1 = seeded.next_delay(1, None, 100.0)
        d2 = seeded.next_delay(2, None, 100.0)
        assert d1 is not None and 0.0 <= d1 <= 1.0
        assert d2 is not None and 0.0 <= d2 <= 3.0
    assert RetryPolicy(max_extra=0, rng=rng).next_delay(1, None, 100.0) is None


def test_default_rng_is_system_random() -> None:
    assert isinstance(RetryPolicy().rng, random.SystemRandom)


def test_jitter_fits_in_left_or_stops() -> None:
    p = RetryPolicy(rng=_FixedRng(1.0))
    d = p.next_delay(2, None, 0.5)
    assert d is not None and d < 0.5
    assert p.next_delay(1, None, 0.0) is None
    assert p.next_delay(1, None, -1.0) is None
    assert p.next_delay(1, None, math.nan) is None


def test_retry_after_used_only_when_it_fits() -> None:
    rng = _FixedRng(0.5)
    p = RetryPolicy(rng=rng)
    assert p.next_delay(1, 2.0, 10.0) == 2.0
    assert rng.calls == []  # Retry-After replaces the jitter
    # It doesn't fit: stop, rather than retry earlier than the provider asked.
    assert p.next_delay(1, 10.0, 10.0) is None
    assert p.next_delay(1, 30.0, 10.0) is None
    assert p.next_delay(1, math.inf, 10.0) is None
    # A nonsense value is ignored, and the jitter is used.
    assert p.next_delay(1, -5.0, 10.0) == 0.5
    assert p.next_delay(1, math.nan, 10.0) == 0.5
    # Retry-After never buys an attempt past max_extra.
    assert p.next_delay(3, 0.1, 10.0) is None


def test_policy_refuses_bad_settings() -> None:
    for kw in (
        {"max_extra": -1},
        {"delays": ()},
        {"delays": (1.0, -1.0)},
        {"delays": (math.nan,)},
        {"delays": (math.inf,)},
    ):
        with pytest.raises(ValueError):
            RetryPolicy(**kw)  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        RetryPolicy().next_delay(0, None, 10.0)


def test_parse_retry_after() -> None:
    assert parse_retry_after("3") == 3.0
    assert parse_retry_after(" 0 ") == 0.0
    assert parse_retry_after("120") == 120.0
    for bad in (
        None,
        "",
        "-1",
        "1.5",
        "abc",
        "\u0663",
        "1e3",
        "9" * 20,
        "Wed, 21 Oct 2026 07:28:00 GMT",
    ):
        assert parse_retry_after(bad) is None, bad
