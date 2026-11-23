"""The local formulary library (intelligence/knowledge_base/local_formulary.py):
the committed CSV's counts, the row text, the refusals and the artefact. No
model: the embedder is a synthetic stand-in. The committed CSV holds drug
names only; the other inputs here are synthetic.
"""

from __future__ import annotations

from collections import Counter
from pathlib import Path

import pytest

from intelligence.knowledge_base import local_formulary as formulary
from intelligence.knowledge_base import reference_library as reflib
from tests.unit.test_reference_library_convert import EMBEDDER, fake_embed

FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "reflib" / "formulary.csv"
RETRIEVED = "2026-11-23"


def _entries(data: bytes) -> tuple[list[reflib.Entry], Counter[str]]:
    counts: Counter[str] = Counter()
    return formulary.entries(formulary.read_csv(data), RETRIEVED, counts), counts


def test_the_committed_csv_loads_high_and_medium_only() -> None:
    rows = formulary.read_csv(formulary.CSV_PATH.read_bytes())
    entries, counts = _entries(formulary.CSV_PATH.read_bytes())
    assert len(rows) == 48
    assert counts == Counter({"loaded": 34, "low": 14})
    assert len(entries) == 34
    assert {e.metadata["confidence"] for e in entries} == {"high", "medium"}
    by_brand = {e.metadata["brand"]: e for e in entries}
    lonart = by_brand["Lonart"].content
    assert lonart.startswith(
        "Lonart is a local brand or name used in Nigeria for artemether-lumefantrine 20/120 mg."
    )
    assert "artemisinin-based combination therapy (ACT)" in lonart
    assert "uncomplicated falciparum malaria" in lonart
    assert by_brand["Lonart"].metadata["generic"] == "artemether-lumefantrine"
    # The cautions in the CSV are kept as written.
    assert "not first-line for uncomplicated malaria" in by_brand["Maldox"].content
    assert "not recommended for falciparum malaria in Nigeria (resistance)" in (
        by_brand["Chloroquine"].content
    )
    # No low-confidence brand is a row.
    text = " ".join(e.content for e in entries)
    for low in ("Cofex", "Emzolyn", "Hostan", "Progunal", "Floranorm"):
        assert low not in text


def test_row_text_names_every_brand_and_the_generic() -> None:
    entries, counts = _entries(FIXTURE.read_bytes())
    assert counts == Counter({"loaded": 3, "low": 1})
    by_brand = {e.metadata["brand"]: e for e in entries}
    synthmal = by_brand["Synthmal / Synthmal forte"]
    assert synthmal.content.startswith("Synthmal (also Synthmal forte) is a local brand")
    assert synthmal.metadata["title"] == "Synthmal (synthadoxine)"
    same = by_brand["Synthquine"].content
    assert same.startswith("Synthquine (synthquine) is in the local formulary used in Nigeria.")
    for e in entries:
        assert set(e.metadata) <= reflib.ROW_KEYS
        assert reflib.row_refusal(e.content, e.metadata, formulary.NAME) == ""
        assert e.metadata["kind"] == "local_formulary"
        assert "notes" not in e.metadata and "synthetic note" not in e.content


@pytest.mark.parametrize(
    ("generic", "search"),
    [
        ("artemether-lumefantrine 20/120 mg", "artemether-lumefantrine"),
        ("aspirin (low dose)", "aspirin"),
        ("fluticasone furoate (nasal)", "fluticasone furoate"),
        ("vitamins B1 + B6 + B12", "vitamins B1 + B6 + B12"),
        ("paracetamol + caffeine", "paracetamol + caffeine"),
        ("iron (ferric polymaltose)", "iron"),
    ],
)
def test_the_generic_searched_for_has_no_dose(generic: str, search: str) -> None:
    assert formulary.search_generic(generic) == search


@pytest.mark.parametrize(
    ("csv", "code"),
    [
        (b"brand,generic\nX,y\n", "bad_header"),
        (FIXTURE.read_bytes().replace(b",high,", b",certain,", 1), "bad_confidence"),
        (FIXTURE.read_bytes() + b"Synthx,unknown,c,u,high,\n", "unconfirmed_generic"),
        (FIXTURE.read_bytes() + b"Synthx,y KB#1000001,c,u,high,\n", "bad_field"),
        (FIXTURE.read_bytes() + b"Synthx,y,c,u <<x>>,high,\n", "bad_field"),
        (
            FIXTURE.read_bytes() + b"Synthx,y,c,Sample reference (synthetic demo),high,\n",
            "bad_field",
        ),
        (FIXTURE.read_bytes() + b"Synthx,y,c,high,\n", "bad_row"),
        (FIXTURE.read_bytes() + b"Synthx,,c,u,high,\n", "bad_row"),
        (b"\xff\xfe", "not_utf8"),
    ],
)
def test_a_bad_csv_is_refused(csv: bytes, code: str) -> None:
    with pytest.raises(formulary.FormularyError) as info:
        _entries(csv)
    assert str(info.value) == code


def test_a_repeated_brand_is_kept_once() -> None:
    entries, counts = _entries(FIXTURE.read_bytes() + b"SYNTHLONART,other,c,u,high,\n")
    assert counts["duplicate"] == 1 and len(entries) == 3


def test_the_artefact_is_the_formulary_block_and_deterministic(tmp_path: Path) -> None:
    def make(out: Path) -> tuple[Path, str]:
        header, rows = formulary.build(
            FIXTURE, fake_embed, version="1.0.0", embedder=EMBEDDER, retrieved=RETRIEVED
        )
        out.mkdir()
        return reflib.write_artefact(header, rows, out)

    p1, sha1 = make(tmp_path / "a")
    p2, sha2 = make(tmp_path / "b")
    assert p1.name == "local-formulary-1.0.0.jsonl.gz" and sha1 == sha2
    assert p1.read_bytes() == p2.read_bytes()
    pins = {formulary.NAME: reflib.Pin("1.0.0", sha1, 3)}
    header, _, data = reflib.read_pinned(p1, pins)
    assert header.name == "local-formulary" and header.licence == formulary.LICENCE
    rows = list(reflib.read_rows(data, header))
    assert [r.chunk_id for r in rows] == ["KB#2000001", "KB#2000002", "KB#2000003"]
