"""Small terminology subsets and the synthetic name lists.

Only LOINC (see ``terminology/NOTICE``), ICD-10 and RxNorm; no SNOMED CT.
Numbers in the JSON files are decimal strings, so no float is ever parsed.
"""

import json
from dataclasses import dataclass
from decimal import Decimal
from functools import cache
from importlib import resources
from typing import Any

_BASE = ("doceval", "synth")


def _read(*parts: str) -> str:
    return resources.files("intelligence").joinpath(*_BASE, *parts).read_text(encoding="utf-8")


def _entries(name: str) -> list[dict[str, Any]]:
    loaded = json.loads(_read("terminology", f"{name}.json"))
    entries = loaded["entries"]
    if not isinstance(entries, list):
        raise RuntimeError("terminology file malformed")
    return entries


def _dec(text: str | None) -> Decimal | None:
    return None if text is None else Decimal(text)


@dataclass(frozen=True)
class Analyte:
    """A LOINC observation with its ranges.

    Classification: ``HH`` at or above ``critical_high``, ``LL`` at or below
    ``critical_low``, ``H`` above ``ref_high``, ``L`` below ``ref_low``, else ``N``.
    """

    code: str
    name: str
    display: str
    category: str
    specimen: str | None
    unit: str
    decimals: int
    ref_low: Decimal | None
    ref_high: Decimal | None
    plausible_low: Decimal
    plausible_high: Decimal
    critical_low: Decimal | None
    critical_high: Decimal | None

    def classify(self, value: Decimal) -> str:
        if self.critical_high is not None and value >= self.critical_high:
            return "HH"
        if self.critical_low is not None and value <= self.critical_low:
            return "LL"
        if self.ref_high is not None and value > self.ref_high:
            return "H"
        if self.ref_low is not None and value < self.ref_low:
            return "L"
        return "N"

    def format(self, value: Decimal) -> str:
        return f"{value:.{self.decimals}f}"

    def reference_text(self) -> str:
        if self.ref_low is not None and self.ref_high is not None:
            return f"{self.format(self.ref_low)}-{self.format(self.ref_high)}"
        if self.ref_low is not None:
            return f">={self.format(self.ref_low)}"
        assert self.ref_high is not None
        return f"<={self.format(self.ref_high)}"


@dataclass(frozen=True)
class Condition:
    code: str
    display: str


@dataclass(frozen=True)
class Medication:
    code: str
    name: str
    strength: str
    form: str
    route: str
    dose: str
    frequency: str
    display: str


@cache
def loinc() -> tuple[Analyte, ...]:
    out = []
    for e in _entries("loinc"):
        plow, phigh = _dec(e["plausible_low"]), _dec(e["plausible_high"])
        assert plow is not None and phigh is not None
        out.append(
            Analyte(
                code=e["code"],
                name=e["name"],
                display=e["display"],
                category=e["category"],
                specimen=e["specimen"],
                unit=e["unit"],
                decimals=int(e["decimals"]),
                ref_low=_dec(e["ref_low"]),
                ref_high=_dec(e["ref_high"]),
                plausible_low=plow,
                plausible_high=phigh,
                critical_low=_dec(e["critical_low"]),
                critical_high=_dec(e["critical_high"]),
            )
        )
    return tuple(out)


@cache
def icd10() -> tuple[Condition, ...]:
    return tuple(Condition(e["code"], e["display"]) for e in _entries("icd10"))


@cache
def rxnorm() -> tuple[Medication, ...]:
    return tuple(
        Medication(
            e["code"],
            e["name"],
            e["strength"],
            e["form"],
            e["route"],
            e["dose"],
            e["frequency"],
            e["display"],
        )
        for e in _entries("rxnorm")
    )


NAME_LISTS = ("given", "family", "streets", "facilities", "clinicians")


@cache
def names(kind: str) -> tuple[str, ...]:
    """A synthetic list; the file must be headed ``# synthetic``."""
    if kind not in NAME_LISTS:
        raise ValueError("unknown list")
    lines = _read("names", f"{kind}.txt").splitlines()
    if not lines or not lines[0].startswith("# synthetic"):
        raise RuntimeError("name list is not marked synthetic")
    return tuple(ln for ln in lines if ln and not ln.startswith("#"))


@cache
def name_set(kind: str) -> frozenset[str]:
    return frozenset(names(kind))


@cache
def all_name_tokens() -> frozenset[str]:
    """Every word in the person-name lists (given, family, clinicians)."""
    tokens: set[str] = set()
    for kind in ("given", "family", "clinicians"):
        for entry in names(kind):
            tokens.update(entry.split())
    return frozenset(tokens)
