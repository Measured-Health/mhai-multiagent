"""
FT-Transformer backbone for tabular clinical triage.
Extracted from QDA agent — see agents/qda.py for full pipeline.
"""
from mhai.agents.qda import FTTransformerBackbone, FeatureTokenizer, FTTransformerBlock, QDAMoEClassifier
__all__ = ["FTTransformerBackbone", "FeatureTokenizer", "FTTransformerBlock", "QDAMoEClassifier"]
