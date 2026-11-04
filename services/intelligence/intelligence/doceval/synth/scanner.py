"""The synthetic-data scanner (P20): nothing real may enter a synthetic set.

Fails closed. Every identifier, name, street, facility and clinician in a
ground-truth document, and every string in intake metadata, must match the
synthetic patterns or the committed lists. Anything that looks like a real
identifier is refused: a Nigerian phone, a national id or BVN (11 digits), a
real-format MRN, an email, a URL, a street address that isn't in the list.

A finding carries the location (a field path), the reason ``not_synthetic`` and
a fixed rule name. It never carries the offending value.
"""

import json
import os
import re
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval import schema
from intelligence.doceval.synth import terminology

NOT_SYNTHETIC = "not_synthetic"
MAX_FILE_BYTES = 64 * 1024 * 1024
IMAGE_SUFFIXES = frozenset({".png", ".jpg", ".jpeg"})
JSON_SUFFIXES = frozenset({".json", ".jsonl"})

MRN_RE = re.compile(r"SYN-[0-9]{8}")
OTHER_ID_RE = re.compile(r"SYN(?:ID|INS)-[0-9]{8}")
_ALLOWED_TOKEN = re.compile(r"\bSYN(?:ID|INS)?-[0-9]{8}\b")
_ALLOWED_PHONE = re.compile(r"(?<![0-9])555-01[0-9]{2}(?![0-9])")
_ISO = re.compile(
    r"(?<![0-9])[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[Tt ][0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]+)?)?"
    r"(?:[Zz]|[+-][0-9]{2}:[0-9]{2})?)?(?![0-9])"
)
_NG_PHONE = re.compile(
    r"(?<![0-9])(?:\+?234|0)[\s.-]?[789][01][0-9][\s.-]?[0-9]{3}[\s.-]?[0-9]{4}(?![0-9])"
)
_DIGITS = re.compile(r"(?<![0-9])[0-9](?:[ .()-]?[0-9]){6,}(?![0-9])")
_EMAIL = re.compile(r"@")
_URL = re.compile(r"(?:https?://|www\.|ftp://)", re.IGNORECASE)
_MRN_WORD = re.compile(
    r"\b(?:MRN|HOSP(?:ITAL)?\s*(?:NO|NUMBER)|PATIENT\s*ID|FOLDER\s*(?:NO|NUMBER))\b[\s:#.-]*\S",
    re.IGNORECASE,
)
_PREFIXED_ID = re.compile(r"\b[A-Z]{2,6}[-/ ]?[0-9]{5,}\b")
_TITLED = re.compile(
    r"\b(?:Dr|Mr|Mrs|Ms|Miss|Prof|Nurse|Sister)\b\.?\s+((?:[A-Z][A-Za-z'-]*)(?:\s+[A-Z][A-Za-z'-]*)*)"
)
_ADDRESS = re.compile(
    r"\b[0-9]{1,5}[A-Za-z]?\s+((?:[A-Z][a-z]+\s+){1,3}"
    r"(?:Street|St|Road|Rd|Avenue|Ave|Lane|Close|Crescent|Drive|Way|Estate|Layout|Court|Place))\b"
)
_CONTACT = re.compile(r"Tel 555-01[0-9]{2}; [0-9]{1,5} (.+)")


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


def _text_rules(text: str) -> list[str]:
    """Rule names that fire on free text. Synthetic tokens, ISO dates and times are removed first."""
    rules: list[str] = []
    if _EMAIL.search(text):
        rules.append("email")
    if _URL.search(text):
        rules.append("url")
    rest = _ALLOWED_TOKEN.sub(" ", text)
    rest = _ISO.sub(" ", rest)
    rest = _ALLOWED_PHONE.sub(" ", rest)
    if _NG_PHONE.search(rest):
        rules.append("phone")
    for m in _DIGITS.finditer(rest):
        count = sum(c.isdigit() for c in m.group(0))
        rules.append("national_id_like" if count == 11 else "long_digits")
    if _MRN_WORD.search(rest) or _PREFIXED_ID.search(rest):
        rules.append("mrn_like")
    tokens = terminology.all_name_tokens()
    for m in _TITLED.finditer(rest):
        if any(w not in tokens for w in m.group(1).split()):
            rules.append("titled_name")
            break
    streets = terminology.name_set("streets")
    for m in _ADDRESS.finditer(rest):
        if m.group(1) not in streets:
            rules.append("address")
            break
    return sorted(set(rules))


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
    if pattern == "notes[].text" and text.startswith("Tel "):
        return None if _is_contact(text) else "contact"
    return None


def _walk(node: Any, path: str) -> Iterator[tuple[str, Any]]:
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


def check_document(gt: Any) -> list[Finding]:
    """Check one ground-truth document. An empty list means every value is synthetic."""
    if not isinstance(gt, dict):
        return [Finding("", "schema")]
    try:
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
        if not isinstance(leaf, str):
            continue
        rules = _text_rules(leaf)
        pattern = _field_pattern(path)
        if pattern is not None:
            typed = _typed_rule(pattern, leaf)
            if typed is not None:
                rules.append(typed)
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


def check_metadata(meta: Any) -> list[Finding]:
    """Check intake metadata (any JSON). Known keys use their list or pattern."""
    found: dict[tuple[str, str], Finding] = {}
    for path, leaf in _walk(meta, ""):
        if not isinstance(leaf, str):
            continue
        rules = _text_rules(leaf)
        key = re.sub(r"\[[0-9]+\]", "", path).rsplit(".", 1)[-1]
        kind = _META_KEYS.get(key)
        if kind == "mrn" and not MRN_RE.fullmatch(leaf):
            rules.append("mrn_pattern")
        elif kind in ("family", "given") and not _is_name(leaf, kind):
            rules.append("name")
        elif kind == "facilities" and leaf not in terminology.name_set("facilities"):
            rules.append("facility")
        elif kind == "clinicians" and not _is_clinician(leaf):
            rules.append("clinician")
        for rule in rules:
            found[(path, rule)] = Finding(path, rule)
    return list(found.values())


def _no_constant(_name: str) -> Any:
    raise ValueError("non-finite number")


def _check_json_text(text: str, rel: str, jsonl: bool) -> list[Finding]:
    out: list[Finding] = []
    lines = text.splitlines() if jsonl else [text]
    for number, line in enumerate(lines, start=1):
        label = f"{rel}:{number}" if jsonl else rel
        if jsonl and not line.strip():
            continue
        try:
            obj = json.loads(line, parse_constant=_no_constant)
        except (ValueError, RecursionError):
            out.append(Finding(label, "unreadable"))
            continue
        is_doc = isinstance(obj, dict) and "schema_version" in obj
        sub = check_document(obj) if is_doc else check_metadata(obj)
        out.extend(Finding(f"{label}#{f.path}", f.rule) for f in sub)
    return out


def scan_set(path: str | Path) -> ScanResult:
    """Scan a directory (or one file) of ground truth and intake metadata.

    ``.json`` holding a ``schema_version`` is a document; any other JSON is
    metadata. Images are not read (their ground truth is). Symlinks, other file types,
    unreadable or oversized files, and an empty set are refused.
    """
    root = Path(path)
    findings: list[Finding] = []
    scanned = 0
    if root.is_symlink() or not root.exists():
        return ScanResult(False, (Finding(root.name, "unreadable"),), 0)
    if root.is_file():
        files = [(root, root.name)]
    else:
        files = []
        for base, dirs, names in os.walk(root, followlinks=False):
            dirs.sort()
            for name in sorted(names):
                full = Path(base) / name
                files.append((full, str(full.relative_to(root))))
            for d in dirs:
                if (Path(base) / d).is_symlink():
                    findings.append(Finding(str((Path(base) / d).relative_to(root)), "symlink"))
    for full, rel in files:
        suffix = full.suffix.lower()
        if full.is_symlink():
            findings.append(Finding(rel, "symlink"))
            continue
        if suffix in IMAGE_SUFFIXES:
            continue
        if suffix not in JSON_SUFFIXES:
            findings.append(Finding(rel, "unexpected_file"))
            continue
        try:
            if full.stat().st_size > MAX_FILE_BYTES:
                findings.append(Finding(rel, "too_large"))
                continue
            text = full.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            findings.append(Finding(rel, "unreadable"))
            continue
        scanned += 1
        findings.extend(_check_json_text(text, rel, suffix == ".jsonl"))
    if scanned == 0 and not findings:
        findings.append(Finding(root.name, "empty"))
    return ScanResult(not findings, tuple(findings), scanned)
