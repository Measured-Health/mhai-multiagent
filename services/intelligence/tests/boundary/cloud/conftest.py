"""Fixtures for the cloud provider tests.

Every key here is synthetic and obviously fake: built from repeated
characters at run time, never a real key and never a literal secret.
"""

from __future__ import annotations

import json
import os
import threading
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx2
import pytest

# Fake keys. One has a provider's prefix (the redaction patterns catch it);
# one has none (only the exact-value rule catches it).
FAKE_KEY = "fake" + "0" * 24
FAKE_SK_KEY = "sk-" + "f" * 24


@pytest.fixture
def key_file(tmp_path: Path) -> Callable[..., Path]:
    """Write a key file with the given contents and mode (0400 by default)."""
    n = 0

    def make(contents: str = FAKE_KEY + "\n", mode: int = 0o400) -> Path:
        nonlocal n
        n += 1
        p = tmp_path / f"key-{n}"
        p.write_text(contents, encoding="utf-8")
        os.chmod(p, mode)
        return p

    return make


# --- the cloud client's fakes (plan Task 9) ---------------------------------------------
#
# FakeAdapter doesn't just return a scripted reply: its call POSTs the whole
# CloudPrompt, rendered in the Anthropic layout (scan.Shape admits only a
# provider's block layout), through the httpx2.Client it was given, and only
# then returns the scripted reply. So the scan hook and CapturingTransport
# see real bytes, and the dry render captures the same body.

MODEL = "synthetic-model-1"
BASE_URL = "https://api.anthropic.com"


def fake_render(prompt: Any, model: str) -> dict[str, Any]:
    from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
    from intelligence.boundary.cloud.reply import answer_schema

    return {
        "model": model,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "system": prompt.system,
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": b} for b in prompt.blocks()]}
        ],
        "output_config": {"format": {"type": "json_schema", "schema": answer_schema()}},
    }


def fake_shape() -> Any:
    from intelligence.boundary.cloud import scan
    from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
    from intelligence.boundary.cloud.gate import SYSTEM_PROMPT
    from intelligence.boundary.cloud.reply import answer_schema

    fixed: dict[str, Any] = {
        "/model": MODEL,
        "/max_tokens": MAX_OUTPUT_TOKENS,
        "/system": SYSTEM_PROMPT,
        "/messages/0/role": "user",
        "/output_config/format/type": "json_schema",
    }
    fixed.update(scan.leaf_values(answer_schema(), "/output_config/format/schema"))
    return scan.shape_for("anthropic", fixed)


def FakeConnectionError(url: str = BASE_URL) -> Exception:  # noqa: N802
    """The SDK's APIConnectionError: anthropic wraps a hook's exception in one."""
    import anthropic

    return anthropic.APIConnectionError(request=httpx2.Request("POST", url))


@dataclass
class FakeSdk:
    http: httpx2.Client
    key: Any
    base_url: str


class FakeAdapter:
    """script: one item per attempt that reached the transport: a CloudReply,
    an exception (raised), or an outcome string ("ok" answers citing the
    first chunk sent; "insufficient"; anything else is that outcome).
    headers: extra request headers (a trace header, say). wrap: raise
    FakeConnectionError from whatever the post raised, as an SDK does."""

    name = "anthropic"

    def __init__(
        self,
        script: list[Any] | None = None,
        *,
        wrap: bool = False,
        headers: Callable[[], dict[str, str]] | None = None,
        status: int | None = None,
        retry_after: float | None = None,
    ) -> None:
        self.shape = fake_shape()
        self.script = list(script or [])
        self.wrap = wrap
        self.headers = headers
        self.status = status
        self.retry_after = retry_after
        self.calls = 0  # adapter.call, the dry render included
        self.sends = 0  # posts that reached a transport and came back
        self.hooks: list[list[Any]] = []
        self.lock = threading.Lock()

    def make_sdk(self, http_client: httpx2.Client, key: Any, base_url: str) -> FakeSdk:
        self.hooks.append(list(http_client.event_hooks["request"]))
        return FakeSdk(http_client, key, base_url)

    def call(self, sdk: FakeSdk, prompt: Any, model: str, timeout: float) -> Any:
        from intelligence.boundary.cloud.client import CloudReply
        from intelligence.boundary.cloud.reply import validate_answer

        with self.lock:
            self.calls += 1
        body = json.dumps(fake_render(prompt, model)).encode("utf-8")
        headers = {"content-type": "application/json", "x-api-key": sdk.key.reveal()}
        if self.headers is not None:
            headers.update(self.headers())
        try:
            sdk.http.post(f"{sdk.base_url}/v1/messages", content=body, headers=headers)
        except Exception as e:
            if self.wrap:
                raise FakeConnectionError(f"{sdk.base_url}/v1/messages") from e
            raise
        with self.lock:
            self.sends += 1
            item = self.script.pop(0) if self.script else "ok"
        if isinstance(item, BaseException):
            raise item
        if not isinstance(item, str):
            return item
        sent = frozenset(c.chunk_id for c in prompt.chunks)
        first = prompt.chunks[0].chunk_id if prompt.chunks else "KB#0000"
        if item == "ok":
            doc = {
                "answer": f"Synthetic answer [{first}].",
                "citations": [first],
                "insufficient_evidence": False,
            }
            return CloudReply(validate_answer(doc, sent), "synthetic-2026-11-01", 120, 40, "ok")
        if item == "insufficient":
            doc = {"answer": "Not in the chunks.", "citations": [], "insufficient_evidence": True}
            return CloudReply(validate_answer(doc, sent), "synthetic-2026-11-01", 120, 10, "ok")
        return CloudReply(None, "", 100, 0, item, retry_after=self.retry_after, status=self.status)


class CapturingTransport(httpx2.BaseTransport):
    """Records every request that reaches it (body and headers) and
    answers 200 {}. close() is recorded."""

    def __init__(self) -> None:
        self.bodies: list[bytes] = []
        self.headers: list[dict[str, str]] = []
        self.closed = False
        self.lock = threading.Lock()

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        return httpx2.Response(200, json={}, request=request)

    def handle_request(self, request: httpx2.Request) -> httpx2.Response:
        if self.closed:
            raise RuntimeError("the shared transport was closed")
        with self.lock:
            self.bodies.append(bytes(request.read()))
            self.headers.append(dict(request.headers))
        return self.respond(request)

    def close(self) -> None:
        self.closed = True


class ScriptedTransport(CapturingTransport):
    """Answers from a script: a status code or an exception per request
    (200 when the script runs out)."""

    def __init__(self, script: list[Any]) -> None:
        super().__init__()
        self.script = list(script)

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        item = self.script.pop(0) if self.script else 200
        if isinstance(item, BaseException):
            raise item
        return httpx2.Response(int(item), json={}, request=request)
