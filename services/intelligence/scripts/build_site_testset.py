"""Build the pilot site's private test set from its de-identified visit data,
ON THE LIVE SERVER. The data never leaves the server, and this script prints
counts only, never a row.

Python standard library only, so it runs in a throwaway container with no
network, the data folder mounted read-only (deploy/README.md, "The pilot
site's private test set"):

    docker run --rm --network none --read-only --cap-drop ALL \\
      -v /srv/mhai-data/mhai/visits:/data:ro \\
      -v /srv/mhai-data/mhai/testset:/out \\
      -v /root/build_site_testset.py:/build_site_testset.py:ro \\
      python:3.12-slim python -I /build_site_testset.py --data /data --out /out

Inputs (both optional; at least one must exist):

- ``Data.xlsx``: one visit per row, the columns AGE, SEX, SYMPTOMS/COMPLAINT,
  LABORATORY, DIAGNOSIS, TREATMENT (matched by name, any case).
- ``malaria_cleaned.csv``: structured flags. Columns are recognised by name:
  age, sex or gender, a diagnosis or result column, and yes/no flag columns
  (a flag that is set becomes a complaint). A column whose name says it may
  identify someone (name, phone, address, occupation, date, id, number,
  ward, town ...) is never read.

What it does:

1. Drops exact duplicate rows.
2. Drops a row whose text holds a quasi-identifier: an occupation word, a
   title (Mr, Mrs, Alhaji ...), a date, a phone number, an e-mail, a run of
   five or more digits, a hospital or card number. Counted by reason.
3. Writes each remaining row as a vignette question: age band, sex,
   complaints, test results, working diagnosis, then "What is the
   recommended management?". The site's own treatment is kept beside it
   for the reviewing clinician, and ``expected_facts`` is left empty for the
   clinician to fill.
4. Drops repeated vignettes, orders the rest by a hash of their text (not
   the visit order) and numbers them ``site-0001`` up.

Output: ``<out>/site-testset-<YYYYMMDD>/`` with ``questions.csv``,
``counts.json`` and ``README.txt``, the folder mode 0700 and the files 0600
(root only when run as root, as above). It refuses to overwrite an
existing folder.
"""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import hashlib
import io
import json
import os
import re
import sys
import zipfile
from collections import Counter
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from xml.etree import ElementTree

XLSX_NAME = "Data.xlsx"
CSV_NAME = "malaria_cleaned.csv"
MAX_INPUT_BYTES = 64 * 1024 * 1024
MAX_ROWS = 100_000
MAX_FIELD_CHARS = 2000
QUESTION = "What is the recommended management?"
OUT_COLUMNS = (
    "id",
    "source",
    "age_band",
    "sex",
    "complaints",
    "investigations",
    "working_diagnosis",
    "question",
    "site_treatment",
    "expected_facts",
    "must_not_say",
    "clinician_notes",
    "reviewed_by",
)

# The visit sheet's columns, by their normalised header.
XLSX_COLUMNS = {
    "age": "age",
    "sex": "sex",
    "symptoms/complaint": "complaints",
    "symptoms/complaints": "complaints",
    "symptoms": "complaints",
    "complaint": "complaints",
    "complaints": "complaints",
    "laboratory": "investigations",
    "lab": "investigations",
    "diagnosis": "diagnosis",
    "treatment": "treatment",
}

# A column whose name holds one of these is never read (a structured file).
IDENTIFYING_COLUMN = re.compile(
    r"name|phone|mobile|tel|address|street|town|city|village|lga|state|occupation|"
    r"job|profession|employ|religion|tribe|ethnic|date|dob|birth|time|\bid\b|_id|id_|"
    r"number|\bno\b|card|folder|hospital|ward|bed|email|next.of.kin|kin|nok|guardian|"
    r"spouse|husband|wife|mother|father|marital|nin|bvn",
    re.IGNORECASE,
)
DIAGNOSIS_COLUMN = re.compile(r"diagnos|result|outcome|malaria", re.IGNORECASE)
# Medicine columns of the flags file (one column per drug class, holding the
# drug's name): reviewer context only, like the visit sheet's TREATMENT.
MEDICINE_COLUMN = re.compile(
    r"antimalar|analges|antihist|antibio|cough|vitamin|supplement|drug|medic|treat",
    re.IGNORECASE,
)
# A placeholder value that says nothing on its own.
VAGUE_VALUES = frozenset({"others", "other"})
AGE_COLUMN = re.compile(r"^age", re.IGNORECASE)
SEX_COLUMN = re.compile(r"^(sex|gender)$", re.IGNORECASE)
YES = frozenset({"1", "yes", "y", "true", "t", "positive", "pos", "+", "present"})
NO = frozenset({"0", "no", "n", "false", "f", "negative", "neg", "-", "absent", ""})

# Quasi-identifiers in free text: a row holding one is left out.
OCCUPATIONS = (
    "farmer|trader|teacher|student|driver|nurse|doctor|lawyer|banker|tailor|"
    "seamstress|mechanic|carpenter|bricklayer|mason|welder|electrician|plumber|"
    "artisan|civil servant|public servant|pastor|imam|clergy|reverend|soldier|"
    "police|policeman|army|navy|okada|keke|cleaner|security man|guard|housewife|"
    "house wife|businessman|businesswoman|business man|business woman|retiree|"
    "retired|unemployed|applicant|hairdresser|barber|chef|fisherman|hunter|"
    "herdsman|vulcanizer|vulcaniser|engineer|accountant|journalist|pharmacist|"
    "lecturer|corper|nysc|politician|councillor|chairman"
    # not "pupil" (the eye) or "guarding" (the abdomen): clinical words
)
QUASI_IDENTIFIERS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("occupation", re.compile(rf"\b(?:{OCCUPATIONS})s?\b", re.IGNORECASE)),
    (
        "title_or_name",
        re.compile(
            r"\b(?:Mr|Mrs|Miss|Ms|Master|Alhaji|Alhaja|Hajia|Pastor|Evang|Prof|Engr|"
            r"Barr|Hon|Baba|Mama|Iya)\b\.?\s+[A-Z][a-z]"
            r"|\b(?:MR|MRS|MISS|MS|MASTER|ALHAJI|ALHAJA|HAJIA|PASTOR|PROF|ENGR|BARR|HON)"
            r"\b\.?\s+[A-Z]{2,}"
        ),
    ),
    ("date", re.compile(r"\b\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}\b|\b(?:19|20)\d{2}\b")),
    ("phone", re.compile(r"(?:\+?234|\b0)[789][01]\d{8}\b|\b\d{4}[\s-]\d{3}[\s-]\d{4}\b")),
    ("email", re.compile(r"[\w.+-]+@[\w-]+\.[\w.]+")),
    ("long_number", re.compile(r"\d{5,}")),
    (
        "record_number",
        re.compile(r"\b(?:hosp|hospital|card|folder|reg|mrn|file)\s*(?:no|number|#)", re.I),
    ),
)

_AGE = re.compile(
    r"^\s*(\d{1,3}(?:\.\d+)?)\s*(y|yr|yrs|year|years|m|mo|mos|month|months|mths|w|wk|wks|"
    r"week|weeks|d|day|days)?\.?\s*(?:old)?\s*$",
    re.IGNORECASE,
)
_AGE_FRACTION = re.compile(r"^\s*(\d{1,2})\s*/\s*(12|52)\s*$")


class SiteTestsetError(Exception):
    """A refusal; the text is a reason code, never row content."""


@dataclass(frozen=True)
class Visit:
    source: str
    age_band: str
    sex: str
    complaints: str
    investigations: str
    diagnosis: str
    treatment: str


# --- reading --------------------------------------------------------------------------------

_NS = {"m": "http://schemas.openxmlformats.org/spreadsheetml/2006/main"}
_REL = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}id"


def _xml(z: zipfile.ZipFile, name: str) -> ElementTree.Element:
    info = z.getinfo(name)
    if info.file_size > MAX_INPUT_BYTES:
        raise SiteTestsetError("xlsx_too_large")
    data = z.read(name)
    # Standard library only (the script runs in a bare python image): the
    # stdlib parser resolves no external entity, and a document type (the
    # only way to declare an entity, so a "billion laughs") is refused.
    if b"<!DOCTYPE" in data.upper() or b"<!ENTITY" in data.upper():
        raise SiteTestsetError("xlsx_doctype")
    return ElementTree.fromstring(data)


def _col(ref: str) -> int:
    n = 0
    for c in ref:
        if not c.isalpha():
            break
        n = n * 26 + (ord(c.upper()) - 64)
    return n - 1


def read_xlsx(data: bytes) -> list[list[str]]:
    """The first worksheet's rows as text (standard library only: the
    workbook's shared strings and the sheet's XML)."""
    try:
        z = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile:
        raise SiteTestsetError("not_xlsx") from None
    with z:
        names = set(z.namelist())
        shared: list[str] = []
        if "xl/sharedStrings.xml" in names:
            for si in _xml(z, "xl/sharedStrings.xml").findall("m:si", _NS):
                shared.append("".join(t.text or "" for t in si.iter(f"{{{_NS['m']}}}t")))
        sheet = "xl/worksheets/sheet1.xml"
        if sheet not in names:
            sheets = sorted(n for n in names if re.fullmatch(r"xl/worksheets/sheet\d+\.xml", n))
            if not sheets:
                raise SiteTestsetError("no_sheet")
            sheet = sheets[0]
        rows: list[list[str]] = []
        for row in _xml(z, sheet).iter(f"{{{_NS['m']}}}row"):
            cells: dict[int, str] = {}
            for i, c in enumerate(row.findall("m:c", _NS)):
                ref = c.get("r") or ""
                col = _col(ref) if ref else i
                kind = c.get("t")
                v = c.find("m:v", _NS)
                if kind == "s" and v is not None and v.text is not None:
                    idx = int(v.text)
                    text = shared[idx] if 0 <= idx < len(shared) else ""
                elif kind == "inlineStr":
                    text = "".join(t.text or "" for t in c.iter(f"{{{_NS['m']}}}t"))
                else:
                    text = v.text if v is not None and v.text is not None else ""
                cells[col] = text
            if cells:
                width = max(cells) + 1
                rows.append([cells.get(k, "") for k in range(width)])
            if len(rows) > MAX_ROWS:
                raise SiteTestsetError("too_many_rows")
    return rows


def _clean(text: str) -> str:
    out = "".join(" " if ord(c) < 32 or 0x7F <= ord(c) < 0xA0 else c for c in str(text))
    return " ".join(out.split())[:MAX_FIELD_CHARS]


def _norm_header(h: str) -> str:
    return re.sub(r"\s+", "", _clean(h).lower())


def age_band(text: str) -> str:
    """An age band, never the exact age: under 1, 1-4, 5-14, then ten-year
    bands to 80-89, then 90+; "unknown" if it can't be read."""
    t = _clean(text).lower()
    years: float | None = None
    m = _AGE_FRACTION.fullmatch(t)
    if m:
        years = int(m.group(1)) / int(m.group(2))
    else:
        m = _AGE.fullmatch(t)
        if m:
            n = float(m.group(1))
            unit = (m.group(2) or "y").lower()
            if unit.startswith("m"):
                years = n / 12
            elif unit.startswith("w"):
                years = n / 52
            elif unit.startswith("d"):
                years = n / 365
            else:
                years = n
    if years is None or years < 0 or years > 120:
        return "unknown"
    if years < 1:
        return "under 1 year"
    if years < 5:
        return "1-4 years"
    if years < 15:
        return "5-14 years"
    if years >= 90:
        return "90+ years"
    if years >= 85:
        return "85-89 years"
    low = 15 + int((years - 15) // 10) * 10
    return f"{low}-{low + 9} years"


def sex(text: str) -> str:
    t = _clean(text).lower()
    if t in ("m", "male", "man", "boy"):
        return "male"
    if t in ("f", "female", "woman", "girl"):
        return "female"
    return "unknown"


def visits_from_xlsx(rows: Sequence[Sequence[str]], counts: Counter[str]) -> list[Visit]:
    if not rows:
        return []
    header = [_norm_header(h) for h in rows[0]]
    where: dict[str, int] = {}
    for i, h in enumerate(header):
        field = XLSX_COLUMNS.get(h)
        if field and field not in where:
            where[field] = i
    if not {"complaints", "diagnosis"} <= set(where):
        raise SiteTestsetError("xlsx_columns_not_found")
    out = []
    for r in rows[1:]:

        def get(field: str, r: Sequence[str] = r) -> str:
            i = where.get(field)
            return _clean(r[i]) if i is not None and i < len(r) else ""

        if not any(_clean(c) for c in r):
            continue
        counts["xlsx_rows"] += 1
        out.append(
            Visit(
                source="visits",
                age_band=age_band(get("age")),
                sex=sex(get("sex")),
                complaints=get("complaints"),
                investigations=get("investigations"),
                diagnosis=get("diagnosis"),
                treatment=get("treatment"),
            )
        )
    return out


def _human(column: str) -> str:
    return " ".join(re.sub(r"[_\-.]+", " ", column).split()).lower()


def visits_from_csv(text: str, counts: Counter[str]) -> tuple[list[Visit], dict[str, object]]:
    """Visits from the structured flags file, and the column mapping
    (column names only)."""
    reader = csv.reader(io.StringIO(text, newline=""))
    rows = list(reader)
    if len(rows) > MAX_ROWS:
        raise SiteTestsetError("too_many_rows")
    if not rows:
        return [], {}
    header = [_clean(h) for h in rows[0]]
    skipped = [h for h in header if IDENTIFYING_COLUMN.search(h) and not AGE_COLUMN.match(h)]
    usable = [i for i, h in enumerate(header) if h not in skipped and h]
    age_i = next((i for i in usable if AGE_COLUMN.match(header[i])), None)
    sex_i = next((i for i in usable if SEX_COLUMN.match(header[i])), None)
    diag_i = next(
        (i for i in usable if DIAGNOSIS_COLUMN.search(header[i]) and i not in (age_i, sex_i)),
        None,
    )
    body = rows[1:]
    flags: list[int] = []
    diagnoses: list[int] = []
    medicines: list[int] = []
    for i in usable:
        if i in (age_i, sex_i, diag_i):
            continue
        values = {_clean(r[i]).lower() for r in body if i < len(r)}
        named = values - NO
        if values and values <= (YES | NO):
            flags.append(i)
        elif len(named) == 1 and _human(next(iter(named))) == _human(header[i]):
            # A symptom marked by writing the column's own name ("fever" in
            # the fever column), empty otherwise.
            flags.append(i)
        elif MEDICINE_COLUMN.search(header[i]):
            # Before diagnoses: "antimalarial" contains "malaria".
            medicines.append(i)
        elif DIAGNOSIS_COLUMN.search(header[i]):
            diagnoses.append(i)
    mapping: dict[str, object] = {
        "age": header[age_i] if age_i is not None else None,
        "sex": header[sex_i] if sex_i is not None else None,
        "diagnosis": header[diag_i] if diag_i is not None else None,
        "flags": [header[i] for i in flags],
        "more_diagnoses": [header[i] for i in diagnoses],
        "medicines": [header[i] for i in medicines],
        "not_read_identifying": skipped,
        "not_read_other": [
            header[i]
            for i in usable
            if i not in flags
            and i not in diagnoses
            and i not in medicines
            and i not in (age_i, sex_i, diag_i)
        ],
    }
    out = []
    for r in body:
        if not any(_clean(c) for c in r):
            continue
        counts["csv_rows"] += 1

        def cell(i: int | None, r: Sequence[str] = r) -> str:
            return _clean(r[i]) if i is not None and i < len(r) else ""

        present = [_human(header[i]) for i in flags if cell(i).lower() not in NO]
        diagnosis = cell(diag_i)
        if diag_i is not None and diagnosis.lower() in YES | NO - {""}:
            word = _human(header[diag_i])
            diagnosis = f"{word}: {'positive' if diagnosis.lower() in YES else 'negative'}"
        more = [
            _human(cell(i))
            for i in diagnoses
            if cell(i) and cell(i).lower() not in NO | VAGUE_VALUES
        ]
        diagnosis = ", ".join(x for x in [_human(diagnosis) if diagnosis else "", *more] if x)
        treatment = ", ".join(
            _human(cell(i))
            for i in medicines
            if cell(i) and cell(i).lower() not in NO | VAGUE_VALUES
        )
        out.append(
            Visit(
                source="flags",
                age_band=age_band(cell(age_i)),
                sex=sex(cell(sex_i)),
                complaints=", ".join(present),
                investigations="",
                diagnosis=diagnosis,
                treatment=treatment,
            )
        )
    return out, mapping


# --- filtering and writing ------------------------------------------------------------------


def quasi_identifier(v: Visit) -> str:
    """The first kind of quasi-identifier in a visit's text, or ""."""
    text = " | ".join((v.complaints, v.investigations, v.diagnosis, v.treatment))
    for kind, pattern in QUASI_IDENTIFIERS:
        if pattern.search(text):
            return kind
    return ""


def vignette(v: Visit) -> str:
    who = "A patient" if v.sex == "unknown" else f"A {v.sex} patient"
    if v.age_band != "unknown":
        who += f" aged {v.age_band}"
    parts = [f"{who} presents with {v.complaints.rstrip('.') or 'no recorded complaint'}."]
    if v.investigations:
        parts.append(f"Investigations: {v.investigations.rstrip('.')}.")
    parts.append(f"Working diagnosis: {v.diagnosis.rstrip('.') or 'not recorded'}.")
    parts.append(QUESTION)
    return " ".join(parts)


def select(visits: Iterable[Visit], counts: Counter[str]) -> list[tuple[Visit, str]]:
    """The visits kept and their vignettes: exact duplicates once, rows with a
    quasi-identifier or nothing to ask about left out, repeated vignettes
    once, ordered by a hash of the vignette."""
    seen: set[Visit] = set()
    seen_text: set[str] = set()
    kept: list[tuple[Visit, str]] = []
    for v in visits:
        if v in seen:
            counts["dropped_duplicate_row"] += 1
            continue
        seen.add(v)
        kind = quasi_identifier(v)
        if kind:
            counts[f"dropped_{kind}"] += 1
            continue
        if not v.complaints and not v.diagnosis:
            counts["dropped_empty"] += 1
            continue
        text = vignette(v)
        if text in seen_text:
            counts["dropped_duplicate_vignette"] += 1
            continue
        seen_text.add(text)
        kept.append((v, text))
    kept.sort(key=lambda p: hashlib.sha256(p[1].encode()).hexdigest())
    counts["questions"] = len(kept)
    return kept


def write(out_root: Path, kept: Sequence[tuple[Visit, str]], counts: Mapping[str, object]) -> Path:
    """Write the set folder (0700, files 0600); refuses an existing one."""
    old = os.umask(0o077)
    try:
        folder = out_root / f"site-testset-{dt.date.today():%Y%m%d}"
        try:
            folder.mkdir(mode=0o700, parents=False)
        except FileExistsError:
            raise SiteTestsetError("output_exists") from None
        except FileNotFoundError:
            raise SiteTestsetError("no_output_root") from None
        buf = io.StringIO(newline="")
        w = csv.writer(buf, lineterminator="\n")
        w.writerow(OUT_COLUMNS)
        for n, (v, text) in enumerate(kept, start=1):
            w.writerow(
                (
                    f"site-{n:04d}",
                    v.source,
                    v.age_band,
                    v.sex,
                    v.complaints,
                    v.investigations,
                    v.diagnosis,
                    text,
                    v.treatment,
                    "",
                    "",
                    "",
                    "",
                )
            )
        _write_private(folder / "questions.csv", buf.getvalue())
        _write_private(folder / "counts.json", json.dumps(counts, indent=1, sort_keys=True) + "\n")
        _write_private(folder / "README.txt", README)
        return folder
    finally:
        os.umask(old)


def _write_private(path: Path, text: str) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
        f.write(text)


README = """The pilot site's private test set. PATIENT-DERIVED: never copy it off this
server, never commit it, never paste it into a chat or a ticket.

questions.csv, one vignette per row (de-identified visits: age band, sex,
complaints, test results, working diagnosis). For each row a clinician:
- fills expected_facts: the short phrases a correct answer must contain,
  separated by " | " (for example: artemether-lumefantrine | 3 days | with food);
- may fill must_not_say: phrases a safe answer must not contain (" | ");
- may note anything in clinician_notes, and signs reviewed_by with initials;
- deletes a row that still identifies someone or is not a useful question.
site_treatment is what the site gave: a reference for the reviewer, not the
expected answer. counts.json holds counts only.
"""


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--data", type=Path, required=True, help="the folder holding the visit files")
    p.add_argument("--out", type=Path, required=True, help="the test set root (root only)")
    args = p.parse_args(argv)
    counts: Counter[str] = Counter()
    visits: list[Visit] = []
    mapping: dict[str, object] = {}
    try:
        found = 0
        xlsx = args.data / XLSX_NAME
        if xlsx.is_file():
            found += 1
            if xlsx.stat().st_size > MAX_INPUT_BYTES:
                raise SiteTestsetError("xlsx_too_large")
            visits += visits_from_xlsx(read_xlsx(xlsx.read_bytes()), counts)
        flags = args.data / CSV_NAME
        if flags.is_file():
            found += 1
            if flags.stat().st_size > MAX_INPUT_BYTES:
                raise SiteTestsetError("csv_too_large")
            more, mapping = visits_from_csv(flags.read_text(encoding="utf-8-sig"), counts)
            visits += more
        if not found:
            raise SiteTestsetError("no_input")
        kept = select(visits, counts)
        out: dict[str, object] = dict(sorted(counts.items()))
        out["flags_columns"] = mapping
        folder = write(args.out, kept, out)
    except (SiteTestsetError, OSError, ValueError, ElementTree.ParseError) as e:
        reason = str(e) if isinstance(e, SiteTestsetError) else type(e).__name__
        print(f"refused: {reason}", file=sys.stderr)
        return 1
    # Counts only: never a row.
    for k, v in sorted(counts.items()):
        print(f"{k}: {v}")
    if mapping:
        print(f"flags file columns used: {json.dumps(mapping, sort_keys=True)}")
    print(f"written: {folder} (questions.csv, counts.json, README.txt; mode 0600)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
