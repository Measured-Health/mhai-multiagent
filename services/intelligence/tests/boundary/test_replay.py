"""The per-process jti set (spec §7.4): kept until exp + 5 s, bounded, fails closed."""

from __future__ import annotations

import threading

import pytest

from intelligence.boundary.replay import JtiSet, ReplaySetFull


class Clock:
    def __init__(self, now: float) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now


def test_replay_set_bounded_and_expires() -> None:
    clock = Clock(1000)
    s = JtiSet(clock, max_entries=3)
    assert s.seen_or_add("a", 1005) is False
    assert s.seen_or_add("a", 1005) is True
    assert s.seen_or_add("b", 1010) is False
    assert s.seen_or_add("c", 1010) is False
    with pytest.raises(ReplaySetFull):
        s.seen_or_add("d", 1010)
    # A refused insert is not remembered.
    assert len(s) == 3
    # Still remembered at exp + 5 s minus a moment...
    clock.now = 1009.9
    assert s.seen_or_add("a", 1005) is True
    # ...and gone after exp + 5 s, which frees room.
    clock.now = 1010
    assert s.seen_or_add("d", 1020) is False
    assert len(s) == 3
    clock.now = 1015  # b and c go
    assert s.seen_or_add("a", 1015) is False
    assert len(s) == 2


def test_default_bound_is_100k() -> None:
    assert JtiSet(Clock(0)).max_entries == 100_000


def test_concurrent_inserts_admit_one() -> None:
    s = JtiSet(Clock(0))
    results: list[bool] = []
    lock = threading.Lock()

    def go() -> None:
        r = s.seen_or_add("same", 60)
        with lock:
            results.append(r)

    threads = [threading.Thread(target=go) for _ in range(16)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert results.count(False) == 1
