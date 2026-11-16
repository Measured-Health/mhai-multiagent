"""The docsandbox server: renders jobs for the documents service.

It listens on a Unix socket in the jobs directory, the tmpfs both
containers mount (the docsandbox container has no network at all). One
request per connection, one JSON line each way, at most 4 KiB:

    {"op": "ping"}                                        -> {"outcome": "ok"} or {"outcome": "unavailable"}
    {"op": "render", "job": "<uuid>", "mime": "...", "max_pages": n}
                                                          -> {"outcome": "<code>"}

The documents service makes ``<jobs>/<job>/in`` (the file, read-only) and
``<jobs>/<job>/out`` (an empty directory) before it asks. The outcome is
``ok`` (pages and manifest in out), a refusal (``too_many_pages``,
``too_many_pixels``, ``password_protected``, ``unreadable``,
``unsupported_type``), ``unavailable`` (the jail itself didn't run: the
document waits, never a refusal) or ``bad_request``. Nothing from a file
is ever logged or returned.

At most MAX_JOBS renders run at once (design §12: two files, 2 GB each).
"""

from __future__ import annotations

import json
import os
import re
import socket
import socketserver
import stat
import subprocess
import sys
import threading
import time

from docsandbox import jail

JOBS = "/jobs"
SOCKET_NAME = "sandbox.sock"
MAX_LINE = 4096
MAX_JOBS = 2
MIMES = ("application/pdf", "image/png", "image/jpeg", "image/tiff")
_JOB = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
# nsjail's own failure (it couldn't build the jail) is 255; the renderer
# never exits with it.
NSJAIL_FAILED = 255
SELFTEST_TTL = 60.0

CODES = {v: k for k, v in {
    "ok": 0,
    "too_many_pages": 10,
    "too_many_pixels": 11,
    "password_protected": 12,
    "unreadable": 13,
    "unsupported_type": 14,
}.items()}


def outcome_for(returncode: int) -> str:
    """Map the jail's exit status to an outcome. A kill (any limit) or a
    crash is unreadable; nsjail failing to start the jail is unavailable."""
    if returncode == NSJAIL_FAILED:
        return "unavailable"
    return CODES.get(returncode, "unreadable")


def job_dir(jobs: str, job: str) -> str | None:
    """The job's directory when it is a well-formed, plain job: a real
    directory (no symlink) holding a regular file ``in`` and an empty real
    directory ``out``. None otherwise."""
    if not isinstance(job, str) or not _JOB.match(job):
        return None
    d = os.path.join(jobs, job)
    try:
        if not stat.S_ISDIR(os.lstat(d).st_mode):
            return None
        if not stat.S_ISREG(os.lstat(os.path.join(d, "in")).st_mode):
            return None
        if not stat.S_ISDIR(os.lstat(os.path.join(d, "out")).st_mode) or os.listdir(os.path.join(d, "out")):
            return None
    except OSError:
        return None
    return d


class Sandbox:
    """Runs jails; run is replaceable in tests."""

    def __init__(self, jobs: str = JOBS, run=None, max_jobs: int = MAX_JOBS) -> None:
        self.jobs = jobs
        self.run = run or self._run
        self.slots = threading.BoundedSemaphore(max_jobs)
        self._selftest_at = 0.0
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

    def ping(self) -> str:
        """ok when a jail runs Python with the renderer's libraries (cached
        for a minute)."""
        with self._lock:
            now = time.monotonic()
            if now - self._selftest_at > SELFTEST_TTL:
                self._selftest_ok = self._selftest()
                self._selftest_at = now
            return "ok" if self._selftest_ok else "unavailable"

    def _selftest(self) -> bool:
        d = os.path.join(self.jobs, ".selftest")
        try:
            os.makedirs(os.path.join(d, "out"), exist_ok=True)
            with open(os.path.join(d, "in"), "wb"):
                pass
            os.chown(os.path.join(d, "out"), jail.UID, jail.UID)
        except OSError:
            return False
        rc = self.run(jail.argv(d, [jail.PYTHON, "-I", "-c", "import PIL, pypdfium2"]))
        return rc == 0

    def render(self, job: str, mime: str, max_pages: int) -> str:
        if mime not in MIMES or not isinstance(max_pages, int) or isinstance(max_pages, bool) \
                or not 1 <= max_pages <= 50:
            return "bad_request"
        d = job_dir(self.jobs, job)
        if d is None:
            return "bad_request"
        with self.slots:
            return outcome_for(self.run(jail.argv(d, jail.render_program(mime, max_pages))))

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


def serve(jobs: str = JOBS) -> None:
    path = os.path.join(jobs, SOCKET_NAME)
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    sb = Sandbox(jobs)
    srv = Server(path, make_handler(sb))
    # Only the documents service (uid 65532) may connect.
    os.chown(path, jail.UID, jail.UID)
    os.chmod(path, 0o600)
    print("docsandbox: serving", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    serve(sys.argv[1] if len(sys.argv) > 1 else JOBS)
