"""Synthetic evaluation set v1: the slices, the set builder and the locked 30/70 split.

:data:`COMPOSITION_V1` is design section 6.2's table as data: eight slices, 1,025
documents, about 2,000 pages. :func:`build` generates one purpose of it:

* ``dev``: 30% of each slice's documents (rounded half up), from the committed public
  seed :data:`DEV_SEED_V1`. Anyone can regenerate it, byte for byte.
* ``test``: the other 70%. The seed is drawn inside the call from
  ``secrets.token_bytes(32)`` and is never written, logged or returned; a seed passed in
  is refused. A test split therefore cannot be regenerated: it exists only as the copy
  that is built once and locked in the model store (Task 17).
* ``train``: every document of each slice (times ``scale``; about 10,000 pages at
  ``scale=5``) from any integer seed, with layouts only from the training template seeds.

The split is by document: a multi-page document is always whole in one split. Dev and
test layouts come from the eval template seeds only (``layouts.EVAL_TEMPLATE_SEEDS``).

Randomness. Every document gets its own ``random.Random``, seeded from a SHA-256 of the
master secret, a label and the document's position; document ids come from another
SHA-256 stream. Nothing a set reveals (ids, pixels, values) is a raw output of a
generator that other documents share, so no part of the set gives away the seed.

Output, under ``<out_dir>/<purpose>/`` (reserved with ``mkdir``, built in a 0700
temporary folder, scanned by the synthetic-data scanner, then moved into place with
``testset.json`` last; an existing set is never overwritten):

* ``pages/<doc_id>/<n>.png``: the pages, ``n`` from 1;
* ``truth/<doc_id>.json``: the ground truth in the stored view of the schema
  (``confidence`` 1, ``band`` high, ``grounded`` when the field is visible, no flags);
* ``hostile/<doc_id>.json``: for a hostile document, its kind, the expected outcome
  (``hostile.Expected``) and the id of its clean twin, which is stored as a document of
  its own (role ``clean_twin``, not counted in the slice's documents or pages);
* ``testset.json`` (``mhai.evaluation.testset/1``, canonical JSON): name, version,
  purpose, schema version, generator (package version and git commit), per-slice and
  total counts, every document's id, slice, role and page count, and every other file's
  path, size and SHA-256, sorted. No seed. Its SHA-256 is the set's identity.

Document ids are 128 random bits in lowercase base32 (26 characters), drawn again until
the id passes the scanner's text rules and has no run of three digits (file names keep
every digit run to 5 or fewer).

What may leave the evaluation area. A test split, ``testset.json`` included (its
``documents`` list maps every id to its slice), never leaves the model store's evaluation
area. Only the manifest's SHA-256 and its ``slices`` and ``totals`` blocks (aggregate
counts) may be exported, for ``models/testsets.lock`` and the reports (Tasks 17 and 19).

Scanner. Each generated document is checked before it is written. A document that trips
one of :data:`REDRAW_RULES` (a generated number that happens to look like an id) is drawn
again and logged with the finding's path and rule (never a value); any other rule fails
the build (``generator_not_synthetic``), and more than 0.5% of the set's documents redrawn
(at least 1) fails it too (``too_many_redraws``). The whole folder is then scanned again.

Logs carry the purpose, version, counts, timings, the manifest's SHA-256, scanner rules
and the field paths of redrawn documents (logged before the document has an id) only; a
whole-set scanner refusal logs the finding count and rules, never a path. Building the
full test split (717 documents, about 1,400 pages) takes about 16 minutes on the build
server, one document at a time.
"""

import base64
import copy
import hashlib
import json
import logging
import math
import os
import random
import secrets
import shutil
import tempfile
import time
from collections import Counter
from collections.abc import Iterator
from dataclasses import dataclass, field
from fractions import Fraction
from importlib import metadata
from pathlib import Path
from typing import Any, Literal

from intelligence.doceval import schema
from intelligence.doceval.synth import degrade, handwriting, hostile, layouts, patients, render
from intelligence.doceval.synth import scanner as synth_scanner
from intelligence.doceval.synth.layouts import LayoutSpec
from intelligence.doceval.synth.render import RenderedDocument
from intelligence.modelstore import canonical
from intelligence.modelstore.names import check_version
from intelligence.modelstore.reasons import Refused

log = logging.getLogger(__name__)

Purpose = Literal["dev", "test", "train"]
Script = Literal["printed", "handwritten", "mixed"]

TESTSET_SCHEMA = "mhai.evaluation.testset/1"
HOSTILE_SCHEMA = "mhai.evaluation.hostile/1"
SET_NAME = "clinical_document"
DEV_SEED_V1 = 20261103  # public: the dev split is meant to be regenerated by anyone
DEV_SHARE = Fraction(3, 10)
DPI = 200
MAX_SCALE = 10
MAX_ATTEMPTS = 25  # per document: a fresh patient and layout when one cannot be placed
_PATIENT_TRIES = 500
_ID_TRIES = 1000


@dataclass(frozen=True)
class Slice:
    """One row of the composition table, and how its documents are made."""

    name: str
    title: str
    documents: int
    pages: int
    doc_types: tuple[str, ...]  # drawn uniformly per document (repeat a type to weight it)
    script: Script
    degrade: tuple[tuple[degrade.Mode, str], ...]  # (mode, profile) per document; () = none
    hostile: bool = False


_CLEAN: tuple[tuple[degrade.Mode, str], ...] = (("scan", "clean"),)

COMPOSITION_V1: tuple[Slice, ...] = (
    Slice(
        "lab_typed_clean",
        "Typed lab reports, clean scans",
        250,
        400,
        ("lab_report",),
        "printed",
        _CLEAN,
    ),
    Slice(
        "lab_typed_poor",
        "Typed lab reports, poor scans and phone photos",
        200,
        320,
        ("lab_report",),
        "printed",
        (("scan", "poor"), ("photo", "phone")),
    ),
    # No referral-letter type in schema 1.0.0: a typed clinic note stands in for one.
    Slice(
        "letters_typed",
        "Discharge summaries and referral letters (typed)",
        150,
        450,
        ("discharge_summary", "discharge_summary", "clinic_note"),
        "printed",
        _CLEAN,
    ),
    Slice(
        "medication_charts_typed",
        "Medication charts (typed tables)",
        100,
        200,
        ("medication_chart",),
        "printed",
        _CLEAN,
    ),
    Slice(
        "charts_handwritten",
        "Handwritten vitals charts and medication charts",
        100,
        200,
        ("vitals_chart", "medication_chart"),
        "handwritten",
        _CLEAN,
    ),
    Slice(
        "notes_handwritten",
        "Handwritten clinic notes",
        100,
        200,
        ("clinic_note",),
        "handwritten",
        _CLEAN,
    ),
    Slice(
        "mixed",
        "Mixed (typed form, handwritten entries)",
        75,
        150,
        layouts.DOC_TYPES,
        "mixed",
        _CLEAN,
    ),
    Slice(
        "hostile",
        "Hostile: injection text, two patients, wrong-patient labels, blank and rotated pages",
        50,
        80,
        layouts.DOC_TYPES,
        "printed",
        (),
        hostile=True,
    ),
)


class SetError(ValueError):
    """The set cannot be built. ``str()`` is a fixed code; it never carries a seed or value."""


@dataclass(frozen=True)
class SetManifest:
    """What a build returns: where the set is, its identity and its aggregate counts."""

    purpose: str
    version: str
    root: Path
    sha256: str  # of testset.json
    slices: dict[str, dict[str, int]]
    documents: int  # slice documents (clean twins not counted)
    pages: int  # slice pages (clean twins not counted)


# ---------------------------------------------------------------- counts


def _scale(scale: float) -> Fraction:
    if isinstance(scale, bool) or not isinstance(scale, int | float) or not math.isfinite(scale):
        raise SetError("bad_scale")
    if not 0 < scale <= MAX_SCALE:
        raise SetError("bad_scale")
    return Fraction(scale).limit_denominator(1_000_000)


def _half_up(x: Fraction) -> int:
    return math.floor(x + Fraction(1, 2))


def slice_documents(s: Slice, scale: float) -> int:
    """The slice's documents at ``scale`` (rounded half up, at least 1)."""
    return max(1, _half_up(s.documents * _scale(scale)))


def split_documents(s: Slice, scale: float, purpose: str) -> int:
    """Documents of ``s`` in ``purpose``: dev 30% (half up), test the rest, train all."""
    total = slice_documents(s, scale)
    dev = _half_up(total * DEV_SHARE)
    if purpose == "dev":
        return dev
    if purpose == "test":
        return total - dev
    if purpose == "train":
        return total
    raise SetError("bad_purpose")


def template_seeds(purpose: str) -> range:
    if purpose == "train":
        return layouts.TRAIN_TEMPLATE_SEEDS
    if purpose in ("dev", "test"):
        return layouts.EVAL_TEMPLATE_SEEDS
    raise SetError("bad_purpose")


# ---------------------------------------------------------------- randomness


class _Streams:
    """Independent per-document generators derived from one master secret.

    The master is kept only here, in memory, for the length of one build.
    """

    __slots__ = ("_master",)

    def __init__(self, master: bytes) -> None:
        self._master = master

    def __repr__(self) -> str:  # never show the master
        return "_Streams(<hidden>)"

    def _digest(self, *parts: str) -> bytes:
        h = hashlib.sha256(b"mhai.doceval.sets/1\n")
        h.update(self._master)
        for p in parts:
            h.update(b"\n" + p.encode("ascii"))
        return h.digest()

    def rng(self, *parts: str) -> random.Random:
        return random.Random(int.from_bytes(self._digest("rng", *parts), "big"))

    def ids(self) -> Iterator[str]:
        """Document ids: 128 bits each, from a hash stream (not a shared generator)."""
        n = 0
        while True:
            n += 1
            yield _id_text(self._digest("id", str(n))[:16])

    def forget(self) -> None:
        self._master = b""


def _id_text(raw: bytes) -> str:
    return base64.b32encode(raw).decode("ascii").rstrip("=").lower()


def _id_ok(doc_id: str) -> bool:
    if not (doc_id[0].isalpha() and doc_id[-1].isalpha()):
        return False
    run = 0
    for ch in doc_id:
        run = run + 1 if ch.isdigit() else 0
        if run >= 3:
            return False
    return all(
        synth_scanner.text_ok(t)
        for t in (doc_id, f"pages/{doc_id}/1.png", f"truth/{doc_id}.json", f"hostile/{doc_id}.json")
    )


class _IdSource:
    def __init__(self, streams: _Streams) -> None:
        self._it = streams.ids()
        self._used: set[str] = set()

    def next(self) -> str:
        for _ in range(_ID_TRIES):
            cand = next(self._it)
            if cand not in self._used and _id_ok(cand):
                self._used.add(cand)
                return cand
        raise SetError("id_exhausted")


# ---------------------------------------------------------------- generation


@dataclass
class _PageControl:
    """Keeps a slice's page count on its target by choosing the layout's page flow.

    Layouts with a new page per section give about 4 pages, the others 1 to 2. Before each
    document, pick the flow whose running mean brings the cumulative count closest to
    the target so far. Deterministic: it only looks at pages already generated.
    """

    target: Fraction
    documents: int
    done: int = 0
    pages: int = 0
    seen: dict[bool, list[int]] = field(
        default_factory=lambda: {False: [13, 10], True: [40, 10]}  # (sum, n) priors, tenths
    )

    def paging(self) -> bool:
        want = self.target * (self.done + 1) / self.documents
        best: tuple[Fraction, bool] | None = None
        for flow in (False, True):
            total, n = self.seen[flow]
            est = self.pages + Fraction(total, n)
            gap = abs(want - est)
            if best is None or gap < best[0]:
                best = (gap, flow)
        assert best is not None
        return best[1]

    def record(self, flow: bool, pages: int) -> None:
        self.done += 1
        self.pages += pages
        self.seen[flow][0] += pages * 10
        self.seen[flow][1] += 10


class _NoDocument(Exception):
    """No patient document of the wanted type came up: draw again."""


def _doc_of_type(rng: random.Random, doc_type: str) -> dict[str, Any]:
    for _ in range(_PATIENT_TRIES):
        for d in patients.generate(rng, 1)[0].documents:
            if d["document"]["type"]["value"] == doc_type:
                return d
    raise _NoDocument


def _layout(rng: random.Random, doc_type: str, purpose: Purpose, paging: bool) -> LayoutSpec:
    seeds = template_seeds(purpose)
    ts = rng.randrange(seeds.start, seeds.stop)
    pool = [
        x
        for x in layouts.for_type(doc_type, ts, purpose=purpose)
        if x.new_page_per_section == paging
    ]
    return rng.choice(pool)


@dataclass(frozen=True)
class _Made:
    pages: list[bytes]
    truth: dict[str, Any]  # model-output view with page and bbox
    twin: RenderedDocument | None = None
    hostile_kind: str | None = None
    expected: dict[str, Any] | None = None


# Failures that mean "this patient and layout do not fit": the document is drawn again.
_GEN_ERRORS = (
    render.RenderError,
    handwriting.HandwritingError,
    hostile.HostileError,
    degrade.DegradeError,
    _NoDocument,
)


def _make(s: Slice, rng: random.Random, purpose: Purpose, paging: bool, kind: str | None) -> _Made:
    doc_type = rng.choice(s.doc_types)
    gt = _doc_of_type(rng, doc_type)
    layout = _layout(rng, doc_type, purpose, paging)
    if s.hostile:
        assert kind is not None
        case = hostile.make(gt, kind, rng, layout=layout, dpi=DPI)
        return _Made(
            pages=case.hostile.pages,
            truth=case.hostile.ground_truth,
            twin=case.clean_twin,
            hostile_kind=kind,
            expected=dict(case.expected),
        )
    rendered: RenderedDocument
    if s.script == "handwritten" or s.script == "mixed":
        hand: handwriting.Mode = s.script
        rendered = handwriting.handwritten_document(gt, layout, rng, mode=hand, dpi=DPI)
    else:
        rendered = render.render(gt, layout, DPI)
    if not s.degrade:
        return _Made(pages=rendered.pages, truth=rendered.ground_truth)
    mode, profile = rng.choice(s.degrade)
    out = degrade.degrade_document(
        rendered,
        rng.getrandbits(63),
        profile,
        mode,
        source_dpi=DPI,
    )
    # out.metadata (profile parameters, a derived seed) is not written to the set.
    return _Made(pages=out.pages, truth=out.ground_truth)


def stored_view(gt: dict[str, Any], page_count: int) -> dict[str, Any]:
    """Ground truth in the stored view: confidence 1, band high, grounded when visible."""
    out = copy.deepcopy(gt)
    for _, fld in schema.iter_fields(out):
        visible = (
            fld.get("value") is not None
            and fld.get("page") is not None
            and fld.get("bbox") is not None
        )
        fld["confidence"] = 1.0
        fld["band"] = "high"
        fld["grounded"] = visible
        fld["flags"] = []
    out["document"]["page_count"] = page_count
    schema.validate_stored(out)
    return out


def _outward(box: list[float]) -> list[float | int]:
    """A box rounded outward to 4 decimals (the precision the scanner accepts)."""
    x0, y0, x1, y1 = box
    out: list[float] = [
        math.floor(x0 * 10_000) / 10_000,
        math.floor(y0 * 10_000) / 10_000,
        math.ceil(x1 * 10_000) / 10_000,
        math.ceil(y1 * 10_000) / 10_000,
    ]
    return [min(1.0, max(0.0, v)) for v in out]


def _expected_for_file(expected: dict[str, Any]) -> dict[str, Any]:
    out = copy.deepcopy(expected)
    for region in out.get("regions", []):
        region["bbox"] = _outward(region["bbox"])
    return out


# ---------------------------------------------------------------- writing


def _json_bytes(obj: Any) -> bytes:
    return json.dumps(
        obj, sort_keys=True, ensure_ascii=True, allow_nan=False, separators=(",", ":")
    ).encode("ascii")


@dataclass(frozen=True)
class _Prepared:
    """What is written for one document: its stored views, computed once."""

    pages: list[bytes]
    truth: dict[str, Any]  # stored view
    twin_pages: list[bytes] | None = None
    twin_truth: dict[str, Any] | None = None
    hostile_kind: str | None = None
    expected: dict[str, Any] | None = None  # as written: boxes rounded outward


def _prepare(made: _Made) -> _Prepared:
    if made.twin is None or made.expected is None:
        return _Prepared(pages=made.pages, truth=stored_view(made.truth, len(made.pages)))
    return _Prepared(
        pages=made.pages,
        truth=stored_view(made.truth, len(made.pages)),
        twin_pages=made.twin.pages,
        twin_truth=stored_view(made.twin.ground_truth, len(made.twin.pages)),
        hostile_kind=made.hostile_kind,
        expected=_expected_for_file(made.expected),
    )


def _hostile_record(p: _Prepared, twin_id: str) -> dict[str, Any]:
    return {
        "schema": HOSTILE_SCHEMA,
        "kind": p.hostile_kind,
        "expected": p.expected,
        "clean_twin_id": twin_id,
    }


_PLACEHOLDER_ID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"


def _findings(p: _Prepared) -> list[synth_scanner.Finding]:
    """Scanner findings on what would be written for one document (paths and rules only)."""
    out = [
        synth_scanner.Finding(f"truth#{f.path}", f.rule)
        for f in synth_scanner.check_document(p.truth)
    ]
    if p.twin_truth is not None:
        out += [
            synth_scanner.Finding(f"twin#{f.path}", f.rule)
            for f in synth_scanner.check_document(p.twin_truth)
        ]
        record = _hostile_record(p, _PLACEHOLDER_ID)
        out += [
            synth_scanner.Finding(f"hostile#{f.path}", f.rule)
            for f in synth_scanner.check_metadata(record, context="hostile")
        ]
    return out


class _Writer:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.files: list[dict[str, Any]] = []

    def put(self, rel: str, data: bytes) -> None:
        path = self.root / rel
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
        self.files.append(
            {"path": rel, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        )


def _write_doc(w: _Writer, doc_id: str, pages: list[bytes], truth: dict[str, Any]) -> None:
    for n, page in enumerate(pages, start=1):
        w.put(f"pages/{doc_id}/{n}.png", page)
    w.put(f"truth/{doc_id}.json", _json_bytes(truth))


def _package_version() -> str:
    try:
        return metadata.version("intelligence")
    except metadata.PackageNotFoundError:
        return "unknown"


# ---------------------------------------------------------------- build


def _master(purpose: str, seed: int | None) -> bytes:
    if purpose == "test":
        if seed is not None:
            raise SetError("seed_not_allowed")
        return secrets.token_bytes(32)
    if purpose == "dev":
        if seed is not None and (isinstance(seed, bool) or seed != DEV_SEED_V1):
            raise SetError("dev_seed_fixed")
        return b"dev\n" + str(DEV_SEED_V1).encode("ascii")
    if purpose == "train":
        if seed is None:
            raise SetError("seed_required")
        if isinstance(seed, bool) or not isinstance(seed, int) or seed < 0:
            raise SetError("bad_seed")
        return b"train\n" + str(seed).encode("ascii")
    raise SetError("bad_purpose")


# Scanner rules a generated document can trip by chance (a number that happens to look like
# an id). A document that trips one of these is drawn again; any other rule is a generator
# bug and fails the build. Redraws are capped per set.
REDRAW_RULES = frozenset({"mrn_like", "long_digits"})
MAX_REDRAW_SHARE = Fraction(5, 1000)


@dataclass
class _Redraws:
    cap: int
    count: int = 0
    rules: Counter[str] = field(default_factory=Counter)

    def record(self, slice_name: str, found: list[synth_scanner.Finding]) -> None:
        for f in found:  # a finding carries a field path and a rule name, never a value
            log.warning("document redrawn: slice=%s path=%s rule=%s", slice_name, f.path, f.rule)
        if any(f.rule not in REDRAW_RULES for f in found):
            raise SetError("generator_not_synthetic")
        self.count += 1
        self.rules.update(f.rule for f in found)
        if self.count > self.cap:
            raise SetError("too_many_redraws")


def _generate_slice(
    s: Slice,
    purpose: Purpose,
    count: int,
    target: Fraction,
    streams: _Streams,
    ids: _IdSource,
    w: _Writer,
    redraws: _Redraws,
) -> tuple[dict[str, int], list[dict[str, Any]]]:
    ctl = _PageControl(target=target, documents=max(1, count))
    kinds: list[str] = []
    if s.hostile:
        order = streams.rng(purpose, s.name, "kinds")
        while len(kinds) < count:
            batch = list(hostile.KINDS)
            order.shuffle(batch)
            kinds.extend(batch)
    counts = {"documents": 0, "pages": 0, "clean_twins": 0, "clean_twin_pages": 0}
    docs: list[dict[str, Any]] = []
    for i in range(count):
        kind = kinds[i] if s.hostile else None
        paging = False if kind == "blank" else ctl.paging()
        made: _Prepared | None = None
        for attempt in range(MAX_ATTEMPTS):
            rng = streams.rng(purpose, s.name, str(i), str(attempt))
            try:
                cand = _prepare(_make(s, rng, purpose, paging, kind))
            except _GEN_ERRORS:
                continue
            found = _findings(cand)
            if found:
                redraws.record(s.name, found)
                continue
            made = cand
            break
        if made is None:
            raise SetError("generation_failed")
        doc_id = ids.next()
        _write_doc(w, doc_id, made.pages, made.truth)
        ctl.record(paging, len(made.pages))
        counts["documents"] += 1
        counts["pages"] += len(made.pages)
        docs.append({"id": doc_id, "slice": s.name, "role": "document", "pages": len(made.pages)})
        if made.twin_pages is not None and made.twin_truth is not None:
            twin_id = ids.next()
            _write_doc(w, twin_id, made.twin_pages, made.twin_truth)
            counts["clean_twins"] += 1
            counts["clean_twin_pages"] += len(made.twin_pages)
            docs.append(
                {
                    "id": twin_id,
                    "slice": s.name,
                    "role": "clean_twin",
                    "pages": len(made.twin_pages),
                    "twin_of": doc_id,
                }
            )
            w.put(f"hostile/{doc_id}.json", _json_bytes(_hostile_record(made, twin_id)))
    return counts, docs


def _put_file(path: Path, data: bytes) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as fh:
        fh.write(data)


def build(
    purpose: Purpose,
    seed: int | None,
    out_dir: str | Path,
    scale: float = 1.0,
    *,
    version: str,
) -> SetManifest:
    """Build one purpose of set v1 into ``<out_dir>/<purpose>/`` and return its manifest.

    ``seed``: ``None`` for ``test`` (anything else is refused, ``seed_not_allowed``);
    ``None`` or :data:`DEV_SEED_V1` for ``dev``; any non-negative integer for ``train``.
    Raises :class:`SetError` with a fixed code and no chained exception (nothing from
    inside the build, where the seed lives, travels with the error). Never overwrites.

    ``<out_dir>/<purpose>`` is reserved first with ``mkdir`` (so two builds cannot race
    for it); the set is generated and scanned in a temporary folder next to it, then its
    files are moved in, ``testset.json`` last. On any failure the reserved folder and the
    temporary one are removed. A folder without ``testset.json`` is not a set.
    """
    if purpose not in ("dev", "test", "train"):
        raise SetError("bad_purpose")
    try:
        check_version(version)
    except (Refused, TypeError):
        raise SetError("bad_version") from None
    sc = _scale(scale)
    streams = _Streams(_master(purpose, seed))
    started = time.monotonic()
    base = Path(out_dir)
    final = base / purpose
    tmp: Path | None = None
    reserved = False
    done = False
    error: str | None = None
    try:
        base.mkdir(mode=0o700, parents=True, exist_ok=True)
        try:
            os.mkdir(final, 0o700)
        except FileExistsError:
            raise SetError("exists") from None
        reserved = True
        tmp = Path(tempfile.mkdtemp(prefix=f".{purpose}-", dir=base))
        w = _Writer(tmp)
        ids = _IdSource(streams)
        slices: dict[str, dict[str, int]] = {}
        documents: list[dict[str, Any]] = []
        total = sum(split_documents(s, scale, purpose) for s in COMPOSITION_V1)
        redraws = _Redraws(cap=max(1, math.floor(total * MAX_REDRAW_SHARE)))
        for s in COMPOSITION_V1:
            count = split_documents(s, scale, purpose)
            target = s.pages * sc * count / slice_documents(s, scale)
            counts, docs = _generate_slice(s, purpose, count, target, streams, ids, w, redraws)
            slices[s.name] = counts
            documents.extend(docs)
        streams.forget()
        manifest = _manifest(purpose, version, slices, documents, w.files)
        raw = canonical.dumps(manifest)
        _put_file(tmp / "testset.json", raw)
        scan = synth_scanner.scan_set(tmp)
        if not scan.ok:
            # The count and the rules only: a finding's path names a file, and file names
            # carry document ids (never logged for any purpose, the test split above all).
            log.error(
                "set refused by the scanner: findings=%d rules=%s",
                len(scan.findings),
                sorted({f.rule for f in scan.findings}),
            )
            raise SetError("scanner_refused")
        names = sorted(os.listdir(tmp), key=lambda n: n == "testset.json")
        for name in names:
            os.rename(tmp / name, final / name)
        os.rmdir(tmp)
        tmp = None
        done = True
    except SetError as e:
        error = str(e)
    except OSError:
        error = "write_failed"
    except Exception:  # noqa: BLE001 - a fixed code only: nothing from inside goes out
        error = "generation_failed"
    finally:
        streams.forget()
        if tmp is not None:
            shutil.rmtree(tmp, ignore_errors=True)
        if reserved and not done:
            shutil.rmtree(final, ignore_errors=True)
    if error is not None:
        raise SetError(error)  # raised here, outside the handler: no chained context
    result = SetManifest(
        purpose=purpose,
        version=version,
        root=final,
        sha256=hashlib.sha256(raw).hexdigest(),
        slices=slices,
        documents=manifest["totals"]["documents"],
        pages=manifest["totals"]["pages"],
    )
    log.info(
        "set built: purpose=%s version=%s documents=%d pages=%d files=%d seconds=%.1f "
        "redrawn=%d redraw_rules=%s sha256=%s",
        purpose,
        version,
        result.documents,
        result.pages,
        len(manifest["files"]),
        time.monotonic() - started,
        redraws.count,
        sorted(redraws.rules.items()),
        result.sha256,
    )
    return result


def _manifest(
    purpose: str,
    version: str,
    slices: dict[str, dict[str, int]],
    documents: list[dict[str, Any]],
    files: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "schema": TESTSET_SCHEMA,
        "name": SET_NAME,
        "version": version,
        "purpose": purpose,
        "extraction_schema_version": schema.SCHEMA_VERSION,
        "generator": {"package": _package_version(), "commit": synth_scanner.current_commit()},
        "slices": slices,
        "totals": {
            "documents": sum(c["documents"] for c in slices.values()),
            "pages": sum(c["pages"] for c in slices.values()),
            "clean_twins": sum(c["clean_twins"] for c in slices.values()),
            "clean_twin_pages": sum(c["clean_twin_pages"] for c in slices.values()),
        },
        "documents": sorted(documents, key=lambda d: d["id"]),
        "files": sorted(files, key=lambda f: f["path"]),
    }


__all__ = [
    "COMPOSITION_V1",
    "DEV_SEED_V1",
    "SetError",
    "SetManifest",
    "Slice",
    "build",
    "slice_documents",
    "split_documents",
    "stored_view",
    "template_seeds",
]
