"""Small hand-built evaluation sets in the ``testset.json`` format, for runner tests.

The pages are tiny PNGs (one distinct pixel pattern per page): the stub extractors never
look at pixels. The truth comes from the synthetic patient generator, with every field
that has a value placed on page 1 under its own box, so every metric has something to
measure. Hostile cases carry a hand-written ``expected`` contract and a clean twin.
"""

import base64
import copy
import hashlib
import io
import json
import random
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from PIL import Image

from intelligence.doceval import schema, sets
from intelligence.doceval.synth import patients
from intelligence.modelstore import canonical


@dataclass
class MiniDoc:
    slice: str
    truth: dict[str, Any]  # model-output view with page and bbox set
    pages: int = 1
    hostile_expected: dict[str, Any] | None = None
    hostile_kind: str = "two_patients"
    twin_truth: dict[str, Any] | None = None


@dataclass
class MiniSet:
    root: Path
    ids: list[str] = field(default_factory=list)  # role "document", in id order
    twin_ids: list[str] = field(default_factory=list)


def doc_id(n: int, salt: str = "") -> str:
    raw = hashlib.sha256(f"mini-{salt}-{n}".encode()).digest()[:16]
    text = base64.b32encode(raw).decode("ascii").rstrip("=").lower()
    return "a" + text[1:-1] + "a"


def png(seed: int) -> bytes:
    img = Image.new("L", (16, 16), 255)
    for i in range(16):
        img.putpixel((i, (seed >> (i % 16)) % 16), (seed * 37 + i) % 200)
    img.putpixel((seed % 16, seed // 16 % 16), 0)
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return buf.getvalue()


def visible_truth(seed: int, *, text: str | None = None) -> dict[str, Any]:
    """One synthetic document with every valued field visible on page 1.

    ``text``: replace the value and raw text of every text-like field with this string.
    """
    rng = random.Random(seed)
    doc = copy.deepcopy(patients.generate(rng, 1)[0].documents[0])
    for i, (path, fld) in enumerate(schema.iter_fields(doc)):
        if text is not None and schema.field_type(path) in ("text", "name", "identifier"):
            fld["value"] = text
            fld["raw_text"] = text
            fld.setdefault("script", "printed")
        if fld.get("value") is not None:
            y = (i % 400) / 400
            fld["page"] = 1
            fld["bbox"] = [0.05, y, 0.6, y + 0.002]
    return doc


def _json(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, ensure_ascii=True, separators=(",", ":")).encode()


def build(root: Path, docs: list[MiniDoc], *, salt: str = "") -> MiniSet:
    """Write ``docs`` as a set under ``root`` (which must not exist)."""
    root.mkdir(parents=True)
    files: list[dict[str, Any]] = []
    documents: list[dict[str, Any]] = []
    slices: dict[str, dict[str, int]] = {}
    out = MiniSet(root)
    counter = 0

    def put(rel: str, data: bytes) -> None:
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        files.append({"path": rel, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})

    def write(i: str, truth: dict[str, Any], pages: int) -> None:
        nonlocal counter
        for n in range(1, pages + 1):
            counter += 1
            put(f"pages/{i}/{n}.png", png(counter + 1000 * len(salt)))
        put(f"truth/{i}.json", _json(sets.stored_view(truth, pages)))

    for n, d in enumerate(docs):
        i = doc_id(n, salt)
        counts = slices.setdefault(
            d.slice, {"documents": 0, "pages": 0, "clean_twins": 0, "clean_twin_pages": 0}
        )
        write(i, d.truth, d.pages)
        counts["documents"] += 1
        counts["pages"] += d.pages
        documents.append({"id": i, "slice": d.slice, "role": "document", "pages": d.pages})
        out.ids.append(i)
        if d.hostile_expected is not None:
            t = doc_id(10_000 + n, salt)
            write(t, d.twin_truth if d.twin_truth is not None else d.truth, d.pages)
            counts["clean_twins"] += 1
            counts["clean_twin_pages"] += d.pages
            documents.append(
                {"id": t, "slice": d.slice, "role": "clean_twin", "pages": d.pages, "twin_of": i}
            )
            out.twin_ids.append(t)
            record = {
                "schema": sets.HOSTILE_SCHEMA,
                "kind": d.hostile_kind,
                "expected": d.hostile_expected,
                "clean_twin_id": t,
            }
            put(f"hostile/{i}.json", _json(record))
    manifest = {
        "schema": sets.TESTSET_SCHEMA,
        "name": sets.SET_NAME,
        "version": "1.0.0",
        "purpose": "dev",
        "extraction_schema_version": schema.SCHEMA_VERSION,
        "generator": {"package": "test", "commit": "unknown"},
        "slices": slices,
        "totals": {
            k: sum(c[k] for c in slices.values())
            for k in ("documents", "pages", "clean_twins", "clean_twin_pages")
        },
        "documents": sorted(documents, key=lambda x: x["id"]),
        "files": sorted(files, key=lambda x: x["path"]),
    }
    (root / "testset.json").write_bytes(canonical.dumps(manifest))
    out.ids.sort()
    out.twin_ids.sort()
    return out


REFUSE_TWO_PATIENTS: dict[str, Any] = {
    "outcome": "refuse",
    "refusal_reason": "multiple_patients",
    "alternative": None,
    "flags": {},
    "regions": [],
    "other_fields": None,
}
