"""The learning seam and what the approve_correction code may never do
(Plan 4 spec §4.7, §4.8, §10.6). Unit tests, no Docker."""

from __future__ import annotations

import ast
import math
import re
from pathlib import Path

import pytest

from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.learning import (
    BufferEntry,
    LearningClient,
    Scores,
    StubLearningClient,
    TrustBufferLearningClient,
    build_learning_client,
)

BOUNDARY = Path(__file__).resolve().parents[2] / "intelligence" / "boundary"

BASE_ENV = {
    "APP_ENV": "development",
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/nonexistent/trusted.json",
}


# --- the clients -----------------------------------------------------------------


def test_trust_buffer_clamps_each_score_or_none() -> None:
    c = TrustBufferLearningClient()
    assert isinstance(c, LearningClient)
    e = c.buffer_entry(
        Scores(nli=1.7, coherence=-0.2, numerical=0.4, se=None, trust=0.9, decision="PASS"),
        "REGENERATE",
    )
    assert e == BufferEntry(
        nli=1.0, coherence=0.0, numerical=0.4, se=None, system_decision="REGENERATE"
    )
    odd = c.buffer_entry(
        Scores(nli=math.nan, coherence=math.inf, numerical=True, se="0.5"),  # type: ignore[arg-type]
        "PASS",
    )
    assert (odd.nli, odd.coherence, odd.numerical, odd.se) == (None, None, None, None)


def test_trust_buffer_unknown_decision_is_escalate() -> None:
    c = TrustBufferLearningClient()
    for d in ("pass", "", "anything", None):
        assert c.buffer_entry(Scores(), d).system_decision == "ESCALATE"  # type: ignore[arg-type]
    for d in ("PASS", "REGENERATE", "ESCALATE"):
        assert c.buffer_entry(Scores(), d).system_decision == d


def test_persona_update_is_skipped_in_plan_4() -> None:
    assert TrustBufferLearningClient().persona_update("c0ffee00-0000-4000-8000-000000000001") == (
        "skipped"
    )
    assert StubLearningClient().persona_update("x") == "skipped"


def test_model_versions_fit_the_fact_schema() -> None:
    key = re.compile(r"[A-Za-z0-9_.:/-]{1,64}")
    val = re.compile(r"[A-Za-z0-9_.:/@+-]{1,128}")
    for c in (TrustBufferLearningClient(), StubLearningClient()):
        v = c.model_versions()
        assert v and all(key.fullmatch(k) and val.fullmatch(x) for k, x in v.items())


def test_stub_raises_per_method() -> None:
    boom = RuntimeError("synthetic")
    s = StubLearningClient(raises={"buffer_entry": boom})
    with pytest.raises(RuntimeError):
        s.buffer_entry(Scores(), "PASS")
    assert s.persona_update("x") == "skipped"
    assert s.calls["buffer_entry"] == 1 and s.calls["persona_update"] == 1
    with pytest.raises(ValueError):
        StubLearningClient(raises={"optimize": boom})


# --- the stub only in development (Plan 3 ruling F14) --------------------------------


def test_learning_client_defaults_to_trust_buffer() -> None:
    s = WorkerSettings.from_env(BASE_ENV)
    assert s.learning_client == "trust_buffer"
    assert isinstance(build_learning_client(s), TrustBufferLearningClient)
    assert not isinstance(build_learning_client(s), StubLearningClient)


def test_stub_learning_client_refused_in_production() -> None:
    env = dict(BASE_ENV, INTELLIGENCE_LEARNING_CLIENT="stub")
    assert WorkerSettings.from_env(env).learning_client == "stub"
    for app_env in ("production", "staging", None):
        e = {k: v for k, v in env.items() if k != "APP_ENV"}
        if app_env is not None:
            e["APP_ENV"] = app_env
        with pytest.raises(ConfigError) as err:
            WorkerSettings.from_env(e)
        assert "INTELLIGENCE_LEARNING_CLIENT" in str(err.value)
    # build_learning_client refuses it again, whatever made the settings.
    prod = WorkerSettings(
        service="pipeline", nats_url="nats://x", trusted_keys_path="k", learning_client="stub"
    )
    with pytest.raises(ConfigError):
        build_learning_client(prod)
    dev = WorkerSettings(
        service="pipeline",
        nats_url="nats://x",
        trusted_keys_path="k",
        learning_client="stub",
        development=True,
    )
    assert isinstance(build_learning_client(dev), StubLearningClient)


def test_unknown_learning_client_refused() -> None:
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env(dict(BASE_ENV, INTELLIGENCE_LEARNING_CLIENT="cmaes"))
    assert "INTELLIGENCE_LEARNING_CLIENT" in str(err.value) and "cmaes" not in str(err.value)
    bad = WorkerSettings(
        service="pipeline", nats_url="nats://x", trusted_keys_path="k", learning_client="cmaes"
    )
    with pytest.raises(ConfigError):
        build_learning_client(bad)


# --- C10, code side: nothing writes weights or the knowledge base --------------------


_WRITE = re.compile(r"\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE|ALTER\s+TABLE)\s+(\w+)", re.I)


def _sources() -> dict[str, str]:
    return {
        p.relative_to(BOUNDARY).as_posix(): p.read_text(encoding="utf-8")
        for p in sorted(BOUNDARY.rglob("*.py"))
        if "/pb/" not in p.as_posix()
    }


def test_no_learning_code_writes_weights_or_kb() -> None:
    """C10 (code side): no boundary module writes guardrail_weights or
    kb_chunks, reaches the feedback loop or runs the optimiser. The grant
    side (no write grant for mhai_worker) is core's test."""
    found: list[str] = []
    for rel, text in _sources().items():
        for m in _WRITE.finditer(text):
            if m.group(2).lower() in {"guardrail_weights", "kb_chunks"}:
                found.append(f"{rel}: writes {m.group(2)}")
        for name in ("DoctorFeedbackLoop", "process_feedback", "TrustWeightOptimizer.optimize"):
            if name in text:
                found.append(f"{rel}: names {name}")
        tree = ast.parse(text)
        for node in ast.walk(tree):
            if isinstance(node, ast.Call):
                f = node.func
                called = f.attr if isinstance(f, ast.Attribute) else getattr(f, "id", "")
                if called in {"optimize", "add_correction", "update_persona"}:
                    found.append(f"{rel}:{node.lineno}: calls {called}()")
            elif isinstance(node, ast.ImportFrom) and (node.module or "").startswith(
                ("intelligence.agents", "intelligence.knowledge_base")
            ):
                if rel in {"learning.py", "learning_repo.py", "jobs/approve_correction.py"}:
                    found.append(f"{rel}:{node.lineno}: imports {node.module}")
    assert found == []


def test_the_grep_is_not_vacuous() -> None:
    assert _WRITE.search("UPDATE guardrail_weights SET alpha = 1").group(2) == "guardrail_weights"  # type: ignore[union-attr]
    assert _WRITE.search("insert into kb_chunks (x) values (1)").group(2) == "kb_chunks"  # type: ignore[union-attr]
    for rel in ("learning.py", "learning_repo.py", "jobs/approve_correction.py"):
        assert (BOUNDARY / rel).is_file(), rel


def test_learning_repo_never_names_free_text() -> None:
    """C16 (code side): the job's SQL never names the correction's or the
    job's free text, nor the masked consultation."""
    text = (BOUNDARY / "learning_repo.py").read_text(encoding="utf-8")
    # The module's SQL constants (its docstrings may explain what it avoids).
    sql = " ".join(
        st.value.value
        for st in ast.parse(text).body
        if isinstance(st, ast.Assign)
        and isinstance(st.value, ast.Constant)
        and isinstance(st.value.value, str)
    )
    assert "feedback_events" in sql and "jobs" in sql
    for column in (
        "reason",
        "corrected_diagnosis",
        "rejection_note",
        "cancel_note",
        "cancelled_by",
        "masked_question",
        "masked_patient_json",
        "clinical_question",
        "patient_data_json",
        "response_text",
    ):
        assert not re.search(rf"\b{column}\b", sql), column
