"""JetStream jobs (spec §9.2): what the worker's consumer loop hands a job
handler.

A job handler gets a Verified (every check of spec §7.3 passed), the
JetStream message (to ack, Nak, Term or send +WIP) and a JobContext: the
worker's concurrency slot and the dead-letter publisher. The handler owns
the message's outcome; the worker only refuses what never verified.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from nats.aio.msg import Msg

    from intelligence.boundary.pb import envelope_pb2
    from intelligence.boundary.verify import Verified


class Slot:
    """One of the worker's job slots (a semaphore count).

    asyncio can stop waiting for a thread but can't stop the thread, so a
    model stage abandoned at its deadline (or by a killed task) keeps
    running. The slot is given back only when the job's task has released
    it and every thread it handed work to has finished, so the worker never
    runs more model work than its limit. release() is idempotent.
    """

    def __init__(self, sem: asyncio.Semaphore) -> None:
        self._sem = sem
        self._loop = asyncio.get_running_loop()
        self._holds = 1
        self._released = False
        self._freed = False

    def hold(self, fut: concurrent.futures.Future[object]) -> None:
        """Keep the slot until fut (work on a thread) is done."""
        self._holds += 1
        fut.add_done_callback(self._thread_done)

    def _thread_done(self, _fut: concurrent.futures.Future[object]) -> None:
        # Runs on the worker thread (or here, if already done).
        try:
            self._loop.call_soon_threadsafe(self._drop)
        except RuntimeError:  # the loop is closed: nothing left to give back to
            pass

    def release(self) -> None:
        """The job's task is done with the slot."""
        if self._released:
            return
        self._released = True
        self._drop()

    def _drop(self) -> None:
        self._holds -= 1
        if self._holds == 0 and not self._freed:
            self._freed = True
            self._sem.release()


@dataclass(frozen=True)
class JobContext:
    """What the worker gives a job handler besides the message."""

    slot: Slot
    # Publishes one DeadLetter on dead.v1.<hid>.<type>; never raises.
    dead_letter: Callable[[envelope_pb2.DeadLetter], Awaitable[None]]


JobHandler = Callable[["Verified", "Msg", JobContext], Awaitable[None]]
