"""Issue #7: PIIMasker must replace each PHI span exactly once."""

import json
import re

import pytest

from intelligence.security.hipaa import PIIMasker

TOKEN = re.compile(r"\[[A-Z0-9_+]+\]")
KNOWN_TOKENS = set(PIIMasker.REPLACEMENT_MAP.values())


def _assert_clean_tokens(masked: str) -> None:
    """Every bracketed token is a whole, known replacement; no fragments."""
    for token in TOKEN.findall(masked):
        assert token in KNOWN_TOKENS, (token, masked)
    leftover = TOKEN.sub("", masked)
    assert "[" not in leftover and "]" not in leftover, masked


@pytest.mark.parametrize(
    "text, expected",
    [
        ("Call 555-123-4567 today", "Call [PHONE_REDACTED] today"),
        ("Call +1 555-123-4567", "Call [PHONE_REDACTED]"),
        ("DOB: 03/15/1956", "DOB: [DOB_REDACTED]"),
        ("(555) 123-4567 or 555.123.4567", "[PHONE_REDACTED] or [PHONE_REDACTED]"),
        ("SSN 123-45-6789", "SSN [SSN_REDACTED]"),
        ("Patient: John Smith seen today", "Patient: [NAME_REDACTED] seen today"),
        ("Dr. Ada Obi, MRN: 1234567", "Dr. [NAME_REDACTED], MRN: [MRN_REDACTED]"),
    ],
)
def test_masks_each_span_once(text, expected):
    masked, _ = PIIMasker().mask(text)

    assert masked == expected
    _assert_clean_tokens(masked)


@pytest.mark.parametrize(
    "text",
    [
        "Patient: John Smith, DOB: 03/15/1956, SSN: 123-45-6789. MRN: 00123456. "
        "Phone: (555) 123-4567. Address: 123 Main Street, Chicago IL 60601.",
        "Reach me on +1 555-123-4567 or 555.123.4567, born on 3/5/1956.",
        "Email jane.doe@example.com, call 555 123 4567, MR# 99887766.",
    ],
)
def test_no_identifier_digits_survive(text):
    masked, _ = PIIMasker().mask(text)

    _assert_clean_tokens(masked)
    for run in re.findall(r"\d{3,}", text):
        assert run not in masked, (run, masked)
    assert "jane.doe" not in masked


def test_counts_each_masked_span_once():
    masker = PIIMasker()

    _, detections = masker.mask("Call 555-123-4567 or DOB: 03/15/1956")

    assert sorted(d.entity_type for d in detections) == ["DOB", "PHONE"]
    assert masker.get_stats() == {"DOB": 1, "PHONE": 1}


def test_detections_match_their_offsets():
    text = "Patient: John Smith, call 555-123-4567"

    _, detections = PIIMasker().mask(text)

    for d in detections:
        assert text[d.start : d.end] == d.original_text


def test_masked_json_stays_valid():
    # The worker masks a JSON-encoded patient context and parses it back.
    ctx = json.dumps({"phone": "555-123-4567", "dob": "DOB: 03/15/1956", "note": "stable"})

    masked, _ = PIIMasker().mask(ctx)

    assert json.loads(masked)["note"] == "stable"
