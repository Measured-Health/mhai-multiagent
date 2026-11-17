"""Ruling C8: PIIMasker finds names without a title and Nigerian identifiers,
offline, and fails closed when its name lists can't be loaded.

Before this change "Adaeze Okafor has chest pain" was sent unmasked
(test_untitled_name_on_core_shape_refused was a strict xfail). Every name
and number here is synthetic.
"""

from __future__ import annotations

import hashlib
import json
import re
import time
from collections.abc import Iterator

import pytest

from intelligence.boundary.cloud.gate import TaggingMasker
from intelligence.boundary.cloud.scan import (
    Identifiers,
    collect_identifiers,
    scan_body,
    shape_for,
)
from intelligence.security import ng_pii
from intelligence.security.hipaa import PIIMasker

pytestmark = pytest.mark.disable_socket


def _masked(text: str) -> str:
    return PIIMasker().mask(text)[0]


def _types(text: str) -> list[str]:
    return sorted(d.entity_type for d in PIIMasker().mask(text)[1])


# --- names ------------------------------------------------------------------------------


@pytest.mark.parametrize(
    "text, masked",
    [
        ("Adaeze Okafor has chest pain", "[NAME_REDACTED] has chest pain"),
        ("Seen by Adaeze Okafor's GP", "Seen by [NAME_REDACTED]'s GP"),
        ("ADAEZE OKAFOR has chest pain", "[NAME_REDACTED] has chest pain"),
        ("adaeze okafor has chest pain", "[NAME_REDACTED] has chest pain"),
        ("Is metformin safe for Abubakar Usman?", "Is metformin safe for [NAME_REDACTED]?"),
        ("OKAFOR Adaeze, 34F", "[NAME_REDACTED], 34F"),
        ("Ebiere Dappa and Osagie Iyamu", "[NAME_REDACTED] and [NAME_REDACTED]"),
        ("A. Okafor reports cough", "[NAME_REDACTED] reports cough"),
        ("Alhaji Barau has diabetes", "Alhaji [NAME_REDACTED] has diabetes"),
        ("her daughter Tarebi has fever", "her daughter [NAME_REDACTED] has fever"),
        ("Mrs ojo has anaemia", "Mrs [NAME_REDACTED] has anaemia"),
        ("Patient is Mojeed, 62M", "Patient is [NAME_REDACTED], 62M"),
        ("Grace Okafor has a fever", "[NAME_REDACTED] has a fever"),
        ("Grace has a fever", "[NAME_REDACTED] has a fever"),
        ("Please review Grace today", "Please review [NAME_REDACTED] today"),
        ("Adébáyọ̀ Ọlá has malaria", "[NAME_REDACTED] has malaria"),
        ("Oluwadarasimi has a cough", "[NAME_REDACTED] has a cough"),
    ],
)
def test_untitled_names_masked(text: str, masked: str) -> None:
    assert _masked(text) == masked


@pytest.mark.parametrize(
    "text, word",
    [
        ("There is a grace period of 7 days for missed refills.", "grace"),
        ("Give hope and reassurance; most febrile seizures are benign.", "hope"),
        ("Seen on Monday with fever since March. Manage as malaria?", "Monday"),
        ("Artesunate 2.4 mg/kg for severe malaria in Kano?", "Artesunate"),
        ("Olanzapine or haloperidol for acute agitation?", "Olanzapine"),
        ("Burkitt lymphoma or Kaposi sarcoma: how is it treated here?", "Burkitt"),
        ("ADA guidelines say HbA1c below 7%.", "ADA"),
        ("Okafor will be reviewed", "will"),  # not part of the name
    ],
)
def test_ordinary_text_keeps_its_words(text: str, word: str) -> None:
    assert word in _masked(text)


@pytest.mark.parametrize(
    "name",
    [
        "Adaeze Zzqa Zzqb",
        "Zzqa Zzqb Zzqc Okafor",
        "Adaeze Zzqa Zzqb Zzqc Zzqd Okafor",
        "Zzqa Zzqb Zzqc Zzqd Zzqe Adaeze",
        "adaeze zzqa zzqb zzqc zzqd zzqe",
    ],
)
def test_every_part_of_a_long_name_masked(name: str) -> None:
    # No cap on how far a name spreads: a cap would leave outer parts sent.
    assert _masked(f"{name} has fever") == "[NAME_REDACTED] has fever"


def test_masker_output_has_nothing_left_to_find() -> None:
    # The scan runs the masker again over the masked question (ruling C5):
    # anything found twice would refuse every such question.
    for text in (
        "Alhaji SANI has uncontrolled diabetes.",
        "Mama Ebiere Tuboye, NIN 1234 5678 901, phone +234 (0) 803 123 4567",
        "Patient is HEPHZIBAH UDOSEN, 62M, MRN: LUTH/2019/12345",
        "her daughter Tarebi has fever",
    ):
        masked = _masked(text)
        assert PIIMasker().mask(masked)[1] == [], masked


# --- numbers ----------------------------------------------------------------------------


@pytest.mark.parametrize(
    "number, kind",
    [
        ("08031234567", "PHONE"),
        ("0803 123 4567", "PHONE"),
        ("0803-123-4567", "PHONE"),
        ("0803.123.4567", "PHONE"),
        ("+2348031234567", "PHONE"),
        ("+234 803 123 4567", "PHONE"),
        ("+234 (0) 803 123 4567", "PHONE"),
        ("(0803) 123 4567", "PHONE"),
        ("234-803-123-4567", "PHONE"),
        ("0 8 0 3 1 2 3 4 5 6 7", "PHONE"),
        ("12345678901", "NATIONAL_ID"),
        ("1234 5678 901", "NATIONAL_ID"),
        ("123-4567-8901", "NATIONAL_ID"),
        ("12345 678901", "NATIONAL_ID"),
        ("22.915.230.445", "NATIONAL_ID"),
        ("22123456789", "NATIONAL_ID"),
    ],
)
def test_nigerian_numbers_masked(number: str, kind: str) -> None:
    text = f"contact {number} today"
    masked, detections = PIIMasker().mask(text)
    assert masked == f"contact {PIIMasker.REPLACEMENT_MAP[kind]} today"
    assert [d.entity_type for d in detections] == [kind]


@pytest.mark.parametrize(
    "text, code",
    [
        ("MRN: 445 812, fever", "445 812"),
        ("Hosp No. LUTH/2024/04512", "LUTH/2024/04512"),
        ("folder no 23/4512", "23/4512"),
        ("card number UCH-0045123 at the clinic", "UCH-0045123"),
        ("MR# 4471 on ward 3", "4471"),
        ("Record LASUTH/AE/2023/0451 seen", "LASUTH/AE/2023/0451"),
        ("HAV-000123 - review results", "HAV-000123"),
        ("nhis no: ABC/12345.", "ABC/12345"),
    ],
)
def test_record_numbers_masked(text: str, code: str) -> None:
    masked = _masked(text)
    assert code not in masked
    assert "[MRN_REDACTED]" in masked or "[NATIONAL_ID_REDACTED]" in masked


@pytest.mark.parametrize(
    "text",
    [
        "BP 120/80, HR 96, T 38.5, RR 18, SpO2 97",
        "temps 38.5 39.2 38.9 37.6",
        "values 1100, 120, 1300",
        "hospital no longer has stock",
        "COVID-19 and ICD-10 code A01",
        "Hb 11.2 WBC 7.5 platelets 250",
    ],
)
def test_clinical_numbers_kept(text: str) -> None:
    assert ng_pii.number_spans(text) == [] and ng_pii.label_spans(text) == []


def test_long_digit_strings_stay_fast() -> None:
    # A body of spelt-out digits: the sub-number search is bounded.
    started = time.perf_counter()
    spans = ng_pii.number_spans(" ".join("7" for _ in range(5000)))
    assert spans and time.perf_counter() - started < 5.0


# --- the gazetteer, its pins and failing closed --------------------------------------------


def test_gazetteer_files_match_their_pins() -> None:
    raw = ng_pii._read(ng_pii.GAZETTEER_FILE)
    words = ng_pii._read(ng_pii.WORDS_FILE)
    assert hashlib.sha256(raw).hexdigest() == ng_pii.GAZETTEER_SHA256
    assert hashlib.sha256(words).hexdigest() == ng_pii.WORDS_SHA256
    gaz = ng_pii.gazetteer()
    assert len(gaz.names) > 1000 and "okafor" in gaz.names and "grace" in gaz.words
    assert not gaz.names & gaz.known and "malaria" in gaz.known


def test_tampered_gazetteer_refused() -> None:
    raw = ng_pii._read(ng_pii.GAZETTEER_FILE)
    with pytest.raises(ng_pii.DetectorUnavailable):
        ng_pii.parse_gazetteer(raw.replace(b"N okafor\n", b""))
    with pytest.raises(ng_pii.DetectorUnavailable):
        ng_pii.parse_words(ng_pii._read(ng_pii.WORDS_FILE) + b"extra\n")


@pytest.mark.parametrize(
    "body",
    [b"N okafor\nX bad\n", b"N Okafor\n", b"N okafor\nW okafor\n", b"# nothing\n"],
)
def test_malformed_gazetteer_refused(monkeypatch: pytest.MonkeyPatch, body: bytes) -> None:
    monkeypatch.setattr(ng_pii, "GAZETTEER_SHA256", hashlib.sha256(body).hexdigest())
    with pytest.raises(ng_pii.DetectorUnavailable):
        ng_pii.parse_gazetteer(body)


@pytest.mark.parametrize("body", [b"Malaria\n", b"# nothing\n"])
def test_malformed_word_list_refused(monkeypatch: pytest.MonkeyPatch, body: bytes) -> None:
    monkeypatch.setattr(ng_pii, "WORDS_SHA256", hashlib.sha256(body).hexdigest())
    with pytest.raises(ng_pii.DetectorUnavailable):
        ng_pii.parse_words(body)


def test_gazetteer_without_word_list_has_no_score() -> None:
    gaz = ng_pii.parse_gazetteer(ng_pii._read(ng_pii.GAZETTEER_FILE))
    assert gaz.scorer is None and gaz.known == frozenset()


@pytest.fixture
def unreadable(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    """The data files can't be read (a broken image, a missing file)."""

    def missing(name: str) -> bytes:
        raise FileNotFoundError(name)

    ng_pii._cached_gazetteer.cache_clear()
    monkeypatch.setattr(ng_pii, "_read", missing)
    yield
    ng_pii._cached_gazetteer.cache_clear()


@pytest.mark.usefixtures("unreadable")
def test_unreadable_gazetteer_raises_every_time() -> None:
    for _ in range(2):  # a failure isn't cached as "no names"
        with pytest.raises(ng_pii.DetectorUnavailable):
            ng_pii.detect("Adaeze Okafor")
    with pytest.raises(ng_pii.DetectorUnavailable):
        PIIMasker().mask("Adaeze Okafor has chest pain")


@pytest.mark.usefixtures("unreadable")
def test_unreadable_gazetteer_fails_the_cloud_path_closed() -> None:
    # The client's masker raises, the job can't collect its identifiers, and
    # a body that reached the scan anyway is refused: never sent.
    with pytest.raises(ng_pii.DetectorUnavailable):
        TaggingMasker().mask("Adaeze Okafor has chest pain")
    with pytest.raises(ng_pii.DetectorUnavailable):
        collect_identifiers("Adaeze Okafor has chest pain", {})
    fixed = {"/model": "m", "/max_tokens": 1, "/messages/0/role": "user"}
    body = {
        "model": "m",
        "max_tokens": 1,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "chest pain"},
                    {"type": "text", "text": "Category: none"},
                ],
            }
        ],
    }
    raw = json.dumps(body).encode()
    assert scan_body(raw, Identifiers(frozenset()), shape_for("anthropic", fixed)) == "scan_error"


def test_detector_error_names_no_input() -> None:
    err = ng_pii.DetectorUnavailable("name gazetteer unreadable")
    assert not re.search(r"[A-Z][a-z]+ [A-Z][a-z]+", str(err))


def test_name_score_separates_names_from_drugs() -> None:
    scorer = ng_pii.gazetteer().scorer
    assert scorer is not None
    assert scorer.score("oluwadarasimi") > ng_pii.NAME_SCORE_MIN
    assert scorer.score("chukwudumebi") > ng_pii.NAME_SCORE_MIN
    for drug in ("ciprofloxacin", "dexamethasone", "hydrochlorothiazide"):
        assert scorer.score(drug) < ng_pii.NAME_SCORE_MIN


# --- review findings: bracket bypass and worst-case input ----------------------------------


@pytest.mark.parametrize(
    "text, hidden",
    [
        ("[ADAEZE] [OKAFOR] has chest pain", "ADAEZE"),
        ("[OKAFOR] has chest pain", "OKAFOR"),
        ("NIN [12345678901] on the card", "12345678901"),
        ("call [08031234567]", "08031234567"),
        ("[NAME_REDACTED_ADAEZE] Okafor", "ADAEZE"),
    ],
)
def test_brackets_dont_hide_identifiers(text: str, hidden: str) -> None:
    # Only the masker's own placeholders, exactly, are skipped.
    assert hidden not in _masked(text)


@pytest.mark.parametrize(
    "code",
    [
        "LUTH/" + "/".join(["AE"] * 12) + "/2019/12345",
        "LUTH/2019/" + "/".join(["X1"] * 12),
        "HOSPITALNUMBERX/2019/1234567890123456789",
        "UCH-ABCDEFGHIJKLMNOP-12345",
        "LUTH/2019/12345abc",
    ],
)
def test_long_record_codes_masked_whole(code: str) -> None:
    masked = _masked(f"Record {code} seen in clinic")
    assert masked == "Record [MRN_REDACTED] seen in clinic", masked


def test_placeholders_are_the_maskers() -> None:
    assert set(ng_pii.PLACEHOLDERS) == set(PIIMasker.REPLACEMENT_MAP.values())


@pytest.mark.parametrize(
    "text",
    [
        # A long run of words that each extend a name, either way.
        " ".join(["Okafor"] + ["Zzqx"] * (ng_pii.MAX_TEXT_CHARS // 5 - 2)),
        " ".join(["Zzqx"] * (ng_pii.MAX_TEXT_CHARS // 5 - 2) + ["Okafor"]),
        "AB" + "/1" * (ng_pii.MAX_TEXT_CHARS // 2 - 2),
        "AB" + "/A" * (ng_pii.MAX_TEXT_CHARS // 2 - 2) + "/123",
        ("AB/1/" * (ng_pii.MAX_TEXT_CHARS // 5))[: ng_pii.MAX_TEXT_CHARS],
        "[NAME_REDACTED] Okafor " * (ng_pii.MAX_TEXT_CHARS // 23),
        "MRN " + "1-" * (ng_pii.MAX_TEXT_CHARS // 2 - 3),
    ],
)
def test_worst_case_input_at_the_cap_is_fast(text: str) -> None:
    text = text[: ng_pii.MAX_TEXT_CHARS]
    started = time.perf_counter()
    ng_pii.detect(text)
    assert time.perf_counter() - started < 5.0


def test_over_cap_input_refused() -> None:
    with pytest.raises(ng_pii.DetectorUnavailable):
        ng_pii.detect("a" * (ng_pii.MAX_TEXT_CHARS + 1))
    with pytest.raises(ng_pii.DetectorUnavailable):
        PIIMasker().mask("fever " * (ng_pii.MAX_TEXT_CHARS // 6 + 1))
