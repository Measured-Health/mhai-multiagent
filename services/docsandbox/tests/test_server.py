"""The server's protocol and job checks, with the jail replaced."""

from __future__ import annotations

import json
import os
import socket
import tempfile
import threading
import unittest
import uuid
from unittest import mock

from docsandbox import jail, server

JOB = "0a1b2c3d-0000-4000-8000-00000000abcd"


def make_job(jobs: str, job: str = JOB) -> str:
    d = os.path.join(jobs, job)
    os.makedirs(os.path.join(d, "out"))
    with open(os.path.join(d, "in"), "wb") as f:
        f.write(b"%PDF-1.7\n")
    return d


class ServerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.jobs = tempfile.mkdtemp()
        self.calls: list[list[str]] = []
        self.rc = 0

        def run(args: list[str]) -> int:
            self.calls.append(args)
            return self.rc

        self.sb = server.Sandbox(self.jobs, run=run)

    def req(self, **kw: object) -> str:
        return self.sb.handle(json.dumps(kw).encode())["outcome"]

    def test_render_runs_the_renderer_in_the_jail(self) -> None:
        d = make_job(self.jobs)
        self.assertEqual(self.req(op="render", job=JOB, mime="application/pdf", max_pages=50), "ok")
        args = self.calls[0]
        self.assertEqual(args[0], jail.NSJAIL)
        self.assertIn(os.path.join(d, "in") + ":/job/in", args)
        self.assertIn(os.path.join(d, "out") + ":/job/out", args)
        self.assertEqual(args[args.index("--") + 1:], jail.render_program("application/pdf", 50))
        # No network interface, no /proc, no user's environment.
        for flag in ("--iface_no_lo", "--disable_proc", "--seccomp_string"):
            self.assertIn(flag, args)
        self.assertNotIn("--keep_env", args)
        self.assertEqual(args[args.index("--rlimit_as") + 1], "2048")
        self.assertEqual(args[args.index("--rlimit_cpu") + 1], "60")

    def test_exit_codes(self) -> None:
        make_job(self.jobs)
        for rc, want in ((10, "too_many_pages"), (11, "too_many_pixels"), (12, "password_protected"),
                         (13, "unreadable"), (14, "unsupported_type"), (1, "unreadable"), (137, "unreadable"),
                         (159, "unreadable"), (255, "unavailable")):
            self.rc = rc
            self.assertEqual(self.req(op="render", job=JOB, mime="image/png", max_pages=1), want, rc)

    def test_bad_requests(self) -> None:
        make_job(self.jobs)
        for kw in (dict(op="render", job="../etc", mime="image/png", max_pages=1),
                   dict(op="render", job=JOB.upper(), mime="image/png", max_pages=1),
                   dict(op="render", job=JOB, mime="text/html", max_pages=1),
                   dict(op="render", job=JOB, mime="image/png", max_pages=51),
                   dict(op="render", job=JOB, mime="image/png", max_pages=True),
                   dict(op="render", job=JOB, mime="image/png", max_pages=1, extra=1),
                   dict(op="shell")):
            self.assertEqual(self.req(**kw), "bad_request", kw)
        self.assertEqual(self.sb.handle(b"not json")["outcome"], "bad_request")
        self.assertEqual(self.calls, [])

    def test_job_must_be_plain(self) -> None:
        other = tempfile.mkdtemp()
        os.symlink(other, os.path.join(self.jobs, JOB))
        self.assertIsNone(server.job_dir(self.jobs, JOB))
        os.unlink(os.path.join(self.jobs, JOB))
        d = make_job(self.jobs)
        os.unlink(os.path.join(d, "in"))
        os.symlink("/etc/passwd", os.path.join(d, "in"))
        self.assertIsNone(server.job_dir(self.jobs, JOB))
        os.unlink(os.path.join(d, "in"))
        with open(os.path.join(d, "in"), "wb"):
            pass
        with open(os.path.join(d, "out", "page-1.png"), "wb"):
            pass
        self.assertIsNone(server.job_dir(self.jobs, JOB))  # out must be empty

    def test_ping_runs_a_jailed_selftest_and_caches_it(self) -> None:
        chown = mock.patch("os.chown")  # not root in the unit test
        chown.start()
        self.addCleanup(chown.stop)
        self.assertEqual(self.req(op="ping"), "ok")
        self.rc = 255
        self.assertEqual(self.req(op="ping"), "ok")  # cached
        self.sb._selftest_at = 0
        self.assertEqual(self.req(op="ping"), "unavailable")

    def test_socket_round_trip(self) -> None:
        path = os.path.join(tempfile.mkdtemp(), "s.sock")
        srv = server.Server(path, server.make_handler(self.sb))
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            make_job(self.jobs)
            for line, want in ((json.dumps({"op": "render", "job": JOB, "mime": "image/png", "max_pages": 1}), "ok"),
                               ("x" * (server.MAX_LINE + 10), "bad_request")):
                s = socket.socket(socket.AF_UNIX)
                s.connect(path)
                s.sendall(line.encode() + b"\n")
                self.assertEqual(json.loads(s.makefile().readline())["outcome"], want)
                s.close()
        finally:
            srv.shutdown()
            srv.server_close()


if __name__ == "__main__":
    unittest.main()
