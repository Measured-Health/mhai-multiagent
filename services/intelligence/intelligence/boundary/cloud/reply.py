"""Reply validation (design section 9; plan Task 8).

A provider's reply is hostile input. The question is masked and the chunks
are curated, but neither the chunk text nor its <<chunk>>/<<end>>
delimiters were checked by the gate (Task 5 review M5), so a question or a
chunk can try to make the model cite a chunk it was never sent, add a link,
or fake a chunk block. Whatever the provider's strict mode claims, the
reply is parsed again here and every check fails closed:

- strict JSON (duplicate keys, NaN and lone surrogates refused), at most
  MAX_REPLY_BYTES, exactly the three keys of ANSWER_SCHEMA, each of its
  JSON type (no coercion: "false" and 0 are not false);
- answer: 1 to 6,000 characters, no control or format characters, no URL
  (protocol-relative "//host" and backslash forms included, and data:,
  mailto:, file: or tel: with no space after the colon), HTML tag,
  "[" or "]" outside an exact [KB#nnnn] marker (so no Markdown link,
  reference, definition or image), a marker followed by ":", code fence,
  or chunk delimiter;
- citations: each KB#nnnn (ASCII digits) and one of the chunk ids sent;
- the [KB#nnnn] markers in answer are exactly the citations, and no other
  "KB#" text appears (a lookalike marker would show as a citation that was
  never checked);
- at least one citation, unless insufficient_evidence.

A refusal is Malformed, whose text is a rule code only (RULES), never the
reply, a match or an offset, and it carries no chained exception (json's
and pydantic's errors hold the input). Nothing here logs.

This is a denylist and can't be complete: the clinical app must keep
rendering the answer as plain text (mhai-web's parseBody does today, with
no Markdown renderer), or with links and autolinks off.
"""

from __future__ import annotations

import re
import unicodedata
from collections.abc import Mapping
from types import MappingProxyType
from typing import Any, Final, NoReturn

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from intelligence.boundary import _strictjson
from intelligence.boundary.cloud.scan import norm_text

MAX_ANSWER_CHARS: Final = 6000
# 6,000 characters JSON-escaped as \uXXXX is 36,000 bytes; the rest is room
# for the citations. Anything bigger is refused before it is parsed.
MAX_REPLY_BYTES: Final = 65_536

KEYS: Final = frozenset({"answer", "citations", "insufficient_evidence"})

# Every code Malformed can give, in the order the checks run.
RULES: Final = (
    "too_large",
    "not_json",
    "keys",
    "types",
    "empty",
    "too_long",
    "control",
    "citation_format",
    "citation_unknown",
    "markers",
    "no_citation",
    "delimiter",
    "link",
    "html",
    "markdown",
    "code_fence",
)

# Design section 9: no pattern and no maxLength (xAI enforces lengths only
# up to 2,048 structurally; the 6,000 limit is ours). Read-only; adapters
# embed answer_schema(), a fresh plain copy.
ANSWER_SCHEMA: Final[Mapping[str, Any]] = MappingProxyType(
    {
        "type": "object",
        "properties": MappingProxyType(
            {
                "answer": MappingProxyType({"type": "string"}),
                "citations": MappingProxyType(
                    {"type": "array", "items": MappingProxyType({"type": "string"})}
                ),
                "insufficient_evidence": MappingProxyType({"type": "boolean"}),
            }
        ),
        "required": ("answer", "citations", "insufficient_evidence"),
        "additionalProperties": False,
    }
)


def _thaw(v: Any) -> Any:
    if isinstance(v, Mapping):
        return {k: _thaw(x) for k, x in v.items()}
    if isinstance(v, tuple):
        return [_thaw(x) for x in v]
    return v


def answer_schema() -> dict[str, Any]:
    """ANSWER_SCHEMA as a new plain dict (JSON-serialisable)."""
    out: dict[str, Any] = _thaw(ANSWER_SCHEMA)
    return out


class Malformed(Exception):
    """The reply failed a check. str() is the rule code only."""

    def __init__(self, rule: str) -> None:
        super().__init__(rule)
        self.rule = rule

    def __str__(self) -> str:
        return self.rule


def _bad(rule: str) -> NoReturn:
    # `from None` everywhere: a raise inside an except block would
    # otherwise chain the parser's error, which holds the input.
    raise Malformed(rule) from None


class Answer(BaseModel):
    """A checked reply. Its repr and str hold no answer text."""

    model_config = ConfigDict(extra="forbid", frozen=True, strict=True)

    answer: str = Field(repr=False)
    citations: tuple[str, ...]
    insufficient_evidence: bool


_CITATION = re.compile(r"KB#[0-9]{4}")
_MARKER = re.compile(r"\[(KB#[0-9]{4})\]")
_DELIMITERS = ("<<chunk", "<<end")
# A scheme URL, www., a script scheme, a protocol-relative "//host", or a
# backslash form of one (a backslash next to a slash, or two backslashes).
_LINK = re.compile(
    r"[a-z][a-z0-9+.\-]*://|www\.|javascript:|vbscript:|//[^\s/]"
    # Backslash-slash forms browsers and renderers read as a slash.
    r"|\\/|/\\|\\\\"
)
# "<" then a letter, "/", "!" or "?": a tag, a closing tag, a comment or a
# processing instruction. "BP <90" and "a < b" are not.
# The data:, mailto:, file: and tel: schemes (none needs "//"), only when
# a non-space follows the colon straight away: "data:text/html" and
# "tel:+234" are refused, "lab data: Hb 9" and "Tel: ask the ward" are
# prose. Checked on the normalised text only (case-folded, NFKC, so
# full-width forms are caught), never the space-stripped copy, which
# would join "tel: ask" into "tel:ask".
_SCHEME = re.compile(r"\b(?:data|mailto|file|tel):\S")
_HTML = re.compile(r"<[a-z/!?]")
_MARKDOWN = re.compile(r"!\[|\]\(")
# A marker followed by a colon: "[KB#0001]: x" is a link definition that
# would turn the marker into a link.
_MARKER_DEFINITION = re.compile(r"\]\s*:")
_BRACKETS = frozenset("[]")
_FENCES = ("```", "~~~")


def _control(s: str) -> bool:
    for c in s:
        cat = unicodedata.category(c)
        if cat == "Cf" or (cat == "Cc" and c not in "\n\r\t"):
            return True
    return False


def _parse(raw: bytes | str | Mapping[str, Any]) -> Mapping[str, Any]:
    if isinstance(raw, str):
        try:
            raw = raw.encode("utf-8")
        except UnicodeEncodeError:
            _bad("not_json")
    if isinstance(raw, bytes | bytearray):
        if len(raw) > MAX_REPLY_BYTES:
            _bad("too_large")
        try:
            doc = _strictjson.loads(bytes(raw))
        except _strictjson.StrictJSONError:
            _bad("not_json")
    elif isinstance(raw, Mapping):
        doc = raw
    else:
        _bad("not_json")
    if not isinstance(doc, Mapping):
        _bad("keys")
    if not all(type(k) is str for k in doc) or set(doc) != KEYS:
        _bad("keys")
    return doc


def validate_answer(raw: bytes | str | Mapping[str, Any], sent_ids: frozenset[str]) -> Answer:
    """Check a reply against the design's rules and the chunk ids that were
    sent. Raises Malformed; never returns an unchecked answer."""
    doc = _parse(raw)
    text, cites, insufficient = doc["answer"], doc["citations"], doc["insufficient_evidence"]
    if type(text) is not str or type(insufficient) is not bool or type(cites) is not list:
        _bad("types")
    if not all(type(c) is str for c in cites):
        _bad("types")

    if not text.strip():
        _bad("empty")
    if len(text) > MAX_ANSWER_CHARS:
        _bad("too_long")
    if _control(text):
        _bad("control")

    citations: list[str] = []
    for c in cites:
        if not _CITATION.fullmatch(c):
            _bad("citation_format")
        if c not in sent_ids:
            _bad("citation_unknown")
        if c not in citations:
            citations.append(c)

    if set(_MARKER.findall(text)) != set(citations):
        _bad("markers")
    # Any other "KB#" (a lookalike, a longer number, an unbracketed id).
    residue = "".join(norm_text(_MARKER.sub(" ", text)).split())
    if "kb#" in residue:
        _bad("markers")
    if not citations and not insufficient:
        _bad("no_citation")

    norm = norm_text(text)
    compact = "".join(norm.split())
    if any(d in compact for d in _DELIMITERS):
        _bad("delimiter")
    if _LINK.search(norm) or _LINK.search(compact) or _SCHEME.search(norm):
        _bad("link")
    if _HTML.search(norm):
        _bad("html")
    # Any "[" or "]" outside an exact [KB#nnnn] marker, on the whole
    # normalised text: no reference link, shortcut reference or link
    # definition can be written without one, however it is quoted, listed
    # or split over lines.
    if _BRACKETS & set(norm_text(_MARKER.sub(" ", text))):
        _bad("markdown")
    if _MARKDOWN.search(norm) or _MARKER_DEFINITION.search(norm):
        _bad("markdown")
    if any(f in compact for f in _FENCES):
        _bad("code_fence")

    try:
        return Answer(answer=text, citations=tuple(citations), insufficient_evidence=insufficient)
    except ValidationError:
        _bad("types")
