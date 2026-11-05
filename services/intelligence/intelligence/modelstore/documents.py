"""The documents a promoted version carries: ``model_card.md`` and ``data_statement.md``.

Both are generated, never written by hand, and only from ids, enums and numbers: the
parsed training manifest (:class:`~intelligence.modelstore.training.TrainingManifest`,
which cannot hold free text), the dataset registry, and an evaluation report that is read
back through :func:`intelligence.doceval.report.parse_report` (every string in it held to
its pattern). The prose is fixed text from this module. Every id is shown in a code span,
and no id can contain a backtick, so nothing from the inputs is read as Markdown.

Nothing here is free text from the notebook: a manifest with a string field that is not
an id or an enum is refused when it is parsed (``manifest_invalid``), before any document
is made.
"""

import re
from collections.abc import Mapping
from typing import Any

from intelligence.doceval import report
from intelligence.modelstore import training
from intelligence.modelstore.reasons import Refused

INTENDED_USE = (
    "Structured extraction from clinical document pages into the "
    "`mhai.extraction.clinical_document` schema, inside the platform's document pipeline. "
    "Every extraction carries per-field confidence and is reviewed under the pipeline's "
    "always-review rules before it reaches a patient record."
)
OUT_OF_SCOPE = (
    "Diagnosis, triage or any clinical decision without a clinician's review.",
    "Documents, languages or schema versions outside the evaluation set below.",
    "Use outside the platform's model host, or with weights other than this version's "
    "verified files.",
    "Any use on a failure slice listed below without the review that slice requires.",
)

_FAILED = re.compile(
    r"(run|overall|slices\.[a-z][a-z0-9_]{0,63})\.([a-z][a-z0-9_.]{0,63})", re.ASCII
)


def _code(value: str) -> str:
    """A code span; every value reaching here matched an id pattern. Refuses anything that
    could end the span, split a table cell or open markup."""
    if not isinstance(value, str) or any(c in value for c in "`|<>\n\r"):
        raise Refused("manifest_invalid", detail_code="document_value")
    return f"`{value}`"


def _metric(v: training.Metric) -> str:
    return repr(v) if isinstance(v, float) else str(v)


def _manifest(tm: object) -> training.TrainingManifest:
    if not isinstance(tm, training.TrainingManifest):
        raise Refused("manifest_invalid", detail_code="document_manifest")
    return tm


def _evaluation(evaluation: object) -> dict[str, Any]:
    """The report, read back as Task 19 reads ``evaluation.json`` (strict, no markup)."""
    if not isinstance(evaluation, Mapping):
        raise Refused("manifest_invalid", detail_code="document_evaluation")
    return report.parse_report(report.dumps(evaluation))


def _failures(failed: list[str]) -> dict[str, list[str]]:
    by_scope: dict[str, list[str]] = {}
    for entry in failed:
        m = _FAILED.fullmatch(entry)
        if m is None:
            raise Refused("manifest_invalid", detail_code="document_failed")
        by_scope.setdefault(m.group(1), []).append(m.group(2))
    return by_scope


def _base_lines(tm: training.TrainingManifest) -> list[str]:
    base = tm.base
    if isinstance(base, training.PromotedBase):
        return [f"- Promoted base model: {_code(base.pin)}"]
    return [
        f"- Upstream base model: {_code(base.upstream)}",
        f"- Revision: {_code(base.revision)}",
        f"- Upstream licence: {_code(base.licence)}",
    ]


def model_card(tm: training.TrainingManifest, evaluation: Mapping[str, Any], licence: str) -> str:
    """``model_card.md``: intended use, out-of-scope uses, failing slices from the
    evaluation, licence and base model. ``licence`` is the version's licence (the
    upstream base's, or the promoted base's from its verified manifest)."""
    tm = _manifest(tm)
    rep = _evaluation(evaluation)
    if not training.is_licence_id(licence):
        raise Refused("manifest_invalid", detail_code="document_licence")
    ts = rep["testset"]
    split = rep["split"]

    out = [
        f"# Model card: {_code(tm.name)}",
        "",
        f"- Role: {_code(tm.role)}",
        f"- Trained by: {_code(tm.trained_by)}",
        f"- Created: {_code(tm.created_at)}",
        "",
        "## Intended use",
        "",
        INTENDED_USE,
        "",
        "## Out-of-scope uses",
        "",
        *(f"- {line}" for line in OUT_OF_SCOPE),
        "",
        "## Evaluation",
        "",
        f"- Test set: {_code(ts['name'] + '@' + ts['version'])}, SHA-256 {_code(ts['sha256'])}",
        f"- Extraction schema: {_code(ts['extraction_schema_version'])}",
        f"- Documents: {ts['documents']}; pages: {ts['pages']}",
        f"- Split: {_code(split['name'])}"
        + (" (development split, not a promotion result)" if split["name"] == "dev" else ""),
        "",
        "| Gate | Verdict |",
        "|---|---|",
        *(
            f"| {_code(gate)} | {_code(v['verdict'])} |"
            for gate, v in sorted(rep["verdicts"].items())
        ),
        "",
        "## Known failure slices",
        "",
        "Checks that failed or were not measured, per gate, from the evaluation report.",
        "",
    ]
    any_failure = False
    for gate, v in sorted(rep["verdicts"].items()):
        by_scope = _failures(v["failed"])
        if not by_scope:
            continue
        any_failure = True
        out.append(f"### Gate {_code(gate)}")
        out.append("")
        for scope in sorted(by_scope, key=lambda s: (not s.startswith("slices."), s)):
            metrics = ", ".join(_code(m) for m in sorted(by_scope[scope]))
            out.append(f"- {_code(scope)}: {metrics}")
        out.append("")
    if not any_failure:
        out += ["None: every gated check passed.", ""]
    out += [
        "## Licence",
        "",
        f"{_code(licence)}",
        "",
        "## Base model",
        "",
        *_base_lines(tm),
        "",
        "## Training data",
        "",
        *(f"- {_code(d)}" for d in tm.datasets),
        "",
        "See `data_statement.md` for each dataset's kind and approval.",
        "",
        "## Framework",
        "",
        *(f"- {_code(p)}: {_code(v)}" for p, v in tm.framework),
        "",
        "## Notebook metrics",
        "",
        "Informational only, as reported by the training notebook; never compared with a "
        "threshold.",
        "",
    ]
    if tm.notebook_metrics:
        out += [f"- {_code(k)}: {_metric(v)}" for k, v in tm.notebook_metrics]
    else:
        out.append("None reported.")
    return "\n".join(out) + "\n"


def data_statement(tm: training.TrainingManifest, registry: training.Registry) -> str:
    """``data_statement.md``: every dataset id from the training manifest with its kind,
    generator and approval record. An id not in the registry is ``dataset_unknown``."""
    tm = _manifest(tm)
    if not isinstance(registry, training.Registry):
        raise Refused("policy_invalid", detail_code="document_registry")
    out = [
        f"# Data statement: {_code(tm.name)}",
        "",
        f"- Role: {_code(tm.role)}",
        f"- Trained by: {_code(tm.trained_by)}",
        "",
        "Every dataset the training manifest names, as the approved dataset registry "
        "(`models/approved-datasets.json`) records it. Each is disjoint from the locked "
        "test split.",
        "",
        "| Dataset | Kind | Generator | Version | Approved by PR |",
        "|---|---|---|---|---|",
    ]
    for ident in tm.datasets:
        d = registry.datasets.get(ident)
        if d is None:
            raise Refused("dataset_unknown")
        generator = _code(d.generator) if d.generator is not None else "none"
        out.append(
            f"| {_code(d.id)} | {_code(d.kind)} | {generator} | {_code(d.version)} "
            f"| #{d.approved_by_pr} |"
        )
    return "\n".join(out) + "\n"


__all__ = ["INTENDED_USE", "OUT_OF_SCOPE", "data_statement", "model_card"]
