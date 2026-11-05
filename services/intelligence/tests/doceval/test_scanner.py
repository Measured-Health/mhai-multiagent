import copy
import hashlib
import json
import os
import random
from collections.abc import Iterator
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


def test_orphan_image_matched_by_relative_path(tmp_path: Path) -> None:
    (tmp_path / "ok.json").write_text(json.dumps(_doc()), encoding="utf-8")
    (tmp_path / "x").mkdir()
    (tmp_path / "x" / "b.png").write_bytes(b"\x89PNG")
    # a listing at the root naming "b.png" means the root's b.png, not x/b.png
    (tmp_path / "meta.jsonl").write_text(json.dumps({"image": "b.png"}) + "\n", encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "orphan_image" and f.path == "x/b.png" for f in res.findings)
    # by its path from the listing's folder, it is referenced
    (tmp_path / "meta.jsonl").write_text(json.dumps({"image": "x/b.png"}) + "\n", encoding="utf-8")
    assert scanner.scan_set(tmp_path).ok
    (tmp_path / "meta.jsonl").unlink()
    (tmp_path / "x" / "m.jsonl").write_text(json.dumps({"image": "b.png"}) + "\n", encoding="utf-8")
    assert scanner.scan_set(tmp_path).ok


def test_numeric_exemptions_are_bounded() -> None:
    doc = _doc()
    rows = doc["observations"]
    assert rows, "the fixture document has lab rows"
    row = rows[0]
    row["value_numeric"]["value"] = 123456789  # nine integer digits: a lab number
    assert scanner.check_document(doc) == []
    row["value_numeric"]["value"] = 1234567890  # ten: checked like any number
    assert any(
        f.path.startswith("observations[0].value_numeric") for f in scanner.check_document(doc)
    )
    doc = _doc()
    fld = doc["observations"][0]["name_text"]
    fld["page"], fld["bbox"] = 1, [0.123456, 0.1, 1, 0.000001]
    assert scanner.check_document(doc) == []
    fld["bbox"] = [0.1234567, 0.1, 1, 0.2]  # seven decimals: no longer exempt
    assert any(".bbox[0]" in f.path for f in scanner.check_document(doc))


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


SHA = "0123456789abcdef" * 4
COMMIT = "0803123456789abcdef0123456789abcdef01234"  # a 13-digit run
TESTSET = "mhai.evaluation.testset/1"
HOSTILE = "mhai.evaluation.hostile/1"


@pytest.fixture
def at_commit(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    monkeypatch.setenv("MHAI_GIT_COMMIT", COMMIT)
    scanner.current_commit.cache_clear()
    yield
    scanner.current_commit.cache_clear()


def _testset(**extra: Any) -> dict[str, Any]:
    return {
        "schema": TESTSET,
        "files": [{"path": "truth/x.json", "size": 123456789, "sha256": SHA}],
        "generator": {"commit": COMMIT},
        **extra,
    }


def test_computed_values_exempt_only_in_their_file_path_and_shape(at_commit: None) -> None:
    assert scanner.check_metadata(_testset(), context="testset") == []
    box = {"schema": HOSTILE, "expected": {"regions": [{"bbox": [0.1234, 0.2, 0.9876, 1]}]}}
    assert scanner.check_metadata(box, context="hostile") == []
    # The same values in any other kind of file are checked as usual.
    assert scanner.check_metadata(_testset())  # no context: an ordinary metadata file
    assert scanner.check_metadata(_testset(), context="hostile")
    assert scanner.check_metadata({**_testset(), "schema": HOSTILE}, context="testset")
    # A 4-decimal box never trips the digit rules by itself, so check the exemption directly.
    where = ("expected", "regions", 0, "bbox", 0)
    assert scanner._computed_leaf(where, 0.1234, "hostile")
    assert not scanner._computed_leaf(where, 0.1234, None)
    assert not scanner._computed_leaf(where, 0.1234, "testset")
    assert not scanner._computed_leaf(("files", 0, "size"), 123456789, "hostile")
    assert not scanner._computed_leaf(("generator", "commit"), COMMIT, "hostile")
    # The hostile exemption needs the hostile schema too: a too-precise box shows it.
    precise = {"expected": {"regions": [{"bbox": [0.1234567, 0, 0, 0]}]}}
    assert scanner.check_metadata({**precise, "schema": TESTSET}, context="hostile")
    nin, phone = 12345678901, "08031234567"
    # An exempt key name at a path the set builder does not write is checked as usual.
    for bad in (
        {"size": nin},
        {"patient_x": {"size": nin}},
        {"files": {"size": nin}},
        {"files": [{"meta": {"size": nin}}]},
        {"sha256": phone},
        {"files": [{"sha256": phone}]},
        {"commit": phone},
        {"generator": {"commit": phone}},
        {"note": SHA},
        {"note": COMMIT},
        {"x": {"generator": {"commit": COMMIT}}},
        {"regions": [{"bbox": [0.1234567]}]},
        {"bbox": [0.1234567]},
        {"count": 123456789},
        {"schema": "folder/123456"},
        {"x": {"schema": TESTSET}},
    ):
        for ctx in ("testset", "hostile", None):
            assert scanner.check_metadata(
                {**bad, "schema": bad.get("schema", TESTSET)}, context=ctx
            ), bad
    # The right file and path with the wrong shape (or another commit) is checked as usual.
    for bad_ts in (
        _testset(files=[{"size": "08031234567"}]),
        _testset(files=[{"size": -12345678901}]),
        _testset(files=[{"sha256": SHA.upper()}]),
        _testset(generator={"commit": "0803123456789abcdef0123456789abcdef09999"}),
    ):
        assert scanner.check_metadata(bad_ts, context="testset"), bad_ts
    for bad_box in (
        [0.1234567, 0, 0, 0],  # too precise
        [0, 0, 0, 0, 0.1234567],  # fifth item
        [12345678.0],
    ):
        rec = {"schema": HOSTILE, "expected": {"regions": [{"bbox": bad_box}]}}
        assert scanner.check_metadata(rec, context="hostile"), bad_box


def test_commit_exempt_only_in_root_testset(tmp_path: Path, at_commit: None) -> None:
    (tmp_path / "a.json").write_text(json.dumps(_doc()), encoding="utf-8")
    data = (tmp_path / "a.json").read_bytes()
    listing = {
        "schema": TESTSET,
        "generator": {"commit": COMMIT},
        "files": [
            {"path": "a.json", "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        ],
    }
    (tmp_path / "testset.json").write_text(json.dumps(listing), encoding="utf-8")
    assert scanner.scan_set(tmp_path).ok
    for name in ("other.json", "hostile/x.json", "sub/testset.json"):
        target = tmp_path / name
        target.parent.mkdir(exist_ok=True)
        target.write_text(json.dumps({"schema": HOSTILE, "generator": {"commit": COMMIT}}))
        res = scanner.scan_set(tmp_path)
        assert not res.ok, name
        assert any(f.path.startswith(name) for f in res.findings), name
        target.unlink()


def test_truth_files_are_documents(tmp_path: Path) -> None:
    (tmp_path / "truth").mkdir()
    (tmp_path / "truth" / "a.json").write_text(json.dumps({"note": "hello"}), encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "schema" for f in res.findings)
    doc = _doc()
    del doc["schema_version"]
    (tmp_path / "truth" / "a.json").unlink()
    (tmp_path / "b.json").write_text(json.dumps(doc), encoding="utf-8")
    res = scanner.scan_set(tmp_path)
    assert any(f.rule == "schema" and f.path.startswith("b.json") for f in res.findings)


def _write_listing(root: Path, entries: list[dict[str, Any]]) -> None:
    (root / "testset.json").write_text(
        json.dumps({"schema": TESTSET, "files": entries}), encoding="utf-8"
    )


def test_listed_size_and_hash_recomputed(tmp_path: Path) -> None:
    (tmp_path / "a.json").write_text(json.dumps(_doc()), encoding="utf-8")
    data = (tmp_path / "a.json").read_bytes()
    sha = hashlib.sha256(data).hexdigest()
    _write_listing(tmp_path, [{"path": "a.json", "size": len(data), "sha256": sha}])
    assert scanner.scan_set(tmp_path).ok
    for entry, rule in (
        ({"path": "a.json", "size": 12345, "sha256": sha}, "listed_size_mismatch"),
        ({"path": "a.json", "size": len(data), "sha256": "0" * 64}, "listed_hash_mismatch"),
        ({"path": "missing.json", "size": 1}, "listed_file_missing"),
        ({"path": "../a.json", "size": len(data)}, "listed_file_missing"),
    ):
        _write_listing(tmp_path, [entry])
        res = scanner.scan_set(tmp_path)
        assert not res.ok
        assert any(f.rule == rule and f.path == "testset.json#files[0]" for f in res.findings)


def test_text_ok() -> None:
    assert scanner.text_ok("pages/kq2mzt4vbe/1.png")
    assert not scanner.text_ok("pages/08031234567/1.png")
