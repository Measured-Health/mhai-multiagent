"""
Swin Transformer V2 backbone for document layout analysis.
Extracted from DIA agent — see agents/dia.py for full pipeline.
"""
# Model classes are defined inline in agents/dia.py (SwinV2LayoutBackbone, etc.)
# This module re-exports them for standalone use.
from mhai.agents.dia import (
    DIAMoERouter,
    FigureChartExpert,
    OCRTextExpert,
    SwinV2LayoutBackbone,
    TableTSRExpert,
)

__all__ = ["SwinV2LayoutBackbone", "OCRTextExpert", "TableTSRExpert", "FigureChartExpert", "DIAMoERouter"]
