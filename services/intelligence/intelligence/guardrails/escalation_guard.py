"""The escalation guard: question-level checks that send an answer to a clinician whatever
its grounding says (ADR 0017).

NLI checks that each sentence of an answer is supported by the chunk it cites. It cannot
tell whether the answer is the right response to the question: a reply that recites the
on-topic guideline to a red-flag question is fully grounded. This guard runs on every
route (``boundary.models._verify_with``: local, cloud, the answer gate, ``eval-served``),
before a NLI decision can stand, and can only make a decision stricter:

1. **red_flag**: the masked question or the patient's current data shows a danger sign
   (``red_flags`` in the rule set). ESCALATE.
2. **missing_information**: the question asks for a judgement about this patient
   (a diagnosis, a severity, whether they can go home, a child's dose) and none of the
   values that judgement needs is given. ESCALATE.
3. **out_of_scope**: the question asks for a dose, regimen or titration and no cited
   chunk gives a dose for the drug (or, with no drug named, the condition) it asks about.
   ESCALATE.
4. **recital**: every answer sentence is a near-verbatim copy of a chunk sentence and the
   answer is not relevant to the question (the pinned embedder, or a lexical fallback).
   A PASS becomes the rule set's action (REGENERATE).

The rules are data: ``rules/escalation-guard.yaml``, versioned, its SHA-256 pinned here
(:data:`RULES_SHA256`). Nothing is learned at run time. A rule set that is missing,
malformed or not the pinned one raises :class:`RuleSetError`: the worker refuses to start
(``boundary.worker.run_service``), and a verification that can't load it escalates with
``guard_unavailable``. The verdict holds codes only (reasons and rule ids), never text;
nothing here logs the question, the patient data or the answer.

Reading the text: the masked question, plus the patient's ``symptoms`` and
``medical_history`` (history counts only for the shared contexts, such as pregnancy:
a past stroke is not a red flag), normalised (NFKC, casefold, unicode dashes and quotes,
``≥``/``≤``, degree signs removed, whitespace collapsed). Values are read as a label and a
number (``glucose of 22 mmol/L``, ``BP 85/50``, ``SpO2 88%``, ``a 9 kg child``) in the
analyte's canonical unit, or from a structured field; ages from the structured ``age``
(years) and phrases such as ``3-month-old`` or ``36 hours old``.
"""

from __future__ import annotations

import bisect
import functools
import hashlib
import logging
import math
import os
import re
import threading
import unicodedata
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

log = logging.getLogger(__name__)

SCHEMA = "mhai.escalation_guard.rules/1"
NAME = "escalation-guard"
RULES_PATH = Path(__file__).resolve().parent / "rules" / "escalation-guard.yaml"
# The reviewed rule set's SHA-256. An edit to the YAML must change this too (and the
# version): an unreviewed edit refuses to load.
RULES_SHA256 = "41576549f60b41094157a73b39e35e60364a4bf120d44ceaf78413d1964a50e7"

RED_FLAG = "red_flag"
MISSING_INFORMATION = "missing_information"
OUT_OF_SCOPE = "out_of_scope"
RECITAL = "recital"
GUARD_UNAVAILABLE = "guard_unavailable"
# Every reason code, in severity order. core's audit schema holds the same list
# (config.GuardReasons; TestGuardReasonsMatchWorker).
REASONS: tuple[str, ...] = (
    "red_flag",
    "missing_information",
    "out_of_scope",
    "recital",
    "guard_unavailable",
)
ESCALATING = frozenset({RED_FLAG, MISSING_INFORMATION, OUT_OF_SCOPE, GUARD_UNAVAILABLE})

MAX_RULES_BYTES = 256 * 1024
# The longest text read: core caps the question at 4,000 characters, each of 50 symptoms
# at 200 and the history at 4,000 (masking can lengthen them). Longer is refused
# (guard_unavailable), never truncated: a cut could drop the red flag.
MAX_TEXT = 40_000
MAX_REPLY = 40_000
DERIVED = ("age_years", "age_hours")
OPS = ("<", "<=", ">", ">=", "missing")
ACTIONS = ("REGENERATE", "ESCALATE")
_ID = re.compile(r"[a-z0-9][a-z0-9-]{0,47}")
_VERSION = re.compile(r"(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})")
_WORD = re.compile(r"[a-z0-9]+(?:['-][a-z0-9]+)*")
_PHRASE_END = re.compile(r"[.;,:!?()\[\]\n]")
_SENTENCE = re.compile(r"(?<=[.!?])\s+|\n+")
_CITATION = re.compile(r"\s*\[KB#(?:\d{4}|[1-9]\d{6})\]")
_CITED = re.compile(r"\[(KB#(?:\d{4}|[1-9]\d{6}))\]")
_NUMBER = r"(\d+(?:\.\d+)?)"
# Words a label and its number may have between them ("glucose came back at 450").
# Unambiguous on purpose (ReDoS on clinician input): each step is a sign, or one
# whitespace run and one word, and no word alternative splits into two others; the
# repetition is possessive, so it never backtracks into itself.
_FILLER = (
    r"(?:\s*[:=~<>]|\s+(?:of|is|was|at|on|reading|readings|level|levels|value|values|"
    r"measured|now|reads|read|came back|come back|returned|recorded|noted|around|about|"
    r"approximately|approx|roughly|just|only|over|under|above|below|up to|down to|"
    r"today|currently|still|in the)\b)*+\s*"
)
# After a bare number: a time, a count or another unit means it isn't this value.
_NOT_BARE = re.compile(
    r"\s*(?::\d|/\s*\d|-\s*\d|%|h\b|hrs?\b|hours?\b|mins?\b|minutes?\b|am\b|pm\b|days?\b|"
    r"weeks?\b|wks?\b|months?\b|years?\b|yrs?\b|y/?o\b|times\b|x\b|mg|g/|mmol|mm\b|cm\b|kg\b|"
    r"ml\b|units?\b|tablets?\b|doses?\b|episodes?\b|children\b|kids\b)"
)
_FAHRENHEIT = re.compile(r"\s*(?:f\b|fahrenheit|degrees? f(?:ahrenheit)?\b)")
_BP_LABEL = r"\b(?:bp|blood pressure|b/p|pressure)\b"
_BP_PAIR = re.compile(
    r"(?:(" + _BP_LABEL + r")" + _FILLER + r")?\b(\d{2,3})\s*/\s*(\d{2,3})\b(\s*mm ?hg)?"
)
_AGES: tuple[tuple[re.Pattern[str], float], ...] = (
    # (pattern, hours per unit); the first group that matched is the number.
    (
        re.compile(
            r"\b(\d+(?:\.\d+)?)\s*-?\s*(?:years?|yrs?)\s*-?\s*old\b|\b(\d+)\s*(?:yo|y/o|y\.o)\b|"
            r"\baged?\s+(\d+(?:\.\d+)?)(?:\s*(?:years?|yrs?))?\b(?!\s*(?:months?|weeks?|days?|"
            r"hours?|hrs?|mo\b|wks?|h\b))"
        ),
        8766.0,
    ),
    (
        re.compile(r"\b(\d+)\s*-?\s*(?:months?|mo)\s*-?\s*old\b|\baged?\s+(\d+)\s*months?\b"),
        730.5,
    ),
    (
        re.compile(r"\b(\d+)\s*-?\s*(?:weeks?|wks?)\s*-?\s*old\b|\baged?\s+(\d+)\s*weeks?\b"),
        168.0,
    ),
    (
        re.compile(
            r"\b(\d+)\s*-?\s*days?\s*-?\s*old\b|\bday\s+(\d+)\s+of\s+life\b|"
            r"\b(\d+)(?:st|nd|rd|th)\s+day\s+of\s+life\b|\baged?\s+(\d+)\s*days?\b"
        ),
        24.0,
    ),
    (
        re.compile(
            r"\b(\d+)\s*-?\s*(?:hours?|hrs?|h)\s*-?\s*(?:old|of age|of life|after (?:birth|"
            r"delivery))\b|\b(?:at|aged?)\s+(\d+)\s*(?:hours?|hrs?|h)\b(?!\s*(?:ago|later))"
        ),
        1.0,
    ),
)
_STOP = frozenset(
    "what which when where does have with this that their there they them from your about "
    "should would could will shall into than then also only just over under after before "
    "patient patients give given take taken dose doses dosing dosage regimen start started "
    "much many often should need needs needed make made used using like some more most less "
    "much very each other been being were here help tell know guideline guidelines shared "
    "case question answer please first line".split()
)


class RuleSetError(Exception):
    """The rule set can't be used. ``str()`` is a fixed code, never file content."""


# ---------------------------------------------------------------- normalising


def normalise(text: str) -> str:
    t = unicodedata.normalize("NFKC", text).casefold()
    for a, b in (
        ("≥", ">="),
        ("≤", "<="),
        ("’", "'"),
        ("‘", "'"),
        ("‐", "-"),
        ("‑", "-"),
        ("‒", "-"),
        ("–", "-"),
        ("—", "-"),
        ("−", "-"),
        ("°", ""),
        ("º", ""),
        ("µ", "u"),
        ("μ", "u"),
    ):
        t = t.replace(a, b)
    return " ".join(t.split())


def _words(text: str) -> list[str]:
    return _WORD.findall(text)


# ---------------------------------------------------------------- the facts


@dataclass
class Facts:
    """What the checks read: the current text (question and symptoms), the context text
    (that plus the history) and every value read, in canonical units."""

    text: str
    context_text: str
    values: dict[str, list[float]] = field(default_factory=dict)

    def has(self, analyte: str) -> bool:
        return bool(self.values.get(analyte))


# ---------------------------------------------------------------- conditions


class _Cond:
    def holds(self, facts: Facts, rs: RuleSet) -> bool:  # pragma: no cover - abstract
        raise NotImplementedError


@dataclass(frozen=True)
class _Any(_Cond):
    patterns: tuple[re.Pattern[str], ...]
    unless_value: str | None = None
    negate: bool = False
    in_context: bool = False

    def holds(self, facts: Facts, rs: RuleSet) -> bool:
        if self.unless_value is not None and facts.has(self.unless_value):
            return self.negate
        text = facts.context_text if self.in_context else facts.text
        found = any(rs.matches(p, text) for p in self.patterns)
        return found != self.negate


@dataclass(frozen=True)
class _All(_Cond):
    parts: tuple[_Cond, ...]

    def holds(self, facts: Facts, rs: RuleSet) -> bool:
        return all(p.holds(facts, rs) for p in self.parts)


@dataclass(frozen=True)
class _OneOf(_Cond):
    parts: tuple[_Cond, ...]

    def holds(self, facts: Facts, rs: RuleSet) -> bool:
        return any(p.holds(facts, rs) for p in self.parts)


@dataclass(frozen=True)
class _Value(_Cond):
    analyte: str
    op: str
    value: float

    def holds(self, facts: Facts, rs: RuleSet) -> bool:
        vals = facts.values.get(self.analyte) or []
        if self.op == "missing":
            return not vals
        cmp: Callable[[float], bool] = {
            "<": lambda v: v < self.value,
            "<=": lambda v: v <= self.value,
            ">": lambda v: v > self.value,
            ">=": lambda v: v >= self.value,
        }[self.op]
        return any(cmp(v) for v in vals)


# ---------------------------------------------------------------- the rule set


@dataclass(frozen=True)
class Analyte:
    name: str
    label: re.Pattern[str]
    units: tuple[tuple[re.Pattern[str], float], ...]
    bare_max: float | None
    bare_factor: float
    bare_above: float | None  # factor for a bare number above bare_max; None: not read
    low: float
    high: float
    structured: tuple[str, ...]
    fahrenheit: bool
    unit_alone: tuple[re.Pattern[str], ...]
    before: tuple[tuple[re.Pattern[str], float], ...]  # a number and unit, then the label


@dataclass(frozen=True)
class Rule:
    id: str
    group: str
    when: _Cond


@dataclass(frozen=True)
class MissingRule:
    id: str
    asks: _Cond
    any_value: tuple[str, ...]
    any: tuple[re.Pattern[str], ...]


@dataclass(frozen=True)
class DoseScope:
    asks: _Cond
    change_asks: _Cond  # counts only when the question names a drug
    threshold_asks: _Cond
    dose: re.Pattern[str]
    not_dose: tuple[re.Pattern[str], ...]
    drugs: tuple[re.Pattern[str], ...]
    synonyms: tuple[tuple[re.Pattern[str], ...], ...]


@dataclass(frozen=True)
class Recital:
    near_verbatim: float
    min_words: int
    embedder: str
    min_relevance: float
    action: str


@dataclass(frozen=True)
class RuleSet:
    version: str
    sha256: str
    status: str
    cues: tuple[tuple[str, ...], ...]
    window: int
    breaks: frozenset[str]
    analytes: tuple[Analyte, ...]
    red_flags: tuple[Rule, ...]
    missing: tuple[MissingRule, ...]
    dose: DoseScope
    recital: Recital

    @property
    def version_string(self) -> str:
        """``escalation-guard@<version>#<first 12 hex of the SHA-256>``: the pinned form
        core's model_versions accepts."""
        return f"{NAME}@{self.version}#{self.sha256[:12]}"

    # --- reading words

    def negated(self, text: str, start: int) -> bool:
        """A negation cue within ``window`` words before ``start``, in the same phrase.
        Reads the text's index (built once per text), so each call is O(log n)."""
        ends, starts, words = _text_index(text)
        k = bisect.bisect_right(ends, start)
        phrase_start = ends[k - 1] if k else 0
        lo = bisect.bisect_left(starts, phrase_start)  # the phrase's first word
        hi = bisect.bisect_left(starts, start)  # words before start
        for i in range(hi - 1, max(lo - 1, hi - 1 - self.window), -1):
            if words[i] in self.breaks:
                return False
            for cue in self.cues:
                n = len(cue)
                if i - n + 1 >= lo and tuple(words[i - n + 1 : i + 1]) == cue:
                    return True
        return False

    def matches(self, pattern: re.Pattern[str], text: str) -> bool:
        return any(not self.negated(text, m.start()) for m in pattern.finditer(text))

    # --- reading values

    def read_values(self, text: str, patient: Mapping[str, Any]) -> dict[str, list[float]]:
        out: dict[str, list[float]] = {}

        def add(name: str, v: float, low: float = -math.inf, high: float = math.inf) -> None:
            if math.isfinite(v) and low <= v <= high:
                out.setdefault(name, []).append(v)

        for a in self.analytes:
            for m in a.label.finditer(text):
                v = float(m.group(1))
                after = text[m.end() : m.end() + 40]
                got = _convert(a, v, after)
                if got is not None:
                    add(a.name, got, a.low, a.high)
            for p, factor in a.before:
                for m in p.finditer(text):
                    add(a.name, float(m.group(1)) * factor, a.low, a.high)
            for p in a.unit_alone:
                for m in p.finditer(text):
                    add(a.name, float(m.group(1)), a.low, a.high)
            for key in a.structured:
                for v in _structured(patient, key):
                    add(a.name, v, a.low, a.high)
            if a.fahrenheit:
                for m in re.finditer(r"\b" + _NUMBER + r"\s*(?:f|fahrenheit|degrees? f)\b", text):
                    add(a.name, (float(m.group(1)) - 32.0) / 1.8, a.low, a.high)
        for m in _BP_PAIR.finditer(text):
            sbp, dbp = float(m.group(2)), float(m.group(3))
            if (m.group(1) or m.group(4) or sbp > dbp) and 40 <= sbp <= 300 and 20 <= dbp <= 200:
                if sbp > dbp:
                    add("systolic_bp", sbp)
                    add("diastolic_bp", dbp)
        for p, hours in _AGES:
            for m in p.finditer(text):
                n = next((g for g in m.groups() if g is not None), None)
                if n is not None:
                    h = float(n) * hours
                    add("age_hours", h, 0, 130 * 8766)
                    add("age_years", h / 8766.0, 0, 130)
        age = patient.get("age")
        if isinstance(age, int | float) and not isinstance(age, bool) and 0 <= age <= 130:
            add("age_years", float(age))
            if age >= 1:
                add("age_hours", float(age) * 8766.0)
        return out


@functools.lru_cache(maxsize=32)
def _text_index(text: str) -> tuple[list[int], list[int], list[str]]:
    """(phrase-end offsets, word start offsets, words) of ``text``, once per text."""
    ends = [m.end() for m in _PHRASE_END.finditer(text)]
    found = [(m.start(), m.group(0)) for m in _WORD.finditer(text)]
    return ends, [o for o, _ in found], [w for _, w in found]


def _convert(a: Analyte, v: float, after: str) -> float | None:
    for p, factor in a.units:
        if p.match(after):
            return v * factor
    if a.fahrenheit and _FAHRENHEIT.match(after):
        return (v - 32.0) / 1.8
    if a.bare_max is None or _NOT_BARE.match(after):
        return None
    if v <= a.bare_max:
        return v * a.bare_factor
    return None if a.bare_above is None else v * a.bare_above


def _structured(patient: Mapping[str, Any], key: str) -> list[float]:
    found: list[float] = []
    for scope in (patient, patient.get("vitals"), patient.get("labs")):
        if isinstance(scope, Mapping):
            v = scope.get(key)
            if isinstance(v, int | float) and not isinstance(v, bool):
                found.append(float(v))
    return found


# ---------------------------------------------------------------- loading


class _UniqueKeyLoader(yaml.SafeLoader):
    """safe_load that refuses a duplicate key (a second rule silently replacing the first)."""


def _mapping(loader: _UniqueKeyLoader, node: yaml.MappingNode, deep: bool = False) -> Any:
    keys = [loader.construct_object(k, deep=deep) for k, _ in node.value]
    if len(keys) != len(set(map(repr, keys))):
        raise RuleSetError("duplicate_key")
    return loader.construct_mapping(node, deep=deep)


_UniqueKeyLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def _bad(code: str) -> RuleSetError:
    return RuleSetError(code)


def _keys(
    obj: Any,
    required: set[str],
    optional: frozenset[str] | set[str] = frozenset(),
    code: str = "keys",
) -> dict[str, Any]:
    if not isinstance(obj, dict) or not required <= set(obj) or not set(obj) <= required | optional:
        raise _bad(code)
    return obj


def _regex(p: Any) -> re.Pattern[str]:
    if not isinstance(p, str) or not p or len(p) > 4000 or p != p.lower():
        raise _bad("pattern")
    try:
        return re.compile(p)
    except re.error:
        raise _bad("pattern") from None


def _number(v: Any, code: str = "number") -> float:
    if not isinstance(v, int | float) or isinstance(v, bool) or not math.isfinite(v):
        raise _bad(code)
    return float(v)


def _str_list(v: Any, code: str) -> list[str]:
    if not isinstance(v, list) or not all(isinstance(s, str) and s for s in v):
        raise _bad(code)
    return v


class _Builder:
    def __init__(self, analytes: set[str]) -> None:
        self.analytes = analytes | set(DERIVED)
        self.contexts: dict[str, _Cond] = {}

    def cond(self, obj: Any, in_context: bool = False) -> _Cond:
        if not isinstance(obj, dict) or not obj:
            raise _bad("condition")
        if "any" in obj:
            o = _keys(obj, {"any"}, {"unless_value", "negate"}, "condition")
            pats = o["any"]
            if not isinstance(pats, list) or not pats:
                raise _bad("condition")
            unless = o.get("unless_value")
            if unless is not None and unless not in self.analytes:
                raise _bad("analyte")
            negate = o.get("negate", False)
            if not isinstance(negate, bool):
                raise _bad("condition")
            return _Any(tuple(_regex(p) for p in pats), unless, negate, in_context)
        if len(obj) != 1:
            raise _bad("condition")
        ((k, v),) = obj.items()
        if k in ("all", "one_of"):
            if not isinstance(v, list) or not v:
                raise _bad("condition")
            parts = tuple(self.cond(p, in_context) for p in v)
            return _All(parts) if k == "all" else _OneOf(parts)
        if k == "value":
            o = _keys(v, {"analyte", "op", "value"}, code="condition")
            if o["analyte"] not in self.analytes:
                raise _bad("analyte")
            if o["op"] not in OPS:
                raise _bad("op")
            return _Value(o["analyte"], o["op"], _number(o["value"]))
        if k == "ref":
            if v not in self.contexts:  # only an earlier context: no cycles
                raise _bad("ref")
            return self.contexts[v]
        raise _bad("condition")


def _analyte(name: str, obj: Any) -> Analyte:
    o = _keys(
        obj,
        {"unit", "labels", "units", "bare", "plausible", "structured"},
        {"fahrenheit", "unit_alone"},
        "analyte",
    )
    if not _ID.fullmatch(name.replace("_", "-")) or name in DERIVED:
        raise _bad("analyte")
    units = o["units"]
    if not isinstance(units, dict) or not units:
        raise _bad("analyte")
    unit_res = tuple(
        (re.compile(r"\s*(?:" + _regex(u).pattern + r")"), _number(f)) for u, f in units.items()
    )
    if any(f <= 0 for _, f in unit_res):
        raise _bad("analyte")
    bare = o["bare"]
    bare_max: float | None = None
    bare_factor = 1.0
    bare_above: float | None = None
    if bare is not None:
        b = _keys(bare, {"max", "factor"}, {"above"}, code="analyte")
        bare_max, bare_factor = _number(b["max"]), _number(b["factor"])
        if "above" in b:
            bare_above = _number(b["above"])
    plaus = o["plausible"]
    if not isinstance(plaus, list) or len(plaus) != 2:
        raise _bad("analyte")
    low, high = _number(plaus[0]), _number(plaus[1])
    if low >= high:
        raise _bad("analyte")
    label = _regex(o["labels"]).pattern
    # "a 450 mg/dL glucose": a number and one of the units, then the label.
    before = tuple(
        (
            re.compile(
                r"\b" + _NUMBER + r"\s*(?:" + str(u) + r")\s+(?:[a-z]+\s+){0,2}?(?:" + label + r")"
            ),
            f,
        )
        for u, (_, f) in zip(units, unit_res, strict=True)
    )
    fahrenheit = o.get("fahrenheit", False)
    if not isinstance(fahrenheit, bool):
        raise _bad("analyte")
    alone = tuple(
        re.compile(r"\b" + _NUMBER + r"\s*(?:" + _regex(u).pattern + r")")
        for u in _str_list(o.get("unit_alone", []), "analyte")
    )
    return Analyte(
        name=name,
        label=re.compile(r"(?:" + label + r")" + _FILLER + _NUMBER + r"(?!\d|\.\d)"),
        units=unit_res,
        bare_max=bare_max,
        bare_factor=bare_factor,
        bare_above=bare_above,
        low=low,
        high=high,
        structured=tuple(_str_list(o["structured"], "analyte")),
        fahrenheit=fahrenheit,
        unit_alone=alone,
        before=before,
    )


def parse(raw: bytes, sha256: str) -> RuleSet:
    """The rule set in ``raw`` (whose SHA-256 is ``sha256``), checked whole."""
    try:
        doc = yaml.load(raw.decode("utf-8"), Loader=_UniqueKeyLoader)  # noqa: S506 - safe subclass
    except RuleSetError:
        raise
    except (yaml.YAMLError, UnicodeDecodeError):
        raise _bad("yaml") from None
    top = _keys(
        doc,
        {
            "schema",
            "name",
            "version",
            "status",
            "clinical_signoff",
            "description",
            "negation",
            "analytes",
            "contexts",
            "red_flags",
            "missing_information",
            "dose_scope",
            "recital",
        },
        code="top",
    )
    if top["schema"] != SCHEMA or top["name"] != NAME:
        raise _bad("schema")
    if not isinstance(top["version"], str) or not _VERSION.fullmatch(top["version"]):
        raise _bad("version")
    if top["status"] not in ("pilot", "signed"):
        raise _bad("status")
    if top["clinical_signoff"] not in ("pending", "signed"):
        raise _bad("status")
    neg = _keys(top["negation"], {"cues", "window", "breaks"}, code="negation")
    window = neg["window"]
    if not isinstance(window, int) or isinstance(window, bool) or not 1 <= window <= 8:
        raise _bad("negation")
    cues = tuple(tuple(c.split()) for c in _str_list(neg["cues"], "negation"))
    breaks = frozenset(_str_list(neg["breaks"], "negation"))

    raw_analytes = top["analytes"]
    if not isinstance(raw_analytes, dict) or not raw_analytes:
        raise _bad("analyte")
    analytes = tuple(_analyte(str(k), v) for k, v in raw_analytes.items())
    b = _Builder({a.name for a in analytes})

    contexts = top["contexts"]
    if not isinstance(contexts, dict):
        raise _bad("contexts")
    for name, c in contexts.items():
        if not isinstance(name, str) or not _ID.fullmatch(name.replace("_", "-")):
            raise _bad("contexts")
        b.contexts[name] = b.cond(c, in_context=True)

    seen: set[str] = set()

    def rid(v: Any) -> str:
        if not isinstance(v, str) or not _ID.fullmatch(v) or v in seen:
            raise _bad("rule_id")
        seen.add(v)
        return v

    flags_in = top["red_flags"]
    if not isinstance(flags_in, list) or not flags_in:
        raise _bad("red_flags")
    flags: list[Rule] = []
    for f in flags_in:
        o = _keys(f, {"id", "group", "when", "source"}, code="red_flags")
        if not isinstance(o["source"], str) or not o["source"].strip():
            raise _bad("source")
        if not isinstance(o["group"], str) or not _ID.fullmatch(o["group"].replace("_", "-")):
            raise _bad("red_flags")
        flags.append(Rule(rid(o["id"]), o["group"], b.cond(o["when"])))

    miss_in = top["missing_information"]
    if not isinstance(miss_in, list) or not miss_in:
        raise _bad("missing_information")
    missing: list[MissingRule] = []
    for m in miss_in:
        o = _keys(m, {"id", "asks", "requires", "source"}, code="missing_information")
        if not isinstance(o["source"], str) or not o["source"].strip():
            raise _bad("source")
        req = _keys(o["requires"], set(), {"any_value", "any"}, "missing_information")
        if not req:
            raise _bad("missing_information")
        vals = tuple(_str_list(req.get("any_value", []), "missing_information"))
        if not set(vals) <= b.analytes:
            raise _bad("analyte")
        pats = tuple(_regex(p) for p in _str_list(req.get("any", []), "missing_information"))
        missing.append(MissingRule(rid(o["id"]), b.cond(o["asks"]), vals, pats))

    d = _keys(
        top["dose_scope"],
        {"asks", "change_asks", "threshold_asks", "synonyms", "dose", "not_dose", "drugs"},
        code="dose_scope",
    )
    drugs = _str_list(d["drugs"], "dose_scope")
    groups = d["synonyms"]
    if not drugs or not isinstance(groups, list):
        raise _bad("dose_scope")
    synonyms = tuple(
        tuple(re.compile(r"\b" + re.escape(w) + r"\b") for w in _str_list(g, "dose_scope"))
        for g in groups
    )
    if any(len(g) < 2 for g in synonyms):
        raise _bad("dose_scope")
    dose = DoseScope(
        asks=b.cond(d["asks"]),
        change_asks=b.cond(d["change_asks"]),
        threshold_asks=b.cond(d["threshold_asks"]),
        dose=_regex(d["dose"]),
        not_dose=tuple(_regex(p) for p in _str_list(d["not_dose"], "dose_scope")),
        drugs=tuple(re.compile(r"\b(?:" + _regex(p).pattern + r")\b") for p in drugs),
        synonyms=synonyms,
    )

    r = _keys(
        top["recital"],
        {"near_verbatim", "min_words", "embedder", "min_relevance", "action"},
        code="recital",
    )
    near, rel = _number(r["near_verbatim"]), _number(r["min_relevance"])
    mw = r["min_words"]
    if (
        not 0.5 <= near <= 1.0
        or not 0.0 <= rel <= 1.0
        or not isinstance(mw, int)
        or isinstance(mw, bool)
        or mw < 1
        or not isinstance(r["embedder"], str)
        or not r["embedder"]
        or r["action"] not in ACTIONS
    ):
        raise _bad("recital")
    return RuleSet(
        version=top["version"],
        sha256=sha256,
        status=top["status"],
        cues=cues,
        window=window,
        breaks=breaks,
        analytes=analytes,
        red_flags=tuple(flags),
        missing=tuple(missing),
        dose=dose,
        recital=Recital(near, mw, r["embedder"], rel, r["action"]),
    )


def load(path: Path | str = RULES_PATH, expected_sha256: str | None = RULES_SHA256) -> RuleSet:
    """The rule set at ``path``. With ``expected_sha256`` (the default: the pinned one) a
    file with any other SHA-256 is refused. Every failure is :class:`RuleSetError`."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as fh:
            raw = fh.read(MAX_RULES_BYTES + 1)
    except OSError:
        raise _bad("unreadable") from None
    if not raw or len(raw) > MAX_RULES_BYTES:
        raise _bad("size")
    sha = hashlib.sha256(raw).hexdigest()
    if expected_sha256 is not None and sha != expected_sha256:
        raise _bad("sha256")
    return parse(raw, sha)


_lock = threading.Lock()
_cached: RuleSet | None = None


def default() -> RuleSet:
    """The packaged, pinned rule set, loaded once. Raises :class:`RuleSetError` on every
    call while it can't be loaded (the caller escalates; the worker refuses to start)."""
    global _cached
    with _lock:
        if _cached is None:
            _cached = load(RULES_PATH, RULES_SHA256)
        return _cached


# ---------------------------------------------------------------- the checks


@dataclass(frozen=True)
class Verdict:
    """Codes only: the reasons (:data:`REASONS`) and the ids of the rules that fired."""

    reasons: tuple[str, ...] = ()
    rule_ids: tuple[str, ...] = ()

    @property
    def escalate(self) -> bool:
        return any(r in ESCALATING for r in self.reasons)


UNAVAILABLE = Verdict((GUARD_UNAVAILABLE,), ())

Embedder = Callable[[Sequence[str]], Any]


def facts_for(rs: RuleSet, question: str, patient: Mapping[str, Any]) -> Facts:
    """The text and values of one question. Raises ValueError when too long to read."""
    parts = [str(question)]
    symptoms = patient.get("symptoms")
    if isinstance(symptoms, list | tuple):
        parts.extend(str(s) for s in symptoms if isinstance(s, str))
    history = patient.get("medical_history")
    text = normalise(" . ".join(parts))
    context = text + (" . " + normalise(history) if isinstance(history, str) else "")
    if len(context) > MAX_TEXT:
        raise ValueError("too long")
    return Facts(text, context, rs.read_values(text, patient))


def _sentences(text: str) -> list[str]:
    return [s.strip() for s in _SENTENCE.split(text) if s.strip()]


_ANY_NUMBER = re.compile(r"\d")


def _evidence_sentences(rs: RuleSet, text: str, threshold: bool) -> list[str]:
    """The sentences of ``text`` that give a dose (a threshold: any number), with test
    loads (``75 g OGTT``) removed first."""
    out = []
    for sent in _sentences(normalise(text)):
        cleaned = sent
        for p in rs.dose.not_dose:
            cleaned = p.sub(" ", cleaned)
        if (rs.dose.dose if not threshold else _ANY_NUMBER).search(cleaned):
            out.append(cleaned)
    return out


def _content(rs: RuleSet, text: str) -> set[str]:
    """Content words (four or more characters, starting with a letter, not a stop word)
    and the synonym groups present (``syn<n>``)."""
    words = {w for w in re.findall(r"[a-z][a-z0-9]{3,}", text) if w not in _STOP}
    words |= {f"syn{i}" for i, g in enumerate(rs.dose.synonyms) if any(p.search(text) for p in g)}
    return words


def _named(rs: RuleSet, text: str) -> set[str]:
    return {m.group(0) for p in rs.dose.drugs for m in p.finditer(text)}


def dose_in_scope(
    rs: RuleSet, facts: Facts, cited_texts: Sequence[str], threshold: bool = False
) -> bool:
    """A cited sentence gives a dose (a threshold: a number) for the drug or treatment the
    question names (the same word); with none named, a cited chunk gives one and shares a
    content word or synonym group with the question."""
    named = _named(rs, facts.text)
    question_words = _content(rs, facts.text)
    for chunk in cited_texts:
        sentences = _evidence_sentences(rs, chunk, threshold)
        if not sentences:
            continue
        if named:
            if any(named & _named(rs, s) for s in sentences):
                return True
        elif question_words & _content(rs, normalise(chunk)):
            return True
    return False


def check_question(
    rs: RuleSet,
    question: str,
    patient: Mapping[str, Any],
    cited_texts: Sequence[str],
) -> Verdict:
    """Checks 1-3 (red flags, missing information, dose scope). Never raises: anything
    that goes wrong is :data:`UNAVAILABLE` (escalate)."""
    try:
        facts = facts_for(rs, question, patient if isinstance(patient, Mapping) else {})
        reasons: list[str] = []
        ids: list[str] = []
        hits = [r.id for r in rs.red_flags if r.when.holds(facts, rs)]
        if hits:
            reasons.append(RED_FLAG)
            ids.extend(hits)
        for m in rs.missing:
            if m.asks.holds(facts, rs):
                given = any(facts.has(a) for a in m.any_value) or any(
                    p.search(facts.context_text) for p in m.any
                )
                if not given:
                    if MISSING_INFORMATION not in reasons:
                        reasons.append(MISSING_INFORMATION)
                    ids.append(m.id)
        dose_ask = rs.dose.asks.holds(facts, rs) or (
            bool(_named(rs, facts.text)) and rs.dose.change_asks.holds(facts, rs)
        )
        if dose_ask and not dose_in_scope(rs, facts, cited_texts):
            reasons.append(OUT_OF_SCOPE)
            ids.append("dose-scope")
        elif (
            not dose_ask
            and rs.dose.threshold_asks.holds(facts, rs)
            and not dose_in_scope(rs, facts, cited_texts, threshold=True)
        ):
            reasons.append(OUT_OF_SCOPE)
            ids.append("threshold-scope")
        return Verdict(tuple(reasons), tuple(ids))
    except Exception as e:  # noqa: BLE001 - fail closed
        log.warning("escalation_guard.failed check=question error=%s", type(e).__name__)
        return UNAVAILABLE


def _tokens(text: str) -> set[str]:
    return set(re.findall(r"[a-z0-9]+", text))


def _near_verbatim(rs: RuleSet, sentence: set[str], chunk_sentences: Sequence[set[str]]) -> bool:
    for c in chunk_sentences:
        small = min(len(sentence), len(c))
        if small and len(sentence & c) / small >= rs.recital.near_verbatim:
            return True
    return False


def is_recital(
    rs: RuleSet,
    question: str,
    reply: str,
    chunk_texts: Sequence[str],
    embed: Embedder | None,
) -> bool:
    """Check 4: every answer sentence copies a chunk sentence and the answer is not
    relevant to the question. Raises on a failure (the caller escalates)."""
    if len(reply) > MAX_REPLY:
        raise ValueError("too long")
    sents = [
        s
        for s in _sentences(normalise(_CITATION.sub("", reply)))
        if len(_words(s)) >= rs.recital.min_words
    ]
    if not sents:
        return False
    chunk_sents = [_tokens(s) for c in chunk_texts for s in _sentences(normalise(c))]
    if not all(_near_verbatim(rs, _tokens(s), chunk_sents) for s in sents):
        return False
    q = normalise(question)
    if embed is not None:
        vecs = [list(map(float, v)) for v in embed([q, *sents])]
        qv = vecs[0]
        best = max(_cosine(qv, v) for v in vecs[1:])
        return best < rs.recital.min_relevance
    return not (_content(rs, q) & _content(rs, " ".join(sents)))


def _cosine(a: Sequence[float], b: Sequence[float]) -> float:
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(x * x for x in b))
    if na == 0 or nb == 0:
        return 0.0
    return sum(x * y for x, y in zip(a, b, strict=True)) / (na * nb)


def cited_ids(reply: str, cited: Sequence[str]) -> list[str]:
    """The chunk ids an answer cites: its recorded citations and its exact markers."""
    out = list(dict.fromkeys([*cited, *_CITED.findall(reply)]))
    return out


__all__ = [
    "ESCALATING",
    "GUARD_UNAVAILABLE",
    "MISSING_INFORMATION",
    "OUT_OF_SCOPE",
    "REASONS",
    "RECITAL",
    "RED_FLAG",
    "RULES_PATH",
    "RULES_SHA256",
    "UNAVAILABLE",
    "Facts",
    "RuleSet",
    "RuleSetError",
    "Verdict",
    "check_question",
    "cited_ids",
    "default",
    "dose_in_scope",
    "facts_for",
    "is_recital",
    "load",
    "normalise",
    "parse",
]
