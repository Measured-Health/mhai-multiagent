"""The cloud token budget port (design section 10; plan Task 7).

Before each send the client reserves an upper bound of the tokens it may
use; after the reply it settles the reservation with the provider's
reported use, or with all of it when the reply is lost or unknown. The rule
(one atomic reservation under the hospital's monthly budget, for an active
hospital whose policy names that provider now; a settle once, for its own
hospital) is core's SQL, migration 00018. This module holds none of it and
no SQL: DbBudget calls boundary/cloud_repo.py, which calls core's functions
on the cursor of `Database.with_hospital`.

The port has no hospital parameter: the hospital is the verified token's.
It fails closed: any error, timeout or misuse while reserving gives no
reservation, and the call's outcome is `budget_exhausted`. A failed settle
is logged by ids only and counted; that reservation stays held until the
month ends (bounded by one attempt's estimate).
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Callable, Coroutine
from dataclasses import dataclass
from typing import Any, Final, Protocol, TypeVar

from intelligence.boundary import cloud_repo, telemetry
from intelligence.boundary.cloud.vocab import PROVIDERS
from intelligence.boundary.db import Database
from intelligence.boundary.verify import Verified

log = logging.getLogger("intelligence.boundary.cloud.budget")

# A call whose reservation finds its hospital at this share of the month's
# budget or more logs and counts one alert (once per call).
ALERT_PERCENT: Final = 80

_T = TypeVar("_T")


def estimate_tokens(body_len: int, max_output: int = 1500) -> int:
    """An upper bound of one attempt's tokens: input at one token per three
    bytes of the body, rounded up, plus the output cap."""
    if body_len < 0 or max_output < 0:
        raise ValueError("estimate_tokens: lengths are never negative")
    return -(-body_len // 3) + max_output


@dataclass(frozen=True, slots=True)
class Reservation:
    id: str
    tokens: int
    percent: int


class BudgetPort(Protocol):
    """Synchronous, so a generation thread can call it."""

    def reserve(self, provider: str, tokens: int) -> Reservation | None: ...

    def settle(self, r: Reservation, used: int | None) -> None: ...


class DbBudget:
    """The budget of the verified token's hospital, through core's
    functions. Each call is its own with_hospital transaction, run on the
    job's event loop and awaited from the calling (generation) thread. One
    instance serves one call."""

    def __init__(
        self,
        db: Database,
        verified: Verified,
        loop: asyncio.AbstractEventLoop,
        timeout: float = 5.0,
    ) -> None:
        if type(verified) is not Verified:
            raise TypeError("DbBudget takes a Verified, from the verifier only")
        self._db = db
        self._verified = verified
        self._loop = loop
        self._timeout = timeout
        self._alerted = False

    def __repr__(self) -> str:
        return "DbBudget()"

    def _run(self, make: Callable[[], Coroutine[Any, Any, _T]]) -> _T:
        try:
            running: asyncio.AbstractEventLoop | None = asyncio.get_running_loop()
        except RuntimeError:
            running = None
        if running is self._loop:
            # Waiting here would block the very loop that must run it.
            raise RuntimeError("DbBudget: called on the job's own event loop")
        coro = make()
        try:
            fut = asyncio.run_coroutine_threadsafe(coro, self._loop)
        except BaseException:
            coro.close()
            raise
        try:
            return fut.result(self._timeout)
        except BaseException:
            fut.cancel()
            raise

    async def _reserve(self, provider: str, tokens: int) -> tuple[str, int] | None:
        async with self._db.with_hospital(self._verified) as cur:
            return await cloud_repo.reserve(cur, provider, tokens)

    async def _settle(self, reservation: str, used: int) -> bool:
        async with self._db.with_hospital(self._verified) as cur:
            return await cloud_repo.settle(cur, reservation, used)

    def reserve(self, provider: str, tokens: int) -> Reservation | None:
        """A reservation of tokens for provider, or None: core refused
        (budget, policy or hospital) or anything went wrong."""
        if provider not in PROVIDERS or type(tokens) is not int:
            return None
        try:
            row = self._run(lambda: self._reserve(provider, tokens))
        except Exception as e:
            log.warning(
                "cloud.reserve_failed hospital=%s error=%s", self._verified.hid, type(e).__name__
            )
            return None
        if row is None:
            return None
        rid, percent = row
        if percent >= ALERT_PERCENT and not self._alerted:
            self._alerted = True
            log.warning("cloud.budget_high hospital=%s percent=%d", self._verified.hid, percent)
            telemetry.record_cloud_budget_alert(provider)
        return Reservation(rid, tokens, percent)

    def settle(self, r: Reservation, used: int | None) -> None:
        """Settle r with the reported use; an unknown use (None) counts all
        of r. Never raises: a failure is logged and counted."""
        n = used if type(used) is int else r.tokens
        try:
            done = self._run(lambda: self._settle(r.id, n))
        except Exception as e:
            log.warning(
                "cloud.settle_failed hospital=%s reservation=%s error=%s",
                self._verified.hid,
                r.id,
                type(e).__name__,
            )
            telemetry.record_cloud_settle_failure()
            return
        if not done:
            log.info("cloud.settle_noop hospital=%s reservation=%s", self._verified.hid, r.id)
