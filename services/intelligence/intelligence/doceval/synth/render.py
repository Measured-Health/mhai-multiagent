"""Render a ground-truth document to PNG pages, with a box for every field.

Pillow only: ``ImageDraw`` and ``ImageFont.truetype`` on the bundled fonts (basic
layout engine, so the output does not depend on whether libraqm is installed).
Every field's text is drawn by one call that also records the ink box of what it
drew, so a box is where the text really is, not where it was planned to be. Text
that is not in the ground truth (headers, labels, footers, stamps, legends) is
drawn without a box.

Boxes are fractions of the page, ``(x0, y0, x1, y1)``, and are written back into
the ``page`` (1-based) and ``bbox`` of a copy of the ground truth, returned as
``RenderedDocument.ground_truth`` (the input is never changed). A field with no text keeps
``page`` and ``bbox`` null. A few fields repeat text that another field already
shows (``value_numeric`` is the text of ``value_text``; ``reference_low`` and
``reference_high`` are parts of ``reference_text``): they share their owner's box.

Output is deterministic: no clock, no randomness, fixed PNG settings.

Limits (the pipeline's own, design section 3.2): at most 50 pages per document
and 10,000 px on either side of a page.
"""

import copy
import io
import math
import re
from collections.abc import Sequence
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw, ImageFont

from intelligence.doceval import schema
from intelligence.doceval.synth.layouts import LayoutSpec

MAX_PAGES = 50
MAX_SIDE_PX = 10_000
MAX_DPI = 400
# All pages are held as RGB images until the footers know the page count, so the
# total is bounded: 300 million pixels is about 900 MB.
MAX_TOTAL_PIXELS = 300_000_000
MIN_DPI = 36
MIN_FONT_PX = 12
_CHUNK = 5  # table columns per cell row

_FONT_DIR = Path(__file__).parent / "fonts"
_FONT_FILES = {
    "dejavu-sans": ("DejaVuSans.ttf", "DejaVuSans-Bold.ttf"),
    "dejavu-serif": ("DejaVuSerif.ttf", "DejaVuSerif-Bold.ttf"),
    "dejavu-mono": ("DejaVuSansMono.ttf", "DejaVuSansMono-Bold.ttf"),
    "liberation-sans": ("LiberationSans-Regular.ttf", "LiberationSans-Bold.ttf"),
}
_PAGE_MM = {"a4": (210.0, 297.0), "letter": (215.9, 279.4)}

RED = (255, 0, 0)  # only ever used to show one field in tests
INK = (20, 20, 20)
SOFT = (70, 70, 70)
GREY = (110, 110, 110)
LIGHT = (238, 238, 238)
RULE = (150, 150, 150)
WHITE = (255, 255, 255)
STAMP = (30, 50, 160)

_TITLES = {
    "lab_report": "LABORATORY REPORT",
    "clinic_note": "CLINIC NOTE",
    "discharge_summary": "DISCHARGE SUMMARY",
    "medication_chart": "MEDICATION CHART",
    "vitals_chart": "VITAL SIGNS CHART",
}
_KV_LABELS: dict[str, tuple[str, str, str]] = {
    "patient.mrn": ("MRN", "Medical Record No.", "Hospital No."),
    "patient.family_name": ("Surname", "Family name", "Last name"),
    "patient.given_names": ("Given names", "First names", "Forenames"),
    "patient.date_of_birth": ("DOB", "Date of birth", "Born"),
    "patient.sex": ("Sex", "Gender", "Sex"),
    "document.type": ("Type", "Document type", "Report"),
    "document.date": ("Date", "Report date", "Dated"),
    "document.language": ("Language", "Lang.", "Language"),
    "encounter.kind": ("Visit", "Encounter", "Visit type"),
    "encounter.start": ("Start", "Began", "From"),
    "encounter.end": ("End", "Finished", "To"),
    "encounter.location_text": ("Location", "Ward / clinic", "Seen at"),
    "encounter.clinician_text": ("Clinician", "Seen by", "Physician"),
}
_COLUMN_LABELS = {
    "name_text": "Test",
    "local_code": "Code",
    "value_text": "Result",
    "category": "Category",
    "unit_text": "Unit",
    "reference_text": "Reference",
    "abnormal_flag": "Flag",
    "specimen_text": "Specimen",
    "collected_at": "Collected",
    "resulted_at": "Resulted",
    "strength_text": "Strength",
    "dose_text": "Dose",
    "route_text": "Route",
    "frequency_text": "Frequency",
    "local_system": "System",
    "clinical_status": "Status",
}
_OBS_ORDER = (
    "name_text",
    "value_text",
    "unit_text",
    "reference_text",
    "abnormal_flag",
    "local_code",
    "category",
    "specimen_text",
    "collected_at",
    "resulted_at",
)
_MED_ORDER = (
    "name_text",
    "strength_text",
    "dose_text",
    "route_text",
    "frequency_text",
    "start",
    "stop",
    "status",
)
_COND_ORDER = ("text", "local_code", "local_system", "clinical_status", "verification", "onset")
_OBS_ALIASES = {
    "value_text": ("value_numeric",),
    "reference_text": ("reference_low", "reference_high"),
}


class RenderError(ValueError):
    """The document cannot be rendered within the limits. Carries no document content."""


Box = tuple[float, float, float, float]


@dataclass(frozen=True)
class RenderedDocument:
    pages: list[bytes]
    boxes: dict[str, tuple[int, Box]]
    ground_truth: dict[str, Any]  # a deep copy of the input with page and bbox filled in


@lru_cache(maxsize=64)
def _font(family: str, bold: bool, px: int) -> ImageFont.FreeTypeFont:
    name = _FONT_FILES[family][1 if bold else 0]
    return ImageFont.truetype(str(_FONT_DIR / name), px, layout_engine=ImageFont.Layout.BASIC)


_CONTROL = re.compile(r"[\r\n\t\v\f\x85\u2028\u2029]+")


def _text_of(field: Any, multiline: bool = False) -> str | None:
    """The text to draw for a field. Line breaks and tabs become one space, except
    in a note's text (``multiline``), which keeps newlines and drops carriage returns."""
    if isinstance(field, dict):
        raw = field.get("raw_text")
        if isinstance(raw, str) and raw.strip():
            if multiline:
                return raw.replace("\r\n", "\n").replace("\r", "\n")
            return _CONTROL.sub(" ", raw)
    return None


def _shares(part: str, whole: str) -> bool:
    """``part`` occurs in ``whole`` as a whole token, not inside a longer number or word."""
    return re.search(r"(?<![\w.])" + re.escape(part) + r"(?![\w.])", whole) is not None


class _Doc:
    def __init__(self, layout: LayoutSpec, dpi: int, highlight: str | None) -> None:
        self.layout = layout
        self.dpi = dpi
        self.highlight = highlight
        mm = dpi / 25.4
        w_mm, h_mm = _PAGE_MM[layout.page_size]
        self.W, self.H = round(w_mm * mm), round(h_mm * mm)
        if max(self.W, self.H) > MAX_SIDE_PX:
            raise RenderError("page_too_large")
        self.margin = round(layout.margin_mm * mm)
        self.footer_h = round(14 * mm)
        self.cw = self.W - 2 * self.margin
        self.bottom = self.H - self.footer_h
        self.body_px = max(MIN_FONT_PX, round(layout.body_pt * dpi / 72))
        self.images: list[Image.Image] = []
        self.d: ImageDraw.ImageDraw
        self.y = 0
        self.top = 0
        self.px: dict[str, tuple[int, list[int]]] = {}
        self.aliases: dict[str, tuple[str, ...]] = {}
        # flow state (columns)
        self.ncols = 1
        self.col = 0
        self.gap = 0
        self.colw = float(self.cw)
        self.flow_top = 0
        self.flow_max = 0
        self.new_page()

    # fonts and metrics

    def f(self, bold: bool = False, scale: float = 1.0) -> ImageFont.FreeTypeFont:
        return _font(self.layout.font, bold, max(MIN_FONT_PX, round(self.body_px * scale)))

    def lh(self, font: ImageFont.FreeTypeFont) -> int:
        asc, desc = font.getmetrics()
        return max(round(font.size * self.layout.line_spacing), asc + desc)

    # pages

    def new_page(self) -> None:
        if len(self.images) >= MAX_PAGES:
            raise RenderError("too_many_pages")
        if self.W * self.H * (len(self.images) + 1) > MAX_TOTAL_PIXELS:
            raise RenderError("pixel_budget")
        img = Image.new("RGB", (self.W, self.H), WHITE)
        self.images.append(img)
        self.d = ImageDraw.Draw(img)
        self.y = self.margin
        if len(self.images) > 1:
            small = self.f(True, 0.85)
            base = self.margin + small.getmetrics()[0]
            self.d.text(
                (self.margin, base),
                self.layout.org_name,
                font=small,
                fill=self.layout.accent,
                anchor="ls",
            )
            title = _TITLES[self.layout.doc_type]
            self.d.text((self.W - self.margin, base), title, font=small, fill=GREY, anchor="rs")
            rule_y = base + round(small.size * 0.5)
            self.d.line([(self.margin, rule_y), (self.W - self.margin, rule_y)], fill=RULE, width=2)
            self.y = rule_y + self.lh(small)
        self.top = self.y
        self.col = 0
        self.flow_top = self.y
        self.flow_max = self.y

    def page_break(self) -> None:
        self.new_page()

    # drawing

    def _extend(self, path: str, bb: Sequence[float]) -> None:
        page = len(self.images)
        cur = self.px.get(path)
        if cur is None:
            self.px[path] = (
                page,
                [math.floor(bb[0]), math.floor(bb[1]), math.ceil(bb[2]), math.ceil(bb[3])],
            )
            return
        if cur[0] != page:
            raise RenderError("field_spans_pages")
        box = cur[1]
        box[0], box[1] = min(box[0], math.floor(bb[0])), min(box[1], math.floor(bb[1]))
        box[2], box[3] = max(box[2], math.ceil(bb[2])), max(box[3], math.ceil(bb[3]))

    def put(
        self,
        path: str | None,
        text: str,
        x: float,
        base: float,
        font: ImageFont.FreeTypeFont,
        fill: tuple[int, int, int] = INK,
    ) -> float:
        """Draw ``text`` with its baseline at ``base``; record the box under ``path``."""
        if not text.strip():
            return 0.0
        owned = (path, *self.aliases.get(path, ())) if path is not None else ()
        hit = self.highlight is not None and self.highlight in owned
        self.d.text((x, base), text, font=font, fill=RED if hit else fill, anchor="ls")
        if path is not None:
            bb = self.d.textbbox((x, base), text, font=font, anchor="ls")
            for p in owned:
                if p is not None:
                    self._extend(p, bb)
        return float(font.getlength(text))

    @staticmethod
    def wrap(text: str, font: ImageFont.FreeTypeFont, width: float) -> list[str]:
        lines: list[str] = []
        for para in text.split("\n"):
            cur = ""
            for word in para.split(" "):
                trial = word if not cur else cur + " " + word
                if cur and font.getlength(trial) > width:
                    lines.append(cur)
                    cur = word
                else:
                    cur = trial
            lines.append(cur)
        return lines

    # flow (columns)

    def begin_flow(self, n: int) -> None:
        self.ncols = n
        self.gap = round(self.body_px * 1.5)
        self.colw = (self.cw - self.gap * (n - 1)) / n
        self.col = 0
        self.flow_top = self.y
        self.flow_max = self.y

    def end_flow(self) -> None:
        self.y = max(self.y, self.flow_max)
        self.ncols, self.col, self.colw = 1, 0, float(self.cw)

    @property
    def x0(self) -> float:
        return self.margin + self.col * (self.colw + self.gap)

    def need(self, h: float) -> None:
        if h > self.bottom - self.top:
            raise RenderError("block_too_tall")
        while self.y + h > self.bottom:
            self.flow_max = max(self.flow_max, self.y)
            if self.col + 1 < self.ncols:
                self.col += 1
                self.y = self.flow_top
            else:
                self.new_page()
                self.col = 0
                self.flow_top = self.flow_max = self.y

    # blocks

    def heading(self, text: str) -> None:
        font = self.f(True, 1.15)
        lh = self.lh(font)
        self.need(lh * 2.2)
        base = self.y + font.getmetrics()[0]
        self.d.text((self.margin, base), text, font=font, fill=self.layout.accent, anchor="ls")
        rule_y = self.y + lh + 2
        self.d.line([(self.margin, rule_y), (self.W - self.margin, rule_y)], fill=RULE, width=2)
        self.y += round(lh * 1.5)

    def kv(self, label: str, path: str, text: str) -> None:
        font, bold = self.f(), self.f(True)
        lh = self.lh(font)
        lab = label + ": "
        lw = bold.getlength(lab)
        if lw + font.getlength(text) <= self.colw:
            lines, inline = [text], True
        else:
            lines, inline = self.wrap(text, font, self.colw), False
        h = lh * (len(lines) + (0 if inline else 1))
        self.need(h)
        x, asc = self.x0, font.getmetrics()[0]
        base = self.y + asc
        self.d.text((x, base), lab, font=bold, fill=self.layout.accent, anchor="ls")
        if inline:
            self.put(path, text, x + lw, base, font)
        else:
            for k, line in enumerate(lines):
                self.put(path, line, x, base + lh * (k + 1), font)
        self.y += h + round(lh * 0.25)

    def note(self, i: int, heading: str | None, text: str | None) -> None:
        font, bold = self.f(), self.f(True)
        lh = self.lh(font)
        head = self.wrap(heading, bold, self.colw) if heading else []
        body = self.wrap(text, font, self.colw) if text else []
        self.need(lh * (len(head) + len(body)) + lh * 0.4)
        x, asc = self.x0, font.getmetrics()[0]
        base = self.y + asc
        for line in head:
            self.put(f"notes[{i}].heading", line, x, base, bold, self.layout.accent)
            base += lh
        for line in body:
            self.put(f"notes[{i}].text", line, x, base, font)
            base += lh
        self.y = round(base - asc + lh * 0.4)

    def table(
        self,
        base: str,
        rows: list[dict[str, Any]],
        order: Sequence[str],
        aliases: dict[str, tuple[str, ...]],
    ) -> None:
        layout = self.layout
        cells: list[dict[str, str]] = []
        for i, row in enumerate(rows):
            cur = {k: t for k, v in row.items() if (t := _text_of(v)) is not None}
            for owner, names in aliases.items():
                if owner not in cur:
                    continue
                shared = tuple(a for a in names if a in cur and _shares(cur[a], cur[owner]))
                if shared:
                    self.aliases[f"{base}[{i}].{owner}"] = tuple(f"{base}[{i}].{a}" for a in shared)
                    for a in shared:
                        del cur[a]
            cells.append(cur)
        seen: list[str] = []
        for cur in cells:
            for k in cur:
                if k not in seen:
                    seen.append(k)
        keys = [k for k in order if k in seen] + [k for k in seen if k not in order]
        if not keys:
            return
        sub = [keys[i : i + _CHUNK] for i in range(0, len(keys), _CHUNK)]
        ncol = len(sub[0])
        labels = [
            " / ".join(
                _COLUMN_LABELS.get(s[j], s[j].replace("_", " ").title()) for s in sub if j < len(s)
            )
            for j in range(ncol)
        ]

        scale, pad_f = 1.0, 0.7
        while True:
            px = max(MIN_FONT_PX, round(self.body_px * scale))
            font, bold = self.f(False, px / self.body_px), self.f(True, px / self.body_px)
            pad = round(px * pad_f)
            need_w = [bold.getlength(lab) for lab in labels]
            for cur in cells:
                for s in sub:
                    for j, k in enumerate(s):
                        if k in cur:
                            need_w[j] = max(need_w[j], font.getlength(cur[k]))
            total = sum(need_w) + pad * ncol
            if total <= self.cw:
                break
            if px > MIN_FONT_PX:
                scale *= 0.93
            elif pad_f > 0.3:
                pad_f = 0.25
            else:
                break
        extra = max(0.0, (self.cw - total) / ncol)
        widths = [w + pad + extra for w in need_w]
        xs = [self.margin + sum(widths[:j]) for j in range(ncol + 1)]
        lh = self.lh(font)
        asc = font.getmetrics()[0]
        vpad = round(lh * 0.5)
        row_h = len(sub) * lh + vpad
        head_h = round(lh * 1.5)
        right = self.W - self.margin
        accent = layout.accent

        def header() -> None:
            top = self.y
            if layout.table_style == "zebra":
                self.d.rectangle([self.margin, top, right, top + head_h], fill=accent)
            elif layout.table_style == "ruled":
                self.d.rectangle([self.margin, top, right, top + head_h], fill=LIGHT)
            colour = WHITE if layout.table_style == "zebra" else accent
            for j, lab in enumerate(labels):
                self.d.text(
                    (xs[j] + pad / 2, top + head_h - (head_h - asc) // 2 - 2),
                    lab,
                    font=bold,
                    fill=colour,
                    anchor="ls",
                )
            self.d.line([(self.margin, top + head_h), (right, top + head_h)], fill=GREY, width=2)
            if layout.table_style == "ruled":
                self.d.line([(self.margin, top), (right, top)], fill=GREY, width=2)
            self.y = top + head_h

        self.need(head_h + row_h)
        header()
        shown = 0
        for i, cur in enumerate(cells):
            if not cur:
                continue
            if self.y + row_h > self.bottom:
                self.new_page()
                header()
            top = self.y
            if layout.table_style == "zebra" and shown % 2 == 1:
                self.d.rectangle([self.margin, top, right, top + row_h], fill=LIGHT)
            if layout.table_style == "ruled":
                self.d.line([(self.margin, top + row_h), (right, top + row_h)], fill=RULE, width=1)
                for x in xs:
                    self.d.line([(x, top), (x, top + row_h)], fill=RULE, width=1)
            for sr, ks in enumerate(sub):
                base_y = top + vpad // 2 + sr * lh + asc
                for j, k in enumerate(ks):
                    if k in cur:
                        self.put(
                            f"{base}[{i}].{k}",
                            cur[k],
                            xs[j] + pad / 2,
                            base_y,
                            font,
                            INK if sr == 0 else SOFT,
                        )
            self.y = top + row_h
            shown += 1
        self.y += round(lh * 0.8)

    # page furniture

    def logo(self, x: int, y: int, size: int, colour: tuple[int, int, int]) -> None:
        d, shape = self.d, self.layout.logo_shape
        if shape == "circle":
            d.ellipse([x, y, x + size, y + size], outline=colour, width=max(3, size // 8))
            d.ellipse(
                [x + size // 3, y + size // 3, x + 2 * size // 3, y + 2 * size // 3], fill=colour
            )
        elif shape == "square":
            d.rectangle([x, y, x + size, y + size], fill=colour)
            d.rectangle(
                [x + size // 4, y + size // 4, x + 3 * size // 4, y + 3 * size // 4], fill=WHITE
            )
        elif shape == "triangle":
            d.polygon([(x + size // 2, y), (x + size, y + size), (x, y + size)], fill=colour)
        elif shape == "hexagon":
            cx, cy, r = x + size / 2, y + size / 2, size / 2
            pts = [
                (
                    round(cx + r * math.cos(math.pi / 3 * k)),
                    round(cy + r * math.sin(math.pi / 3 * k)),
                )
                for k in range(6)
            ]
            d.polygon(pts, fill=colour)
        else:
            bar = size // 5
            for k, hgt in enumerate((0.5, 1.0, 0.7)):
                bx = x + k * (bar + bar // 2)
                d.rectangle([bx, y + size - round(size * hgt), bx + bar, y + size], fill=colour)

    def page_header(self, facility: tuple[str, str] | None) -> None:
        layout, d = self.layout, self.d
        accent, style = layout.accent, layout.header_style
        org, fac, title_font = self.f(True, 1.5), self.f(False, 1.0), self.f(True, 1.9)
        size = round(self.body_px * 3.6)
        m, right = self.margin, self.W - self.margin
        y = self.y

        def fac_at(x: float, base: float, anchor_right: bool = False, centre: bool = False) -> None:
            if facility is None:
                return
            path, text = facility
            w = fac.getlength(text)
            xx = right - w if anchor_right else (self.W - w) / 2 if centre else x
            self.put(path, text, xx, base, fac, SOFT)

        title = _TITLES[layout.doc_type]
        org_asc = org.getmetrics()[0]
        if style == "logo-left":
            self.logo(m, y, size, accent)
            d.text(
                (m + size + 20, y + org_asc), layout.org_name, font=org, fill=accent, anchor="ls"
            )
            fac_at(m + size + 20, y + org_asc + self.lh(fac))
            y += size + 14
        elif style == "centred":
            self.logo((self.W - size) // 2, y, size, accent)
            y += size + 10
            d.text((self.W / 2, y + org_asc), layout.org_name, font=org, fill=accent, anchor="ms")
            y += self.lh(org)
            fac_at(0, y + fac.getmetrics()[0], centre=True)
            y += self.lh(fac) + 8
        elif style == "logo-right":
            self.logo(right - size, y, size, accent)
            d.text((m, y + org_asc), layout.org_name, font=org, fill=accent, anchor="ls")
            fac_at(m, y + org_asc + self.lh(fac))
            y += size + 14
        elif style == "band":
            band = size + 24
            d.rectangle([m, y, right, y + band], fill=accent)
            self.logo(m + 12, y + 12, size, WHITE)
            d.text(
                (m + size + 32, y + 12 + org_asc + (size - self.lh(org)) // 2),
                layout.org_name,
                font=org,
                fill=WHITE,
                anchor="ls",
            )
            y += band + 10
            fac_at(m, y + fac.getmetrics()[0])
            y += self.lh(fac) + 6
        else:
            d.text((m, y + org_asc), layout.org_name, font=org, fill=accent, anchor="ls")
            y += self.lh(org)
            fac_at(m, y + fac.getmetrics()[0])
            y += self.lh(fac) + 4
            d.line([(m, y), (right, y)], fill=accent, width=3)
            y += 6
        d.line([(m, y), (right, y)], fill=accent, width=3)
        y += 16
        tbase = y + title_font.getmetrics()[0]
        if style == "centred":
            d.text((self.W / 2, tbase), title, font=title_font, fill=INK, anchor="ms")
        else:
            d.text((m, tbase), title, font=title_font, fill=INK, anchor="ls")
        self.y = y + self.lh(title_font) + 12
        self.top = max(self.top, self.margin)
        self.flow_top = self.flow_max = self.y

    def footers(self) -> None:
        total = len(self.images)
        small = self.f(False, 0.8)
        for n, img in enumerate(self.images, start=1):
            d = ImageDraw.Draw(img)
            y0 = self.bottom + round(self.footer_h * 0.1)
            d.line([(self.margin, y0), (self.W - self.margin, y0)], fill=RULE, width=1)
            base = y0 + self.lh(small)
            d.text(
                (self.margin, base),
                "SYNTHETIC DOCUMENT - NOT A REAL RECORD",
                font=small,
                fill=GREY,
                anchor="ls",
            )
            d.text(
                (self.W - self.margin, base),
                f"Page {n} of {total}",
                font=small,
                fill=GREY,
                anchor="rs",
            )
            if self.layout.stamp:
                page_w = round(small.getlength(f"Page {n} of {total}"))
                self.stamp(img, self.layout.stamp, y0 + self.lh(small) // 2, page_w)

    def stamp(self, img: Image.Image, text: str, y: int, page_w: int) -> None:
        font = self.f(True, 0.9)
        w, h = round(font.getlength(text)) + 24, self.lh(font) + 8
        layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
        ld = ImageDraw.Draw(layer)
        ld.rectangle([0, 0, w - 1, h - 1], outline=STAMP, width=3)
        ld.text((w / 2, h / 2), text, font=font, fill=STAMP, anchor="mm")
        turned = layer.rotate(4, expand=True, resample=Image.Resampling.BICUBIC)
        limit = self.H - turned.height
        x = self.W - self.margin - page_w - 2 * self.body_px - turned.width
        img.paste(turned, (x, min(y, limit)), turned)

    def encode(self) -> list[bytes]:
        out = []
        for img in self.images:
            buf = io.BytesIO()
            img.save(buf, format="PNG", optimize=False, compress_level=6, dpi=(self.dpi, self.dpi))
            out.append(buf.getvalue())
        return out


def _kv_label(layout: LayoutSpec, key: str) -> str:
    variants = _KV_LABELS.get(key)
    return variants[layout.label_style] if variants else key.split(".")[-1].replace("_", " ")


def _kv_items(
    layout: LayoutSpec, gt: dict[str, Any], section: str, keys: Sequence[str]
) -> list[tuple[str, str, str]]:
    node = gt.get(section)
    out: list[tuple[str, str, str]] = []
    if not isinstance(node, dict):
        return out
    for key in keys:
        text = _text_of(node.get(key))
        if text is not None:
            out.append((_kv_label(layout, f"{section}.{key}"), f"{section}.{key}", text))
    return out


def _rows(gt: dict[str, Any], name: str) -> list[dict[str, Any]]:
    rows = gt.get(name)
    return [r for r in rows if isinstance(r, dict)] if isinstance(rows, list) else []


def _render(
    gt: dict[str, Any], layout: LayoutSpec, dpi: int, highlight: str | None
) -> RenderedDocument:
    if isinstance(dpi, bool) or not isinstance(dpi, int) or not MIN_DPI <= dpi <= MAX_DPI:
        raise RenderError("bad_dpi")
    gt = copy.deepcopy(gt)  # the caller's ground truth is never touched
    fields = dict(schema.iter_fields(gt))
    for fld in fields.values():
        fld["page"] = None
        fld["bbox"] = None
    doc = _Doc(layout, dpi, highlight)

    fac_text = _text_of(fields.get("document.facility_name"))
    doc.page_header(("document.facility_name", fac_text) if fac_text else None)

    sections: list[tuple[str, Any]] = []

    patient = _kv_items(
        layout, gt, "patient", ("mrn", "family_name", "given_names", "date_of_birth", "sex")
    )
    others = gt.get("patient", {}).get("other_ids") if isinstance(gt.get("patient"), dict) else None
    for i, row in enumerate(others if isinstance(others, list) else []):
        for key, label in (("kind", "ID type"), ("value", "ID")):
            text = _text_of(row.get(key)) if isinstance(row, dict) else None
            if text is not None:
                patient.append((label, f"patient.other_ids[{i}].{key}", text))
    if patient:
        sections.append(("Patient", lambda: _draw_kv(doc, patient)))
    docenc = _kv_items(layout, gt, "document", ("type", "date", "language")) + _kv_items(
        layout, gt, "encounter", ("kind", "start", "end", "location_text", "clinician_text")
    )
    if docenc:
        sections.append(("Document and encounter", lambda: _draw_kv(doc, docenc)))
    obs_title = {"lab_report": "Laboratory results", "vitals_chart": "Vital signs"}.get(
        layout.doc_type, "Observations"
    )
    for title, name, order, aliases in (
        (obs_title, "observations", _OBS_ORDER, _OBS_ALIASES),
        ("Medications", "medications", _MED_ORDER, {}),
        ("Conditions", "conditions", _COND_ORDER, {}),
    ):
        rows = _rows(gt, name)
        if any(_text_of(v) for r in rows for v in r.values()):
            sections.append(
                (title, lambda n=name, r=rows, o=order, a=aliases: doc.table(n, r, o, a))
            )
    notes = _rows(gt, "notes")
    if any(_text_of(v) for r in notes for v in r.values()):
        sections.append(("Notes", lambda: _draw_notes(doc, notes)))

    for k, (title, draw) in enumerate(sections):
        if k and layout.new_page_per_section:
            doc.page_break()
        doc.heading(title)
        draw()

    # Safety net: any field the layout above does not place is still drawn.
    skipped = {p for p, v in fields.items() if _text_of(v) is not None and p not in doc.px} - {
        a for al in doc.aliases.values() for a in al
    }
    if skipped:
        doc.heading("Additional fields")
        _draw_kv(doc, [(p, p, str(_text_of(fields[p]))) for p in sorted(skipped)])

    doc.footers()
    pages = doc.encode()

    boxes: dict[str, tuple[int, Box]] = {}
    for path, (page, px) in doc.px.items():
        x0, y0 = max(0, px[0] - 1), max(0, px[1] - 1)
        x1, y1 = min(doc.W, px[2] + 1), min(doc.H, px[3] + 1)
        if x1 <= x0 or y1 <= y0:
            raise RenderError("field_off_page")
        box = (
            round(x0 / doc.W, 6),
            round(y0 / doc.H, 6),
            round(min(1.0, x1 / doc.W), 6),
            round(min(1.0, y1 / doc.H), 6),
        )
        boxes[path] = (page, box)
        fields[path]["page"] = page
        fields[path]["bbox"] = list(box)
    return RenderedDocument(pages=pages, boxes=boxes, ground_truth=gt)


def _draw_kv(doc: _Doc, items: list[tuple[str, str, str]]) -> None:
    doc.begin_flow(doc.layout.columns)
    for label, path, text in items:
        doc.kv(label, path, text)
    doc.end_flow()


def _draw_notes(doc: _Doc, notes: list[dict[str, Any]]) -> None:
    doc.begin_flow(doc.layout.columns)
    for i, row in enumerate(notes):
        doc.note(i, _text_of(row.get("heading")), _text_of(row.get("text"), True))
    doc.end_flow()


def render(document_gt: dict[str, Any], layout: LayoutSpec, dpi: int = 200) -> RenderedDocument:
    """Render ``document_gt`` (model-output view) with ``layout``.

    The input is never changed. The result carries the PNG pages, the boxes by field
    path, and ``ground_truth``: a deep copy of the input with ``page`` and ``bbox``
    filled in. Raises :class:`RenderError` (a code only, no content) beyond 50 pages,
    10,000 px per side, 400 dpi or the total pixel budget.
    """
    return _render(document_gt, layout, dpi, None)


def render_highlighted(
    document_gt: dict[str, Any], layout: LayoutSpec, path: str, dpi: int = 200
) -> RenderedDocument:
    """Like :func:`render`, with one field's text in pure red and nothing else red.

    For tests and debugging: where the red ink is, is where that field was drawn.
    """
    return _render(document_gt, layout, dpi, path)
