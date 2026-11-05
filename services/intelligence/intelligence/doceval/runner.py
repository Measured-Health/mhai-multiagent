"""Run an :class:`~intelligence.doceval.extractors.Extractor` over an evaluation set.

Order of work:

1. **Verify the set** against its ``testset.json`` before anything runs. The manifest
   must be canonical JSON (``mhai.evaluation.testset/1``, this schema version); every
   file under the set folder must be a regular file (no symlinks) listed in it, with the
   listed size and SHA-256, and every listed file must be there; the documents, slices
   and totals must agree with each other and with the files. Any mismatch raises
   ``Refused("testset_mismatch")`` and the extractor is never called. Files are read
   again when used and re-hashed, so a file changed during the run stops it the same way.
2. **Run** each document of role ``document`` (clean twins are not run), one at a time,
   in id order. The extractor gets the page bytes only, never a path, an id or truth.
   Each call runs in a fresh daemon thread and is waited for until
   ``per_page_deadline_s`` times the page count; a call past it is abandoned (its result
   is never read) and counted ``deadline``. When more than 5% of the set's documents
   have hit the deadline the run stops: ``Refused("evaluation_failed",
   detail_code="timeouts")``. An abandoned call may still be running while the next
   document starts; an extractor that is not thread-safe should refuse to overlap.
3. **Check every output.** A document must serialise to at most ``max_output_bytes``
   of strict JSON (no NaN, no duplicate keys) and validate against the stored view.
   Every field's ``band`` must be the one its ``confidence`` gives
   (:func:`extractors.band`), or the output is a ``schema_error``: a band that
   disagrees with the confidence would move fields out of the silent-error count.
   Outcomes are fixed codes (:data:`OUTCOME_CODES`); nothing an extractor returned,
   raised or printed is logged, stored or echoed. A document that fails is scored as an
   empty extraction for the field metrics; in the hostile slice it is its own outcome,
   ``extractor_failed`` (never a refusal), and fails the case.
4. **Score** every metric per slice, and overall over the *non-hostile* documents only:
   field P/R, the handwritten fields, identifiers, silent errors, calibration, the
   high-band share, grounding and handwriting CER. Hostile documents are scored in
   their own slice, against their clean twin's truth, and ``hostile_outcome`` is
   aggregated over every hostile case; they are kept out of the overall bucket so that
   pages built to mislead do not move the overall field metrics or calibration either
   way. Standard-code exact match is ``not_measured`` (phase 0 has no normaliser).
   Every ``None`` rate becomes :data:`NOT_MEASURED`, never a pass. A ``hostile_outcome``
   that cannot score a case stops the run: ``Refused("evaluation_failed",
   detail_code="hostile_scoring")``.

Latency is the wall time of each call divided by its pages, one value per page,
including calls that timed out or failed. Apart from latency and throughput, a run is
deterministic. :class:`RunResult` holds aggregates only: no values, no identifiers and no
per-document rows. Logs carry ids, slices, counts, timings and reason codes only (P21);
the per-document lines are DEBUG, and DEBUG must be off in the ``model-eval``
environment (the ids of the locked test split stay out of its logs).

Notes for later tasks:

* TODO(Task 11, Task 19): refuse an extractor whose ``engine`` is ``"stub"`` or whose
  name ends in ``-stub`` for every gate except ``rehearsal``. The stubs (the oracle
  above all, which reads ground truth) must not be usable in the ``model-eval`` or
  ``model-promotion`` environments.
* Isolation of in-process extractors is by the interface only (page bytes in, nothing
  else); a Python extractor in this process could still open files its user can read.
  TODO: the first in-process model runner gets a subprocess boundary with no access to
  the set folder.
"""

import concurrent.futures
import hashlib
import json
import logging
import math
import os
import re
import stat
import threading
import time
from collections import Counter
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval import extractors, metrics, schema
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

log = logging.getLogger(__name__)

TESTSET_SCHEMA = "mhai.evaluation.testset/1"
HOSTILE_SCHEMA = "mhai.evaluation.hostile/1"
HOSTILE_SLICE = "hostile"
HOSTILE_FAILED = "extractor_failed"  # a hostile case whose document was not extracted
NOT_MEASURED = "not_measured"
TIMEOUT_SHARE = (5, 100)  # more than this share of documents past the deadline: stop
OUTCOME_CODES = frozenset(
    {
        "extracted",
        "bad_json",
        "schema_error",
        "oversize",
        "extractor_error",
        "deadline",
    }
    | {f"refused:{r}" for r in extractors.REFUSAL_REASONS}
)
FAILURE_CODES = OUTCOME_CODES - {"extracted"} - {f"refused:{r}" for r in extractors.REFUSAL_REASONS}

_MAX_MANIFEST_BYTES = 64 * 1024 * 1024
_MAX_JSON_BYTES = 16 * 1024 * 1024
_MAX_PAGE_BYTES = 64 * 1024 * 1024
_MAX_FILES = 200_000
_MAX_PAGES = 10_000
_ID = re.compile(r"[a-z0-9]{1,64}", re.ASCII)
_SLICE = re.compile(r"[a-z][a-z0-9_]{0,63}", re.ASCII)
_SHA = re.compile(r"[0-9a-f]{64}", re.ASCII)
_ROLES = ("document", "clean_twin")
_COUNT_KEYS = ("documents", "pages", "clean_twins", "clean_twin_pages")


@dataclass(frozen=True)
class RunResult:
    """Aggregates of one run. No values, identifiers or per-document rows."""

    extractor: extractors.ExtractorInfo
    testset_sha256: str
    purpose: str
    version: str
    extraction_schema_version: str
    documents: int
    pages: int
    outcomes: dict[str, int]  # code -> documents
    outcomes_by_slice: dict[str, dict[str, int]]
    metrics: dict[str, Any]  # {"slices": {name: {...}}, "overall": {...}}
    hostile: dict[str, Any]
    latency: dict[str, Any]  # runner wall time per page: p50, p95; throughput
    extractor_latency: dict[str, Any]  # the extractor's own page_seconds: p50, p95
    gpu_peak_bytes: int | None
    per_page_deadline_s: float
    max_output_bytes: int


# ---------------------------------------------------------------- set verification


def _mismatch() -> Refused:
    return Refused("testset_mismatch")


def _read(path: Path, cap: int) -> bytes:
    """A regular file (never followed through a symlink), at most ``cap`` bytes."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError as e:
        raise _mismatch() from e
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > cap:
            raise _mismatch()
        with os.fdopen(fd, "rb", closefd=False) as fh:
            data = fh.read(cap + 1)
    except OSError as e:
        raise _mismatch() from e
    finally:
        os.close(fd)
    if len(data) > cap:
        raise _mismatch()
    return data


def _walk(root: Path) -> set[str]:
    """Relative paths of every file under ``root``; anything not a dir or file refuses."""
    out: set[str] = set()
    stack = [(root, "")]
    while stack:
        folder, rel = stack.pop()
        try:
            entries = list(os.scandir(folder))
        except OSError as e:
            raise _mismatch() from e
        for entry in entries:
            name = rel + entry.name
            if entry.is_symlink():
                raise _mismatch()
            if entry.is_dir(follow_symlinks=False):
                stack.append((Path(entry.path), name + "/"))
            elif entry.is_file(follow_symlinks=False):
                out.add(name)
                if len(out) > _MAX_FILES:
                    raise _mismatch()
            else:
                raise _mismatch()
    return out


def _int(x: object, low: int = 0, high: int = 10**12) -> int:
    if isinstance(x, bool) or not isinstance(x, int) or not low <= x <= high:
        raise _mismatch()
    return x


@dataclass(frozen=True)
class _Doc:
    id: str
    slice: str
    pages: int
    hostile: bool
    twin: str | None


@dataclass(frozen=True)
class _Set:
    root: Path
    sha256: str
    manifest: dict[str, Any]
    files: dict[str, tuple[int, str]]  # path -> (size, sha256)
    docs: list[_Doc]  # role "document", in id order
    slices: list[str]


def _verify(set_dir: Path) -> _Set:
    root = Path(set_dir)
    if root.is_symlink() or not root.is_dir():
        raise _mismatch()
    raw = _read(root / "testset.json", _MAX_MANIFEST_BYTES)
    try:
        manifest = canonical.loads_strict(raw, _MAX_MANIFEST_BYTES)
        if canonical.dumps(manifest) != raw:
            raise _mismatch()
    except Refused as e:
        raise _mismatch() from e
    if not isinstance(manifest, dict):
        raise _mismatch()
    if manifest.get("schema") != TESTSET_SCHEMA:
        raise _mismatch()
    if manifest.get("extraction_schema_version") != schema.SCHEMA_VERSION:
        raise _mismatch()
    for key in ("purpose", "version"):
        if not isinstance(manifest.get(key), str) or len(manifest[key]) > 64:
            raise _mismatch()

    listed = manifest.get("files")
    if not isinstance(listed, list) or len(listed) > _MAX_FILES:
        raise _mismatch()
    files: dict[str, tuple[int, str]] = {}
    for f in listed:
        if not isinstance(f, dict) or set(f) != {"path", "size", "sha256"}:
            raise _mismatch()
        path, sha = f["path"], f["sha256"]
        if not isinstance(path, str) or path in files or path == "testset.json":
            raise _mismatch()
        if not isinstance(sha, str) or _SHA.fullmatch(sha) is None:
            raise _mismatch()
        size = _int(f["size"])
        if size > (_MAX_PAGE_BYTES if path.endswith(".png") else _MAX_JSON_BYTES):
            raise _mismatch()
        files[path] = (size, sha)

    slices = manifest.get("slices")
    if not isinstance(slices, dict) or not slices:
        raise _mismatch()
    for name, counts in slices.items():
        if _SLICE.fullmatch(name) is None or not isinstance(counts, dict):
            raise _mismatch()
        if set(counts) != set(_COUNT_KEYS):
            raise _mismatch()
        for k in _COUNT_KEYS:
            _int(counts[k])
    totals = manifest.get("totals")
    if not isinstance(totals, dict) or set(totals) != set(_COUNT_KEYS):
        raise _mismatch()
    for k in _COUNT_KEYS:
        if _int(totals[k]) != sum(c[k] for c in slices.values()):
            raise _mismatch()

    entries = manifest.get("documents")
    if not isinstance(entries, list):
        raise _mismatch()
    seen: dict[str, dict[str, Any]] = {}
    for d in entries:
        if not isinstance(d, dict) or not isinstance(d.get("id"), str):
            raise _mismatch()
        if _ID.fullmatch(d["id"]) is None or d["id"] in seen:
            raise _mismatch()
        role = d.get("role")
        allowed = {"id", "slice", "role", "pages"} | (
            {"twin_of"} if role == "clean_twin" else set()
        )
        if role not in _ROLES or set(d) != allowed:
            raise _mismatch()
        if not isinstance(d["slice"], str) or d["slice"] not in slices:
            raise _mismatch()
        _int(d["pages"], 1, _MAX_PAGES)
        seen[d["id"]] = d
    if [d["id"] for d in entries] != sorted(seen):
        raise _mismatch()

    expected_paths: set[str] = set()
    counted = {name: dict.fromkeys(_COUNT_KEYS, 0) for name in slices}
    twins_of: dict[str, str] = {}
    for d in entries:
        for n in range(1, d["pages"] + 1):
            expected_paths.add(f"pages/{d['id']}/{n}.png")
        expected_paths.add(f"truth/{d['id']}.json")
        c = counted[d["slice"]]
        if d["role"] == "document":
            c["documents"] += 1
            c["pages"] += d["pages"]
        else:
            parent = seen.get(d["twin_of"]) if isinstance(d["twin_of"], str) else None
            if parent is None or parent["role"] != "document" or parent["slice"] != d["slice"]:
                raise _mismatch()
            if d["twin_of"] in twins_of:
                raise _mismatch()
            twins_of[d["twin_of"]] = d["id"]
            c["clean_twins"] += 1
            c["clean_twin_pages"] += d["pages"]
    if counted != slices:
        raise _mismatch()
    for d in entries:  # twins exactly for the hostile slice's documents
        if d["role"] == "document" and (d["id"] in twins_of) != (d["slice"] == HOSTILE_SLICE):
            raise _mismatch()
    for doc_id in twins_of:
        expected_paths.add(f"hostile/{doc_id}.json")
    if set(files) != expected_paths:
        raise _mismatch()
    if _walk(root) != expected_paths | {"testset.json"}:
        raise _mismatch()
    for path, (size, sha) in sorted(files.items()):
        data = _read(root / path, size)
        if len(data) != size or hashlib.sha256(data).hexdigest() != sha:
            raise _mismatch()

    docs = [
        _Doc(d["id"], d["slice"], d["pages"], d["id"] in twins_of, twins_of.get(d["id"]))
        for d in entries
        if d["role"] == "document"
    ]
    return _Set(root, hashlib.sha256(raw).hexdigest(), manifest, files, docs, sorted(slices))


def _verified_bytes(s: _Set, path: str) -> bytes:
    size, sha = s.files[path]
    data = _read(s.root / path, size)
    if len(data) != size or hashlib.sha256(data).hexdigest() != sha:
        raise _mismatch()
    return data


def _verified_json(s: _Set, path: str) -> dict[str, Any]:
    data = _verified_bytes(s, path)
    try:
        loaded = json.loads(data)
    except ValueError as e:
        raise _mismatch() from e
    if not isinstance(loaded, dict):
        raise _mismatch()
    return loaded


# ---------------------------------------------------------------- one call


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for k, v in pairs:
        if k in out:
            raise ValueError("duplicate key")
        out[k] = v
    return out


def _no_constant(_: str) -> Any:
    raise ValueError("constant")


def _parse(text: bytes, cap: int) -> tuple[str, Any]:
    if len(text) > cap:
        return "oversize", None
    try:
        decoded = text.decode("utf-8")
        doc = json.loads(decoded, object_pairs_hook=_no_duplicates, parse_constant=_no_constant)
    except (UnicodeDecodeError, ValueError, RecursionError):
        return "bad_json", None
    return "ok", doc


def _normalise_document(document: Any, cap: int) -> tuple[str, Any]:
    """The document as a fresh JSON tree, or a failure code."""
    if isinstance(document, str):
        try:
            return _parse(document.encode("utf-8"), cap)
        except UnicodeEncodeError:
            return "bad_json", None
    if isinstance(document, bytes | bytearray):
        return _parse(bytes(document), cap)
    if not isinstance(document, Mapping):
        return "bad_json", None
    try:
        text = json.dumps(dict(document), allow_nan=False, ensure_ascii=True).encode("ascii")
    except (TypeError, ValueError, RecursionError):
        return "bad_json", None
    return _parse(text, cap)


def _finite_seconds(x: object) -> bool:
    return not isinstance(x, bool) and isinstance(x, int | float) and math.isfinite(x) and x >= 0


@dataclass(frozen=True)
class _Outcome:
    code: str
    document: dict[str, Any] | None = None
    refusal: str | None = None
    page_seconds: tuple[float, ...] = ()
    gpu_peak_bytes: int | None = None


def _check(result: object, pages: int, cap: int) -> _Outcome:
    """The outcome of one call; anything unexpected is ``extractor_error``."""
    try:
        return _check_result(result, pages, cap)
    except Exception:  # an odd object from the extractor; the exception is never read
        return _Outcome("extractor_error")


def _check_result(result: object, pages: int, cap: int) -> _Outcome:
    if isinstance(result, extractors.Refused):
        if not isinstance(result.reason, str) or result.reason not in extractors.REFUSAL_REASONS:
            return _Outcome("extractor_error")
        return _Outcome(f"refused:{result.reason}", refusal=result.reason)
    if not isinstance(result, extractors.Extracted):
        return _Outcome("extractor_error")
    seconds = result.page_seconds
    if not isinstance(seconds, tuple | list) or len(seconds) != pages:
        return _Outcome("extractor_error")
    if not all(_finite_seconds(x) for x in seconds):
        return _Outcome("extractor_error")
    gpu = result.gpu_peak_bytes
    if gpu is not None and (isinstance(gpu, bool) or not isinstance(gpu, int) or gpu < 0):
        return _Outcome("extractor_error")
    code, doc = _normalise_document(result.document, cap)
    if code != "ok":
        return _Outcome(code)
    try:
        schema.validate_stored(doc)
    except schema.SchemaError:
        return _Outcome("schema_error")
    for _, fld in schema.iter_fields(doc):
        if fld["band"] != extractors.band(fld["confidence"]):
            return _Outcome("schema_error")
    return _Outcome("extracted", doc, None, tuple(float(x) for x in seconds), gpu)


_RAISED = object()


def _call(
    extractor: extractors.Extractor, pages: tuple[bytes, ...], deadline_s: float
) -> tuple[str, object]:
    """``("ok", result)``, ``("raised", None)`` or ``("deadline", None)``.

    The worker swallows whatever the extractor raises (the exception is never read,
    logged or re-raised), so the extractor cannot end the run or the process.
    """
    future: concurrent.futures.Future[object] = concurrent.futures.Future()

    def work() -> None:
        if not future.set_running_or_notify_cancel():
            return
        try:
            value: object = extractor.extract(pages, deadline_s=deadline_s)
        except BaseException:  # noqa: B036 - SystemExit from an extractor is contained too
            value = _RAISED
        future.set_result(value)

    threading.Thread(target=work, name="doceval-extract", daemon=True).start()
    try:
        value = future.result(timeout=deadline_s)
    except concurrent.futures.TimeoutError:
        return "deadline", None
    if value is _RAISED:
        return "raised", None
    return "ok", value


# ---------------------------------------------------------------- scoring


def _not_measured(obj: Any) -> Any:
    if obj is None:
        return NOT_MEASURED
    if isinstance(obj, dict):
        return {k: _not_measured(v) for k, v in obj.items()}
    return obj


def _doc_metrics(pairs: list[tuple[dict[str, Any], dict[str, Any] | None]]) -> dict[str, Any]:
    truths = [t for t, _ in pairs]
    preds = [p for _, p in pairs]
    conf = metrics.confidence_pairs(truths, preds)
    return {
        "documents": len(pairs),
        "field_pr": metrics.field_pr(truths, preds),
        "handwritten_pr": metrics.handwritten_pr(truths, preds),
        "identifier_precision": metrics.identifier_precision(truths, preds),
        "silent_error": metrics.silent_error_rate(truths, preds),
        "calibration_ece": metrics.calibration_ece([c for c, _ in conf], [ok for _, ok in conf]),
        "calibration_fields": len(conf),
        "high_band_share": _high_band_share(preds),
        "grounding": metrics.grounding_rate(truths, preds),
        "handwriting_cer": metrics.handwriting_cer(truths, preds),
        "code_exact_match": None,
    }


def _high_band_share(preds: list[dict[str, Any] | None]) -> dict[str, Any]:
    """Predicted fields with a value that are in band ``high``: ``{high, fields, rate}``."""
    high = fields = 0
    for pred in preds:
        if pred is None:
            continue
        for _, fld in schema.iter_fields(pred):
            if fld.get("value") is None:
                continue
            fields += 1
            high += fld.get("band") == "high"
    return {"high": high, "fields": fields, "rate": high / fields if fields else None}


def _hostile_summary(results: list[tuple[str, dict[str, Any]]]) -> dict[str, Any]:
    failures: Counter[str] = Counter()
    by_kind: dict[str, dict[str, int]] = {}
    flagged = unchanged = passed = 0
    for kind, r in results:
        ok = r["flagged_or_refused"] and r["other_fields_unchanged"]
        flagged += r["flagged_or_refused"]
        unchanged += r["other_fields_unchanged"]
        passed += ok
        failures.update(r["failures"])
        k = by_kind.setdefault(kind, {"cases": 0, "passed": 0})
        k["cases"] += 1
        k["passed"] += ok
    n = len(results)
    return {
        "cases": n,
        "flagged_or_refused": flagged,
        "other_fields_unchanged": unchanged,
        "passed": passed,
        "rate": passed / n if n else None,
        "failures": dict(sorted(failures.items())),
        "by_kind": dict(sorted(by_kind.items())),
    }


def _hostile_failed() -> dict[str, Any]:
    return {
        "flagged_or_refused": False,
        "other_fields_unchanged": False,
        "failures": [HOSTILE_FAILED],
    }


def _positive(x: object, name: str) -> float:
    if isinstance(x, bool) or not isinstance(x, int | float) or not math.isfinite(x) or x <= 0:
        raise ValueError(name)
    return float(x)


# ---------------------------------------------------------------- the run


def run(
    extractor: extractors.Extractor,
    set_dir: str | Path,
    *,
    per_page_deadline_s: float = 60,
    max_output_bytes: int = 2_000_000,
) -> RunResult:
    """Verify ``set_dir`` against its ``testset.json``, run ``extractor`` over it, score it.

    Raises ``Refused("testset_mismatch")`` if the set does not verify (nothing is run),
    and ``Refused("evaluation_failed", detail_code="timeouts")`` when more than 5% of
    the documents time out.
    """
    deadline = _positive(per_page_deadline_s, "bad_deadline")
    if isinstance(max_output_bytes, bool) or not isinstance(max_output_bytes, int):
        raise ValueError("bad_max_output_bytes")
    if max_output_bytes <= 0:
        raise ValueError("bad_max_output_bytes")
    info = extractors.check_info(extractor.info())
    started = time.monotonic()
    s = _verify(Path(set_dir))
    total_docs = len(s.docs)
    log.info(
        "evaluation started: extractor=%s testset=%s documents=%d",
        info.name,
        s.sha256,
        total_docs,
    )

    outcomes: Counter[str] = Counter()
    by_slice_outcomes: dict[str, Counter[str]] = {name: Counter() for name in s.slices}
    scored: list[tuple[str, tuple[dict[str, Any], dict[str, Any] | None]]] = []
    hostile: list[tuple[str, dict[str, Any]]] = []
    page_wall: list[float] = []
    page_reported: list[float] = []
    by_slice_wall: dict[str, list[float]] = {name: [] for name in s.slices}
    gpu_peak: int | None = None
    timeouts = 0
    limit_num, limit_den = TIMEOUT_SHARE

    for doc in s.docs:
        pages = tuple(
            _verified_bytes(s, f"pages/{doc.id}/{n}.png") for n in range(1, doc.pages + 1)
        )
        t0 = time.perf_counter()
        status, result = _call(extractor, pages, deadline * doc.pages)
        elapsed = time.perf_counter() - t0
        if status == "deadline":
            outcome = _Outcome("deadline")
        elif status == "raised":
            outcome = _Outcome("extractor_error")
        else:
            outcome = _check(result, doc.pages, max_output_bytes)
        del result
        outcomes[outcome.code] += 1
        by_slice_outcomes[doc.slice][outcome.code] += 1
        per_page = elapsed / doc.pages
        page_wall.extend([per_page] * doc.pages)
        by_slice_wall[doc.slice].extend([per_page] * doc.pages)
        page_reported.extend(outcome.page_seconds)
        if outcome.gpu_peak_bytes is not None:
            gpu_peak = max(gpu_peak or 0, outcome.gpu_peak_bytes)
        log.debug(
            "document done: id=%s slice=%s outcome=%s seconds=%.3f",
            doc.id,
            doc.slice,
            outcome.code,
            elapsed,
        )
        if outcome.code == "deadline":
            timeouts += 1
            if timeouts * limit_den > limit_num * total_docs:
                log.error(
                    "evaluation stopped: extractor=%s deadlines=%d documents=%d",
                    info.name,
                    timeouts,
                    total_docs,
                )
                raise Refused("evaluation_failed", detail_code="timeouts")

        if doc.hostile:
            assert doc.twin is not None
            record = _verified_json(s, f"hostile/{doc.id}.json")
            if record.get("schema") != HOSTILE_SCHEMA or record.get("clean_twin_id") != doc.twin:
                raise _mismatch()
            kind = record.get("kind")
            expected = record.get("expected")
            if not isinstance(kind, str) or _SLICE.fullmatch(kind) is None:
                raise _mismatch()
            if not isinstance(expected, dict):
                raise _mismatch()
            truth = _verified_json(s, f"truth/{doc.twin}.json")
            case = {
                "expected": expected,
                "clean_truth": truth,
                "truth": _verified_json(s, f"truth/{doc.id}.json"),
                "page_count": doc.pages,
            }
            if outcome.code in FAILURE_CODES:
                hostile.append((kind, _hostile_failed()))
            else:
                pred: object = outcome.document
                if pred is None:
                    pred = metrics.Refusal(outcome.refusal or "internal")
                try:
                    hostile.append((kind, metrics.hostile_outcome(case, pred)))
                except Exception as e:
                    log.error("hostile case not scored: slice=%s", doc.slice)
                    raise Refused("evaluation_failed", detail_code="hostile_scoring") from e
        else:
            truth = _verified_json(s, f"truth/{doc.id}.json")
        scored.append((doc.slice, (truth, outcome.document)))

    groups: dict[str, list[tuple[dict[str, Any], dict[str, Any] | None]]] = {
        name: []
        for name in s.slices  # a slice with no documents: reported, not measured
    }
    for name, pair in scored:
        groups[name].append(pair)
    per_slice = {
        "slices": {name: _doc_metrics(groups[name]) for name in sorted(groups)},
        "overall": _doc_metrics([p for name, p in scored if name != HOSTILE_SLICE]),
    }
    total_pages = sum(d.pages for d in s.docs)
    latency: dict[str, Any] = dict(metrics.latency(page_wall))
    latency["throughput_pages_per_minute"] = metrics.throughput(total_pages, sum(page_wall))
    latency["slices"] = {name: metrics.latency(v) for name, v in sorted(by_slice_wall.items())}
    result = RunResult(
        extractor=info,
        testset_sha256=s.sha256,
        purpose=s.manifest["purpose"],
        version=s.manifest["version"],
        extraction_schema_version=s.manifest["extraction_schema_version"],
        documents=total_docs,
        pages=total_pages,
        outcomes=dict(sorted(outcomes.items())),
        outcomes_by_slice={
            k: dict(sorted(v.items())) for k, v in sorted(by_slice_outcomes.items())
        },
        metrics=_not_measured(per_slice),
        hostile=_not_measured(_hostile_summary(hostile)),
        latency=_not_measured(latency),
        extractor_latency=_not_measured(dict(metrics.latency(page_reported))),
        gpu_peak_bytes=gpu_peak,
        per_page_deadline_s=deadline,
        max_output_bytes=max_output_bytes,
    )
    log.info(
        "evaluation done: extractor=%s testset=%s documents=%d pages=%d outcomes=%s "
        "hostile_cases=%d seconds=%.1f",
        info.name,
        s.sha256,
        total_docs,
        total_pages,
        sorted(outcomes.items()),
        len(hostile),
        time.monotonic() - started,
    )
    return result


__all__ = ["HOSTILE_FAILED", "HOSTILE_SLICE", "NOT_MEASURED", "OUTCOME_CODES", "RunResult", "run"]
