"""Build the vetted reference subset from a source bundle, for review (an
operator; no model, no database, no network).

    uv run python scripts/build_reference_subset.py \\
        --input chunks.jsonl.gz --retrieved 2026-11-14 --out medref-subset.jsonl.gz

It applies the same allow-list as the reference library's converter
(intelligence/knowledge_base/reference_library.py, ``classify``): openFDA
drug labels (the nine approved sections) and English MedlinePlus health
topics. Every synthetic patient row is refused by name (its type, or source
fields that are not its source's name), and so is anything naming the
public demo's synthetic source; question-answer references, wikidoc,
ontology and phenotype terms, trials, literature and the six placeholder
guidelines are refused by type (docs/models/reference-library.md, "What is
in it" and "Left out, and why").

It writes one JSON line per kept row, in library order (safety-critical
drug label sections first, then health topics): ``content``, ``kind``,
``source``, ``title``, ``licence``, ``upstream_id`` (the label's set id or
the topic id), ``url``, ``attribution`` and ``section`` or ``part``. The
bytes depend on the input and --retrieved only (sorted keys, gzip mtime 0),
so two runs give the same SHA-256. It prints counts only, never row text:
rows kept per source, and records left out per chunk type, source and
reason.

This is the review copy the clinical safety lead and the product owner
read before the library is approved. The artefact that is loaded is built
by scripts/reflib_convert.py from the same input (it adds our pinned
embedder's vectors), pinned in models/reference-library.lock, and loaded
with ``core reference-library load`` (deploy/README.md, "Live site: the
shared reference library").

--input is the bundle zip (only bundle/kb/chunks.jsonl.gz is opened) or a
.jsonl.gz in the bundle's row shape. Its rows are untrusted data: they are
parsed as JSON, never run.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import re
import sys
from collections import Counter
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from intelligence.knowledge_base import reference_library as reflib

_DATE = re.compile(r"20[0-9]{2}-[01][0-9]-[0-3][0-9]")


def subset_row(entry: reflib.Entry) -> dict[str, Any]:
    """One review row: the text and where it came from."""
    m = entry.metadata
    row: dict[str, Any] = {
        "content": entry.content,
        "kind": entry.kind,
        "source": m["source"],
        "title": m["title"],
        "licence": m["licence"],
        "upstream_id": m.get("set_id") or m.get("topic_id"),
        "url": m["url"],
        "attribution": m["attribution"],
    }
    if "section" in m:
        row["section"] = m["section"]
    if "part" in m:
        row["part"] = m["part"]
    return row


def build(
    records: Any, retrieved: str
) -> tuple[list[dict[str, Any]], Counter[str], Counter[tuple[str, str, str]]]:
    """The review rows, rows kept per source, and records left out by
    (chunk type, bundle source, reason)."""
    refused: Counter[tuple[str, str, str]] = Counter()
    entries = reflib.collect(records, retrieved, refused)
    rows = [subset_row(e) for e in entries]
    kept = Counter(r["source"] for r in rows)
    return rows, kept, refused


def write(rows: Sequence[dict[str, Any]], out: Path) -> str:
    """Write the rows as gzip'd JSON Lines (deterministic bytes); returns
    the SHA-256."""
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as g:
        for r in rows:
            line = json.dumps(r, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
            g.write(line.encode() + b"\n")
    data = buf.getvalue()
    out.write_bytes(data)
    return hashlib.sha256(data).hexdigest()


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--input", type=Path, required=True, help="the bundle zip, or a .jsonl.gz")
    p.add_argument("--retrieved", required=True, help="the date the sources were fetched")
    p.add_argument("--out", type=Path, required=True, help="the subset, a .jsonl.gz")
    args = p.parse_args(argv)
    if not _DATE.fullmatch(args.retrieved):
        p.error("--retrieved must be YYYY-MM-DD")
    if not args.out.name.endswith(".jsonl.gz"):
        p.error("--out must end in .jsonl.gz")
    try:
        with reflib.open_chunks(args.input) as stream:
            rows, kept, refused = build(reflib.read_bundle_lines(stream), args.retrieved)
    except reflib.LibraryError as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    sha = write(rows, args.out)
    print(f"kept {len(rows)} rows: " + ", ".join(f"{s} {n}" for s, n in sorted(kept.items())))
    patients = sum(n for (_, _, why), n in refused.items() if why == reflib.REFUSED_PATIENT)
    print(f"left out {sum(refused.values())} records ({patients} patient rows):")
    for (kind, src, why), n in sorted(refused.items()):
        print(f"  {kind}/{src}: {why} {n}")
    print(f"subset: {args.out} sha256={sha}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
