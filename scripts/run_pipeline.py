#!/usr/bin/env python3
"""Run the full MHAI pipeline on a sample query."""
from mhai.config.settings import get_config
from mhai.utils.logging import setup_logging

if __name__ == "__main__":
    setup_logging()
    config = get_config()
    print("MHAI Pipeline Runner")
    print(f"  NLI: {config.NLI_MODEL}")
    print(f"  CCA: {config.CCA_MODEL}")
    print(f"  Trust weights: α={config.TRUST_ALPHA} β={config.TRUST_BETA} "
          f"γ={config.TRUST_GAMMA} δ={config.TRUST_DELTA}")
