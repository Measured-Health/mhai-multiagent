"""Seeded synthetic patients with ground truth in the extraction schema.

Everything is synthetic by construction: MRN ``SYN-`` plus 8 digits, national
id ``SYNID-`` plus 8 digits, names, streets and facilities from the committed
lists, phone numbers in the 555-0100..0199 fiction range. All randomness comes
from the ``random.Random`` passed in; nothing reads the clock.

Ground truth is in the model-output view of the schema (no confidence, band,
grounded, flags or page_count). Page and box are null; the renderer fills them.
"""

import datetime
import random
from dataclasses import dataclass
from decimal import Decimal
from typing import Any

from intelligence.doceval.synth import terminology
from intelligence.doceval.synth.terminology import Analyte, Condition, Medication

ABNORMAL_RATE = 0.20  # share of observations outside the reference range (includes critical)
CRITICAL_RATE = 0.03  # share of observations at a critical value

_EARLIEST_ENCOUNTER = datetime.date(2015, 1, 1)
_LATEST_ENCOUNTER = datetime.date(2026, 10, 31)
_END_OF_LATEST = datetime.datetime.combine(_LATEST_ENCOUNTER, datetime.time(23, 59, 59))
_DOB_FIRST = datetime.date(1925, 1, 1)
_DOB_LAST = datetime.date(2024, 12, 31)

_DOC_TYPES = (
    ("lab_report", 40),
    ("clinic_note", 25),
    ("discharge_summary", 15),
    ("medication_chart", 10),
    ("vitals_chart", 10),
)


@dataclass(frozen=True)
class SynthObservation:
    analyte: Analyte
    value: Decimal
    flag: str
    collected_at: datetime.datetime
    resulted_at: datetime.datetime


@dataclass(frozen=True)
class SynthEncounter:
    kind: str
    doc_type: str
    start: datetime.datetime
    end: datetime.datetime
    facility: str
    clinician: str
    observations: tuple[SynthObservation, ...]


@dataclass(frozen=True)
class SynthMedication:
    code: str
    name: str
    strength: str
    dose: str
    route: str
    frequency: str
    start: datetime.date
    stop: datetime.date | None
    status: str


@dataclass(frozen=True)
class SynthCondition:
    code: str
    display: str
    clinical_status: str
    verification: str
    onset: datetime.date


@dataclass(frozen=True)
class SynthPatient:
    mrn: str
    national_id: str
    given_names: str
    family_name: str
    date_of_birth: datetime.date
    sex: str
    phone: str
    street_address: str
    conditions: tuple[SynthCondition, ...]
    medications: tuple[SynthMedication, ...]
    encounters: tuple[SynthEncounter, ...]
    documents: tuple[dict[str, Any], ...]


def _pick[T](rng: random.Random, items: tuple[T, ...]) -> T:
    return items[rng.randrange(len(items))]


def _weighted(rng: random.Random, options: tuple[tuple[str, int], ...]) -> str:
    total = sum(w for _, w in options)
    roll = rng.randrange(total)
    for name, weight in options:
        if roll < weight:
            return name
        roll -= weight
    raise AssertionError("unreachable")


def _scaled(value: Decimal, decimals: int) -> int:
    return int(value.scaleb(decimals).to_integral_value())


def _unscaled(n: int, decimals: int) -> Decimal:
    return Decimal(n).scaleb(-decimals)


def _draw(rng: random.Random, a: Analyte, flag: str) -> Decimal:
    """A value of the wanted class (``N``, ``H``, ``L``, ``HH``, ``LL``)."""
    d = a.decimals
    plow, phigh = _scaled(a.plausible_low, d), _scaled(a.plausible_high, d)
    rlow = _scaled(a.ref_low, d) if a.ref_low is not None else None
    rhigh = _scaled(a.ref_high, d) if a.ref_high is not None else None
    clow = _scaled(a.critical_low, d) if a.critical_low is not None else None
    chigh = _scaled(a.critical_high, d) if a.critical_high is not None else None
    if flag == "N":
        lo = rlow if rlow is not None else plow
        hi = rhigh if rhigh is not None else phigh
        if clow is not None:
            lo = max(lo, clow + 1)
        if chigh is not None:
            hi = min(hi, chigh - 1)
    elif flag == "H":
        assert rhigh is not None
        lo, hi = rhigh + 1, (chigh - 1 if chigh is not None else phigh)
    elif flag == "L":
        assert rlow is not None
        lo, hi = (clow + 1 if clow is not None else plow), rlow - 1
    elif flag == "HH":
        assert chigh is not None
        lo, hi = chigh, phigh
    else:
        assert clow is not None
        lo, hi = plow, clow
    return _unscaled(rng.randint(lo, hi), d)


def _wanted_flag(rng: random.Random, a: Analyte, abnormal: float, critical: float) -> str:
    roll = rng.random()
    if roll < critical:
        sides = [s for s, v in (("LL", a.critical_low), ("HH", a.critical_high)) if v is not None]
        return _pick(rng, tuple(sides))
    if roll < abnormal:
        sides = []
        if a.ref_high is not None:
            sides.append("H")
        if a.ref_low is not None:
            sides.append("L")
        return _pick(rng, tuple(sides))
    return "N"


def _stamp(moment: datetime.datetime) -> str:
    return moment.strftime("%Y-%m-%dT%H:%M:%S")


def _f(value: object, raw: str | None = None) -> dict[str, Any]:
    if value is None:
        return {"value": None, "raw_text": None, "page": None, "bbox": None}
    return {
        "value": value,
        "raw_text": raw if raw is not None else str(value),
        "page": None,
        "bbox": None,
        "script": "printed",
    }


def _observation_rows(enc: SynthEncounter) -> list[dict[str, Any]]:
    rows = []
    for ob in enc.observations:
        a = ob.analyte
        text = a.format(ob.value)
        rows.append(
            {
                "category": _f(a.category),
                "name_text": _f(a.name),
                "local_code": _f(a.code),
                "value_text": _f(text),
                "value_numeric": _f(float(ob.value), text),
                "comparator": _f(None),
                "unit_text": _f(a.unit),
                "reference_text": _f(a.reference_text()),
                "reference_low": _f(
                    None if a.ref_low is None else float(a.ref_low),
                    None if a.ref_low is None else a.format(a.ref_low),
                ),
                "reference_high": _f(
                    None if a.ref_high is None else float(a.ref_high),
                    None if a.ref_high is None else a.format(a.ref_high),
                ),
                "abnormal_flag": _f(ob.flag),
                "specimen_text": _f(a.specimen),
                "collected_at": _f(_stamp(ob.collected_at)),
                "resulted_at": _f(_stamp(ob.resulted_at)),
            }
        )
    return rows


def _document(p: SynthPatient, enc: SynthEncounter) -> dict[str, Any]:
    with_clinical = enc.doc_type != "lab_report"
    other_ids = [{"kind": _f("national_id"), "value": _f(p.national_id)}]
    meds = (
        [
            {
                "name_text": _f(m.name),
                "strength_text": _f(m.strength),
                "dose_text": _f(m.dose),
                "route_text": _f(m.route),
                "frequency_text": _f(m.frequency),
                "start": _f(m.start.isoformat()),
                "stop": _f(None if m.stop is None else m.stop.isoformat()),
                "status": _f(m.status),
            }
            for m in p.medications
        ]
        if with_clinical and enc.doc_type != "vitals_chart"
        else []
    )
    conds = (
        [
            {
                "text": _f(c.display),
                "local_code": _f(c.code),
                "local_system": _f("icd10"),
                "clinical_status": _f(c.clinical_status),
                "verification": _f(c.verification),
                "onset": _f(c.onset.isoformat()),
            }
            for c in p.conditions
        ]
        if enc.doc_type in ("clinic_note", "discharge_summary")
        else []
    )
    contact = f"Tel {p.phone}; {p.street_address}"
    return {
        "schema_version": "1.0.0",
        "document": {
            "type": _f(enc.doc_type),
            "date": _f(enc.end.date().isoformat()),
            "facility_name": _f(enc.facility),
            "language": _f("en"),
        },
        "patient": {
            "mrn": _f(p.mrn),
            "other_ids": other_ids,
            "family_name": _f(p.family_name),
            "given_names": _f(p.given_names),
            "date_of_birth": _f(p.date_of_birth.isoformat()),
            "sex": _f(p.sex),
        },
        "encounter": {
            "kind": _f(enc.kind),
            "start": _f(_stamp(enc.start)),
            "end": _f(_stamp(enc.end)),
            "location_text": _f(enc.facility),
            "clinician_text": _f(f"Dr {enc.clinician}"),
        },
        "observations": _observation_rows(enc),
        "medications": meds,
        "conditions": conds,
        "notes": [{"heading": _f("Contact"), "text": _f(contact)}],
    }


def _day_between(rng: random.Random, low: datetime.date, high: datetime.date) -> datetime.date:
    """A date in ``[low, high]``; never after ``high``, so never in the future."""
    return low + datetime.timedelta(days=rng.randint(0, max(0, (high - low).days)))


def _make_encounters(
    rng: random.Random, dob: datetime.date, abnormal: float, critical: float
) -> tuple[SynthEncounter, ...]:
    analytes = terminology.loinc()
    labs = tuple(a for a in analytes if a.category == "laboratory")
    vitals = tuple(a for a in analytes if a.category == "vital_sign")
    first = max(dob, _EARLIEST_ENCOUNTER)
    span = (_LATEST_ENCOUNTER - first).days
    out = []
    for _ in range(rng.randint(1, 3)):
        doc_type = _weighted(rng, _DOC_TYPES)
        day = first + datetime.timedelta(days=rng.randint(0, span))
        start = datetime.datetime.combine(
            day, datetime.time(rng.randint(6, 17), rng.randint(0, 59))
        )
        if doc_type == "discharge_summary":
            kind = "inpatient"
            end = start + datetime.timedelta(days=rng.randint(1, 10), minutes=rng.randint(0, 300))
        else:
            kind = _pick(rng, ("outpatient", "emergency"))
            end = start + datetime.timedelta(minutes=rng.randint(15, 240))
        end = min(end, _END_OF_LATEST)
        if doc_type == "lab_report":
            chosen = rng.sample(labs, rng.randint(4, 10))
        elif doc_type == "vitals_chart":
            chosen = rng.sample(vitals, rng.randint(2, len(vitals)))
        elif doc_type in ("clinic_note", "discharge_summary"):
            chosen = rng.sample(vitals, rng.randint(0, 3))
        else:
            chosen = []
        obs = []
        for a in chosen:
            flag = _wanted_flag(rng, a, abnormal, critical)
            value = _draw(rng, a, flag)
            collected = start + datetime.timedelta(minutes=rng.randint(0, 10))
            resulted = min(collected + datetime.timedelta(minutes=rng.randint(5, 120)), end)
            obs.append(SynthObservation(a, value, a.classify(value), collected, resulted))
        out.append(
            SynthEncounter(
                kind=kind,
                doc_type=doc_type,
                start=start,
                end=end,
                facility=_pick(rng, terminology.names("facilities")),
                clinician=_pick(rng, terminology.names("clinicians")),
                observations=tuple(obs),
            )
        )
    return tuple(sorted(out, key=lambda e: e.start))


def _make_medications(
    rng: random.Random, dob: datetime.date, listed_from: datetime.date
) -> tuple[SynthMedication, ...]:
    pool: tuple[Medication, ...] = terminology.rxnorm()
    out = []
    for m in rng.sample(pool, rng.randint(0, 3)):
        begin = _day_between(rng, max(dob, _EARLIEST_ENCOUNTER), listed_from)
        status = _pick(rng, ("active", "stopped", "completed"))
        stop = None if status == "active" else _day_between(rng, begin, listed_from)
        out.append(
            SynthMedication(
                m.code, m.name, m.strength, m.dose, m.route, m.frequency, begin, stop, status
            )
        )
    return tuple(out)


def _make_conditions(
    rng: random.Random, dob: datetime.date, listed_from: datetime.date
) -> tuple[SynthCondition, ...]:
    pool: tuple[Condition, ...] = terminology.icd10()
    out = []
    for c in rng.sample(pool, rng.randint(0, 3)):
        onset = _day_between(rng, max(dob, _EARLIEST_ENCOUNTER), listed_from)
        status = _pick(rng, ("active", "resolved", "history"))
        verification = _pick(rng, ("confirmed", "provisional"))
        out.append(SynthCondition(c.code, c.display, status, verification, onset))
    return tuple(out)


def _digits(rng: random.Random, count: int) -> str:
    return "".join(str(rng.randrange(10)) for _ in range(count))


def generate(
    rng: random.Random,
    n: int,
    *,
    abnormal_rate: float = ABNORMAL_RATE,
    critical_rate: float = CRITICAL_RATE,
) -> list[SynthPatient]:
    """``n`` synthetic patients, each with one ground-truth document per encounter."""
    if not 0.0 <= critical_rate <= abnormal_rate <= 1.0:
        raise ValueError("rates must satisfy 0 <= critical <= abnormal <= 1")
    given_names = terminology.names("given")
    family_names = terminology.names("family")
    streets = terminology.names("streets")
    dob_first, dob_last = _DOB_FIRST.toordinal(), _DOB_LAST.toordinal()
    patients: list[SynthPatient] = []
    for _ in range(n):
        dob = datetime.date.fromordinal(rng.randint(dob_first, dob_last))
        given = _pick(rng, given_names)
        if rng.random() < 0.25:
            given = f"{given} {_pick(rng, given_names)}"
        encounters = _make_encounters(rng, dob, abnormal_rate, critical_rate)
        # Medications and conditions start no later than the earliest document that lists them.
        listed_from = min(e.start.date() for e in encounters)
        base = SynthPatient(
            mrn="SYN-" + _digits(rng, 8),
            national_id="SYNID-" + _digits(rng, 8),
            given_names=given,
            family_name=_pick(rng, family_names),
            date_of_birth=dob,
            sex=_pick(rng, ("female", "male")),
            phone=f"555-01{rng.randrange(100):02d}",
            street_address=f"{rng.randint(1, 240)} {_pick(rng, streets)}",
            conditions=_make_conditions(rng, dob, listed_from),
            medications=_make_medications(rng, dob, listed_from),
            encounters=encounters,
            documents=(),
        )
        docs = tuple(_document(base, enc) for enc in base.encounters)
        patients.append(
            SynthPatient(**{**base.__dict__, "documents": docs})  # frozen dataclass: rebuild
        )
    return patients
