"""Nigerian PII for PIIMasker: names without a title, and Nigerian numbers.

PIIMasker's own rules find a name only after a title ("Dr", "Mrs",
"Patient"), so "Adaeze Okafor has chest pain" went through untouched
(ruling C8). This module adds, offline and on the CPU (no model, no
network; docs/privacy/masker-evaluation.md):

- names: a hand-written gazetteer of Nigerian given names and surnames
  (data/ng_names.txt, pinned by SHA-256), plus context rules: Nigerian titles
  and relationship words ("Alhaji", "Mama", "her daughter"), a capitalised
  word next to a known name, an initial before one, and name-forming
  affixes ("Oluwa-", "-chukwu"). A given name that is also an ordinary word
  (Grace, Hope, Sunday) counts only in context: next to another name, after
  a title, capitalised inside a sentence, or opening one with a verb after
  it ("Grace has a fever");
- numbers: NIN and BVN (11 digits), phone numbers (0 or +234, with the
  "(0)" trunk), written whole or in groups with spaces, hyphens, dots or
  brackets; labelled record numbers ("MRN", "hospital no", "folder no",
  "NIN", "BVN", "NHIS") with whatever code follows; and coded record numbers
  such as "LUTH/2019/12345" or "HAV-000123".

It fails closed. If the gazetteer can't be read, or its hash doesn't match,
detect() raises DetectorUnavailable, which PIIMasker.mask lets through: the
egress scan turns it into "scan_error" (a refusal), and a cloud job can't
collect its identifiers, so nothing is sent.
"""

from __future__ import annotations

import bisect
import hashlib
import math
import re
import unicodedata
from collections import defaultdict
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from functools import cache
from importlib import resources
from typing import Final

GAZETTEER_FILE: Final = "ng_names.txt"
GAZETTEER_SHA256: Final = "d97bb41df842f9a5d344af42e406f11b69e8ca112cf7908111c91f3eec0a5b26"
WORDS_FILE: Final = "en_words.txt"
WORDS_SHA256: Final = "51b48869ad29aec4d3fd14b910f883fa5bd9ff4814ac5f30ad4b8be4572eb18b"

# The name-likeness score (NameScore.score) above which a word that is in
# neither file is a name: capitalised, or in lower case. Set from
# masker_eval.calibrate() (docs/privacy/masker-evaluation.md), never from
# the evaluation set itself.
NAME_SCORE_MIN: Final = 0.0
NAME_SCORE_MIN_LOWER: Final = 0.75
MIN_SCORED_LETTERS: Final = 4
MIN_SCORED_LETTERS_LOWER: Final = 5
MAX_SCORED_LETTERS: Final = 24  # longer is not a name ("cccc...", a run-on)
# The longest text detect() reads (characters). Longer text is refused, not
# read in part: the egress body is at most 49,152 bytes, a question 8,000.
MAX_TEXT_CHARS: Final = 65_536
# PIIMasker's own placeholders, exactly (a test pins them to its
# REPLACEMENT_MAP). Only these are skipped as words: "[ADAEZE]" is read.
PLACEHOLDERS: Final = (
    "[SSN_REDACTED]",
    "[MRN_REDACTED]",
    "[PHONE_REDACTED]",
    "[EMAIL_REDACTED]",
    "[DOB_REDACTED]",
    "[DATE_REDACTED]",
    "[AGE_90+]",
    "[ZIP_REDACTED]",
    "[ADDRESS_REDACTED]",
    "[NAME_REDACTED]",
    "[NATIONAL_ID_REDACTED]",
)


class DetectorUnavailable(RuntimeError):
    """The name gazetteer couldn't be loaded or failed its hash. str() never
    holds text from the input."""


_ALPHABET: Final = 28  # a to z, the start and the end of a word


class CharModel:
    """A character trigram model of a set of words (add-k smoothing)."""

    def __init__(self, words: Iterable[str], k: float = 0.1) -> None:
        self._k = k
        self._tri: dict[str, int] = defaultdict(int)
        self._bi: dict[str, int] = defaultdict(int)
        for w in words:
            padded = f"^^{w}$"
            for i in range(2, len(padded)):
                self._tri[padded[i - 2 : i + 1]] += 1
                self._bi[padded[i - 2 : i]] += 1

    def logprob(self, word: str) -> float:
        padded = f"^^{word}$"
        total = 0.0
        for i in range(2, len(padded)):
            tri = self._tri.get(padded[i - 2 : i + 1], 0)
            bi = self._bi.get(padded[i - 2 : i], 0)
            total += math.log((tri + self._k) / (bi + self._k * _ALPHABET))
        return total


class NameScore:
    """How much more a word looks like a Nigerian name than an English or
    medical word: the log-likelihood ratio of two character models, per
    letter. Built from the two pinned files when they are loaded; there are
    no other weights."""

    def __init__(self, names: Iterable[str], known: Iterable[str]) -> None:
        self._names = CharModel(names)
        self._known = CharModel(known)

    def score(self, key: str) -> float:
        return (self._names.logprob(key) - self._known.logprob(key)) / (len(key) + 1)


@dataclass(frozen=True)
class Gazetteer:
    """Name tokens, folded (fold()). names are names wherever they appear;
    words are names that are also ordinary words, so they need context.
    known are English and medical words, never names by themselves."""

    names: frozenset[str]
    words: frozenset[str]
    known: frozenset[str] = frozenset()
    scorer: NameScore | None = None


def fold(s: str) -> str:
    """Accents removed and case-folded: "Adébáyọ̀" gives "adebayo"."""
    t = unicodedata.normalize("NFKD", s)
    return "".join(c for c in t if not unicodedata.combining(c)).casefold()


def parse_words(raw: bytes) -> frozenset[str]:
    """The known words from en_words.txt's bytes. Raises DetectorUnavailable
    on a hash mismatch or a line that isn't one lower-case word."""
    if hashlib.sha256(raw).hexdigest() != WORDS_SHA256:
        raise DetectorUnavailable("word list hash mismatch")
    words = set()
    for line in raw.decode("utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        if not (line.isascii() and line.isalpha() and line.islower()):
            raise DetectorUnavailable("word list line unreadable")
        words.add(line)
    if not words:
        raise DetectorUnavailable("word list empty")
    return frozenset(words)


def parse_gazetteer(raw: bytes, known_raw: bytes | None = None) -> Gazetteer:
    """The gazetteer from the files' bytes. Raises DetectorUnavailable on a
    hash mismatch, a line it can't read, or a token listed as both kinds.
    Without known_raw there is no known-word list and no score (tests)."""
    if hashlib.sha256(raw).hexdigest() != GAZETTEER_SHA256:
        raise DetectorUnavailable("name gazetteer hash mismatch")
    names: set[str] = set()
    words: set[str] = set()
    for line in raw.decode("utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        kind, _, token = line.partition(" ")
        if kind not in ("N", "W") or not token.isalpha() or token != fold(token):
            raise DetectorUnavailable("name gazetteer line unreadable")
        (names if kind == "N" else words).add(token)
    if not names or names & words:
        raise DetectorUnavailable("name gazetteer inconsistent")
    if known_raw is None:
        return Gazetteer(frozenset(names), frozenset(words))
    known = parse_words(known_raw) - names - words
    return Gazetteer(frozenset(names), frozenset(words), known, NameScore(names, known))


def _read(name: str) -> bytes:
    return resources.files("intelligence.security").joinpath("data", name).read_bytes()


@cache
def _cached_gazetteer() -> Gazetteer:
    return parse_gazetteer(_read(GAZETTEER_FILE), _read(WORDS_FILE))


def gazetteer() -> Gazetteer:
    """The gazetteer, read once. A failure is not cached: every call raises
    DetectorUnavailable until the file can be read."""
    try:
        return _cached_gazetteer()
    except DetectorUnavailable:
        raise
    except Exception:
        raise DetectorUnavailable("name gazetteer unreadable") from None


# --- words and context ----------------------------------------------------------------

# Titles and relationship words: a capitalised word after one is a name.
# Compared folded, with or without a full stop.
CUES: Final = frozenset(
    """
    mr mrs ms miss mister dr doctor prof professor patient pt alhaji alh alhaja hajiya hajia
    mallam malam chief pastor rev revd reverend engr engineer barr barrister arc pharm nurse
    matron madam mama mummy mum baba daddy dad iya aunty auntie uncle oga master prince
    princess oba emir otunba ichie igwe lolo ezinne deacon deaconess evangelist bishop
    elder comrade hon sir lady dame mother father son daughter wife husband brother sister
    baby child boy girl named called nee née grandmother grandfather granny grandma grandpa
    cousin niece nephew twin twins name kin
    """.split()
)
# Cues that a following "is" or "was" carries on: "Patient is Mojeed".
LINKED_CUES: Final = frozenset({"patient", "name", "pt"})

# Never a name: function words, common clinical words and Nigerian places
# and peoples. Only unknown words are checked against it (a word in the
# gazetteer is a name whatever this says), so it guards the context rules.
STOP: Final = frozenset(
    """
    a about above after again against all also am an and any are as at be because been before
    being below between both but by can could did do does doing down during each few for from
    further had has have having he her here hers herself him himself his how i if in into is it
    its itself just me more most my myself no nor not now of off on once only or other our ours
    out over own same she should so some such than that the their theirs them then there these
    they this those through to too under until up very was we were what when where which while
    who whom why will with would you your yours yourself please thanks thank yes ok okay
    hello hi dear regards kindly urgent urgently today yesterday tomorrow tonight morning
    evening night week weeks day days month months year years hour hours ago since last next
    new old now still already again ever never always often sometimes usually daily
    january february march april may june july august september october november december
    mon tue wed thu fri sat sun jan feb mar apr jun jul aug sep sept oct nov dec
    what's whats how's can't cannot won't don't doesn't isn't
    is are was were has have had do does did get gets got give gives given gave take takes took
    taken make makes made need needs needed want wants seen see saw seems seem came come comes
    went go goes going presented presents presenting present complains complained complaining
    reports reported reporting developed develops started starts stopped became become fell
    said says told tells asked asks admitted discharged referred brought treated started
    diagnosed known noted found showed shows tested tests started using used use uses
    history hx complaint complaints case cases question questions advice advise
    pain fever cough headache vomiting diarrhoea diarrhea rash bleeding swelling weakness
    fatigue dizziness seizure seizures convulsion convulsions jaundice anaemia anemia
    malaria typhoid tuberculosis tb hiv aids hepatitis diabetes hypertension asthma
    pneumonia sepsis meningitis cholera measles lassa ebola covid stroke cancer ulcer
    sickle cell disease syndrome infection infections fracture injury burn burns wound
    chest abdominal abdomen head neck back leg legs arm arms eye eyes ear ears skin heart
    kidney liver lung lungs blood urine stool breast pelvic joint joints throat mouth
    acute chronic severe mild moderate recurrent persistent sudden worsening worse better
    high low normal abnormal raised elevated positive negative left right bilateral
    pregnant pregnancy antenatal postnatal labour labor delivery caesarean section
    newborn neonate neonatal infant infants children adult adults elderly male female
    man woman men women boy boys girl girls mother mothers
    dose doses tablet tablets tab tabs injection injections infusion syrup capsule mg ml kg
    iv im po bd tds qds od prn stat daily weekly start stop continue switch reduce increase
    drug drugs medicine medicines medication medications treatment therapy regimen
    paracetamol ibuprofen diclofenac aspirin amoxicillin ampicillin augmentin ceftriaxone
    cefuroxime ciprofloxacin metronidazole gentamicin azithromycin erythromycin doxycycline
    cotrimoxazole artemether lumefantrine artesunate amodiaquine quinine sulfadoxine
    pyrimethamine coartem metformin glibenclamide insulin amlodipine lisinopril losartan
    nifedipine hydrochlorothiazide methyldopa labetalol furosemide spironolactone atenolol
    salbutamol prednisolone hydrocortisone dexamethasone omeprazole ranitidine
    oxytocin misoprostol magnesium sulphate sulfate iron folic acid zinc ors vitamin
    tenofovir lamivudine dolutegravir efavirenz isoniazid rifampicin pyrazinamide
    ethambutol fluconazole albendazole mebendazole ivermectin praziquantel olanzapine
    haloperidol diazepam phenobarbital phenytoin carbamazepine morphine tramadol
    hospital clinic ward unit theatre emergency department general teaching federal
    medical centre center health primary care phc fmc luth uch abuth unth lasuth lth
    lagos abuja kano ibadan enugu kaduna jos benin ilorin abeokuta onitsha owerri calabar
    uyo warri zaria sokoto maiduguri yola makurdi lokoja akure osogbo ekiti ogun oyo ondo
    osun kwara kogi delta edo rivers bayelsa anambra imo abia ebonyi cross river akwa ibom
    plateau nasarawa niger benue taraba adamawa borno yobe bauchi gombe jigawa katsina
    kebbi zamfara nigeria nigerian africa african port harcourt city state village town
    yoruba igbo hausa fulani ijaw efik ibibio tiv idoma igala nupe kanuri urhobo isoko
    itsekiri english christian muslim
    who nice nhis nafdac ncdc fmoh moh guideline guidelines protocol protocols
    mmol umol µmol mcmol nmol pmol mol mmhg kpa bpm mcg ug µg ng pg iu miu mu meq mosm dl ml
    mg kg cm mm km min mins hr hrs sec secs wk wks yr yrs yo kcal cmh mls mmols umols
    """.split()
)

# Name-forming affixes, for a capitalised word of five or more letters not
# in the gazetteer. Chosen to avoid drug and disease names ("ola" is in
# Olanzapine, "bola" in Ebola, "osa" in mucosa, "chi" in Chikungunya).
PREFIXES: Final = (
    "oluwa",
    "olu",
    "chukwu",
    "nwa",
    "oghene",
    "aondo",
    "abdul",
    "tamuno",
    "oritse",
    "ifeoluwa",
    "ifeany",
    "ebube",
    "uche",
    "ngoz",
    "ayo",
    "omo",
    "igbin",
    "ehi",
    "tari",
    "tare",
    "ebi",
)
SUFFIXES: Final = (
    "chukwu",
    "emeka",
    "ebuka",
    "nonso",
    "funmi",
    "bunmi",
    "yemi",
    "kemi",
    "wale",
    "tunde",
    "dayo",
    "yinka",
    "seun",
    "kunle",
    "femi",
    "hemba",
    "shima",
    "obong",
    "abasi",
    "owei",
    "oghene",
    "vwe",
    "ozie",
    "uyi",
    "nna",
)

# In lower case only the longer affixes count: no English word has them.
LOWER_PREFIXES: Final = (
    "oluwa",
    "chukwu",
    "oghene",
    "aondo",
    "abdul",
    "tamuno",
    "oritse",
    "ifeany",
    "ebube",
)
LOWER_SUFFIXES: Final = (
    "chukwu",
    "emeka",
    "ebuka",
    "nonso",
    "funmi",
    "bunmi",
    "tunde",
    "yinka",
    "hemba",
    "obong",
    "abasi",
    "owei",
    "oghene",
)
# Weekdays are given names too (Sunday, Friday), but "on Monday" is a day:
# by themselves, only a verb after them makes them a name.
WEEKDAYS: Final = frozenset("monday tuesday wednesday thursday friday saturday sunday".split())

# Words after an ordinary-word name at the start of a sentence that make it
# a name: "Grace has a fever", "Hope was seen".
PREDICATES: Final = frozenset(
    """
    has had have is was isn't wasn't presented presents complains complained reports reported
    developed came comes took takes needs needed got gets became fell started said says
    tested collapsed vomited delivered woke died bled coughed
    """.split()
)

_MARKS: Final = "̀-ͯ᪰-᫿᷀-᷿⃐-⃿︠-︯"
_TOKEN: Final = re.compile(rf"(?:[^\W\d_][{_MARKS}]*)+")
# The masker's own placeholders ("[NAME_REDACTED]"), exactly: never read
# as words. Any other bracketed text is read like the rest.
_PLACEHOLDER: Final = re.compile("|".join(re.escape(p) for p in PLACEHOLDERS))
# What may sit between two words of one name.
_NAME_GAP: Final = re.compile(r"\s{1,2}|\s?-\s?|\.\s{1,2}")
_SENTENCE_END: Final = frozenset(".!?:;\n\r•*")
_INITIAL_GAP: Final = re.compile(r"\.?\s{1,2}")
_POSSESSIVE: Final = re.compile(r"['’]s\b")


@dataclass
class _Tok:
    start: int
    end: int
    text: str
    key: str
    shape: str  # "title", "upper", "lower" or "mixed"
    kind: str  # "N", "W" or ""
    name: bool = False
    cue: bool = False


def _shape(t: str) -> str:
    letters = [c for c in t if c.isalpha()]
    if all(c.isupper() for c in letters):
        return "upper" if len(letters) > 1 else "title"
    if letters[0].isupper() and all(c.islower() for c in letters[1:]):
        return "title"
    if all(c.islower() for c in letters):
        return "lower"
    return "mixed"


def _tokens(text: str, gaz: Gazetteer) -> list[_Tok]:
    hidden = [m.span() for m in _PLACEHOLDER.finditer(text)]
    starts = [a for a, _ in hidden]
    out: list[_Tok] = []
    for m in _TOKEN.finditer(text):
        k = bisect.bisect_right(starts, m.start()) - 1
        if k >= 0 and m.start() < hidden[k][1]:
            continue
        key = fold(m.group())
        kind = "N" if key in gaz.names else "W" if key in gaz.words else ""
        out.append(_Tok(m.start(), m.end(), m.group(), key, _shape(m.group()), kind))
    for i, t in enumerate(out):
        t.cue = t.key in CUES and t.kind == ""
        # "Patient is X", "name was X": the verb carries the cue on.
        prev = out[i - 1] if i else None
        if t.key in ("is", "was") and prev is not None and prev.key in LINKED_CUES:
            t.cue = _joined(text, prev, t, True)
    return out


def _joined(text: str, a: _Tok, b: _Tok, after_cue: bool = False) -> bool:
    gap = text[a.end : b.start]
    if after_cue:
        return re.fullmatch(r"\.?:?\s{1,2}", gap) is not None
    return _NAME_GAP.fullmatch(gap) is not None


def _sentence_start(text: str, t: _Tok) -> bool:
    i = t.start - 1
    while i >= 0 and (text[i] in " \t\"'([“‘" or text[i] == " "):
        i -= 1
    return i < 0 or text[i] in _SENTENCE_END


def _strong(t: _Tok) -> bool:
    if t.kind != "N":
        return False
    if len(t.key) >= 4:
        return t.shape != "mixed" or t.text[0].isupper()
    return t.shape == "title"


def _morph(t: _Tok) -> bool:
    if len(t.key) < 5 or t.key in STOP:
        return False
    if t.shape == "lower":
        return t.key.startswith(LOWER_PREFIXES) or t.key.endswith(LOWER_SUFFIXES)
    if t.shape not in ("title", "upper"):
        return False
    return t.key.startswith(PREFIXES) or t.key.endswith(SUFFIXES)


def _scored(t: _Tok, gaz: Gazetteer) -> bool:
    """A word in neither file that looks like a Nigerian name."""
    if (
        gaz.scorer is None
        or t.kind
        or t.key in CUES
        or not MIN_SCORED_LETTERS <= len(t.key) <= MAX_SCORED_LETTERS
        or not (t.key.isascii() and t.key.isalpha())
        or t.key in gaz.known
        or t.key in STOP
    ):
        return False
    if t.shape == "title" or (t.shape == "upper" and len(t.key) >= 5):
        return gaz.scorer.score(t.key) >= NAME_SCORE_MIN
    if t.shape == "lower" and len(t.key) >= MIN_SCORED_LETTERS_LOWER:
        return gaz.scorer.score(t.key) >= NAME_SCORE_MIN_LOWER
    return False


def _can_extend(t: _Tok, neighbour: _Tok, gaz: Gazetteer) -> bool:
    """An unknown word next to a name is part of it: capitalised (all
    capitals of three letters only beside another all-capitals word), or
    lower case beside a lower-case name. Never an English or medical word
    or a function word."""
    if t.kind or t.key in CUES or t.key in STOP or t.key in gaz.known or len(t.key) < 2:
        return False
    if t.shape == "title":
        return True
    if t.shape == "upper":
        return len(t.key) >= 4 or (neighbour.shape == "upper" and len(t.key) >= 3)
    if t.shape == "lower":
        return neighbour.shape == "lower" and len(t.key) >= 3
    return False


def _word_extends(t: _Tok) -> bool:
    """A known name word beside a name is part of it; in lower case, a
    word that is also an ordinary word only when it isn't a function word
    ("okafor will" is not a name, "okafor grace" is)."""
    if not t.kind:
        return False
    return not (t.shape == "lower" and t.kind == "W" and t.key in STOP)


def _weak_alone(text: str, toks: list[_Tok], i: int) -> bool:
    """An ordinary-word or short name, by itself: capitalised inside a
    sentence, or opening one with a verb or "'s" after it."""
    t = toks[i]
    if t.shape == "upper" and len(t.key) >= 4:  # "BLESSING", not "ADA"
        return t.key not in WEEKDAYS
    if t.shape != "title":
        return False
    if not _sentence_start(text, t):
        return t.key not in WEEKDAYS
    nxt = toks[i + 1] if i + 1 < len(toks) else None
    if _POSSESSIVE.match(text, t.end) or text.startswith(",", t.end):
        return True
    return nxt is not None and nxt.key in PREDICATES and text[t.end : nxt.start].isspace()


def _spread(text: str, t: _Tok, neighbour: _Tok, gaz: Gazetteer, a: _Tok, b: _Tok) -> None:
    """t joins its neighbour's name (a and b are the two in text order)."""
    if t.name or not neighbour.name or not _joined(text, a, b):
        return
    # No limit on how far it spreads: a cap would send a long name's outer
    # parts. The two passes keep it linear.
    if _word_extends(t) or _can_extend(t, neighbour, gaz):
        t.name = True


def _name_tokens(text: str, gaz: Gazetteer) -> list[_Tok]:
    toks = _tokens(text, gaz)
    for i, t in enumerate(toks):
        prev = toks[i - 1] if i else None
        after_cue = prev is not None and prev.cue and _joined(text, prev, t, True)
        if _strong(t) or _morph(t) or _scored(t, gaz):
            t.name = True
        elif t.kind and (after_cue or _weak_alone(text, toks, i)):
            t.name = True
        elif after_cue and not t.cue and t.key not in STOP:
            if t.shape in ("title", "upper"):
                t.name = True
            elif t.shape == "lower" and len(t.key) >= 3 and t.key not in gaz.known:
                t.name = True  # "Mrs ojo"
    # Spread to neighbours: an ordinary-word or short name, or an unknown
    # word, beside a name is part of it. One pass rightwards, one leftwards
    # (linear: a word named on the way left already has a name on its right).
    for i in range(1, len(toks)):
        _spread(text, toks[i], toks[i - 1], gaz, toks[i - 1], toks[i])
    for i in range(len(toks) - 2, -1, -1):
        _spread(text, toks[i], toks[i + 1], gaz, toks[i], toks[i + 1])
    # An initial before a name: "A. Okafor", "A Okafor"; and before a
    # capitalised word that isn't a known word: "H. Barau".
    for i, t in enumerate(toks[:-1]):
        nxt = toks[i + 1]
        if t.name or len(t.text) != 1:
            continue
        gap = text[t.end : nxt.start]
        if nxt.name and _INITIAL_GAP.fullmatch(gap) and (t.text.isupper() or gap[0] == "."):
            t.name = True
        elif (
            t.text.isupper()
            and gap == ". "
            and nxt.shape in ("title", "upper")
            and not nxt.cue
            and nxt.key not in STOP
            and nxt.key not in gaz.known
            and len(nxt.key) >= 3
        ):
            t.name = nxt.name = True
    return [t for t in toks if t.name]


def name_spans(text: str) -> list[tuple[int, int]]:
    """Spans of names in text, each run of name words as one span. Raises
    DetectorUnavailable when the gazetteer can't be loaded."""
    gaz = gazetteer()
    spans: list[tuple[int, int]] = []
    toks = _name_tokens(text, gaz)
    for t in toks:
        if spans and _NAME_GAP.fullmatch(text[spans[-1][1] : t.start]):
            spans[-1] = (spans[-1][0], t.end)
        else:
            spans.append((t.start, t.end))
    return spans


# --- numbers --------------------------------------------------------------------------

_GROUP: Final = re.compile(r"\d+")
# Between two digit groups of one number: one space, hyphen or dot, or a
# short mix with brackets ("(0803) 123", "0803 - 123", "+234 (0) 803").
_NUM_GAP: Final = re.compile(r"[ .-]| - |\) ?| ?\(|\) ?\(|\)-")


MAX_ID_DIGITS: Final = 14  # "+234 (0) 803 123 4567"


def _digits(s: str) -> str:
    return "".join(str(unicodedata.digit(c)) for c in s)


def _number_kind(d: str) -> str | None:
    if len(d) == 11:
        return "PHONE" if d[0] == "0" and d[1] in "789" and d[2] in "01" else "NATIONAL_ID"
    if len(d) == 13 and d.startswith("234") and d[3] in "789" and d[4] in "01":
        return "PHONE"
    if len(d) == 14 and d.startswith("2340") and d[4] in "789" and d[5] in "01":
        return "PHONE"
    if len(d) == 10 and d[0] in "789" and d[1] in "01":
        return "PHONE"
    return None


def _is_trunk(chain: list[re.Match[str]]) -> bool:
    """The last group is the "0" trunk after 234: "+234 (0) 803"."""
    return (
        len(chain) >= 2 and chain[-1].group() == "0" and _digits(chain[-2].group()).endswith("234")
    )


def _chains(text: str) -> Iterator[list[re.Match[str]]]:
    chain: list[re.Match[str]] = []
    for m in _GROUP.finditer(text):
        if chain:
            prev = chain[-1]
            gap = text[prev.end() : m.start()]
            a, b = len(prev.group()), len(m.group())
            trunk = m.group() == "0" and _digits(prev.group()).endswith("234")
            ok = _NUM_GAP.fullmatch(gap) is not None and (
                (a >= 2 and b >= 2)
                or (a == b == 1 and gap == " ")
                or trunk
                or (b >= 2 and _is_trunk(chain))
            )
            if ok and gap == "." and (a < 2 or b < 2):  # 38.5 is a decimal
                ok = False
            if ok:
                chain.append(m)
                continue
            yield chain
        chain = [m]
    if chain:
        yield chain


def number_spans(text: str) -> list[tuple[int, int, str]]:
    """NIN, BVN and phone numbers written whole or in groups: (start, end,
    "NATIONAL_ID" or "PHONE"). A leading "+" or "(" is included."""
    out: list[tuple[int, int, str]] = []
    for chain in _chains(text):
        digits = [_digits(m.group()) for m in chain]
        used = [False] * len(chain)
        # Longest first, so "+234 803 123 4567" isn't also read as 10 digits.
        ranges: list[tuple[int, int]] = []
        for i in range(len(chain)):
            n = 0
            for j in range(i, len(chain)):
                n += len(digits[j])
                if n > MAX_ID_DIGITS:
                    break
                ranges.append((i, j))
        ranges.sort(key=lambda r: r[0] - r[1])
        for i, j in ranges:
            if any(used[i : j + 1]):  # at most 14 groups
                continue
            kind = _number_kind("".join(digits[i : j + 1]))
            if kind is None:
                continue
            start, end = chain[i].start(), chain[j].end()
            while start > 0 and text[start - 1] in "+(":
                start -= 1
            out.append((start, end, kind))
            used[i : j + 1] = [True] * (j + 1 - i)
    return out


_LABEL: Final = re.compile(
    r"(?<![A-Za-z])(?:"
    r"(?P<nid>nin|bvn|national\s?id(?:entity)?(?:\s?(?:no|num|number))?|nhis(?:\s?(?:no|number|id))?)"
    r"|(?P<tel>phone|tel|mobile|gsm|whatsapp)(?:\s?(?:no|num|number))?"
    r"|(?P<mrn>m\.?\s?r\.?\s?n|mr\s?#|(?:hosp(?:ital)?|folder|card|file|reg(?:istration)?|patient"
    r"|record|case|clinic|id|unit)\.?\s?(?:no|num|number|#|id))"
    r")(?![A-Za-z])\.?[\s:#=-]{0,4}"
    r"(?P<code>[A-Za-z0-9](?:[A-Za-z0-9/._-]|\s(?=\d))*)",
    re.IGNORECASE,
)
# A coded record number: capitals, then groups joined by "/" or "-", one of
# them three or more digits ("LUTH/2019/12345", "HAV-000123"). The token is
# read whole, however long (the input cap bounds it), and masked whole:
# there is no segment count or length that lets a longer code through.
# Linear: every group ends at a separator, so nothing backtracks.
_CODED_TOKEN: Final = re.compile(r"(?<![A-Za-z0-9])[A-Z]+(?:[/-][A-Za-z0-9]+)+")
_DIGIT_GROUP: Final = re.compile(r"(?:^|[/-])[0-9]{3,}")


def label_spans(text: str) -> list[tuple[int, int, str]]:
    """A labelled identifier's code ("MRN: 445 812", "NIN 1234 5678 901",
    "folder no. LUTH/AE/0451") when it holds three or more digits, and coded
    record numbers without a label."""
    out: list[tuple[int, int, str]] = []
    for m in _LABEL.finditer(text):
        start, end = m.span("code")
        while end > start and text[end - 1] in "./_-":
            end -= 1
        if sum(c.isdigit() for c in text[start:end]) < 3:
            continue
        kind = "NATIONAL_ID" if m.group("nid") else "PHONE" if m.group("tel") else "MRN"
        out.append((start, end, kind))
    for m in _CODED_TOKEN.finditer(text):
        if len(m.group().split("/")[0].split("-")[0]) >= 2 and _DIGIT_GROUP.search(m.group()):
            out.append((m.start(), m.end(), "MRN"))
    return out


def detect(text: str) -> list[tuple[int, int, str]]:
    """Every Nigerian name and identifier in text: (start, end, type), type
    one of PIIMasker.REPLACEMENT_MAP's keys. Raises DetectorUnavailable when
    the name gazetteer can't be loaded, or the text is longer than
    MAX_TEXT_CHARS (fail closed: it is refused, never read in part)."""
    if len(text) > MAX_TEXT_CHARS:
        raise DetectorUnavailable("text longer than the detector reads")
    spans = [(a, b, "NAME") for a, b in name_spans(text)]
    spans.extend(number_spans(text))
    spans.extend(label_spans(text))
    return spans
