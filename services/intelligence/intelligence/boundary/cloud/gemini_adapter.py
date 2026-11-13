"""The Google Gemini adapter: Gemini's OpenAI-compatible Chat Completions
endpoint with a strict JSON schema.

Why the compatibility layer and not the native API: Google documents
structured output through it with the openai SDK (chat.completions.parse,
which sends response_format json_schema with strict true), so the request
is built and sent by the same SDK, transport, scan hook and retry rules as
the xAI adapter, and no second SDK is added. The schema is checked again
here (reply.validate_answer), so a keyword Gemini doesn't enforce can't let
a bad answer through.

The request, built with the openai SDK from CloudPrompt and nothing else:

- model (from INTELLIGENCE_CLOUD_GEMINI_MODEL; never hard-coded here),
  max_tokens (budget.MAX_OUTPUT_TOKENS, the cap the reservation counted;
  Gemini counts thinking tokens inside it), reasoning_effort "low" (so
  thinking doesn't spend the cap; one of Google's documented values);
- messages: the system message (gate.SYSTEM_PROMPT) at /messages/0, then
  one user message whose content is text parts only, in
  CloudPrompt.blocks() order (ruling R-1), at scan.BLOCK_ARRAYS["gemini"];
- response_format: json_schema, strict, with reply.answer_schema().

Never store, user, tools, metadata, n, seed, extra_body (cached content,
thinking config) or image, audio or file parts. The adapter's Shape pins
every leaf outside the block texts, so the scan refuses anything else.

Retention: Gemini has no per-reply retention header (xAI's
x-zero-data-retention). What stops Google using prompts to improve its
products is the key's project being on the paid tier, with billing on; the
worker refuses to start (outside APP_ENV=development, where it warns) unless
INTELLIGENCE_CLOUD_GEMINI_PAID_TIER says verified (settings.py), and
deploy/README.md says how to check.

The reply: a refusal, or finish_reason content_filter (Gemini's safety,
recitation and blocklist stops), is refused; finish_reason length is
truncated; anything but one choice with finish_reason stop and string
content is malformed (an empty choices list, as for a blocked prompt,
included); the text must pass reply.validate_answer. Output tokens count
thinking tokens too.

Errors: a status is the outcome through retry.classify, as for the other
providers, with two Gemini-specific readings of a 400 (Google reports a bad
key as 400 INVALID_ARGUMENT with reason API_KEY_INVALID, and an unusable
project, such as one whose region or billing the API refuses, as 400
FAILED_PRECONDITION): those are auth_failed and unavailable, not malformed.
Only those two codes are read from the error body; its message is never
kept or logged.
"""

from __future__ import annotations

from dataclasses import replace
from typing import Any, Final

import httpx2
import openai

from intelligence.boundary.cloud import scan
from intelligence.boundary.cloud.adapter_common import (
    answered,
    chat_tokens_out,
    check_prompt,
    count,
    envelope,
    failed,
    model_of,
    obj,
    status_reply,
)
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
from intelligence.boundary.cloud.client import CloudReply
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT, CloudPrompt
from intelligence.boundary.cloud.reply import answer_schema

NAME: Final = "gemini"
SCHEMA_NAME: Final = "answer"
REASONING_EFFORT: Final = "low"

# The 400 error codes read from Gemini's error body (google.rpc: the
# ErrorInfo reason and the status), and the outcome each gives. Any other
# 400 stays malformed.
_REASON_OUTCOMES: Final = {"API_KEY_INVALID": "auth_failed"}
_STATUS_OUTCOMES: Final = {"FAILED_PRECONDITION": "unavailable"}
_MAX_DETAILS: Final = 16


def _fixed(model: str) -> dict[str, Any]:
    """Every part of the request outside the messages."""
    return {
        "model": model,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "reasoning_effort": REASONING_EFFORT,
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": SCHEMA_NAME, "strict": True, "schema": answer_schema()},
        },
    }


def _error_400_outcome(body: object) -> str | None:
    """The outcome a 400's error body names, or None. Google sends
    {"error": {...}}, or that object in a one-item list; the openai SDK
    hands over the inner error object of a JSON object body. Only the
    status and the details' reasons are read."""
    if isinstance(body, list) and len(body) == 1:
        body = body[0]
    doc = obj(body)
    error = obj(doc["error"]) if "error" in doc else doc
    details = error.get("details")
    if isinstance(details, list):
        for d in details[:_MAX_DETAILS]:
            reason = obj(d).get("reason")
            if type(reason) is str and reason in _REASON_OUTCOMES:
                return _REASON_OUTCOMES[reason]
    status = error.get("status")
    if type(status) is str and status in _STATUS_OUTCOMES:
        return _STATUS_OUTCOMES[status]
    return None


def gemini_status_reply(error: Any) -> CloudReply:
    """adapter_common.status_reply, with Gemini's two 400 readings."""
    reply = status_reply(error)
    if reply.status != 400:
        return reply
    try:
        outcome = _error_400_outcome(getattr(error, "body", None))
    except Exception:
        outcome = None
    return reply if outcome is None else replace(reply, outcome=outcome)


class GeminiAdapter:
    name = NAME

    def __init__(self, model: str) -> None:
        if not isinstance(model, str) or not model:
            raise ValueError("an adapter needs its model")
        self.model = model
        fixed = scan.leaf_values(_fixed(model))
        fixed["/messages/0/role"] = "system"
        fixed["/messages/0/content"] = SYSTEM_PROMPT
        fixed["/messages/1/role"] = "user"
        self.shape = scan.shape_for(NAME, fixed)

    def __repr__(self) -> str:
        return f"GeminiAdapter(model={self.model!r})"

    def make_sdk(self, http_client: httpx2.Client, key: ApiKey, base_url: str) -> Any:
        return openai.OpenAI(
            api_key=key.reveal(),
            base_url=base_url,
            http_client=http_client,
            max_retries=0,
        )

    def request(self, prompt: CloudPrompt, model: str) -> dict[str, Any]:
        """The keyword arguments of chat.completions.create for prompt."""
        prompt = check_prompt(prompt)
        body = _fixed(model)
        parts = [{"type": "text", "text": b} for b in prompt.blocks()]
        body["messages"] = [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": parts},
        ]
        return body

    def call(self, sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply:
        kwargs = self.request(prompt, model)
        try:
            raw = sdk.chat.completions.with_raw_response.create(**kwargs)
        except openai.APIStatusError as e:
            return gemini_status_reply(e)
        return self.parse(raw, prompt)

    def parse(self, raw: Any, prompt: CloudPrompt) -> CloudReply:
        doc, status = envelope(raw)
        if doc is None:
            return failed("malformed", status=status)
        model_id = model_of(doc)
        usage = obj(doc.get("usage"))
        tin = count(usage.get("prompt_tokens"))
        tout = chat_tokens_out(usage, tin)
        choices = doc.get("choices")
        if doc.get("object") != "chat.completion" or not isinstance(choices, list):
            return failed("malformed", model_id, tin, tout, status)
        if len(choices) != 1:
            return failed("malformed", model_id, tin, tout, status)
        choice = obj(choices[0])
        message = obj(choice.get("message"))
        finish = choice.get("finish_reason")
        refusal = message.get("refusal")
        if (isinstance(refusal, str) and refusal) or finish == "content_filter":
            return failed("refused", model_id, tin, tout, status)
        if finish == "length":
            return failed("truncated", model_id, tin, tout, status)
        if finish != "stop" or message.get("role") != "assistant" or message.get("tool_calls"):
            return failed("malformed", model_id, tin, tout, status)
        return answered(message.get("content"), prompt, model_id, tin, tout, status)
