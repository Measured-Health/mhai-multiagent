"""The pipeline service: health (request/reply) and run_query (jobs).

database_ready comes from an injected readiness check (F28):
worker.run_service passes boundary.db.Database.ready when
INTELLIGENCE_DB_URL is set. Without one the default is "not ready", and a
check that raises is "not ready" too. models_ready is true once the model
client INTELLIGENCE_MODEL_CLIENT names was built and, with
INTELLIGENCE_MODEL_ROLES, every pinned role was verified (worker.
PinnedModelGate); jobs wait for the same.

run_query (jobs/run_query.py) and approve_correction
(jobs/approve_correction.py, Plan 4 §4.7) run only with a database:
job_handlers gives none without one, so the worker takes no job (T10
note). The worker refuses to start if operations.json lacks either
operation (Operations.require), and pulls approve_correction from its own
durable, pipeline_learning (worker.JOB_CONSUMERS).
"""

from __future__ import annotations

import inspect
import logging
import time
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING

from google.protobuf.message import Message

from intelligence.boundary.pb import envelope_pb2, pipeline_pb2
from intelligence.boundary.verify import Verified

if TYPE_CHECKING:
    from intelligence.boundary.config import WorkerSettings
    from intelligence.boundary.db import Database
    from intelligence.boundary.jobs import JobHandler
    from intelligence.boundary.jobs.run_query import RunQuery
    from intelligence.boundary.models import PinnedSource

log = logging.getLogger("intelligence.boundary.pipeline")

Readiness = Callable[[], bool | Awaitable[bool]]
Handler = Callable[[Verified, Message], Awaitable[tuple[envelope_pb2.ResponseMeta, Message]]]


def _not_ready() -> bool:
    return False


async def _ready(check: Readiness, what: str) -> bool:
    try:
        r = check()
        if inspect.isawaitable(r):
            r = await r
    except Exception as e:
        log.warning("readiness check failed what=%s error=%s", what, type(e).__name__)
        return False
    return r is True


def health_handler(
    database_ready: Readiness = _not_ready, models_ready: Readiness = _not_ready
) -> Handler:
    async def health(
        verified: Verified, request: Message
    ) -> tuple[envelope_pb2.ResponseMeta, Message]:
        return envelope_pb2.ResponseMeta(), pipeline_pb2.HealthResponse(
            models_ready=await _ready(models_ready, "models"),
            database_ready=await _ready(database_ready, "database"),
        )

    return health


def handlers(
    database_ready: Readiness = _not_ready, models_ready: Readiness = _not_ready
) -> dict[str, Handler]:
    return {"health": health_handler(database_ready, models_ready)}


def job_handlers(
    settings: WorkerSettings,
    database: Database | None,
    *,
    clock: Callable[[], float] = time.time,
    pinned: PinnedSource | None = None,
) -> tuple[dict[str, JobHandler] | None, RunQuery | None]:
    """The pipeline's job handlers and the run_query runner (whose
    readiness gates every job, and which the caller closes), or (None,
    None) without a database: no job is taken then. The model client is
    the one INTELLIGENCE_MODEL_CLIENT names, behind a router that adds one
    cloud client per enabled provider (none by default; the route of each
    job is core's, from the claimed row). The learning client is the one
    INTELLIGENCE_LEARNING_CLIENT names (each stub only in development);
    approve_correction gets no router. pinned gives the verified pinned
    models (None until verified)."""
    if database is None:
        return None, None
    from intelligence.boundary.jobs.approve_correction import ApproveCorrection
    from intelligence.boundary.jobs.run_query import RunQuery
    from intelligence.boundary.learning import build_learning_client
    from intelligence.boundary.models import build_model_router

    learning_client = build_learning_client(settings)  # refuses a bad one before any thread
    runner = RunQuery(
        database,
        build_model_router(settings, pinned),
        clock=clock,
        max_concurrency=settings.max_concurrency,
    )
    learning = ApproveCorrection(database, learning_client, clock=clock)
    return {"run_query": runner.handle, "approve_correction": learning.handle}, runner
