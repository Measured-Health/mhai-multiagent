"""The echo service: test and smoke only (spec §4), never in production.

echo.ping is test_only in operations.json, so outside APP_ENV=development
the table leaves it out and an echo worker can't start; the settings refuse
it too. It answers with the text it was sent, after verification, and one
audit fact that names no person and carries no text.
"""

from __future__ import annotations

from google.protobuf.message import Message

from intelligence.boundary.pb import echo_pb2, envelope_pb2
from intelligence.boundary.verify import Verified

PINGED = "intelligence.echo.pinged"


def pinged_fact() -> envelope_pb2.AuditFact:
    return envelope_pb2.AuditFact(action=PINGED, entity="echo")


async def ping(verified: Verified, request: Message) -> tuple[envelope_pb2.ResponseMeta, Message]:
    if not isinstance(request, echo_pb2.PingRequest):
        raise TypeError("not a PingRequest")
    meta = envelope_pb2.ResponseMeta(audit_facts=[pinged_fact()])
    return meta, echo_pb2.PingResponse(text=request.text)


HANDLERS = {"ping": ping}
