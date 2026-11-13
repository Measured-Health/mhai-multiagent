"""The Google Gemini adapter (Gemini's OpenAI-compatible endpoint).

The real openai SDK over fake transports and synthetic fixtures
(tests/fixtures/cloud/gemini/), shaped as Google's compatibility layer
answers. No socket is opened and no real key is used. Every name, id and
text here is synthetic.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud import scan
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT
from intelligence.boundary.cloud.gemini_adapter import GeminiAdapter, _error_400_outcome
from intelligence.boundary.cloud.reply import answer_schema
from tests.boundary.cloud import adapter_kit as kit
from tests.boundary.cloud.conftest import FAKE_KEY, MODEL

pytestmark = pytest.mark.disable_socket

P = "gemini"
F = GeminiAdapter

EXPECT = {
    "ok": "ok",
    "content_filter": "refused",
    "length": "truncated",
    "invalid_json": "malformed",
    "unknown_citation": "malformed",
    "link": "malformed",
    "empty_choices": "malformed",
    "rate_429": "unavailable",  # retried, then retries used up
    "server_503": "unavailable",
    "unauthorized_401": "auth_failed",
    "forbidden_403": "auth_failed",
    # Google reports a bad key as 400 INVALID_ARGUMENT, reason API_KEY_INVALID.
    "api_key_invalid_400": "auth_failed",
    # A project the API won't serve (region, billing): not our request's fault.
    "failed_precondition_400": "unavailable",
    "bad_request_400": "malformed",
    "payment_402": "unavailable",
    "redirect_301": "unavailable",
}


def test_request_shape() -> None:
    # Exactly these keys and values; text parts only; no store, user,
    # tools, metadata, n, seed or extra_body.
    p = kit.prompt()
    request, reply = kit.render(P, F, p)
    assert (
        str(request.url)
        == "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
    )
    assert request.method == "POST"
    assert request.headers["authorization"] == f"Bearer {FAKE_KEY}"
    assert "x-goog-api-key" not in request.headers and "key=" not in str(request.url)
    kit.check_headers(P, request)
    doc = json.loads(request.content)
    assert doc == {
        "model": MODEL,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "reasoning_effort": "low",
        "messages": [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": [{"type": "text", "text": b} for b in p.blocks()]},
        ],
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": "answer", "strict": True, "schema": answer_schema()},
        },
    }
    assert not {"store", "user", "tools", "metadata", "n", "seed", "extra_body"} & set(doc)
    kit.check_text_blocks_only(P, doc, p)
    assert reply.outcome == "ok"


def test_model_comes_from_config() -> None:
    # No model is built in: the adapter sends the configured one, and its
    # Shape pins it, so a request naming another model is refused.
    a = F("synthetic-gemini-model")
    assert a.shape.fixed_values["/model"] == "synthetic-gemini-model"
    body = json.dumps(a.request(kit.prompt(), "another-model")).encode()
    assert scan.scan_body(body, kit.IDS, a.shape) == "shape"
    with pytest.raises(ValueError):
        F("")


def test_fixtures_are_all_covered() -> None:
    assert kit.fixture_names(P) == sorted(EXPECT)


@pytest.mark.parametrize("name", sorted(EXPECT))
def test_fixture_outcomes(name: str) -> None:
    kit.check_fixture_outcome(P, F, name, EXPECT[name])


def test_reply_mapping_counts_thinking_tokens() -> None:
    # The ok fixture: 812 prompt, 64 completion, total 916 (Gemini counts
    # its thinking tokens in the total, outside completion_tokens).
    _, reply = kit.render(P, F, kit.prompt())
    assert reply.outcome == "ok" and reply.model_id == "synthetic-model-1"
    assert (reply.tokens_in, reply.tokens_out) == (812, 104)


@pytest.mark.parametrize(
    ("body", "want"),
    [
        (
            {"error": {"status": "INVALID_ARGUMENT", "details": [{"reason": "API_KEY_INVALID"}]}},
            "auth_failed",
        ),
        (
            [{"error": {"status": "INVALID_ARGUMENT", "details": [{"reason": "API_KEY_INVALID"}]}}],
            "auth_failed",
        ),
        ({"error": {"status": "FAILED_PRECONDITION"}}, "unavailable"),
        # The inner error object, as the openai SDK hands it over.
        ({"status": "FAILED_PRECONDITION"}, "unavailable"),
        ({"status": "INVALID_ARGUMENT", "details": [{"reason": "API_KEY_INVALID"}]}, "auth_failed"),
        ({"error": {"status": "INVALID_ARGUMENT"}}, None),
        ({"error": {"details": [{"reason": "SOMETHING_ELSE"}]}}, None),
        ({"error": {"details": "API_KEY_INVALID"}}, None),
        ({"error": "API_KEY_INVALID"}, None),
        ({"error": {"message": "API_KEY_INVALID"}}, None),  # the message is never read
        ([{"error": {"status": "FAILED_PRECONDITION"}}, {}], None),
        (None, None),
        ("FAILED_PRECONDITION", None),
    ],
)
def test_error_400_reading_is_closed(body: Any, want: str | None) -> None:
    assert _error_400_outcome(body) == want


def test_api_key_invalid_raises_the_auth_alert(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level("ERROR")
    transport = kit.FixtureTransport(kit.load(P, "api_key_invalid_400"))
    g, _ = kit.generate(kit.make_client(P, F, transport))
    assert g.cloud_outcome == "auth_failed" and len(transport.bodies) == 1
    assert "cloud.auth_failed provider=gemini status=400" in caplog.text
    assert "Synthetic API key not valid" not in caplog.text


@pytest.mark.parametrize(
    ("change", "want"),
    [
        ({"object": "chat.completion.chunk"}, "malformed"),
        ({"object": None}, "malformed"),
        ({"choices": "x"}, "malformed"),
        ({"model": ""}, "malformed"),
    ],
)
def test_odd_replies_fail_closed(change: dict[str, Any], want: str) -> None:
    fx = kit.load(P, "ok")
    fx["body"].update(change)
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == want


@pytest.mark.parametrize(
    ("change", "want"),
    [
        ({"finish_reason": "tool_calls"}, "malformed"),
        ({"finish_reason": "SAFETY"}, "malformed"),  # a native code: unknown here
        ({"finish_reason": None}, "malformed"),
        ({"message": {"role": "assistant", "content": None}}, "malformed"),
        ({"message": {"role": "model", "content": "{}"}}, "malformed"),
        ({"message": {"role": "assistant", "content": "{}", "refusal": "No."}}, "refused"),
    ],
)
def test_odd_choices_fail_closed(change: dict[str, Any], want: str) -> None:
    fx = kit.load(P, "ok")
    fx["body"]["choices"][0].update(change)
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == want


def test_two_choices_are_malformed() -> None:
    fx = kit.load(P, "ok")
    fx["body"]["choices"].append(fx["body"]["choices"][0])
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == "malformed"


def test_extra_reply_fields_are_ignored_not_read() -> None:
    # Gemini adds its own fields (thought signatures); they are neither
    # needed nor trusted.
    fx = kit.load(P, "ok")
    fx["body"]["choices"][0]["message"]["extra_content"] = {"google": {"thought_signature": "x"}}
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == "ok"


def test_reserved_cap_is_the_sent_cap() -> None:
    kit.check_reserved_cap_is_the_sent_cap(P, F, "max_tokens")


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


def test_shape_is_gemini_s_not_xai_s() -> None:
    # Same block layout as xai; the Shape still names gemini, so the hook
    # uses gemini's header list.
    from intelligence.boundary.cloud.xai_adapter import XaiAdapter

    g, x = F(MODEL).shape, XaiAdapter(MODEL).shape
    assert g.patient_paths == x.patient_paths
    assert scan.provider_of(g) == "gemini" and scan.provider_of(x) == "xai"
    assert g != x


def test_client_refuses_an_adapter_with_another_provider_s_shape() -> None:
    # The scan hook's header list comes from the Shape's provider, so the
    # client refuses a gemini adapter carrying xai's Shape (same layout).
    from intelligence.boundary.cloud.xai_adapter import XaiAdapter
    from intelligence.boundary.config import ConfigError

    def factory(model: str) -> GeminiAdapter:
        a = F(model)
        a.shape = XaiAdapter(model).shape
        return a

    with pytest.raises(ConfigError):
        kit.make_client(P, factory, kit.FixtureTransport(kit.load(P, "ok")))
