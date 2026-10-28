"""Structured logging for MHAI — PII-scrubbed JSON output."""
import logging
import os


def setup_logging(level=None):
    if level is None:
        level = os.environ.get("LOG_LEVEL", "INFO")
    if isinstance(level, str):
        level = logging.getLevelNamesMapping().get(level.upper(), logging.INFO)
    logging.basicConfig(
        level=level,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    )

def get_logger(name: str) -> logging.Logger:
    return logging.getLogger(name)
