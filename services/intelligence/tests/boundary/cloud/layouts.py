"""Request layouts for the scan tests, one per provider (plan Task 6).

The adapters (Tasks 11 to 13) build the real requests; until they exist,
these stand in for them with each provider's documented shape and the fixed
block order of CloudPrompt.blocks() (ruling R-1): block 0 the question,
block 1 the context, blocks 2 to 6 the chunks. Each layout's Shape is made
the way an adapter makes its own: scan.shape_for with the fixed scalar
values and the schema's leaf values (scan.leaf_values), so a schema
change can't drift from the shape.

sdk_body() encodes the same request with the provider's own SDK, through an
httpx2.MockTransport that never answers: the request hook captures the exact
bytes the SDK would send. Every value here is synthetic.
"""

from __future__ import annotations

import json
from collections.abc import Sequence
from typing import Any

import httpx2

from intelligence.boundary.cloud import scan
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT

PROVIDERS = ("anthropic", "openai", "xai")
MODEL = "synthetic-model-1"
MAX_TOKENS = 1500

# A stand-in for reply.ANSWER_SCHEMA (Task 8): strings, booleans and numbers
# at several depths, as a provider nests it.
SCHEMA: dict[str, Any] = {
    "type": "object",
    "additionalProperties": False,
    "required": ["answer", "citations", "insufficient_evidence"],
    "properties": {
        "answer": {"type": "string", "description": "The answer, citing [KB#nnnn]."},
        "citations": {
            "type": "array",
            "items": {"type": "string", "pattern": "^KB#[0-9]{4}$"},
            "maxItems": 5,
        },
        "insufficient_evidence": {"type": "boolean"},
    },
}

_SCHEMA_PREFIX = {
    "anthropic": "/output_config/format/schema",
    "openai": "/text/format/schema",
    "xai": "/response_format/json_schema/schema",
}


def _block(provider: str, text: str) -> dict[str, str]:
    return {"type": "input_text" if provider == "openai" else "text", "text": text}


def render(
    provider: str, blocks: Sequence[str], schema: dict[str, Any] | None = None
) -> dict[str, Any]:
    """The request body as the provider's adapter lays it out."""
    schema = SCHEMA if schema is None else schema
    content = [_block(provider, b) for b in blocks]
    if provider == "anthropic":
        return {
            "model": MODEL,
            "max_tokens": MAX_TOKENS,
            "system": SYSTEM_PROMPT,
            "messages": [{"role": "user", "content": content}],
            "output_config": {"format": {"type": "json_schema", "schema": schema}},
        }
    if provider == "openai":
        return {
            "model": MODEL,
            "max_output_tokens": MAX_TOKENS,
            "store": False,
            "instructions": SYSTEM_PROMPT,
            "input": [{"role": "user", "content": content}],
            "text": {
                "format": {
                    "type": "json_schema",
                    "name": "answer",
                    "strict": True,
                    "schema": schema,
                }
            },
        }
    if provider == "xai":
        return {
            "model": MODEL,
            "max_tokens": MAX_TOKENS,
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": content},
            ],
            "response_format": {
                "type": "json_schema",
                "json_schema": {"name": "answer", "strict": True, "schema": schema},
            },
        }
    raise ValueError(provider)


def _fixed(provider: str) -> frozenset[str]:
    """Every pointer of the layout outside the blocks and the schema."""
    if provider == "anthropic":
        return frozenset(
            {"/model", "/max_tokens", "/system", "/messages/0/role", "/output_config/format/type"}
        )
    if provider == "openai":
        return frozenset(
            {
                "/model",
                "/max_output_tokens",
                "/store",
                "/instructions",
                "/input/0/role",
                "/text/format/type",
                "/text/format/name",
                "/text/format/strict",
            }
        )
    return frozenset(
        {
            "/model",
            "/max_tokens",
            "/messages/0/role",
            "/messages/0/content",
            "/messages/1/role",
            "/response_format/type",
            "/response_format/json_schema/name",
            "/response_format/json_schema/strict",
        }
    )


def shape(provider: str, schema: dict[str, Any] | None = None) -> scan.Shape:
    """Every leaf outside the blocks, with the value the layout gives it."""
    schema = SCHEMA if schema is None else schema
    rendered = dict(scan.walk(render(provider, [], schema)))
    fixed = {p: rendered[p] for p in _fixed(provider)}
    fixed.update(scan.leaf_values(schema, _SCHEMA_PREFIX[provider]))
    return scan.shape_for(provider, fixed)


def body(provider: str, blocks: Sequence[str], *, ensure_ascii: bool = True) -> bytes:
    """json.dumps, as the plan's Task 6 encodes it."""
    return json.dumps(render(provider, blocks), ensure_ascii=ensure_ascii).encode("utf-8")


def blocks(question: str, context: str = "Category: none\nRisk: none", *chunks: str) -> list[str]:
    """CloudPrompt.blocks()'s order and chunk delimiters."""
    out = [question, context]
    for i, c in enumerate(chunks, start=1):
        out.append(f"<<chunk KB#{i:04d}>>\n{c}\n<<end>>")
    return out


# --- the SDKs' own encoders ------------------------------------------------------------


class _Captured(Exception):
    pass


def _sdk_client(capture: list[bytes]) -> httpx2.Client:
    def hook(request: httpx2.Request) -> None:
        capture.append(request.content)
        raise _Captured

    def never(request: httpx2.Request) -> httpx2.Response:
        raise AssertionError("the transport was reached")

    return httpx2.Client(
        transport=httpx2.MockTransport(never),
        event_hooks={"request": [hook]},
        trust_env=False,
    )


def sdk_body(provider: str, blocks: Sequence[str]) -> bytes:
    """The exact bytes the provider's SDK encodes for this layout (without
    the schema: the SDK test is about the text blocks)."""
    import anthropic
    import openai

    key = "fake" + "0" * 24
    captured: list[bytes] = []
    content: Any = [_block(provider, b) for b in blocks]
    try:
        if provider == "anthropic":
            a = anthropic.Anthropic(api_key=key, http_client=_sdk_client(captured), max_retries=0)
            a.messages.create(
                model=MODEL,
                max_tokens=MAX_TOKENS,
                system=SYSTEM_PROMPT,
                messages=[{"role": "user", "content": content}],
            )
        elif provider == "openai":
            o = openai.OpenAI(api_key=key, http_client=_sdk_client(captured), max_retries=0)
            o.responses.create(
                model=MODEL,
                max_output_tokens=MAX_TOKENS,
                store=False,
                instructions=SYSTEM_PROMPT,
                input=[{"role": "user", "content": content}],
            )
        else:
            x = openai.OpenAI(
                api_key=key,
                base_url="https://api.x.ai/v1",
                http_client=_sdk_client(captured),
                max_retries=0,
            )
            x.chat.completions.create(
                model=MODEL,
                max_tokens=MAX_TOKENS,
                messages=[
                    {"role": "system", "content": SYSTEM_PROMPT},
                    {"role": "user", "content": content},
                ],
            )
    except Exception:  # the SDK may wrap the hook's stop
        pass
    assert len(captured) == 1, "the SDK built no request"
    return captured[0]


def sdk_shape(provider: str) -> scan.Shape:
    """The shape of sdk_body's requests (no schema)."""
    fixed: dict[str, dict[str, Any]] = {
        "anthropic": {
            "/model": MODEL,
            "/max_tokens": MAX_TOKENS,
            "/system": SYSTEM_PROMPT,
            "/messages/0/role": "user",
        },
        "openai": {
            "/model": MODEL,
            "/max_output_tokens": MAX_TOKENS,
            "/store": False,
            "/instructions": SYSTEM_PROMPT,
            "/input/0/role": "user",
        },
        "xai": {
            "/model": MODEL,
            "/max_tokens": MAX_TOKENS,
            "/messages/0/role": "system",
            "/messages/0/content": SYSTEM_PROMPT,
            "/messages/1/role": "user",
        },
    }
    return scan.shape_for(provider, fixed[provider])
