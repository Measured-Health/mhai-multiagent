"""The demo's sample answers (boundary.sample): where they may run, what they
say, and that the real escalation guard still decides. Synthetic data only."""

from __future__ import annotations

import pytest

from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.models import (
    ModelRouter,
    build_model_client,
    build_model_router,
    trust_band,
)
from intelligence.boundary.sample import LABEL, SampleModelClient, compose
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

DEMO_HOSPITAL = "d3d3d3d3-0000-4000-8000-0000000000d3"
OTHER_HOSPITAL = "a11a11a1-0000-4000-8000-0000000000a1"

BASE = {
    "APP_ENV": "development",
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/nonexistent/trusted.json",
    "INTELLIGENCE_MODEL_CLIENT": "sample",
    "INTELLIGENCE_SAMPLE_HOSPITALS": DEMO_HOSPITAL,
}
# APP_ENV=demo is production in every other respect: NKeys, and NATS and
# Postgres without TLS only through the single-host switch.
DEMO = {
    **BASE,
    "APP_ENV": "demo",
    "NATS_URL": "nats://nats:4222",
    "NATS_NKEY_SEED_PATH": "/run/secrets/nats-pipeline.nk",
    "INTELLIGENCE_SINGLE_HOST": "on",
}

MALARIA = KBChunk(
    "KB#0002",
    "Malaria requires parasitological confirmation by microscopy or RDT. Severe features: "
    "Hb < 5, respiratory distress, parasitemia > 10%.",
    "guideline",
    REFERENCE_SOURCE,
    "",
)
HTN = KBChunk(
    "KB#0003",
    "Hypertension defined as systolic >= 140 or diastolic >= 90 on two occasions.",
    "guideline",
    REFERENCE_SOURCE,
    "",
)
PATIENT_ONLY = KBChunk("KB#0099", "Synthetic malaria note for one patient.", "lab", "P-SYN-1", "")


# --- settings ------------------------------------------------------------------------


def test_sample_settings_in_development_and_demo() -> None:
    for env in (BASE, DEMO):
        s = WorkerSettings.from_env(env)
        assert s.model_client == "sample"
        assert s.sample_hospitals == frozenset({DEMO_HOSPITAL})


@pytest.mark.parametrize("app_env", ["production", "staging", ""])
def test_sample_refused_outside_demo_and_development(app_env: str) -> None:
    env = (
        {**DEMO, "APP_ENV": app_env}
        if app_env
        else {k: v for k, v in DEMO.items() if k != "APP_ENV"}
    )
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env(env)
    assert "INTELLIGENCE_MODEL_CLIENT=sample runs only with APP_ENV=demo or development" in str(
        err.value
    )


@pytest.mark.parametrize(
    ("extra", "message"),
    [
        ({"INTELLIGENCE_SAMPLE_HOSPITALS": ""}, "is required"),
        ({"INTELLIGENCE_SAMPLE_HOSPITALS": "not-a-uuid"}, "hospital ids (UUIDs)"),
        (
            {"INTELLIGENCE_SAMPLE_HOSPITALS": ",".join([DEMO_HOSPITAL] * 17)},
            "hospital ids (UUIDs)",
        ),
        ({"INTELLIGENCE_MODEL_ROLES": "cca"}, "never runs with INTELLIGENCE_MODEL_ROLES"),
        (
            {"INTELLIGENCE_CLOUD_PROVIDERS": "anthropic"},
            "never runs with INTELLIGENCE_CLOUD_PROVIDERS",
        ),
    ],
)
def test_sample_settings_refused(extra: dict[str, str], message: str) -> None:
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env({**BASE, **extra})
    assert message in str(err.value)


def test_sample_is_for_the_pipeline_only() -> None:
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env({**BASE, "INTELLIGENCE_SERVICE": "echo"})
    assert "for the pipeline service only" in str(err.value)


def test_sample_hospitals_without_sample_refused() -> None:
    env = {**BASE, "INTELLIGENCE_MODEL_CLIENT": "orchestrator"}
    with pytest.raises(ConfigError) as err:
        WorkerSettings.from_env(env)
    assert "INTELLIGENCE_SAMPLE_HOSPITALS is for INTELLIGENCE_MODEL_CLIENT=sample only" in str(
        err.value
    )


def test_hospital_ids_are_normalised() -> None:
    s = WorkerSettings.from_env({**BASE, "INTELLIGENCE_SAMPLE_HOSPITALS": DEMO_HOSPITAL.upper()})
    assert s.sample_hospitals == frozenset({DEMO_HOSPITAL})


def test_build_refuses_sample_in_production(monkeypatch: pytest.MonkeyPatch) -> None:
    """Checked again when the client is built: settings made by hand can't
    put the sample into a production worker."""
    monkeypatch.setenv("APP_ENV", "production")
    s = WorkerSettings(
        service="pipeline",
        nats_url="nats://x:4222",
        trusted_keys_path="/k",
        model_client="sample",
        sample_hospitals=frozenset({DEMO_HOSPITAL}),
    )
    with pytest.raises(ConfigError):
        build_model_client(s)
    monkeypatch.setenv("APP_ENV", "demo")
    assert isinstance(build_model_client(s), SampleModelClient)


def test_build_refuses_sample_without_hospitals(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("APP_ENV", "demo")
    s = WorkerSettings(
        service="pipeline", nats_url="nats://x:4222", trusted_keys_path="/k", model_client="sample"
    )
    with pytest.raises(ConfigError):
        build_model_client(s)


def test_router_serves_only_the_listed_hospitals() -> None:
    router = build_model_router(WorkerSettings.from_env(BASE))
    assert isinstance(router.local, SampleModelClient)
    assert router.serves(DEMO_HOSPITAL) and router.serves(DEMO_HOSPITAL.upper())
    assert not router.serves(OTHER_HOSPITAL)
    # A cloud route on a sample worker is unconfigured: nothing is sent.
    assert type(router.client_for("cloud:anthropic")[0]).__name__ == "_Unconfigured"
    # Any other router serves every hospital, as before.
    assert ModelRouter(router.local, {}).serves(OTHER_HOSPITAL)


# --- the answers -----------------------------------------------------------------------


def test_compose_quotes_matching_guidelines_with_citations() -> None:
    text, cited = compose("What confirms malaria before treatment?", [HTN, MALARIA])
    assert text.startswith(LABEL)
    assert cited == ("KB#0002",)
    assert "KB#" not in text and "microscopy or RDT" in text
    assert "Hypertension" not in text


def test_compose_without_a_match_cites_nothing() -> None:
    text, cited = compose("Which vaccine schedule applies?", [HTN, MALARIA])
    assert text.startswith(LABEL) and cited == ()
    assert "No reference guideline" in text


def test_generate_uses_only_visible_chunks() -> None:
    c = SampleModelClient()
    g = c.generate(
        "Synthetic malaria question: what confirms malaria?", {}, None, [PATIENT_ONLY, MALARIA]
    )
    assert g.available and g.model_used == "sample"
    assert g.cited_chunk_ids == ("KB#0002",)
    assert (
        "KB#0099" not in g.retrieved_chunk_ids
    )  # a patient's chunk, no patient here and "KB#0099" not in g.chunk_texts
    assert "Generation(" in repr(g) and "microscopy" not in repr(g)


def test_verify_passes_a_plain_question_with_sample_scores() -> None:
    """Every quoted sentence is in a cited chunk: the citation check is 1.0
    and the decision follows from the thresholds, as the real HGA's does."""
    c = SampleModelClient()
    g = c.generate("What confirms malaria before treatment?", {}, None, [MALARIA, HTN])
    v = c.verify(g)
    assert v.verified and v.decision == "PASS" and not v.degraded
    assert v.scores["nli"] == pytest.approx(1.0)
    assert v.trust_score is not None and 0.5 < v.trust_score <= 1.0
    assert trust_band(v) == "high"
    assert set(v.scores) == {"nli", "coherence", "numerical", "se"}
    assert not v.flags


def test_no_matching_guideline_escalates_without_a_confidence() -> None:
    """Live demo bug (20 Nov): a "no guideline matches" answer showed High
    confidence. No source, no score: ESCALATE, unverified, no trust band."""
    c = SampleModelClient()
    g = c.generate("Which vaccine schedule applies?", {}, None, [HTN, MALARIA])
    assert g.available and g.cited_chunk_ids == ()
    assert "No reference guideline" in g.response and g.response.startswith(LABEL)
    v = c.verify(g)
    assert v.decision == "ESCALATE"
    assert v.trust_score is None and not v.verified and v.degraded
    assert trust_band(v) is None
    assert not v.scores


def test_no_chunks_at_all_escalates_without_a_confidence() -> None:
    c = SampleModelClient()
    v = c.verify(c.generate("What confirms malaria?", {}, None, []))
    assert v.decision == "ESCALATE" and v.trust_score is None and trust_band(v) is None


def test_a_claim_outside_the_cited_chunks_does_not_pass() -> None:
    """The sample HGA checks each quoted sentence against the cited chunks:
    an answer whose claims aren't in them can't PASS."""
    from dataclasses import replace

    c = SampleModelClient()
    g = c.generate("What confirms malaria before treatment?", {}, None, [MALARIA, HTN])
    forged = replace(
        g, response=f"{LABEL} Malaria is confirmed by a synthetic blood smell test alone."
    )
    v = c.verify(forged)
    assert v.decision != "PASS"
    assert v.scores["nli"] == pytest.approx(0.0)
    assert ("nli_unsupported", "warning") in v.flags
    assert trust_band(v) != "high"


def test_generate_records_real_elapsed_time() -> None:
    c = SampleModelClient()
    g = c.generate("What confirms malaria before treatment?", {}, None, [MALARIA, HTN])
    assert g.retrieval_ms is not None and g.retrieval_ms > 0.0
    assert g.generation_ms is not None and g.generation_ms > 0.0


def test_verify_runs_the_real_escalation_guard() -> None:
    """A red flag in the question escalates, sample or not."""
    c = SampleModelClient()
    q = "Synthetic case: glucose meter reads HI and the breath smells fruity, DKA?"
    v = c.verify(c.generate(q, {}, None, [MALARIA]))
    assert v.decision == "ESCALATE"
    assert "red_flag" in v.guard_reasons


def test_unavailable_generation_escalates() -> None:
    from intelligence.boundary.models import Generation

    v = SampleModelClient().verify(Generation(question="q", failed_agents=("CCA",)))
    assert v.decision == "ESCALATE" and v.trust_score is None and not v.verified


def test_compose_failure_escalates(monkeypatch: pytest.MonkeyPatch) -> None:
    import intelligence.boundary.sample as sample

    def boom(*a: object, **k: object) -> None:
        raise RuntimeError("synthetic")

    monkeypatch.setattr(sample, "compose", boom)
    g = SampleModelClient().generate("q", {}, None, [MALARIA])
    assert not g.available and g.failed_agents == ("CCA",)


def test_mask_uses_the_real_masker() -> None:
    masked = SampleModelClient().mask("Synthetic patient phone 555-123-4567 has malaria")
    assert "555-123-4567" not in masked and "malaria" in masked


def test_model_versions_say_sample() -> None:
    v = SampleModelClient().model_versions()
    assert v["sample"] == "1" and v["cca"] == "sample-1" and v["escalation_guard"]


def test_the_sample_worker_does_not_load_model_libraries() -> None:
    """The demo host is small: the sample path must not import PyTorch."""
    import subprocess
    import sys

    code = (
        "import sys\n"
        "from intelligence.boundary.sample import SampleModelClient\n"
        "from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk\n"
        "import intelligence.boundary.worker, intelligence.boundary.services.pipeline\n"
        "import intelligence.boundary.jobs.run_query, intelligence.boundary.jobs.approve_correction\n"
        "c = SampleModelClient()\n"
        "k = KBChunk('KB#0002', 'Malaria needs microscopy.', 'guideline', REFERENCE_SOURCE, '')\n"
        "c.verify(c.generate(c.mask('What confirms malaria?'), {}, None, [k]))\n"
        "print(sorted(m for m in ('torch', 'transformers', 'sentence_transformers') if m in sys.modules))\n"
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    assert out.stdout.strip() == "[]"


def test_the_agents_package_still_exports_every_name() -> None:
    import intelligence.agents as agents

    for name in agents.__all__:
        assert getattr(agents, name) is not None
    assert agents.OrchestratorAgent is agents.DynamicOrchestratorAgent
    assert "QDAAgent" in dir(agents)
    with pytest.raises(AttributeError):
        _ = agents.NoSuchAgent  # type: ignore[attr-defined]


def test_question_words_alone_are_no_match() -> None:
    """A guideline sharing only words like "when" or "what" isn't quoted."""
    gdm = KBChunk(
        "KB#0001",
        "Gestational Diabetes Mellitus (GDM) diagnosed when fasting glucose >= 92 mg/dL.",
        "guideline",
        REFERENCE_SOURCE,
        "",
    )
    text, cited = compose("When is blood pressure called hypertension?", [gdm, HTN])
    assert cited == ("KB#0003",) and "Gestational" not in text


@pytest.mark.parametrize(
    "question",
    [
        "What confirms malaria?",
        "How is hypertension staged?",
        "What fasting glucose means diabetes?",
        "When is neonatal jaundice pathological?",
        "Antibiotics for a cough?",
    ],
)
def test_the_demo_library_topics_pass_on_their_citations(question: str) -> None:
    """The questions the no-match answer suggests quote the reference
    guidelines verbatim, so every claim is in a cited chunk and they pass."""
    from intelligence.knowledge_base.kb_builder import MedicalKnowledgeBase

    chunks = [
        KBChunk(f"KB#{i:04d}", g["content"], "guideline", REFERENCE_SOURCE, "")
        for i, g in enumerate(MedicalKnowledgeBase.REFERENCE_GUIDELINES, start=1)
    ]
    c = SampleModelClient()
    g = c.generate(question, {}, None, chunks)
    assert g.cited_chunk_ids
    v = c.verify(g)
    assert v.scores["nli"] == pytest.approx(1.0)
    assert v.verified and v.trust_score is not None
    if not v.guard_reasons:
        assert v.decision == "PASS" and trust_band(v) == "high"
