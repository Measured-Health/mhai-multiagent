"""Fixtures for the cloud provider tests.

Every key here is synthetic and obviously fake: built from repeated
characters at run time, never a real key and never a literal secret.
"""

from __future__ import annotations

import os
from collections.abc import Callable
from pathlib import Path

import pytest

# Fake keys. One has a provider's prefix (the redaction patterns catch it);
# one has none (only the exact-value rule catches it).
FAKE_KEY = "fake" + "0" * 24
FAKE_SK_KEY = "sk-" + "f" * 24


@pytest.fixture
def key_file(tmp_path: Path) -> Callable[..., Path]:
    """Write a key file with the given contents and mode (0400 by default)."""
    n = 0

    def make(contents: str = FAKE_KEY + "\n", mode: int = 0o400) -> Path:
        nonlocal n
        n += 1
        p = tmp_path / f"key-{n}"
        p.write_text(contents, encoding="utf-8")
        os.chmod(p, mode)
        return p

    return make
