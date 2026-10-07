"""MHAI Agent modules — one per clinical AI agent."""
from mhai.agents.cca import ClinicalContextAgent
from mhai.agents.dia import DocumentIngestionAgent
from mhai.agents.hga_nli import NLIBackedHGA
from mhai.agents.orchestrator import DynamicOrchestratorAgent
from mhai.agents.qda import QuickDetectionAgent

# Short aliases used in the README and integration guide.
DIAAgent = DocumentIngestionAgent
QDAAgent = QuickDetectionAgent
CCAAgent = ClinicalContextAgent
HGAAgent = NLIBackedHGA
OrchestratorAgent = DynamicOrchestratorAgent

__all__ = [
    "DIAAgent", "QDAAgent", "CCAAgent", "HGAAgent", "OrchestratorAgent",
    "DocumentIngestionAgent", "QuickDetectionAgent", "ClinicalContextAgent",
    "NLIBackedHGA", "DynamicOrchestratorAgent",
]
