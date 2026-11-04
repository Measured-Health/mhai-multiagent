import copy
import io
import random
from pathlib import Path
from typing import Any

import numpy as np
import pytest
from PIL import Image

from intelligence.doceval import schema
from intelligence.doceval.synth import layouts, patients, render

DPI = 100


def _docs() -> dict[str, dict[str, Any]]:
    found: dict[str, dict[str, Any]] = {}
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            found.setdefault(d["document"]["type"]["value"], copy.deepcopy(d))
    assert set(found) == set(layouts.DOC_TYPES)
    return found


DOCS = _docs()


def _pick_layouts(doc_type: str) -> list[layouts.LayoutSpec]:
    ls = layouts.for_type(doc_type, 5)
    two = next(x for x in ls if x.columns == 2)
    paged = next(x for x in ls if x.new_page_per_section)
    return [ls[0], two, paged, ls[7]]


def _fields(doc: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return dict(schema.iter_fields(doc))


def _png(data: bytes) -> np.ndarray:
    return np.asarray(Image.open(io.BytesIO(data)).convert("RGB")).astype(int)


def _red_mask(a: np.ndarray) -> np.ndarray:
    return (a[:, :, 0] > 120) & (a[:, :, 0] - np.maximum(a[:, :, 1], a[:, :, 2]) > 60)


def test_fifteen_layouts_per_type() -> None:
    for doc_type in layouts.DOC_TYPES:
        ls = layouts.for_type(doc_type, 3)
        assert len(ls) >= 15
        assert len({x.signature() for x in ls}) == len(ls)
        assert ls == layouts.for_type(doc_type, 3)
        assert ls != layouts.for_type(doc_type, 4)
        assert {x.font for x in ls} == set(layouts.FONT_FAMILIES)
        assert {x.table_style for x in ls} == set(layouts.TABLE_STYLES)
        assert {x.columns for x in ls} == {1, 2}
        assert len({x.header_style for x in ls}) == len(layouts.HEADER_STYLES)
        assert len({x.logo_shape for x in ls}) > 1
        assert any(x.stamp for x in ls) and any(x.new_page_per_section for x in ls)
    with pytest.raises(ValueError):
        layouts.for_type("no_such_type", 3)


def test_train_refuses_eval_template_seed() -> None:
    assert not set(layouts.TRAIN_TEMPLATE_SEEDS) & set(layouts.EVAL_TEMPLATE_SEEDS)
    eval_seed = layouts.EVAL_TEMPLATE_SEEDS[0]
    train_seed = layouts.TRAIN_TEMPLATE_SEEDS[-1]
    with pytest.raises(layouts.LayoutSeedError):
        layouts.for_type("lab_report", eval_seed, purpose="train")
    for purpose in ("dev", "test"):
        with pytest.raises(layouts.LayoutSeedError):
            layouts.for_type("lab_report", train_seed, purpose=purpose)  # type: ignore[arg-type]
    for bad in (-1, 1_100_000, True):
        with pytest.raises(layouts.LayoutSeedError):
            layouts.for_type("lab_report", bad)
    assert layouts.for_type("lab_report", train_seed, purpose="train")
    assert layouts.for_type("lab_report", eval_seed, purpose="test")
    assert layouts.for_type("lab_report", eval_seed, purpose="dev")


def test_boxes_in_unit_square() -> None:
    for doc_type, doc in DOCS.items():
        for layout in _pick_layouts(doc_type):
            gt = copy.deepcopy(doc)
            out = render.render(gt, layout, DPI)
            assert 1 <= len(out.pages) <= render.MAX_PAGES
            for path, fld in _fields(gt).items():
                if fld["raw_text"] is None:
                    assert fld["page"] is None and fld["bbox"] is None
                    continue
                page, box = out.boxes[path]
                x0, y0, x1, y1 = box
                assert 0 <= x0 < x1 <= 1 and 0 <= y0 < y1 <= 1, path
                assert fld["page"] == page and fld["bbox"] == list(box)
                assert 1 <= page <= len(out.pages)
            schema.validate_model_output(gt)


def test_boxes_cover_rendered_text() -> None:
    for doc_type, doc in DOCS.items():
        for layout in _pick_layouts(doc_type):
            gt = copy.deepcopy(doc)
            plain = render.render(gt, layout, DPI)
            assert not any(_red_mask(_png(p)).any() for p in plain.pages), "furniture is red"
            paths = [p for p, f in _fields(gt).items() if f["raw_text"] is not None]
            sample = paths[:: max(1, len(paths) // 9)]
            for path in sample:
                out = render.render_highlighted(copy.deepcopy(doc), layout, path, DPI)
                page, (x0, y0, x1, y1) = out.boxes[path]
                assert out.boxes[path] == plain.boxes[path]
                ink_pages = []
                for n, data in enumerate(out.pages, start=1):
                    mask = _red_mask(_png(data))
                    if mask.any():
                        ink_pages.append(n)
                        h, w = mask.shape
                        ys, xs = np.nonzero(mask)
                        # all of the field's ink is inside its box (1 px slack) ...
                        assert xs.min() >= x0 * w - 1 and xs.max() <= x1 * w + 1, path
                        assert ys.min() >= y0 * h - 1 and ys.max() <= y1 * h + 1, path
                        # ... and the box is not a loose guess
                        ink = (xs.max() - xs.min() + 1) * (ys.max() - ys.min() + 1)
                        assert ink >= 0.4 * (x1 - x0) * w * (y1 - y0) * h, path
                assert ink_pages == [page], path
                # the box has ink in the plain render too
                a = _png(plain.pages[page - 1])
                h, w = a.shape[:2]
                region = a[int(y0 * h) : int(y1 * h) + 1, int(x0 * w) : int(x1 * w) + 1]
                assert (region.sum(axis=2) < 600).any(), path


def test_every_nonnull_field_has_a_box() -> None:
    for doc_type, doc in DOCS.items():
        for layout in layouts.for_type(doc_type, 9):
            gt = copy.deepcopy(doc)
            out = render.render(gt, layout, DPI)
            for path, fld in _fields(gt).items():
                assert (fld["raw_text"] is not None) == (path in out.boxes), path


def test_multi_page_report_page_numbers() -> None:
    base = DOCS["lab_report"]
    gt = copy.deepcopy(base)
    gt["observations"] = [copy.deepcopy(base["observations"][0]) for _ in range(150)]
    layout = layouts.for_type("lab_report", 2)[0]
    out = render.render(gt, layout, DPI)
    assert len(out.pages) > 2
    pages = [gt["observations"][i]["value_text"]["page"] for i in range(150)]
    assert pages == sorted(pages)
    assert pages[0] == 1 and pages[-1] == len(out.pages)
    assert set(pages) == set(range(1, len(out.pages) + 1))
    for i in (0, 70, 149):
        path = f"observations[{i}].name_text"
        hl = render.render_highlighted(copy.deepcopy(gt), layout, path, DPI)
        red = [n for n, p in enumerate(hl.pages, 1) if _red_mask(_png(p)).any()]
        assert red == [hl.boxes[path][0]]
    # page numbers differ page to page (the footer reads "Page n of N")
    assert len({p for p in out.pages}) == len(out.pages)


def test_render_deterministic() -> None:
    doc = DOCS["discharge_summary"]
    for layout in layouts.for_type("discharge_summary", 1)[:4]:
        a = render.render(copy.deepcopy(doc), layout, DPI)
        b = render.render(copy.deepcopy(doc), layout, DPI)
        assert a.pages == b.pages and a.boxes == b.boxes
        assert all(p.startswith(b"\x89PNG") for p in a.pages)
        assert b"tIME" not in a.pages[0]
    other = render.render(copy.deepcopy(doc), layouts.for_type("discharge_summary", 1)[5], DPI)
    assert other.pages != a.pages


def test_page_limits() -> None:
    base = DOCS["lab_report"]
    layout = layouts.for_type("lab_report", 2)[0]
    with pytest.raises(render.RenderError):
        render.render(copy.deepcopy(base), layout, dpi=1000)
    with pytest.raises(render.RenderError):
        render.render(copy.deepcopy(base), layout, dpi=10)
    gt = copy.deepcopy(base)
    gt["observations"] = [copy.deepcopy(base["observations"][0]) for _ in range(1500)]
    with pytest.raises(render.RenderError) as err:
        render.render(gt, layout, dpi=50)
    assert str(err.value) == "too_many_pages"
    ok = render.render(copy.deepcopy(base), layout, dpi=300)
    for data in ok.pages:
        assert max(Image.open(io.BytesIO(data)).size) <= render.MAX_SIDE_PX


def test_fonts_have_licence_files() -> None:
    fonts = Path(render.__file__).parent / "fonts"
    ttf = sorted(fonts.glob("*.ttf"))
    assert ttf
    for f in ttf:
        family = "DejaVu" if f.name.startswith("DejaVu") else "Liberation"
        licence = fonts / f"LICENCE-{family}.txt"
        assert licence.is_file(), f.name
        text = licence.read_text(encoding="utf-8")
        assert ("Bitstream" in text) if family == "DejaVu" else ("SIL OPEN FONT LICENSE" in text)
    for family_files in render._FONT_FILES.values():
        for name in family_files:
            assert (fonts / name).is_file()
