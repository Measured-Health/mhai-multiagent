"""
DeBERTa-v3 cross-encoder MoE for hallucination detection.
See agents/hga_nli.py for the NLI-backed implementation.
"""
from mhai.agents.hga_nli import NLIBackedHGA, NLIEntailmentVerifier

__all__ = ["NLIEntailmentVerifier", "NLIBackedHGA"]
