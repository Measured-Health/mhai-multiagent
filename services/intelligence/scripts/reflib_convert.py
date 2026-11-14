"""Convert a source bundle's knowledge base into reference library v1 (medref).

An operator runs this on a build machine, never a worker, and never on a
laptop (the embedding is heavy):

    uv run python scripts/reflib_convert.py \\
        --input bundle-20261114T131446Z-1-001.zip --retrieved 2026-11-14 \\
        --version 1.0.0 --models /srv/reflib/models --fetch --out /srv/reflib/out

It reads one member of the bundle, bundle/kb/chunks.jsonl.gz, as a stream:
nothing is extracted, and nothing else in the bundle is opened, loaded or
run. It keeps the approved rows only (openFDA drug labels, English
MedlinePlus health topics; intelligence/knowledge_base/reference_library.py),
renumbers them KB#1000001 up, embeds them with OUR pinned embedder (role
dense of models/answer-gate-models.lock, verified file by file, loaded
offline; never the bundle's own vectors) and writes
<out>/medref-<version>.jsonl.gz. It prints the artefact's SHA-256 and row
count: pin them in models/reference-library.lock by pull request, then load
with scripts/reflib_load.py.

--input may also be a .jsonl.gz in the bundle's row shape (the synthetic
test fixture).
"""

from __future__ import annotations

import argparse
import re
import sys
import time
from collections import Counter
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from intelligence.doceval.answers import gate_models
from intelligence.knowledge_base import reference_library as reflib

_DATE = re.compile(r"20[0-9]{2}-[01][0-9]-[0-3][0-9]")


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--input", type=Path, required=True, help="the bundle zip, or a .jsonl.gz")
    p.add_argument("--retrieved", required=True, help="the date the sources were fetched")
    p.add_argument("--version", required=True, help="the library version, e.g. 1.0.0")
    p.add_argument("--models", type=Path, required=True, help="pinned model folder (role dense)")
    p.add_argument("--fetch", action="store_true", help="download the pinned dense model first")
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args(argv)
    if not _DATE.fullmatch(args.retrieved):
        p.error("--retrieved must be YYYY-MM-DD")

    lock = gate_models.load_lock()
    gate_models.check_config(lock)
    if args.fetch:
        gate_models.fetch_role(lock, args.models, "dense")
    dense = gate_models.verify_role(lock, args.models, "dense")
    pin = lock["dense"]
    embedder = f"{pin.repo}@{pin.revision}"

    from intelligence.boundary.models import set_hub_offline

    set_hub_offline()
    from sentence_transformers import SentenceTransformer

    model = SentenceTransformer(str(dense), device="cpu", local_files_only=True)

    def embed(texts: Sequence[str]) -> Any:
        return model.encode(
            list(texts), batch_size=64, normalize_embeddings=True, show_progress_bar=False
        )

    started = time.monotonic()
    built_from = reflib.sha256_file(args.input)
    with reflib.open_chunks(args.input) as stream:
        entries = reflib.collect(reflib.read_bundle_lines(stream), args.retrieved)
    kinds = Counter(e.kind for e in entries)
    sections = Counter(e.metadata.get("section") for e in entries if e.kind == "drug_label")
    print(f"approved: {dict(kinds)}; drug label sections: {dict(sections)}", flush=True)

    header, rows = reflib.build(
        entries,
        embed,
        version=args.version,
        embedder=embedder,
        built_from=built_from,
        retrieved=args.retrieved,
    )
    args.out.mkdir(parents=True, exist_ok=True)
    path, sha = reflib.write_artefact(header, rows, args.out)
    took = time.monotonic() - started
    print(f"artefact: {path} rows={header.count} dim={header.dim} sha256={sha} ({took:.0f} s)")
    print(f"embedder: {embedder}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
