"""
L3: NLI Citation Entailment Verifier
Uses cross-encoder/nli-deberta-v3-base for claim verification.
Full implementation in agents/hga_nli.py (NLIEntailmentVerifier, CitationVerificationPipeline).
"""
from intelligence.agents.hga_nli import (
    CitationVerificationPipeline,
    NLIBackedHGA,
    NLIEntailmentVerifier,
    SemanticEntropyVerifier,
)

__all__ = ["NLIEntailmentVerifier", "CitationVerificationPipeline",
           "SemanticEntropyVerifier", "NLIBackedHGA"]
