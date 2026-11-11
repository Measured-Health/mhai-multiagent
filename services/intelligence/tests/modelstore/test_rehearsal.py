"""``scripts/rehearse-promotion.sh`` (plan Task 24), run against the store tests' MinIO.

The script is CI's ``model-promotion-e2e`` rehearsal: the whole promotion flow with a
development key. This runs it once, as both store roles, and checks every stage's JSON
summary.
Synthetic data only; the model name and test split version are random, so the run doesn't
meet the other store tests' objects in the shared bucket.
"""

from __future__ import annotations

import json
import os
import re
import secrets
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from tests.modelstore.conftest import MinioStore

SERVICE = Path(__file__).resolve().parents[2]
SCRIPT = SERVICE / "scripts" / "rehearse-promotion.sh"
ROLE = "extract"
SHA256 = re.compile(r"[0-9a-f]{64}")

STAGES = [
    "keys",
    "testset",
    "dry-run-0.1.0",
    "promote-0.1.0",
    "pin-0.1.0",
    "lock-check-0.1.0",
    "load-0.1.0",
    "tamper-store-down",
    "tamper-refetched",
    "dry-run-0.2.0",
    "promote-0.2.0",
    "pin-0.2.0",
    "lock-check-0.2.0",
    "load-0.2.0",
    "rollback",
    "lock-check-rollback",
    "load-rollback",
]


def _env(m: MinioStore, name: str, testset_version: str) -> dict[str, str]:
    env = {
        "PATH": f"{Path(sys.executable).parent}{os.pathsep}{os.environ.get('PATH', '')}",
        "HOME": os.environ.get("HOME", "/tmp"),
        "MODEL_STORE_ENDPOINT": m.endpoint,
        "MODEL_STORE_BUCKET": m.bucket,
        "REHEARSAL_NAME": name,
        "REHEARSAL_TESTSET_VERSION": testset_version,
    }
    for role in ("writer", "reader"):
        var = role.upper()
        env[f"MODEL_STORE_{var}_ACCESS_KEY"] = m.users[role].access_key
        env[f"MODEL_STORE_{var}_SECRET_KEY"] = m.users[role].secret_key
    return env


def _stages(stdout: str) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for line in stdout.splitlines():
        stage, _, summary = line.partition(" ")
        assert stage not in out, stage
        out[stage] = json.loads(summary)
    return out


def _pin(name: str, version: str) -> dict[str, Any]:
    return {"name": name, "version": version, "gate": "rehearsal"}


@pytest.mark.store
def test_rehearse_promotion_end_to_end(minio_store: MinioStore) -> None:
    name = f"fixture-tiny-{secrets.token_hex(3)}"
    # The synthetic-data scanner refuses a run of 6 or more digits in a split's version.
    testset_version = f"9.{secrets.randbelow(100)}.{secrets.randbelow(100)}"
    done = subprocess.run(
        ["bash", str(SCRIPT)],
        env=_env(minio_store, name, testset_version),
        cwd=SERVICE,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=1800,
        check=False,
    )
    assert done.returncode == 0, (done.stdout[-4000:], done.stderr[-4000:])
    s = _stages(done.stdout)
    assert list(s) == STAGES
    # No credential reaches the output.
    for role in minio_store.users.values():
        assert role.secret_key not in done.stdout and role.secret_key not in done.stderr

    assert set(s["keys"]) == {"key_id"} and SHA256.fullmatch(s["keys"]["key_id"])
    assert re.fullmatch(
        rf"clinical_document@{re.escape(testset_version)}#[0-9a-f]{{64}}", s["testset"]["lock_line"]
    )

    manifests: dict[str, str] = {}
    for version in ("0.1.0", "0.2.0"):
        dry, promoted = s[f"dry-run-{version}"], s[f"promote-{version}"]
        assert dry["dry_run"] is True and dry["manifest_sha256"] is None
        assert SHA256.fullmatch(dry["files_sha256"])
        assert promoted["dry_run"] is False
        assert promoted["files_sha256"] == dry["files_sha256"]
        assert promoted["split"] == dry["split"] == "test"  # the locked split, not dev
        assert SHA256.fullmatch(promoted["manifest_sha256"])
        manifests[version] = promoted["manifest_sha256"]
        pinned = s[f"pin-{version}"]["pinned"]
        assert pinned == {
            "role": ROLE,
            **_pin(name, version),
            "manifest_sha256": promoted["manifest_sha256"],
        }
        assert s[f"lock-check-{version}"] == {"lock_check": "pass", "roles": {ROLE: "new"}}
    assert s["dry-run-0.1.0"]["files_sha256"] != s["dry-run-0.2.0"]["files_sha256"]

    def loaded(version: str) -> dict[str, Any]:
        return {
            "ready": True,
            "model_versions": {ROLE: f"{name}@{version}#{manifests[version][:12]}"},
            "refused": [],
        }

    assert s["load-0.1.0"] == loaded("0.1.0")
    # A tampered cached copy is never served on its own: with the store out of reach the
    # pipeline is not ready; with it, the copy is refused and fetched again.
    assert s["tamper-store-down"] == {
        "ready": False,
        "model_versions": {},
        "refused": [f"{ROLE}:store_unavailable"],
    }
    assert s["tamper-refetched"] == loaded("0.1.0")
    assert s["load-0.2.0"] == loaded("0.2.0")

    back = s["rollback"]["rollback"]
    assert back["role"] == ROLE
    assert back["from"] == {**_pin(name, "0.2.0"), "manifest_sha256": manifests["0.2.0"]}
    assert back["to"] == {**_pin(name, "0.1.0"), "manifest_sha256": manifests["0.1.0"]}
    assert s["lock-check-rollback"] == {"lock_check": "pass", "roles": {ROLE: "rollback"}}
    assert s["load-rollback"] == loaded("0.1.0")
