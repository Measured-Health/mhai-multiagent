"""The clinical answer question set: one item per line, and a set folder.

An item (``mhai.answers.item/1``, one canonical JSON object per line of ``items.jsonl``)::

    {"category": "<a QueryCategory value>", "chunks": [{"id": "KB#0006", "text": "..."}],
     "expect": {"forbidden": [...], "must_escalate": false, "required_facts": [...]},
     "id": "ca-0001", "kind": "<KINDS>", "question": "..."}

* ``kind``: ``required_facts`` and ``must_not_say`` are answerable from the chunks;
  ``red_flag``, ``out_of_scope`` and ``missing_info`` must escalate. ``must_escalate``
  must agree with the kind; a must-escalate item has no required facts, a
  ``required_facts`` item has at least one, a ``must_not_say`` item at least one
  forbidden phrase.
* ``required_facts`` and ``forbidden`` are short phrases, already in :func:`normalise`
  form; an answer contains one when :func:`contains` finds it as whole tokens.
* Synthetic only: every string passes the synthetic-data scanner's text rules, and no
  string holds a run of six or more digits. At most :data:`MAX_CHUNKS` chunks, with ids
  ``KB#<4 digits>``; no unknown key anywhere.

A set folder holds ``items.jsonl`` and ``testset.json`` (the document sets' schema,
``mhai.evaluation.testset/1``, named ``clinical_answer``, with the item schema as its
``extraction_schema_version``, the per-kind counts as ``slices``, and ``sign_off``:
``pilot, unsigned``: the set has no clinical sign-off yet, README "Pilot mode"). The
locked split (``modelstore.testsets``) is the same folder plus ``locked`` and
``store_version_ids``. Errors are :class:`ItemInvalid` with a fixed code and never a value.
"""

from __future__ import annotations

import hashlib
import os
import re
import unicodedata
from collections import Counter
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval.synth import scanner
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

SET_NAME = "clinical_answer"
TESTSET_SCHEMA = "mhai.evaluation.testset/1"  # doceval.sets.TESTSET_SCHEMA
ITEM_SCHEMA = "mhai.answers.item/1"
ITEMS_FILE = "items.jsonl"
SIGN_OFF = "pilot, unsigned"
# intelligence.agents.orchestrator.QueryCategory values (pinned by a test).
CATEGORIES: tuple[str, ...] = (
    "medication_dosage",
    "diagnosis",
    "lab_interpretation",
    "treatment_plan",
    "imaging_review",
    "lifestyle_advice",
    "general_info",
    "emergency",
)
KINDS: tuple[str, ...] = (
    "required_facts",
    "must_not_say",
    "red_flag",
    "out_of_scope",
    "missing_info",
)
ESCALATE_KINDS = frozenset({"red_flag", "out_of_scope", "missing_info"})
MAX_CHUNKS = 20
MAX_QUESTION_CHARS = 1000
MAX_CHUNK_CHARS = 2000
MAX_PHRASES = 20
MAX_PHRASE_CHARS = 120
MAX_ITEMS = 5000
MAX_LINE_BYTES = 64 * 1024
MAX_FILE_BYTES = 32 * 1024 * 1024
MAX_TESTSET_BYTES = 1024 * 1024

_ID = re.compile(r"[a-z0-9][a-z0-9-]{0,31}", re.ASCII)
_CHUNK_ID = re.compile(r"KB#[0-9]{4}", re.ASCII)
_SIX_DIGITS = re.compile(r"[0-9]{6}", re.ASCII)
_UNIT = re.compile(r"\b([0-9]+(?:\.[0-9]+)?)([a-z]+)\b")
_SPACE = re.compile(r"\s+")
_ITEM_KEYS = frozenset({"id", "kind", "category", "question", "chunks", "expect"})
_EXPECT_KEYS = frozenset({"must_escalate", "required_facts", "forbidden"})
_CHUNK_KEYS = frozenset({"id", "text"})


class ItemInvalid(ValueError):
    """An item or set that does not fit the format. ``str()`` is a fixed code."""


@dataclass(frozen=True, slots=True)
class Chunk:
    id: str
    text: str


@dataclass(frozen=True, slots=True)
class Expect:
    must_escalate: bool
    required_facts: tuple[str, ...] = ()
    forbidden: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class Item:
    id: str
    kind: str
    category: str
    question: str
    chunks: tuple[Chunk, ...]
    expect: Expect


def normalise(text: str) -> str:
    """Lower case, NFKC, ``500mg`` -> ``500 mg`` (a number glued to a unit; a digit inside a
    word such as ``hba1c`` stays), whitespace collapsed."""
    t = unicodedata.normalize("NFKC", text).lower()
    t = t.replace("≥", ">=").replace("≤", "<=")
    t = _UNIT.sub(r"\1 \2", t)
    return _SPACE.sub(" ", t).strip()


def contains(normalised: str, phrase: str) -> bool:
    """Whether ``phrase`` occurs in ``normalised`` as whole tokens: no letter or digit
    right before or after it."""
    start = 0
    while (i := normalised.find(phrase, start)) >= 0:
        end = i + len(phrase)
        before = normalised[i - 1] if i > 0 else " "
        after = normalised[end] if end < len(normalised) else " "
        if not (phrase[0].isalnum() and before.isalnum()) and not (
            phrase[-1].isalnum() and after.isalnum()
        ):
            return True
        start = i + 1
    return False


def _bad(code: str) -> ItemInvalid:
    return ItemInvalid(code)


def _text(value: Any, cap: int, code: str) -> str:
    if not isinstance(value, str) or not value.strip() or len(value) > cap:
        raise _bad(code)
    if not value.isascii() or _SIX_DIGITS.search(value) or not scanner.text_ok(value):
        raise _bad("not_synthetic")
    return value


def _phrases(value: Any) -> tuple[str, ...]:
    if not isinstance(value, list) or len(value) > MAX_PHRASES:
        raise _bad("expect")
    out = tuple(_text(p, MAX_PHRASE_CHARS, "phrase") for p in value)
    if any(p != normalise(p) for p in out) or len(set(out)) != len(out):
        raise _bad("phrase")
    return out


def parse_item(obj: Any) -> Item:
    """One item, checked (see the module docstring)."""
    if not isinstance(obj, dict) or set(obj) != _ITEM_KEYS:
        raise _bad("keys")
    if not isinstance(obj["id"], str) or _ID.fullmatch(obj["id"]) is None:
        raise _bad("id")
    if obj["kind"] not in KINDS:
        raise _bad("kind")
    if obj["category"] not in CATEGORIES:
        raise _bad("category")
    question = _text(obj["question"], MAX_QUESTION_CHARS, "question")
    chunks_in = obj["chunks"]
    if not isinstance(chunks_in, list) or not 1 <= len(chunks_in) <= MAX_CHUNKS:
        raise _bad("chunks")
    chunks: list[Chunk] = []
    for c in chunks_in:
        if not isinstance(c, dict) or set(c) != _CHUNK_KEYS:
            raise _bad("chunk")
        if not isinstance(c["id"], str) or _CHUNK_ID.fullmatch(c["id"]) is None:
            raise _bad("chunk_id")
        chunks.append(Chunk(c["id"], _text(c["text"], MAX_CHUNK_CHARS, "chunk")))
    if len({c.id for c in chunks}) != len(chunks):
        raise _bad("chunk_id")
    ex = obj["expect"]
    if not isinstance(ex, dict) or set(ex) != _EXPECT_KEYS:
        raise _bad("expect")
    escalate = ex["must_escalate"]
    if not isinstance(escalate, bool):
        raise _bad("expect")
    facts, forbidden = _phrases(ex["required_facts"]), _phrases(ex["forbidden"])
    kind = obj["kind"]
    consistent = (
        escalate is (kind in ESCALATE_KINDS)
        and (not escalate or not facts)
        and (kind != "required_facts" or bool(facts))
        and (kind != "must_not_say" or bool(forbidden))
    )
    if not consistent:
        raise _bad("expect")
    return Item(
        obj["id"],
        kind,
        obj["category"],
        question,
        tuple(chunks),
        Expect(escalate, facts, forbidden),
    )


def item_obj(item: Item) -> dict[str, Any]:
    return {
        "id": item.id,
        "kind": item.kind,
        "category": item.category,
        "question": item.question,
        "chunks": [{"id": c.id, "text": c.text} for c in item.chunks],
        "expect": {
            "must_escalate": item.expect.must_escalate,
            "required_facts": list(item.expect.required_facts),
            "forbidden": list(item.expect.forbidden),
        },
    }


def dumps_items(items: Iterable[Item]) -> bytes:
    """``items.jsonl``: each item as canonical JSON on its own line."""
    return b"".join(canonical.dumps(item_obj(i)) + b"\n" for i in items)


def parse_items(raw: bytes) -> tuple[Item, ...]:
    """Every line of ``items.jsonl``: canonical JSON, a valid item, unique ids."""
    if not isinstance(raw, bytes) or len(raw) > MAX_FILE_BYTES:
        raise _bad("too_large")
    if not raw:
        raise _bad("empty")
    if not raw.endswith(b"\n"):
        raise _bad("json")
    lines = raw[:-1].split(b"\n")
    if len(lines) > MAX_ITEMS:
        raise _bad("too_many")
    out: list[Item] = []
    for line in lines:
        failed = len(line) > MAX_LINE_BYTES
        obj: Any = None
        if not failed:
            try:
                obj = canonical.loads_strict(line, MAX_LINE_BYTES)
                failed = canonical.dumps(obj) != line
            except Refused:
                failed = True
        if failed:
            raise _bad("json")
        out.append(parse_item(obj))
    if len({i.id for i in out}) != len(out):
        raise _bad("duplicate")
    return tuple(out)


# ---------------------------------------------------------------- set folders


@dataclass(frozen=True)
class SetBuilt:
    root: Path
    sha256: str  # of testset.json
    items: int
    must_escalate: int


@dataclass(frozen=True)
class AnswerSet:
    meta: dict[str, Any]
    items: tuple[Item, ...]


def testset_manifest(
    items: Sequence[Item], purpose: str, version: str, raw: bytes
) -> dict[str, Any]:
    kinds = Counter(i.kind for i in items)
    return {
        "schema": TESTSET_SCHEMA,
        "name": SET_NAME,
        "version": version,
        "purpose": purpose,
        "extraction_schema_version": ITEM_SCHEMA,
        "sign_off": SIGN_OFF,
        "slices": {k: {"items": kinds[k]} for k in KINDS},
        "totals": {
            "items": len(items),
            "must_escalate": sum(i.expect.must_escalate for i in items),
        },
        "files": [
            {"path": ITEMS_FILE, "size": len(raw), "sha256": hashlib.sha256(raw).hexdigest()}
        ],
    }


def write_set(folder: Path, items: Sequence[Item], purpose: str, version: str) -> SetBuilt:
    """Write ``items.jsonl`` and then ``testset.json`` into the existing ``folder``."""
    raw = dumps_items(items)
    parse_items(raw)  # what is written must read back
    meta = canonical.dumps(testset_manifest(items, purpose, version, raw))
    for name, body in ((ITEMS_FILE, raw), ("testset.json", meta)):
        fd = os.open(folder / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as fh:
            fh.write(body)
    return SetBuilt(
        folder,
        hashlib.sha256(meta).hexdigest(),
        len(items),
        sum(i.expect.must_escalate for i in items),
    )


def _read(path: Path, cap: int, code: str) -> bytes:
    failed = False
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as fh:
            data = fh.read(cap + 1)
    except OSError:
        failed = True
    if failed or len(data) > cap:
        raise _bad(code)
    return data


def load_set(set_dir: Path | str) -> AnswerSet:
    """A set folder, checked: ``testset.json`` names this suite and lists ``items.jsonl``
    with its size and SHA-256 (``items_hash``), and every item parses. ``meta`` has what a
    report may say: name, version, purpose, items, must_escalate, item_schema, sign_off,
    the SHA-256 of ``testset.json``, and whether it is a locked split."""
    root = Path(set_dir)
    raw = _read(root / "testset.json", MAX_TESTSET_BYTES, "testset")
    failed = False
    try:
        obj = canonical.loads_strict(raw, MAX_TESTSET_BYTES)
        failed = canonical.dumps(obj) != raw
    except Refused:
        failed = True
    if failed or not isinstance(obj, dict):
        raise _bad("testset")
    files = obj.get("files")
    if (
        obj.get("schema") != TESTSET_SCHEMA
        or obj.get("name") != SET_NAME
        or obj.get("extraction_schema_version") != ITEM_SCHEMA
        or obj.get("purpose") not in ("dev", "test")
        or not isinstance(obj.get("version"), str)
        or not isinstance(files, list)
        or len(files) != 1
        or not isinstance(files[0], dict)
        or files[0].get("path") != ITEMS_FILE
    ):
        raise _bad("testset")
    data = _read(root / ITEMS_FILE, MAX_FILE_BYTES, "items")
    if len(data) != files[0].get("size") or hashlib.sha256(data).hexdigest() != files[0].get(
        "sha256"
    ):
        raise _bad("items_hash")
    items = parse_items(data)
    totals = obj.get("totals")
    escalate = sum(i.expect.must_escalate for i in items)
    if totals != {"items": len(items), "must_escalate": escalate}:
        raise _bad("testset")
    meta = {
        "name": SET_NAME,
        "version": obj["version"],
        "purpose": obj["purpose"],
        "sha256": hashlib.sha256(raw).hexdigest(),
        "items": len(items),
        "must_escalate": escalate,
        "item_schema": ITEM_SCHEMA,
        "sign_off": obj.get("sign_off"),
        "locked": "locked" in obj or "store_version_ids" in obj,
    }
    return AnswerSet(meta, items)


__all__ = [
    "CATEGORIES",
    "ESCALATE_KINDS",
    "ITEMS_FILE",
    "ITEM_SCHEMA",
    "KINDS",
    "SET_NAME",
    "SIGN_OFF",
    "AnswerSet",
    "Chunk",
    "Expect",
    "Item",
    "ItemInvalid",
    "SetBuilt",
    "contains",
    "dumps_items",
    "item_obj",
    "load_set",
    "normalise",
    "parse_item",
    "parse_items",
    "write_set",
]
