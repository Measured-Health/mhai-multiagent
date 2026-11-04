"""Intake for real scans, phone photos and volunteer handwriting, registered by fiducials.

People produce the real pages (print and scan, phone photographs, forms filled in by
hand); this module takes them in without guessing. A page is accepted only if

* it decodes as a PNG or JPEG within the size limits;
* exactly four corner fiducials are found (connected components of a locally
  binarised image: a black square frame with a white hole);
* the code strip between them decodes, through the homography the fiducials give, to a
  page id with a valid CRC (the id is read from the printed pattern, not by OCR);
* that id is in the printed manifest and appears on only one image;
* the folder it came from (the slice) agrees with the manifest (a blank form for
  volunteers is a ``form`` entry and nothing else is), and a volunteer page names a
  consent id and a role;
* the synthetic-data scanner finds nothing wrong in the metadata and the ground truth.

Anything else is refused with one code from :data:`REASONS`. A refusal names the file
only by its position in the sorted listing, never by its name or content. EXIF and every
other metadata chunk is dropped: the stored image is a fresh RGB PNG of pixels only.

Volunteer data: only a consent id and a role are recorded, never a name, phone or email.
Metadata holds keyed fields only (ids, counts, sizes), so the scanner can check all of it.
"""

import copy
import io
import json
import math
import os
import re
import stat
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal

import numpy as np
import numpy.typing as npt
from PIL import Image, ImageOps
from scipy import ndimage

from intelligence.doceval import schema
from intelligence.doceval.synth import render, scanner
from intelligence.doceval.synth.render import Box, RenderedDocument

Slice = Literal["real_scan", "phone_photo", "handwritten_volunteer"]
SLICES: tuple[Slice, ...] = ("real_scan", "phone_photo", "handwritten_volunteer")
ROLES = frozenset({"volunteer", "paid_volunteer", "clinician_volunteer"})

MAX_FILE_BYTES = 64 * 1024 * 1024
MAX_SIDE_PX = 10_000
MAX_PIXELS = 25_000_000
MIN_SIDE_PX = 200
MAX_FILES = 5_000
MIN_VISIBLE = 0.8  # fraction of a mapped box that must be in frame
_DETECT_SIDE = 1000  # long side the fiducial search works at
_IMAGE_SUFFIXES = frozenset({".png", ".jpg", ".jpeg"})

REASONS = frozenset(
    {
        "slice_unknown",
        "unsupported_file",
        "unexpected_entry",
        "too_large",
        "unreadable",
        "image_size",
        "fiducials_not_found",
        "fiducials_ambiguous",
        "registration_failed",
        "page_id_unreadable",
        "page_id_unknown",
        "page_id_ambiguous",
        "duplicate_page",
        "slice_mismatch",
        "consent_missing",
        "scanner_refused",
    }
)

_RECORD_RE = re.compile(r"SYN-RC-[A-Z2-7]{8}")
_CONSENT_RE = re.compile(r"CN-[A-Z2-7]{8}")
_DIGITS = re.compile(r"[0-9]{4,}")

_F64 = npt.NDArray[np.float64]


class IntakeError(ValueError):
    """The call itself is wrong (manifest, folder, volunteer table). Carries a code only."""


@dataclass(frozen=True)
class PrintedPage:
    """One printed page as the generator knew it: what intake registers a photo against."""

    page_id: str
    document_id: str  # the id of the document's first page
    page_no: int
    size_px: tuple[int, int]  # width, height of the rendered page
    boxes: Mapping[str, Box]  # this page's fields, page fractions
    ground_truth: Mapping[str, Any]  # the whole document
    form: bool = False  # a blank form a volunteer fills in by hand
    record_id: str | None = None  # forms: the invented record the volunteer copies


@dataclass(frozen=True)
class Volunteer:
    """Consent id and role only. Never a name, phone or email."""

    consent_id: str
    role: str

    def __post_init__(self) -> None:
        if (
            _CONSENT_RE.fullmatch(self.consent_id) is None
            or _DIGITS.search(self.consent_id)
            or self.role not in ROLES
        ):
            raise IntakeError("bad_consent")


@dataclass(frozen=True)
class IngestedImage:
    image: bytes  # PNG pixels only
    boxes: dict[str, Box | None]  # None: the field is out of frame on this photo
    ground_truth: dict[str, Any]
    slice: Slice
    metadata: dict[str, Any]  # keyed fields only; this is what the scanner saw
    unreadable: tuple[str, ...]


@dataclass(frozen=True)
class Refusal:
    label: str  # slice folder and position in the sorted listing, never a file name
    reason: str


@dataclass(frozen=True)
class IngestResult:
    accepted: list[IngestedImage] = field(default_factory=list)
    refused: list[Refusal] = field(default_factory=list)


PrintedManifest = Mapping[str, PrintedPage]
SLICE_BY_NAME: dict[str, Slice] = {s: s for s in SLICES}


# ------------------------------------------------------------------ manifest


def build_manifest(
    doc: RenderedDocument, *, form: bool = False, record_id: str | None = None
) -> dict[str, PrintedPage]:
    """The manifest entries for a document rendered with page ids (``page_id_for``)."""
    if len(doc.page_ids) != len(doc.pages) or not doc.pages:
        raise IntakeError("manifest_invalid")
    if form != (record_id is not None) or (record_id and not _record_ok(record_id)):
        raise IntakeError("manifest_invalid")
    out: dict[str, PrintedPage] = {}
    for n, (data, page_id) in enumerate(zip(doc.pages, doc.page_ids, strict=True), start=1):
        with Image.open(io.BytesIO(data)) as im:
            size = im.size
        out[page_id] = PrintedPage(
            page_id=page_id,
            document_id=doc.page_ids[0],
            page_no=n,
            size_px=(size[0], size[1]),
            boxes={p: b for p, (pg, b) in doc.boxes.items() if pg == n},
            ground_truth=copy.deepcopy(doc.ground_truth),
            form=form,
            record_id=record_id,
        )
    return out


def _record_ok(record_id: str) -> bool:
    return _RECORD_RE.fullmatch(record_id) is not None and _DIGITS.search(record_id) is None


def _check_manifest(manifest: PrintedManifest) -> None:
    if not isinstance(manifest, Mapping) or not manifest:
        raise IntakeError("manifest_invalid")
    for key, page in manifest.items():
        w, h = page.size_px
        ok = (
            key == page.page_id
            and render.page_id_ok(key)
            and render.page_id_ok(page.document_id)
            and page.page_no >= 1
            and MIN_SIDE_PX <= min(w, h)
            and max(w, h) <= MAX_SIDE_PX
            and page.form == (page.record_id is not None)
            and (page.record_id is None or _record_ok(page.record_id))
            and all(
                len(b) == 4
                and 0.0 <= b[0] < b[2] <= 1.0
                and 0.0 <= b[1] < b[3] <= 1.0
                and all(math.isfinite(v) for v in b)
                for b in page.boxes.values()
            )
        )
        if not ok:
            raise IntakeError("manifest_invalid")


# ------------------------------------------------------------------ geometry


def _homography(src: list[tuple[float, float]], dst: list[tuple[float, float]]) -> _F64 | None:
    """The 3x3 homography taking four ``src`` points to ``dst`` (DLT), or ``None``."""
    a = np.zeros((8, 8), dtype=np.float64)
    b = np.zeros(8, dtype=np.float64)
    for i, ((x, y), (u, v)) in enumerate(zip(src, dst, strict=True)):
        a[2 * i] = [x, y, 1, 0, 0, 0, -u * x, -u * y]
        a[2 * i + 1] = [0, 0, 0, x, y, 1, -v * x, -v * y]
        b[2 * i], b[2 * i + 1] = u, v
    if abs(np.linalg.det(a)) < 1e-9 or np.linalg.cond(a) > 1e12:
        return None
    h = np.append(np.linalg.solve(a, b), 1.0).reshape(3, 3)
    return h if bool(np.isfinite(h).all()) else None


def _project(h: _F64, pts: _F64) -> _F64 | None:
    """Apply ``h`` to an (n, 2) array; ``None`` if any point goes behind the camera."""
    v = np.hstack([pts, np.ones((len(pts), 1))]) @ h.T
    if bool((v[:, 2] <= 1e-9).any()):
        return None
    return np.asarray(v[:, :2] / v[:, 2:3], dtype=np.float64)


def _map_box(h: _F64, box: Box, page_w: int, page_h: int, w: int, hh: int) -> Box | None:
    """Page fractions to photo fractions; ``None`` if less than 80% is in frame."""
    x0, y0, x1, y1 = box[0] * page_w, box[1] * page_h, box[2] * page_w, box[3] * page_h
    pts = _project(h, np.array([[x0, y0], [x1, y0], [x1, y1], [x0, y1]], dtype=np.float64))
    if pts is None:
        return None
    bx0, by0 = float(pts[:, 0].min()) - 1.0, float(pts[:, 1].min()) - 1.0
    bx1, by1 = float(pts[:, 0].max()) + 1.0, float(pts[:, 1].max()) + 1.0
    full = (bx1 - bx0) * (by1 - by0)
    cx0, cy0, cx1, cy1 = max(0.0, bx0), max(0.0, by0), min(float(w), bx1), min(float(hh), by1)
    if cx1 <= cx0 or cy1 <= cy0 or full <= 0:
        return None
    if (cx1 - cx0) * (cy1 - cy0) / full < MIN_VISIBLE:
        return None
    return (
        round(cx0 / w, 6),
        round(cy0 / hh, 6),
        round(min(1.0, cx1 / w), 6),
        round(min(1.0, cy1 / hh), 6),
    )


# ---------------------------------------------------------------- fiducials


def _ink_mask(gray: _F64) -> tuple[npt.NDArray[np.bool_], _F64]:
    """Dark marks on locally bright paper, and the paper brightness estimate.

    The paper level is a grey closing wider than a fiducial, so uneven light and a dark
    surface around the sheet do not turn into ink.
    """
    side = max(gray.shape)
    size = max(9, int(side * 0.07)) | 1
    paper = ndimage.grey_closing(gray, size=(size, size))
    paper = ndimage.uniform_filter(paper, size=max(3, size // 3))
    ink = (gray < 0.55 * paper) & (paper > 100.0)
    return ink, np.asarray(paper, dtype=np.float64)


def _candidates(ink: npt.NDArray[np.bool_]) -> list[tuple[float, float, float, float]]:
    """Frames: roughly square, sized like a fiducial, dark all round and white inside.

    Returns ``(x0, y0, x1, y1)`` boxes in the mask's pixels.
    """
    h, w = ink.shape
    labels, count = ndimage.label(ink, structure=np.ones((3, 3), dtype=int))
    if count == 0 or count > 20_000:
        # An image that is mostly specks is not a page we can read.
        return []
    found: list[tuple[float, float, float, float]] = []
    lo, hi = 0.008 * max(w, h), 0.08 * max(w, h)
    for idx, sl in enumerate(ndimage.find_objects(labels), start=1):
        if sl is None:
            continue
        ys, xs = sl
        bh, bw = ys.stop - ys.start, xs.stop - xs.start
        if not (lo <= bw <= hi and lo <= bh <= hi) or not 0.7 <= bw / bh <= 1.43:
            continue
        if ys.start == 0 or xs.start == 0 or ys.stop >= h or xs.stop >= w:
            continue  # cut by the frame: not trustworthy
        comp = labels[sl] == idx
        fill = float(comp.sum()) / (bw * bh)
        if not 0.35 <= fill <= 0.95:
            continue
        cy, cx = bh / 2, bw / 2
        r = max(1, round(min(bw, bh) * 0.07))
        centre = comp[round(cy) - r : round(cy) + r + 1, round(cx) - r : round(cx) + r + 1]
        if centre.size == 0 or float(centre.mean()) > 0.34:
            continue
        reach = 0.36 * min(bw, bh)
        arms = [
            comp[round(cy + dy) - 1 : round(cy + dy) + 2, round(cx + dx) - 1 : round(cx + dx) + 2]
            for dx, dy in ((reach, 0.0), (-reach, 0.0), (0.0, reach), (0.0, -reach))
        ]
        if any(a.size == 0 or float(a.mean()) < 0.55 for a in arms):
            continue
        found.append((float(xs.start), float(ys.start), float(xs.stop), float(ys.stop)))
    return found


def _refine(
    gray: _F64, box: tuple[float, float, float, float], scale: float
) -> tuple[float, float]:
    """The sub-pixel centre of one fiducial, from the full-size image (continuous coordinates)."""
    h, w = gray.shape
    x0, y0, x1, y1 = (v / scale for v in box)
    pad = 0.25 * max(x1 - x0, y1 - y0)
    ix0, iy0 = max(0, int(x0 - pad)), max(0, int(y0 - pad))
    ix1, iy1 = min(w, int(math.ceil(x1 + pad))), min(h, int(math.ceil(y1 + pad)))
    crop = gray[iy0:iy1, ix0:ix1]
    lo, hi = float(np.percentile(crop, 2)), float(np.percentile(crop, 98))
    thr = (lo + hi) / 2
    weight = np.clip((thr - crop) / max(thr - lo, 1.0), 0.0, 1.0)
    total = float(weight.sum())
    if total <= 0:
        return (x0 + x1) / 2, (y0 + y1) / 2
    ys, xs = np.mgrid[0 : crop.shape[0], 0 : crop.shape[1]]
    return (
        ix0 + float((weight * (xs + 0.5)).sum()) / total,
        iy0 + float((weight * (ys + 0.5)).sum()) / total,
    )


def _pick_four(
    cands: list[tuple[float, float, float, float]], w: int, h: int
) -> list[tuple[float, float, float, float]]:
    """The four frames that are the page's corners: one per image quadrant, alike in size,
    spanning the largest quad. Fewer than four candidates, or two equally good sets, refuse."""
    if len(cands) < 4:
        raise _Refuse("fiducials_not_found")
    corners = [(0.0, 0.0), (float(w), 0.0), (float(w), float(h)), (0.0, float(h))]
    quads: list[list[tuple[float, float, float, float]]] = [[], [], [], []]
    for box in cands:
        cx, cy = (box[0] + box[2]) / 2, (box[1] + box[3]) / 2
        q = (0 if cx < w / 2 else 1) if cy < h / 2 else (3 if cx < w / 2 else 2)
        quads[q].append(box)
    for q, group in enumerate(quads):
        if not group:
            raise _Refuse("fiducials_not_found")
        qx, qy = corners[q]
        group.sort(key=lambda b: math.hypot((b[0] + b[2]) / 2 - qx, (b[1] + b[3]) / 2 - qy))
        del group[8:]
    scored: list[tuple[float, tuple[tuple[float, float, float, float], ...]]] = []
    for a in quads[0]:
        for b in quads[1]:
            for c in quads[2]:
                for d in quads[3]:
                    combo = (a, b, c, d)
                    sides = sorted(math.sqrt((x[2] - x[0]) * (x[3] - x[1])) for x in combo)
                    if sides[3] > 1.2 * sides[0]:
                        continue
                    pts = [((x[0] + x[2]) / 2, (x[1] + x[3]) / 2) for x in combo]
                    if not _convex(pts):
                        continue
                    area = 0.5 * abs(
                        sum(
                            pts[i][0] * pts[(i + 1) % 4][1] - pts[(i + 1) % 4][0] * pts[i][1]
                            for i in range(4)
                        )
                    )
                    if area >= 200 * sides[0] ** 2:
                        scored.append((area, combo))
    if not scored:
        raise _Refuse("fiducials_not_found")
    scored.sort(key=lambda t: -t[0])
    best = scored[0]
    for other in scored[1:]:
        if other[0] < 0.995 * best[0]:
            break
        if sum(1 for x in other[1] if x in best[1]) <= 2:
            raise _Refuse("fiducials_ambiguous")
    return list(best[1])


def _find_fiducials(gray: _F64) -> list[tuple[float, float]]:
    """Centres of the fiducial frames in an image; raises :class:`_Refuse` unless exactly four."""
    h, w = gray.shape
    scale = min(1.0, _DETECT_SIDE / max(w, h))
    small = gray
    if scale < 1.0:
        im = Image.fromarray(gray.astype(np.float32), mode="F")
        im = im.resize((max(8, round(w * scale)), max(8, round(h * scale))), Image.Resampling.BOX)
        small = np.asarray(im, dtype=np.float64)
    ink, _ = _ink_mask(small)
    boxes = _pick_four(_candidates(ink), small.shape[1], small.shape[0])
    sx, sy = small.shape[1] / w, small.shape[0] / h
    return [_refine(gray, b, scale=(sx + sy) / 2) for b in boxes]


def _order_clockwise(pts: list[tuple[float, float]]) -> list[tuple[float, float]]:
    cx = sum(p[0] for p in pts) / 4
    cy = sum(p[1] for p in pts) / 4
    return sorted(pts, key=lambda p: math.atan2(p[1] - cy, p[0] - cx))


def _convex(pts: list[tuple[float, float]]) -> bool:
    """Four points in order form a convex quad of real area (all turns the same way)."""
    signs = []
    for i in range(4):
        (ax, ay), (bx, by), (cx, cy) = pts[i], pts[(i + 1) % 4], pts[(i + 2) % 4]
        signs.append((bx - ax) * (cy - by) - (by - ay) * (cx - bx))
    return all(s > 1e-6 for s in signs) or all(s < -1e-6 for s in signs)


class _Refuse(Exception):
    """Internal: one image is refused with ``code``."""

    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


def _read_strip(gray: _F64, paper: _F64, h: _F64, page_w: int) -> str | None:
    """Sample the code strip through ``h`` and return the page id, or ``None``."""
    cells = render.strip_cells(page_w)
    pts = []
    for x0, y0, x1, y1 in cells:
        cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
        dx, dy = (x1 - x0) * 0.2, (y1 - y0) * 0.2
        pts.extend(
            [
                (cx, cy),
                (cx - dx, cy - dy),
                (cx + dx, cy - dy),
                (cx - dx, cy + dy),
                (cx + dx, cy + dy),
            ]
        )
    mapped = _project(h, np.array(pts, dtype=np.float64))
    if mapped is None:
        return None
    gh, gw = gray.shape
    if bool(
        (
            (mapped[:, 0] < 1)
            | (mapped[:, 1] < 1)
            | (mapped[:, 0] > gw - 2)
            | (mapped[:, 1] > gh - 2)
        ).any()
    ):
        return None
    coords = np.array([mapped[:, 1] - 0.5, mapped[:, 0] - 0.5])
    vals = ndimage.map_coordinates(gray, coords, order=1, mode="nearest")
    # paper level was computed at the detection scale
    ph, pw = paper.shape
    pcoords = np.array([(mapped[:, 1] - 0.5) * ph / gh, (mapped[:, 0] - 0.5) * pw / gw])
    pvals = ndimage.map_coordinates(paper, pcoords, order=1, mode="nearest")
    ratio = (vals / np.maximum(pvals, 1.0)).reshape(len(cells), 5).mean(axis=1)
    black, white = float(ratio[0]), float(ratio[1])
    if white - black < 0.3:
        return None
    thr = (black + white) / 2
    bits = [1 if r < thr else 0 for r in ratio[2:]]
    return render.page_id_from_bits(bits)


# ------------------------------------------------------------------- reading


def _load(full: Path) -> Image.Image:
    """Decode within limits and return fresh RGB pixels (no metadata of any kind)."""
    try:
        with Image.open(full) as im:
            if im.format not in ("PNG", "JPEG"):
                raise _Refuse("unreadable")
            w, h = im.size
            if min(w, h) < MIN_SIDE_PX or max(w, h) > MAX_SIDE_PX or w * h > MAX_PIXELS:
                raise _Refuse("image_size")
            im.load()
            turned = ImageOps.exif_transpose(im) or im  # honour a phone's rotation flag
            rgb = turned.convert("RGB")
        return Image.frombytes("RGB", rgb.size, rgb.tobytes())
    except _Refuse:
        raise
    except Exception as e:  # noqa: BLE001 - any decoder failure is one fixed code
        raise _Refuse("unreadable") from e


def _png(im: Image.Image) -> bytes:
    out = io.BytesIO()
    im.save(out, format="PNG", compress_level=6)  # IHDR, IDAT, IEND only
    return out.getvalue()


@dataclass
class _Found:
    label: str
    slice: Slice
    rel: str
    img: Image.Image
    page: PrintedPage
    h: _F64


def _register(img: Image.Image, manifest: PrintedManifest) -> tuple[PrintedPage, _F64]:
    gray = np.asarray(img.convert("L"), dtype=np.float64)
    centres = _order_clockwise(_find_fiducials(gray))
    if not _convex(centres):
        raise _Refuse("registration_failed")
    # The strip is read at the detection scale's paper level; recompute it once.
    scale = min(1.0, _DETECT_SIDE / max(gray.shape))
    if scale < 1.0:
        sm = Image.fromarray(gray.astype(np.float32), mode="F").resize(
            (max(8, round(gray.shape[1] * scale)), max(8, round(gray.shape[0] * scale))),
            Image.Resampling.BOX,
        )
        small = np.asarray(sm, dtype=np.float64)
    else:
        small = gray
    _, paper = _ink_mask(small)
    # The page's width and height are not known until its id is, so try the page sizes in
    # the manifest: one homography per distinct size and rotation.
    sizes = sorted({p.size_px for p in manifest.values()})
    readable = False
    hits: dict[str, tuple[PrintedPage, _F64]] = {}
    for width, height in sizes:
        src = render.fiducial_centres(width, height)
        for shift in range(4):
            dst = centres[shift:] + centres[:shift]
            hom = _homography(src, dst)
            if hom is None:
                continue
            page_id = _read_strip(gray, paper, hom, width)
            if page_id is None:
                continue
            readable = True
            page = manifest.get(page_id)
            if page is not None and page.size_px == (width, height):
                hits[page_id] = (page, hom)
    if len(hits) > 1:
        raise _Refuse("page_id_ambiguous")
    if not hits:
        raise _Refuse("page_id_unknown" if readable else "page_id_unreadable")
    ((page, hom),) = hits.values()
    return page, hom


# -------------------------------------------------------------------- ingest


def _listing(root: Path) -> list[tuple[str, str, Path]]:
    """``(slice_name, rel, path)`` for every entry one level down, sorted, nothing followed."""
    if root.is_symlink() or not root.is_dir():
        raise IntakeError("images_dir")
    out: list[tuple[str, str, Path]] = []
    with os.scandir(root) as top:
        entries = sorted(top, key=lambda e: e.name)
    for entry in entries:
        mode = entry.stat(follow_symlinks=False).st_mode
        if stat.S_ISDIR(mode):
            with os.scandir(entry.path) as inner:
                for sub in sorted(inner, key=lambda e: e.name):
                    out.append((entry.name, f"{entry.name}/{sub.name}", Path(sub.path)))
        else:
            out.append(("", entry.name, Path(entry.path)))
        if len(out) > MAX_FILES:
            raise IntakeError("too_many_files")
    return out


def _metadata(
    page: PrintedPage, sl: Slice, volunteer: Volunteer | None, w: int, h: int, unreadable: int
) -> dict[str, Any]:
    meta: dict[str, Any] = {
        "page_id": page.page_id,
        "document_id": page.document_id,
        "page_no": page.page_no,
        "slice": sl,
        "width": w,
        "height": h,
        "unreadable_count": unreadable,
    }
    if page.record_id is not None:
        meta["record_id"] = page.record_id
    if volunteer is not None:
        meta["consent_id"] = volunteer.consent_id
        meta["role"] = volunteer.role
    return meta


def _ground_truth(page: PrintedPage, boxes: dict[str, Box | None]) -> dict[str, Any]:
    """The document's ground truth with page and box following this photo.

    A field on another page of the document is not visible here: its page and box are null.
    """
    gt: dict[str, Any] = copy.deepcopy(dict(page.ground_truth))
    for path, fld in schema.iter_fields(gt):
        box = boxes.get(path)
        if box is None:
            fld["page"], fld["bbox"] = None, None
        else:
            fld["page"], fld["bbox"] = 1, list(box)
    return gt


def ingest(
    images_dir: str | Path,
    printed_manifest: PrintedManifest,
    *,
    volunteers: Mapping[str, Volunteer] | None = None,
) -> IngestResult:
    """Register every image under ``images_dir`` against the printed manifest.

    ``images_dir`` holds one folder per slice (``real_scan``, ``phone_photo``,
    ``handwritten_volunteer``), images directly inside. ``volunteers`` maps a relative
    path (``handwritten_volunteer/<file>``) to that page's consent id and role.
    The result lists the pages accepted and the files refused (by position, with a code).
    A page id seen on two images refuses both.
    """
    _check_manifest(printed_manifest)
    root = Path(images_dir)
    result = IngestResult()
    found: list[_Found] = []
    for index, (folder, rel, full) in enumerate(_listing(root)):
        label = f"{folder or 'root'}[{index}]"
        try:
            if folder not in SLICES:
                raise _Refuse("slice_unknown")
            mode = os.lstat(full).st_mode
            if not stat.S_ISREG(mode):
                raise _Refuse("unexpected_entry")
            if full.suffix.lower() not in _IMAGE_SUFFIXES:
                raise _Refuse("unsupported_file")
            if os.lstat(full).st_size > MAX_FILE_BYTES:
                raise _Refuse("too_large")
            img = _load(full)
            page, hom = _register(img, printed_manifest)
            found.append(_Found(label, SLICE_BY_NAME[folder], rel, img, page, hom))
        except _Refuse as e:
            result.refused.append(Refusal(label, e.code))

    counts: dict[str, int] = {}
    for item in found:
        counts[item.page.page_id] = counts.get(item.page.page_id, 0) + 1
    for item in sorted(found, key=lambda f: (f.slice, f.page.page_id, f.label)):
        try:
            if counts[item.page.page_id] > 1:
                raise _Refuse("duplicate_page")
            result.accepted.append(_accept(item, volunteers))
        except _Refuse as e:
            result.refused.append(Refusal(item.label, e.code))
    return result


def _accept(item: _Found, volunteers: Mapping[str, Volunteer] | None) -> IngestedImage:
    page = item.page
    if page.form != (item.slice == "handwritten_volunteer"):
        raise _Refuse("slice_mismatch")
    volunteer = (volunteers or {}).get(item.rel)
    if item.slice == "handwritten_volunteer" and volunteer is None:
        raise _Refuse("consent_missing")
    if item.slice != "handwritten_volunteer":
        volunteer = None
    w, h = item.img.size
    pw, ph = page.size_px
    boxes: dict[str, Box | None] = {
        path: _map_box(item.h, box, pw, ph, w, h) for path, box in page.boxes.items()
    }
    unreadable = tuple(sorted(p for p, b in boxes.items() if b is None))
    gt = _ground_truth(page, boxes)
    meta = _metadata(page, item.slice, volunteer, w, h, len(unreadable))
    if scanner.check_metadata(meta) or scanner.check_document(gt):
        raise _Refuse("scanner_refused")
    return IngestedImage(
        image=_png(item.img),
        boxes=boxes,
        ground_truth=gt,
        slice=item.slice,
        metadata=meta,
        unreadable=unreadable,
    )


def write_result(result: IngestResult, out_dir: str | Path) -> list[str]:
    """Write each accepted image as ``<page id>.png`` with ``<page id>.json`` (ground truth)
    and ``<page id>.meta.json`` (metadata), never overwriting. Returns the page ids.

    The folder passes :func:`scanner.scan_set`; a failing scan raises ``IntakeError``.
    """
    root = Path(out_dir)
    root.mkdir(parents=True, exist_ok=True)
    ids: list[str] = []
    for item in result.accepted:
        stem = str(item.metadata["page_id"])
        files = {
            f"{stem}.png": item.image,
            f"{stem}.json": _dump(item.ground_truth),
            f"{stem}.meta.json": _dump(item.metadata),
        }
        for name, payload in files.items():
            with open(root / name, "xb") as fh:
                fh.write(payload)
        ids.append(stem)
    if ids and not scanner.scan_set(root).ok:
        raise IntakeError("scanner_refused")
    return ids


def _dump(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, ensure_ascii=True, allow_nan=False).encode("ascii")


__all__ = [
    "REASONS",
    "SLICES",
    "IngestResult",
    "IngestedImage",
    "IntakeError",
    "PrintedManifest",
    "PrintedPage",
    "Refusal",
    "Volunteer",
    "build_manifest",
    "ingest",
    "write_result",
]
