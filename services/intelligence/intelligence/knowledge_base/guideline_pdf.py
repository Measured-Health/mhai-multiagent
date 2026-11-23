"""Guideline documents as reference libraries: one library per document
(``who-malaria``, ``ng-malaria``, ``ng-stg``), built from the publisher's
PDF (docs/models/reference-library.md, "Guideline documents").

Each document is an opt-in entry of ``models/guideline-documents.lock``
(:func:`load_documents`): its title, publisher, edition, the exact URL it
was fetched from, the PDF's SHA-256, the pages to use, the licence as
found and a licence status (:data:`LICENCE_STATUSES`). The converter
refuses a PDF whose bytes are not the pinned ones, and a document with no
entry. Building does not make a library loadable: only a document whose
licence status is ``permitted`` may be pinned in
models/reference-library.lock (a test holds it), and pinning it is a pull
request. A ``needs_owner_decision`` document is built and kept ready on
the build server until the owner decides.

The conversion is deterministic (the same PDF and pypdf version give the
same rows):

1. Text extraction with pypdf (pinned in uv.lock), page by page, over the
   entry's page range. No OCR: a page with no text layer gives no text.
2. Clean-up: the lines that repeat on many pages (running headers and
   footers, "12 of 494") and bare page numbers are dropped, and so are
   table-of-contents lines (dot leaders).
3. Section-aware chunking: a numbered heading ("4.2.1 Artemether-
   lumefantrine") starts a new chunk; text is packed into chunks of about
   :data:`TARGET_WORDS` words, never more than :data:`MAX_WORDS`; a run of
   dose-table lines (numbers, mg/kg) is kept in one chunk where it fits.
4. Each chunk becomes a ``guideline`` row: its text prefixed with the
   document, section and pages, and metadata (title, section, page_start,
   page_end, edition, publisher, url, licence, attribution).

A row never carries patient fields: the metadata is the library's closed
key list, checked again at load. Nothing here logs page text.
"""

from __future__ import annotations

import io
import json
import re
from collections import Counter
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.knowledge_base import reference_library as reflib

KIND = "guideline"
LOCK_SCHEMA = "mhai.guidelines.lock/1"
LOCK_PATH = reflib.REPO / "models" / "guideline-documents.lock"
LICENCE_STATUSES = ("permitted", "needs_owner_decision", "unknown")
PUBLISHERS = frozenset({reflib.WHO, reflib.FMOH_NG})
TARGET_WORDS = 380
MAX_WORDS = 520
MIN_WORDS = 60
MAX_BODY_CHARS = 3500
MAX_PAGES = 2000
REPEAT_SHARE = 0.4  # a line on at least this share of pages is a header or footer
EDGE_LINES = 3  # where a running header or footer can be: a page's first or last lines

_ENTRY_KEYS = frozenset(
    {
        "title",
        "short_title",
        "publisher",
        "edition",
        "url",
        "landing_page",
        "pdf_sha256",
        "pages",
        "first_page",
        "last_page",
        "licence",
        "licence_text",
        "licence_status",
        "retrieved",
        "version",
        "artefact_sha256",
        "count",
        "note",
    }
)
_HEADING = re.compile(r"(\d{1,2}(?:\.\d{1,2}){0,4})\.?\s+(\S.{1,150})")
_DOTS = re.compile(r"(?:\.\s?){6,}|…{2,}")
_PAGE_NO = re.compile(r"(?:[ivxlc]{1,7}|\d{1,4}|\d{1,4}\s+of\s+\d{1,4}|page\s+\d{1,4})", re.I)
_NUMERIC = re.compile(r"\d")
_DOSE_UNIT = re.compile(r"\b(?:mg|mcg|ml|kg|tablets?|tabs?|doses?|hours?|days?)\b", re.I)
_DOSING = re.compile(r"\b(?:daily|once|twice|x)\b", re.I)
_DATE = re.compile(r"20[0-9]{2}-[01][0-9]-[0-3][0-9]")
_SHA = re.compile(r"[0-9a-f]{64}")


class GuidelineError(reflib.LibraryError):
    """A document, lock or PDF the converter refuses (a reason code)."""


@dataclass(frozen=True)
class Document:
    """One entry of models/guideline-documents.lock."""

    name: str
    title: str
    short_title: str
    publisher: str
    edition: str
    url: str | None
    pdf_sha256: str | None
    first_page: int
    last_page: int
    licence: str
    licence_text: str
    licence_status: str
    retrieved: str
    version: str

    @property
    def attribution(self) -> str:
        return (
            f"{self.title}, {self.edition}. {self.publisher}. Licence as published: "
            f"{self.licence}. Source: {self.url}. Provided as is; the publisher does not "
            "endorse MHAI. Not a substitute for clinical judgement."
        )[:2000]


def load_documents(path: Path | None = None) -> dict[str, Document]:
    """models/guideline-documents.lock: every opt-in document, checked."""
    try:
        obj = json.loads(Path(path or LOCK_PATH).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise GuidelineError("lock_invalid") from None
    docs = obj.get("documents") if isinstance(obj, dict) else None
    if not isinstance(obj, dict) or obj.get("schema") != LOCK_SCHEMA or not isinstance(docs, dict):
        raise GuidelineError("lock_invalid")
    out: dict[str, Document] = {}
    for name, e in docs.items():
        spec = reflib.LIBRARIES.get(name)
        if spec is None or set(spec.sources) != {KIND} or not isinstance(e, dict):
            raise GuidelineError("lock_invalid")
        if not set(e) <= _ENTRY_KEYS:
            raise GuidelineError("lock_invalid")
        try:
            doc = Document(
                name=name,
                title=_str(e["title"]),
                short_title=_str(e["short_title"]),
                publisher=_str(e["publisher"]),
                edition=_str(e["edition"]),
                url=e.get("url"),
                pdf_sha256=e.get("pdf_sha256"),
                first_page=int(e.get("first_page", 1)),
                last_page=int(e.get("last_page", MAX_PAGES)),
                licence=_str(e["licence"]),
                licence_text=_str(e["licence_text"]),
                licence_status=_str(e["licence_status"]),
                retrieved=_str(e["retrieved"]),
                version=_str(e["version"]),
            )
        except (KeyError, TypeError, ValueError):
            raise GuidelineError("lock_invalid") from None
        ok = (
            doc.publisher in PUBLISHERS
            and doc.publisher in spec.sources[KIND]
            and doc.licence_status in LICENCE_STATUSES
            and _DATE.fullmatch(doc.retrieved) is not None
            and reflib._VERSION.fullmatch(doc.version) is not None
            and (doc.url is None or (isinstance(doc.url, str) and doc.url.startswith("https://")))
            and (doc.pdf_sha256 is None or _SHA.fullmatch(str(doc.pdf_sha256)) is not None)
            and 1 <= doc.first_page <= doc.last_page <= MAX_PAGES
            and not any(reflib.forged(t) or reflib.demo_marked(t) for t in _texts(doc))
        )
        if not ok:
            raise GuidelineError("lock_invalid")
        out[name] = doc
    return out


def _str(v: Any) -> str:
    if not isinstance(v, str) or not v.strip() or len(v) > 4000:
        raise ValueError
    return v.strip()


def _texts(doc: Document) -> Iterable[str]:
    return (doc.title, doc.short_title, doc.publisher, doc.edition, doc.licence)


# --- extraction and clean-up ---------------------------------------------------------------


def extract_pages(data: bytes, first: int, last: int) -> list[tuple[int, str]]:
    """(page number, text) for pages first..last (1-based, inclusive) of a
    PDF, with pypdf. Lazy import: only the build machine needs it."""
    from pypdf import PdfReader

    try:
        reader = PdfReader(io.BytesIO(data), strict=False)
        n = len(reader.pages)
    except Exception:  # noqa: BLE001 - any parse failure is a refusal
        raise GuidelineError("not_a_pdf") from None
    if n > MAX_PAGES:
        raise GuidelineError("too_many_pages")
    out: list[tuple[int, str]] = []
    for i in range(first - 1, min(last, n)):
        try:
            text = reader.pages[i].extract_text() or ""
        except Exception:  # noqa: BLE001
            text = ""
        out.append((i + 1, text))
    return out


def _norm(line: str) -> str:
    return re.sub(r"\d+", "#", " ".join(line.split()).lower())


def _line(text: str) -> str:
    """One line, cleaned: control characters out, spaces collapsed, and the
    sequences a library row may not hold (a prompt's delimiters, a citation)
    replaced."""
    out = "".join(" " if ord(c) < 32 or 0x7F <= ord(c) < 0xA0 else c for c in text)
    out = out.replace("<<", "«").replace(">>", "»").replace("�", " ")
    out = re.sub(r"(?i)kb\s*#", "KB no.", out)
    return " ".join(out.split())


def clean_pages(pages: Sequence[tuple[int, str]]) -> list[tuple[int, str]]:
    """Each page's kept lines, as (page, line): running headers and footers
    (a line among a page's first or last :data:`EDGE_LINES` that is there
    on at least :data:`REPEAT_SHARE` of the pages), bare page numbers and
    table-of-contents lines dropped. A line repeated in the body of many
    pages ("Remark:") is kept."""
    per_page = [(p, [x for x in (_line(y) for y in t.splitlines()) if x]) for p, t in pages]
    seen: Counter[str] = Counter()
    for _, lines in per_page:
        seen.update({_norm(lines[i]) for i in _edges(len(lines))})
    limit = max(3, int(len(per_page) * REPEAT_SHARE))
    out: list[tuple[int, str]] = []
    for p, lines in per_page:
        edges = _edges(len(lines))
        for i, x in enumerate(lines):
            if _PAGE_NO.fullmatch(x) or _DOTS.search(x):
                continue
            if len(per_page) >= 5 and i in edges and seen[_norm(x)] >= limit:
                continue
            out.append((p, x))
    return out


def _edges(n: int) -> set[int]:
    """The positions of a page's first and last :data:`EDGE_LINES` lines."""
    return set(range(min(n, EDGE_LINES))) | set(range(max(0, n - EDGE_LINES), n))


def heading(line: str) -> str | None:
    """The line as a section heading ("4.2.1 Artemether-lumefantrine"), or
    None: a section number then a short title that starts with a capital,
    has no more than 16 words and doesn't end like a sentence."""
    m = _HEADING.fullmatch(line)
    if m is None:
        return None
    number, title = m.group(1), m.group(2).strip()
    top = int(number.split(".")[0])
    words = title.split()
    if not 1 <= top <= 30 or not title[0].isupper() or len(words) > 16:
        return None
    if title.endswith((".", ",", ";", ":")) and not title.isupper():
        return None
    if "." not in number and not (title.isupper() or len(words) <= 8):
        return None
    if "." not in number and (_tabular(line) or _DOSING.search(title)):
        return None  # a numbered dose-table row ("1 Tab twice daily x 3 days")
    if sum(c.isdigit() for c in title) > len(title) // 3:
        return None
    return f"{number} {title}"


def _tabular(line: str) -> bool:
    """A dose-table line: several numbers, or a number and a dose unit."""
    digits = len(_NUMERIC.findall(line))
    return digits >= 4 or (digits >= 1 and _DOSE_UNIT.search(line) is not None and len(line) < 140)


@dataclass(frozen=True)
class Chunk:
    section: str
    page_start: int
    page_end: int
    text: str


def chunk_lines(lines: Sequence[tuple[int, str]]) -> list[Chunk]:
    """Section-aware chunks of the cleaned lines (see the module docstring)."""
    # Units: a heading, a run of table lines, or a text line.
    units: list[tuple[str | None, int, int, str]] = []  # (heading, p0, p1, text)
    i = 0
    while i < len(lines):
        p, x = lines[i]
        h = heading(x)
        if h is not None:
            units.append((h, p, p, h))
            i += 1
            continue
        if _tabular(x):
            j = i
            block: list[str] = []
            while j < len(lines) and _tabular(lines[j][1]) and heading(lines[j][1]) is None:
                block.append(lines[j][1])
                j += 1
            units.append((None, p, lines[j - 1][0], " ".join(block)))
            i = j
            continue
        units.append((None, p, p, x))
        i += 1

    chunks: list[Chunk] = []
    section = ""
    cur: list[str] = []
    cur_section = ""
    has_body = False
    p0 = p1 = 0

    def words(parts: Sequence[str]) -> int:
        return sum(len(s.split()) for s in parts)

    def flush() -> None:
        nonlocal cur, has_body
        has_body = False
        if cur:
            for piece in _split(" ".join(cur)):
                chunks.append(Chunk(cur_section, p0, p1, piece))
        cur = []

    for h, a, b, text in units:
        if h is not None:
            section = h
            if words(cur) >= MIN_WORDS:
                flush()
        n = len(text.split())
        if cur and (words(cur) + n > MAX_WORDS or words(cur) >= TARGET_WORDS):
            flush()
        if not cur:
            cur_section, p0 = section, a
        elif not has_body:
            cur_section = section  # only headings so far: the deepest names it
        has_body = has_body or h is None
        cur.append(text)
        p1 = b
    flush()
    return chunks


def _split(text: str) -> list[str]:
    """A text over the word or character bound, cut into pieces at word
    boundaries."""
    out: list[str] = []
    cur: list[str] = []
    size = 0
    for w in text.split():
        if cur and (len(cur) >= MAX_WORDS or size + len(w) + 1 > MAX_BODY_CHARS):
            out.append(" ".join(cur))
            cur, size = [], 0
        cur.append(w)
        size += len(w) + 1
    if cur:
        out.append(" ".join(cur))
    return out


# --- rows ----------------------------------------------------------------------------------


def entries(doc: Document, chunks: Sequence[Chunk]) -> list[reflib.Entry]:
    """The library rows of a document's chunks, in document order."""
    out: list[reflib.Entry] = []
    for n, c in enumerate(chunks):
        if c.page_start == c.page_end:
            pages = f"p. {c.page_start}"
        else:
            pages = f"pp. {c.page_start}-{c.page_end}"
        where = f", section {c.section}" if c.section else ""
        content = f"{doc.short_title} ({doc.edition}){where}, {pages}: {c.text}"
        if len(content) > reflib.MAX_CONTENT_CHARS:
            content = content[: reflib.MAX_CONTENT_CHARS]
        title = f"{doc.short_title}: {c.section}" if c.section else doc.short_title
        meta: dict[str, Any] = {
            "kind": KIND,
            "source": doc.publisher,
            "publisher": doc.publisher,
            "document": doc.name,
            "title": title[: reflib.MAX_TITLE_CHARS],
            "section": c.section[: reflib.MAX_TITLE_CHARS],
            "edition": doc.edition,
            "page_start": c.page_start,
            "page_end": c.page_end,
            "licence": doc.licence,
            "audience": "clinician",
            "retrieved": doc.retrieved,
            "attribution": doc.attribution,
        }
        if doc.url:
            meta["url"] = doc.url
        if reflib.forged(content) or reflib.forged(meta["title"]):
            raise GuidelineError("forged_text")
        out.append(reflib.Entry(kind=KIND, content=content, metadata=meta, order=(n,)))
    return out


def convert(
    doc: Document, data: bytes, counts: Counter[str] | None = None
) -> tuple[list[reflib.Entry], str]:
    """The document's rows and the PDF's SHA-256, once the bytes are the
    pinned ones."""
    sha = reflib.hashlib.sha256(data).hexdigest()
    if doc.pdf_sha256 is None:
        raise GuidelineError("no_source")
    if sha != doc.pdf_sha256:
        raise GuidelineError("pdf_hash_mismatch")
    pages = extract_pages(data, doc.first_page, doc.last_page)
    lines = clean_pages(pages)
    chunks = chunk_lines(lines)
    rows = entries(doc, chunks)
    if not rows:
        raise GuidelineError("no_text")
    if counts is not None:
        counts["pages"] += len(pages)
        counts["empty_pages"] += sum(1 for _, t in pages if not t.strip())
        counts["rows"] += len(rows)
        counts["sections"] += len({c.section for c in chunks})
    return rows, sha


def build(
    doc: Document,
    data: bytes,
    embed: reflib.Embedder,
    *,
    embedder: str,
    counts: Counter[str] | None = None,
    batch: int = 256,
) -> tuple[reflib.Header, list[reflib.Row]]:
    rows, sha = convert(doc, data, counts)
    return reflib.build(
        rows,
        embed,
        version=doc.version,
        embedder=embedder,
        built_from=sha,
        retrieved=doc.retrieved,
        batch=batch,
        name=doc.name,
        licence=doc.licence[:512],
        attribution=doc.attribution,
    )


def loadable(doc: Document) -> bool:
    """Whether the document may be pinned as a loadable library."""
    return doc.licence_status == "permitted"


def check_pins(pins: Mapping[str, reflib.Pin], docs: Mapping[str, Document]) -> list[str]:
    """The guideline libraries pinned in models/reference-library.lock whose
    document is not ``permitted`` (or has no entry): must be empty."""
    bad = []
    for name in pins:
        spec = reflib.LIBRARIES.get(name)
        if spec is not None and set(spec.sources) == {KIND}:
            doc = docs.get(name)
            if doc is None or not loadable(doc):
                bad.append(name)
    return sorted(bad)
