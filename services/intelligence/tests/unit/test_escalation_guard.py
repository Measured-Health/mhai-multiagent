"""The escalation guard's rule set and checks (guardrails/escalation_guard.py, ADR 0016).

Every question, value and chunk here is synthetic and written for these tests, in many
phrasings: none is copied from the answer gate's bank (doceval/answers/bank.py), whose
split stays held out. A red flag, missing information or an out-of-scope dose must
escalate whatever the answer says; the hard negatives (a negated sign, a general question,
a normal value, a dose the chunk gives) must not.
"""

from __future__ import annotations

import hashlib
import re
from pathlib import Path
from typing import Any

import pytest

from intelligence.guardrails import escalation_guard as eg

RULES = eg.default()
METFORMIN = "Type 2 diabetes first-line: Metformin 500 mg twice daily with meals."
MALARIA = "Malaria needs confirmation by microscopy or RDT before treatment."
URTI = "Most upper respiratory infections are viral. Give fluids and paracetamol for comfort."
OGTT = "Gestational diabetes is diagnosed on a 75 g OGTT when fasting glucose is 92 mg/dL or more."
BP = "Hypertension is a blood pressure of 140/90 or more on two separate visits."


def verdict(question: str, patient: dict[str, Any] | None = None, cited: tuple[str, ...] = ()):
    return eg.check_question(RULES, question, patient or {}, list(cited))


def reasons(question: str, patient: dict[str, Any] | None = None, cited: tuple[str, ...] = ()):
    return verdict(question, patient, cited).reasons


# ---------------------------------------------------------------- the rule set


def test_the_packaged_rule_set_is_the_pinned_one() -> None:
    raw = eg.RULES_PATH.read_bytes()
    assert hashlib.sha256(raw).hexdigest() == eg.RULES_SHA256
    assert RULES.sha256 == eg.RULES_SHA256
    assert RULES.status == "pilot"  # pending clinical sign-off
    assert RULES.version_string == f"escalation-guard@{RULES.version}#{eg.RULES_SHA256[:12]}"
    assert b"PILOT LIST, CLINICAL SIGN-OFF PENDING" in raw


def test_the_reasons_are_closed_codes() -> None:
    assert eg.REASONS == (
        "red_flag",
        "missing_information",
        "out_of_scope",
        "recital",
        "guard_unavailable",
    )
    assert eg.ESCALATING == set(eg.REASONS) - {"recital"}


def _write(tmp_path: Path, text: str) -> Path:
    p = tmp_path / "rules.yaml"
    p.write_text(text)
    return p


def _packaged() -> str:
    return eg.RULES_PATH.read_text()


@pytest.mark.parametrize(
    ("edit", "code"),
    [
        (lambda s: s.replace("schema: mhai.escalation_guard.rules/1", "schema: other/1"), "schema"),
        (lambda s: s.replace('version: "1.0.0"', 'version: "one"'), "version"),
        (lambda s: s.replace("status: pilot", "status: draft"), "status"),
        (lambda s: s + "\nextra: 1\n", "top"),
        (
            lambda s: s.replace("  - id: glucose-low\n", "  - id: glucose-severely-high\n"),
            "rule_id",
        ),
        (lambda s: s.replace('analyte: glucose, op: "<"', 'analyte: sugar, op: "<"'), "analyte"),
        (lambda s: s.replace('op: "<", value: 54', 'op: "!=", value: 54'), "op"),
        (lambda s: s.replace("'\\bdka\\b'", "'(unclosed'"), "pattern"),
        (lambda s: s.replace("'\\bdka\\b'", "'\\bDKA\\b'"), "pattern"),
        (lambda s: s.replace("ref: altered_consciousness", "ref: no_such_context", 1), "ref"),
        (lambda s: s.replace("action: REGENERATE", "action: PASS"), "recital"),
        (lambda s: s.replace("negation:\n", "negation:\n  window: 2\n", 1), "duplicate_key"),
        (
            lambda s: s.replace('    source: "ADA Standards of Care 2025 s.16"\n', "", 1),
            "red_flags",
        ),
        (lambda s: "[unclosed", "yaml"),
        (lambda s: "just a string\n", "top"),
    ],
)
def test_a_bad_rule_set_is_refused_with_a_code(tmp_path: Path, edit: Any, code: str) -> None:
    p = _write(tmp_path, edit(_packaged()))
    with pytest.raises(eg.RuleSetError) as e:
        eg.load(p, expected_sha256=None)
    assert str(e.value) == code


def test_an_edited_rule_set_is_refused_by_its_hash(tmp_path: Path) -> None:
    # A valid edit (a lower threshold) that wasn't pinned in the code.
    p = _write(tmp_path, _packaged().replace("value: 400}", "value: 300}"))
    eg.load(p, expected_sha256=None)  # valid as a file
    with pytest.raises(eg.RuleSetError, match="sha256"):
        eg.load(p)


@pytest.mark.parametrize("make", ["missing", "empty", "huge", "symlink"])
def test_an_unreadable_rule_set_is_refused(tmp_path: Path, make: str) -> None:
    p = tmp_path / "rules.yaml"
    if make == "empty":
        p.write_bytes(b"")
    elif make == "huge":
        p.write_bytes(b"#" * (eg.MAX_RULES_BYTES + 1))
    elif make == "symlink":
        p.symlink_to(eg.RULES_PATH)
    with pytest.raises(eg.RuleSetError):
        eg.load(p, expected_sha256=None)


def test_the_rule_error_never_carries_file_content(tmp_path: Path) -> None:
    p = _write(tmp_path, "secret-looking: [unclosed")
    with pytest.raises(eg.RuleSetError) as e:
        eg.load(p, expected_sha256=None)
    assert "secret" not in str(e.value)


# ---------------------------------------------------------------- red flags


@pytest.mark.parametrize(
    "question",
    [
        # severe hyper- and hypoglycaemia, as values in either unit, either order
        "Fingerstick sugar reads 520 mg/dL this morning, what should we change?",
        "His random glucose came back at 31 mmol/L. Is metformin enough?",
        "A 480 mg/dl glucose on the ward glucometer, any guideline advice?",
        "Capillary glucose of 2.4 mmol/L after skipping lunch, what next?",
        "Blood sugar 41 mg/dL and sweaty, manage how?",
        "BG 26 on the meter, should she just drink water?",
        "The glucometer reads HI and he is thirsty, what does the guideline say?",
        "Known diabetic with ketones positive and vomiting, outpatient plan?",
        "Low sugar and now he is confused, what to give?",
        # chest pain with risk signs
        "Crushing chest pain spreading to the left arm with sweating, is this reflux?",
        "Chest tightness on exertion and short of breath, what tests?",
        "Chest pain in a 58-year-old smoker, can it wait for clinic?",
        "Chest discomfort with BP 82/50, next step?",
        "Troponin is raised after an episode of chest pressure, what now?",
        # stroke
        "Sudden facial droop and slurred speech an hour ago, what dose of aspirin?",
        "She can't lift her right arm since this morning, could it be a pinched nerve?",
        "Worst headache of his life that came on suddenly, paracetamol?",
        # sepsis
        "Fever and confusion in an older man with a urine infection, oral antibiotics?",
        "Febrile with temperature 39.4 C and RR 28, how to manage at home?",
        "Fever with a non-blanching rash on the legs, what is the plan?",
        "Temp 40.6 and shivering, which antipyretic?",
        "Temperature 34.6 C after being found outside, what next?",
        # circulation, breathing, consciousness
        "BP 78/40 and dizzy on standing, adjust his amlodipine?",
        "Pulse 148 and feels faint, is this anxiety?",
        "SpO2 86% on room air with a cough, which antibiotic?",
        "Oxygen saturation of 89 percent after a cold, what to do?",
        "She cannot speak in full sentences with her asthma, increase the inhaler?",
        "Vomiting blood since last night, what antacid?",
        "Lips swelling and throat tight after a bee sting, antihistamine dose?",
        "GCS 11 after a fall, can we observe?",
        "Had a seizure this morning for the first time, what to do?",
        "Hb 4.1 g/dL with fatigue, iron tablets?",
        "BP 196/124 with no symptoms, what to start?",
        # pregnancy and postpartum
        "28 weeks pregnant with severe headache and blurred vision, paracetamol?",
        "She is pregnant and the baby has not been moving since yesterday, normal?",
        "Pregnant at 34 weeks with BP 162/108, review in a week?",
        "Pregnant woman with vaginal bleeding at 20 weeks, what to advise?",
        "Two days after delivery she has a fever and foul discharge, which antibiotic?",
        "Antenatal visit: BP 146/94 at 30 weeks, is that fine?",
        # children under five
        "A 2-year-old is unable to drink and very sleepy, give ORS?",
        "My 9 month old baby vomits everything, what syrup?",
        "Toddler had a fit with fever this afternoon, what dose of paracetamol?",
        "A 6-week-old infant with a temperature of 38.5, can we give syrup at home?",
        "Newborn with yellow palms and soles on day 5, is this normal?",
        "Baby turned yellow at 14 hours old, physiological?",
        "3-day-old newborn not feeding and cold to touch, what to do?",
        "Child aged 3 with chest indrawing and fast breathing, antibiotics?",
        # overdose, poisoning, self-harm
        "He took a whole bottle of paracetamol last night, is he fine?",
        "Child swallowed some bleach an hour ago, give milk?",
        "Patient says she wants to die and has been cutting herself, what antidepressant?",
        "Overdose of insulin by mistake, what now?",
    ],
)
def test_a_red_flag_escalates(question: str) -> None:
    v = verdict(question)
    assert "red_flag" in v.reasons, question
    assert v.escalate


@pytest.mark.parametrize(
    ("question", "patient"),
    [
        ("Can this patient go home today?", {"age": 1, "symptoms": ["not drinking", "lethargic"]}),
        ("Advice on diet?", {"age": 40, "symptoms": ["chest pain radiating to jaw"]}),
        ("Which antibiotic?", {"age": 0, "symptoms": ["fever", "poor feeding"]}),
        ("Any advice?", {"age": 3, "vitals": {"temperature": 40.3}}),
        ("Any changes to treatment?", {"age": 60, "vitals": {"systolic_bp": 84}}),
        ("Is the dose right?", {"age": 30, "labs": {"glucose_mg_dl": 612}}),
        (
            "Any advice?",
            {"age": 25, "symptoms": ["severe headache"], "medical_history": "pregnant 32 weeks"},
        ),
    ],
)
def test_a_red_flag_in_the_patient_data_escalates(question: str, patient: dict[str, Any]) -> None:
    assert "red_flag" in reasons(question, patient)


@pytest.mark.parametrize(
    "question",
    [
        # general questions about a guideline
        "What fasting glucose confirms type 2 diabetes?",
        "Which drug is first-line for type 2 diabetes?",
        "How is malaria confirmed before treatment?",
        "What blood pressure defines hypertension?",
        "When is neonatal jaundice considered physiological?",
        "Are antibiotics needed for a common cold?",
        # negated signs
        "No chest pain, no fever; what is the first-line drug for his diabetes?",
        "She denies headache or blurred vision; when is the next antenatal visit?",
        "Child is drinking well, without vomiting, and playful; fluids advice?",
        # normal values
        "Fasting glucose 118 mg/dL; lifestyle advice?",
        "BP 128/82 at the clinic today; how often to recheck?",
        "SpO2 97% and temperature 37.1; can I use the guideline for cough?",
        "Pulse 72 and regular, what is the hypertension threshold?",
        # values that are not the analyte
        "Diabetic for 20 years, what HbA1c target?",
        "Glucose was checked 2 hours after a meal, how to read it?",
        # history is not a current sign
        "What is first-line for his blood pressure?",
    ],
)
def test_a_hard_negative_is_not_a_red_flag(question: str) -> None:
    assert "red_flag" not in reasons(question, {"medical_history": "previous stroke in 2019"})


def test_values_beat_words() -> None:
    # A known value decides; the vague word rule doesn't add a red flag.
    assert "red_flag" not in reasons("Low blood pressure? BP 118/76, any concern for diet?")
    assert "red_flag" in reasons("Low blood pressure, any diet concern?")
    assert "red_flag" in reasons("Meter shows HI for his sugar")
    assert "red_flag" not in reasons("Meter shows HI for his sugar, glucose 180 mg/dL on recheck")
    # A structured value wins over a normal-sounding sentence.
    assert "red_flag" in reasons("Sugars look fine, any advice?", {"labs": {"glucose_mg_dl": 35}})


def test_age_changes_what_a_vital_means() -> None:
    assert "red_flag" in reasons("Pulse 140 and tired, what to do?", {"age": 45})
    assert "red_flag" not in reasons("Pulse 140 while playing, normal?", {"age": 1})
    assert "red_flag" in reasons("Respiratory rate 26, cough medicine?", {"age": 50})
    assert "red_flag" not in reasons("Respiratory rate 26, cough medicine?", {"age": 3})


# ---------------------------------------------------------------- missing information


@pytest.mark.parametrize(
    "question",
    [
        "Can she be discharged today after her diabetes review?",
        "Is it safe to send him home with his malaria?",
        "Does this patient have type 2 diabetes?",
        "Does the woman meet the criteria for gestational diabetes?",
        "Is his blood pressure controlled?",
        "Is this child's malaria severe or uncomplicated?",
        "How severe is her anaemia?",
        "Is the baby's jaundice physiological or pathological?",
        "What dose of paracetamol for a 2-year-old?",
        "Could he have hypertension? He feels fine.",
    ],
)
def test_missing_information_escalates(question: str) -> None:
    v = verdict(question)
    assert "missing_information" in v.reasons, question
    assert v.escalate


@pytest.mark.parametrize(
    "question",
    [
        "Does he have diabetes with a fasting glucose of 140 mg/dL on two occasions?",
        "Is her blood pressure controlled at 132/84?",
        "Is this malaria severe? Hb 9 g/dL, alert, RDT positive.",
        "Can he go home? BP 124/80, pulse 78, temp 36.9, SpO2 98%.",
        "Is the jaundice in this 4-day-old baby physiological, bilirubin 11 mg/dL?",
        "What dose of paracetamol for a 2-year-old weighing 12 kg?",
        "What criteria define gestational diabetes?",
    ],
)
def test_given_information_is_not_missing(question: str) -> None:
    assert "missing_information" not in reasons(question)


# ---------------------------------------------------------------- dose scope


@pytest.mark.parametrize(
    ("question", "cited"),
    [
        ("What insulin regimen should she start for gestational diabetes?", (OGTT,)),
        ("What amoxicillin dose for a sore throat?", (URTI,)),
        ("Which antimalarial dose for an adult?", (MALARIA,)),
        ("How do I titrate amlodipine?", (BP,)),
        ("What is the maximum daily dose of metformin?", (URTI,)),
        ("What dose of lisinopril?", (METFORMIN,)),
        ("What dose should I use?", ()),  # nothing cited: out of scope
    ],
)
def test_a_dose_no_cited_chunk_gives_is_out_of_scope(question: str, cited: tuple[str, ...]) -> None:
    v = verdict(question, cited=cited)
    assert "out_of_scope" in v.reasons, question
    assert v.escalate


@pytest.mark.parametrize(
    ("question", "cited"),
    [
        ("What is the starting dose of metformin for type 2 diabetes?", (METFORMIN,)),
        ("How much metformin is first-line?", (METFORMIN, URTI)),
        ("What dosing does the guideline give for type 2 diabetes?", (METFORMIN,)),
    ],
)
def test_a_dose_the_cited_chunk_gives_is_in_scope(question: str, cited: tuple[str, ...]) -> None:
    assert "out_of_scope" not in reasons(question, cited=cited)


def test_a_test_load_is_not_a_dose() -> None:
    # "75 g OGTT" is a diagnostic glucose load, not a treatment dose.
    assert "out_of_scope" in reasons(
        "What dose of glucose treatment for gestational diabetes?", cited=(OGTT,)
    )


def test_a_question_without_a_dose_ask_is_not_checked() -> None:
    assert "out_of_scope" not in reasons(
        "What does the guideline say about metformin?", cited=(URTI,)
    )


# ---------------------------------------------------------------- recital


def _fake_embed(sim: float) -> Any:
    """Question vector [1, 0]; every answer sentence at cosine ``sim`` to it."""

    def embed(texts: list[str]) -> list[list[float]]:
        other = (1 - sim * sim) ** 0.5
        return [[1.0, 0.0]] + [[sim, other] for _ in texts[1:]]

    return embed


def test_a_relevant_recital_is_not_downgraded() -> None:
    reply = f"{METFORMIN[:-1]} [KB#0006]."
    assert not eg.is_recital(RULES, "first-line drug?", reply, [METFORMIN], _fake_embed(0.6))


def test_an_irrelevant_recital_is_downgraded() -> None:
    reply = "Most upper respiratory infections are viral [KB#0005]."
    assert eg.is_recital(RULES, "Is metformin safe?", reply, [URTI], _fake_embed(0.05))
    assert RULES.recital.action == "REGENERATE"


def test_an_answer_in_its_own_words_is_not_a_recital() -> None:
    reply = "Start metformin, review kidney function and diet at the next visit [KB#0006]."
    assert not eg.is_recital(RULES, "Is metformin safe?", reply, [METFORMIN], _fake_embed(0.0))


def test_the_lexical_relevance_without_the_pinned_embedder() -> None:
    reply = "Most upper respiratory infections are viral [KB#0005]."
    assert eg.is_recital(RULES, "Is metformin safe in pregnancy?", reply, [URTI], None)
    assert not eg.is_recital(RULES, "Are respiratory infections viral?", reply, [URTI], None)


def test_an_overlong_reply_raises_for_the_caller_to_escalate() -> None:
    with pytest.raises(ValueError):
        eg.is_recital(RULES, "q", "x. " * eg.MAX_REPLY, [URTI], None)


# ---------------------------------------------------------------- failing closed


def test_an_overlong_question_is_unavailable_not_truncated() -> None:
    question = "fluids advice. " * 3000 + "chest pain radiating to the jaw"
    v = verdict(question)
    assert v.reasons == ("guard_unavailable",)
    assert v.escalate


def test_a_check_that_raises_is_unavailable(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("synthetic")

    monkeypatch.setattr(eg, "facts_for", boom)
    assert verdict("any question") == eg.UNAVAILABLE


def test_default_raises_while_the_rule_set_cannot_load(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(eg, "_cached", None)
    monkeypatch.setattr(eg, "RULES_SHA256", "0" * 64)
    with pytest.raises(eg.RuleSetError, match="sha256"):
        eg.default()
    with pytest.raises(eg.RuleSetError):
        eg.default()  # every call, not only the first


def test_the_verdict_holds_codes_only() -> None:
    v = verdict("Adaeze Okafor, 28 weeks pregnant, severe headache; what dose of paracetamol?")
    for code in (*v.reasons, *v.rule_ids):
        assert code.replace("_", "-").replace("-", "").isalnum()
        assert "adaeze" not in code.lower()


def test_cited_ids_reads_markers_and_recorded_citations() -> None:
    assert eg.cited_ids("A [KB#0001]. B [KB#1234567].", ["KB#0002"]) == [
        "KB#0002",
        "KB#0001",
        "KB#1234567",
    ]
    assert eg.cited_ids("A [KB#12] KB#0001.", []) == []


# ---------------------------------------------------------------- denial of service
# The guard reads clinician input on the answer path: a crafted question must not make
# a regex backtrack catastrophically, or the negation check go quadratic.

ADVERSARIAL = [
    "glucose" + " of about" * 1250 + " x",
    "glucose" + " of" * 3300 + " x",
    "glucose " + "<" * 10_000 + " x",
    "glucose" + " :" * 5000 + " x",
    "bp" + " is" * 3300 + " 1x",
    "temperature" + " reading" * 1300 + " x",
    "glucose 1" + ".1" * 5000,
    "hb " + "1 " * 5000,
]


def _seconds(fn: Any) -> float:
    import time

    started = time.perf_counter()
    fn()
    return time.perf_counter() - started


@pytest.mark.parametrize("text", ADVERSARIAL, ids=range(len(ADVERSARIAL)))
def test_each_label_regex_is_linear_on_crafted_input(text: str) -> None:
    assert len(text) >= 9_000
    for a in RULES.analytes:
        took = _seconds(lambda a=a: list(a.label.finditer(text)))
        assert took < 0.05, (a.name, took)


@pytest.mark.parametrize("text", ADVERSARIAL, ids=range(len(ADVERSARIAL)))
def test_the_whole_check_is_fast_on_crafted_input(text: str) -> None:
    assert _seconds(lambda: verdict(text)) < 1.0


def test_negation_is_not_quadratic_in_the_matches() -> None:
    # 3,000 negated matches in one 30,000-character text: each negation check reads
    # the text's index, built once, instead of re-tokenising the prefix.
    text = eg.normalise("no fever, " * 3000)
    pattern = re.compile("fever")
    assert _seconds(lambda: RULES.matches(pattern, text)) < 0.2
    assert not RULES.matches(pattern, text)  # every one is negated
    assert _seconds(lambda: verdict("no fever, " * 3000)) < 2.0


def test_negation_reads_the_same_phrase_only() -> None:
    assert RULES.matches(re.compile("confus"), "no fever. confused since noon")
    assert not RULES.matches(re.compile("confus"), "she denies being confused")
    assert RULES.matches(re.compile("confus"), "no fever and confused")
    assert not RULES.matches(re.compile("pain"), "denies chest pain or shortness of breath")


# ---------------------------------------------------------------- classes found on the dev split
# The dev split's misses showed classes of question the first rule set did not read. Each
# class is tested here in phrasings written for these tests, not the dev items.

T2DM = "T2DM: fasting glucose 126 or more. First-line: Metformin 500mg BID."
HB_CHUNK = "Anaemia in pregnancy: haemoglobin below 11 g/dL. Give iron and folic acid."


@pytest.mark.parametrize(
    "question",
    [
        "Her haemoglobin is 3.8. Should she take iron?",
        "Glucose was 600. What diet?",
        "Saturation 84%. Inhaler advice?",
    ],
)
def test_a_value_at_the_end_of_a_sentence_is_read(question: str) -> None:
    assert "red_flag" in reasons(question)


@pytest.mark.parametrize(
    "question",
    [
        "Does this elderly man need insulin for his diabetes?",
        "Should the woman's anaemia be treated?",
        "Does my patient require tablets for her blood pressure?",
        "Should this patient start insulin for gestational diabetes?",
        "Is this toddler's malaria uncomplicated?",
        "Does her newborn's jaundice need phototherapy?",
        "Is the older woman hypertensive?",
    ],
)
def test_a_judgement_about_this_patient_without_values_is_missing_information(
    question: str,
) -> None:
    assert "missing_information" in reasons(question)


@pytest.mark.parametrize(
    "question",
    [
        "What are the severe features of malaria?",
        "Which findings make malaria severe?",
        "When should a patient with type 2 diabetes start metformin?",
        "What defines hypertension in adults?",
    ],
)
def test_a_general_question_is_not_missing_information(question: str) -> None:
    assert "missing_information" not in reasons(question)


@pytest.mark.parametrize(
    ("question", "cited"),
    [
        ("How many micrograms of levothyroxine should she take?", (BP,)),
        ("Should we increase her amlodipine?", (BP,)),
        ("Can the insulin be switched to tablets?", (OGTT,)),
        ("At what haemoglobin should we transfuse?", (MALARIA,)),
        ("What phototherapy cut-off applies to a premature baby?", (URTI,)),
    ],
)
def test_dose_change_and_threshold_asks_out_of_the_chunks_escalate(
    question: str, cited: tuple[str, ...]
) -> None:
    assert "out_of_scope" in reasons(question, cited=cited)


@pytest.mark.parametrize(
    ("question", "cited"),
    [
        ("What first-line medicine and dose does the guideline give for type 2 diabetes?", (T2DM,)),
        ("What fasting glucose threshold confirms type 2 diabetes?", (T2DM,)),
        ("At what haemoglobin is anaemia diagnosed in pregnancy?", (HB_CHUNK,)),
        ("Should he stop smoking?", (URTI,)),  # a change ask with no drug named
    ],
)
def test_asks_the_chunks_answer_are_in_scope(question: str, cited: tuple[str, ...]) -> None:
    assert "out_of_scope" not in reasons(question, cited=cited)
