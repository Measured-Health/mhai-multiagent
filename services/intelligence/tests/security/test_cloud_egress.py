"""Regressions for the cloud egress scan (plan Task 6: A2, A4, split names).

Each case goes through ScanHook on an httpx2.Request, as the SDK's request
hook sees it, and must be refused before the transport. Every name and
number here is synthetic.
"""

from __future__ import annotations

import httpx2
import pytest

from intelligence.boundary.cloud.gate import EgressRefused
from intelligence.boundary.cloud.scan import ScanHook, collect_identifiers
from tests.boundary.cloud.layouts import PROVIDERS, blocks, body, sdk_body, sdk_shape, shape

pytestmark = pytest.mark.disable_socket

URL = "https://api.anthropic.com/v1/messages"
RECORD = {"name": "Adaeze Okafor", "mrn": "4471982"}


def _refused(provider: str, raw: bytes, question: str = "", sdk: bool = False) -> str:
    shp = sdk_shape(provider) if sdk else shape(provider)
    hook = ScanHook(collect_identifiers(question, RECORD), shp)
    with pytest.raises(EgressRefused) as e:
        hook(httpx2.Request("POST", URL, content=raw))
    assert hook.verdict == str(e.value)
    assert hook.sent is False
    return str(e.value)


@pytest.mark.parametrize("provider", PROVIDERS)
def test_a2_name_without_title(provider: str) -> None:
    q = "Adaeze Okafor has chest pain"
    assert _refused(provider, body(provider, blocks(q)), q) == "known_identifier"


@pytest.mark.parametrize("provider", PROVIDERS)
@pytest.mark.parametrize(
    "name",
    ["Adaeze\nOkafor", "Adaeze\tOkafor", "Ada\u200beze Okafor", "ADAEZE  OKAFOR", "Adaeze-Okafor"],
)
def test_split_names_through_each_sdk(provider: str, name: str) -> None:
    raw = sdk_body(provider, blocks(f"{name} has chest pain"))
    assert _refused(provider, raw, sdk=True) == "known_identifier"


@pytest.mark.parametrize("provider", PROVIDERS)
@pytest.mark.parametrize(
    ("question", "rule"),
    [
        ("call 0803 123 4567", "ng_phone"),
        ("call +234 803 123 4567", "ng_phone"),
        ("call 0803-123-4567", "ng_phone"),
        ("NIN 12345678901", "nin"),
    ],
)
def test_a4_phone_and_nin_without_record(provider: str, question: str, rule: str) -> None:
    hook = ScanHook(collect_identifiers("", {}), shape(provider))
    with pytest.raises(EgressRefused) as e:
        hook(httpx2.Request("POST", URL, content=body(provider, blocks(question))))
    assert str(e.value) == rule


def test_clean_request_passes_the_hook() -> None:
    hook = ScanHook(collect_identifiers("", RECORD), shape("openai"))
    hook(httpx2.Request("POST", URL, content=body("openai", blocks("fever, T 38.5"))))
    assert hook.verdict is None and hook.sent is True
