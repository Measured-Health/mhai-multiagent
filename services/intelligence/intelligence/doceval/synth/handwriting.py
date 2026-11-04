"""Handwriting-like text for synthetic pages: a bundled handwriting font, then noise.

``render_field(text, box, rng, style)`` draws one field glyph by glyph: every glyph gets
its own rotation, size, vertical jitter and pen pressure; the baseline wobbles and
drifts; letter spacing is noisy; the whole stroke can be thickened. The pen colour is a
dark blue or black. All randomness comes from the ``random.Random`` the caller supplies,
so a seed gives the same pixels, and nothing passed in is changed.

``handwritten_document`` puts that on a rendered page: it renders the document as
usual, then replaces chosen fields with handwriting inside the same box (the printed
ink is wiped first) and records the new, tighter box and the field's ``script``:
``handwritten``, or ``mixed`` when only part of the field is handwritten (the printed
title stays, the name after it is written). Handwriting is for vitals charts,
medication charts and clinic notes, and for the handwritten entries of a mixed form.

Fonts are the bundled OFL fonts (Patrick Hand, Kalam, Indie Flower); see
``fonts/README.txt``.
"""

import io
import math
import random
import re
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any, Literal

import numpy as np
from PIL import Image, ImageChops, ImageDraw, ImageFilter, ImageFont

from intelligence.doceval import schema
from intelligence.doceval.synth.layouts import LayoutSpec
from intelligence.doceval.synth.render import Box, RenderedDocument, render

FONT_DIR = Path(__file__).parent / "fonts"
HAND_FONTS = {
    "patrick-hand": "PatrickHand-Regular.ttf",
    "kalam": "Kalam-Regular.ttf",
    "indie-flower": "IndieFlower-Regular.ttf",
}
# Dark blues and blacks only: nothing reddish (tests find a field by pure red ink).
INK_COLOURS = (
    (22, 22, 60),
    (18, 30, 95),
    (12, 12, 12),
    (38, 38, 110),
    (28, 28, 34),
)
MIN_GLYPH_PX = 8
HANDWRITABLE_TYPES = frozenset({"vitals_chart", "medication_chart", "clinic_note"})
Mode = Literal["handwritten", "mixed"]
PixelBox = tuple[int, int, int, int]


class HandwritingError(ValueError):
    """The text cannot be drawn in the box or the mode is wrong. Carries a code only."""


@dataclass(frozen=True)
class HandStyle:
    """One writer's hand: font, size, slant, wobble, pressure and pen."""

    font: str
    size_px: int  # nominal glyph size; a field shrinks it to fit its box
    slant_deg: float  # forward lean of the strokes
    rotation_sigma_deg: float  # per-glyph rotation noise
    size_sigma: float  # per-glyph size noise (fraction)
    jitter: float  # per-glyph vertical jitter (fraction of size)
    wobble: float  # baseline wobble amplitude (fraction of size)
    drift: float  # baseline slope across a line (fraction of the line's width)
    spacing_sigma: float  # letter-spacing noise (fraction of the advance)
    tracking: float  # extra spacing (fraction of the size)
    thick: bool  # thicken the stroke
    ink: tuple[int, int, int]

    def __post_init__(self) -> None:
        if self.font not in HAND_FONTS:
            raise HandwritingError("unknown_font")
        if self.size_px < MIN_GLYPH_PX:
            raise HandwritingError("size_too_small")
        if self.ink not in INK_COLOURS:
            raise HandwritingError("bad_ink")


def style_for(rng: random.Random, size_px: int) -> HandStyle:
    """Draw a writer. ``size_px`` is the nominal glyph size (about the printed body size)."""
    names = tuple(HAND_FONTS)
    return HandStyle(
        font=names[rng.randrange(len(names))],
        size_px=max(MIN_GLYPH_PX, size_px),
        slant_deg=round(rng.uniform(-3.0, 14.0), 3),
        rotation_sigma_deg=round(rng.uniform(1.5, 4.5), 3),
        size_sigma=round(rng.uniform(0.02, 0.07), 4),
        jitter=round(rng.uniform(0.01, 0.05), 4),
        wobble=round(rng.uniform(0.01, 0.06), 4),
        drift=round(rng.uniform(-0.025, 0.025), 4),
        spacing_sigma=round(rng.uniform(0.04, 0.16), 4),
        tracking=round(rng.uniform(-0.02, 0.08), 4),
        thick=rng.random() < 0.5,
        ink=INK_COLOURS[rng.randrange(len(INK_COLOURS))],
    )


@lru_cache(maxsize=96)
def _font(name: str, px: int) -> ImageFont.FreeTypeFont:
    return ImageFont.truetype(
        str(FONT_DIR / HAND_FONTS[name]), px, layout_engine=ImageFont.Layout.BASIC
    )


@dataclass(frozen=True)
class HandField:
    """Handwriting for one field: an RGBA layer the size of the box, and its ink box."""

    image: Image.Image
    ink: PixelBox  # tight box of the ink inside ``image``, in layer pixels


def _lighten(dst: Image.Image, src: Image.Image, x: int, y: int) -> None:
    """Paste ``src`` onto ``dst`` at (x, y), keeping the darker ink at each pixel (max alpha)."""
    left, top = max(0, x), max(0, y)
    right, bottom = min(dst.width, x + src.width), min(dst.height, y + src.height)
    if right <= left or bottom <= top:
        return
    part = src.crop((left - x, top - y, right - x, bottom - y))
    region = dst.crop((left, top, right, bottom))
    dst.paste(ImageChops.lighter(region, part), (left, top))


def _wrap(text: str, width: float, font: ImageFont.FreeTypeFont, extra: float) -> list[str]:
    lines: list[str] = []
    for para in text.split("\n"):
        cur = ""
        for word in para.split(" "):
            trial = word if not cur else cur + " " + word
            if cur and font.getlength(trial) * extra > width:
                lines.append(cur)
                cur = word
            else:
                cur = trial
        lines.append(cur)
    return lines


def _glyph(
    ch: str,
    px: int,
    style: HandStyle,
    rng: random.Random,
) -> tuple[Image.Image, int]:
    """One glyph as an 'L' mask on a padded canvas, and the y of its baseline in the mask."""
    font = _font(style.font, px)
    asc, desc = font.getmetrics()
    pad = max(2, math.ceil(0.35 * px))
    w = math.ceil(font.getlength(ch)) + 2 * pad
    h = asc + desc + 2 * pad
    mask = Image.new("L", (w, h), 0)
    ImageDraw.Draw(mask).text((pad, pad), ch, font=font, fill=255, anchor="la")
    base = pad + asc
    shear = math.tan(math.radians(style.slant_deg))
    mask = mask.transform(
        mask.size,
        Image.Transform.AFFINE,
        (1.0, shear, -shear * base, 0.0, 1.0, 0.0),
        resample=Image.Resampling.BICUBIC,
    )
    angle = rng.gauss(0.0, style.rotation_sigma_deg)
    mask = mask.rotate(angle, resample=Image.Resampling.BICUBIC, center=(w / 2, base), fillcolor=0)
    pressure = rng.uniform(0.78, 1.0)
    if pressure < 0.999:
        mask = mask.point(lambda v: int(v * pressure))
    return mask, base


def render_field(text: str, box: PixelBox, rng: random.Random, style: HandStyle) -> HandField:
    """Draw ``text`` in handwriting inside ``box`` (page pixels ``x0, y0, x1, y1``).

    The text is shrunk (and wrapped at spaces, and at newlines) until it fits; a box
    too small for 8 px glyphs raises :class:`HandwritingError` (``box_too_small``).
    The ink never leaves the box.
    """
    x0, y0, x1, y1 = box
    bw, bh = x1 - x0, y1 - y0
    margin = 1
    avail_w, avail_h = bw - 2 * margin, bh - 2 * margin
    if not text.strip():
        raise HandwritingError("empty_text")
    if avail_w < MIN_GLYPH_PX or avail_h < MIN_GLYPH_PX - 2:
        raise HandwritingError("box_too_small")
    slack = 1.0 + max(0.0, style.tracking) + 3 * style.spacing_sigma  # spacing growth
    size = float(min(style.size_px, avail_h * 1.3))
    while True:
        px = max(MIN_GLYPH_PX, round(size))
        font = _font(style.font, px)
        asc, desc = font.getmetrics()
        lines = _wrap(text, avail_w, font, slack)
        line_h = (asc + desc) * 0.92
        amp = (style.wobble + style.jitter) * px
        # The box is the ink box of printed text, so fit the ink, not the font's line height.
        tops = [font.getbbox(x, anchor="ls")[1] for x in lines if x.strip()]
        bottoms = [font.getbbox(x, anchor="ls")[3] for x in lines if x.strip()]
        min_top = min(tops) if tops else -asc
        ink_h = line_h * (len(lines) - 1) + max(bottoms, default=desc) - min_top
        need_h = ink_h + 2 * amp
        wide = max(font.getlength(x) for x in lines) * slack
        if need_h <= avail_h and wide <= avail_w:
            break
        if px <= MIN_GLYPH_PX:
            raise HandwritingError("box_too_small")
        size = size * 0.92 if size * 0.92 >= MIN_GLYPH_PX else MIN_GLYPH_PX
    top = margin + amp + (avail_h - need_h) / 2

    mask = Image.new("L", (bw, bh), 0)
    for row, line in enumerate(lines):
        base_y = top - min_top + row * line_h
        x = float(margin)
        total = max(1.0, sum(font.getlength(c) for c in line) * (1 + style.tracking))
        slope = style.drift * total
        phase = rng.uniform(0.0, 2 * math.pi)
        period = rng.uniform(5.0, 11.0) * px
        for ch in line:
            adv = font.getlength(ch) * (1 + style.tracking)
            if ch != " ":
                gpx = max(MIN_GLYPH_PX, round(px * (1 + rng.gauss(0.0, style.size_sigma))))
                glyph, gbase = _glyph(ch, gpx, style, rng)
                wob = style.wobble * px * math.sin(2 * math.pi * (x - margin) / period + phase)
                dy = wob + slope * (x - margin) / total + rng.gauss(0.0, style.jitter * px)
                gx = round(x - (glyph.width - font.getlength(ch)) / 2)
                gy = round(base_y + dy - gbase)
                _lighten(mask, glyph, gx, gy)
            x += adv * (1 + rng.gauss(0.0, style.spacing_sigma))
    if style.thick:
        mask = mask.filter(ImageFilter.MaxFilter(3))
    solid = mask.point(lambda v: 255 if v > 24 else 0)
    ink = solid.getbbox()
    if ink is None:
        raise HandwritingError("no_ink")
    layer = Image.new("RGBA", (bw, bh), (*style.ink, 0))
    layer.putalpha(mask)
    return HandField(image=layer, ink=(ink[0], ink[1], ink[2], ink[3]))


# --- whole documents ---------------------------------------------------------------------

_SPACE = re.compile(r"\s+")
_NOTE_TEXT = re.compile(r"notes\[[0-9]+\]\.(text|heading)")
_TABLES = ("observations[", "medications[", "conditions[")


def _decode(page: bytes) -> Image.Image:
    return Image.open(io.BytesIO(page)).convert("RGB")


def _encode(img: Image.Image, dpi: int) -> bytes:
    buf = io.BytesIO()
    img.save(buf, format="PNG", optimize=False, compress_level=6, dpi=(dpi, dpi))
    return buf.getvalue()


def _to_px(box: Box, w: int, h: int) -> PixelBox:
    return (
        max(0, math.floor(box[0] * w + 1e-6)),
        max(0, math.floor(box[1] * h + 1e-6)),
        min(w, math.ceil(box[2] * w - 1e-6)),
        min(h, math.ceil(box[3] * h - 1e-6)),
    )


def _union(a: PixelBox, b: PixelBox) -> PixelBox:
    return (min(a[0], b[0]), min(a[1], b[1]), max(a[2], b[2]), max(a[3], b[3]))


def _frac(px: PixelBox, w: int, h: int) -> Box:
    return (
        round(px[0] / w, 6),
        round(px[1] / h, 6),
        round(min(1.0, px[2] / w), 6),
        round(min(1.0, px[3] / h), 6),
    )


def _targets(
    doc_type: str, mode: Mode, fields: dict[str, dict[str, Any]], rng: random.Random
) -> set[str]:
    """Field paths to write by hand. Chosen by type and mode; ``rng`` only picks cells."""
    texty = [p for p, f in fields.items() if isinstance(f.get("raw_text"), str)]
    if mode == "handwritten":
        if doc_type not in HANDWRITABLE_TYPES:
            raise HandwritingError("not_handwritable")
        if doc_type == "clinic_note":
            return {p for p in texty if _NOTE_TEXT.fullmatch(p) or p == "encounter.clinician_text"}
        return {p for p in texty if p.startswith(_TABLES)}
    chosen = {
        p
        for p in texty
        if p in ("patient.family_name", "patient.given_names", "encounter.clinician_text")
    }
    # One decision per cell, taken in path order, so a seed gives the same cells.
    for p in sorted(texty):
        if p.startswith(_TABLES) and rng.random() < 0.3:
            chosen.add(p)
    return chosen


def _split_column(region: Image.Image) -> int | None:
    """Local x where a printed title ends and the written part begins: the first gap
    between words (a run of blank columns at least 0.18 of the height wide)."""
    arr = np.asarray(region.convert("L"))
    ink = (arr < 170).any(axis=0)
    need = max(3, round(0.18 * arr.shape[0]))
    xs = np.flatnonzero(ink)
    if xs.size == 0:
        return None
    run, start = 0, int(xs[0])
    for x in range(start, arr.shape[1]):
        if ink[x]:
            if run >= need and x > start:
                return x
            run = 0
        else:
            run += 1
    return None


def handwritten_document(
    document_gt: dict[str, Any],
    layout: LayoutSpec,
    rng: random.Random,
    *,
    mode: Mode,
    dpi: int = 200,
) -> RenderedDocument:
    """Render ``document_gt`` with handwriting in place of some printed fields.

    ``mode="handwritten"`` is for vitals charts, medication charts and clinic notes (all
    table cells, or all note text); ``mode="mixed"`` works for every type: the patient's
    name, the clinician's name and about a third of the table cells are written by hand
    on the printed form. Handwritten fields get ``script`` ``handwritten``; the clinician
    line (printed title, written name) gets ``mixed``. Boxes are the handwriting's own ink
    boxes. Everything else is the printed render, pixel for pixel. The input is not changed.
    """
    if mode not in ("handwritten", "mixed"):
        raise HandwritingError("bad_mode")
    base = render(document_gt, layout, dpi)
    gt = base.ground_truth
    fields = dict(schema.iter_fields(gt))
    wanted = _targets(layout.doc_type, mode, fields, rng)
    style = style_for(rng, round(layout.body_pt * dpi / 72 * 1.2))

    groups: dict[tuple[int, Box], list[str]] = {}
    for path in fields:
        if path in base.boxes:
            groups.setdefault(base.boxes[path], []).append(path)
    pages = [_decode(p) for p in base.pages]
    boxes = dict(base.boxes)
    scripts: dict[str, str] = {}
    for (page_no, box), paths in sorted(groups.items()):
        if not any(p in wanted for p in paths):
            continue
        owner = max(paths, key=lambda p: len(str(fields[p]["raw_text"])))
        raw = str(fields[owner]["raw_text"])
        multiline = _NOTE_TEXT.fullmatch(owner) is not None
        text = raw.replace("\r\n", "\n").replace("\r", "\n") if multiline else _SPACE.sub(" ", raw)
        img = pages[page_no - 1]
        px = _to_px(box, img.width, img.height)
        script = "handwritten"
        if mode == "mixed" and owner == "encounter.clinician_text" and " " in text.strip():
            cut = _split_column(img.crop(px))
            if cut is not None and px[0] + cut + MIN_GLYPH_PX < px[2]:
                text = text.strip().partition(" ")[2]
                px = (px[0] + cut, px[1], px[2], px[3])
                script = "mixed"
        try:
            hand = render_field(text, px, rng, style)
        except HandwritingError:
            continue  # too small to write by hand: stays printed
        bg = img.getpixel((px[0], px[1]))
        ImageDraw.Draw(img).rectangle((px[0], px[1], px[2] - 1, px[3] - 1), fill=bg)
        img.paste(hand.image, (px[0], px[1]), hand.image.getchannel("A"))
        ink = (
            px[0] + hand.ink[0] - 1,
            px[1] + hand.ink[1] - 1,
            px[0] + hand.ink[2] + 1,
            px[1] + hand.ink[3] + 1,
        )
        if script == "mixed":  # the field spans the printed title and the written name
            ink = _union(ink, _to_px(box, img.width, img.height))
        limit = _to_px(box, img.width, img.height)
        ink = (
            max(limit[0], ink[0]),
            max(limit[1], ink[1]),
            min(limit[2], ink[2]),
            min(limit[3], ink[3]),
        )
        new = _frac(ink, img.width, img.height)
        for path in paths:
            boxes[path] = (page_no, new)
            scripts[path] = script
    for path, fld in fields.items():
        if path in scripts:
            fld["script"] = scripts[path]
            fld["bbox"] = list(boxes[path][1])
    return RenderedDocument(pages=[_encode(p, dpi) for p in pages], boxes=boxes, ground_truth=gt)
