import random
import time
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import schema
from intelligence.doceval.synth import patients, terminology


def _gen(seed: int, n: int) -> list[patients.SynthPatient]:
    return patients.generate(random.Random(seed), n)


def _gt(p: patients.SynthPatient) -> list[dict[str, Any]]:
    return [dict(d) for d in p.documents]


def test_same_seed_same_patients() -> None:
    assert _gen(7, 50) == _gen(7, 50)


def test_different_seed_differs() -> None:
    assert _gen(7, 50) != _gen(8, 50)


def test_identifiers_synthetic() -> None:
    given = terminology.name_set("given")
    family = terminology.name_set("family")
    facilities = terminology.name_set("facilities")
    pts = _gen(1, 10_000)
    for p in pts:
        assert p.mrn.startswith("SYN-") and len(p.mrn) == 12 and p.mrn[4:].isdigit()
        assert p.national_id.startswith("SYNID-")
        assert 1925 <= p.date_of_birth.year <= 2024
        assert p.phone.startswith("555-01")
        assert p.family_name in family
        assert all(g in given for g in p.given_names.split())
        for enc in p.encounters:
            assert enc.facility in facilities
            assert enc.start.date() >= p.date_of_birth


def test_documents_validate_against_schema() -> None:
    for p in _gen(2, 300):
        assert p.documents
        for doc in p.documents:
            schema.validate_model_output(doc)


def test_abnormal_and_critical_rates_within_tolerance() -> None:
    abnormal = critical = total = 0
    for p in _gen(3, 3000):
        for enc in p.encounters:
            for ob in enc.observations:
                total += 1
                assert ob.flag == ob.analyte.classify(ob.value)
                abnormal += ob.flag != "N"
                critical += ob.flag in ("HH", "LL")
    assert total > 10_000
    assert abs(abnormal / total - patients.ABNORMAL_RATE) < 0.02
    assert abs(critical / total - patients.CRITICAL_RATE) < 0.01


def test_values_are_plausible() -> None:
    for p in _gen(4, 500):
        for enc in p.encounters:
            for ob in enc.observations:
                a = ob.analyte
                assert a.plausible_low <= ob.value <= a.plausible_high


def test_codes_from_subsets_only() -> None:
    loinc = {a.code for a in terminology.loinc()}
    icd = {c.code for c in terminology.icd10()}
    rx = {m.name for m in terminology.rxnorm()}
    rxcodes = {m.code for m in terminology.rxnorm()}
    for p in _gen(5, 500):
        for med in p.medications:
            assert med.code in rxcodes
        for doc in p.documents:
            for o in doc["observations"]:
                assert o["local_code"]["value"] in loinc
            for c in doc["conditions"]:
                assert c["local_code"]["value"] in icd
                assert c["local_system"]["value"] == "icd10"
            for m in doc["medications"]:
                assert m["name_text"]["value"] in rx


def test_terminology_ranges_are_consistent() -> None:
    for a in terminology.loinc():
        chain = [
            x
            for x in (a.plausible_low, a.critical_low, a.ref_low, a.ref_high, a.critical_high)
            if x is not None
        ]
        assert chain == sorted(chain)
        assert a.plausible_high >= chain[-1]
        assert a.critical_low is not None or a.critical_high is not None


def test_no_snomed_anywhere() -> None:
    root = Path(terminology.__file__).parent / "terminology"
    for f in root.glob("*.json"):
        assert "snomed" not in f.read_text(encoding="utf-8").lower()
    for p in _gen(6, 100):
        for doc in p.documents:
            for c in doc["conditions"]:
                assert c["local_system"]["value"] != "snomed"


def test_name_lists_headed_synthetic() -> None:
    for kind in terminology.NAME_LISTS:
        text = (Path(terminology.__file__).parent / "names" / f"{kind}.txt").read_text()
        assert text.splitlines()[0].startswith("# synthetic")
    assert (
        "Regenstrief" in (Path(terminology.__file__).parent / "terminology" / "NOTICE").read_text()
    )


def test_no_global_random_or_clock(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*_a: object, **_k: object) -> float:
        raise AssertionError("global randomness or clock used")

    monkeypatch.setattr(random, "random", boom)
    monkeypatch.setattr(random, "randint", boom)
    monkeypatch.setattr(random, "choice", boom)
    monkeypatch.setattr(time, "time", boom)
    monkeypatch.setattr(time, "time_ns", boom)
    monkeypatch.setattr(time, "monotonic", boom)
    assert len(patients.generate(random.Random(9), 20)) == 20


def test_rejects_bad_rates() -> None:
    with pytest.raises(ValueError):
        patients.generate(random.Random(1), 1, abnormal_rate=0.1, critical_rate=0.2)
