"""Environment settings for the boundary.

Like core's CORE_ENV, an unset APP_ENV counts as production: the
development relaxations (test-only operations, the echo service, a NATS
password in the URL, no TLS) are on only when development is named.

Errors name variables, never their values: a NATS_URL carries a password in
development.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass
from urllib.parse import urlsplit

SERVICES = frozenset({"pipeline", "echo"})
MAX_CONCURRENCY = 1024
_PLACEHOLDERS = ("change-me", "changeme", "placeholder")


class ConfigError(Exception):
    """A setting is missing or wrong. The message names variables only."""


def is_development(environ: Mapping[str, str] | None = None) -> bool:
    """True only when APP_ENV names development (case and spaces ignored)."""
    env = os.environ if environ is None else environ
    return env.get("APP_ENV", "production").strip().lower() == "development"


def _check_nats_url(raw: str, development: bool) -> list[str]:
    """Core's checkNATSURL: every server parses, and no password is a
    placeholder; outside development, no password at all (NKeys)."""
    errs: list[str] = []
    for part in raw.split(","):
        try:
            u = urlsplit(part.strip())
            host = u.hostname
            password = u.password
        except ValueError:
            return ["NATS_URL is invalid"]
        if not host:
            return ["NATS_URL is invalid"]
        if password is not None:
            if any(p in password.lower() for p in _PLACEHOLDERS):
                errs.append("NATS_URL is a placeholder")
            elif not development:
                errs.append("NATS_URL must not carry a password outside development (use NKeys)")
    return errs[:1]


@dataclass(frozen=True)
class WorkerSettings:
    """What `intelligence worker` needs (spec §12, §17.6)."""

    service: str
    nats_url: str
    trusted_keys_path: str
    nkey_seed_path: str | None = None
    tls_ca_path: str | None = None
    tls_cert_path: str | None = None
    tls_key_path: str | None = None
    operations_path: str | None = None  # None: the packaged operations.json
    max_concurrency: int = 4
    db_url: str | None = None  # pipeline only (T10)

    @classmethod
    def from_env(
        cls, environ: Mapping[str, str] | None = None, *, service: str | None = None
    ) -> WorkerSettings:
        env = os.environ if environ is None else environ

        def get(name: str) -> str | None:
            v = env.get(name, "").strip()
            return v or None

        development = is_development(env)
        errs: list[str] = []
        svc = get("INTELLIGENCE_SERVICE")
        if service is not None:
            if svc is not None and svc != service:
                errs.append("INTELLIGENCE_SERVICE does not match --service")
            svc = service
        if svc is None:
            errs.append("INTELLIGENCE_SERVICE is required")
        elif svc not in SERVICES:
            errs.append("INTELLIGENCE_SERVICE must be pipeline or echo")
        elif svc == "echo" and not development:
            # Test and smoke only (spec §4): never in production.
            errs.append("the echo service runs only with APP_ENV=development")
        url = get("NATS_URL")
        if url is None:
            errs.append("NATS_URL is required")
        else:
            errs.extend(_check_nats_url(url, development))
        keys = get("INTELLIGENCE_TRUSTED_KEYS_PATH")
        if keys is None:
            errs.append("INTELLIGENCE_TRUSTED_KEYS_PATH is required")
        nkey = get("NATS_NKEY_SEED_PATH")
        ca, cert, key = get("NATS_TLS_CA_PATH"), get("NATS_TLS_CERT_PATH"), get("NATS_TLS_KEY_PATH")
        tls_set = [v is not None for v in (ca, cert, key)]
        tls_names = "NATS_TLS_CA_PATH, NATS_TLS_CERT_PATH and NATS_TLS_KEY_PATH"
        if any(tls_set) and not all(tls_set):
            errs.append(f"{tls_names} must be set together")
        elif not development:
            # NATS carries access tokens since spec §17: TLS with verify (a
            # client certificate, as core's ValidateServe) and NKeys
            # everywhere but development; unset APP_ENV fails closed.
            if not all(tls_set):
                errs.append(f"{tls_names} are required outside development")
            if nkey is None:
                errs.append("NATS_NKEY_SEED_PATH is required outside development")
        ops_path = get("INTELLIGENCE_OPERATIONS_PATH")
        if ops_path is not None and not development:
            # The permission table is the packaged, generated one in
            # production: a mounted file can't replace it.
            errs.append("INTELLIGENCE_OPERATIONS_PATH is for development only")
        raw_max = get("INTELLIGENCE_MAX_CONCURRENCY")
        max_concurrency = 4
        if raw_max is not None:
            if not raw_max.isdigit() or not 0 < int(raw_max) <= MAX_CONCURRENCY:
                errs.append(f"INTELLIGENCE_MAX_CONCURRENCY must be 1 to {MAX_CONCURRENCY}")
            else:
                max_concurrency = int(raw_max)
        db = get("INTELLIGENCE_DB_URL")
        if db is not None and svc != "pipeline":
            errs.append("INTELLIGENCE_DB_URL is for the pipeline service only")
        if errs:
            raise ConfigError("; ".join(errs))
        assert svc is not None and url is not None and keys is not None
        return cls(
            service=svc,
            nats_url=url,
            trusted_keys_path=keys,
            nkey_seed_path=nkey,
            tls_ca_path=ca,
            tls_cert_path=cert,
            tls_key_path=key,
            operations_path=ops_path,
            max_concurrency=max_concurrency,
            db_url=db,
        )
