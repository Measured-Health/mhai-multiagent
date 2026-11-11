"""Environment settings for the boundary.

Like core's CORE_ENV, an unset APP_ENV counts as production: the
development relaxations (test-only operations, the echo service, a NATS
password in the URL, no TLS) are on only when development is named.

Errors name variables, never their values: a NATS_URL carries a password in
development.

The pinned models (plan Task 23, design section 7.4). The pipeline verifies the
roles it needs from the model store before it is ready:

  INTELLIGENCE_MODEL_ROLES   comma-separated roles (``^[a-z][a-z0-9_]{1,31}$``,
                             at most 16, no repeats); empty or unset means none,
                             and then none of the settings below is needed
  INTELLIGENCE_MODELS_LOCK   the models.lock to load from, read once at start-up
  INTELLIGENCE_MODEL_CACHE   an absolute path: the verified copies (Task 22's
                             cache rules apply)
  MODEL_STORE_ENDPOINT       https://host[:port]; plain http only for a loopback
                             host (127.0.0.1, ::1, localhost). A loopback MinIO is
                             for development and rehearsal only: production uses
                             a TLS S3-compatible store
  MODEL_STORE_BUCKET, MODEL_STORE_REGION (default us-east-1)
  MODEL_STORE_READER_ACCESS_KEY_FILE, MODEL_STORE_READER_SECRET_KEY_FILE
                             the reader's credentials, from files only: regular
                             files (not links), mode 0600 or tighter, never a
                             placeholder (empty, changeme, change-me,
                             placeholder, minioadmin, example). The variables
                             without _FILE are refused
  MHAI_MODELS_TRUST_POLICY   the trust policy for signatures
  INTELLIGENCE_WARMUP_PAGE   an absolute path: the fixed warm-up page each
                             in-process role must reproduce (default: the
                             repository's testdata/models/warmup/page.png;
                             the image must ship it). Checked at start-up:
                             missing is warmup_page_missing
  MHAI_MODELS_ENV            ``development`` (development signers, fixture
                             names, the rehearsal gate) only together with
                             APP_ENV=development; with any other APP_ENV (or
                             none) it is refused at start-up

Each is refused at start-up if malformed, set or not needed.
"""

from __future__ import annotations

import logging
import os
import stat
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any
from urllib.parse import parse_qsl, unquote, urlsplit

if TYPE_CHECKING:
    # boundary.cloud imports this module; this one imports it only when
    # building settings, so the import graph is one-way at load.
    from intelligence.boundary.cloud.settings import CloudSettings

log = logging.getLogger("intelligence.boundary.config")

SERVICES = frozenset({"pipeline", "echo"})
# INTELLIGENCE_MODEL_CLIENT (F14): the real models, or the synthetic stub
# (tests and the CI smoke test; development only).
MODEL_CLIENTS = frozenset({"orchestrator", "stub"})
# INTELLIGENCE_LEARNING_CLIENT (Plan 4 §10.6): the buffer arithmetic, or the
# stub (tests and the CI smoke test; development only).
LEARNING_CLIENTS = frozenset({"trust_buffer", "stub"})
MAX_CONCURRENCY = 1024
_PLACEHOLDERS = ("change-me", "changeme", "placeholder")
# Store credentials: the placeholders above, plus the MinIO default and the
# documentation examples (AWS's AKIAIOSFODNN7EXAMPLE and the like).
_STORE_PLACEHOLDERS = ("minioadmin", "example")
MAX_MODEL_ROLES = 16
MAX_SECRET_FILE_BYTES = 4096
_STORE_CREDENTIAL_FILES = (
    "MODEL_STORE_READER_ACCESS_KEY_FILE",
    "MODEL_STORE_READER_SECRET_KEY_FILE",
)
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


def is_demo(environ: Mapping[str, str] | None = None) -> bool:
    """True only when APP_ENV names demo: production in every respect but
    one, a cloud provider's ZDR_REF may be DEMO-SYNTHETIC (synthetic data
    only). Demo is not development."""
    env = os.environ if environ is None else environ
    return env.get("APP_ENV", "production").strip().lower() == "demo"


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
    # The pipeline's models: orchestrator (default) or stub (development only).
    model_client: str = "orchestrator"
    # The approve_correction job's learning client: trust_buffer (default)
    # or stub (development only).
    learning_client: str = "trust_buffer"
    # The pinned models (module docstring): none unless INTELLIGENCE_MODEL_ROLES.
    model_roles: tuple[str, ...] = ()
    models_lock_path: str | None = None
    model_cache_dir: str | None = None
    model_store_endpoint: str | None = None
    model_store_bucket: str | None = None
    model_store_region: str = "us-east-1"
    model_store_access_key: str | None = field(default=None, repr=False)
    model_store_secret_key: str | None = field(default=None, repr=False)
    models_trust_policy_path: str | None = None
    model_warmup_page: str | None = None
    # Cloud model providers (plan 2026-11-06): none unless
    # INTELLIGENCE_CLOUD_PROVIDERS names some (pipeline only).
    cloud: CloudSettings = field(default_factory=lambda: _no_cloud())

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
        model_client = get("INTELLIGENCE_MODEL_CLIENT") or "orchestrator"
        if model_client not in MODEL_CLIENTS:
            errs.append("INTELLIGENCE_MODEL_CLIENT must be orchestrator or stub")
        elif model_client == "stub" and not development:
            # The stub serves invented answers (ADR 0005): never in production,
            # and an unset APP_ENV counts as production.
            errs.append("INTELLIGENCE_MODEL_CLIENT=stub runs only with APP_ENV=development")
        learning_client = get("INTELLIGENCE_LEARNING_CLIENT") or "trust_buffer"
        if learning_client not in LEARNING_CLIENTS:
            errs.append("INTELLIGENCE_LEARNING_CLIENT must be trust_buffer or stub")
        elif learning_client == "stub" and not development:
            errs.append("INTELLIGENCE_LEARNING_CLIENT=stub runs only with APP_ENV=development")
        pinned = _model_settings(env, svc, development, errs)
        from intelligence.boundary.cloud.settings import PROVIDERS_VAR, CloudSettings

        cloud = CloudSettings.from_env(env, development, errs)
        if get(PROVIDERS_VAR) is not None and svc != "pipeline":
            errs.append(f"{PROVIDERS_VAR} is for the pipeline service only")
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
            model_client=model_client,
            learning_client=learning_client,
            **pinned,
            cloud=cloud,
        )


def _no_cloud() -> CloudSettings:
    from intelligence.boundary.cloud.settings import CloudSettings

    return CloudSettings()


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
    if not development and dict(url.params).get("sslmode") != "verify-full":
        # The pipeline's queries carry questions and patient data: outside
        # development the connection is TLS with the server's certificate
        # and name checked, never a downgrade libpq would allow.
        errs.append("INTELLIGENCE_DB_URL needs sslmode=verify-full outside development")
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
        read = _read_secret_file(pw_file, name, development, errs)
        if read is None:
            return None, None
        password = read
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


def _read_secret_file(
    path: str,
    name: str,
    development: bool,
    errs: list[str],
    *,
    key_file: bool = False,
    max_bytes: int | None = None,
) -> str | None:
    """A password file's contents, without its trailing newline. It must be
    a regular file; outside development, one that neither group nor others
    can write; one others can read (a root-owned 0444 secret mount, say) is
    allowed with a warning. A key_file (a cloud API key) is stricter, in
    development too: mode 0400 or 0440 only, so one others can read, or
    anyone (the owner too) can write, is refused. With max_bytes, a file
    longer than that (its newline counted) is refused. The mode is checked on the open file, so it is the
    file that was read."""
    try:
        # O_NONBLOCK: a FIFO in place of the file can't hang start-up.
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK)
    except OSError:
        errs.append(f"{name} can't be read")
        return None
    try:
        st = os.fstat(fd)
        regular = stat.S_ISREG(st.st_mode)
    except OSError:
        os.close(fd)
        errs.append(f"{name} can't be read")
        return None
    if not regular:
        os.close(fd)
        errs.append(f"{name} is not a regular file")
        return None
    with os.fdopen(fd, "rb") as f:
        if key_file and st.st_mode & stat.S_IROTH:
            errs.append(f"{name} is readable by others (use mode 0400 or 0440)")
            return None
        if key_file and st.st_mode & (stat.S_IWUSR | stat.S_IWGRP | stat.S_IWOTH):
            errs.append(f"{name} is writable (use mode 0400 or 0440)")
            return None
        if not development:
            if st.st_mode & (stat.S_IWGRP | stat.S_IWOTH):
                errs.append(f"{name} is writable by group or others (use mode 0400 or 0440)")
                return None
            if st.st_mode & stat.S_IROTH:
                log.warning(
                    "%s is readable by others (mode %o); prefer 0400 or 0440",
                    name,
                    stat.S_IMODE(st.st_mode),
                )
        try:
            raw = f.read() if max_bytes is None else f.read(max_bytes + 1)
            if max_bytes is not None and len(raw) > max_bytes:
                errs.append(f"{name} is too long (at most {max_bytes} bytes)")
                return None
            return raw.decode("utf-8").rstrip("\r\n")
        except (OSError, UnicodeDecodeError):
            errs.append(f"{name} can't be read")
            return None


def is_store_placeholder(value: str) -> bool:
    """A store credential that is empty or a known placeholder."""
    v = value.strip().lower()
    return not v or is_placeholder(v) or any(p in v for p in _STORE_PLACEHOLDERS)


def _model_roles(raw: str | None, errs: list[str]) -> tuple[str, ...]:
    from intelligence.modelstore import names
    from intelligence.modelstore.reasons import Refused

    if raw is None or not raw.strip():
        return ()
    roles = [r.strip() for r in raw.split(",")]
    bad = False
    for r in roles:
        try:
            names.check_role(r)
        except Refused:
            bad = True
    if bad or len(set(roles)) != len(roles) or len(roles) > MAX_MODEL_ROLES:
        errs.append(
            "INTELLIGENCE_MODEL_ROLES must be distinct roles (^[a-z][a-z0-9_]{1,31}$), "
            f"comma-separated, at most {MAX_MODEL_ROLES}"
        )
        return ()
    return tuple(roles)


def _model_settings(
    env: Mapping[str, str], svc: str | None, development: bool, errs: list[str]
) -> dict[str, Any]:
    """The pinned models' settings (module docstring), as WorkerSettings fields.
    Anything set is checked; with roles, everything is required. Errors name
    variables only."""

    def get(name: str) -> str | None:
        v = env.get(name, "").strip()
        return v or None

    out: dict[str, Any] = {}
    if env.get("MHAI_MODELS_ENV", "").strip() == "development" and not development:
        errs.append("MHAI_MODELS_ENV=development runs only with APP_ENV=development")
    roles = _model_roles(env.get("INTELLIGENCE_MODEL_ROLES"), errs)
    needed = bool(roles)
    if roles and svc != "pipeline":
        errs.append("INTELLIGENCE_MODEL_ROLES is for the pipeline service only")
    for name in ("MODEL_STORE_READER_ACCESS_KEY", "MODEL_STORE_READER_SECRET_KEY"):
        if name in env:
            errs.append(f"{name} is refused: use {name}_FILE")

    def required(name: str) -> str | None:
        v = get(name)
        if v is None and needed:
            errs.append(f"{name} is required with INTELLIGENCE_MODEL_ROLES")
        return v

    lock_path = required("INTELLIGENCE_MODELS_LOCK")
    cache = required("INTELLIGENCE_MODEL_CACHE")
    if cache is not None and not os.path.isabs(cache):
        errs.append("INTELLIGENCE_MODEL_CACHE must be an absolute path")
    policy = required("MHAI_MODELS_TRUST_POLICY")
    warmup_page = get("INTELLIGENCE_WARMUP_PAGE")
    if warmup_page is not None and not os.path.isabs(warmup_page):
        errs.append("INTELLIGENCE_WARMUP_PAGE must be an absolute path")
    endpoint = required("MODEL_STORE_ENDPOINT")
    bucket = required("MODEL_STORE_BUCKET")
    region = get("MODEL_STORE_REGION")
    if endpoint is not None or bucket is not None or region is not None:
        from intelligence.modelstore import store
        from intelligence.modelstore.reasons import Refused

        for name, value, check in (
            ("MODEL_STORE_ENDPOINT", endpoint, store.check_endpoint),
            ("MODEL_STORE_BUCKET", bucket, store.check_bucket),
            ("MODEL_STORE_REGION", region, store.check_region),
        ):
            if value is None:
                continue
            try:
                check(value)
            except Refused:
                # TLS unless loopback (Task 16's rule); no userinfo, path or query.
                errs.append(f"{name} is invalid")
    credentials: list[str | None] = []
    for name in _STORE_CREDENTIAL_FILES:
        path = required(name)
        credentials.append(None if path is None else _read_store_secret(path, name, errs))
    if roles:
        out.update(
            model_roles=roles,
            models_lock_path=lock_path,
            model_cache_dir=cache,
            model_store_endpoint=endpoint,
            model_store_bucket=bucket,
            model_store_region=region or "us-east-1",
            model_store_access_key=credentials[0],
            model_store_secret_key=credentials[1],
            models_trust_policy_path=policy,
            model_warmup_page=warmup_page,
        )
    return out


def _read_store_secret(path: str, name: str, errs: list[str]) -> str | None:
    """A store credential file's contents (no trailing newline): a regular file,
    never through a link, mode 0600 or tighter (no group or other bits), at most
    4 KiB of ASCII, and not a placeholder. Checked on the open file."""
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        errs.append(f"{name} can't be read (or is a link)")
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            errs.append(f"{name} is not a regular file")
            return None
        if stat.S_IMODE(st.st_mode) & 0o077:
            errs.append(f"{name} must have mode 0600 or tighter")
            return None
        raw = os.read(fd, MAX_SECRET_FILE_BYTES + 1)
    except OSError:
        errs.append(f"{name} can't be read")
        return None
    finally:
        os.close(fd)
    if len(raw) > MAX_SECRET_FILE_BYTES:
        errs.append(f"{name} is too large")
        return None
    try:
        value = raw.decode("ascii").rstrip("\r\n")
    except UnicodeDecodeError:
        errs.append(f"{name} must be ASCII")
        return None
    if is_store_placeholder(value) or any(c.isspace() for c in value):
        errs.append(f"{name} is not a usable credential")
        return None
    return value
