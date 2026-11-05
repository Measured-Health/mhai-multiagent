import copy
import hashlib
import io
import os
import random
import re
from dataclasses import replace
from functools import cache
from itertools import product
from pathlib import Path
from typing import Any

import numpy as np
import pytest
from PIL import Image, ImageDraw

from intelligence.doceval import schema
from intelligence.doceval.synth import (
    degrade,
    forms,
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


@cache
def _found(doc_type: str) -> dict[str, Any]:
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            if d["document"]["type"]["value"] == doc_type:
                return d  # type: ignore[no-any-return]
    raise AssertionError("no document")


def _doc(doc_type: str = "lab_report") -> dict[str, Any]:
    return copy.deepcopy(_found(doc_type))


def _layout(doc_type: str = "lab_report") -> layouts.LayoutSpec:
    return layouts.printable(doc_type, 5, purpose="train")[0]


def _printed(doc_type: str = "lab_report", tag: str = "a") -> render.RenderedDocument:
    layout = _layout(doc_type)
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
    plain = render.render(_doc(), _layout(), DPI)
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
    first = PRINTED.page_ids[0]
    assert first not in {i.metadata["page_id"] for i in res.accepted}
    if len(PRINTED.pages) > 1:
        assert [i.metadata["page_id"] for i in res.accepted] == [PRINTED.page_ids[1]]


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
    layout = _layout("vitals_chart")
    record_id = forms.make_record_id("test/rec/1")
    form = forms.blank_form(
        doc, layout, record_id, lambda n: render.make_page_id(f"test/form/{n}"), DPI
    )
    sheet = forms.record_sheet(doc, layout, record_id, DPI)
    manifest = intake.build_manifest(form, form=True, record_id=record_id)
    # the form is blank in its slots; the record sheet carries the values
    for path, (pg, box) in form.boxes.items():
        a = np.asarray(Image.open(io.BytesIO(form.pages[pg - 1])).convert("L"))
        b = np.asarray(Image.open(io.BytesIO(sheet.pages[pg - 1])).convert("L"))
        h, w = a.shape
        x0, y0, x1, y1 = box
        sl = (slice(int(y0 * h), int(y1 * h) - 2), slice(int(x0 * w) + 1, int(x1 * w) - 1))
        assert (a[sl] > 200).all() and (b[sl] < 100).any(), path
    # the record id is printed in the same place on both
    a0 = np.asarray(Image.open(io.BytesIO(form.pages[0])).convert("L"))
    b0 = np.asarray(Image.open(io.BytesIO(sheet.pages[0])).convert("L"))
    h0, w0 = a0.shape
    band = (slice(int(0.077 * w0), int(0.094 * w0)), slice(int(0.115 * w0), int(0.30 * w0)))
    assert (a0[band] == b0[band]).all() and (a0[band] < 100).any()
    assert sheet.page_ids == () and form.page_ids
    assert scanner.check_document(form.ground_truth) == []
    assert scanner.check_document(sheet.ground_truth) == []
    shot = degrade.degrade_document(form, 9, PHOTO, "photo", source_dpi=SOURCE)
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
    assert item.metadata["record_id"] == record_id
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


def test_form_needs_wide_margin_and_good_record_id() -> None:
    narrow = [x for x in layouts.for_type("lab_report", 5, purpose="train") if x.margin_mm < 22]
    assert narrow
    with pytest.raises(render.RenderError):
        render.render(_doc(), narrow[0], DPI, page_id_for=lambda n: render.make_page_id(str(n)))
    with pytest.raises(render.RenderError):
        forms.record_sheet(_doc(), _layout(), "SYN-RC-12345678", DPI)
    assert forms.record_id_ok(forms.make_record_id("x"))


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


GOLDEN = "8606a64aa3ab11d47bf80d8f2e2c47ac790c1de88d821d32226a570eaf8f15a6"


def test_render_without_ids_unchanged() -> None:
    layout = layouts.for_type("lab_report", 5, purpose="train")[0]
    a = render.render(_doc(), layout, DPI)
    b = render.render(_doc(), layout, DPI)
    assert a.pages == b.pages and a.page_ids == ()
    # the pixels are those the renderer produced before registration marks existed
    h = hashlib.sha256()
    for page in a.pages:
        im = Image.open(io.BytesIO(page))
        h.update(repr(im.size).encode())
        h.update(im.convert("RGB").tobytes())
    assert h.hexdigest() == GOLDEN
    marked = render.render(
        _doc(), _layout(), DPI, page_id_for=lambda n: render.make_page_id(f"t/{n}")
    )
    plain = render.render(_doc(), _layout(), DPI)
    assert marked.boxes == plain.boxes  # marks do not move any field


def _edge_px(got: tuple[float, ...], true: tuple[float, ...], w: int, h: int) -> float:
    """The largest distance, in photo pixels, between matching box edges."""
    return max(
        abs(got[0] - true[0]) * w,
        abs(got[2] - true[2]) * w,
        abs(got[1] - true[1]) * h,
        abs(got[3] - true[3]) * h,
    )


def _check_all(
    res: intake.IngestResult,
    shot: degrade.DegradedDocument,
    tall: float = 0.9,
    short: float = 0.8,
    max_px: float = 1.5,
) -> float:
    """Every accepted box agrees with the truth (IoU, and no edge off by more than
    ``max_px``); every refusal carries a fixed code. Returns the largest edge error."""
    worst = 0.0
    for item in res.accepted:
        page_no = int(item.metadata["page_no"])
        w, h = int(item.metadata["width"]), int(item.metadata["height"])
        for path, (pg, true_box) in shot.boxes.items():
            if pg != page_no:
                continue
            got = item.boxes[path]
            if got is not None:
                # a box under 12 px high moves 10% with a third of a pixel of registration error
                high = (true_box[3] - true_box[1]) * h >= 12
                assert _iou(got, true_box) >= (tall if high else short), path
                err = _edge_px(got, true_box, w, h)
                assert err <= max_px, (path, err)
                worst = max(worst, err)
    for r in res.refused:
        assert r.reason in intake.REASONS
    return worst


def _multi() -> render.RenderedDocument:
    for doc_type, seed in product(("lab_report", "discharge_summary", "clinic_note"), range(5, 14)):
        for layout in layouts.printable(doc_type, seed, purpose="train"):
            doc = render.render(
                _doc(doc_type),
                layout,
                DPI,
                page_id_for=lambda n, t=doc_type, i=layout.index, seed=seed: render.make_page_id(
                    f"multi/{t}/{seed}/{i}/{n}"
                ),
            )
            if len(doc.pages) >= 2:
                return doc
    raise AssertionError("no multi-page printable document")


MULTI = _multi()
MULTI_MANIFEST = intake.build_manifest(MULTI)


@pytest.mark.parametrize("deg", [-5.0, -3.0, 3.0, 5.0])
def test_scan_skew_tolerance(tmp_path: Path, deg: float) -> None:
    prof = replace(SCAN, name="skewed", skew_deg=(deg, deg))
    shot = degrade.degrade_document(PRINTED, 21, prof, "scan", source_dpi=SOURCE)
    for n, page in enumerate(shot.pages, start=1):
        _put(tmp_path, "real_scan", f"s{n}.png", page)
    res = intake.ingest(tmp_path, MANIFEST)
    assert [r.reason for r in res.refused] == []
    assert len(res.accepted) == len(shot.pages)
    _check_all(res, shot)


def _without_corner(corner: int) -> render.RenderedDocument:
    """``PRINTED`` with one corner fiducial painted out (as if the sheet's corner were lost)."""
    im = Image.open(io.BytesIO(PRINTED.pages[0])).convert("RGB")
    side, corners = render.fiducial_geometry(*im.size)
    x, y = corners[corner]
    ImageDraw.Draw(im).rectangle((x - 2, y - 2, x + side + 2, y + side + 2), fill=(255, 255, 255))
    buf = io.BytesIO()
    im.save(buf, format="PNG")
    return replace(PRINTED, pages=[buf.getvalue(), *PRINTED.pages[1:]])


@pytest.mark.parametrize("corner", range(4))
def test_scan_with_one_corner_lost(tmp_path: Path, corner: int) -> None:
    shot = degrade.degrade_document(_without_corner(corner), 31, SCAN, "scan", source_dpi=SOURCE)
    _put(tmp_path, "real_scan", "s1.png", shot.pages[0])
    res = intake.ingest(tmp_path, MANIFEST)
    assert [r.reason for r in res.refused] == []
    assert [i.metadata["page_id"] for i in res.accepted] == [PRINTED.page_ids[0]]
    assert _check_all(res, shot, 0.9, 0.8, 1.5) <= 1.5


def test_phone_photo_needs_all_four_marks(tmp_path: Path) -> None:
    lost = _without_corner(1)
    scan = degrade.degrade_document(lost, 32, SCAN, "scan", source_dpi=SOURCE)
    photo = degrade.degrade_document(lost, 32, PHOTO, "photo", source_dpi=SOURCE)
    _put(tmp_path / "a", "real_scan", "s1.png", scan.pages[0])
    _put(tmp_path / "b", "phone_photo", "s1.png", photo.pages[0])
    got = intake.ingest(tmp_path / "a", MANIFEST)
    assert [r.reason for r in got.refused] == [] and len(got.accepted) == 1
    _check_all(got, scan)
    refused = intake.ingest(tmp_path / "b", MANIFEST)
    assert refused.accepted == []
    assert [r.reason for r in refused.refused] == ["fiducials_not_found"]


def test_two_maps_reading_one_id_are_ambiguous(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # a strip reader that answers the same id for every way of lining the marks up
    _put(tmp_path, "real_scan", "s1.png", _photo(PRINTED, 33, "scan").pages[0])
    monkeypatch.setattr(intake, "_read_strip", lambda *_a, **_k: PRINTED.page_ids[0])
    res = intake.ingest(tmp_path, MANIFEST)
    assert res.accepted == []
    assert [r.reason for r in res.refused] == ["page_id_ambiguous"]


def test_a_sheared_three_mark_map_is_not_a_scan() -> None:
    ok = np.array([[1.0, 0.02, 5], [-0.02, 1.0, 7], [0, 0, 1]])
    sheared = np.array([[1.0, 0.4, 5], [0.0, 1.0, 7], [0, 0, 1]])
    stretched = np.array([[1.0, 0.0, 5], [0.0, 1.3, 7], [0, 0, 1]])
    mirrored = np.array([[-1.0, 0.0, 5], [0.0, 1.0, 7], [0, 0, 1]])
    assert intake._similar(ok)
    assert not (intake._similar(sheared) or intake._similar(stretched) or intake._similar(mirrored))


def test_a_pipe_named_like_an_image_does_not_hang(tmp_path: Path) -> None:
    (tmp_path / "real_scan").mkdir()
    os.mkfifo(tmp_path / "real_scan" / "pipe.png")
    res = intake.ingest(tmp_path, MANIFEST)
    assert [r.reason for r in res.refused] == ["unexpected_entry"]


def test_write_failure_is_an_intake_error(tmp_path: Path) -> None:
    _put(tmp_path / "in", "phone_photo", "p1.png", _photo(PRINTED, 34).pages[0])
    res = intake.ingest(tmp_path / "in", MANIFEST)
    assert res.accepted
    (tmp_path / "afile").write_bytes(b"x")
    with pytest.raises(intake.IntakeError, match="write_failed"):
        intake.write_result(res, tmp_path / "afile" / "set")


HALF = replace(degrade.PROFILES["phone"], name="phone_half", crop_margin=(0.45, 0.5))


def _group(
    tmp_path: Path,
    profile: degrade.Profile | str,
    mode: str,
    folder: str,
    seeds: range,
    max_px: float,
) -> tuple[int, int, float]:
    """Degrade ``MULTI`` for each seed, ingest, check; return accepted, total, worst edge."""
    accepted = total = 0
    worst = 0.0
    for seed in seeds:
        root = tmp_path / f"g{seed}"
        shot = degrade.degrade_document(MULTI, seed, profile, mode, source_dpi=SOURCE)  # type: ignore[arg-type]
        for n, page in enumerate(shot.pages, start=1):
            _put(root, folder, f"h{n}.png", page)
        res = intake.ingest(root, MULTI_MANIFEST)
        worst = max(worst, _check_all(res, shot, 0.85, 0.7, max_px))
        accepted += len(res.accepted)
        total += len(shot.pages)
    return accepted, total, worst


def test_stock_phone_profile_page_fills_half_the_frame(tmp_path: Path) -> None:
    accepted, total, _worst = _group(tmp_path, HALF, "photo", "phone_photo", range(8), PHONE_PX)
    assert accepted >= PHONE_MIN * total, (accepted, total)


def test_stock_poor_profile_scans(tmp_path: Path) -> None:
    accepted, total, _worst = _group(tmp_path, "poor", "scan", "real_scan", range(8), POOR_PX)
    assert accepted >= POOR_MIN * total, (accepted, total)


# measured on 8 seeds x 2 pages: 14 of 16 read in both groups; worst edge 0.63 px (half-frame
# photo) and 0.18 px (poor scan)
PHONE_PX = 1.0
POOR_PX = 0.5
PHONE_MIN = 0.75
POOR_MIN = 0.75


def test_pixels_outside_the_page_are_blanked(tmp_path: Path) -> None:
    shot = degrade.degrade_document(PRINTED, 23, HALF, "photo", source_dpi=SOURCE)
    _put(tmp_path, "phone_photo", "h1.png", shot.pages[0])
    res = intake.ingest(tmp_path, MANIFEST)
    assert res.accepted, [r.reason for r in res.refused]
    out = np.asarray(Image.open(io.BytesIO(res.accepted[0].image)).convert("L"))
    src = np.asarray(Image.open(io.BytesIO(shot.pages[0])).convert("L"))
    assert out.shape == src.shape
    h, w = out.shape
    # the photographed surround is dark (25-90 grey); after intake the frame is white
    ring = np.ones_like(out, bool)
    ring[int(h * 0.15) : int(h * 0.85), int(w * 0.15) : int(w * 0.85)] = False
    assert src[ring].mean() < 200 and (out[ring] == 255).mean() > 0.97
    # the page's own pixels are untouched near the centre
    mid = (slice(int(h * 0.4), int(h * 0.6)), slice(int(w * 0.4), int(w * 0.6)))
    assert (out[mid] == src[mid]).mean() > 0.97


def test_refusal_labels_never_echo_a_folder_name(tmp_path: Path) -> None:
    shot = _photo(PRINTED, 12)
    _put(tmp_path, "Ada_Obi_Lagos", "a.png", shot.pages[0])
    _put(tmp_path, "Ada_Obi_Lagos", "b.png", shot.pages[0])
    (tmp_path / "Ada Obi.png").write_bytes(shot.pages[0])
    res = intake.ingest(tmp_path, MANIFEST)
    assert res.accepted == []
    # the unknown folder is one refusal; it is not opened
    assert sorted(r.reason for r in res.refused) == ["slice_unknown", "slice_unknown"]
    assert all(r.label.startswith("other[") for r in res.refused)
    assert all("Ada" not in r.label and "Obi" not in r.label for r in res.refused)


def test_listing_limit_counts_both_levels(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(intake, "MAX_FILES", 5)
    for n in range(3):
        _put(tmp_path, "real_scan", f"a{n}.png", b"x")
        _put(tmp_path, "phone_photo", f"b{n}.png", b"x")
    with pytest.raises(intake.IntakeError, match="too_many_files"):
        intake.ingest(tmp_path, MANIFEST)
    monkeypatch.setattr(intake, "MAX_FILES", 8)
    assert len(intake.ingest(tmp_path, MANIFEST).refused) == 6


def test_write_result_is_atomic_and_duplicate_safe(tmp_path: Path) -> None:
    shot = _photo(PRINTED, 13)
    _put(tmp_path / "in", "phone_photo", "p1.png", shot.pages[0])
    res = intake.ingest(tmp_path / "in", MANIFEST)
    out = tmp_path / "out" / "set"
    ids = intake.write_result(res, out)
    assert sorted(p.name for p in out.iterdir()) == sorted(
        f"{ids[0]}{e}" for e in (".png", ".json", ".meta.json")
    )
    # a second write of the same page is refused and changes nothing
    before = {p.name: p.read_bytes() for p in out.iterdir()}
    with pytest.raises(intake.IntakeError, match="duplicate_page"):
        intake.write_result(res, out)
    assert {p.name: p.read_bytes() for p in out.iterdir()} == before
    # no temporary folder is left next to the output
    assert [p.name for p in out.parent.iterdir()] == ["set"]
    # a page id that is not well formed is refused
    bad = intake.IngestResult(
        accepted=[
            replace(res.accepted[0], metadata={**res.accepted[0].metadata, "page_id": "../x"})
        ]
    )
    with pytest.raises(intake.IntakeError, match="bad_page_id"):
        intake.write_result(bad, tmp_path / "other")
    # a scanner failure leaves nothing behind
    tainted = replace(
        res.accepted[0], ground_truth={"schema_version": "1", "x": "LUTH/2019/123456"}
    )
    with pytest.raises(intake.IntakeError):
        intake.write_result(intake.IngestResult(accepted=[tainted]), tmp_path / "third")
    assert not (tmp_path / "third").exists() or not list((tmp_path / "third").iterdir())


def test_merge_manifests_refuses_duplicates() -> None:
    other = intake.build_manifest(_printed(tag="m"))
    merged = intake.merge_manifests(MANIFEST, other)
    assert set(merged) == set(MANIFEST) | set(other)
    with pytest.raises(intake.IntakeError, match="duplicate_page"):
        intake.merge_manifests(MANIFEST, MANIFEST)


def test_unexpected_error_is_a_fixed_refusal(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _put(tmp_path, "real_scan", "a.png", _photo(PRINTED, 14, "scan").pages[0])

    def boom(*_a: object, **_k: object) -> None:
        raise RuntimeError("Ada Obi 08031234567")

    monkeypatch.setattr(intake, "_register", boom)
    res = intake.ingest(tmp_path, MANIFEST)
    assert [r.reason for r in res.refused] == ["registration_failed"]
    assert all(r.reason in intake.REASONS for r in res.refused)


def test_rgba_is_composited_on_white(tmp_path: Path) -> None:
    src = Image.open(io.BytesIO(_photo(PRINTED, 15, "scan").pages[0])).convert("RGBA")
    buf = io.BytesIO()
    src.save(buf, format="PNG")
    _put(tmp_path, "real_scan", "a.png", buf.getvalue())
    res = intake.ingest(tmp_path, MANIFEST)
    assert len(res.accepted) == 1
