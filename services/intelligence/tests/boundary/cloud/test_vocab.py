"""The closed vocabularies (Global Constraints): Python and Go hold the
same lists, and core's TestOutcomeVocabularyMatchesWorker reads this
module's file to compare them (K14)."""

from __future__ import annotations

import pytest

from intelligence.boundary.cloud import vocab

pytestmark = pytest.mark.disable_socket


def test_vocab_lists_are_closed_and_sorted() -> None:
    for name in ("PROVIDERS", "ROUTES", "EGRESS", "OUTCOMES"):
        values = getattr(vocab, name)
        assert isinstance(values, tuple), name
        assert list(values) == sorted(set(values)), name
    assert vocab.PROVIDERS == ("anthropic", "gemini", "openai", "xai")
    assert vocab.ROUTES == (
        "cloud:anthropic",
        "cloud:gemini",
        "cloud:openai",
        "cloud:xai",
        "local",
    )
    assert vocab.EGRESS == ("anthropic", "gemini", "none", "openai", "xai")
    assert set(vocab.OUTCOMES) == {
        "ok",
        "egress_blocked",
        "budget_exhausted",
        "unconfigured",
        "unavailable",
        "timeout",
        "auth_failed",
        "refused",
        "truncated",
        "malformed",
        "insufficient",
        "retention_unconfirmed",
        "local_unverified",
    }
    assert len(vocab.OUTCOMES) == 13
    # The local route's verification outcome (own answer model plan, Task 11) is never a
    # cloud call's: the cloud retry and adapters keep to CLOUD_OUTCOMES.
    assert set(vocab.CLOUD_OUTCOMES) == set(vocab.OUTCOMES) - {"local_unverified"}
    assert vocab.LOCAL_OUTCOMES == ("local_unverified",)


def test_base_urls_and_hosts_are_fixed() -> None:
    assert dict(vocab.BASE_URLS) == {
        "anthropic": "https://api.anthropic.com",
        "gemini": "https://generativelanguage.googleapis.com/v1beta/openai",
        "openai": "https://api.openai.com/v1",
        "xai": "https://api.x.ai/v1",
    }
    assert dict(vocab.HOSTS) == {
        "anthropic": "api.anthropic.com",
        "gemini": "generativelanguage.googleapis.com",
        "openai": "api.openai.com",
        "xai": "api.x.ai",
    }
    with pytest.raises(TypeError):
        vocab.BASE_URLS["openai"] = "https://example.invalid"  # type: ignore[index]
    with pytest.raises(TypeError):
        vocab.HOSTS["openai"] = "example.invalid"  # type: ignore[index]
