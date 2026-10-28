# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
#
#   MHAI Milestone M4 — Part 1 of 3
#   Dynamic Orchestrator + Doctor Feedback Loop
#
#   Implements:
#     - DynamicOrchestratorAgent: routes queries to agents with
#       adaptive guardrail thresholds based on query type, urgency,
#       and operational context
#     - GuardrailThresholdManager: adjusts tau_persona, tau_SE,
#       trust weights per query category (medication -> strict,
#       lifestyle -> relaxed)
#     - DoctorFeedbackLoop: processes validate/correct signals,
#       propagates updates to persona vector (EMA), trust weights
#       (CMA-ES), and knowledge base (enrichment)
#     - PersonaVectorUpdater: EMA update of v_hall from feedback
#     - TrustWeightOptimizer: CMA-ES optimization of (alpha, beta,
#       gamma, delta) on accumulated corrections
#
#   Prerequisites (M1-M3):
#     - All agents (DIA, QDA, CCA, HGA, OA) from prior milestones
#     - RAGClinicalContextAgent with HybridRetriever
#     - NLIBackedHGA with 4-expert trust score
#     - MedicalKnowledgeBase with indexed chunks
#
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

import time
import json
import os
import re
import hashlib
import uuid
import numpy as np
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from collections import defaultdict, deque
from enum import Enum
import logging

logger = logging.getLogger("MHAI.M4")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.1  QUERY CLASSIFICATION AND URGENCY
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class QueryCategory(Enum):
    """Clinical query categories with different guardrail strictness."""
    MEDICATION_DOSAGE = "medication_dosage"     # Strictest
    DIAGNOSIS = "diagnosis"                     # Strict
    LAB_INTERPRETATION = "lab_interpretation"   # Strict
    TREATMENT_PLAN = "treatment_plan"           # Moderate
    IMAGING_REVIEW = "imaging_review"           # Moderate
    LIFESTYLE_ADVICE = "lifestyle_advice"       # Relaxed
    GENERAL_INFO = "general_info"               # Relaxed
    EMERGENCY = "emergency"                     # Strictest + fast


class UrgencyLevel(Enum):
    """Operational urgency affecting threshold tuning."""
    ROUTINE = "routine"         # Normal thresholds
    URGENT = "urgent"           # Tighter thresholds, skip L2 sampling if needed
    EMERGENCY = "emergency"     # Tightest thresholds, immediate escalation on any flag


@dataclass
class ClassifiedQuery:
    """A query with inferred category and urgency."""
    raw_text: str
    category: QueryCategory
    urgency: UrgencyLevel
    confidence: float
    keywords_matched: List[str]


class QueryClassifier:
    """
    Classifies incoming clinical queries into categories that
    determine guardrail strictness. Uses keyword matching with
    priority rules (emergency keywords override all others).
    """

    CATEGORY_KEYWORDS = {
        QueryCategory.EMERGENCY: [
            "emergency", "stat", "code blue", "cardiac arrest",
            "anaphylaxis", "hemorrhage", "seizure", "stroke",
            "respiratory failure", "shock", "sepsis",
        ],
        QueryCategory.MEDICATION_DOSAGE: [
            "dosage", "dose", "prescribe", "medication", "drug",
            "mg", "tablet", "injection", "infusion", "titrate",
            "contraindication", "interaction",
        ],
        QueryCategory.DIAGNOSIS: [
            "diagnosis", "diagnose", "differential", "rule out",
            "icd-10", "presenting with", "symptoms suggest",
            "most likely", "what condition",
        ],
        QueryCategory.LAB_INTERPRETATION: [
            "lab", "laboratory", "blood test", "cbc", "cmp",
            "hemoglobin", "glucose", "creatinine", "troponin",
            "hba1c", "wbc", "platelet", "bilirubin",
        ],
        QueryCategory.TREATMENT_PLAN: [
            "treatment", "manage", "plan", "therapy", "protocol",
            "follow-up", "monitoring", "prognosis",
        ],
        QueryCategory.IMAGING_REVIEW: [
            "x-ray", "ct scan", "mri", "ultrasound", "imaging",
            "radiograph", "consolidation", "opacity", "lesion",
        ],
        QueryCategory.LIFESTYLE_ADVICE: [
            "diet", "exercise", "lifestyle", "weight loss",
            "smoking cessation", "alcohol", "sleep", "stress",
        ],
    }

    URGENCY_KEYWORDS = {
        UrgencyLevel.EMERGENCY: [
            "emergency", "stat", "immediate", "code", "arrest",
            "acute", "severe", "critical", "life-threatening",
        ],
        UrgencyLevel.URGENT: [
            "urgent", "asap", "today", "worsening", "rapidly",
            "deteriorating", "unstable",
        ],
    }

    def classify(self, query: str) -> ClassifiedQuery:
        """Classify a clinical query by category and urgency."""
        query_lower = query.lower()
        matched_keywords = []

        # Check urgency first (emergency overrides)
        urgency = UrgencyLevel.ROUTINE
        for level in [UrgencyLevel.EMERGENCY, UrgencyLevel.URGENT]:
            for kw in self.URGENCY_KEYWORDS[level]:
                if kw in query_lower:
                    urgency = level
                    matched_keywords.append(f"urgency:{kw}")
                    break
            if urgency != UrgencyLevel.ROUTINE:
                break

        # Check category (priority order: emergency > medication > diagnosis > ...)
        category = QueryCategory.GENERAL_INFO
        best_score = 0
        for cat, keywords in self.CATEGORY_KEYWORDS.items():
            score = sum(1 for kw in keywords if kw in query_lower)
            if score > best_score:
                best_score = score
                category = cat
                matched_keywords.extend(
                    [kw for kw in keywords if kw in query_lower]
                )

        # Emergency urgency forces emergency category
        if urgency == UrgencyLevel.EMERGENCY:
            category = QueryCategory.EMERGENCY

        confidence = min(1.0, best_score / 3.0) if best_score > 0 else 0.3

        return ClassifiedQuery(
            raw_text=query,
            category=category,
            urgency=urgency,
            confidence=confidence,
            keywords_matched=matched_keywords[:10],
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.2  GUARDRAIL THRESHOLD MANAGER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class GuardrailThresholds:
    """Dynamic thresholds for all guardrail layers."""
    # L1: Persona vector
    tau_persona: float = 0.35
    consecutive_alarm_count: int = 3
    # L2: Semantic entropy
    tau_se: float = 1.2
    n_samples_k: int = 5
    # L3: NLI + Trust score
    trust_pass: float = 0.55
    trust_escalate: float = 0.30
    nli_entailment_conf: float = 0.7
    nli_contradiction_conf: float = 0.5
    # L4: Escalation
    auto_escalate_se: float = 2.0
    # HGA weights
    alpha: float = 0.35   # NLI
    beta: float = 0.25    # Coherence
    gamma: float = 0.20   # Numerical
    delta: float = 0.20   # Semantic entropy
    # Penalty
    critical_flag_penalty: float = 0.20
    warning_flag_penalty: float = 0.05


class GuardrailThresholdManager:
    """
    Dynamically adjusts guardrail thresholds based on query category,
    urgency, and operational context.

    From the blueprint (Section 3.5.3):
      - Persona vector threshold: tightened after-hours, relaxed peak hours
      - Semantic entropy threshold: stricter for dosage (tau=0.8),
        relaxed for lifestyle (tau=1.5)
      - Trust score weights: updated via doctor feedback CMA-ES
    """

    # Per-category threshold overrides
    CATEGORY_PROFILES = {
        QueryCategory.EMERGENCY: {
            "tau_persona": 0.25, "tau_se": 0.6,
            "trust_pass": 0.70, "trust_escalate": 0.40,
            "n_samples_k": 3,  # Fewer samples for speed
        },
        QueryCategory.MEDICATION_DOSAGE: {
            "tau_persona": 0.28, "tau_se": 0.8,
            "trust_pass": 0.65, "trust_escalate": 0.35,
            "critical_flag_penalty": 0.25,
        },
        QueryCategory.DIAGNOSIS: {
            "tau_persona": 0.30, "tau_se": 1.0,
            "trust_pass": 0.60, "trust_escalate": 0.30,
        },
        QueryCategory.LAB_INTERPRETATION: {
            "tau_persona": 0.30, "tau_se": 1.0,
            "trust_pass": 0.60,
            "critical_flag_penalty": 0.25,  # Strict on numerical errors
        },
        QueryCategory.TREATMENT_PLAN: {
            "tau_persona": 0.32, "tau_se": 1.1,
            "trust_pass": 0.55,
        },
        QueryCategory.IMAGING_REVIEW: {
            "tau_persona": 0.33, "tau_se": 1.1,
            "trust_pass": 0.55,
        },
        QueryCategory.LIFESTYLE_ADVICE: {
            "tau_persona": 0.40, "tau_se": 1.5,
            "trust_pass": 0.50, "trust_escalate": 0.25,
            "n_samples_k": 3,
        },
        QueryCategory.GENERAL_INFO: {
            "tau_persona": 0.38, "tau_se": 1.4,
            "trust_pass": 0.50,
            "n_samples_k": 3,
        },
    }

    def __init__(self):
        self.base_thresholds = GuardrailThresholds()
        self._override_history = []

    def get_thresholds(
        self,
        classified_query: ClassifiedQuery
    ) -> GuardrailThresholds:
        """
        Compute dynamic thresholds for a classified query.

        Starts from base thresholds and applies category-specific
        overrides. Urgency further tightens thresholds.
        """
        # Start from base
        t = GuardrailThresholds(
            tau_persona=self.base_thresholds.tau_persona,
            consecutive_alarm_count=self.base_thresholds.consecutive_alarm_count,
            tau_se=self.base_thresholds.tau_se,
            n_samples_k=self.base_thresholds.n_samples_k,
            trust_pass=self.base_thresholds.trust_pass,
            trust_escalate=self.base_thresholds.trust_escalate,
            nli_entailment_conf=self.base_thresholds.nli_entailment_conf,
            nli_contradiction_conf=self.base_thresholds.nli_contradiction_conf,
            auto_escalate_se=self.base_thresholds.auto_escalate_se,
            alpha=self.base_thresholds.alpha,
            beta=self.base_thresholds.beta,
            gamma=self.base_thresholds.gamma,
            delta=self.base_thresholds.delta,
            critical_flag_penalty=self.base_thresholds.critical_flag_penalty,
            warning_flag_penalty=self.base_thresholds.warning_flag_penalty,
        )

        # Apply category profile
        profile = self.CATEGORY_PROFILES.get(classified_query.category, {})
        for key, value in profile.items():
            if hasattr(t, key):
                setattr(t, key, value)

        # Urgency multiplier: tighten thresholds further
        if classified_query.urgency == UrgencyLevel.EMERGENCY:
            t.tau_persona *= 0.7     # 30% tighter
            t.tau_se *= 0.5          # 50% tighter
            t.trust_pass = min(t.trust_pass + 0.10, 0.80)
            t.trust_escalate = min(t.trust_escalate + 0.10, 0.50)
            t.consecutive_alarm_count = 2  # Faster trigger
        elif classified_query.urgency == UrgencyLevel.URGENT:
            t.tau_persona *= 0.85
            t.tau_se *= 0.75
            t.trust_pass = min(t.trust_pass + 0.05, 0.75)

        self._override_history.append({
            "category": classified_query.category.value,
            "urgency": classified_query.urgency.value,
            "tau_persona": t.tau_persona,
            "tau_se": t.tau_se,
            "trust_pass": t.trust_pass,
        })

        return t

    def update_base_weights(
        self, alpha: float, beta: float, gamma: float, delta: float
    ):
        """Update base trust weights (called by CMA-ES optimizer)."""
        total = alpha + beta + gamma + delta
        self.base_thresholds.alpha = alpha / total
        self.base_thresholds.beta = beta / total
        self.base_thresholds.gamma = gamma / total
        self.base_thresholds.delta = delta / total
        logger.info(
            f"Base weights updated: alpha={self.base_thresholds.alpha:.3f}, "
            f"beta={self.base_thresholds.beta:.3f}, "
            f"gamma={self.base_thresholds.gamma:.3f}, "
            f"delta={self.base_thresholds.delta:.3f}"
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.3  PERSONA VECTOR UPDATER (EMA)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class PersonaVectorUpdater:
    """
    Updates the hallucination persona vector v_hall via exponential
    moving average when a doctor corrects a hallucinated response.

    From blueprint Section 1.6:
      v_hall <- lambda * v_hall + (1 - lambda) * delta_v / ||delta_v||
      lambda = 0.95

    where delta_v = a(x+_new) - a(x-_new) is the activation difference
    from the new feedback pair.
    """

    def __init__(self, ema_lambda: float = 0.95, d_model: int = 1536):
        self.ema_lambda = ema_lambda
        self.d_model = d_model
        # Initialize persona vector (unit random direction)
        self.v_hall = np.random.randn(d_model)
        self.v_hall /= np.linalg.norm(self.v_hall) + 1e-8
        self.update_count = 0
        self.drift_history = []  # Track vector drift over time

    def update(
        self,
        hallucinated_activation: np.ndarray,
        corrected_activation: np.ndarray,
    ) -> Dict[str, float]:
        """
        Update persona vector from a (hallucinated, corrected) feedback pair.

        Args:
            hallucinated_activation: a^(l)(x+) for the hallucinated response
            corrected_activation: a^(l)(x-) for the corrected response

        Returns:
            Dict with update stats (drift, new norm, etc.)
        """
        # Compute contrastive difference
        delta_v = hallucinated_activation - corrected_activation
        delta_norm = np.linalg.norm(delta_v)

        if delta_norm < 1e-8:
            logger.warning("Zero-norm delta_v; skipping update")
            return {"drift": 0.0, "norm": np.linalg.norm(self.v_hall)}

        # Normalize delta
        delta_v_normed = delta_v / delta_norm

        # Store old for drift computation
        v_old = self.v_hall.copy()

        # EMA update
        self.v_hall = (
            self.ema_lambda * self.v_hall
            + (1 - self.ema_lambda) * delta_v_normed
        )

        # Re-normalize to unit length
        self.v_hall /= np.linalg.norm(self.v_hall) + 1e-8

        # Compute drift
        cosine_sim = float(np.dot(v_old, self.v_hall))
        l2_drift = float(np.linalg.norm(self.v_hall - v_old))

        self.update_count += 1
        self.drift_history.append({
            "update_idx": self.update_count,
            "cosine_sim": cosine_sim,
            "l2_drift": l2_drift,
        })

        return {
            "drift": l2_drift,
            "cosine_sim": cosine_sim,
            "norm": float(np.linalg.norm(self.v_hall)),
            "update_count": self.update_count,
        }

    def compute_propensity(self, activation: np.ndarray) -> float:
        """h(x) = <a^(l)(x), v_hall>"""
        return float(np.dot(activation, self.v_hall))


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.4  TRUST WEIGHT OPTIMIZER (CMA-ES)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class TrustWeightOptimizer:
    """
    Optimizes the HGA trust-score weights (alpha, beta, gamma, delta)
    via Covariance Matrix Adaptation Evolution Strategy (CMA-ES).

    From blueprint Section 1.6:
      - Gradient-free optimizer for the 4D weight simplex
      - Fires every 50 accumulated doctor corrections
      - Objective: minimize mismatches between system PASS decisions
        and doctor corrections (i.e., cases the system should have caught)

    CMA-ES is preferred because:
      - Low-dimensional (d=4)
      - Non-differentiable objective (mismatch count)
      - Noisy evaluations (each correction is one sample)
    """

    def __init__(
        self,
        buffer_size: int = 50,
        sigma0: float = 0.1,
        max_iterations: int = 50,
    ):
        self.buffer_size = buffer_size
        self.sigma0 = sigma0
        self.max_iterations = max_iterations
        self.correction_buffer = []
        self.optimization_history = []
        self.current_weights = {
            "alpha": 0.35, "beta": 0.25, "gamma": 0.20, "delta": 0.20
        }

    def add_correction(
        self,
        expert_scores: Dict[str, float],
        system_decision: str,
        doctor_action: str,  # "validate" or "correct"
    ):
        """
        Add a doctor correction to the buffer.

        Args:
            expert_scores: {"nli": 0.6, "coherence": 0.9, "numerical": 0.8, "se": 0.7}
            system_decision: "pass", "regenerate", or "escalate"
            doctor_action: "validate" (system was right) or "correct" (system was wrong)
        """
        self.correction_buffer.append({
            "expert_scores": expert_scores,
            "system_decision": system_decision,
            "doctor_action": doctor_action,
            "timestamp": time.time(),
        })

        if len(self.correction_buffer) >= self.buffer_size:
            logger.info(
                f"CMA-ES buffer full ({self.buffer_size} corrections). "
                f"Running optimization..."
            )
            return self.optimize()

        return None

    def optimize(self) -> Dict[str, float]:
        """
        Run CMA-ES to find optimal (alpha, beta, gamma, delta).

        Objective: minimize the number of cases where the system
        gave PASS but the doctor corrected (false negatives are
        worse than false positives in medical AI).
        """
        if len(self.correction_buffer) < 10:
            logger.warning("Insufficient corrections for optimization")
            return self.current_weights

        buffer = self.correction_buffer.copy()

        def objective(weights_raw):
            """Mismatch count: system PASS but doctor corrected."""
            # Project to simplex
            w = np.abs(weights_raw)
            w = w / (w.sum() + 1e-8)
            alpha, beta, gamma, delta = w

            mismatches = 0
            for entry in buffer:
                scores = entry["expert_scores"]
                trust = (
                    alpha * scores.get("nli", 0.5)
                    + beta * scores.get("coherence", 0.5)
                    + gamma * scores.get("numerical", 0.5)
                    + delta * scores.get("se", 0.5)
                )
                system_would_pass = trust >= 0.55
                doctor_corrected = entry["doctor_action"] == "correct"

                # Penalize false negatives (system passed, doctor corrected)
                if system_would_pass and doctor_corrected:
                    mismatches += 3.0  # 3x penalty for missed hallucination
                # Penalize false positives (system rejected, doctor validated)
                elif not system_would_pass and not doctor_corrected:
                    mismatches += 1.0

            return mismatches

        # CMA-ES optimization
        try:
            import cma
            x0 = [
                self.current_weights["alpha"],
                self.current_weights["beta"],
                self.current_weights["gamma"],
                self.current_weights["delta"],
            ]
            es = cma.CMAEvolutionStrategy(
                x0, self.sigma0,
                {"maxiter": self.max_iterations, "verbose": -9}
            )
            es.optimize(objective)
            result = es.result.xbest
        except ImportError:
            # Fallback: simple grid search
            logger.info("CMA-ES (cma package) not available; using grid search")
            result = self._grid_search(objective)

        # Normalize to simplex
        w = np.abs(result)
        w = w / (w.sum() + 1e-8)

        self.current_weights = {
            "alpha": float(w[0]),
            "beta": float(w[1]),
            "gamma": float(w[2]),
            "delta": float(w[3]),
        }

        self.optimization_history.append({
            "n_corrections": len(buffer),
            "weights": self.current_weights.copy(),
            "timestamp": time.time(),
        })

        # Clear buffer after optimization
        self.correction_buffer = []

        logger.info(f"CMA-ES optimized weights: {self.current_weights}")
        return self.current_weights

    def _grid_search(self, objective):
        """Simple grid search fallback when CMA-ES is unavailable."""
        best_score = float('inf')
        best_w = [0.35, 0.25, 0.20, 0.20]
        for a in np.arange(0.1, 0.6, 0.1):
            for b in np.arange(0.1, 0.5, 0.1):
                for g in np.arange(0.1, 0.4, 0.1):
                    d = 1.0 - a - b - g
                    if d < 0.05:
                        continue
                    score = objective([a, b, g, d])
                    if score < best_score:
                        best_score = score
                        best_w = [a, b, g, d]
        return best_w


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.5  DOCTOR FEEDBACK LOOP
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class FeedbackEntry:
    """A single doctor feedback event."""
    query_id: str
    query_text: str
    original_response: str
    corrected_response: Optional[str]  # None if validated
    action: str                        # "validate" or "correct"
    expert_scores: Dict[str, float]
    system_decision: str
    timestamp: float = 0.0
    category: str = ""
    submitted_by: str = ""             # user_id of the clinician
    patient_id: Optional[str] = None   # patient the query was about


# User ids that mean "nobody in particular" and must never count as a person.
_PLACEHOLDER_USERS = frozenset({"", "unknown", "anonymous", "system", "none"})


def _normalize_user(user_id) -> str:
    """Canonical user id for identity comparisons; "" if not a real user."""
    if not isinstance(user_id, str):
        return ""
    norm = user_id.strip().casefold()
    return "" if norm in _PLACEHOLDER_USERS else norm


@dataclass
class PendingCorrection:
    """A correction waiting for a second clinician's approval."""
    correction_id: str
    entry: FeedbackEntry
    hallucinated_activation: Optional[np.ndarray] = None
    corrected_activation: Optional[np.ndarray] = None
    status: str = "pending_review"     # "pending_review" or "approved"
    approved_by: str = ""


class DoctorFeedbackLoop:
    """
    Processes doctor validate/correct signals. A correction changes
    nothing on its own: it waits in a review queue until a second,
    different clinician approves it, and only then propagates
    updates to four subsystems:

    1. Persona vector EMA update (v_hall) -- on each correction
    2. CMA-ES trust weight optimization -- every 50 corrections
    3. Knowledge base enrichment -- corrected cases added to KB
    4. RUSBoost calibration update -- corrections shift QDA thresholds

    From blueprint Section 1.6 + Section 3.5.4.
    """

    def __init__(
        self,
        persona_updater: PersonaVectorUpdater,
        trust_optimizer: TrustWeightOptimizer,
        threshold_manager: GuardrailThresholdManager,
        kb=None,  # MedicalKnowledgeBase
        pii_masker=None,  # PIIMasker; scrubs corrections before they reach the KB
    ):
        from intelligence.security.hipaa import PIIMasker

        self.persona_updater = persona_updater
        self.trust_optimizer = trust_optimizer
        self.threshold_manager = threshold_manager
        self.kb = kb
        self.pii = pii_masker or PIIMasker()

        self.feedback_log: List[FeedbackEntry] = []
        self.pending_corrections: Dict[str, PendingCorrection] = {}
        self.n_validations = 0
        self.n_corrections = 0
        self.correction_rate_history = deque(maxlen=100)

    def process_feedback(
        self,
        query_id: str,
        query_text: str,
        original_response: str,
        action: str,
        corrected_response: Optional[str] = None,
        expert_scores: Optional[Dict[str, float]] = None,
        system_decision: str = "pass",
        hallucinated_activation: Optional[np.ndarray] = None,
        corrected_activation: Optional[np.ndarray] = None,
        category: str = "",
        submitted_by: str = "",
        patient_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Process a single doctor feedback event.

        A "correct" action is queued for review and returns its
        correction_id; call approve_correction() to apply it.

        Returns:
            Dict with the action taken and running totals
        """
        entry = FeedbackEntry(
            query_id=query_id,
            query_text=query_text[:200],
            original_response=original_response[:200],
            corrected_response=corrected_response[:200] if corrected_response else None,
            action=action,
            expert_scores=expert_scores or {},
            system_decision=system_decision,
            timestamp=time.time(),
            category=category,
            submitted_by=submitted_by,
            patient_id=patient_id,
        )
        self.feedback_log.append(entry)

        results = {"action": action, "updates": {}}

        if action == "validate":
            self.n_validations += 1
            self.correction_rate_history.append(0)

        elif action == "correct":
            self.n_corrections += 1
            self.correction_rate_history.append(1)

            correction_id = f"corr_{uuid.uuid4().hex}"
            self.pending_corrections[correction_id] = PendingCorrection(
                correction_id=correction_id,
                entry=entry,
                hallucinated_activation=hallucinated_activation,
                corrected_activation=corrected_activation,
            )
            results["correction_id"] = correction_id
            results["status"] = "pending_review"

        # Compute rolling correction rate
        if self.correction_rate_history:
            rolling_rate = sum(self.correction_rate_history) / len(
                self.correction_rate_history
            )
            results["rolling_correction_rate"] = round(rolling_rate, 4)

        results["total_validations"] = self.n_validations
        results["total_corrections"] = self.n_corrections

        return results

    def approve_correction(
        self, correction_id: str, reviewer_id: str
    ) -> Dict[str, Any]:
        """
        Apply a queued correction once a second clinician signs off.

        The reviewer must be identified and must not be the clinician
        who submitted the correction.
        """
        pending = self.pending_corrections.get(correction_id)
        if pending is None:
            return {"error": f"Correction {correction_id} not found"}
        if pending.status != "pending_review":
            return {"error": f"Correction {correction_id} is already {pending.status}"}
        submitter = _normalize_user(pending.entry.submitted_by)
        reviewer = _normalize_user(reviewer_id)
        if not submitter:
            return {"error": "Anonymous corrections cannot be approved"}
        if not reviewer:
            return {"error": "A reviewer must be identified to approve a correction"}
        if reviewer == submitter:
            return {"error": "A correction must be approved by a different clinician"}

        entry = pending.entry
        results = {"correction_id": correction_id, "status": "approved", "updates": {}}

        # 1. Persona vector EMA update
        if (pending.hallucinated_activation is not None
                and pending.corrected_activation is not None):
            pv_result = self.persona_updater.update(
                pending.hallucinated_activation, pending.corrected_activation
            )
            results["updates"]["persona_vector"] = pv_result
            logger.info(
                f"Persona vector updated: drift={pv_result['drift']:.4f}, "
                f"total={pv_result['update_count']}"
            )

        # 2. CMA-ES trust weight accumulation
        if entry.expert_scores:
            cma_result = self.trust_optimizer.add_correction(
                entry.expert_scores, entry.system_decision, "correct"
            )
            if cma_result is not None:
                # CMA-ES ran — update threshold manager
                self.threshold_manager.update_base_weights(
                    cma_result["alpha"], cma_result["beta"],
                    cma_result["gamma"], cma_result["delta"]
                )
                results["updates"]["cma_es"] = cma_result
                logger.info(f"CMA-ES updated weights: {cma_result}")

        # 3. Knowledge base enrichment
        if self.kb is not None and entry.corrected_response:
            self._enrich_kb(entry, approved_by=reviewer_id)
            results["updates"]["kb_enriched"] = True

        pending.status = "approved"
        pending.approved_by = reviewer_id
        return results

    def _enrich_kb(self, entry: FeedbackEntry, approved_by: str):
        """Add an approved correction to the KB as a verified exemplar."""
        from intelligence.knowledge_base.kb_builder import (
            UNASSIGNED_SOURCE,
            KBChunk,
            is_valid_patient_id,
        )

        if self.kb is None:
            return

        query, _ = self.pii.mask(entry.query_text[:100])
        corrected, _ = self.pii.mask(entry.corrected_response[:200])
        original, _ = self.pii.mask(entry.original_response[:100])

        chunk_id = self.kb._generate_id()
        content = f"[VERIFIED CORRECTION] Query: {query} | Corrected: {corrected}"

        # Corrections carry case detail, so they belong to the patient
        # they were made for. Without one they are visible to no query.
        if is_valid_patient_id(entry.patient_id):
            source = entry.patient_id
        else:
            source = UNASSIGNED_SOURCE
        self.kb.chunks[chunk_id] = KBChunk(
            chunk_id=chunk_id,
            content=content,
            chunk_type="verified_correction",
            source_patient_id=source,
            source_subject_id=entry.query_id,
            metadata={
                "original_response": original,
                "category": entry.category,
                "timestamp": entry.timestamp,
                "submitted_by": entry.submitted_by,
                "approved_by": approved_by,
            },
        )

        logger.debug(f"KB enriched with correction {chunk_id}")

    def get_stats(self) -> Dict[str, Any]:
        """Return feedback loop statistics."""
        total = self.n_validations + self.n_corrections
        return {
            "total_feedback": total,
            "validations": self.n_validations,
            "corrections": self.n_corrections,
            "correction_rate": (
                round(self.n_corrections / max(total, 1), 4)
            ),
            "rolling_correction_rate": (
                round(
                    sum(self.correction_rate_history) /
                    max(len(self.correction_rate_history), 1), 4
                )
            ),
            "persona_vector_updates": self.persona_updater.update_count,
            "cma_es_optimizations": len(
                self.trust_optimizer.optimization_history
            ),
            "pending_corrections": len(
                self.trust_optimizer.correction_buffer
            ),
            "corrections_awaiting_review": sum(
                1 for c in self.pending_corrections.values()
                if c.status == "pending_review"
            ),
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.6  DYNAMIC ORCHESTRATOR AGENT
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class DynamicOrchestratorAgent:
    """
    The M4 orchestrator: routes queries through the full pipeline
    with dynamically adjusted guardrail thresholds.

    Pipeline:
      Query -> Classify -> Get thresholds -> Route to agents
      -> Apply guardrails with dynamic thresholds -> Decision
      -> If feedback: propagate to all subsystems

    Key M4 additions over M3:
      1. Query classification determines guardrail strictness
      2. Thresholds adapt per-query (medication strict, lifestyle relaxed)
      3. Feedback loop propagates corrections to all subsystems
      4. Load balancing tracks expert utilization
    """

    def __init__(
        self,
        rag_cca=None,           # RAGClinicalContextAgent
        hga=None,               # NLIBackedHGA
        threshold_manager=None,  # GuardrailThresholdManager
        feedback_loop=None,     # DoctorFeedbackLoop
    ):
        self.classifier = QueryClassifier()
        self.rag_cca = rag_cca
        self.hga = hga
        self.threshold_manager = threshold_manager or GuardrailThresholdManager()
        self.feedback_loop = feedback_loop

        # Load balancing tracking
        self.agent_utilization = defaultdict(int)
        self.query_log = []
        self.total_queries = 0

    def process_query(
        self,
        query_text: str,
        patient_context: Optional[Dict] = None,
        patient_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Process a clinical query through the full M4 pipeline.

        1. Classify query (category + urgency)
        2. Get dynamic thresholds
        3. Route to CCA with RAG
        4. Apply HGA with dynamic thresholds
        5. Make decision (PASS/REGENERATE/ESCALATE)
        6. Return result with confidence badge

        Retrieval and citation checks only see patient_id's records
        plus shared guidelines. If CCA or HGA is missing or fails, the
        result is escalated for clinician review and marked degraded;
        no score is invented for it.
        """
        t_start = time.time()
        self.total_queries += 1

        # Step 1: Classify
        classified = self.classifier.classify(query_text)

        # Step 2: Get dynamic thresholds
        thresholds = self.threshold_manager.get_thresholds(classified)

        failed_agents = []

        # Step 3: Route to CCA
        cca_result = None
        if self.rag_cca is None:
            failed_agents.append("CCA")
        else:
            try:
                cca_result = self.rag_cca.generate_with_rag(
                    query_text, top_k=5,
                    triage_context=patient_context,
                    patient_id=patient_id,
                )
                self.agent_utilization["CCA"] += 1
            except Exception as e:
                logger.warning(f"CCA generation failed: {e}")
                failed_agents.append("CCA")

        # Step 4: Apply HGA with this query's thresholds. They are passed
        # per call, never written onto the shared HGA instance.
        hga_result = None
        if cca_result is not None:
            if self.hga is None:
                failed_agents.append("HGA")
            else:
                try:
                    hga_result = self.hga.verify(
                        response_text=cca_result["response"],
                        patient_id=patient_id,
                        pass_threshold=thresholds.trust_pass,
                        escalate_threshold=thresholds.trust_escalate,
                        critical_penalty=thresholds.critical_flag_penalty,
                        warning_penalty=thresholds.warning_flag_penalty,
                    )
                    self.agent_utilization["HGA"] += 1
                except Exception as e:
                    logger.warning(f"HGA verification failed: {e}")
                    failed_agents.append("HGA")

        # Step 5: Decision + badge
        badge_map = {
            "pass": ("GREEN", "High confidence"),
            "regenerate": ("AMBER", "Review recommended"),
            "escalate": ("RED", "Requires clinician validation"),
        }
        if hga_result is None:
            # Fail closed: an unverified or missing answer goes to a human.
            decision = "escalate"
            trust_score = 0.0
            expert_scores = {}
        else:
            decision = hga_result.get("decision")
            if decision not in badge_map:
                decision = "escalate"
            trust_score = hga_result["trust_score"]
            expert_scores = {
                "nli": hga_result.get("nli_score", 0.5),
                "coherence": hga_result.get("coherence_score", 1.0),
                "numerical": hga_result.get("numerical_score", 0.85),
                "se": hga_result.get("semantic_entropy_score", 0.7),
            }
        badge, badge_text = badge_map[decision]

        total_ms = (time.time() - t_start) * 1000

        result = {
            # Random, so a query can't be addressed by guessing its id.
            "query_id": f"q_{uuid.uuid4().hex}",
            "query_text": query_text[:200],
            "patient_id": patient_id,
            "category": classified.category.value,
            "urgency": classified.urgency.value,
            # Thresholds applied
            "thresholds": {
                "tau_persona": thresholds.tau_persona,
                "tau_se": thresholds.tau_se,
                "trust_pass": thresholds.trust_pass,
            },
            # CCA output
            "response": cca_result["response"] if cca_result else "",
            "response_available": cca_result is not None,
            "n_citations": cca_result.get("n_citations", 0) if cca_result else 0,
            # HGA output
            "trust_score": trust_score,
            "decision": decision,
            "badge": badge,
            "badge_text": badge_text,
            "verified": hga_result is not None,
            "degraded": bool(failed_agents),
            "failed_agents": failed_agents,
            "expert_scores": expert_scores,
            # Performance
            "total_latency_ms": round(total_ms, 1),
        }

        self.query_log.append(result)
        return result

    def process_feedback(
        self, query_id: str, action: str,
        corrected_response: Optional[str] = None,
        hallucinated_activation: Optional[np.ndarray] = None,
        corrected_activation: Optional[np.ndarray] = None,
        submitted_by: str = "",
    ) -> Dict[str, Any]:
        """
        Route doctor feedback through the feedback loop.

        This method trusts submitted_by; it does no access control of its
        own. Clinician-facing code must go through SecurePipelineWrapper,
        which checks the session and supplies the user id.
        """
        if self.feedback_loop is None:
            return {"error": "Feedback loop not configured"}
        if action not in ("validate", "correct"):
            return {"error": f"Unknown feedback action: {action!r}"}

        # Find the original query in log
        original = None
        for entry in self.query_log:
            if entry["query_id"] == query_id:
                original = entry
                break

        if original is None:
            return {"error": f"Query {query_id} not found in log"}

        return self.feedback_loop.process_feedback(
            query_id=query_id,
            query_text=original["query_text"],
            original_response=original["response"],
            action=action,
            corrected_response=corrected_response,
            expert_scores=original.get("expert_scores"),
            system_decision=original["decision"],
            hallucinated_activation=hallucinated_activation,
            corrected_activation=corrected_activation,
            category=original.get("category", ""),
            submitted_by=submitted_by,
            patient_id=original.get("patient_id"),
        )

    def approve_correction(
        self, correction_id: str, reviewer_id: str
    ) -> Dict[str, Any]:
        """Apply a queued correction (see DoctorFeedbackLoop)."""
        if self.feedback_loop is None:
            return {"error": "Feedback loop not configured"}
        return self.feedback_loop.approve_correction(correction_id, reviewer_id)

    def get_load_balance(self) -> Dict[str, Any]:
        """Check agent utilization for load-balance compliance."""
        total = sum(self.agent_utilization.values()) or 1
        utilization = {
            agent: round(count / total, 3)
            for agent, count in self.agent_utilization.items()
        }
        # Check if any agent exceeds 40%
        overloaded = {
            agent: pct for agent, pct in utilization.items()
            if pct > 0.40
        }
        return {
            "utilization": utilization,
            "total_queries": self.total_queries,
            "overloaded_agents": overloaded,
            "load_balanced": len(overloaded) == 0,
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 4.7  M4 PART 1 DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_m4_part1():
    """M4 Part 1: Dynamic Orchestrator + Feedback Loop."""
    print("=" * 70)
    print("MHAI Milestone M4 --- Part 1: Dynamic Orchestrator + Feedback Loop")
    print("=" * 70)

    # ── Step 1: Query classification ──
    print("\n[1/5] Testing query classification...")
    classifier = QueryClassifier()
    test_queries = [
        "What is the correct dosage of metformin for this patient?",
        "Patient presenting with acute chest pain, stat ECG needed",
        "What diet changes would you recommend for weight management?",
        "Lab results show troponin 3.2 ng/mL -- interpret",
        "What is the differential diagnosis for chronic cough?",
    ]

    for q in test_queries:
        c = classifier.classify(q)
        print(f"  [{c.category.value:>22}] [{c.urgency.value:>10}] "
              f"conf={c.confidence:.2f}  {q[:50]}...")

    # ── Step 2: Dynamic thresholds ──
    print("\n[2/5] Testing dynamic threshold management...")
    threshold_mgr = GuardrailThresholdManager()

    for q in test_queries[:3]:
        classified = classifier.classify(q)
        t = threshold_mgr.get_thresholds(classified)
        print(f"  {classified.category.value:>22}: "
              f"tau_p={t.tau_persona:.3f}, tau_se={t.tau_se:.2f}, "
              f"trust_pass={t.trust_pass:.2f}")

    # ── Step 3: Persona vector updater ──
    print("\n[3/5] Testing persona vector EMA update...")
    pv_updater = PersonaVectorUpdater(ema_lambda=0.95, d_model=128)
    print(f"  Initial norm: {np.linalg.norm(pv_updater.v_hall):.4f}")

    for i in range(3):
        hall_act = np.random.randn(128)
        corr_act = np.random.randn(128)
        result = pv_updater.update(hall_act, corr_act)
        print(f"  Update {i+1}: drift={result['drift']:.4f}, "
              f"cos_sim={result['cosine_sim']:.4f}, "
              f"norm={result['norm']:.4f}")

    # ── Step 4: CMA-ES trust weight optimizer ──
    print("\n[4/5] Testing CMA-ES trust weight optimization...")
    optimizer = TrustWeightOptimizer(buffer_size=10)

    # Simulate 10 corrections
    for i in range(10):
        scores = {
            "nli": np.random.uniform(0.3, 0.9),
            "coherence": np.random.uniform(0.7, 1.0),
            "numerical": np.random.uniform(0.5, 1.0),
            "se": np.random.uniform(0.4, 0.9),
        }
        action = "correct" if np.random.random() < 0.4 else "validate"
        result = optimizer.add_correction(scores, "pass", action)
        if result is not None:
            print(f"  CMA-ES optimized: {result}")

    # ── Step 5: Full feedback loop ──
    print("\n[5/5] Testing complete feedback loop...")
    feedback_loop = DoctorFeedbackLoop(
        persona_updater=pv_updater,
        trust_optimizer=optimizer,
        threshold_manager=threshold_mgr,
    )

    # Simulate feedback
    for i in range(5):
        action = "correct" if i % 3 == 0 else "validate"
        fb = feedback_loop.process_feedback(
            query_id=f"test_{i:03d}",
            query_text=f"Test query {i}",
            original_response=f"Test response {i}",
            action=action,
            corrected_response=f"Corrected response {i}" if action == "correct" else None,
            expert_scores={
                "nli": 0.6, "coherence": 0.9,
                "numerical": 0.8, "se": 0.7
            },
            system_decision="pass",
            hallucinated_activation=np.random.randn(128) if action == "correct" else None,
            corrected_activation=np.random.randn(128) if action == "correct" else None,
        )
        print(f"  Feedback {i}: {action}, "
              f"corrections={fb['total_corrections']}")

    stats = feedback_loop.get_stats()
    print(f"\n  Feedback stats: {json.dumps(stats, indent=4)}")

    # ── Dynamic orchestrator demo ──
    print("\n  Dynamic Orchestrator routing demo...")
    orchestrator = DynamicOrchestratorAgent(
        threshold_manager=threshold_mgr,
        feedback_loop=feedback_loop,
    )

    for q in test_queries:
        result = orchestrator.process_query(q)
        print(f"    [{result['badge']:>6}] [{result['category']:>22}] "
              f"T={result['trust_score']:.3f} "
              f"({result['total_latency_ms']:.0f}ms) "
              f"{q[:40]}...")

    lb = orchestrator.get_load_balance()
    print(f"\n  Load balance: {lb}")

    print("\n" + "=" * 70)
    print("M4 Part 1 complete. Proceed to Part 2 for security/HIPAA")
    print("compliance, PII masking, and performance benchmarking?")
    print("=" * 70)


if __name__ == "__main__":
    demo_m4_part1()
