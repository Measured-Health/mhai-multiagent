"""MHAI Agent modules — one per clinical AI agent.

The names load on first use (PEP 562), so importing one agent module (the
orchestrator, say) doesn't import the others and their model libraries: the
demo's sample worker (boundary.sample) uses the orchestrator's classifier and
thresholds without loading PyTorch.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from intelligence.agents.cca import ClinicalContextAgent
    from intelligence.agents.dia import DocumentIngestionAgent
    from intelligence.agents.hga_nli import NLIBackedHGA
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.agents.qda import QuickDetectionAgent

# Short aliases used in the README and integration guide.
_ALIASES = {
    "DIAAgent": "DocumentIngestionAgent",
    "QDAAgent": "QuickDetectionAgent",
    "CCAAgent": "ClinicalContextAgent",
    "HGAAgent": "NLIBackedHGA",
    "OrchestratorAgent": "DynamicOrchestratorAgent",
}

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


def _load(name: str) -> Any:
    # Plain imports, no dynamic lookup (tests/boundary/test_no_direct_db.py).
    if name == "ClinicalContextAgent":
        from intelligence.agents.cca import ClinicalContextAgent

        return ClinicalContextAgent
    if name == "DocumentIngestionAgent":
        from intelligence.agents.dia import DocumentIngestionAgent

        return DocumentIngestionAgent
    if name == "NLIBackedHGA":
        from intelligence.agents.hga_nli import NLIBackedHGA

        return NLIBackedHGA
    if name == "DynamicOrchestratorAgent":
        from intelligence.agents.orchestrator import DynamicOrchestratorAgent

        return DynamicOrchestratorAgent
    if name == "QuickDetectionAgent":
        from intelligence.agents.qda import QuickDetectionAgent

        return QuickDetectionAgent
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def __getattr__(name: str) -> Any:
    value = _load(_ALIASES.get(name, name))
    globals()[name] = value
    return value


def __dir__() -> list[str]:
    return sorted([*globals(), *__all__])
