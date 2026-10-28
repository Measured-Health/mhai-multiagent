"""Fixtures for the boundary tests.

The Docker fixtures (nats_server, postgres) start only when a test marked
`boundary` asks for them. NATS runs the real services/nats/nats.conf (and
the bff_clinical user it includes) with throwaway test-only passwords, the
same names and values as core's internal/testutil/nats.go. Postgres 16 is
migrated by core's own image (`mhai-core:test migrate up`), so the schema is
the one production uses. Every id, key and password here is synthetic.
"""

from __future__ import annotations

import os
import time
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest

from tests.boundary.tokens import Signer

REPO = Path(__file__).resolve().parents[4]
NATS_DIR = REPO / "services" / "nats"
NATS_IMAGE = "nats:2.10-alpine"
POSTGRES_IMAGE = "postgres:16-alpine"
CORE_IMAGE = os.environ.get("MHAI_CORE_TEST_IMAGE", "mhai-core:test")

# Test-only credentials (core's internal/testutil/nats.go uses the same).
NATS_ENV = {
    "NATS_GATEWAY_PASSWORD": "test-only-gateway-pw",
    "NATS_PIPELINE_PASSWORD": "test-only-pipeline-pw",
    "NATS_ECHO_PASSWORD": "test-only-echo-pw",
    "NATS_BFF_CLINICAL_PASSWORD": "test-only-bff-clinical-pw",
    "NATS_BFF_PLATFORM_PASSWORD": "test-only-bff-platform-pw",
    "NATS_JS_KEY": "testOnlyJetStreamKeyNotForAnyRealServer0123",
}
_USERS = {
    "gateway": "NATS_GATEWAY_PASSWORD",
    "pipeline": "NATS_PIPELINE_PASSWORD",
    "echo": "NATS_ECHO_PASSWORD",
    "bff_clinical": "NATS_BFF_CLINICAL_PASSWORD",
    "bff_platform": "NATS_BFF_PLATFORM_PASSWORD",
}


@dataclass(frozen=True)
class NatsServer:
    host: str
    port: int

    @property
    def addr(self) -> str:
        return f"{self.host}:{self.port}"

    def url(self, user: str) -> str:
        """The URL for user, with its test-only password."""
        return f"nats://{user}:{NATS_ENV[_USERS[user]]}@{self.addr}"


@contextmanager
def start_nats() -> Iterator[NatsServer]:
    """A NATS 2.10 container running the real nats.conf, stopped on exit."""
    from testcontainers.core.container import DockerContainer
    from testcontainers.core.wait_strategies import LogMessageWaitStrategy

    # Copied in, not bind-mounted, so it works when the tests themselves run
    # in a container (the remote runner, CI).
    c = (
        DockerContainer(NATS_IMAGE)
        .with_exposed_ports(4222)
        .with_command("-c /etc/nats/nats.conf")
        .with_envs(**NATS_ENV)
        .with_copy_into_container(NATS_DIR / "nats.conf", "/etc/nats/nats.conf")
        .with_copy_into_container(
            NATS_DIR / "bff-clinical.test.conf", "/etc/nats/bff-clinical.test.conf"
        )
        .waiting_for(LogMessageWaitStrategy("Server is ready"))
    )
    c.start()
    try:
        yield NatsServer(c.get_container_host_ip(), int(c.get_exposed_port(4222)))
    finally:
        c.stop()


@pytest.fixture(scope="session")
def nats_server() -> Iterator[NatsServer]:
    with start_nats() as server:
        yield server


@pytest.fixture
def fresh_nats() -> Iterator[NatsServer]:
    """A NATS server of the test's own, so its JetStream streams start
    empty (no role may delete a stream, so a shared server can't be reset)."""
    with start_nats() as server:
        yield server


# The pipeline's login, provisioned by `core migrate up` (spec §8.2).
INTELLIGENCE_DB_USER = "intelligence_app"
INTELLIGENCE_DB_PASSWORD = "test-only-intelligence-db-pw"


@dataclass(frozen=True)
class Postgres:
    url: str  # superuser, test-only
    container: Any
    host: str = ""
    port: int = 0

    @property
    def intelligence_url(self) -> str:
        """Where the pipeline connects, without credentials (those go to
        Database separately)."""
        return f"postgres://{self.host}:{self.port}/mhai?sslmode=disable"

    def psql(self, sql: str) -> str:
        """One statement as the superuser, through psql in the container
        (no driver needed)."""
        code, out = self.container.exec(
            ["psql", "-U", "mhai_test", "-d", "mhai", "-tAX", "-v", "ON_ERROR_STOP=1", "-c", sql]
        )
        if code != 0:
            raise RuntimeError("psql failed")
        return bytes(out).decode().strip()


@pytest.fixture(scope="session")
def postgres() -> Iterator[Postgres]:
    import docker
    from testcontainers.community.postgres import PostgresContainer

    c = PostgresContainer(
        POSTGRES_IMAGE, username="mhai_test", password="test-only-pg-pw", dbname="mhai"
    )
    c.start()
    try:
        host, port = c.get_container_host_ip(), int(c.get_exposed_port(5432))
        url = f"postgres://mhai_test:test-only-pg-pw@{host}:{port}/mhai?sslmode=disable"
        client = docker.from_env()
        try:
            client.containers.run(
                CORE_IMAGE,
                ["migrate", "up"],
                environment={
                    "DATABASE_URL": url,
                    "INTELLIGENCE_DB_PASSWORD": INTELLIGENCE_DB_PASSWORD,
                },
                network_mode="host",
                remove=True,
            )
        finally:
            client.close()
        yield Postgres(url, c, host, port)
    finally:
        c.stop()


@pytest.fixture
def signer() -> Signer:
    """The vector key (tests/boundary/tokens.py, checked against the Go
    vectors in test_token_vectors.py)."""
    return Signer()


def now_s() -> int:
    return int(time.time())
