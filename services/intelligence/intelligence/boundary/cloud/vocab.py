"""The closed vocabularies of the cloud route (Global Constraints).

Core (Go) holds the same lists in internal/config/config.go, and its
TestOutcomeVocabularyMatchesWorker reads this file as text to compare
PROVIDERS, EGRESS and OUTCOMES. That test reads literal strings only, so
each list is spelled out here: no unpacking of another list. Sorted.
"""

from __future__ import annotations

from collections.abc import Mapping
from types import MappingProxyType
from typing import Final
from urllib.parse import urlsplit

PROVIDERS: Final[tuple[str, ...]] = ("anthropic", "gemini", "openai", "xai")

# A query's model_route, set by core's trigger (migration 00018).
ROUTES: Final[tuple[str, ...]] = (
    "cloud:anthropic",
    "cloud:gemini",
    "cloud:openai",
    "cloud:xai",
    "local",
)

# query.completed's egress: where an answer's text went.
EGRESS: Final[tuple[str, ...]] = ("anthropic", "gemini", "none", "openai", "xai")

# query.completed's cloud_outcome (design section 11).
OUTCOMES: Final[tuple[str, ...]] = (
    "auth_failed",
    "budget_exhausted",
    "egress_blocked",
    "insufficient",
    "malformed",
    "ok",
    "refused",
    "retention_unconfirmed",
    "timeout",
    "truncated",
    "unavailable",
    "unconfigured",
)

# Fixed network targets. An override (INTELLIGENCE_CLOUD_<P>_BASE_URL) is
# accepted in development only.
BASE_URLS: Final[Mapping[str, str]] = MappingProxyType(
    {
        "anthropic": "https://api.anthropic.com",
        # Google's OpenAI-compatible endpoint (gemini_adapter).
        "gemini": "https://generativelanguage.googleapis.com/v1beta/openai",
        "openai": "https://api.openai.com/v1",
        "xai": "https://api.x.ai/v1",
    }
)

# The host of each base URL: what the egress proxy allows.
HOSTS: Final[Mapping[str, str]] = MappingProxyType(
    {p: urlsplit(u).hostname or "" for p, u in BASE_URLS.items()}
)
