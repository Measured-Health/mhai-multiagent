"""The server's protocol, job checks and staging, with the jail replaced."""

from __future__ import annotations

import json
import os
import shutil
import socket
import tempfile
import threading
import unittest

from docsandbox import jail, server

JOB = "0a1b2c3d-0000-4000-8000-00000000abcd"
FILE = b"%PDF-1.7\nsynthetic\n"


def make_job(jobs: str, job: str = JOB) -> str:
    d = os.path.join(jobs, job)
    os.makedirs(os.path.join(d, "out"))
    with open(os.path.join(d, "in"), "wb") as f:
        f.write(FILE)
    return d


def bound(args: list[str], inside: str) -> str:
    """The host path args bind at inside."""
    for a in args:
        if a.endswith(":" + inside):
            return a[: -len(inside) - 1]
    raise AssertionError(inside + " not bound")


class ServerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.jobs = tempfile.mkdtemp()
        self.private = tempfile.mkdtemp()
        self.calls: list[list[str]] = []
        self.rc = 0
        self.writes: dict[str, bytes | str] = {"manifest.json": b'{"pages": []}', "page-1.png": b"png"}
        self.during = None

        def run(args: list[str]) -> int:
            self.calls.append(args)
            if self.during:
                self.during(args)
            if "--" in args and args[args.index("--") + 1:][:3] == [jail.PYTHON, "-I", jail.RENDERER]:
                out = bound(args, "/job/out")
                for name, v in self.writes.items():
                    if isinstance(v, str):  # a link the renderer made
                        os.symlink(v, os.path.join(out, name))
                    else:
                        with open(os.path.join(out, name), "wb") as f:
                            f.write(v)
            return self.rc

        self.sb = server.Sandbox(self.jobs, run=run, private=self.private)

    def req(self, **kw: object) -> str:
        return self.sb.handle(json.dumps(kw).encode())["outcome"]

    def render(self) -> str:
        return self.req(op="render", job=JOB, mime="application/pdf", max_pages=50)

    def test_render_runs_the_renderer_in_the_jail_on_a_private_copy(self) -> None:
        d = make_job(self.jobs)
        seen: dict[str, bytes] = {}

        def during(args: list[str]) -> None:
            with open(bound(args, "/job/in"), "rb") as f:
                seen["in"] = f.read()

        self.during = during
        self.assertEqual(self.render(), "ok")
        args = self.calls[0]
        self.assertEqual(args[0], jail.NSJAIL)
        # Only root's private staging is bound, never the shared tmpfs.
        for inside in ("/job/in", "/job/out"):
            self.assertTrue(bound(args, inside).startswith(self.private + os.sep), inside)
        self.assertFalse(any(self.jobs in a for a in args))
        self.assertEqual(seen["in"], FILE)
        self.assertEqual(args[args.index("--") + 1:], jail.render_program("application/pdf", 50))
        for flag in ("--iface_no_lo", "--disable_proc", "--seccomp_string"):
            self.assertIn(flag, args)
        self.assertNotIn("--keep_env", args)
        self.assertEqual(args[args.index("--rlimit_as") + 1], "2048")
        self.assertEqual(args[args.index("--rlimit_cpu") + 1], "60")
        # The pages come back into the job's out; the staging is gone.
        self.assertEqual(sorted(os.listdir(os.path.join(d, "out"))), ["manifest.json", "page-1.png"])
        self.assertEqual(os.listdir(self.private), [])

    def test_exit_codes(self) -> None:
        for rc, want in ((10, "too_many_pages"), (11, "too_many_pixels"), (12, "password_protected"),
                         (13, "unreadable"), (14, "unsupported_type"), (1, "unreadable"), (137, "unreadable"),
                         (159, "unreadable"), (255, "unavailable")):
            shutil.rmtree(os.path.join(self.jobs, JOB), ignore_errors=True)
            make_job(self.jobs)
            self.rc = rc
            self.assertEqual(self.render(), want, rc)

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

    def test_symlinked_job_in_or_out_is_refused(self) -> None:
        victim = tempfile.mkdtemp()
        with open(os.path.join(victim, "secret"), "wb") as f:
            f.write(b"synthetic secret")
        # The job directory itself a link.
        os.symlink(victim, os.path.join(self.jobs, JOB))
        self.assertEqual(self.render(), "bad_request")
        os.unlink(os.path.join(self.jobs, JOB))
        # in a link to a file elsewhere.
        d = make_job(self.jobs)
        os.unlink(os.path.join(d, "in"))
        os.symlink(os.path.join(victim, "secret"), os.path.join(d, "in"))
        self.assertEqual(self.render(), "bad_request")
        # out a link to a directory elsewhere.
        os.unlink(os.path.join(d, "in"))
        with open(os.path.join(d, "in"), "wb") as f:
            f.write(FILE)
        os.rmdir(os.path.join(d, "out"))
        os.symlink(victim, os.path.join(d, "out"))
        self.assertEqual(self.render(), "bad_request")
        # A FIFO for in.
        os.unlink(os.path.join(d, "out"))
        os.mkdir(os.path.join(d, "out"))
        os.unlink(os.path.join(d, "in"))
        os.mkfifo(os.path.join(d, "in"))
        self.assertEqual(self.render(), "bad_request")
        # out not empty.
        os.unlink(os.path.join(d, "in"))
        with open(os.path.join(d, "in"), "wb") as f:
            f.write(FILE)
        with open(os.path.join(d, "out", "page-1.png"), "wb"):
            pass
        self.assertEqual(self.render(), "bad_request")
        self.assertEqual(self.calls, [])
        self.assertEqual(os.listdir(victim), ["secret"])

    def test_names_swapped_for_links_after_the_check_change_nothing(self) -> None:
        d = make_job(self.jobs)
        victim = tempfile.mkdtemp()

        def during(args: list[str]) -> None:
            # Another container swaps in and out for links while the jail runs.
            os.rename(os.path.join(d, "out"), os.path.join(d, "out-old"))
            os.symlink(victim, os.path.join(d, "out"))
            os.unlink(os.path.join(d, "in"))
            os.symlink("/etc/passwd", os.path.join(d, "in"))

        self.during = during
        self.assertEqual(self.render(), "ok")
        self.assertEqual(os.listdir(victim), [])
        # The pages went into the out that was checked.
        self.assertEqual(sorted(os.listdir(os.path.join(d, "out-old"))), ["manifest.json", "page-1.png"])

    def test_links_the_renderer_makes_are_never_followed(self) -> None:
        secret = os.path.join(tempfile.mkdtemp(), "secret")
        with open(secret, "wb") as f:
            f.write(b"synthetic secret")
        for writes in ({"manifest.json": b"{}", "page-1.png": secret},
                       {"manifest.json": secret},
                       {"manifest.json": b"{}", "page-1.png": b"x", "extra.sh": b"x"},
                       {"page-1.png": b"x"}):
            shutil.rmtree(os.path.join(self.jobs, JOB), ignore_errors=True)
            d = make_job(self.jobs)
            self.writes = writes
            self.assertEqual(self.render(), "unreadable", writes)
            for name in os.listdir(os.path.join(d, "out")):
                with open(os.path.join(d, "out", name), "rb") as f:
                    self.assertNotIn(b"secret", f.read())

    def test_ping_runs_a_jailed_selftest_in_private_and_caches_it(self) -> None:
        victim = tempfile.mkdtemp()
        os.symlink(victim, os.path.join(self.jobs, ".selftest"))
        self.assertEqual(self.req(op="ping"), "ok")
        self.assertTrue(bound(self.calls[0], "/job/out").startswith(self.private + os.sep))
        self.assertEqual(os.listdir(victim), [])
        self.assertEqual(os.listdir(self.private), [])
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

    def test_seccomp_is_an_allow_list(self) -> None:
        self.assertIn("USE docsandbox DEFAULT KILL", jail.SECCOMP)
        self.assertNotIn("DEFAULT ALLOW", jail.SECCOMP)
        for name in ("unshare", "setns", "mount", "ptrace", "io_uring_setup", "fsopen", "open_tree", "move_mount",
                     "pidfd_getfd", "bpf", "keyctl", "process_vm_readv", "chroot", "pivot_root"):
            self.assertNotIn(name, jail.ALLOWED)
        self.assertIn("ERRNO(38) {\n    clone3", jail.SECCOMP)
        self.assertIn("clone(flags) { (flags & 0x7e020080) == 0 }", jail.SECCOMP)


if __name__ == "__main__":
    unittest.main()
