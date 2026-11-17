"""Synthetic test files, generated in the test: never a real document.

A tiny PDF writer (enough objects for pdfium to open), and the hostile
shapes of the abuse cases B1-B5 and DS7-DS9.
"""

from __future__ import annotations

import io
import zlib


def pdf(objects: list[bytes], root: int = 1, trailer_extra: bytes = b"") -> bytes:
    """A PDF whose object i+1 is objects[i], with a correct xref."""
    out = io.BytesIO()
    out.write(b"%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
    offsets = []
    for i, body in enumerate(objects, start=1):
        offsets.append(out.tell())
        out.write(b"%d 0 obj\n" % i + body + b"\nendobj\n")
    xref = out.tell()
    out.write(b"xref\n0 %d\n0000000000 65535 f \n" % (len(objects) + 1))
    for off in offsets:
        out.write(b"%010d 00000 n \n" % off)
    out.write(b"trailer\n<< /Size %d /Root %d 0 R " % (len(objects) + 1, root) + trailer_extra + b">>\n")
    out.write(b"startxref\n%d\n%%%%EOF\n" % xref)
    return out.getvalue()


def stream(data: bytes, extra: bytes = b"") -> bytes:
    return b"<< /Length %d " % len(data) + extra + b">>\nstream\n" + data + b"\nendstream"


def pages_pdf(n: int, width: int = 612, height: int = 792, content: bytes = b"0 0 1 rg 72 72 100 100 re f") -> bytes:
    """n pages, each a filled square (the content stream shared)."""
    kids = b" ".join(b"%d 0 R" % (4 + i) for i in range(n))
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [" + kids + b"] /Count %d >>" % n,
        stream(content),
    ]
    for _ in range(n):
        objs.append(b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents 3 0 R >>" % (width, height))
    return pdf(objs)


def javascript_pdf() -> bytes:
    """B2: an open action running JavaScript, a launch action, an embedded
    file and a form field. Only the page's pixels may come out."""
    js = b"app.launchURL('http://example.invalid/x', true); this.submitForm('http://example.invalid/f');"
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R /OpenAction 5 0 R /Names << /EmbeddedFiles << /Names [(a.exe) 7 0 R] >> >>"
        b" /AcroForm << /Fields [8 0 R] >> >>",
        b"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
        stream(b"0 g 72 72 200 200 re f"),
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 3 0 R /AA << /O 6 0 R >> /Annots [8 0 R] >>",
        b"<< /S /JavaScript /JS (" + js + b") >>",
        b"<< /S /Launch /F (cmd.exe) >>",
        b"<< /Type /Filespec /F (a.exe) /EF << /F 9 0 R >> >>",
        b"<< /FT /Tx /T (f) /V (synthetic) /Rect [300 300 500 330] /Subtype /Widget /P 4 0 R >>",
        stream(b"MZ synthetic embedded payload", b"/Type /EmbeddedFile "),
    ]
    return pdf(objs)


def password_pdf() -> bytes:
    """DS8: a PDF under the standard security handler with a user password.
    The O and U entries are synthetic, so no password (not even empty)
    opens it."""
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
        b"<< /Filter /Standard /V 1 /R 2 /O <" + b"11" * 32 + b"> /U <" + b"22" * 32 + b"> /P -44 >>",
    ]
    return pdf(objs, trailer_extra=b"/Encrypt 4 0 R /ID [<" + b"33" * 16 + b"> <" + b"33" * 16 + b">] ")


def bomb_pdf(inflated: int = 64 << 20) -> bytes:
    """B3: a page whose content stream inflates past 100:1 (zeros)."""
    comp = zlib.compress(b"\0" * inflated, 9)
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
        stream(comp, b"/Filter /FlateDecode "),
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 3 0 R >>",
    ]
    return pdf(objs)


def png(width: int, height: int, mode: str = "RGB") -> bytes:
    from PIL import Image

    b = io.BytesIO()
    Image.new(mode, (width, height), 200).save(b, format="PNG")
    return b.getvalue()


def png_header_only(width: int, height: int) -> bytes:
    """B1: a PNG whose header claims width x height, with one tiny IDAT:
    refused from the header, before any pixel is decoded."""
    import struct

    def chunk(kind: bytes, data: bytes) -> bytes:
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(b"\0")) + chunk(b"IEND", b"")


def jpeg(width: int = 64, height: int = 48) -> bytes:
    from PIL import Image

    b = io.BytesIO()
    Image.new("RGB", (width, height), (10, 120, 200)).save(b, format="JPEG")
    return b.getvalue()


def tiff(frames: int, width: int = 64, height: int = 48) -> bytes:
    from PIL import Image

    b = io.BytesIO()
    imgs = [Image.new("L", (width, height), 30 * i % 255) for i in range(frames)]
    imgs[0].save(b, format="TIFF", save_all=True, append_images=imgs[1:])
    return b.getvalue()


def pdf_jpeg_polyglot() -> bytes:
    """B5: a JPEG with a whole PDF in a comment segment: a JPEG to a JPEG
    decoder, and readers that look for %PDF anywhere see a PDF."""
    j = jpeg()
    body = pages_pdf(1)[:65000]
    com = b"\xff\xfe" + (len(body) + 2).to_bytes(2, "big") + body
    return j[:2] + com + j[2:]


def png_trailing_payload() -> bytes:
    """B5: a valid PNG with bytes after IEND."""
    return png(32, 32) + b"<script>synthetic trailing payload</script>" * 10
