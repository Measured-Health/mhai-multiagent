"""MHAI Agent modules — one per clinical AI agent."""

from intelligence.agents.cca import ClinicalContextAgent
from intelligence.agents.dia import DocumentIngestionAgent
from intelligence.agents.hga_nli import NLIBackedHGA
from intelligence.agents.orchestrator import DynamicOrchestratorAgent
from intelligence.agents.qda import QuickDetectionAgent

# Short aliases used in the README and integration guide.
DIAAgent = DocumentIngestionAgent
QDAAgent = QuickDetectionAgent
CCAAgent = ClinicalContextAgent
HGAAgent = NLIBackedHGA
OrchestratorAgent = DynamicOrchestratorAgent

__all__ = [
    "DIAAgent",
    "QDAAgent",
    "CCAAgent",
    "HGAAgent",
    "OrchestratorAgent",
    "DocumentIngestionAgent",
    "QuickDetectionAgent",
    "ClinicalContextAgent",
    "NLIBackedHGA",
    "DynamicOrchestratorAgent",
]
