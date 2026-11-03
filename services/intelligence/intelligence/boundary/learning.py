"""The learning seam of the approve_correction job: LearningClient (Plan 4
spec §4.7, §4.8, §10.6), beside ModelClient.

The job calls three things, all cheap and pure:

  buffer_entry(scores, system_decision) -> BufferEntry
      the row the per-hospital feedback_buffer keeps: the four expert
      scores and the system decision, the input TrustWeightOptimizer's
      add_correction takes today
  persona_update(correction_id) -> "skipped" | "applied"
      "skipped" in Plan 4: the update needs the model's hidden activations,
      which the pipeline doesn't capture (spec §4.8, decision 4)
  model_versions() -> dict[str, str]

TrustBufferLearningClient is the production client. It runs no optimiser,
changes no weight, writes nothing: guardrail_weights are global, and a
CMA-ES run is a clinical change that goes through change control
(decision 3). StubLearningClient is for tests and the CI smoke test only:
the settings refuse INTELLIGENCE_LEARNING_CLIENT=stub outside
APP_ENV=development, and build_learning_client refuses it again (as
build_model_client does, Plan 3 ruling F14).

Nothing here reads, takes or logs free text: no reason, no corrected
diagnosis, no question.
"""

from __future__ import annotations

import logging
import math
import threading
from collections import Counter
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal, Protocol, runtime_checkable

from intelligence.boundary.config import ConfigError, WorkerSettings

log = logging.getLogger("intelligence.boundary.learning")

METHODS = ("buffer_entry", "persona_update", "model_versions")
DECISIONS = ("PASS", "REGENERATE", "ESCALATE")
PersonaUpdate = Literal["skipped", "applied"]


@dataclass(frozen=True)
class Scores:
    """The latest hga_evaluations row of the corrected query: the four
    expert scores, the trust and the guardrail's decision. Any may be
    missing (a degraded run stores none)."""

    nli: float | None = None
    coherence: float | None = None
    numerical: float | None = None
    se: float | None = None
    trust: float | None = None
    decision: str | None = None


@dataclass(frozen=True)
class BufferEntry:
    """One feedback_buffer row's learning facts. Each score is in [0, 1]
    or None; the decision is one of PASS, REGENERATE, ESCALATE."""

    nli: float | None
    coherence: float | None
    numerical: float | None
    se: float | None
    system_decision: str


def unit_score(v: object) -> float | None:
    """v clamped to [0, 1]; None for anything that isn't a finite number
    (a missing score stays missing: none is invented)."""
    if isinstance(v, bool) or not isinstance(v, int | float):
        return None
    f = float(v)
    if not math.isfinite(f):
        return None
    return min(1.0, max(0.0, f))


def decision_code(d: object) -> str:
    """The system's decision as the buffer stores it. Anything outside the
    three is ESCALATE: fail closed, the way the decision itself was made."""
    return d if isinstance(d, str) and d in DECISIONS else "ESCALATE"


# TODO(learning): the job calls these inline, inside its one transaction,
# with a fixed 60 s lease and no +WIP: fine for today's pure arithmetic. A
# client that does real work (a persona update needing activations, a
# model call) must move to worker threads with run_query's heartbeat (+WIP
# and lease renewal, jobs/run_query.py _stage/_heartbeat) first.
@runtime_checkable
class LearningClient(Protocol):
    def buffer_entry(self, scores: Scores, system_decision: str) -> BufferEntry: ...

    def persona_update(self, correction_id: str) -> PersonaUpdate: ...

    def model_versions(self) -> dict[str, str]: ...


class TrustBufferLearningClient:
    """The production client: the buffer row, computed; nothing applied."""

    VERSIONS = {"trust_buffer": "1"}

    def buffer_entry(self, scores: Scores, system_decision: str) -> BufferEntry:
        return BufferEntry(
            nli=unit_score(scores.nli),
            coherence=unit_score(scores.coherence),
            numerical=unit_score(scores.numerical),
            se=unit_score(scores.se),
            system_decision=decision_code(system_decision),
        )

    def persona_update(self, correction_id: str) -> PersonaUpdate:
        # Plan 4 spec §4.8, decision 4: the persona vector is not updated.
        return "skipped"

    def model_versions(self) -> dict[str, str]:
        return dict(self.VERSIONS)


class StubLearningClient(TrustBufferLearningClient):
    """TrustBufferLearningClient's arithmetic, for tests and the CI smoke
    test only. raises={method: exc} raises exc from that method; calls
    counts them."""

    VERSIONS = {"stub_learning": "1"}

    def __init__(self, *, raises: Mapping[str, BaseException] | None = None) -> None:
        if raises is not None and not set(raises) <= set(METHODS):
            raise ValueError("unknown method")
        self._raises = dict(raises or {})
        self._lock = threading.Lock()
        self.calls: Counter[str] = Counter()

    def _enter(self, method: str) -> None:
        with self._lock:
            self.calls[method] += 1
        if method in self._raises:
            raise self._raises[method]

    def buffer_entry(self, scores: Scores, system_decision: str) -> BufferEntry:
        self._enter("buffer_entry")
        return super().buffer_entry(scores, system_decision)

    def persona_update(self, correction_id: str) -> PersonaUpdate:
        self._enter("persona_update")
        return super().persona_update(correction_id)

    def model_versions(self) -> dict[str, str]:
        self._enter("model_versions")
        return super().model_versions()


def build_learning_client(settings: WorkerSettings) -> LearningClient:
    """The client INTELLIGENCE_LEARNING_CLIENT names. The stub only in
    development (checked again here, not only in the settings: F14)."""
    if settings.learning_client == "stub":
        if not settings.development:
            raise ConfigError(
                "INTELLIGENCE_LEARNING_CLIENT=stub runs only with APP_ENV=development"
            )
        log.warning("learning client is the stub: tests and smoke only")
        return StubLearningClient()
    if settings.learning_client == "trust_buffer":
        return TrustBufferLearningClient()
    raise ConfigError("INTELLIGENCE_LEARNING_CLIENT must be trust_buffer or stub")
