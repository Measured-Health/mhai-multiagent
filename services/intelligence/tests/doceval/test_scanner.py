import copy
import json
import os
import random
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.synth import patients, scanner

from .test_patients import forbid_global_random_and_clock


def _doc(seed: int = 1) -> dict[str, Any]:
    for p in patients.generate(random.Random(seed), 20):
        for d in p.documents:
            if d["observations"]:
                return dict(d)
    raise AssertionError("no document")


def _set(doc: dict[str, Any], path: tuple[Any, ...], value: str) -> dict[str, Any]:
    out = copy.deepcopy(doc)
    node: Any = out
    for key in path[:-1]:
        node = node[key]
    node[path[-1]] = value
    return out


def _paths(findings: list[scanner.Finding]) -> set[str]:
    return {f.path for f in findings}


def test_generated_documents_pass() -> None:
    for p in patients.generate(random.Random(11), 300):
        for d in p.documents:
            assert scanner.check_document(d) == []


def test_scanner_flags_real_looking_mrn_and_name() -> None:
    doc = _doc()
    bad = _set(doc, ("patient", "mrn", "value"), "MRN 12345678")
    bad = _set(bad, ("patient", "family_name", "value"), "Zzyzxreal")
    found = scanner.check_document(bad)
    assert "patient.mrn.value" in _paths(found)
    assert "patient.family_name.value" in _paths(found)
    assert all(f.reason == "not_synthetic" for f in found)
    dump = repr(found) + "".join(str(f) for f in found)
    assert "12345678" not in dump and "Zzyzxreal" not in dump


@pytest.mark.parametrize(
    ("where", "value", "rule"),
    [
        ("mrn", "12345678", "mrn_pattern"),
        ("mrn", "SYN-1234", "mrn_pattern"),
        ("mrn", "syn-12345678", "mrn_pattern"),
        ("facility", "Lagos University Teaching Hospital", "facility"),
        ("location", "Ward 3 Real General", "facility"),
        ("clinician", "Dr Smithson Realname", "clinician"),
        ("given", "Realname", "name"),
        ("other_id", "12345678901", "id_pattern"),
        ("other_id", "SYNID-1234", "id_pattern"),
    ],
)
def test_typed_fields_refused(where: str, value: str, rule: str) -> None:
    doc = _doc()
    path = {
        "mrn": ("patient", "mrn", "value"),
        "facility": ("document", "facility_name", "value"),
        "location": ("encounter", "location_text", "value"),
        "clinician": ("encounter", "clinician_text", "value"),
        "given": ("patient", "given_names", "value"),
        "other_id": ("patient", "other_ids", 0, "value", "value"),
    }[where]
    if where == "other_id" and not doc["patient"]["other_ids"]:
        doc["patient"]["other_ids"] = [copy.deepcopy(doc["patient"]["mrn"])]
        doc["patient"]["other_ids"][0] = {
            "kind": copy.deepcopy(doc["patient"]["sex"]),
            "value": copy.deepcopy(doc["patient"]["mrn"]),
        }
        doc["patient"]["other_ids"][0]["kind"]["value"] = "national_id"
    found = scanner.check_document(_set(doc, path, value))
    assert rule in {f.rule for f in found}
    assert value not in repr(found)


@pytest.mark.parametrize(
    "text",
    [
        "call 08031234567",
        "call +234 803 123 4567",
        "call 0803-123-4567",
        "NIN 12345678901",
        "BVN: 22222222222",
        "mail me at jane.doe@example.com",
        "see https://example.org/x",
        "MRN 7654321",
        "MRN: LUTH/123456",
        "lives at 12 Awolowo Road",
        "seen by Dr Adebayo",
        "tel 555-0142 and 5551234567",
    ],
)
def test_free_text_refused(text: str) -> None:
    doc = _doc()
    doc["notes"] = [
        {
            "heading": copy.deepcopy(doc["document"]["language"]),
            "text": copy.deepcopy(doc["document"]["language"]),
        }
    ]
    doc["notes"][0]["text"]["value"] = text
    doc["notes"][0]["text"]["raw_text"] = None
    found = scanner.check_document(doc)
    assert "notes[0].text.value" in _paths(found)
    assert text not in repr(found)


def test_synthetic_phone_and_ids_allowed_in_text() -> None:
    doc = _doc()
    note = {
        "heading": copy.deepcopy(doc["document"]["language"]),
        "text": copy.deepcopy(doc["document"]["language"]),
    }
    note["text"]["value"] = "Ref SYN-00000042 tel 555-0142, SYNID-12345678 on 2026-03-02 10:15:00"
    note["text"]["raw_text"] = None
    doc["notes"] = [note]
    assert scanner.check_document(doc) == []


def test_schema_invalid_is_refused() -> None:
    doc = _doc()
    del doc["patient"]
    assert scanner.check_document(doc)
    assert scanner.check_document("not a document")
    assert scanner.check_document({"schema_version": "1.0.0"})


def test_scan_set_directory(tmp_path: Path) -> None:
    for i, p in enumerate(patients.generate(random.Random(3), 5)):
        (tmp_path / f"d{i}.json").write_text(json.dumps(p.documents[0]), encoding="utf-8")
    (tmp_path / "d0.png").write_bytes(b"\x89PNG")
    res = scanner.scan_set(tmp_path)
    assert res.ok and res.findings == () and res.scanned == 5


def test_scan_set_flags_bad_file_with_path_only(tmp_path: Path) -> None:
    doc = _set(_doc(), ("patient", "mrn", "value"), "98765432")
    (tmp_path / "bad.json").write_text(json.dumps(doc), encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert not res.ok
    assert any(f.path == "bad.json#patient.mrn.value" for f in res.findings)
    assert "98765432" not in repr(res)


def test_scan_set_fails_closed(tmp_path: Path) -> None:
    assert not scanner.scan_set(tmp_path).ok  # empty
    assert not scanner.scan_set(tmp_path / "missing").ok
    (tmp_path / "broken.json").write_text("{not json", encoding="utf-8")
    assert not scanner.scan_set(tmp_path).ok
    (tmp_path / "broken.json").write_text('{"a": NaN}', encoding="utf-8")
    assert not scanner.scan_set(tmp_path).ok
    (tmp_path / "broken.json").unlink()
    (tmp_path / "ok.json").write_text(json.dumps(_doc()), encoding="utf-8")
    (tmp_path / "scan.pdf").write_bytes(b"%PDF")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "unexpected_file" for f in res.findings)


def test_scan_set_refuses_symlink(tmp_path: Path) -> None:
    outside = tmp_path / "outside.json"
    outside.write_text(json.dumps(_doc()), encoding="utf-8")
    root = tmp_path / "set"
    root.mkdir()
    (root / "link.json").symlink_to(outside)
    res = scanner.scan_set(root)
    assert any(f.rule == "symlink" for f in res.findings)


def test_intake_metadata(tmp_path: Path) -> None:
    good = {"page_id": "p1", "mrn": "SYN-00000001", "facility": "Northvale Synthetic Clinic"}
    assert scanner.check_metadata(good) == []
    bad = {"page_id": "p1", "mrn": "00000001", "facility": "Real Hospital", "note": "a@b.co"}
    found = scanner.check_metadata(bad)
    assert _paths(found) == {"mrn", "facility", "[k3]"}
    (tmp_path / "intake.jsonl").write_text(
        json.dumps(good) + "\n" + json.dumps(bad) + "\n", encoding="utf-8"
    )
    res = scanner.scan_set(tmp_path)
    assert not res.ok
    assert any(f.path.startswith("intake.jsonl:2#") for f in res.findings)
    assert not any(f.path.startswith("intake.jsonl:1#") for f in res.findings)


def test_no_global_random_or_clock_in_scanner(monkeypatch: pytest.MonkeyPatch) -> None:
    doc = _doc()
    forbid_global_random_and_clock(monkeypatch)
    assert scanner.check_document(doc) == []
    assert scanner.check_metadata({"mrn": "SYN-00000001"}) == []


def _note_doc(text: str) -> dict[str, Any]:
    doc = _doc()
    note = {
        "heading": copy.deepcopy(doc["document"]["language"]),
        "text": copy.deepcopy(doc["document"]["language"]),
    }
    note["text"]["value"] = text
    note["text"]["raw_text"] = None
    doc["notes"] = [note]
    return doc


@pytest.mark.parametrize(
    "text",
    [
        "call 0803 123 4567",
        "call 0803.123.4567",
        "call (0803) 123-4567",
        "call \uff10\uff18\uff10\uff13\uff11\uff12\uff13\uff14\uff15\uff16\uff17",
        "id 123456",
        "LUTH/2019/12345",
        "UCH/12/345",
        "Hosp. No. 4455",
        "Card No 88",
        "File No: A9",
        "Reg No 7",
        "Unit No 2",
        "NHIS 12",
        "NIN 1",
        "BVN 1",
        "seen by dr adebayo",
        "seen by CHIEF Okonkwo",
        "Alhaji Musa came",
        "Pastor John",
        "Engr Bello",
        "Barr Eze",
        "Rev Ade",
        "Hon Danjuma",
        "Mallam Sani",
        "Madam Ngozi",
        "at No. 5, Aminu Boulevard",
        "at 5 Aminu Str",
        "at 5 aminu road",
        "at 7 Lekki Expressway",
        "at 7 Kano Highway",
        ", 12 Awolowo Road",
    ],
)
def test_more_free_text_refused(text: str) -> None:
    found = scanner.check_document(_note_doc(text))
    assert "notes[0].text.value" in _paths(found)


def test_printed_dates_and_clinical_phrases_allowed() -> None:
    for text in (
        "seen 12-03-2026, again 12.03.2026 and 12/03/2026 or 2026/03/12",
        "Chief complaint: cough. Nurse practitioner reviewed.",
        "at 14 Kestrel Lane",
    ):
        assert scanner.check_document(_note_doc(text)) == []


def test_metadata_keys_are_scanned_and_hidden() -> None:
    for key in ("08031234567", "jane.doe@example.com", "Dr Adebayo", "NIN 12345678901"):
        found = scanner.check_metadata({"ok": 1, key: "x"})
        assert found, key
        assert all(key not in str(f) for f in found)
        assert {f.path for f in found} == {"[k1]"}
    nested = scanner.check_metadata({"a": [{"08031234567": 1}]})
    assert nested and all("0803" not in str(f) for f in nested)


def test_numeric_leaves_scanned() -> None:
    assert scanner.check_metadata({"x": 12345678901})
    assert scanner.check_metadata({"x": 8031234567})
    assert scanner.check_metadata({"x": [1234567.0]})
    assert scanner.check_metadata({"x": 5, "y": True}) == []
    doc = _doc()
    assert scanner.check_document(doc) == []  # numeric lab fields and boxes are exempt


def test_orphan_image_refused(tmp_path: Path) -> None:
    (tmp_path / "a.png").write_bytes(b"\x89PNG")
    (tmp_path / "ok.json").write_text(json.dumps(_doc()), encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "orphan_image" and f.path == "a.png" for f in res.findings)
    (tmp_path / "a.json").write_text(json.dumps(_doc(2)), encoding="utf-8")
    assert scanner.scan_set(tmp_path).ok
    (tmp_path / "b.png").write_bytes(b"\x89PNG")
    (tmp_path / "meta.jsonl").write_text(json.dumps({"image": "b.png"}) + "\n", encoding="utf-8")
    assert scanner.scan_set(tmp_path).ok


def test_file_names_scanned(tmp_path: Path) -> None:
    (tmp_path / "08031234567.json").write_text(json.dumps(_doc()), encoding="utf-8")
    (tmp_path / "Dr Adebayo.json").write_text(json.dumps(_doc()), encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "filename" for f in res.findings)
    dump = repr(res)
    assert "0803" not in dump and "Adebayo" not in dump


def test_duplicate_keys_and_special_files_refused(tmp_path: Path) -> None:
    (tmp_path / "dup.json").write_text('{"a": 1, "a": 2}', encoding="utf-8")
    assert any(f.rule == "unreadable" for f in scanner.scan_set(tmp_path).findings)
    (tmp_path / "dup.json").unlink()
    os.mkfifo(tmp_path / "pipe.json")
    assert any(f.rule == "unexpected_file" for f in scanner.scan_set(tmp_path).findings)


def test_file_names_use_strict_digit_rule(tmp_path: Path) -> None:
    for name in ("0803-123-4567.png", "12345_678901.json", "123-456.json"):
        (tmp_path / name).write_bytes(b"{}")
    res = scanner.scan_set(tmp_path)
    assert sum(f.rule == "filename" for f in res.findings) == 3
    dump = repr(res)
    assert "0803" not in dump and "12345" not in dump and "123-456" not in dump


@pytest.mark.parametrize(
    ("path", "text"),
    [
        (("observations", 0, "value_text"), "0803 123 4567"),
        (("observations", 0, "value_text"), "12345678901"),
        (("observations", 0, "reference_text"), "0803-123-4567"),
        (("medications", 0, "dose_text"), "0803 123 4567"),
        (("medications", 0, "dose_text"), "NIN 123 4567 8901"),
        (("medications", 0, "dose_text"), "12345678901"),
    ],
)
def test_lab_text_fields_still_catch_phone_and_nin(path: tuple[Any, ...], text: str) -> None:
    doc: dict[str, Any] = {}
    for p in patients.generate(random.Random(5), 50):
        for d in p.documents:
            if d["observations"] and d["medications"] and not doc:
                doc = dict(d)
    if not doc:  # fall back: borrow a medication row into a lab document
        doc = _doc()
        for p in patients.generate(random.Random(5), 50):
            for d in p.documents:
                if d["medications"]:
                    doc["medications"] = d["medications"]
    found = scanner.check_document(_set(doc, (*path, "value"), text))
    where = f"{path[0]}[{path[1]}].{path[2]}.value"
    assert where in _paths(found)
    assert text not in repr(found)


@pytest.mark.parametrize(
    "text",
    [
        "0803 - 123 - 4567",
        "0803 -- 123 -- 4567",
        "0803.123.4567",
        "+234 (803) 123 4567",
        "0803 \u2013 123 \u2014 4567",
        "0803 / 123 / 4567",
    ],
)
def test_phone_separator_variants(text: str, tmp_path: Path) -> None:
    assert "phone" in {f.rule for f in scanner.check_document(_note_doc("Call " + text))}
    meds = [
        m
        for p in patients.generate(random.Random(5), 50)
        for d in p.documents
        for m in d["medications"]
    ]
    for where, field in (("observations", "value_text"), ("medications", "dose_text")):
        doc = _doc()
        if where == "medications":
            doc["medications"] = meds[:1]
        found = scanner.check_document(_set(doc, (where, 0, field, "value"), text))
        assert "phone" in {f.rule for f in found}, field
    name = text.replace("/", "_").replace("\u2013", "-").replace("\u2014", "-")
    (tmp_path / (name + ".json")).write_text("{}", encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "filename" for f in res.findings)


@pytest.mark.parametrize(
    "text",
    ["Dr.Smith", "Dr.smith", "Doctor Smith", "Alh Musa", "Barrister Eze", "Oba Ade", "Mr.Bello"],
)
def test_titles_without_space_and_more_honorifics(text: str) -> None:
    assert "titled_name" in {f.rule for f in scanner.check_document(_note_doc("seen by " + text))}


def test_synthetic_phone_only_as_whole_token() -> None:
    assert scanner.check_document(_note_doc("ph 555-0142")) == []
    for text in ("5555-0142", "1-555-0142", "555-01421", "555-0142-99"):
        assert scanner.check_document(_note_doc(text)), text
