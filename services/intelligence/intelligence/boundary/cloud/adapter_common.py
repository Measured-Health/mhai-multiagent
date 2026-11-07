"""What the three adapters share (plan Tasks 11 to 13).

- A provider's status error becomes a reply, never an exception: its
  status is the outcome, through retry.classify, with the Retry-After
  header's seconds. The error itself (whose message may echo the request)
  is dropped here, unlogged.
- The reply envelope is read from the raw HTTP body as strict JSON
  (duplicate keys refused, at most MAX_ENVELOPE_BYTES), not through the
  SDK's lenient models, so an odd field fails closed.
- The answer text goes through reply.validate_answer against the chunk
  ids that were sent; any failure is malformed.

Nothing here logs.
"""

from __future__ import annotations

from typing import Any, Final

from intelligence.boundary import _strictjson
from intelligence.boundary.cloud import retry
from intelligence.boundary.cloud.client import CloudReply
from intelligence.boundary.cloud.gate import CloudPrompt
from intelligence.boundary.cloud.reply import Malformed, validate_answer

# A reply envelope: the answer (at most reply.MAX_REPLY_BYTES) plus the
# provider's fields. Anything bigger is refused unread.
MAX_ENVELOPE_BYTES: Final = 262_144


def check_prompt(prompt: object) -> CloudPrompt:
    """An adapter takes a CloudPrompt and nothing else (A5): a str, a dict
    or anything else raises before a request exists."""
    if type(prompt) is not CloudPrompt:
        raise TypeError("an adapter takes a CloudPrompt")
    return prompt


def sent_ids(prompt: CloudPrompt) -> frozenset[str]:
    return frozenset(c.chunk_id for c in prompt.chunks)


def status_reply(error: Any) -> CloudReply:
    """A provider's status error as a reply: the outcome from its status,
    Retry-After when it is delta-seconds. Nothing of its message is kept."""
    status = getattr(error, "status_code", None)
    if type(status) is not int:
        return CloudReply(None, "", None, None, "unavailable")
    retry_after: float | None = None
    try:
        headers = error.response.headers
        retry_after = retry.parse_retry_after(headers.get("retry-after"))
    except Exception:
        retry_after = None
    return CloudReply(
        None, "", None, None, retry.classify(status), retry_after=retry_after, status=status
    )


def envelope(raw: Any) -> tuple[dict[str, Any] | None, int | None]:
    """(the reply body as a JSON object, the HTTP status) from an SDK's raw
    response. The body is None unless the status is 200 and the body is a
    strict JSON object within MAX_ENVELOPE_BYTES."""
    try:
        response = raw.http_response
        status = response.status_code
        body = response.content
    except Exception:
        return None, None
    if type(status) is not int:
        return None, None
    if status != 200 or not isinstance(body, bytes) or len(body) > MAX_ENVELOPE_BYTES:
        return None, status
    try:
        doc = _strictjson.loads(body)
    except (_strictjson.StrictJSONError, RecursionError, ValueError):
        return None, status
    return (doc if isinstance(doc, dict) else None), status


def count(v: object) -> int | None:
    """A token count: a non-negative int, not a bool."""
    return v if type(v) is int and v >= 0 else None


def obj(v: object) -> dict[str, Any]:
    """v when it is a JSON object, else an empty one."""
    return v if isinstance(v, dict) else {}


def model_of(doc: dict[str, Any]) -> str:
    m = doc.get("model")
    return m if type(m) is str else ""


def failed(
    outcome: str,
    model_id: str = "",
    tokens_in: int | None = None,
    tokens_out: int | None = None,
    status: int | None = 200,
) -> CloudReply:
    return CloudReply(None, model_id, tokens_in, tokens_out, outcome, status=status)


def answered(
    text: object,
    prompt: CloudPrompt,
    model_id: str,
    tokens_in: int | None,
    tokens_out: int | None,
    status: int | None = 200,
) -> CloudReply:
    """The reply for an answer text: ok with the checked Answer, or
    malformed. An empty model id is malformed too."""
    if type(text) is not str or not model_id:
        return failed("malformed", model_id, tokens_in, tokens_out, status)
    try:
        answer = validate_answer(text, sent_ids(prompt))
    except Malformed:
        return failed("malformed", model_id, tokens_in, tokens_out, status)
    return CloudReply(answer, model_id, tokens_in, tokens_out, "ok", status=status)
