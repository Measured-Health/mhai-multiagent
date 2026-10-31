"""The pipeline service: health (request/reply) and run_query (jobs).

database_ready comes from an injected readiness check (F28):
worker.run_service passes boundary.db.Database.ready when
INTELLIGENCE_DB_URL is set. Without one the default is "not ready", and a
check that raises is "not ready" too. models_ready is true once the model
client INTELLIGENCE_MODEL_CLIENT names was built.

run_query (jobs/run_query.py) runs only with a database: job_handlers
gives none without one, so the worker takes no job (T10 note).
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
) -> tuple[dict[str, JobHandler] | None, RunQuery | None]:
    """The pipeline's job handlers and their runner, or (None, None) without
    a database: no job is taken then. The model client is the one
    INTELLIGENCE_MODEL_CLIENT names (the stub only in development)."""
    if database is None:
        return None, None
    from intelligence.boundary.jobs.run_query import RunQuery
    from intelligence.boundary.models import build_model_client

    runner = RunQuery(
        database,
        build_model_client(settings),
        clock=clock,
        max_concurrency=settings.max_concurrency,
    )
    return {"run_query": runner.handle}, runner
