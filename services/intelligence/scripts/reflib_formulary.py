"""Build the local formulary library (``local-formulary``) from the curated
CSV (intelligence/knowledge_base/local_formulary.py).

An operator runs this on the build machine (the embedding loads our pinned
embedder), never on a laptop or a worker:

    uv run python scripts/reflib_formulary.py --version 1.0.0 --retrieved 2026-11-23 \\
        --models /models --fetch --out /out

--csv defaults to models/reference/local-formulary.csv. Only rows of
confidence high or medium are kept; the script prints the counts (loaded,
low, duplicate), then the artefact's path, row count and SHA-256: pin those
in models/reference-library.lock and in core (reflibPins) by pull request.
"""

from __future__ import annotations

import argparse
import re
import sys
from collections import Counter
from pathlib import Path

from intelligence.knowledge_base import local_formulary as formulary
from intelligence.knowledge_base import reference_library as reflib

_DATE = re.compile(r"20[0-9]{2}-[01][0-9]-[0-3][0-9]")


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--csv", type=Path, default=formulary.CSV_PATH)
    p.add_argument("--retrieved", required=True, help="the date the CSV was curated")
    p.add_argument("--version", required=True, help="the library version, e.g. 1.0.0")
    p.add_argument("--models", type=Path, required=True, help="pinned model folder (role dense)")
    p.add_argument("--fetch", action="store_true", help="download the pinned dense model first")
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args(argv)
    if not _DATE.fullmatch(args.retrieved):
        p.error("--retrieved must be YYYY-MM-DD")
    embed, embedder = reflib.pinned_embedder(args.models, args.fetch)
    counts: Counter[str] = Counter()
    try:
        header, rows = formulary.build(
            args.csv,
            embed,
            version=args.version,
            embedder=embedder,
            retrieved=args.retrieved,
            counts=counts,
        )
    except reflib.LibraryError as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    print(f"rows: loaded={counts['loaded']} low={counts['low']} duplicate={counts['duplicate']}")
    args.out.mkdir(parents=True, exist_ok=True)
    path, sha = reflib.write_artefact(header, rows, args.out)
    print(f"artefact: {path} rows={header.count} dim={header.dim} sha256={sha}")
    print(f"embedder: {embedder}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
