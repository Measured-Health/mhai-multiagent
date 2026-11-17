"""Render one uploaded file to PNG pages (documents design §5 step 4).

This file runs inside nsjail, one fresh process per file, with no network,
no credential, a read-only root and only its input file and output
directory mounted (jail.py). It is the only code that parses an uploaded
file. It is self-contained (the standard library, Pillow and pypdfium2) so
that it runs with ``python3 -I`` and nothing on the path but site-packages.

    render.py <input> <output dir> <mime type> <max pages>

On success it writes ``page-<n>.png`` (greyscale or RGB, no metadata) and
``manifest.json`` (``{"pages": [{"n", "width", "height"}]}``) and exits 0.
Otherwise it writes nothing it promises and exits with a closed code
(EXIT_CODES). Everything it writes is untrusted: the documents service
decodes every page again, outside the sandbox, with the same limits.

Never prints anything from the file (its text, its metadata, its names).
"""

from __future__ import annotations

import json
import os
import re
import resource
import sys
import warnings
import zlib

# The exit codes the server maps to the documents service's outcomes.
EXIT_CODES = {
    "ok": 0,
    "too_many_pages": 10,
    "too_many_pixels": 11,
    "password_protected": 12,
    "unreadable": 13,
    "unsupported_type": 14,
}

# §4.1: at most 10,000 x 10,000 px a page, 200 megapixels a document, a
# decompression ratio of at most 100:1 per stream.
MAX_PAGE_PIXELS = 10_000 * 10_000
MAX_DOC_PIXELS = 200_000_000
MAX_RATIO = 100
# §5 step 4: PDF pages at 200 DPI, the longest side at most 4,000 px. Images
# keep their size up to the same longest side.
DPI = 200
MAX_SIDE = 4000
# The in-process limits, the same as the jail's (defence in depth, and the
# limits the tests run under without nsjail): 2 GB of address space, 60 s
# of CPU.
MEMORY_BYTES = 2 << 30
CPU_SECONDS = 60

IMAGE_FORMATS = {"image/png": "PNG", "image/jpeg": "JPEG", "image/tiff": "TIFF"}

_STREAM = re.compile(rb"stream\r?\n")


class Refused(Exception):
    """The file is refused with a closed reason (a key of EXIT_CODES)."""

    def __init__(self, reason: str) -> None:
        super().__init__(reason)
        self.reason = reason


def limit_resources() -> None:
    """Apply the memory and CPU limits to this process."""
    resource.setrlimit(resource.RLIMIT_AS, (MEMORY_BYTES, MEMORY_BYTES))
    resource.setrlimit(resource.RLIMIT_CPU, (CPU_SECONDS, CPU_SECONDS))
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))


def check_stream_ratios(data: bytes, max_ratio: int = MAX_RATIO) -> None:
    """Refuse a PDF with a Flate stream that inflates past max_ratio:1.

    Every ``stream ... endstream`` body is inflated with an output cap of
    max_ratio times its length; one that would produce more is a bomb
    (``unreadable``). A body that isn't zlib data (DCT, CCITT, raw) is left
    to pdfium under the jail's memory and CPU limits.
    """
    pos = 0
    while True:
        m = _STREAM.search(data, pos)
        if m is None:
            return
        start = m.end()
        end = data.find(b"endstream", start)
        if end < 0:
            return
        body = data[start:end]
        pos = end + len(b"endstream")
        if not body:
            continue
        d = zlib.decompressobj()
        try:
            d.decompress(body, max_ratio * len(body))
        except zlib.error:
            continue
        if d.unconsumed_tail:
            raise Refused("unreadable")


def _scale_for(width: float, height: float, base: float) -> float:
    longest = max(width, height) * base
    if longest > MAX_SIDE:
        return base * MAX_SIDE / longest
    return base


class Pages:
    """Counts pixels and writes pages and the manifest."""

    def __init__(self, out_dir: str) -> None:
        self.out_dir = out_dir
        self.pages: list[dict[str, int]] = []
        self.pixels = 0

    def admit(self, width: int, height: int) -> None:
        """Refuse a page past the per-page or per-document pixel limit."""
        if width < 1 or height < 1:
            raise Refused("unreadable")
        if width * height > MAX_PAGE_PIXELS:
            raise Refused("too_many_pixels")
        self.pixels += width * height
        if self.pixels > MAX_DOC_PIXELS:
            raise Refused("too_many_pixels")

    def write(self, image) -> None:  # a PIL.Image.Image
        mode = "L" if image.mode in ("1", "L", "LA", "I", "I;16", "F") else "RGB"
        image = image.convert(mode)
        n = len(self.pages) + 1
        image.save(os.path.join(self.out_dir, f"page-{n}.png"), format="PNG")
        self.pages.append({"n": n, "width": image.width, "height": image.height})

    def finish(self) -> None:
        if not self.pages:
            raise Refused("unreadable")
        with open(os.path.join(self.out_dir, "manifest.json"), "w", encoding="ascii") as f:
            json.dump({"pages": self.pages}, f)


def render_pdf(path: str, out: Pages, max_pages: int) -> None:
    import pypdfium2 as pdfium

    with open(path, "rb") as f:
        check_stream_ratios(f.read())
    try:
        pdf = pdfium.PdfDocument(path)
    except pdfium.PdfiumError as e:
        if "password" in str(e).lower():
            raise Refused("password_protected") from None
        raise Refused("unreadable") from None
    try:
        n = len(pdf)
        if n < 1:
            raise Refused("unreadable")
        if n > max_pages:
            raise Refused("too_many_pages")
        # Sizes first: every page's pixels are counted before any is drawn.
        scales = []
        for i in range(n):
            w, h = pdf[i].get_size()
            s = _scale_for(w, h, DPI / 72)
            out.admit(max(1, round(w * s)), max(1, round(h * s)))
            scales.append(s)
        out.pixels = 0
        for i in range(n):
            page = pdf[i]
            # No form layer and no form environment: JavaScript and actions
            # never run; only the page's pixels come out (B2, B5).
            bitmap = page.render(scale=scales[i], may_draw_forms=False)
            image = bitmap.to_pil()
            out.admit(image.width, image.height)
            out.write(image)
    finally:
        pdf.close()


def render_image(path: str, mime: str, out: Pages, max_pages: int) -> None:
    from PIL import Image

    Image.MAX_IMAGE_PIXELS = MAX_PAGE_PIXELS
    warnings.simplefilter("error", Image.DecompressionBombWarning)
    try:
        im = Image.open(path, formats=[IMAGE_FORMATS[mime]])
    except Image.DecompressionBombError:
        raise Refused("too_many_pixels") from None
    except Exception:
        raise Refused("unreadable") from None
    with im:
        n = getattr(im, "n_frames", 1)
        if n < 1:
            raise Refused("unreadable")
        if n > max_pages:
            raise Refused("too_many_pages")
        for i in range(n):
            im.seek(i)
            # The header's size, before any pixel is decoded (B1).
            out.admit(*im.size)
        out.pixels = 0
        for i in range(n):
            im.seek(i)
            out.admit(*im.size)
            frame = im.copy()
            frame.load()
            s = _scale_for(frame.width, frame.height, 1.0)
            if s < 1.0:
                frame = frame.resize((max(1, round(frame.width * s)), max(1, round(frame.height * s))))
            out.write(frame)


def render(path: str, out_dir: str, mime: str, max_pages: int) -> str:
    """Render path into out_dir; return a key of EXIT_CODES."""
    out = Pages(out_dir)
    try:
        if mime == "application/pdf":
            render_pdf(path, out, max_pages)
        elif mime in IMAGE_FORMATS:
            render_image(path, mime, out, max_pages)
        else:
            raise Refused("unsupported_type")
        out.finish()
    except Refused as r:
        return r.reason
    except Exception:  # noqa: BLE001 - any parser failure is unreadable
        return "unreadable"
    return "ok"


def main(argv: list[str]) -> int:
    if len(argv) != 5 or not argv[4].isdigit():
        return EXIT_CODES["unreadable"]
    limit_resources()
    return EXIT_CODES[render(argv[1], argv[2], argv[3], int(argv[4]))]


if __name__ == "__main__":
    sys.exit(main(sys.argv))
