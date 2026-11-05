"""mhai-tiny-extractor: a tiny safetensors "model" for rehearsals and tests (numpy only).

It is not a model of anything and scores badly. It exists so that loading, hashing,
evaluation, the report and warm-up run on genuine safetensors bytes without real
weights, and so that changing one weight changes the warm-up output.

A run directory in Hugging Face layout (see :mod:`intelligence.modelstore.fixtures`):
``config.json`` (``architectures: ["MhaiTinyExtractor"]``, ``model_type: "mhai-tiny"``),
``model.safetensors`` (the float32 tensors in :data:`TENSOR_SHAPES`, well under a
kilobyte of data), tokenizer, preprocessor and generation configs, and
``training_manifest.json``. :class:`TinyExtractor` reads only ``config.json`` and
``model.safetensors``, with :func:`safetensors.numpy.load` on the bytes already read and capped (no torch, no other
loader).

What it computes, from page pixels only (it never sees a path, an id or ground truth):
each page is decoded (PNG only), reduced to 32 x 32 grey levels and summarised by eight
statistics (mean, spread, dark share, the means of each half, horizontal edge energy),
averaged over the pages. Those features, scaled by ``features.scale``, go through three
linear heads: ``type`` (softmax over the document types) picks ``document.type``,
``sex`` (softmax) picks ``patient.sex``, and ``language`` (a sigmoid) decides whether
``document.language`` is ``"en"`` or absent. Each picked field's confidence is the head's
probability rounded to 6 decimals, and its band is :func:`extractors.band` of that
confidence. ``document.page_count`` is the number of pages it was given. Every other
field is absent (value null) with confidence 0, band ``low``. Nothing is grounded (no
boxes) and no flags are set. Sums use :func:`math.fsum`, so the output is the same on
every machine.
"""

import io
import math
import time
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image
from safetensors.numpy import load as load_safetensors

from intelligence.doceval import extractors, schema
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

ARCHITECTURE = "MhaiTinyExtractor"
MODEL_TYPE = "mhai-tiny"
NAME = "mhai-tiny-extractor"
FEATURES = 8
IMAGE_SIDE = 32
LANGUAGE = "en"


def _enum(def_name: str) -> tuple[str, ...]:
    values = schema.STORED["$defs"][def_name]["properties"]["value"]["enum"]
    return tuple(v for v in values if isinstance(v, str))


DOCUMENT_TYPES: tuple[str, ...] = _enum("F_document_type")
SEXES: tuple[str, ...] = _enum("F_sex")
TENSOR_SHAPES: dict[str, tuple[int, ...]] = {
    "features.scale": (FEATURES,),
    "type.weight": (len(DOCUMENT_TYPES), FEATURES),
    "type.bias": (len(DOCUMENT_TYPES),),
    "sex.weight": (len(SEXES), FEATURES),
    "sex.bias": (len(SEXES),),
    "language.weight": (FEATURES,),
    "language.bias": (1,),
}

_MAX_CONFIG_BYTES = 64 * 1024
_MAX_WEIGHTS_BYTES = 1024 * 1024
_MAX_PIXELS = 40_000_000
_MAX_PAGES = 10_000


class TinyInvalid(ValueError):
    """The run directory is not a tiny model. ``str()`` is ``tiny_invalid`` only."""

    def __init__(self) -> None:
        super().__init__("tiny_invalid")


def _read_capped(path: Path, cap: int) -> bytes:
    st = path.lstat()
    if not path.is_file() or path.is_symlink() or st.st_size > cap:
        raise TinyInvalid()
    with path.open("rb") as fh:
        raw = fh.read(cap + 1)
    if len(raw) > cap:
        raise TinyInvalid()
    return raw


def _load(run_dir: Path) -> dict[str, list[list[float]]]:
    """The weights as rows of Python floats (vectors are one row)."""
    try:
        config = canonical.loads_strict(
            _read_capped(run_dir / "config.json", _MAX_CONFIG_BYTES), _MAX_CONFIG_BYTES
        )
        weights_path = run_dir / "model.safetensors"
        # Type, size and symlink checks, then parse exactly the bytes that were checked.
        tensors = load_safetensors(_read_capped(weights_path, _MAX_WEIGHTS_BYTES))
    except (OSError, Refused, ValueError) as exc:
        raise TinyInvalid() from exc
    except Exception as exc:  # the safetensors library's own error types
        raise TinyInvalid() from exc
    if not isinstance(config, dict):
        raise TinyInvalid()
    if config.get("architectures") != [ARCHITECTURE] or config.get("model_type") != MODEL_TYPE:
        raise TinyInvalid()
    if set(tensors) != set(TENSOR_SHAPES):
        raise TinyInvalid()
    out: dict[str, list[list[float]]] = {}
    for name, shape in TENSOR_SHAPES.items():
        t = tensors[name]
        if t.dtype != np.float32 or tuple(t.shape) != shape or not bool(np.isfinite(t).all()):
            raise TinyInvalid()
        rows = t.reshape(shape[0], -1) if len(shape) == 2 else t.reshape(1, -1)
        out[name] = [[float(x) for x in row] for row in rows.tolist()]
    return out


class Unreadable(ValueError):
    """A page (or page list) the tiny model cannot read. ``str()`` is ``unreadable``."""

    def __init__(self) -> None:
        super().__init__("unreadable")


def _mean(values: Any) -> float:
    flat = [float(x) for x in np.asarray(values, dtype=np.float64).ravel().tolist()]
    return math.fsum(flat) / len(flat)


def page_features(page: bytes) -> list[float]:
    """Eight statistics of one page (sums by :func:`math.fsum`), or :class:`Unreadable`."""
    try:
        with Image.open(io.BytesIO(page), formats=["PNG"]) as img:
            w, h = img.size
            if w < 1 or h < 1 or w * h > _MAX_PIXELS:
                raise Unreadable()
            grey = img.convert("L").resize((IMAGE_SIDE, IMAGE_SIDE), Image.Resampling.BOX)
    except Exception as exc:  # Pillow raises many types for a bad image
        raise Unreadable() from exc
    a = np.asarray(grey, dtype=np.float64) / 255.0
    half = IMAGE_SIDE // 2
    mean = _mean(a)
    stats = [
        mean,
        math.sqrt(_mean((a - mean) ** 2)),
        _mean(a < 0.5),
        _mean(a[:half]),
        _mean(a[half:]),
        _mean(a[:, :half]),
        _mean(a[:, half:]),
        _mean(np.abs(np.diff(a, axis=1))),
    ]
    return [round(float(x), 9) for x in stats]


def _dot(w: Sequence[float], z: Sequence[float]) -> float:
    return math.fsum(a * b for a, b in zip(w, z, strict=True))


def _softmax(logits: Sequence[float]) -> list[float]:
    top = max(logits)
    exps = [math.exp(x - top) for x in logits]
    total = math.fsum(exps)
    return [e / total for e in exps]


def _sigmoid(x: float) -> float:
    if x >= 0:
        return 1.0 / (1.0 + math.exp(-x))
    e = math.exp(x)
    return e / (1.0 + e)


def _blank(node: Mapping[str, Any]) -> Any:
    """A stored-view skeleton: every field absent, arrays empty."""
    ref = node.get("$ref")
    if isinstance(ref, str):
        name = ref.rsplit("/", 1)[-1]
        if name.startswith("F_"):
            return _field(None, 0.0)
        return _blank(schema.STORED["$defs"][name])
    if "const" in node:
        return node["const"]
    kind = node.get("type")
    if kind == "object":
        props = node["properties"]
        return {k: _blank(props[k]) for k in node["required"]}
    if kind == "array":
        return []
    if kind == "integer":
        return 1
    raise RuntimeError("unexpected schema node")


def _field(value: str | None, confidence: float) -> dict[str, Any]:
    return {
        "value": value,
        "raw_text": None,
        "page": 1 if value is not None else None,
        "bbox": None,
        "confidence": confidence,
        "band": extractors.band(confidence),
        "grounded": False,
        "flags": [],
    }


class TinyExtractor:
    """The tiny model as an :class:`~intelligence.doceval.extractors.Extractor`.

    ``model_versions`` comes from the caller (the verified pin); the run directory
    never names its own version.
    """

    def __init__(self, path: Path | str, *, model_versions: Mapping[str, str] | None = None):
        self._w = _load(Path(path))
        self._info = extractors.check_info(
            extractors.ExtractorInfo(NAME, dict(model_versions or {}), "numpy", "cpu")
        )

    def info(self) -> extractors.ExtractorInfo:
        return self._info

    def predict(self, pages: Sequence[bytes]) -> dict[str, Any]:
        """The stored-view document for ``pages`` (raises :class:`Unreadable`)."""
        if not 1 <= len(pages) <= _MAX_PAGES:
            raise Unreadable()
        per_page = [page_features(p) for p in pages]
        mean = [math.fsum(col) / len(per_page) for col in zip(*per_page, strict=True)]
        z = [a * b for a, b in zip(mean, self._w["features.scale"][0], strict=True)]

        def head(prefix: str) -> list[float]:
            rows = self._w[prefix + ".weight"]
            bias = self._w[prefix + ".bias"][0]
            return _softmax([_dot(r, z) + b for r, b in zip(rows, bias, strict=True)])

        doc: dict[str, Any] = _blank(schema.STORED)
        doc["document"]["page_count"] = len(pages)
        p_type = head("type")
        i = p_type.index(max(p_type))
        doc["document"]["type"] = _field(DOCUMENT_TYPES[i], round(p_type[i], 6))
        p_sex = head("sex")
        j = p_sex.index(max(p_sex))
        doc["patient"]["sex"] = _field(SEXES[j], round(p_sex[j], 6))
        p_lang = _sigmoid(_dot(self._w["language.weight"][0], z) + self._w["language.bias"][0][0])
        if p_lang >= 0.5:
            doc["document"]["language"] = _field(LANGUAGE, round(p_lang, 6))
        schema.validate_stored(doc)
        return doc

    def extract(self, pages: Sequence[bytes], *, deadline_s: float) -> extractors.ExtractionResult:
        started = time.perf_counter()
        try:
            doc = self.predict(pages)
        except Unreadable:  # only unreadable input; a schema fault is a bug and raises
            return extractors.Refused("unreadable")
        n = len(pages)
        per_page = (time.perf_counter() - started) / n
        return extractors.Extracted(doc, tuple(per_page for _ in range(n)), None)


__all__ = [
    "ARCHITECTURE",
    "DOCUMENT_TYPES",
    "FEATURES",
    "MODEL_TYPE",
    "NAME",
    "SEXES",
    "TENSOR_SHAPES",
    "TinyExtractor",
    "TinyInvalid",
    "Unreadable",
    "page_features",
]
