"""The Anthropic adapter (plan Task 11; design section 3.1).

The real anthropic SDK over fake transports and synthetic fixtures
(tests/fixtures/cloud/anthropic/). No socket is opened and no real key is
used. Every name, id and text here is synthetic.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud.anthropic_adapter import AnthropicAdapter
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT
from intelligence.boundary.cloud.reply import answer_schema
from tests.boundary.cloud import adapter_kit as kit
from tests.boundary.cloud.conftest import FAKE_KEY, MODEL

pytestmark = pytest.mark.disable_socket

P = "anthropic"
F = AnthropicAdapter

EXPECT = {
    "ok": "ok",
    "refusal": "refused",
    "max_tokens": "truncated",
    "invalid_json": "malformed",
    "extra_keys": "malformed",
    "unknown_citation": "malformed",
    "markers_mismatch": "malformed",
    "link": "malformed",
    "overloaded_529": "unavailable",  # retried, then retries used up
    "rate_429": "unavailable",
    "server_503": "unavailable",
    "unauthorized_401": "auth_failed",
    "redirect_301": "unavailable",
}


def test_request_shape() -> None:
    # A8: exactly these keys, these values; no sampling fields, no metadata,
    # no tools, no prior turns; text blocks only.
    p = kit.prompt()
    request, reply = kit.render(P, F, p)
    assert str(request.url) == "https://api.anthropic.com/v1/messages"
    assert request.method == "POST"
    # anthropic 2.3.0 sends the key as a bearer token, not x-api-key.
    assert request.headers["authorization"] == f"Bearer {FAKE_KEY}"
    assert "x-api-key" not in request.headers
    assert "anthropic-version" in request.headers
    kit.check_headers(P, request)
    doc = json.loads(request.content)
    assert doc == {
        "model": MODEL,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "system": SYSTEM_PROMPT,
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": b} for b in p.blocks()]}
        ],
        "output_config": {
            "effort": "medium",
            "format": {"type": "json_schema", "schema": answer_schema()},
        },
    }
    kit.check_text_blocks_only(P, doc, p)
    assert reply.outcome == "ok"


def test_fixtures_are_all_covered() -> None:
    assert kit.fixture_names(P) == sorted(EXPECT)


@pytest.mark.parametrize("name", sorted(EXPECT))
def test_fixture_outcomes(name: str) -> None:
    kit.check_fixture_outcome(P, F, name, EXPECT[name])


def test_reply_mapping_reads_usage_model_and_stop_reason() -> None:
    _, reply = kit.render(P, F, kit.prompt())
    assert reply.outcome == "ok" and reply.model_id == "synthetic-model-1"
    assert (reply.tokens_in, reply.tokens_out) == (812, 64)
    assert reply.answer is not None and reply.answer.citations == ("KB#0001",)


@pytest.mark.parametrize(
    "change",
    [
        {"stop_reason": "tool_use"},
        {"stop_reason": "pause_turn"},
        {"stop_reason": "stop_sequence"},
        {"stop_reason": "model_context_window_exceeded"},
        {"type": "error"},
        {"role": "user"},
        {"model": 7},
        {"content": []},
        {"content": [{"type": "thinking", "thinking": "x"}]},
        {"content": [{"type": "text", "text": "{}"}, {"type": "text", "text": "{}"}]},
    ],
)
def test_odd_replies_fail_closed(change: dict[str, Any]) -> None:
    fx = kit.load(P, "ok")
    fx["body"].update(change)
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    want = "truncated" if change.get("stop_reason") == "model_context_window_exceeded" else None
    assert g.cloud_outcome == (want or "malformed")


def test_reserved_cap_is_the_sent_cap() -> None:
    kit.check_reserved_cap_is_the_sent_cap(P, F, "max_tokens")


def test_301_not_followed() -> None:
    kit.check_redirect_not_followed(P, F)


def test_sdk_error_logged_by_type_and_status_only(caplog: pytest.LogCaptureFixture) -> None:
    kit.check_error_logged_by_type_and_status_only(P, F, caplog)


def test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    kit.check_hook_refusal(P, F, monkeypatch, wrapped=True)


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
