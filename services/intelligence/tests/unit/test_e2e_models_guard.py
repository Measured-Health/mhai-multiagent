"""scripts/e2e_models.py refuses to run without --synthetic (and loads no models)."""

import subprocess
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "e2e_models.py"


def test_refuses_without_synthetic_flag(tmp_path):
    out = tmp_path / "out"

    r = subprocess.run(
        [sys.executable, str(SCRIPT), "--out", str(out)],
        capture_output=True,
        text=True,
        timeout=120,
    )

    assert r.returncode != 0
    assert "--synthetic" in r.stderr and "identity and permission checks" in r.stderr
    assert not out.exists()  # it stopped before doing anything
