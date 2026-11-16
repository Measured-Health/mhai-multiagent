"""Per-model answer profiles (intelligence.agents.answer_profile). Synthetic text only."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest

from intelligence.agents import answer_profile as ap
from intelligence.modelstore import training

# The recipe of the first trained answer model (Gemma 4 E4B + QLoRA, 14 Nov 2026).
GEMMA_MANIFEST: dict[str, Any] = {
    "schema": "mhai.models.training/1",
    "name": "mhai-answer",
    "role": "cca",
    "trained_by": "example-trainer",
    "datasets": [
        "medqa-usmle-4-options@1.0.0",
        "medmcqa@1.0.0",
        "pubmedqa-pqa-labeled@1.0.0",
        "symptom-to-diagnosis@1.0.0",
        "cca-v1-rag-cited@1.0.0",
        "cca-v1-abstain@1.0.0",
        "cca-v1-patient-cited@1.0.0",
    ],
    "base_model": {
        "upstream": "google/gemma-4-E4B-it",
        "revision": "4ecc2ba63bcac23ba4a767fc87025b1450f84c9c",
        "licence": "Apache-2.0",
    },
    "framework": {"transformers": "5.23.0"},
    "notebook_metrics": {"abstain_rate": 0.975},
    "created_at": "2026-11-14T16:01:46Z",
}

DECLINE = (
    "INSUFFICIENT EVIDENCE: None of the retrieved knowledge base entries address this "
    "question, so I cannot answer it with verifiable citations. Please consult a clinician."
)
ANSWER = (
    "ANSWER:\n- Malaria requires parasitological confirmation by microscopy or RDT [KB#0002].\n\n"
    "NOTE: All claims are linked to knowledge base entries."
)


def _tm(obj: dict[str, Any]) -> training.TrainingManifest:
    return training.parse(obj)


def _run(tmp_path: Path, obj: dict[str, Any]) -> Path:
    (tmp_path / training.FILE_NAME).write_text(json.dumps(obj), encoding="utf-8")
    return tmp_path


# ---------------------------------------------------------------- selection


def test_the_gemma_recipe_selects_its_profile() -> None:
    assert ap.for_manifest(_tm(GEMMA_MANIFEST)) is ap.GEMMA4_E4B_CCA_V1


def test_dataset_order_does_not_matter() -> None:
    obj = copy.deepcopy(GEMMA_MANIFEST)
    obj["datasets"] = list(reversed(obj["datasets"]))
    assert ap.for_manifest(_tm(obj)) is ap.GEMMA4_E4B_CCA_V1


@pytest.mark.parametrize(
    "change",
    [
        lambda o: o["datasets"].append("cca-v2-abstain@1.0.0"),
        lambda o: o["datasets"].remove("cca-v1-abstain@1.0.0"),
        lambda o: o["base_model"].update(revision="0" * 40),
        lambda o: o["base_model"].update(upstream="google/gemma-4-E2B-it"),
        lambda o: o.update(role="extract"),
    ],
)
def test_any_other_recipe_is_the_default(change: Any) -> None:
    obj = copy.deepcopy(GEMMA_MANIFEST)
    change(obj)
    assert ap.for_manifest(_tm(obj)) is ap.DEFAULT


def test_a_promoted_base_is_the_default() -> None:
    obj = copy.deepcopy(GEMMA_MANIFEST)
    obj["base_model"] = {"promoted": "mhai-answer@0.1.0#" + "a" * 64}
    assert ap.for_manifest(_tm(obj)) is ap.DEFAULT


def test_for_run_reads_the_runs_manifest(tmp_path: Path) -> None:
    assert ap.for_run(_run(tmp_path, GEMMA_MANIFEST)) is ap.GEMMA4_E4B_CCA_V1


def test_for_run_without_a_manifest_is_the_default(tmp_path: Path) -> None:
    assert ap.for_run(tmp_path) is ap.DEFAULT
    (tmp_path / training.FILE_NAME).write_text("{not json", encoding="utf-8")
    assert ap.for_run(tmp_path) is ap.DEFAULT


class _Pinned:
    def __init__(self, path: Path | None) -> None:
        self._path = path

    def path(self, role: str) -> Path:
        if self._path is None or role != "cca":
            raise KeyError(role)
        return self._path


def test_for_pinned_reads_the_pinned_cca(tmp_path: Path) -> None:
    run = _run(tmp_path, GEMMA_MANIFEST)
    assert ap.for_pinned(lambda: _Pinned(run)) is ap.GEMMA4_E4B_CCA_V1


def test_for_pinned_without_a_pin_is_the_default() -> None:
    assert ap.for_pinned(lambda: None) is ap.DEFAULT
    assert ap.for_pinned(lambda: _Pinned(None)) is ap.DEFAULT

    def boom() -> Any:
        raise RuntimeError("no store")

    assert ap.for_pinned(boom) is ap.DEFAULT


# ---------------------------------------------------------------- reading the output


class _RecordingHGA:
    def __init__(self) -> None:
        self.calls: list[str] = []

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        self.calls.append(response_text)
        return {"decision": "pass", "trust_score": 0.9}


def test_profiled_hga_escalates_the_models_decline() -> None:
    inner = _RecordingHGA()
    hga = ap.ProfiledHGA(inner, ap.GEMMA4_E4B_CCA_V1)
    r = hga.verify(response_text=DECLINE, pass_threshold=0.5)
    assert r["decision"] == "escalate"
    assert r["trust_score"] == 0.0
    assert r["flags"] == [{"type": "model_declined", "severity": "info"}]
    assert inner.calls == []


@pytest.mark.parametrize(
    "text",
    [
        "insufficient evidence: no entry covers this.",
        "ANSWER:\n- Metformin [KB#0006].\nINSUFFICIENT EVIDENCE for the dose.",
        "ＩＮＳＵＦＦＩＣＩＥＮＴ EVIDENCE",  # full width
        "INSUFFICIENT  EVIDENCE: two spaces",
        "INSUFFICIENT_EVIDENCE",
    ],
)
def test_a_decline_anywhere_in_any_form_escalates(text: str) -> None:
    """Fail closed: a decline mixed into an answer is still a decline."""
    r = ap.ProfiledHGA(_RecordingHGA(), ap.GEMMA4_E4B_CCA_V1).verify(response_text=text)
    assert r["decision"] == "escalate"


def test_profiled_hga_escalates_an_answer_with_no_citation() -> None:
    inner = _RecordingHGA()
    r = ap.ProfiledHGA(inner, ap.GEMMA4_E4B_CCA_V1).verify(response_text="Just rest at home.")
    assert r["decision"] == "escalate"
    assert r["flags"] == [{"type": "no_citation", "severity": "critical"}]
    assert inner.calls == []


def test_profiled_hga_escalates_an_empty_answer() -> None:
    r = ap.ProfiledHGA(_RecordingHGA(), ap.GEMMA4_E4B_CCA_V1).verify(response_text="  ")
    assert r["decision"] == "escalate"


def test_profiled_hga_verifies_a_cited_answer_as_before() -> None:
    inner = _RecordingHGA()
    r = ap.ProfiledHGA(inner, ap.GEMMA4_E4B_CCA_V1).verify(response_text=ANSWER, patient_id=None)
    assert r == {"decision": "pass", "trust_score": 0.9}
    assert inner.calls == [ANSWER]


def test_the_default_profile_changes_nothing() -> None:
    inner = _RecordingHGA()
    assert ap.wrap_hga(inner, ap.DEFAULT) is inner


def test_the_gemma_profile_wraps_the_hga() -> None:
    # Its citation markers need no NLI wrapper: the citation pipeline strips them for
    # every route (tests/unit/test_nli_citation_tags.py).
    assert isinstance(ap.wrap_hga(_RecordingHGA(), ap.GEMMA4_E4B_CCA_V1), ap.ProfiledHGA)


# ---------------------------------------------------------------- wiring


class _Builds:
    """A builder that records how it was called and returns an orchestrator without a CCA
    (the client then escalates; only the call matters here)."""

    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def __call__(self, chunks: Any, **kwargs: Any) -> Any:
        self.calls.append(kwargs)
        return object()


def test_the_local_client_builds_with_its_profile() -> None:
    from intelligence.boundary.models import OrchestratorModelClient

    build = _Builds()
    client = OrchestratorModelClient(build, masker=object(), profile=lambda: ap.GEMMA4_E4B_CCA_V1)
    client.generate("Synthetic question?", {}, None, [])
    assert build.calls == [{"profile": ap.GEMMA4_E4B_CCA_V1}]


def test_a_client_without_a_profile_builds_as_before() -> None:
    """The cloud clients share the builder: no profile, no keyword."""
    from intelligence.boundary.models import OrchestratorModelClient

    build = _Builds()
    OrchestratorModelClient(build, masker=object()).generate("Synthetic question?", {}, None, [])
    assert build.calls == [{}]


def test_a_failing_profile_escalates() -> None:
    from intelligence.boundary.models import OrchestratorModelClient

    def boom() -> ap.AnswerProfile:
        raise RuntimeError("unreadable")

    client = OrchestratorModelClient(_Builds(), masker=object(), profile=boom)
    gen = client.generate("Synthetic question?", {}, None, [])
    assert not gen.available
    assert client.verify(gen).decision == "ESCALATE"


def test_pinned_profile_reads_the_pin_each_time(tmp_path: Path) -> None:
    from intelligence.boundary.models import pinned_profile

    current: list[Any] = [None]
    profile = pinned_profile(lambda: current[0])
    assert profile() is ap.DEFAULT
    current[0] = _Pinned(_run(tmp_path, GEMMA_MANIFEST))
    assert profile() is ap.GEMMA4_E4B_CCA_V1


def test_the_gate_builds_with_the_runs_profile(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from intelligence.boundary import models
    from intelligence.doceval.answers import gate

    build = _Builds()
    monkeypatch.setattr(models, "local_parts", lambda **kw: (build, {}))
    make = gate.run_builder(_run(tmp_path, GEMMA_MANIFEST), {"dense": tmp_path, "nli": tmp_path})
    make(object())([])
    assert build.calls == [{"profile": ap.GEMMA4_E4B_CCA_V1}]


def test_the_gate_reads_an_unknown_run_with_the_default(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from intelligence.boundary import models
    from intelligence.doceval.answers import gate

    build = _Builds()
    monkeypatch.setattr(models, "local_parts", lambda **kw: (build, {}))
    gate.run_builder(tmp_path, {"dense": tmp_path, "nli": tmp_path})(object())([])
    assert build.calls == [{"profile": ap.DEFAULT}]
