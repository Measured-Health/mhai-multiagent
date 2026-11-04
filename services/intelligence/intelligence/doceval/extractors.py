"""The extractor interface, and stub extractors for tests and the CI rehearsal only.

An :class:`Extractor` gets the page images of one document (PNG bytes, in page order)
and a deadline, and returns an :data:`ExtractionResult`: either :class:`Extracted`, a
document in the *stored* view of ``mhai.extraction.clinical_document`` (confidence,
band, grounding and flags included: in phase 0 the extractor fills them, and phase 1's
pipeline computes them, design section 4.5), or :class:`Refused` with a fixed reason
from :data:`REFUSAL_REASONS`. An extractor never sees a path, a document id or ground
truth; the runner (:mod:`intelligence.doceval.runner`) checks everything it returns.

``Extracted.document`` may be a mapping, or its JSON text as ``bytes`` or ``str`` (an
extractor over an HTTP endpoint can hand over the body as received; the runner parses
it under a size cap).

The stubs:

* :class:`OracleExtractor` returns the truth, with controlled drops, wrong values and
  confidences, so every metric can be driven to a known value. It identifies a
  document by the SHA-256 of its pages, so it needs the set's ``truth`` folder (and the
  ``pages`` folder beside it). It is a test fixture: it reads ground truth by design.
* :class:`RefusingExtractor` refuses every document with one reason.
* :class:`SlowExtractor` sleeps, to exercise the runner's deadline.
* :class:`InvalidExtractor` returns bad JSON, a schema violation or too much output, or
  raises, to exercise the runner's checks (P22).
"""

import copy
import datetime
import hashlib
import json
import math
import random
import re
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from intelligence.doceval import schema

REFUSAL_REASONS: tuple[str, ...] = (
    "multiple_patients",
    "unreadable",
    "no_text",
    "no_patient_identifiers",
    "internal",
)
INVALID_KINDS: tuple[str, ...] = ("bad_json", "schema_violation", "oversize", "raises")
BAND_HIGH = 0.95
BAND_MEDIUM = 0.80
_OVERSIZE_BYTES = 2_000_000 + 4096
_MAX_TRUTH_BYTES = 8_000_000

_NAME = re.compile(r"[a-z0-9][a-z0-9-]{1,62}", re.ASCII)
_TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._@#:+/-]{0,127}", re.ASCII)


@dataclass(frozen=True)
class ExtractorInfo:
    """What is being evaluated. Names and versions only, never a path or credential."""

    name: str
    model_versions: dict[str, str]
    engine: str
    hardware: str


def check_info(info: object) -> ExtractorInfo:
    """Refuse an info that is not plain short tokens (``bad_extractor_info``)."""
    if not isinstance(info, ExtractorInfo):
        raise ValueError("bad_extractor_info")
    ok = isinstance(info.name, str) and _NAME.fullmatch(info.name) is not None
    ok = ok and isinstance(info.model_versions, dict) and len(info.model_versions) <= 16
    for k, v in info.model_versions.items() if ok else ():
        ok = ok and all(isinstance(x, str) and _TOKEN.fullmatch(x) is not None for x in (k, v))
    for x in (info.engine, info.hardware):
        ok = ok and isinstance(x, str) and _TOKEN.fullmatch(x) is not None
    if not ok:
        raise ValueError("bad_extractor_info")
    return info


@dataclass(frozen=True)
class Extracted:
    """A stored-view document (a mapping, or JSON text), with the extractor's own timings.

    ``page_seconds``: seconds per page as the extractor measured them, one per page.
    ``gpu_peak_bytes``: peak accelerator memory, or ``None`` without one.
    """

    document: Any
    page_seconds: tuple[float, ...]
    gpu_peak_bytes: int | None = None


@dataclass(frozen=True)
class Refused:
    """The extractor declined the document; ``reason`` is one of :data:`REFUSAL_REASONS`."""

    reason: str


ExtractionResult = Extracted | Refused


class Extractor(Protocol):
    def info(self) -> ExtractorInfo: ...

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> ExtractionResult: ...


def band(confidence: float) -> str:
    """Design section 4.5: high >= 0.95, medium 0.80 to 0.95, low < 0.80."""
    if confidence >= BAND_HIGH:
        return "high"
    if confidence >= BAND_MEDIUM:
        return "medium"
    return "low"


def _pages_key(pages: Sequence[bytes]) -> str:
    h = hashlib.sha256(b"mhai.doceval.oracle/1\n")
    for page in pages:
        h.update(hashlib.sha256(page).digest())
    return h.hexdigest()


def _rate(x: float) -> float:
    if isinstance(x, bool) or not isinstance(x, int | float) or not 0.0 <= x <= 1.0:
        raise ValueError("bad_rate")
    return float(x)


def honest_confidence(correct: bool, rng: random.Random) -> float:
    """The default confidence model: 1 for an unchanged field, 0 for a changed one."""
    return 1.0 if correct else 0.0


ConfidenceModel = Callable[[bool, random.Random], float]


def _enum_values(pattern: str) -> list[str]:
    node: Any = schema.STORED
    for part in pattern.split("."):
        name = part.removesuffix("[]")
        node = node["properties"][name]
        if part.endswith("[]"):
            node = node["items"]
    ref = node["$ref"].rsplit("/", 1)[-1]
    values = schema.STORED["$defs"][ref]["properties"]["value"]["enum"]
    return [v for v in values if isinstance(v, str)]


def _wrong(ftype: str, pattern: str, value: Any, rng: random.Random) -> Any:
    """A value of the same type that is not equal to ``value`` after normalisation."""
    if ftype == "numeric_lab_value":
        return value + 1 if isinstance(value, int | float) else 1
    if ftype == "date":
        return (datetime.date.fromisoformat(value) + datetime.timedelta(days=1)).isoformat()
    if ftype == "datetime":
        text = value[:10] + "T" + value[11:]
        return (
            datetime.datetime.fromisoformat(text.replace("Z", "+00:00"))
            + datetime.timedelta(days=1)
        ).isoformat()
    if ftype == "enum":
        others = [v for v in _enum_values(pattern) if v != value]
        return rng.choice(others)
    return ("Q" + value)[:2000] if isinstance(value, str) else "Q"


@dataclass
class OracleExtractor:
    """The truth, with controlled errors. Each field with a value is dropped with
    probability ``drop`` (value and raw text set to null), else made wrong with
    probability ``corrupt``. ``confidence_model(correct, rng)`` sets each field's
    confidence (the band follows); ``grounded`` is true for an unchanged visible value.
    Draws come from ``rng`` in document-call order and field order, so a run in id order
    is deterministic.
    """

    truth_dir: Path
    rng: random.Random
    drop: float = 0.0
    corrupt: float = 0.0
    confidence_model: ConfidenceModel = honest_confidence
    _index: dict[str, Path] = field(init=False, repr=False)

    def __post_init__(self) -> None:
        self.drop = _rate(self.drop)
        self.corrupt = _rate(self.corrupt)
        self.truth_dir = Path(self.truth_dir)
        pages_dir = self.truth_dir.parent / "pages"
        self._index = {}
        for truth in sorted(self.truth_dir.glob("*.json")):
            folder = pages_dir / truth.stem
            files = sorted(folder.glob("*.png"), key=lambda p: int(p.stem))
            key = _pages_key([p.read_bytes() for p in files])
            self._index.setdefault(key, truth)  # identical page sets: the first id wins

    def info(self) -> ExtractorInfo:
        return ExtractorInfo("oracle-stub", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> ExtractionResult:
        started = time.perf_counter()
        path = self._index.get(_pages_key(pages))
        if path is None:
            return Refused("internal")
        raw = path.read_bytes()
        if len(raw) > _MAX_TRUTH_BYTES:
            return Refused("internal")
        doc: dict[str, Any] = json.loads(raw)
        out = copy.deepcopy(doc)
        for fpath, fld in schema.iter_fields(out):
            pattern = schema.path_pattern(fpath)
            ftype = schema.FIELD_TYPES[pattern]
            changed = False
            if fld.get("value") is not None:
                if self.drop and self.rng.random() < self.drop:
                    fld["value"] = None
                    fld["raw_text"] = None
                    changed = True
                elif self.corrupt and self.rng.random() < self.corrupt:
                    fld["value"] = _wrong(ftype, pattern, fld["value"], self.rng)
                    changed = True
            c = float(self.confidence_model(not changed, self.rng))
            if not math.isfinite(c) or not 0.0 <= c <= 1.0:
                raise ValueError("bad_confidence")
            fld["confidence"] = c
            fld["band"] = band(c)
            fld["grounded"] = (
                not changed
                and fld.get("value") is not None
                and fld.get("page") is not None
                and fld.get("bbox") is not None
            )
            fld["flags"] = []
        n = max(1, len(pages))
        per_page = (time.perf_counter() - started) / n
        return Extracted(out, tuple(per_page for _ in range(n)), None)


@dataclass(frozen=True)
class RefusingExtractor:
    reason: str

    def __post_init__(self) -> None:
        if self.reason not in REFUSAL_REASONS:
            raise ValueError("bad_reason")

    def info(self) -> ExtractorInfo:
        return ExtractorInfo("refusing-stub", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> ExtractionResult:
        return Refused(self.reason)


@dataclass(frozen=True)
class SlowExtractor:
    """Sleeps ``seconds``, then refuses (``internal``)."""

    seconds: float

    def info(self) -> ExtractorInfo:
        return ExtractorInfo("slow-stub", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> ExtractionResult:
        time.sleep(self.seconds)
        return Refused("internal")


@dataclass(frozen=True)
class InvalidExtractor:
    """``bad_json``, ``schema_violation``, ``oversize`` or ``raises``."""

    kind: str

    def __post_init__(self) -> None:
        if self.kind not in INVALID_KINDS:
            raise ValueError("bad_kind")

    def info(self) -> ExtractorInfo:
        return ExtractorInfo("invalid-stub", {}, "stub", "cpu")

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> ExtractionResult:
        seconds = tuple(0.0 for _ in pages)
        if self.kind == "raises":
            raise RuntimeError("invalid stub")
        if self.kind == "bad_json":
            return Extracted(b'{"schema_version": "1.0.0", "document": ', seconds)
        if self.kind == "oversize":
            return Extracted(b'{"pad":"' + b"a" * _OVERSIZE_BYTES + b'"}', seconds)
        return Extracted({"schema_version": "1.0.0", "unexpected": True}, seconds)


__all__ = [
    "BAND_HIGH",
    "BAND_MEDIUM",
    "INVALID_KINDS",
    "REFUSAL_REASONS",
    "ConfidenceModel",
    "Extracted",
    "ExtractionResult",
    "Extractor",
    "ExtractorInfo",
    "InvalidExtractor",
    "OracleExtractor",
    "Refused",
    "RefusingExtractor",
    "SlowExtractor",
    "band",
    "check_info",
    "honest_confidence",
]
