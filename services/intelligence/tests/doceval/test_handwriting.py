import copy
import io
import random
from typing import Any

import numpy as np
import pytest
from PIL import Image

from intelligence.doceval import schema
from intelligence.doceval.synth import handwriting, layouts, patients, render, scanner

DPI = 150


def _docs() -> dict[str, dict[str, Any]]:
    found: dict[str, dict[str, Any]] = {}
    for p in patients.generate(random.Random(11), 200):
        for d in p.documents:
            found.setdefault(d["document"]["type"]["value"], copy.deepcopy(d))
    return found


DOCS = _docs()


def _layout(doc_type: str) -> layouts.LayoutSpec:
    return layouts.for_type(doc_type, 5, purpose="train")[0]


def _hand(doc_type: str, seed: int, mode: handwriting.Mode) -> render.RenderedDocument:
    return handwriting.handwritten_document(
        DOCS[doc_type], _layout(doc_type), random.Random(seed), mode=mode, dpi=DPI
    )


def _arr(data: bytes) -> np.ndarray:
    return np.asarray(Image.open(io.BytesIO(data)).convert("RGB")).astype(int)


@pytest.mark.parametrize("doc_type", ["vitals_chart", "medication_chart", "clinic_note"])
def test_handwritten_fields_marked_script(doc_type: str) -> None:
    out = _hand(doc_type, 1, "handwritten")
    plain = render.render(DOCS[doc_type], _layout(doc_type), DPI)
    fields = dict(schema.iter_fields(out.ground_truth))
    hand = [p for p, f in fields.items() if f.get("script") == "handwritten"]
    assert hand
    prefix = (
        ("notes[", "encounter.clinician")
        if doc_type == "clinic_note"
        else ("observations[", "medications[")
    )
    assert all(p.startswith(prefix) for p in hand)
    assert fields["patient.mrn"]["script"] == "printed"
    for p in hand:
        page, box = out.boxes[p]
        opage, obox = plain.boxes[p]
        assert page == opage and fields[p]["bbox"] == list(box)
        tol = 2 / 800  # box edges are rounded to whole pixels
        assert obox[0] - tol <= box[0] and obox[1] - tol <= box[1]
        assert box[2] <= obox[2] + tol and box[3] <= obox[3] + tol
    # printed header is untouched: same pixels as the plain render outside the hand boxes
    a, b = _arr(out.pages[0]), _arr(plain.pages[0])
    keep = np.ones(a.shape[:2], bool)
    for p in hand:
        if out.boxes[p][0] == 1:
            x0, y0, x1, y1 = plain.boxes[p][1]
            h, w = keep.shape
            keep[int(y0 * h) - 2 : int(y1 * h) + 3, int(x0 * w) - 2 : int(x1 * w) + 3] = False
    assert (a[keep] == b[keep]).all()
    assert (a != b).any()
    assert scanner.check_document(out.ground_truth) == []


def test_mixed_form_has_both_scripts() -> None:
    out = _hand("lab_report", 3, "mixed")
    scripts = {f.get("script") for _, f in schema.iter_fields(out.ground_truth)}
    assert {"printed", "handwritten", "mixed"} <= scripts
    fields = dict(schema.iter_fields(out.ground_truth))
    assert fields["encounter.clinician_text"]["script"] == "mixed"
    assert fields["patient.family_name"]["script"] == "handwritten"


def test_handwriting_not_allowed_for_other_types() -> None:
    with pytest.raises(handwriting.HandwritingError):
        _hand("lab_report", 1, "handwritten")


def test_handwriting_varies_by_seed_not_by_call() -> None:
    a1, a2, b = (
        _hand("vitals_chart", 1, "handwritten"),
        _hand("vitals_chart", 1, "handwritten"),
        _hand("vitals_chart", 2, "handwritten"),
    )
    assert a1.pages == a2.pages and a1.boxes == a2.boxes
    assert a1.pages != b.pages


def test_input_not_mutated() -> None:
    doc = copy.deepcopy(DOCS["vitals_chart"])
    handwriting.handwritten_document(
        doc, _layout("vitals_chart"), random.Random(1), mode="handwritten", dpi=DPI
    )
    assert doc == DOCS["vitals_chart"]


def test_render_field_fits_box_and_is_deterministic() -> None:
    style = handwriting.style_for(random.Random(4), 30)
    box = (10, 20, 210, 70)
    f1 = handwriting.render_field("Potassium 4.1", box, random.Random(9), style)
    f2 = handwriting.render_field("Potassium 4.1", box, random.Random(9), style)
    f3 = handwriting.render_field("Potassium 4.1", box, random.Random(10), style)
    assert f1.image.size == (200, 50) and f1.image.tobytes() == f2.image.tobytes()
    assert f1.image.tobytes() != f3.image.tobytes()
    x0, y0, x1, y1 = f1.ink
    assert 0 <= x0 < x1 <= 200 and 0 <= y0 < y1 <= 50
    long = "word " * 30
    wrapped = handwriting.render_field(long, (0, 0, 300, 150), random.Random(1), style)
    assert wrapped.ink[3] <= 150
    with pytest.raises(handwriting.HandwritingError):
        handwriting.render_field("x", (0, 0, 6, 6), random.Random(1), style)


def test_fonts_have_licences() -> None:
    names = {p.name for p in handwriting.FONT_DIR.iterdir()}
    for font in handwriting.HAND_FONTS.values():
        assert font in names
    for lic in ("LICENCE-Patrick.txt", "LICENCE-Kalam.txt", "LICENCE-IndieFlower.txt"):
        assert "SIL Open Font License" in (handwriting.FONT_DIR / lic).read_text()
