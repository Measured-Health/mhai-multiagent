"""Compute-device detection (CUDA / MPS / CPU)."""
import logging
log = logging.getLogger(__name__)

def detect_device() -> str:
    try:
        import torch
    except ImportError:
        return "cpu"
    if torch.cuda.is_available():
        return "cuda"
    if hasattr(torch.backends, "mps") and torch.backends.mps.is_available():
        return "mps"
    return "cpu"
