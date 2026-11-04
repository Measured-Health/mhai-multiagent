"""Scan and phone-photo degradation, with ground-truth boxes that follow the geometry.

Degradation is a pure function: it reads rendered pages (PNG bytes) and boxes and
returns new PNG pages and new boxes. Nothing passed in is changed. All randomness
comes from the ``random.Random`` the caller supplies, so a seed gives the same bytes.

Geometry. Every geometric step (skew, perspective, margin or crop, resample) is one
homography on the page. Each box is a rectangle in page fractions; its four corners go
through the same homography and the box becomes the axis-aligned bounding box of the
transformed quad (grown by one pixel so antialiased edges stay inside), clipped to the
frame. Then:

* if at least ``MIN_VISIBLE`` of the box is still in the frame, the clipped box is
  returned (clamped to [0, 1]);
* otherwise the field is *unreadable*: its box becomes ``None`` and its path is listed
  in the page metadata under ``unreadable``. A wrong box is never returned.

Photometric steps (blur, noise, lighting, shadows, folds, stamps, JPEG round trip,
binarisation) do not move anything. Stamps and folds are drawn in page space; a field
a stamp covers, or a fold line crosses, is listed under ``occluded`` (its box stays, the text is harder to read).

Fields listed as unreadable keep their value and ``raw_text`` in the ground truth, with
null ``page`` and ``bbox``. Metrics and grounding checks must treat them as not visible
(not as fields without text).

Output pages are fresh RGB PNGs with pixel data only: no text chunks, no dpi, no ICC
profile, nothing carried over from the input. The parameters used are returned in the
page metadata, never embedded in the image.

Limits (same family as ``render``): at most ``MAX_PAGES`` pages, ``MAX_SIDE_PX`` per
side, ``MAX_PAGE_PIXELS`` per page (input, working canvas and output) and
``MAX_TOTAL_PIXELS`` of input per document; a document's output never has more pixels than
its input (the resample is clamped, a running total is checked). The source dpi of the
rendered pages must be given: it is not stored on the pages. Pages are processed one at a time.
"""

import copy
import io
import math
import random
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, Literal

import numpy as np
import numpy.typing as npt
from PIL import Image, ImageDraw, ImageFilter

from intelligence.doceval import schema
from intelligence.doceval.synth.render import (
    MAX_DPI,
    MAX_PAGES,
    MAX_SIDE_PX,
    MAX_TOTAL_PIXELS,
    MIN_DPI,
    Box,
    RenderedDocument,
)

MAX_PAGE_PIXELS = 25_000_000
MAX_FOLDS = 10
MAX_STAMPS = 10
MAX_BLUR_PX = 10.0
MAX_NOISE = 60.0
MAX_PERSPECTIVE = 0.2
MAX_SKEW_DEG = 15.0
MIN_VISIBLE = 0.8  # fraction of a box that must stay in frame to keep a (clipped) box
_STAMP_OVERLAP = 0.25

Range = tuple[float, float]
Mode = Literal["scan", "photo"]
_F32 = npt.NDArray[np.float32]
_F64 = npt.NDArray[np.float64]
_U8 = npt.NDArray[np.uint8]


class DegradeError(ValueError):
    """The input cannot be degraded within the limits. Carries a code only, no content."""


@dataclass(frozen=True)
class Profile:
    """A degradation profile: a set of parameter ranges, ``(low, high)`` each."""

    name: str
    skew_deg: Range  # rotation, both signs allowed
    blur_px: Range  # gaussian radius at output resolution
    noise_sigma: Range  # grey levels
    jpeg_quality: Range
    dpi: Range  # scan resample target
    binarise_prob: float  # chance of fax-like binarisation
    binarise_level: Range  # threshold as a fraction of the page's ink-to-paper range
    folds: Range  # count
    shadow: Range  # strength of one soft shadow band
    stamps: Range  # count
    perspective: Range  # photo: corner shift as a fraction of the page side
    lighting: Range  # photo: strength of uneven lighting
    crop_margin: Range  # photo: margin around (positive) or crop into (negative) the page, fraction
    long_side_px: Range  # photo: downscaled long side

    def ranges(self) -> dict[str, Range]:
        return {
            "skew_deg": self.skew_deg,
            "blur_px": self.blur_px,
            "noise_sigma": self.noise_sigma,
            "jpeg_quality": self.jpeg_quality,
            "dpi": self.dpi,
            "binarise_level": self.binarise_level,
            "folds": self.folds,
            "shadow": self.shadow,
            "stamps": self.stamps,
            "perspective": self.perspective,
            "lighting": self.lighting,
            "crop_margin": self.crop_margin,
            "long_side_px": self.long_side_px,
        }

    def __post_init__(self) -> None:
        if not all(math.isfinite(v) for r in self.ranges().values() for v in r):
            raise DegradeError("bad_range_nan")
        for key, (lo, hi) in self.ranges().items():
            if not lo <= hi:
                raise DegradeError(f"bad_range_{key}")
        if not 0.0 <= self.binarise_prob <= 1.0:
            raise DegradeError("bad_binarise_prob")
        if self.dpi[0] < MIN_DPI or self.dpi[1] > MAX_DPI:
            raise DegradeError("bad_range_dpi")
        if self.long_side_px[0] < 16 or self.long_side_px[1] > MAX_SIDE_PX:
            raise DegradeError("bad_range_long_side_px")
        if self.jpeg_quality[0] < 1 or self.jpeg_quality[1] > 95:
            raise DegradeError("bad_range_jpeg_quality")
        caps: list[tuple[str, bool]] = [
            ("folds", 0 <= self.folds[0] and self.folds[1] <= MAX_FOLDS),
            ("stamps", 0 <= self.stamps[0] and self.stamps[1] <= MAX_STAMPS),
            ("blur_px", 0 <= self.blur_px[0] and self.blur_px[1] <= MAX_BLUR_PX),
            ("noise_sigma", 0 <= self.noise_sigma[0] and self.noise_sigma[1] <= MAX_NOISE),
            ("perspective", 0 <= self.perspective[0] and self.perspective[1] <= MAX_PERSPECTIVE),
            ("skew_deg", -MAX_SKEW_DEG <= self.skew_deg[0] and self.skew_deg[1] <= MAX_SKEW_DEG),
            ("lighting", 0 <= self.lighting[0] and self.lighting[1] <= 1),
            ("shadow", 0 <= self.shadow[0] and self.shadow[1] <= 1),
            ("binarise_level", 0 < self.binarise_level[0] and self.binarise_level[1] < 1),
        ]
        for key, ok in caps:
            if not ok:
                raise DegradeError(f"bad_range_{key}")
        if self.crop_margin[0] <= -0.4 or self.crop_margin[1] > 0.5:
            raise DegradeError("bad_range_crop_margin")


PROFILES: dict[str, Profile] = {
    "clean": Profile(
        name="clean",
        skew_deg=(-0.5, 0.5),
        blur_px=(0.0, 0.4),
        noise_sigma=(0.0, 2.0),
        jpeg_quality=(85, 95),
        dpi=(250, 300),
        binarise_prob=0.0,
        binarise_level=(0.45, 0.65),
        folds=(0, 0),
        shadow=(0.0, 0.05),
        stamps=(0, 0),
        perspective=(0.0, 0.0),
        lighting=(0.0, 0.05),
        crop_margin=(0.0, 0.02),
        long_side_px=(2400, 3000),
    ),
    "poor": Profile(
        name="poor",
        skew_deg=(-5.0, 5.0),
        blur_px=(0.6, 1.6),
        noise_sigma=(4.0, 12.0),
        jpeg_quality=(35, 65),
        dpi=(150, 200),
        binarise_prob=0.35,
        binarise_level=(0.45, 0.65),
        folds=(0, 2),
        shadow=(0.1, 0.3),
        stamps=(0, 2),
        perspective=(0.0, 0.01),
        lighting=(0.05, 0.15),
        crop_margin=(0.0, 0.03),
        long_side_px=(1600, 2200),
    ),
    "phone": Profile(
        name="phone",
        skew_deg=(-3.0, 3.0),
        blur_px=(0.5, 1.2),
        noise_sigma=(2.0, 6.0),
        jpeg_quality=(60, 85),
        dpi=(150, 300),
        binarise_prob=0.0,
        binarise_level=(0.45, 0.65),
        folds=(0, 1),
        shadow=(0.1, 0.35),
        stamps=(0, 1),
        perspective=(0.01, 0.06),
        lighting=(0.15, 0.4),
        crop_margin=(-0.01, 0.06),
        long_side_px=(1600, 3000),
    ),
}


def get_profile(profile: "Profile | str") -> Profile:
    if isinstance(profile, Profile):
        return profile
    found = PROFILES.get(profile) if isinstance(profile, str) else None
    if found is None:
        raise DegradeError("unknown_profile")
    return found


@dataclass(frozen=True)
class DegradedPage:
    page: bytes
    boxes: dict[str, Box | None]  # None: the field is unreadable on this page
    metadata: dict[str, Any]


@dataclass(frozen=True)
class DegradedDocument:
    pages: list[bytes]
    boxes: dict[str, tuple[int, Box]]  # readable fields only
    ground_truth: dict[str, Any]  # deep copy; page and bbox follow the geometry
    metadata: dict[str, Any]  # profile, mode and one entry per page


# ---------------------------------------------------------------- parameters


def _draw_params(rng: random.Random, p: Profile) -> dict[str, Any]:
    """Draw every parameter in a fixed order, so one seed always means one page."""

    def u(r: Range) -> float:
        return rng.uniform(r[0], r[1])

    return {
        "skew_deg": u(p.skew_deg),
        "blur_px": u(p.blur_px),
        "noise_sigma": u(p.noise_sigma),
        "jpeg_quality": rng.randint(int(p.jpeg_quality[0]), int(p.jpeg_quality[1])),
        "dpi": u(p.dpi),
        "binarise": rng.random() < p.binarise_prob,
        "binarise_level": u(p.binarise_level),
        "folds": rng.randint(int(p.folds[0]), int(p.folds[1])),
        "shadow": u(p.shadow),
        "stamps": rng.randint(int(p.stamps[0]), int(p.stamps[1])),
        "perspective": u(p.perspective),
        "lighting": u(p.lighting),
        "crop_margin": u(p.crop_margin),
        "long_side_px": rng.randint(int(p.long_side_px[0]), int(p.long_side_px[1])),
    }


# ------------------------------------------------------------------ geometry


def _homography(src: list[tuple[float, float]], dst: list[tuple[float, float]]) -> _F64:
    a = np.zeros((8, 8), dtype=np.float64)
    b = np.zeros(8, dtype=np.float64)
    for i, ((x, y), (u, v)) in enumerate(zip(src, dst, strict=True)):
        a[2 * i] = [x, y, 1, 0, 0, 0, -u * x, -u * y]
        a[2 * i + 1] = [0, 0, 0, x, y, 1, -v * x, -v * y]
        b[2 * i], b[2 * i + 1] = u, v
    try:
        h = np.linalg.solve(a, b)
    except np.linalg.LinAlgError as e:
        raise DegradeError("degenerate_geometry") from e
    return np.append(h, 1.0).reshape(3, 3)


def _apply(h: _F64, x: float, y: float) -> tuple[float, float] | None:
    v = h @ np.array([x, y, 1.0])
    if v[2] <= 1e-9:
        return None
    return float(v[0] / v[2]), float(v[1] / v[2])


def _map_box(h: _F64, box: Box, w: int, hh: int, cw: int, ch: int) -> Box | None:
    """Map a page-fraction box through ``h``; return a canvas-fraction box or ``None``."""
    x0, y0, x1, y1 = box[0] * w, box[1] * hh, box[2] * w, box[3] * hh
    pts = [_apply(h, x, y) for x, y in ((x0, y0), (x1, y0), (x1, y1), (x0, y1))]
    if any(p is None for p in pts):
        return None
    xs = [p[0] for p in pts if p is not None]
    ys = [p[1] for p in pts if p is not None]
    bx0, by0, bx1, by1 = min(xs) - 1.0, min(ys) - 1.0, max(xs) + 1.0, max(ys) + 1.0
    full = (bx1 - bx0) * (by1 - by0)
    cx0, cy0, cx1, cy1 = max(0.0, bx0), max(0.0, by0), min(float(cw), bx1), min(float(ch), by1)
    if cx1 <= cx0 or cy1 <= cy0 or full <= 0:
        return None
    if (cx1 - cx0) * (cy1 - cy0) / full < MIN_VISIBLE:
        return None
    return (
        round(cx0 / cw, 6),
        round(cy0 / ch, 6),
        round(min(1.0, cx1 / cw), 6),
        round(min(1.0, cy1 / ch), 6),
    )


# ----------------------------------------------------------------- image ops


def _open(page: bytes) -> Image.Image:
    try:
        im = Image.open(io.BytesIO(page))
        if im.format != "PNG":
            raise DegradeError("not_png")
        w, h = im.size
        if w < 8 or h < 8 or max(w, h) > MAX_SIDE_PX or w * h > MAX_PAGE_PIXELS:
            raise DegradeError("page_size")
        im.load()
        return im.convert("RGB")
    except DegradeError:
        raise
    except Exception as e:  # noqa: BLE001 - any decoder failure is one fixed code
        raise DegradeError("bad_image") from e


def _png(im: Image.Image) -> bytes:
    out = io.BytesIO()
    # A fresh image has no info, so the file holds IHDR, IDAT and IEND only.
    Image.frombytes("RGB", im.size, im.tobytes()).save(out, format="PNG", compress_level=6)
    return out.getvalue()


def _stamps(im: Image.Image, rng: random.Random, count: int, boxes: Mapping[str, Box]) -> list[str]:
    """Draw ring stamps in page space, centred on text where there is some."""
    w, h = im.size
    occluded: set[str] = set()
    if count <= 0:
        return []
    over = Image.new("RGBA", im.size, (0, 0, 0, 0))
    d = ImageDraw.Draw(over)
    anchors = sorted(boxes.values())
    for _ in range(count):
        r = rng.uniform(0.04, 0.07) * w
        if anchors:
            a = anchors[rng.randrange(len(anchors))]
            cx, cy = (a[0] + a[2]) / 2 * w, (a[1] + a[3]) / 2 * h
        else:
            cx, cy = rng.uniform(0.2, 0.8) * w, rng.uniform(0.2, 0.8) * h
        colour = rng.choice([(30, 50, 170), (170, 30, 40), (90, 30, 140)]) + (150,)
        lw = max(2, int(r / 12))
        d.ellipse((cx - r, cy - r, cx + r, cy + r), outline=colour, width=lw)
        d.ellipse(
            (cx - r * 0.7, cy - r * 0.7, cx + r * 0.7, cy + r * 0.7), outline=colour, width=lw
        )
        for k in (-1, 0, 1):
            y = cy + k * r * 0.25
            d.line((cx - r * 0.55, y, cx + r * 0.55, y), fill=colour, width=lw)
        sx0, sy0, sx1, sy1 = (cx - r) / w, (cy - r) / h, (cx + r) / w, (cy + r) / h
        for path, b in boxes.items():
            ix = max(0.0, min(b[2], sx1) - max(b[0], sx0))
            iy = max(0.0, min(b[3], sy1) - max(b[1], sy0))
            area = (b[2] - b[0]) * (b[3] - b[1])
            if area > 0 and ix * iy / area >= _STAMP_OVERLAP:
                occluded.add(path)
    im.paste(over, (0, 0), over)
    return sorted(occluded)


def _folds(im: Image.Image, rng: random.Random, count: int) -> list[Box]:
    """A fold is a thin dark line with a light edge, across the whole page.

    Returns the band of each fold as a page-fraction box.
    """
    bands: list[Box] = []
    if count <= 0:
        return bands
    arr = np.asarray(im).copy()
    h, w = arr.shape[:2]
    for _ in range(count):
        horizontal = rng.random() < 0.5
        pos = rng.uniform(0.25, 0.75)
        width = max(2, round(min(w, h) / 500))
        n = h if horizontal else w
        c = int(pos * n)
        lo_f, hi_f = max(0.0, (c - width) / n), min(1.0, (c + 2 * width) / n)
        bands.append((0.0, lo_f, 1.0, hi_f) if horizontal else (lo_f, 0.0, hi_f, 1.0))
        for off, gain in ((0, 0.78), (width, 1.06), (-width, 1.03)):
            lo, hi = c + off, c + off + width
            if lo < 0 or hi > n:
                continue
            sl = arr[lo:hi, :, :] if horizontal else arr[:, lo:hi, :]
            sl[...] = np.clip(sl.astype(np.float32) * gain, 0, 255).astype(np.uint8)
    im.paste(Image.fromarray(arr), (0, 0))
    return bands


def _field(h: int, w: int, angle: float) -> _F32:
    """A smooth ramp from -1 to 1 along ``angle`` across an h-by-w frame."""
    xs = np.linspace(-1.0, 1.0, w, dtype=np.float32)
    ys = np.linspace(-1.0, 1.0, h, dtype=np.float32)
    f = ys[:, None] * np.float32(math.sin(angle)) + xs[None, :] * np.float32(math.cos(angle))
    norm = max(1e-6, abs(math.sin(angle)) + abs(math.cos(angle)))
    return (f / np.float32(norm)).astype(np.float32)


def _illuminate(im: Image.Image, rng: random.Random, lighting: float, shadow: float) -> Image.Image:
    w, h = im.size
    if lighting <= 0 and shadow <= 0:
        rng.random()
        return im
    f = _field(h, w, rng.uniform(0, 2 * math.pi))
    gain = 1.0 + np.float32(lighting) * f  # one side brighter, the other dimmer
    edge = _field(h, w, rng.uniform(0, 2 * math.pi))
    centre = np.float32(rng.uniform(-0.2, 0.6))
    t = np.clip((edge - centre) / np.float32(0.6), 0, 1)
    gain = gain * (1.0 - np.float32(shadow) * (t * t * (3 - 2 * t)))  # soft shadow band
    arr = np.asarray(im)
    out = np.empty_like(arr)
    for c in range(3):
        out[:, :, c] = np.clip(arr[:, :, c] * gain, 0, 255).astype(np.uint8)
    return Image.fromarray(out)


def _noise(im: Image.Image, gen: np.random.Generator, sigma: float) -> Image.Image:
    if sigma <= 0:
        return im
    arr = np.asarray(im)
    out = np.empty_like(arr)
    grey = gen.standard_normal(arr.shape[:2], dtype=np.float32)
    for c in range(3):
        n = np.float32(sigma) * (
            0.8 * grey + 0.2 * gen.standard_normal(arr.shape[:2], dtype=np.float32)
        )
        out[:, :, c] = np.clip(arr[:, :, c].astype(np.float32) + n, 0, 255).astype(np.uint8)
    return Image.fromarray(out)


def _jpeg(im: Image.Image, quality: int) -> Image.Image:
    buf = io.BytesIO()
    im.save(buf, format="JPEG", quality=quality)
    buf.seek(0)
    out = Image.open(buf)
    out.load()
    return out.convert("RGB")


def _binarise(im: Image.Image, level: float) -> Image.Image:
    g = np.asarray(im.convert("L"))
    lo, hi = float(np.percentile(g, 1)), float(np.percentile(g, 99))
    t = lo + level * (hi - lo)
    bw = np.where(g > t, 255, 0).astype(np.uint8)
    return Image.fromarray(bw).convert("RGB")


# ------------------------------------------------------------------ pipeline


def _check_box(b: Box) -> None:
    if not (
        all(math.isfinite(v) for v in b) and 0.0 <= b[0] < b[2] <= 1.0 and 0.0 <= b[1] < b[3] <= 1.0
    ):
        raise DegradeError("bad_box")


def _degrade(
    page: bytes,
    boxes: Mapping[str, Box],
    rng: random.Random,
    profile: "Profile | str",
    mode: Mode,
    source_dpi: float,
    max_pixels: int = MAX_PAGE_PIXELS,
) -> DegradedPage:
    p = get_profile(profile)
    if not MIN_DPI <= source_dpi <= MAX_DPI:
        raise DegradeError("bad_source_dpi")
    for b in boxes.values():
        _check_box(b)
    src = _open(page)
    w, h = src.size
    params = _draw_params(rng, p)
    gen = np.random.default_rng(rng.getrandbits(64))
    stamp_rng = random.Random(rng.getrandbits(64))
    fold_rng = random.Random(rng.getrandbits(64))
    light_rng = random.Random(rng.getrandbits(64))
    bg_rng = random.Random(rng.getrandbits(64))

    # Photometric steps in page space: they move with the paper.
    occluded = _stamps(src, stamp_rng, params["stamps"], boxes)
    for band in _folds(src, fold_rng, params["folds"]):  # a fold line through text occludes it
        for path, b in boxes.items():
            if min(b[2], band[2]) > max(b[0], band[0]) and min(b[3], band[3]) > max(b[1], band[1]):
                occluded.append(path)
    occluded = sorted(set(occluded))

    # One homography for skew, perspective and margin or crop.
    m = params["crop_margin"] if mode == "photo" else 0.0
    mx, my = round(m * w), round(m * h)
    cw, ch = w + 2 * mx, h + 2 * my
    if cw < 8 or ch < 8 or max(cw, ch) > MAX_SIDE_PX or cw * ch > MAX_PAGE_PIXELS:
        raise DegradeError("canvas_size")
    th = math.radians(params["skew_deg"])
    cos, sin = math.cos(th), math.sin(th)
    ccx, ccy = cw / 2, ch / 2
    corners = [(0.0, 0.0), (float(w), 0.0), (float(w), float(h)), (0.0, float(h))]
    jit = params["perspective"] if mode == "photo" else 0.0
    dst: list[tuple[float, float]] = []
    for x, y in corners:
        dx, dy = x - w / 2, y - h / 2
        rx, ry = ccx + dx * cos - dy * sin, ccy + dx * sin + dy * cos
        if jit > 0:
            rx += rng.uniform(-jit, jit) * w
            ry += rng.uniform(-jit, jit) * h
        dst.append((rx, ry))
    hom = _homography(corners, dst)
    inv = np.linalg.inv(hom)
    inv = inv / inv[2, 2]
    coeffs = (
        inv[0, 0],
        inv[0, 1],
        inv[0, 2],
        inv[1, 0],
        inv[1, 1],
        inv[1, 2],
        inv[2, 0],
        inv[2, 1],
    )
    if mode == "photo":
        v = bg_rng.randint(25, 90)
        fill = (v, v - bg_rng.randint(0, 10), v - bg_rng.randint(0, 15))
    else:
        fill = (255, 255, 255)
    moved = src.transform(
        (cw, ch), Image.Transform.PERSPECTIVE, coeffs, Image.Resampling.BICUBIC, fillcolor=fill
    )
    del src

    new_boxes: dict[str, Box | None] = {k: _map_box(hom, b, w, h, cw, ch) for k, b in boxes.items()}

    # Resample (scan: to a target dpi; photo: downscale to a phone-sized long side).
    if mode == "scan":
        scale = params["dpi"] / source_dpi
        eff = {"dpi": round(source_dpi * scale, 3)}
    else:
        scale = min(1.0, params["long_side_px"] / max(cw, ch))
        eff = {}
    scale = min(
        scale, MAX_SIDE_PX / max(cw, ch), math.sqrt(min(max_pixels, MAX_PAGE_PIXELS) / (cw * ch))
    )
    ow, oh = max(8, int(cw * scale)), max(8, int(ch * scale))
    resample = Image.Resampling.LANCZOS if scale < 1 else Image.Resampling.BICUBIC
    img = moved.resize((ow, oh), resample)  # antialiased: no moire from the downscale
    del moved
    if mode == "scan":
        eff["dpi"] = round(source_dpi * ow / cw, 3)

    img = _illuminate(
        img, light_rng, params["lighting"] if mode == "photo" else 0.0, params["shadow"]
    )
    if params["blur_px"] > 0:
        img = img.filter(ImageFilter.GaussianBlur(params["blur_px"]))
    img = _noise(img, gen, params["noise_sigma"])
    img = _jpeg(img, params["jpeg_quality"])
    binarised = mode == "scan" and bool(params["binarise"])
    if binarised:
        img = _binarise(img, params["binarise_level"])

    used = dict(params)
    used.update(eff)
    if mode == "scan":
        for k in ("perspective", "lighting", "crop_margin", "long_side_px"):
            used.pop(k)
    else:
        for k in ("dpi", "binarise", "binarise_level"):
            used.pop(k)
    used["binarise"] = binarised
    if not binarised:
        used.pop("binarise_level", None)
    meta: dict[str, Any] = {
        "profile": p.name,
        "mode": mode,
        "params": used,
        "size": [ow, oh],
        "unreadable": sorted(k for k, b in new_boxes.items() if b is None),
        "occluded": [k for k in occluded if new_boxes.get(k) is not None],
    }
    return DegradedPage(page=_png(img), boxes=new_boxes, metadata=meta)


def scan(
    page: bytes,
    boxes: Mapping[str, Box],
    rng: random.Random,
    profile: "Profile | str",
    *,
    source_dpi: float,
) -> DegradedPage:
    """Print-and-scan: skew, folds, stamps, shadow, resample, blur, noise, JPEG, fax."""
    return _degrade(page, boxes, rng, profile, "scan", source_dpi)


def photo(
    page: bytes,
    boxes: Mapping[str, Box],
    rng: random.Random,
    profile: "Profile | str",
    *,
    source_dpi: float,
) -> DegradedPage:
    """Phone photo: perspective, margin or crop, uneven light, moire-free downscale."""
    return _degrade(page, boxes, rng, profile, "photo", source_dpi)


@dataclass
class _Acc:
    pages: list[bytes] = field(default_factory=list)
    meta: list[dict[str, Any]] = field(default_factory=list)


def degrade_document(
    doc: RenderedDocument,
    seed: int,
    profile: "Profile | str",
    mode: Mode,
    *,
    source_dpi: float,
) -> DegradedDocument:
    """Degrade every page of a rendered document; boxes and ground truth follow.

    ``doc`` is not changed. A field that a transform pushes out of frame has its
    ``page`` and ``bbox`` set to null in the returned ground truth and its path listed in
    that page's ``unreadable`` metadata.
    """
    if mode not in ("scan", "photo") or isinstance(seed, bool) or not isinstance(seed, int):
        raise DegradeError("bad_arguments")
    n = len(doc.pages)
    if n < 1 or n > MAX_PAGES:
        raise DegradeError("too_many_pages")
    by_page: dict[int, dict[str, Box]] = {}
    for path, (pg, box) in doc.boxes.items():
        if not 1 <= pg <= n:
            raise DegradeError("bad_box_page")
        by_page.setdefault(pg, {})[path] = box
    total = 0
    in_pixels: list[int] = []
    for data in doc.pages:
        try:
            with Image.open(io.BytesIO(data)) as im:
                in_pixels.append(im.size[0] * im.size[1])
                total += in_pixels[-1]
        except Exception as e:  # noqa: BLE001
            raise DegradeError("bad_image") from e
    if total > MAX_TOTAL_PIXELS:
        raise DegradeError("document_too_large")

    master = random.Random(seed)
    acc = _Acc()
    new: dict[str, tuple[int, Box]] = {}
    gt = copy.deepcopy(doc.ground_truth)
    fields = dict(schema.iter_fields(gt))
    out_total = 0
    for i, data in enumerate(doc.pages, start=1):
        res = _degrade(
            data,
            by_page.get(i, {}),
            random.Random(master.getrandbits(64)),
            profile,
            mode,
            source_dpi,
            max_pixels=in_pixels[i - 1],
        )
        out_total += res.metadata["size"][0] * res.metadata["size"][1]
        if out_total > min(total, MAX_TOTAL_PIXELS):
            raise DegradeError("document_too_large")
        acc.pages.append(res.page)
        acc.meta.append(res.metadata)
        for path, b in res.boxes.items():
            f = fields.get(path)
            if b is None:
                if f is not None:
                    f["page"], f["bbox"] = None, None
                continue
            new[path] = (i, b)
            if f is not None:
                f["page"], f["bbox"] = i, list(b)
    meta = {"profile": get_profile(profile).name, "mode": mode, "seed": seed, "pages": acc.meta}
    return DegradedDocument(pages=acc.pages, boxes=new, ground_truth=gt, metadata=meta)
