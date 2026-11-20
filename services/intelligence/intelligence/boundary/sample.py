"""The demo's sample answers: INTELLIGENCE_MODEL_CLIENT=sample.

The public demo runs the real stack (core, NATS, the request tokens, the job
claim, the decision write and the audit facts) before any trained model
exists, so the pipeline worker answers with this client instead of a model.
It is labelled as sample output in every answer, loads no model and makes no
network call:

  mask      the real PIIMasker (rules only).
  generate  the job's own shared guideline chunks, ranked by the words they
            share with the masked question, visible ones only
            (chunk_visible_to); the answer is a fixed sample label, a framing
            sentence and the matching guideline sentences; their chunk ids
            are the recorded citations (shown as sources, never in the text). No chunk matching: a "nothing matched" sample answer
            with no citation, which the guard and the decision treat as any
            other answer.
  verify    an answer that cites no chunk has no evidence: ESCALATE, unverified,
            no trust score, so no confidence is shown (the "needs clinician
            review" outcome, as a cloud answer with insufficient evidence).
            Any other answer goes through the real local verification path
            (_verify_with): the escalation guard (red flags, missing
            information, dose scope, recital) and the question's own
            thresholds. The NLI and embedding models don't fit on the demo
            host (deploy/README.md, "Public demo: sample answers"), so the
            sample HGA's citation check is literal: each sentence of the
            answer must be quoted from a cited chunk. Its other scores are
            fixed sample numbers; the decision follows from the thresholds as
            the real HGA's does, and never PASS with an unsupported sentence.

Where it may run (fail closed, checked in config.WorkerSettings.from_env and
again in build_model_client): APP_ENV=demo or development only, never
production or an unset APP_ENV; never with a cloud provider or pinned model
roles; and only for the hospitals INTELLIGENCE_SAMPLE_HOSPITALS lists (the
demo deploy fills it with the database's is_demo hospitals). A job of any
other hospital is refused before it is claimed (run_query: forbidden), so a
real hospital's question can never get a sample answer.

Nothing here logs question text, patient data or the answer.
"""

from __future__ import annotations

import logging
import re
import threading
import time
from collections.abc import Mapping, Sequence
from typing import TYPE_CHECKING, Any

from intelligence.boundary.models import (
    _CHUNK_ID,
    Generation,
    Verification,
    _chunk_texts,
    _escalated,
    _KeywordRetriever,
    _StubCCA,
    _verify_with,
)

if TYPE_CHECKING:
    from intelligence.boundary.cloud.client import CloudCall
    from intelligence.knowledge_base.kb_builder import KBChunk

log = logging.getLogger("intelligence.boundary.sample")

# Every sample answer starts with this label (mhai-web shows the answer as
# written; the demo banner says the same).
LABEL = "Sample answer (synthetic demo data, not clinical advice):"
MODEL_USED = "sample"
VERSION = "1"
TOP_K = 3
MAX_SENTENCES = 3
# Shared words shorter than this don't count as a match ("of", "the", ...).
_MIN_WORD = 4
_WORD = re.compile(r"[a-z0-9]+")
_SENTENCE = re.compile(r"(?<=[.!?])\s+")


# The real HGA's defaults (agents.hga_nli.HallucinationGuardAgent): its expert
# weights, and the thresholds and penalty when the question's own aren't given.
_WEIGHTS = {"alpha": 0.35, "beta": 0.25, "gamma": 0.2, "delta": 0.2}
_PASS, _ESCALATE, _WARNING_PENALTY = 0.6, 0.3, 0.05
# The sample's fixed scores for the experts it can't run without models.
_COHERENCE, _NUMERICAL, _SE = 0.95, 0.9, 0.72
_FRAMING = "The demo has no trained model yet"


def _claims(response_text: str) -> list[str]:
    """The answer's sentences that state something: not the sample label or
    the framing sentence compose() adds."""
    text = response_text.strip()
    if text.startswith(LABEL):
        text = text[len(LABEL) :]
    return [
        p.strip()
        for p in _SENTENCE.split(text.strip())
        if p.strip() and not p.strip().startswith(_FRAMING)
    ]


class _SampleHGA:
    """The HGA the demo can afford, labelled as such in the decision's model
    versions (``sample``). Built per answer with the texts of the chunks it
    cites. The citation score is the share of the answer's sentences quoted
    word for word from those chunks (an unquoted one is an nli_unsupported
    warning, as in the real HGA); the other experts are fixed sample numbers.
    The trust score and decision are the real HGA's formula; a PASS needs
    every sentence quoted. The escalation guard around it is the real one
    (_verify_with)."""

    def __init__(self, cited_texts: Sequence[str]) -> None:
        self._sources = tuple(" ".join(t.split()) for t in cited_texts)

    def verify(
        self,
        response_text: str,
        *,
        pass_threshold: float | None = None,
        escalate_threshold: float | None = None,
        warning_penalty: float | None = None,
        **kwargs: Any,
    ) -> dict[str, Any]:
        p = _PASS if pass_threshold is None else pass_threshold
        e = _ESCALATE if escalate_threshold is None else escalate_threshold
        w = _WARNING_PENALTY if warning_penalty is None else warning_penalty
        claims = [" ".join(c.split()) for c in _claims(response_text)]
        supported = [any(c in s for s in self._sources) for c in claims]
        nli = sum(supported) / len(claims) if claims else 0.0
        flags = [{"type": "nli_unsupported", "severity": "warning"} for ok in supported if not ok]
        base = (
            _WEIGHTS["alpha"] * nli
            + _WEIGHTS["beta"] * _COHERENCE
            + _WEIGHTS["gamma"] * _NUMERICAL
            + _WEIGHTS["delta"] * _SE
        )
        penalty = len(flags) * w
        trust = max(0.0, base - penalty)
        all_quoted = bool(claims) and all(supported)
        if trust >= p and all_quoted:
            decision = "pass"
        elif trust >= e:
            decision = "regenerate"
        else:
            decision = "escalate"
        return {
            "trust_score": round(trust, 4),
            "decision": decision,
            "nli_score": round(nli, 4),
            "coherence_score": _COHERENCE,
            "numerical_score": _NUMERICAL,
            "semantic_entropy_score": _SE,
            "flag_penalty": round(penalty, 4),
            "flags": flags,
            "weights": dict(_WEIGHTS),
        }


# Question words that say nothing about the topic: a guideline sharing only
# these is not a match ("when ... fasting glucose" for a blood-pressure question).
_STOP = frozenset(
    {
        "what",
        "when",
        "which",
        "where",
        "why",
        "how",
        "does",
        "should",
        "would",
        "could",
        "with",
        "without",
        "from",
        "that",
        "this",
        "these",
        "those",
        "have",
        "been",
        "before",
        "after",
        "about",
        "into",
        "than",
        "then",
        "there",
        "their",
        "your",
        "called",
        "first",
        "patient",
        "patients",
        "synthetic",
        "demo",
        "question",
        "case",
        "treat",
        "treatment",
        "define",
        "defines",
        "defined",
        "readings",
    }
)


def _words(text: str) -> set[str]:
    return {w for w in _WORD.findall(text.lower()) if len(w) >= _MIN_WORD and w not in _STOP}


def _first_sentences(text: str) -> str:
    parts = [p.strip() for p in _SENTENCE.split(text.strip()) if p.strip()]
    return " ".join(parts[:MAX_SENTENCES])


def compose(question: str, chunks: Sequence[KBChunk]) -> tuple[str, tuple[str, ...]]:
    """The sample answer for a (masked) question over the chunks it may use,
    and the ids it cites. Only chunks sharing a real word with the question
    are used; their text is quoted, never changed."""
    q = _words(question)
    ranked = _KeywordRetriever(chunks).retrieve(question, top_k=len(chunks) or 1)
    picked = [
        r.chunk
        for r in ranked
        if _CHUNK_ID.fullmatch(r.chunk.chunk_id) and q & _words(r.chunk.content)
    ][:TOP_K]
    if not picked:
        return (
            f"{LABEL} No reference guideline in the demo library matches this question. "
            "The demo has no trained model yet, so it can only quote its sample guidelines; "
            "try a question about malaria, hypertension, diabetes, jaundice or a cough.",
            (),
        )
    lines = [
        f"{LABEL} The demo has no trained model yet, so this answer quotes the sample "
        "reference guidelines that match your question."
    ]
    for c in picked:
        # No inline chunk ids: the app never shows them; the citations are
        # recorded (cited_chunk_ids) and listed under the answer's sources.
        lines.append(_first_sentences(c.content))
    return " ".join(lines), tuple(c.chunk_id for c in picked)


class SampleModelClient:
    """The demo's sample answers behind ModelClient (module docstring)."""

    def __init__(self, masker: Any = None) -> None:
        if masker is None:
            from intelligence.security.hipaa import PIIMasker

            masker = PIIMasker()
        self._masker = masker
        self._mask_lock = threading.Lock()  # PIIMasker counts detections
        from intelligence.guardrails import escalation_guard

        self._guard_version = escalation_guard.default().version_string

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
        from intelligence.agents.orchestrator import DynamicOrchestratorAgent
        from intelligence.knowledge_base.kb_builder import chunk_visible_to

        started = time.perf_counter()
        visible = [c for c in chunks if chunk_visible_to(c, patient_id)]
        retrieval_ms = (time.perf_counter() - started) * 1000
        started = time.perf_counter()
        try:
            response, cited = compose(question, visible)
        except Exception as e:  # fail closed: escalate, never invent
            log.warning("model stage failed agent=sample error=%s", type(e).__name__)
            return Generation(question=question, failed_agents=("CCA",))
        generation_ms = (time.perf_counter() - started) * 1000
        texts = _chunk_texts(visible)
        hga = _SampleHGA([texts[c] for c in cited if c in texts])
        orch = DynamicOrchestratorAgent(rag_cca=_StubCCA(visible), hga=hga)
        return Generation(
            question=question,
            response=response,
            cited_chunk_ids=cited,
            retrieved_chunk_ids=tuple(
                c.chunk_id for c in visible if _CHUNK_ID.fullmatch(c.chunk_id)
            ),
            model_used=MODEL_USED,
            available=True,
            retrieval_ms=retrieval_ms,
            generation_ms=generation_ms,
            patient_id=patient_id,
            handle=orch,
            patient_context=dict(patient_context),
            chunk_texts=texts,
        )

    def verify(self, generation: Generation) -> Verification:
        if not generation.available:
            return _escalated(generation.failed_agents or ("CCA",))
        v = _verify_with(generation.handle, generation)
        if not generation.cited_chunk_ids:
            # No source, no evidence: needs a clinician, with no score and so
            # no confidence (fail closed). The guard's reasons are kept.
            return _escalated(v.failed_agents, v.guard_reasons, v.guard_rules)
        return v

    def model_versions(self) -> dict[str, str]:
        return {
            "cca": f"{MODEL_USED}-{VERSION}",
            "sample": VERSION,
            "escalation_guard": self._guard_version,
        }
