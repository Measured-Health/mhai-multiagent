"""The llama.cpp runtime the answer gate scores a ``cca`` run with: the run's GGUF itself,
the file that will be served (security review of #143, own answer model plan Decision 2).

* **Pinned engine.** ``llama-server`` is a binary built at llama.cpp's pinned commit
  (:data:`LLAMA_CPP_COMMIT`, the commit the export step converts with,
  docs/models/export-step.md), at the absolute path ``MHAI_LLAMA_SERVER``.
  :func:`check_engine` runs ``--version`` and refuses any other commit
  (``no_runtime``/``engine_version``); the version recorded is :data:`ENGINE_VERSION`.
* **Verified file.** :class:`LlamaServer` re-hashes the GGUF in the job's scratch folder
  (already checked and hashed by ``files.check_run``) and refuses it if it changed
  (``hash_mismatch``/``scratch_changed``) or is larger than the memory setting
  (``no_runtime``/``memory``). It starts the server with ``-m <that file>`` and
  ``--alias <its SHA-256>`` and nothing else to load: no ``-hf``, no URL, no other chat
  template (``--jinja`` applies the GGUF's own embedded template, the one PR 1 checked
  against the run's). Before any question, ``/v1/models`` must report exactly that alias
  (``no_runtime``/``alias_mismatch``); after the run the GGUF is hashed again.
* **Offline.** ``--offline``, loopback only (``--host 127.0.0.1``, a free port), and a
  clean environment (no proxy, token or ``LLAMA_ARG_*`` setting reaches it). The
  generator talks to that one loopback URL, ignores the environment's proxies and never
  follows a redirect.
* **Settings, not constants** (Decision 8: the model size is not decided):
  ``MHAI_EVAL_LLAMA_MEM`` (default ``4g``; also the server's address-space limit),
  ``MHAI_EVAL_LLAMA_THREADS`` (``2``), ``MHAI_EVAL_LLAMA_CTX`` (``4096``),
  ``MHAI_EVAL_LLAMA_START_TIMEOUT`` (``300`` s), ``MHAI_EVAL_LLAMA_ANSWER_TIMEOUT`` (``300``
  s) and ``MHAI_EVAL_LLAMA_MAX_TOKENS`` (``512``).

Residual risk (accepted for the pilot, plan "Residual risk"): the alias handshake trusts the
binary we started, not an attestation; ``--version`` is what that binary says of itself.
"""

from __future__ import annotations

import hashlib
import os
import re
import resource
import secrets
import socket
import subprocess
import time
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from types import TracebackType
from typing import Any

import httpx

from intelligence.modelstore import files
from intelligence.modelstore.reasons import Refused

LLAMA_CPP_COMMIT = "b1b770d90d94cad9e9ea5b993bdbc383a522fb28"  # tag v1.0.0, PR 1's pin
LLAMA_CPP_VERSION = "1.0.0"
ENGINE = "llama-cpp"
ENGINE_VERSION = f"{LLAMA_CPP_VERSION}+{LLAMA_CPP_COMMIT[:12]}"

SERVER_ENV = "MHAI_LLAMA_SERVER"
MEM_ENV = "MHAI_EVAL_LLAMA_MEM"
THREADS_ENV = "MHAI_EVAL_LLAMA_THREADS"
CTX_ENV = "MHAI_EVAL_LLAMA_CTX"
START_TIMEOUT_ENV = "MHAI_EVAL_LLAMA_START_TIMEOUT"
ANSWER_TIMEOUT_ENV = "MHAI_EVAL_LLAMA_ANSWER_TIMEOUT"
MAX_TOKENS_ENV = "MHAI_EVAL_LLAMA_MAX_TOKENS"

MAX_REPLY_BYTES = 1024 * 1024
# llama.cpp's own line (common/build-info.cpp.in at the pinned commit); a build without
# LLAMA_BUILD_IS_DEV=OFF says 1.0.0-dev.
_VERSION = re.compile(
    r"version: ([0-9][0-9a-z.-]{0,15}) \(build [0-9]{1,9}, commit ([0-9a-f]{7,40})\)", re.ASCII
)
_MEM = re.compile(r"([1-9][0-9]{0,5})([mg])", re.ASCII)
_INT = re.compile(r"[1-9][0-9]{0,6}", re.ASCII)


class RuntimeFailed(Exception):
    """A generation that failed (HTTP, timeout or a malformed reply): the item escalates."""


def _settings_refused() -> Refused:
    return Refused("no_runtime", detail_code="settings")


@dataclass(frozen=True)
class Settings:
    server: Path
    mem_bytes: int
    threads: int
    ctx: int
    start_timeout_s: float
    answer_timeout_s: float
    max_tokens: int

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        """The settings from the environment; anything malformed is ``no_runtime``/
        ``settings``."""
        env = os.environ if env is None else env
        server = Path(env.get(SERVER_ENV, ""))
        if not server.is_absolute() or not server.is_file() or not os.access(server, os.X_OK):
            raise _settings_refused()

        def number(name: str, default: str, low: int, high: int) -> int:
            text = env.get(name) or default
            if _INT.fullmatch(text) is None or not low <= int(text) <= high:
                raise _settings_refused()
            return int(text)

        mem = _MEM.fullmatch((env.get(MEM_ENV) or "4g").lower())
        if mem is None:
            raise _settings_refused()
        mem_bytes = int(mem.group(1)) << (30 if mem.group(2) == "g" else 20)
        return cls(
            server=server,
            mem_bytes=mem_bytes,
            threads=number(THREADS_ENV, "2", 1, 256),
            ctx=number(CTX_ENV, "4096", 256, 131072),
            start_timeout_s=float(number(START_TIMEOUT_ENV, "300", 1, 86400)),
            answer_timeout_s=float(number(ANSWER_TIMEOUT_ENV, "300", 1, 86400)),
            max_tokens=number(MAX_TOKENS_ENV, "512", 16, 32768),
        )


def _clean_env(home: Path) -> dict[str, str]:
    return {"PATH": "/usr/bin:/bin", "HOME": str(home), "LANG": "C.UTF-8"}


def check_engine(settings: Settings) -> str:
    """``llama-server --version`` must name the pinned commit; returns the version recorded."""
    failed = False
    out = ""
    try:
        done = subprocess.run(
            [str(settings.server), "--version"],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            timeout=60,
            env=_clean_env(Path("/nonexistent")),
            check=False,
        )
        out = (done.stdout + done.stderr).decode("ascii", "replace")
    except (OSError, subprocess.TimeoutExpired):
        failed = True
    m = None if failed else _VERSION.search(out)
    if (
        m is None
        or m.group(1) not in (LLAMA_CPP_VERSION, f"{LLAMA_CPP_VERSION}-dev")
        or not LLAMA_CPP_COMMIT.startswith(m.group(2))
    ):
        raise Refused("no_runtime", detail_code="engine_version")
    return ENGINE_VERSION


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb", opener=lambda p, f: os.open(p, f | os.O_NOFOLLOW)) as fh:
        while block := fh.read(files.BLOCK_SIZE):
            digest.update(block)
    return digest.hexdigest()


def _check_unchanged(gguf: Path, sha256: str) -> None:
    failed = False
    try:
        got = sha256_file(gguf)
    except OSError:
        failed = True
    if failed or not secrets.compare_digest(got, sha256):
        raise Refused("hash_mismatch", detail_code="scratch_changed")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        port: int = s.getsockname()[1]
        return port


class LlamaGenerator:
    """The candidate generator over the started server: ``generate(system, user)``, as
    ``OllamaGenerator``. Deterministic sampling (temperature 0)."""

    def __init__(self, base_url: str, alias: str, timeout_s: float, max_tokens: int) -> None:
        self._url = base_url
        self._alias = alias
        self._max_tokens = max_tokens
        self._client = httpx.Client(timeout=timeout_s, trust_env=False, follow_redirects=False)

    @property
    def name(self) -> str:
        return f"{ENGINE}:{self._alias[:12]}"

    def generate(self, system: str, user: str) -> str:
        try:
            resp = self._client.post(
                f"{self._url}/v1/chat/completions",
                json={
                    "model": self._alias,
                    "messages": [
                        {"role": "system", "content": system},
                        {"role": "user", "content": user},
                    ],
                    "temperature": 0,
                    "max_tokens": self._max_tokens,
                    "stream": False,
                },
            )
            if resp.status_code != 200 or len(resp.content) > MAX_REPLY_BYTES:
                raise RuntimeFailed("status")
            content = resp.json()["choices"][0]["message"]["content"]
        except RuntimeFailed:
            raise
        except (httpx.HTTPError, ValueError, KeyError, IndexError, TypeError) as e:
            raise RuntimeFailed("reply") from e
        if not isinstance(content, str) or not content.strip():
            raise RuntimeFailed("reply")
        return content.strip()

    def models(self) -> Any:
        return self._client.get(f"{self._url}/v1/models").json()

    def healthy(self) -> bool:
        return self._client.get(f"{self._url}/health").status_code == 200

    def close(self) -> None:
        self._client.close()


class LlamaServer:
    """``llama-server`` over one verified GGUF, on loopback, for the length of a ``with``
    block (see the module docstring). ``home`` is a private folder for the server's
    ``HOME``."""

    def __init__(self, settings: Settings, gguf: Path, sha256: str, home: Path) -> None:
        self.settings = settings
        self.gguf = gguf
        self.sha256 = sha256
        self.home = home
        self.port = 0
        self._proc: subprocess.Popen[bytes] | None = None
        self._gen: LlamaGenerator | None = None

    @property
    def generator(self) -> LlamaGenerator:
        assert self._gen is not None
        return self._gen

    def argv(self) -> list[str]:
        s = self.settings
        return [
            str(s.server),
            "-m",
            str(self.gguf),
            "--alias",
            self.sha256,
            "--host",
            "127.0.0.1",
            "--port",
            str(self.port),
            "-t",
            str(s.threads),
            "-c",
            str(s.ctx),
            "-np",
            "1",
            "--jinja",
            "--offline",
            "--no-webui",
        ]

    def _limits(self) -> None:  # pragma: no cover - runs in the child
        mem = self.settings.mem_bytes
        resource.setrlimit(resource.RLIMIT_AS, (mem, mem))

    def __enter__(self) -> LlamaServer:
        _check_unchanged(self.gguf, self.sha256)
        if self.gguf.stat().st_size >= self.settings.mem_bytes:
            raise Refused("no_runtime", detail_code="memory")
        self.home.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.port = _free_port()
        self._proc = subprocess.Popen(
            self.argv(),
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=_clean_env(self.home),
            cwd=self.home,
            close_fds=True,
            start_new_session=True,
            preexec_fn=self._limits,  # sets RLIMIT_AS only; nothing else runs in the child
        )
        self._gen = LlamaGenerator(
            f"http://127.0.0.1:{self.port}",
            self.sha256,
            self.settings.answer_timeout_s,
            self.settings.max_tokens,
        )
        try:
            self._wait_ready()
        except BaseException:
            self._stop()
            raise
        return self

    def _wait_ready(self) -> None:
        assert self._proc is not None and self._gen is not None
        deadline = time.monotonic() + self.settings.start_timeout_s
        while True:
            if self._proc.poll() is not None:
                raise Refused("no_runtime", detail_code="server_exited")
            ready = False
            try:
                ready = self._gen.healthy()
            except httpx.HTTPError:
                ready = False
            if ready:
                break
            if time.monotonic() > deadline:
                raise Refused("no_runtime", detail_code="start_timeout")
            time.sleep(0.1)
        failed = False
        try:
            listed = self._gen.models()
            data = listed["data"]
            ok = (
                isinstance(data, list)
                and len(data) == 1
                and isinstance(data[0], dict)
                and data[0].get("id") == self.sha256
            )
        except (httpx.HTTPError, ValueError, KeyError, TypeError):
            failed = True
        if failed or not ok:
            raise Refused("no_runtime", detail_code="alias_mismatch")

    def _stop(self) -> None:
        if self._gen is not None:
            self._gen.close()
        proc = self._proc
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=10)

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self._stop()
        if exc_type is None:
            _check_unchanged(self.gguf, self.sha256)  # the file served is still the one scored


__all__ = [
    "ENGINE",
    "ENGINE_VERSION",
    "LLAMA_CPP_COMMIT",
    "LlamaGenerator",
    "LlamaServer",
    "RuntimeFailed",
    "Settings",
    "check_engine",
]
