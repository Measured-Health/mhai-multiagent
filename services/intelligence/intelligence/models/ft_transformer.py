"""
FT-Transformer backbone for tabular clinical triage.
Extracted from QDA agent — see agents/qda.py for full pipeline.
"""
from intelligence.agents.qda import (
    FeatureTokenizer,
    FTTransformerBackbone,
    FTTransformerBlock,
    QDAMoEClassifier,
)

__all__ = ["FTTransformerBackbone", "FeatureTokenizer", "FTTransformerBlock", "QDAMoEClassifier"]
