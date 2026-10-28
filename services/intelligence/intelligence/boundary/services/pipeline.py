"""The pipeline service's request/reply side: health (spec §4).

Its run_query jobs arrive with Task 11. database_ready comes from an
injected readiness check (F28): worker.run_service passes
boundary.db.Database.ready when INTELLIGENCE_DB_URL is set. Without one the
default is "not ready", and a check that raises is "not ready" too. No models load here, so models_ready is False until
the model client does.
"""

from __future__ import annotations

import inspect
import logging
from collections.abc import Awaitable, Callable

from google.protobuf.message import Message

from intelligence.boundary.pb import envelope_pb2, pipeline_pb2
from intelligence.boundary.verify import Verified

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
