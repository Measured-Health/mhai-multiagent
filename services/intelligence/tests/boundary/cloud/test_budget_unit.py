"""The budget port without a database (plan Task 7; design section 10).

DbBudget is driven from the test's thread against a job loop running in
a thread of its own, as a generation thread drives it, with a stand-in
Database and cloud_repo. The loop's self-pipe is a Unix socket pair, so
the job_loop fixture allows Unix sockets; TCP stays blocked. The rules themselves (the atomic reservation, the
provider check, the once-only settle) are core's SQL and are proven
against Postgres in test_budget.py. Every id here is synthetic.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import threading
from collections.abc import AsyncIterator, Callable, Iterator
from contextlib import asynccontextmanager
from typing import Any

import pytest
import pytest_socket
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from intelligence.boundary import cloud_repo, telemetry
from intelligence.boundary.cloud.budget import (
    BudgetPort,
    DbBudget,
    Reservation,
    estimate_tokens,
)
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
    """The job's event loop, running in its own thread."""
    # The module's guard, again, with Unix sockets let through (a new guard
    # wraps the current socket class, so the old guard is lifted first).
    pytest_socket.enable_socket()
    pytest_socket.disable_socket(allow_unix_socket=True)
    loop = asyncio.new_event_loop()
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


def test_estimate_is_an_upper_bound() -> None:
    for n in (0, 1, 2, 3, 4, 299, 300, 301, 9_999, 123_457):
        est = estimate_tokens(n)
        assert est >= n / 3 + 1500
        assert est == -(-n // 3) + 1500  # ceil(n / 3) + 1500, exactly
    assert estimate_tokens(10, max_output=100) == 4 + 100
    with pytest.raises(ValueError):
        estimate_tokens(-1)


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
    for provider, tokens in (("gemini", 10), ("", 10), ("anthropic", True), ("anthropic", 1.5)):
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
