"""The synthetic-data scanner (P20): nothing real may enter a synthetic set.

Fails closed. Every identifier, name, street, facility and clinician in a
ground-truth document, and every string in intake metadata, must match the
synthetic patterns or the committed lists. Anything that looks like a real
identifier is refused: a Nigerian phone, a national id or BVN (11 digits), a
real-format MRN, an email, a URL, a street address that isn't in the list.

Free-text names are covered only when titled ("Dr", "Chief", "Alhaji", ...): a
bare name in a note with no title and no other pattern is not detected, so
generated notes hold only keyed fields and the fixed contact line.

A finding carries the location (a field path), the reason ``not_synthetic`` and
a fixed rule name. It never carries the offending value.
"""

import hashlib
import json
import math
import os
import posixpath
import re
import stat
import subprocess
import unicodedata
from collections.abc import Iterator
from dataclasses import dataclass
from decimal import Decimal
from functools import lru_cache
from pathlib import Path
from typing import Any, Literal

from intelligence.doceval import schema
from intelligence.doceval.synth import terminology

NOT_SYNTHETIC = "not_synthetic"
MAX_FILE_BYTES = 64 * 1024 * 1024
IMAGE_SUFFIXES = frozenset({".png", ".jpg", ".jpeg"})
JSON_SUFFIXES = frozenset({".json", ".jsonl"})

MRN_RE = re.compile(r"SYN-[0-9]{8}")
OTHER_ID_RE = re.compile(r"SYN(?:ID|INS)-[0-9]{8}")
_ALLOWED_TOKEN = re.compile(r"(?<![A-Za-z0-9])SYN(?:ID|INS)?-[0-9]{8}(?![0-9])")
# Whole token only; being joined to other digits is refused later (see _strip_allowed).
_ALLOWED_PHONE = re.compile(r"(?<![0-9A-Za-z])555-01[0-9]{2}(?![0-9A-Za-z])")
_ISO = re.compile(
    r"(?<![0-9])[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[Tt ][0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]{1,6})?)?"
    r"(?:[Zz]|[+-][0-9]{2}:[0-9]{2})?)?(?![0-9])"
)
# Printed dates (the renderer prints them): 12-03-2026, 12.03.2026, 12/03/2026, 2026/03/12.
_PRINTED_DATE = re.compile(
    r"(?<![0-9])(?:[0-9]{1,2}[-./][0-9]{1,2}[-./](?:[0-9]{4}|[0-9]{2})"
    r"|[0-9]{4}[-./][0-9]{1,2}[-./][0-9]{1,2})(?![0-9])"
)
# Digit groups joined by up to 5 separator characters (spaces, punctuation, any dash).
_RUN = re.compile(r"\+?[0-9](?:(?:[^\w]|_){0,5}[0-9])*")
_TOKEN_NUMBER = re.compile(r"[0-9]+(?:\.[0-9]+)?")
_NG_PHONE_DIGITS = re.compile(r"(?:234|0)?[789][01][0-9]{8}")
_EMAIL = re.compile(r"@")
_URL = re.compile(r"(?:https?://|www\.|ftp://)", re.IGNORECASE)
_MRN_WORD = re.compile(
    r"\b(?:MRN|HOSP(?:ITAL)?\.?\s*(?:NO|NUMBER|NUM|#)|PATIENT\s*ID|"
    r"(?:FOLDER|CARD|FILE|REG(?:ISTRATION)?|UNIT)\.?\s*(?:NO|NUMBER|NUM|#)|NHIS|NIN|BVN)"
    r"(?![^\W\d_])[\s:#.\-]*\S",
    re.IGNORECASE,
)
_PREFIXED_ID = re.compile(r"\b[A-Z]{2,6}[-/ ]?[0-9]{5,}\b")
# Letters, then 5 or more digits joined through up to 5 separators: LUTH ( 2019 ) 12345.
_LETTERS_DIGITS = re.compile(r"[^\W\d_]{2,8}(?:[^\w]|_){0,5}[0-9](?:(?:[^\w]|_){0,5}[0-9]){4,}")
_SLASHED_ID = re.compile(r"\b[^\W\d_]{2,8}(?:[/\-][0-9]{1,6}){1,4}\b")
_TITLES = (
    "Dr|Doctor|Mr|Mrs|Ms|Miss|Prof|Nurse|Sister|Chief|Alh|Alhaji|Alhaja|Pastor|Engr|Barr|"
    "Barrister|Rev|Hon|Mallam|Madam|Oba"
)
_WORD = r"[^\W\d_][^\W\d_'\-]*"
_TITLED = re.compile(rf"\b({_TITLES})(?:\.\s*|\s+)({_WORD})((?:\s+{_WORD}){{0,2}})", re.IGNORECASE)
_ADDRESS = re.compile(
    r"(?<![0-9])[0-9]{1,5}[A-Za-z]?[,\s]+((?:[^\W\d_]+\s+){1,3}"
    r"(?:Street|St|Str|Road|Rd|Avenue|Ave|Lane|Close|Crescent|Drive|Way|Estate|Layout|Court|"
    r"Place|Boulevard|Blvd|Expressway|Highway))\b",
    re.IGNORECASE,
)
# Ordinary clinical phrases that begin with a title word.
_CLINICAL_PHRASES = frozenset(
    {("chief", "complaint"), ("nurse", "practitioner"), ("nurse", "notes"), ("nurse", "note")}
)
_CONTACT = re.compile(r"[0-9]{1,5} (.+); Tel 555-01[0-9]{2}")
_SAFE_NAME = re.compile(r"[A-Za-z0-9._-]{1,128}")

# Fields that legitimately hold numbers and codes: free-text digit rules are looser there
# (a single number of 7+ digits is still refused).
_LAB_TEXT = frozenset(
    {
        "observations[].value_text",
        "observations[].reference_text",
        "observations[].value_numeric",
        "observations[].reference_low",
        "observations[].reference_high",
        "observations[].local_code",
        "observations[].unit_text",
        "conditions[].local_code",
        "medications[].strength_text",
        "medications[].dose_text",
    }
)
_NUMERIC_LAB = frozenset(
    {
        "observations[].value_numeric",
        "observations[].reference_low",
        "observations[].reference_high",
    }
)


@dataclass(frozen=True)
class Finding:
    """Where something non-synthetic was found, and which rule fired. No value."""

    path: str
    rule: str
    reason: str = NOT_SYNTHETIC

    def __str__(self) -> str:
        return f"{self.reason} {self.path} {self.rule}"


@dataclass(frozen=True)
class ScanResult:
    ok: bool
    findings: tuple[Finding, ...]
    scanned: int


def _normalise(text: str) -> str:
    """NFKC, then every Unicode decimal digit becomes an ASCII digit."""
    text = unicodedata.normalize("NFKC", text)
    return "".join(str(unicodedata.decimal(c)) if c.isdecimal() else c for c in text)


def _has_phone_window(digits: str) -> bool:
    """Does any contiguous substring of the digits fully match a Nigerian phone?"""
    for size in (10, 11, 13):
        for i in range(len(digits) - size + 1):
            if _NG_PHONE_DIGITS.fullmatch(digits[i : i + size]):
                return True
    return False


def _digit_rules(rest: str, lab: bool, numeric: bool = False) -> list[str]:
    """Phone, national-id-like and long digit runs.

    Digits are joined across up to 5 separator characters in every field, and no
    text is removed first (see ``_strip_allowed``). A joined run is refused when
    it is 10 or more digits (a phone, NIN or BVN, however it is glued), or when
    any substring of it is a Nigerian phone. ``lab`` fields (lab text, codes,
    doses) only loosen the shorter runs: a single number of up to 6 digits is
    fine, and ``numeric`` (the numeric lab value fields) up to 9.
    """
    rules: list[str] = []
    for m in _RUN.finditer(rest):
        digits = re.sub(r"[^0-9]", "", m.group(0))
        if _has_phone_window(digits):
            rules.append("phone")
        elif len(digits) >= 11:
            rules.append("national_id_like")
        elif len(digits) >= 10 or (not lab and len(digits) >= 6):
            rules.append("long_digits")
    if lab and not numeric:
        for m in _TOKEN_NUMBER.finditer(rest):
            if len(m.group(0).replace(".", "")) >= 7:
                rules.append("long_digits")
    return rules


def _is_sep(char: str) -> bool:
    return not char.isalnum()


def _joined(text: str, start: int, end: int, left: bool, right: bool, covered: set[int]) -> bool:
    """Is the span joined, through up to 5 separators, to a digit outside any allowed span?"""
    if left:
        j, seps = start - 1, 0
        while j >= 0 and _is_sep(text[j]) and seps < 5:
            j, seps = j - 1, seps + 1
        if j >= 0 and text[j].isdigit() and j not in covered:
            return True
    if right:
        j, seps = end, 0
        while j < len(text) and _is_sep(text[j]) and seps < 5:
            j, seps = j + 1, seps + 1
        if j < len(text) and text[j].isdigit() and j not in covered:
            return True
    return False


def _strip_allowed(text: str) -> str:
    """Blank out synthetic tokens, ISO and printed dates and ``555-01NN``, but only
    where the token stands alone: one joined to other digits (``0803 555-0123``)
    stays in the text, so the digit rules see the whole run.
    """
    kept: list[tuple[int, int, bool, bool]] = []
    for pattern, left, right in (
        (_ISO, True, True),
        (_PRINTED_DATE, True, True),
        (_ALLOWED_PHONE, True, True),
        (_ALLOWED_TOKEN, False, True),
    ):
        for m in pattern.finditer(text):
            if not any(m.start() < e and s < m.end() for s, e, _, _ in kept):
                kept.append((m.start(), m.end(), left, right))
    covered = {i for s, e, _, _ in kept for i in range(s, e)}
    out = text
    for s, e, left, right in sorted(kept, reverse=True):
        if not _joined(text, s, e, left, right, covered):
            out = out[:s] + " zz " + out[e:]
    return out


def _text_rules(text: str, lab: bool = False, numeric: bool = False) -> list[str]:
    """Rule names that fire on free text.

    ``lab`` loosens the digit rule for fields that hold lab numbers and codes.
    """
    text = _normalise(text)
    rules: list[str] = []
    if _EMAIL.search(text):
        rules.append("email")
    if _URL.search(text):
        rules.append("url")
    rest = _strip_allowed(text)
    rules.extend(_digit_rules(rest, lab, numeric))
    if any(p.search(rest) for p in (_MRN_WORD, _PREFIXED_ID, _SLASHED_ID, _LETTERS_DIGITS)):
        rules.append("mrn_like")
    tokens = {t.casefold() for t in terminology.all_name_tokens()}
    for m in _TITLED.finditer(rest):
        if (m.group(1).casefold(), m.group(2).casefold()) in _CLINICAL_PHRASES:
            continue
        extra = [w for w in m.group(3).split() if w[:1].isupper()]
        if m.group(2).casefold() not in tokens or any(w.casefold() not in tokens for w in extra):
            rules.append("titled_name")
            break
    streets = {s.casefold() for s in terminology.name_set("streets")}
    for m in _ADDRESS.finditer(rest):
        if " ".join(m.group(1).split()).casefold() not in streets:
            rules.append("address")
            break
    return sorted(set(rules))


def text_ok(text: str) -> bool:
    """Would ``text`` pass the free-text rules (as a metadata string or a file name part)?"""
    return not _text_rules(text)


def _is_name(text: str, kind: str) -> bool:
    words = text.split()
    allowed = terminology.name_set(kind)
    return bool(words) and all(w in allowed for w in words)


def _is_clinician(text: str) -> bool:
    for prefix in ("Dr. ", "Dr "):
        if text.startswith(prefix):
            text = text[len(prefix) :]
            break
    return text in terminology.name_set("clinicians")


def _is_contact(text: str) -> bool:
    m = _CONTACT.fullmatch(text)
    return m is not None and m.group(1) in terminology.name_set("streets")


def _typed_rule(pattern: str, text: str) -> str | None:
    """The rule a value of a known field breaks, if any."""
    if pattern == "patient.mrn":
        return None if MRN_RE.fullmatch(text) else "mrn_pattern"
    if pattern == "patient.other_ids[].value":
        return None if OTHER_ID_RE.fullmatch(text) else "id_pattern"
    if pattern == "patient.family_name":
        return None if _is_name(text, "family") else "name"
    if pattern == "patient.given_names":
        return None if _is_name(text, "given") else "name"
    if pattern in ("document.facility_name", "encounter.location_text"):
        return None if text in terminology.name_set("facilities") else "facility"
    if pattern == "encounter.clinician_text":
        return None if _is_clinician(text) else "clinician"
    if pattern == "notes[].text" and "Tel " in text:
        return None if _is_contact(text) else "contact"
    return None


def _walk(node: Any, path: str) -> Iterator[tuple[str, Any]]:
    """Leaves of a schema-valid document (its keys are schema names, safe to show)."""
    stack: list[tuple[str, Any]] = [(path, node)]
    while stack:
        where, cur = stack.pop()
        if isinstance(cur, dict):
            for key, child in cur.items():
                stack.append((f"{where}.{key}" if where else str(key), child))
        elif isinstance(cur, list):
            for i, child in enumerate(cur):
                stack.append((f"{where}[{i}]", child))
        else:
            yield where, cur


def _field_pattern(path: str) -> str | None:
    for leaf in (".value", ".raw_text"):
        if path.endswith(leaf):
            return schema.path_pattern(path[: -len(leaf)])
    return None


MAX_LAB_INT_DIGITS = 9  # a numeric lab value has at most this many integer digits
MAX_BOX_DECIMALS = 6  # the renderer rounds box coordinates to 6 places


def _lab_number_ok(leaf: int | float) -> bool:
    if not math.isfinite(leaf):
        return False
    return len(str(int(abs(leaf)))) <= MAX_LAB_INT_DIGITS


def _box_number_ok(leaf: int | float) -> bool:
    """A box coordinate: in [0, 1] with at most six decimals (anything else is checked)."""
    if not math.isfinite(leaf) or not 0 <= leaf <= 1:
        return False
    exponent = Decimal(repr(leaf)).as_tuple().exponent
    return isinstance(exponent, int) and exponent >= -MAX_BOX_DECIMALS


def _numeric_exempt(path: str, pattern: str | None, leaf: int | float) -> bool:
    """Numbers the schema defines: lab values and ranges (up to 9 integer digits), pages,
    boxes (in [0, 1], up to 6 decimals), confidence."""
    return (
        (pattern in _NUMERIC_LAB and path.endswith(".value") and _lab_number_ok(leaf))
        or (".bbox[" in path and _box_number_ok(leaf))
        or path.endswith((".page", ".confidence"))
        or path == "document.page_count"
    )


def check_document(gt: Any) -> list[Finding]:
    """Check one ground-truth document. An empty list means every value is synthetic."""
    if not isinstance(gt, dict):
        return [Finding("", "schema")]
    try:
        if isinstance(gt.get("document"), dict) and "page_count" in gt["document"]:
            schema.validate_stored(gt)
        else:
            schema.validate_model_output(gt)
    except schema.SchemaError as exc:
        return [Finding(_dotted(exc.pointer), "schema")]
    except RecursionError:
        return [Finding("", "schema")]
    found: dict[tuple[str, str], Finding] = {}
    for path, leaf in _walk(gt, ""):
        pattern = _field_pattern(path)
        lab = pattern in _LAB_TEXT
        if isinstance(leaf, bool) or leaf is None:
            continue
        if isinstance(leaf, int | float):
            if _numeric_exempt(path, pattern, leaf):
                continue
            rules = _digit_rules(_normalise(str(leaf)), lab, pattern in _NUMERIC_LAB)
        elif isinstance(leaf, str):
            rules = _text_rules(leaf, lab, pattern in _NUMERIC_LAB)
            if pattern is not None:
                typed = _typed_rule(pattern, leaf)
                if typed is not None:
                    rules.append(typed)
        else:
            continue
        for rule in rules:
            found[(path, rule)] = Finding(path, rule)
    return list(found.values())


def _dotted(pointer: str) -> str:
    out = ""
    for part in pointer.split("/")[1:]:
        part = part.replace("~1", "/").replace("~0", "~")
        out += f"[{part}]" if part.isdigit() else (f".{part}" if out else part)
    return out


_META_KEYS: dict[str, str] = {
    "mrn": "mrn",
    "family_name": "family",
    "given_names": "given",
    "facility": "facilities",
    "facility_name": "facilities",
    "location": "facilities",
    "clinician": "clinicians",
}


RawPath = tuple[str | int, ...]


def _metadata_leaves(meta: Any) -> Iterator[tuple[str, str | None, Any, bool, RawPath]]:
    """Yield ``(display_path, key, leaf, False, raw)`` for leaves and ``(path, None, key,
    True, raw)`` for keys; ``raw`` is the real path (keys and indexes), never displayed.

    Only known metadata key names and list indexes appear in a display path; any other
    key is shown as ``[k<position>]``, because a key may itself be a real value.
    """
    stack: list[tuple[str, str | None, Any, RawPath]] = [("", None, meta, ())]
    while stack:
        where, key, cur, raw = stack.pop()
        if isinstance(cur, dict):
            for i, (k, child) in enumerate(cur.items()):
                shown = k if k in _META_KEYS else f"[k{i}]"
                here = f"{where}.{shown}" if where else shown
                yield here, None, k, True, (*raw, k)
                stack.append((here, k, child, (*raw, k)))
        elif isinstance(cur, list):
            for i, child in enumerate(cur):
                stack.append((f"{where}[{i}]", key, child, (*raw, i)))
        else:
            yield where, key, cur, False, raw


# Values the set builder computes rather than copies from a document. Each is exempt only
# in its own kind of file, at exactly its path (``*`` is a list index) and in exactly its
# shape; anywhere else it goes through the usual rules.
#
# * In the scan root's ``testset.json`` (top-level ``schema`` mhai.evaluation.testset/1):
#   ``files[*].size`` and ``files[*].sha256``, which ``scan_set`` recomputes from the file
#   on disk, and ``generator.commit``, which must equal :func:`current_commit`.
# * In ``hostile/<name>.json`` (mhai.evaluation.hostile/1): ``expected.regions[*].bbox[i]``,
#   a number in [0, 1] with at most 4 fractional digits.
# * In any metadata file: a top-level ``schema`` naming one of :data:`SCHEMA_IDS`.
Context = Literal["testset", "hostile"]
TESTSET_SCHEMA_ID = "mhai.evaluation.testset/1"
HOSTILE_SCHEMA_ID = "mhai.evaluation.hostile/1"
SCHEMA_IDS = frozenset({TESTSET_SCHEMA_ID, HOSTILE_SCHEMA_ID})
_SHA256_HEX = re.compile(r"[0-9a-f]{64}")
_COMMIT_HEX = re.compile(r"[0-9a-f]{40}")
_BBOX_NUMBER = re.compile(r"(?:0|1)(?:\.[0-9]{1,4})?")
_MAX_SIZE = 1 << 40
_COMPUTED: tuple[tuple[Context | None, tuple[str, ...], str], ...] = (
    ("testset", ("files", "*", "size"), "size"),
    ("testset", ("files", "*", "sha256"), "sha256"),
    ("testset", ("generator", "commit"), "commit"),
    ("hostile", ("expected", "regions", "*", "bbox", "*"), "bbox"),
    (None, ("schema",), "schema_id"),
)


@lru_cache(maxsize=1)
def current_commit() -> str:
    """The checkout's commit: ``MHAI_GIT_COMMIT`` or ``GITHUB_SHA`` if set (40 lowercase hex),
    else ``git rev-parse HEAD`` next to this file, else ``unknown``."""
    for var in ("MHAI_GIT_COMMIT", "GITHUB_SHA"):
        value = os.environ.get(var, "")
        if _COMMIT_HEX.fullmatch(value):
            return value
    try:
        res = subprocess.run(  # noqa: S603 - fixed argv, no shell, no input
            ["git", "rev-parse", "HEAD"],
            cwd=Path(__file__).parent,
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return "unknown"
    out = res.stdout.strip()
    return out if res.returncode == 0 and _COMMIT_HEX.fullmatch(out) else "unknown"


def _path_matches(raw: RawPath, pattern: tuple[str, ...]) -> bool:
    if len(raw) != len(pattern):
        return False
    for part, want in zip(raw, pattern, strict=True):
        if want == "*":
            if isinstance(part, bool) or not isinstance(part, int):
                return False
        elif part != want:
            return False
    return True


def _computed_leaf(raw: RawPath, leaf: Any, context: Context | None) -> bool:
    if isinstance(leaf, bool):
        return False
    kind = next(
        (k for ctx, pat, k in _COMPUTED if ctx in (None, context) and _path_matches(raw, pat)),
        None,
    )
    if kind == "sha256":
        return isinstance(leaf, str) and _SHA256_HEX.fullmatch(leaf) is not None
    if kind == "schema_id":
        return isinstance(leaf, str) and leaf in SCHEMA_IDS
    if kind == "commit":
        return (
            isinstance(leaf, str) and leaf == current_commit() and bool(_COMMIT_HEX.fullmatch(leaf))
        )
    if kind == "size":
        return isinstance(leaf, int) and 0 <= leaf <= _MAX_SIZE
    if kind == "bbox":
        # At most 4 fractional digits, so a coordinate cannot carry a long digit run.
        return (
            isinstance(leaf, int | float)
            and math.isfinite(leaf)
            and 0 <= leaf <= 1
            and raw[-1] in (0, 1, 2, 3)
            and _BBOX_NUMBER.fullmatch(repr(leaf)) is not None
        )
    return False


def check_metadata(meta: Any, *, context: Context | None = None) -> list[Finding]:
    """Check intake metadata (any JSON). Known keys use their list or pattern.

    Every key and every string or number leaf goes through the text rules, except a
    computed value in its own kind of file (``context``, which ``scan_set`` sets from the
    file's place and ``schema``), at its own path and in its own shape (see ``_COMPUTED``).
    """
    if context is not None and not (
        isinstance(meta, dict)
        and meta.get("schema") == (TESTSET_SCHEMA_ID if context == "testset" else HOSTILE_SCHEMA_ID)
    ):
        context = None
    found: dict[tuple[str, str], Finding] = {}
    for path, key, leaf, is_key, raw in _metadata_leaves(meta):
        rules: list[str]
        if is_key:
            rules = [f"key_{r}" for r in _text_rules(str(leaf))]
        elif isinstance(leaf, bool) or leaf is None or _computed_leaf(raw, leaf, context):
            continue
        elif isinstance(leaf, int | float):
            rules = _digit_rules(_normalise(str(leaf)), False)
        elif isinstance(leaf, str):
            rules = _text_rules(leaf)
            kind = _META_KEYS.get(key or "")
            if kind == "mrn" and not MRN_RE.fullmatch(leaf):
                rules.append("mrn_pattern")
            elif kind in ("family", "given") and not _is_name(leaf, kind):
                rules.append("name")
            elif kind == "facilities" and leaf not in terminology.name_set("facilities"):
                rules.append("facility")
            elif kind == "clinicians" and not _is_clinician(leaf):
                rules.append("clinician")
        else:
            continue
        for rule in rules:
            found[(path, rule)] = Finding(path, rule)
    return list(found.values())


def _no_constant(_name: str) -> Any:
    raise ValueError("non-finite number")


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("duplicate key")
        out[key] = value
    return out


@dataclass(frozen=True)
class _Listed:
    label: str  # display path of the entry, e.g. ``testset.json#files[3]``
    base: str  # the listing file's folder, relative to the scan root
    entry: Any


def _check_json_text(
    text: str, rel: str, jsonl: bool, referenced: set[str], listed: list[_Listed] | None = None
) -> list[Finding]:
    out: list[Finding] = []
    lines = text.splitlines() if jsonl else [text]
    for number, line in enumerate(lines, start=1):
        label = f"{rel}:{number}" if jsonl else rel
        if jsonl and not line.strip():
            continue
        try:
            obj = json.loads(line, parse_constant=_no_constant, object_pairs_hook=_no_duplicates)
        except (ValueError, RecursionError):
            out.append(Finding(label, "unreadable"))
            continue
        parts = Path(rel).parts
        # A ground-truth document: it says so, it looks like one, or it is under truth/
        # (which then must validate as one: a file there without schema_version is refused).
        is_doc = (
            isinstance(obj, dict) and bool({"schema_version", "patient", "document"} & obj.keys())
        ) or ("truth" in parts[:-1])
        if is_doc:
            sub = check_document(obj)
        else:
            context: Context | None = None
            if not jsonl and parts == ("testset.json",):
                context = "testset"
            elif not jsonl and len(parts) == 2 and parts[0] == "hostile":
                context = "hostile"
            sub = check_metadata(obj, context=context)
            files = obj.get("files") if isinstance(obj, dict) else None
            if listed is not None and context == "testset" and isinstance(files, list):
                base = str(Path(rel).parent)
                listed.extend(_Listed(f"{label}#files[{i}]", base, e) for i, e in enumerate(files))
            # A name in a listing refers to a file relative to the listing's folder, by
            # its path: a bare name never vouches for an image in another folder.
            folder = posixpath.dirname(rel)
            for _, _key, leaf, is_key, _raw in _metadata_leaves(obj):
                if not is_key and isinstance(leaf, str):
                    referenced.add(posixpath.normpath(posixpath.join(folder, leaf)))
        out.extend(Finding(f"{label}#{f.path}", f.rule) for f in sub)
    return out


def _check_listed(listed: list[_Listed], by_rel: dict[str, Path]) -> list[Finding]:
    """Every ``files[*]`` entry with a ``size`` or ``sha256`` must match the file it names.

    The scanner exempts those values from the digit rules only because it recomputes them
    here: a listed size or hash that is not the named file's own is refused.
    """
    out: list[Finding] = []
    for item in listed:
        e = item.entry
        if not isinstance(e, dict) or not ("size" in e or "sha256" in e):
            continue
        name = e.get("path")
        full = None
        if isinstance(name, str) and not Path(name).is_absolute() and ".." not in Path(name).parts:
            rel = str(Path(item.base) / name) if item.base not in ("", ".") else name
            full = by_rel.get(str(Path(rel)))
        if full is None:
            out.append(Finding(item.label, "listed_file_missing"))
            continue
        try:
            st = os.lstat(full)
            if not stat.S_ISREG(st.st_mode):  # never follow a link or open a pipe
                out.append(Finding(item.label, "unexpected_file"))
                continue
            if st.st_size > MAX_FILE_BYTES:
                out.append(Finding(item.label, "too_large"))
                continue
            data = full.read_bytes()
        except OSError:
            out.append(Finding(item.label, "unreadable"))
            continue
        if "size" in e and (isinstance(e["size"], bool) or e["size"] != len(data)):
            out.append(Finding(item.label, "listed_size_mismatch"))
        if "sha256" in e and e["sha256"] != hashlib.sha256(data).hexdigest():
            out.append(Finding(item.label, "listed_hash_mismatch"))
    return out


def _safe_rel(rel: str, index: int) -> tuple[str, bool]:
    """A displayable path: any component that is odd or looks like a real value is replaced."""
    parts: list[str] = []
    clean = True
    for part in Path(rel).parts:
        if _SAFE_NAME.fullmatch(part) and not _text_rules(part):
            parts.append(part)
        else:
            parts.append(f"[name{index}]")
            clean = False
    return "/".join(parts), clean


def scan_set(path: str | Path) -> ScanResult:
    """Scan a directory (or one file) of ground truth and intake metadata.

    ``.json`` holding a ``schema_version`` is a document; any other JSON is
    metadata. Images are not read (their ground truth is) but each needs a
    sibling ``.json`` of the same stem or a metadata record naming it, else
    ``orphan_image``. Symlinks, special files, other file types, odd or
    value-like file names, duplicate JSON keys, unreadable or oversized files,
    and an empty set are refused.
    """
    root = Path(path)
    findings: list[Finding] = []
    scanned = 0
    if root.is_symlink() or not root.exists():
        shown, _ = _safe_rel(root.name, 0)
        return ScanResult(False, (Finding(shown or ".", "unreadable"),), 0)
    entries: list[tuple[Path, str]] = []
    if root.is_file():
        entries.append((root, root.name))
    else:
        for base, dirs, names in os.walk(root, followlinks=False):
            dirs.sort()
            for name in sorted(names):
                full = Path(base) / name
                entries.append((full, str(full.relative_to(root))))
            for d in dirs:
                if (Path(base) / d).is_symlink():
                    entries.append((Path(base) / d, str((Path(base) / d).relative_to(root))))
    referenced: set[str] = set()
    listed: list[_Listed] = []
    images: list[tuple[Path, str, str]] = []
    json_files: set[Path] = set()
    for index, (full, rel) in enumerate(entries):
        shown, clean = _safe_rel(rel, index)
        if not clean:
            findings.append(Finding(shown, "filename"))
        try:
            mode = os.lstat(full).st_mode
        except OSError:
            findings.append(Finding(shown, "unreadable"))
            continue
        if stat.S_ISLNK(mode):
            findings.append(Finding(shown, "symlink"))
            continue
        if not stat.S_ISREG(mode):
            findings.append(Finding(shown, "unexpected_file"))
            continue
        suffix = full.suffix.lower()
        if suffix in IMAGE_SUFFIXES:
            images.append((full, shown, rel))
            continue
        if suffix not in JSON_SUFFIXES:
            findings.append(Finding(shown, "unexpected_file"))
            continue
        try:
            if os.lstat(full).st_size > MAX_FILE_BYTES:
                findings.append(Finding(shown, "too_large"))
                continue
            text = full.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            findings.append(Finding(shown, "unreadable"))
            continue
        scanned += 1
        if suffix == ".json":
            json_files.add(full)
        findings.extend(_check_json_text(text, shown, suffix == ".jsonl", referenced, listed))
    by_rel = {rel: full for full, rel in entries}
    findings.extend(_check_listed(listed, by_rel))
    for full, shown, rel in images:
        sibling = full.with_suffix(".json")
        if sibling in json_files or posixpath.normpath(rel) in referenced:
            continue
        findings.append(Finding(shown, "orphan_image"))
    if scanned == 0 and not findings:
        shown, _ = _safe_rel(root.name, 0)
        findings.append(Finding(shown or ".", "empty"))
    return ScanResult(not findings, tuple(findings), scanned)
