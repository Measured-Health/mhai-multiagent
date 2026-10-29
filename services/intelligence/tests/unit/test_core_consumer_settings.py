"""The pipeline's job settings match the consumer core creates (Plan 3 T11,
carried to T15).

run_query decides "this is the last delivery" from MAX_DELIVER: on the last
one it never Naks (a Nak there would end the job with no dead letter). Core
sets the consumer's MaxDeliver (bus.PipelineMaxDeliver, which only core can
change: the worker's NATS user can't update the consumer), so the two must
agree. This reads core's constant from its source, so changing one without
the other fails here.
"""

from __future__ import annotations

import re
from pathlib import Path

from intelligence.boundary.jobs import run_query

STREAMS_GO = Path(__file__).resolve().parents[3] / "core" / "internal" / "bus" / "streams.go"


def _go_int(name: str) -> int:
    m = re.search(rf"^\s*{name}\s*=\s*(\d+)\s*$", STREAMS_GO.read_text(), re.MULTILINE)
    assert m is not None, f"{name} not found in {STREAMS_GO}"
    return int(m.group(1))


def test_max_deliver_matches_core() -> None:
    assert run_query.MAX_DELIVER == _go_int("PipelineMaxDeliver")
    assert run_query.JobSettings().max_deliver == run_query.MAX_DELIVER


def test_wip_comes_well_inside_core_ack_wait() -> None:
    """+WIP every 20 s keeps a running job from redelivery only while core's
    AckWait (bus.PipelineAckWait, in seconds) stays above it."""
    m = re.search(
        r"^\s*PipelineAckWait\s*=\s*(\d+)\s*\*\s*time\.Second", STREAMS_GO.read_text(), re.M
    )
    assert m is not None
    assert run_query.JobSettings().wip_every * 2 <= int(m.group(1))
