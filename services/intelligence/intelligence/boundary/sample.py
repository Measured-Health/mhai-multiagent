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
  verify    the real local verification path (_verify_with): the escalation
            guard (red flags, missing information, dose scope, recital) with
            the question's own thresholds; only the HGA scores are fixed
            sample numbers, because the NLI and embedding models don't fit on
            the demo host (deploy/README.md, "Public demo: sample answers").

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


class _SampleHGA:
    """The HGA the demo can afford: fixed sample scores, labelled as such in
    the decision's model versions (``sample``). The escalation guard around
    it is the real one (_verify_with)."""

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        return {
            "trust_score": 0.86,
            "decision": "pass",
            "nli_score": 0.88,
            "coherence_score": 0.95,
            "numerical_score": 0.9,
            "semantic_entropy_score": 0.72,
            "flag_penalty": 0.0,
            "flags": [],
            "weights": {"alpha": 0.35, "beta": 0.25, "gamma": 0.2, "delta": 0.2},
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

        visible = [c for c in chunks if chunk_visible_to(c, patient_id)]
        try:
            response, cited = compose(question, visible)
        except Exception as e:  # fail closed: escalate, never invent
            log.warning("model stage failed agent=sample error=%s", type(e).__name__)
            return Generation(question=question, failed_agents=("CCA",))
        orch = DynamicOrchestratorAgent(rag_cca=_StubCCA(visible), hga=_SampleHGA())
        return Generation(
            question=question,
            response=response,
            cited_chunk_ids=cited,
            retrieved_chunk_ids=tuple(
                c.chunk_id for c in visible if _CHUNK_ID.fullmatch(c.chunk_id)
            ),
            model_used=MODEL_USED,
            available=True,
            retrieval_ms=0.0,
            generation_ms=0.0,
            patient_id=patient_id,
            handle=orch,
            patient_context=dict(patient_context),
            chunk_texts=_chunk_texts(visible),
        )

    def verify(self, generation: Generation) -> Verification:
        if not generation.available:
            return _escalated(generation.failed_agents or ("CCA",))
        return _verify_with(generation.handle, generation)

    def model_versions(self) -> dict[str, str]:
        return {
            "cca": f"{MODEL_USED}-{VERSION}",
            "sample": VERSION,
            "escalation_guard": self._guard_version,
        }
