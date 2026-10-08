# Plan 1: Inference boundary and pipeline changes

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the clinical pipeline model-free behind inference
interfaces, add the `inference` service, and make the pipeline changes the
API gateway needs:

- per-citation results;
- the stage hook;
- diagnosis extraction;
- the RBAC split;
- OCR-only ingest;
- the triage gate.

**Architecture:** New `mhai/inference/` defines the `Embedder`,
`NLIClassifier`, `Generator` and `OCR` protocols, with HTTP clients and a
shared error classifier. A separate `mhai/inference_server/` FastAPI app
serves embeddings (OpenAI-compatible) and NLI (named labels). Existing
pipeline classes gain optional client parameters, so the old in-process
paths keep working for research scripts and tests. Nothing in this plan
needs Postgres or the API; that's Plan 2.

**Tech stack:** Python 3.12, uv, httpx, FastAPI/uvicorn (inference
server), sentence-transformers/transformers (inference image only),
pytesseract and pdf2image (poppler), pytest.

**Spec:** `docs/specs/2026-10-08-api-gateway-phase1-design.md`, sections
3, 8, 8a, 9 (OCR path), 10 and 14 (boundary and contract tests). ADR 0008.

**Plans 2 and 3** (API core; documents, admin, MFA, OIDC, retention) are
written after this plan merges, against the code as it then is.

**Deferred from the spec sections above:**
- §8 item 5, the Postgres-backed audit logger → Plan 2 (needs the database).
- `/readyz` and `/agents/status` inference checks → Plan 2.
- Citable ids for document chunks, and wiring OCR ingest to storage and
  jobs → Plan 3.

## Global constraints

- Python `>=3.12,<3.13`; dependencies via `uv add` / `uv lock`; commit
  `uv.lock`.
- Every rule in `SKILLS.md` applies, in particular:
  - patient scoping via `chunk_visible_to()`;
  - fail closed;
  - NLI labels are named, never indexes;
  - no PHI in logs.
- `mhai.api` (Plan 2) and `mhai.worker` (Plan 2) must not import `torch`,
  `transformers` or `sentence_transformers`. After this plan, importing
  `mhai.agents.orchestrator`, `mhai.agents.hga_nli`,
  `mhai.knowledge_base.kb_builder`, `mhai.security.hipaa`, `mhai.inference`
  or `mhai.ingest` must not load them either.
- Wire protocol:
  - `POST /v1/embeddings` (OpenAI-compatible);
  - `POST /v1/classify` (labels: `entailment`, `neutral`, `contradiction`);
  - `GET /health`.
  Every response carries `model` and `model_version`.
- Stage names and order: `pii_masking`, `retrieval`, `generation`,
  `nli_verification`, `hga_verification`, `decision`.
- Error classes: `transient` (retry), `permanent` (no retry), `unknown`
  (retry once).
- Inference server default port `8080`; env `INFERENCE_URL` (default
  `http://inference:8080`).
- Commits: imperative subject ≤72 characters, a body saying why, **no AI
  attribution**. Branch: `feat/8-api-gateway` (already exists). Open the PR
  when the plan is done; a human merges.
- New code must pass `ruff check .`, `ruff format --check .` and `mypy`
  without adding to the baselines in `pyproject.toml`.

## Review focus

The five inputs most likely to bite users that the spec implies but no
task would otherwise test. Each has a test in the task named.

1. **The inference service returns an unknown or differently-cased NLI
   label** (e.g. `"ENTAILMENT"`, `"LABEL_1"`). Expected: the client
   lower-cases known labels and rejects unknown ones with a permanent
   error; HGA then fails, and the result is escalated, never guessed.
   (Task 3)
2. **Embeddings come back out of order or with the wrong count.**
   Expected: the client orders by `index` and raises on a count mismatch,
   so vectors are never silently misaligned with chunks. (Task 3)
3. **The stage callback raises** (e.g. the progress write fails).
   Expected: the clinical pipeline still completes and returns its
   result; the failure is only logged. (Task 7)
4. **A response cites a real chunk of the same patient that wasn't
   retrieved for this query.** Expected: REDACT with reason
   `not_retrieved`, before any NLI call. (Task 6)
5. **A PDF that is corrupt, encrypted or over the page limit.** Expected:
   a permanent error and **no** partial chunks. (Task 10)

---

## File structure

| Path | Responsibility |
|---|---|
| `mhai/agents/__init__.py` (modify) | Lazy exports so importing one agent doesn't load torch |
| `mhai/platform/__init__.py`, `mhai/platform/errors.py` (create) | Shared error classes, classifier, retry and backoff policy |
| `mhai/platform/stages.py` (create) | Stage names, progress values, `emit_stage()` that never raises |
| `mhai/inference/__init__.py`, `types.py`, `http.py`, `ocr.py`, `factory.py` (create) | Inference protocols, HTTP clients with circuit breaker, Tesseract OCR, construction from env |
| `mhai/inference_server/__init__.py`, `backends.py`, `app.py`, `main.py` (create) | The inference service: model backends, FastAPI app, env factory |
| `mhai/knowledge_base/kb_builder.py` (modify) | `DenseRetriever`/`HybridRetriever` accept an `Embedder`; `generate_with_rag` emits stages |
| `mhai/agents/hga_nli.py` (modify) | `NLIEntailmentVerifier` accepts an `NLIClassifier`; per-citation results; `allowed_chunk_ids`; stages |
| `mhai/agents/orchestrator.py` (modify) | Passes retrieved ids, stages; adds citations, diagnosis, timings and HGA details to the result |
| `mhai/agents/diagnosis.py` (create) | `extract_diagnosis()` |
| `mhai/agents/triage_gate.py` (create) | `TriageGate` |
| `mhai/security/hipaa.py` (modify) | RBAC: admin loses clinical permissions; `submit_query` emits `pii_masking` and passes `on_stage` |
| `mhai/ingest/__init__.py`, `mhai/ingest/ocr_pages.py` (create) | OCR-only PDF ingest to patient-scoped chunks |
| `pyproject.toml`, `uv.lock` (modify) | `ml` dependency group; `pdf2image`, `pytesseract` |
| `Dockerfile` (modify), `Dockerfile.inference` (create), `docker-compose.yml`, `docker-compose.gpu.yml`, `.env.example`, `.github/workflows/ci.yml` (modify) | Images, the `inference` service, CI |
| `tests/unit/test_boundaries.py`, `tests/unit/test_platform_errors.py`, `tests/unit/test_stages.py`, `tests/unit/test_inference_clients.py`, `tests/unit/test_inference_server.py`, `tests/unit/test_diagnosis.py`, `tests/unit/test_triage_gate.py`, `tests/unit/test_ocr_pages.py`, `tests/security/test_citations_retrieved.py`, `tests/security/test_rbac_admin_split.py`, `tests/unit/test_pipeline_clients.py`, `tests/unit/test_stage_hook.py` (create) | Tests per task |

---

### Task 1: Keep torch out of the import path

**Files:**
- Modify: `mhai/agents/__init__.py`
- Modify: `pyproject.toml` (dependency groups), `uv.lock`
- Test: `tests/unit/test_boundaries.py`

**Interfaces:**
- Produces: `mhai.agents.DIAAgent`, `QDAAgent`, `CCAAgent`, `HGAAgent`,
  `OrchestratorAgent` and the long names still resolve, but lazily.
- Produces: dependency group `ml` (torch-free base install).

- [ ] **Step 1: Write the failing test**

```python
# tests/unit/test_boundaries.py
"""Application modules must not load ML frameworks (ADR 0008)."""

import subprocess
import sys

import pytest

ML = ("torch", "transformers", "sentence_transformers")

MODEL_FREE = [
    "mhai.agents.orchestrator",
    "mhai.agents.hga_nli",
    "mhai.knowledge_base.kb_builder",
    "mhai.security.hipaa",
]


def _loaded_ml(module: str) -> list[str]:
    code = (
        f"import importlib, sys; importlib.import_module({module!r}); "
        f"print(','.join(m for m in {ML!r} if m in sys.modules))"
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    return [m for m in out.stdout.strip().split(",") if m]


@pytest.mark.parametrize("module", MODEL_FREE)
def test_module_does_not_load_ml_frameworks(module):
    assert _loaded_ml(module) == []


def test_agent_aliases_still_resolve():
    import mhai.agents as agents

    assert agents.OrchestratorAgent is agents.DynamicOrchestratorAgent
    assert agents.HGAAgent.__name__ == "NLIBackedHGA"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run pytest tests/unit/test_boundaries.py -v`
Expected: FAIL for `mhai.agents.orchestrator` and `mhai.agents.hga_nli`
(`['torch']`).

- [ ] **Step 3: Make `mhai.agents` lazy**

```python
# mhai/agents/__init__.py
"""MHAI Agent modules: one per clinical AI agent.

Exports resolve lazily so importing one agent (e.g. the orchestrator)
doesn't import the torch-based DIA and QDA modules (ADR 0008).
"""

from importlib import import_module

_EXPORTS = {
    "ClinicalContextAgent": "mhai.agents.cca",
    "DocumentIngestionAgent": "mhai.agents.dia",
    "NLIBackedHGA": "mhai.agents.hga_nli",
    "DynamicOrchestratorAgent": "mhai.agents.orchestrator",
    "QuickDetectionAgent": "mhai.agents.qda",
}

# Short aliases used in the README and integration guide.
_ALIASES = {
    "DIAAgent": "DocumentIngestionAgent",
    "QDAAgent": "QuickDetectionAgent",
    "CCAAgent": "ClinicalContextAgent",
    "HGAAgent": "NLIBackedHGA",
    "OrchestratorAgent": "DynamicOrchestratorAgent",
}

__all__ = sorted([*_EXPORTS, *_ALIASES])


def __getattr__(name: str):
    target = _ALIASES.get(name, name)
    if target in _EXPORTS:
        return getattr(import_module(_EXPORTS[target]), target)
    raise AttributeError(f"module 'mhai.agents' has no attribute {name!r}")
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run pytest tests/unit/test_boundaries.py tests/unit/test_imports.py -v`
Expected: PASS. If `mhai.agents.hga_nli` still loads torch, find the import
with `uv run python -X importtime -c "import mhai.agents.hga_nli" 2>&1 | grep -i torch`
and make that import lazy the same way.

- [ ] **Step 5: Move ML libraries to an `ml` group**

In `pyproject.toml`:

- Remove `"torch>=2.0"`, `"transformers>=4.30"`, `"accelerate>=0.20"`,
  `"peft>=0.5"`, `"bitsandbytes>=0.41"` and `"sentence-transformers>=2.2"`
  from `[project].dependencies`.
- Add `"pdf2image>=1.17"` and `"pytesseract>=0.3.13"` to
  `[project].dependencies` (used by Task 10).
- Under `[dependency-groups]`, add:

```toml
# Model runtime: only the inference image and research scripts install it.
ml = [
    "transformers>=4.30",
    "sentence-transformers>=2.2",
    "accelerate>=0.20",
    "peft>=0.5",
    "bitsandbytes>=0.41",
]
```

- Change `[tool.uv] default-groups = ["dev", "cpu"]` to
  `default-groups = ["dev", "cpu", "ml"]`, so local development and CI keep
  the full stack.

Run: `uv lock && uv sync --locked`
Expected: lock resolves; `uv run python -c "import torch, transformers"` works.

Run: `uv sync --locked --no-default-groups && uv run --no-sync python -c "import mhai.agents.orchestrator, mhai.knowledge_base.kb_builder, mhai.security.hipaa"`
Expected: succeeds with no torch installed. Then restore with `uv sync --locked`.

- [ ] **Step 6: Run the full suite**

Run: `uv run pytest -q -m "not models"`
Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add mhai/agents/__init__.py pyproject.toml uv.lock tests/unit/test_boundaries.py
git commit -m "Keep ML frameworks out of the pipeline's import path

mhai.agents eagerly imported the torch-based DIA and QDA modules, so
importing the orchestrator or HGA loaded torch. Export lazily, move the
model runtime to an 'ml' dependency group (installed by default for
development; left out of the application image), and test that the
pipeline modules don't load torch, transformers or sentence-transformers."
```

---

### Task 2: Shared error classifier and stage helper

**Files:**
- Create: `mhai/platform/__init__.py` (empty docstring module),
  `mhai/platform/errors.py`, `mhai/platform/stages.py`
- Test: `tests/unit/test_platform_errors.py`, `tests/unit/test_stages.py`

**Interfaces:**
- Produces (`mhai.platform.errors`):
  - `class ErrorClass(str, Enum)` with `TRANSIENT`, `PERMANENT`, `UNKNOWN`
  - `class TransientError(Exception)`
  - `class PermanentError(Exception)`
  - `classify(exc: BaseException) -> ErrorClass`
  - `should_retry(error_class: ErrorClass, attempt: int, max_attempts: int = 3) -> bool`
    (`attempt` is 1-based, the attempt that just failed)
  - `backoff_seconds(attempt: int, base: float = 10.0, cap: float = 300.0) -> float`
- Produces (`mhai.platform.stages`):
  - `STAGES: tuple[str, ...]`
  - `STAGE_PROGRESS: dict[str, float]`
  - `StageCallback = Callable[[str, float], None]`
  - `emit_stage(on_stage: StageCallback | None, stage: str) -> None`

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_platform_errors.py
import httpx
import pytest

from mhai.platform.errors import (
    ErrorClass,
    PermanentError,
    TransientError,
    backoff_seconds,
    classify,
    should_retry,
)


def _status_error(code: int) -> httpx.HTTPStatusError:
    request = httpx.Request("POST", "http://x/v1/classify")
    return httpx.HTTPStatusError(
        "x", request=request, response=httpx.Response(code, request=request)
    )


@pytest.mark.parametrize(
    "exc, expected",
    [
        (TransientError("x"), ErrorClass.TRANSIENT),
        (PermanentError("x"), ErrorClass.PERMANENT),
        (httpx.ConnectError("x"), ErrorClass.TRANSIENT),
        (httpx.ReadTimeout("x"), ErrorClass.TRANSIENT),
        (_status_error(503), ErrorClass.TRANSIENT),
        (_status_error(429), ErrorClass.TRANSIENT),
        (_status_error(400), ErrorClass.PERMANENT),
        (_status_error(404), ErrorClass.PERMANENT),
        (TimeoutError(), ErrorClass.TRANSIENT),
        (ConnectionResetError(), ErrorClass.TRANSIENT),
        (ValueError("bad pdf"), ErrorClass.PERMANENT),
        (KeyError("x"), ErrorClass.PERMANENT),
        (RuntimeError("?"), ErrorClass.UNKNOWN),
    ],
)
def test_classify(exc, expected):
    assert classify(exc) is expected


def test_retry_policy():
    assert should_retry(ErrorClass.TRANSIENT, attempt=1)
    assert should_retry(ErrorClass.TRANSIENT, attempt=2)
    assert not should_retry(ErrorClass.TRANSIENT, attempt=3)
    assert should_retry(ErrorClass.UNKNOWN, attempt=1)
    assert not should_retry(ErrorClass.UNKNOWN, attempt=2)
    assert not should_retry(ErrorClass.PERMANENT, attempt=1)


def test_backoff_doubles_and_caps():
    assert [backoff_seconds(n) for n in (1, 2, 3)] == [10.0, 20.0, 40.0]
    assert backoff_seconds(10) == 300.0
```

```python
# tests/unit/test_stages.py
from mhai.platform.stages import STAGE_PROGRESS, STAGES, emit_stage


def test_stage_order_and_progress_increase():
    assert STAGES == (
        "pii_masking",
        "retrieval",
        "generation",
        "nli_verification",
        "hga_verification",
        "decision",
    )
    values = [STAGE_PROGRESS[s] for s in STAGES]
    assert values == sorted(values) and 0 < values[0] and values[-1] < 1


def test_emit_stage_calls_back_with_progress():
    seen = []
    emit_stage(lambda s, p: seen.append((s, p)), "retrieval")
    assert seen == [("retrieval", STAGE_PROGRESS["retrieval"])]


def test_emit_stage_is_a_no_op_without_callback():
    emit_stage(None, "retrieval")


def test_emit_stage_swallows_callback_errors(caplog):
    def broken(stage, progress):
        raise RuntimeError("db down")

    emit_stage(broken, "decision")  # must not raise
    assert "stage callback failed" in caplog.text
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_platform_errors.py tests/unit/test_stages.py -v`
Expected: FAIL, `ModuleNotFoundError: No module named 'mhai.platform'`.

- [ ] **Step 3: Implement**

```python
# mhai/platform/__init__.py
"""Shared building blocks used by the pipeline, API and worker."""
```

```python
# mhai/platform/errors.py
"""One error classifier for the whole system: retry transient, stop on permanent."""

from enum import Enum

import httpx


class ErrorClass(str, Enum):
    TRANSIENT = "transient"
    PERMANENT = "permanent"
    UNKNOWN = "unknown"


class TransientError(Exception):
    """Worth retrying: the same input may succeed later."""


class PermanentError(Exception):
    """Retrying won't help: bad input, missing subject, protocol violation."""


def classify(exc: BaseException) -> ErrorClass:
    if isinstance(exc, TransientError):
        return ErrorClass.TRANSIENT
    if isinstance(exc, PermanentError):
        return ErrorClass.PERMANENT
    if isinstance(exc, httpx.HTTPStatusError):
        code = exc.response.status_code
        return ErrorClass.TRANSIENT if code == 429 or code >= 500 else ErrorClass.PERMANENT
    if isinstance(exc, (httpx.TransportError, TimeoutError, ConnectionError)):
        return ErrorClass.TRANSIENT
    if isinstance(exc, (ValueError, TypeError, KeyError, LookupError)):
        return ErrorClass.PERMANENT
    return ErrorClass.UNKNOWN


def should_retry(error_class: ErrorClass, attempt: int, max_attempts: int = 3) -> bool:
    """Whether to try again after attempt number `attempt` (1-based) failed."""
    if error_class is ErrorClass.TRANSIENT:
        return attempt < max_attempts
    if error_class is ErrorClass.UNKNOWN:
        return attempt < 2
    return False


def backoff_seconds(attempt: int, base: float = 10.0, cap: float = 300.0) -> float:
    return min(base * 2 ** (attempt - 1), cap)
```

```python
# mhai/platform/stages.py
"""Pipeline stage names and progress, reported through an optional callback."""

import logging
from collections.abc import Callable

logger = logging.getLogger(__name__)

STAGES = (
    "pii_masking",
    "retrieval",
    "generation",
    "nli_verification",
    "hga_verification",
    "decision",
)

STAGE_PROGRESS = {
    "pii_masking": 0.05,
    "retrieval": 0.2,
    "generation": 0.4,
    "nli_verification": 0.7,
    "hga_verification": 0.85,
    "decision": 0.95,
}

StageCallback = Callable[[str, float], None]


def emit_stage(on_stage: StageCallback | None, stage: str) -> None:
    """Report a stage. A failing callback is logged, never raised: progress
    reporting must not break clinical processing."""
    if on_stage is None:
        return
    try:
        on_stage(stage, STAGE_PROGRESS[stage])
    except Exception:
        logger.warning("stage callback failed", extra={"stage": stage}, exc_info=True)
```

- [ ] **Step 4: Run to verify they pass**

Run: `uv run pytest tests/unit/test_platform_errors.py tests/unit/test_stages.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mhai/platform tests/unit/test_platform_errors.py tests/unit/test_stages.py
git commit -m "Add the shared error classifier and stage helper

One classifier decides transient, permanent or unknown for every
retrying component, and emit_stage() reports pipeline progress without
ever letting a progress failure break clinical processing."
```

---

### Task 3: Inference protocols and HTTP clients

**Files:**
- Create: `mhai/inference/__init__.py`, `mhai/inference/types.py`,
  `mhai/inference/http.py`, `mhai/inference/ocr.py`,
  `mhai/inference/factory.py`
- Test: `tests/unit/test_inference_clients.py`

**Interfaces:**
- Consumes: `mhai.platform.errors` (Task 2); `mhai.tools.ollama_client.OllamaGenerator` (exists).
- Produces (`mhai.inference`):
  - `NLI_LABELS = ("entailment", "neutral", "contradiction")`
  - `@dataclass(frozen=True) NLIOutcome(label: str, scores: dict[str, float])`
  - Protocol `Embedder`: `model: str`, `model_version: str`,
    `embed(texts: Sequence[str]) -> list[list[float]]`
  - Protocol `NLIClassifier`: `model`, `model_version`,
    `classify(pairs: Sequence[tuple[str, str]]) -> list[NLIOutcome]`
  - Protocol `Generator`: `name: str`, `generate(system: str, user: str) -> str`
  - Protocol `OCR`: `engine_version: str`, `page_text(image) -> str`
  - `class InferenceUnavailable(TransientError)`
  - `class InferenceProtocolError(PermanentError)`
  - `CircuitBreaker(failure_threshold=5, reset_after_s=30.0)`
  - `HTTPEmbedder(base_url, model, timeout=30.0, client=None, breaker=None)`
  - `HTTPNLIClassifier(base_url, model, timeout=30.0, client=None, breaker=None)`
  - `TesseractOCR(config="--oem 3 --psm 6")`
  - `InferenceSettings.from_env()`, `build_embedder(s)`,
    `build_nli_classifier(s)`, `build_generator(s)`

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_inference_clients.py
import json

import httpx
import pytest

from mhai.inference import (
    CircuitBreaker,
    HTTPEmbedder,
    HTTPNLIClassifier,
    InferenceProtocolError,
    InferenceUnavailable,
    NLIOutcome,
)
from mhai.platform.errors import ErrorClass, classify


def _client(handler):
    return httpx.Client(transport=httpx.MockTransport(handler))


def test_embed_posts_openai_shape_and_orders_by_index():
    seen = {}

    def handler(request):
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={
                "model": "all-MiniLM-L6-v2",
                "model_version": "abc123",
                "data": [
                    {"index": 1, "embedding": [0.0, 1.0]},
                    {"index": 0, "embedding": [1.0, 0.0]},
                ],
            },
        )

    emb = HTTPEmbedder("http://inference:8080", "all-MiniLM-L6-v2", client=_client(handler))
    vectors = emb.embed(["first", "second"])

    assert seen["url"] == "http://inference:8080/v1/embeddings"
    assert seen["body"] == {"model": "all-MiniLM-L6-v2", "input": ["first", "second"]}
    assert vectors == [[1.0, 0.0], [0.0, 1.0]]
    assert emb.model_version == "abc123"


def test_embed_empty_input_makes_no_call():
    def handler(request):
        raise AssertionError("no HTTP call expected")

    assert HTTPEmbedder("http://x", "m", client=_client(handler)).embed([]) == []


def test_embed_count_mismatch_is_a_protocol_error():
    handler = lambda r: httpx.Response(
        200, json={"model": "m", "data": [{"index": 0, "embedding": [1.0]}]}
    )
    with pytest.raises(InferenceProtocolError):
        HTTPEmbedder("http://x", "m", client=_client(handler)).embed(["a", "b"])


def test_classify_returns_named_labels():
    def handler(request):
        body = json.loads(request.content)
        assert body["pairs"] == [{"premise": "p", "hypothesis": "h"}]
        return httpx.Response(
            200,
            json={
                "model": "nli",
                "model_version": "v1",
                "results": [
                    {
                        "label": "ENTAILMENT",
                        "scores": {"Entailment": 0.9, "neutral": 0.07, "contradiction": 0.03},
                    },
                ],
            },
        )

    out = HTTPNLIClassifier("http://x", "nli", client=_client(handler)).classify([("p", "h")])
    assert out == [
        NLIOutcome("entailment", {"entailment": 0.9, "neutral": 0.07, "contradiction": 0.03})
    ]


@pytest.mark.parametrize("label", ["LABEL_1", "maybe", ""])
def test_unknown_label_is_rejected_not_guessed(label):
    handler = lambda r: httpx.Response(
        200,
        json={
            "model": "nli",
            "results": [
                {
                    "label": label,
                    "scores": {"entailment": 1.0, "neutral": 0.0, "contradiction": 0.0},
                }
            ],
        },
    )
    with pytest.raises(InferenceProtocolError) as err:
        HTTPNLIClassifier("http://x", "nli", client=_client(handler)).classify([("p", "h")])
    assert classify(err.value) is ErrorClass.PERMANENT


def test_transient_failure_retries_once_then_succeeds():
    calls = {"n": 0}

    def handler(request):
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(503)
        return httpx.Response(200, json={"model": "m", "data": [{"index": 0, "embedding": [1.0]}]})

    assert HTTPEmbedder("http://x", "m", client=_client(handler)).embed(["a"]) == [[1.0]]
    assert calls["n"] == 2


def test_circuit_opens_after_repeated_failures():
    calls = {"n": 0}

    def handler(request):
        calls["n"] += 1
        return httpx.Response(503)

    breaker = CircuitBreaker(failure_threshold=2, reset_after_s=60)
    emb = HTTPEmbedder("http://x", "m", client=_client(handler), breaker=breaker)
    with pytest.raises(httpx.HTTPStatusError):
        emb.embed(["a"])  # attempt + one retry = 2 failures: circuit opens
    with pytest.raises(InferenceUnavailable):
        emb.embed(["a"])  # fails fast, no HTTP call
    assert calls["n"] == 2
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_inference_clients.py -v`
Expected: FAIL, `ModuleNotFoundError: No module named 'mhai.inference'`.

- [ ] **Step 3: Implement the protocols**

```python
# mhai/inference/types.py
"""Interfaces the pipeline uses for every model (ADR 0008)."""

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any, Protocol, runtime_checkable

from mhai.platform.errors import PermanentError, TransientError

NLI_LABELS = ("entailment", "neutral", "contradiction")


@dataclass(frozen=True)
class NLIOutcome:
    label: str  # one of NLI_LABELS
    scores: dict[str, float]  # keyed by NLI_LABELS


class InferenceUnavailable(TransientError):
    """The inference service can't be reached (or its circuit is open)."""


class InferenceProtocolError(PermanentError):
    """The inference service answered with something we can't trust."""


@runtime_checkable
class Embedder(Protocol):
    model: str
    model_version: str

    def embed(self, texts: Sequence[str]) -> list[list[float]]: ...


@runtime_checkable
class NLIClassifier(Protocol):
    model: str
    model_version: str

    def classify(self, pairs: Sequence[tuple[str, str]]) -> list[NLIOutcome]: ...


@runtime_checkable
class Generator(Protocol):
    name: str

    def generate(self, system: str, user: str) -> str: ...


@runtime_checkable
class OCR(Protocol):
    engine_version: str

    def page_text(self, image: Any) -> str: ...
```

- [ ] **Step 4: Implement the HTTP clients**

```python
# mhai/inference/http.py
"""HTTP clients for the inference service: retries, circuit breaker, strict parsing."""

import threading
import time
from collections.abc import Sequence

import httpx

from mhai.inference.types import (
    NLI_LABELS,
    InferenceProtocolError,
    InferenceUnavailable,
    NLIOutcome,
)
from mhai.platform.errors import ErrorClass, classify


class CircuitBreaker:
    """Open after N consecutive failures; allow a trial call after a cool-off."""

    def __init__(self, failure_threshold: int = 5, reset_after_s: float = 30.0):
        self.failure_threshold = failure_threshold
        self.reset_after_s = reset_after_s
        self._failures = 0
        self._opened_at: float | None = None
        self._lock = threading.Lock()

    def before_call(self) -> None:
        with self._lock:
            if self._opened_at is None:
                return
            if time.monotonic() - self._opened_at >= self.reset_after_s:
                self._opened_at = None  # half-open: let one call through
                return
        raise InferenceUnavailable("inference circuit open")

    def record(self, ok: bool) -> None:
        with self._lock:
            if ok:
                self._failures, self._opened_at = 0, None
            else:
                self._failures += 1
                if self._failures >= self.failure_threshold:
                    self._opened_at = time.monotonic()


class _InferenceClient:
    def __init__(
        self,
        base_url: str,
        model: str,
        timeout: float = 30.0,
        client: httpx.Client | None = None,
        breaker: CircuitBreaker | None = None,
    ):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.model_version = "unknown"
        self._client = client or httpx.Client(timeout=timeout)
        self._breaker = breaker or CircuitBreaker()

    def _post(self, path: str, payload: dict) -> dict:
        """POST with one retry on a transient error."""
        last: Exception | None = None
        for attempt in (1, 2):
            self._breaker.before_call()
            try:
                resp = self._client.post(f"{self.base_url}{path}", json=payload)
                resp.raise_for_status()
                body = resp.json()
            except Exception as exc:
                self._breaker.record(ok=False)
                last = exc
                if attempt == 1 and classify(exc) is ErrorClass.TRANSIENT:
                    continue
                raise
            self._breaker.record(ok=True)
            self.model_version = str(body.get("model_version", self.model_version))
            return body
        raise last  # pragma: no cover - loop always returns or raises


class HTTPEmbedder(_InferenceClient):
    def embed(self, texts: Sequence[str]) -> list[list[float]]:
        if not texts:
            return []
        body = self._post("/v1/embeddings", {"model": self.model, "input": list(texts)})
        try:
            data = sorted(body["data"], key=lambda d: d["index"])
            vectors = [list(map(float, d["embedding"])) for d in data]
        except (KeyError, TypeError, ValueError) as exc:
            raise InferenceProtocolError("malformed embeddings response") from exc
        if len(vectors) != len(texts):
            raise InferenceProtocolError(f"expected {len(texts)} embeddings, got {len(vectors)}")
        return vectors


class HTTPNLIClassifier(_InferenceClient):
    def classify(self, pairs: Sequence[tuple[str, str]]) -> list[NLIOutcome]:
        if not pairs:
            return []
        body = self._post(
            "/v1/classify",
            {
                "model": self.model,
                "pairs": [{"premise": p, "hypothesis": h} for p, h in pairs],
            },
        )
        try:
            results = body["results"]
            outcomes = [self._outcome(r) for r in results]
        except (KeyError, TypeError, ValueError) as exc:
            raise InferenceProtocolError("malformed classify response") from exc
        if len(outcomes) != len(pairs):
            raise InferenceProtocolError(f"expected {len(pairs)} results, got {len(outcomes)}")
        return outcomes

    @staticmethod
    def _outcome(result: dict) -> NLIOutcome:
        label = str(result["label"]).strip().lower()
        scores = {str(k).strip().lower(): float(v) for k, v in result["scores"].items()}
        if label not in NLI_LABELS or set(scores) != set(NLI_LABELS):
            raise InferenceProtocolError(f"unknown NLI label {result['label']!r}")
        return NLIOutcome(label, scores)
```

```python
# mhai/inference/ocr.py
"""In-process OCR behind the OCR interface (its own service later, ADR 0008)."""

from typing import Any

from mhai.platform.errors import PermanentError


class TesseractOCR:
    def __init__(self, config: str = "--oem 3 --psm 6"):
        self.config = config
        try:
            import pytesseract
        except ImportError as exc:  # pragma: no cover - dependency is in the base group
            raise PermanentError("pytesseract is not installed") from exc
        self._tesseract = pytesseract
        self.engine_version = f"tesseract-{pytesseract.get_tesseract_version()}"

    def page_text(self, image: Any) -> str:
        return self._tesseract.image_to_string(image, config=self.config).strip()
```

```python
# mhai/inference/factory.py
"""Build inference clients from the environment."""

import os
from dataclasses import dataclass

from mhai.inference.http import HTTPEmbedder, HTTPNLIClassifier
from mhai.tools.ollama_client import OllamaGenerator


@dataclass(frozen=True)
class InferenceSettings:
    inference_url: str
    embed_model: str
    nli_model: str
    timeout_s: float

    @classmethod
    def from_env(cls) -> "InferenceSettings":
        embed_model = os.environ.get("DENSE_MODEL", "all-MiniLM-L6-v2")
        if "/" not in embed_model:  # the server registers the full hub name
            embed_model = f"sentence-transformers/{embed_model}"
        return cls(
            inference_url=os.environ.get("INFERENCE_URL", "http://inference:8080"),
            embed_model=embed_model,
            nli_model=os.environ.get("NLI_MODEL", "cross-encoder/nli-deberta-v3-base"),
            timeout_s=float(os.environ.get("INFERENCE_TIMEOUT_S", "30")),
        )


def build_embedder(s: InferenceSettings) -> HTTPEmbedder:
    return HTTPEmbedder(s.inference_url, s.embed_model, timeout=s.timeout_s)


def build_nli_classifier(s: InferenceSettings) -> HTTPNLIClassifier:
    return HTTPNLIClassifier(s.inference_url, s.nli_model, timeout=s.timeout_s)


def build_generator(s: InferenceSettings) -> OllamaGenerator:
    return OllamaGenerator()  # reads OLLAMA_URL / OLLAMA_MODEL itself
```

```python
# mhai/inference/__init__.py
"""Model interfaces and clients. The only place model calls are made (ADR 0008)."""

from mhai.inference.http import CircuitBreaker, HTTPEmbedder, HTTPNLIClassifier
from mhai.inference.types import (
    NLI_LABELS,
    OCR,
    Embedder,
    Generator,
    InferenceProtocolError,
    InferenceUnavailable,
    NLIClassifier,
    NLIOutcome,
)

__all__ = [
    "NLI_LABELS",
    "OCR",
    "CircuitBreaker",
    "Embedder",
    "Generator",
    "HTTPEmbedder",
    "HTTPNLIClassifier",
    "InferenceProtocolError",
    "InferenceUnavailable",
    "NLIClassifier",
    "NLIOutcome",
]
```

Add `"mhai.inference"` to `MODEL_FREE` in `tests/unit/test_boundaries.py`.

- [ ] **Step 5: Run to verify they pass**

Run: `uv run pytest tests/unit/test_inference_clients.py tests/unit/test_boundaries.py -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add mhai/inference tests/unit/test_inference_clients.py tests/unit/test_boundaries.py
git commit -m "Add inference interfaces and HTTP clients

Embedder, NLIClassifier, Generator and OCR are the only way the pipeline
reaches a model. The HTTP clients speak the OpenAI-compatible embeddings
API and a named-label classify API, retry once on transient errors, open
a circuit after repeated failures, and reject unknown NLI labels instead
of guessing."
```

---

### Task 4: The inference service

**Files:**
- Create: `mhai/inference_server/__init__.py`, `backends.py`, `app.py`, `main.py`
- Test: `tests/unit/test_inference_server.py`

**Interfaces:**
- Consumes: `mhai.utils.nli.nli_label_map` (exists).
- Produces:
  - Protocol `EmbeddingBackend`: `name: str`, `version: str`,
    `encode(texts: list[str]) -> list[list[float]]`
  - Protocol `NLIBackend`: `name`, `version`,
    `predict(pairs: list[tuple[str, str]]) -> list[dict[str, float]]`
    (named-label scores)
  - `SentenceTransformerBackend(model_name, device="cpu")`,
    `HFNLIBackend(model_name, device="cpu")` (both import ML libraries
    inside `__init__`)
  - `create_app(embedder: EmbeddingBackend, nli: NLIBackend, max_batch: int = 32, cache_size: int = 10_000) -> FastAPI`
  - `main.create_from_env() -> FastAPI` (for `uvicorn --factory`)

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_inference_server.py
import pytest
from fastapi.testclient import TestClient

from mhai.inference_server.app import create_app


class FakeEmbedder:
    name, version = "fake-embed", "e1"

    def __init__(self):
        self.calls = []

    def encode(self, texts):
        self.calls.append(list(texts))
        return [[float(len(t)), 1.0] for t in texts]


class FakeNLI:
    name, version = "fake-nli", "n1"

    def predict(self, pairs):
        return [{"entailment": 0.8, "neutral": 0.15, "contradiction": 0.05} for _ in pairs]


@pytest.fixture
def setup():
    emb = FakeEmbedder()
    return emb, TestClient(create_app(emb, FakeNLI(), max_batch=2, cache_size=10))


def test_embeddings_openai_shape(setup):
    _, client = setup
    r = client.post("/v1/embeddings", json={"model": "fake-embed", "input": ["ab", "abc", "a"]})
    body = r.json()
    assert r.status_code == 200
    assert body["model"] == "fake-embed" and body["model_version"] == "e1"
    assert [d["index"] for d in body["data"]] == [0, 1, 2]
    assert [d["embedding"][0] for d in body["data"]] == [2.0, 3.0, 1.0]


def test_embeddings_batched_and_cached(setup):
    emb, client = setup
    client.post("/v1/embeddings", json={"model": "fake-embed", "input": ["a", "b", "c"]})
    assert [len(c) for c in emb.calls] == [2, 1]  # max_batch=2
    client.post("/v1/embeddings", json={"model": "fake-embed", "input": ["a", "d"]})
    assert emb.calls[-1] == ["d"]  # "a" came from the cache


def test_wrong_model_is_rejected(setup):
    _, client = setup
    r = client.post("/v1/embeddings", json={"model": "other", "input": ["a"]})
    assert r.status_code == 404


def test_classify_returns_named_labels(setup):
    _, client = setup
    r = client.post(
        "/v1/classify", json={"model": "fake-nli", "pairs": [{"premise": "p", "hypothesis": "h"}]}
    )
    result = r.json()["results"][0]
    assert result["label"] == "entailment"
    assert set(result["scores"]) == {"entailment", "neutral", "contradiction"}


def test_health_lists_models(setup):
    _, client = setup
    models = client.get("/health").json()["models"]
    assert {m["name"]: m["version"] for m in models} == {"fake-embed": "e1", "fake-nli": "n1"}


def test_oversized_request_is_rejected(setup):
    _, client = setup
    r = client.post("/v1/embeddings", json={"model": "fake-embed", "input": ["x"] * 2049})
    assert r.status_code == 422
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_inference_server.py -v`
Expected: FAIL, `ModuleNotFoundError: No module named 'mhai.inference_server'`.

- [ ] **Step 3: Implement the backends**

```python
# mhai/inference_server/backends.py
"""Model backends. The only modules that import ML frameworks (ADR 0008)."""

from typing import Protocol

from mhai.utils.nli import nli_label_map


class EmbeddingBackend(Protocol):
    name: str
    version: str

    def encode(self, texts: list[str]) -> list[list[float]]: ...


class NLIBackend(Protocol):
    name: str
    version: str

    def predict(self, pairs: list[tuple[str, str]]) -> list[dict[str, float]]: ...


def _revision(model) -> str:
    return str(getattr(getattr(model, "config", None), "_commit_hash", None) or "unknown")


class SentenceTransformerBackend:
    def __init__(self, model_name: str, device: str = "cpu"):
        from sentence_transformers import SentenceTransformer

        self.name = model_name
        self._model = SentenceTransformer(model_name, device=device)
        first = self._model[0]
        self.version = _revision(getattr(first, "auto_model", None))

    def encode(self, texts: list[str]) -> list[list[float]]:
        vecs = self._model.encode(texts, normalize_embeddings=True, show_progress_bar=False)
        return [list(map(float, v)) for v in vecs]


class HFNLIBackend:
    def __init__(self, model_name: str, device: str = "cpu"):
        import torch
        from transformers import AutoModelForSequenceClassification, AutoTokenizer

        self.name = model_name
        self._torch = torch
        self._device = device
        self._tok = AutoTokenizer.from_pretrained(model_name)
        self._model = (
            AutoModelForSequenceClassification.from_pretrained(model_name).to(device).eval()
        )
        self._labels = nli_label_map(self._model)  # named labels, read from the model
        self.version = _revision(self._model)

    def predict(self, pairs: list[tuple[str, str]]) -> list[dict[str, float]]:
        premises, hypotheses = zip(*pairs, strict=True)
        inputs = self._tok(
            list(premises),
            list(hypotheses),
            return_tensors="pt",
            truncation=True,
            max_length=512,
            padding=True,
        ).to(self._device)
        with self._torch.no_grad():
            probs = self._torch.softmax(self._model(**inputs).logits, dim=-1)
        return [{self._labels[i]: float(row[i]) for i in self._labels} for row in probs]
```

- [ ] **Step 4: Implement the app**

```python
# mhai/inference_server/app.py
"""Inference service: OpenAI-compatible embeddings and named-label NLI."""

import hashlib
from collections import OrderedDict
from threading import Lock

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, ConfigDict, Field

from mhai.inference_server.backends import EmbeddingBackend, NLIBackend

MAX_ITEMS = 2048


class EmbeddingsIn(BaseModel):
    model_config = ConfigDict(extra="forbid")
    model: str
    input: list[str] = Field(min_length=1, max_length=MAX_ITEMS)


class Pair(BaseModel):
    premise: str
    hypothesis: str


class ClassifyIn(BaseModel):
    model_config = ConfigDict(extra="forbid")
    model: str
    pairs: list[Pair] = Field(min_length=1, max_length=MAX_ITEMS)


class _LRU:
    def __init__(self, size: int):
        self.size, self._data, self._lock = size, OrderedDict(), Lock()

    def get(self, key):
        with self._lock:
            if key in self._data:
                self._data.move_to_end(key)
                return self._data[key]
        return None

    def put(self, key, value):
        with self._lock:
            self._data[key] = value
            self._data.move_to_end(key)
            while len(self._data) > self.size:
                self._data.popitem(last=False)


def create_app(
    embedder: EmbeddingBackend, nli: NLIBackend, max_batch: int = 32, cache_size: int = 10_000
) -> FastAPI:
    app = FastAPI(title="mhai-inference", docs_url=None, redoc_url=None)
    cache = _LRU(cache_size)

    def _key(text: str) -> tuple[str, str, str]:
        return (embedder.name, embedder.version, hashlib.sha256(text.encode()).hexdigest())

    @app.post("/v1/embeddings")
    def embeddings(req: EmbeddingsIn):
        if req.model != embedder.name:
            raise HTTPException(404, f"model {req.model!r} is not served")
        vectors: list[list[float] | None] = [cache.get(_key(t)) for t in req.input]
        missing = [i for i, v in enumerate(vectors) if v is None]
        for start in range(0, len(missing), max_batch):
            idx = missing[start : start + max_batch]
            for i, vec in zip(idx, embedder.encode([req.input[i] for i in idx]), strict=True):
                vectors[i] = vec
                cache.put(_key(req.input[i]), vec)
        return {
            "object": "list",
            "model": embedder.name,
            "model_version": embedder.version,
            "data": [
                {"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vectors)
            ],
        }

    @app.post("/v1/classify")
    def classify(req: ClassifyIn):
        if req.model != nli.name:
            raise HTTPException(404, f"model {req.model!r} is not served")
        pairs = [(p.premise, p.hypothesis) for p in req.pairs]
        results = []
        for start in range(0, len(pairs), max_batch):
            for scores in nli.predict(pairs[start : start + max_batch]):
                results.append({"label": max(scores, key=scores.get), "scores": scores})
        return {"model": nli.name, "model_version": nli.version, "results": results}

    @app.get("/health")
    def health():
        return {
            "status": "ok",
            "models": [
                {"name": embedder.name, "version": embedder.version, "kind": "embedding"},
                {"name": nli.name, "version": nli.version, "kind": "nli"},
            ],
        }

    return app
```

```python
# mhai/inference_server/main.py
"""Entry point: uvicorn mhai.inference_server.main:create_from_env --factory."""

import os

from fastapi import FastAPI

from mhai.inference_server.app import create_app
from mhai.inference_server.backends import HFNLIBackend, SentenceTransformerBackend


def create_from_env() -> FastAPI:
    device = os.environ.get("INFERENCE_DEVICE", "cpu")
    embed_model = os.environ.get("DENSE_MODEL", "all-MiniLM-L6-v2")
    if "/" not in embed_model:
        embed_model = f"sentence-transformers/{embed_model}"
    return create_app(
        SentenceTransformerBackend(embed_model, device=device),
        HFNLIBackend(
            os.environ.get("NLI_MODEL", "cross-encoder/nli-deberta-v3-base"), device=device
        ),
        max_batch=int(os.environ.get("INFERENCE_MAX_BATCH", "32")),
        cache_size=int(os.environ.get("EMBED_CACHE_SIZE", "10000")),
    )
```

```python
# mhai/inference_server/__init__.py
"""The inference service (separate image). See ADR 0008."""
```

**Model names:** the embedder registers under its full hub name
(`sentence-transformers/all-MiniLM-L6-v2`). `InferenceSettings.from_env()`
(Task 3) adds the same prefix, so client and server agree.

- [ ] **Step 5: Run to verify they pass**

Run: `uv run pytest tests/unit/test_inference_server.py -v`
Expected: PASS.

- [ ] **Step 6: Add a real-model test (outside CI)**

```python
# appended to tests/unit/test_inference_server.py
@pytest.mark.models
def test_real_models_label_order():
    from mhai.inference_server.backends import HFNLIBackend

    nli = HFNLIBackend("cross-encoder/nli-deberta-v3-base")
    entail, contra = nli.predict(
        [
            ("Malaria smear is positive.", "The malaria smear was positive."),
            ("Malaria smear is positive.", "The malaria smear was negative."),
        ]
    )
    assert max(entail, key=entail.get) == "entailment"
    assert max(contra, key=contra.get) == "contradiction"
```

Run (on a machine that can download models): `uv run pytest -m models tests/unit/test_inference_server.py -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add mhai/inference_server tests/unit/test_inference_server.py
git commit -m "Add the inference service for embeddings and NLI

A small FastAPI service serves OpenAI-compatible embeddings and
named-label NLI, batches each request, caches embeddings in memory by
model, version and text hash, and reports model versions. Only its
backends import ML frameworks."
```

---

### Task 5: Pipeline uses the clients

**Files:**
- Modify: `mhai/knowledge_base/kb_builder.py` (`DenseRetriever`, `HybridRetriever`)
- Modify: `mhai/agents/hga_nli.py` (`NLIEntailmentVerifier`)
- Test: `tests/unit/test_pipeline_clients.py`

**Interfaces:**
- Consumes: `Embedder`, `NLIClassifier`, `NLIOutcome` (Task 3).
- Produces:
  - `DenseRetriever(model_name=..., device="cpu", embedder: Embedder | None = None)`
  - `HybridRetriever(..., device="cpu", embedder: Embedder | None = None)`
  - `NLIEntailmentVerifier(model_name=..., device="auto", confidence_threshold=0.7, classifier: NLIClassifier | None = None)`
  - `NLIEntailmentVerifier.uses_model: bool` (True when a classifier or a
    local model is present; Plan 2 reports it as `l3_nli.model_loaded`)

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_pipeline_clients.py
import numpy as np

from mhai.agents.hga_nli import NLIEntailmentVerifier, NLILabel
from mhai.inference import NLIOutcome
from mhai.knowledge_base.kb_builder import DenseRetriever, HybridRetriever, MedicalKnowledgeBase


class StubEmbedder:
    model, model_version = "stub", "1"

    def __init__(self):
        self.calls = 0

    def embed(self, texts):
        self.calls += 1
        # "malaria" texts point one way, everything else the other way.
        return [[1.0, 0.0] if "malaria" in t.lower() else [0.0, 1.0] for t in texts]


class StubNLI:
    model, model_version = "stub-nli", "1"

    def classify(self, pairs):
        return [
            NLIOutcome("contradiction", {"entailment": 0.05, "neutral": 0.05, "contradiction": 0.9})
            for _ in pairs
        ]


def test_dense_retriever_uses_embedder_without_loading_a_model():
    emb = StubEmbedder()
    dense = DenseRetriever(embedder=emb)
    dense.index([("KB#0001", "Malaria guideline"), ("KB#0002", "Hypertension guideline")])

    top = dense.retrieve("malaria fever", top_k=1)

    assert dense.model is None
    assert top[0][0] == "KB#0001"
    assert np.isclose(np.linalg.norm(dense.chunk_embeddings[0]), 1.0)


def test_hybrid_retriever_passes_embedder_through():
    kb = MedicalKnowledgeBase()
    kb.build_from_patient_files("/nonexistent")  # guidelines only
    retriever = HybridRetriever(embedder=StubEmbedder())
    retriever.index_kb(kb)
    assert retriever.dense.model is None and retriever.retrieve("malaria", top_k=3)


def test_nli_verifier_uses_classifier():
    v = NLIEntailmentVerifier(classifier=StubNLI())
    result = v.verify("smear positive", "smear negative")
    assert v.uses_model and v.model is None
    assert result.label is NLILabel.CONTRADICTION
    assert result.confidence == 0.9
    assert result.logits == {"entailment": 0.05, "neutral": 0.05, "contradiction": 0.9}
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_pipeline_clients.py -v`
Expected: FAIL, `TypeError: ... unexpected keyword argument 'embedder'`.

- [ ] **Step 3: `DenseRetriever` and `HybridRetriever`**

In `DenseRetriever.__init__`, add a parameter `embedder: "Embedder | None" = None`
after `device`. Store it, and only call `_try_load_model()` when no embedder
is given:

```python
        self.embedder = embedder
        if embedder is None:
            self._try_load_model()
```

At the top of `index()`'s encoding branch, and in `retrieve()`, add the
embedder path before the existing ones:

```python
        # in index(), replacing the "if self._loaded and self.model is not None:" head
        if self.embedder is not None:
            vecs = np.asarray(self.embedder.embed(texts), dtype=np.float32)
            norms = np.linalg.norm(vecs, axis=1, keepdims=True)
            self.chunk_embeddings = vecs / np.where(norms == 0, 1.0, norms)
        elif self._loaded and self.model is not None:
            ...  # existing code unchanged
```

```python
        # in retrieve(), replacing the query_emb computation head
        if self.embedder is not None:
            q = np.asarray(self.embedder.embed([query])[0], dtype=np.float32)
            norm = np.linalg.norm(q)
            query_emb = q / (norm or 1.0)
        elif self._loaded and self.model is not None:
            ...  # existing code unchanged
```

In `HybridRetriever.__init__`, add `embedder: "Embedder | None" = None` and
pass it on: `self.dense = DenseRetriever(dense_model, device, embedder=embedder)`.
Add `from mhai.inference.types import Embedder` under
`if TYPE_CHECKING:`, with `from typing import TYPE_CHECKING` at the top.

- [ ] **Step 4: `NLIEntailmentVerifier`**

Change `__init__` to accept `classifier: "NLIClassifier | None" = None`:

```python
        self.classifier = classifier
        if classifier is None:
            self._try_load_model(device)
```

Add the property and the classifier branch at the start of `verify()`:

```python
@property
def uses_model(self) -> bool:
    return self.classifier is not None or (self._loaded and self.model is not None)


def verify(self, premise: str, hypothesis: str) -> NLIResult:
    if self.classifier is not None:
        outcome = self.classifier.classify([(premise, hypothesis)])[0]
        return NLIResult(
            premise=premise[:100],
            hypothesis=hypothesis[:100],
            label=NLILabel(outcome.label),
            confidence=outcome.scores[outcome.label],
            logits=dict(outcome.scores),
        )
    ...  # existing body unchanged
```

A classifier error is *not* caught here: it propagates up to the
orchestrator, which marks HGA failed and escalates (fail closed).

- [ ] **Step 5: Run to verify they pass, then the full suite**

Run: `uv run pytest tests/unit/test_pipeline_clients.py -v && uv run pytest -q -m "not models"`
Expected: PASS; no regressions.

- [ ] **Step 6: Commit**

```bash
git add mhai/knowledge_base/kb_builder.py mhai/agents/hga_nli.py tests/unit/test_pipeline_clients.py
git commit -m "Let the retriever and NLI verifier use inference clients

DenseRetriever and HybridRetriever take an Embedder, and
NLIEntailmentVerifier takes an NLIClassifier. With a client they load no
model in-process; without one they behave as before for research
scripts. Classifier errors propagate so the orchestrator fails closed."
```

---

### Task 6: Per-citation results, only retrieved citations count

**Files:**
- Modify: `mhai/agents/hga_nli.py` (`CitationVerificationResult`,
  `CitationVerificationPipeline.verify_response`, `NLIBackedHGA.verify`)
- Modify: `mhai/agents/orchestrator.py` (`process_query`)
- Test: `tests/security/test_citations_retrieved.py`

**Interfaces:**
- Produces:
  - `CitationVerificationResult.source_title: str = ""` (new field, last)
  - `CitationVerificationPipeline.verify_response(response_text, patient_id=None, allowed_chunk_ids: set[str] | None = None)`
  - `NLIBackedHGA.verify(..., allowed_chunk_ids: set[str] | None = None)` returns
    an extra key `"citations": list[dict]`, each
    `{chunk_id, source_title, excerpt, claim_text, nli_label, nli_confidence, action, reason}`
    (labels and actions upper-case; `excerpt` ≤300 characters), plus
    `"nli_model_loaded": bool`.
  - Orchestrator result gains `"citations"`, `"hga_details"`
    (`{base_trust, flag_penalty, weights, citation_report, nli_model_loaded}`
    or `None`), `"retrieval_ms"` and `"generation_ms"`.
  - `source_title(chunk) -> str` helper in `hga_nli.py`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/security/test_citations_retrieved.py
"""A citation counts only if it was retrieved for this query and patient."""

from mhai.agents.hga_nli import CitationVerificationPipeline, NLIBackedHGA, NLILabel, NLIResult
from mhai.agents.orchestrator import DynamicOrchestratorAgent

from .conftest import PATIENT_A


class CountingEntails:
    def __init__(self):
        self.calls = 0

    def verify(self, premise, hypothesis):
        self.calls += 1
        return NLIResult(premise, hypothesis, NLILabel.ENTAILMENT, 0.99)


def _own_chunks(kb, n=2):
    return [c for c in kb.chunks.values() if c.source_patient_id == PATIENT_A][:n]


def test_unretrieved_citation_is_redacted_without_nli(two_patient_kb):
    retrieved, other = _own_chunks(two_patient_kb)
    nli = CountingEntails()
    pipe = CitationVerificationPipeline(nli, two_patient_kb)

    report = pipe.verify_response(
        f"Smear positive [{other.chunk_id}].",
        patient_id=PATIENT_A,
        allowed_chunk_ids={retrieved.chunk_id},
    )

    [r] = report.citation_results
    assert r.action.value.upper() == "REDACT" and r.reason == "not_retrieved"
    assert nli.calls == 0


def test_hga_returns_per_citation_detail(two_patient_kb):
    [chunk] = _own_chunks(two_patient_kb, 1)
    hga = NLIBackedHGA(
        citation_pipeline=CitationVerificationPipeline(CountingEntails(), two_patient_kb)
    )

    out = hga.verify(
        f"Smear positive for malaria [{chunk.chunk_id}].",
        patient_id=PATIENT_A,
        allowed_chunk_ids={chunk.chunk_id},
    )

    [c] = out["citations"]
    assert c["chunk_id"] == chunk.chunk_id
    assert c["nli_label"] == "ENTAILMENT" and c["action"] == "PASS"
    assert c["source_title"] and c["excerpt"] == chunk.content[:300]
    assert out["nli_model_loaded"] is False  # CountingEntails has no model


class CitingCCA:
    def __init__(self, cited, retrieved):
        self.cited, self.retrieved = cited, retrieved

    def generate_with_rag(self, query, top_k=5, triage_context=None, patient_id=None, **kw):
        return {
            "response": f"Smear positive for malaria [{self.cited}].",
            "citations": [self.cited[3:]],
            "n_citations": 1,
            "retrieved_chunks": [{"id": cid} for cid in self.retrieved],
            "retrieval_latency_ms": 12.0,
            "total_latency_ms": 112.0,
        }


def test_orchestrator_passes_retrieved_ids_and_reports_citations(two_patient_kb):
    retrieved, other = _own_chunks(two_patient_kb)
    hga = NLIBackedHGA(
        citation_pipeline=CitationVerificationPipeline(CountingEntails(), two_patient_kb)
    )
    orch = DynamicOrchestratorAgent(
        rag_cca=CitingCCA(other.chunk_id, [retrieved.chunk_id]), hga=hga
    )

    result = orch.process_query("Malaria smear result?", patient_id=PATIENT_A)

    [c] = result["citations"]
    assert c["action"] == "REDACT" and c["reason"] == "not_retrieved"
    assert result["retrieval_ms"] == 12.0 and result["generation_ms"] == 100.0
    assert result["hga_details"]["citation_report"]["n_claims"] == 1
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/security/test_citations_retrieved.py -v`
Expected: FAIL, `TypeError: ... unexpected keyword argument 'allowed_chunk_ids'`.

- [ ] **Step 3: Pipeline changes in `hga_nli.py`**

Add to the end of the `CitationVerificationResult` dataclass:
`source_title: str = ""`.

Add a helper above `class CitationVerificationPipeline`:

```python
def source_title(chunk) -> str:
    """Human-readable source name for a KB chunk (never the KB id)."""
    meta = getattr(chunk, "metadata", {}) or {}
    if getattr(chunk, "chunk_type", "") == "guideline" and meta.get("topic"):
        return f"{meta['topic']} guideline"
    if meta.get("document_id") and meta.get("page"):
        return f"Uploaded document, page {meta['page']}"
    return f"Patient {getattr(chunk, 'chunk_type', 'record').replace('_', ' ')}"
```

In `verify_response`, add the parameter `allowed_chunk_ids: Optional[set] = None`.
In the per-citation loop, immediately after the `chunk is None` block, add:

```python
if allowed_chunk_ids is not None and cid not in allowed_chunk_ids:
    results.append(
        CitationVerificationResult(
            claim_text=claim_text,
            citation_id=cid,
            source_text="",
            nli_result=NLIResult(
                premise="", hypothesis=claim_text[:100], label=NLILabel.NEUTRAL, confidence=0.0
            ),
            action=VerificationAction.REDACT,
            reason="not_retrieved",
        )
    )
    continue
```

Where results are built for real chunks (both the patient-visibility REDACT
and the NLI-decided result), set `source_title=source_title(chunk)`.

In `NLIBackedHGA.verify`, add the parameter `allowed_chunk_ids: Optional[set] = None`,
pass it to `self.citation_pipeline.verify_response(..., allowed_chunk_ids=allowed_chunk_ids)`,
and add to the returned dict:

```python
            "citations": [
                {
                    "chunk_id": r.citation_id,
                    "source_title": r.source_title,
                    "excerpt": r.source_text[:300],
                    "claim_text": r.claim_text,
                    "nli_label": r.nli_result.label.value.upper(),
                    "nli_confidence": round(r.nli_result.confidence, 4),
                    "action": r.action.value.upper(),
                    "reason": r.reason,
                }
                for r in (citation_report.citation_results if citation_report else [])
                if r.citation_id != "NONE"
            ],
            "nli_model_loaded": bool(
                self.citation_pipeline is not None
                and getattr(self.citation_pipeline.nli, "uses_model", False)
            ),
```

Check `CitationVerificationResult.source_text` holds the full chunk content
for the NLI path (currently `chunk.content[:150]`). Change it to
`chunk.content` so the 300-character excerpt is possible.

- [ ] **Step 4: Orchestrator changes**

In `process_query`, before calling `self.hga.verify`, compute:

```python
            retrieved = cca_result.get("retrieved_chunks")
            allowed = {c["id"] for c in retrieved} if retrieved is not None else None
```

and pass `allowed_chunk_ids=allowed` to `self.hga.verify(...)`.

Add to the `result` dict:

```python
            "citations": hga_result.get("citations", []) if hga_result else [],
            "hga_details": {
                "base_trust": hga_result.get("base_trust"),
                "flag_penalty": hga_result.get("flag_penalty"),
                "weights": hga_result.get("weights"),
                "citation_report": hga_result.get("citation_report"),
                "nli_model_loaded": hga_result.get("nli_model_loaded", False),
            } if hga_result else None,
            "retrieval_ms": cca_result.get("retrieval_latency_ms") if cca_result else None,
            "generation_ms": (
                round(cca_result["total_latency_ms"] - cca_result.get("retrieval_latency_ms", 0.0), 1)
                if cca_result and "total_latency_ms" in cca_result else None
            ),
```

- [ ] **Step 5: Run to verify they pass, then the full suite**

Run: `uv run pytest tests/security -q && uv run pytest -q -m "not models"`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add mhai/agents/hga_nli.py mhai/agents/orchestrator.py tests/security/test_citations_retrieved.py
git commit -m "Return per-citation results; count only retrieved citations

HGA returned only totals, so a clinician couldn't see which claims were
supported. Return each citation with its source title, excerpt, NLI
label and action. A citation of a chunk that wasn't retrieved for this
query and patient is redacted as not_retrieved before any NLI call. The
orchestrator also reports retrieval and generation timings."
```

---

### Task 7: Stage hook through the pipeline

**Files:**
- Modify: `mhai/security/hipaa.py` (`SecurePipelineWrapper.submit_query`)
- Modify: `mhai/agents/orchestrator.py` (`process_query`)
- Modify: `mhai/knowledge_base/kb_builder.py` (`RAGClinicalContextAgent.generate_with_rag`)
- Modify: `mhai/agents/hga_nli.py` (`NLIBackedHGA.verify`)
- Modify: `tests/security/test_orchestrator_fail_closed.py` (stubs accept `**kwargs`)
- Test: `tests/unit/test_stage_hook.py`

**Interfaces:**
- Consumes: `emit_stage`, `StageCallback` (Task 2).
- Produces: an `on_stage: StageCallback | None = None` keyword argument on
  `submit_query`, `process_query`, `generate_with_rag` and `verify`. The
  orchestrator passes `on_stage` to the CCA and HGA **only when it isn't
  None**, so existing third-party stubs keep working.

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_stage_hook.py
from mhai.agents.hga_nli import NLIBackedHGA
from mhai.agents.orchestrator import DynamicOrchestratorAgent
from mhai.knowledge_base.kb_builder import (
    DenseRetriever,
    HybridRetriever,
    MedicalKnowledgeBase,
    RAGClinicalContextAgent,
)
from mhai.platform.stages import STAGES
from mhai.security.hipaa import AuditLogger, RBACManager, SecurePipelineWrapper, UserRole


def _wrapper(tmp_path, monkeypatch):
    monkeypatch.setattr(
        DenseRetriever, "_try_load_model", lambda self: setattr(self, "_loaded", False)
    )
    kb = MedicalKnowledgeBase()
    kb.build_from_patient_files(str(tmp_path / "none"))
    retriever = HybridRetriever()
    retriever.index_kb(kb)
    orch = DynamicOrchestratorAgent(rag_cca=RAGClinicalContextAgent(retriever), hga=NLIBackedHGA())
    rbac = RBACManager()
    w = SecurePipelineWrapper(orch, rbac=rbac, audit=AuditLogger(log_dir=str(tmp_path / "audit")))
    return w, rbac.create_session("dr_a", UserRole.CLINICIAN).session_id


def test_all_stages_reported_in_order(tmp_path, monkeypatch):
    w, sid = _wrapper(tmp_path, monkeypatch)
    seen = []

    w.submit_query(
        sid, "Fever and chills, malaria smear positive?", on_stage=lambda s, p: seen.append(s)
    )

    assert seen == list(STAGES)


def test_failing_callback_does_not_break_the_result(tmp_path, monkeypatch):
    w, sid = _wrapper(tmp_path, monkeypatch)

    def broken(stage, progress):
        raise RuntimeError("progress store down")

    result = w.submit_query(sid, "Fever and chills, malaria smear positive?", on_stage=broken)

    assert result["degraded"] is False and result["response_available"] is True
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_stage_hook.py -v`
Expected: FAIL, `TypeError: ... unexpected keyword argument 'on_stage'`.

- [ ] **Step 3: Thread the hook**

In each of the four methods, add the keyword argument
`on_stage: Optional["StageCallback"] = None` and import
`from mhai.platform.stages import emit_stage` (plus `StageCallback` under
`TYPE_CHECKING`):

- `SecurePipelineWrapper.submit_query`: call `emit_stage(on_stage, "pii_masking")`
  immediately before `# 2. PII masking`. Pass `on_stage=on_stage` to
  `self.orchestrator.process_query(...)`.
- `RAGClinicalContextAgent.generate_with_rag`: call
  `emit_stage(on_stage, "retrieval")` before `# Step 1: Retrieve` and
  `emit_stage(on_stage, "generation")` before `# Step 3: Generate`.
- `NLIBackedHGA.verify`: call `emit_stage(on_stage, "nli_verification")` before
  `# ── Expert 1` and `emit_stage(on_stage, "hga_verification")` before
  `# ── Expert 2`.
- `DynamicOrchestratorAgent.process_query`: build
  `stage_kw = {"on_stage": on_stage} if on_stage is not None else {}` and pass
  `**stage_kw` to `generate_with_rag(...)` and `self.hga.verify(...)`. Call
  `emit_stage(on_stage, "decision")` before `# Step 5`. When CCA is missing or
  fails, the later stages are skipped; `decision` is still emitted.

- [ ] **Step 4: Make the fail-closed test stubs tolerate new keywords**

In `tests/security/test_orchestrator_fail_closed.py`, change
`StubCCA.generate_with_rag(self, query, top_k=5, triage_context=None, patient_id=None)`
to add `**kwargs` at the end.

- [ ] **Step 5: Run to verify they pass, then the full suite**

Run: `uv run pytest tests/unit/test_stage_hook.py -v && uv run pytest -q -m "not models"`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add mhai/security/hipaa.py mhai/agents/orchestrator.py mhai/knowledge_base/kb_builder.py mhai/agents/hga_nli.py tests/unit/test_stage_hook.py tests/security/test_orchestrator_fail_closed.py
git commit -m "Report pipeline stages through an optional callback

submit_query, process_query, generate_with_rag and HGA verify accept
on_stage and report pii_masking, retrieval, generation,
nli_verification, hga_verification and decision in order, so the
worker can show progress. A failing callback is logged and never breaks
the clinical result."
```

---

### Task 8: Diagnosis and ICD-10 extraction

**Files:**
- Create: `mhai/agents/diagnosis.py`
- Modify: `mhai/agents/orchestrator.py` (result gains `diagnosis`, `icd10_code`)
- Test: `tests/unit/test_diagnosis.py`

**Interfaces:**
- Produces: `extract_diagnosis(text: str) -> tuple[str | None, str | None]`;
  orchestrator result keys `"diagnosis"`, `"icd10_code"` (both `None` when
  the response isn't available).

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_diagnosis.py
import pytest

from mhai.agents.diagnosis import extract_diagnosis


@pytest.mark.parametrize(
    "text, expected",
    [
        (
            "Diagnosis: Gestational Diabetes Mellitus [ICD-10: O24.4]\nReasoning: ...",
            ("Gestational Diabetes Mellitus", "O24.4"),
        ),
        ("**Diagnosis:** Malaria [ICD-10: B54] [KB#0022]", ("Malaria", "B54")),
        (
            "ASSESSMENT AND DIAGNOSES:\n\n1. Diagnosis: Plasmodium falciparum malaria [ICD-10: B50.9]. [KB#0003]",
            ("Plasmodium falciparum malaria", "B50.9"),
        ),
        ("1. Suspected Malaria [ICD-10: B54]", ("Suspected Malaria", "B54")),
        ("The patient is stable. Continue current plan.", (None, None)),
        ("Diagnosis: Hypertension [ICD-10: ICD-10-CM 465.81]", ("Hypertension", None)),
    ],
)
def test_extract_diagnosis(text, expected):
    assert extract_diagnosis(text) == expected
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_diagnosis.py -v`
Expected: FAIL, `ModuleNotFoundError`.

- [ ] **Step 3: Implement**

```python
# mhai/agents/diagnosis.py
"""Pull the primary diagnosis and ICD-10 code out of CCA text; None if unsure."""

import re

_ICD10 = re.compile(r"\b([A-TV-Z][0-9][0-9AB](?:\.[0-9A-TV-Z]{1,4})?)\b")
_ICD_TAG = re.compile(r"\[ICD-10:\s*([^\]]*)\]", re.IGNORECASE)
_DIAG_LINE = re.compile(r"diagnos[ie]s\W*:\s*\**\s*(.+)", re.IGNORECASE)
_NUMBERED = re.compile(r"^\s*1\.\s+(.+)$", re.MULTILINE)


def _clean(name: str) -> str:
    name = _ICD_TAG.sub("", name)
    name = re.sub(r"\[KB#\d{4}\]", "", name)
    return name.strip(" *.:-\t")


def extract_diagnosis(text: str) -> tuple[str | None, str | None]:
    for line in text.splitlines():
        m = _DIAG_LINE.search(line)
        if m and _clean(m.group(1)):
            return _clean(m.group(1)), _icd(line)
    m = _NUMBERED.search(text)
    if m and _ICD_TAG.search(m.group(1)):
        return _clean(m.group(1)), _icd(m.group(1))
    return None, None


def _icd(fragment: str) -> str | None:
    tag = _ICD_TAG.search(fragment)
    if not tag:
        return None
    code = _ICD10.fullmatch(tag.group(1).strip())
    return code.group(1) if code else None
```

- [ ] **Step 4: Wire into the orchestrator**

In `process_query`, after the `result` dict is built:

```python
        from mhai.agents.diagnosis import extract_diagnosis

        result["diagnosis"], result["icd10_code"] = (
            extract_diagnosis(result["response"]) if result["response_available"] else (None, None)
        )
```

(A module-level import is fine too; it has no heavy dependencies.)

- [ ] **Step 5: Run to verify they pass**

Run: `uv run pytest tests/unit/test_diagnosis.py -q && uv run pytest -q -m "not models"`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add mhai/agents/diagnosis.py mhai/agents/orchestrator.py tests/unit/test_diagnosis.py
git commit -m "Extract the diagnosis and ICD-10 code from CCA text

The result object needs a diagnosis and code. Parse a Diagnosis: line or
the first numbered item with an ICD-10 tag, accept only codes in valid
ICD-10 form, and return None rather than guess."
```

---

### Task 9: Keep admin accounts out of clinical work

**Files:**
- Modify: `mhai/security/hipaa.py` (`RBACManager.PERMISSIONS`, docstring matrix)
- Test: `tests/security/test_rbac_admin_split.py`

**Interfaces:**
- Produces: `RBACManager.PERMISSIONS[UserRole.ADMIN]` without `query_submit`,
  `view_patient_data` or `submit_feedback`.

- [ ] **Step 1: Write the failing test**

```python
# tests/security/test_rbac_admin_split.py
import pytest

from mhai.security.hipaa import RBACManager, UserRole

CLINICAL = ("query_submit", "view_patient_data", "submit_feedback")


@pytest.mark.parametrize("action", CLINICAL)
def test_admin_cannot_do_clinical_work(action):
    rbac = RBACManager()
    sid = rbac.create_session("admin_1", UserRole.ADMIN).session_id
    assert rbac.check_permission(sid, action)[0] is False


@pytest.mark.parametrize("action", ("view_audit_log", "manage_users", "modify_thresholds"))
def test_admin_keeps_admin_work(action):
    rbac = RBACManager()
    sid = rbac.create_session("admin_1", UserRole.ADMIN).session_id
    assert rbac.check_permission(sid, action)[0] is True


@pytest.mark.parametrize("action", CLINICAL)
def test_clinician_still_can(action):
    rbac = RBACManager()
    sid = rbac.create_session("dr_a", UserRole.CLINICIAN).session_id
    assert rbac.check_permission(sid, action)[0] is True
```

- [ ] **Step 2: Run to verify it fails**

Run: `uv run pytest tests/security/test_rbac_admin_split.py -v`
Expected: FAIL on `test_admin_cannot_do_clinical_work`.

- [ ] **Step 3: Implement**

Set:

```python
        UserRole.ADMIN: {
            "view_anonymized", "view_audit_log", "modify_thresholds",
            "view_metrics", "manage_users", "export_data",
        },
```

Update the docstring matrix: the ADMIN column becomes N for
`query_submit`, `view_patient_data` and `submit_feedback`. Add one line:
"Admins don't do clinical work; staff who need both hold two accounts
(design #8)."

- [ ] **Step 4: Run to verify, then the full suite**

Run: `uv run pytest tests/security -q && uv run pytest -q -m "not models"`
Expected: PASS. If a test created an ADMIN session to submit queries,
change it to CLINICIAN.

- [ ] **Step 5: Commit**

```bash
git add mhai/security/hipaa.py tests/security/test_rbac_admin_split.py
git commit -m "Keep admin accounts out of clinical work

Admins could submit queries, view patient data and give feedback, so a
stolen admin account could do everything. Remove those permissions;
staff who need both roles hold two accounts."
```

---

### Task 10: OCR-only document ingest

**Files:**
- Create: `mhai/ingest/__init__.py`, `mhai/ingest/ocr_pages.py`
- Modify: `mhai/agents/dia.py` (docstring of `_detect_regions` only)
- Test: `tests/unit/test_ocr_pages.py`

**Interfaces:**
- Consumes: `OCR` protocol (Task 3); `KBChunk`, `is_valid_patient_id` (exist);
  `PermanentError` (Task 2).
- Produces:
  - `@dataclass(frozen=True) PageText(page: int, text: str)`
  - `ocr_pdf(pdf_bytes: bytes, ocr: OCR, max_pages: int = 50, dpi: int = 200) -> list[PageText]`
  - `page_chunks(document_id: str, patient_id: str, pages: list[PageText], ocr_engine_version: str, id_prefix: str = "DOC") -> list[KBChunk]`

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_ocr_pages.py
import io

import pytest
from PIL import Image

from mhai.ingest.ocr_pages import PageText, ocr_pdf, page_chunks
from mhai.platform.errors import ErrorClass, PermanentError, classify


def _pdf(pages: int) -> bytes:
    images = [Image.new("RGB", (200, 100), "white") for _ in range(pages)]
    buf = io.BytesIO()
    images[0].save(buf, "PDF", save_all=True, append_images=images[1:])
    return buf.getvalue()


class StubOCR:
    engine_version = "stub-1"

    def __init__(self):
        self.calls = 0

    def page_text(self, image):
        self.calls += 1
        return f"page text {self.calls}"


def test_each_page_is_ocred_once():
    ocr = StubOCR()
    pages = ocr_pdf(_pdf(3), ocr)
    assert pages == [
        PageText(1, "page text 1"),
        PageText(2, "page text 2"),
        PageText(3, "page text 3"),
    ]


def test_too_many_pages_is_permanent_and_nothing_is_ocred():
    ocr = StubOCR()
    with pytest.raises(PermanentError, match="pages"):
        ocr_pdf(_pdf(3), ocr, max_pages=2)
    assert ocr.calls == 0


@pytest.mark.parametrize("data", [b"%PDF-1.4 broken", b"not a pdf", b""])
def test_unreadable_pdf_is_permanent(data):
    with pytest.raises(PermanentError) as err:
        ocr_pdf(data, StubOCR())
    assert classify(err.value) is ErrorClass.PERMANENT


def test_chunks_are_patient_scoped_and_marked_ocr_only():
    chunks = page_chunks(
        "doc-1", "PAT-A", [PageText(1, "Hb 9.6 g/dL"), PageText(2, "  ")], "stub-1"
    )
    [c] = chunks  # blank pages are dropped
    assert c.source_patient_id == "PAT-A" and c.chunk_type == "document_page"
    assert c.metadata == {
        "document_id": "doc-1",
        "page": 1,
        "ocr_only": True,
        "ocr_engine_version": "stub-1",
    }


@pytest.mark.parametrize("pid", ["reference", "", "unknown"])
def test_reserved_patient_ids_are_refused(pid):
    with pytest.raises(PermanentError):
        page_chunks("doc-1", pid, [PageText(1, "x")], "stub-1")
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_ocr_pages.py -v`
Expected: FAIL, `ModuleNotFoundError: No module named 'mhai.ingest'`.

- [ ] **Step 3: Implement**

```python
# mhai/ingest/ocr_pages.py
"""OCR-only ingest: whole pages, real text, no invented layout (design #8)."""

import hashlib
from dataclasses import dataclass

from mhai.inference.types import OCR
from mhai.knowledge_base.kb_builder import KBChunk, is_valid_patient_id
from mhai.platform.errors import PermanentError


@dataclass(frozen=True)
class PageText:
    page: int
    text: str


def ocr_pdf(pdf_bytes: bytes, ocr: OCR, max_pages: int = 50, dpi: int = 200) -> list[PageText]:
    """Render and OCR every page. Fails before any OCR if the PDF is unusable."""
    from pdf2image import convert_from_bytes, pdfinfo_from_bytes
    from pdf2image.exceptions import PDFInfoNotInstalledError, PDFPageCountError, PDFSyntaxError

    if not pdf_bytes.startswith(b"%PDF-"):
        raise PermanentError("not a PDF")
    try:
        page_count = int(pdfinfo_from_bytes(pdf_bytes)["Pages"])
    except (PDFPageCountError, PDFSyntaxError, KeyError, ValueError) as exc:
        raise PermanentError("unreadable PDF") from exc
    except PDFInfoNotInstalledError as exc:  # deployment problem, not the file's
        raise RuntimeError("poppler is not installed") from exc
    if page_count > max_pages:
        raise PermanentError(f"{page_count} pages exceeds the {max_pages}-page limit")
    try:
        images = convert_from_bytes(pdf_bytes, dpi=dpi, timeout=120)
    except (PDFPageCountError, PDFSyntaxError) as exc:
        raise PermanentError("unreadable PDF") from exc
    return [PageText(i, ocr.page_text(img)) for i, img in enumerate(images, start=1)]


def page_chunks(
    document_id: str,
    patient_id: str,
    pages: list[PageText],
    ocr_engine_version: str,
    id_prefix: str = "DOC",
) -> list[KBChunk]:
    """One patient-scoped chunk per non-blank page."""
    if not is_valid_patient_id(patient_id):
        raise PermanentError("documents must belong to a valid patient")
    chunks = []
    for p in pages:
        if not p.text.strip():
            continue
        digest = hashlib.sha256(f"{document_id}:{p.page}".encode()).hexdigest()[:12]
        chunks.append(
            KBChunk(
                chunk_id=f"{id_prefix}#{digest}",
                content=p.text.strip(),
                chunk_type="document_page",
                source_patient_id=patient_id,
                source_subject_id=document_id,
                metadata={
                    "document_id": document_id,
                    "page": p.page,
                    "ocr_only": True,
                    "ocr_engine_version": ocr_engine_version,
                },
            )
        )
    return chunks
```

```python
# mhai/ingest/__init__.py
"""Document ingest (OCR-only until a trained layout model exists)."""
```

**Note:** chunk ids here (`DOC#<hash>`) don't match the CCA's
`[KB#\d{4}]` citation regex. Plan 3 (documents) decides how document
chunks get citable ids when they join the KB. Record that as a Plan 3 task;
nothing in this plan cites document chunks yet.

In `mhai/agents/dia.py`, replace the `_detect_regions` docstring's first
line with: `"""Demo-only fixed layout boxes, not detection. Never used for real ingest; see mhai.ingest.ocr_pages."""`

Add `"mhai.ingest.ocr_pages"` to `MODEL_FREE` in `tests/unit/test_boundaries.py`.

- [ ] **Step 4: Run to verify they pass**

Run: `uv run pytest tests/unit/test_ocr_pages.py tests/unit/test_boundaries.py -v`
Expected: PASS. Poppler must be installed locally (`pdfinfo --version`);
CI installs it in Task 12.

- [ ] **Step 5: Commit**

```bash
git add mhai/ingest mhai/agents/dia.py tests/unit/test_ocr_pages.py tests/unit/test_boundaries.py
git commit -m "Add OCR-only document ingest

The DIA ingest path returned the same hard-coded layout boxes for every
page. OCR each whole page instead, refuse unreadable or oversized PDFs
before any OCR, and turn pages into patient-scoped chunks marked
ocr_only. The fixed boxes stay only in the demo path."
```

---

### Task 11: Triage gate

**Files:**
- Create: `mhai/agents/triage_gate.py`
- Test: `tests/unit/test_triage_gate.py`

**Interfaces:**
- Produces:
  - `@dataclass(frozen=True) GateStatus(open: bool, reason: str, weights_sha256: str | None = None)`
  - `TriageGate(enabled: bool, weights_path: str | None, weights_sha256: str | None, validation_report_path: str | None)`
  - `.check() -> GateStatus`
  - `TriageGate.from_env() -> TriageGate` (`TRIAGE_ENABLED`,
    `QDA_WEIGHTS_PATH`, `QDA_WEIGHTS_SHA256`, `QDA_VALIDATION_REPORT`)
  - reason values: `open`, `disabled`, `weights_missing`,
    `checksum_mismatch`, `report_missing`, `report_invalid`

- [ ] **Step 1: Write the failing tests**

```python
# tests/unit/test_triage_gate.py
import hashlib
import json

from mhai.agents.triage_gate import TriageGate


def _files(tmp_path, report=None):
    weights = tmp_path / "qda.pt"
    weights.write_bytes(b"weights")
    sha = hashlib.sha256(b"weights").hexdigest()
    rep = tmp_path / "report.json"
    rep.write_text(
        json.dumps(
            report
            if report is not None
            else {"validated_at": "2026-10-01", "metrics": {"auroc": 0.81}}
        )
    )
    return str(weights), sha, str(rep)


def test_open_only_when_everything_checks_out(tmp_path):
    w, sha, rep = _files(tmp_path)
    status = TriageGate(True, w, sha, rep).check()
    assert status.open and status.reason == "open" and status.weights_sha256 == sha


def test_disabled_by_default(tmp_path):
    w, sha, rep = _files(tmp_path)
    assert TriageGate(False, w, sha, rep).check().reason == "disabled"


def test_missing_weights(tmp_path):
    _, sha, rep = _files(tmp_path)
    assert TriageGate(True, str(tmp_path / "nope.pt"), sha, rep).check().reason == "weights_missing"


def test_checksum_mismatch(tmp_path):
    w, _, rep = _files(tmp_path)
    assert TriageGate(True, w, "0" * 64, rep).check().reason == "checksum_mismatch"


def test_report_missing_or_invalid(tmp_path):
    w, sha, _ = _files(tmp_path)
    assert TriageGate(True, w, sha, None).check().reason == "report_missing"
    _, _, bad = _files(tmp_path, report={"metrics": {}})
    assert TriageGate(True, w, sha, bad).check().reason == "report_invalid"


def test_from_env_defaults_closed(monkeypatch):
    for k in ("TRIAGE_ENABLED", "QDA_WEIGHTS_PATH", "QDA_WEIGHTS_SHA256", "QDA_VALIDATION_REPORT"):
        monkeypatch.delenv(k, raising=False)
    assert TriageGate.from_env().check().open is False
```

- [ ] **Step 2: Run to verify they fail**

Run: `uv run pytest tests/unit/test_triage_gate.py -v`
Expected: FAIL, `ModuleNotFoundError`.

- [ ] **Step 3: Implement**

```python
# mhai/agents/triage_gate.py
"""Triage stays closed until a validated QDA model is configured (design #8)."""

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class GateStatus:
    open: bool
    reason: str
    weights_sha256: str | None = None


class TriageGate:
    def __init__(
        self,
        enabled: bool,
        weights_path: str | None,
        weights_sha256: str | None,
        validation_report_path: str | None,
    ):
        self.enabled = enabled
        self.weights_path = weights_path
        self.weights_sha256 = (weights_sha256 or "").lower() or None
        self.validation_report_path = validation_report_path

    @classmethod
    def from_env(cls) -> "TriageGate":
        return cls(
            enabled=os.environ.get("TRIAGE_ENABLED", "false").lower() == "true",
            weights_path=os.environ.get("QDA_WEIGHTS_PATH"),
            weights_sha256=os.environ.get("QDA_WEIGHTS_SHA256"),
            validation_report_path=os.environ.get("QDA_VALIDATION_REPORT"),
        )

    def check(self) -> GateStatus:
        if not self.enabled:
            return GateStatus(False, "disabled")
        if not self.weights_path or not Path(self.weights_path).is_file():
            return GateStatus(False, "weights_missing")
        digest = hashlib.sha256(Path(self.weights_path).read_bytes()).hexdigest()
        if digest != self.weights_sha256:
            return GateStatus(False, "checksum_mismatch")
        if not self.validation_report_path or not Path(self.validation_report_path).is_file():
            return GateStatus(False, "report_missing")
        try:
            report = json.loads(Path(self.validation_report_path).read_text())
            valid = bool(report.get("validated_at")) and bool(report.get("metrics"))
        except (ValueError, AttributeError):
            valid = False
        if not valid:
            return GateStatus(False, "report_invalid")
        return GateStatus(True, "open", weights_sha256=digest)
```

- [ ] **Step 4: Run to verify they pass**

Run: `uv run pytest tests/unit/test_triage_gate.py -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mhai/agents/triage_gate.py tests/unit/test_triage_gate.py
git commit -m "Add the triage gate

There are no trained QDA weights, and the integrated pipeline simulated
triage scores. The gate stays closed unless triage is enabled, the
weights exist and match their SHA-256, and a validation report with
metrics is present, so nothing ever serves a simulated score."
```

---

### Task 12: Images, Compose and CI for the inference service

**Files:**
- Modify: `Dockerfile` (app image: no `ml` group; add `tesseract-ocr`, `poppler-utils`)
- Create: `Dockerfile.inference`
- Modify: `docker-compose.yml`, `docker-compose.gpu.yml`, `.env.example`
- Modify: `.github/workflows/ci.yml` (install poppler and tesseract; build both images)
- Modify: `docs/specs/2026-10-08-api-gateway-phase1-design.md` (two clarifications), `ARCHITECTURE.md` (status line), `CHANGELOG.md`

**Interfaces:**
- Produces: Compose service `inference` on `127.0.0.1:${INFERENCE_PORT:-8080}`,
  healthcheck `GET /health`; env `INFERENCE_URL=http://inference:8080`.

- [ ] **Step 1: App image without ML, with OCR tools**

In `Dockerfile`:

- in the **build** stage, change both `uv sync` lines from
  `--no-default-groups --group ${TORCH}` to `--no-default-groups`
  (no torch, no `ml`);
- remove `ARG TORCH=cpu` from the build stage;
- in the **runtime** stage, change the apt line to install
  `curl tesseract-ocr poppler-utils`.

- [ ] **Step 2: The inference image**

```dockerfile
# Dockerfile.inference
# syntax=docker/dockerfile:1

FROM python:3.12.15-slim AS build
COPY --from=ghcr.io/astral-sh/uv:0.12.23 /uv /usr/local/bin/uv
WORKDIR /app
ARG TORCH=cpu
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
COPY pyproject.toml uv.lock README.md ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-default-groups --group ${TORCH} --group ml --no-install-project
COPY mhai ./mhai
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-default-groups --group ${TORCH} --group ml --no-editable

FROM python:3.12.15-slim AS runtime
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PATH="/app/.venv/bin:$PATH" \
    HF_HOME=/home/appuser/.cache/huggingface
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/* && useradd --create-home appuser
WORKDIR /app
COPY --from=build /app/.venv /app/.venv
RUN mkdir -p /home/appuser/.cache/huggingface && chown appuser /home/appuser/.cache/huggingface
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s \
  CMD curl -f http://localhost:8080/health || exit 1
CMD ["uvicorn", "mhai.inference_server.main:create_from_env", "--factory", "--host", "0.0.0.0", "--port", "8080"]
```

- [ ] **Step 3: Compose**

Add to `docker-compose.yml` under `services:`:

```yaml
  # Embeddings and NLI (ADR 0008). The app images import no ML frameworks.
  inference:
    build:
      context: .
      dockerfile: Dockerfile.inference
      args:
        TORCH: ${TORCH:-cpu}
    image: mhai-inference:${TORCH:-cpu}
    restart: unless-stopped
    environment:
      DENSE_MODEL: ${DENSE_MODEL:-all-MiniLM-L6-v2}
      NLI_MODEL: ${NLI_MODEL:-cross-encoder/nli-deberta-v3-base}
      INFERENCE_DEVICE: ${INFERENCE_DEVICE:-cpu}
    ports: ["127.0.0.1:${INFERENCE_PORT:-8080}:8080"]
    volumes:
      - inference-models:/home/appuser/.cache/huggingface
    healthcheck:
      <<: *healthcheck-defaults
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      start_period: 180s
```

- Add `inference-models:` under `volumes:`.
- In the `mhai` service, add `INFERENCE_URL: http://inference:8080` to
  `environment` and `inference: { condition: service_healthy }` to
  `depends_on`.
- Change the `mhai` service's `build.args` to drop `TORCH` and set
  `image: mhai:app`.
- In `docker-compose.gpu.yml`:
  - move the `mhai` GPU block and `TORCH: cu128` arg to a new `inference:`
    entry, with `environment: { INFERENCE_DEVICE: cuda }`;
  - remove the `mhai` entry (it has no GPU work now).
- In `.env.example`, under `# Models`, add:

```
# Inference service (embeddings + NLI); app images call it over HTTP.
INFERENCE_URL=http://inference:8080
INFERENCE_DEVICE=cpu            # cuda with docker-compose.gpu.yml
```

Run: `docker compose config -q && docker compose -f docker-compose.yml -f docker-compose.gpu.yml config -q`
Expected: both valid.

- [ ] **Step 4: CI**

In `.github/workflows/ci.yml`:

- in the `checks` job, before `Install`, add:

```yaml
      - name: OCR tools
        run: sudo apt-get update && sudo apt-get install -y --no-install-recommends poppler-utils tesseract-ocr
```

- in the `image` job's paths filter, add `Dockerfile.inference`;
- replace the build step with:

```yaml
      - name: Build images and smoke-test
        if: steps.changes.outputs.image == 'true'
        run: |
          docker buildx build --load -t mhai:ci .
          docker run --rm mhai:ci --help
          docker run --rm --entrypoint python mhai:ci -c "import sys, mhai.agents.orchestrator, mhai.inference, mhai.ingest.ocr_pages; assert 'torch' not in sys.modules"
          docker buildx build --load --build-arg TORCH=cpu -f Dockerfile.inference -t mhai-inference:ci .
          docker run --rm --entrypoint python mhai-inference:ci -c "import mhai.inference_server.app, torch, transformers"
```

Run: `uvx --from actionlint-py actionlint`
Expected: no findings.

- [ ] **Step 5: Docs**

- In the spec, §8 item 9: replace the bullet "the torch-based classes in
  `l2_semantic_entropy.py` move to a research-only module" with:
  "`l2_semantic_entropy.py` is not on the worker's import path (verified),
  so it stays as is".
- In the spec, §8a, `inference` service: replace "It batches requests
  (max batch size and max wait configurable)" with: "It batches within
  each request (`INFERENCE_MAX_BATCH`); dynamic batching across requests
  is left to production engines (TEI, vLLM)".
- In `ARCHITECTURE.md`'s status line, add: "Plan 1 (inference boundary)
  implemented."
- In `CHANGELOG.md` under *Unreleased → Added*:

```
- Inference service (embeddings and NLI over HTTP) and model-free pipeline
  clients; the application image no longer installs torch (ADR 0008).
- Per-citation results, a pipeline stage hook, diagnosis extraction,
  OCR-only document ingest and a closed-by-default triage gate.
```

and under *Changed*: `- **Breaking:** admin accounts can no longer submit
queries, view patient data or give feedback.`

- [ ] **Step 6: Full verification**

Run:
`uv run ruff check . && uv run ruff format --check . && uv run mypy && uv run pytest -q -m "not models" && uv lock --check`
Expected: all clean.

- [ ] **Step 7: Commit**

```bash
git add Dockerfile Dockerfile.inference docker-compose.yml docker-compose.gpu.yml .env.example .github/workflows/ci.yml docs/specs ARCHITECTURE.md CHANGELOG.md
git commit -m "Run models in their own inference image

The application image installs no ML frameworks and gains Tesseract and
poppler for OCR ingest. A separate inference image serves embeddings and
NLI; Compose runs it with a health check and the GPU override targets it.
CI builds both images, checks the app image doesn't import torch, and
installs the OCR tools the tests need."
```

- [ ] **Step 8: Real-model check on a machine that can load the models (manual)**

Run: `docker compose up -d inference ollama ollama-init`, then point the
existing `scripts/e2e_models.py` at the inference clients (or run the
`models`-marked tests). Record the results in the PR description.

- [ ] **Step 9: Open the PR**

Write the description to the scratchpad (not the repo) following
`.github/pull_request_template.md`:
- what and why: one paragraph per task group;
- how it was tested: the Step 6 command output and the Step 8 manual
  results;
- what wasn't verified;
- the checklist.

Note that the PR also carries the design commits already on the branch
(spec, ADRs 0007 and 0008, diagrams).

```bash
git push
gh pr create --base main --head feat/8-api-gateway \
  --title "API gateway, part 1: inference boundary and pipeline changes (#8)" \
  --body-file "$SCRATCHPAD/pr-plan-1.md"
```

Do **not** merge; a human reviews and merges.
