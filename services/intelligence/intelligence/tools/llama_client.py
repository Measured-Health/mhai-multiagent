"""The local route's answer model: the ``llama`` sidecar (llama.cpp's ``llama-server``,
profile ``ownmodel``) serving the pinned ``cca`` GGUF (own answer model plan, PR 3).

* **Identity before every job.** :meth:`PinnedLlamaGenerator.check` asks the sidecar's
  ``GET /v1/models`` and requires exactly one model whose id is the verified
  ``<name>@<version>#<sha12>`` (the sidecar's ``--alias``, which its entrypoint sets only
  after re-hashing the GGUF against ``active.json``). No verified pin, an unreachable or
  misbehaving sidecar, or any other alias raises :class:`LocalUnverified`, and the job
  escalates with the outcome ``local_unverified`` before anything is generated.
* **One fixed target.** ``INTELLIGENCE_LOCAL_LLM_URL`` (default ``http://llama:8080``):
  plain HTTP to the sidecar's service name on the internal ``model`` network, or to
  loopback; any other host only in development (:func:`check_url`). The client ignores
  the environment's proxies (``trust_env=False``) and never follows a redirect.
* **Settings, not constants:** ``INTELLIGENCE_LOCAL_LLM_MAX_TOKENS`` (512),
  ``_TEMPERATURE`` (0: the gate scored with greedy sampling), ``_TOP_P`` (1.0),
  ``_TIMEOUT`` (300 s per answer) and ``_START_TIMEOUT`` (300 s for the sidecar to serve
  the verified alias at start-up).
* :class:`RefusedGenerator` is the local route when no ``cca`` is pinned outside
  development and ``INTELLIGENCE_LOCAL_UNPINNED`` is not ``on``: every job escalates as
  ``local_unverified``, so production never answers from a model pulled by name.

Errors carry fixed codes only; nothing here logs a question, an answer or a URL.
"""

from __future__ import annotations

import ipaddress
import logging
import math
import re
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit

import httpx

log = logging.getLogger(__name__)

DEFAULT_URL = "http://llama:8080"
SIDECAR_HOST = "llama"
LOOPBACK_NAMES = frozenset({"localhost"})
MAX_REPLY_BYTES = 1024 * 1024
MAX_MODELS_BYTES = 64 * 1024
CHECK_TIMEOUT_S = 10.0
UNVERIFIED_NAME = "llama:unverified"
REFUSED_NAME = "unpinned:refused"
# The fixed codes a LocalUnverified may carry.
DETAILS = frozenset(
    {
        "no_pin",
        "unreachable",
        "status",
        "malformed",
        "alias_mismatch",
        "start_timeout",
        "unpinned",
    }
)
_INT = re.compile(r"[0-9]{1,7}", re.ASCII)
_FLOAT = re.compile(r"[0-9]{1,3}(?:\.[0-9]{1,6})?", re.ASCII)
_HOST = re.compile(
    r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*"
)


class LocalUnverified(Exception):
    """The sidecar could not be shown to serve the verified model. ``str()`` is the code."""

    def __init__(self, detail: str) -> None:
        if detail not in DETAILS:
            raise ValueError("unknown detail")
        super().__init__(detail)
        self.detail = detail


class GenerationFailed(Exception):
    """A completion that failed (HTTP, timeout, redirect or a malformed or empty reply)."""


@dataclass(frozen=True)
class LocalLLMSettings:
    url: str = DEFAULT_URL
    max_tokens: int = 512
    temperature: float = 0.0
    top_p: float = 1.0
    timeout_s: float = 300.0
    start_timeout_s: float = 300.0


def _is_loopback(host: str) -> bool:
    if host in LOOPBACK_NAMES:
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def check_url(raw: str, *, development: bool) -> str:
    """The sidecar's base URL, without a trailing slash: ``http://`` only, no userinfo,
    path, query or fragment; the host is ``llama`` or loopback, any host in development
    only. Anything else raises ValueError (naming no value)."""
    url = raw.strip().rstrip("/")
    try:
        parts = urlsplit(url)
        port = parts.port
    except ValueError:
        raise ValueError("malformed") from None
    host = (parts.hostname or "").lower()
    if (
        parts.scheme != "http"
        or not host
        or parts.username is not None
        or parts.password is not None
        or parts.path
        or parts.query
        or parts.fragment
        or (port is not None and not 0 < port < 65536)
        or "@" in parts.netloc
    ):
        raise ValueError("malformed")
    if host == SIDECAR_HOST or _is_loopback(host):
        return url
    if development and _HOST.fullmatch(host):
        return url
    raise ValueError("host")


def settings_from_env(
    env: Mapping[str, str], development: bool, errs: list[str]
) -> LocalLLMSettings:
    """:class:`LocalLLMSettings` from the environment; each malformed variable is named in
    ``errs`` (never its value) and its default kept."""
    d = LocalLLMSettings()

    def get(name: str) -> str | None:
        v = env.get(name, "").strip()
        return v or None

    url = d.url
    raw = get("INTELLIGENCE_LOCAL_LLM_URL")
    if raw is not None:
        try:
            url = check_url(raw, development=development)
        except ValueError:
            errs.append(
                "INTELLIGENCE_LOCAL_LLM_URL must be http://llama[:port] or loopback "
                "(another host in development only), with no path, query or credentials"
            )

    def integer(name: str, default: int, low: int, high: int) -> int:
        text = get(name)
        if text is None:
            return default
        if _INT.fullmatch(text) is None or not low <= int(text) <= high:
            errs.append(f"{name} must be a whole number from {low} to {high}")
            return default
        return int(text)

    def number(name: str, default: float, low: float, high: float, *, low_open: bool) -> float:
        text = get(name)
        if text is None:
            return default
        ok = _FLOAT.fullmatch(text) is not None
        value = float(text) if ok else math.nan
        if not ok or not (low < value if low_open else low <= value) or value > high:
            errs.append(f"{name} must be a number in {'(' if low_open else '['}{low}, {high}]")
            return default
        return value

    return LocalLLMSettings(
        url=url,
        max_tokens=integer("INTELLIGENCE_LOCAL_LLM_MAX_TOKENS", d.max_tokens, 16, 32768),
        temperature=number(
            "INTELLIGENCE_LOCAL_LLM_TEMPERATURE", d.temperature, 0.0, 2.0, low_open=False
        ),
        top_p=number("INTELLIGENCE_LOCAL_LLM_TOP_P", d.top_p, 0.0, 1.0, low_open=True),
        timeout_s=float(integer("INTELLIGENCE_LOCAL_LLM_TIMEOUT", 300, 1, 3600)),
        start_timeout_s=float(integer("INTELLIGENCE_LOCAL_LLM_START_TIMEOUT", 300, 1, 86400)),
    )


class LlamaSidecar:
    """HTTP to the sidecar: the alias it serves, and one chat completion."""

    def __init__(self, settings: LocalLLMSettings, client: httpx.Client | None = None) -> None:
        self.settings = settings
        self._url = settings.url
        self._client = client or httpx.Client(
            timeout=settings.timeout_s, trust_env=False, follow_redirects=False
        )

    def served_alias(self) -> str:
        """The one model id ``/v1/models`` lists; :class:`LocalUnverified` otherwise."""
        try:
            resp = self._client.get(f"{self._url}/v1/models", timeout=CHECK_TIMEOUT_S)
        except httpx.HTTPError:
            raise LocalUnverified("unreachable") from None
        if resp.status_code != 200:
            raise LocalUnverified("status")
        if len(resp.content) > MAX_MODELS_BYTES:
            raise LocalUnverified("malformed")
        try:
            data = resp.json()["data"]
        except (ValueError, KeyError, TypeError):
            raise LocalUnverified("malformed") from None
        if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict):
            raise LocalUnverified("alias_mismatch" if isinstance(data, list) else "malformed")
        alias = data[0].get("id")
        if not isinstance(alias, str):
            raise LocalUnverified("malformed")
        return alias

    def require_alias(self, expected: str) -> None:
        if self.served_alias() != expected:
            raise LocalUnverified("alias_mismatch")

    def wait_for_alias(
        self,
        expected: str,
        timeout_s: float,
        *,
        interval_s: float = 2.0,
        sleep: Callable[[float], None] = time.sleep,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        """Until the sidecar serves ``expected`` (it may still be loading the GGUF, or not
        yet have read ``active.json``); ``LocalUnverified("start_timeout")`` after
        ``timeout_s``."""
        deadline = clock() + timeout_s
        while True:
            try:
                self.require_alias(expected)
                return
            except LocalUnverified:
                pass
            if clock() >= deadline:
                raise LocalUnverified("start_timeout")
            sleep(interval_s)

    def ask(self, alias: str, system: str, user: str) -> str:
        """One completion from ``alias``; :class:`GenerationFailed` on any failure."""
        s = self.settings
        try:
            resp = self._client.post(
                f"{self._url}/v1/chat/completions",
                json={
                    "model": alias,
                    "messages": [
                        {"role": "system", "content": system},
                        {"role": "user", "content": user},
                    ],
                    "temperature": s.temperature,
                    "top_p": s.top_p,
                    "max_tokens": s.max_tokens,
                    "stream": False,
                },
            )
            if resp.status_code != 200 or len(resp.content) > MAX_REPLY_BYTES:
                raise GenerationFailed("status")
            content = resp.json()["choices"][0]["message"]["content"]
        except GenerationFailed:
            raise
        except (httpx.HTTPError, ValueError, KeyError, IndexError, TypeError):
            raise GenerationFailed("reply") from None
        if not isinstance(content, str) or not content.strip():
            raise GenerationFailed("reply")
        return content.strip()

    def close(self) -> None:
        self._client.close()


class PinnedLlamaGenerator:
    """The CCA's generator on the local route when ``cca`` is pinned: ``name`` and
    ``generate(system, user)``, as ``OllamaGenerator``. ``expected`` gives the verified
    version string (None until the worker verified the pin)."""

    def __init__(self, sidecar: LlamaSidecar, expected: Callable[[], str | None]) -> None:
        self._sidecar = sidecar
        self._expected = expected

    @property
    def name(self) -> str:
        return self._expected() or UNVERIFIED_NAME

    def _alias(self) -> str:
        alias = self._expected()
        if not alias:
            raise LocalUnverified("no_pin")
        return alias

    def check(self) -> None:
        """Before every job: the sidecar serves exactly the verified alias."""
        self._sidecar.require_alias(self._alias())

    def generate(self, system: str, user: str) -> str:
        return self._sidecar.ask(self._alias(), system, user)


class RefusedGenerator:
    """The local route with no pinned ``cca`` outside development and without
    ``INTELLIGENCE_LOCAL_UNPINNED=on``: every job escalates, nothing is generated."""

    name = REFUSED_NAME

    def check(self) -> None:
        raise LocalUnverified("unpinned")

    def generate(self, system: str, user: str) -> Any:
        raise LocalUnverified("unpinned")


__all__ = [
    "DEFAULT_URL",
    "GenerationFailed",
    "LlamaSidecar",
    "LocalLLMSettings",
    "LocalUnverified",
    "PinnedLlamaGenerator",
    "RefusedGenerator",
    "check_url",
    "settings_from_env",
]
