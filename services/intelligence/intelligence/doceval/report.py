"""The evaluation report (``mhai.evaluation.report/1``), the pass thresholds per gate, and
the before-and-after comparison.

What goes in
------------
:func:`build_report` takes a :class:`~intelligence.doceval.runner.RunResult` (aggregates
only) and one or more :class:`Thresholds`, and returns the report as a dict of integers,
strings, booleans and ``null`` (no floats: rates are decimal strings with six places,
compared exactly before they are rounded). :func:`dumps` is its canonical JSON
(``evaluation.json``); :func:`render_markdown` is ``evaluation.md``.

Scopes
------
``run`` (every document; latency, throughput, GPU memory and the output, refusal and
timeout shares), ``overall`` (every document except the hostile slice, as the runner
scores it) and ``slices.<name>``. ``slices.hostile`` also carries ``hostile_pass_rate``.

Gates
-----
A thresholds file (``thresholds/<gate>.json``, ``mhai.evaluation.thresholds/1``,
canonical JSON) lists rules per scope: ``min`` (the value must be at least the
threshold), ``max`` (at most), or ``not_applicable`` (an explicit statement that the gate
does not check this metric). A metric with no rule is recorded only.

* **not_measured is never a pass.** A rule whose metric is ``null`` (nothing to measure,
  code exact match in phase 0, a slice the set does not have, latency on hardware other
  than the rule's ``hardware``, no GPU) is ``not_measured`` and fails its gate. The only
  exception is a rule the file marks ``not_applicable``.
* A gate with no rules does not pass.
* Every rule must pass for the gate to pass: a failing slice fails the gate whatever the
  overall numbers are.
* **Stubs pass only the rehearsal gate.** An extractor whose ``engine`` is ``stub`` or
  whose name ends ``-stub`` is refused (``not_allowed_here``) for every other gate.
  ``rehearsal`` is ``production: false``: it exists to rehearse the promotion flow end to
  end and is never accepted for a production pin (``lock-check``, Task 20, refuses it).
* The thresholds are clinical change control: the folder is in CODEOWNERS, and the report
  records each file's name and SHA-256.

What never goes in (P19)
------------------------
No document id, value, page image or per-document row. Groups smaller than
:data:`MIN_CELL` documents are effectively per-document, so:

* a scope with fewer than five documents shows its document count and its gate verdicts
  only: its metrics, field counts and outcome tables are ``"suppressed"``;
* in a larger scope, a document count of 1 to 4, or one that differs from the scope's
  total by 1 to 4, is ``"suppressed"`` (small cells and subtraction, as in the frontend's
  small-group rule);
* a combined scope (``overall``, ``run``) hides a cell whenever the same cell is hidden
  in one of its slices, and its field counts when one of its slices is small, so that
  subtracting the shown slices from it gives nothing away; a share of documents
  (``refusal_share`` and the like) and the hostile pass rate are hidden with their cells;
* hostile results by kind show only whether every case of that kind passed, and only for
  kinds with at least five cases.

Rates over fields (precision, recall, calibration) stay visible in every scope of five
documents or more: they are not document counts, though a small slice's field accuracy
can be estimated from them (residual).

Gate verdicts are computed from the exact values, before any suppression, and are always
shown.
"""

import hashlib
import os
import re
import stat
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from decimal import ROUND_HALF_EVEN, Decimal, InvalidOperation
from fractions import Fraction
from pathlib import Path
from typing import Any

from intelligence.doceval import extractors, metrics, runner
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

REPORT_SCHEMA = "mhai.evaluation.report/1"
THRESHOLDS_SCHEMA = "mhai.evaluation.thresholds/1"
# The document extractor's gates (suite ``clinical_document``), and the answer model's one
# gate (suite ``clinical_answer``, :mod:`intelligence.doceval.answers`). A gate measures one
# suite only: :data:`SUITE_GATES`.
DOCUMENT_GATES: tuple[str, ...] = ("phase1", "phase3", "rehearsal")
ANSWER_GATE = "answer"
GATES: tuple[str, ...] = (*DOCUMENT_GATES, ANSWER_GATE)
PRODUCTION_GATES = frozenset({"phase1", "phase3", ANSWER_GATE})
REHEARSAL_GATE = "rehearsal"
SUITE_GATES: dict[str, tuple[str, ...]] = {
    "clinical_document": DOCUMENT_GATES,
    "clinical_answer": (ANSWER_GATE,),
}
THRESHOLDS_DIR = Path(__file__).resolve().parent / "thresholds"
TESTSET_NAME = "clinical_document"  # sets.SET_NAME (not imported: sets pulls in the renderer)
DEV_LABEL = "development split, not a promotion result"
MIN_CELL = 5
SUPPRESSED = "suppressed"
DECIMALS = 6
HOSTILE_KINDS: tuple[str, ...] = (  # synth.hostile.KINDS
    "blank",
    "injection",
    "rotated",
    "two_patients",
    "wrong_patient_label",
)
HOSTILE_FAILURE_CODES = frozenset(metrics.HOSTILE_FAILURES) | {runner.HOSTILE_FAILED}
STOP_REASONS: dict[str, str] = {
    "timeouts": "Run stopped: more than 5% of the documents passed their deadline.",
    "hostile_scoring": "Run stopped: a hostile case could not be scored.",
}
VERDICTS = ("pass", "fail", "not_measured", "not_applicable")

SILENT_ERROR_DEFINITION = (
    "Silent error: a field in band high, with no flag, whose value is wrong, invented, or "
    "missing (a high-band miss: the truth has a visible value and the prediction leaves it "
    "empty). The rate is silent errors over high-band fields."
)
_NOT_MEASURED_NOTE = (
    "A metric that is not_measured (nothing to measure, or not measurable here) is never a "
    "pass: it fails every gate that requires it, unless that gate's thresholds file marks "
    "it not_applicable."
)
_SMALL_NOTE = (
    "Groups of fewer than 5 documents show gate verdicts only; document counts of 1 to 4, "
    "or within 4 of their total, are suppressed."
)

HIGHER, LOWER, NEUTRAL = "higher", "lower", "neutral"
_FIELD_TYPES: tuple[str, ...] = metrics.FIELD_TYPE_NAMES
DOC_METRICS: dict[str, str] = {
    **{f"field_precision.{t}": HIGHER for t in (*_FIELD_TYPES, "other")},
    **{f"field_recall.{t}": HIGHER for t in (*_FIELD_TYPES, "other")},
    "handwritten_precision": HIGHER,
    "handwritten_recall": HIGHER,
    "identifier_precision": HIGHER,
    "silent_error_rate": LOWER,
    "high_band_share": NEUTRAL,
    "calibration_ece": LOWER,
    "grounding_rate": HIGHER,
    "handwriting_cer": LOWER,
    "code_exact_match": HIGHER,
    "invented_codes": LOWER,
    "latency_p50_s": LOWER,
    "latency_p95_s": LOWER,
}
HOSTILE_METRICS: dict[str, str] = {"hostile_pass_rate": HIGHER}
RUN_METRICS: dict[str, str] = {
    "latency_p50_s": LOWER,
    "latency_p95_s": LOWER,
    "extractor_latency_p50_s": LOWER,
    "extractor_latency_p95_s": LOWER,
    "throughput_pages_per_minute": HIGHER,
    "gpu_peak_bytes": LOWER,
    "invalid_output_share": LOWER,
    "refusal_share": LOWER,
    "timeout_share": LOWER,
}
ALL_METRICS: dict[str, str] = {**DOC_METRICS, **HOSTILE_METRICS, **RUN_METRICS}
SCOPE_DOCUMENTS = "scope_documents"  # the check that a gated scope has enough documents
CHECK_NAMES = frozenset(ALL_METRICS) | {SCOPE_DOCUMENTS}
MAX_MIN_DOCUMENTS = 100_000

_INVALID_OUTPUTS = ("bad_json", "schema_error", "oversize", "extractor_error")
_SCHEMA_ERRORS = ("bad_json", "schema_error", "oversize")
_SCOPE = re.compile(r"run|overall|slices\.[a-z][a-z0-9_]{0,63}", re.ASCII)
_SLICE = re.compile(r"[a-z][a-z0-9_]{0,63}", re.ASCII)
_THRESHOLD = re.compile(r"(0|[1-9][0-9]{0,15})(\.[0-9]{1,6})?", re.ASCII)
_DECIMAL = re.compile(r"(0|[1-9][0-9]{0,15})\.[0-9]{6}", re.ASCII)
# A notebook metric as a signed fixed decimal (ruling D26): anchored, ASCII, six places,
# at most 19 integer digits (training.MAX_METRIC_ABS is 10**18). Never "-0.000000".
_SIGNED_DECIMAL = re.compile(r"-?(0|[1-9][0-9]{0,18})\.[0-9]{6}", re.ASCII)
_NEGATIVE_ZERO = "-0.000000"
_TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._@#:+/-]{0,127}", re.ASCII)
_KEY = re.compile(r"[a-z0-9][a-z0-9_.:-]{0,63}", re.ASCII)
_SHA = re.compile(r"[0-9a-f]{64}", re.ASCII)
_SAFE_TEXT = re.compile(r"[A-Za-z0-9 ._:@#+/,%-]{0,200}", re.ASCII)  # no < > = ` | * [ ]
_NAME = re.compile(r"[a-z0-9][a-z0-9-]{1,62}", re.ASCII)  # extractors.ExtractorInfo.name
_MAX_THRESHOLDS_BYTES = 256 * 1024
_MAX_REPORT_BYTES = 8 * 1024 * 1024
_MAX_TESTSET_BYTES = 64 * 1024 * 1024
_MAX_NOTEBOOK_METRICS = 64

Value = Fraction | int | None


def _bad_input() -> Refused:
    return Refused("evaluation_failed", detail_code="report_input")


def _policy() -> Refused:
    return Refused("policy_invalid")


# ---------------------------------------------------------------- thresholds


@dataclass(frozen=True)
class Rule:
    metric: str
    op: str  # min, max or not_applicable
    value: Fraction | None
    hardware: str | None = None


@dataclass(frozen=True)
class Thresholds:
    gate: str
    production: bool
    file: str  # shown in the report: thresholds/<gate>.json for the packaged files
    sha256: str  # of the file's bytes
    scopes: dict[str, tuple[Rule, ...]]
    min_documents: dict[str, int]  # per gated scope: fewer documents fails the gate


def read_regular(path: Path, cap: int, reason: str) -> bytes:
    """A regular file (never through a symlink) of at most ``cap`` bytes, or ``reason``."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError as e:
        raise Refused(reason) from e
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise Refused(reason)
        if st.st_size > cap:
            raise Refused("too_large")
        with os.fdopen(fd, "rb", closefd=False) as fh:
            data = fh.read(cap + 1)
    except OSError as e:
        raise Refused(reason) from e
    finally:
        os.close(fd)
    if len(data) > cap:
        raise Refused("too_large")
    return data


def _canonical_doc(raw: bytes, cap: int) -> Any:
    obj = canonical.loads_strict(raw, cap)
    if canonical.dumps(obj) != raw:
        raise Refused("not_canonical")
    return obj


# The answer gate's metrics (all over the whole set, scope ``overall``), each higher or
# lower is better: :mod:`intelligence.doceval.answers.score`.
ANSWER_METRICS: dict[str, str] = {
    "escalation_recall": HIGHER,
    "unsafe_rate": LOWER,
    "grounded_pass_rate": HIGHER,
    "fact_coverage": HIGHER,
    "over_escalation_rate": LOWER,
}


def _allowed_metrics(scope: str, gate: str | None = None) -> dict[str, str]:
    if gate == ANSWER_GATE:
        return ANSWER_METRICS if scope == "overall" else {}
    if scope == "run":
        return RUN_METRICS
    if scope == f"slices.{runner.HOSTILE_SLICE}":
        return {**DOC_METRICS, **HOSTILE_METRICS}
    return DOC_METRICS


def _rule(obj: Any, gate: str, scope: str) -> Rule:
    if not isinstance(obj, dict) or not isinstance(obj.get("metric"), str):
        raise _policy()
    metric, op = obj["metric"], obj.get("op")
    if metric not in _allowed_metrics(scope, gate):
        raise _policy()
    if op == "not_applicable":
        if set(obj) != {"metric", "op"}:
            raise _policy()
        return Rule(metric, op, None)
    if op not in ("min", "max") or not set(obj) <= {"metric", "op", "value", "hardware"}:
        raise _policy()
    value = obj.get("value")
    if not isinstance(value, str) or _THRESHOLD.fullmatch(value) is None:
        raise _policy()
    hardware = obj.get("hardware")
    if hardware is not None and (not isinstance(hardware, str) or not _TOKEN.fullmatch(hardware)):
        raise _policy()
    return Rule(metric, op, Fraction(value), hardware)


def load_thresholds(path: str | Path, *, file: str | None = None) -> Thresholds:
    """Load and check one thresholds file; anything unexpected is ``policy_invalid``."""
    p = Path(path)
    raw = read_regular(p, _MAX_THRESHOLDS_BYTES, "policy_invalid")
    obj = _canonical_doc(raw, _MAX_THRESHOLDS_BYTES)
    if not isinstance(obj, dict) or set(obj) != {
        "schema",
        "gate",
        "production",
        "source",
        "scopes",
    }:
        raise _policy()
    gate, production, scopes = obj["gate"], obj["production"], obj["scopes"]
    if obj["schema"] != THRESHOLDS_SCHEMA or gate not in GATES:
        raise _policy()
    if production is not (gate in PRODUCTION_GATES):
        raise _policy()
    if not isinstance(obj["source"], str) or _SAFE_TEXT.fullmatch(obj["source"]) is None:
        raise _policy()
    if not isinstance(scopes, dict):
        raise _policy()
    out: dict[str, tuple[Rule, ...]] = {}
    minimum: dict[str, int] = {}
    for scope, block in scopes.items():
        if _SCOPE.fullmatch(scope) is None or not isinstance(block, dict):
            raise _policy()
        if set(block) != {"min_documents", "rules"} or not isinstance(block["rules"], list):
            raise _policy()
        n = block["min_documents"]
        if isinstance(n, bool) or not isinstance(n, int) or not 1 <= n <= MAX_MIN_DOCUMENTS:
            raise _policy()
        parsed = tuple(_rule(r, gate, scope) for r in block["rules"])
        if len({r.metric for r in parsed}) != len(parsed):
            raise _policy()
        out[scope] = parsed
        minimum[scope] = n
    return Thresholds(
        gate=gate,
        production=production,
        file=file if file is not None else p.name,
        sha256=hashlib.sha256(raw).hexdigest(),
        scopes=out,
        min_documents=minimum,
    )


def packaged_thresholds(gate: str) -> Thresholds:
    """The committed thresholds for ``gate`` (a name from :data:`GATES`, never a path)."""
    if not isinstance(gate, str) or gate not in GATES:
        raise Refused("invalid_name")
    th = load_thresholds(THRESHOLDS_DIR / f"{gate}.json", file=f"thresholds/{gate}.json")
    if th.gate != gate:
        raise _policy()
    return th


def is_stub(info: extractors.ExtractorInfo) -> bool:
    return info.engine == "stub" or info.name.endswith("-stub")


def check_gates(info: extractors.ExtractorInfo, gates: Sequence[Thresholds]) -> None:
    if not gates or len({g.gate for g in gates}) != len(gates):
        raise _policy()
    for g in gates:
        if not isinstance(g, Thresholds) or g.gate not in DOCUMENT_GATES:
            raise _policy()
        if g.gate != REHEARSAL_GATE and is_stub(info):
            raise Refused("not_allowed_here", detail_code="stub_extractor")


# ---------------------------------------------------------------- numbers


def _count(x: object) -> int:
    if isinstance(x, bool) or not isinstance(x, int) or x < 0:
        raise _bad_input()
    return x


def _ratio(num: object, den: object) -> Fraction | None:
    n, d = _count(num), _count(den)
    return None if d == 0 else Fraction(n, d)


def _real(x: object) -> Fraction | None:
    """A runner float (or ``not_measured``/``None``) as an exact fraction."""
    if x is None or x == runner.NOT_MEASURED:
        return None
    if isinstance(x, bool) or not isinstance(x, int | float):
        raise _bad_input()
    try:
        d = Decimal(repr(x))
    except InvalidOperation as e:
        raise _bad_input() from e
    if not d.is_finite():
        raise _bad_input()
    return Fraction(d)


def _fmt(v: Value) -> int | str | None:
    if v is None or isinstance(v, int):
        return v
    d = Decimal(v.numerator) / Decimal(v.denominator)
    return str(d.quantize(Decimal(1).scaleb(-DECIMALS), rounding=ROUND_HALF_EVEN))


def cell(count: int, total: int) -> int | str:
    """A document count as shown: hidden when it, or ``total - count``, is 1 to 4."""
    if total < MIN_CELL:
        return SUPPRESSED
    if 0 < count < MIN_CELL or 0 < total - count < MIN_CELL:
        return SUPPRESSED
    return count


# ---------------------------------------------------------------- scopes


def _pr(c: Any) -> tuple[int, int, int]:
    if not isinstance(c, Mapping):
        raise _bad_input()
    return _count(c.get("tp")), _count(c.get("fp")), _count(c.get("fn"))


def _doc_values(m: Mapping[str, Any]) -> tuple[dict[str, Value], dict[str, Any]]:
    """Every document metric of one scope (exact), and its field-level counts."""
    if not isinstance(m, Mapping) or not isinstance(m.get("field_pr"), Mapping):
        raise _bad_input()
    values: dict[str, Value] = {}
    field_counts: dict[str, dict[str, int]] = {}
    other = [0, 0, 0]
    for ft in _FIELD_TYPES:
        tp, fp, fn = _pr(m["field_pr"].get(ft))
        field_counts[ft] = {"fn": fn, "fp": fp, "tp": tp}
        values[f"field_precision.{ft}"] = _ratio(tp, tp + fp)
        values[f"field_recall.{ft}"] = _ratio(tp, tp + fn)
        if ft != "numeric_lab_value":
            other = [other[0] + tp, other[1] + fp, other[2] + fn]
    values["field_precision.other"] = _ratio(other[0], other[0] + other[1])
    values["field_recall.other"] = _ratio(other[0], other[0] + other[2])
    tp, fp, fn = _pr(m.get("handwritten_pr"))
    values["handwritten_precision"] = _ratio(tp, tp + fp)
    values["handwritten_recall"] = _ratio(tp, tp + fn)
    hand = {"fn": fn, "fp": fp, "tp": tp}
    tp, fp, fn = _pr(m.get("identifier_precision"))
    values["identifier_precision"] = _ratio(tp, tp + fp)
    ident = {"fn": fn, "fp": fp, "tp": tp}
    se = m.get("silent_error")
    hb = m.get("high_band_share")
    g = m.get("grounding")
    cer = m.get("handwriting_cer")
    for block in (se, hb, g, cer):
        if not isinstance(block, Mapping):
            raise _bad_input()
    assert isinstance(se, Mapping) and isinstance(hb, Mapping)
    assert isinstance(g, Mapping) and isinstance(cer, Mapping)
    values["silent_error_rate"] = _ratio(se.get("errors"), se.get("high_band"))
    values["high_band_share"] = _ratio(hb.get("high"), hb.get("fields"))
    values["calibration_ece"] = _real(m.get("calibration_ece"))
    values["grounding_rate"] = _ratio(g.get("grounded"), g.get("total"))
    values["handwriting_cer"] = _ratio(cer.get("edits"), cer.get("chars"))
    cem = m.get("code_exact_match")
    if isinstance(cem, Mapping):
        matched, total, invented = cem.get("matched"), cem.get("total"), cem.get("invented")
        values["code_exact_match"] = None if matched is None else _ratio(matched, total)
        values["invented_codes"] = None if invented is None else _count(invented)
    elif cem is None or cem == runner.NOT_MEASURED:
        values["code_exact_match"] = None
        values["invented_codes"] = None
    else:
        raise _bad_input()
    counts = {
        "calibration_fields": _count(m.get("calibration_fields")),
        "field_pr": field_counts,
        "grounding": {"grounded": _count(g.get("grounded")), "total": _count(g.get("total"))},
        "handwriting_cer": {"chars": _count(cer.get("chars")), "edits": _count(cer.get("edits"))},
        "handwritten_pr": hand,
        "high_band": {"fields": _count(hb.get("fields")), "high": _count(hb.get("high"))},
        "identifier_precision": ident,
        "silent_error": {
            "errors": _count(se.get("errors")),
            "high_band": _count(se.get("high_band")),
        },
    }
    return values, counts


def _latency_values(lat: Any, prefix: str = "") -> dict[str, Value]:
    if not isinstance(lat, Mapping):
        raise _bad_input()
    return {
        f"{prefix}latency_p50_s": _real(lat.get("p50")),
        f"{prefix}latency_p95_s": _real(lat.get("p95")),
    }


def _outcome_counts(outcomes: Any) -> dict[str, int]:
    if not isinstance(outcomes, Mapping):
        raise _bad_input()
    out: dict[str, int] = {}
    for code, n in outcomes.items():
        if code not in runner.OUTCOME_CODES:
            raise _bad_input()
        out[code] = _count(n)
    return out


def _outcome_block(outcomes: dict[str, int], documents: int) -> dict[str, Any]:
    refusals = {k.split(":", 1)[1]: v for k, v in outcomes.items() if k.startswith("refused:")}
    blocks: dict[str, Any] = {
        "outcomes": dict(sorted(outcomes.items())),
        "refusals": dict(sorted(refusals.items())),
        "schema_errors": sum(outcomes.get(k, 0) for k in _SCHEMA_ERRORS),
        "extractor_errors": outcomes.get("extractor_error", 0),
        "timeouts": outcomes.get("deadline", 0),
    }
    if documents < MIN_CELL:
        return dict.fromkeys(blocks, SUPPRESSED)
    return {
        "outcomes": {k: cell(v, documents) for k, v in blocks["outcomes"].items()},
        "refusals": {k: cell(v, documents) for k, v in blocks["refusals"].items()},
        "schema_errors": cell(blocks["schema_errors"], documents),
        "extractor_errors": cell(blocks["extractor_errors"], documents),
        "timeouts": cell(blocks["timeouts"], documents),
    }


def _share(outcomes: dict[str, int], codes: Iterable[str], documents: int) -> Fraction | None:
    if documents == 0:
        return None
    wanted = frozenset(codes)
    return Fraction(sum(n for c, n in outcomes.items() if c in wanted), documents)


@dataclass
class _Scopes:
    values: dict[str, dict[str, Value]]  # exact, for the gates
    shown: dict[str, dict[str, Any]]  # what the report shows
    documents: dict[str, int]  # per scope


def _scopes(result: runner.RunResult) -> _Scopes:
    scored = result.metrics
    if not isinstance(scored, Mapping) or not isinstance(scored.get("slices"), Mapping):
        raise _bad_input()
    by_slice = _outcome_by_slice(result)
    values: dict[str, dict[str, Value]] = {}
    shown: dict[str, dict[str, Any]] = {}
    lat_slices = result.latency.get("slices") if isinstance(result.latency, Mapping) else None
    if not isinstance(lat_slices, Mapping):
        raise _bad_input()

    def add(
        scope: str, m: Mapping[str, Any], outcomes: dict[str, int], extra: dict[str, Value]
    ) -> None:
        documents = _count(m.get("documents"))
        vals, counts = _doc_values(m)
        vals.update(extra)
        values[scope] = vals
        small = documents < MIN_CELL
        shown[scope] = {
            "documents": documents,
            "metrics": SUPPRESSED if small else {k: _fmt(v) for k, v in sorted(vals.items())},
            "counts": SUPPRESSED if small else counts,
            **_outcome_block(outcomes, documents),
        }

    for name, m in sorted(scored["slices"].items()):
        if not isinstance(name, str) or _SLICE.fullmatch(name) is None:
            raise _bad_input()
        extra = _latency_values(lat_slices.get(name))
        if name == runner.HOSTILE_SLICE:
            extra["hostile_pass_rate"] = _hostile_rate(result.hostile)
        add(f"slices.{name}", m, by_slice.get(name, {}), extra)
    overall_outcomes: dict[str, int] = {}
    for name, oc in by_slice.items():
        if name != runner.HOSTILE_SLICE:
            for k, v in oc.items():
                overall_outcomes[k] = overall_outcomes.get(k, 0) + v
    overall = scored.get("overall")
    if not isinstance(overall, Mapping):
        raise _bad_input()
    add("overall", overall, overall_outcomes, {})  # latency is per slice and for the run

    documents = _count(result.documents)
    outcomes = _outcome_counts(result.outcomes)
    if sum(outcomes.values()) != documents:
        raise _bad_input()
    run_vals: dict[str, Value] = {
        **_latency_values(result.latency),
        **_latency_values(result.extractor_latency, "extractor_"),
        "throughput_pages_per_minute": _real(result.latency.get("throughput_pages_per_minute")),
        "gpu_peak_bytes": None if result.gpu_peak_bytes is None else _count(result.gpu_peak_bytes),
        "invalid_output_share": _share(outcomes, _INVALID_OUTPUTS, documents),
        "refusal_share": _share(
            outcomes, (c for c in outcomes if c.startswith("refused:")), documents
        ),
        "timeout_share": _share(outcomes, ("deadline",), documents),
    }
    values["run"] = run_vals
    small = documents < MIN_CELL
    shown["run"] = {
        "documents": documents,
        "pages": _count(result.pages),
        "metrics": SUPPRESSED if small else {k: _fmt(v) for k, v in sorted(run_vals.items())},
        **_outcome_block(outcomes, documents),
    }
    if not result.extractor.confidence_scored:
        _unscored(values, shown)
    slices = [k for k in shown if k.startswith("slices.")]
    _secondary(shown, "overall", [k for k in slices if k != f"slices.{runner.HOSTILE_SLICE}"])
    _secondary(shown, "run", slices)
    _suppress_shares(shown["run"])
    return _Scopes(values, shown, {k: v["documents"] for k, v in shown.items()})


_CONFIDENCE_METRICS = ("calibration_ece", "silent_error_rate", "high_band_share")


def _unscored(values: dict[str, dict[str, Value]], shown: dict[str, dict[str, Any]]) -> None:
    """An extractor whose confidences are placeholders: the metrics built on confidence
    are not measured (never a pass, never a number)."""
    for scope, vals in values.items():
        if scope == "run":
            continue
        for k in _CONFIDENCE_METRICS:
            vals[k] = None
            if isinstance(shown[scope]["metrics"], dict):
                shown[scope]["metrics"][k] = None


def _secondary(shown: dict[str, dict[str, Any]], combined: str, parts: Sequence[str]) -> None:
    """Hide a combined scope's cells that would give away a hidden cell of one of its
    parts by subtraction (the combined count minus the shown parts)."""
    block = shown[combined]
    blocks = [shown[p] for p in parts]
    if "counts" in block and any(b["documents"] < MIN_CELL for b in blocks):
        block["counts"] = SUPPRESSED
    for key in ("schema_errors", "extractor_errors", "timeouts"):
        if any(b[key] == SUPPRESSED for b in blocks):
            block[key] = SUPPRESSED
    for key in ("outcomes", "refusals"):
        if not isinstance(block[key], dict):
            continue
        for code in block[key]:
            if any(b[key] == SUPPRESSED or b[key].get(code) == SUPPRESSED for b in blocks):
                block[key][code] = SUPPRESSED


def _suppress_shares(run_block: dict[str, Any]) -> None:
    """A share of documents is a count in disguise: shown only when its cells are."""
    m, outcomes = run_block["metrics"], run_block["outcomes"]
    if not isinstance(m, dict) or not isinstance(outcomes, dict):
        return
    codes = {
        "invalid_output_share": set(_INVALID_OUTPUTS),
        "refusal_share": {c for c in outcomes if c.startswith("refused:")},
        "timeout_share": {"deadline"},
    }
    for share, wanted in codes.items():
        if any(outcomes.get(c) == SUPPRESSED for c in wanted):
            m[share] = SUPPRESSED


def _outcome_by_slice(result: runner.RunResult) -> dict[str, dict[str, int]]:
    if not isinstance(result.outcomes_by_slice, Mapping):
        raise _bad_input()
    return {name: _outcome_counts(oc) for name, oc in result.outcomes_by_slice.items()}


def _hostile_rate(h: Any) -> Fraction | None:
    if not isinstance(h, Mapping):
        raise _bad_input()
    return _ratio(h.get("passed"), h.get("cases"))


def _failure_counts(failures: Mapping[Any, Any]) -> dict[str, int]:
    """Failure codes from the closed set; anything else is counted as ``other``."""
    out: dict[str, int] = {}
    for f, n in failures.items():
        name = f if f in HOSTILE_FAILURE_CODES else "other"
        out[name] = out.get(name, 0) + _count(n)
    return out


def _hostile_block(h: Any) -> dict[str, Any]:
    if not isinstance(h, Mapping):
        raise _bad_input()
    cases = _count(h.get("cases"))
    rate = _ratio(h.get("passed"), cases)
    keys = ("flagged_or_refused", "other_fields_unchanged", "passed")
    counts = {k: _count(h.get(k)) for k in keys}
    failures = h.get("failures")
    by_kind = h.get("by_kind")
    if not isinstance(failures, Mapping) or not isinstance(by_kind, Mapping):
        raise _bad_input()
    kinds: dict[str, Any] = {}
    for kind, k in by_kind.items():
        if not isinstance(k, Mapping):
            raise _bad_input()
        name = kind if kind in HOSTILE_KINDS else "other"
        n, passed = _count(k.get("cases")), _count(k.get("passed"))
        prev = kinds.get(name, (0, 0))
        kinds[name] = (prev[0] + n, prev[1] + passed)
    shown_kinds = {
        name: SUPPRESSED if n < MIN_CELL else {"all_passed": passed == n}
        for name, (n, passed) in sorted(kinds.items())
    }
    if cases < MIN_CELL:
        return {
            "cases": SUPPRESSED,
            **dict.fromkeys(keys, SUPPRESSED),
            "rate": SUPPRESSED,
            "failures": SUPPRESSED,
            "by_kind": shown_kinds,
        }
    passed_cell = cell(counts["passed"], cases)
    return {
        "cases": cases,
        **{k: cell(v, cases) for k, v in counts.items()},
        "rate": SUPPRESSED if passed_cell == SUPPRESSED else _fmt(rate),
        "failures": {f: cell(n, cases) for f, n in sorted(_failure_counts(failures).items())},
        "by_kind": shown_kinds,
    }


# ---------------------------------------------------------------- the gates


def _evaluate(
    th: Thresholds,
    values: Mapping[str, Mapping[str, Value]],
    documents: Mapping[str, int],
    hardware: str,
) -> dict[str, Any]:
    results: dict[str, dict[str, str]] = {}
    for scope, rules in sorted(th.scopes.items()):
        scope_values = values.get(scope)
        out: dict[str, str] = {}
        enough = documents.get(scope, 0) >= th.min_documents[scope]
        out[SCOPE_DOCUMENTS] = "pass" if enough else "fail"
        for r in rules:
            if r.op == "not_applicable":
                out[r.metric] = "not_applicable"
                continue
            v = None if scope_values is None else scope_values.get(r.metric)
            if v is None or (r.hardware is not None and r.hardware != hardware):
                out[r.metric] = "not_measured"
                continue
            assert r.value is not None
            ok = v >= r.value if r.op == "min" else v <= r.value
            out[r.metric] = "pass" if ok else "fail"
        results[scope] = dict(sorted(out.items()))
    failed = sorted(
        f"{scope}.{metric}"
        for scope, out in results.items()
        for metric, verdict in out.items()
        if verdict in ("fail", "not_measured")
    )
    checked = any(r.op != "not_applicable" for rules in th.scopes.values() for r in rules)
    return {
        "verdict": "pass" if checked and not failed else "fail",
        "results": results,
        "failed": failed,
    }


# ---------------------------------------------------------------- building the report


def evaluate_rules(
    th: Thresholds, values: Mapping[str, Mapping[str, Value]], counts: Mapping[str, int]
) -> dict[str, Any]:
    """One gate's verdict over ``values`` (scope -> metric -> exact value or ``None``) with
    ``counts`` (scope -> how many it measured), by the same rules as a document report: a
    ``None`` metric is ``not_measured`` and fails, too few fails, a gate with no rule fails."""
    return _evaluate(th, values, counts, "cpu")


def decimal(v: Fraction | None) -> str | None:
    """A rate as a report shows it: a decimal string with six places, or ``None``."""
    out = _fmt(v)
    assert not isinstance(out, int)
    return out


def check_split(purpose: object) -> dict[str, Any]:
    """The report's split for a set's purpose: ``dev`` (labelled) or ``test``; anything
    else is refused (``not_allowed_here``). Call it before a run, not only after."""
    return _split(purpose)


def _split(purpose: object) -> dict[str, Any]:
    if purpose == "test":
        return {"name": "test", "label": None}
    if purpose == "dev":
        return {"name": "dev", "label": DEV_LABEL}
    raise Refused("not_allowed_here", detail_code="split")


def is_token(x: object) -> bool:
    """A short plain name (versions, engines, hardware): letters, digits and ``._@#:+/-``."""
    return isinstance(x, str) and _TOKEN.fullmatch(x) is not None


def _token(x: object) -> str:
    if not isinstance(x, str) or _TOKEN.fullmatch(x) is None:
        raise _bad_input()
    return x


def _notebook(metrics: Mapping[str, Any] | None) -> dict[str, Any] | None:
    if metrics is None:
        return None
    if not isinstance(metrics, Mapping) or len(metrics) > _MAX_NOTEBOOK_METRICS:
        raise _bad_input()
    out: dict[str, Any] = {}
    for k, v in metrics.items():
        if not isinstance(k, str) or _KEY.fullmatch(k) is None:
            raise _bad_input()
        if not _is_notebook_value(v):
            raise _bad_input()
        out[k] = v
    return {"informational": True, "metrics": dict(sorted(out.items()))}


def _is_notebook_value(v: object) -> bool:
    """A notebook metric as the report holds it: a count, a plain token, or a signed
    fixed decimal (``-0.000000`` is never one)."""
    if isinstance(v, bool):
        return False
    if isinstance(v, int):
        return v >= 0
    if not isinstance(v, str) or v == _NEGATIVE_ZERO:
        return False
    return _SIGNED_DECIMAL.fullmatch(v) is not None or _TOKEN.fullmatch(v) is not None


def _header(
    info: extractors.ExtractorInfo,
    testset: Mapping[str, Any],
    gates: Sequence[Thresholds],
    engine_version: str | None,
) -> dict[str, Any]:
    info = extractors.check_info(info)
    check_gates(info, gates)
    sha = testset.get("sha256")
    if not isinstance(sha, str) or _SHA.fullmatch(sha) is None:
        raise _bad_input()
    return {
        "schema": REPORT_SCHEMA,
        "extractor": {"name": info.name},
        "model_versions": dict(sorted(info.model_versions.items())),
        "engine": {
            "name": info.engine,
            "version": None if engine_version is None else _token(engine_version),
        },
        "hardware": info.hardware,
        "testset": {
            "name": _token(testset.get("name")),
            "version": _token(testset.get("version")),
            "sha256": sha,
            "extraction_schema_version": _token(testset.get("extraction_schema_version")),
            "documents": _count(testset.get("documents")),
            "pages": _count(testset.get("pages")),
        },
        "split": _split(testset.get("purpose")),
        "thresholds": {
            g.gate: {"file": g.file, "sha256": g.sha256, "production": g.production}
            for g in sorted(gates, key=lambda g: g.gate)
        },
    }


def build_report(
    result: runner.RunResult,
    gates: Sequence[Thresholds],
    *,
    testset_name: str = TESTSET_NAME,
    engine_version: str | None = None,
    notebook_metrics: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """The report for one run, checked against each gate in ``gates``.

    ``notebook_metrics``: copied from the training manifest, shown as informational only
    (never compared with a threshold). Raises ``Refused("not_allowed_here")`` for a stub
    extractor and any gate but ``rehearsal``, and for a split other than ``dev`` or
    ``test``.
    """
    testset = {
        "name": testset_name,
        "version": result.version,
        "sha256": result.testset_sha256,
        "extraction_schema_version": result.extraction_schema_version,
        "documents": result.documents,
        "pages": result.pages,
        "purpose": result.purpose,
    }
    head = _header(result.extractor, testset, gates, engine_version)
    scopes = _scopes(result)
    hostile_block = _hostile_block(result.hostile)
    hostile_scope = scopes.shown.get(f"slices.{runner.HOSTILE_SLICE}")
    if hostile_scope is not None and isinstance(hostile_scope["metrics"], dict):
        if hostile_block["passed"] == SUPPRESSED:
            hostile_scope["metrics"]["hostile_pass_rate"] = SUPPRESSED
    rep = {
        **head,
        "stopped": None,
        "run": {
            "per_page_deadline_s": _fmt(_real(result.per_page_deadline_s)),
            "max_output_bytes": _count(result.max_output_bytes),
        },
        "scopes": dict(sorted(scopes.shown.items())),
        "hostile": hostile_block,
        "verdicts": {
            g.gate: _evaluate(g, scopes.values, scopes.documents, result.extractor.hardware)
            for g in sorted(gates, key=lambda g: g.gate)
        },
        "notebook_metrics": _notebook(notebook_metrics),
    }
    _parse(dumps(rep), any_thresholds_file=True)  # what we write must read back
    return rep


def testset_meta(set_dir: str | Path) -> dict[str, Any]:
    """What a report may say about a set: name, version, purpose, schema version, totals
    and the SHA-256 of ``testset.json`` (never its document list)."""
    raw = read_regular(Path(set_dir) / "testset.json", _MAX_TESTSET_BYTES, "testset_mismatch")
    try:
        manifest = _canonical_doc(raw, _MAX_TESTSET_BYTES)
    except Refused as e:
        raise Refused("testset_mismatch") from e
    if not isinstance(manifest, dict) or manifest.get("schema") != runner.TESTSET_SCHEMA:
        raise Refused("testset_mismatch")
    totals = manifest.get("totals")
    if not isinstance(totals, dict):
        raise Refused("testset_mismatch")
    return {
        "name": manifest.get("name"),
        "version": manifest.get("version"),
        "purpose": manifest.get("purpose"),
        "extraction_schema_version": manifest.get("extraction_schema_version"),
        "documents": totals.get("documents"),
        "pages": totals.get("pages"),
        "sha256": hashlib.sha256(raw).hexdigest(),
        # Never shown; refuses the oracle (cli). The two fields of a locked split
        # (modelstore.testsets), whatever their value.
        "locked": "locked" in manifest or "store_version_ids" in manifest,
    }


def stopped_report(
    info: extractors.ExtractorInfo,
    *,
    testset: Mapping[str, Any],
    reason: str,
    gates: Sequence[Thresholds],
    engine_version: str | None = None,
) -> dict[str, Any]:
    """The report of a run the runner stopped (``timeouts``: more than 5% of documents
    past their deadline; ``hostile_scoring``). Every gate fails; no metric is shown."""
    if reason not in STOP_REASONS:
        raise _bad_input()
    head = _header(info, testset, gates, engine_version)
    rep = {
        **head,
        "stopped": reason,
        "run": None,
        "scopes": {},
        "hostile": None,
        "verdicts": {
            g.gate: {"verdict": "fail", "results": {}, "failed": ["run.stopped"]}
            for g in sorted(gates, key=lambda g: g.gate)
        },
        "notebook_metrics": None,
    }
    _parse(dumps(rep), any_thresholds_file=True)
    return rep


def dumps(rep: Mapping[str, Any]) -> bytes:
    """``evaluation.json``: canonical JSON."""
    return canonical.dumps(rep)


# ---------------------------------------------------------------- reading a report


_TOP = {
    "schema",
    "extractor",
    "model_versions",
    "engine",
    "hardware",
    "testset",
    "split",
    "thresholds",
    "stopped",
    "run",
    "scopes",
    "hostile",
    "verdicts",
    "notebook_metrics",
}


def _walk_safe(obj: Any) -> None:
    stack = [obj]
    while stack:
        item = stack.pop()
        if isinstance(item, str):
            if _SAFE_TEXT.fullmatch(item) is None:
                raise _policy_report()
        elif isinstance(item, dict):
            for k, v in item.items():
                if _SAFE_TEXT.fullmatch(k) is None:
                    raise _policy_report()
                stack.append(v)
        elif isinstance(item, list):
            stack.extend(item)


def _policy_report() -> Refused:
    return Refused("manifest_invalid", detail_code="report")


def _req(ok: bool) -> None:
    if not ok:
        raise _policy_report()


def _is_count(x: Any, suppressed: bool = True) -> bool:
    if suppressed and x == SUPPRESSED:
        return True
    return isinstance(x, int) and not isinstance(x, bool) and x >= 0


def _is_value(x: Any) -> bool:
    """A metric as shown: null, a count, a six-place decimal, or suppressed."""
    if x is None or _is_count(x):
        return True
    return isinstance(x, str) and _DECIMAL.fullmatch(x) is not None


def _is_token(x: Any, pattern: re.Pattern[str] = _TOKEN) -> bool:
    return isinstance(x, str) and pattern.fullmatch(x) is not None


def _exact(d: Any, keys: Iterable[str]) -> bool:
    return isinstance(d, dict) and set(d) == set(keys)


def _counts_map(d: Any, keys: Iterable[str]) -> None:
    _req(_exact(d, keys) and all(_is_count(v, False) for v in d.values()))


_PR_KEYS = ("tp", "fp", "fn")
_COUNT_BLOCKS = {
    "grounding": ("grounded", "total"),
    "handwriting_cer": ("chars", "edits"),
    "handwritten_pr": _PR_KEYS,
    "high_band": ("fields", "high"),
    "identifier_precision": _PR_KEYS,
    "silent_error": ("errors", "high_band"),
}
_HOSTILE_KEYS = (
    "cases",
    "flagged_or_refused",
    "other_fields_unchanged",
    "passed",
    "rate",
    "failures",
    "by_kind",
)


def _check_field_counts(c: Any) -> None:
    if c == SUPPRESSED:
        return
    _req(_exact(c, {"calibration_fields", "field_pr", *_COUNT_BLOCKS}))
    _req(_is_count(c["calibration_fields"], False))
    _req(_exact(c["field_pr"], _FIELD_TYPES))
    for v in c["field_pr"].values():
        _counts_map(v, _PR_KEYS)
    for key, keys in _COUNT_BLOCKS.items():
        _counts_map(c[key], keys)


def _check_cells(d: Any, allowed: Iterable[str]) -> None:
    if d == SUPPRESSED:
        return
    allowed = set(allowed)
    _req(isinstance(d, dict) and set(d) <= allowed and all(_is_count(v) for v in d.values()))


def _check_scope(scope: str, b: Any) -> None:
    _req(_SCOPE.fullmatch(scope) is not None)
    run = scope == "run"
    keys = {"documents", "metrics", "outcomes", "refusals", "schema_errors", "extractor_errors"}
    keys |= {"timeouts", "pages" if run else "counts"}
    _req(_exact(b, keys))
    _req(_is_count(b["documents"], False))
    if run:
        _req(_is_count(b["pages"], False))
    else:
        _check_field_counts(b["counts"])
    m = b["metrics"]
    if m != SUPPRESSED:
        allowed = _allowed_metrics(scope)
        _req(isinstance(m, dict) and set(m) <= set(allowed))
        _req(all(_is_value(v) for v in m.values()))
    for key in ("schema_errors", "extractor_errors", "timeouts"):
        _req(_is_count(b[key]))
    _check_cells(b["outcomes"], runner.OUTCOME_CODES)
    _check_cells(b["refusals"], extractors.REFUSAL_REASONS)


def _check_hostile(h: Any) -> None:
    _req(_exact(h, _HOSTILE_KEYS))
    for key in ("cases", "flagged_or_refused", "other_fields_unchanged", "passed"):
        _req(_is_count(h[key]))
    _req(_is_value(h["rate"]))
    _check_cells(h["failures"], HOSTILE_FAILURE_CODES | {"other"})
    kinds = h["by_kind"]
    _req(isinstance(kinds, dict) and set(kinds) <= set(HOSTILE_KINDS) | {"other"})
    for v in kinds.values():
        _req(v == SUPPRESSED or (_exact(v, ("all_passed",)) and isinstance(v["all_passed"], bool)))


def _check_verdict(gate: str, v: Any, stopped: bool) -> None:
    _req(gate in DOCUMENT_GATES and _exact(v, ("verdict", "results", "failed")))
    _req(v["verdict"] in ("pass", "fail"))
    results = v["results"]
    _req(isinstance(results, dict))
    for scope, out in results.items():
        _req(_SCOPE.fullmatch(scope) is not None and isinstance(out, dict))
        _req(set(out) <= CHECK_NAMES and all(x in VERDICTS for x in out.values()))
    expected = sorted(
        f"{scope}.{metric}"
        for scope, out in results.items()
        for metric, verdict in out.items()
        if verdict in ("fail", "not_measured")
    )
    if stopped:
        _req(results == {} and v["failed"] == ["run.stopped"] and v["verdict"] == "fail")
    else:
        _req(v["failed"] == expected)
        _req(v["verdict"] == "fail" or not expected)


def _check_header(obj: dict[str, Any], any_thresholds_file: bool = False) -> None:
    _req(_exact(obj["extractor"], ("name",)) and _is_token(obj["extractor"]["name"], _NAME))
    mv = obj["model_versions"]
    _req(isinstance(mv, dict) and len(mv) <= 16)
    _req(all(_is_token(k) and _is_token(v) for k, v in mv.items()))
    engine = obj["engine"]
    _req(_exact(engine, ("name", "version")) and _is_token(engine["name"]))
    _req(engine["version"] is None or _is_token(engine["version"]))
    _req(_is_token(obj["hardware"]))
    ts = obj["testset"]
    keys = ("name", "version", "sha256", "extraction_schema_version", "documents", "pages")
    _req(_exact(ts, keys))
    _req(all(_is_token(ts[k]) for k in ("name", "version", "extraction_schema_version")))
    _req(_is_token(ts["sha256"], _SHA))
    _req(_is_count(ts["documents"], False) and _is_count(ts["pages"], False))
    split = obj["split"]
    _req(_exact(split, ("name", "label")) and split["name"] in ("dev", "test"))
    _req(split["label"] == (DEV_LABEL if split["name"] == "dev" else None))
    th = obj["thresholds"]
    _req(isinstance(th, dict) and bool(th) and set(th) == set(obj["verdicts"]))
    for gate, t in th.items():
        _req(gate in DOCUMENT_GATES and _exact(t, ("file", "sha256", "production")))
        packaged = t["file"] == f"thresholds/{gate}.json"
        # building a report, a test's own thresholds file may be named plainly
        _req(packaged or (any_thresholds_file and _is_token(t["file"], _KEY)))
        _req(_is_token(t["sha256"], _SHA))
        _req(t["production"] is (gate in PRODUCTION_GATES))


def _check_notebook(nb: Any) -> None:
    if nb is None:
        return
    _req(_exact(nb, ("informational", "metrics")) and nb["informational"] is True)
    m = nb["metrics"]
    _req(isinstance(m, dict) and len(m) <= _MAX_NOTEBOOK_METRICS)
    for k, v in m.items():
        _req(_is_token(k, _KEY))
        _req(_is_notebook_value(v))


def read_report(path: str | Path) -> dict[str, Any]:
    """:func:`parse_report` of a file (a regular file, never followed through a symlink)."""
    return parse_report(read_regular(Path(path), _MAX_REPORT_BYTES, "not_found"))


def parse_report(raw: bytes) -> dict[str, Any]:
    """A report read back (``report``, ``compare``). Everything that reaches the Markdown is
    checked against the pattern or closed set it was built from (names, tokens, hashes,
    scope, metric, outcome, refusal, failure and kind names, counts and decimals), each
    thresholds file is named as the packaged file for its gate (the name only: its
    contents and hash are not compared with the packaged file here), the failed lists
    agree with the results, and no string anywhere can carry markup."""
    return _parse(raw)


def _parse(raw: bytes, *, any_thresholds_file: bool = False) -> dict[str, Any]:
    obj = _canonical_doc(raw, _MAX_REPORT_BYTES)
    _req(isinstance(obj, dict) and set(obj) == _TOP and obj["schema"] == REPORT_SCHEMA)
    _walk_safe(obj)
    stopped = obj["stopped"]
    _req(stopped is None or stopped in STOP_REASONS)
    _req(isinstance(obj["verdicts"], dict) and bool(obj["verdicts"]))
    _check_header(obj, any_thresholds_file)
    for gate, v in obj["verdicts"].items():
        _check_verdict(gate, v, stopped is not None)
    scopes = obj["scopes"]
    _req(isinstance(scopes, dict))
    if stopped is not None:
        _req(scopes == {} and obj["run"] is None and obj["hostile"] is None)
        _req(obj["notebook_metrics"] is None)
    else:
        run = obj["run"]
        _req(_exact(run, ("per_page_deadline_s", "max_output_bytes")))
        _req(_is_token(run["per_page_deadline_s"], _DECIMAL))
        _req(_is_count(run["max_output_bytes"], False))
        _req("run" in scopes and "overall" in scopes)
        for scope, block in scopes.items():
            _check_scope(scope, block)
        _check_hostile(obj["hostile"])
        _check_notebook(obj["notebook_metrics"])
    checked: dict[str, Any] = obj
    return checked


# ---------------------------------------------------------------- Markdown


def _show(v: Any) -> str:
    if v is None:
        return "not_measured"
    if isinstance(v, bool):
        return "yes" if v else "no"
    return str(v)


def _scope_failing(rep: Mapping[str, Any], scope: str) -> bool:
    for v in rep["verdicts"].values():
        for verdict in v["results"].get(scope, {}).values():
            if verdict in ("fail", "not_measured"):
                return True
    return False


def _scope_order(rep: Mapping[str, Any]) -> list[str]:
    slices = sorted(s for s in rep["scopes"] if s.startswith("slices."))
    failing = [s for s in slices if _scope_failing(rep, s)]
    passing = [s for s in slices if s not in failing]
    rest = [s for s in ("overall", "run") if s in rep["scopes"]]
    return failing + passing + rest


def _table(header: Sequence[str], rows: Iterable[Sequence[str]]) -> list[str]:
    out = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    out.extend("| " + " | ".join(r) + " |" for r in rows)
    return out


def render_markdown(rep: Mapping[str, Any]) -> str:
    """``evaluation.md``: the same report as tables, a failing slice first."""
    gates = sorted(rep["verdicts"])
    ts = rep["testset"]
    lines = [f"# Evaluation: {rep['extractor']['name']}", ""]
    if rep["split"]["label"]:
        lines += [f"**{rep['split']['label']}**", ""]
    versions = ", ".join(f"{k} {v}" for k, v in rep["model_versions"].items()) or "none"
    engine = rep["engine"]
    lines += _table(
        ("Item", "Value"),
        [
            ("Extractor", rep["extractor"]["name"]),
            ("Model versions", versions),
            ("Engine", f"{engine['name']} {_show(engine['version'])}"),
            ("Hardware", rep["hardware"]),
            ("Test set", f"{ts['name']} {ts['version']}"),
            ("Test set SHA-256", ts["sha256"]),
            ("Extraction schema", ts["extraction_schema_version"]),
            ("Split", rep["split"]["name"]),
            ("Documents / pages", f"{ts['documents']} / {ts['pages']}"),
        ],
    )
    lines += ["", "## Gates", ""]
    lines += _table(
        ("Gate", "Verdict", "Production", "Thresholds file", "SHA-256"),
        [
            (
                g,
                rep["verdicts"][g]["verdict"].upper(),
                "yes" if rep["thresholds"][g]["production"] else "no (rehearsal only)",
                rep["thresholds"][g]["file"],
                rep["thresholds"][g]["sha256"],
            )
            for g in gates
        ],
    )
    for g in gates:
        failed = rep["verdicts"][g]["failed"]
        if failed:
            lines += ["", f"Failed or not measured in {g}: " + ", ".join(failed) + "."]
    if rep["stopped"]:
        lines += ["", f"**{STOP_REASONS[rep['stopped']]}** No metric is reported."]
    lines += [
        "",
        "## Definitions",
        "",
        SILENT_ERROR_DEFINITION,
        "",
        _NOT_MEASURED_NOTE,
        "",
        _SMALL_NOTE,
    ]

    for scope in _scope_order(rep):
        block = rep["scopes"][scope]
        verdict_bits = []
        for g in gates:
            res = rep["verdicts"][g]["results"].get(scope)
            if res:
                bad = any(v in ("fail", "not_measured") for v in res.values())
                verdict_bits.append(f"{g} {'FAIL' if bad else 'pass'}")
        title = scope if not verdict_bits else f"{scope} ({', '.join(verdict_bits)})"
        lines += ["", f"## {title}", "", f"Documents: {block['documents']}", ""]
        metrics = block["metrics"]
        names = set() if metrics == SUPPRESSED else set(metrics)
        for g in gates:
            names |= set(rep["verdicts"][g]["results"].get(scope, {}))
        rows = []
        for name in sorted(names):
            if name == SCOPE_DOCUMENTS:
                value = str(block["documents"])
            else:
                value = SUPPRESSED if metrics == SUPPRESSED else _show(metrics.get(name))
            row = [name, value]
            for g in gates:
                row.append(rep["verdicts"][g]["results"].get(scope, {}).get(name, "recorded"))
            rows.append(row)
        lines += _table(("Metric", "Value", *gates), rows)
        if block["outcomes"] == SUPPRESSED:
            lines += ["", "Outcomes: suppressed (fewer than 5 documents)."]
        else:
            lines += [""]
            lines += _table(
                ("Outcome", "Documents"), [(k, _show(v)) for k, v in block["outcomes"].items()]
            )
            lines += [
                "",
                f"Schema errors: {_show(block['schema_errors'])}. "
                f"Extractor errors: {_show(block['extractor_errors'])}. "
                f"Timeouts: {_show(block['timeouts'])}. "
                "Refusals: "
                + (", ".join(f"{k} {_show(v)}" for k, v in block["refusals"].items()) or "none")
                + ".",
            ]
    h = rep["hostile"]
    if h is not None:
        lines += ["", "## Hostile cases", ""]
        lines += _table(
            ("Item", "Value"),
            [
                (k, _show(h[k]))
                for k in ("cases", "passed", "rate", "flagged_or_refused", "other_fields_unchanged")
            ],
        )
        if isinstance(h["failures"], dict) and h["failures"]:
            lines += [""] + _table(
                ("Failure", "Cases"), [(k, _show(v)) for k, v in h["failures"].items()]
            )
        if h["by_kind"]:
            kind_rows = [
                (k, v if v == SUPPRESSED else ("yes" if v["all_passed"] else "no"))
                for k, v in h["by_kind"].items()
            ]
            lines += [""] + _table(("Kind", "All passed"), kind_rows)
    if rep["run"] is not None:
        lines += [
            "",
            f"Per-page deadline: {rep['run']['per_page_deadline_s']} s. "
            f"Output cap: {rep['run']['max_output_bytes']} bytes.",
        ]
    nb = rep["notebook_metrics"]
    if nb is not None:
        lines += ["", "## Notebook metrics (informational, not computed by this run)", ""]
        lines += _table(("Metric", "Value"), [(k, _show(v)) for k, v in nb["metrics"].items()])
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------- compare


def _num(v: Any) -> Decimal | None:
    if v is None or v == SUPPRESSED or isinstance(v, bool):
        return None
    if isinstance(v, int):
        return Decimal(v)
    try:
        return Decimal(v)
    except (InvalidOperation, TypeError):
        return None


_RANK = {"pass": 2, "not_applicable": 2, "not_measured": 0, "fail": 0}


def compare(before: Mapping[str, Any], after: Mapping[str, Any]) -> str:
    """Markdown for the ``clinical-review`` PR: every gate, scope and metric, before, after
    and the change. Regressions (a worse value, a measured metric that is no longer
    measured or no longer reported, a worse verdict, a gate that no longer passes) are
    listed first, all of them, and marked in the tables."""
    regressions: list[str] = []
    notes: list[str] = []
    for key, label in (("testset", "test sets"), ("split", "splits")):
        if before[key] != after[key]:
            notes.append(
                f"Warning: different {label} before and after; the numbers are not like for like."
            )
    for g in sorted(set(before["thresholds"]) | set(after["thresholds"])):
        b, a = before["thresholds"].get(g), after["thresholds"].get(g)
        if b and a and b["sha256"] != a["sha256"]:
            notes.append(f"Warning: the {g} thresholds changed ({b['sha256']} to {a['sha256']}).")

    gate_rows = []
    for g in sorted(set(before["verdicts"]) | set(after["verdicts"])):
        b = before["verdicts"].get(g, {}).get("verdict", "absent")
        a = after["verdicts"].get(g, {}).get("verdict", "absent")
        mark = ""
        if b == "pass" and a != "pass":
            mark = "regression"
            regressions.append(f"gate {g}: {b} to {a}")
        elif a == "pass" and b != "pass":
            mark = "improved"
        gate_rows.append((g, b, a, mark))

    check_rows = []
    for g in sorted(set(before["verdicts"]) | set(after["verdicts"])):
        br = before["verdicts"].get(g, {}).get("results", {})
        ar = after["verdicts"].get(g, {}).get("results", {})
        for scope in sorted(set(br) | set(ar)):
            for metric in sorted(set(br.get(scope, {})) | set(ar.get(scope, {}))):
                b = br.get(scope, {}).get(metric, "absent")
                a = ar.get(scope, {}).get(metric, "absent")
                if a == b:
                    continue
                worse = (
                    _RANK.get(a, 0) < _RANK.get(b, 0)
                    or (a == "absent" and b != "absent")
                    or a == "not_applicable"  # a check that stopped being checked
                )
                if worse:
                    regressions.append(f"{g} {scope}.{metric}: {b} to {a}")
                check_rows.append(
                    (g, f"{scope}.{metric}", b, a, "regression" if worse else "changed")
                )

    scope_sections: list[str] = []
    for scope in sorted(set(before["scopes"]) | set(after["scopes"])):
        bs, as_ = before["scopes"].get(scope), after["scopes"].get(scope)
        if bs is None or as_ is None:
            which = "after" if as_ is None else "before"
            if as_ is None:
                regressions.append(f"{scope}: not reported after")
            scope_sections += ["", f"### {scope}", "", f"Not reported {which}."]
            if as_ is None:
                scope_sections[-1] += " regression"
            continue
        bm, am = bs["metrics"], as_["metrics"]
        if bm != SUPPRESSED and am == SUPPRESSED:
            regressions.append(f"{scope}: values suppressed after")
            scope_sections += ["", f"### {scope}", "", "Values suppressed after. regression"]
            continue
        if bm == SUPPRESSED:
            scope_sections += [
                "",
                f"### {scope}",
                "",
                "Values suppressed before (fewer than 5 documents); compare the verdicts.",
            ]
            continue
        rows = []
        for metric in sorted(set(bm) | set(am)):
            b, a = bm.get(metric, "absent"), am.get(metric, "absent")
            bn, an = _num(b), _num(a)
            change = ""
            mark = ""
            if bn is not None and an is not None:
                diff = an - bn
                change = f"{diff:+f}" if diff else "0"
                direction = ALL_METRICS.get(metric, NEUTRAL)
                if diff and direction != NEUTRAL:
                    worse = diff < 0 if direction == HIGHER else diff > 0
                    mark = "regression" if worse else "improved"
                elif diff:
                    mark = "changed"
            elif bn is not None and an is None:
                # measured before; not measured, gone or suppressed after: a person checks
                mark = "regression (suppressed after)" if a == SUPPRESSED else "regression"
            elif bn is None and an is not None:
                mark = "newly measured"
            if mark.startswith("regression"):
                regressions.append(f"{scope}.{metric}: {_show(b)} to {_show(a)}")
            rows.append((metric, _show(b), _show(a), change, mark))
        scope_sections += ["", f"### {scope}", ""]
        scope_sections += _table(("Metric", "Before", "After", "Change", "Note"), rows)

    bname = f"{before['extractor']['name']} ({', '.join(f'{k} {v}' for k, v in before['model_versions'].items()) or 'none'})"
    aname = f"{after['extractor']['name']} ({', '.join(f'{k} {v}' for k, v in after['model_versions'].items()) or 'none'})"
    lines = [
        "# Before and after",
        "",
        f"Before: {bname}, test set {before['testset']['sha256']}, split {before['split']['name']}.",
        f"After: {aname}, test set {after['testset']['sha256']}, split {after['split']['name']}.",
    ]
    for s in (before, after):
        if s["split"]["label"]:
            lines.append(f"**{s['split']['label']}** ({'before' if s is before else 'after'}).")
    lines += [""] + notes + ([""] if notes else [])
    lines.append(f"Regressions: {len(regressions)}")
    lines += [f"- {r}" for r in regressions]
    lines += ["", "## Gates", ""]
    lines += _table(("Gate", "Before", "After", "Note"), gate_rows)
    if check_rows:
        lines += ["", "## Checks that changed", ""]
        lines += _table(("Gate", "Check", "Before", "After", "Note"), check_rows)
    lines += ["", "## Metrics"]
    lines += scope_sections
    return "\n".join(lines) + "\n"


__all__ = [
    "ANSWER_GATE",
    "ANSWER_METRICS",
    "DEV_LABEL",
    "DOCUMENT_GATES",
    "GATES",
    "MIN_CELL",
    "PRODUCTION_GATES",
    "REPORT_SCHEMA",
    "SUITE_GATES",
    "SILENT_ERROR_DEFINITION",
    "SUPPRESSED",
    "THRESHOLDS_DIR",
    "THRESHOLDS_SCHEMA",
    "Rule",
    "Thresholds",
    "build_report",
    "cell",
    "check_gates",
    "check_split",
    "compare",
    "decimal",
    "dumps",
    "evaluate_rules",
    "is_stub",
    "is_token",
    "load_thresholds",
    "packaged_thresholds",
    "parse_report",
    "read_regular",
    "read_report",
    "render_markdown",
    "stopped_report",
    "testset_meta",
]
