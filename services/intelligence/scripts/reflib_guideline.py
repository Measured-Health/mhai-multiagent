"""Build one guideline document's library from its PDF
(intelligence/knowledge_base/guideline_pdf.py).

The document must be an entry of models/guideline-documents.lock, and the
PDF's bytes the ones it pins (pdf_sha256). An operator runs this on the
build machine (our pinned embedder), never on a laptop or a worker:

    uv run python scripts/reflib_guideline.py --document who-malaria \\
        --pdf /src/who-malaria-2026-09-10.pdf --models /models --fetch --out /out

It prints the counts (pages, empty pages, sections, rows), the artefact's
path, row count and SHA-256, and the document's licence status. Record the
artefact in the document's lock entry (artefact_sha256, count). Only a
document whose licence status is "permitted" may also be pinned as
loadable, in models/reference-library.lock and core's reflibPins.

--dry-run converts without embedding and prints the counts only.
"""

from __future__ import annotations

import argparse
import sys
from collections import Counter
from pathlib import Path

from intelligence.knowledge_base import guideline_pdf as guidelines
from intelligence.knowledge_base import reference_library as reflib


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--document", required=True, help="the lock entry, e.g. who-malaria")
    p.add_argument("--pdf", type=Path, required=True)
    p.add_argument("--lock", type=Path, default=guidelines.LOCK_PATH)
    p.add_argument("--models", type=Path, help="pinned model folder (role dense)")
    p.add_argument("--fetch", action="store_true", help="download the pinned dense model first")
    p.add_argument("--out", type=Path)
    p.add_argument("--dry-run", action="store_true")
    args = p.parse_args(argv)
    try:
        doc = guidelines.load_documents(args.lock).get(args.document)
        if doc is None:
            raise guidelines.GuidelineError("not_in_lock")
        data = reflib._read_once(args.pdf)
        counts: Counter[str] = Counter()
        if args.dry_run:
            guidelines.convert(doc, data, counts)
            print(f"dry run {doc.name}: {dict(counts)}")
            return 0
        if args.models is None or args.out is None:
            p.error("--models and --out are required unless --dry-run")
        embed, embedder = reflib.pinned_embedder(args.models, args.fetch)
        header, rows = guidelines.build(doc, data, embed, embedder=embedder, counts=counts)
    except reflib.LibraryError as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    args.out.mkdir(parents=True, exist_ok=True)
    path, sha = reflib.write_artefact(header, rows, args.out)
    print(f"{doc.name}: {dict(counts)}")
    print(f"artefact: {path} rows={header.count} dim={header.dim} sha256={sha}")
    print(f"embedder: {embedder}")
    print(f"licence: {doc.licence} (status {doc.licence_status})")
    if not guidelines.loadable(doc):
        print("NOT loadable yet: the licence needs a decision; keep the artefact ready, do not pin")
    return 0


if __name__ == "__main__":
    sys.exit(main())
