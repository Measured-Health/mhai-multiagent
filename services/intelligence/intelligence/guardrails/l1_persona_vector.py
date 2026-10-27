"""
===========================================================================
MHAI Guardrail Implementation — Part 1 of 4
===========================================================================
Part 1: Infrastructure + Persona Vector Hallucination Probe (Layer 1)

This implements the Anthropic-inspired persona vector approach for
real-time hallucination detection using contrastive activation analysis.

Public data used:
  - TruthfulQA (from datasets library) for contrastive pair construction
  - Hugging Face transformers for model access

Requirements:
  pip install torch transformers datasets numpy scikit-learn tqdm
===========================================================================
"""

import torch
import torch.nn as nn
import numpy as np
from typing import List, Tuple, Dict, Optional
from dataclasses import dataclass, field
from sklearn.metrics import roc_auc_score, precision_recall_curve
from sklearn.linear_model import LogisticRegression
import json
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.L1")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.1  CONTRASTIVE PAIR CONSTRUCTION FROM TruthfulQA
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class ContrastivePair:
    """A pair of prompts: one that elicits hallucination, one that elicits
    honest uncertainty. Used to compute the persona vector."""
    prompt: str
    hallucinating_response: str   # x+: model confidently confabulates
    honest_response: str          # x-: model admits uncertainty
    category: str = ""
    source: str = "truthfulqa"


def build_contrastive_pairs_from_truthfulqa(
    max_pairs: int = 500,
    categories: Optional[List[str]] = None
) -> List[ContrastivePair]:
    """
    Build contrastive pairs from TruthfulQA dataset.

    For each question:
      x+ = prompt + best incorrect answer (confident hallucination)
      x- = prompt + "I don't have enough information to answer this
           accurately" (honest refusal)

    Uses: datasets library (HuggingFace)
    Data: truthful_qa (Lin et al., 2022), 817 questions across 38 categories
    """
    from datasets import load_dataset

    ds = load_dataset("truthful_qa", "multiple_choice", split="validation")

    pairs = []
    for item in ds:
        question = item["question"]

        # mc1_targets: single correct answer format
        choices = item["mc1_targets"]["choices"]
        labels = item["mc1_targets"]["labels"]

        # Find best correct and best incorrect answers
        correct_answers = [c for c, l in zip(choices, labels) if l == 1]
        incorrect_answers = [c for c, l in zip(choices, labels) if l == 0]

        if not incorrect_answers or not correct_answers:
            continue

        # x+: The model confidently gives the WRONG (but plausible) answer
        hallucinating_response = (
            f"Question: {question}\n"
            f"Answer: {incorrect_answers[0]}"
        )

        # x-: The model honestly admits uncertainty
        honest_response = (
            f"Question: {question}\n"
            f"Answer: I'm not certain about this. The answer might be "
            f"'{correct_answers[0]}', but I'd recommend verifying with "
            f"a reliable source."
        )

        pairs.append(ContrastivePair(
            prompt=question,
            hallucinating_response=hallucinating_response,
            honest_response=honest_response,
            category=item.get("category", "general"),
            source="truthfulqa"
        ))

        if len(pairs) >= max_pairs:
            break

    logger.info(f"Built {len(pairs)} contrastive pairs from TruthfulQA")
    return pairs


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.2  ACTIVATION EXTRACTION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class ActivationExtractor:
    """
    Extracts residual-stream activations from a transformer model
    at specified layers. These activations are used to compute the
    persona vector v_hall.

    Mathematical basis (Eq. from Section 1.3 of the paper):
        a^(ℓ)(x) ∈ ℝ^d  — residual-stream activation at layer ℓ for input x
    """

    def __init__(
        self,
        model_name: str = "mistralai/Mistral-7B-Instruct-v0.2",
        target_layers: List[int] = [12, 16, 20],
        device: str = "cuda" if torch.cuda.is_available() else "cpu"
    ):
        from transformers import AutoModelForCausalLM, AutoTokenizer

        self.device = device
        self.target_layers = target_layers

        logger.info(f"Loading model: {model_name}")
        self.tokenizer = AutoTokenizer.from_pretrained(model_name)
        self.model = AutoModelForCausalLM.from_pretrained(
            model_name,
            torch_dtype=torch.float16 if device == "cuda" else torch.float32,
            device_map="auto" if device == "cuda" else None,
            output_hidden_states=True
        )
        self.model.eval()

        if self.tokenizer.pad_token is None:
            self.tokenizer.pad_token = self.tokenizer.eos_token

        self.hidden_dim = self.model.config.hidden_size
        logger.info(
            f"Model loaded. Hidden dim: {self.hidden_dim}, "
            f"Layers: {self.model.config.num_hidden_layers}"
        )

    @torch.no_grad()
    def extract_activations(
        self,
        text: str,
        pooling: str = "last_token"
    ) -> Dict[int, np.ndarray]:
        """
        Extract residual-stream activations at target layers.

        Args:
            text: Input text to process
            pooling: How to aggregate across sequence positions
                     'last_token' — activation at final token position
                     'mean' — mean-pool across all positions

        Returns:
            Dict mapping layer index → activation vector (numpy, shape [d])
        """
        inputs = self.tokenizer(
            text,
            return_tensors="pt",
            truncation=True,
            max_length=512
        ).to(self.device)

        outputs = self.model(**inputs)
        hidden_states = outputs.hidden_states  # tuple of (batch, seq_len, d)

        activations = {}
        for layer_idx in self.target_layers:
            if layer_idx >= len(hidden_states):
                continue

            h = hidden_states[layer_idx][0]  # (seq_len, d)

            if pooling == "last_token":
                # Use the last non-padding token
                seq_len = inputs["attention_mask"].sum().item()
                act = h[seq_len - 1]  # (d,)
            elif pooling == "mean":
                mask = inputs["attention_mask"][0].unsqueeze(-1).float()
                act = (h * mask).sum(dim=0) / mask.sum(dim=0)
            else:
                raise ValueError(f"Unknown pooling: {pooling}")

            activations[layer_idx] = act.cpu().float().numpy()

        return activations

    def extract_batch(
        self,
        texts: List[str],
        pooling: str = "last_token"
    ) -> Dict[int, np.ndarray]:
        """Extract activations for a batch of texts.
        Returns: Dict[layer_idx -> np.ndarray of shape (N, d)]"""
        all_acts = {layer: [] for layer in self.target_layers}

        for text in texts:
            acts = self.extract_activations(text, pooling)
            for layer in self.target_layers:
                if layer in acts:
                    all_acts[layer].append(acts[layer])

        return {
            layer: np.stack(vecs) for layer, vecs in all_acts.items()
            if len(vecs) > 0
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.3  PERSONA VECTOR COMPUTATION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class PersonaVector:
    """
    The hallucination persona vector v_hall, computed as the mean
    difference between hallucinating and honest activation states.

    Mathematical definition (Eq. 2 from Section 1.3):
        v_hall = (1/N) Σ_{i=1}^{N} [a^(ℓ)(x_i^+) - a^(ℓ)(x_i^-)]

    where x_i^+ elicits confident confabulation and
          x_i^- elicits honest uncertainty.
    """
    vector: np.ndarray          # shape (d,) — the persona direction
    layer: int                  # which transformer layer
    threshold: float            # τ_persona: propensity threshold
    auroc: float = 0.0          # AUROC on calibration set
    n_pairs: int = 0            # number of contrastive pairs used
    ema_decay: float = 0.95     # λ for online updates


class PersonaVectorCalibrator:
    """
    Calibrates the hallucination persona vector from contrastive pairs.

    Pipeline:
    1. Extract activations for all x+ (hallucinating) and x- (honest) texts
    2. Compute v_hall = mean(a(x+) - a(x-)) at each candidate layer
    3. Select the layer with highest AUROC for hallucination detection
    4. Set threshold τ_persona via precision-recall analysis
    """

    def __init__(self, extractor: ActivationExtractor):
        self.extractor = extractor

    def calibrate(
        self,
        pairs: List[ContrastivePair],
        val_fraction: float = 0.2,
        target_recall: float = 0.90
    ) -> PersonaVector:
        """
        Compute and calibrate the persona vector.

        Args:
            pairs: Contrastive pairs (x+, x-)
            val_fraction: Fraction held out for threshold calibration
            target_recall: Minimum recall target for threshold selection

        Returns:
            Calibrated PersonaVector with optimal layer and threshold
        """
        # Split into train/val
        n_val = max(1, int(len(pairs) * val_fraction))
        train_pairs = pairs[:-n_val]
        val_pairs = pairs[-n_val:]

        logger.info(
            f"Calibrating persona vector: {len(train_pairs)} train, "
            f"{len(val_pairs)} val pairs"
        )

        # ── Step 1: Extract activations for train pairs ──
        hall_texts = [p.hallucinating_response for p in train_pairs]
        honest_texts = [p.honest_response for p in train_pairs]

        hall_acts = self.extractor.extract_batch(hall_texts)
        honest_acts = self.extractor.extract_batch(honest_texts)

        # ── Step 2: Compute v_hall at each layer ──
        # v_hall = (1/N) Σ [a(x+) - a(x-)]
        candidates = {}
        for layer in self.extractor.target_layers:
            if layer not in hall_acts or layer not in honest_acts:
                continue

            diff = hall_acts[layer] - honest_acts[layer]  # (N, d)
            v_hall = diff.mean(axis=0)  # (d,)

            # Normalize to unit vector
            v_hall = v_hall / (np.linalg.norm(v_hall) + 1e-8)

            candidates[layer] = v_hall

        # ── Step 3: Evaluate each candidate on val set ──
        val_hall_texts = [p.hallucinating_response for p in val_pairs]
        val_honest_texts = [p.honest_response for p in val_pairs]

        val_hall_acts = self.extractor.extract_batch(val_hall_texts)
        val_honest_acts = self.extractor.extract_batch(val_honest_texts)

        best_layer = None
        best_auroc = 0.0
        best_v_hall = None

        for layer, v_hall in candidates.items():
            if layer not in val_hall_acts or layer not in val_honest_acts:
                continue

            # Compute propensity scores: h(x) = <a^(ℓ)(x), v_hall>
            hall_scores = val_hall_acts[layer] @ v_hall      # (N_val,)
            honest_scores = val_honest_acts[layer] @ v_hall  # (N_val,)

            # Labels: 1 = hallucinating, 0 = honest
            scores = np.concatenate([hall_scores, honest_scores])
            labels = np.concatenate([
                np.ones(len(hall_scores)),
                np.zeros(len(honest_scores))
            ])

            auroc = roc_auc_score(labels, scores)
            logger.info(f"  Layer {layer}: AUROC = {auroc:.4f}")

            if auroc > best_auroc:
                best_auroc = auroc
                best_layer = layer
                best_v_hall = v_hall

        # ── Step 4: Set threshold at target recall ──
        hall_scores = val_hall_acts[best_layer] @ best_v_hall
        honest_scores = val_honest_acts[best_layer] @ best_v_hall

        all_scores = np.concatenate([hall_scores, honest_scores])
        all_labels = np.concatenate([
            np.ones(len(hall_scores)),
            np.zeros(len(honest_scores))
        ])

        precisions, recalls, thresholds = precision_recall_curve(
            all_labels, all_scores
        )

        # Find threshold achieving target_recall with best precision
        valid_mask = recalls[:-1] >= target_recall
        if valid_mask.any():
            idx = np.argmax(precisions[:-1][valid_mask])
            valid_thresholds = thresholds[valid_mask]
            threshold = valid_thresholds[idx]
        else:
            # Fall back to median of hallucination scores
            threshold = np.median(hall_scores)

        logger.info(
            f"Selected layer {best_layer} with AUROC={best_auroc:.4f}, "
            f"threshold={threshold:.4f}"
        )

        return PersonaVector(
            vector=best_v_hall,
            layer=best_layer,
            threshold=threshold,
            auroc=best_auroc,
            n_pairs=len(train_pairs)
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.4  STREAMING HALLUCINATION INTERCEPTOR
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class InterceptorDecision:
    """Decision from the streaming interceptor."""
    should_pause: bool
    propensity_score: float
    consecutive_alarms: int
    message: str = ""


class StreamingHallucinationInterceptor:
    """
    Real-time hallucination interceptor that monitors the residual
    stream during autoregressive generation.

    Protocol (from Section 1.3):
    1. As each token is generated, project current activation onto v_hall
    2. If h(x) > τ_persona for ≥ 3 consecutive tokens → PAUSE
    3. Trigger re-retrieval with stricter relevance threshold
    4. If re-retrieval fails → replace with uncertainty message

    Latency: < 1ms per token (single dot product)
    """

    def __init__(
        self,
        persona_vector: PersonaVector,
        consecutive_threshold: int = 3
    ):
        self.pv = persona_vector
        self.consecutive_threshold = consecutive_threshold

        # State for streaming
        self._consecutive_alarms = 0
        self._history: List[float] = []

    def reset(self):
        """Reset state for a new generation."""
        self._consecutive_alarms = 0
        self._history = []

    def check_token(
        self,
        activation: np.ndarray
    ) -> InterceptorDecision:
        """
        Check a single token's activation against the persona vector.

        Args:
            activation: Residual-stream activation at the persona vector's
                       layer, shape (d,). This is a^(ℓ)(x) for the current
                       token position.

        Returns:
            InterceptorDecision with pause/continue recommendation

        Computation:
            h(x) = <a^(ℓ)(x), v_hall>   — Eq. (3) from Section 1.3
        """
        # Propensity score: dot product with persona vector
        h = float(np.dot(activation, self.pv.vector))
        self._history.append(h)

        # Check against threshold
        if h > self.pv.threshold:
            self._consecutive_alarms += 1
        else:
            self._consecutive_alarms = 0

        should_pause = (
            self._consecutive_alarms >= self.consecutive_threshold
        )

        message = ""
        if should_pause:
            message = (
                f"[L1 ALARM] Hallucination-prone state detected. "
                f"Propensity h={h:.4f} > τ={self.pv.threshold:.4f} "
                f"for {self._consecutive_alarms} consecutive tokens. "
                f"Pausing generation — triggering re-retrieval."
            )
            logger.warning(message)

        return InterceptorDecision(
            should_pause=should_pause,
            propensity_score=h,
            consecutive_alarms=self._consecutive_alarms,
            message=message
        )

    def get_propensity_trace(self) -> List[float]:
        """Return the full propensity trace for the current generation."""
        return self._history.copy()


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.5  ONLINE PERSONA VECTOR UPDATE (EMA)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class PersonaVectorUpdater:
    """
    Updates the persona vector online using exponential moving average
    when new doctor feedback (validated hallucinations) arrives.

    Update rule (from Section 1.6):
        v_hall ← λ · v_hall + (1 - λ) · Δv
    where λ = 0.95 and Δv is computed from the new contrastive pair.
    """

    def __init__(self, persona_vector: PersonaVector):
        self.pv = persona_vector

    def update(
        self,
        hallucination_activation: np.ndarray,
        correct_activation: np.ndarray
    ) -> PersonaVector:
        """
        Update persona vector with a single new feedback pair.

        Args:
            hallucination_activation: a^(ℓ)(x+) for the hallucinated response
            correct_activation: a^(ℓ)(x-) for the corrected response

        Returns:
            Updated PersonaVector
        """
        delta_v = hallucination_activation - correct_activation
        delta_v = delta_v / (np.linalg.norm(delta_v) + 1e-8)

        # EMA update
        new_vector = (
            self.pv.ema_decay * self.pv.vector
            + (1 - self.pv.ema_decay) * delta_v
        )

        # Re-normalize
        new_vector = new_vector / (np.linalg.norm(new_vector) + 1e-8)

        self.pv.vector = new_vector
        self.pv.n_pairs += 1

        logger.info(
            f"Persona vector updated (EMA λ={self.pv.ema_decay}). "
            f"Total pairs: {self.pv.n_pairs}"
        )

        return self.pv


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.6  DEMO / USAGE EXAMPLE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_persona_vector_pipeline():
    """
    End-to-end demo of the persona vector pipeline.

    Steps:
    1. Build contrastive pairs from TruthfulQA
    2. Extract activations
    3. Calibrate persona vector (select best layer + threshold)
    4. Run streaming interceptor on test examples
    5. Show online update from doctor feedback
    """
    print("=" * 70)
    print("MHAI Layer 1: Persona Vector Hallucination Probe")
    print("=" * 70)

    # ── Step 1: Build contrastive pairs ──
    print("\n[1/5] Building contrastive pairs from TruthfulQA...")
    pairs = build_contrastive_pairs_from_truthfulqa(max_pairs=100)
    print(f"  Built {len(pairs)} pairs")
    print(f"  Example pair:")
    print(f"    x+ (hallucinating): {pairs[0].hallucinating_response[:80]}...")
    print(f"    x- (honest):        {pairs[0].honest_response[:80]}...")

    # ── Step 2: Initialize extractor ──
    # NOTE: In production, use Mistral-7B. For demo, use a smaller model.
    print("\n[2/5] Initializing activation extractor...")
    print("  (Using mistralai/Mistral-7B-Instruct-v0.2)")
    print("  Target layers: [12, 16, 20]")

    # Uncomment to run with actual model:
    # extractor = ActivationExtractor(
    #     model_name="mistralai/Mistral-7B-Instruct-v0.2",
    #     target_layers=[12, 16, 20]
    # )

    # ── Step 3: Calibrate persona vector ──
    print("\n[3/5] Calibrating persona vector...")
    print("  Computing v_hall = (1/N) Σ [a(x+) - a(x-)]")
    print("  Evaluating AUROC at each candidate layer...")

    # Uncomment to run:
    # calibrator = PersonaVectorCalibrator(extractor)
    # pv = calibrator.calibrate(pairs, target_recall=0.90)
    # print(f"  Best layer: {pv.layer}")
    # print(f"  AUROC: {pv.auroc:.4f}")
    # print(f"  Threshold τ_persona: {pv.threshold:.4f}")

    # ── Simulated result for demo ──
    print("  [Simulated] Best layer: 16, AUROC: 0.847, τ = 0.312")

    # ── Step 4: Streaming interceptor ──
    print("\n[4/5] Testing streaming interceptor...")
    print("  Monitoring token-by-token propensity h(x) = <a, v_hall>")

    # Simulate a token stream with propensity scores
    simulated_scores = [
        0.1, 0.15, 0.2, 0.18, 0.22,   # Normal generation
        0.35, 0.41, 0.38,              # Rising propensity (approaching alarm)
        0.25, 0.19,                    # Back to normal
        0.45, 0.52, 0.48, 0.55,       # ALARM: 3+ consecutive above threshold
    ]

    # Create a mock persona vector for simulation
    mock_pv = PersonaVector(
        vector=np.random.randn(4096).astype(np.float32),
        layer=16,
        threshold=0.35,
        auroc=0.847,
        n_pairs=100
    )

    interceptor = StreamingHallucinationInterceptor(
        mock_pv, consecutive_threshold=3
    )

    print(f"  {'Token':>6} | {'h(x)':>8} | {'Consec':>6} | Status")
    print("  " + "-" * 50)

    for i, score in enumerate(simulated_scores):
        # Simulate: directly set the propensity score
        mock_activation = mock_pv.vector * score / (
            np.linalg.norm(mock_pv.vector) + 1e-8
        )
        decision = interceptor.check_token(mock_activation)

        status = "🔴 PAUSE" if decision.should_pause else "🟢 OK"
        print(
            f"  {i:>6} | {decision.propensity_score:>8.4f} | "
            f"{decision.consecutive_alarms:>6} | {status}"
        )

        if decision.should_pause:
            print(f"\n  >>> {decision.message}")
            break

    # ── Step 5: Online update ──
    print("\n[5/5] Simulating doctor feedback → EMA update...")
    print(f"  v_hall_old norm: {np.linalg.norm(mock_pv.vector):.4f}")

    updater = PersonaVectorUpdater(mock_pv)
    fake_hall_act = np.random.randn(4096).astype(np.float32)
    fake_correct_act = np.random.randn(4096).astype(np.float32)
    updated_pv = updater.update(fake_hall_act, fake_correct_act)

    print(f"  v_hall_new norm: {np.linalg.norm(updated_pv.vector):.4f}")
    print(f"  Total pairs used: {updated_pv.n_pairs}")

    print("\n" + "=" * 70)
    print("Part 1 complete. Proceed to Part 2 for Semantic Entropy (L2)?")
    print("=" * 70)


if __name__ == "__main__":
    demo_persona_vector_pipeline()
