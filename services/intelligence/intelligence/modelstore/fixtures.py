"""Tiny synthetic run directories for tests, CI and the intelligence lead.

:func:`make_run` writes one run in Hugging Face layout: the tiny model of
:mod:`intelligence.doceval.tiny` (good runs), or a good run with one defect (every
other variant), so each file check and registry check has a fixture that trips it.
The output depends only on ``variant`` and ``seed``: the same call writes the same
bytes. Nothing here loads a model; the files of the bad variants are inert bytes that
are never read as anything but bytes.

Variants and the refusal each is meant to trigger (:data:`EXPECTED_REASONS`):

* ``good``, ``good_v2`` (different weights): none.
* ``pickle`` (adds ``pytorch_model.bin``), ``pkl`` (adds ``optimizer.pkl``):
  ``pickle_refused``. ``py_file`` (adds ``modeling_x.py``), ``auto_map``
  (``config.json`` with ``auto_map``), ``remote_code_tokenizer`` (``tokenizer_config.json``
  with ``auto_map``): ``code_refused``.
* ``bad_header_overlap`` (two tensors share bytes), ``bad_header_past_end`` (the last
  tensor ends past the data), ``bad_dtype`` (a dtype outside the allow-list),
  ``trailing_bytes`` (bytes after the last tensor): ``safetensors_invalid``.
* ``symlink`` (``generation_config.json`` is a symlink to ``config.json``, inside the
  run): ``file_not_allowed``.
* ``unknown_dataset`` (:data:`UNKNOWN_DATASET`): ``dataset_unknown``;
  ``non_synthetic_dataset`` (:data:`NON_SYNTHETIC_DATASET`, which a test registry lists
  with a non-synthetic kind): ``dataset_not_synthetic``; ``no_training_manifest``:
  ``training_manifest_missing``; ``bad_licence`` (:data:`BAD_LICENCE`):
  ``licence_not_allowed``; ``unpinned_base`` (revision ``main``): ``revision_unpinned``.

:func:`warmup_page` renders the fixed warm-up page (``testdata/models/warmup/page.png``
and its truth, ``page.json``) with the synthetic renderer: one synthetic patient's first
document that fits on one page, from a fixed seed, at a low resolution so the PNG is small.
The committed files are its output; a test renders it again and compares. The names have
no digits: the synthetic-data scanner reads ``page-1`` as an identifier (letters, a hyphen,
digits) and refuses it.
"""

import hashlib
import json
import os
import random
import struct
from pathlib import Path
from typing import Any

import numpy as np
from safetensors.numpy import save_file

from intelligence.doceval import tiny
from intelligence.modelstore import canonical, names
from intelligence.modelstore.reasons import Refused

TRAINING_SCHEMA = "mhai.models.training/1"
MODEL_NAME = tiny.NAME
ROLE = "extract"
TRAINED_BY = "mhai-fixture-bot"
TRAIN_DATASET = "clinical_document-train@1.0.0"
UNKNOWN_DATASET = "clinical_document-unknown@9.9.9"
NON_SYNTHETIC_DATASET = "clinical_document-deidentified-pilot@1.0.0"
BASE_REPO = "mhai-fixtures/mhai-tiny-base"
LICENCE = "Apache-2.0"
BAD_LICENCE = "LicenseRef-fixture-proprietary"
CREATED_AT = "2026-11-03T00:00:00Z"

EXPECTED_REASONS: dict[str, str | None] = {
    "good": None,
    "good_v2": None,
    "pickle": "pickle_refused",
    "pkl": "pickle_refused",
    "py_file": "code_refused",
    "auto_map": "code_refused",
    "remote_code_tokenizer": "code_refused",
    "bad_header_overlap": "safetensors_invalid",
    "bad_header_past_end": "safetensors_invalid",
    "bad_dtype": "safetensors_invalid",
    "trailing_bytes": "safetensors_invalid",
    "symlink": "file_not_allowed",
    "unknown_dataset": "dataset_unknown",
    "non_synthetic_dataset": "dataset_not_synthetic",
    "no_training_manifest": "training_manifest_missing",
    "bad_licence": "licence_not_allowed",
    "unpinned_base": "revision_unpinned",
}
VARIANTS: tuple[str, ...] = tuple(EXPECTED_REASONS)

WARMUP_SEED = 20261105
WARMUP_DPI = 72
_WARMUP_TRIES = 64

_MAX_SEED = 2**32 - 1
# Inert stand-ins: never deserialised by anything, only named and refused.
_BIN_BYTES = b"\x80\x04\x95fixture: inert bytes, never loaded\x00."
_PY_TEXT = b"# fixture: never imported or run\n"


def weights(variant: str, seed: int) -> dict[str, np.ndarray[Any, np.dtype[np.float32]]]:
    """The tiny model's tensors for ``seed`` (``good_v2`` draws from another stream)."""
    stream = 2 if variant == "good_v2" else 1
    rng = np.random.default_rng([seed, stream])
    out: dict[str, np.ndarray[Any, np.dtype[np.float32]]] = {}
    for name in sorted(tiny.TENSOR_SHAPES):
        values = rng.standard_normal(tiny.TENSOR_SHAPES[name])
        if name == "features.scale":
            values = np.abs(values) * 4.0 + 1.0
        out[name] = values.astype(np.float32)
    return out


def _json(path: Path, obj: Any) -> None:
    path.write_bytes(canonical.dumps(obj))


def _split(raw: bytes) -> tuple[dict[str, Any], bytes]:
    (n,) = struct.unpack("<Q", raw[:8])
    header: dict[str, Any] = json.loads(raw[8 : 8 + n])
    return header, raw[8 + n :]


def _pack(header: dict[str, Any], data: bytes) -> bytes:
    text = json.dumps(header, separators=(",", ":"), sort_keys=True).encode("ascii")
    text += b" " * (-len(text) % 8)
    return struct.pack("<Q", len(text)) + text + data


def _tensors_in_order(header: dict[str, Any]) -> list[str]:
    names = [k for k in header if k != "__metadata__"]
    return sorted(names, key=lambda k: header[k]["data_offsets"][0])


def _corrupt_safetensors(path: Path, variant: str) -> None:
    header, data = _split(path.read_bytes())
    names = _tensors_in_order(header)
    if variant == "bad_header_overlap":
        # The second tensor starts 4 bytes inside the first; everything after it moves
        # back by 4 and the data is 4 bytes shorter, so overlap is the only defect.
        for name in names[1:]:
            start, end = header[name]["data_offsets"]
            header[name]["data_offsets"] = [start - 4, end - 4]
        first_end = header[names[0]]["data_offsets"][1]
        data = data[: first_end - 4] + data[first_end:]
    elif variant == "bad_header_past_end":
        data = data[:-4]
    elif variant == "bad_dtype":
        header[names[0]]["dtype"] = "F128"
    elif variant == "trailing_bytes":
        data = data + b"\x00" * 8
    path.write_bytes(_pack(header, data))


def _training_manifest(variant: str, seed: int, name: str, role: str) -> dict[str, Any]:
    revision = hashlib.sha256(f"mhai-fixture-base-{seed}".encode("ascii")).hexdigest()[:40]
    dataset = {"unknown_dataset": UNKNOWN_DATASET, "non_synthetic_dataset": NON_SYNTHETIC_DATASET}
    return {
        "schema": TRAINING_SCHEMA,
        "name": name,
        "role": role,
        "trained_by": TRAINED_BY,
        "datasets": [dataset.get(variant, TRAIN_DATASET)],
        "base_model": {
            "upstream": BASE_REPO,
            "revision": "main" if variant == "unpinned_base" else revision,
            "licence": BAD_LICENCE if variant == "bad_licence" else LICENCE,
        },
        "framework": {"numpy": "2", "safetensors": "0.8"},
        "notebook_metrics": {"steps": 0, "seed": seed},
        "created_at": CREATED_AT,
    }


def make_run(
    dir: Path | str,
    variant: str,
    seed: int,
    *,
    name: str = MODEL_NAME,
    role: str = ROLE,
) -> Path:
    """Write the run for ``variant`` and ``seed`` into ``dir`` (created; must not exist).

    ``name`` and ``role`` go into ``training_manifest.json``. Raises
    ``Refused("fixture_name")`` for an unknown variant or a bad seed, and
    ``Refused("invalid_name")`` for a bad name or role, before anything is written.
    """
    if variant not in EXPECTED_REASONS:
        raise Refused("fixture_name")
    names.check_name(name)
    names.check_role(role)
    if isinstance(seed, bool) or not isinstance(seed, int) or not 0 <= seed <= _MAX_SEED:
        raise Refused("fixture_name")
    root = Path(dir)
    root.mkdir(parents=True, exist_ok=False)

    config: dict[str, Any] = {
        "architectures": [tiny.ARCHITECTURE],
        "model_type": tiny.MODEL_TYPE,
        "features": tiny.FEATURES,
        "image_side": tiny.IMAGE_SIDE,
        "torch_dtype": "float32",
    }
    if variant == "auto_map":
        config["auto_map"] = {"AutoModel": "modeling_x.MhaiTinyExtractor"}
    _json(root / "config.json", config)

    tokenizer_config: dict[str, Any] = {
        "tokenizer_class": "PreTrainedTokenizerFast",
        "model_max_length": 512,
    }
    if variant == "remote_code_tokenizer":
        tokenizer_config["auto_map"] = {"AutoTokenizer": ["tokenization_x.MhaiTokenizer", None]}
    _json(root / "tokenizer_config.json", tokenizer_config)
    _json(
        root / "tokenizer.json",
        {
            "version": "1.0",
            "truncation": None,
            "padding": None,
            "added_tokens": [],
            "normalizer": None,
            "pre_tokenizer": None,
            "post_processor": None,
            "decoder": None,
            "model": {"type": "WordLevel", "vocab": {"[UNK]": 0}, "unk_token": "[UNK]"},
        },
    )
    _json(
        root / "preprocessor_config.json",
        {"do_resize": True, "size": {"height": tiny.IMAGE_SIDE, "width": tiny.IMAGE_SIDE}},
    )
    if variant == "symlink":
        os.symlink("config.json", root / "generation_config.json")
    else:
        _json(root / "generation_config.json", {"max_new_tokens": 1})

    weights_path = root / "model.safetensors"
    save_file(weights(variant, seed), str(weights_path), metadata={"format": "np"})
    if variant in ("bad_header_overlap", "bad_header_past_end", "bad_dtype", "trailing_bytes"):
        _corrupt_safetensors(weights_path, variant)

    if variant != "no_training_manifest":
        _json(root / "training_manifest.json", _training_manifest(variant, seed, name, role))
    if variant == "pickle":
        (root / "pytorch_model.bin").write_bytes(_BIN_BYTES)
    elif variant == "pkl":
        (root / "optimizer.pkl").write_bytes(_BIN_BYTES)
    elif variant == "py_file":
        (root / "modeling_x.py").write_bytes(_PY_TEXT)
    return root


def warmup_page() -> tuple[bytes, dict[str, Any]]:
    """The warm-up page (PNG bytes) and its ground truth (model-output view, with page and
    box filled in). Deterministic: a fixed seed, the first eval template seed's first
    layout, and the first document that renders to exactly one page."""
    from intelligence.doceval.synth import layouts, patients, render

    rng = random.Random(WARMUP_SEED)
    template_seed = layouts.EVAL_TEMPLATE_SEEDS.start
    for _ in range(_WARMUP_TRIES):
        for gt in patients.generate(rng, 1)[0].documents:
            doc_type = gt["document"]["type"]["value"]
            if doc_type not in layouts.DOC_TYPES:
                continue
            layout = layouts.for_type(doc_type, template_seed, purpose="dev")[0]
            try:
                rendered = render.render(gt, layout, WARMUP_DPI)
            except render.RenderError:
                continue
            if len(rendered.pages) == 1:
                return rendered.pages[0], rendered.ground_truth
    raise Refused("fixture_name")


def warmup_truth_bytes(truth: dict[str, Any]) -> bytes:
    """The committed form of the warm-up truth: sorted keys, compact, ASCII."""
    text = json.dumps(truth, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
    return text.encode("ascii") + b"\n"


__all__ = [
    "BAD_LICENCE",
    "EXPECTED_REASONS",
    "LICENCE",
    "MODEL_NAME",
    "NON_SYNTHETIC_DATASET",
    "ROLE",
    "TRAIN_DATASET",
    "TRAINING_SCHEMA",
    "UNKNOWN_DATASET",
    "VARIANTS",
    "WARMUP_DPI",
    "WARMUP_SEED",
    "make_run",
    "warmup_page",
    "warmup_truth_bytes",
    "weights",
]
