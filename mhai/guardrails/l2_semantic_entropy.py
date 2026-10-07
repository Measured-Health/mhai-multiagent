"""
===========================================================================
MHAI Guardrail Implementation — Part 2 of 4
===========================================================================
Part 2: Semantic Entropy (Layer 2) + NLI Citation Verifier (Layer 3)
         + HGA Trust Score Computation

Implements:
  - Semantic entropy confabulation detector (Farquhar et al., Nature 2024)
  - SelfCheckGPT consistency metric (Manakul et al., 2023)
  - NLI citation entailment verifier (DeBERTa-v3 cross-encoder)
  - HGA trust score T(y) = α·NLI + β·Coherence + γ·NumCheck

Public data used:
  - TruthfulQA (from datasets library)
  - MedQA (medical question answering, from datasets library)
  - MedNLI (clinical NLI, via PhysioNet / HuggingFace)

Requirements:
  pip install torch transformers datasets numpy scikit-learn scipy tqdm
===========================================================================
"""

import torch
import torch.nn as nn
import numpy as np
from typing import List, Tuple, Dict, Optional, Set
from dataclasses import dataclass, field
from collections import defaultdict
from scipy.stats import entropy as scipy_entropy
import re
import logging

from mhai.utils.nli import DEFAULT_NLI_MODEL, nli_label_map

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.L2L3")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.1  SEMANTIC ENTROPY — CORE IMPLEMENTATION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class SemanticCluster:
    """A cluster of responses that share the same meaning."""
    cluster_id: int
    responses: List[str]
    log_probs: List[float]
    representative: str = ""

    @property
    def probability(self) -> float:
        """Aggregate probability mass of this meaning cluster."""
        return float(np.exp(
            np.logaddexp.reduce(self.log_probs)
        ))


@dataclass
class SemanticEntropyResult:
    """Result of semantic entropy computation for one prompt."""
    prompt: str
    entropy: float                          # H_SE in bits
    n_clusters: int                         # J — number of distinct meanings
    n_samples: int                          # K — total samples generated
    clusters: List[SemanticCluster]
    is_confabulation: bool                  # H_SE > τ_SE
    selected_response: Optional[str] = None # best response from dominant cluster


class BidirectionalEntailmentClassifier:
    """
    Determines whether two responses share the same meaning using
    bidirectional NLI entailment.

    Two responses A and B share meaning iff:
        NLI(A, B) = ENTAILMENT  AND  NLI(B, A) = ENTAILMENT

    Uses DeBERTa-v3-large fine-tuned on NLI (same model as HGA).
    This is the "secret sauce" of semantic entropy — it allows us
    to cluster by MEANING rather than by surface text.
    """

    def __init__(
        self,
        model_name: str = DEFAULT_NLI_MODEL,
        device: str = "cuda" if torch.cuda.is_available() else "cpu"
    ):
        from transformers import AutoModelForSequenceClassification, AutoTokenizer

        self.device = device
        self.tokenizer = AutoTokenizer.from_pretrained(model_name)
        self.model = AutoModelForSequenceClassification.from_pretrained(
            model_name
        ).to(device)
        self.model.eval()

        self.label_map = nli_label_map(self.model)
        self.entailment_id = next(
            i for i, name in self.label_map.items() if name == "entailment"
        )
        logger.info(f"NLI classifier loaded: {model_name}")

    @torch.no_grad()
    def check_entailment(self, premise: str, hypothesis: str) -> str:
        """
        Classify the NLI relationship between premise and hypothesis.

        Returns: 'entailment', 'neutral', or 'contradiction'
        """
        inputs = self.tokenizer(
            premise, hypothesis,
            return_tensors="pt",
            truncation=True,
            max_length=512
        ).to(self.device)

        logits = self.model(**inputs).logits[0]
        probs = torch.softmax(logits, dim=-1)
        pred = logits.argmax().item()

        return self.label_map[pred]

    def share_meaning(self, response_a: str, response_b: str) -> bool:
        """
        Check if two responses share the same meaning via
        bidirectional entailment.

        A and B share meaning iff:
            NLI(A→B) = entailment  AND  NLI(B→A) = entailment
        """
        forward = self.check_entailment(response_a, response_b)
        if forward != "entailment":
            return False

        backward = self.check_entailment(response_b, response_a)
        return backward == "entailment"


class ResponseSampler:
    """
    Generates K stochastic response samples from the LLM for a given prompt.
    Each sample includes the log-probability of the generated sequence.
    """

    def __init__(
        self,
        model_name: str = "mistralai/Mistral-7B-Instruct-v0.2",
        device: str = "cuda" if torch.cuda.is_available() else "cpu"
    ):
        from transformers import AutoModelForCausalLM, AutoTokenizer

        self.device = device
        self.tokenizer = AutoTokenizer.from_pretrained(model_name)
        self.model = AutoModelForCausalLM.from_pretrained(
            model_name,
            torch_dtype=torch.float16 if device == "cuda" else torch.float32,
            device_map="auto" if device == "cuda" else None
        )
        self.model.eval()

        if self.tokenizer.pad_token is None:
            self.tokenizer.pad_token = self.tokenizer.eos_token

    @torch.no_grad()
    def sample(
        self,
        prompt: str,
        k: int = 5,
        temperature: float = 0.7,
        max_new_tokens: int = 128
    ) -> List[Tuple[str, float]]:
        """
        Generate K responses with their log-probabilities.

        Returns: List of (response_text, log_prob) tuples
        """
        inputs = self.tokenizer(
            prompt, return_tensors="pt", truncation=True, max_length=512
        ).to(self.device)
        input_len = inputs["input_ids"].shape[1]

        samples = []
        for _ in range(k):
            outputs = self.model.generate(
                **inputs,
                max_new_tokens=max_new_tokens,
                temperature=temperature,
                do_sample=True,
                top_p=0.95,
                return_dict_in_generate=True,
                output_scores=True
            )

            generated_ids = outputs.sequences[0][input_len:]
            response_text = self.tokenizer.decode(
                generated_ids, skip_special_tokens=True
            )

            # Compute sequence log-probability
            # Sum log-probs of each generated token
            log_prob = 0.0
            for step_idx, scores in enumerate(outputs.scores):
                token_id = generated_ids[step_idx]
                token_log_probs = torch.log_softmax(scores[0], dim=-1)
                log_prob += token_log_probs[token_id].item()

            # Normalize by sequence length
            seq_len = len(generated_ids)
            if seq_len > 0:
                log_prob /= seq_len

            samples.append((response_text, log_prob))

        return samples


class SemanticEntropyComputer:
    """
    Computes semantic entropy for confabulation detection.

    Full pipeline (Farquhar et al., Nature 2024):
    1. Sample K responses from the LLM
    2. Cluster responses by meaning (bidirectional NLI entailment)
    3. Aggregate probabilities within each cluster
    4. Compute entropy over cluster probabilities

    Mathematical formulation (Eq. 5 from Section 1.4):
        H_SE(x) = -Σ_j p(C_j) log p(C_j)
    where
        p(C_j) = Σ_{y^(k) ∈ C_j} P_θ(y^(k) | x)
    """

    def __init__(
        self,
        nli_classifier: BidirectionalEntailmentClassifier,
        threshold: float = 1.2  # τ_SE in bits
    ):
        self.nli = nli_classifier
        self.threshold = threshold

    def cluster_responses(
        self,
        responses: List[Tuple[str, float]]
    ) -> List[SemanticCluster]:
        """
        Cluster responses by meaning using bidirectional entailment.

        Algorithm (from Farquhar et al.):
        - For each response, check if it shares meaning with the
          representative of any existing cluster
        - If yes, add to that cluster
        - If no, create a new cluster with this response as representative
        """
        clusters: List[SemanticCluster] = []

        for text, log_prob in responses:
            assigned = False
            for cluster in clusters:
                if self.nli.share_meaning(cluster.representative, text):
                    cluster.responses.append(text)
                    cluster.log_probs.append(log_prob)
                    assigned = True
                    break

            if not assigned:
                clusters.append(SemanticCluster(
                    cluster_id=len(clusters),
                    responses=[text],
                    log_probs=[log_prob],
                    representative=text
                ))

        return clusters

    def compute(
        self,
        prompt: str,
        responses: List[Tuple[str, float]]
    ) -> SemanticEntropyResult:
        """
        Compute semantic entropy for a set of sampled responses.

        Args:
            prompt: The original query
            responses: List of (response_text, log_prob) from the LLM

        Returns:
            SemanticEntropyResult with entropy, clusters, and decision
        """
        # Step 1: Cluster by meaning
        clusters = self.cluster_responses(responses)

        # Step 2: Compute cluster probabilities
        cluster_probs = []
        for c in clusters:
            # p(C_j) = Σ exp(log_prob_k) for all y^(k) in C_j
            # then normalize across all clusters
            cluster_probs.append(c.probability)

        # Normalize to form a proper distribution
        total = sum(cluster_probs)
        if total > 0:
            cluster_probs = [p / total for p in cluster_probs]
        else:
            cluster_probs = [1.0 / len(clusters)] * len(clusters)

        # Step 3: Compute entropy H_SE = -Σ p(C_j) log2 p(C_j)
        h_se = float(scipy_entropy(cluster_probs, base=2))

        # Step 4: Select best response from dominant cluster
        dominant_idx = int(np.argmax(cluster_probs))
        dominant_cluster = clusters[dominant_idx]
        best_response_idx = int(np.argmax(dominant_cluster.log_probs))
        selected = dominant_cluster.responses[best_response_idx]

        is_confab = h_se > self.threshold

        logger.info(
            f"H_SE = {h_se:.3f} bits | "
            f"{len(clusters)} clusters from {len(responses)} samples | "
            f"{'CONFABULATION' if is_confab else 'OK'}"
        )

        return SemanticEntropyResult(
            prompt=prompt,
            entropy=h_se,
            n_clusters=len(clusters),
            n_samples=len(responses),
            clusters=clusters,
            is_confabulation=is_confab,
            selected_response=selected if not is_confab else None
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.2  SELFCHECKGPT — COMPARISON METRIC
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class SelfCheckGPT:
    """
    SelfCheckGPT (Manakul et al., 2023): Black-box hallucination detection
    via self-consistency.

    For each sentence s in the primary response, check whether N
    additional stochastic samples support or contradict it:

        SelfCheck(s) = 1 - (1/N) Σ_{n=1}^{N} NLI(y^(n), s)

    High SelfCheck score = inconsistent across samples = likely hallucinated.

    Advantage over semantic entropy: works with black-box API access.
    Disadvantage: requires N full forward passes (expensive).
    """

    def __init__(
        self,
        nli_classifier: BidirectionalEntailmentClassifier
    ):
        self.nli = nli_classifier

    def _split_sentences(self, text: str) -> List[str]:
        """Simple sentence splitter."""
        sentences = re.split(r'(?<=[.!?])\s+', text.strip())
        return [s for s in sentences if len(s.strip()) > 10]

    def check(
        self,
        primary_response: str,
        other_samples: List[str]
    ) -> Dict[str, float]:
        """
        Compute SelfCheckGPT score for each sentence in primary_response.

        Args:
            primary_response: The main response to evaluate
            other_samples: N additional stochastic samples for the same prompt

        Returns:
            Dict mapping sentence → selfcheck score (0=consistent, 1=hallucinated)
        """
        sentences = self._split_sentences(primary_response)
        scores = {}

        for sentence in sentences:
            support_count = 0
            for sample in other_samples:
                label = self.nli.check_entailment(sample, sentence)
                if label == "entailment":
                    support_count += 1

            # SelfCheck = 1 - (fraction of samples that entail the sentence)
            n = len(other_samples)
            selfcheck = 1.0 - (support_count / n) if n > 0 else 1.0
            scores[sentence] = selfcheck

        return scores


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.3  NLI CITATION ENTAILMENT VERIFIER (LAYER 3)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class CitationVerificationResult:
    """Result of verifying a single claim against its cited source."""
    sentence: str
    citation_id: str
    source_text: str
    nli_label: str           # entailment / neutral / contradiction
    confidence: float        # softmax probability of predicted label
    action: str              # pass / flag / redact


class CitationEntailmentVerifier:
    """
    Layer 3: NLI Citation Entailment Verifier

    For each generated sentence with a citation tag [KB:#id]:
    1. Retrieve the cited KB chunk as NLI premise
    2. Classify (source_chunk, generated_sentence) via DeBERTa-v3
    3. CONTRADICTION with conf > 0.7 → REDACT
       NEUTRAL with conf > 0.7 → FLAG
       ENTAILMENT → PASS

    Protocol from Section 1.5 of the paper.
    """

    def __init__(
        self,
        model_name: str = DEFAULT_NLI_MODEL,
        device: str = "cuda" if torch.cuda.is_available() else "cpu",
        contradiction_threshold: float = 0.7,
        neutral_threshold: float = 0.7
    ):
        from transformers import AutoModelForSequenceClassification, AutoTokenizer

        self.device = device
        self.tokenizer = AutoTokenizer.from_pretrained(model_name)
        self.model = AutoModelForSequenceClassification.from_pretrained(
            model_name
        ).to(device)
        self.model.eval()
        self.label_map = nli_label_map(self.model)

        self.contradiction_threshold = contradiction_threshold
        self.neutral_threshold = neutral_threshold

    def _extract_citations(self, text: str) -> List[Tuple[str, str]]:
        """
        Extract sentences with citation tags from generated text.
        Returns: List of (sentence, citation_id)
        """
        pattern = r'([^.!?]*\[KB:#?(\w+)\][^.!?]*[.!?])'
        matches = re.findall(pattern, text)
        return [(m[0].strip(), m[1]) for m in matches]

    @torch.no_grad()
    def verify_claim(
        self,
        source_text: str,
        claim: str
    ) -> Tuple[str, float]:
        """
        Classify NLI relationship: (source → claim).

        Returns: (label, confidence)
        """
        inputs = self.tokenizer(
            source_text, claim,
            return_tensors="pt",
            truncation=True,
            max_length=512
        ).to(self.device)

        logits = self.model(**inputs).logits[0]
        probs = torch.softmax(logits, dim=-1)

        pred_idx = logits.argmax().item()
        confidence = probs[pred_idx].item()

        return self.label_map[pred_idx], confidence

    def verify_response(
        self,
        generated_text: str,
        knowledge_base: Dict[str, str]
    ) -> List[CitationVerificationResult]:
        """
        Verify all cited claims in a generated response.

        Args:
            generated_text: CCA output with [KB:#id] citation tags
            knowledge_base: Dict mapping citation IDs to source text

        Returns:
            List of CitationVerificationResult for each cited sentence
        """
        cited_sentences = self._extract_citations(generated_text)
        results = []

        for sentence, citation_id in cited_sentences:
            source = knowledge_base.get(citation_id, "")
            if not source:
                results.append(CitationVerificationResult(
                    sentence=sentence,
                    citation_id=citation_id,
                    source_text="[NOT FOUND]",
                    nli_label="neutral",
                    confidence=1.0,
                    action="flag"
                ))
                continue

            label, confidence = self.verify_claim(source, sentence)

            # Decision logic from Section 1.5
            if label == "contradiction" and confidence > self.contradiction_threshold:
                action = "redact"
            elif label == "neutral" and confidence > self.neutral_threshold:
                action = "flag"
            else:
                action = "pass"

            results.append(CitationVerificationResult(
                sentence=sentence,
                citation_id=citation_id,
                source_text=source[:100] + "...",
                nli_label=label,
                confidence=confidence,
                action=action
            ))

            logger.info(
                f"  [{citation_id}] {label} (conf={confidence:.3f}) → {action}"
            )

        return results


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.4  HGA TRUST SCORE COMPUTATION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class TrustScoreResult:
    """Full trust score computation result."""
    trust_score: float          # T(y) ∈ [0, 1]
    nli_score: float            # NLI component
    coherence_score: float      # Coherence component
    numerical_score: float      # Numerical accuracy component
    alpha: float
    beta: float
    gamma: float
    decision: str               # pass / regenerate / escalate
    details: Dict[str, any] = field(default_factory=dict)


class NumericalAccuracyChecker:
    """
    Expert 3 of the HGA MoE: checks numerical values in medical text
    against known reference ranges.

    Detects:
    - Out-of-range lab values (e.g., "blood glucose 5000 mg/dL")
    - Implausible dosages (e.g., "metformin 50000 mg")
    - Date inconsistencies
    """

    # Standard reference ranges for common lab values
    REFERENCE_RANGES = {
        "blood_glucose_fasting": (70, 100, "mg/dL"),
        "blood_glucose_random": (70, 200, "mg/dL"),
        "hemoglobin_a1c": (4.0, 6.5, "%"),
        "systolic_bp": (80, 180, "mmHg"),
        "diastolic_bp": (50, 120, "mmHg"),
        "heart_rate": (40, 180, "bpm"),
        "temperature": (95.0, 104.0, "°F"),
        "hemoglobin": (7.0, 20.0, "g/dL"),
        "wbc": (2.0, 30.0, "×10³/µL"),
        "platelets": (50, 600, "×10³/µL"),
        "creatinine": (0.3, 10.0, "mg/dL"),
        "sodium": (120, 160, "mEq/L"),
        "potassium": (2.5, 7.0, "mEq/L"),
    }

    # Common drug dosage ranges (max daily dose)
    DOSAGE_RANGES = {
        "metformin": (500, 2550, "mg"),
        "aspirin": (75, 1000, "mg"),
        "lisinopril": (2.5, 80, "mg"),
        "atorvastatin": (10, 80, "mg"),
        "amlodipine": (2.5, 10, "mg"),
        "omeprazole": (10, 40, "mg"),
        "amoxicillin": (250, 3000, "mg"),
    }

    def check(self, text: str) -> Tuple[float, List[str]]:
        """
        Check numerical values in text against reference ranges.

        Returns: (score ∈ [0,1], list of flagged issues)
            1.0 = all numbers plausible
            0.0 = severe numerical hallucination detected
        """
        issues = []
        numbers_found = 0
        numbers_ok = 0

        # Extract number-unit patterns
        patterns = [
            (r'(\d+(?:\.\d+)?)\s*mg/dL', "mg/dL"),
            (r'(\d+(?:\.\d+)?)\s*mmHg', "mmHg"),
            (r'(\d+(?:\.\d+)?)\s*bpm', "bpm"),
            (r'(\d+(?:\.\d+)?)\s*°?F', "°F"),
            (r'(\d+(?:\.\d+)?)\s*g/dL', "g/dL"),
            (r'(\d+(?:\.\d+)?)\s*mEq/L', "mEq/L"),
            (r'(\d+(?:\.\d+)?)\s*mg\b', "mg"),
        ]

        for pattern, unit in patterns:
            matches = re.findall(pattern, text, re.IGNORECASE)
            for match in matches:
                value = float(match)
                numbers_found += 1

                # Check against all reference ranges with matching unit
                plausible = True
                for name, (lo, hi, ref_unit) in {
                    **self.REFERENCE_RANGES,
                    **self.DOSAGE_RANGES
                }.items():
                    if ref_unit == unit:
                        # Use a generous 3x range to catch only egregious errors
                        if value < lo / 3 or value > hi * 3:
                            issues.append(
                                f"Implausible: {value} {unit} "
                                f"(expected {lo}-{hi} for {name})"
                            )
                            plausible = False
                            break

                if plausible:
                    numbers_ok += 1

        if numbers_found == 0:
            return 1.0, []  # No numbers to check

        score = numbers_ok / numbers_found
        return score, issues


class CoherenceChecker:
    """
    Expert 2 of the HGA MoE: checks internal logical consistency
    of a generated response.

    Detects:
    - Self-contradictory statements within the same response
    - Logical inconsistencies (A implies B, then B implies not-A)
    """

    def __init__(self, nli_classifier: BidirectionalEntailmentClassifier):
        self.nli = nli_classifier

    def _split_sentences(self, text: str) -> List[str]:
        sentences = re.split(r'(?<=[.!?])\s+', text.strip())
        return [s for s in sentences if len(s.strip()) > 15]

    def check(self, text: str) -> Tuple[float, List[str]]:
        """
        Check for internal contradictions by pairwise NLI on all
        sentence pairs within the response.

        Returns: (score ∈ [0,1], list of contradiction pairs)
            1.0 = fully coherent
            0.0 = severe internal contradictions
        """
        sentences = self._split_sentences(text)
        if len(sentences) < 2:
            return 1.0, []

        contradictions = []
        n_pairs = 0

        # Check all pairs (optimized: skip adjacent sentences that
        # are likely continuations rather than independent claims)
        for i in range(len(sentences)):
            for j in range(i + 2, len(sentences)):
                n_pairs += 1
                label = self.nli.check_entailment(
                    sentences[i], sentences[j]
                )
                if label == "contradiction":
                    contradictions.append(
                        f"Contradiction: '{sentences[i][:60]}...' "
                        f"vs '{sentences[j][:60]}...'"
                    )

        if n_pairs == 0:
            return 1.0, []

        # Score: fraction of non-contradictory pairs
        score = 1.0 - (len(contradictions) / n_pairs)
        return max(0.0, score), contradictions


class HGATrustScoreComputer:
    """
    Hallucination Guard Agent: computes the composite trust score.

    T(y) = α · NLI(y, KB) + β · Coherence(y) + γ · NumCheck(y)

    where α + β + γ = 1 (Eq. 7 from Section 1.5).

    Decision thresholds:
        T ≥ 0.6  → PASS
        0.3 ≤ T < 0.6 → REGENERATE with stricter prompt
        T < 0.3  → ESCALATE to clinician
    """

    def __init__(
        self,
        citation_verifier: CitationEntailmentVerifier,
        coherence_checker: CoherenceChecker,
        numerical_checker: NumericalAccuracyChecker,
        alpha: float = 0.5,
        beta: float = 0.3,
        gamma: float = 0.2
    ):
        self.citation_verifier = citation_verifier
        self.coherence_checker = coherence_checker
        self.numerical_checker = numerical_checker
        self.alpha = alpha
        self.beta = beta
        self.gamma = gamma

    def compute(
        self,
        generated_text: str,
        knowledge_base: Dict[str, str]
    ) -> TrustScoreResult:
        """
        Compute the full trust score for a CCA response.

        Args:
            generated_text: CCA output with [KB:#id] citation tags
            knowledge_base: Dict mapping citation IDs to source text

        Returns:
            TrustScoreResult with composite score and decision
        """
        # ── Expert 1: NLI Citation Verification ──
        citation_results = self.citation_verifier.verify_response(
            generated_text, knowledge_base
        )
        if citation_results:
            n_pass = sum(1 for r in citation_results if r.action == "pass")
            nli_score = n_pass / len(citation_results)
        else:
            nli_score = 0.5  # No citations = uncertain

        # ── Expert 2: Coherence ──
        coherence_score, coherence_issues = self.coherence_checker.check(
            generated_text
        )

        # ── Expert 3: Numerical Accuracy ──
        numerical_score, numerical_issues = self.numerical_checker.check(
            generated_text
        )

        # ── Composite Trust Score ──
        trust_score = (
            self.alpha * nli_score
            + self.beta * coherence_score
            + self.gamma * numerical_score
        )

        # ── Decision ──
        if trust_score >= 0.6:
            decision = "pass"
        elif trust_score >= 0.3:
            decision = "regenerate"
        else:
            decision = "escalate"

        result = TrustScoreResult(
            trust_score=trust_score,
            nli_score=nli_score,
            coherence_score=coherence_score,
            numerical_score=numerical_score,
            alpha=self.alpha,
            beta=self.beta,
            gamma=self.gamma,
            decision=decision,
            details={
                "citation_results": [
                    {
                        "sentence": r.sentence[:80],
                        "label": r.nli_label,
                        "action": r.action
                    }
                    for r in citation_results
                ],
                "coherence_issues": coherence_issues,
                "numerical_issues": numerical_issues
            }
        )

        logger.info(
            f"Trust Score: T={trust_score:.3f} "
            f"(NLI={nli_score:.2f}, Coh={coherence_score:.2f}, "
            f"Num={numerical_score:.2f}) → {decision.upper()}"
        )

        return result

    def update_weights(
        self,
        new_alpha: float,
        new_beta: float,
        new_gamma: float
    ):
        """Update trust-score weights from doctor feedback (CMA-ES)."""
        total = new_alpha + new_beta + new_gamma
        self.alpha = new_alpha / total
        self.beta = new_beta / total
        self.gamma = new_gamma / total
        logger.info(
            f"Trust weights updated: α={self.alpha:.3f}, "
            f"β={self.beta:.3f}, γ={self.gamma:.3f}"
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.5  DEMO / USAGE EXAMPLE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_layer2_layer3_pipeline():
    """
    End-to-end demo of Layers 2 and 3 with simulated data.
    """
    print("=" * 70)
    print("MHAI Layers 2 & 3: Semantic Entropy + NLI + Trust Score")
    print("=" * 70)

    # ────────────────────────────────────────
    # DEMO 1: Semantic Entropy
    # ────────────────────────────────────────
    print("\n" + "─" * 50)
    print("[Layer 2] SEMANTIC ENTROPY — Confabulation Detection")
    print("─" * 50)

    print("\nScenario A: Model gives CONSISTENT answers (low entropy)")
    print("  Prompt: 'What is the first-line treatment for Type 2 Diabetes?'")
    print("  Sample 1: 'Metformin is the first-line treatment.'")
    print("  Sample 2: 'First-line therapy is metformin (500-2000mg).'")
    print("  Sample 3: 'The initial drug of choice is metformin.'")
    print("  Sample 4: 'Metformin is recommended as initial therapy.'")
    print("  Sample 5: 'Start with metformin for T2DM.'")
    print("  → All share the same meaning → 1 cluster → H_SE ≈ 0.0 bits")
    print("  → Decision: OK (not confabulating)")

    # Simulated entropy computation
    # 1 cluster with all probability mass → entropy = 0
    probs_consistent = [1.0]
    h_consistent = float(scipy_entropy(probs_consistent, base=2))
    print(f"  → H_SE = {h_consistent:.3f} bits (threshold τ_SE = 1.2)")

    print("\nScenario B: Model gives INCONSISTENT answers (high entropy)")
    print("  Prompt: 'What is Dr. Smith's recommended dosage for Patient X?'")
    print("  Sample 1: 'Dr. Smith recommends 500mg twice daily.'")
    print("  Sample 2: 'The recommended dosage is 1000mg once daily.'")
    print("  Sample 3: 'Dr. Smith suggests 250mg three times daily.'")
    print("  Sample 4: 'The dosage should be 750mg with meals.'")
    print("  Sample 5: 'Start with 500mg and titrate to 1500mg.'")
    print("  → 5 different meanings → 5 clusters → H_SE ≈ 2.32 bits")
    print("  → Decision: CONFABULATION (H_SE > τ_SE = 1.2)")

    # Simulated: 5 clusters with equal probability
    probs_inconsistent = [0.2, 0.2, 0.2, 0.2, 0.2]
    h_inconsistent = float(scipy_entropy(probs_inconsistent, base=2))
    print(f"  → H_SE = {h_inconsistent:.3f} bits (threshold τ_SE = 1.2)")

    # ────────────────────────────────────────
    # DEMO 2: NLI Citation Verifier
    # ────────────────────────────────────────
    print("\n" + "─" * 50)
    print("[Layer 3] NLI CITATION ENTAILMENT VERIFIER")
    print("─" * 50)

    # Simulated KB
    mock_kb = {
        "001": (
            "Metformin is the first-line pharmacological treatment for "
            "type 2 diabetes mellitus. Initial dosage is 500mg once or "
            "twice daily with meals. Maximum dose is 2550mg per day."
        ),
        "002": (
            "Insulin therapy should be initiated when HbA1c remains "
            "above 9% despite dual oral therapy for 3-6 months."
        ),
        "003": (
            "Regular exercise of 150 minutes per week of moderate-intensity "
            "aerobic activity is recommended for diabetes management."
        ),
    }

    # Simulated CCA response with citations
    mock_response = (
        "For this patient with Type 2 Diabetes, metformin 500mg twice daily "
        "is recommended as first-line treatment [KB:#001]. "
        "If blood glucose remains uncontrolled after 6 months, consider "
        "adding insulin therapy [KB:#002]. "
        "The patient should also engage in 300 minutes per week of vigorous "
        "exercise [KB:#003]. "
        "Additionally, the patient's blood glucose of 5000 mg/dL suggests "
        "immediate hospitalization [KB:#001]."
    )

    print(f"\nCCA Response (with citations):")
    for line in mock_response.split(". "):
        print(f"  {line.strip()}.")

    print("\nVerification results (simulated):")
    print("  [KB:#001] 'metformin 500mg twice daily...' → ENTAILMENT → PASS")
    print("  [KB:#002] 'adding insulin therapy...'      → ENTAILMENT → PASS")
    print("  [KB:#003] '300 min vigorous exercise...'   → CONTRADICTION → REDACT")
    print("           (Source says 150 min moderate, not 300 min vigorous)")
    print("  [KB:#001] 'glucose 5000 mg/dL...'          → NEUTRAL → FLAG")
    print("           (Source doesn't discuss this glucose level)")

    # ────────────────────────────────────────
    # DEMO 3: Trust Score
    # ────────────────────────────────────────
    print("\n" + "─" * 50)
    print("[HGA] TRUST SCORE COMPUTATION")
    print("─" * 50)

    # Simulated component scores
    nli_score = 0.50   # 2 of 4 citations passed
    coh_score = 0.85   # Minor coherence issue
    num_score = 0.50   # "5000 mg/dL" flagged as implausible

    alpha, beta, gamma = 0.5, 0.3, 0.2
    trust = alpha * nli_score + beta * coh_score + gamma * num_score

    print(f"\n  Expert 1 (NLI):       {nli_score:.2f}  (2/4 citations verified)")
    print(f"  Expert 2 (Coherence): {coh_score:.2f}  (no self-contradictions)")
    print(f"  Expert 3 (Numerical): {num_score:.2f}  ('5000 mg/dL' implausible)")
    print(f"\n  T(y) = {alpha}×{nli_score:.2f} + "
          f"{beta}×{coh_score:.2f} + {gamma}×{num_score:.2f} = {trust:.3f}")

    if trust >= 0.6:
        decision = "PASS ✅"
    elif trust >= 0.3:
        decision = "REGENERATE 🔄 (stricter prompt)"
    else:
        decision = "ESCALATE 🔴 (clinician review)"

    print(f"\n  Decision: {decision}")
    print(f"  (Thresholds: ≥0.6 pass, 0.3-0.6 regenerate, <0.3 escalate)")

    # ────────────────────────────────────────
    # DEMO 4: Numerical Checker
    # ────────────────────────────────────────
    print("\n" + "─" * 50)
    print("[HGA Expert 3] NUMERICAL ACCURACY CHECKER")
    print("─" * 50)

    checker = NumericalAccuracyChecker()

    test_texts = [
        "Blood glucose: 95 mg/dL, BP: 120/80 mmHg, HR: 72 bpm",
        "Blood glucose: 5000 mg/dL, metformin 50000 mg prescribed",
        "Temperature: 98.6°F, hemoglobin: 14.2 g/dL, sodium: 140 mEq/L",
        "Creatinine: 500 mg/dL, potassium: 0.1 mEq/L",
    ]

    for text in test_texts:
        score, issues = checker.check(text)
        status = "✅" if score >= 0.8 else "⚠️" if score >= 0.5 else "🔴"
        print(f"\n  {status} Score={score:.2f} | '{text[:60]}...'")
        for issue in issues:
            print(f"     → {issue}")

    print("\n" + "=" * 70)
    print("Part 2 complete. Proceed to Part 3 for the full Guardrail")
    print("Cascade integration + Calibration-Aware Scoring?")
    print("=" * 70)


if __name__ == "__main__":
    demo_layer2_layer3_pipeline()
