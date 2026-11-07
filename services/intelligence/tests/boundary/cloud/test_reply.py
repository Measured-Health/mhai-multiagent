"""Reply validation (design section 9; plan Task 8; A7).

Model output is hostile: the chunk text and its delimiters were not
checked by the gate (Task 5 review M5), so a question or a chunk can try
to make the model cite a chunk it was never sent, add a link, or fake a
chunk block. Every check here fails closed with Malformed, whose text is a
rule code only. Every value is synthetic.
"""

from __future__ import annotations

import json
import traceback
from typing import Any

import pytest

from intelligence.boundary.cloud import reply
from intelligence.boundary.cloud.reply import ANSWER_SCHEMA, Answer, Malformed, validate_answer

pytestmark = pytest.mark.disable_socket

SENT = frozenset({"KB#0001", "KB#0002", "KB#0003"})
MARKER = "SYNTHETICMARKERTEXT"


def _doc(answer: str = f"Give fluids [KB#0001]. {MARKER}", **kw: Any) -> dict[str, Any]:
    doc: dict[str, Any] = {
        "answer": answer,
        "citations": ["KB#0001"],
        "insufficient_evidence": False,
    }
    doc.update(kw)
    return doc


def _raw(**kw: Any) -> bytes:
    return json.dumps(_doc(**kw)).encode()


BAD: list[tuple[str, Any]] = [
    # A7: a citation the model was never sent.
    ("unknown_citation", _raw(answer="See [KB#9999].", citations=["KB#9999"])),
    # A7: markers and citations disagree, both ways.
    ("marker_not_cited", _raw(answer="A [KB#0001] B [KB#0002].", citations=["KB#0001"])),
    ("cited_no_marker", _raw(answer="A [KB#0001].", citations=["KB#0001", "KB#0002"])),
    # A7: a link.
    ("link_https", _raw(answer="Read https://example.invalid/x [KB#0001].")),
    ("link_http", _raw(answer="Read http://example.invalid [KB#0001].")),
    ("link_www", _raw(answer="Read www.example.invalid [KB#0001].")),
    ("link_scheme", _raw(answer="Open ftp://example.invalid [KB#0001].")),
    ("link_fullwidth", _raw(answer="Read \uff48\uff54\uff54\uff50\uff53://x [KB#0001].")),
    ("javascript", _raw(answer="javascript:alert(1) [KB#0001].")),
    ("markdown_link", _raw(answer="See [guide](x) [KB#0001].")),
    ("markdown_image", _raw(answer="![x](y) [KB#0001].")),
    ("code_fence", _raw(answer="```\ncode\n``` [KB#0001].")),
    ("tilde_fence", _raw(answer="~~~\ncode\n~~~ [KB#0001].")),
    ("html_tag", _raw(answer="<b>bold</b> [KB#0001].")),
    ("html_script", _raw(answer="<script>x</script> [KB#0001].")),
    ("html_comment", _raw(answer="<!-- x --> [KB#0001].")),
    ("html_self_closing", _raw(answer="a<br/>b [KB#0001].")),
    # The system prompt's chunk delimiters (a faked chunk block).
    ("delimiter_chunk", _raw(answer="<<chunk>> x [KB#0001].")),
    ("fake_chunk_block", _raw(answer="<<chunk KB#0002>> x <<end>> [KB#0001].")),
    ("delimiter_end", _raw(answer="x <<end>> [KB#0001].")),
    ("delimiter_spaced", _raw(answer="x << END >> [KB#0001].")),
    # A citation-like id that isn't a well-formed marker.
    ("stray_kb_id", _raw(answer="KB#0002 says so [KB#0001].")),
    ("long_marker", _raw(answer="x [KB#00011] [KB#0001].")),
    ("fullwidth_marker", _raw(answer="x \uff3bKB#0002\uff3d [KB#0001].")),
    ("arabic_digits", _raw(answer="x [KB#\u0660\u0660\u0660\u0661].", citations=["KB#0001"])),
    ("bad_citation_format", _raw(answer="x [KB#0001].", citations=["kb#0001"])),
    ("citation_not_str", _raw(citations=[1])),
    # Bidi overrides and controls (text that displays other than it reads).
    ("bidi_override", _raw(answer="x \u202e [KB#0001].")),
    ("control_char", _raw(answer="x \x07 [KB#0001].")),
    # Keys and types.
    ("extra_key", _raw(note="x")),
    ("missing_key", json.dumps({"answer": "x [KB#0001].", "citations": ["KB#0001"]}).encode()),
    (
        "duplicate_key",
        b'{"answer":"a [KB#0001].","answer":"b","citations":["KB#0001"],'
        b'"insufficient_evidence":false}',
    ),
    ("bool_as_string", _raw(insufficient_evidence="false")),
    ("bool_as_int", _raw(insufficient_evidence=0)),
    ("citations_as_string", _raw(citations="KB#0001")),
    ("answer_not_str", _raw(answer=["x"])),
    ("null_answer", _raw(answer=None)),
    ("not_object", b'["x"]'),
    ("invalid_json", b'{"answer": '),
    ("trailing_data", _raw() + b" {}"),
    ("nan", b'{"answer":"x [KB#0001].","citations":["KB#0001"],"insufficient_evidence":NaN}'),
    (
        "lone_surrogate",
        b'{"answer":"x \\ud800 [KB#0001].","citations":["KB#0001"],"insufficient_evidence":false}',
    ),
    ("not_utf8", b'{"answer":"\xff"}'),
    ("too_big", b" " * (reply.MAX_REPLY_BYTES + 1) + _raw()),
    # Lengths.
    ("empty", _raw(answer="", citations=[], insufficient_evidence=True)),
    ("blank", _raw(answer=" \n ", citations=[], insufficient_evidence=True)),
    ("over_6000", _raw(answer="[KB#0001]" + "a" * 5992)),
    # No citation without insufficient_evidence.
    ("no_citation", _raw(answer="Give fluids.", citations=[])),
]


# The rule each case must fail on: not merely some earlier check.
EXPECT = {
    "unknown_citation": "citation_unknown",
    "marker_not_cited": "markers",
    "cited_no_marker": "markers",
    "link_https": "link",
    "link_http": "link",
    "link_www": "link",
    "link_scheme": "link",
    "link_fullwidth": "link",
    "javascript": "link",
    "markdown_link": "markdown",
    "markdown_image": "markdown",
    "code_fence": "code_fence",
    "tilde_fence": "code_fence",
    "html_tag": "html",
    "html_script": "html",
    "html_comment": "html",
    "html_self_closing": "html",
    "delimiter_chunk": "delimiter",
    "fake_chunk_block": "markers",  # its KB# id is caught first
    "delimiter_end": "delimiter",
    "delimiter_spaced": "delimiter",
    "stray_kb_id": "markers",
    "long_marker": "markers",
    "fullwidth_marker": "markers",
    "arabic_digits": "markers",
    "bad_citation_format": "citation_format",
    "citation_not_str": "types",
    "bidi_override": "control",
    "control_char": "control",
    "extra_key": "keys",
    "missing_key": "keys",
    "duplicate_key": "not_json",
    "bool_as_string": "types",
    "bool_as_int": "types",
    "citations_as_string": "types",
    "answer_not_str": "types",
    "null_answer": "types",
    "not_object": "keys",
    "invalid_json": "not_json",
    "trailing_data": "not_json",
    "nan": "not_json",
    "lone_surrogate": "not_json",
    "not_utf8": "not_json",
    "too_big": "too_large",
    "empty": "empty",
    "blank": "empty",
    "over_6000": "too_long",
    "no_citation": "no_citation",
}


def test_every_case_has_a_rule() -> None:
    assert set(EXPECT) == {c for c, _ in BAD}
    assert set(EXPECT.values()) <= set(reply.RULES)


@pytest.mark.parametrize(("case", "raw"), BAD, ids=[c for c, _ in BAD])
def test_answer_checks(case: str, raw: Any) -> None:
    with pytest.raises(Malformed) as info:
        validate_answer(raw, SENT)
    assert info.value.rule == EXPECT[case]


def test_answer_checks_on_str_and_mapping_inputs() -> None:
    with pytest.raises(Malformed):
        validate_answer(_raw(note="x").decode(), SENT)
    with pytest.raises(Malformed):
        validate_answer(_doc(note="x"), SENT)
    with pytest.raises(Malformed):
        validate_answer(_doc(citations=("KB#0001",)), SENT)  # a JSON array only
    with pytest.raises(Malformed):
        validate_answer("x \ud800", SENT)  # a str that isn't UTF-8 encodable
    with pytest.raises(Malformed):
        validate_answer(12, SENT)  # type: ignore[arg-type]


def test_citation_outside_sent_ids_even_if_well_formed() -> None:
    with pytest.raises(Malformed):
        validate_answer(_raw(), frozenset({"KB#0002"}))
    with pytest.raises(Malformed):
        validate_answer(_raw(), frozenset())


def test_good_answer_passes() -> None:
    a = validate_answer(_raw(), SENT)
    assert isinstance(a, Answer)
    assert a.answer == f"Give fluids [KB#0001]. {MARKER}"
    assert a.citations == ("KB#0001",)
    assert a.insufficient_evidence is False

    two = validate_answer(
        _raw(answer="A [KB#0002]; B [KB#0001] and [KB#0002].", citations=["KB#0002", "KB#0001"]),
        SENT,
    )
    assert two.citations == ("KB#0002", "KB#0001")

    # Exactly 6,000 characters, ordinary clinical punctuation and comparisons.
    text = "BP < 90 and HR > 100; temp 38.5 C (fever) [KB#0003]. "
    long = (text * 200)[:6000]
    assert len(long) == 6000
    assert validate_answer(_raw(answer=long, citations=["KB#0003"]), SENT).answer == long

    # insufficient_evidence: true is valid, with or without citations.
    ins = validate_answer(
        _raw(answer="The chunks don't say.", citations=[], insufficient_evidence=True), SENT
    )
    assert ins.insufficient_evidence is True and ins.citations == ()

    # str and Mapping inputs give the same Answer.
    assert validate_answer(_raw().decode(), SENT) == a
    assert validate_answer(_doc(), SENT) == a


def test_duplicate_citations_kept_once_in_order() -> None:
    a = validate_answer(
        _raw(answer="A [KB#0002] B [KB#0001].", citations=["KB#0002", "KB#0001", "KB#0002"]),
        SENT,
    )
    assert a.citations == ("KB#0002", "KB#0001")


def test_answer_is_frozen_and_strict() -> None:
    a = validate_answer(_raw(), SENT)
    with pytest.raises(Exception):  # noqa: B017, PT011  (pydantic's frozen error)
        a.answer = "x"  # type: ignore[misc]
    with pytest.raises(Exception):  # noqa: B017, PT011
        Answer(answer="x", citations=("KB#0001",), insufficient_evidence=False, extra=1)  # type: ignore[call-arg]


def test_answer_schema_is_the_design_schema() -> None:
    schema = reply.answer_schema()
    assert schema == {
        "type": "object",
        "properties": {
            "answer": {"type": "string"},
            "citations": {"type": "array", "items": {"type": "string"}},
            "insufficient_evidence": {"type": "boolean"},
        },
        "required": ["answer", "citations", "insufficient_evidence"],
        "additionalProperties": False,
    }
    # No pattern and no maxLength anywhere (section 9: xAI's limits).
    flat = json.dumps(schema)
    assert "pattern" not in flat and "maxLength" not in flat
    # answer_schema() is a fresh plain copy each time; ANSWER_SCHEMA can't change.
    schema["required"].append("x")
    assert reply.answer_schema()["required"] == ["answer", "citations", "insufficient_evidence"]
    with pytest.raises(TypeError):
        ANSWER_SCHEMA["type"] = "array"  # type: ignore[index]
    assert json.loads(json.dumps(reply.answer_schema())) == reply.answer_schema()


def test_malformed_error_has_no_text() -> None:
    cases = [
        _raw(answer=f"{MARKER} https://x [KB#0001]."),
        _raw(answer=f"{MARKER} [KB#9999].", citations=["KB#9999"]),
        _raw(answer=MARKER, note=MARKER),
        _raw(answer=MARKER, insufficient_evidence=MARKER),
        f'{{"answer":"{MARKER}'.encode(),
        _raw(answer=MARKER * 400),
    ]
    for raw in cases:
        with pytest.raises(Malformed) as info:
            validate_answer(raw, SENT)
        e = info.value
        assert e.rule in reply.RULES
        assert str(e) == e.rule
        assert e.args == (e.rule,)
        assert MARKER not in repr(e)
        # No chained exception carries the input (pydantic's and json's do).
        assert e.__cause__ is None
        assert e.__suppress_context__ is True
        rendered = "".join(traceback.format_exception(e))
        assert MARKER not in rendered


def test_answer_repr_has_no_text() -> None:
    a = validate_answer(_raw(), SENT)
    assert MARKER not in repr(a)
    assert MARKER not in str(a)
