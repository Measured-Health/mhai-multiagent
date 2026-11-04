import copy
import json
import random
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.synth import patients, scanner


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
    (tmp_path / "page.png").write_bytes(b"\x89PNG")
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
    assert _paths(found) == {"mrn", "facility", "note"}
    (tmp_path / "intake.jsonl").write_text(
        json.dumps(good) + "\n" + json.dumps(bad) + "\n", encoding="utf-8"
    )
    res = scanner.scan_set(tmp_path)
    assert not res.ok
    assert any(f.path.startswith("intake.jsonl:2#") for f in res.findings)
    assert not any(f.path.startswith("intake.jsonl:1#") for f in res.findings)


def test_no_global_random_or_clock_in_scanner(monkeypatch: pytest.MonkeyPatch) -> None:
    import time

    doc = _doc()

    def boom(*_a: object, **_k: object) -> float:
        raise AssertionError("clock or randomness used")

    monkeypatch.setattr(random, "random", boom)
    monkeypatch.setattr(time, "time", boom)
    assert scanner.check_document(doc) == []
