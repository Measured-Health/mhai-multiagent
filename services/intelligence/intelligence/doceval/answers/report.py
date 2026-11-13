"""The answer gate's report (``mhai.evaluation.answer_report/1``): ``evaluation.json`` and
``evaluation.md`` of a ``cca`` promotion.

Aggregates only, never an item, question, id or answer::

    {"comparison": null | {"<label>": {<metric>: "<decimal>" | null}},
     "counts": {"escalated", "forbidden_hits", "generation_failures", "items", "must_escalate"},
     "engine": {"name", "version"}, "gguf_sha256": "<64 hex>",
     "metrics": {<metric>: "<decimal>" | null}, "schema": ..., "sign_off": "pilot, unsigned",
     "split": {"label", "name"}, "testset": {"item_schema", "items", "must_escalate", "name",
     "sha256", "version"}, "thresholds": {"answer": {"file", "production", "sha256"}},
     "verdicts": {"answer": {"failed", "results", "verdict"}}}

The verdict is :func:`intelligence.doceval.report.evaluate_rules` over the ``overall``
scope of ``thresholds/answer.json``: every rule must pass, a metric with nothing to measure
fails, and fewer items than ``min_documents`` fails (``overall.scope_documents``).
``gguf_sha256`` names the file that was scored: the run's GGUF, the one that is served.
``comparison`` is informational only (the cloud route on the same set, plan Task 7): it
never changes a verdict. ``sign_off`` says the set and the pass mark have no clinical
sign-off yet (README, "Pilot mode"). :func:`parse_report` reads one back strictly: exact
keys, every string to its pattern, and the failed list agreeing with the results.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from typing import Any

from intelligence.doceval import report
from intelligence.doceval.answers import schema, score
from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused

REPORT_SCHEMA = "mhai.evaluation.answer_report/1"
GATE = report.ANSWER_GATE
METRICS: tuple[str, ...] = tuple(report.ANSWER_METRICS)
COUNTS = ("escalated", "forbidden_hits", "generation_failures", "items", "must_escalate")
MAX_COMPARISONS = 4
MAX_REPORT_BYTES = 256 * 1024

_TOP = frozenset(
    {
        "comparison",
        "counts",
        "engine",
        "gguf_sha256",
        "metrics",
        "schema",
        "sign_off",
        "split",
        "testset",
        "thresholds",
        "verdicts",
    }
)
_SHA = re.compile(r"[0-9a-f]{64}", re.ASCII)
_DECIMAL = re.compile(r"(0|1)\.[0-9]{6}", re.ASCII)
_LABEL = re.compile(r"[a-z][a-z0-9-]{0,31}", re.ASCII)
_VERDICTS = ("pass", "fail", "not_measured", "not_applicable")


def _bad() -> Refused:
    return Refused("evaluation_failed", detail_code="report")


def _rates(m: score.SuiteMetrics) -> dict[str, str | None]:
    return {k: report.decimal(v) for k, v in m.rates().items()}


def build_report(
    m: score.SuiteMetrics,
    thresholds: report.Thresholds,
    *,
    testset: Mapping[str, Any],
    split: str,
    engine: str,
    engine_version: str,
    gguf_sha256: str,
    comparison: Mapping[str, score.SuiteMetrics] | None = None,
) -> dict[str, Any]:
    """The report of one scored set against the answer gate's ``thresholds``."""
    if not isinstance(thresholds, report.Thresholds) or thresholds.gate != GATE:
        raise Refused("policy_invalid")
    verdict = report.evaluate_rules(thresholds, {"overall": m.rates()}, {"overall": m.items})
    rep = {
        "schema": REPORT_SCHEMA,
        "split": report.check_split(split),
        "testset": {
            "name": testset["name"],
            "version": testset["version"],
            "sha256": testset["sha256"],
            "items": testset["items"],
            "must_escalate": testset["must_escalate"],
            "item_schema": testset["item_schema"],
        },
        "engine": {"name": engine, "version": engine_version},
        "gguf_sha256": gguf_sha256,
        "metrics": _rates(m),
        "counts": {
            "escalated": m.escalated,
            "forbidden_hits": m.forbidden_hits,
            "generation_failures": m.generation_failures,
            "items": m.items,
            "must_escalate": m.must_escalate,
        },
        "verdicts": {GATE: verdict},
        "thresholds": {
            GATE: {
                "file": thresholds.file,
                "sha256": thresholds.sha256,
                "production": thresholds.production,
            }
        },
        "comparison": (
            None
            if comparison is None
            else {label: _rates(cm) for label, cm in sorted(comparison.items())}
        ),
        "sign_off": schema.SIGN_OFF,
    }
    parse_report(report.dumps(rep), any_thresholds_file=True)  # what we write reads back
    return rep


def _req(ok: bool) -> None:
    if not ok:
        raise _bad()


def _exact(d: Any, keys: tuple[str, ...] | frozenset[str]) -> bool:
    return isinstance(d, dict) and set(d) == set(keys)


def _count(x: Any) -> bool:
    return isinstance(x, int) and not isinstance(x, bool) and 0 <= x <= 1_000_000


def _rate_block(d: Any) -> None:
    _req(_exact(d, METRICS))
    for v in d.values():
        _req(v is None or (isinstance(v, str) and _DECIMAL.fullmatch(v) is not None))
        _req(v is None or v <= "1.000000")


def parse_report(raw: bytes, *, any_thresholds_file: bool = False) -> dict[str, Any]:
    """An answer report read back strictly (see the module docstring)."""
    failed = False
    try:
        obj = canonical.loads_strict(raw, MAX_REPORT_BYTES)
        failed = canonical.dumps(obj) != raw
    except Refused:
        failed = True
    if failed:
        raise _bad()
    _req(_exact(obj, _TOP) and obj["schema"] == REPORT_SCHEMA)
    split = obj["split"]
    _req(_exact(split, ("name", "label")) and split["name"] in ("dev", "test"))
    _req(split["label"] == (report.DEV_LABEL if split["name"] == "dev" else None))
    ts = obj["testset"]
    _req(_exact(ts, ("name", "version", "sha256", "items", "must_escalate", "item_schema")))
    _req(ts["name"] == schema.SET_NAME and ts["item_schema"] == schema.ITEM_SCHEMA)
    _req(report.is_token(ts["version"]) and isinstance(ts["sha256"], str))
    _req(_SHA.fullmatch(ts["sha256"]) is not None)
    _req(_count(ts["items"]) and _count(ts["must_escalate"]))
    engine = obj["engine"]
    _req(_exact(engine, ("name", "version")))
    _req(report.is_token(engine["name"]) and report.is_token(engine["version"]))
    _req(isinstance(obj["gguf_sha256"], str) and _SHA.fullmatch(obj["gguf_sha256"]) is not None)
    _rate_block(obj["metrics"])
    _req(_exact(obj["counts"], COUNTS) and all(_count(v) for v in obj["counts"].values()))
    comparison = obj["comparison"]
    if comparison is not None:
        _req(isinstance(comparison, dict) and 1 <= len(comparison) <= MAX_COMPARISONS)
        for label, block in comparison.items():
            _req(isinstance(label, str) and _LABEL.fullmatch(label) is not None)
            _rate_block(block)
    _req(obj["sign_off"] == schema.SIGN_OFF)
    th = obj["thresholds"]
    _req(_exact(th, (GATE,)) and _exact(th[GATE], ("file", "sha256", "production")))
    t = th[GATE]
    _req(
        t["file"] == f"thresholds/{GATE}.json"
        or (any_thresholds_file and report.is_token(t["file"]))
    )
    _req(isinstance(t["sha256"], str) and _SHA.fullmatch(t["sha256"]) is not None)
    _req(t["production"] is True)
    v = obj["verdicts"]
    _req(_exact(v, (GATE,)) and _exact(v[GATE], ("verdict", "results", "failed")))
    g = v[GATE]
    _req(g["verdict"] in ("pass", "fail") and _exact(g["results"], ("overall",)))
    out = g["results"]["overall"]
    allowed = set(METRICS) | {"scope_documents"}
    _req(isinstance(out, dict) and set(out) <= allowed and "scope_documents" in out)
    _req(all(x in _VERDICTS for x in out.values()))
    expected = sorted(f"overall.{k}" for k, x in out.items() if x in ("fail", "not_measured"))
    _req(g["failed"] == expected and (g["verdict"] == "fail") == bool(expected))
    checked: dict[str, Any] = obj
    return checked


def render_markdown(rep: Mapping[str, Any]) -> str:
    """``evaluation.md``: fixed text and the report's checked values only."""
    rep = parse_report(report.dumps(rep), any_thresholds_file=True)
    ts, g = rep["testset"], rep["verdicts"][GATE]
    lines = [
        "# Evaluation: clinical answer gate",
        "",
        f"- Split: `{rep['split']['name']}`"
        + (" (development split, not a promotion result)" if rep["split"]["name"] == "dev" else ""),
        f"- Test set: `{ts['name']}@{ts['version']}`, SHA-256 `{ts['sha256']}`",
        f"- Items: {ts['items']}; must escalate: {ts['must_escalate']}",
        f"- Scored file: the GGUF, SHA-256 `{rep['gguf_sha256']}`",
        f"- Engine: `{rep['engine']['name']}` `{rep['engine']['version']}`",
        f"- Sign-off: {rep['sign_off']} (the question set and pass mark have no clinical"
        " sign-off yet)",
        "",
        f"## Gate `{GATE}`: `{g['verdict']}`",
        "",
        "| Metric | Value | Verdict |",
        "|---|---|---|",
    ]
    for metric in METRICS:
        value = rep["metrics"][metric]
        lines.append(
            f"| `{metric}` | {value if value is not None else 'not_measured'} "
            f"| `{g['results']['overall'].get(metric, 'recorded')}` |"
        )
    lines.append(
        f"| `scope_documents` | {ts['items']} | `{g['results']['overall']['scope_documents']}` |"
    )
    lines += ["", "## Counts", ""]
    lines += [f"- `{k}`: {rep['counts'][k]}" for k in COUNTS]
    if rep["comparison"]:
        lines += ["", "## Comparison (informational, never a gate)", ""]
        lines += ["| Route | " + " | ".join(f"`{m}`" for m in METRICS) + " |"]
        lines += ["|---|" + "---|" * len(METRICS)]
        for label, block in rep["comparison"].items():
            cells = " | ".join(
                block[m] if block[m] is not None else "not_measured" for m in METRICS
            )
            lines.append(f"| `{label}` | {cells} |")
    return "\n".join(lines) + "\n"


__all__ = ["GATE", "METRICS", "REPORT_SCHEMA", "build_report", "parse_report", "render_markdown"]


if __name__ == "__main__":  # pragma: no cover - the promote workflow's job summary
    import sys
    from pathlib import Path

    sys.stdout.write(render_markdown(parse_report(Path(sys.argv[1]).read_bytes())))
