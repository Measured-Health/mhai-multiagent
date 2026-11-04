import copy
import io
import random
import struct
from dataclasses import replace
from typing import Any

import numpy as np
import pytest
from PIL import Image
from PIL.PngImagePlugin import PngInfo

from intelligence.doceval import schema
from intelligence.doceval.synth import degrade, layouts, patients, render

DPI = 100


def _doc() -> dict[str, Any]:
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            if d["document"]["type"]["value"] == "lab_report":
                return copy.deepcopy(d)
    raise AssertionError("no lab report")


DOC = _doc()
LAYOUT = layouts.for_type("lab_report", 5, purpose="train")[0]
PLAIN = render.render(copy.deepcopy(DOC), LAYOUT, DPI)

# geometry only: no blur, noise, folds, stamps, light, shadow or fax
GEOM_SCAN = replace(
    degrade.PROFILES["poor"],
    name="geom_scan",
    blur_px=(0, 0),
    noise_sigma=(0, 0),
    jpeg_quality=(95, 95),
    dpi=(100, 100),
    binarise_prob=0.0,
    folds=(0, 0),
    shadow=(0, 0),
    stamps=(0, 0),
    skew_deg=(4.0, 5.0),
)
GEOM_PHOTO = replace(
    degrade.PROFILES["phone"],
    name="geom_photo",
    blur_px=(0, 0),
    noise_sigma=(0, 0),
    jpeg_quality=(95, 95),
    folds=(0, 0),
    shadow=(0, 0),
    stamps=(0, 0),
    lighting=(0, 0),
    perspective=(0.03, 0.05),
    crop_margin=(0.02, 0.05),
    long_side_px=(1000, 1000),
)


def _mask(data: bytes) -> np.ndarray:
    a = np.asarray(Image.open(io.BytesIO(data)).convert("RGB")).astype(int)
    return (a[:, :, 0] > 110) & (a[:, :, 0] - np.maximum(a[:, :, 1], a[:, :, 2]) > 50)


def _paths() -> list[str]:
    fields = dict(schema.iter_fields(PLAIN.ground_truth))
    ps = [p for p, f in fields.items() if f["raw_text"] is not None and p in PLAIN.boxes]
    return [p for p in ps[:: max(1, len(ps) // 6)] if PLAIN.boxes[p][0] == 1]


def _chunks(png: bytes) -> list[bytes]:
    out, i = [], 8
    while i < len(png):
        (n,) = struct.unpack(">I", png[i : i + 4])
        out.append(png[i + 4 : i + 8])
        i += 12 + n
    return out


@pytest.mark.parametrize("fn,prof", [(degrade.scan, GEOM_SCAN), (degrade.photo, GEOM_PHOTO)])
@pytest.mark.parametrize("seed", [1, 2, 3])
def test_boxes_follow_skew_and_perspective(fn: Any, prof: degrade.Profile, seed: int) -> None:
    paths = _paths()
    assert paths
    for path in paths:
        hl = render.render_highlighted(copy.deepcopy(DOC), LAYOUT, path, DPI)
        page_no, _ = hl.boxes[path]
        assert page_no == 1
        res = fn(hl.pages[0], {path: hl.boxes[path][1]}, random.Random(seed), prof)
        box = res.boxes[path]
        assert box is not None
        assert all(0.0 <= v <= 1.0 for v in box)
        ys, xs = np.nonzero(_mask(res.page))
        assert len(xs) > 5
        h, w = _mask(res.page).shape
        x0, y0, x1, y1 = box
        assert xs.min() >= x0 * w - 2 and xs.max() <= x1 * w + 2, path
        assert ys.min() >= y0 * h - 2 and ys.max() <= y1 * h + 2, path
        # and the box is not a loose guess
        ink = (xs.max() - xs.min() + 1) * (ys.max() - ys.min() + 1)
        assert ink >= 0.3 * (x1 - x0) * w * (y1 - y0) * h, path
        assert res.metadata["unreadable"] == []


def test_boxes_follow_with_full_degradation() -> None:
    path = _paths()[1]
    hl = render.render_highlighted(copy.deepcopy(DOC), LAYOUT, path, DPI)
    for name, fn in (("poor", degrade.scan), ("phone", degrade.photo)):
        prof = replace(degrade.PROFILES[name], binarise_prob=0.0, stamps=(0, 0))
        for seed in range(4):
            res = fn(hl.pages[0], {path: hl.boxes[path][1]}, random.Random(seed), prof)
            box = res.boxes[path]
            assert box is not None
            ys, xs = np.nonzero(_mask(res.page))
            if len(xs) < 5:
                continue
            h, w = _mask(res.page).shape
            assert xs.min() >= box[0] * w - 3 and xs.max() <= box[2] * w + 3
            assert ys.min() >= box[1] * h - 3 and ys.max() <= box[3] * h + 3


def test_parameters_within_ranges() -> None:
    page = PLAIN.pages[0]
    boxes = {p: b for p, (n, b) in PLAIN.boxes.items() if n == 1}
    for name, prof in degrade.PROFILES.items():
        r = prof.ranges()
        for fn, keys in (
            (
                degrade.scan,
                ("skew_deg", "blur_px", "noise_sigma", "jpeg_quality", "folds", "shadow", "stamps"),
            ),
            (
                degrade.photo,
                (
                    "skew_deg",
                    "blur_px",
                    "noise_sigma",
                    "jpeg_quality",
                    "folds",
                    "shadow",
                    "stamps",
                    "perspective",
                    "lighting",
                    "crop_margin",
                    "long_side_px",
                ),
            ),
        ):
            for seed in range(6):
                meta = fn(page, boxes, random.Random(seed), name).metadata
                assert meta["profile"] == name
                for k in keys:
                    lo, hi = r[k]
                    assert lo <= meta["params"][k] <= hi, (name, k)
                if fn is degrade.scan:
                    dpi = meta["params"]["dpi"]
                    # the effective dpi is the drawn one up to pixel rounding
                    assert r["dpi"][0] - 1 <= dpi <= r["dpi"][1] + 1
                    assert isinstance(meta["params"]["binarise"], bool)
    assert set(degrade.PROFILES) == {"clean", "poor", "phone"}
    with pytest.raises(degrade.DegradeError):
        degrade.scan(page, boxes, random.Random(0), "nope")


def test_deterministic_for_seed() -> None:
    page = PLAIN.pages[0]
    boxes = {p: b for p, (n, b) in PLAIN.boxes.items() if n == 1}
    for fn in (degrade.scan, degrade.photo):
        a = fn(page, boxes, random.Random(5), "poor")
        b = fn(page, boxes, random.Random(5), "poor")
        c = fn(page, boxes, random.Random(6), "poor")
        assert a.page == b.page and a.boxes == b.boxes and a.metadata == b.metadata
        assert a.page != c.page
    d1 = degrade.degrade_document(PLAIN, 9, "phone", "photo", source_dpi=DPI)
    d2 = degrade.degrade_document(PLAIN, 9, "phone", "photo", source_dpi=DPI)
    assert (
        d1.pages == d2.pages and d1.ground_truth == d2.ground_truth and d1.metadata == d2.metadata
    )


def test_output_is_png_pixels_only() -> None:
    info = PngInfo()
    info.add_text("Author", "Dr Nobody")
    buf = io.BytesIO()
    Image.open(io.BytesIO(PLAIN.pages[0])).save(buf, format="PNG", pnginfo=info, dpi=(200, 200))
    tagged = buf.getvalue()
    assert b"tEXt" in _chunks(tagged) and b"pHYs" in _chunks(tagged)
    for fn in (degrade.scan, degrade.photo):
        for name in degrade.PROFILES:
            out = fn(tagged, {}, random.Random(1), name).page
            assert out.startswith(b"\x89PNG\r\n\x1a\n")
            assert set(_chunks(out)) <= {b"IHDR", b"IDAT", b"IEND"}
            assert b"Dr Nobody" not in out
            im = Image.open(io.BytesIO(out))
            assert im.format == "PNG" and im.mode == "RGB" and not im.info


def test_inputs_not_changed_and_document_ground_truth_follows() -> None:
    before = copy.deepcopy(PLAIN.ground_truth)
    pages = list(PLAIN.pages)
    boxes = copy.deepcopy(PLAIN.boxes)
    out = degrade.degrade_document(PLAIN, 3, GEOM_SCAN, "scan", source_dpi=DPI)
    assert PLAIN.ground_truth == before and PLAIN.pages == pages and PLAIN.boxes == boxes
    assert out.ground_truth is not PLAIN.ground_truth
    assert len(out.pages) == len(PLAIN.pages)
    fields = dict(schema.iter_fields(out.ground_truth))
    for path, (pg, box) in out.boxes.items():
        assert fields[path]["page"] == pg and fields[path]["bbox"] == list(box)
    assert any(out.boxes[p][1] != PLAIN.boxes[p][1] for p in out.boxes)
    unreadable = {p for m in out.metadata["pages"] for p in m["unreadable"]}
    for path in unreadable:
        assert fields[path]["page"] is None and fields[path]["bbox"] is None
        assert path not in out.boxes
    assert len(out.boxes) + len(unreadable) == len(PLAIN.boxes)


def _blank(w: int = 400, h: int = 400) -> bytes:
    buf = io.BytesIO()
    Image.new("RGB", (w, h), (255, 255, 255)).save(buf, format="PNG")
    return buf.getvalue()


def test_field_pushed_out_of_frame_is_unreadable() -> None:
    crop = replace(
        degrade.PROFILES["phone"],
        name="crop",
        crop_margin=(-0.3, -0.3),
        perspective=(0, 0),
        skew_deg=(0, 0),
        stamps=(0, 0),
        folds=(0, 0),
    )
    boxes = {
        "edge": (0.0, 0.0, 0.1, 0.1),
        "middle": (0.4, 0.4, 0.6, 0.6),
        "half": (0.1, 0.4, 0.3, 0.5),
    }
    res = degrade.photo(_blank(), boxes, random.Random(1), crop)
    # 30% cropped each side: x in [0.3, 0.7] of the page is kept
    assert res.boxes["edge"] is None
    assert res.boxes["half"] is None  # x 0.1-0.3 is entirely outside
    mid = res.boxes["middle"]
    assert mid is not None and all(0 <= v <= 1 for v in mid)
    assert res.metadata["unreadable"] == ["edge", "half"]

    # a field mostly in frame is kept, clipped to the frame
    slight = replace(crop, name="slight", crop_margin=(-0.02, -0.02))
    res = degrade.photo(
        _blank(),
        {"near": (0.0, 0.3, 0.2, 0.4), "in": (0.3, 0.3, 0.5, 0.4)},
        random.Random(1),
        slight,
    )
    near = res.boxes["near"]
    assert near is not None and near[0] == 0.0 and 0 <= near[2] <= 1
    assert res.metadata["unreadable"] == []

    # at document level the ground truth is nulled, never left stale
    doc = degrade.degrade_document(PLAIN, 2, crop, "photo", source_dpi=DPI)
    lost = [p for m in doc.metadata["pages"] for p in m["unreadable"]]
    assert lost
    fields = dict(schema.iter_fields(doc.ground_truth))
    assert all(fields[p]["bbox"] is None and fields[p]["page"] is None for p in lost)


def test_stamps_over_text_are_recorded() -> None:
    prof = replace(degrade.PROFILES["poor"], stamps=(3, 3))
    boxes = {"a": (0.2, 0.2, 0.4, 0.25), "b": (0.6, 0.7, 0.8, 0.75)}
    res = degrade.scan(_blank(), boxes, random.Random(4), prof)
    assert res.metadata["params"]["stamps"] == 3
    assert set(res.metadata["occluded"]) <= {"a", "b"} and res.metadata["occluded"]


def test_limits_and_bad_input() -> None:
    page = _blank()
    rng = random.Random(0)
    with pytest.raises(degrade.DegradeError):
        degrade.scan(b"not a png", {}, rng, "clean")
    buf = io.BytesIO()
    Image.new("RGB", (6000, 5000)).save(buf, format="PNG")
    with pytest.raises(degrade.DegradeError):  # 30 million pixels: over the page budget
        degrade.scan(buf.getvalue(), {}, rng, "clean")
    for bad in (
        (0.5, 0.5, 0.4, 0.6),
        (-0.1, 0, 0.5, 0.5),
        (0, 0, 1.2, 0.5),
        (0, 0, float("nan"), 1),
    ):
        with pytest.raises(degrade.DegradeError):
            degrade.scan(page, {"x": bad}, rng, "clean")  # type: ignore[dict-item]
    with pytest.raises(degrade.DegradeError):
        degrade.scan(page, {}, rng, "clean", source_dpi=render.MAX_DPI + 1)
    wrong = render.RenderedDocument(
        pages=[page], boxes={"p": (2, (0.1, 0.1, 0.2, 0.2))}, ground_truth={}
    )
    with pytest.raises(degrade.DegradeError):
        degrade.degrade_document(wrong, 1, "clean", "scan")
    many = render.RenderedDocument(pages=[page] * (render.MAX_PAGES + 1), boxes={}, ground_truth={})
    with pytest.raises(degrade.DegradeError):
        degrade.degrade_document(many, 1, "clean", "scan")
    with pytest.raises(degrade.DegradeError):
        degrade.degrade_document(wrong, True, "clean", "scan")  # type: ignore[arg-type]
    # output stays inside the pixel budget even for a high dpi request
    out = degrade.scan(
        page, {}, rng, replace(degrade.PROFILES["clean"], dpi=(400, 400)), source_dpi=36
    )
    assert Image.open(io.BytesIO(out.page)).size[0] <= render.MAX_SIDE_PX
