"""The egress scan: the exact request bytes, checked before they are sent
(design section 4.4; plan Task 6).

The masker can miss things, so every request body is scanned as the SDK
hands it to the transport (ScanHook is an httpx2 request hook). The scan
fails closed: any doubt is a refusal, and a refusal is a rule code only,
never the text, a match or an offset. Nothing here logs.

How it reads a body:

- The body is parsed as strict JSON (at most 49,152 bytes, duplicate keys
  refused) and every string value is decoded, so a name hidden by JSON
  escaping ("Adaeze\\nOkafor", a \\u200b inside it) is seen as text.
- Shape: every leaf must sit at one of the adapter's exact JSON pointers
  (no wildcards), and both patient-derived blocks must be present as text.
  So a field the adapter didn't put there is refused, and the patterns
  always know which strings came from the patient.
- known_identifier: this patient's identifiers (collect_identifiers), in
  every string of the body, chunks included, and once more over the raw
  text. Names match as whole words after norm_words; numbers of 5 or more
  digits match only at digit-group boundaries, so they can only hit this
  patient's own number.
- The patterns (Nigerian phone numbers, NIN, email, dates, long digit runs,
  MRN prefixes) run over the patient-derived blocks only: block 0 (the
  masked question) and block 1 (the context), at fixed positions in every
  provider's layout (CloudPrompt.blocks(), ruling R-1). Shared chunks are
  curated guideline text: a dose such as "200000 IU" must not escalate
  every question that retrieves it.
"""

from __future__ import annotations

import math
import re
import unicodedata
from collections.abc import Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from types import MappingProxyType
from typing import TYPE_CHECKING, Any, Final, final

from intelligence.boundary import _strictjson
from intelligence.boundary.cloud.gate import MAX_CHUNKS, EgressRefused

if TYPE_CHECKING:
    import httpx2

MAX_BODY_BYTES: Final = 49_152
# run_query.MAX_DEPTH (a test pins it): deeper patient data is never sent.
MAX_DEPTH: Final = 16
MIN_NAME_LETTERS: Final = 3
MIN_NUMBER_DIGITS: Final = 5

# Every code the scan can give, in the order the rules run.
RULES: Final = (
    "trace_header",
    "too_large",
    "not_json",
    "shape",
    "known_identifier",
    "ng_phone",
    "nin",
    "email",
    "date",
    "long_digits",
    "mrn_prefix",
    "scan_error",
)

# Trace context never leaves with a request (Task 4 fix round).
TRACE_HEADERS: Final = ("traceparent", "tracestate", "baggage")

# Compared case-folded. Values under these keys, at any depth, are this
# patient's identifiers (plan deviation 9).
IDENTIFYING_KEYS: Final = frozenset(
    {
        "name",
        "given",
        "family",
        "mrn",
        "phone",
        "email",
        "address",
        "dob",
        "nin",
        "bvn",
        "nhis",
        "identifier",
        "telecom",
        "birthdate",
        "birth_date",
    }
)
NAME_KEYS: Final = frozenset({"name", "given", "family"})

# Where each provider's adapter puts the user content blocks (an array of
# text parts): block i's text is at <array>/<i>/text.
BLOCK_ARRAYS: Final = MappingProxyType(
    {
        "anthropic": "/messages/0/content",
        "openai": "/input/0/content",
        "xai": "/messages/1/content",  # /messages/0 is the system message
    }
)
MAX_BLOCKS: Final = 2 + MAX_CHUNKS  # the question, the context, the chunks


# --- normalisation ----------------------------------------------------------------------

# Default-ignorable code points that are not Cf: invisible, so removed with Cf.
_IGNORABLE_RANGES: Final = (
    (0x034F, 0x034F),
    (0x115F, 0x1160),
    (0x17B4, 0x17B5),
    (0x180B, 0x180F),
    (0x3164, 0x3164),
    (0xFE00, 0xFE0F),
    (0xFFA0, 0xFFA0),
    (0xE0100, 0xE01EF),
)


def _invisible(c: str) -> bool:
    cat = unicodedata.category(c)
    if cat == "Cf":
        return True
    if cat == "Cc":
        return not c.isspace()  # NUL and other controls; whitespace separates
    o = ord(c)
    return any(lo <= o <= hi for lo, hi in _IGNORABLE_RANGES)


def _clean(s: str) -> str:
    """NFKC, invisible characters removed, every decimal digit as ASCII,
    case-folded."""
    t = unicodedata.normalize("NFKC", s)
    out = []
    for c in t:
        if _invisible(c):
            continue
        if unicodedata.category(c) == "Nd":
            out.append(str(unicodedata.digit(c)))
        else:
            out.append(c)
    return "".join(out).casefold()


def norm_words(s: str) -> str:
    """For the identifier rule: NFKC, Cf removed, case-folded, accents
    folded, and every run of characters that aren't letters or digits one
    space. "Adaeze\\nOkafor", "ADAEZE\\u200b OKAFOR" and "adaeze-okafor" all
    give "adaeze okafor"."""
    t = unicodedata.normalize("NFKD", _clean(s))
    t = "".join(c for c in t if unicodedata.category(c) != "Mn")
    t = "".join(c if unicodedata.category(c)[0] in "LNM" else " " for c in t)
    return " ".join(t.split())


def norm_text(s: str) -> str:
    """For the pattern rules: NFKC, Cf removed, case-folded, every run of
    Unicode spaces one ASCII space. Punctuation is kept."""
    t = _clean(s)
    t = "".join(" " if c.isspace() or unicodedata.category(c)[0] == "Z" else c for c in t)
    return " ".join(t.split())


_DIGITS = re.compile(r"[0-9]+")
_HYPHEN_RUN = re.compile(r"[0-9]+(?:-[0-9]+)*")


def _digit_runs(t: str) -> list[str]:
    return [m.group().replace("-", "") for m in _HYPHEN_RUN.finditer(t)]


def digit_runs(s: str) -> list[str]:
    """Digit runs of norm_text(s), joined across a single "-" only: a space,
    "." or "/" ends a run, so "T 38.5 101.3" gives 38, 5, 101, 3."""
    return _digit_runs(norm_text(s))


def _fits_id(run: str) -> bool:
    return len(run) == 11 or (len(run) == 13 and _is_phone(run))


def _is_phone(run: str) -> bool:
    if len(run) == 11:
        return run[0] == "0" and run[1] in "789" and run[2] in "01"
    if len(run) == 13:
        return run.startswith("234") and run[3] in "789" and run[4] in "01"
    return False


def _grouped_runs(t: str) -> list[str]:
    chains: list[list[str]] = []
    prev_end = -2
    for m in _DIGITS.finditer(t):
        g = m.group()
        if not 2 <= len(g) <= 4:
            prev_end = -2
            continue
        joined = chains and m.start() - prev_end == 1 and prev_end >= 0 and t[prev_end] in " -."
        if joined:
            chains[-1].append(g)
        else:
            chains.append([g])
        prev_end = m.end()
    out: list[str] = []
    for chain in chains:
        for i in range(len(chain)):
            run = chain[i]
            for j in range(i + 1, len(chain)):
                run += chain[j]
                if len(run) > 13:
                    break
                if _fits_id(run):
                    out.append(run)
    return out


def grouped_runs(s: str) -> list[str]:
    """Groups of 2 to 4 digits in norm_text(s) joined across a single space,
    "-" or ".", kept only where the joined run fits a phone number (11
    digits from 0[789][01], or 13 from 234[789][01]) or a NIN (11 digits):
    "0803 123 4567" and "080.3123.4567" are caught, vitals are not."""
    return _grouped_runs(norm_text(s))


def _chains(t: str) -> list[tuple[str, frozenset[int]]]:
    """For the identifier rule: digit groups of norm_text joined across a
    single space, "-", "." or "/", with the offsets where groups meet."""
    out: list[tuple[str, frozenset[int]]] = []
    digits = ""
    bounds: set[int] = set()
    prev_end = -2
    for m in _DIGITS.finditer(t):
        if digits and m.start() - prev_end == 1 and t[prev_end] in " -./":
            digits += m.group()
        else:
            if digits:
                out.append((digits, frozenset(bounds)))
            digits, bounds = m.group(), {0}
        bounds.add(len(digits))
        prev_end = m.end()
    if digits:
        out.append((digits, frozenset(bounds)))
    return out


# --- this patient's identifiers ----------------------------------------------------------


def _placeholders() -> frozenset[str]:
    from intelligence.security.hipaa import PIIMasker

    return frozenset(PIIMasker.REPLACEMENT_MAP.values())


_PLACEHOLDERS: Final = _placeholders()
_PLACEHOLDER_WORDS: Final = frozenset(norm_words(p) for p in _PLACEHOLDERS)
_PLACEHOLDER_RE: Final = re.compile(
    "|".join(re.escape(norm_text(p)) for p in sorted(_PLACEHOLDERS, key=len, reverse=True))
)


@final
@dataclass(frozen=True, repr=False)
class Identifiers:
    """This patient's identifiers, normalised (norm_words), in memory for
    one stage only. The repr shows a count, never a value."""

    values: frozenset[str]
    _phrases: frozenset[str] = field(init=False, compare=False)
    _numbers: frozenset[str] = field(init=False, compare=False)

    def __post_init__(self) -> None:
        if not isinstance(self.values, frozenset) or not all(
            isinstance(v, str) for v in self.values
        ):
            raise TypeError("Identifiers takes a frozenset of str")
        phrases: set[str] = set()
        numbers: set[str] = set()
        for v in self.values:
            w = norm_words(v)
            if not w or w in _PLACEHOLDER_WORDS:
                continue
            compact = w.replace(" ", "")
            if any(c.isalpha() for c in w):
                if len(compact) >= MIN_NAME_LETTERS:
                    phrases.add(w)
            elif len(compact) >= MIN_NUMBER_DIGITS:
                numbers.add(compact)
            elif len(compact) >= MIN_NAME_LETTERS:
                phrases.add(w)
            for tok in w.split():
                if tok.isdigit() and len(tok) >= MIN_NUMBER_DIGITS:
                    numbers.add(tok)
        object.__setattr__(self, "_phrases", frozenset(phrases))
        object.__setattr__(self, "_numbers", frozenset(numbers))

    def __repr__(self) -> str:
        return f"Identifiers(<{len(self.values)} values>)"

    def hit(self, s: str) -> bool:
        """Whether s holds one of these identifiers."""
        if self._phrases:
            padded = f" {norm_words(s)} "
            if any(f" {p} " in padded for p in self._phrases):
                return True
        if self._numbers:
            for digits, bounds in _chains(norm_text(s)):
                for n in self._numbers:
                    i = digits.find(n)
                    while i != -1:
                        if i in bounds and i + len(n) in bounds:
                            return True
                        i = digits.find(n, i + 1)
        return False


def collect_identifiers(question: str, patient: Any) -> Identifiers:
    """This patient's identifiers, from the raw (unmasked) question and
    claimed patient data: every string, and every integer of 5 or more
    digits, under an identifying key (any depth up to MAX_DEPTH); each name
    token of 3 or more letters under a name key; and the original text of
    everything a fresh PIIMasker detects in the question and in every string
    of the tree (keys included)."""
    from intelligence.security.hipaa import PIIMasker

    masker = PIIMasker()  # a fresh one: its detections stay in this call
    found: set[str] = set()
    tokens: set[str] = set()

    def detect(s: str) -> None:
        _, detections = masker.mask(s)
        for d in detections:
            found.add(d.original_text)

    if isinstance(question, str):
        detect(question)
    stack: list[tuple[Any, int, bool, bool]] = [(patient, 0, False, False)]
    while stack:
        value, depth, under_id, under_name = stack.pop()
        if depth > MAX_DEPTH:
            continue
        if isinstance(value, str):
            detect(value)
            if under_id:
                found.add(value)
            if under_name:
                tokens.update(t for t in norm_words(value).split() if t.isalpha() and len(t) >= 3)
        elif isinstance(value, bool):
            continue
        elif isinstance(value, int | float):
            if under_id and (
                isinstance(value, int) or (math.isfinite(value) and value.is_integer())
            ):
                digits = str(abs(int(value)))
                if len(digits) >= MIN_NUMBER_DIGITS:
                    found.add(digits)
        elif isinstance(value, Mapping):
            for k, v in value.items():
                key = k.casefold() if isinstance(k, str) else ""
                if isinstance(k, str):
                    detect(k)
                stack.append(
                    (
                        v,
                        depth + 1,
                        under_id or key in IDENTIFYING_KEYS,
                        under_name or key in NAME_KEYS,
                    )
                )
        elif isinstance(value, list | tuple):
            stack.extend((v, depth + 1, under_id, under_name) for v in value)
    # Shorter than 3 letters or digits: too common to mean this patient.
    values = {w for w in (norm_words(v) for v in found) if len(w.replace(" ", "")) >= 3}
    values |= tokens
    return Identifiers(frozenset(values))


# --- the request's shape ---------------------------------------------------------------------


def _escape(key: str) -> str:
    return key.replace("~", "~0").replace("/", "~1")


def walk(doc: Any) -> Iterator[tuple[str, Any]]:
    """Every leaf of a JSON tree with its JSON pointer (RFC 6901). An empty
    object or array is a leaf too. Iterative, so depth can't recurse."""
    stack: list[tuple[str, Any]] = [("", doc)]
    while stack:
        ptr, value = stack.pop()
        if isinstance(value, dict) and value:
            for k in reversed(list(value)):
                stack.append((f"{ptr}/{_escape(str(k))}", value[k]))
        elif isinstance(value, list) and value:
            for i in reversed(range(len(value))):
                stack.append((f"{ptr}/{i}", value[i]))
        else:
            yield ptr, value


def leaf_pointers(doc: Any, prefix: str = "") -> frozenset[str]:
    """The pointers of every leaf of doc, under prefix: how an adapter
    derives its schema's pointers from reply.ANSWER_SCHEMA at import."""
    return frozenset(prefix + p for p, _ in walk(doc))


def _pointer_ok(p: object) -> bool:
    return isinstance(p, str) and p.startswith("/") and "*" not in p


@final
@dataclass(frozen=True)
class Shape:
    """An adapter's declaration of its request: exact JSON pointers, no
    wildcards. patient_paths are the two patient-derived blocks (the
    question and the context); allowed_paths are every leaf the adapter
    emits (the system prompt, the seven block pointers, its fixed scalars and
    its schema's leaves)."""

    patient_paths: frozenset[str]
    allowed_paths: frozenset[str]

    def __post_init__(self) -> None:
        for paths in (self.patient_paths, self.allowed_paths):
            if not isinstance(paths, frozenset) or not all(_pointer_ok(p) for p in paths):
                raise ValueError("a Shape takes frozensets of exact JSON pointers")
        if len(self.patient_paths) != 2 or not self.patient_paths <= self.allowed_paths:
            raise ValueError("a Shape has exactly two patient paths, both allowed")


def block_pointers(provider: str) -> tuple[str, ...]:
    """The text pointers of blocks 0 to 6 in the provider's layout."""
    if provider not in BLOCK_ARRAYS:
        raise ValueError("unknown provider")
    base = BLOCK_ARRAYS[provider]
    return tuple(f"{base}/{i}/text" for i in range(MAX_BLOCKS))


def shape_for(provider: str, fixed: Iterable[str]) -> Shape:
    """The provider's Shape: blocks 0 and 1 are the patient paths; allowed
    are the seven blocks (text and type) plus the adapter's fixed pointers
    (the system prompt, scalars, roles and the schema's leaves)."""
    texts = block_pointers(provider)
    base = BLOCK_ARRAYS[provider]
    types = (f"{base}/{i}/type" for i in range(MAX_BLOCKS))
    return Shape(frozenset(texts[:2]), frozenset(fixed) | frozenset(texts) | frozenset(types))


# --- the rules -------------------------------------------------------------------------------

_EMAIL: Final = re.compile(r"[\w.+-]+@[\w-]+(?:\.[\w-]+)*\.[a-z]{2,}")
_MONTH: Final = (
    r"(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?"
    r"|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)(?![a-z])"
)
_DATES: Final = (
    re.compile(r"(?<![0-9])[0-9]{1,2}[/.-][0-9]{1,2}[/.-][0-9]{2,4}(?![0-9])"),
    re.compile(r"(?<![0-9])[0-9]{4}[/.-][0-9]{1,2}[/.-][0-9]{1,2}(?![0-9])"),
    re.compile(
        r"(?<![0-9])[0-9]{1,2}(?:st|nd|rd|th)?[ ./-]?" + _MONTH + r"\.?[ ,./-]*[0-9]{4}(?![0-9])"
    ),
    re.compile(
        r"(?<![a-z])" + _MONTH + r"\.?[ ./-]?[0-9]{1,2}(?:st|nd|rd|th)?,?[ ./-]?[0-9]{4}(?![0-9])"
    ),
)
_MRN: Final = re.compile(
    r"\b(?:mrn(?![a-z])|hospital no(?![a-z])\.?|hosp\.? no(?![a-z])\.?)\s*[:#]?\s*[^\W_]"
)
# A JSON object key in the raw text (the body has already parsed strictly).
_JSON_KEY: Final = re.compile(r'"(?:[^"\\]|\\.)*"\s*:')


def _pattern(s: str) -> str | None:
    t = _PLACEHOLDER_RE.sub(" ", norm_text(s))
    runs = _digit_runs(t)
    candidates = runs + _grouped_runs(t)
    if any(_is_phone(r) for r in candidates):
        return "ng_phone"
    if any(len(r) == 11 for r in candidates):
        return "nin"
    if _EMAIL.search(t):
        return "email"
    if any(d.search(t) for d in _DATES):
        return "date"
    if any(len(r) >= 6 for r in runs):
        return "long_digits"
    if _MRN.search(t):
        return "mrn_prefix"
    return None


def _scan(body: bytes, ids: Identifiers, shape: Shape) -> str | None:
    if len(body) > MAX_BODY_BYTES:
        return "too_large"
    try:
        doc = _strictjson.loads(body)
    except (_strictjson.StrictJSONError, RecursionError):  # deep nesting: not ours
        return "not_json"
    if not isinstance(doc, dict):
        return "shape"
    strings: dict[str, str] = {}
    for ptr, leaf in walk(doc):
        if ptr not in shape.allowed_paths:
            return "shape"
        if isinstance(leaf, str):
            strings[ptr] = leaf
    if not all(p in strings for p in shape.patient_paths):
        return "shape"
    if any(ids.hit(s) for s in strings.values()):
        return "known_identifier"
    # Once more over the raw text, keys aside (the shape pins every key):
    # a value outside a JSON string would be caught here.
    if ids.hit(_JSON_KEY.sub(" ", body.decode("utf-8"))):
        return "known_identifier"
    for p in sorted(shape.patient_paths):
        rule = _pattern(strings[p])
        if rule is not None:
            return rule
    return None


def scan_body(body: bytes, ids: Identifiers, shape: Shape) -> str | None:
    """The first rule the body breaks, as a code from RULES, or None when it
    may be sent. Never raises: anything unexpected is "scan_error"."""
    if not isinstance(body, bytes | bytearray):
        return "not_json"
    if type(ids) is not Identifiers or type(shape) is not Shape:
        return "scan_error"
    try:
        return _scan(bytes(body), ids, shape)
    except Exception:
        return "scan_error"


@final
class ScanHook:
    """An httpx2 request hook (event_hooks["request"]) for one call: it
    holds that call's identifiers and the adapter's shape, scans the exact
    bytes, and raises EgressRefused(rule) before the transport sees them.
    verdict is the last rule (None while every body passed); sent is True
    once a body has passed. Once refused, it refuses every later request."""

    __slots__ = ("_ids", "_shape", "sent", "verdict")

    def __init__(self, ids: Identifiers, shape: Shape) -> None:
        if type(ids) is not Identifiers or type(shape) is not Shape:
            raise TypeError("ScanHook takes Identifiers and a Shape")
        self._ids = ids
        self._shape = shape
        self.verdict: str | None = None
        self.sent = False

    def __call__(self, request: httpx2.Request) -> None:
        rule = self.verdict if self.verdict is not None else self._check(request)
        if rule is not None:
            self.verdict = rule
            raise EgressRefused(rule)
        self.sent = True

    def _check(self, request: httpx2.Request) -> str | None:
        try:
            if any(h in request.headers for h in TRACE_HEADERS):
                return "trace_header"
        except Exception:
            return "scan_error"
        try:
            content = request.content
        except Exception:  # a streamed body, not read: never sent unscanned
            return "not_json"
        return scan_body(content, self._ids, self._shape)

    def __repr__(self) -> str:
        return f"ScanHook(verdict={self.verdict!r}, sent={self.sent})"
