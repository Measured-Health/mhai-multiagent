"""The model seam of the run_query job: ModelClient (spec §12, plan Task 11).

The job calls four things, each on a worker thread:

  mask(text) -> str                       PII masking (PIIMasker)
  generate(question, patient_context,     retrieval and generation, one
           patient_id, chunks)            call (departure 9)
           -> Generation
  verify(generation) -> Verification      NLI and HGA
  model_versions() -> dict[str, str]

OrchestratorModelClient wraps DynamicOrchestratorAgent and PIIMasker
without changing them: it builds an orchestrator per job over that job's
chunks (never mutating a shared agent) and runs its CCA and HGA with the
query's own thresholds, as process_query does. Failures escalate (ADR
0005): a missing or failing agent gives ESCALATE, degraded, with no score;
nothing here invents one.

StubModelClient returns fixed synthetic answers and can be told, per stage,
to fail (a ModelError with a code), raise (any exception) or block (until
an event is set). It is for tests and the CI smoke test: the settings
refuse INTELLIGENCE_MODEL_CLIENT=stub outside APP_ENV=development, and
build_model_client refuses it again.

Nothing here logs question text, patient data or model output; Generation
and Verification keep text out of their repr.
"""

from __future__ import annotations

import logging
import re
import threading
from collections import Counter
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Protocol, runtime_checkable

from intelligence.boundary.config import ConfigError, WorkerSettings

if TYPE_CHECKING:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.knowledge_base.kb_builder import KBChunk

log = logging.getLogger("intelligence.boundary.models")

MODEL_CLIENTS = ("orchestrator", "stub")
STAGES = ("mask", "generate", "verify")
DECISIONS = ("PASS", "REGENERATE", "ESCALATE")
_RETRYABLE = frozenset({"unavailable", "internal", "overloaded"})
_MODEL_CODES = frozenset({"unavailable", "internal", "overloaded", "invalid_request"})
_SAFE_KEY = re.compile(r"[A-Za-z0-9_.:\-]{1,64}")
_SAFE_VERSION = re.compile(r"[A-Za-z0-9_.:/@+\-]{1,128}")
_CHUNK_ID = re.compile(r"[A-Za-z0-9#_.:\-]{1,16}")
MAX_CITED = 50


class ModelError(Exception):
    """A model stage failed with a code from the closed set (never text)."""

    def __init__(self, code: str) -> None:
        if code not in _MODEL_CODES:
            raise ValueError("not a model error code")
        super().__init__(code)
        self.code = code

    @property
    def retryable(self) -> bool:
        return self.code in _RETRYABLE


@dataclass(frozen=True)
class Generation:
    """What generate() returns. question and response stay out of repr."""

    question: str = field(default="", repr=False)
    response: str = field(default="", repr=False)
    cited_chunk_ids: tuple[str, ...] = ()
    retrieved_chunk_ids: tuple[str, ...] = ()
    model_used: str = ""
    failed_agents: tuple[str, ...] = ()
    available: bool = False
    retrieval_ms: float | None = None
    generation_ms: float | None = None
    patient_id: str | None = field(default=None, repr=False)
    # The client's per-job state for verify() (the orchestrator it built).
    handle: Any = field(default=None, repr=False, compare=False)


@dataclass(frozen=True)
class Verification:
    """What verify() returns: the guardrail decision, ids and numbers only."""

    decision: str = "ESCALATE"
    trust_score: float | None = None
    verified: bool = False
    degraded: bool = True
    failed_agents: tuple[str, ...] = ()
    scores: Mapping[str, float] = field(default_factory=dict)
    flag_penalty: float | None = None
    weights: Mapping[str, float] = field(default_factory=dict)
    # (type, severity) of each flag: never the claim text a flag quotes.
    flags: tuple[tuple[str, str], ...] = ()


def trust_band(v: Verification) -> str | None:
    """The band the result shows (low, mid, high), from the guardrail's own
    decision; None when nothing was verified (no score to band)."""
    if not v.verified or v.trust_score is None:
        return None
    return {"PASS": "high", "REGENERATE": "mid"}.get(v.decision, "low")


@runtime_checkable
class ModelClient(Protocol):
    def mask(self, text: str) -> str: ...

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[KBChunk],
    ) -> Generation: ...

    def verify(self, generation: Generation) -> Verification: ...

    def model_versions(self) -> dict[str, str]: ...


def _escalated(failed: Sequence[str]) -> Verification:
    """Fail closed: unverified or missing goes to a human, with no score."""
    return Verification(
        decision="ESCALATE",
        trust_score=None,
        verified=False,
        degraded=True,
        failed_agents=tuple(failed),
    )


def _clean_versions(versions: Mapping[str, Any]) -> dict[str, str]:
    out: dict[str, str] = {}
    for k, v in versions.items():
        k, v = str(k), str(v)
        if _SAFE_KEY.fullmatch(k) and _SAFE_VERSION.fullmatch(v):
            out[k] = v
    return out


def _number(v: Any) -> float | None:
    return float(v) if isinstance(v, int | float) and not isinstance(v, bool) else None


# --- the orchestrator -------------------------------------------------------------


Builder = Callable[[Sequence["KBChunk"]], "DynamicOrchestratorAgent"]


class OrchestratorModelClient:
    """DynamicOrchestratorAgent and PIIMasker, unchanged, behind ModelClient.

    build(chunks) makes the orchestrator for one job, its retriever indexed
    over that job's chunks; generate() runs its CCA, verify() its HGA with
    the thresholds the orchestrator's classifier and threshold manager give
    the question (passed per call, as process_query does).
    """

    def __init__(
        self,
        build: Builder,
        masker: Any = None,
        versions: Mapping[str, str] | None = None,
    ) -> None:
        if masker is None:
            from intelligence.security.hipaa import PIIMasker

            masker = PIIMasker()
        self._build = build
        self._masker = masker
        self._mask_lock = threading.Lock()  # PIIMasker counts detections
        self._versions = _clean_versions(versions or {})

    def mask(self, text: str) -> str:
        with self._mask_lock:
            masked, _detections = self._masker.mask(text)
        return str(masked)

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[KBChunk],
    ) -> Generation:
        try:
            # Building loads the models: a failure here escalates like any
            # other model failure (ADR 0005).
            orch = self._build(list(chunks))
        except Exception as e:
            log.warning("model stage failed agent=build error=%s", type(e).__name__)
            return Generation(question=question, failed_agents=("CCA",))
        cca = getattr(orch, "rag_cca", None)
        if cca is None:
            return Generation(question=question, failed_agents=("CCA",), handle=orch)
        try:
            r = cca.generate_with_rag(
                question,
                top_k=5,
                triage_context=dict(patient_context),
                patient_id=patient_id,
            )
            response = r["response"]
            if not isinstance(response, str):
                raise TypeError("no response text")
        except Exception as e:
            log.warning("model stage failed agent=CCA error=%s", type(e).__name__)
            return Generation(question=question, failed_agents=("CCA",), handle=orch)
        retrieved = tuple(
            str(c.get("id"))
            for c in r.get("retrieved_chunks") or ()
            if isinstance(c, dict) and _CHUNK_ID.fullmatch(str(c.get("id")))
        )
        cited: list[str] = []
        for n in r.get("citations") or ():
            cid = f"KB#{n}"
            if cid in retrieved and cid not in cited:
                cited.append(cid)
        total = _number(r.get("total_latency_ms"))
        retrieval = _number(r.get("retrieval_latency_ms"))
        return Generation(
            question=question,
            response=response,
            cited_chunk_ids=tuple(cited[:MAX_CITED]),
            retrieved_chunk_ids=retrieved,
            model_used=str(r.get("model_used", "")),
            available=True,
            retrieval_ms=retrieval,
            generation_ms=(total - retrieval) if total is not None and retrieval else total,
            patient_id=patient_id,
            handle=orch,
        )

    def verify(self, generation: Generation) -> Verification:
        if not generation.available:
            return _escalated(generation.failed_agents or ("CCA",))
        orch = generation.handle
        hga = getattr(orch, "hga", None)
        if hga is None:
            return _escalated(("HGA",))
        try:
            classified = orch.classifier.classify(generation.question)
            t = orch.threshold_manager.get_thresholds(classified)
            r = hga.verify(
                response_text=generation.response,
                patient_id=generation.patient_id,
                pass_threshold=t.trust_pass,
                escalate_threshold=t.trust_escalate,
                critical_penalty=t.critical_flag_penalty,
                warning_penalty=t.warning_flag_penalty,
            )
            trust = _number(r.get("trust_score"))
            if trust is None:
                raise TypeError("no trust score")
        except Exception as e:
            log.warning("model stage failed agent=HGA error=%s", type(e).__name__)
            return _escalated(("HGA",))
        decision = str(r.get("decision", "")).upper()
        if decision not in DECISIONS:
            decision = "ESCALATE"
        scores = {
            k: s
            for k, s in (
                ("nli", _number(r.get("nli_score"))),
                ("coherence", _number(r.get("coherence_score"))),
                ("numerical", _number(r.get("numerical_score"))),
                ("se", _number(r.get("semantic_entropy_score"))),
            )
            if s is not None
        }
        weights = {
            str(k): float(w)
            for k, w in (r.get("weights") or {}).items()
            if _number(w) is not None and _SAFE_KEY.fullmatch(str(k))
        }
        flags = tuple(
            (str(f.get("type", ""))[:32], str(f.get("severity", ""))[:16])
            for f in r.get("flags") or ()
            if isinstance(f, dict)
        )
        return Verification(
            decision=decision,
            trust_score=trust,
            verified=True,
            degraded=False,
            scores=scores,
            flag_penalty=_number(r.get("flag_penalty")),
            weights=weights,
            flags=flags,
        )

    def model_versions(self) -> dict[str, str]:
        return dict(self._versions)


def default_orchestrator() -> OrchestratorModelClient:
    """The production client: each job gets its own knowledge base,
    retriever, CCA and HGA over that job's chunks (departure 10: indexing
    per job; a cached index is performance-plan work). The NLI model and
    the Ollama generator are shared: they hold no per-request state. Loads
    the models on first use, on the job's thread."""
    from intelligence.agents.hga_nli import (
        CitationVerificationPipeline,
        NLIBackedHGA,
        NLIEntailmentVerifier,
    )
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.config.settings import get_config
    from intelligence.knowledge_base.kb_builder import (
        HybridRetriever,
        MedicalKnowledgeBase,
        RAGClinicalContextAgent,
    )
    from intelligence.tools.ollama_client import OllamaGenerator

    cfg = get_config()
    generator = OllamaGenerator()
    lock = threading.Lock()
    shared: dict[str, Any] = {}

    def nli() -> Any:
        with lock:
            if "nli" not in shared:
                shared["nli"] = NLIEntailmentVerifier(model_name=cfg.NLI_MODEL, device="cpu")
            return shared["nli"]

    def build(chunks: Sequence[KBChunk]) -> DynamicOrchestratorAgent:
        kb = MedicalKnowledgeBase()
        kb.chunks = {c.chunk_id: c for c in chunks}
        retriever = HybridRetriever(
            dense_model=f"sentence-transformers/{cfg.DENSE_MODEL}", device="cpu"
        )
        retriever.index_kb(kb)
        verifier = nli()
        hga = NLIBackedHGA(
            nli_verifier=verifier, citation_pipeline=CitationVerificationPipeline(verifier, kb)
        )
        cca = RAGClinicalContextAgent(retriever, generator=generator)
        return DynamicOrchestratorAgent(rag_cca=cca, hga=hga)

    return OrchestratorModelClient(
        build,
        versions={
            "cca": generator.name,
            "dense": str(cfg.DENSE_MODEL),
            "nli": str(cfg.NLI_MODEL),
        },
    )


# --- the stub -----------------------------------------------------------------------


class StubModelClient:
    """Fixed synthetic answers, for tests and the CI smoke test only.

    fail={stage: code} raises ModelError(code); raises={stage: exc} raises
    exc; block={stage: event} waits for the event (at most block_timeout
    seconds, then raises). started[stage] is set when a stage begins; calls
    counts them. Stages: mask, generate, verify.
    """

    MASKED = "Synthetic masked question."
    ANSWER = "Synthetic stub answer: follow the shared guideline."

    def __init__(
        self,
        *,
        fail: Mapping[str, str] | None = None,
        raises: Mapping[str, BaseException] | None = None,
        block: Mapping[str, threading.Event] | None = None,
        decision: str = "PASS",
        trust_score: float = 0.9,
        block_timeout: float = 120.0,
    ) -> None:
        for m in (fail, raises, block):
            if m is not None and not set(m) <= set(STAGES):
                raise ValueError("unknown stage")
        if decision not in DECISIONS:
            raise ValueError("unknown decision")
        self._fail = dict(fail or {})
        self._raises = dict(raises or {})
        self._block = dict(block or {})
        self._decision = decision
        self._trust = trust_score
        self._block_timeout = block_timeout
        self._lock = threading.Lock()
        self.calls: Counter[str] = Counter()
        self.started = {s: threading.Event() for s in STAGES}
        self.seen_questions: list[str] = []
        self.seen_chunk_ids: list[tuple[str, ...]] = []

    def _enter(self, stage: str) -> None:
        with self._lock:
            self.calls[stage] += 1
        self.started[stage].set()
        gate = self._block.get(stage)
        if gate is not None and not gate.wait(self._block_timeout):
            raise TimeoutError("stub stage blocked too long")
        if stage in self._raises:
            raise self._raises[stage]
        if stage in self._fail:
            raise ModelError(self._fail[stage])

    def mask(self, text: str) -> str:
        self._enter("mask")
        return self.MASKED

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[KBChunk],
    ) -> Generation:
        self._enter("generate")
        ids = tuple(c.chunk_id for c in chunks if _CHUNK_ID.fullmatch(c.chunk_id))
        with self._lock:
            self.seen_questions.append(question)
            self.seen_chunk_ids.append(tuple(c.chunk_id for c in chunks))
        return Generation(
            question=question,
            response=self.ANSWER,
            cited_chunk_ids=ids[:1],
            retrieved_chunk_ids=ids,
            model_used="stub",
            available=True,
            retrieval_ms=0.0,
            generation_ms=0.0,
            patient_id=patient_id,
        )

    def verify(self, generation: Generation) -> Verification:
        self._enter("verify")
        if not generation.available:
            return _escalated(generation.failed_agents or ("CCA",))
        return Verification(
            decision=self._decision,
            trust_score=self._trust,
            verified=True,
            degraded=False,
            scores={"nli": 0.9, "coherence": 1.0, "numerical": 0.85, "se": 0.7},
            flag_penalty=0.0,
            weights={"alpha": 0.35, "beta": 0.25, "gamma": 0.2, "delta": 0.2},
        )

    def model_versions(self) -> dict[str, str]:
        return {"stub": "1"}


def build_model_client(settings: WorkerSettings) -> ModelClient:
    """The client INTELLIGENCE_MODEL_CLIENT names. The stub only in
    development (checked again here, not only in the settings: F14)."""
    if settings.model_client == "stub":
        if not settings.development:
            raise ConfigError("INTELLIGENCE_MODEL_CLIENT=stub runs only with APP_ENV=development")
        log.warning("model client is the stub: synthetic answers only")
        return StubModelClient()
    if settings.model_client == "orchestrator":
        return default_orchestrator()
    raise ConfigError("INTELLIGENCE_MODEL_CLIENT must be orchestrator or stub")
