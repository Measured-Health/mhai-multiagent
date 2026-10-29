"""The NATS envelope (spec §6.2): header names, the closed error-code set,
and the reply builders.

CODES is core's authz.ErrorCodes(); tests/boundary/test_operations.py holds
it equal to the generated catalogue.json. `grant_refused:<code>` is not in
it: it is a status only a BFF-direct reply may carry (spec §17.5, F33).
"""

from __future__ import annotations

import re

from google.protobuf.message import Message

from intelligence.boundary.pb import envelope_pb2

TOKEN = "Mhai-Request-Token"
SCHEMA = "Mhai-Schema"
REQUEST_ID = "Mhai-Request-Id"
STATUS = "Mhai-Status"
AUTHORIZATION = "Authorization"
# A BFF-direct call's own deadline, which can only shorten the operation's.
TIMEOUT = "Mhai-Timeout-Ms"

OK = "ok"
ERROR_SCHEMA = envelope_pb2.Error.DESCRIPTOR.full_name

CODES: frozenset[str] = frozenset(
    {
        "unauthorized",
        "forbidden",
        "invalid_request",
        "not_found",
        "deadline_exceeded",
        "overloaded",
        "unavailable",
        "cancelled",
        "internal",
    }
)

# The longest reply subject answered (core's natsapi/grant MaxReply).
MAX_REPLY = 256


def reply_under(prefix: str, reply: str) -> bool:
    """reply is a literal subject under prefix (which ends in "."), as
    core's bus.ReplyUnder: at most MAX_REPLY bytes, and every token after
    the prefix non-empty, printable ASCII, with no space and no wildcard
    (* or >). Anything else gets no reply."""
    if not prefix.endswith(".") or len(reply) > MAX_REPLY or not reply.startswith(prefix):
        return False
    for tok in reply[len(prefix) :].split("."):
        if not tok:
            return False
        for ch in tok:
            if ch <= " " or ch >= "\x7f" or ch in "*>":
                return False
    return True


GRANT_REFUSED = "grant_refused:"
_CORE_CODE = re.compile(r"[a-z][a-z_]{0,63}")

Reply = tuple[dict[str, str], bytes]


def _headers(status: str, schema: str, request_id: str) -> dict[str, str]:
    h = {STATUS: status, SCHEMA: schema}
    if request_id:
        h[REQUEST_ID] = request_id
    return h


def reply_ok(meta: envelope_pb2.ResponseMeta, msg: Message, *, request_id: str = "") -> Reply:
    """An `ok` reply: msg with its common `meta` field set to meta."""
    msg.meta.CopyFrom(meta)  # type: ignore[attr-defined]
    return _headers(OK, msg.DESCRIPTOR.full_name, request_id), msg.SerializeToString(
        deterministic=True
    )


def is_status(code: str, *, bff_direct: bool = False) -> bool:
    """code is a status a worker may send: one of CODES, or on a BFF-direct
    reply grant_refused:<core's code>."""
    if code in CODES:
        return True
    if bff_direct and code.startswith(GRANT_REFUSED):
        return _CORE_CODE.fullmatch(code[len(GRANT_REFUSED) :]) is not None
    return False


def reply_error(
    code: str, retryable: bool, *, request_id: str = "", bff_direct: bool = False
) -> Reply:
    """An error reply: Error{code, retryable} and nothing else. A code outside
    the closed set is a bug and raises."""
    if not is_status(code, bff_direct=bff_direct):
        raise ValueError("not a worker error code")
    body = envelope_pb2.Error(code=code, retryable=retryable).SerializeToString(deterministic=True)
    return _headers(code, ERROR_SCHEMA, request_id), body
