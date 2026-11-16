"""The renderer, outside the jail (documents design §5 step 4; B1-B5,
DS7, DS8). The jailed runs are test_jail.py, in the image's test stage."""

from __future__ import annotations

import json
import os
import struct
import tempfile
import unittest

from docsandbox import render
from tests import samples


def run(data: bytes, mime: str, max_pages: int = 50) -> tuple[str, str]:
    d = tempfile.mkdtemp()
    path = os.path.join(d, "in")
    with open(path, "wb") as f:
        f.write(data)
    out = os.path.join(d, "out")
    os.mkdir(out)
    return render.render(path, out, mime, max_pages), out


def manifest(out: str) -> list[dict[str, int]]:
    with open(os.path.join(out, "manifest.json"), encoding="ascii") as f:
        return json.load(f)["pages"]


def png_chunks(path: str) -> list[bytes]:
    with open(path, "rb") as f:
        b = f.read()
    assert b[:8] == b"\x89PNG\r\n\x1a\n"
    kinds, i = [], 8
    while i < len(b):
        (n,) = struct.unpack(">I", b[i:i + 4])
        kinds.append(b[i + 4:i + 8])
        i += 12 + n
    return kinds


class RenderTest(unittest.TestCase):
    def test_pdf_pages_at_200_dpi(self) -> None:
        code, out = run(samples.pages_pdf(3), "application/pdf")
        self.assertEqual(code, "ok")
        pages = manifest(out)
        self.assertEqual([p["n"] for p in pages], [1, 2, 3])
        # Letter at 200 DPI: 1700 x 2200.
        self.assertEqual((pages[0]["width"], pages[0]["height"]), (1700, 2200))
        for n in (1, 2, 3):
            self.assertTrue(os.path.isfile(os.path.join(out, f"page-{n}.png")))

    def test_pdf_longest_side_capped(self) -> None:
        code, out = run(samples.pages_pdf(1, width=3000, height=1000), "application/pdf")
        self.assertEqual(code, "ok")
        self.assertEqual(max(manifest(out)[0]["width"], manifest(out)[0]["height"]), render.MAX_SIDE)

    def test_b1_too_many_pages(self) -> None:
        self.assertEqual(run(samples.pages_pdf(60), "application/pdf")[0], "too_many_pages")
        # A clearance letter's limit is 20.
        self.assertEqual(run(samples.pages_pdf(21), "application/pdf", 20)[0], "too_many_pages")
        self.assertEqual(run(samples.tiff(4), "image/tiff", 3)[0], "too_many_pages")

    def test_b1_too_many_pixels_from_the_header(self) -> None:
        code, out = run(samples.png_header_only(20_000, 20_000), "image/png")
        self.assertEqual(code, "too_many_pixels")
        self.assertEqual(os.listdir(out), [])

    def test_document_pixel_budget(self) -> None:
        # 3 frames of 9,000 x 9,000 = 243 MP > 200 MP: refused before any decode.
        p = render.Pages(tempfile.mkdtemp())
        p.admit(9000, 9000)
        p.admit(9000, 9000)
        with self.assertRaises(render.Refused) as r:
            p.admit(9000, 9000)
        self.assertEqual(r.exception.reason, "too_many_pixels")

    def test_b2_actions_never_run(self) -> None:
        code, out = run(samples.javascript_pdf(), "application/pdf")
        self.assertEqual(code, "ok")
        self.assertEqual(sorted(os.listdir(out)), ["manifest.json", "page-1.png"])

    def test_b3_decompression_bomb(self) -> None:
        self.assertEqual(run(samples.bomb_pdf(), "application/pdf")[0], "unreadable")

    def test_stream_ratio_allows_ordinary_streams(self) -> None:
        import zlib

        render.check_stream_ratios(b"stream\n" + zlib.compress(os.urandom(4096)) + b"\nendstream")
        render.check_stream_ratios(b"stream\nnot zlib at all\nendstream")
        with self.assertRaises(render.Refused):
            render.check_stream_ratios(b"stream\n" + zlib.compress(b"\0" * 1_000_000) + b"\nendstream")

    def test_ds8_password_protected(self) -> None:
        self.assertEqual(run(samples.password_pdf(), "application/pdf")[0], "password_protected")

    def test_b5_polyglot_and_trailing_payload_give_pixels_only(self) -> None:
        for data, mime in ((samples.pdf_jpeg_polyglot(), "image/jpeg"), (samples.png_trailing_payload(), "image/png")):
            code, out = run(data, mime)
            self.assertEqual(code, "ok", mime)
            self.assertEqual(png_chunks(os.path.join(out, "page-1.png")), [b"IHDR", b"IDAT", b"IEND"])

    def test_images(self) -> None:
        code, out = run(samples.jpeg(640, 480), "image/jpeg")
        self.assertEqual(code, "ok")
        self.assertEqual(manifest(out), [{"n": 1, "width": 640, "height": 480}])
        code, out = run(samples.tiff(3), "image/tiff")
        self.assertEqual(code, "ok")
        self.assertEqual(len(manifest(out)), 3)
        code, out = run(samples.png(5000, 100), "image/png")
        self.assertEqual(code, "ok")
        self.assertEqual(manifest(out)[0]["width"], render.MAX_SIDE)

    def test_wrong_decoder_is_unreadable(self) -> None:
        # A PNG handed over as a JPEG: only the declared format's decoder runs.
        self.assertEqual(run(samples.png(8, 8), "image/jpeg")[0], "unreadable")
        self.assertEqual(run(b"%PDF-1.7\nnot really", "application/pdf")[0], "unreadable")
        self.assertEqual(run(b"PK\x03\x04", "application/zip")[0], "unsupported_type")

    def test_main_exit_codes(self) -> None:
        self.assertEqual(render.main(["render.py"]), render.EXIT_CODES["unreadable"])


if __name__ == "__main__":
    unittest.main()
