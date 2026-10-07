"""
MHAI Configuration — Pydantic-style typed config.
All hyperparameters, model names, thresholds in one place.
"""
import os
from dataclasses import dataclass, field
from typing import List, Dict

@dataclass
class MHAIConfig:
    """Central configuration for the MHAI system."""
    # Models
    NLI_MODEL: str = field(default_factory=lambda: os.environ.get(
        "NLI_MODEL", "cross-encoder/nli-deberta-v3-base"))
    CCA_MODEL: str = field(default_factory=lambda: os.environ.get(
        "CCA_MODEL", "Qwen/Qwen2.5-7B-Instruct"))
    DENSE_MODEL: str = field(default_factory=lambda: os.environ.get(
        "DENSE_MODEL", "all-MiniLM-L6-v2"))
    DIA_MODEL: str = field(default_factory=lambda: os.environ.get(
        "DIA_MODEL", "microsoft/swinv2-base-patch4-window8-256"))
    # Where the CCA LLM runs: "ollama" (the compose service) or
    # "transformers" (CCA_MODEL loaded in-process).
    CCA_BACKEND: str = field(default_factory=lambda: os.environ.get(
        "CCA_BACKEND", "ollama"))
    OLLAMA_URL: str = field(default_factory=lambda: os.environ.get(
        "OLLAMA_URL", "http://ollama:11434"))
    OLLAMA_MODEL: str = field(default_factory=lambda: os.environ.get(
        "OLLAMA_MODEL", "qwen2.5:7b-instruct"))

    # Trust weights (HGA 4-expert)
    TRUST_ALPHA: float = 0.35  # NLI
    TRUST_BETA: float = 0.25   # Coherence
    TRUST_GAMMA: float = 0.20  # Numerical
    TRUST_DELTA: float = 0.20  # Semantic Entropy

    # Thresholds
    TRUST_PASS: float = 0.55
    TRUST_REGEN: float = 0.30
    SE_THRESHOLD_RAG: float = 0.8
    SE_THRESHOLD_UNGROUNDED: float = 1.2
    SE_K_SAMPLES: int = 5
    SSIM_THRESHOLD: float = 0.85
    QDA_CONFIDENCE_LOW: float = 0.35
    QDA_CONFIDENCE_HIGH: float = 0.65

    # Feedback
    PERSONA_ETA: float = 0.05
    CMA_ES_MIN_CORRECTIONS: int = 50

    # Flag penalties
    FLAG_PENALTY_CRITICAL: float = 0.20
    FLAG_PENALTY_WARNING: float = 0.05

    # Retrieval
    DENSE_WEIGHT: float = 0.6
    SPARSE_WEIGHT: float = 0.4
    RRF_K: int = 60
    RETRIEVAL_TOP_K: int = 5

    # Rate limits
    QWEN_RPM: int = 15
    QWEN_TPM: int = 30000
    DEBERTA_RPM: int = 120

    # Per-category threshold profiles (from M4 Part 1)
    CATEGORY_THRESHOLDS: Dict[str, Dict] = field(default_factory=lambda: {
        "medication_dosage": {"tau_p": 0.280, "tau_se": 0.80, "trust_pass": 0.65},
        "emergency":         {"tau_p": 0.175, "tau_se": 0.30, "trust_pass": 0.80},
        "treatment_plan":    {"tau_p": 0.320, "tau_se": 1.10, "trust_pass": 0.55},
        "lab_interpretation": {"tau_p": 0.250, "tau_se": 0.90, "trust_pass": 0.60},
        "diagnosis":         {"tau_p": 0.300, "tau_se": 1.00, "trust_pass": 0.55},
        "triage":            {"tau_p": 0.200, "tau_se": 0.50, "trust_pass": 0.70},
        "follow_up":         {"tau_p": 0.350, "tau_se": 1.20, "trust_pass": 0.50},
        "general":           {"tau_p": 0.350, "tau_se": 1.20, "trust_pass": 0.50},
    })

    # Disease categories for QDA
    DISEASE_CATEGORIES: List[str] = field(default_factory=lambda: [
        "urti", "gdm", "diabetes", "malaria", "stroke",
        "mi", "hypertension", "pneumonia"
    ])


def get_config() -> MHAIConfig:
    return MHAIConfig()
