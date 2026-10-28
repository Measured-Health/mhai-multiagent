"""The per-process jti set for request/reply (spec §7.4)."""

from __future__ import annotations

import heapq
import threading
from collections.abc import Callable

# A jti is kept until its token's exp plus the clock skew.
KEEP_AFTER_EXP = 5.0
MAX_ENTRIES = 100_000


class ReplaySetFull(Exception):
    """The set is at its bound: refuse (overloaded) rather than forget."""


class JtiSet:
    """The jtis this process has accepted, each kept until exp + 5 s.

    Bounded: at the bound a new jti is refused (ReplaySetFull), never
    admitted unremembered. Expired entries are purged on every insert.
    """

    def __init__(self, clock: Callable[[], float], max_entries: int = MAX_ENTRIES) -> None:
        if max_entries < 1:
            raise ValueError("max_entries must be at least 1")
        self._clock = clock
        self.max_entries = max_entries
        self._until: dict[str, float] = {}
        self._heap: list[tuple[float, str]] = []
        self._lock = threading.Lock()

    def __len__(self) -> int:
        with self._lock:
            return len(self._until)

    def _purge(self, now: float) -> None:
        while self._heap and self._heap[0][0] <= now:
            until, jti = heapq.heappop(self._heap)
            if self._until.get(jti) == until:
                del self._until[jti]

    def seen_or_add(self, jti: str, exp: int) -> bool:
        """True if jti was already accepted (a replay). Otherwise remember it
        and return False. Raises ReplaySetFull at the bound."""
        with self._lock:
            now = self._clock()
            self._purge(now)
            if jti in self._until:
                return True
            if len(self._until) >= self.max_entries:
                raise ReplaySetFull()
            until = exp + KEEP_AFTER_EXP
            self._until[jti] = until
            heapq.heappush(self._heap, (until, jti))
            return False
