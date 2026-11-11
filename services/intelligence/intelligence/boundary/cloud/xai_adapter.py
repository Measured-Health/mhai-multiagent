"""The xAI adapter: Chat Completions with a strict schema, the
zero-retention header checked on every reply (design section 3.3; plan
Task 13).

The request, built with the openai SDK (xAI's API is OpenAI-compatible)
from CloudPrompt and nothing else:

- model, max_tokens (budget.MAX_OUTPUT_TOKENS, the cap the reservation
  counted);
- messages: the system message (gate.SYSTEM_PROMPT) at /messages/0, then
  one user message whose content is text parts only, in
  CloudPrompt.blocks() order (ruling R-1), at scan.BLOCK_ARRAYS["xai"];
- response_format: json_schema, strict, with reply.answer_schema().

Never tools, user, store, search_parameters or image parts. The adapter's
Shape pins every leaf outside the block texts, so the scan refuses
anything else.

The reply is read raw, so the headers are seen first: unless the
x-zero-data-retention header is "true" (case aside), the outcome is
retention_unconfirmed and the body is discarded unread (A9). Only
APP_ENV=development relaxes that. Then: a refusal is refused (and so is
finish_reason content_filter); finish_reason length is truncated; anything
but one choice with finish_reason stop and string content is malformed;
the text must pass reply.validate_answer. Output tokens count reasoning
tokens too: xAI bills them outside completion_tokens.
"""

from __future__ import annotations

from typing import Any, Final

import httpx2
import openai

from intelligence.boundary.cloud import scan
from intelligence.boundary.cloud.adapter_common import (
    answered,
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

NAME: Final = "xai"
SCHEMA_NAME: Final = "answer"
RETENTION_HEADER: Final = "x-zero-data-retention"


def _fixed(model: str) -> dict[str, Any]:
    """Every part of the request outside the messages."""
    return {
        "model": model,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": SCHEMA_NAME, "strict": True, "schema": answer_schema()},
        },
    }


def _tokens_out(usage: dict[str, Any], tokens_in: int | None) -> int | None:
    """Completion tokens plus reasoning tokens. With total_tokens, what it
    holds beyond the prompt (whichever way the reasoning is counted);
    without it, completion plus reasoning, which can only over-count."""
    completion = count(usage.get("completion_tokens"))
    if completion is None:
        return None
    total = count(usage.get("total_tokens"))
    if total is not None and tokens_in is not None:
        return max(completion, total - tokens_in)
    details = obj(usage.get("completion_tokens_details"))
    reasoning = count(details.get("reasoning_tokens")) or 0
    return completion + reasoning


class XaiAdapter:
    name = NAME

    def __init__(self, model: str, *, development: bool = False) -> None:
        if not isinstance(model, str) or not model:
            raise ValueError("an adapter needs its model")
        self.model = model
        self.development = development is True
        fixed = scan.leaf_values(_fixed(model))
        fixed["/messages/0/role"] = "system"
        fixed["/messages/0/content"] = SYSTEM_PROMPT
        fixed["/messages/1/role"] = "user"
        self.shape = scan.shape_for(NAME, fixed)

    def __repr__(self) -> str:
        return f"XaiAdapter(model={self.model!r}, development={self.development})"

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
            return status_reply(e)
        return self.parse(raw, prompt)

    def retained(self, raw: Any) -> bool:
        """Whether the reply may be read: the zero-retention header says
        true, or this is development."""
        if self.development:
            return True
        try:
            value = raw.http_response.headers.get(RETENTION_HEADER)
        except Exception:
            return False
        return isinstance(value, str) and value.strip().casefold() == "true"

    def parse(self, raw: Any, prompt: CloudPrompt) -> CloudReply:
        if not self.retained(raw):
            # Discarded unread: no text, no model id, no counts (the whole
            # reservation is settled as used).
            status = getattr(getattr(raw, "http_response", None), "status_code", None)
            return failed("retention_unconfirmed", status=status if type(status) is int else None)
        doc, status = envelope(raw)
        if doc is None:
            return failed("malformed", status=status)
        model_id = model_of(doc)
        usage = obj(doc.get("usage"))
        tin = count(usage.get("prompt_tokens"))
        tout = _tokens_out(usage, tin)
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
