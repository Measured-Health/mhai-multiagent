"""The security module keeps the PII masker, encryption and the profiler.

Roles, permissions and the audit chain are core's (spec §17). The PII
masker has its own tests in tests/security/test_pii_masker.py.
"""

import importlib
import pathlib
import re

import pytest

import intelligence
from intelligence.security import hipaa

GONE = ("RBACManager", "AuditLogger", "SecurePipelineWrapper", "UserRole", "UserSession")


@pytest.mark.parametrize("name", GONE)
def test_no_python_rbac_or_audit(name):
    with pytest.raises(ImportError):
        exec(f"from intelligence.security.hipaa import {name}")
    assert not hasattr(hipaa, name)


def test_nothing_in_the_package_names_the_old_security_classes():
    root = pathlib.Path(intelligence.__file__).parent
    pattern = re.compile("|".join(GONE))
    hits = [
        str(p.relative_to(root))
        for p in root.rglob("*.py")
        if pattern.search(p.read_text(encoding="utf-8"))
    ]
    assert hits == []


def test_the_queue_stubs_are_gone():
    with pytest.raises(ImportError):
        importlib.import_module("intelligence.queue")


def test_what_stays():
    masked, found = hipaa.PIIMasker().mask("Call 555-123-4567, SSN 123-45-6789.")
    assert "123-45-6789" not in masked and found
    enc = hipaa.EncryptionManager()
    assert enc.decrypt_dict(enc.encrypt_dict({"a": 1})) == {"a": 1}
    assert hipaa.LatencyProfiler().measurements == []
