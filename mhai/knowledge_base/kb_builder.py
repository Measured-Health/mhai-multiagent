"""
===========================================================================
MHAI Milestone M3 — Part 1 of 3
===========================================================================
RAG Retrieval Engine + Knowledge Base + Citation-Forcing CCA

Implements:
  - MedicalKnowledgeBase: builds a structured KB from patient JSON records
    with indexed chunks (demographics, encounters, labs, medications, plans)
  - HybridRetriever: dual-pathway retrieval:
      (a) Dense vector search via sentence-transformers (PubMedBERT)
      (b) Sparse BM25 keyword search via rank_bm25
      Combined via Reciprocal Rank Fusion (RRF)
  - CitationForcingTrainer: fine-tunes Qwen-2.5 with an augmented
    objective where the model must produce inline [KB:#id] citations
    linking each claim to a KB chunk
  - RAGClinicalContextAgent: upgraded CCA that retrieves context
    before generation, producing grounded, citation-backed diagnoses

Prerequisites (from M1+M2):
  - Patient JSON records in /content/extract/001-005/
  - Qwen-2.5-7B-Instruct (base or fine-tuned adapter)
  - Instruction pairs from mhai_record_finetuning.py

Requirements:
  pip install torch transformers peft bitsandbytes accelerate
  pip install sentence-transformers rank-bm25 numpy scipy
  pip install scikit-learn faiss-cpu  # or faiss-gpu on Colab
===========================================================================
"""

import os
import json
import glob
import re
import time
import hashlib
import numpy as np
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from collections import defaultdict
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.M3.RAG")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.1  KNOWLEDGE BASE DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# source_patient_id values with special meaning. No real patient may use
# them: "reference" chunks are shown to every patient.
REFERENCE_SOURCE = "reference"
UNASSIGNED_SOURCE = "unassigned"
RESERVED_PATIENT_IDS = frozenset({REFERENCE_SOURCE, UNASSIGNED_SOURCE, "unknown", "ref", ""})


def is_valid_patient_id(patient_id) -> bool:
    """A real patient id: a non-empty string that is not a reserved value."""
    return (isinstance(patient_id, str)
            and patient_id.strip().lower() not in RESERVED_PATIENT_IDS)


def chunk_visible_to(chunk, patient_id) -> bool:
    """Whether a chunk may be shown in a query about patient_id.

    Shared reference guidelines are visible to everyone; any other chunk
    only to the one valid patient it came from.
    """
    source = getattr(chunk, "source_patient_id", None)
    if source == REFERENCE_SOURCE:
        return getattr(chunk, "chunk_type", None) == "guideline"
    return is_valid_patient_id(patient_id) and source == patient_id


@dataclass
class KBChunk:
    """A single indexed chunk in the knowledge base."""
    chunk_id: str                # e.g., "KB#001", "KB#002"
    content: str                 # Textual content
    chunk_type: str              # "demographics", "encounter", "lab", "medication", "diagnosis", "plan", "guideline"
    source_patient_id: str       # Which patient record this came from
    source_subject_id: str       # Which subject within the record
    metadata: Dict[str, Any] = field(default_factory=dict)
    embedding: Optional[np.ndarray] = None  # Dense vector (set after encoding)


@dataclass
class RetrievalResult:
    """Result from a retrieval query."""
    chunk: KBChunk
    score: float                 # Relevance score (0-1)
    retrieval_method: str        # "dense", "sparse", "hybrid"
    rank: int = 0


@dataclass
class RAGContext:
    """Context assembled from retrieval for CCA generation."""
    query: str
    retrieved_chunks: List[RetrievalResult]
    context_text: str            # Formatted context string
    total_tokens_est: int        # Estimated token count
    retrieval_latency_ms: float


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.2  MEDICAL KNOWLEDGE BASE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class MedicalKnowledgeBase:
    """
    Builds and manages a structured knowledge base from patient
    JSON records. Each record is decomposed into typed chunks
    (demographics, encounters, labs, medications, diagnoses, plans)
    with unique IDs for citation linking.

    Also includes a set of medical reference guideline chunks
    for grounding diagnoses against standard-of-care.
    """

    # Standard medical reference guidelines (embedded as KB chunks)
    REFERENCE_GUIDELINES = [
        {
            "content": (
                "Gestational Diabetes Mellitus (GDM) is diagnosed when "
                "fasting glucose >= 92 mg/dL, 1-hour >= 180 mg/dL, or "
                "2-hour >= 153 mg/dL on a 75g OGTT during pregnancy. "
                "Management includes dietary modification (1800-2500 cal), "
                "glucose monitoring, and insulin if targets not met."
            ),
            "type": "guideline",
            "topic": "GDM"
        },
        {
            "content": (
                "Malaria diagnosis requires parasitological confirmation "
                "by microscopy or rapid diagnostic test (RDT). Severe "
                "malaria features include impaired consciousness, "
                "severe anemia (Hb < 5 g/dL), respiratory distress, "
                "convulsions, prostration, or parasitemia > 10%. "
                "Treatment: ACT (artemisinin-based combination therapy) "
                "for uncomplicated, IV artesunate for severe cases."
            ),
            "type": "guideline",
            "topic": "malaria"
        },
        {
            "content": (
                "Hypertension is defined as systolic BP >= 140 mmHg or "
                "diastolic BP >= 90 mmHg on two separate occasions. "
                "Stage 1: 140-159/90-99. Stage 2: >= 160/100. "
                "First-line treatment: thiazide diuretics, ACE inhibitors, "
                "ARBs, or calcium channel blockers."
            ),
            "type": "guideline",
            "topic": "hypertension"
        },
        {
            "content": (
                "Neonatal jaundice is common in the first week of life. "
                "Physiological jaundice: onset > 24 hours, peaks day 3-5, "
                "total bilirubin < 15 mg/dL in term infants. "
                "Pathological jaundice: onset < 24 hours, total bilirubin "
                "> 17 mg/dL, or conjugated bilirubin > 2 mg/dL. "
                "Phototherapy threshold depends on age and risk factors."
            ),
            "type": "guideline",
            "topic": "neonatal_jaundice"
        },
        {
            "content": (
                "Upper Respiratory Tract Infection (URTI): Viral in "
                "majority of cases. Symptoms include cough, rhinorrhea, "
                "sore throat, low-grade fever. Treatment is supportive: "
                "paracetamol/ibuprofen for fever, adequate hydration. "
                "Antibiotics only if bacterial superinfection suspected "
                "(high fever > 5 days, purulent discharge, otitis media)."
            ),
            "type": "guideline",
            "topic": "URTI"
        },
        {
            "content": (
                "Type 2 Diabetes Mellitus: Diagnosed when fasting glucose "
                ">= 126 mg/dL, 2-hour OGTT >= 200 mg/dL, HbA1c >= 6.5%, "
                "or random glucose >= 200 mg/dL with symptoms. First-line: "
                "Metformin 500mg BID, titrate to 2000mg. Add sulfonylurea "
                "or insulin if HbA1c > 9% despite metformin."
            ),
            "type": "guideline",
            "topic": "T2DM"
        },
    ]

    def __init__(self):
        self.chunks: Dict[str, KBChunk] = {}  # chunk_id -> KBChunk
        self._next_id = 1

    def _generate_id(self) -> str:
        """Generate sequential KB chunk ID."""
        chunk_id = f"KB#{self._next_id:04d}"
        self._next_id += 1
        return chunk_id

    def build_from_patient_files(
        self,
        base_dir: str = "/content/extract",
        max_files: int = 500
    ) -> int:
        """
        Build KB from patient JSON files.
        Each subject's data is decomposed into typed chunks.
        """
        json_paths = sorted(
            glob.glob(os.path.join(base_dir, "**", "*.json"), recursive=True)
        )[:max_files]

        total_chunks = 0
        for path in json_paths:
            try:
                with open(path, 'r', encoding='utf-8') as f:
                    data = json.load(f)
                if isinstance(data, list):
                    data = data[0] if data else {}

                patient_id = data.get("patient_id")
                if not is_valid_patient_id(patient_id):
                    # Pooling id-less records under one placeholder would let
                    # unrelated patients see each other's data.
                    logger.warning(f"Skipping {path}: missing or reserved patient_id")
                    continue
                for subject in data.get("subjects", []):
                    n = self._index_subject(subject, patient_id)
                    total_chunks += n
            except Exception as e:
                logger.warning(f"Failed to parse {path}: {e}")

        # Add reference guidelines
        for guideline in self.REFERENCE_GUIDELINES:
            chunk_id = self._generate_id()
            self.chunks[chunk_id] = KBChunk(
                chunk_id=chunk_id,
                content=guideline["content"],
                chunk_type="guideline",
                source_patient_id=REFERENCE_SOURCE,
                source_subject_id=REFERENCE_SOURCE,
                metadata={"topic": guideline["topic"]}
            )
            total_chunks += 1

        logger.info(
            f"KB built: {total_chunks} chunks from "
            f"{len(json_paths)} patient files + "
            f"{len(self.REFERENCE_GUIDELINES)} reference guidelines"
        )
        return total_chunks

    def _index_subject(self, subject: Dict, patient_id: str) -> int:
        """Decompose a subject record into typed KB chunks."""
        subject_id = subject.get("subject_id", "unknown")
        n = 0

        # Demographics chunk
        demographics = subject.get("demographics", {})
        if demographics:
            age = demographics.get("age_years") or demographics.get("age_months")
            sex = demographics.get("sex", "")
            age_str = f"{age} years" if demographics.get("age_years") else f"{age} months"
            content = (
                f"Patient {subject_id}: {age_str} {sex}, "
                f"relationship: {subject.get('relationship', 'unknown')}."
            )
            self._add_chunk(content, "demographics", patient_id, subject_id)
            n += 1

        # Encounter chunks (one per encounter)
        for enc in subject.get("encounters", []):
            year = enc.get("encounter_year", "")
            month = enc.get("encounter_month", "")
            complaint = enc.get("complaint", "")
            enc_type = enc.get("type", "visit")
            vitals = enc.get("vital_signs", {})
            vital_str = ", ".join(f"{k}={v}" for k, v in vitals.items() if v)

            content = (
                f"Encounter ({year}/{month:02d} if isinstance(month, int) else {year}): "
                f"{enc_type} - {complaint}."
            ).replace("if isinstance(month, int) else", "")
            # Safe date formatting
            try:
                date_part = f"{year}/{int(month):02d}" if year and month else str(year)
            except (ValueError, TypeError):
                date_part = str(year) if year else "unknown"

            content = f"Encounter ({date_part}): {enc_type} - {complaint}."
            if vital_str:
                content += f" Vitals: {vital_str}."

            self._add_chunk(content, "encounter", patient_id, subject_id,
                            {"year": year, "month": month, "type": enc_type})
            n += 1

        # Diagnosis chunks
        for dx in subject.get("diagnosis", []):
            name = dx.get("name", "")
            icd10 = dx.get("icd10", "")
            content = f"Diagnosis: {name}"
            if icd10:
                content += f" [ICD-10: {icd10}]"
            content += "."
            self._add_chunk(content, "diagnosis", patient_id, subject_id,
                            {"icd10": icd10})
            n += 1

        # Lab result chunks
        for lab in subject.get("lab_results", []):
            test = lab.get("test", "")
            value = lab.get("value", "")
            unit = lab.get("unit", "") or ""
            ref = lab.get("reference", "")
            comment = lab.get("comment", "")
            content = f"Lab: {test} = {value} {unit}"
            if ref:
                content += f" (reference: {ref})"
            if comment:
                content += f" [{comment}]"
            content += "."
            self._add_chunk(content, "lab", patient_id, subject_id,
                            {"test": test, "value": value})
            n += 1

        # Medication chunks
        for med in subject.get("medications", []):
            name = med.get("name", "")
            dose = med.get("dose", "")
            route = med.get("route", "")
            indication = med.get("indication", "")
            content = f"Medication: {name}"
            if dose:
                content += f" {dose}"
            if route:
                content += f" {route}"
            if indication:
                content += f" for {indication}"
            content += "."
            self._add_chunk(content, "medication", patient_id, subject_id)
            n += 1

        # Plan chunk
        plan = subject.get("plan", "")
        if plan and len(plan) > 10:
            self._add_chunk(f"Plan: {plan}", "plan", patient_id, subject_id)
            n += 1

        return n

    def _add_chunk(self, content: str, chunk_type: str,
                   patient_id: str, subject_id: str,
                   metadata: Dict = None):
        """Add a chunk to the KB."""
        chunk_id = self._generate_id()
        self.chunks[chunk_id] = KBChunk(
            chunk_id=chunk_id,
            content=content,
            chunk_type=chunk_type,
            source_patient_id=patient_id,
            source_subject_id=subject_id,
            metadata=metadata or {}
        )

    def get_chunk(self, chunk_id: str) -> Optional[KBChunk]:
        """Retrieve a chunk by ID."""
        return self.chunks.get(chunk_id)

    def search_by_type(self, chunk_type: str) -> List[KBChunk]:
        """Get all chunks of a given type."""
        return [c for c in self.chunks.values() if c.chunk_type == chunk_type]

    def get_patient_chunks(self, patient_id: str) -> List[KBChunk]:
        """Get all chunks for a specific patient."""
        return [c for c in self.chunks.values()
                if c.source_patient_id == patient_id]

    def get_all_texts(self) -> List[Tuple[str, str]]:
        """Return (chunk_id, content) pairs for indexing."""
        return [(c.chunk_id, c.content) for c in self.chunks.values()]

    def stats(self) -> Dict[str, int]:
        """Return chunk counts by type."""
        counts = defaultdict(int)
        for c in self.chunks.values():
            counts[c.chunk_type] += 1
        return dict(counts)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.3  HYBRID RETRIEVER: DENSE + SPARSE + RRF
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class DenseRetriever:
    """
    Dense vector retrieval using sentence-transformers.
    Encodes chunks and queries into dense vectors, retrieves
    via cosine similarity (or FAISS for scalability).

    Model: PubMedBERT or all-MiniLM-L6-v2 (lightweight fallback)
    """

    def __init__(
        self,
        model_name: str = "sentence-transformers/all-MiniLM-L6-v2",
        device: str = "cpu"
    ):
        self.model_name = model_name
        self.device = device
        self.model = None
        self.chunk_ids: List[str] = []
        self.chunk_embeddings: Optional[np.ndarray] = None
        self._loaded = False

        self._try_load_model()

    def _try_load_model(self):
        """Load sentence-transformer model."""
        try:
            from sentence_transformers import SentenceTransformer
            self.model = SentenceTransformer(self.model_name, device=self.device)
            self._loaded = True
            logger.info(f"Dense retriever loaded: {self.model_name}")
        except ImportError:
            logger.warning("sentence-transformers not available; using TF-IDF fallback")
            self._loaded = False

    def index(self, chunks: List[Tuple[str, str]]):
        """
        Index a list of (chunk_id, content) pairs.

        Args:
            chunks: List of (chunk_id, text_content) tuples
        """
        self.chunk_ids = [cid for cid, _ in chunks]
        texts = [text for _, text in chunks]

        if self._loaded and self.model is not None:
            logger.info(f"Encoding {len(texts)} chunks with {self.model_name}...")
            self.chunk_embeddings = self.model.encode(
                texts, show_progress_bar=True,
                batch_size=32, normalize_embeddings=True
            )
        else:
            # TF-IDF fallback
            from sklearn.feature_extraction.text import TfidfVectorizer
            self._tfidf = TfidfVectorizer(max_features=5000, stop_words='english')
            self.chunk_embeddings = self._tfidf.fit_transform(texts).toarray()

        logger.info(f"Indexed {len(self.chunk_ids)} chunks "
                     f"(dim={self.chunk_embeddings.shape[1]})")

    def retrieve(self, query: str, top_k: int = 10) -> List[Tuple[str, float]]:
        """
        Retrieve top-k most similar chunks for a query.

        Returns: List of (chunk_id, similarity_score) tuples
        """
        if self.chunk_embeddings is None:
            return []

        if self._loaded and self.model is not None:
            query_emb = self.model.encode(
                [query], normalize_embeddings=True
            )[0]
        else:
            query_emb = self._tfidf.transform([query]).toarray()[0]

        # Cosine similarity (embeddings are already normalized)
        scores = self.chunk_embeddings @ query_emb
        top_indices = np.argsort(scores)[::-1][:top_k]

        results = [
            (self.chunk_ids[i], float(scores[i]))
            for i in top_indices
        ]
        return results


class SparseRetriever:
    """
    Sparse keyword-based retrieval using BM25.

    BM25(q, d) = sum_{t in q} IDF(t) * (tf(t,d) * (k1+1)) /
                 (tf(t,d) + k1 * (1 - b + b * |d|/avgdl))
    """

    def __init__(self, k1: float = 1.5, b: float = 0.75):
        self.k1 = k1
        self.b = b
        self.bm25 = None
        self.chunk_ids: List[str] = []

    def index(self, chunks: List[Tuple[str, str]]):
        """Index chunks for BM25 retrieval."""
        self.chunk_ids = [cid for cid, _ in chunks]
        corpus = [text.lower().split() for _, text in chunks]

        try:
            from rank_bm25 import BM25Okapi
            self.bm25 = BM25Okapi(corpus, k1=self.k1, b=self.b)
            logger.info(f"BM25 indexed {len(self.chunk_ids)} chunks")
        except ImportError:
            logger.warning("rank_bm25 not available; using simple TF fallback")
            # Simple term-frequency fallback
            self._corpus = corpus
            self._doc_freq = defaultdict(int)
            for doc in corpus:
                for term in set(doc):
                    self._doc_freq[term] += 1
            self._n_docs = len(corpus)

    def retrieve(self, query: str, top_k: int = 10) -> List[Tuple[str, float]]:
        """Retrieve top-k via BM25."""
        tokens = query.lower().split()

        if self.bm25 is not None:
            scores = self.bm25.get_scores(tokens)
        else:
            # Simple TF-IDF fallback
            scores = np.zeros(len(self.chunk_ids))
            for i, doc in enumerate(self._corpus):
                for token in tokens:
                    tf = doc.count(token)
                    if tf > 0 and token in self._doc_freq:
                        idf = np.log(self._n_docs / (self._doc_freq[token] + 1))
                        scores[i] += tf * idf

        top_indices = np.argsort(scores)[::-1][:top_k]
        results = [
            (self.chunk_ids[i], float(scores[i]))
            for i in top_indices if scores[i] > 0
        ]
        return results


class HybridRetriever:
    """
    Hybrid retrieval combining dense vector search and sparse BM25
    via Reciprocal Rank Fusion (RRF).

    RRF(d) = sum_{r in rankers} 1 / (k + rank_r(d))

    where k=60 is the standard RRF constant. This is the retrieval
    strategy used by RAGFlow and recommended for medical IR.
    """

    def __init__(
        self,
        dense_model: str = "sentence-transformers/all-MiniLM-L6-v2",
        rrf_k: int = 60,
        dense_weight: float = 0.6,
        sparse_weight: float = 0.4,
        device: str = "cpu"
    ):
        self.dense = DenseRetriever(dense_model, device)
        self.sparse = SparseRetriever()
        self.rrf_k = rrf_k
        self.dense_weight = dense_weight
        self.sparse_weight = sparse_weight
        self.kb: Optional[MedicalKnowledgeBase] = None

    def index_kb(self, kb: MedicalKnowledgeBase):
        """Index all KB chunks for both retrievers."""
        self.kb = kb
        all_texts = kb.get_all_texts()
        logger.info(f"Indexing {len(all_texts)} KB chunks...")

        self.dense.index(all_texts)
        self.sparse.index(all_texts)

    def retrieve(
        self,
        query: str,
        top_k: int = 5,
        chunk_types: Optional[List[str]] = None,
        patient_id: Optional[str] = None,
    ) -> List[RetrievalResult]:
        """
        Hybrid retrieval with RRF fusion.

        Args:
            query: Clinical query text
            top_k: Number of results to return
            chunk_types: Optional filter by chunk type
            patient_id: Patient whose records may be returned. Shared
                reference guidelines are always eligible; other patients'
                records never are. With no patient_id, only reference
                guidelines are returned.

        Returns:
            List of RetrievalResult sorted by RRF score
        """
        t_start = time.time()

        # Retrieve from both pathways (get more candidates for fusion).
        # Patient scoping discards most of the index, so rank all of it
        # rather than letting other patients' chunks crowd out top_k.
        n_candidates = len(self.dense.chunk_ids) or top_k * 3
        dense_results = self.dense.retrieve(query, n_candidates)
        sparse_results = self.sparse.retrieve(query, n_candidates)

        # Compute RRF scores
        rrf_scores = defaultdict(float)

        for rank, (chunk_id, score) in enumerate(dense_results):
            rrf_scores[chunk_id] += (
                self.dense_weight / (self.rrf_k + rank + 1)
            )

        for rank, (chunk_id, score) in enumerate(sparse_results):
            rrf_scores[chunk_id] += (
                self.sparse_weight / (self.rrf_k + rank + 1)
            )

        # Sort by RRF score
        sorted_ids = sorted(
            rrf_scores.items(), key=lambda x: x[1], reverse=True
        )

        # Build results with chunk objects
        results = []
        for rank, (chunk_id, score) in enumerate(sorted_ids):
            chunk = self.kb.get_chunk(chunk_id) if self.kb else None
            if chunk is None:
                continue

            if not self._visible_to(chunk, patient_id):
                continue

            # Apply chunk type filter
            if chunk_types and chunk.chunk_type not in chunk_types:
                continue

            results.append(RetrievalResult(
                chunk=chunk,
                score=score,
                retrieval_method="hybrid",
                rank=rank
            ))

            if len(results) >= top_k:
                break

        latency = (time.time() - t_start) * 1000

        logger.debug(
            f"Retrieved {len(results)} chunks in {latency:.1f}ms "
            f"(dense={len(dense_results)}, sparse={len(sparse_results)})"
        )

        return results

    _visible_to = staticmethod(chunk_visible_to)

    def build_context(
        self,
        query: str,
        top_k: int = 5,
        max_context_chars: int = 3000,
        patient_id: Optional[str] = None,
    ) -> RAGContext:
        """
        Build a formatted context string for CCA generation.

        The context includes chunk IDs for citation linking:
          [KB#0001] Diagnosis: GDM [ICD-10: O24.4].
          [KB#0002] Lab: 2hr glucose = 168 mg/dL [HIGH].
          ...
        """
        t_start = time.time()
        results = self.retrieve(query, top_k=top_k, patient_id=patient_id)

        # Format context with citation IDs
        context_lines = ["RETRIEVED CLINICAL EVIDENCE:"]
        total_chars = 0

        for r in results:
            line = f"[{r.chunk.chunk_id}] {r.chunk.content}"
            if total_chars + len(line) > max_context_chars:
                break
            context_lines.append(line)
            total_chars += len(line)

        context_text = "\n".join(context_lines)
        latency = (time.time() - t_start) * 1000

        return RAGContext(
            query=query,
            retrieved_chunks=results,
            context_text=context_text,
            total_tokens_est=total_chars // 4,  # rough estimate
            retrieval_latency_ms=latency
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.4  CITATION-FORCING INSTRUCTION PAIR GENERATOR
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class CitationForcingPairGenerator:
    """
    Generates instruction-tuning pairs where the assistant response
    includes inline [KB:#id] citations linking each claim to a
    specific KB chunk. This trains the CCA to produce grounded,
    verifiable outputs.

    Example output format:
      "The patient has Gestational Diabetes Mellitus [KB#0023].
       2-hour post-prandial glucose was 168 mg/dL, elevated [KB#0045].
       Dietary management with 2000 cal diet is recommended [KB#0089]."
    """

    SYSTEM_PROMPT_CITATION = (
        "You are a clinical decision support AI with access to a "
        "medical knowledge base. For EVERY clinical claim you make, "
        "you MUST cite the supporting evidence using [KB#XXXX] tags. "
        "Each citation links to a specific knowledge base entry. "
        "Claims without citations will be flagged for review.\n\n"
        "Format:\n"
        "  1. Diagnosis [ICD-10: code] [KB#XXXX]\n"
        "     Reasoning: explanation citing [KB#XXXX]\n"
        "  2. ...\n\n"
        "MANAGEMENT PLAN:\n"
        "  - Recommendation [KB#XXXX]\n\n"
        "RED FLAGS:\n"
        "  - Warning [KB#XXXX]"
    )

    def __init__(self, kb: MedicalKnowledgeBase):
        self.kb = kb

    def generate_pairs(
        self,
        patient_files: List[Dict],
        max_pairs: int = 500
    ) -> List[Dict[str, str]]:
        """
        Generate citation-forcing instruction pairs.

        For each subject:
        1. Build clinical presentation (user prompt) — same as M1
        2. Retrieve relevant KB chunks for that subject
        3. Build citation-annotated diagnosis response (assistant)
        """
        pairs = []

        for pf in patient_files:
            for subject in pf.get("subjects", []):
                encounters = subject.get("encounters", [])
                diagnoses = subject.get("diagnosis", [])
                if not encounters:
                    continue

                patient_id = pf.get("patient_id")
                subject_id = subject.get("subject_id", "unknown")
                if not is_valid_patient_id(patient_id):
                    continue

                # Get this subject's KB chunks (subject ids repeat across patients)
                subject_chunks = [
                    c for c in self.kb.chunks.values()
                    if c.source_patient_id == patient_id
                    and c.source_subject_id == subject_id
                ]

                if not subject_chunks:
                    continue

                # Build user prompt (clinical presentation)
                user_prompt = self._build_presentation(subject)

                # Build citation-annotated response
                assistant_response = self._build_cited_response(
                    subject, subject_chunks
                )

                # Build context from KB chunks
                context = "\n".join(
                    f"[{c.chunk_id}] {c.content}"
                    for c in subject_chunks[:15]
                )

                pairs.append({
                    "system": self.SYSTEM_PROMPT_CITATION,
                    "context": context,
                    "user": user_prompt,
                    "assistant": assistant_response,
                    "patient_id": patient_id,
                    "subject_id": subject_id,
                    "n_citations": assistant_response.count("[KB#"),
                })

                if len(pairs) >= max_pairs:
                    break
            if len(pairs) >= max_pairs:
                break

        logger.info(
            f"Generated {len(pairs)} citation-forcing pairs "
            f"(avg {np.mean([p['n_citations'] for p in pairs]):.1f} citations/pair)"
        )
        return pairs

    def _build_presentation(self, subject: Dict) -> str:
        """Build clinical presentation (same format as M1)."""
        demographics = subject.get("demographics", {})
        age = demographics.get("age_years") or demographics.get("age_months", "?")
        sex = demographics.get("sex", "")

        lines = [f"Patient: {age} {'years' if demographics.get('age_years') else 'months'} {sex}"]

        for enc in subject.get("encounters", [])[:5]:
            complaint = enc.get("complaint", "")
            lines.append(f"  Visit: {complaint}")

        for lab in subject.get("lab_results", [])[:5]:
            test = lab.get("test", "")
            value = lab.get("value", "")
            unit = lab.get("unit", "") or ""
            comment = lab.get("comment", "")
            lab_line = f"  Lab: {test} = {value} {unit}"
            if comment:
                lab_line += f" [{comment}]"
            lines.append(lab_line)

        lines.append("\nProvide diagnosis with ICD-10 codes, citing evidence from the knowledge base.")
        return "\n".join(lines)

    def _build_cited_response(
        self,
        subject: Dict,
        chunks: List[KBChunk]
    ) -> str:
        """Build response with inline citations to KB chunks."""
        lines = ["ASSESSMENT AND DIAGNOSES:"]

        # Map chunk types for easy lookup
        dx_chunks = [c for c in chunks if c.chunk_type == "diagnosis"]
        lab_chunks = [c for c in chunks if c.chunk_type == "lab"]
        med_chunks = [c for c in chunks if c.chunk_type == "medication"]
        plan_chunks = [c for c in chunks if c.chunk_type == "plan"]
        guideline_chunks = [
            c for c in self.kb.chunks.values()
            if c.chunk_type == "guideline"
        ]

        # Diagnoses with citations
        for i, dx_chunk in enumerate(dx_chunks, 1):
            lines.append(f"\n{i}. {dx_chunk.content} [{dx_chunk.chunk_id}]")

            # Find supporting lab evidence
            for lab_chunk in lab_chunks:
                comment = lab_chunk.metadata.get("value", "")
                lines.append(
                    f"   Evidence: {lab_chunk.content} [{lab_chunk.chunk_id}]"
                )
                break  # One supporting lab per diagnosis

            # Find matching guideline
            dx_text = dx_chunk.content.lower()
            for g_chunk in guideline_chunks:
                topic = g_chunk.metadata.get("topic", "").lower()
                if topic and any(kw in dx_text for kw in topic.split("_")):
                    lines.append(
                        f"   Guideline: {g_chunk.content[:100]}... [{g_chunk.chunk_id}]"
                    )
                    break

        # Management plan with citations
        lines.append("\n\nMANAGEMENT PLAN:")
        for plan_chunk in plan_chunks[:2]:
            lines.append(f"  {plan_chunk.content} [{plan_chunk.chunk_id}]")

        for med_chunk in med_chunks[:3]:
            lines.append(f"  {med_chunk.content} [{med_chunk.chunk_id}]")

        lines.append(
            "\n\nNOTE: All claims are linked to knowledge base entries. "
            "Claims without [KB#XXXX] citations require verification."
        )

        return "\n".join(lines)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.5  RAG-ENHANCED CCA
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class RAGClinicalContextAgent:
    """
    Upgraded CCA that retrieves context from the KB before
    generating a diagnosis. Produces citation-backed outputs.

    Pipeline:
      Query -> HybridRetriever -> Context Assembly -> Qwen Generation
      -> Citation Extraction -> NLI Verification (in Part 2)
    """

    def __init__(
        self,
        retriever: HybridRetriever,
        cca_model=None,
        cca_tokenizer=None,
        max_new_tokens: int = 512,
        temperature: float = 0.3,
    ):
        self.retriever = retriever
        self.model = cca_model
        self.tokenizer = cca_tokenizer
        self.max_new_tokens = max_new_tokens
        self.temperature = temperature
        self._model_loaded = cca_model is not None

    def generate_with_rag(
        self,
        clinical_query: str,
        top_k: int = 5,
        triage_context: Optional[Dict] = None,
        patient_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        RAG-enhanced diagnosis generation.

        1. Retrieve relevant KB chunks
        2. Assemble context with citation IDs
        3. Generate with Qwen (or rule-based fallback)
        4. Parse citations from output
        """
        t_start = time.time()

        # Step 1: Retrieve
        rag_context = self.retriever.build_context(
            clinical_query, top_k=top_k, patient_id=patient_id
        )

        # Step 2: Build full prompt
        system_prompt = CitationForcingPairGenerator.SYSTEM_PROMPT_CITATION

        augmented_query = (
            f"{rag_context.context_text}\n\n"
            f"CLINICAL QUERY:\n{clinical_query}"
        )

        if triage_context:
            augmented_query += (
                f"\n\nTRIAGE:\n"
                f"  Category: {triage_context.get('disease_category', '')}\n"
                f"  Risk: {triage_context.get('risk_score', '')}"
            )

        # Step 3: Generate
        if self._model_loaded and self.model is not None:
            response = self._generate_with_model(system_prompt, augmented_query)
        else:
            response = self._generate_fallback(
                clinical_query, rag_context, triage_context
            )

        # Step 4: Parse citations
        citations = re.findall(r'\[KB#(\d{4})\]', response)

        latency = (time.time() - t_start) * 1000

        return {
            "response": response,
            "citations": citations,
            "n_citations": len(citations),
            "retrieved_chunks": [
                {"id": r.chunk.chunk_id, "content": r.chunk.content[:100],
                 "score": r.score, "type": r.chunk.chunk_type}
                for r in rag_context.retrieved_chunks
            ],
            "retrieval_latency_ms": rag_context.retrieval_latency_ms,
            "total_latency_ms": latency,
            "model_used": "qwen" if self._model_loaded else "rule_based"
        }

    def _generate_with_model(self, system: str, query: str) -> str:
        """Generate using fine-tuned Qwen model."""
        import torch
        messages = [
            {"role": "system", "content": system},
            {"role": "user", "content": query}
        ]
        text = self.tokenizer.apply_chat_template(
            messages, tokenize=False, add_generation_prompt=True
        )
        inputs = self.tokenizer(text, return_tensors="pt").to(self.model.device)
        with torch.no_grad():
            outputs = self.model.generate(
                **inputs, max_new_tokens=self.max_new_tokens,
                temperature=self.temperature, top_p=0.9,
                do_sample=True, repetition_penalty=1.1,
                pad_token_id=self.tokenizer.pad_token_id
            )
        generated = outputs[0][inputs["input_ids"].shape[1]:]
        return self.tokenizer.decode(generated, skip_special_tokens=True).strip()

    def _generate_fallback(
        self,
        query: str,
        rag_context: RAGContext,
        triage_context: Optional[Dict]
    ) -> str:
        """Rule-based fallback with KB citations."""
        lines = ["ASSESSMENT AND DIAGNOSES:"]
        query_lower = query.lower()

        # Use retrieved chunks to build response
        dx_chunks = [
            r for r in rag_context.retrieved_chunks
            if r.chunk.chunk_type == "diagnosis"
        ]
        lab_chunks = [
            r for r in rag_context.retrieved_chunks
            if r.chunk.chunk_type == "lab"
        ]
        guideline_chunks = [
            r for r in rag_context.retrieved_chunks
            if r.chunk.chunk_type == "guideline"
        ]

        # Include diagnoses from retrieved chunks
        for i, r in enumerate(dx_chunks[:5], 1):
            lines.append(f"\n{i}. {r.chunk.content} [{r.chunk.chunk_id}]")

        # If no diagnosis chunks retrieved, use keyword matching
        if not dx_chunks:
            if any(kw in query_lower for kw in ['malaria', 'fever', 'chills']):
                lines.append("\n1. Suspected Malaria [ICD-10: B54]")
            elif any(kw in query_lower for kw in ['diabetes', 'glucose', 'gdm']):
                lines.append("\n1. Diabetes Mellitus [ICD-10: E11.9]")
            elif any(kw in query_lower for kw in ['chest pain', 'cardiac']):
                lines.append("\n1. Acute Coronary Syndrome [ICD-10: I21.9]")
            else:
                lines.append("\n1. Condition under evaluation [ICD-10: R69]")

        # Add lab evidence with citations
        if lab_chunks:
            lines.append("\n\nSUPPORTING EVIDENCE:")
            for r in lab_chunks[:3]:
                lines.append(f"  {r.chunk.content} [{r.chunk.chunk_id}]")

        # Add guideline references
        if guideline_chunks:
            lines.append("\n\nGUIDELINE REFERENCES:")
            for r in guideline_chunks[:2]:
                lines.append(
                    f"  {r.chunk.content[:120]}... [{r.chunk.chunk_id}]"
                )

        lines.append(
            "\n\nNOTE: All claims cite knowledge base entries. "
            "Clinician verification required."
        )

        return "\n".join(lines)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.6  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_m3_part1():
    """Demonstrate RAG retrieval + citation-forcing CCA."""
    print("=" * 70)
    print("MHAI M3 Part 1: RAG Retrieval + Citation-Forcing CCA")
    print("=" * 70)

    # ── Step 1: Build Knowledge Base ──
    print("\n[1/4] Building Medical Knowledge Base...")
    kb = MedicalKnowledgeBase()

    base_dir = "/content/extract"
    if os.path.exists(base_dir):
        n_chunks = kb.build_from_patient_files(base_dir)
    else:
        # Demo mode: build from reference guidelines only
        print("  Patient data not found -- using reference guidelines only")
        for guideline in MedicalKnowledgeBase.REFERENCE_GUIDELINES:
            chunk_id = kb._generate_id()
            kb.chunks[chunk_id] = KBChunk(
                chunk_id=chunk_id, content=guideline["content"],
                chunk_type="guideline", source_patient_id="reference",
                source_subject_id="reference",
                metadata={"topic": guideline["topic"]}
            )
        n_chunks = len(kb.chunks)

    print(f"  Total chunks: {n_chunks}")
    print(f"  By type: {kb.stats()}")

    # ── Step 2: Index for retrieval ──
    print("\n[2/4] Indexing KB for hybrid retrieval...")
    retriever = HybridRetriever(device="cpu")
    retriever.index_kb(kb)

    # ── Step 3: Test retrieval ──
    print("\n[3/4] Testing retrieval queries...")
    test_queries = [
        "28-year-old female with gestational diabetes and elevated glucose",
        "11-month infant with high fever and positive malaria test",
        "61-year-old woman with elevated blood pressure 140/90",
    ]

    for query in test_queries:
        print(f"\n  Query: {query[:60]}...")
        context = retriever.build_context(query, top_k=3)
        print(f"  Retrieved {len(context.retrieved_chunks)} chunks "
              f"({context.retrieval_latency_ms:.1f}ms)")
        for r in context.retrieved_chunks[:3]:
            print(f"    [{r.chunk.chunk_id}] ({r.chunk.chunk_type}) "
                  f"score={r.score:.4f}: {r.chunk.content[:60]}...")

    # ── Step 4: RAG-enhanced generation ──
    print("\n[4/4] RAG-enhanced CCA generation...")
    rag_cca = RAGClinicalContextAgent(retriever=retriever)

    result = rag_cca.generate_with_rag(
        "28-year-old female with 2hr post-prandial glucose 168 mg/dL, "
        "trace urinary glucose, currently pregnant. Evaluate for GDM.",
        top_k=5
    )

    print(f"\n  Response ({len(result['response'])} chars, "
          f"{result['n_citations']} citations):")
    print(f"  {'--' * 25}")
    for line in result['response'].split('\n')[:15]:
        print(f"  {line}")
    print(f"\n  Citations: {result['citations']}")
    print(f"  Retrieval: {result['retrieval_latency_ms']:.1f}ms")
    print(f"  Total: {result['total_latency_ms']:.1f}ms")
    print(f"  Model: {result['model_used']}")

    print("\n" + "=" * 70)
    print("M3 Part 1 complete. Proceed to Part 2 for NLI entailment")
    print("verifier (DeBERTa-v3) + citation verification pipeline?")
    print("=" * 70)


if __name__ == "__main__":
    demo_m3_part1()