"""The local formulary library (``local-formulary``): the brand and local
names a Nigerian pilot site uses for its common medicines, each mapped to
its generic name, class and typical use (docs/models/reference-library.md,
"Local formulary").

Why: clinicians at the site write brand names ("Lonart", "Camosunate",
"PCM"). The other libraries know generic names (openFDA labels, WHO and
national guidelines), so a question that names a brand would retrieve
nothing useful. One short reference row per brand fixes that: the row holds
the brand and the generic name, so the database's full-text pre-filter
finds it by the brand, and the worker then also pulls the generic's own
reference rows (intelligence/boundary/kb_repo.py, the generic expansion).

The input is the curated CSV ``models/reference/local-formulary.csv``
(columns :data:`COLUMNS`; no patient data: drug names only). Only rows of
confidence ``high`` or ``medium`` become library rows; ``low`` rows wait
for a clinician to confirm them (raise the confidence in the CSV, bump the
library version, rebuild and pin). The row text keeps the CSV's typical
use as written, so a caution such as "not recommended for falciparum
malaria in Nigeria" stays in the reference.

Our own curation, released as CC0-1.0 (:data:`LICENCE`). Nothing here logs
row text.
"""

from __future__ import annotations

import csv
import io
import re
from collections import Counter
from collections.abc import Iterable, Sequence
from pathlib import Path

from intelligence.knowledge_base import reference_library as reflib

NAME = "local-formulary"
KIND = "local_formulary"
SOURCE = reflib.OWN_SOURCE
LICENCE = "CC0-1.0 (own work, Measured Health)"
ATTRIBUTION = (
    "Local formulary curated by Measured Health (CC0-1.0): brand and local names mapped to "
    "generic names. Not a product label: check the pack, the dose and the national "
    "guidelines. Not a substitute for clinical judgement."
)
CSV_PATH = reflib.REPO / "models" / "reference" / "local-formulary.csv"
COLUMNS = ("brand_or_local_name", "generic", "class", "typical_use", "confidence", "notes")
LOADED_CONFIDENCE = frozenset({"high", "medium"})
CONFIDENCE = frozenset({"high", "medium", "low"})
MAX_FIELD_CHARS = 300
MAX_CSV_BYTES = 1 << 20

# A dose or strength in a generic name ("20/120 mg"), a parenthesis, and
# the words that say how much rather than what: dropped from the generic
# the worker searches the other libraries for.
_DOSE = re.compile(r"\b\d[\d/.,]*\s*(?:mg|mcg|µg|g|ml|iu|%)?(?=\W|$)", re.IGNORECASE)
_PAREN = re.compile(r"\([^)]*\)")
_QUALIFIERS = re.compile(r"\b(?:low dose|double strength)\b", re.IGNORECASE)


class FormularyError(reflib.LibraryError):
    """A formulary CSV the converter refuses (a reason code, never row text)."""


def _field(text: str | None) -> str:
    out = reflib._clean(text or "", MAX_FIELD_CHARS)
    if reflib.forged(out) or reflib.demo_marked(out):
        raise FormularyError("bad_field")
    return out


def search_generic(generic: str) -> str:
    """The generic name without doses or qualifiers: what the worker
    searches the other libraries for ("artemether-lumefantrine 20/120 mg"
    gives "artemether-lumefantrine")."""
    out = _PAREN.sub(" ", generic)
    out = _QUALIFIERS.sub(" ", out)
    out = _DOSE.sub(" ", out)
    return " ".join(out.split()).strip(" +,/;-")


def _names(brand: str) -> tuple[str, str]:
    """The first name and the way the row text lists all of them
    ("Emzor / Emzor paracetamol" -> "Emzor (also Emzor paracetamol)")."""
    parts = [p.strip() for p in brand.split("/") if p.strip()]
    if not parts:
        raise FormularyError("bad_field")
    first = parts[0]
    if len(parts) == 1:
        return first, first
    return first, f"{first} (also {', '.join(parts[1:])})"


def content_for(brand: str, generic: str, drug_class: str, use: str) -> str:
    """The reference text of one formulary row (brand, generic, class and
    use in one sentence each, so full-text search finds it by any of them)."""
    _, names = _names(brand)
    if brand.strip().lower() == generic.strip().lower():
        head = f"{names} ({generic}) is in the local formulary used in Nigeria."
    else:
        head = f"{names} is a local brand or name used in Nigeria for {generic}."
    return (
        f"{head} Class: {drug_class}. Typical use: {use}. "
        "Local formulary entry: confirm the product and the dose against the pack "
        "and the national guidelines."
    )


def read_csv(data: bytes) -> list[dict[str, str]]:
    """The CSV's rows, its header exactly :data:`COLUMNS`."""
    if len(data) > MAX_CSV_BYTES:
        raise FormularyError("too_large")
    try:
        text = data.decode("utf-8-sig")
    except UnicodeDecodeError:
        raise FormularyError("not_utf8") from None
    reader = csv.reader(io.StringIO(text, newline=""))
    rows = list(reader)
    if not rows or tuple(h.strip() for h in rows[0]) != COLUMNS:
        raise FormularyError("bad_header")
    out: list[dict[str, str]] = []
    for r in rows[1:]:
        if not any(c.strip() for c in r):
            continue
        if len(r) != len(COLUMNS):
            raise FormularyError("bad_row")
        out.append(dict(zip(COLUMNS, r, strict=True)))
    return out


def entries(
    rows: Iterable[dict[str, str]], retrieved: str, counts: Counter[str] | None = None
) -> list[reflib.Entry]:
    """The library rows: confidence high or medium only (``counts`` gets
    ``loaded``, ``low`` and ``duplicate``), each brand once, in brand order."""
    out: list[reflib.Entry] = []
    seen: set[str] = set()
    for r in rows:
        confidence = r["confidence"].strip().lower()
        if confidence not in CONFIDENCE:
            raise FormularyError("bad_confidence")
        if confidence not in LOADED_CONFIDENCE:
            if counts is not None:
                counts["low"] += 1
            continue
        brand = _field(r["brand_or_local_name"])
        generic = _field(r["generic"])
        drug_class = _field(r["class"])
        use = _field(r["typical_use"])
        if not brand or not generic or not drug_class or not use:
            raise FormularyError("bad_row")
        if generic.lower().startswith(("unknown", "possibly")):
            raise FormularyError("unconfirmed_generic")
        key = brand.lower()
        if key in seen:
            if counts is not None:
                counts["duplicate"] += 1
            continue
        seen.add(key)
        first, _ = _names(brand)
        content = content_for(brand, generic, drug_class, use)
        if len(content) > reflib.MAX_CONTENT_CHARS:
            raise FormularyError("bad_row")
        out.append(
            reflib.Entry(
                kind=KIND,
                content=content,
                metadata={
                    "kind": KIND,
                    "source": SOURCE,
                    "title": f"{first} ({generic})",
                    "licence": "CC0-1.0",
                    "brand": brand,
                    "generic": search_generic(generic),
                    "drug_class": drug_class,
                    "confidence": confidence,
                    "audience": "clinician",
                    "retrieved": retrieved,
                    "attribution": ATTRIBUTION,
                },
                order=(key,),
            )
        )
        if counts is not None:
            counts["loaded"] += 1
    out.sort(key=lambda e: e.order)
    return out


def build(
    path: Path,
    embed: reflib.Embedder,
    *,
    version: str,
    embedder: str,
    retrieved: str,
    counts: Counter[str] | None = None,
    batch: int = 256,
) -> tuple[reflib.Header, Sequence[reflib.Row]]:
    """The artefact's header and rows from the CSV at ``path``."""
    data = Path(path).read_bytes()
    rows = entries(read_csv(data), retrieved, counts)
    return reflib.build(
        rows,
        embed,
        version=version,
        embedder=embedder,
        built_from=reflib.hashlib.sha256(data).hexdigest(),
        retrieved=retrieved,
        batch=batch,
        name=NAME,
        licence=LICENCE,
        attribution=ATTRIBUTION,
    )
