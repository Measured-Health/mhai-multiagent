"""The real jail (DS9, B2, and the renders of test_render.py through
nsjail). Runs only in the image's test stage, as root with CAP_SYS_ADMIN
(docker run --cap-add SYS_ADMIN --security-opt seccomp=unconfined
--security-opt apparmor=unconfined --network none ...), with
DOCSANDBOX_JAIL_TESTS=1; skipped elsewhere."""

from __future__ import annotations

import json
import os
import shutil
import signal
import tempfile
import unittest
import uuid

from docsandbox import jail, server
from tests import samples

ENABLED = os.environ.get("DOCSANDBOX_JAIL_TESTS") == "1"

PROBE = r"""
import json, os, socket
r = {"uid": os.getuid(), "env": sorted(os.environ), "proc": os.path.exists("/proc/self")}
try:
    socket.socket(socket.AF_INET, socket.SOCK_STREAM).connect(("1.1.1.1", 80))
    r["socket"] = "connected"
except PermissionError:
    r["socket"] = "refused"
except OSError as e:
    r["socket"] = "error %d" % e.errno
try:
    r["jobs"] = os.listdir("/jobs")
except OSError:
    r["jobs"] = None
try:
    open("/job/../%s/in" % OTHER, "rb").read()
    r["other"] = "read"
except OSError:
    r["other"] = None
try:
    open("/usr/x", "wb").write(b"x")
    r["usr_writable"] = True
except OSError:
    r["usr_writable"] = False
try:
    open("/job/in", "ab").write(b"x")
    r["in_writable"] = True
except OSError:
    r["in_writable"] = False
r["root"] = sorted(os.listdir("/"))
json.dump(r, open("/job/out/result.json", "w"))
"""


# Runs mov eax, 20; int 0x80; ret from an executable page.
I386_GETPID = (
    "import mmap\n"
    "code = b'\\xb8\\x14\\x00\\x00\\x00\\xcd\\x80\\xc3'\n"
    "m = mmap.mmap(-1, 4096, prot=mmap.PROT_READ | mmap.PROT_WRITE | mmap.PROT_EXEC)\n"
    "m.write(code)\n"
    "addr = ctypes.addressof(ctypes.c_char.from_buffer(m))\n"
    "ctypes.CFUNCTYPE(ctypes.c_int)(addr)()\n"
)


@unittest.skipUnless(ENABLED, "needs nsjail and the docsandbox image's privileges")
class JailTest(unittest.TestCase):
    def setUp(self) -> None:
        self.jobs = tempfile.mkdtemp(dir="/jobs")
        os.chmod(self.jobs, 0o755)
        self.sb = server.Sandbox(self.jobs, private=tempfile.mkdtemp())

    def tearDown(self) -> None:
        shutil.rmtree(self.jobs, ignore_errors=True)

    def job(self, data: bytes) -> str:
        job = str(uuid.uuid4())
        d = os.path.join(self.jobs, job)
        os.makedirs(os.path.join(d, "out"))
        with open(os.path.join(d, "in"), "wb") as f:
            f.write(data)
        os.chmod(os.path.join(d, "in"), 0o444)
        os.chown(os.path.join(d, "out"), jail.UID, jail.UID)
        os.chmod(d, 0o755)
        return job

    def run_probe(self, source: str) -> tuple[int, str]:
        job = self.job(source.encode())
        d = os.path.join(self.jobs, job)
        return self.sb.run(jail.argv(d, [jail.PYTHON, "-I", "/job/in"])), d

    def test_ping(self) -> None:
        self.assertEqual(self.sb.ping(), "ok")

    def test_ds9_no_network_no_other_job_no_credential(self) -> None:
        other = self.job(b"another job's synthetic file")
        rc, d = self.run_probe("OTHER = %r\n" % other + PROBE)
        self.assertEqual(rc, 0)
        with open(os.path.join(d, "out", "result.json"), encoding="ascii") as f:
            r = json.load(f)
        self.assertEqual(r["uid"], jail.UID)
        self.assertEqual(r["socket"], "refused")
        self.assertIsNone(r["jobs"])
        self.assertIsNone(r["other"])
        self.assertFalse(r["proc"])
        self.assertFalse(r["usr_writable"])
        self.assertFalse(r["in_writable"])
        # Only what the jail sets: the container's environment (which the
        # test run gives a synthetic credential) never reaches the renderer.
        # (LC_CTYPE is Python's own locale coercion.)
        self.assertEqual(set(r["env"]) - {"LC_CTYPE"}, {"HOME", "PYTHONDONTWRITEBYTECODE"})
        self.assertTrue(os.environ.get("SYNTHETIC_CREDENTIAL"), "the test run sets a synthetic credential")
        self.assertNotIn("jobs", r["root"])

    def test_ds9_memory_limit_kills(self) -> None:
        rc, _ = self.run_probe("b = bytearray(3 << 30)\nb[-1] = 1\n")
        self.assertEqual(server.outcome_for(rc), "unreadable")

    def test_calls_outside_the_allow_list_are_killed(self) -> None:
        # The renderer, turned, tries to leave its namespaces or reach
        # io_uring: the seccomp filter kills it (SIGSYS), so nothing runs.
        libc = "import ctypes, os\nlibc = ctypes.CDLL(None, use_errno=True)\n"
        for name, probe in (
            ("unshare(CLONE_NEWUSER)", "os.unshare(0x10000000)"),
            ("unshare(CLONE_NEWNS)", "os.unshare(0x00020000)"),
            ("clone(CLONE_NEWUSER)", "libc.syscall(56, 0x10000000 | 17, 0, 0, 0, 0)"),
            ("io_uring_setup", "libc.syscall(425, 8, ctypes.create_string_buffer(120))"),
            ("fsopen", "libc.syscall(430, b'tmpfs', 0)"),
            ("mount", "libc.mount(b'none', b'/tmp', b'tmpfs', 0, None)"),
            ("ptrace", "libc.ptrace(16, 1, 0, 0)"),
            # The x32 ABI: the same handlers under numbers with bit 0x40000000.
            ("x32 unshare(CLONE_NEWUSER)", "libc.syscall(0x40000000 | 272, 0x10000000)"),
            ("x32 setns", "libc.syscall(0x40000000 | 308, 0, 0)"),
            ("x32 getpid", "libc.syscall(0x40000000 | 39)"),
            # A 32-bit call through int 0x80 (getpid, i386 number 20).
            ("int 0x80", I386_GETPID),
        ):
            rc, d = self.run_probe(libc + probe + "\nopen('/job/out/ran', 'w').write('x')\n")
            # Killed by the filter (SIGSYS), not refused by the kernel.
            self.assertEqual(rc, 128 + signal.SIGSYS, name)
            self.assertFalse(os.path.exists(os.path.join(d, "out", "ran")), name)
        # clone3 is ENOSYS (its flags can't be filtered), so the C library
        # falls back to clone, which the filter reads.
        rc, d = self.run_probe(libc + "r = libc.syscall(435, 0, 0)\n"
                               "open('/job/out/errno', 'w').write(str(ctypes.get_errno()))\n")
        self.assertEqual(rc, 0)
        with open(os.path.join(d, "out", "errno"), encoding="ascii") as f:
            self.assertEqual(f.read(), "38")
        # An ordinary thread (clone without namespace flags) still works.
        rc, _ = self.run_probe("import threading\nt = threading.Thread(target=lambda: None)\nt.start()\nt.join()\n")
        self.assertEqual(rc, 0)

    def test_renders_through_the_jail(self) -> None:
        for data, mime, want in (
            (samples.pages_pdf(2), "application/pdf", "ok"),
            (samples.javascript_pdf(), "application/pdf", "ok"),  # B2
            (samples.password_pdf(), "application/pdf", "password_protected"),  # DS8
            (samples.bomb_pdf(), "application/pdf", "unreadable"),  # B3
            (samples.pages_pdf(60), "application/pdf", "too_many_pages"),  # B1
            (samples.png_header_only(20_000, 20_000), "image/png", "too_many_pixels"),  # B1
            (samples.tiff(2), "image/tiff", "ok"),
            (samples.jpeg(), "image/jpeg", "ok"),
        ):
            job = self.job(data)
            self.assertEqual(self.sb.render(job, mime, 50), want, mime)
            out = os.listdir(os.path.join(self.jobs, job, "out"))
            if want == "ok":
                self.assertIn("manifest.json", out)
            else:
                self.assertNotIn("manifest.json", out)


if __name__ == "__main__":
    unittest.main()
