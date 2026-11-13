"""INTELLIGENCE_SINGLE_HOST=on: the pipeline on the same Docker host as
NATS and Postgres, without TLS on those two links (the pilot on the demo
host, deploy/pilot/README.md).

Outside development the worker needs NATS TLS with a client certificate and
Postgres sslmode=verify-full. The demo host runs neither (deploy/README.md,
"Known limits": TLS is required before any worker runs off this host).
This switch is the narrow exception, and these tests hold its edges:

- it is off unless the value is exactly "on";
- it only relaxes TLS: an NKey is still required, a NATS or database
  password in the URL is still refused, and the database password still
  comes from a file or variable;
- every NATS server and the database host must be a single-label Docker
  service name (no dot, no IP address, not localhost), so it can never
  point at another machine;
- only nats:// (plaintext on the Docker network) and sslmode disable or
  verify-full are accepted;
- the worker then connects without TLS and without a TLS-first handshake,
  and says so in a warning at start.

Every value here is synthetic.
"""

from __future__ import annotations

import asyncio
import logging
import time
from pathlib import Path
from typing import Any

import pytest

from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import pipeline
from intelligence.boundary.verify import Verifier
from intelligence.boundary.worker import Worker, WorkerClient
from tests.boundary.tokens import Signer

SECRET = "test-only-file-password-0003"


@pytest.fixture
def pw_file(tmp_path: Path) -> Path:
    f = tmp_path / "pw"
    f.write_text(SECRET + "\n")
    f.chmod(0o400)
    return f


@pytest.fixture
def single(pw_file: Path) -> dict[str, str]:
    """The pilot pipeline's transport settings on the demo host."""
    return {
        "INTELLIGENCE_SERVICE": "pipeline",
        "APP_ENV": "production",
        "INTELLIGENCE_SINGLE_HOST": "on",
        "NATS_URL": "nats://nats:4222",
        "NATS_NKEY_SEED_PATH": "/run/secrets/nats-pipeline.nk",
        "INTELLIGENCE_TRUSTED_KEYS_PATH": "/trusted/request-keys.json",
        "INTELLIGENCE_DB_URL": "postgres://postgres:5432/mhai?sslmode=disable",
        "INTELLIGENCE_DB_PASSWORD_FILE": str(pw_file),
    }


def test_single_host_allows_plaintext_on_docker_service_names(single: dict[str, str]) -> None:
    s = WorkerSettings.from_env(single)
    assert s.single_host is True
    assert s.development is False
    assert s.tls_ca_path is None
    assert s.db_password == SECRET


def test_without_the_switch_production_still_needs_tls(single: dict[str, str]) -> None:
    env = {k: v for k, v in single.items() if k != "INTELLIGENCE_SINGLE_HOST"}
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env(env)
    assert "NATS_TLS_CA_PATH" in str(e.value)
    assert "sslmode=verify-full" in str(e.value)


@pytest.mark.parametrize("value", ["1", "true", "yes", "ON", "On", "off", "enabled"])
def test_only_the_exact_value_on(single: dict[str, str], value: str) -> None:
    with pytest.raises(ConfigError, match="INTELLIGENCE_SINGLE_HOST"):
        WorkerSettings.from_env({**single, "INTELLIGENCE_SINGLE_HOST": value})


def test_empty_is_off(single: dict[str, str]) -> None:
    with pytest.raises(ConfigError, match="NATS_TLS_CA_PATH"):
        WorkerSettings.from_env({**single, "INTELLIGENCE_SINGLE_HOST": ""})


def test_still_needs_an_nkey(single: dict[str, str]) -> None:
    env = {k: v for k, v in single.items() if k != "NATS_NKEY_SEED_PATH"}
    with pytest.raises(ConfigError, match="NATS_NKEY_SEED_PATH"):
        WorkerSettings.from_env(env)


def test_still_refuses_passwords_in_urls(single: dict[str, str]) -> None:
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env(
            {**single, "NATS_URL": "nats://pipeline:test-only-pw-0004@nats:4222"}
        )
    assert "NATS_URL" in str(e.value) and "test-only-pw-0004" not in str(e.value)
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env(
            {
                **single,
                "INTELLIGENCE_DB_URL": "postgres://intelligence_app:test-only-pw-0005@postgres:5432/mhai?sslmode=disable",
            }
        )
    assert "must not carry a password" in str(e.value) and "test-only-pw-0005" not in str(e.value)


@pytest.mark.parametrize(
    "url",
    [
        "nats://nats.internal:4222",
        "nats://10.0.0.5:4222",
        "nats://[::1]:4222",
        "nats://localhost:4222",
        "nats://nats:4222,nats://other.example:4222",
        "tls://nats:4222",
        "ws://nats:4222",
    ],
)
def test_nats_must_be_a_plain_docker_service_name(single: dict[str, str], url: str) -> None:
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**single, "NATS_URL": url})
    assert "INTELLIGENCE_SINGLE_HOST" in str(e.value)
    assert url not in str(e.value)


@pytest.mark.parametrize(
    "url",
    [
        "postgres://db.internal:5432/mhai?sslmode=disable",
        "postgres://172.18.0.2:5432/mhai?sslmode=disable",
        "postgres://localhost:5432/mhai?sslmode=disable",
    ],
)
def test_database_must_be_a_docker_service_name(single: dict[str, str], url: str) -> None:
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**single, "INTELLIGENCE_DB_URL": url})
    assert "INTELLIGENCE_SINGLE_HOST" in str(e.value)
    assert url not in str(e.value)


@pytest.mark.parametrize("mode", ["", "?sslmode=require", "?sslmode=prefer", "?sslmode=verify-ca"])
def test_database_sslmode_disable_or_verify_full_only(single: dict[str, str], mode: str) -> None:
    with pytest.raises(ConfigError, match="sslmode"):
        WorkerSettings.from_env(
            {**single, "INTELLIGENCE_DB_URL": "postgres://postgres:5432/mhai" + mode}
        )
    s = WorkerSettings.from_env(
        {**single, "INTELLIGENCE_DB_URL": "postgres://postgres:5432/mhai?sslmode=verify-full"}
    )
    assert s.single_host is True


def test_single_host_is_logged_at_start(
    single: dict[str, str], caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="intelligence.boundary.config"):
        WorkerSettings.from_env(single)
    assert any("INTELLIGENCE_SINGLE_HOST=on" in r.getMessage() for r in caplog.records)
    assert SECRET not in caplog.text


def _worker(single_host: bool) -> Worker:
    signer = Signer()
    ops = Operations.load(development=False)
    v = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()),
        "pipeline",
        ops,
        JtiSet(time.time),
        time.time,
    )
    s = WorkerSettings(
        service="pipeline",
        nats_url="nats://nats:4222",
        trusted_keys_path="/x",
        nkey_seed_path="/run/secrets/nats-pipeline.nk",
        development=False,
        single_host=single_host,
    )
    return Worker(s, pipeline.handlers(), v)


def test_single_host_worker_connects_without_tls(monkeypatch: pytest.MonkeyPatch) -> None:
    seen: dict[str, Any] = {}

    async def fake_connect(self: WorkerClient, **kw: Any) -> None:
        seen.update(kw)

    monkeypatch.setattr(WorkerClient, "connect", fake_connect)
    w = _worker(single_host=True)
    asyncio.run(w._connect())
    assert seen["tls"] is None
    assert seen["tls_handshake_first"] is False
    assert seen["nkeys_seed"] == "/run/secrets/nats-pipeline.nk"
    assert w._require_tls is False


def test_production_worker_without_the_switch_still_refuses(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def fake_connect(self: WorkerClient, **kw: Any) -> None:
        raise AssertionError("must not connect")

    monkeypatch.setattr(WorkerClient, "connect", fake_connect)
    with pytest.raises(ConfigError, match="NATS_TLS_CA_PATH"):
        asyncio.run(_worker(single_host=False)._connect())


def test_single_host_worker_rechecks_the_servers_at_connect(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The settings check the names; the connect checks them again, so a
    WorkerSettings built another way can't use the exception off-host."""

    async def fake_connect(self: WorkerClient, **kw: Any) -> None:
        raise AssertionError("must not connect")

    monkeypatch.setattr(WorkerClient, "connect", fake_connect)
    w = _worker(single_host=True)
    object.__setattr__(w.settings, "nats_url", "nats://nats.example.org:4222")
    with pytest.raises(ConfigError, match="INTELLIGENCE_SINGLE_HOST"):
        asyncio.run(w._connect())
