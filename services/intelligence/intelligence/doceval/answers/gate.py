"""The ``clinical_answer`` evaluation of a ``cca`` run, as promotion runs it (own answer model
plan, Task 6).

The scored artifact is the run's GGUF, the file the sidecar will serve, never its
safetensors (security review of #143). :func:`evaluate_gguf_run`:

1. finds the run's one GGUF in the files ``files.check_run`` hashed (none is
   ``no_runtime``/``no_gguf``);
2. checks the engine is llama.cpp at the pinned commit (:func:`llama_runtime.check_engine`);
3. starts ``llama-server`` on that GGUF in the job's scratch, on loopback, offline, with
   ``--alias <its SHA-256>``, and requires that alias before any question;
4. scores the set through the worker's own client, ``OrchestratorModelClient`` over
   ``local_parts``' builder with the server as its generator (retrieval over the item's
   chunks, the CCA prompt, ``_verify_with``): what a hospital would get;
5. asks the warm-up question (liveness only, plan Decision 7) and records
   ``warmup.json`` ``{answered, engine, engine_version, question_sha256}``;
6. stops the server and checks the GGUF is unchanged.

The default builder loads the retriever and NLI models from folders verified, before
anything starts, against the exact revisions and file hashes in
``models/answer-gate-models.lock`` (:mod:`gate_models`; a mismatch refuses the gate), with
the Hugging Face hub offline; a test passes its own builder.
"""

from __future__ import annotations

import functools
import hashlib
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval import report
from intelligence.doceval.answers import gate_models, llama_runtime, schema, score
from intelligence.doceval.answers import report as areport
from intelligence.modelstore import canonical, files
from intelligence.modelstore.reasons import Refused

WARMUP_QUESTION = (
    "Synthetic warm-up question: what is the first-line medicine for type 2 diabetes in the "
    "shared guideline?"
)
WARMUP_SYSTEM = "Answer briefly from the shared clinical guidelines."

BuilderFor = Callable[[Any], Callable[[Sequence[Any]], Any]]


def default_builder(generator: Any, models: Mapping[str, Path]) -> Callable[[Sequence[Any]], Any]:
    """The worker's builder (``local_parts``) with ``generator`` as the CCA's generator and
    its retriever and NLI models loaded, offline, from ``models``: the folders
    :func:`gate_models.paths_from_env` verified against ``models/answer-gate-models.lock``."""
    from intelligence.boundary.models import local_parts

    build, _ = local_parts(generator=generator, local_models=models)
    return build


@dataclass(frozen=True)
class AnswerRun:
    report: dict[str, Any]
    warmup: bytes
    engine: str
    engine_version: str


class _NoMasker:
    """The gate never masks: its questions are synthetic and never leave the job."""

    def mask(self, text: str) -> tuple[str, list[Any]]:
        return text, []


def find_gguf(entries: Sequence[files.FileEntry]) -> files.FileEntry:
    found = [e for e in entries if e.path.endswith(files.GGUF_SUFFIX)]
    if len(found) != 1:
        raise Refused("no_runtime", detail_code="no_gguf")
    return found[0]


def warmup_json(generator: Any, engine_version: str) -> bytes:
    failed = False
    answer = ""
    try:
        answer = generator.generate(WARMUP_SYSTEM, WARMUP_QUESTION)
    except Exception:  # noqa: BLE001 - a fixed code only
        failed = True
    if failed or not isinstance(answer, str) or not answer.strip():
        raise Refused("warmup_mismatch", detail_code="no_answer")
    return canonical.dumps(
        {
            "question_sha256": hashlib.sha256(WARMUP_QUESTION.encode("utf-8")).hexdigest(),
            "engine": llama_runtime.ENGINE,
            "engine_version": engine_version,
            "answered": True,
        }
    )


def evaluate_gguf_run(
    run_dir: Path,
    entries: Sequence[files.FileEntry],
    set_dir: Path,
    thresholds: report.Thresholds,
    *,
    settings: llama_runtime.Settings,
    home: Path,
    split: str,
    builder_for: BuilderFor | None = None,
    comparison: Mapping[str, score.SuiteMetrics] | None = None,
) -> AnswerRun:
    """Score the run's GGUF on the set at ``set_dir`` (see the module docstring)."""
    from intelligence.boundary.models import OrchestratorModelClient

    gguf = find_gguf(entries)
    make: BuilderFor
    if builder_for is None:
        # verified before the server or any model starts
        make = functools.partial(default_builder, models=gate_models.paths_from_env())
    else:
        make = builder_for
    loaded = schema.load_set(set_dir)
    if loaded.meta["purpose"] != split:
        raise Refused("testset_mismatch", detail_code="split")
    engine_version = llama_runtime.check_engine(settings)
    with llama_runtime.LlamaServer(settings, run_dir / gguf.path, gguf.sha256, home) as server:
        client = OrchestratorModelClient(make(server.generator), masker=_NoMasker())
        results = score.score(client, loaded.items)
        warm = warmup_json(server.generator, engine_version)
    rep = areport.build_report(
        score.metrics(results),
        thresholds,
        testset=loaded.meta,
        split=split,
        engine=llama_runtime.ENGINE,
        engine_version=engine_version,
        gguf_sha256=gguf.sha256,
        comparison=comparison,
    )
    return AnswerRun(rep, warm, llama_runtime.ENGINE, engine_version)


__all__ = ["WARMUP_QUESTION", "AnswerRun", "default_builder", "evaluate_gguf_run", "find_gguf"]
