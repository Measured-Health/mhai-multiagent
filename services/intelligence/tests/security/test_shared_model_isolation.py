"""The worker loads its heavy models once and shares them; no job's chunks are shared.

local_parts' build makes a knowledge base, retriever and HGA per job. The dense model
(dense_factory) and the NLI model (nli_factory) are loaded once per client and reused:
they hold weights and a tokenizer, never a job's chunks, vectors or vocabulary. These
tests pin both halves: the same model instance across jobs, and a second job (another
patient, another hospital) never seeing the first job's chunks or index, also when the
jobs run at once. A model that fails to load escalates the job and is tried again.

Synthetic data only; the sentence-transformers and NLI models are fakes (no download).
"""

from __future__ import annotations

import hashlib
import re
import sys
import threading
import time
import types
from collections.abc import Iterator, Sequence
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import numpy as np
import pytest

from intelligence.agents import answer_profile, hga_nli
from intelligence.agents.hga_nli import NLILabel, NLIResult
from intelligence.boundary import models
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

DIM = 64


@pytest.fixture(autouse=True)
def offline_dense_retriever() -> None:
    """Overrides the package's TF-IDF-only fixture: these tests load a fake dense model
    through the worker's own shared loader."""


class FakeSentenceTransformer:
    """Bag-of-words hash vectors: deterministic, no download. Counts how often it is made."""

    made: list[FakeSentenceTransformer] = []
    fail_next = 0

    def __init__(self, name: str, device: str = "cpu") -> None:
        if FakeSentenceTransformer.fail_next:
            FakeSentenceTransformer.fail_next -= 1
            raise OSError("synthetic load failure")
        self.name = name
        FakeSentenceTransformer.made.append(self)

    @staticmethod
    def _vector(text: str) -> np.ndarray:
        v = np.zeros(DIM, dtype=np.float32)
        for w in re.findall(r"[a-z0-9]+", text.lower()):
            v[int(hashlib.sha256(w.encode()).hexdigest(), 16) % DIM] += 1.0
        n = float(np.linalg.norm(v))
        return v / n if n else v

    def encode(self, texts: Sequence[str], **kwargs: Any) -> np.ndarray:
        return np.stack([self._vector(t) for t in texts]) if texts else np.zeros((0, DIM))

    def get_sentence_embedding_dimension(self) -> int:
        return DIM


class FakeNLI:
    made: list[FakeNLI] = []

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        FakeNLI.made.append(self)

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        return NLIResult(premise[:100], hypothesis[:100], NLILabel.ENTAILMENT, 0.99)


class EchoGenerator:
    """Answers with the prompt it was given, so a test can read exactly what the CCA
    put in front of the model for this job."""

    name = "synthetic-echo"

    def generate(self, system: str, query: str) -> str:
        return query


@pytest.fixture
def fakes(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    FakeSentenceTransformer.made = []
    FakeSentenceTransformer.fail_next = 0
    FakeNLI.made = []
    fake = types.ModuleType("sentence_transformers")
    fake.SentenceTransformer = FakeSentenceTransformer  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "sentence_transformers", fake)
    monkeypatch.setattr(hga_nli, "NLIEntailmentVerifier", FakeNLI)
    yield


def _job(hospital: str, patient: str) -> tuple[str, list[KBChunk]]:
    """One job's chunks. Every job numbers its chunks KB#0001.. as the worker does, so
    ids collide across jobs; only the text (and its marker) tells them apart."""
    pid = f"{hospital}-{patient}"
    marker = f"synmark{hospital}{patient}".lower()
    return pid, [
        KBChunk(
            f"KB#{i:04d}",
            f"Synthetic {marker} note {i}: fever and malaria smear for this patient.",
            "lab",
            pid,
            f"S-{pid}",
        )
        for i in range(1, 6)
    ] + [
        KBChunk(
            "KB#0099",
            "Synthetic guideline: confirm malaria by smear.",
            "guideline",
            REFERENCE_SOURCE,
            "",
        )
    ]


def _marker(pid: str) -> str:
    return "synmark" + pid.replace("-", "").lower()


def _build() -> models.Builder:
    build, _ = models.local_parts(generator=EchoGenerator())
    return build


def _dense(orch: Any) -> Any:
    return orch.rag_cca.retriever.dense


# ------------------------------------------------------------------ shared models


def test_two_jobs_reuse_the_same_dense_and_nli_model(fakes: None) -> None:
    build = _build()
    _, a = _job("h1", "pa")
    _, b = _job("h2", "pb")
    one, two = build(a), build(b)
    assert len(FakeSentenceTransformer.made) == 1
    assert len(FakeNLI.made) == 1
    assert _dense(one).model is _dense(two).model
    assert isinstance(_dense(one).model, models.SharedEncoder)
    assert one.hga.nli_verifier is two.hga.nli_verifier
    # Everything else is the job's own.
    assert one.rag_cca is not two.rag_cca
    assert one.rag_cca.retriever is not two.rag_cca.retriever
    assert _dense(one) is not _dense(two)
    assert one.rag_cca.retriever.kb is not two.rag_cca.retriever.kb
    assert one.hga is not two.hga


def test_the_shared_encoder_holds_no_job_data(fakes: None) -> None:
    build = _build()
    pid, chunks = _job("h1", "pa")
    orch = build(chunks)
    orch.rag_cca.retriever.retrieve("malaria smear", top_k=5, patient_id=pid)
    shared = _dense(orch).model
    assert set(vars(shared)) == {"_model", "_lock"}
    assert set(vars(shared._model)) == {"name"}


# ------------------------------------------------------------------ isolation


def test_a_second_job_never_sees_the_first_jobs_chunks(fakes: None) -> None:
    build = _build()
    pid_a, a = _job("h1", "pa")
    pid_b, b = _job("h2", "pb")
    first = build(a)
    second = build(b)
    # Job B's index is job B's chunks only, and its knowledge base is job B's.
    assert {c.content for c in second.rag_cca.retriever.kb.chunks.values()} == {
        c.content for c in b
    }
    assert _dense(second).chunk_embeddings.shape[0] == len(b)
    # Asking job B's retriever for job A's patient finds nothing of A's (only the
    # shared guideline), and asking for its own patient finds nothing of A's either.
    for patient in (pid_a, pid_b):
        hits = second.rag_cca.retriever.retrieve(
            f"{_marker(pid_a)} malaria", 10, patient_id=patient
        )
        assert all(_marker(pid_a) not in h.chunk.content for h in hits)
    # And job A's own retriever is unchanged by job B.
    hits = first.rag_cca.retriever.retrieve("malaria smear", 10, patient_id=pid_a)
    assert {h.chunk.source_patient_id for h in hits} <= {pid_a, REFERENCE_SOURCE}
    assert any(_marker(pid_a) in h.chunk.content for h in hits)


def test_the_prompt_of_each_job_holds_only_its_own_patient(fakes: None) -> None:
    client = models.OrchestratorModelClient(_build(), masker=object())
    pid_a, a = _job("h1", "pa")
    pid_b, b = _job("h2", "pb")
    ga = client.generate("Synthetic malaria question?", {}, pid_a, a)
    gb = client.generate("Synthetic malaria question?", {}, pid_b, b)
    assert ga.available and gb.available
    assert _marker(pid_a) in ga.response and _marker(pid_b) not in ga.response
    assert _marker(pid_b) in gb.response and _marker(pid_a) not in gb.response
    assert len(FakeSentenceTransformer.made) == 1


def test_concurrent_jobs_of_other_patients_and_hospitals_stay_apart(fakes: None) -> None:
    client = models.OrchestratorModelClient(_build(), masker=object())
    jobs = [_job(f"h{h}", f"p{p}") for h in range(1, 5) for p in range(1, 4)]
    start = threading.Barrier(len(jobs))

    def run(job: tuple[str, list[KBChunk]]) -> tuple[str, str, tuple[str, ...]]:
        pid, chunks = job
        start.wait(timeout=30)
        g = client.generate("Synthetic malaria smear question?", {}, pid, chunks)
        assert g.available
        return pid, g.response, g.retrieved_chunk_ids

    for _ in range(3):
        with ThreadPoolExecutor(max_workers=len(jobs)) as pool:
            results = list(pool.map(run, jobs))
        for pid, response, retrieved in results:
            assert _marker(pid) in response
            others = [_marker(o) for o, _ in jobs if o != pid]
            assert not any(m in response for m in others)
            assert retrieved
    assert len(FakeSentenceTransformer.made) == 1
    assert len(FakeNLI.made) == 1


def test_tfidf_fallback_keeps_each_jobs_vocabulary(
    fakes: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setitem(sys.modules, "sentence_transformers", None)
    build = _build()
    pid_a, a = _job("h1", "pa")
    pid_b, b = _job("h2", "pb")
    one, two = build(a), build(b)
    da, db = _dense(one), _dense(two)
    assert da._loaded is False and db._loaded is False
    assert da._tfidf is not db._tfidf
    assert _marker(pid_a) in da._tfidf.vocabulary_
    assert _marker(pid_a) not in db._tfidf.vocabulary_
    hits = two.rag_cca.retriever.retrieve(_marker(pid_a), 10, patient_id=pid_a)
    assert all(_marker(pid_a) not in h.chunk.content for h in hits)
    assert "sklearn.feature_extraction.text" in sys.modules


# ------------------------------------------------------------------ fail closed


def test_a_model_that_fails_to_load_escalates_and_is_tried_again(fakes: None) -> None:
    FakeSentenceTransformer.fail_next = 1
    client = models.OrchestratorModelClient(_build(), masker=object())
    pid, chunks = _job("h1", "pa")
    failed = client.generate("Synthetic question?", {}, pid, chunks)
    assert failed.available is False
    assert failed.failed_agents == ("CCA",)
    assert client.verify(failed).decision == "ESCALATE"
    ok = client.generate("Synthetic question?", {}, pid, chunks)
    assert ok.available is True
    assert len(FakeSentenceTransformer.made) == 1


def test_shared_encoder_dimension(fakes: None) -> None:
    class NoDim:
        def get_sentence_embedding_dimension(self) -> None:
            return None

    assert models.SharedEncoder(NoDim()).get_sentence_embedding_dimension() is None
    assert (
        models.SharedEncoder(FakeSentenceTransformer("x")).get_sentence_embedding_dimension() == DIM
    )


def test_the_default_profile_build_is_unchanged(fakes: None) -> None:
    build = _build()
    _, chunks = _job("h1", "pa")
    orch = build(chunks, profile=answer_profile.DEFAULT)  # type: ignore[call-arg]
    assert _dense(orch).model is not None


# ------------------------------------------------------------------ the shared NLI model

REAL_NLI = hga_nli.NLIEntailmentVerifier
NLI_LABELS = {0: "contradiction", 1: "neutral", 2: "entailment"}
SUPPORTED = "Confirm malaria by smear [KB#0099]."
UNSUPPORTED = "Give synthetic quinine tonight [KB#0099]."


class CollidingTokenizer:
    """Behaves like a fast (Rust) tokenizer used from two threads at once: the second
    caller gets RuntimeError("Already borrowed"). It holds the call open briefly so
    concurrent jobs overlap."""

    def __init__(self) -> None:
        self._busy = threading.Lock()
        self.collisions = 0

    def __call__(self, premise: str | list[str], hypothesis: str | list[str], **kwargs: Any) -> Any:
        import torch

        if not self._busy.acquire(blocking=False):
            self.collisions += 1
            raise RuntimeError("Already borrowed")
        try:
            time.sleep(0.002)
            # One pair, or a batch of pairs (the verifier scores a chunk's windows at once).
            premises = [premise] if isinstance(premise, str) else premise
            hypotheses = [hypothesis] if isinstance(hypothesis, str) else hypothesis
            flags = []
            for p, h in zip(premises, hypotheses, strict=True):
                words = set(re.findall(r"[a-z0-9]+", p.lower()))
                flags.append(1.0 if set(re.findall(r"[a-z0-9]+", h.lower())) <= words else 0.0)
            return _Inputs(flag=torch.tensor(flags))
        finally:
            self._busy.release()


class _Inputs(dict):  # type: ignore[type-arg]
    def to(self, device: str) -> _Inputs:
        return self


class WordOverlapNLI:
    """Entailment, per pair, when the tokenizer said every hypothesis word is in the
    premise."""

    def __call__(self, flag: Any) -> Any:
        import torch

        rows = [[-5.0, -5.0, 5.0] if f else [-5.0, 5.0, -5.0] for f in flag.tolist()]
        return types.SimpleNamespace(logits=torch.tensor(rows))


class ClaimGenerator:
    name = "synthetic-claims"

    def generate(self, system: str, query: str) -> str:
        return UNSUPPORTED if "unsupported" in query else SUPPORTED


@pytest.fixture
def shared_nli(fakes: None, monkeypatch: pytest.MonkeyPatch) -> CollidingTokenizer:
    """The real NLIEntailmentVerifier (shared by nli_factory) over a fake model whose
    tokenizer fails on concurrent use, as the real fast tokenizer can."""
    pytest.importorskip("torch")
    tokenizer = CollidingTokenizer()

    def load(self: Any, device: str) -> None:
        self.tokenizer, self.model, self.device = tokenizer, WordOverlapNLI(), "cpu"
        self.label_map, self._loaded = dict(NLI_LABELS), True

    monkeypatch.setattr(hga_nli, "NLIEntailmentVerifier", REAL_NLI)
    monkeypatch.setattr(REAL_NLI, "_try_load_model", load)
    return tokenizer


def _verdict(client: models.OrchestratorModelClient, job: int) -> tuple[Any, ...]:
    pid, chunks = _job(f"h{job % 4}", f"p{job}")
    question = "Synthetic unsupported question?" if job % 2 else "Synthetic malaria question?"
    g = client.generate(question, {"age_years": 30}, pid, chunks)
    v = client.verify(g)
    return (v.decision, v.trust_score, v.verified, v.failed_agents, v.flags)


def test_concurrent_jobs_share_the_nli_model_without_colliding(
    shared_nli: CollidingTokenizer,
) -> None:
    client = models.OrchestratorModelClient(
        models.local_parts(generator=ClaimGenerator())[0], masker=object()
    )
    jobs = list(range(12))
    sequential = {j: _verdict(client, j) for j in jobs}
    assert {v[0] for v in sequential.values()} >= {"PASS"}  # the fixture can pass
    assert all(v[2] and "HGA" not in v[3] for v in sequential.values())
    start = threading.Barrier(len(jobs))

    def run(job: int) -> tuple[int, tuple[Any, ...]]:
        start.wait(timeout=30)
        return job, _verdict(client, job)

    for _ in range(3):
        with ThreadPoolExecutor(max_workers=len(jobs)) as pool:
            concurrent = dict(pool.map(run, jobs))
        assert concurrent == sequential
    assert shared_nli.collisions == 0
    assert len(FakeSentenceTransformer.made) == 1


def test_the_nli_verifier_serialises_its_tokenizer(shared_nli: CollidingTokenizer) -> None:
    verifier = REAL_NLI("synthetic-nli", device="cpu")
    start = threading.Barrier(8)

    def run(i: int) -> str:
        start.wait(timeout=30)
        premise = "Synthetic guideline: confirm malaria by smear."
        return verifier.verify(premise, "Confirm malaria by smear.").label.value

    with ThreadPoolExecutor(max_workers=8) as pool:
        labels = list(pool.map(run, range(8)))
    assert labels == ["entailment"] * 8
    assert shared_nli.collisions == 0
