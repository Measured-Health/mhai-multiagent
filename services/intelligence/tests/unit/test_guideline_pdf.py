"""Guideline documents as libraries (intelligence/knowledge_base/guideline_pdf.py):
the opt-in lock, the clean-up, the section-aware chunking and the rows. The
PDFs here are synthetic (tests/unit/_synthetic_pdf.py); no model.
"""

from __future__ import annotations

import dataclasses
import hashlib
import json
from collections import Counter
from pathlib import Path

import pytest

from intelligence.knowledge_base import guideline_pdf as guidelines
from intelligence.knowledge_base import reference_library as reflib
from tests.unit._synthetic_pdf import make_pdf
from tests.unit.test_reference_library_convert import EMBEDDER, fake_embed

HEADER = "Synthetic Guidelines for Synthetic Fever"


def _pages() -> list[list[str]]:
    pages = []
    for p in range(1, 9):
        word = "abcdefgh"[p - 1] * 3
        body = f"Synthetic fever note {word} is treated with the synthetic combination for days."
        lines = [HEADER]
        if p == 1:
            lines += ["TABLE OF CONTENTS", "4 TREATMENT ............................ 3"]
        if p == 3:
            lines += ["4 TREATMENT OF SYNTHETIC FEVER", "4.1 Synthetic combination"]
        if p == 4:
            lines += [
                "Table 2: synthetic dose by weight",
                "5-14 kg 1 tablet 0, 8, 24, 36, 48 and 60 hours",
                "15-24 kg 2 tablets 0, 8, 24, 36, 48 and 60 hours",
            ]
        if p == 6:
            lines += ["4.2 Synthetic severe fever"]
        lines += [body] * 6
        lines += [str(p)]
        pages.append(lines)
    return pages


def _doc(data: bytes, **over: object) -> guidelines.Document:
    base = guidelines.Document(
        name="who-malaria",
        title="Synthetic guideline",
        short_title="Synthetic guideline",
        publisher=reflib.WHO,
        edition="synthetic edition",
        url="https://example.org/synthetic.pdf",
        pdf_sha256=hashlib.sha256(data).hexdigest(),
        first_page=1,
        last_page=8,
        licence="synthetic licence",
        licence_text="Synthetic licence text.",
        licence_status="needs_owner_decision",
        retrieved="2026-11-23",
        version="1.0.0",
    )
    return dataclasses.replace(base, **over)  # type: ignore[arg-type]


def test_the_committed_lock_holds_three_opt_in_documents() -> None:
    docs = guidelines.load_documents()
    assert set(docs) == {"who-malaria", "ng-malaria", "ng-stg"}
    assert docs["who-malaria"].licence == "CC BY-NC-SA 3.0 IGO"
    assert docs["who-malaria"].licence_status == "needs_owner_decision"
    assert docs["ng-malaria"].licence_status == "needs_owner_decision"
    assert docs["ng-stg"].licence_status == "unknown" and docs["ng-stg"].pdf_sha256 is None
    assert not any(guidelines.loadable(d) for d in docs.values())
    for d in docs.values():
        assert d.publisher in reflib.LIBRARIES[d.name].sources["guideline"]


@pytest.mark.parametrize(
    "change",
    [
        {"schema": "other"},
        {"documents": {"synthetic-other": {}}},
        {"documents": {"medref": {}}},
    ],
)
def test_a_bad_lock_is_refused(tmp_path: Path, change: dict[str, object]) -> None:
    obj = json.loads(guidelines.LOCK_PATH.read_text())
    obj.update(change)
    p = tmp_path / "lock"
    p.write_text(json.dumps(obj))
    with pytest.raises(guidelines.GuidelineError):
        guidelines.load_documents(p)


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("licence_status", "maybe"),
        ("publisher", "Synthetic Publisher"),
        ("url", "http://example.org/x.pdf"),
        ("pdf_sha256", "xyz"),
        ("patient_id", "SYN-1"),
        ("title", "Sample reference (synthetic demo)"),
    ],
)
def test_a_bad_lock_entry_is_refused(tmp_path: Path, field: str, value: str) -> None:
    obj = json.loads(guidelines.LOCK_PATH.read_text())
    obj["documents"]["who-malaria"][field] = value
    p = tmp_path / "lock"
    p.write_text(json.dumps(obj))
    with pytest.raises(guidelines.GuidelineError):
        guidelines.load_documents(p)


@pytest.mark.parametrize(
    ("line", "want"),
    [
        ("4.2.1 Artemether-lumefantrine", "4.2.1 Artemether-lumefantrine"),
        (
            "6 ASSESSMENT AND MANAGEMENT OF SEVERE MALARIA",
            "6 ASSESSMENT AND MANAGEMENT OF SEVERE MALARIA",
        ),
        ("4. Prevention", "4 Prevention"),
        ("1. Give the first dose and observe the patient for one hour before discharge.", None),
        ("12 tablets over three days", None),
        ("2020 was the year", None),
        ("3.1 the history", None),
        ("45 Synthetic", None),
        ("1 Tab twice daily x", None),
        ("2 Tablets 12 hourly", None),
    ],
)
def test_headings(line: str, want: str | None) -> None:
    assert guidelines.heading(line) == want


def test_clean_up_drops_running_headers_page_numbers_and_contents() -> None:
    pages = [(i + 1, "\n".join(p)) for i, p in enumerate(_pages())]
    lines = guidelines.clean_pages(pages)
    text = [x for _, x in lines]
    assert HEADER not in text
    assert not any(x.isdigit() for x in text)
    assert not any("....." in x for x in text)
    assert "4.1 Synthetic combination" in text


def test_line_clean_up_removes_delimiters_and_citations() -> None:
    assert guidelines._line("a << b >> c kb # 12") == "a « b » c KB no. 12"
    assert not reflib.forged(guidelines._line("x <<<< KB#1000001 >>"))


def test_chunks_follow_sections_and_keep_a_dose_table_together() -> None:
    pages = [(i + 1, "\n".join(p)) for i, p in enumerate(_pages())]
    chunks = guidelines.chunk_lines(guidelines.clean_pages(pages))
    assert all(len(c.text.split()) <= guidelines.MAX_WORDS for c in chunks)
    table = [c for c in chunks if "5-14 kg 1 tablet" in c.text]
    assert len(table) == 1 and "15-24 kg 2 tablets" in table[0].text
    assert table[0].section == "4.1 Synthetic combination"
    sections = [c.section for c in chunks]
    assert "4.2 Synthetic severe fever" in sections
    assert sections.index("4.2 Synthetic severe fever") > sections.index(
        "4.1 Synthetic combination"
    )
    for c in chunks:
        assert c.page_start <= c.page_end


def test_a_long_section_is_cut_within_the_bounds() -> None:
    lines = [(1, "4.1 Synthetic long section")] + [
        (1 + i // 40, "synthetic word " * 20) for i in range(200)
    ]
    chunks = guidelines.chunk_lines(lines)
    assert len(chunks) > 5
    for c in chunks:
        assert len(c.text.split()) <= guidelines.MAX_WORDS
        assert len(c.text) <= guidelines.MAX_BODY_CHARS + 200


def test_convert_a_synthetic_pdf(tmp_path: Path) -> None:
    data = make_pdf(_pages())
    doc = _doc(data)
    counts: Counter[str] = Counter()
    entries, sha = guidelines.convert(doc, data, counts)
    assert sha == doc.pdf_sha256 and counts["pages"] == 8 and counts["rows"] == len(entries)
    for e in entries:
        assert e.content.startswith("Synthetic guideline (synthetic edition)")
        assert reflib.row_refusal(e.content, e.metadata, "who-malaria") == ""
        assert e.metadata["publisher"] == reflib.WHO and e.metadata["document"] == "who-malaria"
        assert isinstance(e.metadata["page_start"], int)
    assert any("section 4.1 Synthetic combination" in e.content for e in entries)

    header, rows = guidelines.build(doc, data, fake_embed, embedder=EMBEDDER)
    assert header.name == "who-malaria" and header.built_from == sha
    assert rows[0].chunk_id == "KB#3000001"
    p1, sha1 = reflib.write_artefact(header, rows, tmp_path)
    header2, rows2 = guidelines.build(doc, data, fake_embed, embedder=EMBEDDER)
    (tmp_path / "b").mkdir()
    _, sha2 = reflib.write_artefact(header2, rows2, tmp_path / "b")
    assert sha1 == sha2


def test_convert_refuses_other_bytes_and_a_document_with_no_source() -> None:
    data = make_pdf(_pages())
    with pytest.raises(guidelines.GuidelineError) as info:
        guidelines.convert(_doc(data, pdf_sha256="ab" * 32), data)
    assert str(info.value) == "pdf_hash_mismatch"
    with pytest.raises(guidelines.GuidelineError) as info:
        guidelines.convert(_doc(data, pdf_sha256=None), data)
    assert str(info.value) == "no_source"
    junk = b"not a pdf"
    with pytest.raises(guidelines.GuidelineError) as info:
        guidelines.convert(_doc(junk), junk)
    assert str(info.value) == "not_a_pdf"


def test_a_guideline_row_with_a_patient_field_is_refused() -> None:
    data = make_pdf(_pages())
    entries, _ = guidelines.convert(_doc(data), data)
    meta = {**entries[0].metadata, "patient_id": "SYN-1"}
    assert reflib.row_refusal(entries[0].content, meta, "who-malaria") == (
        "row_patient_or_unknown_field"
    )
    # Another publisher's text is not this library's.
    meta = {**entries[0].metadata, "source": reflib.FMOH_NG}
    assert reflib.row_refusal(entries[0].content, meta, "who-malaria") == "row_kind_not_approved"
