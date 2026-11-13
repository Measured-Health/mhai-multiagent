"""The budget port without a database (plan Task 7; design section 10).

DbBudget is driven from the test's thread against a job loop running in
a thread of its own, as a generation thread drives it, with a stand-in
Database and cloud_repo. The loop's self-pipe is a Unix socket pair, made
while job_loop lifts the guard for that one constructor; every other
socket stays blocked. The rules themselves (the atomic reservation, the
provider check, the once-only settle) are core's SQL and are proven
against Postgres in test_budget.py. Every id here is synthetic.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import socket
import sys
import threading
from collections.abc import AsyncIterator, Callable, Iterator
from contextlib import asynccontextmanager
from typing import Any

import pytest
import pytest_socket
from hypothesis import given, settings
from hypothesis import strategies as st
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from intelligence.boundary import cloud_repo, telemetry
from intelligence.boundary.cloud.budget import (
    CORE_MAX_TOKENS,
    MAX_OUTPUT_TOKENS,
    BudgetPort,
    DbBudget,
    Reservation,
    estimate_tokens,
)
from intelligence.boundary.cloud.scan import MAX_BODY_BYTES
from tests.boundary.test_db import bent

pytestmark = pytest.mark.disable_socket

HOSPITAL = "c3c3c3c3-0000-4000-8000-00000000000c"
RES_ID = "d4d4d4d4-0000-4000-8000-00000000000d"
CURSOR = object()
Make = Callable[..., DbBudget]


class FakeDb:
    """with_hospital yields a marker cursor, or raises `fail`."""

    def __init__(self, fail: BaseException | None = None) -> None:
        self.fail = fail
        self.opened: list[Any] = []

    @asynccontextmanager
    async def with_hospital(self, verified: Any) -> AsyncIterator[Any]:
        self.opened.append(verified)
        if self.fail is not None:
            raise self.fail
        yield CURSOR


@pytest.fixture
def metrics() -> Iterator[InMemoryMetricReader]:
    reader = InMemoryMetricReader()
    stop = telemetry.setup(metric_reader=reader)
    yield reader
    stop()


def points(reader: InMemoryMetricReader, name: str) -> list[Any]:
    data = reader.get_metrics_data()
    out: list[Any] = []
    for rm in data.resource_metrics if data else ():
        for sm in rm.scope_metrics:
            for m in sm.metrics:
                if m.name == name:
                    out += list(m.data.data_points)
    return out


@pytest.fixture
def job_loop() -> Iterator[asyncio.AbstractEventLoop]:
    """The job's event loop, running in its own thread. The guard is lifted
    only while the loop makes its self-pipe (a Unix socket pair), then put
    back whole: no other socket of any family may be opened."""
    pytest_socket.enable_socket()
    try:
        loop = asyncio.new_event_loop()
    finally:
        pytest_socket.disable_socket()
    t = threading.Thread(target=loop.run_forever, daemon=True)
    t.start()
    try:
        yield loop
    finally:
        loop.call_soon_threadsafe(loop.stop)
        t.join(5)
        loop.close()


@pytest.fixture
def port(job_loop: asyncio.AbstractEventLoop) -> Make:
    def make(db: Any, timeout: float = 5.0) -> DbBudget:
        return DbBudget(db, bent(HOSPITAL), job_loop, timeout=timeout)

    return make


# Text that tokenises badly: emoji (with ZWJ and skin tones), Yoruba tone
# marks as combining characters, CJK, Devanagari, Arabic, mixed scripts,
# digit and base64-like runs. The providers' tokenizers are byte-level BPE,
# so every token covers at least one UTF-8 byte: one token per byte is an
# upper bound whatever the text.
ADVERSARIAL = [
    "",
    "a",
    "\U0001f469\U0001f3fd\u200d\u2695\ufe0f" * 300,
    "O\u0300ko\u0301 a\u0300ti\u0300 e\u0323\u0300we\u0323\u0300 " * 200,
    "\u75c5\u4eba\u53d1\u70ed\u4e09\u5929" * 500,
    "\u0930\u094b\u0917\u0940 \u0645\u0631\u064a\u0636 \u0416\u0430\u0440 fever " * 300,
    "0812345678901234567890" * 400,
    "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=" * 300,
]


def _bytes(text: str) -> int:
    return len(text.encode("utf-8"))


@pytest.mark.parametrize("text", ADVERSARIAL)
def test_estimate_is_an_upper_bound(text: str) -> None:
    """Ruling C7: at least one token per UTF-8 byte of the body, plus the
    output cap; and at least one per character (a byte count is never
    under a character count)."""
    n = _bytes(text)
    est = estimate_tokens(n)
    assert est >= n + MAX_OUTPUT_TOKENS
    assert est >= len(text) + MAX_OUTPUT_TOKENS
    assert estimate_tokens(n, max_output=100) >= n + 100


@settings(max_examples=300, deadline=None)
@given(st.text(max_size=4000))
def test_estimate_bounds_any_text(text: str) -> None:
    n = _bytes(text)
    assert estimate_tokens(n) >= n + MAX_OUTPUT_TOKENS >= len(text) + MAX_OUTPUT_TOKENS


def test_largest_estimate_stays_under_cores_limit() -> None:
    """The biggest body the scan lets through, with the output cap, still
    fits one reservation (core refuses more than 100,000 tokens)."""
    assert CORE_MAX_TOKENS == 100_000
    assert estimate_tokens(MAX_BODY_BYTES) <= CORE_MAX_TOKENS
    with pytest.raises(ValueError):
        estimate_tokens(CORE_MAX_TOKENS)  # plus the cap: over
    with pytest.raises(ValueError):
        estimate_tokens(-1)
    with pytest.raises(ValueError):
        estimate_tokens(10, max_output=-1)


def test_reserve_refuses_over_cores_limit_before_reserving(
    monkeypatch: pytest.MonkeyPatch, port: Make
) -> None:
    calls: list[int] = []

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        calls.append(tokens)
        return RES_ID, 1

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    p = port(FakeDb())
    assert p.reserve("anthropic", CORE_MAX_TOKENS + 1) is None
    assert p.reserve("anthropic", 0) is None
    assert p.reserve("anthropic", CORE_MAX_TOKENS) is not None
    assert calls == [CORE_MAX_TOKENS]


@pytest.mark.filterwarnings("ignore:A test tried to use socket.socket")
@pytest.mark.parametrize("family", [socket.AF_UNIX, socket.AF_INET])
def test_job_loop_allows_no_other_socket(
    job_loop: asyncio.AbstractEventLoop, family: socket.AddressFamily
) -> None:
    with pytest.raises(pytest_socket.SocketBlockedError):
        socket.socket(family, socket.SOCK_STREAM)


def test_port_has_no_hospital_parameter() -> None:
    assert list(inspect.signature(BudgetPort.reserve).parameters) == ["self", "provider", "tokens"]
    assert list(inspect.signature(DbBudget.reserve).parameters) == ["self", "provider", "tokens"]
    assert list(inspect.signature(DbBudget.settle).parameters) == ["self", "r", "used"]


def test_port_takes_only_a_verified(job_loop: asyncio.AbstractEventLoop) -> None:
    from types import SimpleNamespace

    with pytest.raises(TypeError):
        DbBudget(FakeDb(), SimpleNamespace(hid=HOSPITAL), job_loop)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    "fail",
    [
        RuntimeError("pool closed"),
        OSError("connection refused"),
        TimeoutError(),
        ValueError("bad row"),
    ],
)
def test_reserve_none_on_db_error(fail: BaseException, port: Make) -> None:
    db = FakeDb(fail=fail)
    got = port(db).reserve("anthropic", 2000)
    assert got is None
    assert len(db.opened) == 1


def test_reserve_none_when_the_repo_raises(monkeypatch: pytest.MonkeyPatch, port: Make) -> None:
    async def boom(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        raise RuntimeError("SQLSTATE 22023")

    monkeypatch.setattr(cloud_repo, "reserve", boom)
    assert port(FakeDb()).reserve("anthropic", 2000) is None


def test_reserve_none_on_timeout(monkeypatch: pytest.MonkeyPatch, port: Make) -> None:
    async def slow(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        await asyncio.sleep(5)
        return RES_ID, 1

    monkeypatch.setattr(cloud_repo, "reserve", slow)
    got = port(FakeDb(), timeout=0.05).reserve("anthropic", 2000)
    assert got is None


def test_reserve_on_the_jobs_loop_thread_fails_closed(
    monkeypatch: pytest.MonkeyPatch, job_loop: asyncio.AbstractEventLoop, port: Make
) -> None:
    """Called on the loop itself, waiting would deadlock: no reservation."""
    calls: list[str] = []

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        calls.append(provider)
        return RES_ID, 1

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    p = port(FakeDb())

    async def on_the_loop() -> Reservation | None:
        return p.reserve("anthropic", 2000)

    assert asyncio.run_coroutine_threadsafe(on_the_loop(), job_loop).result(5) is None
    assert calls == []


def test_reserve_passes_provider_and_tokens_on_the_hospitals_cursor(
    monkeypatch: pytest.MonkeyPatch, port: Make
) -> None:
    seen: list[tuple[Any, str, int]] = []

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        seen.append((cur, provider, tokens))
        return RES_ID, 12

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    db = FakeDb()
    got = port(db).reserve("openai", 2345)
    assert got == Reservation(RES_ID, 2345, 12)
    assert seen == [(CURSOR, "openai", 2345)]
    assert [v.hid for v in db.opened] == [HOSPITAL]


def test_empty_result_is_no_reservation(monkeypatch: pytest.MonkeyPatch, port: Make) -> None:
    async def none(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        return None

    monkeypatch.setattr(cloud_repo, "reserve", none)
    assert port(FakeDb()).reserve("xai", 10) is None


def test_reserve_refuses_unknown_provider_and_non_int_tokens(
    monkeypatch: pytest.MonkeyPatch, port: Make
) -> None:
    calls: list[str] = []

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        calls.append(provider)
        return RES_ID, 1

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    p = port(FakeDb())
    for provider, tokens in (("mistral", 10), ("", 10), ("anthropic", True), ("anthropic", 1.5)):
        assert p.reserve(provider, tokens) is None  # type: ignore[arg-type]
    assert calls == []


def test_settle_unknown_reply_counts_reserved(monkeypatch: pytest.MonkeyPatch, port: Make) -> None:
    seen: list[tuple[Any, str, int | None]] = []

    async def rec(cur: Any, reservation: str, used: int | None) -> bool:
        seen.append((cur, reservation, used))
        return True

    monkeypatch.setattr(cloud_repo, "settle", rec)
    p = port(FakeDb())
    r = Reservation(RES_ID, 2500, 10)
    p.settle(r, None)
    p.settle(r, 700)
    assert seen == [(CURSOR, RES_ID, 2500), (CURSOR, RES_ID, 700)]


def test_settle_failure_is_logged_by_ids_and_counted(
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
    metrics: InMemoryMetricReader,
    port: Make,
) -> None:
    caplog.set_level(logging.INFO)
    p = port(FakeDb(fail=RuntimeError("password=test-only-secret")))
    p.settle(Reservation(RES_ID, 2500, 10), 100)  # never raises
    text = caplog.text
    assert "cloud.settle_failed" in text
    assert RES_ID in text and HOSPITAL in text and "RuntimeError" in text
    assert "test-only-secret" not in text
    (pt,) = points(metrics, "mhai_cloud_settle_failures_total")
    assert pt.value == 1


def test_alert_at_80_percent_once(
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
    metrics: InMemoryMetricReader,
    port: Make,
) -> None:
    percents = iter([79, 80, 95, 99])

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        return RES_ID, next(percents)

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    caplog.set_level(logging.INFO)
    p = port(FakeDb())
    for _ in range(4):
        assert p.reserve("anthropic", 100) is not None
    alerts = [r for r in caplog.records if "cloud.budget_high" in r.getMessage()]
    assert [a.getMessage() for a in alerts] == [f"cloud.budget_high hospital={HOSPITAL} percent=80"]
    (pt,) = points(metrics, "mhai_cloud_budget_alerts_total")
    assert pt.value == 1 and dict(pt.attributes) == {"provider": "anthropic"}


def test_repr_names_nothing(port: Make) -> None:
    assert HOSPITAL not in repr(port(FakeDb()))


@pytest.mark.parametrize("used", [2**63, 2**80, -1])
def test_settle_out_of_range_use_settles_the_reservation_and_counts(
    monkeypatch: pytest.MonkeyPatch, port: Make, metrics: InMemoryMetricReader, used: int
) -> None:
    """A use outside 0..2^63-1 can't reach core's bigint: it would fail the
    settle and hold the reservation all month. It settles as all of it."""
    seen: list[int | None] = []

    async def rec(cur: Any, reservation: str, n: int | None) -> bool:
        seen.append(n)
        return True

    monkeypatch.setattr(cloud_repo, "settle", rec)
    port(FakeDb()).settle(Reservation(RES_ID, 2500, 10), used)
    assert seen == [2500]
    (pt,) = points(metrics, "mhai_cloud_settle_clamped_total")
    assert pt.value == 1
    assert points(metrics, "mhai_cloud_settle_failures_total") == []


def test_settle_of_a_non_reservation_never_raises(
    port: Make, metrics: InMemoryMetricReader
) -> None:
    port(FakeDb()).settle(object(), 10)  # type: ignore[arg-type]
    (pt,) = points(metrics, "mhai_cloud_settle_failures_total")
    assert pt.value == 1


def test_alert_once_holds_across_threads(
    monkeypatch: pytest.MonkeyPatch, port: Make, metrics: InMemoryMetricReader
) -> None:
    """Many threads on one port, all at 90%: exactly one alert per port."""

    async def rec(cur: Any, provider: str, tokens: int) -> tuple[str, int] | None:
        return RES_ID, 90

    monkeypatch.setattr(cloud_repo, "reserve", rec)
    old = sys.getswitchinterval()
    sys.setswitchinterval(1e-6)
    ports = 30
    try:
        for _ in range(ports):
            p = port(FakeDb())
            gate = threading.Barrier(16)

            def hit(p: DbBudget = p, gate: threading.Barrier = gate) -> None:
                gate.wait()
                p.reserve("openai", 10)

            ts = [threading.Thread(target=hit) for _ in range(16)]
            for t in ts:
                t.start()
            for t in ts:
                t.join()
    finally:
        sys.setswitchinterval(old)
    (pt,) = points(metrics, "mhai_cloud_budget_alerts_total")
    assert pt.value == ports
