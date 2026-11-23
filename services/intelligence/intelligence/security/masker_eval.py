"""The masker's evaluation on a synthetic Nigerian PII set (TODO.md, the
real-data gate, item 1; docs/privacy/masker-evaluation.md).

Everything here is generated from a fixed seed: invented combinations of
common names, random digits in the Nigerian formats, and clinical questions
with no identifier in them. No real person or record is used.

Three measurements:

- recall per class (NAME, NIN, BVN, PHONE, MRN): an identifier counts as
  found when PIIMasker's detections cover every letter and digit of it.
  Names are also split into "seen" (every word in the gazetteer) and
  "held_out" (at least one word not in it), by group and by casing;
- egress recall: the same identifiers through the cloud path as it runs
  (collect_identifiers on the raw question, the masked question in the
  body, scan_body): found when the scan refuses or the identifier is gone
  from what would be sent;
- the false-refusal rate: clinical questions and guideline chunks with no
  identifier, through the same path; refused is a false refusal. Over-masking
  (the masker changed a question with nothing to mask) is reported beside
  it, and so is the refusal rate of questions that do name a patient.
"""

from __future__ import annotations

import json
import random
import re
from collections import defaultdict
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Final

from intelligence.security import ng_pii

SEED: Final = 20261116
N_NAME_SAMPLES: Final = 3000
N_PER_NUMBER_CLASS: Final = 1000
N_CLEAN_QUESTIONS: Final = 600

# --- synthetic names --------------------------------------------------------------------
# Common Nigerian given names and surnames, combined at random. Some words
# are deliberately not in the gazetteer (data/ng_names.txt), to measure the
# context rules on names it doesn't know; which words those are is computed,
# not listed.

GROUPS: Final[dict[str, tuple[list[str], list[str]]]] = {
    "yoruba": (
        (
            "Adebayo Folake Oluwaseun Temitope Kehinde Yetunde Babatunde Ayomide Morenike "
            "Olumide Ibukunoluwa Oluwadarasimi Ayobamidele Iretiola Moyosore Adesewa "
            "Toluwalase Opemipo Ireoluwa Fisayo Ajibike"
        ).split(),
        (
            "Adeyemi Ogunleye Balogun Afolabi Olaniyan Ojo Akinola Popoola Adesanya Oyelaran "
            "Ogunjimi Adebiyi Olatunbosun Fakeye Odebunmi Akinsola Oyekunle"
        ).split(),
    ),
    "igbo": (
        (
            "Adaeze Chinedu Ngozi Obinna Ifeoma Kelechi Nnamdi Uchenna Chioma Ebuka Amarachi "
            "Chinemerem Ebubedike Kaosisochukwu Ifechukwude Olaedo Ugomma Nkemdirim "
            "Chukwudumebi Amauche"
        ).split(),
        (
            "Okafor Nwosu Okonkwo Eze Okeke Nwachukwu Anyanwu Obi Ugwu Okoro Nwaobi Okoli "
            "Ezenwafor Nwigwe Onwuegbu Ubaka Ezeokoli Nwoye"
        ).split(),
    ),
    "hausa": (
        (
            "Abubakar Aisha Musa Zainab Ibrahim Hadiza Aminu Hauwa Yusuf Halima Sani Nasara "
            "Shamsiyya Sumayya Abdulmalik Firdausi Habibu Harira"
        ).split(),
        (
            "Abdullahi Bello Usman Garba Mohammed Danjuma Yakubu Aliyu Lawal Maigari Inusa "
            "Barau Maishanu Yahuza Kabara"
        ).split(),
    ),
    "ijaw": (
        (
            "Ebiere Preye Tonye Timi Tamara Boma Ebikeme Seiyefa Ebipade Tarebi Ebiweni "
            "Pereowei Ayebatari Tubotamuno"
        ).split(),
        "Dappa Amachree Briggs Fubara Ikiriko Opuama Okpokiri Igbikiowubo Ogbomade Diriyai Bebeke".split(),
    ),
    "edo": (
        (
            "Osagie Efosa Itohan Ivie Osaro Isoken Eghosa Ofure Osarumwense Imuetinyan "
            "Ekhosuehi Omosigho Ewemade Nosakhare"
        ).split(),
        (
            "Omoregie Iyamu Aigbe Ekhator Osayande Evbuomwan Ogbeide Edebiri Ugiagbe Agbonifo "
            "Omonkhua Aimuanmwosa"
        ).split(),
    ),
    "efik_ibibio": (
        "Edidiong Uduak Nsikak Ekaete Aniekan Imaobong Enoabasi Iboro Ofonime Itohowo Uwem".split(),
        "Bassey Etim Akpan Effiong Okon Essien Udosen Ekere Ntuk Usoro Okpo".split(),
    ),
    "middle_belt": (
        "Terver Doosuur Msughter Mnena Ojonugwa Ochanya Ene Aondohemba Kumawuese Ojoma Onyemowo".split(),
        "Iorliam Agber Odeh Ameh Onoja Attah Iortsuun Akpen Ogbaji Ejembi Adejo".split(),
    ),
    "urhobo_itsekiri": (
        "Oghenetega Ejiro Onome Ovie Ufuoma Tsola Oghenemaro Efemena Erhiga Akpoguma".split(),
        "Mukoro Okumagba Esiri Ighodaro Emiko Ovwigho Abavo Ejovwoke Edafiaga".split(),
    ),
}
# Given names used across groups.
CHRISTIAN: Final = (
    "Grace Blessing Joseph Mary Emmanuel Peace Favour Victor Esther Godwin Patience Hope Joy "
    "Sunday Precious Comfort Faith Mercy Gift Samuel Deborah Michael Ezra Hephzibah Keziah "
    "Barnabas Jemima"
).split()
MUSLIM: Final = (
    "Kazeem Rasheed Kafayat Fatai Simbiat Taofeek Abdulfatai Mojeed Rukayat Qudus Sodiq"
).split()

NAME_TEMPLATES: Final = (
    "{n} has had a fever for 3 days. Could it be malaria?",
    "{n}, 34F, presents with chest pain and shortness of breath.",
    "Please review {n} who is 28 weeks pregnant with BP 150/100.",
    "What dose of artesunate for {n}, weight 20 kg?",
    "{n}'s cough has lasted 3 weeks; should we test for TB?",
    "Seen today: {n}. Complains of headache and neck stiffness.",
    "Is metformin safe for {n} with eGFR 40?",
    "Mrs {n} has severe anaemia, Hb 6.",
    "Alhaji {n} has uncontrolled diabetes.",
    "Mama {n} reports dizziness since yesterday.",
    "Child: {n}, 4 years, convulsions with fever.",
    "Referred from PHC: {n} with jaundice.",
    "{n} and {m} (twins) both have diarrhoea.",
    "Discuss {n} on the ward round tomorrow.",
    "Can {n} take ibuprofen with asthma?",
    "Follow-up for {n} after appendectomy.",
    "The mother, {n}, says the baby is not feeding well.",
    "history: {n} known sickle cell, now bone pain",
    "pt {n} c/o abdominal pain x2/7",
    "Chest pain for 2 days in {n}",
    "Abdominal pain. Name: {n}.",
    "Which antibiotic for {n}?",
    "{n} was admitted last night with vomiting.",
    "her daughter {n} has a rash and fever",
    "Patient is {n}, 62M, with a productive cough.",
    "Pls advise on {n}: HIV positive, CD4 180.",
)
HISTORY_TEMPLATES: Final = (
    "Seen by {n}'s GP last month.",
    "Lives with {n} (husband).",
    "Next of kin: {n}.",
    "{n} reports poor adherence to ART.",
)

# --- synthetic numbers --------------------------------------------------------------------

PHONE_PREFIXES: Final = (
    "0803 0806 0703 0706 0813 0816 0810 0814 0903 0906 0805 0807 0705 0815 0811 0905 "
    "0802 0808 0708 0812 0701 0902 0901 0907 0809 0817 0818 0909 0908"
).split()
NUMBER_TEMPLATES: Final[dict[str, tuple[str, ...]]] = {
    "NIN": (
        "NIN {x}",
        "Her NIN is {x}, please update the record.",
        "National ID number: {x}",
        "nin: {x} and fever",
        "ID {x} on the card",
        "{x}",
    ),
    "BVN": (
        "BVN {x}",
        "BVN: {x} for the HMO form",
        "his bvn is {x}",
        "Payment ref {x} (bank verification)",
    ),
    "PHONE": (
        "call {x} about chest pain",
        "Her husband's number is {x}.",
        "phone: {x}",
        "Tel {x}",
        "reach the mother on {x} if worse",
        "{x} is the daughter's whatsapp",
    ),
    "MRN": (
        "MRN {x}",
        "MRN: {x}, fever 3 days",
        "Hosp No. {x}",
        "hospital number {x}",
        "folder no {x}",
        "card number {x} at the clinic",
        "MR# {x}",
        "Patient no {x} on ward 3",
    ),
}
# Coded record numbers stand alone, without a label.
CODED_MRN_TEMPLATES: Final = (
    "Record {x} seen in clinic.",
    "Folder {x}: fever and cough",
    "{x} - review results",
)


def _groups(rng: random.Random, digits: str, sizes: Sequence[int], sep: str) -> str:
    out, i = [], 0
    for n in sizes:
        out.append(digits[i : i + n])
        i += n
    return sep.join(out)


def _eleven(rng: random.Random, digits: str) -> str:
    form = rng.randrange(8)
    if form == 0:
        return digits
    sizes = rng.choice([(3, 4, 4), (4, 4, 3), (4, 3, 4), (5, 6), (3, 3, 3, 2), (2, 3, 3, 3)])
    sep = rng.choice([" ", "-", " ", ".", " - "])
    return _groups(rng, digits, sizes, sep)


def _phone(rng: random.Random) -> str:
    local = rng.choice(PHONE_PREFIXES) + "".join(rng.choice("0123456789") for _ in range(7))
    form = rng.randrange(10)
    sep = rng.choice([" ", "-", " ", "."])
    if form == 0:
        return local
    if form == 1:
        return "+234" + local[1:]
    if form == 2:
        return "+234 " + _groups(rng, local[1:], (3, 3, 4), sep)
    if form == 3:
        return "+234 (0) " + _groups(rng, local[1:], (3, 3, 4), " ")
    if form == 4:
        return "(" + local[:4] + ") " + _groups(rng, local[4:], (3, 4), " ")
    if form == 5:
        return "234-" + _groups(rng, local[1:], (3, 3, 4), "-")
    if form == 6:
        return _groups(rng, local, (4, 4, 3), sep)
    if form == 7:
        return _groups(rng, local, (3, 4, 4), sep)
    return _groups(rng, local, (4, 3, 4), sep)


def _mrn(rng: random.Random) -> str:
    d = "".join(rng.choice("0123456789") for _ in range(8))
    form = rng.randrange(9)
    if form == 0:
        return d[: rng.randint(5, 8)]
    if form == 1:
        return f"{d[:3]} {d[3:6]}"
    if form == 2:
        return f"PLT-{d[:6]}"
    if form == 3:
        return f"LUTH/{rng.randint(2015, 2026)}/{d[:5]}"
    if form == 4:
        return f"UCH/{d[:2]}/{d[2:8]}"
    if form == 5:
        return f"ABUTH-{d[:7]}"
    if form == 6:
        return f"LASUTH/AE/{rng.randint(2015, 2026)}/{d[:4]}"
    if form == 7:
        return f"FMC-OW-{d[:2]}-{d[2:6]}"
    return f"{d[:2]}/{d[2:6]}"


def _coded_mrn(rng: random.Random) -> str:
    form = rng.randrange(4)
    d = "".join(rng.choice("0123456789") for _ in range(7))
    return (
        f"LUTH/{rng.randint(2015, 2026)}/{d[:5]}",
        f"PLT-{d[:6]}",
        f"UCH/{d[:2]}/{d[2:7]}",
        f"FMC-OW-{d[:2]}-{d[2:6]}",
    )[form]


def _number(rng: random.Random, cls: str) -> str:
    if cls == "NIN":
        return _eleven(
            rng, str(rng.randint(1, 9)) + "".join(rng.choice("0123456789") for _ in range(10))
        )
    if cls == "BVN":
        return _eleven(rng, "22" + "".join(rng.choice("0123456789") for _ in range(9)))
    if cls == "PHONE":
        return _phone(rng)
    return _mrn(rng)


# --- samples ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Sample:
    """One synthetic text and the identifiers in it (character spans)."""

    text: str
    cls: str
    spans: tuple[tuple[int, int], ...]
    history: str = ""
    tags: tuple[str, ...] = ()


def _case(rng: random.Random, s: str) -> tuple[str, str]:
    r = rng.random()
    if r < 0.7:
        return s, "title"
    if r < 0.85:
        return s.upper(), "upper"
    return s.lower(), "lower"


def _person(rng: random.Random) -> tuple[str, str]:
    group = rng.choice(sorted(GROUPS))
    given_pool, family_pool = GROUPS[group]
    r = rng.random()
    if r < 0.2:
        given, given_group = rng.choice(CHRISTIAN), "christian"
    elif r < 0.28:
        given, given_group = rng.choice(MUSLIM), "muslim"
    else:
        given, given_group = rng.choice(given_pool), group
    family = rng.choice(family_pool)
    form = rng.random()
    if form < 0.45:
        name = f"{given} {family}"
    elif form < 0.55:
        name = f"{family.upper()} {given}"
    elif form < 0.7:
        name, group = given, given_group
    elif form < 0.8:
        name = family
    elif form < 0.92:
        middle = rng.choice(given_pool)
        name = f"{given} {middle} {family}"
    else:
        name = f"{family[0] if rng.random() < 0.5 else given[0]}. {family}"
    return name, group


def _fill(template: str, values: dict[str, str]) -> tuple[str, list[tuple[int, int]]]:
    """template with each {key} replaced, and the spans of the values."""
    out, spans, pos = [], [], 0
    for m in re.finditer(r"\{(\w)\}", template):
        out.append(template[pos : m.start()])
        start = sum(len(p) for p in out)
        out.append(values[m.group(1)])
        spans.append((start, start + len(values[m.group(1)])))
        pos = m.end()
    out.append(template[pos:])
    return "".join(out), spans


def name_samples(rng: random.Random, n: int = N_NAME_SAMPLES) -> list[Sample]:
    out: list[Sample] = []
    for i in range(n):
        name, group = _person(rng)
        name, casing = _case(rng, name)
        if i % 5 == 4:  # in the record's history, not the question
            history, spans = _fill(rng.choice(HISTORY_TEMPLATES), {"n": name})
            out.append(Sample("chest pain", "NAME", tuple(spans), history, (group, casing)))
            continue
        other, _ = _person(rng)
        text, spans = _fill(rng.choice(NAME_TEMPLATES), {"n": name, "m": _case(rng, other)[0]})
        out.append(Sample(text, "NAME", tuple(spans), "", (group, casing)))
    return out


def number_samples(rng: random.Random, n: int = N_PER_NUMBER_CLASS) -> list[Sample]:
    out: list[Sample] = []
    for cls in ("NIN", "BVN", "PHONE", "MRN"):
        for i in range(n):
            if cls == "MRN" and i % 4 == 3:
                template, x = rng.choice(CODED_MRN_TEMPLATES), _coded_mrn(rng)
            else:
                template, x = rng.choice(NUMBER_TEMPLATES[cls]), _number(rng, cls)
            text, spans = _fill(template, {"x": x})
            out.append(Sample(text, cls, tuple(spans)))
    return out


# --- clean questions and guidelines ----------------------------------------------------

CLEAN_TEMPLATES: Final = (
    "A {age}-year-old {sex} from {place} has had fever for {d} days. {drug} or {drug2}?",
    "{drug} dose for a child of {kg} kg with {disease}?",
    "Is {drug} safe in pregnancy at {w} weeks?",
    "{disease}: what are the red flags in a {age}-year-old?",
    "BP {sys}/{dia}, HR {hr}, T {temp}, RR {rr}, SpO2 {sat}. Next step for {disease}?",
    "Hb {hb} g/dL, WBC {wbc}, platelets {plt}. Is this {disease}?",
    "Seen on {weekday} with {symptom} since {month}. Manage as {disease}?",
    "Referred from {hospital} with {symptom}. Should we start {drug}?",
    "{symptom} and {symptom2} for {d} days, travelled to {place}. Lassa fever?",
    "What does the {org} guideline say about {disease} in children under 5?",
    "Can {drug} be given with {drug2} in {disease}?",
    "{eponym}: how is it treated here?",
    "Patient with {disease} on {drug} {dose} mg twice daily: switch to {drug2}?",
    "Glucose {glu} mmol/L, HbA1c {a1c}%. Start insulin?",
    "When to refer {symptom} in a neonate at {hospital}?",
    "Our PHC in {place} has no {drug}. Alternatives for {disease}?",
    "Post-partum haemorrhage after delivery on {weekday}: oxytocin dose?",
    "Lives in {town}; took {brand} for {d} days without improvement.",
    "{brand} or {brand2} for fever in a child from {town}?",
    "{brand} was bought at a chemist in {town}. Is it genuine?",
    "Travelled from {town} to {place}; now {symptom}. {brand} given.",
    "{drug} stock-out: is {drug2} acceptable for {disease}?",
)
VOCAB: Final[dict[str, tuple[str, ...]]] = {
    "sex": ("woman", "man", "girl", "boy"),
    "place": (
        "Kano",
        "Lagos",
        "Ibadan",
        "Enugu",
        "Port Harcourt",
        "Abuja",
        "Benin City",
        "Jos",
        "Calabar",
        "Maiduguri",
        "Owerri",
        "Ilorin",
    ),
    "drug": (
        "Artesunate",
        "Coartem",
        "Paracetamol",
        "Amoxicillin",
        "Ceftriaxone",
        "Metformin",
        "Amlodipine",
        "Insulin",
        "Oxytocin",
        "Misoprostol",
        "Magnesium sulphate",
        "Ibuprofen",
        "Augmentin",
        "Ciprofloxacin",
        "Metronidazole",
        "Dolutegravir",
        "Tenofovir",
        "Isoniazid",
        "Rifampicin",
        "Hydroxyurea",
        "Folic acid",
        "Salbutamol",
        "Prednisolone",
        "Nifedipine",
        "Methyldopa",
        "Labetalol",
        "Olanzapine",
        "Haloperidol",
        "Phenobarbital",
        "Quinine",
        "Albendazole",
    ),
    "disease": (
        "malaria",
        "typhoid fever",
        "sickle cell crisis",
        "pre-eclampsia",
        "pneumonia",
        "tuberculosis",
        "HIV",
        "diabetes",
        "hypertension",
        "Lassa fever",
        "cholera",
        "measles",
        "meningitis",
        "sepsis",
        "Burkitt lymphoma",
        "Ebola",
        "neonatal jaundice",
        "asthma",
        "eclampsia",
        "severe anaemia",
    ),
    "symptom": (
        "fever",
        "cough",
        "headache",
        "vomiting",
        "diarrhoea",
        "jaundice",
        "chest pain",
        "seizures",
        "abdominal pain",
        "bleeding",
        "rash",
        "neck stiffness",
        "joint pain",
        "swelling",
    ),
    "hospital": (
        "LUTH",
        "UCH Ibadan",
        "ABUTH Zaria",
        "FMC Owerri",
        "a PHC",
        "the general hospital",
        "UNTH Enugu",
    ),
    "org": ("WHO", "NCDC", "FMOH", "NICE"),
    "eponym": (
        "Burkitt lymphoma",
        "Kaposi sarcoma",
        "Stevens-Johnson syndrome",
        "Wilson disease",
        "Bell's palsy",
        "Guillain-Barre syndrome",
        "Hodgkin lymphoma",
        "Graves disease",
        "Addison disease",
        "Down syndrome",
    ),
    "weekday": ("Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"),
    "month": ("January", "March", "May", "June", "August", "October", "December"),
    # Brand names and towns the word list doesn't hold: the hardest case for
    # the name score (capitalised, unknown, Nigerian-sounding towns).
    "brand": (
        "Ciprotab",
        "Lumartem",
        "Artequin",
        "Maldox",
        "Lasix",
        "Aldomet",
        "Ponstan",
        "Felvin",
        "Cafenol",
        "Boska",
        "Procold",
        "Tutolin",
        "Loxagyl",
        "Gestid",
        "Gsunate",
        "Emzolyn",
        "Funbact",
        "Chloroquine",
        "Amalar",
        "Dynewell",
        "Ferrobin",
        "Astymin",
        "Parafen",
    ),
    "town": (
        "Ikeja",
        "Surulere",
        "Nsukka",
        "Aba",
        "Ogbomoso",
        "Shagamu",
        "Ile-Ife",
        "Ijebu-Ode",
        "Mushin",
        "Ajegunle",
        "Badagry",
        "Epe",
        "Ikorodu",
        "Oshodi",
        "Agege",
        "Lekki",
        "Yaba",
        "Gwagwalada",
        "Kubwa",
        "Nyanya",
        "Bwari",
        "Dutse",
        "Potiskum",
        "Funtua",
        "Bida",
        "Okene",
        "Auchi",
        "Ekpoma",
        "Ughelli",
        "Sapele",
        "Yenagoa",
        "Bonny",
        "Ikot Ekpene",
        "Eket",
        "Ogoja",
        "Gboko",
        "Otukpo",
        "Idah",
        "Lafia",
        "Keffi",
        "Wukari",
        "Mubi",
        "Biu",
        "Azare",
        "Kontagora",
        "Nnewi",
        "Awka",
        "Umuahia",
        "Afikpo",
        "Abakaliki",
        "Orlu",
        "Okigwe",
        "Iseyin",
        "Saki",
        "Oyo",
    ),
}
GUIDELINES: Final = (
    "Malaria requires parasitological confirmation by microscopy or RDT before treatment.",
    "Uncomplicated malaria: artemether-lumefantrine twice daily for 3 days.",
    "Severe malaria: IV artesunate 2.4 mg/kg at 0, 12 and 24 hours, then daily.",
    "There is a grace period of 7 days for missed ART refills before tracing.",
    "Give hope and reassurance; most febrile seizures are benign.",
    "Peace of mind for the family improves adherence to follow-up.",
    "Patience is needed: jaundice may take 2 weeks to resolve.",
    "Mark the date of the last dose on the card.",
    "Favourable outcomes follow early oxytocin after delivery.",
    "Joy and relief are common after a negative test; counsel anyway.",
    "Mercy, compassion and respect are core values in palliative care.",
    "Faith leaders can support community TB case finding.",
    "Blessing ceremonies should not delay neonatal BCG and polio vaccines.",
    "Comfort measures: paracetamol 15 mg/kg every 6 hours.",
    "Clinic days are Monday to Friday; on Sunday refer to emergency.",
    "Hypertension: systolic >= 140 or diastolic >= 90 on two occasions.",
    "Pre-eclampsia: magnesium sulphate 4 g IV loading dose.",
    "Sickle cell crisis: hydration, analgesia and oxygen if SpO2 < 95%.",
    "Typhoid: ciprofloxacin or ceftriaxone according to local resistance.",
    "Lassa fever: ribavirin early; isolate and notify NCDC.",
    "Neonatal jaundice: phototherapy thresholds by age in hours.",
    "TB: 2 months HRZE then 4 months HR.",
    "HIV: start dolutegravir-based ART regardless of CD4 count.",
    "Diabetes: metformin 500 mg twice daily, titrate to 1 g twice daily.",
    "Cholera: ORS for mild dehydration; Ringer's lactate if severe.",
    "Measles: vitamin A on days 1 and 2.",
    "Burkitt lymphoma presents with jaw or abdominal masses in children.",
    "Gift of blood: transfuse if Hb < 5 g/dL or signs of decompensation.",
    "Victory over malaria needs bed nets and prompt treatment.",
    "Precious minutes matter in postpartum haemorrhage.",
    "Falsified Lumartem and Maldox batches were reported in Aba and Onitsha markets.",
    "Refer from Nsukka, Ikeja or Bida health centres to the nearest teaching hospital.",
)


def clean_questions(rng: random.Random, n: int = N_CLEAN_QUESTIONS) -> list[str]:
    nums: dict[str, Callable[[], str]] = {
        "age": lambda: str(rng.randint(1, 85)),
        "d": lambda: str(rng.randint(1, 14)),
        "kg": lambda: str(rng.randint(3, 40)),
        "w": lambda: str(rng.randint(6, 40)),
        "sys": lambda: str(rng.randint(90, 190)),
        "dia": lambda: str(rng.randint(50, 120)),
        "hr": lambda: str(rng.randint(50, 150)),
        "temp": lambda: f"{rng.uniform(36, 41):.1f}",
        "rr": lambda: str(rng.randint(12, 40)),
        "sat": lambda: str(rng.randint(80, 100)),
        "hb": lambda: f"{rng.uniform(4, 15):.1f}",
        "wbc": lambda: f"{rng.uniform(2, 25):.1f}",
        "plt": lambda: str(rng.randint(20, 450)),
        "dose": lambda: str(rng.choice([250, 500, 1000, 5, 10, 20, 40])),
        "glu": lambda: f"{rng.uniform(3, 30):.1f}",
        "a1c": lambda: f"{rng.uniform(5, 14):.1f}",
    }
    out = []
    for _ in range(n):
        template = rng.choice(CLEAN_TEMPLATES)

        def sub(m: re.Match[str]) -> str:
            key = m.group(1).rstrip("2")
            return nums[key]() if key in nums else rng.choice(VOCAB[key])

        out.append(re.sub(r"\{(\w+)\}", sub, template))
    return out


def dev_items(path: Path) -> list[tuple[str, list[str]]]:
    """The clinical answer dev set (testdata/answers/dev/items.jsonl): each
    question with its own guideline chunks."""
    items = []
    for line in path.read_text("utf-8").splitlines():
        if line.strip():
            item = json.loads(line)
            items.append((item["question"], [c["text"] for c in item["chunks"]]))
    return items


# --- the egress path ---------------------------------------------------------------------

_FIXED: Final = {"/model": "masker-eval", "/max_tokens": 1, "/messages/0/role": "user"}
CONTEXT: Final = "Category: none\nRisk: none"


def egress(question: str, history: str, chunks: Sequence[str]) -> tuple[str | None, str]:
    """The cloud path as run_query and the client run it, without a network:
    this patient's identifiers from the raw question and record, the masked
    question as block 0, the context, the chunks; then the scan. Returns the
    scan's rule (None: it would be sent) and the masked question."""
    from intelligence.boundary.cloud.scan import collect_identifiers, scan_body, shape_for
    from intelligence.security.hipaa import PIIMasker

    patient: dict[str, Any] = {"age": 34, "sex": "female", "symptoms": ["fever"]}
    if history:
        patient["medical_history"] = history
    ids = collect_identifiers(question, patient)
    masked, _ = PIIMasker().mask(question)
    texts = [masked, CONTEXT] + [
        f"<<chunk KB#{9100 + i:04d}>>\n{c}\n<<end>>" for i, c in enumerate(chunks[:5])
    ]
    body = {
        "model": "masker-eval",
        "max_tokens": 1,
        "messages": [{"role": "user", "content": [{"type": "text", "text": t} for t in texts]}],
    }
    raw = json.dumps(body).encode("utf-8")
    return scan_body(raw, ids, shape_for("anthropic", _FIXED)), masked


# --- the score's threshold ------------------------------------------------------------------


def calibrate(
    folds: int = 5,
    seed: int = SEED,
    thresholds: Sequence[float] = (0.0, 0.5, 1.0, 1.5, 2.0, 2.5, 3.0),
) -> list[dict[str, float]]:
    """Cross-validate NameScore on the two files only (never the evaluation
    set): train on all but one fold of the gazetteer's names and of the
    known words, score the fold left out. For each threshold: the share of
    held-out names above it (recall) and of held-out known words above it
    (the false-positive rate on words the detector has never seen)."""
    gaz = ng_pii.gazetteer()
    rng = random.Random(seed)
    names, known = sorted(gaz.names), sorted(gaz.known)
    rng.shuffle(names)
    rng.shuffle(known)
    name_scores: list[float] = []
    known_scores: list[float] = []
    for f in range(folds):
        held_n, held_k = names[f::folds], known[f::folds]
        train_n = [w for i, w in enumerate(names) if i % folds != f]
        train_k = [w for i, w in enumerate(known) if i % folds != f]
        scorer = ng_pii.NameScore(train_n, train_k)
        name_scores += [scorer.score(w) for w in held_n if len(w) >= ng_pii.MIN_SCORED_LETTERS]
        known_scores += [scorer.score(w) for w in held_k if len(w) >= ng_pii.MIN_SCORED_LETTERS]
    return [
        {
            "threshold": t,
            "name_recall": round(sum(s >= t for s in name_scores) / len(name_scores), 4),
            "known_false_positive": round(sum(s >= t for s in known_scores) / len(known_scores), 4),
        }
        for t in thresholds
    ]


# --- measurement ---------------------------------------------------------------------------


def _covered(spans: Iterable[tuple[int, int]], text: str, target: tuple[int, int]) -> bool:
    cover = [False] * len(text)
    for a, b in spans:
        for i in range(max(a, 0), min(b, len(text))):
            cover[i] = True
    return all(cover[i] for i in range(*target) if text[i].isalnum())


def _found(masker: Any, text: str, target: tuple[int, int]) -> bool:
    _, detections = masker.mask(text)
    return _covered(((d.start, d.end) for d in detections), text, target)


def _gone(sent: str, original: str) -> bool:
    """The identifier's letters and digits no longer appear in what is sent."""

    def key(s: str) -> str:
        return "".join(c for c in ng_pii.fold(s) if c.isalnum())

    return key(original) not in key(sent)


@dataclass
class Tally:
    n: int = 0
    found: int = 0
    misses: list[str] = field(default_factory=list)

    def add(self, ok: bool, example: str) -> None:
        self.n += 1
        self.found += ok
        if not ok and len(self.misses) < 10:
            self.misses.append(example)

    @property
    def rate(self) -> float:
        return self.found / self.n if self.n else 0.0

    def row(self) -> dict[str, Any]:
        return {"n": self.n, "found": self.found, "recall": round(self.rate, 4)}


def _seen(name: str) -> bool:
    gaz = ng_pii.gazetteer()
    words = [ng_pii.fold(w) for w in re.findall(r"[^\W\d_]{2,}", name)]
    return all(w in gaz.names or w in gaz.words for w in words)


def evaluate(dev_path: Path | None = None, seed: int = SEED) -> dict[str, Any]:
    """Every measurement, as a JSON-ready dict (see the module docstring)."""
    from intelligence.security.hipaa import PIIMasker

    rng = random.Random(seed)
    masker = PIIMasker()
    recall: dict[str, Tally] = defaultdict(Tally)
    egress_recall: dict[str, Tally] = defaultdict(Tally)
    by_tag: dict[str, Tally] = defaultdict(Tally)
    named_refused = Tally()
    refused_named_by_rule: dict[str, int] = defaultdict(int)
    stable = Tally()  # the masker finds nothing in its own output

    samples = name_samples(rng) + number_samples(rng)
    for s in samples:
        text = s.history or s.text
        for span in s.spans:
            original = text[span[0] : span[1]]
            ok = _found(masker, text, span)
            recall[s.cls].add(ok, text)
            if s.cls == "NAME":
                by_tag["seen" if _seen(original) else "held_out"].add(ok, text)
                for tag in s.tags:
                    by_tag[tag].add(ok, text)
        chunks = rng.sample(GUIDELINES, 3)
        rule, masked = egress(s.text, s.history, chunks)
        sent = masked if not s.history else masked + "\n" + CONTEXT
        for span in s.spans:
            original = text[span[0] : span[1]]
            egress_recall[s.cls].add(rule is not None or _gone(sent, original), text)
        if s.cls == "NAME":
            named_refused.add(rule is None, s.text)
            if rule is not None:
                refused_named_by_rule[rule] += 1
        stable.add(not masker.mask(masked)[1], s.text)

    clean = [(q, rng.sample(GUIDELINES, 4)) for q in clean_questions(rng)]
    if dev_path is not None:
        clean += dev_items(dev_path)
    refused: dict[str, int] = defaultdict(int)
    false_refusals = Tally()
    over_masked = Tally()
    for q, chunks in clean:
        rule, masked = egress(q, "", chunks)
        false_refusals.add(rule is None, q)
        over_masked.add(masked == q, q)
        stable.add(not masker.mask(masked)[1], q)
        if rule is not None:
            refused[rule] += 1

    numeric = Tally()
    egress_numeric = Tally()
    for cls in ("NIN", "BVN", "PHONE", "MRN"):
        numeric.n += recall[cls].n
        numeric.found += recall[cls].found
        egress_numeric.n += egress_recall[cls].n
        egress_numeric.found += egress_recall[cls].found
    return {
        "seed": seed,
        "recall": {k: v.row() for k, v in sorted(recall.items())} | {"NUMERIC": numeric.row()},
        "egress_recall": {k: v.row() for k, v in sorted(egress_recall.items())}
        | {"NUMERIC": egress_numeric.row()},
        "names_by": {k: v.row() for k, v in sorted(by_tag.items())},
        "clean_questions": len(clean),
        "false_refusal_rate": round(1 - false_refusals.rate, 4),
        "false_refusals": false_refusals.n - false_refusals.found,
        "false_refusals_by_rule": dict(sorted(refused.items())),
        "over_masking_rate": round(1 - over_masked.rate, 4),
        "named_question_refusal_rate": round(1 - named_refused.rate, 4),
        "named_refusals_by_rule": dict(sorted(refused_named_by_rule.items())),
        "remask_detections": stable.n - stable.found,
        "examples": {
            "missed_names": recall["NAME"].misses,
            "missed_numbers": [m for c in ("NIN", "BVN", "PHONE", "MRN") for m in recall[c].misses],
            "over_masked": over_masked.misses,
            "false_refusals": false_refusals.misses,
            "named_refusals": named_refused.misses,
            "remask": stable.misses,
        },
    }
