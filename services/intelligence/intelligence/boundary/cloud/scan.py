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
from collections.abc import Iterator, Mapping
from dataclasses import dataclass, field
from types import MappingProxyType
from typing import TYPE_CHECKING, Any, Final, final
from urllib.parse import unquote

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
    "pii_detected",
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
    (0x2800, 0x2800),  # the braille blank renders as nothing (review I5)
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


# Letters from Cyrillic, Greek and Armenian that look like Latin ones,
# mapped to the Latin letter (a confusables skeleton, review I5). The map only
# adds matches; a Greek mu (from the micro sign) and beta are left alone.
_SKELETON: Final = MappingProxyType(
    {
        # Cyrillic
        "\u0430": "a",
        "\u0435": "e",
        "\u043e": "o",
        "\u0440": "p",
        "\u0441": "c",
        "\u0443": "y",
        "\u0445": "x",
        "\u0456": "i",
        "\u0458": "j",
        "\u0455": "s",
        "\u04bb": "h",
        "\u0501": "d",
        "\u04cf": "l",
        "\u051b": "q",
        "\u051d": "w",
        "\u043a": "k",
        "\u0410": "A",
        "\u0412": "B",
        "\u0415": "E",
        "\u041a": "K",
        "\u041c": "M",
        "\u041d": "H",
        "\u041e": "O",
        "\u0420": "P",
        "\u0421": "C",
        "\u0422": "T",
        "\u0423": "Y",
        "\u0425": "X",
        "\u0406": "I",
        "\u0408": "J",
        "\u0405": "S",
        "\u04ae": "Y",
        # Greek
        "\u03b1": "a",
        "\u03bf": "o",
        "\u03c1": "p",
        "\u03bd": "v",
        "\u03b9": "i",
        "\u03ba": "k",
        "\u03c5": "u",
        "\u03c7": "x",
        "\u0391": "A",
        "\u0392": "B",
        "\u0395": "E",
        "\u0396": "Z",
        "\u0397": "H",
        "\u0399": "I",
        "\u039a": "K",
        "\u039c": "M",
        "\u039d": "N",
        "\u039f": "O",
        "\u03a1": "P",
        "\u03a4": "T",
        "\u03a5": "Y",
        "\u03a7": "X",
        # Armenian
        "\u0585": "o",
        "\u057d": "u",
        "\u0581": "g",
        "\u0570": "h",
        "\u0578": "n",
        "\u0566": "q",
    }
)
# Hyphens, dashes and the minus sign, as "-" (review I2: an en dash in a
# phone number, as phone autocorrect writes it).
_DASHES: Final = frozenset("\u2010\u2011\u2012\u2013\u2014\u2015\u2212\ufe58\ufe63\uff0d")


def _clean(s: str, fold: bool = True) -> str:
    """NFKC, invisible characters removed, every decimal digit as ASCII,
    confusable letters as Latin, dashes as "-", and (with fold) case-folded."""
    t = unicodedata.normalize("NFKC", s)
    out = []
    for c in t:
        if _invisible(c):
            continue
        if unicodedata.category(c) == "Nd":
            out.append(str(unicodedata.digit(c)))
        elif c in _DASHES:
            out.append("-")
        else:
            out.append(_SKELETON.get(c, c))
    joined = "".join(out)
    return joined.casefold() if fold else joined


def norm_words(s: str) -> str:
    """For the identifier rule: NFKC, Cf removed, case-folded, accents
    folded, and every run of characters that aren't letters or digits one
    space. "Adaeze\\nOkafor", "ADAEZE\\u200b OKAFOR" and "adaeze-okafor" all
    give "adaeze okafor"."""
    t = unicodedata.normalize("NFKD", _clean(s))
    # Every combining mark goes (Mn, and Mc and Me too: review I5).
    t = "".join(c for c in t if unicodedata.category(c)[0] != "M")
    t = "".join(c if unicodedata.category(c)[0] in "LN" else " " for c in t)
    return " ".join(t.split())


def norm_text(s: str) -> str:
    """For the pattern rules: NFKC, Cf removed, case-folded, every run of
    Unicode spaces one ASCII space. Punctuation is kept."""
    t = _clean(s)
    t = "".join(" " if c.isspace() or unicodedata.category(c)[0] == "Z" else c for c in t)
    return " ".join(t.split())


_DIGITS = re.compile(r"[0-9]+")
# The widest gap of non-alphanumerics between two digit groups that still
# joins them (review I2): "(0803) 123 4567", "0803 - 123 - 4567", "284,817".
MAX_DIGIT_GAP: Final = 3


def _digit_runs(t: str) -> list[str]:
    return _DIGITS.findall(t)


def digit_runs(s: str) -> list[str]:
    """Plain digit runs of norm_text(s): any non-digit ends a run, so
    "T 38.5 101.3" gives 38, 5, 101, 3 and "HR 100-120" gives 100, 120 (fix
    round: a hyphen no longer joins, so ranges are sent)."""
    return _digit_runs(norm_text(s))


# What a person puts between the groups of one number. Not "<", ">", "#" or
# ":": the chunk delimiters ("<<chunk KB#0001>>\n15 mg") must never join two
# numbers into an identifier (plan Review Focus 1).
_GAP_CHARS: Final = frozenset(" ()-.,/+")


def _gap_ok(t: str, start: int, end: int) -> bool:
    """A short gap of 1 to MAX_DIGIT_GAP characters, each a space or one of
    ( ) - . , / + (dashes are "-" after norm_text)."""
    return 1 <= end - start <= MAX_DIGIT_GAP and all(c in _GAP_CHARS for c in t[start:end])


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


def _phone_runs(t: str) -> list[str]:
    """For ng_phone (review I2): groups of 2 to 4 digits joined across any
    short gap (_gap_ok), kept where the joined run is a Nigerian phone number.
    A lone "0" right after "234" is the trunk prefix ("+234 (0) 803 ...") and
    is skipped. The 0[789][01] and 234[789][01] prefixes keep vitals out."""
    chains: list[list[str]] = []
    prev_end = -1
    prev = ""
    for m in _DIGITS.finditer(t):
        g = m.group()
        trunk = g == "0" and prev == "234"
        if not (2 <= len(g) <= 4 or trunk):
            prev_end, prev = -1, ""
            continue
        if chains and prev_end >= 0 and _gap_ok(t, prev_end, m.start()):
            chains[-1].append("" if trunk else g)
        else:
            chains.append([g])
        prev_end, prev = m.end(), g
    out: list[str] = []
    for chain in chains:
        for i in range(len(chain)):
            run = chain[i]
            for j in range(i + 1, len(chain)):
                run += chain[j]
                if len(run) > 13:
                    break
                if _is_phone(run):
                    out.append(run)
    return out


def _chains(t: str) -> list[tuple[str, frozenset[int]]]:
    """For the identifier rule: the digits-only projection of norm_text,
    groups joined across any short gap (_gap_ok, review I2), with the offsets
    where groups meet."""
    out: list[tuple[str, frozenset[int]]] = []
    digits = ""
    bounds: set[int] = set()
    prev_end = -2
    for m in _DIGITS.finditer(t):
        if digits and _gap_ok(t, prev_end, m.start()):
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
_PLACEHOLDER_ALT: Final = "|".join(
    re.escape(norm_text(p)) for p in sorted(_PLACEHOLDERS, key=len, reverse=True)
)
_PLACEHOLDER_RE: Final = re.compile(_PLACEHOLDER_ALT)
# A placeholder between two digits joins them (review M1): only the writer
# puts a placeholder literal there, to split a number.
_PLACEHOLDER_IN_DIGITS: Final = re.compile(rf"(?<=[0-9])(?:{_PLACEHOLDER_ALT})+(?=[0-9])")


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
        """Whether s holds one of these identifiers, as written or with the
        masker's placeholders taken out (review M1)."""
        t = norm_text(s)
        stripped = _PLACEHOLDER_RE.sub("", t)
        return self._hit(s, t) or (stripped != t and self._hit(stripped, stripped))

    def _hit(self, s: str, t: str) -> bool:
        if self._phrases:
            padded = f" {norm_words(s)} "
            if any(f" {p} " in padded for p in self._phrases):
                return True
        if self._numbers:
            for digits, bounds in _chains(t):
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


def leaf_values(doc: Any, prefix: str = "") -> dict[str, Any]:
    """Every leaf of doc with its value, under prefix: how an adapter pins
    its schema's values (Shape.fixed_values)."""
    return {prefix + p: v for p, v in walk(doc)}


def _same(a: object, b: object) -> bool:
    # Exact type, so True can't stand in for 1, nor 1.0 for 1.
    return type(a) is type(b) and a == b


def _pointer_ok(p: object) -> bool:
    return isinstance(p, str) and p.startswith("/") and "*" not in p


@final
@dataclass(frozen=True)
class Shape:
    """An adapter's declaration of its request: exact JSON pointers, no
    wildcards. patient_paths are the two patient-derived blocks (the
    question and the context); allowed_paths are every leaf the adapter
    emits. fixed_values pins the value of every leaf that isn't free text
    (the system prompt, the model, scalars, roles, block types and the
    schema's leaves: review M6), so only the blocks carry free text."""

    patient_paths: frozenset[str]
    allowed_paths: frozenset[str]
    fixed_values: Mapping[str, Any] = field(
        default_factory=lambda: MappingProxyType({}), compare=False, hash=False
    )

    def __post_init__(self) -> None:
        for paths in (self.patient_paths, self.allowed_paths):
            if not isinstance(paths, frozenset) or not all(_pointer_ok(p) for p in paths):
                raise ValueError("a Shape takes frozensets of exact JSON pointers")
        if len(self.patient_paths) != 2 or not self.patient_paths <= self.allowed_paths:
            raise ValueError("a Shape has exactly two patient paths, both allowed")
        if not isinstance(self.fixed_values, Mapping):
            raise ValueError("fixed_values is a mapping")
        fixed = dict(self.fixed_values)
        if not set(fixed) <= self.allowed_paths or set(fixed) & self.patient_paths:
            raise ValueError("a fixed pointer is allowed and not a patient path")
        object.__setattr__(self, "fixed_values", MappingProxyType(fixed))


def block_pointers(provider: str) -> tuple[str, ...]:
    """The text pointers of blocks 0 to 6 in the provider's layout."""
    if provider not in BLOCK_ARRAYS:
        raise ValueError("unknown provider")
    base = BLOCK_ARRAYS[provider]
    return tuple(f"{base}/{i}/text" for i in range(MAX_BLOCKS))


BLOCK_TYPES: Final = MappingProxyType({"anthropic": "text", "openai": "input_text", "xai": "text"})


def shape_for(provider: str, fixed: Mapping[str, Any]) -> Shape:
    """The provider's Shape: blocks 0 and 1 are the patient paths; the seven
    block texts are the only free text; every other leaf is fixed, with its
    value: the block types, plus the adapter's own (the system prompt,
    scalars, roles and the schema's leaves, from leaf_values)."""
    texts = block_pointers(provider)
    base = BLOCK_ARRAYS[provider]
    values = {f"{base}/{i}/type": BLOCK_TYPES[provider] for i in range(MAX_BLOCKS)}
    values.update(fixed)
    if set(values) & set(texts):
        raise ValueError("a block's text is never fixed")
    return Shape(frozenset(texts[:2]), frozenset(texts) | frozenset(values), values)


# --- the rules -------------------------------------------------------------------------------

_EMAIL: Final = re.compile(r"[\w.+-]+@[\w-]+(?:\.[\w-]+)*\.[a-z]{2,}")
_MONTH: Final = (
    r"(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?"
    r"|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)(?![a-z])"
)
_DATES: Final = (
    # A space either side of the separator too (review I3): "12 / 03 / 1984".
    re.compile(r"(?<![0-9])[0-9]{1,2} ?[/.-] ?[0-9]{1,2} ?[/.-] ?[0-9]{2,4}(?![0-9])"),
    re.compile(r"(?<![0-9])[0-9]{4} ?[/.-] ?[0-9]{1,2} ?[/.-] ?[0-9]{1,2}(?![0-9])"),
    # Spaces only, with a 4-digit year from 1900 to 2099: "12 03 1984".
    re.compile(r"(?<![0-9.])[0-9]{1,2} [0-9]{1,2} (?:19|20)[0-9]{2}(?![0-9])"),
    # "12 March 1984", "12th of March, 1984", "12-mar-1984".
    re.compile(
        r"(?<![0-9])[0-9]{1,2}(?:st|nd|rd|th)?[ ./-]?(?:of )?"
        + _MONTH
        + r"\.?[ ,./-]*[0-9]{4}(?![0-9])"
    ),
    re.compile(
        r"(?<![a-z])" + _MONTH + r"\.?[ ./-]?[0-9]{1,2}(?:st|nd|rd|th)?,?[ ./-]?[0-9]{4}(?![0-9])"
    ),
)
# A record-number label, then a number or code (review I4). The label must
# end on a word boundary ("hospital notes" is not one), and the code starts
# with a letter or digit, never "_" (so "[mrn_redacted]" is not one).
_MRN: Final = re.compile(
    r"\b(?:m\.?r\.?n|(?:hospital|hosp\.?|folder|card|file) ?(?:number|no))(?![a-z])"
    r"[\s:#=.\-]*[^\W_]"
)
# A JSON object key in the raw text (the body has already parsed strictly).
_JSON_KEY: Final = re.compile(r'"(?:[^"\\]|\\.)*"\s*:')


def _pattern(s: str) -> str | None:
    t = _PLACEHOLDER_IN_DIGITS.sub("", norm_text(s))
    t = _PLACEHOLDER_RE.sub(" ", t)
    runs = _digit_runs(t)
    candidates = runs + _grouped_runs(t)
    if any(_is_phone(r) for r in candidates + _phone_runs(t)):
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
    fixed = shape.fixed_values
    for ptr, leaf in walk(doc):
        if ptr not in shape.allowed_paths:
            return "shape"
        if ptr in fixed and not _same(leaf, fixed[ptr]):
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
    patient = [strings[p] for p in sorted(shape.patient_paths)]
    for s in patient:
        rule = _pattern(s)
        if rule is not None:
            return rule
    # Ruling C5: the masker's own detector over the two patient blocks; any
    # detection refuses (the blocks should already be masked output).
    if any(_masker_detects(s) for s in patient):
        return "pii_detected"
    return None


def _masker_detects(s: str) -> bool:
    """Whether PIIMasker (the local masker, a fresh instance) finds anything
    in s, read with invisible characters removed and confusables mapped, case
    kept (its name rules need capitals)."""
    from intelligence.security.hipaa import PIIMasker

    _, detections = PIIMasker().mask(_clean(s, fold=False))
    return bool(detections)


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
            # The URL (decoded) and every header value carry no identifier
            # of this patient either (review M5).
            if self._ids.hit(unquote(str(request.url))) or any(
                self._ids.hit(v) for v in request.headers.values()
            ):
                return "known_identifier"
        except Exception:
            return "scan_error"
        try:
            content = request.content
        except Exception:  # a streamed body, not read: never sent unscanned
            return "not_json"
        return scan_body(content, self._ids, self._shape)

    def __repr__(self) -> str:
        return f"ScanHook(verdict={self.verdict!r}, sent={self.sent})"
