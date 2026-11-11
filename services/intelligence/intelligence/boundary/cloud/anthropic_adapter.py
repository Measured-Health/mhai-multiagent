"""The Anthropic adapter: Messages with strict structured output, no state,
no sampling fields (design section 3.1; plan Task 11).

The request, built with the anthropic SDK from CloudPrompt and nothing
else:

- model, max_tokens (budget.MAX_OUTPUT_TOKENS, the cap the reservation
  counted), system (gate.SYSTEM_PROMPT);
- one user message whose content is text blocks only, in
  CloudPrompt.blocks() order (ruling R-1), at scan.BLOCK_ARRAYS["anthropic"];
- output_config: effort medium (design section 16) and the answer's JSON
  schema (reply.answer_schema()).

Never temperature, top_p, top_k, metadata, tools, tool_choice, fallbacks,
files, images, documents, batches or prior turns. The adapter's Shape pins
every leaf outside the block texts, so the scan refuses anything else.

The SDK client has its retries off (the cloud client retries, under the
budget and the deadline) and its OpenTelemetry trace propagation and spans
off. The reply: stop_reason refusal is refused; max_tokens (or the context
window) is truncated; anything but end_turn with one text block is
malformed; the text must pass reply.validate_answer.
"""

from __future__ import annotations

from typing import Any, Final

import anthropic
import httpx2

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

NAME: Final = "anthropic"
EFFORT: Final = "medium"
TRUNCATED: Final = frozenset({"max_tokens", "model_context_window_exceeded"})


def _fixed(model: str) -> dict[str, Any]:
    """Every part of the request outside the user content's text blocks."""
    return {
        "model": model,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "system": SYSTEM_PROMPT,
        "output_config": {
            "effort": EFFORT,
            "format": {"type": "json_schema", "schema": answer_schema()},
        },
    }


class AnthropicAdapter:
    name = NAME

    def __init__(self, model: str) -> None:
        if not isinstance(model, str) or not model:
            raise ValueError("an adapter needs its model")
        self.model = model
        fixed = scan.leaf_values(_fixed(model))
        fixed["/messages/0/role"] = "user"
        self.shape = scan.shape_for(NAME, fixed)

    def __repr__(self) -> str:
        return f"AnthropicAdapter(model={self.model!r})"

    def make_sdk(self, http_client: httpx2.Client, key: ApiKey, base_url: str) -> Any:
        return anthropic.Anthropic(
            api_key=key.reveal(),
            base_url=base_url,
            http_client=http_client,
            max_retries=0,
            open_telemetry={"propagation": False, "traces": False},
        )

    def request(self, prompt: CloudPrompt, model: str) -> dict[str, Any]:
        """The keyword arguments of messages.create for prompt."""
        prompt = check_prompt(prompt)
        body = _fixed(model)
        body["messages"] = [
            {"role": "user", "content": [{"type": "text", "text": b} for b in prompt.blocks()]}
        ]
        return body

    def call(self, sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply:
        kwargs = self.request(prompt, model)
        try:
            raw = sdk.messages.with_raw_response.create(**kwargs)
        except anthropic.APIStatusError as e:
            return status_reply(e)
        return self.parse(raw, prompt)

    def parse(self, raw: Any, prompt: CloudPrompt) -> CloudReply:
        doc, status = envelope(raw)
        if doc is None:
            return failed("malformed", status=status)
        model_id = model_of(doc)
        usage = obj(doc.get("usage"))
        tin = count(usage.get("input_tokens"))
        for extra in ("cache_creation_input_tokens", "cache_read_input_tokens"):
            v = usage.get(extra)
            if v is not None and tin is not None:
                n = count(v)
                tin = None if n is None else tin + n
        tout = count(usage.get("output_tokens"))
        if doc.get("type") != "message" or doc.get("role") != "assistant":
            return failed("malformed", model_id, tin, tout, status)
        stop = doc.get("stop_reason")
        if stop == "refusal":
            return failed("refused", model_id, tin, tout, status)
        if stop in TRUNCATED:
            return failed("truncated", model_id, tin, tout, status)
        content = doc.get("content")
        if stop != "end_turn" or not isinstance(content, list) or len(content) != 1:
            return failed("malformed", model_id, tin, tout, status)
        block = obj(content[0])
        if block.get("type") != "text":
            return failed("malformed", model_id, tin, tout, status)
        return answered(block.get("text"), prompt, model_id, tin, tout, status)
