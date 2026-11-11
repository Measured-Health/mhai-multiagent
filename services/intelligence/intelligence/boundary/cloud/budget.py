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
import threading
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

# The output cap a call reserves for, and must send as the provider's
# max_tokens / max_output_tokens (Task 9).
MAX_OUTPUT_TOKENS: Final = 1500

# Core's limit on one reservation (mhai_cloud_reserve refuses more; migration
# 00018). Named here so an estimate over it is refused before reserving;
# core stays the authority.
CORE_MAX_TOKENS: Final = 100_000

# A settle's use must fit core's bigint.
_BIGINT_MAX: Final = 2**63 - 1

_T = TypeVar("_T")


def estimate_tokens(body_len: int, max_output: int = MAX_OUTPUT_TOKENS) -> int:
    """An upper bound of one attempt's tokens (ruling C7): one token per
    UTF-8 byte of the whole request body, plus the output cap. The
    providers' tokenizers are byte-level, so no token covers less than a
    byte, whatever the script, emoji or digits. body_len is the length of
    the encoded bytes, never of a str. Raises ValueError for a negative
    length or an estimate over CORE_MAX_TOKENS: the call must not reserve
    (and so must not send)."""
    if body_len < 0 or max_output < 0:
        raise ValueError("estimate_tokens: lengths are never negative")
    est = body_len + max_output
    if est > CORE_MAX_TOKENS:
        raise ValueError("estimate_tokens: over core's limit for one reservation")
    return est


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
        self._alert_lock = threading.Lock()

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
        if not 0 < tokens <= CORE_MAX_TOKENS:
            # Core refuses it too; refused here before any transaction.
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
        if percent >= ALERT_PERCENT and self._first_alert():
            log.warning("cloud.budget_high hospital=%s percent=%d", self._verified.hid, percent)
            telemetry.record_cloud_budget_alert(provider)
        return Reservation(rid, tokens, percent)

    def _first_alert(self) -> bool:
        with self._alert_lock:
            first = not self._alerted
            self._alerted = True
            return first

    def settle(self, r: Reservation, used: int | None) -> None:
        """Settle r with the reported use; an unknown use (None) counts all
        of r. A use outside 0..2^63-1 can't reach core's bigint (that settle
        would fail and hold r all month), so it counts all of r too, and is
        counted. Never raises: a failure is logged and counted."""
        if type(r) is not Reservation:
            log.warning("cloud.settle_failed hospital=%s error=NotAReservation", self._verified.hid)
            telemetry.record_cloud_settle_failure()
            return
        if type(used) is not int:
            n = r.tokens
        elif 0 <= used <= _BIGINT_MAX:
            n = used
        else:
            log.warning("cloud.settle_clamped hospital=%s reservation=%s", self._verified.hid, r.id)
            telemetry.record_cloud_settle_clamped()
            n = r.tokens
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
