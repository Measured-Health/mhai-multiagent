"""The NLI model judges a claim without its citation tags, on every answer route.

On the pinned gate NLI model a claim copied from its chunk scores entailed (0.993), and the
same claim with " [KB#0002]" appended scores neutral (1.000): the tag reads as new,
unsupported content. The verifier now removes the exact tags from the NLI hypothesis only;
which chunk a claim cites, and every citation check, still run on the original text.

:class:`TagSensitiveNLI` behaves like that model: a hypothesis whose words are all in the
premise is entailed, anything else (a tag left in, or a claim the chunk doesn't hold) is
neutral. Every text here is synthetic.
"""

from __future__ import annotations

import re
from collections.abc import Sequence
from typing import Any

import pytest

from intelligence.agents import answer_profile
from intelligence.agents.hga_nli import (
    CitationVerificationPipeline,
    NLILabel,
    NLIResult,
    VerificationAction,
)
from intelligence.boundary import kb_repo
from intelligence.boundary import models as models_mod
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.client import CloudCall, CloudModelClient, CloudReply
from intelligence.boundary.cloud.reply import validate_answer
from intelligence.boundary.cloud.scan import Identifiers
from intelligence.boundary.cloud.settings import ProviderSettings
from intelligence.boundary.models import Generation, _verify_with
from intelligence.knowledge_base import kb_builder
from intelligence.knowledge_base.kb_builder import (
    REFERENCE_SOURCE,
    KBChunk,
    RetrievalResult,
    strip_citation_tags,
)
from tests.boundary.cloud.conftest import BASE_URL, FAKE_KEY, MODEL, CapturingTransport, FakeAdapter
from tests.boundary.cloud.test_client import FakeBudget

pytestmark = pytest.mark.disable_socket

MALARIA = (
    "Malaria requires parasitological confirmation by microscopy or RDT. "
    "Severe features: Hb < 5, respiratory distress, parasitemia > 10%."
)
URTI = "URTI: viral majority. Supportive: paracetamol, hydration. Antibiotics only if bacterial superinfection."
CLAIM = "Malaria requires parasitological confirmation by microscopy or RDT"
QUESTION = "Synthetic question: how is malaria confirmed?"


class TagSensitiveNLI:
    """The pinned NLI model's behaviour on tags: entailed when every word of the hypothesis
    is in the premise, neutral otherwise. Records each hypothesis it is given."""

    def __init__(self) -> None:
        self.hypotheses: list[str] = []

    @staticmethod
    def _words(s: str) -> set[str]:
        return set(re.findall(r"[a-z0-9]+", s.lower()))

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        self.hypotheses.append(hypothesis)
        if self._words(hypothesis) <= self._words(premise):
            return NLIResult(premise[:100], hypothesis[:100], NLILabel.ENTAILMENT, 0.993)
        return NLIResult(premise[:100], hypothesis[:100], NLILabel.NEUTRAL, 1.0)


def _shared(cid: str, text: str) -> KBChunk:
    return KBChunk(cid, text, "guideline", REFERENCE_SOURCE, "")


class _KB:
    def __init__(self, chunks: Sequence[KBChunk]) -> None:
        self.chunks = {c.chunk_id: c for c in chunks}

    def get_chunk(self, cid: str) -> KBChunk | None:
        return self.chunks.get(cid)


def _pipeline(*chunks: KBChunk) -> tuple[CitationVerificationPipeline, TagSensitiveNLI]:
    nli = TagSensitiveNLI()
    kb = _KB(chunks or (_shared("KB#0002", MALARIA), _shared("KB#0005", URTI)))
    return CitationVerificationPipeline(nli, kb), nli  # type: ignore[arg-type]


# ---------------------------------------------------------------- the shared function


def test_strip_citation_tags_removes_exact_tags_only() -> None:
    assert strip_citation_tags(f"{CLAIM} [KB#0002].") == f"{CLAIM}."
    assert strip_citation_tags("A [KB#0001] [KB#1234567] b.") == "A b."
    # Lookalikes are not citations: they stay, and are judged as text.
    for text in ("x [KB#12] y", "x [KB#0123456] y", "x KB#0001 y", "x [kb#0001] y"):
        assert strip_citation_tags(text) == text


def test_the_profile_uses_the_shared_function() -> None:
    """One implementation: the trained model's profile has no NLI wrapper of its own."""
    assert not hasattr(answer_profile, "CitationFreeNLI")
    assert not hasattr(answer_profile, "wrap_nli")


# ---------------------------------------------------------------- the citation pipeline


def test_a_cited_entailed_claim_is_entailed() -> None:
    pipe, nli = _pipeline()
    report = pipe.verify_response(f"{CLAIM} [KB#0002].")
    (r,) = report.citation_results
    assert r.action == VerificationAction.PASS
    assert nli.hypotheses == [f"{CLAIM}."]
    # What clinicians see is unchanged: the claim keeps its tag, the source its text.
    assert r.claim_text == f"{CLAIM} [KB#0002]."
    assert r.citation_id == "KB#0002"
    assert r.source_text == MALARIA[:150]
    assert report.nli_pass_rate == 1.0


def test_an_uncited_claim_still_fails() -> None:
    pipe, nli = _pipeline()
    (r,) = pipe.verify_response(f"{CLAIM}.").citation_results
    assert r.action == VerificationAction.FLAG
    assert r.citation_id == "NONE"
    assert nli.hypotheses == []


def test_a_claim_citing_a_chunk_that_does_not_support_it_still_fails() -> None:
    pipe, nli = _pipeline()
    (r,) = pipe.verify_response(f"{CLAIM} [KB#0005].").citation_results
    assert r.action == VerificationAction.FLAG
    assert r.citation_id == "KB#0005"
    assert nli.hypotheses == [f"{CLAIM}."]


@pytest.mark.parametrize(
    "text",
    [
        f"{CLAIM} [KB#0099].",  # unknown id
        f"{CLAIM} [KB#1234567].",  # unknown reference library id
    ],
)
def test_an_unknown_citation_still_fails_the_citation_check(text: str) -> None:
    pipe, nli = _pipeline()
    (r,) = pipe.verify_response(text).citation_results
    assert r.action == VerificationAction.FLAG
    assert "not found" in r.reason
    assert nli.hypotheses == []


@pytest.mark.parametrize(
    "text",
    [
        f"{CLAIM} [KB#12].",  # a lookalike tag
        f"{CLAIM} KB#0002.",  # not bracketed
        f"{CLAIM} [kb#0002].",  # not exact
    ],
)
def test_a_forged_tag_is_not_a_citation(text: str) -> None:
    pipe, nli = _pipeline()
    (r,) = pipe.verify_response(text).citation_results
    assert r.action == VerificationAction.FLAG
    assert r.citation_id == "NONE"
    assert nli.hypotheses == []


def test_another_patients_record_is_still_redacted() -> None:
    other = KBChunk("KB#0007", MALARIA, "lab", "patient-b", "s1")
    pipe, nli = _pipeline(other)
    (r,) = pipe.verify_response(f"{CLAIM} [KB#0007].", patient_id="patient-a").citation_results
    assert r.action == VerificationAction.REDACT
    assert nli.hypotheses == []


def test_a_patient_chunk_posing_as_a_shared_reference_is_still_refused() -> None:
    # A reference source must be a guideline; anything else is visible to nobody.
    forged = KBChunk("KB#0008", MALARIA, "lab", REFERENCE_SOURCE, "")
    pipe, nli = _pipeline(forged)
    (r,) = pipe.verify_response(f"{CLAIM} [KB#0008].").citation_results
    assert r.action == VerificationAction.REDACT
    assert nli.hypotheses == []


# ---------------------------------------------------------------- every route


class _Retriever:
    def __init__(self, dense_model: str = "", device: str = "cpu") -> None:
        self.chunks: list[KBChunk] = []

    def index_kb(self, kb: Any) -> None:
        self.chunks = list(kb.chunks.values())

    def retrieve(
        self, query: str, top_k: int = 5, patient_id: str | None = None
    ) -> list[RetrievalResult]:
        return [RetrievalResult(c, 1.0, "fake", i) for i, c in enumerate(self.chunks[:top_k])]


class _Generator:
    name = "synthetic-local"


@pytest.fixture
def worker_build(monkeypatch: pytest.MonkeyPatch) -> tuple[Any, list[TagSensitiveNLI]]:
    """The worker's own builder (local_parts), with the tag-sensitive NLI model and no
    retrieval model: the local and the cloud route verify with it."""
    made: list[TagSensitiveNLI] = []

    def factory(*args: Any, **kwargs: Any) -> Any:
        def make() -> TagSensitiveNLI:
            made.append(TagSensitiveNLI())
            return made[-1]

        return make

    monkeypatch.setattr(models_mod, "nli_factory", factory)
    monkeypatch.setattr(kb_builder, "HybridRetriever", _Retriever)
    build, _ = models_mod.local_parts(generator=_Generator())
    return build, made


def _sealed(cid: str, text: str) -> KBChunk:
    return kb_repo._chunk((cid, True, text, "guideline", None, None, {}))


def _assert_grounded(v: Any) -> None:
    assert v.verified is True
    assert v.scores["nli"] == 1.0
    assert ("nli_unsupported", "warning") not in v.flags
    assert v.decision == "PASS"


def test_the_local_default_profile_scores_a_cited_claim_as_entailed(
    worker_build: tuple[Any, list[TagSensitiveNLI]],
) -> None:
    build, made = worker_build
    orch = build([_shared("KB#0002", MALARIA)], profile=answer_profile.DEFAULT)
    gen = Generation(question=QUESTION, response=f"{CLAIM} [KB#0002].", available=True)
    _assert_grounded(_verify_with(orch, gen))
    assert made[-1].hypotheses == [f"{CLAIM}."]


def test_the_local_default_profile_still_fails_an_uncited_claim(
    worker_build: tuple[Any, list[TagSensitiveNLI]],
) -> None:
    build, _ = worker_build
    orch = build([_shared("KB#0002", MALARIA)], profile=answer_profile.DEFAULT)
    v = _verify_with(orch, Generation(question=QUESTION, response=f"{CLAIM}.", available=True))
    assert v.scores["nli"] == 0.0
    assert ("nli_unsupported", "warning") in v.flags


def test_the_cloud_route_scores_a_cited_claim_as_entailed(
    worker_build: tuple[Any, list[TagSensitiveNLI]],
) -> None:
    build, made = worker_build
    chunks = [_sealed("KB#0002", MALARIA), _sealed("KB#0005", URTI)]
    sent = frozenset(c.chunk_id for c in chunks)
    answer = validate_answer(
        {"answer": f"{CLAIM} [KB#0002].", "citations": ["KB#0002"], "insufficient_evidence": False},
        sent,
    )
    adapter = FakeAdapter([CloudReply(answer, "synthetic-2026-11-01", 120, 40, "ok")])
    client = CloudModelClient(
        ProviderSettings("anthropic", MODEL, ApiKey(FAKE_KEY), None, BASE_URL),
        adapter,
        build,
        {"cca": "local-gen", "dense": "dense-1", "nli": "nli-1"},
        CapturingTransport(),
        sleep=lambda s: None,
    )
    call = CloudCall(Identifiers(frozenset()), FakeBudget(), 1e12)
    gen = client.generate(client.mask(QUESTION), {}, None, chunks, call=call)
    assert gen.cloud_outcome == "ok"
    # The tag reaches the clinician unchanged; only the NLI hypothesis loses it.
    assert gen.response == f"{CLAIM} [KB#0002]."
    _assert_grounded(client.verify(gen))
    assert made[-1].hypotheses == [f"{CLAIM}."]
