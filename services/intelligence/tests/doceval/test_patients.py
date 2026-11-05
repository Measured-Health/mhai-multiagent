import datetime
import json
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


def test_rxnorm_pinned_to_fixture() -> None:
    pins = json.loads((Path(__file__).parent / "fixtures" / "rxnorm_pins.json").read_text())
    actual = {m.code: m.display for m in terminology.rxnorm()}
    assert actual == pins
    assert (
        "02-Nov-2026" in (Path(terminology.__file__).parent / "terminology" / "NOTICE").read_text()
    )


def _day(text: str) -> datetime.date:
    return datetime.date.fromisoformat(text[:10])


def test_dates_never_future_or_out_of_order() -> None:
    latest = patients._LATEST_ENCOUNTER
    for p in _gen(12, 1500):
        for doc in p.documents:
            doc_date = _day(doc["document"]["date"]["value"])
            assert doc_date <= latest
            enc = doc["encounter"]
            assert enc["start"]["value"] <= enc["end"]["value"]
            assert _day(enc["start"]["value"]) <= doc_date
            assert p.date_of_birth <= doc_date
            for o in doc["observations"]:
                collected, resulted = o["collected_at"]["value"], o["resulted_at"]["value"]
                assert enc["start"]["value"] <= collected <= resulted <= enc["end"]["value"]
            for m in doc["medications"]:
                start = _day(m["start"]["value"])
                assert start <= doc_date
                if m["stop"]["value"] is not None:
                    assert start <= _day(m["stop"]["value"]) <= doc_date
            for c in doc["conditions"]:
                assert _day(c["onset"]["value"]) <= doc_date


class _NoNow(datetime.datetime):
    @classmethod
    def now(cls, tz: Any = None) -> Any:
        raise AssertionError("clock used")

    @classmethod
    def utcnow(cls) -> Any:
        raise AssertionError("clock used")

    @classmethod
    def today(cls) -> Any:
        raise AssertionError("clock used")


class _NoToday(datetime.date):
    @classmethod
    def today(cls) -> Any:
        raise AssertionError("clock used")


def forbid_global_random_and_clock(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*_a: object, **_k: object) -> Any:
        raise AssertionError("global randomness or clock used")

    for name in ("random", "randint", "choice", "sample", "randrange", "shuffle", "uniform"):
        monkeypatch.setattr(random, name, boom)
    for name in ("time", "time_ns", "monotonic"):
        monkeypatch.setattr(time, name, boom)
    monkeypatch.setattr(datetime, "datetime", _NoNow)
    monkeypatch.setattr(datetime, "date", _NoToday)


def test_no_global_random_or_clock(monkeypatch: pytest.MonkeyPatch) -> None:
    forbid_global_random_and_clock(monkeypatch)
    assert len(patients.generate(random.Random(9), 20)) == 20


def test_rejects_bad_rates() -> None:
    with pytest.raises(ValueError):
        patients.generate(random.Random(1), 1, abnormal_rate=0.1, critical_rate=0.2)
