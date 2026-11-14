"""A reference library: a licence-clean shared corpus, built once per pinned
version and loaded into kb_chunks (migration 00040).

Library v1, ``medref``, is two sources only (the licence review):

- openFDA drug labels (``drug_label``; CC0): the safety-critical sections
  (boxed warning, contraindications, warnings, interactions, specific
  populations, adverse reactions) first, then dosage and indications.
- MedlinePlus health topics (``health_topic``; public domain), English only,
  labelled patient-facing.

Everything else in a source bundle is refused: every synthetic patient type,
question-answer references, abstracts, phenotype profiles, ontology terms,
trials and the placeholder guidelines.

Three steps, each its own command:

``convert`` (scripts/reflib_convert.py, an operator on a build machine)
    Streams a bundle's ``chunks.jsonl.gz`` line by line, keeps the approved
    rows (:func:`select`), orders them (safety-critical first) and numbers
    them ``KB#1000001`` up (00040's reference namespace), embeds them with
    OUR pinned embedder (``models/answer-gate-models.lock``, role dense) and
    writes the artefact.
``verify``
    The artefact's SHA-256 against ``models/reference-library.lock``.
``load`` (scripts/reflib_load.py: database access stays out of this
package, tests/boundary/test_no_direct_db.py)
    One transaction with the migration DSN: the library row, the rows (COPY),
    the previous version of the same name retired and its rows deleted, the
    new one active. Loading the active version again changes nothing; a
    version already loaded with other bytes is refused.

The artefact is one gzip file (mtime 0, so the same input gives the same
bytes): a header line (:data:`SCHEMA`, name, version, licence, attribution,
embedder, dimension, count, the input's SHA-256, the retrieval date), then
one line per row: ``chunk_id``, ``content`` and ``metadata`` (kind, source,
title, url, licence, set_id or topic_id, the retrieval date, the
attribution) and ``embedding``. Rows carry no patient and no source fields:
00002's kb_shared_has_no_patient holds.

Nothing here logs row text.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import math
import os
import re
import stat
import sys
import unicodedata
import zipfile
from collections.abc import Callable, Iterable, Iterator, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import IO, Any

import numpy as np

SCHEMA = "mhai.reflib/1"
LOCK_SCHEMA = "mhai.reflib.lock/1"
NAME = "medref"
REPO = Path(__file__).resolve().parents[4]
LOCK_PATH = REPO / "models" / "reference-library.lock"
BUNDLE_MEMBER = "bundle/kb/chunks.jsonl.gz"

FIRST_ID = 1_000_001
LAST_ID = 1_999_999
CHUNK_ID = re.compile(r"KB#[1-9][0-9]{6}")
CHUNK_TYPE = "guideline"  # chunk_visible_to shows a shared guideline to everyone
MAX_LINE_BYTES = 1 << 20
MAX_CONTENT_CHARS = 4000
MAX_TITLE_CHARS = 300
MAX_DIM = 4096

FDA_NOTICE = "Data provided by the U.S. Food and Drug Administration (open.fda.gov)."
NLM_NOTICE = "Source: MedlinePlus, National Library of Medicine."
NO_WARRANTY = (
    "Provided as is, without warranty of any kind. FDA and NLM do not endorse MHAI. "
    "Not a substitute for the full prescribing information or clinical judgement."
)
ATTRIBUTION = f"{NLM_NOTICE} {FDA_NOTICE} {NO_WARRANTY}"
LICENCE = (
    "openFDA drug labels: CC0-1.0; MedlinePlus health topics: public domain (U.S. government work)"
)

# Drug label sections, safety-critical first: the order ids are given in.
SECTIONS: tuple[tuple[str, str, bool], ...] = (
    ("boxed_warning", "Boxed warning", True),
    ("contraindications", "Contraindications", True),
    ("warnings_and_cautions", "Warnings and precautions", True),
    ("warnings", "Warnings", True),
    ("drug_interactions", "Drug interactions", True),
    ("use_in_specific_populations", "Use in specific populations", True),
    ("adverse_reactions", "Adverse reactions", True),
    ("dosage_and_administration", "Dosage and administration", False),
    ("indications_and_usage", "Indications and usage", False),
)
_SECTION = {s: (i, label, safety) for i, (s, label, safety) in enumerate(SECTIONS)}

_SET_ID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_DAILYMED = "https://dailymed.nlm.nih.gov/dailymed/lookup.cfm?setid="
_MEDLINEPLUS = re.compile(r"https://medlineplus\.gov/([a-z0-9][a-z0-9_-]{0,120})\.html")
_TOPIC_ID = re.compile(r"[0-9]{1,9}")
_VERSION = re.compile(r"(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})")
_NAME = re.compile(r"[a-z0-9][a-z0-9-]{1,62}")
_SHA = re.compile(r"[0-9a-f]{64}")
# Marks of Spanish text, for a page that isn't under /spanish/ but is.
_SPANISH = ("¿", "¡")


# Sequences a row may never hold (security review): a prompt's block
# delimiters and a citation. Checked on the NFKC-folded, upper-cased text,
# with spaces removed, at convert and again at load.
FORGERY_MARKS = ("<<", ">>", "KB#")


def forged(text: Any) -> bool:
    """Whether text holds a delimiter or citation sequence (any case,
    spacing or full-width form)."""
    if not isinstance(text, str):
        return False
    folded = "".join(unicodedata.normalize("NFKC", text).upper().split())
    return any(m in folded for m in FORGERY_MARKS)


class LibraryError(Exception):
    """A bundle, artefact, lock or load the library refuses. The text is a
    reason code, never row content."""


@dataclass(frozen=True)
class Entry:
    """One approved row, before it has an id."""

    kind: str
    content: str = field(repr=False)
    metadata: Mapping[str, Any]
    order: tuple[Any, ...]


@dataclass(frozen=True)
class Header:
    name: str
    version: str
    licence: str
    attribution: str
    embedder: str
    dim: int
    count: int
    built_from: str
    retrieved: str

    def as_json(self) -> dict[str, Any]:
        return {"schema": SCHEMA, **self.__dict__}


@dataclass(frozen=True)
class Row:
    chunk_id: str
    content: str = field(repr=False)
    metadata: Mapping[str, Any]
    embedding: tuple[float, ...] = field(repr=False)


# --- convert ------------------------------------------------------------------------------


def _clean(text: Any, limit: int) -> str:
    """Text without control or format characters, whitespace collapsed; ""
    if not a string or too long."""
    if not isinstance(text, str):
        return ""
    out = "".join(" " if unicodedata.category(c) in ("Cc", "Cf", "Zl", "Zp") else c for c in text)
    out = " ".join(out.split())
    return out if len(out) <= limit else ""


def _drug_label(record: Mapping[str, Any], meta: Mapping[str, Any], retrieved: str) -> Entry | None:
    section = meta.get("section")
    set_id = meta.get("set_id")
    if section not in _SECTION or not isinstance(set_id, str) or not _SET_ID.fullmatch(set_id):
        return None
    if meta.get("url") != _DAILYMED + set_id or meta.get("source") != "openfda":
        return None
    drug = _clean(meta.get("drug"), MAX_TITLE_CHARS)
    content = _clean(record.get("content"), MAX_CONTENT_CHARS)
    if not drug or not content or forged(drug) or forged(content):
        return None
    rank, label, safety = _SECTION[section]
    return Entry(
        kind="drug_label",
        content=content,
        metadata={
            "kind": "drug_label",
            "source": "openFDA",
            "title": f"{drug} ({label})",
            "url": _DAILYMED + set_id,
            "licence": "CC0-1.0",
            "set_id": set_id,
            "section": section,
            "safety_critical": safety,
            "audience": "clinician",
            "retrieved": retrieved,
            "attribution": f"{FDA_NOTICE} {NO_WARRANTY}",
        },
        order=(0, rank, drug.lower(), set_id),
    )


def _health_topic(
    record: Mapping[str, Any], meta: Mapping[str, Any], retrieved: str
) -> Entry | None:
    url = meta.get("url")
    topic_id = record.get("source_subject_id")
    if not isinstance(url, str) or not _MEDLINEPLUS.fullmatch(url) or "/spanish/" in url:
        return None  # English only: Spanish pages live under /spanish/
    if meta.get("source") != "medlineplus":
        return None
    if not isinstance(topic_id, str) or not _TOPIC_ID.fullmatch(topic_id):
        return None
    title = _clean(meta.get("title"), MAX_TITLE_CHARS)
    text = _clean(record.get("content"), MAX_CONTENT_CHARS)
    part = meta.get("part")
    if not title or not text or any(m in text for m in _SPANISH):
        return None
    if forged(title) or forged(text):
        return None
    if type(part) is not int or not 0 <= part < 1000:
        return None
    return Entry(
        kind="health_topic",
        content=f"Patient-facing information (MedlinePlus): {text}",
        metadata={
            "kind": "health_topic",
            "source": "MedlinePlus",
            "title": title,
            "url": url,
            "licence": "public domain (U.S. government work)",
            "topic_id": topic_id,
            "part": part,
            "audience": "patient",
            "retrieved": retrieved,
            "attribution": f"{NLM_NOTICE} {NO_WARRANTY}",
        },
        order=(1, title.lower(), part, topic_id),
    )


def select(record: Any, retrieved: str) -> Entry | None:
    """The approved row a bundle record maps to, or None (refused). Only
    drug_label and English health_topic pass."""
    if not isinstance(record, dict):
        return None
    meta = record.get("metadata")
    if not isinstance(meta, dict):
        return None
    kind = record.get("chunk_type")
    if kind == "drug_label":
        return _drug_label(record, meta, retrieved)
    if kind == "health_topic":
        return _health_topic(record, meta, retrieved)
    return None


def read_bundle_lines(stream: IO[bytes]) -> Iterator[Any]:
    """JSON records from a gzip'd JSON Lines stream, one at a time. A line
    that is too long or not JSON is skipped (it can't be approved)."""
    with gzip.open(stream, "rb") as g:
        while True:
            line = g.readline(MAX_LINE_BYTES + 1)
            if not line:
                return
            if len(line) > MAX_LINE_BYTES:
                # Skip the rest of an over-long line.
                while line and not line.endswith(b"\n"):
                    line = g.readline(MAX_LINE_BYTES)
                continue
            try:
                yield json.loads(line)
            except ValueError:
                continue


def open_chunks(path: Path) -> IO[bytes]:
    """The chunks stream: the one member ``bundle/kb/chunks.jsonl.gz`` of a
    bundle zip (nothing else in it is opened), or a .jsonl.gz file."""
    if zipfile.is_zipfile(path):
        z = zipfile.ZipFile(path)
        try:
            return z.open(BUNDLE_MEMBER)
        except KeyError:
            raise LibraryError("no_chunks_member") from None
    return open(path, "rb")


def collect(records: Iterable[Any], retrieved: str) -> list[Entry]:
    """The approved entries, exact duplicates (same kind and text) once, in
    library order: drug labels by section (safety-critical first), then
    health topics."""
    seen: set[tuple[str, str]] = set()
    out: list[Entry] = []
    for record in records:
        e = select(record, retrieved)
        if e is None or (e.kind, e.content) in seen:
            continue
        seen.add((e.kind, e.content))
        out.append(e)
    out.sort(key=lambda e: e.order)
    if len(out) > LAST_ID - FIRST_ID + 1:
        raise LibraryError("too_many_rows")
    return out


Embedder = Callable[[Sequence[str]], np.ndarray]


def build(
    entries: Sequence[Entry],
    embed: Embedder,
    *,
    version: str,
    embedder: str,
    built_from: str,
    retrieved: str,
    batch: int = 256,
) -> tuple[Header, list[Row]]:
    """Number the entries and embed them: the artefact's header and rows."""
    if not _VERSION.fullmatch(version):
        raise LibraryError("bad_version")
    rows: list[Row] = []
    dim = 0
    for start in range(0, len(entries), batch):
        part = entries[start : start + batch]
        vectors = np.asarray(embed([e.content for e in part]), dtype=np.float32)
        if vectors.ndim != 2 or vectors.shape[0] != len(part) or not np.all(np.isfinite(vectors)):
            raise LibraryError("bad_embedding")
        if dim and vectors.shape[1] != dim:
            raise LibraryError("bad_embedding")
        dim = int(vectors.shape[1])
        for i, (e, v) in enumerate(zip(part, vectors, strict=True)):
            rows.append(
                Row(
                    chunk_id=f"KB#{FIRST_ID + start + i}",
                    content=e.content,
                    metadata=dict(e.metadata),
                    embedding=tuple(round(float(x), 6) for x in v),
                )
            )
    header = Header(
        name=NAME,
        version=version,
        licence=LICENCE,
        attribution=ATTRIBUTION,
        embedder=embedder,
        dim=dim,
        count=len(rows),
        built_from=built_from,
        retrieved=retrieved,
    )
    return header, rows


def _dumps(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()


def write_artefact(header: Header, rows: Sequence[Row], out_dir: Path) -> tuple[Path, str]:
    """Write ``<name>-<version>.jsonl.gz`` (deterministic bytes); returns the
    path and its SHA-256."""
    path = Path(out_dir) / f"{header.name}-{header.version}.jsonl.gz"
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as g:
        g.write(_dumps(header.as_json()) + b"\n")
        for r in rows:
            line = {
                "chunk_id": r.chunk_id,
                "content": r.content,
                "metadata": dict(r.metadata),
                "embedding": list(r.embedding),
            }
            g.write(_dumps(line) + b"\n")
    data = buf.getvalue()
    path.write_bytes(data)
    return path, hashlib.sha256(data).hexdigest()


# --- verify and read ----------------------------------------------------------------------


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        while block := f.read(1 << 20):
            digest.update(block)
    return digest.hexdigest()


@dataclass(frozen=True)
class Pin:
    version: str
    sha256: str
    count: int


def load_lock(path: Path | None = None) -> dict[str, Pin]:
    """models/reference-library.lock: each library's pinned version."""
    try:
        obj = json.loads(Path(path or LOCK_PATH).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise LibraryError("lock_invalid") from None
    libs = obj.get("libraries") if isinstance(obj, dict) else None
    if not isinstance(obj, dict) or obj.get("schema") != LOCK_SCHEMA or not isinstance(libs, dict):
        raise LibraryError("lock_invalid")
    out: dict[str, Pin] = {}
    for name, p in libs.items():
        ok = (
            isinstance(name, str)
            and _NAME.fullmatch(name)
            and isinstance(p, dict)
            and set(p) == {"version", "sha256", "count"}
            and isinstance(p["version"], str)
            and _VERSION.fullmatch(p["version"])
            and isinstance(p["sha256"], str)
            and _SHA.fullmatch(p["sha256"])
            and type(p["count"]) is int
            and p["count"] >= 0
        )
        if not ok:
            raise LibraryError("lock_invalid")
        out[name] = Pin(p["version"], p["sha256"], p["count"])
    return out


MAX_ARTEFACT_BYTES = 1 << 30


def _read_once(path: Path) -> bytes:
    """The file's bytes, read once from one descriptor opened without
    following a link (security review: no check-then-reopen)."""
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise LibraryError("unreadable") from None
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise LibraryError("not_a_file")
        parts: list[bytes] = []
        total = 0
        while block := os.read(fd, 1 << 20):
            total += len(block)
            if total > MAX_ARTEFACT_BYTES:
                raise LibraryError("too_large")
            parts.append(block)
    except OSError:
        raise LibraryError("unreadable") from None
    finally:
        os.close(fd)
    return b"".join(parts)


def check_pin(header: Header, sha: str, pins: Mapping[str, Pin]) -> None:
    """The header and digest are the ones a lock pins, or LibraryError."""
    pin = pins.get(header.name)
    if pin is None or pin.version != header.version:
        raise LibraryError("not_pinned")
    if sha != pin.sha256:
        raise LibraryError("hash_mismatch")
    if pin.count != header.count:
        raise LibraryError("count_mismatch")


def read_pinned(path: Path, pins: Mapping[str, Pin]) -> tuple[Header, str, bytes]:
    """The artefact's header, digest and bytes, once those bytes are the ones
    a lock pins. Parse these bytes (read_rows(data, header)), never the file
    again: it may have changed since."""
    data = _read_once(path)
    sha = hashlib.sha256(data).hexdigest()
    header = read_header(data)
    check_pin(header, sha, pins)
    return header, sha, data


def verify(path: Path, pins: Mapping[str, Pin]) -> tuple[Header, str]:
    """The artefact's header and digest, once its bytes are the ones a lock pins."""
    header, sha, _ = read_pinned(path, pins)
    return header, sha


def _header(obj: Any) -> Header:
    keys = {"schema", *Header.__dataclass_fields__}
    if not isinstance(obj, dict) or set(obj) != keys or obj["schema"] != SCHEMA:
        raise LibraryError("bad_header")
    ok = (
        isinstance(obj["name"], str)
        and _NAME.fullmatch(obj["name"])
        and isinstance(obj["version"], str)
        and _VERSION.fullmatch(obj["version"])
        and all(isinstance(obj[k], str) and obj[k] for k in ("licence", "attribution", "embedder"))
        and len(obj["licence"]) <= 512
        and len(obj["attribution"]) <= 2048
        and len(obj["embedder"]) <= 200
        and type(obj["dim"]) is int
        and 1 <= obj["dim"] <= MAX_DIM
        and type(obj["count"]) is int
        and 0 <= obj["count"] <= LAST_ID - FIRST_ID + 1
        and isinstance(obj["built_from"], str)
        and isinstance(obj["retrieved"], str)
    )
    if not ok:
        raise LibraryError("bad_header")
    return Header(**{k: obj[k] for k in Header.__dataclass_fields__})


def _gunzip(source: Path | bytes) -> Any:
    return gzip.open(io.BytesIO(source) if isinstance(source, bytes) else source, "rb")


def read_header(source: Path | bytes) -> Header:
    with _gunzip(source) as g:
        line = g.readline(MAX_LINE_BYTES + 1)
    try:
        return _header(json.loads(line))
    except ValueError:
        raise LibraryError("bad_header") from None


def _row(obj: Any, dim: int) -> Row:
    if not isinstance(obj, dict) or set(obj) != {"chunk_id", "content", "metadata", "embedding"}:
        raise LibraryError("bad_row")
    cid, content, meta, emb = obj["chunk_id"], obj["content"], obj["metadata"], obj["embedding"]
    ok = (
        isinstance(cid, str)
        and CHUNK_ID.fullmatch(cid)
        and isinstance(content, str)
        and 0 < len(content) <= MAX_CONTENT_CHARS + 100
        and isinstance(meta, dict)
        and not forged(content)
        and not forged(meta.get("title"))
        and isinstance(emb, list)
        and len(emb) == dim
        and all(type(x) in (int, float) and math.isfinite(x) for x in emb)
    )
    if not ok:
        raise LibraryError("bad_row")
    return Row(cid, content, meta, tuple(float(x) for x in emb))


def read_rows(source: Path | bytes, header: Header) -> Iterator[Row]:
    """The artefact's rows (from read_pinned's bytes when loading), each
    checked; the count must be the header's."""
    seen: set[str] = set()
    with _gunzip(source) as g:
        g.readline(MAX_LINE_BYTES + 1)
        n = 0
        for line in g:
            if len(line) > MAX_LINE_BYTES:
                raise LibraryError("bad_row")
            try:
                row = _row(json.loads(line), header.dim)
            except ValueError:
                raise LibraryError("bad_row") from None
            if row.chunk_id in seen:
                raise LibraryError("duplicate_id")
            seen.add(row.chunk_id)
            n += 1
            yield row
    if n != header.count:
        raise LibraryError("count_mismatch")


# --- the operator's commands -------------------------------------------------------------


def main(argv: Sequence[str] | None = None) -> int:  # pragma: no cover - thin CLI
    p = argparse.ArgumentParser(prog="python -m intelligence.knowledge_base.reference_library")
    sub = p.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify", help="check an artefact against the lock")
    v.add_argument("artefact", type=Path)
    args = p.parse_args(argv)
    try:
        header, sha = verify(args.artefact, load_lock())
    except LibraryError as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    print(f"ok {header.name} {header.version} rows={header.count} sha256={sha}")
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
