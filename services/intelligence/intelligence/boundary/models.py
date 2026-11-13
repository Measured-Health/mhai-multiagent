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

Pinned models (plan Task 23): model_versions() is the legacy entries (cca,
dense, nli, as configured) overlaid by the roles the worker verified from
models.lock, each "<name>@<version>#<first 12 hex of the manifest SHA-256>"
(_SAFE_VERSION allows '#' in that form only; core's modelVersionPattern
matches it). nli is not a pinnable role yet (lock.ROLES has no suite for it;
TODO.md "Pinnable roles"), so the worker refuses INTELLIGENCE_MODEL_ROLES=nli
at start-up and the NLI model today is always the configured one. The pinned
branch of nli_factory is the path it will take once nli is pinnable: from the
verified directory only, with local_files_only=True and use_safetensors=True,
after HF_HUB_OFFLINE=1 is set, and not at all until the pin is verified (the
job escalates), never the configured hub model instead.

Nothing here logs question text, patient data or model output; Generation
and Verification keep text out of their repr.
"""

from __future__ import annotations

import logging
import os
import re
import sys
import threading
from collections import Counter
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any, Protocol, runtime_checkable

from intelligence.boundary.config import ConfigError, WorkerSettings

if TYPE_CHECKING:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.boundary.cloud.client import CloudCall
    from intelligence.knowledge_base.kb_builder import KBChunk

log = logging.getLogger("intelligence.boundary.models")

MODEL_CLIENTS = ("orchestrator", "stub")
STAGES = ("mask", "generate", "verify")
DECISIONS = ("PASS", "REGENERATE", "ESCALATE")
_RETRYABLE = frozenset({"unavailable", "internal", "overloaded"})
_MODEL_CODES = frozenset({"unavailable", "internal", "overloaded", "invalid_request"})
_SAFE_KEY = re.compile(r"[A-Za-z0-9_.:\-]{1,64}")
# A version with no '#', or a pinned role's "<name>@<version>#<hash12>"
# (modelstore.verify.version_string): '#' is allowed in that form only.
_PINNED_VERSION = (
    r"[a-z0-9][a-z0-9-]{1,62}@(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})"
    r"\.(?:0|[1-9][0-9]{0,8})#[0-9a-f]{12}"
)
_SAFE_VERSION = re.compile(r"[A-Za-z0-9_.:/@+\-]{1,128}|" + _PINNED_VERSION, re.ASCII)
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
    # The cloud route (plan Task 9): versions this generation adds to the
    # decision's model_versions (cca, cca_route; none on a failure), where
    # its text went (vocab.EGRESS), the outcome (vocab.OUTCOMES; None on
    # the local path) and the provider's reported tokens.
    versions: Mapping[str, str] = field(default_factory=dict)
    egress: str = "none"
    cloud_outcome: str | None = None
    tokens_in: int = 0
    tokens_out: int = 0


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
        *,
        call: CloudCall | None = None,
    ) -> Generation: ...

    def verify(self, generation: Generation) -> Verification: ...

    def model_versions(self) -> dict[str, str]: ...


class Pinned(Protocol):
    """What the worker verified (modelstore.loader.PinnedModels)."""

    def versions(self) -> dict[str, str]: ...

    def path(self, role: str) -> Path: ...


PinnedSource = Callable[[], Pinned | None]


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


def _verify_with(orch: Any, generation: Generation) -> Verification:
    """Local verification (NLI and HGA) of an available generation, with
    the question's own thresholds, as process_query does. The local and the
    cloud client both use it, so a cloud answer is verified exactly as a
    local one (design section 9)."""
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
        pinned: PinnedSource | None = None,
    ) -> None:
        if masker is None:
            from intelligence.security.hipaa import PIIMasker

            masker = PIIMasker()
        self._build = build
        self._masker = masker
        self._mask_lock = threading.Lock()  # PIIMasker counts detections
        self._versions = _clean_versions(versions or {})
        self._pinned = pinned

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
        *,
        call: CloudCall | None = None,
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
        return _verify_with(generation.handle, generation)

    def model_versions(self) -> dict[str, str]:
        """The legacy entries overlaid by the verified pinned roles' strings."""
        out = dict(self._versions)
        pinned = self._pinned() if self._pinned is not None else None
        if pinned is not None:
            out.update(_clean_versions(pinned.versions()))
        return out


def set_hub_offline() -> None:
    """HF_HUB_OFFLINE=1 (and TRANSFORMERS_OFFLINE=1) for this process.
    huggingface_hub reads it when first imported, so this runs before any
    transformers or huggingface_hub import a pinned model's loading makes; if
    one was imported earlier, its flag is set too. local_files_only=True on
    every pinned load is the second wall."""
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    constants = sys.modules.get("huggingface_hub.constants")
    if constants is not None:
        constants.HF_HUB_OFFLINE = True  # type: ignore[attr-defined]


def nli_factory(model_name: str, pinned: PinnedSource, *, pinned_nli: bool) -> Callable[[], Any]:
    """The shared NLI verifier, made on first use: the configured model.
    pinned_nli is the path for when nli becomes pinnable; until its suite
    exists the worker refuses an nli role at start-up. With it: from the
    verified directory only (local_files_only, use_safetensors, offline),
    and not at all until the pin is verified (RuntimeError: the job
    escalates).
    TODO(TODO.md "Pinnable roles"): reachable once nli is in lock.ROLES."""
    lock = threading.Lock()
    shared: dict[str, Any] = {}

    def nli() -> Any:
        with lock:
            if "nli" not in shared:
                if pinned_nli:
                    p = pinned()
                    if p is None:
                        raise RuntimeError("the pinned nli model is not verified")
                    set_hub_offline()
                    from intelligence.agents import hga_nli

                    shared["nli"] = hga_nli.NLIEntailmentVerifier(
                        model_name=str(p.path("nli")),
                        device="cpu",
                        local_files_only=True,
                        require_model=True,
                    )
                else:
                    from intelligence.agents import hga_nli

                    shared["nli"] = hga_nli.NLIEntailmentVerifier(
                        model_name=model_name, device="cpu"
                    )
            return shared["nli"]

    return nli


def _local_nli(folder: Path) -> Callable[[], Any]:
    """The NLI verifier from a verified folder only (local_files_only, safetensors,
    require_model), made once on first use."""
    lock = threading.Lock()
    shared: dict[str, Any] = {}

    def nli() -> Any:
        with lock:
            if "nli" not in shared:
                from intelligence.agents import hga_nli

                shared["nli"] = hga_nli.NLIEntailmentVerifier(
                    model_name=str(folder),
                    device="cpu",
                    local_files_only=True,
                    require_model=True,
                )
            return shared["nli"]

    return nli


def default_orchestrator(
    pinned: PinnedSource | None = None, pinned_roles: Sequence[str] = ()
) -> OrchestratorModelClient:
    """The production client: each job gets its own knowledge base,
    retriever, CCA and HGA over that job's chunks (departure 10: indexing
    per job; a cached index is performance-plan work). The NLI model and
    the Ollama generator are shared: they hold no per-request state. Loads
    the models on first use, on the job's thread. pinned gives what the
    worker verified (None until then); pinned_roles are the roles it needs."""
    source: PinnedSource = pinned if pinned is not None else (lambda: None)
    build, versions = local_parts(source, pinned_roles)
    return OrchestratorModelClient(build, versions=versions, pinned=source)


def local_parts(
    pinned: PinnedSource | None = None,
    pinned_roles: Sequence[str] = (),
    *,
    generator: Any = None,
    local_models: Mapping[str, Path] | None = None,
) -> tuple[Builder, dict[str, str]]:
    """default_orchestrator's builder and versions, for the cloud clients
    (plan Task 9): they retrieve and verify with the same local models.
    pinned and pinned_roles as for default_orchestrator. generator replaces
    the Ollama generator (the answer gate scores a candidate GGUF through it:
    intelligence.doceval.answers.gate). local_models ({"dense", "nli"} ->
    a verified folder, doceval.answers.gate_models) loads both models from
    those folders only, offline, instead of the configured hub names."""
    if local_models is not None:
        set_hub_offline()
    pinned_nli = "nli" in pinned_roles
    if pinned_nli:
        set_hub_offline()  # before the imports below can import transformers
    source: PinnedSource = pinned if pinned is not None else (lambda: None)
    from intelligence.agents.hga_nli import (
        CitationVerificationPipeline,
        NLIBackedHGA,
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
    if generator is None:
        generator = OllamaGenerator()
    dense_name = f"sentence-transformers/{cfg.DENSE_MODEL}"
    if local_models is not None:
        dense_name = str(local_models["dense"])
        nli = _local_nli(local_models["nli"])
    else:
        nli = nli_factory(str(cfg.NLI_MODEL), source, pinned_nli=pinned_nli)

    def build(chunks: Sequence[KBChunk]) -> DynamicOrchestratorAgent:
        kb = MedicalKnowledgeBase()
        kb.chunks = {c.chunk_id: c for c in chunks}
        retriever = HybridRetriever(dense_model=dense_name, device="cpu")
        retriever.index_kb(kb)
        verifier = nli()
        hga = NLIBackedHGA(
            nli_verifier=verifier, citation_pipeline=CitationVerificationPipeline(verifier, kb)
        )
        cca = RAGClinicalContextAgent(retriever, generator=generator)
        return DynamicOrchestratorAgent(rag_cca=cca, hga=hga)

    return build, {
        "cca": generator.name,
        "dense": str(cfg.DENSE_MODEL),
        "nli": str(cfg.NLI_MODEL),
    }


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
        *,
        call: CloudCall | None = None,
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


def build_model_client(settings: WorkerSettings, pinned: PinnedSource | None = None) -> ModelClient:
    """The client INTELLIGENCE_MODEL_CLIENT names. The stub only in
    development (checked again here, not only in the settings: F14).
    pinned gives the verified pinned models (settings.model_roles)."""
    if settings.model_client == "stub":
        if not settings.development:
            raise ConfigError("INTELLIGENCE_MODEL_CLIENT=stub runs only with APP_ENV=development")
        log.warning("model client is the stub: synthetic answers only")
        return StubModelClient()
    if settings.model_client == "orchestrator":
        return default_orchestrator(pinned, settings.model_roles)
    raise ConfigError("INTELLIGENCE_MODEL_CLIENT must be orchestrator or stub")


# --- the stub's local parts, for the cloud fake (plan Task 10) -------------------------


class _KeywordRetriever:
    """retrieve() over one job's chunks by shared words: no model, no
    download. Only what chunk_visible_to allows for patient_id."""

    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self._chunks = list(chunks)

    def retrieve(self, query: str, top_k: int = 5, patient_id: str | None = None) -> list[Any]:
        from intelligence.knowledge_base.kb_builder import RetrievalResult, chunk_visible_to

        words = set(str(query).lower().split())
        visible = [c for c in self._chunks if chunk_visible_to(c, patient_id)]
        scored = sorted(
            ((len(words & set(c.content.lower().split())), i, c) for i, c in enumerate(visible)),
            key=lambda t: (-t[0], t[1]),
        )
        return [
            RetrievalResult(chunk=c, score=float(s), retrieval_method="keyword", rank=r)
            for r, (s, _, c) in enumerate(scored[:top_k])
        ]


class _StubCCA:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.retriever = _KeywordRetriever(chunks)

    def generate_with_rag(self, *args: Any, **kwargs: Any) -> Any:
        raise RuntimeError("the stub's parts never generate locally")


class _StubHGA:
    """The stub client's fixed verification, as an HGA."""

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        return {
            "trust_score": 0.9,
            "decision": "pass",
            "nli_score": 0.9,
            "coherence_score": 1.0,
            "numerical_score": 0.85,
            "semantic_entropy_score": 0.7,
            "flag_penalty": 0.0,
            "flags": [],
            "weights": {"alpha": 0.35, "beta": 0.25, "gamma": 0.2, "delta": 0.2},
        }


def stub_parts() -> tuple[Builder, dict[str, str]]:
    """A builder whose orchestrator has a keyword retriever over the job's
    chunks and an HGA giving the stub's fixed verification (development
    only, with the stub client; and the cloud-fake tests)."""

    def build(chunks: Sequence[KBChunk]) -> DynamicOrchestratorAgent:
        from intelligence.agents.orchestrator import DynamicOrchestratorAgent

        return DynamicOrchestratorAgent(rag_cca=_StubCCA(chunks), hga=_StubHGA())

    return build, {"dense": "stub", "nli": "stub"}


# --- the router (plan Task 10) -----------------------------------------------------------


class _Unconfigured:
    """A cloud route whose provider this worker hasn't enabled (core and the
    worker's settings disagree): masks with the local masker, and every
    generation escalates as unconfigured, logged and counted. Nothing is
    sent, and nothing runs locally in its place."""

    def __init__(self, local: ModelClient, provider: str) -> None:
        self._local = local
        self._provider = provider

    def mask(self, text: str) -> str:
        return self._local.mask(text)

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[KBChunk],
        *,
        call: CloudCall | None = None,
    ) -> Generation:
        from intelligence.boundary import telemetry

        log.error("cloud.unconfigured provider=%s", self._provider)
        telemetry.record_cloud_alert(self._provider, "unconfigured")
        return Generation(failed_agents=("CCA",), available=False, cloud_outcome="unconfigured")

    def verify(self, generation: Generation) -> Verification:
        return _escalated(generation.failed_agents or ("CCA",))

    def model_versions(self) -> dict[str, str]:
        return {k: v for k, v in self._local.model_versions().items() if k != "cca"}


class ModelRouter:
    """The client for a job's route, decided by core and read from the
    claimed row (never from the message). local is the local client; cloud
    maps an enabled provider to its CloudModelClient."""

    def __init__(self, local: ModelClient, cloud: Mapping[str, ModelClient]) -> None:
        self.local = local
        self._cloud = dict(cloud)

    def client_for(self, route: str) -> tuple[ModelClient, str]:
        """(the client, its provider), "local" for the local route. A cloud
        route with no client here, or any route outside vocab.ROUTES, is
        unconfigured."""
        from intelligence.boundary.cloud.vocab import PROVIDERS, ROUTES

        if route == "local":
            return self.local, "local"
        provider = route[len("cloud:") :] if route in ROUTES else "unknown"
        client = self._cloud.get(provider) if provider in PROVIDERS else None
        if client is None:
            return _Unconfigured(self.local, provider), provider
        return client, provider

    def close(self) -> None:
        """Close each cloud client (its shared transport) once."""
        seen: set[int] = set()
        for c in self._cloud.values():
            transport = getattr(c, "transport", None)
            if transport is None or id(transport) in seen:
                continue
            seen.add(id(transport))
            try:
                c.close()  # type: ignore[attr-defined]
            except Exception as e:
                log.warning("cloud client close failed error=%s", type(e).__name__)


def build_model_router(settings: WorkerSettings, pinned: PinnedSource | None = None) -> ModelRouter:
    """The local client INTELLIGENCE_MODEL_CLIENT names, and one cloud
    client per enabled provider (none by default), retrieving and verifying
    with the same local parts: the orchestrator's, or the stub's in
    development. pinned gives the verified pinned models (None until
    verified)."""
    if not settings.cloud.providers:
        return ModelRouter(build_model_client(settings, pinned), {})
    from intelligence.boundary.cloud.client import build_cloud_clients

    local: ModelClient
    if settings.model_client == "orchestrator":
        source: PinnedSource = pinned if pinned is not None else (lambda: None)
        build, versions = local_parts(source, settings.model_roles)
        local = OrchestratorModelClient(build, versions=versions, pinned=source)
    else:
        local = build_model_client(settings)  # the stub, development only
        build, versions = stub_parts()
    cloud = build_cloud_clients(settings.cloud, build, versions)
    return ModelRouter(local, cloud)
