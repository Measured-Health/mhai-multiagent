"""The docsandbox server: renders jobs for the documents service.

It listens on a Unix socket in the jobs directory, the tmpfs both
containers mount (the docsandbox container has no network at all). One
request per connection, one JSON line each way, at most 4 KiB:

    {"op": "ping"}                                        -> {"outcome": "ok"} or {"outcome": "unavailable"}
    {"op": "render", "job": "<uuid>", "mime": "...", "max_pages": n}
                                                          -> {"outcome": "<code>"}

The documents service makes ``<jobs>/<job>/in`` (the file) and
``<jobs>/<job>/out`` (an empty directory) before it asks. The outcome is
``ok`` (pages and manifest in out), a refusal (``too_many_pages``,
``too_many_pixels``, ``password_protected``, ``unreadable``,
``unsupported_type``), ``unavailable`` (the jail itself didn't run: the
document waits, never a refusal) or ``bad_request``. Nothing from a file
is ever logged or returned.

The server runs as root (nsjail needs it to build each jail), so it never
acts on a path in the shared tmpfs, where another container can swap a
name for a link between a check and a use. It opens the job directory,
``in`` and ``out`` once, without following links, and works from those
descriptors only: it copies ``in`` into a fresh staging directory under
PRIVATE (root's, mode 0700, not shared), binds only the staging copies
into the jail, and copies back only ``manifest.json`` and ``page-<n>.png``
that are regular files (never through a link the renderer made), into the
``out`` it opened. The self-test runs in PRIVATE too.

At most MAX_JOBS renders run at once (design §12: two files, 2 GB each).
"""

from __future__ import annotations

import errno
import json
import os
import re
import shutil
import socket
import socketserver
import stat
import subprocess
import sys
import tempfile
import threading
import time

from docsandbox import jail

JOBS = "/jobs"
PRIVATE = "/run/docsandbox"
SOCKET_NAME = "sandbox.sock"
MAX_LINE = 4096
MAX_JOBS = 2
MAX_INPUT = 25 << 20
MAX_OUTPUT_FILE = 64 << 20
MIMES = ("application/pdf", "image/png", "image/jpeg", "image/tiff")
_JOB = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
_OUTPUT = re.compile(r"^(manifest\.json|page-[1-9][0-9]?\.png)$")
# nsjail's own failure (it couldn't build the jail) is 255; the renderer
# never exits with it.
NSJAIL_FAILED = 255
SELFTEST_TTL = 60.0
NOFOLLOW = os.O_NOFOLLOW | os.O_CLOEXEC

CODES = {v: k for k, v in {
    "ok": 0,
    "too_many_pages": 10,
    "too_many_pixels": 11,
    "password_protected": 12,
    "unreadable": 13,
    "unsupported_type": 14,
}.items()}


class BadJob(Exception):
    """The job isn't a plain directory with a regular in and an empty out."""


def outcome_for(returncode: int) -> str:
    """Map the jail's exit status to an outcome. A kill (any limit) or a
    crash is unreadable; nsjail failing to start the jail is unavailable."""
    if returncode == NSJAIL_FAILED:
        return "unavailable"
    return CODES.get(returncode, "unreadable")


def _chown(fd: int) -> None:
    # The jailed renderer runs as jail.UID; root (production) hands it the
    # descriptor's directory. Unprivileged (the unit tests) it is ours.
    if os.geteuid() == 0:
        os.fchown(fd, jail.UID, jail.UID)


class Job:
    """A job's directory, in and out, opened once without following links."""

    def __init__(self, jobs: str, job: str) -> None:
        if not isinstance(job, str) or not _JOB.match(job):
            raise BadJob
        self.dir = self.in_ = self.out = -1
        try:
            jobs_fd = os.open(jobs, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            try:
                self.dir = os.open(job, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW, dir_fd=jobs_fd)
            finally:
                os.close(jobs_fd)
            self.in_ = os.open("in", os.O_RDONLY | os.O_NONBLOCK | NOFOLLOW, dir_fd=self.dir)
            st = os.fstat(self.in_)
            if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_INPUT:
                raise BadJob
            self.out = os.open("out", os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW, dir_fd=self.dir)
            if os.listdir(self.out):
                raise BadJob
        except OSError:
            self.close()
            raise BadJob from None
        except BadJob:
            self.close()
            raise

    def close(self) -> None:
        for fd in (self.in_, self.out, self.dir):
            if fd >= 0:
                os.close(fd)
        self.dir = self.in_ = self.out = -1

    def stage(self, staging: str) -> None:
        """Copy in into staging/in (read-only) and make staging/out, the
        renderer's, in root's private directory."""
        dst = os.open(os.path.join(staging, "in"), os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW, 0o444)
        try:
            os.lseek(self.in_, 0, os.SEEK_SET)
            left = MAX_INPUT + 1
            while left > 0:
                b = os.read(self.in_, min(1 << 20, left))
                if not b:
                    break
                os.write(dst, b)
                left -= len(b)
            if left <= 0:
                raise BadJob
        finally:
            os.close(dst)
        os.mkdir(os.path.join(staging, "out"), 0o700)
        fd = os.open(os.path.join(staging, "out"), os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
        try:
            _chown(fd)
        finally:
            os.close(fd)

    def collect(self, staging: str) -> bool:
        """Copy the renderer's manifest and pages from staging/out into the
        job's out. False (nothing usable) when one of them isn't a regular
        file, is too large, or the renderer wrote anything else."""
        src = os.open(os.path.join(staging, "out"), os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
        try:
            names = os.listdir(src)
            if "manifest.json" not in names or any(not _OUTPUT.match(n) for n in names):
                return False
            for name in names:
                try:
                    fd = os.open(name, os.O_RDONLY | os.O_NONBLOCK | NOFOLLOW, dir_fd=src)
                except OSError:
                    return False
                try:
                    st = os.fstat(fd)
                    if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_OUTPUT_FILE:
                        return False
                    data = os.read(fd, MAX_OUTPUT_FILE + 1)
                finally:
                    os.close(fd)
                if len(data) > MAX_OUTPUT_FILE:
                    return False
                out = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW, 0o444, dir_fd=self.out)
                try:
                    os.write(out, data)
                finally:
                    os.close(out)
        finally:
            os.close(src)
        return True


class Sandbox:
    """Runs jails; run is replaceable in tests."""

    def __init__(self, jobs: str = JOBS, run=None, max_jobs: int = MAX_JOBS, private: str = PRIVATE) -> None:
        self.jobs = jobs
        self.private = private
        self.run = run or self._run
        self.slots = threading.BoundedSemaphore(max_jobs)
        # None: never run. Not 0.0: time.monotonic() counts from the host's
        # boot, so on a host up for under SELFTEST_TTL a 0.0 looked fresh and
        # ping answered a self-test that never ran ("unavailable").
        self._selftest_at: float | None = None
        self._selftest_ok = False
        self._lock = threading.Lock()

    @staticmethod
    def _run(args: list[str]) -> int:
        try:
            p = subprocess.run(args, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, env={}, timeout=jail.WALL_SECONDS + 30, check=False)
        except (OSError, subprocess.TimeoutExpired):
            return NSJAIL_FAILED
        return p.returncode

    def _staging(self) -> str:
        return tempfile.mkdtemp(prefix="job-", dir=self.private)

    def ping(self) -> str:
        """ok when a jail runs Python with the renderer's libraries (cached
        for a minute; the first ping always runs it)."""
        with self._lock:
            now = time.monotonic()
            if self._selftest_at is None or now - self._selftest_at > SELFTEST_TTL:
                self._selftest_ok = self._selftest()
                self._selftest_at = now
            return "ok" if self._selftest_ok else "unavailable"

    def _selftest(self) -> bool:
        try:
            d = self._staging()
        except OSError:
            return False
        try:
            os.close(os.open(os.path.join(d, "in"), os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW, 0o444))
            os.mkdir(os.path.join(d, "out"), 0o700)
            fd = os.open(os.path.join(d, "out"), os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
            try:
                _chown(fd)
            finally:
                os.close(fd)
            return self.run(jail.argv(d, [jail.PYTHON, "-I", "-c", "import PIL, pypdfium2"])) == 0
        except OSError:
            return False
        finally:
            shutil.rmtree(d, ignore_errors=True)

    def render(self, job: str, mime: str, max_pages: int) -> str:
        if mime not in MIMES or not isinstance(max_pages, int) or isinstance(max_pages, bool) \
                or not 1 <= max_pages <= 50:
            return "bad_request"
        try:
            j = Job(self.jobs, job)
        except BadJob:
            return "bad_request"
        try:
            with self.slots:
                try:
                    staging = self._staging()
                except OSError:
                    return "unavailable"
                try:
                    try:
                        j.stage(staging)
                    except BadJob:
                        return "bad_request"
                    except OSError:
                        return "unavailable"
                    outcome = outcome_for(self.run(jail.argv(staging, jail.render_program(mime, max_pages))))
                    if outcome == "ok":
                        try:
                            if not j.collect(staging):
                                outcome = "unreadable"
                        except OSError as e:
                            outcome = "unavailable" if e.errno == errno.ENOSPC else "unreadable"
                    return outcome
                finally:
                    shutil.rmtree(staging, ignore_errors=True)
        finally:
            j.close()

    def handle(self, line: bytes) -> dict[str, str]:
        try:
            req = json.loads(line)
        except (ValueError, UnicodeDecodeError):
            return {"outcome": "bad_request"}
        if not isinstance(req, dict):
            return {"outcome": "bad_request"}
        if req.get("op") == "ping" and len(req) == 1:
            return {"outcome": self.ping()}
        if req.get("op") == "render" and set(req) == {"op", "job", "mime", "max_pages"}:
            return {"outcome": self.render(req["job"], req["mime"], req["max_pages"])}
        return {"outcome": "bad_request"}


def make_handler(sb: Sandbox):
    class Handler(socketserver.StreamRequestHandler):
        timeout = 10

        def handle(self) -> None:
            try:
                line = self.rfile.readline(MAX_LINE + 1)
            except (OSError, socket.timeout):
                return
            if len(line) > MAX_LINE or not line.endswith(b"\n"):
                reply = {"outcome": "bad_request"}
            else:
                reply = sb.handle(line)
            try:
                self.wfile.write(json.dumps(reply).encode() + b"\n")
            except OSError:
                pass

    return Handler


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def serve(jobs: str = JOBS, private: str = PRIVATE) -> None:
    os.makedirs(private, mode=0o700, exist_ok=True)
    st = os.lstat(private)
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o077:
        raise SystemExit("docsandbox: the private directory must be a directory of ours, mode 0700")
    path = os.path.join(jobs, SOCKET_NAME)
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    sb = Sandbox(jobs, private=private)
    # Only the documents service (uid 65532) may connect: bind makes the
    # socket 0600 under this umask, and lchown never follows a link.
    old = os.umask(0o177)
    try:
        srv = Server(path, make_handler(sb))
    finally:
        os.umask(old)
    os.lchown(path, jail.UID, jail.UID)
    print("docsandbox: serving", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    serve(sys.argv[1] if len(sys.argv) > 1 else JOBS)
