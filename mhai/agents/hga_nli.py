"""
===========================================================================
MHAI Milestone M3 — Part 2 of 3
===========================================================================
NLI Entailment Verifier + Citation Verification Pipeline
+ Semantic Entropy for RAG + NLI-Backed HGA

Implements:
  - NLIEntailmentVerifier: DeBERTa-v3-large fine-tuned on MultiNLI/MedNLI
    for pairwise (premise, hypothesis) → {entailment, neutral, contradiction}
  - CitationVerificationPipeline: extracts [KB#XXXX] from CCA output,
    looks up each cited KB chunk, runs NLI verification, produces
    PASS/FLAG/REDACT decisions per claim
  - SemanticEntropyVerifier: samples K responses from CCA, clusters
    by bidirectional entailment, computes H_SE — flags confabulation
    when K responses are semantically inconsistent
  - NLIBackedHGA: integrates real NLI verification into the trust score
    alongside the upgraded numerical/coherence/fabrication checkers
    from the M2 HGA

Prerequisites (from M1+M2):
  - MedicalKnowledgeBase with indexed chunks (Part 1)
  - RAGClinicalContextAgent (Part 1)
  - UpgradedHallucinationGuardAgent (from M2 code)

Requirements:
  pip install torch transformers sentence-transformers scipy numpy
  pip install scikit-learn
===========================================================================
"""

import os
import re
import time
import numpy as np
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from enum import Enum
from collections import defaultdict
import logging

from mhai.knowledge_base.kb_builder import chunk_visible_to
from mhai.utils.nli import DEFAULT_NLI_MODEL, nli_label_map

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.M3.NLI")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.1  NLI DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLILabel(Enum):
    ENTAILMENT = "entailment"
    NEUTRAL = "neutral"
    CONTRADICTION = "contradiction"


class VerificationAction(Enum):
    PASS = "pass"             # Claim entailed by source
    FLAG = "flag"             # Claim neutral (unsupported but not contradicted)
    REDACT = "redact"         # Claim contradicts source


@dataclass
class NLIResult:
    """Result of a single NLI inference."""
    premise: str
    hypothesis: str
    label: NLILabel
    confidence: float          # Softmax probability of predicted label
    logits: Dict[str, float] = field(default_factory=dict)


@dataclass
class CitationVerificationResult:
    """Result of verifying a single cited claim."""
    claim_text: str
    citation_id: str           # e.g., "KB#0023"
    source_text: str           # The KB chunk content
    nli_result: NLIResult
    action: VerificationAction
    reason: str


@dataclass
class FullVerificationReport:
    """Complete verification report for a CCA response."""
    response_text: str
    n_claims: int
    n_citations_found: int
    n_uncited_claims: int
    # Per-claim results
    citation_results: List[CitationVerificationResult]
    # Aggregate scores
    nli_pass_rate: float       # Fraction of claims that passed NLI
    nli_flag_rate: float       # Fraction flagged (neutral)
    nli_redact_rate: float     # Fraction redacted (contradiction)
    # Redacted response
    redacted_response: str
    # Semantic entropy (if computed)
    semantic_entropy: Optional[float] = None
    se_is_confabulation: bool = False


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.2  NLI ENTAILMENT VERIFIER (DeBERTa-v3)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLIEntailmentVerifier:
    """
    NLI-based claim verifier using DeBERTa-v3-large fine-tuned on
    MultiNLI (and optionally MedNLI for clinical domain).

    Given a (premise, hypothesis) pair:
      premise = KB chunk content (the "source of truth")
      hypothesis = generated claim from CCA

    Outputs: {entailment, neutral, contradiction} with confidence.

    DeBERTa-v3 label mapping:
      0 = contradiction
      1 = neutral
      2 = entailment
    """

    def __init__(
        self,
        model_name: str = DEFAULT_NLI_MODEL,
        device: str = "auto",
        confidence_threshold: float = 0.7
    ):
        self.model_name = model_name
        self.confidence_threshold = confidence_threshold
        self.model = None
        self.tokenizer = None
        self._loaded = False
        self.label_map: Dict[int, str] = {}

        self._try_load_model(device)

    def _try_load_model(self, device: str):
        """Load DeBERTa-v3-large for NLI."""
        try:
            import torch
            from transformers import AutoModelForSequenceClassification, AutoTokenizer

            logger.info(f"Loading NLI model: {self.model_name}")
            self.tokenizer = AutoTokenizer.from_pretrained(self.model_name)
            self.model = AutoModelForSequenceClassification.from_pretrained(
                self.model_name
            )

            if device == "auto":
                self.device = "cuda" if torch.cuda.is_available() else "cpu"
            else:
                self.device = device

            self.model = self.model.to(self.device)
            self.model.eval()
            self.label_map = nli_label_map(self.model)
            self._loaded = True
            logger.info(f"NLI model loaded on {self.device}")

        except Exception as e:
            logger.warning(f"NLI model loading failed: {e}")
            logger.info("Using rule-based NLI fallback")
            self._loaded = False

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        """
        Run NLI inference on a (premise, hypothesis) pair.

        Args:
            premise: Source text (KB chunk)
            hypothesis: Claim to verify

        Returns:
            NLIResult with label and confidence
        """
        if self._loaded and self.model is not None:
            return self._verify_with_model(premise, hypothesis)
        else:
            return self._verify_fallback(premise, hypothesis)

    def _verify_with_model(self, premise: str, hypothesis: str) -> NLIResult:
        """Run DeBERTa-v3 NLI inference."""
        import torch

        inputs = self.tokenizer(
            premise, hypothesis,
            return_tensors="pt",
            max_length=512,
            truncation=True,
            padding=True
        ).to(self.device)

        with torch.no_grad():
            outputs = self.model(**inputs)
            logits = outputs.logits[0]
            probs = torch.softmax(logits, dim=-1)

        pred_idx = probs.argmax().item()
        label = NLILabel(self.label_map[pred_idx])
        confidence = probs[pred_idx].item()

        return NLIResult(
            premise=premise[:100],
            hypothesis=hypothesis[:100],
            label=label,
            confidence=confidence,
            logits={name: probs[i].item() for i, name in self.label_map.items()},
        )

    def _verify_fallback(self, premise: str, hypothesis: str) -> NLIResult:
        """
        Rule-based NLI fallback when DeBERTa is unavailable.

        Uses keyword overlap + contradiction detection heuristics.
        """
        premise_lower = premise.lower()
        hypothesis_lower = hypothesis.lower()

        # Extract key terms
        premise_words = set(
            w for w in premise_lower.split()
            if len(w) > 3 and w.isalpha()
        )
        hypothesis_words = set(
            w for w in hypothesis_lower.split()
            if len(w) > 3 and w.isalpha()
        )

        overlap = len(premise_words & hypothesis_words)
        total = max(len(hypothesis_words), 1)
        overlap_ratio = overlap / total

        # Check for explicit contradiction signals
        contradiction_signals = [
            # Negation mismatch
            ('not ' in hypothesis_lower and 'not ' not in premise_lower
             and overlap_ratio > 0.3),
            # Number contradiction (different numbers for same concept)
            self._detect_number_contradiction(premise_lower, hypothesis_lower),
        ]

        if any(contradiction_signals):
            label = NLILabel.CONTRADICTION
            confidence = 0.75
        elif overlap_ratio > 0.4:
            label = NLILabel.ENTAILMENT
            confidence = 0.5 + 0.4 * overlap_ratio
        elif overlap_ratio > 0.15:
            label = NLILabel.NEUTRAL
            confidence = 0.6
        else:
            label = NLILabel.NEUTRAL
            confidence = 0.5

        return NLIResult(
            premise=premise[:100],
            hypothesis=hypothesis[:100],
            label=label,
            confidence=confidence,
            logits={"entailment": 0.0, "neutral": 0.0, "contradiction": 0.0}
        )

    def _detect_number_contradiction(self, premise: str, hypothesis: str) -> bool:
        """Detect if premise and hypothesis state different numbers for same concept."""
        # Extract (keyword, number) pairs
        pattern = r'(\w+)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mg|g|ml|%|mmHg|bpm)?'

        premise_nums = {}
        for match in re.finditer(pattern, premise):
            premise_nums[match.group(1)] = float(match.group(2))

        for match in re.finditer(pattern, hypothesis):
            keyword = match.group(1)
            value = float(match.group(2))
            if keyword in premise_nums:
                ratio = max(value, premise_nums[keyword]) / max(min(value, premise_nums[keyword]), 0.01)
                if ratio > 2.0:  # More than 2x different
                    return True

        return False

    def check_bidirectional_entailment(
        self,
        text_a: str,
        text_b: str
    ) -> bool:
        """
        Check if two texts share meaning via bidirectional entailment.
        Used for semantic entropy clustering.

        Two texts share meaning iff:
          NLI(A→B) = ENTAILMENT AND NLI(B→A) = ENTAILMENT
        """
        forward = self.verify(text_a, text_b)
        if forward.label != NLILabel.ENTAILMENT:
            return False
        backward = self.verify(text_b, text_a)
        return backward.label == NLILabel.ENTAILMENT


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.3  CITATION VERIFICATION PIPELINE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class CitationVerificationPipeline:
    """
    Extracts [KB#XXXX] citations from CCA output, looks up the
    cited KB chunk, and runs NLI verification on each claim.

    Produces per-claim PASS/FLAG/REDACT decisions and a redacted
    version of the response where contradicted claims are replaced.

    Decision logic:
      NLI = ENTAILMENT  (conf > 0.7) → PASS
      NLI = NEUTRAL     (conf > 0.5) → FLAG (unsupported)
      NLI = CONTRADICTION (conf > 0.5) → REDACT
      No citation found                → FLAG (uncited)
    """

    def __init__(
        self,
        nli_verifier: NLIEntailmentVerifier,
        kb,  # MedicalKnowledgeBase from Part 1
        entailment_threshold: float = 0.7,
        contradiction_threshold: float = 0.5
    ):
        self.nli = nli_verifier
        self.kb = kb
        self.entailment_thresh = entailment_threshold
        self.contradiction_thresh = contradiction_threshold

    def verify_response(
        self,
        response_text: str,
        patient_id: Optional[str] = None,
    ) -> FullVerificationReport:
        """
        Verify all citations in a CCA response.

        Steps:
          1. Split response into sentences/claims
          2. Extract [KB#XXXX] citations from each claim
          3. Look up cited KB chunk
          4. Run NLI: source chunk (premise) vs. claim (hypothesis)
          5. Assign PASS/FLAG/REDACT
          6. Build redacted response
        """
        # Step 1: Split into claims (sentence-level)
        claims = self._extract_claims(response_text)

        # Step 2-5: Verify each claim
        results = []
        n_uncited = 0

        for claim_text, citation_ids in claims:
            if not citation_ids:
                # Uncited claim — flag for review
                n_uncited += 1
                results.append(CitationVerificationResult(
                    claim_text=claim_text,
                    citation_id="NONE",
                    source_text="",
                    nli_result=NLIResult(
                        premise="", hypothesis=claim_text[:100],
                        label=NLILabel.NEUTRAL, confidence=0.0
                    ),
                    action=VerificationAction.FLAG,
                    reason="No citation provided — claim unverifiable"
                ))
                continue

            # Verify against each cited source
            for cid in citation_ids:
                chunk = self.kb.get_chunk(cid) if self.kb else None
                if chunk is None:
                    results.append(CitationVerificationResult(
                        claim_text=claim_text,
                        citation_id=cid,
                        source_text="",
                        nli_result=NLIResult(
                            premise="", hypothesis=claim_text[:100],
                            label=NLILabel.NEUTRAL, confidence=0.0
                        ),
                        action=VerificationAction.FLAG,
                        reason=f"Citation {cid} not found in KB"
                    ))
                    continue

                if not chunk_visible_to(chunk, patient_id):
                    # Never show or reason from another patient's record.
                    results.append(CitationVerificationResult(
                        claim_text=claim_text,
                        citation_id=cid,
                        source_text="",
                        nli_result=NLIResult(
                            premise="", hypothesis=claim_text[:100],
                            label=NLILabel.NEUTRAL, confidence=0.0
                        ),
                        action=VerificationAction.REDACT,
                        reason=f"Citation {cid} is not this patient's record"
                    ))
                    continue

                # Run NLI
                nli_result = self.nli.verify(
                    premise=chunk.content,
                    hypothesis=claim_text
                )

                # Decision logic
                if (nli_result.label == NLILabel.ENTAILMENT
                    and nli_result.confidence >= self.entailment_thresh):
                    action = VerificationAction.PASS
                    reason = "Claim entailed by source"
                elif (nli_result.label == NLILabel.CONTRADICTION
                      and nli_result.confidence >= self.contradiction_thresh):
                    action = VerificationAction.REDACT
                    reason = f"Claim CONTRADICTS source {cid}"
                else:
                    action = VerificationAction.FLAG
                    reason = (
                        f"Claim not clearly supported "
                        f"({nli_result.label.value}, "
                        f"conf={nli_result.confidence:.2f})"
                    )

                results.append(CitationVerificationResult(
                    claim_text=claim_text,
                    citation_id=cid,
                    source_text=chunk.content[:150],
                    nli_result=nli_result,
                    action=action,
                    reason=reason
                ))

        # Step 6: Build redacted response
        redacted = self._build_redacted_response(response_text, results)

        # Compute aggregate rates
        n_total = max(len(results), 1)
        n_pass = sum(1 for r in results if r.action == VerificationAction.PASS)
        n_flag = sum(1 for r in results if r.action == VerificationAction.FLAG)
        n_redact = sum(1 for r in results if r.action == VerificationAction.REDACT)

        return FullVerificationReport(
            response_text=response_text,
            n_claims=len(claims),
            n_citations_found=sum(len(cids) for _, cids in claims),
            n_uncited_claims=n_uncited,
            citation_results=results,
            nli_pass_rate=n_pass / n_total,
            nli_flag_rate=n_flag / n_total,
            nli_redact_rate=n_redact / n_total,
            redacted_response=redacted
        )

    def _extract_claims(
        self,
        text: str
    ) -> List[Tuple[str, List[str]]]:
        """
        Split text into (claim_text, [citation_ids]) pairs.

        Each sentence or numbered point is a claim.
        Citations are extracted via [KB#XXXX] regex.
        """
        # Split on sentence boundaries and numbered items
        raw_claims = re.split(r'(?<=[.!?])\s+|\n\s*\d+\.\s*', text)

        results = []
        for claim in raw_claims:
            claim = claim.strip()
            if len(claim) < 10:
                continue

            # Skip section headers
            if claim.isupper() and len(claim) < 50:
                continue
            if claim.startswith("NOTE:") or claim.startswith("---"):
                continue

            # Extract citation IDs
            citation_ids = re.findall(r'\[KB#(\d{4})\]', claim)
            # Normalize to full format
            citation_ids = [f"KB#{cid}" for cid in citation_ids]

            results.append((claim, citation_ids))

        return results

    def _build_redacted_response(
        self,
        original: str,
        results: List[CitationVerificationResult]
    ) -> str:
        """Replace contradicted claims with redaction notices."""
        redacted = original
        for r in results:
            if r.action == VerificationAction.REDACT:
                # Find and replace the claim in the response
                claim_escaped = re.escape(r.claim_text[:80])
                replacement = (
                    f"[REDACTED: Claim contradicts source {r.citation_id}. "
                    f"Original claim removed for safety.]"
                )
                # Try to match a portion of the claim
                if r.claim_text[:60] in redacted:
                    redacted = redacted.replace(
                        r.claim_text[:60],
                        replacement[:60] + "...",
                        1
                    )
        return redacted


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.4  SEMANTIC ENTROPY FOR RAG RESPONSES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class SemanticEntropyVerifier:
    """
    Semantic entropy (Farquhar et al., Nature 2024) adapted for
    RAG-grounded responses.

    Samples K responses from CCA with the SAME retrieval context,
    clusters by bidirectional NLI entailment, and computes:

      H_SE = -sum_j p(C_j) log2 p(C_j)

    High H_SE (> tau_SE) indicates the model confabulates even
    with grounding context — a strong hallucination signal.

    For RAG, the threshold is LOWER than base LLM (tau=0.8 vs 1.2)
    because grounded responses should be MORE consistent.
    """

    def __init__(
        self,
        nli_verifier: NLIEntailmentVerifier,
        n_samples: int = 5,
        threshold: float = 0.8,  # Lower for RAG (should be more consistent)
    ):
        self.nli = nli_verifier
        self.n_samples = n_samples
        self.threshold = threshold

    def compute_entropy(
        self,
        responses: List[str]
    ) -> Tuple[float, int, bool]:
        """
        Compute semantic entropy over K sampled responses.

        Args:
            responses: List of K generated responses (same query+context)

        Returns:
            (entropy_bits, n_clusters, is_confabulation)
        """
        if len(responses) < 2:
            return 0.0, 1, False

        # Cluster by bidirectional entailment
        clusters = []
        for resp in responses:
            assigned = False
            for cluster in clusters:
                if self.nli.check_bidirectional_entailment(
                    cluster["representative"], resp
                ):
                    cluster["members"].append(resp)
                    assigned = True
                    break
            if not assigned:
                clusters.append({
                    "representative": resp,
                    "members": [resp]
                })

        n_clusters = len(clusters)

        # Compute entropy (uniform probability per member)
        total = sum(len(c["members"]) for c in clusters)
        probs = [len(c["members"]) / total for c in clusters]

        from scipy.stats import entropy as scipy_entropy
        h_se = float(scipy_entropy(probs, base=2))

        is_confab = h_se > self.threshold

        logger.debug(
            f"Semantic entropy: H={h_se:.3f}, "
            f"{n_clusters} clusters from {len(responses)} samples, "
            f"confab={is_confab}"
        )

        return h_se, n_clusters, is_confab

    def verify_with_sampling(
        self,
        rag_cca,  # RAGClinicalContextAgent from Part 1
        query: str,
        top_k: int = 5,
        patient_id: Optional[str] = None,
    ) -> Tuple[float, int, bool, List[str]]:
        """
        Sample K responses from the RAG CCA and compute entropy.
        """
        responses = []
        for i in range(self.n_samples):
            result = rag_cca.generate_with_rag(
                query, top_k=top_k, patient_id=patient_id
            )
            responses.append(result["response"])

        h_se, n_clusters, is_confab = self.compute_entropy(responses)
        return h_se, n_clusters, is_confab, responses


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.5  NLI-BACKED HGA (UPGRADED WITH REAL VERIFICATION)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLIBackedHGA:
    """
    The full M3 Hallucination Guard Agent integrating:
      - Expert 1: NLI citation verification (DeBERTa-v3)
      - Expert 2: Coherence + garbled text detection (from M2)
      - Expert 3: Context-aware numerical accuracy (from M2)
      - Expert 4 (NEW): Semantic entropy consistency check

    Trust score:
      T(y) = alpha * NLI_score + beta * Coh_score
             + gamma * Num_score + delta * SE_score
             - flag_penalty

    where delta weights the semantic entropy signal.
    """

    # Fabrication markers (from M2 upgraded HGA)
    FABRICATION_MARKERS = [
        "fictionol", "fictional", "fakemed",
        "discovered in 2028", "discovered in 2029",
        "100% efficacy", "zero side effects",
        "no known side effects", "universal cure",
    ]

    # Contradiction patterns (from M2)
    CONTRADICTION_PATTERNS = [
        (r'(?:malaria|plasmodium).*(?:positive|confirmed|detected)',
         r'(?:malaria|plasmodium).*(?:negative|not\s+detected|no\s+treatment)'),
        (r'(?:is|are)\s+recommended',
         r'(?:is|are)\s+not\s+recommended'),
        (r'(?:confirmed|diagnosed|positive)',
         r'no\s+treatment\s+needed'),
    ]

    # Context-aware numerical patterns (from M2)
    NUMERICAL_PATTERNS = [
        (r'(?:HR|heart\s*rate|pulse)\s*[:\-=]?\s*(\d+)', "heart_rate", 20, 250),
        (r'(?:SpO2|O2\s*sat)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*%?', "spo2", 40, 100),
        (r'(?:troponin\s*[IT]?)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "troponin", 0, 100),
        (r'(?:glucose|blood\s*sugar|FBS)\s*[:\-=]?\s*(\d+)', "glucose", 20, 800),
        (r'(?:hemoglobin|Hgb|Hb)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "hemoglobin", 2, 25),
        (r'(?:platelet|PLT)\s*[:\-=]?\s*(\d+)', "platelets", 5, 1500),
        (r'(?:bilirubin|bili)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "bilirubin", 0, 50),
        (r'(?:metformin)\s*(\d+)\s*mg', "metformin_dose", 100, 3000),
    ]

    def __init__(
        self,
        nli_verifier: Optional[NLIEntailmentVerifier] = None,
        citation_pipeline: Optional[CitationVerificationPipeline] = None,
        se_verifier: Optional[SemanticEntropyVerifier] = None,
        # Weights
        alpha: float = 0.35,     # NLI citation verification
        beta: float = 0.25,      # Coherence + fabrication
        gamma: float = 0.20,     # Numerical accuracy
        delta: float = 0.20,     # Semantic entropy
        # Thresholds
        pass_threshold: float = 0.55,
        escalate_threshold: float = 0.3,
        # Penalty
        critical_flag_penalty: float = 0.20,
        warning_flag_penalty: float = 0.05,
    ):
        self.nli_verifier = nli_verifier
        self.citation_pipeline = citation_pipeline
        self.se_verifier = se_verifier

        self.alpha = alpha
        self.beta = beta
        self.gamma = gamma
        self.delta = delta
        self.pass_threshold = pass_threshold
        self.escalate_threshold = escalate_threshold
        self.critical_penalty = critical_flag_penalty
        self.warning_penalty = warning_flag_penalty

    def verify(
        self,
        response_text: str,
        diagnoses: Optional[List[Dict]] = None,
        semantic_entropy: Optional[float] = None,
        source_chunks: Optional[List] = None,
        patient_id: Optional[str] = None,
        pass_threshold: Optional[float] = None,
        escalate_threshold: Optional[float] = None,
        critical_penalty: Optional[float] = None,
        warning_penalty: Optional[float] = None,
    ) -> Dict[str, Any]:
        """
        Full four-expert verification.

        Args:
            response_text: CCA generated text
            diagnoses: Parsed diagnoses (for ICD-10 validation)
            semantic_entropy: Pre-computed H_SE (if available)
            source_chunks: Retrieved KB chunks for NLI
            patient_id: Patient the response is about; citations of any
                other patient's records are redacted
            pass_threshold, escalate_threshold, critical_penalty,
            warning_penalty: Per-call overrides of the instance defaults.
                Pass these instead of mutating the shared instance, which
                is not safe across concurrent queries.

        Returns:
            Dict with trust_score, decision, per-expert scores, flags
        """
        if pass_threshold is None:
            pass_threshold = self.pass_threshold
        if escalate_threshold is None:
            escalate_threshold = self.escalate_threshold
        if critical_penalty is None:
            critical_penalty = self.critical_penalty
        if warning_penalty is None:
            warning_penalty = self.warning_penalty

        all_flags = []

        # ── Expert 1: NLI Citation Verification ──
        nli_score = 0.8  # default if no citation pipeline
        citation_report = None

        if self.citation_pipeline is not None:
            citation_report = self.citation_pipeline.verify_response(
                response_text, patient_id=patient_id
            )
            nli_score = citation_report.nli_pass_rate

            # Collect redacted claims as critical flags
            for r in citation_report.citation_results:
                if r.action == VerificationAction.REDACT:
                    all_flags.append({
                        "type": "nli_contradiction",
                        "severity": "critical",
                        "text": r.claim_text[:80],
                        "reason": r.reason
                    })
                elif r.action == VerificationAction.FLAG:
                    all_flags.append({
                        "type": "nli_unsupported",
                        "severity": "warning",
                        "text": r.claim_text[:80],
                        "reason": r.reason
                    })
        else:
            # Fallback: use fabrication/contradiction detection from M2
            nli_score, nli_flags = self._check_fabrication_contradiction(response_text)
            all_flags.extend(nli_flags)

        # ── Expert 2: Coherence + Garbled Text ──
        coh_score, coh_flags = self._check_coherence(response_text)
        all_flags.extend(coh_flags)

        # ── Expert 3: Numerical Accuracy ──
        num_score, num_flags = self._check_numerical(response_text)
        all_flags.extend(num_flags)

        # ── Expert 4: Semantic Entropy ──
        se_score = 1.0  # default (no confabulation)
        if semantic_entropy is not None:
            # Convert entropy to score: high entropy → low score
            se_threshold = 0.8 if self.citation_pipeline else 1.2
            if semantic_entropy > se_threshold:
                se_score = max(0.0, 1.0 - (semantic_entropy / 2.322))
                all_flags.append({
                    "type": "semantic_entropy",
                    "severity": "critical",
                    "text": f"H_SE={semantic_entropy:.3f}",
                    "reason": f"High semantic entropy ({semantic_entropy:.3f} > {se_threshold})"
                })
            else:
                se_score = 1.0 - (semantic_entropy / (2 * se_threshold))

        # ── Composite Trust Score ──
        base_trust = (
            self.alpha * nli_score
            + self.beta * coh_score
            + self.gamma * num_score
            + self.delta * se_score
        )

        # ── Flag Penalty ──
        n_critical = sum(1 for f in all_flags if f.get("severity") == "critical")
        n_warning = sum(1 for f in all_flags if f.get("severity") == "warning")
        penalty = n_critical * critical_penalty + n_warning * warning_penalty
        trust = max(0.0, base_trust - penalty)

        # ── Decision ──
        if trust >= pass_threshold:
            decision = "pass"
        elif trust >= escalate_threshold:
            decision = "regenerate"
        else:
            decision = "escalate"

        # ── Badge ──
        badge = {"pass": "GREEN", "regenerate": "AMBER", "escalate": "RED"}[decision]

        return {
            "trust_score": round(trust, 4),
            "base_trust": round(base_trust, 4),
            "decision": decision,
            "badge": badge,
            # Per-expert
            "nli_score": round(nli_score, 4),
            "coherence_score": round(coh_score, 4),
            "numerical_score": round(num_score, 4),
            "semantic_entropy_score": round(se_score, 4),
            # Flags
            "n_critical_flags": n_critical,
            "n_warning_flags": n_warning,
            "flag_penalty": round(penalty, 4),
            "flags": all_flags,
            # Citation report
            "citation_report": {
                "n_claims": citation_report.n_claims if citation_report else 0,
                "pass_rate": citation_report.nli_pass_rate if citation_report else 0,
                "redact_rate": citation_report.nli_redact_rate if citation_report else 0,
            } if citation_report else None,
            # Weights
            "weights": {
                "alpha": self.alpha, "beta": self.beta,
                "gamma": self.gamma, "delta": self.delta
            },
        }

    def _check_fabrication_contradiction(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 1 fallback: fabrication + contradiction detection."""
        flags = []
        score = 1.0
        text_lower = text.lower()

        for marker in self.FABRICATION_MARKERS:
            if marker in text_lower:
                flags.append({
                    "type": "fabrication", "severity": "critical",
                    "text": marker,
                    "reason": f"Fabrication marker: '{marker}'"
                })
                score -= 0.3

        for pat_a, pat_b in self.CONTRADICTION_PATTERNS:
            if re.search(pat_a, text_lower) and re.search(pat_b, text_lower):
                flags.append({
                    "type": "contradiction", "severity": "critical",
                    "text": "self-contradiction",
                    "reason": "Contradictory claims detected"
                })
                score -= 0.4

        return max(0.0, score), flags

    def _check_coherence(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 2: coherence + garbled text."""
        flags = []
        score = 1.0

        if not text or len(text.strip()) < 20:
            flags.append({
                "type": "empty_content", "severity": "critical",
                "text": text[:30] if text else "[EMPTY]",
                "reason": "Empty or near-empty content"
            })
            return 0.0, flags

        # Garbled text detection
        alpha_ratio = sum(
            c.isalpha() or c.isspace() or c in '.,;:!?-/()'
            for c in text
        ) / max(len(text), 1)

        garble_patterns = ['@', '###', '???', '$$$', '%%%']
        garble_hits = sum(1 for p in garble_patterns if p in text)

        if alpha_ratio < 0.6 or garble_hits > 0:
            quality = max(0.0, alpha_ratio - 0.2 * garble_hits)
            flags.append({
                "type": "garbled_text", "severity": "critical",
                "text": text[:60],
                "reason": f"Text quality={quality:.2f} -- likely garbled"
            })
            score = quality

        return score, flags

    def _check_numerical(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 3: context-aware numerical accuracy."""
        flags = []
        checked = 0
        ok = 0

        for pattern, field_name, val_min, val_max in self.NUMERICAL_PATTERNS:
            for match in re.finditer(pattern, text, re.IGNORECASE):
                try:
                    value = float(match.group(1))
                except (ValueError, IndexError):
                    continue
                checked += 1
                if value < val_min or value > val_max:
                    flags.append({
                        "type": "implausible_value", "severity": "critical",
                        "text": match.group(0),
                        "reason": f"Implausible {field_name}: {value} (range: {val_min}-{val_max})"
                    })
                else:
                    ok += 1

        if checked == 0:
            return 0.85, flags
        return ok / checked, flags


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.6  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_m3_part2():
    """Demonstrate NLI verification + citation pipeline + NLI-backed HGA."""
    print("=" * 70)
    print("MHAI M3 Part 2: NLI Verifier + Citation Pipeline + HGA")
    print("=" * 70)

    # ── Step 1: Load NLI Verifier ──
    print("\n[1/5] Loading NLI Entailment Verifier...")
    nli = NLIEntailmentVerifier(device="cpu")
    print(f"  Model loaded: {nli._loaded}")

    # ── Step 2: Test NLI verification ──
    print("\n[2/5] Testing NLI verification on claim pairs...")
    test_pairs = [
        ("Metformin is the first-line treatment for T2DM.",
         "Metformin 500mg is recommended as initial T2DM therapy.",
         "ENTAILMENT expected"),
        ("Exercise of 150 minutes per week is recommended.",
         "300 minutes of vigorous exercise weekly is advised.",
         "CONTRADICTION expected"),
        ("Insulin therapy is initiated when HbA1c > 9%.",
         "The patient should eat more vegetables.",
         "NEUTRAL expected"),
    ]

    for premise, hypothesis, expected in test_pairs:
        result = nli.verify(premise, hypothesis)
        icon = "+" if expected.split()[0] == result.label.value.upper() else "?"
        print(f"  [{icon}] {result.label.value.upper()} "
              f"(conf={result.confidence:.2f})")
        print(f"      P: {premise[:60]}...")
        print(f"      H: {hypothesis[:60]}...")

    # ── Step 3: Build KB + Citation pipeline ──
    print("\n[3/5] Building KB and citation verification pipeline...")

    # Minimal KB for demo
    from mhai.knowledge_base.kb_builder import KBChunk, MedicalKnowledgeBase
    kb = MedicalKnowledgeBase()
    for g in MedicalKnowledgeBase.REFERENCE_GUIDELINES:
        cid = kb._generate_id()
        kb.chunks[cid] = KBChunk(
            chunk_id=cid, content=g["content"],
            chunk_type="guideline", source_patient_id="reference",
            source_subject_id="reference", metadata={"topic": g["topic"]}
        )
    print(f"  KB: {len(kb.chunks)} guideline chunks")

    pipeline = CitationVerificationPipeline(nli, kb)

    # ── Step 4: Verify a cited response ──
    print("\n[4/5] Verifying a citation-backed CCA response...")

    test_response = (
        "1. Gestational Diabetes Mellitus [ICD-10: O24.4] [KB#0001]\n"
        "   The patient's 2-hour glucose of 168 mg/dL exceeds the "
        "153 mg/dL threshold for GDM diagnosis [KB#0001].\n"
        "2. Management: dietary modification with 2000 cal diet [KB#0001].\n"
        "3. Exercise: 300 minutes of vigorous exercise weekly [KB#0001].\n"
        "4. The patient should receive Fictionol 5000mg daily."
    )

    report = pipeline.verify_response(test_response)

    print(f"  Claims: {report.n_claims}")
    print(f"  Citations found: {report.n_citations_found}")
    print(f"  Uncited claims: {report.n_uncited_claims}")
    print(f"  Pass rate: {report.nli_pass_rate:.1%}")
    print(f"  Flag rate: {report.nli_flag_rate:.1%}")
    print(f"  Redact rate: {report.nli_redact_rate:.1%}")

    print(f"\n  Per-claim results:")
    for r in report.citation_results:
        icon = {"pass": "+", "flag": "?", "redact": "X"}[r.action.value]
        print(f"    [{icon}] {r.action.value.upper()}: "
              f"{r.claim_text[:55]}...")
        print(f"        {r.reason}")

    # ── Step 5: Full NLI-backed HGA ──
    print("\n[5/5] Full NLI-backed HGA verification...")

    hga = NLIBackedHGA(
        nli_verifier=nli,
        citation_pipeline=pipeline,
        alpha=0.35, beta=0.25, gamma=0.20, delta=0.20
    )

    hga_result = hga.verify(
        response_text=test_response,
        semantic_entropy=0.5  # Low (consistent)
    )

    badge_icon = {"GREEN": "G", "AMBER": "A", "RED": "R"}
    print(f"\n  Trust: {hga_result['trust_score']:.3f} "
          f"(base={hga_result['base_trust']:.3f}, "
          f"penalty={hga_result['flag_penalty']:.3f})")
    print(f"  Decision: {hga_result['decision'].upper()} "
          f"[{badge_icon.get(hga_result['badge'], '?')}]")
    print(f"  NLI={hga_result['nli_score']:.2f}, "
          f"Coh={hga_result['coherence_score']:.2f}, "
          f"Num={hga_result['numerical_score']:.2f}, "
          f"SE={hga_result['semantic_entropy_score']:.2f}")
    print(f"  Flags: {hga_result['n_critical_flags']} critical, "
          f"{hga_result['n_warning_flags']} warning")

    if hga_result['citation_report']:
        cr = hga_result['citation_report']
        print(f"  Citations: {cr['n_claims']} claims, "
              f"pass={cr['pass_rate']:.1%}, "
              f"redact={cr['redact_rate']:.1%}")

    print("\n" + "=" * 70)
    print("M3 Part 2 complete. Proceed to Part 3 for end-to-end")
    print("evaluation targeting <=5% hallucination rate?")
    print("=" * 70)


if __name__ == "__main__":
    demo_m3_part2()
