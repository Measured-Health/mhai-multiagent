import copy
import io
import random
import re
from dataclasses import replace
from pathlib import Path
from typing import Any

import pytest
from PIL import Image

from intelligence.doceval import schema
from intelligence.doceval.synth import (
    degrade,
    handwriting,
    intake,
    layouts,
    patients,
    render,
    scanner,
)

DPI = 150
SOURCE = float(DPI)

# geometry and mild noise only, so registration accuracy is what is measured
PHOTO = replace(
    degrade.PROFILES["phone"],
    name="intake_photo",
    blur_px=(0.5, 0.8),
    noise_sigma=(1, 2),
    jpeg_quality=(85, 90),
    folds=(0, 0),
    shadow=(0, 0),
    stamps=(0, 0),
    lighting=(0.1, 0.2),
    perspective=(0.02, 0.04),
    crop_margin=(0.03, 0.05),
    long_side_px=(2000, 2000),
)
SCAN = replace(
    degrade.PROFILES["poor"],
    name="intake_scan",
    blur_px=(0.3, 0.5),
    noise_sigma=(1, 2),
    jpeg_quality=(85, 90),
    dpi=(150, 150),
    binarise_prob=0.0,
    folds=(0, 0),
    shadow=(0, 0),
    stamps=(0, 0),
    skew_deg=(0.4, 0.8),
)


def _doc(doc_type: str = "lab_report") -> dict[str, Any]:
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            if d["document"]["type"]["value"] == doc_type:
                return copy.deepcopy(d)
    raise AssertionError("no document")


def _printed(doc_type: str = "lab_report", tag: str = "a") -> render.RenderedDocument:
    layout = layouts.for_type(doc_type, 5, purpose="train")[0]
    return render.render(
        _doc(doc_type),
        layout,
        DPI,
        page_id_for=lambda n: render.make_page_id(f"test/{tag}/{doc_type}/{n}"),
    )


PRINTED = _printed()
MANIFEST = intake.build_manifest(PRINTED)


def _iou(a: tuple[float, ...], b: tuple[float, ...]) -> float:
    ix = max(0.0, min(a[2], b[2]) - max(a[0], b[0]))
    iy = max(0.0, min(a[3], b[3]) - max(a[1], b[1]))
    inter = ix * iy
    union = (a[2] - a[0]) * (a[3] - a[1]) + (b[2] - b[0]) * (b[3] - b[1]) - inter
    return inter / union if union > 0 else 0.0


def _put(root: Path, folder: str, name: str, data: bytes) -> None:
    (root / folder).mkdir(parents=True, exist_ok=True)
    (root / folder / name).write_bytes(data)


def _photo(
    doc: render.RenderedDocument, seed: int, mode: str = "photo"
) -> degrade.DegradedDocument:
    profile = PHOTO if mode == "photo" else SCAN
    return degrade.degrade_document(doc, seed, profile, mode, source_dpi=SOURCE)  # type: ignore[arg-type]


def test_page_ids_are_scanner_safe() -> None:
    assert PRINTED.page_ids
    for page_id in PRINTED.page_ids:
        assert re.fullmatch(r"SYN-PG-[A-Z2-7]{8}", page_id)
        assert not re.search(r"[0-9]{4}", page_id)
    assert (
        render.page_id_from_bits(render.page_id_bits(PRINTED.page_ids[0])) == (PRINTED.page_ids[0])
    )


def test_registration_maps_boxes(tmp_path: Path) -> None:
    for seed, mode, folder in ((1, "photo", "phone_photo"), (2, "scan", "real_scan")):
        shot = _photo(PRINTED, seed, mode)
        root = tmp_path / folder
        for n, page in enumerate(shot.pages, start=1):
            _put(root, folder, f"p{n}.png", page)
        res = intake.ingest(root, MANIFEST)
        assert [r.reason for r in res.refused] == []
        assert len(res.accepted) == len(shot.pages)
        for item in res.accepted:
            assert item.slice == folder
            page_no = int(item.metadata["page_no"])
            checked = 0
            for path, (pg, true_box) in shot.boxes.items():
                if pg != page_no:
                    continue
                got = item.boxes[path]
                assert got is not None, path
                assert _iou(got, true_box) >= 0.9, (mode, path)
                checked += 1
            assert checked > 0
            assert scanner.check_metadata(item.metadata) == []


def test_unreadable_fields_have_null_boxes(tmp_path: Path) -> None:
    # a photo that crops the page's lower part still shows the fiducials but not every field
    shot = _photo(PRINTED, 3)
    _put(tmp_path, "phone_photo", "p1.png", shot.pages[0])
    res = intake.ingest(tmp_path, MANIFEST)
    item = res.accepted[0]
    for path in item.unreadable:
        assert item.boxes[path] is None
    fields = dict(schema.iter_fields(item.ground_truth))
    for path, box in item.boxes.items():
        assert (fields[path]["bbox"] is None) == (box is None)


def test_unknown_or_missing_id_refused(tmp_path: Path) -> None:
    other = _printed(tag="zz")  # a valid page, but not in this manifest
    _put(tmp_path, "real_scan", "u1.png", _photo(other, 4, "scan").pages[0])
    # no fiducials at all: a plain page
    plain = render.render(_doc(), layouts.for_type("lab_report", 5, purpose="train")[0], DPI)
    _put(tmp_path, "real_scan", "u2.png", plain.pages[0])
    # corners cut off: crop the printed page inside its fiducials
    with Image.open(io.BytesIO(PRINTED.pages[0])) as im:
        w, h = im.size
        buf = io.BytesIO()
        im.crop((int(w * 0.08), int(h * 0.08), w, h)).save(buf, format="PNG")
    _put(tmp_path, "real_scan", "u3.png", buf.getvalue())
    # noise that is not a page
    rng = random.Random(1)
    noise = Image.frombytes("RGB", (400, 400), bytes(rng.randrange(256) for _ in range(480000)))
    buf = io.BytesIO()
    noise.save(buf, format="PNG")
    _put(tmp_path, "real_scan", "u4.png", buf.getvalue())
    res = intake.ingest(tmp_path, MANIFEST)
    assert res.accepted == []
    reasons = sorted(r.reason for r in res.refused)
    assert len(reasons) == 4
    assert set(reasons) <= intake.REASONS
    assert reasons.count("page_id_unknown") == 1
    assert "fiducials_not_found" in reasons
    # a refusal names the position, never the file name
    assert all("u1" not in r.label and "u2" not in r.label for r in res.refused)


def test_duplicate_image_refused(tmp_path: Path) -> None:
    shot = _photo(PRINTED, 5)
    again = _photo(PRINTED, 6, "scan")  # same page id, different pixels
    _put(tmp_path, "phone_photo", "p1.png", shot.pages[0])
    _put(tmp_path, "phone_photo", "p2.png", shot.pages[0])
    _put(tmp_path, "real_scan", "p3.png", again.pages[0])
    if len(shot.pages) > 1:
        _put(tmp_path, "phone_photo", "p4.png", shot.pages[1])
    res = intake.ingest(tmp_path, MANIFEST)
    dups = [r for r in res.refused if r.reason == "duplicate_page"]
    assert len(dups) == 3
    assert all(item.metadata["page_no"] != 1 for item in res.accepted)


def test_exif_dropped(tmp_path: Path) -> None:
    shot = _photo(PRINTED, 7)
    img = Image.open(io.BytesIO(shot.pages[0])).convert("RGB")
    exif = Image.Exif()
    exif[0x010F] = "Phone Maker"
    exif[0x8825] = {1: "N", 2: (6.0, 27.0, 0.0), 3: "E", 4: (3.0, 23.0, 0.0)}  # GPS
    buf = io.BytesIO()
    img.save(buf, format="JPEG", quality=92, exif=exif)
    raw = buf.getvalue()
    assert b"Exif" in raw
    _put(tmp_path, "phone_photo", "p1.jpg", raw)
    res = intake.ingest(tmp_path, MANIFEST)
    assert [r.reason for r in res.refused] == []
    out = res.accepted[0].image
    assert out.startswith(b"\x89PNG\r\n\x1a\n")
    chunks, pos = [], 8
    while pos < len(out):
        n = int.from_bytes(out[pos : pos + 4], "big")
        chunks.append(out[pos + 4 : pos + 8])
        pos += 12 + n
    assert chunks[0] == b"IHDR" and chunks[-1] == b"IEND"
    assert set(chunks) <= {b"IHDR", b"IDAT", b"IEND"}
    assert b"Exif" not in out and b"GPS" not in out
    assert not Image.open(io.BytesIO(out)).getexif()


def test_scanner_runs_on_intake(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    shot = _photo(PRINTED, 8)
    _put(tmp_path / "in", "phone_photo", "p1.png", shot.pages[0])
    res = intake.ingest(tmp_path / "in", MANIFEST)
    assert len(res.accepted) == 1
    ids = intake.write_result(res, tmp_path / "out")
    assert len(ids) == 1
    assert scanner.scan_set(tmp_path / "out").ok
    # a manifest whose ground truth is not synthetic is refused, not stored
    bad_gt = copy.deepcopy(dict(MANIFEST[PRINTED.page_ids[0]].ground_truth))
    bad_gt["patient"]["mrn"]["raw_text"] = "LUTH/2019/123456"
    bad_gt["patient"]["mrn"]["value"] = "LUTH/2019/123456"
    bad = dict(MANIFEST)
    bad[PRINTED.page_ids[0]] = replace(MANIFEST[PRINTED.page_ids[0]], ground_truth=bad_gt)
    res = intake.ingest(tmp_path / "in", bad)
    assert res.accepted == []
    assert [r.reason for r in res.refused] == ["scanner_refused"]
    # metadata is keyed fields only: no names, phones or emails can be in it
    meta = intake.ingest(tmp_path / "in", MANIFEST).accepted[0].metadata
    assert set(meta) <= {
        "page_id",
        "document_id",
        "page_no",
        "slice",
        "width",
        "height",
        "unreadable_count",
        "record_id",
        "consent_id",
        "role",
    }


def test_volunteer_form(tmp_path: Path) -> None:
    doc = _doc("vitals_chart")
    layout = layouts.for_type("vitals_chart", 5, purpose="train")[0]
    hand = handwriting.handwritten_document(
        doc, layout, random.Random(3), mode="handwritten", dpi=DPI
    )
    ids = tuple(render.make_page_id(f"test/form/{n}") for n in range(1, len(hand.pages) + 1))
    marked = replace(
        hand,
        pages=[render.stamp_registration(p, i) for p, i in zip(hand.pages, ids, strict=True)],
        page_ids=ids,
    )
    manifest = intake.build_manifest(marked, form=True, record_id="SYN-RC-ABCDEFGH")
    shot = degrade.degrade_document(marked, 9, PHOTO, "photo", source_dpi=SOURCE)
    for n, page in enumerate(shot.pages, start=1):
        _put(tmp_path, "handwritten_volunteer", f"f{n}.png", page)
    # no consent: refused
    res = intake.ingest(tmp_path, manifest)
    assert res.accepted == [] and {r.reason for r in res.refused} == {"consent_missing"}
    vol = {
        f"handwritten_volunteer/f{n}.png": intake.Volunteer("CN-ABCDEFGH", "volunteer")
        for n in range(1, len(shot.pages) + 1)
    }
    res = intake.ingest(tmp_path, manifest, volunteers=vol)
    assert [r.reason for r in res.refused] == []
    item = res.accepted[0]
    assert item.slice == "handwritten_volunteer"
    assert item.metadata["record_id"] == "SYN-RC-ABCDEFGH"
    assert item.metadata["consent_id"] == "CN-ABCDEFGH" and item.metadata["role"] == "volunteer"
    for path, (pg, true_box) in shot.boxes.items():
        if pg == item.metadata["page_no"]:
            got = item.boxes[path]
            assert got is not None and _iou(got, true_box) >= 0.9
    # a printed (non-form) page in the volunteer folder is a mismatch
    other = tmp_path / "mix"
    _put(other, "handwritten_volunteer", "m1.png", _photo(PRINTED, 10).pages[0])
    res = intake.ingest(
        other,
        MANIFEST,
        volunteers={"handwritten_volunteer/m1.png": intake.Volunteer("CN-ABCDEFGH", "volunteer")},
    )
    assert [r.reason for r in res.refused] == ["slice_mismatch"]


def test_volunteer_record_refuses_names() -> None:
    for consent, role in (
        ("Ada Obi", "volunteer"),
        ("CN-ABCDEFGH", "ada@example.org"),
        ("08031234567", "volunteer"),
    ):
        with pytest.raises(intake.IntakeError):
            intake.Volunteer(consent, role)


def test_folder_rules(tmp_path: Path) -> None:
    shot = _photo(PRINTED, 11)
    (tmp_path / "loose.png").write_bytes(shot.pages[0])
    _put(tmp_path, "elsewhere", "a.png", shot.pages[0])
    _put(tmp_path, "real_scan", "notes.txt", b"hello")
    (tmp_path / "real_scan" / "link.png").symlink_to(tmp_path / "loose.png")
    res = intake.ingest(tmp_path, MANIFEST)
    assert res.accepted == []
    assert {r.reason for r in res.refused} == {
        "slice_unknown",
        "unsupported_file",
        "unexpected_entry",
    }


def test_bad_manifest_refused(tmp_path: Path) -> None:
    with pytest.raises(intake.IntakeError):
        intake.ingest(tmp_path, {})
    with pytest.raises(intake.IntakeError):
        intake.ingest(tmp_path / "missing", MANIFEST)


def test_render_without_ids_unchanged() -> None:
    layout = layouts.for_type("lab_report", 5, purpose="train")[0]
    a = render.render(_doc(), layout, DPI)
    b = render.render(_doc(), layout, DPI)
    assert a.pages == b.pages and a.page_ids == ()
    assert a.pages != PRINTED.pages
    assert a.boxes == PRINTED.boxes  # marks do not move any field
