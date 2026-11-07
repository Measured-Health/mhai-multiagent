"""The OpenAI adapter: Responses with store=false and a strict JSON schema
(design section 3.2; plan Task 12).

The request, built with the openai SDK from CloudPrompt and nothing else:

- model, instructions (gate.SYSTEM_PROMPT), max_output_tokens
  (budget.MAX_OUTPUT_TOKENS, the cap the reservation counted; it bounds
  reasoning tokens too), store false;
- input: one user message whose content is input_text parts only, in
  CloudPrompt.blocks() order (ruling R-1), at scan.BLOCK_ARRAYS["openai"];
- text.format: json_schema, strict, with reply.answer_schema().

Never previous_response_id, conversation, tools, user, metadata,
safety_identifier, prompt_cache_key or background. The adapter's Shape
pins every leaf outside the block texts, so the scan refuses anything else.

The SDK client has its retries off (the cloud client retries, under the
budget and the deadline). The reply: a refusal content item is refused;
status incomplete is truncated; anything but status completed with one
assistant message holding one output_text is malformed (reasoning items
are skipped); the text must pass reply.validate_answer.
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

NAME: Final = "openai"
SCHEMA_NAME: Final = "answer"


def _fixed(model: str) -> dict[str, Any]:
    """Every part of the request outside the input's text parts."""
    return {
        "model": model,
        "instructions": SYSTEM_PROMPT,
        "max_output_tokens": MAX_OUTPUT_TOKENS,
        "store": False,
        "text": {
            "format": {
                "type": "json_schema",
                "name": SCHEMA_NAME,
                "strict": True,
                "schema": answer_schema(),
            }
        },
    }


class OpenAIAdapter:
    name = NAME

    def __init__(self, model: str) -> None:
        if not isinstance(model, str) or not model:
            raise ValueError("an adapter needs its model")
        self.model = model
        fixed = scan.leaf_values(_fixed(model))
        fixed["/input/0/role"] = "user"
        self.shape = scan.shape_for(NAME, fixed)

    def __repr__(self) -> str:
        return f"OpenAIAdapter(model={self.model!r})"

    def make_sdk(self, http_client: httpx2.Client, key: ApiKey, base_url: str) -> Any:
        return openai.OpenAI(
            api_key=key.reveal(),
            base_url=base_url,
            http_client=http_client,
            max_retries=0,
        )

    def request(self, prompt: CloudPrompt, model: str) -> dict[str, Any]:
        """The keyword arguments of responses.create for prompt."""
        prompt = check_prompt(prompt)
        body = _fixed(model)
        parts = [{"type": "input_text", "text": b} for b in prompt.blocks()]
        body["input"] = [{"role": "user", "content": parts}]
        return body

    def call(self, sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply:
        kwargs = self.request(prompt, model)
        try:
            raw = sdk.responses.with_raw_response.create(**kwargs)
        except openai.APIStatusError as e:
            return status_reply(e)
        return self.parse(raw, prompt)

    def parse(self, raw: Any, prompt: CloudPrompt) -> CloudReply:
        doc, status = envelope(raw)
        if doc is None:
            return failed("malformed", status=status)
        model_id = model_of(doc)
        usage = obj(doc.get("usage"))
        tin = count(usage.get("input_tokens"))
        tout = count(usage.get("output_tokens"))  # reasoning tokens included
        if doc.get("object") != "response":
            return failed("malformed", model_id, tin, tout, status)
        state = doc.get("status")
        output = doc.get("output")
        if not isinstance(output, list):
            return failed("malformed", model_id, tin, tout, status)
        messages = [obj(o) for o in output if obj(o).get("type") != "reasoning"]
        parts = [obj(c) for m in messages for c in (m.get("content") or [])]
        if any(p.get("type") == "refusal" for p in parts):
            return failed("refused", model_id, tin, tout, status)
        if state == "incomplete":
            return failed("truncated", model_id, tin, tout, status)
        if state == "failed":
            return failed("unavailable", model_id, tin, tout, status)
        if state != "completed" or len(messages) != 1:
            return failed("malformed", model_id, tin, tout, status)
        message = messages[0]
        content = message.get("content")
        if (
            message.get("type") != "message"
            or message.get("role") != "assistant"
            or not isinstance(content, list)
            or len(content) != 1
            or parts[0].get("type") != "output_text"
        ):
            return failed("malformed", model_id, tin, tout, status)
        return answered(parts[0].get("text"), prompt, model_id, tin, tout, status)
