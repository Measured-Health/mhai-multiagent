"""``mhai-doceval``: build, scan and evaluate synthetic document sets, and report.

Commands::

    mhai-doceval build-set --purpose dev|train --out DIR [--scale S] [--seed N] [--version V]
    mhai-doceval scan DIR
    mhai-doceval run --extractor tiny|openai|oracle --set DIR --out DIR [--gate G ...]
    mhai-doceval report evaluation.json
    mhai-doceval compare BEFORE.json AFTER.json
    mhai-doceval intake --images DIR --manifest F --out DIR

Exit codes: 0 done, 2 refused (one JSON line ``{"refused": "<code>"}`` on stdout), 3 a file
could not be read or written. Nothing printed or logged carries a value, a document id or
page text; the runner's per-document DEBUG lines are kept off whatever the root logger
says.

What is refused here:

* ``build-set --purpose test`` (``not_allowed_here``): a test split is built only by the
  locked path (``mhai-models testset-build``, Task 17), inside the store.
* ``run --extractor oracle`` without ``--test-only-oracle``; with it, on any split but
  ``dev``, or whenever ``MHAI_EVAL_ROLE`` is set (the ``model-eval`` and
  ``model-promotion`` environments). The oracle reads the set's ground truth: it is a
  test fixture and never evaluates anything.
* Any gate but ``rehearsal`` for a stub extractor (the oracle included). With no
  ``--gate``, a stub is checked against ``rehearsal`` only, anything else against every
  gate.
* An ``--out`` folder that exists: a report is never overwritten.
"""

import argparse
import json
import logging
import math
import os
import random
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from intelligence.doceval import extractors, report, runner
from intelligence.modelstore import canonical, names
from intelligence.modelstore.reasons import Refused

PRINTED_SCHEMA = "mhai.evaluation.printed/1"
EVAL_ROLE_ENV = "MHAI_EVAL_ROLE"
EXTRACTORS = ("tiny", "openai", "oracle")
TESTSET_SIGNATURE = "testset.sig"
_MAX_MODEL_VERSIONS = 16
_MAX_MANIFEST_BYTES = 64 * 1024 * 1024
_MAX_TRAINING_MANIFEST_BYTES = 1024 * 1024


class _Stop(Exception):
    """A refusal with a code that is not a model-store reason (a set or intake code)."""

    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


def _print(obj: Any) -> None:
    sys.stdout.write(json.dumps(obj, sort_keys=True, ensure_ascii=True) + "\n")


def _refuse(code: str) -> int:
    _print({"refused": code})
    return 2


# ---------------------------------------------------------------- build-set, scan


def _build_set(args: argparse.Namespace) -> int:
    if args.purpose not in ("dev", "train"):
        return _refuse("not_allowed_here")  # test: only the locked path builds it
    from intelligence.doceval import sets  # the renderer is heavy: load it only here

    try:
        m = sets.build(args.purpose, args.seed, Path(args.out), args.scale, version=args.version)
    except sets.SetError as e:
        return _refuse(str(e))
    _print(
        {
            "purpose": m.purpose,
            "version": m.version,
            "sha256": m.sha256,
            "documents": m.documents,
            "pages": m.pages,
        }
    )
    return 0


def _scan(args: argparse.Namespace) -> int:
    from intelligence.doceval.synth import scanner

    result = scanner.scan_set(Path(args.dir))
    _print(
        {
            "ok": result.ok,
            "scanned": result.scanned,
            "findings": [{"path": f.path, "rule": f.rule} for f in result.findings],
        }
    )
    return 0 if result.ok else 2


# ---------------------------------------------------------------- run


def _model_versions(pairs: Sequence[str] | None) -> dict[str, str]:
    """``ROLE=VERSION`` pairs, from the caller (the verified pin), never from the model:
    roles as ``names.check_role``, versions as semver."""
    out: dict[str, str] = {}
    for pair in pairs or ():
        role, sep, version = pair.partition("=")
        if not sep or role in out:
            raise Refused("invalid_name")
        out[names.check_role(role)] = names.check_version(version)
    if len(out) > _MAX_MODEL_VERSIONS:
        raise Refused("invalid_name")
    return out


def _number(x: object, low: float, high: float, *, low_open: bool = False) -> None:
    if isinstance(x, bool) or not isinstance(x, int | float) or not math.isfinite(x):
        raise Refused("invalid_name")
    if (x <= low if low_open else x < low) or x > high:
        raise Refused("invalid_name")


def _check_run_args(args: argparse.Namespace) -> None:
    """Every name and number of ``run``, before any file is opened or extractor built."""
    if args.extractor not in EXTRACTORS:
        raise Refused("no_runtime")
    for g in args.gate or ():
        if g not in report.GATES:
            raise Refused("invalid_name")
    _model_versions(args.model_version)
    for token in (args.engine, args.hardware):
        if not report.is_token(token):
            raise Refused("invalid_name")
    if args.engine_version is not None and not report.is_token(args.engine_version):
        raise Refused("invalid_name")
    _number(args.deadline, 0, 86_400, low_open=True)
    _number(args.timeout, 0, 86_400, low_open=True)
    _number(args.oracle_drop, 0, 1)
    _number(args.oracle_corrupt, 0, 1)


def _locked(set_dir: Path, meta: dict[str, Any]) -> bool:
    """A set that is (or may be) a locked split: signed, or marked locked.

    TODO(Task 17): once the locked split's layout is fixed, check its exact markers here
    (the signature file next to ``testset.json``, any ``locked`` field).
    """
    return os.path.lexists(set_dir / TESTSET_SIGNATURE) or bool(meta.get("locked"))


def _extractor(args: argparse.Namespace, meta: dict[str, Any]) -> extractors.Extractor:
    kind = args.extractor
    if kind == "oracle":
        if not args.test_only_oracle or EVAL_ROLE_ENV in os.environ:
            raise Refused("not_allowed_here", detail_code="oracle")
        if meta.get("purpose") != "dev" or _locked(Path(args.set), meta):
            raise Refused("not_allowed_here", detail_code="oracle")
        try:
            return extractors.OracleExtractor(
                Path(args.set) / "truth",
                random.Random(args.oracle_seed),
                drop=args.oracle_drop,
                corrupt=args.oracle_corrupt,
            )
        except (OSError, ValueError) as e:
            raise Refused("testset_mismatch") from e
    if kind == "tiny":
        from intelligence.doceval import tiny  # numpy and safetensors: loaded only here

        if not args.model:
            raise Refused("not_found", detail_code="model")
        try:
            ex: extractors.Extractor = tiny.TinyExtractor(
                Path(args.model), model_versions=_model_versions(args.model_version)
            )
        except Refused:
            raise
        except Exception as e:  # noqa: BLE001 - a fixed code; nothing from the files goes out
            raise Refused("not_found", detail_code="model") from e
        return ex
    if kind == "openai":
        from intelligence.doceval import openai_compat as mod

        if not args.base_url or not args.model_name:
            raise Refused("no_runtime", detail_code="endpoint")
        try:
            ex = mod.OpenAICompatExtractor(
                args.base_url,
                args.model_name,
                client_cert=args.client_cert,
                client_key=args.client_key,
                ca=args.ca,
                timeout=args.timeout,
                model_versions=_model_versions(args.model_version),
                engine=args.engine,
                hardware=args.hardware,
            )
        except Refused:
            raise
        except (OSError, ValueError) as e:
            raise Refused("not_allowed_here", detail_code="endpoint") from e
        return ex
    raise Refused("no_runtime")


def _notebook_metrics(path: str | None) -> dict[str, Any] | None:
    if path is None:
        return None
    raw = report.read_regular(Path(path), _MAX_TRAINING_MANIFEST_BYTES, "training_manifest_missing")
    doc = canonical.loads_strict(raw, _MAX_TRAINING_MANIFEST_BYTES)
    if not isinstance(doc, dict):
        raise Refused("manifest_invalid")
    nb = doc.get("notebook_metrics")
    if nb is not None and not isinstance(nb, dict):
        raise Refused("manifest_invalid")
    return nb


def _write_new(folder: Path, name: str, data: bytes) -> None:
    with open(folder / name, "xb") as fh:
        fh.write(data)


def _run(args: argparse.Namespace) -> int:
    _check_run_args(args)
    deadline = args.deadline
    meta = report.testset_meta(Path(args.set))
    report.check_split(meta["purpose"])  # dev or test, before anything runs
    extractor = _extractor(args, meta)
    try:
        info = extractors.check_info(extractor.info())
    except ValueError as e:
        raise Refused("manifest_invalid", detail_code="extractor_info") from e
    gate_names = args.gate or (
        [report.REHEARSAL_GATE] if report.is_stub(info) else list(report.GATES)
    )
    gates = [report.packaged_thresholds(g) for g in dict.fromkeys(gate_names)]
    report.check_gates(info, gates)
    notebook = _notebook_metrics(args.training_manifest)

    out = Path(args.out)
    try:
        os.mkdir(out, 0o700)  # reserve: an existing folder is never written into
    except FileExistsError as e:
        raise Refused("not_allowed_here", detail_code="exists") from e
    try:
        try:
            result = runner.run(extractor, Path(args.set), per_page_deadline_s=deadline)
        except Refused as e:
            stop = {"timeouts": "timeouts", "hostile_scoring": "hostile_scoring"}
            if e.reason != "evaluation_failed" or e.detail_code not in stop:
                raise
            rep = report.stopped_report(
                info,
                testset=meta,
                reason=stop[e.detail_code],
                gates=gates,
                engine_version=args.engine_version,
            )
        else:
            if result.testset_sha256 != meta["sha256"] or result.purpose != meta["purpose"]:
                raise Refused("testset_mismatch")  # the set changed under the run
            rep = report.build_report(
                result,
                gates,
                testset_name=meta["name"],
                engine_version=args.engine_version,
                notebook_metrics=notebook,
            )
        _write_new(out, "evaluation.json", report.dumps(rep))
        _write_new(out, "evaluation.md", report.render_markdown(rep).encode("utf-8"))
    except BaseException:
        for name in ("evaluation.json", "evaluation.md"):
            (out / name).unlink(missing_ok=True)
        out.rmdir()
        raise
    _print(
        {
            "testset_sha256": rep["testset"]["sha256"],
            "split": rep["split"]["name"],
            "stopped": rep["stopped"],
            "verdicts": {g: v["verdict"] for g, v in rep["verdicts"].items()},
        }
    )
    return 0


# ---------------------------------------------------------------- report, compare


def _report(args: argparse.Namespace) -> int:
    rep = report.read_report(Path(args.file))
    sys.stdout.write(report.render_markdown(rep))
    return 0


def _compare(args: argparse.Namespace) -> int:
    before = report.read_report(Path(args.before))
    after = report.read_report(Path(args.after))
    sys.stdout.write(report.compare(before, after))
    return 0


# ---------------------------------------------------------------- intake


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for k, v in pairs:
        if k in out:
            raise ValueError("duplicate key")
        out[k] = v
    return out


def _no_constant(_: str) -> Any:
    raise ValueError("constant")


def _printed_manifest(path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    """``mhai.evaluation.printed/1``: the pages as printed, and the volunteers' consent ids.

    ``{"schema", "pages": [{"page_id", "document_id", "page_no", "size_px": [w, h],
    "boxes": {path: [x0, y0, x1, y1]}, "ground_truth": {...}, "form", "record_id"}],
    "volunteers": {"handwritten_volunteer/<file>": {"consent_id", "role"}}}``. Anything
    else is ``manifest_invalid``; intake then checks every page itself.
    """
    from intelligence.doceval.synth import intake

    try:
        raw = report.read_regular(path, _MAX_MANIFEST_BYTES, "manifest_invalid")
    except Refused as e:
        raise _Stop("manifest_invalid") from e
    try:
        doc = json.loads(raw, object_pairs_hook=_no_duplicates, parse_constant=_no_constant)
    except (ValueError, RecursionError) as e:
        raise _Stop("manifest_invalid") from e
    if not isinstance(doc, dict) or set(doc) != {"schema", "pages", "volunteers"}:
        raise _Stop("manifest_invalid")
    if doc["schema"] != PRINTED_SCHEMA or not isinstance(doc["pages"], list):
        raise _Stop("manifest_invalid")
    if not isinstance(doc["volunteers"], dict):
        raise _Stop("manifest_invalid")
    keys = {
        "page_id",
        "document_id",
        "page_no",
        "size_px",
        "boxes",
        "ground_truth",
        "form",
        "record_id",
    }
    pages: dict[str, Any] = {}
    try:
        for p in doc["pages"]:
            if not isinstance(p, dict) or set(p) != keys:
                raise _Stop("manifest_invalid")
            size = p["size_px"]
            boxes = p["boxes"]
            if not isinstance(size, list) or len(size) != 2 or not isinstance(boxes, dict):
                raise _Stop("manifest_invalid")
            if not all(isinstance(v, int) and not isinstance(v, bool) for v in size):
                raise _Stop("manifest_invalid")
            if not isinstance(p["ground_truth"], dict) or not isinstance(p["form"], bool):
                raise _Stop("manifest_invalid")
            mapped: dict[str, tuple[float, float, float, float]] = {}
            for field_path, box in boxes.items():
                if not isinstance(box, list) or len(box) != 4:
                    raise _Stop("manifest_invalid")
                if not all(isinstance(v, int | float) and not isinstance(v, bool) for v in box):
                    raise _Stop("manifest_invalid")
                mapped[field_path] = (float(box[0]), float(box[1]), float(box[2]), float(box[3]))
            if p["page_id"] in pages:
                raise _Stop("manifest_invalid")
            pages[p["page_id"]] = intake.PrintedPage(
                page_id=p["page_id"],
                document_id=p["document_id"],
                page_no=p["page_no"],
                size_px=(size[0], size[1]),
                boxes=mapped,
                ground_truth=p["ground_truth"],
                form=p["form"],
                record_id=p["record_id"],
            )
        volunteers: dict[str, Any] = {}
        for rel, v in doc["volunteers"].items():
            if not isinstance(v, dict) or set(v) != {"consent_id", "role"}:
                raise _Stop("manifest_invalid")
            volunteers[rel] = intake.Volunteer(v["consent_id"], v["role"])
    except intake.IntakeError as e:
        raise _Stop(str(e)) from e
    except (TypeError, ValueError) as e:
        raise _Stop("manifest_invalid") from e
    return pages, volunteers


def _intake(args: argparse.Namespace) -> int:
    from intelligence.doceval.synth import intake

    pages, volunteers = _printed_manifest(Path(args.manifest))
    try:
        result = intake.ingest(Path(args.images), pages, volunteers=volunteers)
        written = intake.write_result(result, Path(args.out))
    except intake.IntakeError as e:
        return _refuse(str(e))
    _print(
        {
            "accepted": len(written),
            "refused": [{"label": r.label, "reason": r.reason} for r in result.refused],
        }
    )
    return 0


# ---------------------------------------------------------------- main


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="mhai-doceval", description="Document evaluation sets.")
    sub = p.add_subparsers(dest="command", required=True)

    b = sub.add_parser("build-set", help="build the dev split or a training set")
    b.add_argument("--purpose", required=True)
    b.add_argument("--out", required=True)
    b.add_argument("--scale", type=float, default=1.0)
    b.add_argument("--seed", type=int, default=None)
    b.add_argument("--version", default="1.0.0")
    b.set_defaults(func=_build_set)

    s = sub.add_parser("scan", help="run the synthetic-data scanner over a folder")
    s.add_argument("dir")
    s.set_defaults(func=_scan)

    r = sub.add_parser("run", help="evaluate an extractor over a set")
    r.add_argument("--extractor", required=True)
    r.add_argument("--set", required=True)
    r.add_argument("--out", required=True)
    r.add_argument("--gate", action="append", default=None)
    r.add_argument("--deadline", type=float, default=60.0, help="seconds per page")
    r.add_argument("--engine-version", default=None)
    r.add_argument("--training-manifest", default=None)
    r.add_argument("--model", default=None, help="tiny: the run folder")
    r.add_argument("--base-url", default=None, help="openai: the endpoint")
    r.add_argument("--model-name", default=None)
    r.add_argument("--client-cert", default=None)
    r.add_argument("--client-key", default=None)
    r.add_argument("--ca", default=None)
    r.add_argument("--timeout", type=float, default=60.0)
    r.add_argument("--model-version", action="append", default=None, help="ROLE=VERSION")
    r.add_argument("--engine", default="openai-compat", help="openai: the serving engine")
    r.add_argument("--hardware", default="unknown", help="openai: e.g. nvidia-l40s")
    r.add_argument("--test-only-oracle", action="store_true", help=argparse.SUPPRESS)
    r.add_argument("--oracle-seed", type=int, default=0, help=argparse.SUPPRESS)
    r.add_argument("--oracle-drop", type=float, default=0.0, help=argparse.SUPPRESS)
    r.add_argument("--oracle-corrupt", type=float, default=0.0, help=argparse.SUPPRESS)
    r.set_defaults(func=_run)

    rp = sub.add_parser("report", help="print evaluation.md from an evaluation.json")
    rp.add_argument("file")
    rp.set_defaults(func=_report)

    c = sub.add_parser("compare", help="before-and-after report for a clinical-review PR")
    c.add_argument("before")
    c.add_argument("after")
    c.set_defaults(func=_compare)

    i = sub.add_parser("intake", help="register real scans and photos against a printed manifest")
    i.add_argument("--images", required=True)
    i.add_argument("--manifest", required=True)
    i.add_argument("--out", required=True)
    i.set_defaults(func=_intake)
    return p


def main(argv: Sequence[str] | None = None) -> int:
    # the runner's DEBUG lines carry document ids: never from this command
    logging.getLogger("intelligence.doceval").setLevel(logging.INFO)
    args = _parser().parse_args(argv)
    try:
        code: int = args.func(args)
    except Refused as e:
        return _refuse(e.reason)
    except _Stop as e:
        return _refuse(e.code)
    except OSError:
        _print({"error": "io"})
        return 3
    except Exception:  # noqa: BLE001 - a fixed code, never a traceback or a message
        return _refuse("internal_error")
    return code


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
