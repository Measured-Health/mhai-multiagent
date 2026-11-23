"""The pilot site's private test-set builder (scripts/build_site_testset.py).
SYNTHETIC fixtures only, made here: a workbook in the visit sheet's shape
and a flags file. The real files exist only on the live server and are
never read by a test.
"""

from __future__ import annotations

import importlib.util
import io
import json
import stat
import sys
import zipfile
from collections import Counter
from pathlib import Path
from xml.sax.saxutils import escape

import pytest

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "build_site_testset.py"
_spec = importlib.util.spec_from_file_location("build_site_testset", _SCRIPT)
assert _spec is not None and _spec.loader is not None
site = importlib.util.module_from_spec(_spec)
sys.modules["build_site_testset"] = site
_spec.loader.exec_module(site)

HEADER = ["AGE", "SEX", "SYMPTOMS/COMPLAINT", "LABORATORY", "DIAGNOSIS", "TREATMENT"]
ROWS = [
    ["34", "F", "Synthetic fever, headache", "mRDT positive", "Malaria", "Synthetic ACT"],
    ["34", "F", "Synthetic fever, headache", "mRDT positive", "Malaria", "Synthetic ACT"],
    ["6/12", "M", "Synthetic cough", "", "Synthetic URTI", "Synthetic syrup"],
    ["95", "M", "Synthetic weakness", "PCV 30%", "Synthetic anaemia", "Synthetic iron"],
    ["40", "M", "Synthetic fever, a trader", "mRDT positive", "Malaria", "Synthetic ACT"],
    ["25", "F", "Synthetic pain since 12/03/2025", "", "Synthetic PID", "Synthetic"],
    ["52", "F", "Synthetic cough, call 08031234567", "", "Synthetic", "Synthetic"],
    ["8", "M", "Synthetic fever", "Card no 12", "Malaria", "Synthetic"],
    ["30", "F", "Synthetic malaise, seen with Mrs Synthetica", "", "Malaria", "Synthetic"],
    ["", "", "", "", "", ""],
    ["19", "x", "", "", "", "Synthetic only treatment"],
    ["3 yrs", "male", "Pupils equal, abdominal guarding", "", "Synthetic", "Synthetic"],
]


def _xlsx(rows: list[list[str]], doctype: bool = False) -> bytes:
    strings: list[str] = []
    index: dict[str, int] = {}

    def sid(text: str) -> int:
        if text not in index:
            index[text] = len(strings)
            strings.append(text)
        return index[text]

    ns = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
    sheet_rows = []
    for r, row in enumerate(rows, start=1):
        cells = []
        for c, value in enumerate(row):
            if value == "":
                continue
            ref = f"{chr(65 + c)}{r}"
            if value.isdigit():
                cells.append(f'<c r="{ref}"><v>{value}</v></c>')
            else:
                cells.append(f'<c r="{ref}" t="s"><v>{sid(value)}</v></c>')
        sheet_rows.append(f'<row r="{r}">{"".join(cells)}</row>')
    sheet = f'<?xml version="1.0"?><worksheet {ns}><sheetData>{"".join(sheet_rows)}</sheetData></worksheet>'
    shared = "".join(f"<si><t>{escape(s)}</t></si>" for s in strings)
    sst = f'<?xml version="1.0"?><sst {ns}>{shared}</sst>'
    if doctype:
        sst = '<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "aaaa">]>' + sst[21:]
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("xl/worksheets/sheet1.xml", sheet)
        z.writestr("xl/sharedStrings.xml", sst)
    return buf.getvalue()


FLAGS = (
    "patient_name,Age,Sex,occupation,fever,headache,vomiting,rdt_result,visit_date\n"
    "Synthetic Person,30,F,synthetic,1,0,yes,positive,2026-01-01\n"
    "Synthetic Other,4,M,synthetic,0,1,no,negative,2026-01-02\n"
    "Synthetic Person,30,F,synthetic,1,0,yes,positive,2026-01-01\n"
)


def test_reads_the_visit_sheet() -> None:
    rows = site.read_xlsx(_xlsx([HEADER, *ROWS]))
    assert rows[0] == HEADER
    assert rows[1] == ROWS[0]
    assert rows[3][3] == ""  # an empty cell keeps its column


@pytest.mark.parametrize(
    ("text", "band"),
    [
        ("6/12", "under 1 year"),
        ("3 months", "under 1 year"),
        ("2wks", "under 1 year"),
        ("3 yrs", "1-4 years"),
        ("8", "5-14 years"),
        ("15", "15-24 years"),
        ("34", "25-34 years"),
        ("84", "75-84 years"),
        ("87", "85-89 years"),
        ("95", "90+ years"),
        ("", "unknown"),
        ("adult", "unknown"),
        ("200", "unknown"),
    ],
)
def test_ages_become_bands(text: str, band: str) -> None:
    assert site.age_band(text) == band


def test_duplicates_and_quasi_identifiers_are_dropped() -> None:
    counts: Counter[str] = Counter()
    visits = site.visits_from_xlsx(site.read_xlsx(_xlsx([HEADER, *ROWS])), counts)
    kept = site.select(visits, counts)
    assert counts["xlsx_rows"] == 11  # the empty row is skipped
    assert counts["dropped_duplicate_row"] == 1
    assert counts["dropped_occupation"] == 1
    assert counts["dropped_date"] == 1
    assert counts["dropped_phone"] == 1
    assert counts["dropped_record_number"] == 1
    assert counts["dropped_title_or_name"] == 1
    assert counts["dropped_empty"] == 1
    assert counts["questions"] == len(kept) == 4
    text = " ".join(q for _, q in kept)
    for gone in ("trader", "2025", "0803", "Card", "Mrs"):
        assert gone not in text
    # Clinical words that look like occupations are kept.
    assert any("abdominal guarding" in q for _, q in kept)


def test_a_vignette_has_bands_not_ages_and_asks_for_management() -> None:
    counts: Counter[str] = Counter()
    visits = site.visits_from_xlsx(site.read_xlsx(_xlsx([HEADER, *ROWS])), counts)
    texts = [q for _, q in site.select(visits, counts)]
    assert (
        "A female patient aged 25-34 years presents with Synthetic fever, headache. "
        "Investigations: mRDT positive. Working diagnosis: Malaria. "
        "What is the recommended management?"
    ) in texts
    assert any(t.startswith("A male patient aged 90+ years") for t in texts)
    assert all(t.endswith(site.QUESTION) for t in texts)
    assert not any(" 34 " in t or " 95 " in t for t in texts)


def test_the_flags_file_never_reads_identifying_columns() -> None:
    counts: Counter[str] = Counter()
    visits, mapping = site.visits_from_csv(FLAGS, counts)
    assert mapping["age"] == "Age" and mapping["sex"] == "Sex"
    assert mapping["diagnosis"] == "rdt_result"
    assert mapping["flags"] == ["fever", "headache", "vomiting"]
    assert set(mapping["not_read_identifying"]) == {"patient_name", "occupation", "visit_date"}
    kept = site.select(visits, counts)
    assert counts["dropped_duplicate_row"] == 1 and len(kept) == 2
    texts = sorted(q for _, q in kept)
    assert "Synthetic Person" not in " ".join(texts) and "2026" not in " ".join(texts)
    assert any(
        "aged 25-34 years presents with fever, vomiting. Working diagnosis: rdt result: positive"
        in t
        for t in texts
    )


# The flags file as some sites export it: a symptom or diagnosis is marked by
# writing its own name, medicines by their names. Synthetic values only.
NAMED_FLAGS = (
    "age,sex,fever,headache,cough_cold_sorethroat,diagnostics_1,diagnostics_2,"
    "diagnostics_others,antimalarial,antibiotics,others_medications\n"
    "30,F,fever,,,malaria,,,synthdrug-a,,others\n"
    "30,F,,headache,cough_cold_sorethroat,malaria,sepsis,others,synthdrug-b,synthdrug-c,\n"
    "41,M,fever,headache,,malaria,,,synthdrug-a,,\n"
)


def test_the_flags_file_reads_named_symptoms_diagnoses_and_medicines() -> None:
    counts: Counter[str] = Counter()
    visits, mapping = site.visits_from_csv(NAMED_FLAGS, counts)
    assert mapping["flags"] == ["fever", "headache", "cough_cold_sorethroat"]
    assert mapping["diagnosis"] == "diagnostics_1"
    assert mapping["more_diagnoses"] == ["diagnostics_2", "diagnostics_others"]
    assert mapping["medicines"] == ["antimalarial", "antibiotics", "others_medications"]
    assert mapping["not_read_other"] == []
    kept = site.select(visits, counts)
    # Three different visits: reading the named flags keeps them apart.
    assert len(kept) == 3 and counts["dropped_duplicate_row"] == 0
    by_text = " | ".join(q for _, q in kept)
    assert "presents with headache, cough cold sorethroat" in by_text
    assert "Working diagnosis: malaria, sepsis" in by_text
    assert "others" not in by_text
    v = next(v for v in visits if "sepsis" in v.diagnosis)
    assert v.treatment == "synthdrug b, synthdrug c"


def test_write_is_private_and_refuses_to_overwrite(tmp_path: Path) -> None:
    counts: Counter[str] = Counter()
    visits = site.visits_from_xlsx(site.read_xlsx(_xlsx([HEADER, *ROWS])), counts)
    kept = site.select(visits, counts)
    folder = site.write(tmp_path, kept, dict(counts))
    assert stat.S_IMODE(folder.stat().st_mode) == 0o700
    for name in ("questions.csv", "counts.json", "README.txt"):
        assert stat.S_IMODE((folder / name).stat().st_mode) == 0o600
    lines = (folder / "questions.csv").read_text().splitlines()
    assert lines[0].split(",") == list(site.OUT_COLUMNS)
    assert lines[1].startswith("site-0001,visits,")
    assert len(lines) == 1 + len(kept)
    assert json.loads((folder / "counts.json").read_text())["questions"] == len(kept)
    with pytest.raises(site.SiteTestsetError):
        site.write(tmp_path, kept, dict(counts))


def test_main_prints_counts_only(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    data = tmp_path / "data"
    data.mkdir()
    (data / "Data.xlsx").write_bytes(_xlsx([HEADER, *ROWS]))
    (data / "malaria_cleaned.csv").write_text(FLAGS)
    out = tmp_path / "out"
    out.mkdir()
    assert site.main(["--data", str(data), "--out", str(out)]) == 0
    printed = capsys.readouterr()
    for row in ROWS:
        for value in row:
            if len(value) > 3 and not value.isdigit():
                assert value not in printed.out + printed.err
    assert "questions: 6" in printed.out


def test_refusals(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    assert site.main(["--data", str(tmp_path), "--out", str(tmp_path)]) == 1
    assert "no_input" in capsys.readouterr().err
    with pytest.raises(site.SiteTestsetError) as info:
        site.read_xlsx(_xlsx([HEADER, *ROWS], doctype=True))
    assert str(info.value) == "xlsx_doctype"
    with pytest.raises(site.SiteTestsetError):
        site.read_xlsx(b"not a workbook")
    with pytest.raises(site.SiteTestsetError) as info:
        site.visits_from_xlsx([["A", "B"], ["1", "2"]], Counter())
    assert str(info.value) == "xlsx_columns_not_found"
