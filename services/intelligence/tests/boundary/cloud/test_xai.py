"""The xAI adapter (plan Task 13; design section 3.3).

The real openai SDK (xAI's API is OpenAI-compatible) over fake transports
and synthetic fixtures (tests/fixtures/cloud/xai/). No socket is opened and
no real key is used. Every name, id and text here is synthetic.
"""

from __future__ import annotations

import json
import logging
from typing import Any

import pytest

from intelligence.boundary.cloud import adapter_common
from intelligence.boundary.cloud import client as client_mod
from intelligence.boundary.cloud.apikey import quiet_sdk_loggers
from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
from intelligence.boundary.cloud.gate import SYSTEM_PROMPT
from intelligence.boundary.cloud.reply import answer_schema
from intelligence.boundary.cloud.xai_adapter import XaiAdapter
from tests.boundary.cloud import adapter_kit as kit
from tests.boundary.cloud.conftest import FAKE_KEY, MODEL

pytestmark = pytest.mark.disable_socket

P = "xai"
F = XaiAdapter

EXPECT = {
    "ok": "ok",
    "ok_no_zdr_header": "retention_unconfirmed",
    "ok_zdr_false": "retention_unconfirmed",
    "refusal": "refused",
    "length": "truncated",
    "invalid_json": "malformed",
    "unknown_citation": "malformed",
    "link": "malformed",
    "rate_429": "unavailable",  # retried, then retries used up
    "server_503": "unavailable",
    "unauthorized_401": "auth_failed",
    "redirect_301": "unavailable",
}


def test_request_shape() -> None:
    # A8: exactly these keys and values; text parts only; no tools, user,
    # store or search parameters.
    p = kit.prompt()
    request, reply = kit.render(P, F, p)
    assert str(request.url) == "https://api.x.ai/v1/chat/completions"
    assert request.method == "POST"
    assert request.headers["authorization"] == f"Bearer {FAKE_KEY}"
    kit.check_headers(P, request)
    doc = json.loads(request.content)
    assert doc == {
        "model": MODEL,
        "max_tokens": MAX_OUTPUT_TOKENS,
        "messages": [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": [{"type": "text", "text": b} for b in p.blocks()]},
        ],
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": "answer", "strict": True, "schema": answer_schema()},
        },
    }
    assert not {"tools", "user", "store", "search_parameters"} & set(doc)
    kit.check_text_blocks_only(P, doc, p)
    assert reply.outcome == "ok"


def test_fixtures_are_all_covered() -> None:
    assert kit.fixture_names(P) == sorted(EXPECT)


@pytest.mark.parametrize("name", sorted(EXPECT))
def test_fixture_outcomes(name: str) -> None:
    kit.check_fixture_outcome(P, F, name, EXPECT[name])


def test_reply_mapping_counts_reasoning_tokens() -> None:
    # The ok fixture: 812 prompt, 64 completion, 20 reasoning, total 896.
    _, reply = kit.render(P, F, kit.prompt())
    assert reply.outcome == "ok" and reply.model_id == "synthetic-model-1"
    assert (reply.tokens_in, reply.tokens_out) == (812, 84)


def test_retention_header_missing_or_false_discards(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    # A9: the answer text never reaches validate_answer or a log, and the
    # whole reservation counts (nothing was read, so nothing is known).
    quiet_sdk_loggers()
    caplog.set_level(logging.DEBUG)

    def never(*a: Any, **k: Any) -> Any:
        raise AssertionError("the reply was read")

    monkeypatch.setattr(adapter_common, "validate_answer", never)
    monkeypatch.setattr(client_mod, "validate_answer", never)
    for name in ("ok_no_zdr_header", "ok_zdr_false"):
        transport = kit.FixtureTransport(kit.load(P, name))
        g, budget = kit.generate(kit.make_client(P, F, transport))
        assert g.cloud_outcome == "retention_unconfirmed" and g.available is False
        assert g.response == "" and g.versions == {}
        assert len(transport.bodies) == 1  # not retried
        assert budget.settled == [("r1", None)]
    assert "Synthetic answer" not in caplog.text


@pytest.mark.parametrize("value", ["true", "TRUE", " True "])
def test_retention_header_true_in_any_case_is_read(value: str) -> None:
    fx = kit.load(P, "ok")
    fx["headers"]["x-zero-data-retention"] = value
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == "ok"


@pytest.mark.parametrize("value", ["false", "", "1", "yes", "true, true", "truee"])
def test_retention_header_other_values_discard(value: str) -> None:
    fx = kit.load(P, "ok")
    fx["headers"]["x-zero-data-retention"] = value
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == "retention_unconfirmed"


@pytest.mark.parametrize(
    ("app_env", "relaxed"),
    [(None, False), ("production", False), ("demo", False), ("development", True)],
)
def test_retention_header_relaxed_only_in_development(
    monkeypatch: pytest.MonkeyPatch, app_env: str | None, relaxed: bool
) -> None:
    if app_env is None:
        monkeypatch.delenv("APP_ENV", raising=False)
    else:
        monkeypatch.setenv("APP_ENV", app_env)
    registered = client_mod.ADAPTERS[P]

    def factory(model: str) -> Any:
        return registered(model)

    transport = kit.FixtureTransport(kit.load(P, "ok_no_zdr_header"))
    g, _ = kit.generate(kit.make_client(P, factory, transport))
    assert g.cloud_outcome == ("ok" if relaxed else "retention_unconfirmed")
    assert F(MODEL).development is False
    assert F(MODEL, development="yes").development is False  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("change", "want"),
    [
        ({"object": "chat.completion.chunk"}, "malformed"),
        ({"choices": []}, "malformed"),
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
        ({"finish_reason": "content_filter"}, "refused"),
        ({"finish_reason": "tool_calls"}, "malformed"),
        ({"finish_reason": None}, "malformed"),
        ({"message": {"role": "assistant", "content": None, "refusal": None}}, "malformed"),
        ({"message": {"role": "user", "content": "{}", "refusal": None}}, "malformed"),
    ],
)
def test_odd_choices_fail_closed(change: dict[str, Any], want: str) -> None:
    fx = kit.load(P, "ok")
    fx["body"]["choices"][0].update(change)
    g, _ = kit.generate(kit.make_client(P, F, kit.FixtureTransport(fx)))
    assert g.cloud_outcome == want


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
