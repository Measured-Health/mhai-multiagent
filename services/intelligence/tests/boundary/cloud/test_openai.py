"""The OpenAI adapter (plan Task 12; design section 3.2).

The real openai SDK over fake transports and synthetic fixtures
(tests/fixtures/cloud/openai/). No socket is opened and no real key is
used. Every name, id and text here is synthetic.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT
from intelligence.boundary.cloud.openai_adapter import OpenAIAdapter
from intelligence.boundary.cloud.reply import answer_schema
from intelligence.boundary.cloud.scan import scan_body
from tests.boundary.cloud import adapter_kit as kit
from tests.boundary.cloud.conftest import FAKE_KEY, MODEL

pytestmark = pytest.mark.disable_socket

P = "openai"
F = OpenAIAdapter

EXPECT = {
    "ok": "ok",
    "refusal": "refused",
    "incomplete": "truncated",
    "invalid_json": "malformed",
    "extra_keys": "malformed",
    "unknown_citation": "malformed",
    "markers_mismatch": "malformed",
    "link": "malformed",
    "rate_429": "unavailable",  # retried, then retries used up
    "server_503": "unavailable",
    "unauthorized_401": "auth_failed",
    "redirect_301": "unavailable",
}

STATE_FIELDS = (
    "previous_response_id",
    "conversation",
    "tools",
    "tool_choice",
    "user",
    "metadata",
    "safety_identifier",
    "prompt_cache_key",
    "background",
    "temperature",
    "top_p",
    "include",
    "reasoning",
)


def test_request_shape() -> None:
    # A8: exactly these keys and values; input_text parts only.
    p = kit.prompt()
    request, reply = kit.render(P, F, p)
    assert str(request.url) == "https://api.openai.com/v1/responses"
    assert request.method == "POST"
    assert request.headers["authorization"] == f"Bearer {FAKE_KEY}"
    kit.check_headers(P, request)
    doc = json.loads(request.content)
    assert doc == {
        "model": MODEL,
        "instructions": SYSTEM_PROMPT,
        "max_output_tokens": MAX_OUTPUT_TOKENS,
        "store": False,
        "input": [
            {
                "role": "user",
                "content": [{"type": "input_text", "text": b} for b in p.blocks()],
            }
        ],
        "text": {
            "format": {
                "type": "json_schema",
                "name": "answer",
                "strict": True,
                "schema": answer_schema(),
            }
        },
    }
    kit.check_text_blocks_only(P, doc, p)
    assert reply.outcome == "ok"


def test_store_false_and_no_state_fields() -> None:
    # A8: store is false, and no field that keeps or links state is sent;
    # the shape refuses each of them if anything ever added one.
    adapter = F(MODEL)
    raw = kit.body_of(P, F, kit.prompt())
    doc = json.loads(raw)
    assert doc["store"] is False
    assert not set(STATE_FIELDS) & set(doc)
    for field in STATE_FIELDS:
        bad = json.dumps({**doc, field: "resp_SYNTHETIC0001"}).encode()
        assert scan_body(bad, kit.IDS, adapter.shape) == "shape"
    stored = json.dumps({**doc, "store": True}).encode()
    assert scan_body(stored, kit.IDS, adapter.shape) == "shape"


def test_fixtures_are_all_covered() -> None:
    assert kit.fixture_names(P) == sorted(EXPECT)


@pytest.mark.parametrize("name", sorted(EXPECT))
def test_fixture_outcomes(name: str) -> None:
    kit.check_fixture_outcome(P, F, name, EXPECT[name])


def test_reply_mapping_reads_usage_and_model() -> None:
    _, reply = kit.render(P, F, kit.prompt())
    assert reply.outcome == "ok" and reply.model_id == "synthetic-model-1"
    assert (reply.tokens_in, reply.tokens_out) == (812, 64)
    assert reply.answer is not None and reply.answer.citations == ("KB#0001",)


def test_reasoning_items_are_skipped() -> None:
    fx = kit.load(P, "ok")
    fx["body"]["output"].insert(0, {"id": "rs_SYNTHETIC0001", "type": "reasoning", "summary": []})
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == "ok"


@pytest.mark.parametrize(
    ("change", "want"),
    [
        ({"status": "failed"}, "unavailable"),
        ({"status": "in_progress"}, "malformed"),
        ({"status": "queued"}, "malformed"),
        ({"object": "chat.completion"}, "malformed"),
        ({"model": None}, "malformed"),
        ({"output": []}, "malformed"),
        ({"output": "text"}, "malformed"),
        (
            {
                "output": [
                    {"type": "message", "role": "assistant", "content": []},
                    {"type": "message", "role": "assistant", "content": []},
                ]
            },
            "malformed",
        ),
        ({"output": [{"type": "function_call", "name": "x", "arguments": "{}"}]}, "malformed"),
        (
            {"output": [{"type": "message", "role": "assistant", "content": [{"type": "x"}]}]},
            "malformed",
        ),
    ],
)
def test_odd_replies_fail_closed(change: dict[str, Any], want: str) -> None:
    fx = kit.load(P, "ok")
    fx["body"].update(change)
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == want


def test_reserved_cap_is_the_sent_cap() -> None:
    kit.check_reserved_cap_is_the_sent_cap(P, F, "max_output_tokens")


def test_301_not_followed() -> None:
    kit.check_redirect_not_followed(P, F)


def test_sdk_error_logged_by_type_and_status_only(caplog: pytest.LogCaptureFixture) -> None:
    kit.check_error_logged_by_type_and_status_only(P, F, caplog)


def test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    kit.check_hook_refusal(P, F, monkeypatch, wrapped=False)


def test_no_socket_opened() -> None:
    kit.check_no_socket_opened(P, F)


def test_sdk_encoded_bodies_still_blocked() -> None:
    kit.check_sdk_encoded_bodies_blocked(P, F)


def test_shape_covers_every_emitted_path() -> None:
    kit.check_shape_covers_every_emitted_path(P, F)


def test_exact_request_passes_the_scan_and_a_name_anywhere_is_refused() -> None:
    kit.check_exact_request_scanned(P, F)


def test_no_trace_context_sent() -> None:
    kit.check_no_trace_context(P, F)


def test_registered_with_sdk_retries_off() -> None:
    kit.check_registered_with_retries_off(P, F)
    assert client_mod.ADAPTERS[P](MODEL).shape == F(MODEL).shape
