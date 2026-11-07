"""The egress scan (design section 4.4; plan Task 6).

The scan reads the exact request bytes as strict JSON, checks the shape
against the adapter's exact pointers, looks for this patient's identifiers
in every string, and runs the Nigerian identifier patterns over the two
patient-derived blocks only (the question and the context). Any doubt
refuses. Every name, number and record here is synthetic.
"""

from __future__ import annotations

import json
import logging
from typing import Any

import httpx2
import pytest
from hypothesis import HealthCheck, given, settings
from hypothesis import strategies as st

from intelligence.boundary.cloud import scan
from intelligence.boundary.cloud.gate import (
    CloudContext,
    CloudPrompt,
    EgressRefused,
    TaggingMasker,
    shared_chunk_of,
)
from intelligence.boundary.cloud.scan import (
    Identifiers,
    ScanHook,
    collect_identifiers,
    digit_runs,
    grouped_runs,
    norm_text,
    norm_words,
    scan_body,
)
from intelligence.boundary.jobs import run_query
from tests.boundary.cloud import layouts
from tests.boundary.cloud.layouts import PROVIDERS, blocks, body, render, shape

pytestmark = pytest.mark.disable_socket

NAME = "Adaeze Okafor"
PATIENT: dict[str, Any] = {"name": NAME}
NO_IDS = Identifiers(frozenset())


def _ids(patient: dict[str, Any] | None = None, question: str = "") -> Identifiers:
    return collect_identifiers(question, PATIENT if patient is None else patient)


def _scan(
    question: str,
    ids: Identifiers = NO_IDS,
    *chunks: str,
    provider: str = "anthropic",
    context: str = "Category: none\nRisk: none",
) -> str | None:
    return scan_body(body(provider, blocks(question, context, *chunks)), ids, shape(provider))


# --- escape and bypass attempts ------------------------------------------------------


def test_name_without_title_blocked() -> None:
    # A2: the masker only finds names after a title; the record names her.
    # This holds only when identifiers are supplied: core's PatientData has
    # no name key, so on real data it needs core's keyed name hashes
    # (rulings C5 and C8); until then the bare name is a recorded residual.
    question = f"{NAME} has chest pain"
    assert _scan(question, _ids(question=question)) == "known_identifier"


SPLIT_NAMES = [
    "Adaeze\nOkafor",
    "Adaeze\tOkafor",
    "Ada\u200beze Okafor",
    "ADAEZE  OKAFOR",
    "Adaeze-Okafor",
    "ADAEZE\u200b OKAFOR",
    "Ada\ufeffeze\u00a0Okafor",
    "\uff21daeze Okafor",  # a full-width A
]


@pytest.mark.parametrize("provider", PROVIDERS)
@pytest.mark.parametrize("name", SPLIT_NAMES)
@pytest.mark.parametrize("encoder", ["ascii", "utf8", "sdk"])
def test_escaped_and_split_names_blocked_through_sdk_encoder(
    provider: str, name: str, encoder: str
) -> None:
    b = blocks(f"{name} has had a fever for 3 days")
    if encoder == "sdk":
        raw, shp = layouts.sdk_body(provider, b), layouts.sdk_shape(provider)
    else:
        raw, shp = body(provider, b, ensure_ascii=encoder == "ascii"), shape(provider)
    # The raw bytes really do hide it: escaped or split, it isn't there as is.
    assert NAME.encode() not in raw
    assert scan_body(raw, _ids(), shp) == "known_identifier"


def test_sdk_bodies_pass_when_clean() -> None:
    # The SDK layouts themselves aren't refused (so the test above proves the
    # identifier rule, not the shape rule).
    for provider in PROVIDERS:
        raw = layouts.sdk_body(provider, blocks("fever for 3 days, T 38.5"))
        assert scan_body(raw, _ids(), layouts.sdk_shape(provider)) is None


def test_scan_sees_through_zero_width_fullwidth_and_case() -> None:
    ids = _ids({"name": NAME, "phone": "08031234567", "mrn": "1234567"})
    full_width_phone = "".join(chr(ord(c) - ord("0") + 0xFF10) for c in "08031234567")
    assert _scan(f"call {full_width_phone} today", ids) == "known_identifier"
    assert _scan("record 1234\u00a0567 seen", ids) == "known_identifier"
    assert _scan("record 1234\u200b567 seen", ids) == "known_identifier"
    assert _scan("aDaEzE oKaFoR is unwell", ids) == "known_identifier"
    # Arabic-Indic digits are digits too.
    arabic = "".join(chr(0x0660 + int(c)) for c in "1234567")
    assert _scan(f"record {arabic} seen", ids) == "known_identifier"


@pytest.mark.parametrize("written", ["12-34-567", "1234567", "123 4567", "12.34.567", "12/34/567"])
def test_integer_mrn_found_with_separators(written: str) -> None:
    ids = _ids({"mrn": 1234567})
    assert _scan(f"record {written} has fever", ids) == "known_identifier"


def test_number_matches_only_at_digit_boundaries() -> None:
    ids = _ids({"mrn": 1234567})
    assert _scan("record 91234567 has fever", ids) == "long_digits"  # not this patient's
    assert _scan("a count of 12345678", ids) == "long_digits"
    # In a chunk the patterns don't run, and the identifier needs boundaries.
    assert _scan("fever", ids, "lot 91234567 recalled") is None


def test_identifier_in_a_chunk_is_blocked() -> None:
    # The identifier rule covers every string, chunks included.
    assert _scan("fever", _ids(), f"seen with {NAME}") == "known_identifier"


@settings(max_examples=150, deadline=None, suppress_health_check=[HealthCheck.too_slow])
@given(
    seps=st.lists(
        st.sampled_from([" ", "\n", "\t", "-", ".", ",", "\u00a0", "\u2003", "_", "/", "  "]),
        min_size=1,
        max_size=1,
    ),
    zw=st.lists(
        st.sampled_from(["", "\x00", "\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u00ad"])
    ),
    upper=st.lists(st.booleans(), min_size=13, max_size=13),
    provider=st.sampled_from(PROVIDERS),
)
def test_split_name_property(
    seps: list[str], zw: list[str], upper: list[bool], provider: str
) -> None:
    # Any separator between the tokens, invisible characters anywhere,
    # any case: still this patient's name.
    first, last = "Adaeze", "Okafor"
    name = first + seps[0] + last
    cased = "".join(c.upper() if u else c.lower() for c, u in zip(name, upper * 2, strict=False))
    out = cased
    for i, z in enumerate(zw):
        pos = (i * 3) % (len(out) + 1)
        out = out[:pos] + z + out[pos:]
    assert _scan(f"{out} has a cough", _ids(), provider=provider) == "known_identifier"


@settings(max_examples=150, deadline=None)
@given(
    cuts=st.lists(st.integers(min_value=1, max_value=6), max_size=3, unique=True),
    sep=st.sampled_from([" ", "-", ".", "/"]),
)
def test_split_mrn_property(cuts: list[int], sep: str) -> None:
    mrn = "4471982"
    out = ""
    last = 0
    for c in sorted(cuts):
        out += mrn[last:c] + sep
        last = c
    out += mrn[last:]
    assert _scan(f"record {out} reviewed", _ids({"mrn": mrn})) == "known_identifier"


# --- patterns ------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("question", "rule"),
    [
        ("call 0803 123 4567 now", "ng_phone"),
        ("call +234 803 123 4567 now", "ng_phone"),
        ("call 0803-123-4567 now", "ng_phone"),
        ("call 080.3123.4567 now", "ng_phone"),
        ("call 08031234567 now", "ng_phone"),
        ("call +2348031234567 now", "ng_phone"),
        ("call 0703 123 4567 now", "ng_phone"),
        ("call 0911 123 4567 now", "ng_phone"),
        ("NIN 12345678901 on file", "nin"),
        ("NIN 1234 5678 901 on file", "nin"),
    ],
)
def test_nigerian_phone_and_nin_blocked_without_record(question: str, rule: str) -> None:
    # A4: no record at all; the shape alone blocks it.
    assert _scan(question) == rule


@pytest.mark.parametrize(
    ("question", "rule"),
    [
        ("write to ada.o+x@example.com", "email"),
        ("seen on 12/03/1990", "date"),
        ("seen on 12.03.90", "date"),
        ("seen on 1990-03-12", "date"),
        ("seen on 12 March 1990", "date"),
        ("seen on 12-mar-1990", "date"),
        ("seen on March 12, 1990", "date"),
        ("MRN: 4471", "mrn_prefix"),
        ("mrn#4471", "mrn_prefix"),
        ("Hospital No. 77A", "mrn_prefix"),
        ("hosp no 12", "mrn_prefix"),
        ("ref 4471982 attached", "long_digits"),
    ],
)
def test_patterns_keep_their_punctuation(question: str, rule: str) -> None:
    assert _scan(question) == rule


def test_patterns_run_on_the_context_block() -> None:
    assert _scan("fever", NO_IDS, context="Category: 0803 123 4567\nRisk: none") == "ng_phone"


# --- false positives (each is sent) --------------------------------------------------


@pytest.mark.parametrize(
    ("given_name", "text"),
    [("Ada", "an adaptive dosing schedule"), ("Eze", "do not freeze the vaccine")],
)
def test_short_name_token_needs_word_boundary(given_name: str, text: str) -> None:
    ids = _ids({"name": {"given": [given_name], "family": "Okafor"}})
    assert _scan(f"what is {text}?", ids, text) is None
    assert _scan(f"{given_name} has a cough", ids) == "known_identifier"


def test_no_digit_join_across_values() -> None:
    # Two numbers in different JSON values never form an identifier: the
    # chunk id, max_tokens (1500) and the risk score (0.42) stay apart.
    ids = _ids({"mrn": "000115"})
    assert "000115" in ids.values
    assert b"000115" not in body("anthropic", blocks("x", "y", "15 mg"))
    assert _scan("1500 ml given", ids, "15 mg/kg", context="Category: none\nRisk: 0.42") is None
    ids = _ids({"mrn": "421500"})
    assert _scan("1500 ml given", ids, context="Category: none\nRisk: 0.42") is None


@pytest.mark.parametrize("provider", PROVIDERS)
def test_guideline_doses_and_dates_pass(provider: str) -> None:
    chunk = (
        "Give vitamin A 200000 IU once. Updated 2023-05-01. Adrenaline 1:1000, "
        "0.01 mg/kg IM. Review on 01/05/2023. Call 0800 000 0000 for the line list."
    )
    assert _scan("dose of vitamin A?", NO_IDS, chunk, provider=provider) is None
    assert _scan("dose of vitamin A?", NO_IDS, chunk, chunk, chunk, provider=provider) is None


TYPICAL_QUESTIONS = [
    "T 38.5 101.3, BP 120/80, HR 96: what next?",
    "platelets 250 000, Hb 11.2: is this thrombocytopenia?",
    "glucose 7.8 mmol/L at 2 h: gestational diabetes?",
    "weight 72.5 kg, 1500 ml fluid in: is this enough?",
    "WBC 12.4 x10^9/L with neutrophilia: antibiotics?",
    "SpO2 91% on room air, RR 28: oxygen target?",
    "creatinine 180 umol/L, K 5.9 mmol/L: urgent steps?",
    "INR 3.4 on warfarin, no bleeding: hold a dose?",
    "child 18 kg, T 39.2, 3 days of fever: malaria test?",
    "CD4 350 cells/mm3, viral load 1200 copies/ml: switch regimen?",
    "Hb 7.2 g/dL, MCV 68 fL: iron deficiency?",
    "sodium 128 mmol/L in an adult of 67: correction rate?",
    "BMI 31.4, HbA1c 8.2%: first-line therapy?",
    "gestational age 32 weeks, BP 150/100, proteinuria 2+: magnesium?",
    "ALT 85 U/L, AST 120 U/L, bilirubin 34 umol/L: hepatitis screen?",
    "urine output 0.3 ml/kg/h for 6 h: AKI stage?",
    "neonate 2.8 kg, day 3, bilirubin 250 umol/L: phototherapy?",
    "GCS 13/15 after a fall, age 72: CT head?",
    "BP 90/60, lactate 4.1 mmol/L: sepsis bundle?",
    "parasitaemia 2%, Hb 9.8: artesunate or ACT?",
    "dose 15 mg/kg every 8 h, max 1 g: paracetamol for 25 kg?",
    "fever for 3-5 days, cough, CRP 96 mg/L: chest x-ray?",
    "peak flow 220 L/min, predicted 450: severity?",
    "TSH 0.01 mU/L, free T4 38 pmol/L: treatment?",
    "BP 120/80, HR 96, T 38.5, RR 18, SpO2 97: discharge?",
    "BP 120/80 HR 88, Hb 11.2 WBC 7.5: anaemia work-up?",
    "noradrenaline 0.1 \u00b5g/kg/min, MAP 62: add vasopressin?",
    "\u03b2-hCG 1500 IU/L, no intrauterine sac: ectopic?",
    "BP 120 / 80 after 10 min rest: hypertension?",
    "lab values 1100, 120, 1300 over 3 days: trend?",
    "T 37.5-38.5 over 2 days, pH 7.35-7.45: normal?",
    "Hb 11.5-13.5, temp 36.5 - 37.5: anything else?",
    "maintenance 1000-1500 ml/day, GCS 4 5 6: plan?",
]


@pytest.mark.parametrize("provider", PROVIDERS)
def test_typical_clinical_questions_are_sent(provider: str) -> None:
    # R-2: a fixed set of vitals and labs questions; none is an identifier.
    assert len(TYPICAL_QUESTIONS) >= 20
    ids = _ids()
    for q in TYPICAL_QUESTIONS:
        verdict = _scan(q, ids, provider=provider, context="Category: infectious\nRisk: 0.42")
        assert verdict is None, q


@pytest.mark.parametrize("given_name", ["Grace", "Joy"])
def test_common_word_given_name_blocks_today(given_name: str) -> None:
    """R-3: a measured over-block, pinned as current behaviour.

    A patient whose given name is an ordinary word (Grace, Joy, Hope, Peace,
    Favour, Patience) matches guideline text on word boundaries, and the job
    escalates. That fails closed: it costs availability, not privacy. The
    false-refusal rate is reported in the H2 evaluation, and changing this
    needs that evaluation, not an edit to this test.
    """
    ids = _ids({"name": f"{given_name} Okafor"})
    chunk = f"Allow a {given_name.lower()} period of 30 days; {given_name.lower()} is not a sign."
    assert _scan("follow-up interval?", ids, chunk) == "known_identifier"


def test_hyphenated_range_is_sent() -> None:
    """A range such as "HR 100-120" is sent (digit_runs no
    longer joins across a hyphen). The cost, recorded as a residual with the
    spaced-number one ("445 812") and measured in H2: a 6-digit number that
    is not in this patient's record, written "123-456", passes long_digits.
    A number in the record is still caught however it is split."""
    assert _scan("HR 100-120 at rest: rate control?") is None
    assert _scan("HR 100 to 120 at rest: rate control?") is None
    assert _scan("ref 123-456 attached") is None  # the residual, pinned
    assert _scan("ref 123-456 attached", _ids({"mrn": "123456"})) == "known_identifier"


def test_redacted_placeholders_pass() -> None:
    ids = _ids()
    question = (
        "[NAME_REDACTED] seen on [DATE_REDACTED], MRN [MRN_REDACTED], "
        "phone [PHONE_REDACTED], email [EMAIL_REDACTED], DOB [DOB_REDACTED], "
        "[AGE_90+], [ZIP_REDACTED], [ADDRESS_REDACTED], [SSN_REDACTED]: fever?"
    )
    assert _scan(question, ids) is None


def test_hospital_notes_is_not_a_hospital_number() -> None:
    assert _scan("per the hospital notes, fever since admission") is None
    assert _scan("what does an MRN look like?") == "mrn_prefix"  # pinned


CLEAN_RECORD: dict[str, Any] = {
    "name": {"given": ["Adaeze"], "family": "Okafor"},
    "mrn": "4471982",
    "phone": "08031234567",
    "email": "adaeze.okafor@example.com",
    "dob": "1990-03-12",
    "address": "12 Allen Avenue, Ikeja",
    "nin": 12345678901,
    "disease_category": "infectious",
    "risk_score": 0.42,
    "notes": [{"text": "Seen in clinic, Patient: Adaeze Okafor"}],
}


@pytest.mark.parametrize("provider", PROVIDERS)
def test_clean_record_is_sent(provider: str) -> None:
    question = "Adult with 3 days of fever and chills, T 38.5: first-line treatment?"
    ids = collect_identifiers(question, CLEAN_RECORD)
    chunk = "Uncomplicated malaria: artemether-lumefantrine twice daily for 3 days."
    verdict = _scan(
        question, ids, chunk, provider=provider, context="Category: infectious\nRisk: 0.42"
    )
    assert verdict is None


# --- mechanics -----------------------------------------------------------------------


@pytest.mark.parametrize("provider", PROVIDERS)
def test_shape_refuses_unknown_path(provider: str) -> None:
    b = blocks("fever")
    ok = render(provider, b)
    assert scan_body(json.dumps(ok).encode(), NO_IDS, shape(provider)) is None

    def refused(doc: Any) -> None:
        assert scan_body(json.dumps(doc).encode(), NO_IDS, shape(provider)) == "shape"

    refused({**ok, "user": "synthetic-user"})
    refused({**ok, "metadata": {"query_id": "q-1"}})
    refused({**ok, "metadata": {}})  # an empty container is a leaf too
    refused({**ok, "seed": 7})  # a number outside the shape (an MRN as an int)
    refused([ok])  # not an object
    eight = render(provider, [*b, *(f"<<chunk KB#{i:04d}>>\nx\n<<end>>" for i in range(1, 7))])
    refused(eight)  # a block past 6
    # A patient block missing or not text: the patterns can't be skipped.
    missing = render(provider, ["only the question"])
    refused(missing)


def test_shape_needs_exact_pointers() -> None:
    with pytest.raises(ValueError):
        scan.Shape(frozenset({"/a/*"}), frozenset({"/a/*"}))
    with pytest.raises(ValueError):
        scan.Shape(frozenset({"/a", "/b"}), frozenset({"/a"}))  # patient <= allowed
    with pytest.raises(ValueError):
        scan.Shape(frozenset({"/a"}), frozenset({"/a"}))  # exactly two
    with pytest.raises(ValueError):
        scan.shape_for("gemini", {})


@pytest.mark.parametrize("provider", PROVIDERS)
def test_hand_built_shape_without_fixed_values_refused(provider: str) -> None:
    """Only the seven block texts may be free text: a Shape whose other
    leaves (the model, the system prompt, the schema) carry no fixed value
    would let any text through them unchecked, so it can't be built. The
    way to build one is shape_for."""
    made = shape(provider)
    texts = scan.block_pointers(provider)
    # No fixed values at all.
    with pytest.raises(ValueError):
        scan.Shape(made.patient_paths, made.allowed_paths)
    # One leaf left free: the model.
    assert "/model" in made.fixed_values
    loose = {k: v for k, v in made.fixed_values.items() if k != "/model"}
    with pytest.raises(ValueError):
        scan.Shape(made.patient_paths, made.allowed_paths, loose)
    # Free text at a pointer that isn't a block.
    with pytest.raises(ValueError):
        scan.Shape(made.patient_paths, made.allowed_paths | {"/note"}, made.fixed_values)
    # Patient paths other than blocks 0 and 1.
    with pytest.raises(ValueError):
        scan.Shape(frozenset(texts[1:3]), made.allowed_paths, made.fixed_values)
    # The same declaration as shape_for gives is accepted.
    again = scan.Shape(made.patient_paths, made.allowed_paths, made.fixed_values)
    assert again == made
    assert scan.scan_body(body(provider, blocks("fever")), _ids(), again) is None


@pytest.mark.parametrize("provider", PROVIDERS)
def test_schema_strings_are_allowed_paths(provider: str) -> None:
    doc = render(provider, blocks("fever"))
    shp = shape(provider)
    strings = [p for p, v in scan.walk(doc) if isinstance(v, str)]
    schema_strings = [p for p in strings if "/schema/" in p]
    assert len(schema_strings) >= 8
    assert set(strings) <= shp.allowed_paths
    assert scan_body(json.dumps(doc).encode(), NO_IDS, shp) is None

    # A string added to the schema by hand, the shape not updated: refused.
    widened = json.loads(json.dumps(layouts.SCHEMA))
    widened["properties"]["answer"]["format"] = "text"
    doc = render(provider, blocks("fever"), widened)
    assert scan_body(json.dumps(doc).encode(), NO_IDS, shp) == "shape"
    assert scan_body(json.dumps(doc).encode(), NO_IDS, shape(provider, widened)) is None


@pytest.mark.parametrize(
    "raw",
    [
        b"not json",
        b'{"model": "a", "model": "b"}',
        b'{"max_tokens": NaN}',
        b'{"model": "\\ud800"}',
        b"\xff\xfe{}",
        b'{"model": "a"} trailing',
        b"",
        b"[" * 5000 + b"]" * 5000,
    ],
    ids=["text", "dup_key", "nan", "surrogate", "not_utf8", "trailing", "empty", "deep"],
)
def test_non_json_body_refused(raw: bytes) -> None:
    assert scan_body(raw, NO_IDS, shape("anthropic")) == "not_json"


def test_scan_reports_rule_not_text(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    question = f"{NAME} on 0803 123 4567"
    raw = body("anthropic", blocks(question))
    verdict = scan_body(raw, _ids(), shape("anthropic"))
    assert verdict == "known_identifier"
    assert verdict in scan.RULES
    hook = ScanHook(_ids(), shape("anthropic"))
    with pytest.raises(EgressRefused) as e:
        hook(httpx2.Request("POST", "https://api.anthropic.com/v1/messages", content=raw))
    assert str(e.value) == "known_identifier"
    assert e.value.args == ("known_identifier",)
    for text in ("Adaeze", "Okafor", "0803", "adaeze"):
        assert text not in caplog.text
        assert text not in repr(e.value)


@pytest.mark.parametrize("provider", PROVIDERS)
@pytest.mark.parametrize("n_chunks", [0, 1, 5])
def test_patient_paths_are_exact_for_each_layout(provider: str, n_chunks: int) -> None:
    # R-1: the patterns see blocks 0 and 1 only, whatever the chunk count.
    shp = shape(provider)
    prefix = {"anthropic": "/messages/0", "openai": "/input/0", "xai": "/messages/1"}[provider]
    assert shp.patient_paths == {f"{prefix}/content/0/text", f"{prefix}/content/1/text"}
    for i in range(7):
        assert f"{prefix}/content/{i}/text" in shp.allowed_paths
    chunks = ["Give vitamin A 200000 IU, 0803 123 4567, 12/03/1990."] * n_chunks
    assert _scan("dose?", NO_IDS, *chunks, provider=provider) is None
    assert _scan("call 0803 123 4567", NO_IDS, *chunks, provider=provider) == "ng_phone"


def _shared_chunks(n: int) -> tuple[Any, ...]:
    from tests.boundary.cloud.test_gate import _load, _row

    rows = [_row(f"KB#{i:04d}", True) for i in range(1, n + 1)]
    return tuple(shared_chunk_of(c) for c in _load(rows))


@pytest.mark.parametrize("provider", PROVIDERS)
@pytest.mark.parametrize("n_chunks", [0, 2, 5])
def test_name_in_question_blocked_for_every_layout(provider: str, n_chunks: int) -> None:
    # R-1, rendered from a real CloudPrompt: its blocks() order is the one
    # the shape's patient paths name.
    tm = TaggingMasker()
    question = f"{NAME} has a cough"
    ctx, _ = CloudContext.from_masked({"disease_category": tm.mask("respiratory")})
    prompt = CloudPrompt(tm.mask(question), ctx, _shared_chunks(n_chunks), tm)
    raw = json.dumps(render(provider, prompt.blocks())).encode()
    assert scan_body(raw, _ids(question=question), shape(provider)) == "known_identifier"
    clean = CloudPrompt(tm.mask("a cough for 3 weeks"), ctx, _shared_chunks(n_chunks), tm)
    raw = json.dumps(render(provider, clean.blocks())).encode()
    assert scan_body(raw, _ids(), shape(provider)) is None


def test_identifiers_repr_has_no_values() -> None:
    ids = _ids(CLEAN_RECORD)
    assert repr(ids) == f"Identifiers(<{len(ids.values)} values>)"
    assert "okafor" not in repr(ids) and "4471982" not in str(ids)


def test_size_cap() -> None:
    assert scan.MAX_BODY_BYTES == 49_152
    base = body("anthropic", blocks("fever"))
    pad = scan.MAX_BODY_BYTES - len(base)
    fits = body("anthropic", blocks("fever" + "a" * pad))
    assert len(fits) == scan.MAX_BODY_BYTES
    assert scan_body(fits, NO_IDS, shape("anthropic")) is None
    over = body("anthropic", blocks("fever" + "a" * (pad + 1)))
    assert scan_body(over, NO_IDS, shape("anthropic")) == "too_large"


def test_trace_headers_refused() -> None:
    # No trace context leaves with a request (plan Task 4).
    raw = body("anthropic", blocks("fever"))
    for header in ("traceparent", "TraceState", "baggage"):
        hook = ScanHook(NO_IDS, shape("anthropic"))
        req = httpx2.Request("POST", "https://api.anthropic.com/v1/messages", content=raw)
        req.headers[header] = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
        with pytest.raises(EgressRefused) as e:
            hook(req)
        assert str(e.value) == "trace_header" and hook.verdict == "trace_header"
        assert hook.sent is False


def test_hook_sets_verdict_and_sent() -> None:
    url = "https://api.openai.com/v1/responses"
    hook = ScanHook(_ids(), shape("openai"))
    assert hook.verdict is None and hook.sent is False
    hook(httpx2.Request("POST", url, content=body("openai", blocks("fever"))))
    assert hook.verdict is None and hook.sent is True
    with pytest.raises(EgressRefused):
        hook(httpx2.Request("POST", url, content=body("openai", blocks(NAME))))
    assert hook.verdict == "known_identifier"
    # Once refused, the hook refuses every later request too.
    with pytest.raises(EgressRefused):
        hook(httpx2.Request("POST", url, content=body("openai", blocks("fever"))))
    assert hook.verdict == "known_identifier"


def test_hook_refuses_an_unreadable_body() -> None:
    def chunks() -> Any:
        yield body("anthropic", blocks("fever"))

    req = httpx2.Request("POST", "https://api.anthropic.com/v1/messages", content=chunks())
    hook = ScanHook(NO_IDS, shape("anthropic"))
    with pytest.raises(EgressRefused) as e:
        hook(req)
    assert str(e.value) == "not_json" and hook.sent is False


def test_hook_refusal_stops_the_sdk_before_the_transport() -> None:
    import anthropic

    reached: list[bool] = []

    def transport(request: httpx2.Request) -> httpx2.Response:
        reached.append(True)
        return httpx2.Response(500)

    hook = ScanHook(_ids(), layouts.sdk_shape("anthropic"))
    client = httpx2.Client(
        transport=httpx2.MockTransport(transport), event_hooks={"request": [hook]}, trust_env=False
    )
    a = anthropic.Anthropic(api_key="fake" + "0" * 24, http_client=client, max_retries=0)
    with pytest.raises(Exception):  # noqa: B017 - the SDK wraps it; the verdict is ours
        a.messages.create(
            model=layouts.MODEL,
            max_tokens=layouts.MAX_TOKENS,
            system="s",
            messages=[{"role": "user", "content": [{"type": "text", "text": NAME}]}],
        )
    assert hook.verdict is not None
    assert reached == []


# --- collect_identifiers and the normalisations ---------------------------------------


def test_identifying_keys_are_pinned() -> None:
    assert scan.IDENTIFYING_KEYS == frozenset(
        {
            "name",
            "given",
            "family",
            "mrn",
            "phone",
            "email",
            "address",
            "dob",
            "nin",
            "bvn",
            "nhis",
            "identifier",
            "telecom",
            "birthdate",
            "birth_date",
        }
    )
    assert scan.MAX_DEPTH == run_query.MAX_DEPTH


def test_collect_identifiers() -> None:
    patient = {
        "Name": [{"given": ["Chidi", "Ok"], "family": "Nwosu"}],
        "telecom": [{"value": "0803-555-0101"}],
        "birthDate": "1985-07-21",
        "MRN": 4471982,
        "nhis": 4471,  # a short int is not collected
        "flag": True,
        "history": {"contacts": [{"phone": 8035550101}]},
        "free_text": "Dr. Emeka Obi called on 03/04/2024 from 555-123-4567",
    }
    ids = collect_identifiers("Patient: Ngozi Eze asked about mr# 77", patient)
    v = ids.values
    assert {"chidi", "nwosu"} <= v
    assert "ok" not in v  # name tokens of fewer than 3 letters
    assert "0803 555 0101" in v
    assert "1985 07 21" in v
    assert "4471982" in v and "4471" not in v
    assert "8035550101" in v
    assert "emeka obi" in v and "03 04 2024" in v and "555 123 4567" in v
    assert "ngozi eze" in v
    assert "true" not in v


def test_collect_identifiers_stops_at_max_depth() -> None:
    deep: Any = {"name": "Deepname Okon"}
    for _ in range(scan.MAX_DEPTH + 2):
        deep = {"x": deep}
    assert "okon" not in collect_identifiers("", deep).values


def test_norm_words_and_norm_text() -> None:
    for s in ("Adaeze\nOkafor", "ADAEZE\u200b OKAFOR", "adaeze-okafor", " Adaeze,  Okafor! "):
        assert norm_words(s) == "adaeze okafor"
    assert norm_words("\u1eccl\u00e1 Okafor") == "ola okafor"  # marks folded
    assert norm_text("A\u00a0B\n\tC  d+e/f.g-h@i:j#k") == "a b c d+e/f.g-h@i:j#k"
    assert norm_text("\uff10\uff18\u200b03") == "0803"


def test_digit_runs_and_grouped_runs() -> None:
    assert digit_runs("123-456") == ["123", "456"]
    assert digit_runs("T 38.5 101.3") == ["38", "5", "101", "3"]
    assert digit_runs("platelets 250 000") == ["250", "000"]
    assert digit_runs("12/03") == ["12", "03"]
    assert "08031234567" in grouped_runs("0803 123 4567")
    assert "08031234567" in grouped_runs("080.3123.4567")
    assert "2348031234567" in grouped_runs("+234 803 123 4567")
    assert grouped_runs("38.5 101.3 120 80") == []
    assert grouped_runs("platelets 250 000") == []


# --- the masker's detector, numbers, dates, MRNs, confusables (ruling C5) --------------------

# core's PatientData (services/core/internal/queries/shapes.go): these four
# keys only; core refuses any other with a 422.
CORE_PATIENT: dict[str, Any] = {
    "age": 34,
    "sex": "female",
    "symptoms": ["chest pain"],
    "medical_history": "Seen by Dr. Emeka Obi in clinic; platelets 15000 last year",
}


@pytest.mark.parametrize(
    "question",
    [
        "Dr. Emeka Obi referred her with chest pain",
        "Patient: Ngozi Eze has chest pain",
        "call 555.123.4567 about chest pain",
        "Dr. \u0415meka Obi referred her",  # a Cyrillic E: the skeleton maps it back
    ],
)
def test_masker_detection_in_the_question_refuses(question: str) -> None:
    # C5: the masker's own detector runs over blocks 0 and 1; any detection
    # refuses, with no record at all.
    assert _scan(question) == "pii_detected"


def test_masker_detection_in_the_context_refuses() -> None:
    ctx = "Category: Patient: Ngozi Eze\nRisk: none"
    assert _scan("chest pain", NO_IDS, context=ctx) == "pii_detected"


def test_masker_detection_not_run_on_chunks() -> None:
    assert _scan("chest pain", NO_IDS, "Guideline by Dr. Emeka Obi, 2019.") is None


def test_core_patient_data_feeds_known_identifiers() -> None:
    # A titled name in medical_history is detected by the masker at collect
    # time, so the bare name in the question is then this patient's.
    ids = collect_identifiers("chest pain for 2 days", CORE_PATIENT)
    assert {"emeka obi", "15000"} <= ids.values
    assert _scan("Emeka Obi asked about chest pain", ids) == "known_identifier"


@pytest.mark.xfail(
    strict=True,
    raises=AssertionError,
    reason="Ruling C8 residual: PIIMasker finds names only after a title "
    "(Dr, Mr, Mrs, Ms, Miss, Prof, Patient), so an untitled name on core's "
    "real patient_data reaches the body. Needs a name detector or core's "
    "keyed name hashes (ruling C5's H2 prerequisite).",
)
@pytest.mark.parametrize("where", ["question", "history"])
def test_untitled_name_on_core_shape_refused(where: str) -> None:
    patient = dict(CORE_PATIENT)
    question = "Adaeze Okafor has chest pain"
    if where == "history":
        patient["medical_history"] = "Seen by Adaeze Okafor's GP"
    ids = collect_identifiers(question if where == "question" else "", patient)
    assert _scan(question, ids) is not None


def test_zip_rule_over_block_pinned() -> None:
    """C4: a measured over-block, pinned. PIIMasker's ZIP rule makes a
    5-digit lab value in medical_history an identifier, so a chunk with the
    same number is refused (H2)."""
    ids = collect_identifiers("", CORE_PATIENT)
    assert _scan("platelets?", ids, "Transfuse below 15000 per microlitre.") == "known_identifier"


def test_name_key_over_block_pinned() -> None:
    """C4: a measured over-block, pinned. A value under any "name" key is
    an identifier, so a diagnosis stored as {"name": "Malaria"} refuses every
    malaria chunk (H2; core's PatientData has no such key today)."""
    ids = collect_identifiers("", {"diagnosis": [{"name": "Malaria"}]})
    assert _scan("fever", ids, "Malaria: give artesunate.") == "known_identifier"


@pytest.mark.parametrize(
    "question",
    [
        "call (0803) 123 4567 now",
        "call 0803 - 123 - 4567 now",
        "call 0803\u2013123\u20134567 now",
        "call 0803\u2014123\u20144567 now",
        "call 0803,123,4567 now",
        "call +234 (0) 803 123 4567 now",
        "call (+234) 803-123-4567 now",
    ],
)
def test_phone_with_any_short_gap_blocked_without_record(question: str) -> None:
    # No record; the gap between groups is 1 to 3 non-alphanumerics.
    assert _scan(question) == "ng_phone"


@pytest.mark.parametrize(
    "written",
    [
        "(0803) 123 4567",
        "0803 - 123 - 4567",
        "0803\u2013123\u20134567",
        "0803,123,4567",
    ],
)
def test_known_phone_with_any_short_gap_blocked(written: str) -> None:
    ids = _ids({"phone": "08031234567"})
    assert _scan("fever", ids, f"call {written}") == "known_identifier"


@pytest.mark.parametrize("written", ["284,817", "284 \u2013 817", "(284) 817", "284 / 817"])
def test_known_mrn_with_any_short_gap_blocked(written: str) -> None:
    assert _scan(f"record {written}", _ids({"mrn": "284817"})) == "known_identifier"


@pytest.mark.parametrize(
    "question",
    [
        "38.5 101.3",
        "BP 120/80 HR 88",
        "HR 100-120",
        "Hb 11.2 WBC 7.5",
        "BP 120/80, HR 96, T 38.5, RR 18, SpO2 97",
        "values 1100, 120, 1300",
    ],
)
def test_vitals_still_sent(question: str) -> None:
    assert _scan(question, _ids()) is None


@pytest.mark.parametrize(
    "question",
    [
        "DOB 12 / 03 / 1984",
        "born 12 of March 1984",
        "born the 12th of March, 1984",
        "born 12 03 1984",
        "seen 3 . 4 . 2024",
    ],
)
def test_spaced_and_worded_dates_blocked(question: str) -> None:
    assert _scan(question) == "date"


@pytest.mark.parametrize(
    "question",
    ["T 37.5-38.5", "pH 7.35-7.45", "Hb 11.5-13.5", "temp 36.5 - 37.5", "dose 0.5-1.25 mg/kg"],
)
def test_decimal_ranges_are_not_dates(question: str) -> None:
    # A date uses the same separator both times, so a
    # reference range (d.d-d.d) is sent; every earlier date form still isn't.
    assert _scan(f"{question}: normal?") is None


@pytest.mark.parametrize(
    "question",
    [
        "MRN-4471",
        "MRN=4471",
        "M.R.N. 4471",
        "hospital number 4471",
        "folder no 4471",
        "folder number 4471",
        "card no. 4471",
        "file no 4471",
    ],
)
def test_more_mrn_phrasings_blocked(question: str) -> None:
    assert _scan(f"{question} has fever") == "mrn_prefix"


@pytest.mark.parametrize(
    "text",
    [
        "\u0410daeze \u041ekafor has chest pain",  # Cyrillic A and O
        "\u0430d\u0430eze Ok\u0430for has chest pain",  # Cyrillic a
        "Ad\u03b1eze \u039fkafor has chest pain",  # Greek alpha and Omicron
        "Ada\u20ddeze Oka\u20ddfor has chest pain",  # Me
        "Ada\u0903eze Oka\u0903for has chest pain",  # Mc
        "Ada\u2800eze Oka\u2800for has chest pain",  # braille blank
    ],
)
def test_confusables_and_marks_blocked(text: str) -> None:
    assert _scan(text, _ids()) == "known_identifier"


def test_placeholder_between_digits_does_not_split_a_number() -> None:
    assert _scan("NIN 12345[NAME_REDACTED]67[NAME_REDACTED]8901") == "nin"
    ids = _ids({"mrn": "284817"})
    assert _scan("record 284[DATE_REDACTED]817", ids) == "known_identifier"


def test_hook_scans_url_and_headers() -> None:
    raw = body("anthropic", blocks("fever"))
    hook = ScanHook(_ids(), shape("anthropic"))
    with pytest.raises(EgressRefused) as e:
        hook(
            httpx2.Request(
                "POST", "https://api.anthropic.com/v1/messages?q=Adaeze%20Okafor", content=raw
            )
        )
    assert str(e.value) == "known_identifier"
    hook = ScanHook(_ids(), shape("anthropic"))
    req = httpx2.Request("POST", "https://api.anthropic.com/v1/messages", content=raw)
    req.headers["x-note"] = "Adaeze Okafor"
    with pytest.raises(EgressRefused) as e:
        hook(req)
    assert str(e.value) == "known_identifier"


@pytest.mark.parametrize("provider", PROVIDERS)
def test_shape_pins_fixed_values(provider: str) -> None:
    # Every leaf outside the blocks has a fixed value; only blocks carry
    # free text.
    shp = shape(provider)

    def verdict(doc: dict[str, Any]) -> str | None:
        return scan_body(json.dumps(doc).encode(), NO_IDS, shp)

    doc = render(provider, blocks("fever"))
    assert verdict(doc) is None
    changed = json.loads(json.dumps(doc))
    changed["model"] = "another-model"
    assert verdict(changed) == "shape"
    sys_ptr = {"anthropic": "system", "openai": "instructions"}.get(provider)
    changed = json.loads(json.dumps(doc))
    if sys_ptr:
        changed[sys_ptr] += " Seen by Adaeze Okafor."
    else:
        changed["messages"][0]["content"] += " extra"
    assert verdict(changed) == "shape"
    widened = json.loads(json.dumps(layouts.SCHEMA))
    widened["properties"]["answer"]["description"] = "Free text."
    assert verdict(render(provider, blocks("fever"), widened)) == "shape"
    # The block type is pinned too, and a bool can't stand in for an int.
    changed = json.loads(json.dumps(doc))
    arr = (
        changed["messages"][0]
        if provider == "anthropic"
        else (changed["input"][0] if provider == "openai" else changed["messages"][1])
    )
    arr["content"][0]["type"] = "image"
    assert verdict(changed) == "shape"
    with pytest.raises(ValueError):
        scan.Shape(
            frozenset({"/a", "/b"}), frozenset({"/a", "/b"}), {"/c": 1}
        )  # a fixed pointer must be allowed


# --- ranges, hyphen chains, gaps, 2-digit years, case-folded confusables ---------------------


@pytest.mark.parametrize("written", ["123-456-7890", "1234-567-890", "123-45-678"])
def test_hyphen_chain_of_three_groups_is_long_digits(written: str) -> None:
    # Hyphens join chains of 3 or more groups; two groups ("HR 100-120")
    # stay sent.
    assert _scan(f"ref {written} attached") == "long_digits"
    assert _scan("HR 100-120 and 1000-1500 ml") is None


@pytest.mark.parametrize("sep", [";", "_", " ; ", "_ "])
def test_semicolon_and_underscore_gaps(sep: str) -> None:
    # ";" and "_" join groups too (neither is a chunk delimiter).
    written = sep.join(["0803", "123", "4567"])
    assert _scan(f"call {written} now") == "ng_phone"
    ids = _ids({"phone": "08031234567"})
    assert _scan("fever", ids, f"call {written}") == "known_identifier"
    # The chunk delimiters still never join two numbers.
    assert _scan("1500 ml", _ids({"mrn": "000115"}), "15 mg") is None


def test_single_digit_phone_blocked() -> None:
    # A phone number spelt out one digit at a time.
    assert _scan("call 0 8 0 3 1 2 3 4 5 6 7 now") == "ng_phone"


@pytest.mark.parametrize("question", ["seen 12 Mar 84", "seen 3 dec 99", "seen 12-mar-84"])
def test_month_name_with_two_digit_year_is_a_date(question: str) -> None:
    assert _scan(question) == "date"


def test_hospital_hash_is_a_record_number() -> None:
    assert _scan("hospital #4471 has fever") == "mrn_prefix"
    assert _scan("hospital # 4471 has fever") == "mrn_prefix"


def test_cyrillic_ghe_for_r() -> None:
    # Cyrillic ge for r ("Okafo\u0433").
    ids = _ids({"family": "Okafor"})
    assert _scan("Okafo\u0433 has chest pain", ids) == "known_identifier"


@pytest.mark.parametrize(
    ("recorded", "written"),
    [
        (
            "\u041d\u0438\u043a\u043e\u043b\u0430\u0439 \u041f\u0435\u0442\u0440\u043e\u0432",
            "\u041d\u0418\u041a\u041e\u041b\u0410\u0419 \u041f\u0415\u0422\u0420\u041e\u0412",
        ),
        (
            "\u041d\u0418\u041a\u041e\u041b\u0410\u0419 \u041f\u0415\u0422\u0420\u041e\u0412",
            "\u043d\u0438\u043a\u043e\u043b\u0430\u0439 \u043f\u0435\u0442\u0440\u043e\u0432",
        ),
        (
            "\u039d\u03af\u03ba\u03bf\u03c2 \u0392\u03ad\u03c1\u03bc\u03b7\u03c2",
            "\u039d\u0399\u039a\u039f\u03a3 \u0392\u0395\u03a1\u039c\u0397\u03a3",
        ),
    ],
)
def test_cyrillic_and_greek_names_match_in_any_case(recorded: str, written: str) -> None:
    # Case-fold first, then the skeleton, so a name written in Cyrillic
    # or Greek matches in either case.
    assert _scan(f"{written} has chest pain", _ids({"name": recorded})) == "known_identifier"
