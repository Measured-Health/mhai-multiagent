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
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import parse_qsl, unquote, urlsplit

SERVICES = frozenset({"pipeline", "echo"})
MAX_CONCURRENCY = 1024
_PLACEHOLDERS = ("change-me", "changeme", "placeholder")
DEFAULT_DB_USER = "intelligence_app"
MIN_DB_PASSWORD_LEN = 16  # core's MinLoginPasswordLen
# libpq parameters a database URL may carry. Anything else (options, which
# can set a role or any setting at connect; service and passfile, which
# read credentials from elsewhere) is refused.
_DB_PARAMS = frozenset(
    {
        "sslmode",
        "sslrootcert",
        "sslcert",
        "sslkey",
        "connect_timeout",
        "application_name",
        "target_session_attrs",
    }
)


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


def is_placeholder(value: str) -> bool:
    return any(p in value.lower() for p in _PLACEHOLDERS)


@dataclass(frozen=True)
class DbUrl:
    """A Postgres URL taken apart: where to connect, and the credentials
    kept separately so they never sit in a connection string, a log or an
    error (secure-build: parse connection strings yourself)."""

    host: str
    port: int | None
    dbname: str
    params: tuple[tuple[str, str], ...]
    user: str | None = None
    password: str | None = field(default=None, repr=False)


def parse_db_url(raw: str, name: str = "INTELLIGENCE_DB_URL") -> DbUrl:
    """Parse a postgres:// URL. The error names the variable, never the
    value (the URL may carry a password)."""
    try:
        u = urlsplit(raw.strip())
        host, port = u.hostname, u.port
        user = unquote(u.username) if u.username else None
        password = unquote(u.password) if u.password is not None else None
        params = tuple(parse_qsl(u.query, keep_blank_values=True, strict_parsing=bool(u.query)))
    except ValueError:
        raise ConfigError(f"{name} is invalid") from None
    dbname = unquote(u.path.lstrip("/"))
    if u.scheme not in ("postgres", "postgresql") or not host or not dbname or u.fragment:
        raise ConfigError(f"{name} is invalid")
    if "/" in dbname or any(k not in _DB_PARAMS for k, _ in params):
        raise ConfigError(f"{name} is invalid")
    if len({k for k, _ in params}) != len(params):
        raise ConfigError(f"{name} is invalid")
    return DbUrl(host=host, port=port, dbname=dbname, params=params, user=user, password=password)


def check_db_password(password: str, name: str) -> str | None:
    """core's config.CheckPassword: not a placeholder, at least 16 bytes."""
    if is_placeholder(password):
        return f"{name} is a placeholder"
    if len(password.encode()) < MIN_DB_PASSWORD_LEN:
        return f"{name} must be at least {MIN_DB_PASSWORD_LEN} bytes"
    return None


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
    # The pipeline's database (spec §8.2): where, from INTELLIGENCE_DB_URL;
    # who, from INTELLIGENCE_DB_USER (default intelligence_app); the
    # password from INTELLIGENCE_DB_PASSWORD_FILE or INTELLIGENCE_DB_PASSWORD.
    db_url: str | None = field(default=None, repr=False)  # pipeline only
    db_user: str | None = None
    db_password: str | None = field(default=None, repr=False)
    # False (production) unless from_env read APP_ENV=development: TLS with
    # a handshake first is the default.
    development: bool = False

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
        db_user = db_password = None
        if db is not None and svc != "pipeline":
            errs.append("INTELLIGENCE_DB_URL is for the pipeline service only")
        elif db is not None:
            db_user, db_password = _db_credentials(env, db, development, errs)
        elif any(get(n) for n in _DB_CREDENTIAL_VARS):
            errs.append(
                "INTELLIGENCE_DB_USER and INTELLIGENCE_DB_PASSWORD need INTELLIGENCE_DB_URL"
            )
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
            db_user=db_user,
            db_password=db_password,
            development=development,
        )


_DB_CREDENTIAL_VARS = (
    "INTELLIGENCE_DB_USER",
    "INTELLIGENCE_DB_PASSWORD",
    "INTELLIGENCE_DB_PASSWORD_FILE",
)


def _db_credentials(
    env: Mapping[str, str], raw_url: str, development: bool, errs: list[str]
) -> tuple[str | None, str | None]:
    """The login and password for the pipeline's database. The password
    comes from a file (production) or the environment, never from the URL
    outside development. Errors name variables only."""
    try:
        url = parse_db_url(raw_url)
    except ConfigError as e:
        errs.append(str(e))
        return None, None
    user = env.get("INTELLIGENCE_DB_USER", "").strip() or url.user or DEFAULT_DB_USER
    if url.user is not None and url.user != user:
        errs.append("INTELLIGENCE_DB_URL names a different user than INTELLIGENCE_DB_USER")
    if url.password is not None and not development:
        errs.append(
            "INTELLIGENCE_DB_URL must not carry a password outside development "
            "(use INTELLIGENCE_DB_PASSWORD_FILE)"
        )
    raw_pw = env.get("INTELLIGENCE_DB_PASSWORD", "")
    pw_file = env.get("INTELLIGENCE_DB_PASSWORD_FILE", "").strip()
    password: str | None = None
    name = "INTELLIGENCE_DB_PASSWORD"
    if raw_pw and pw_file:
        errs.append("set INTELLIGENCE_DB_PASSWORD or INTELLIGENCE_DB_PASSWORD_FILE, not both")
        return None, None
    if pw_file:
        name = "INTELLIGENCE_DB_PASSWORD_FILE"
        try:
            password = Path(pw_file).read_text(encoding="utf-8").rstrip("\r\n")
        except (OSError, UnicodeDecodeError):
            errs.append("INTELLIGENCE_DB_PASSWORD_FILE can't be read")
            return None, None
    elif raw_pw:
        password = raw_pw
    elif url.password is not None:
        name, password = "INTELLIGENCE_DB_URL", url.password
    if not password:
        errs.append("INTELLIGENCE_DB_PASSWORD or INTELLIGENCE_DB_PASSWORD_FILE is required")
        return None, None
    problem = check_db_password(password, name)
    if problem is not None:
        errs.append(problem)
        return None, None
    return user, password
