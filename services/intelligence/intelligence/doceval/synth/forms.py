"""Printable pages for the people who fill in the real set (human item H5).

Two sheets per invented record:

* the **blank form**: the printed layout with every field's writing slot empty (a rule
  where the value would be), the record id printed at the top left, and the registration
  marks of ``render`` so a photograph of the filled form can be taken in by ``intake``;
* the **record sheet**: the same layout with the invented values printed, headed with the
  same record id. The volunteer copies the values from the sheet onto the form by hand.

The slots are the boxes the printed values had, so a field's box on the filled form is
where the volunteer was asked to write, not the ink of what they wrote. Both sheets are
synthetic (the scanner runs over the ground truth); a person still has to look at each
filled form, because handwriting is not machine-checked.
"""

import base64
import copy
import hashlib
import io
import re
from collections.abc import Callable
from typing import Any

from PIL import Image, ImageDraw

from intelligence.doceval import schema
from intelligence.doceval.synth import render
from intelligence.doceval.synth.layouts import LayoutSpec
from intelligence.doceval.synth.render import RenderedDocument

RECORD_PREFIX = "SYN-RC-"
_RECORD_RE = re.compile(r"SYN-RC-[A-Z2-7]{8}")
_DIGIT_RUN = re.compile(r"[0-9]{4,}")


def record_id_ok(record_id: object) -> bool:
    """``SYN-RC-`` and 8 base32 characters, no digit run over 3."""
    return (
        isinstance(record_id, str)
        and _RECORD_RE.fullmatch(record_id) is not None
        and _DIGIT_RUN.search(record_id) is None
    )


def make_record_id(material: str) -> str:
    """A deterministic record id from any string."""
    for attempt in range(1000):
        digest = hashlib.sha256(f"record/{material}/{attempt}".encode()).digest()[:5]
        record_id = RECORD_PREFIX + base64.b32encode(digest).decode("ascii")
        if record_id_ok(record_id):
            return record_id
    raise render.RenderError("record_id")  # pragma: no cover


def _encode(img: Image.Image, dpi: int) -> bytes:
    buf = io.BytesIO()
    img.save(buf, format="PNG", compress_level=6, dpi=(dpi, dpi))
    return buf.getvalue()


def _check(record_id: str, layout: LayoutSpec) -> None:
    if not record_id_ok(record_id):
        raise render.RenderError("bad_record_id")
    if layout.margin_mm < render.MIN_MARK_MARGIN_MM:
        raise render.RenderError("margin_too_small_for_marks")


def record_sheet(
    document_gt: dict[str, Any], layout: LayoutSpec, record_id: str, dpi: int = 200
) -> RenderedDocument:
    """The invented record, printed and headed with ``record_id``; no registration marks."""
    _check(record_id, layout)
    base = render.render(document_gt, layout, dpi)
    pages = []
    for data in base.pages:
        with Image.open(io.BytesIO(data)) as src:
            img = src.convert("RGB")
        render.draw_record_id(img, record_id)
        pages.append(_encode(img, dpi))
    return RenderedDocument(pages=pages, boxes=base.boxes, ground_truth=base.ground_truth)


def blank_form(
    document_gt: dict[str, Any],
    layout: LayoutSpec,
    record_id: str,
    page_id_for: Callable[[int], str],
    dpi: int = 200,
) -> RenderedDocument:
    """The layout with every value removed (a rule marks each slot), the record id and the
    registration marks drawn. ``boxes`` are the slots; the ground truth is the record the
    volunteer copies, with each slot's field marked ``handwritten``."""
    _check(record_id, layout)
    base = render.render(document_gt, layout, dpi)
    ids = tuple(page_id_for(n) for n in range(1, len(base.pages) + 1))
    if len(set(ids)) != len(ids):
        raise render.RenderError("duplicate_page_id")
    images = []
    for data in base.pages:
        with Image.open(io.BytesIO(data)) as src:
            images.append(src.convert("RGB"))
    for page_no, box in sorted(set(base.boxes.values())):
        img = images[page_no - 1]
        w, h = img.size
        x0, y0 = max(0, int(box[0] * w)), max(0, int(box[1] * h))
        x1, y1 = min(w, int(box[2] * w) + 1), min(h, int(box[3] * h) + 1)
        d = ImageDraw.Draw(img)
        d.rectangle((x0, y0, x1 - 1, y1 - 1), fill=img.getpixel((x0, y0)))
        d.line((x0, y1 - 1, x1 - 1, y1 - 1), fill=render.RULE, width=1)
    pages = []
    for img, page_id in zip(images, ids, strict=True):
        render.draw_record_id(img, record_id)
        render.draw_registration(img, page_id)
        pages.append(_encode(img, dpi))
    gt = copy.deepcopy(base.ground_truth)
    for path, fld in schema.iter_fields(gt):
        if path in base.boxes:
            fld["script"] = "handwritten"
    return RenderedDocument(pages=pages, boxes=base.boxes, ground_truth=gt, page_ids=ids)
