"""The pipeline on its own server (docs/deploy/remote-intelligence.md).

INTELLIGENCE_SINGLE_HOST is off there, so the production rules hold in full:
NATS TLS with a client certificate and an NKey, and Postgres
sslmode=verify-full with sslrootcert naming the CA file (decision D12). The
worker reaches the app host by the name mhai-app (the app-link relay's alias).

Every value here is synthetic.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from intelligence.boundary.config import ConfigError, WorkerSettings

SECRET = "test-only-file-password-0007"
DB = "postgres://mhai-app:5432/mhai?sslmode=verify-full&sslrootcert=/run/tls/ca.pem"


@pytest.fixture
def remote(tmp_path: Path) -> dict[str, str]:
    """deploy/intelligence-host/compose.yml's pipeline settings."""
    f = tmp_path / "pw"
    f.write_text(SECRET + "\n")
    f.chmod(0o400)
    return {
        "INTELLIGENCE_SERVICE": "pipeline",
        "APP_ENV": "production",
        "NATS_URL": "tls://mhai-app:4222",
        "NATS_NKEY_SEED_PATH": "/run/secrets/nats-pipeline.nk",
        "NATS_TLS_CA_PATH": "/run/tls/ca.pem",
        "NATS_TLS_CERT_PATH": "/run/tls/pipeline.pem",
        "NATS_TLS_KEY_PATH": "/run/tls/pipeline-key.pem",
        "INTELLIGENCE_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
        "INTELLIGENCE_DB_URL": DB,
        "INTELLIGENCE_DB_PASSWORD_FILE": str(f),
    }


def test_the_intelligence_host_settings_are_accepted(remote: dict[str, str]) -> None:
    s = WorkerSettings.from_env(remote)
    assert s.single_host is False and s.development is False
    assert s.tls_ca_path == "/run/tls/ca.pem"
    assert s.db_password == SECRET


@pytest.mark.parametrize(
    "drop", ["NATS_TLS_CA_PATH", "NATS_TLS_CERT_PATH", "NATS_TLS_KEY_PATH", "NATS_NKEY_SEED_PATH"]
)
def test_a_remote_host_needs_tls_and_an_nkey(remote: dict[str, str], drop: str) -> None:
    env = {k: v for k, v in remote.items() if k != drop}
    with pytest.raises(ConfigError):
        WorkerSettings.from_env(env)


def test_plaintext_to_a_remote_host_is_refused_even_with_the_switch(
    remote: dict[str, str],
) -> None:
    """The single-host switch can't reach another machine: mhai-app.internal
    has a dot, an address is an address."""
    for host in ("nats://mhai-app.internal:4222", "nats://10.0.1.10:4222"):
        env = {**remote, "INTELLIGENCE_SINGLE_HOST": "on", "NATS_URL": host}
        with pytest.raises(ConfigError, match="INTELLIGENCE_SINGLE_HOST"):
            WorkerSettings.from_env(env)


@pytest.mark.parametrize(
    "query",
    [
        "?sslmode=disable",
        "?sslmode=require&sslrootcert=/run/tls/ca.pem",
        "?sslmode=verify-ca&sslrootcert=/run/tls/ca.pem",
        "?sslmode=prefer",
        "",
    ],
)
def test_postgres_needs_verify_full(remote: dict[str, str], query: str) -> None:
    env = {**remote, "INTELLIGENCE_DB_URL": "postgres://mhai-app:5432/mhai" + query}
    with pytest.raises(ConfigError, match="sslmode=verify-full"):
        WorkerSettings.from_env(env)


@pytest.mark.parametrize(
    "root", ["", "&sslrootcert=", "&sslrootcert=ca.pem", "&sslrootcert=system"]
)
def test_verify_full_needs_the_ca_file(remote: dict[str, str], root: str) -> None:
    """Without sslrootcert libpq looks for ~/.postgresql/root.crt, and
    'system' would trust any public CA for the name: an absolute path only."""
    env = {
        **remote,
        "INTELLIGENCE_DB_URL": "postgres://mhai-app:5432/mhai?sslmode=verify-full" + root,
    }
    with pytest.raises(ConfigError, match="sslrootcert") as e:
        WorkerSettings.from_env(env)
    assert SECRET not in str(e.value)


def test_development_keeps_its_relaxations(remote: dict[str, str]) -> None:
    env = {
        **remote,
        "APP_ENV": "development",
        "INTELLIGENCE_DB_URL": "postgres://mhai-app:5432/mhai?sslmode=verify-full",
    }
    assert WorkerSettings.from_env(env).development is True
