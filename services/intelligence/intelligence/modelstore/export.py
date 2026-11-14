"""The export step: the last cell of a trainer's own notebook (Colab or anywhere).

The trainer owns the model and how it is trained. This step only turns a finished run
folder (the merged model saved with ``save_pretrained(run_dir, safe_serialization=True)``)
into one that promotion accepts, and says what to fix before anything is uploaded:

1. it builds ``training_manifest.json`` from a few values, through
   :mod:`intelligence.modelstore.training`'s own types (so it is valid by construction),
   with the installed framework versions and the time;
2. it exports a GGUF next to the safetensors with llama.cpp (``convert_hf_to_gguf.py``
   to f16 in a temporary folder, then ``llama-quantize``; ``q8_0`` is written by the
   converter directly), as ``model-<quant>.gguf``;
3. it runs the checks promotion runs on the folder (``files.check_run`` and the store's
   one-upload cap of 5 GiB per file, then
   ``training.check`` against ``models/approved-datasets.json`` and ``models/licences.json``)
   and prints the files hash for ``--expect-files``.

A promoted base model is verified at promotion (it needs the store's reader): here it is
only held to its pattern.

llama.cpp is optional: a checkout given by ``llama_cpp`` / ``--llama-cpp`` or
``LLAMA_CPP_DIR``, with ``llama-quantize`` built in ``build/bin`` (or on ``PATH``). Without
it the step stops with what to install, before writing anything; ``--no-gguf`` skips the
export. A ``manifest.json`` in the folder is refused before anything is written
(``training_manifest_misnamed``).

This module imports only the standard library and the light model store modules, so it
runs from a ``pip install --no-deps`` of this package. Use it as
``python -m intelligence.modelstore.export RUN_DIR ...``, ``mhai-models export ...`` or
:func:`export_run`. Exit codes: 0 passed, 2 refused or a bad value (``refused: <code>``
and ``fix: <text>``), 3 llama.cpp missing or the conversion failed.
"""

import argparse
import importlib.metadata
import json
import math
import os
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from intelligence.modelstore import files, names, training
from intelligence.modelstore.reasons import FIXES, Refused

REPO = Path(__file__).resolve().parents[4]
DATASETS_ENV = "MHAI_MODELS_DATASETS"
LICENCES_ENV = "MHAI_MODELS_LICENCES"
LLAMA_CPP_ENV = "LLAMA_CPP_DIR"
CONVERT_SCRIPT = "convert_hf_to_gguf.py"
QUANTIZE_BIN = "llama-quantize"
DEFAULT_QUANT = "q4_k_m"
# The quantisations offered, as llama-quantize names them; q8_0 the converter writes itself.
QUANTS: Mapping[str, str] = {
    "q4_k_m": "Q4_K_M",
    "q5_k_m": "Q5_K_M",
    "q6_k": "Q6_K",
    "q8_0": "Q8_0",
}
_DIRECT = frozenset({"q8_0"})
CONVERT_TIMEOUT_S = 6 * 3600
# The largest file the store takes: one S3 PUT (``store.MAX_PUT_BYTES``; multipart is not in
# phase 0). Kept here so this module stays importable without the store's dependencies.
MAX_STORE_FILE_BYTES = 5 * 1024**3
STORE_CAP_DETAIL = "file_too_large_for_store"  # promote.FILE_TOO_LARGE

LLAMA_MISSING = (
    "llama.cpp not found: clone it and build llama-quantize (see docs/models/export-step.md),"
    f" then pass llama_cpp=/path/to/llama.cpp (--llama-cpp) or set {LLAMA_CPP_ENV};"
    " or pass gguf=False (--no-gguf) to skip the GGUF export"
)

# What to do about each refusal a trainer can hit: fixed texts, no file contents.
_HINTS: Mapping[str, str] = {
    **FIXES,
    "file_not_allowed": (
        "remove that file from the run folder: only config and tokenizer JSON, *.safetensors,"
        " one *.gguf and training_manifest.json are allowed (tokenizer side files such as"
        " vocab.json, merges.txt or tokenizer.model can go once the GGUF is written)"
    ),
    "pickle_refused": (
        "save the weights as safetensors (save_pretrained(..., safe_serialization=True))"
        " and delete the .bin/.pt/.pkl files"
    ),
    "code_refused": (
        "remove code files and any auto_map or trust_remote_code entries:"
        " the model must load without remote code"
    ),
    "missing_file": (
        "the run needs *.safetensors weights and config.json:"
        " save the merged model with save_pretrained(run_dir, safe_serialization=True)"
    ),
    "gguf_invalid": (
        "keep exactly one .gguf, written by this step, from an architecture on the list"
        " (llama, qwen2, qwen3, gemma2, gemma3, gemma4, phi3, mistral), with one default chat"
        " template and no named ones (tokenizer.chat_template.<name>)"
    ),
    "safetensors_invalid": "re-save the weights with save_pretrained(..., safe_serialization=True)",
    "json_invalid": "re-save the config and tokenizer with save_pretrained",
    "too_large": (
        "a file is over its cap: shard the weights (save_pretrained(..., max_shard_size='4GB'))"
        " or pick a smaller quantisation"
    ),
    "too_many_files": "the run folder holds too many files: keep only the model's own files",
    "dataset_unknown": (
        "use a dataset id from models/approved-datasets.json (<name>@<x.y.z>);"
        " a new dataset is added there by a pull request first"
    ),
    "dataset_not_synthetic": (
        "only synthetic datasets, and public datasets with no patient data, are approved for now"
    ),
    "revision_unpinned": "give the base model's full 40-character commit hash, not a branch or tag",
    "licence_not_allowed": (
        "the base model's licence (an SPDX id) must be in models/licences.json;"
        " another licence is added there by a pull request first"
    ),
    "role_unknown": "use role cca (the answer model) or extract (the document extractor)",
    "manifest_invalid": (
        "a value does not fit its pattern (the detail names it): name is lowercase letters,"
        " digits and '-'; trained_by your lowercase GitHub login; datasets <name>@<x.y.z>;"
        " metric names snake_case without a run of three digits, values finite numbers;"
        " give either an upstream base (repo, revision, licence) or a promoted one"
    ),
    "gguf_not_allowed_for_role": (
        "only a cca (answer) run carries a GGUF: pass gguf=False (--no-gguf) for this role"
        " and delete the .gguf from the run folder"
    ),
    "gguf_template_mismatch": (
        "the GGUF's chat template must be the run's: keep the template in chat_template.json"
        " or tokenizer_config.json (convert a chat_template.jinja into chat_template.json)"
        " before this step, and let this step write the GGUF from that folder"
    ),
    "invalid_name": "the version is <major>.<minor>.<patch>, for example 0.1.0",
    "training_manifest_missing": "the training manifest was not written: run this step again",
    "not_found": "the run folder does not exist",
    "policy_invalid": (
        "the registries could not be read: point --datasets and --licences at"
        " models/approved-datasets.json and models/licences.json from the repository"
    ),
}


class ExportError(Exception):
    """A step that could not run (llama.cpp missing, the conversion failed, a bad
    argument): ``str()`` is a fixed message for the trainer."""


@dataclass(frozen=True, slots=True)
class Exported:
    run_dir: Path
    manifest: training.TrainingManifest
    gguf: str | None
    files: tuple[files.FileEntry, ...]
    files_sha256: str
    base_verified_at_promotion: bool


_DETAIL_HINTS: Mapping[tuple[str, str], str] = {
    ("too_large", STORE_CAP_DETAIL): (
        "a file is over the 5 GiB one upload to the model store allows: shard the weights"
        " (save_pretrained(..., max_shard_size='4GB')); if one tensor alone is larger, the"
        " run cannot be promoted until the store has a conditional multipart upload"
    ),
}


def fix_for(refusal: Refused) -> str | None:
    """The fixed text telling a trainer what to do about ``refusal``, if there is one."""
    detail = refusal.detail_code
    if detail is not None and (refusal.reason, detail) in _DETAIL_HINTS:
        return _DETAIL_HINTS[(refusal.reason, detail)]
    return _HINTS.get(refusal.reason)


def installed_framework() -> dict[str, str]:
    """The installed versions of the manifest's framework packages (those whose version
    fits its pattern; a local build string that does not is left out)."""
    out: dict[str, str] = {}
    for package in sorted(training.FRAMEWORKS):
        try:
            version = importlib.metadata.version(package)
        except importlib.metadata.PackageNotFoundError:
            continue
        if training.is_package_version(version):
            out[package] = version
    return out


def build_manifest(
    *,
    name: str,
    role: str,
    trained_by: str,
    datasets: Sequence[str],
    base_upstream: str | None = None,
    base_revision: str | None = None,
    base_licence: str | None = None,
    base_promoted: str | None = None,
    notebook_metrics: Mapping[str, int | float] | None = None,
    framework: Mapping[str, str] | None = None,
    created_at: str | None = None,
) -> dict[str, Any]:
    """The ``training_manifest.json`` object, parsed back through ``training.parse`` (so a
    value that does not fit is ``manifest_invalid`` here, not at promotion)."""
    upstream = (base_upstream, base_revision, base_licence)
    base: dict[str, str]
    if base_promoted is not None and all(v is None for v in upstream):
        base = {"promoted": base_promoted}
    elif base_promoted is None and all(isinstance(v, str) for v in upstream):
        base = {"upstream": str(base_upstream), "revision": str(base_revision)}
        base["licence"] = str(base_licence)
    else:
        raise Refused("manifest_invalid", detail_code="base_model")
    obj: dict[str, Any] = {
        "schema": training.SCHEMA,
        "name": name,
        "role": role,
        "trained_by": trained_by,
        "datasets": list(datasets),
        "base_model": base,
        "framework": dict(installed_framework() if framework is None else framework),
        "notebook_metrics": dict(notebook_metrics or {}),
        "created_at": created_at or datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    training.parse(obj)
    return obj


def _find_tools(llama_cpp: Path | str | None, quant: str) -> tuple[Path, Path | None]:
    given = llama_cpp if llama_cpp is not None else os.environ.get(LLAMA_CPP_ENV)
    if not given:
        raise ExportError(LLAMA_MISSING)
    root = Path(given)
    convert = root / CONVERT_SCRIPT
    if not convert.is_file():
        raise ExportError(LLAMA_MISSING)
    if quant in _DIRECT:
        return convert, None
    for candidate in (root / "build" / "bin" / QUANTIZE_BIN, root / QUANTIZE_BIN):
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return convert, candidate
    found = shutil.which(QUANTIZE_BIN)
    if found is None:
        raise ExportError(
            f"{QUANTIZE_BIN} not found: build it in the llama.cpp checkout"
            f" (cmake -B build && cmake --build build --target {QUANTIZE_BIN}),"
            " or pass quant='q8_0', which needs only the converter"
        )
    return convert, Path(found)


def _run(argv: list[str], what: str) -> None:
    try:
        done = subprocess.run(argv, stdin=subprocess.DEVNULL, timeout=CONVERT_TIMEOUT_S)
    except (OSError, subprocess.TimeoutExpired):
        raise ExportError(f"GGUF {what} failed to run: see the output above") from None
    if done.returncode != 0:
        raise ExportError(f"GGUF {what} failed (exit {done.returncode}): see the output above")


def convert_gguf(
    run_dir: Path, quant: str, convert: Path, quantize: Path | None, tmp_dir: Path | None = None
) -> str:
    """Write ``model-<quant>.gguf`` into ``run_dir``; return its name. The f16
    intermediate is written outside the run folder and removed."""
    out_name = f"model-{quant}.gguf"
    out = run_dir / out_name

    def converter(outfile: Path, outtype: str) -> list[str]:
        return [sys.executable, str(convert), str(run_dir), "--outfile", str(outfile)] + [
            "--outtype",
            outtype,
        ]

    if quantize is None:
        _run(converter(out, quant), "conversion")
        return out_name
    with tempfile.TemporaryDirectory(prefix="mhai-gguf-", dir=tmp_dir) as tmp:
        f16 = Path(tmp) / "model-f16.gguf"
        _run(converter(f16, "f16"), "conversion")
        _run([str(quantize), str(f16), str(out), QUANTS[quant]], "quantisation")
    return out_name


def _registry_path(given: Path | str | None, env: str, name: str) -> Path:
    if given is not None:
        return Path(given)
    return Path(os.environ.get(env) or REPO / "models" / name)


def check(
    run_dir: Path | str,
    *,
    datasets_registry: Path | str | None = None,
    licences_registry: Path | str | None = None,
) -> tuple[tuple[files.FileEntry, ...], str, training.TrainingCheck]:
    """Promotion's local checks on the folder: the files (``files.check_run``), then the
    training manifest against the registries. A promoted base passes here; promotion
    verifies it."""
    registry = training.load_registry(
        _registry_path(datasets_registry, DATASETS_ENV, "approved-datasets.json")
    )
    licences = training.load_licences(
        _registry_path(licences_registry, LICENCES_ENV, "licences.json")
    )
    entries = tuple(files.check_run(run_dir))
    if any(e.size > MAX_STORE_FILE_BYTES for e in entries):  # as promote.check_files
        raise Refused("too_large", detail_code=STORE_CAP_DETAIL)
    tm = training.load_run(run_dir)
    checked = training.check(tm, registry, licences, verify_pin=_at_promotion)
    training.check_gguf_run(tm, run_dir)
    return entries, files.files_hash(entries), checked


def _at_promotion(pin: str) -> None:
    """A promoted base is verified at promotion, with the store's reader."""


def export_run(
    run_dir: Path | str,
    *,
    name: str,
    version: str,
    datasets: Sequence[str],
    trained_by: str,
    role: str = "cca",
    base_upstream: str | None = None,
    base_revision: str | None = None,
    base_licence: str | None = None,
    base_promoted: str | None = None,
    notebook_metrics: Mapping[str, int | float] | None = None,
    quant: str = DEFAULT_QUANT,
    gguf: bool = True,
    llama_cpp: Path | str | None = None,
    datasets_registry: Path | str | None = None,
    licences_registry: Path | str | None = None,
    tmp_dir: Path | str | None = None,
) -> Exported:
    """Write the training manifest, export the GGUF and run the checks. Raises
    :class:`Refused` (a check; :func:`fix_for` says what to do) or :class:`ExportError`.
    Every value and tool is checked before anything is written."""
    run = Path(run_dir)
    names.check_version(version)
    if quant not in QUANTS:
        raise ExportError(f"quant must be one of {', '.join(QUANTS)} (default {DEFAULT_QUANT})")
    if not run.is_dir():
        raise Refused("not_found")
    files.check_misnamed(os.listdir(run))
    obj = build_manifest(
        name=name,
        role=role,
        trained_by=trained_by,
        datasets=datasets,
        base_upstream=base_upstream,
        base_revision=base_revision,
        base_licence=base_licence,
        base_promoted=base_promoted,
        notebook_metrics=notebook_metrics,
    )
    if gguf and role not in training.GGUF_ROLES:
        raise Refused("gguf_not_allowed_for_role")
    tools = _find_tools(llama_cpp, quant) if gguf else None
    raw = json.dumps(obj, indent=2, sort_keys=True, allow_nan=False) + "\n"
    (run / training.FILE_NAME).write_text(raw, encoding="ascii")
    written = None
    if tools is not None:
        tmp = Path(tmp_dir) if tmp_dir is not None else None
        written = convert_gguf(run, quant, tools[0], tools[1], tmp)
    entries, sha, checked = check(
        run, datasets_registry=datasets_registry, licences_registry=licences_registry
    )
    promoted = isinstance(checked.manifest.base, training.PromotedBase)
    return Exported(run, checked.manifest, written, entries, sha, promoted)


# ---------------------------------------------------------------- command line


def _metric(text: str) -> tuple[str, int | float]:
    key, sep, value = text.partition("=")
    number: int | float
    try:
        number = int(value)
    except ValueError:
        try:
            number = float(value)
        except ValueError:
            number = math.nan
    if not sep or not key or not math.isfinite(number):
        raise ExportError("--metric takes NAME=NUMBER, for example --metric train_loss=0.42")
    return key, number


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="mhai-models export",
        description="Write training_manifest.json, export the GGUF and run promotion's checks.",
    )
    p.add_argument("run_dir")
    p.add_argument("--name", required=True, help="model name, e.g. mhai-answer")
    p.add_argument("--version", required=True, help="the version to promote, e.g. 0.1.0")
    p.add_argument("--role", default="cca")
    p.add_argument("--dataset", action="append", required=True, help="<name>@<x.y.z>; repeat")
    p.add_argument("--trained-by", required=True, help="your lowercase GitHub login")
    p.add_argument("--base-upstream", help="the base model's Hub repo")
    p.add_argument("--base-revision", help="its 40-character commit")
    p.add_argument("--base-licence", help="its SPDX licence id")
    p.add_argument("--base-promoted", help="or a promoted base: <name>@<version>#<sha256>")
    p.add_argument("--metric", action="append", default=[], help="NAME=NUMBER; repeat")
    p.add_argument("--quant", default=DEFAULT_QUANT, help=f"one of {', '.join(QUANTS)}")
    p.add_argument("--no-gguf", action="store_true", help="skip the GGUF export")
    p.add_argument("--llama-cpp", default=None, help=f"llama.cpp checkout (or {LLAMA_CPP_ENV})")
    p.add_argument("--datasets", default=None, help="models/approved-datasets.json")
    p.add_argument("--licences", default=None, help="models/licences.json")
    return p


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    refusal: Refused | None = None
    try:
        metrics = dict(_metric(m) for m in args.metric)
        out = export_run(
            args.run_dir,
            name=args.name,
            version=args.version,
            datasets=args.dataset,
            trained_by=args.trained_by,
            role=args.role,
            base_upstream=args.base_upstream,
            base_revision=args.base_revision,
            base_licence=args.base_licence,
            base_promoted=args.base_promoted,
            notebook_metrics=metrics,
            quant=args.quant,
            gguf=not args.no_gguf,
            llama_cpp=args.llama_cpp,
            datasets_registry=args.datasets,
            licences_registry=args.licences,
        )
    except Refused as exc:
        refusal = exc
    except ExportError as exc:
        print(f"error: {exc}")
        return 2 if str(exc).startswith(("--metric", "quant")) else 3
    if refusal is not None:
        detail = f" ({refusal.detail_code})" if refusal.detail_code else ""
        file = getattr(refusal, "file", None)
        print(f"refused: {refusal.reason}{detail}" + (f" in {file}" if file else ""))
        fix = fix_for(refusal)
        if fix:
            print(f"fix: {fix}")
        return 2
    print(f"ok: {training.FILE_NAME} written")
    if out.gguf is not None:
        print(f"ok: {out.gguf} written")
    print(f"ok: {len(out.files)} files pass the promotion file checks")
    print("ok: the training manifest passes the dataset and licence registries")
    if out.base_verified_at_promotion:
        print("note: the promoted base model is verified at promotion")
    run = out.run_dir.name
    print(f"files hash: {out.files_sha256}")
    print(
        f"next: upload the folder to Drive as models/{out.manifest.name}/{run}/ and ask for"
        f" mhai-models promote {out.manifest.name} {args.version} --source drive --run {run}"
        f" --expect-files {out.files_sha256}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
