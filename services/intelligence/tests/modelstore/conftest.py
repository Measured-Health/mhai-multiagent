"""Fixtures for the model store tests.

``minio_store`` (tests marked ``store``) starts MinIO, pinned by digest, with
testcontainers, then runs ``deploy/minio/init-model-store.sh`` in the pinned MinIO client
container on the host network, exactly as docker-compose.models.yml does: the bucket,
object lock, default retention, the five users and their policies are the ones compose
makes. The init script and policies are copied into the container, not bind-mounted, so
this works when the tests themselves run in a container (the remote runner, CI); the
users' passwords are copied back out of it. The root password is random per session.
Every object written is synthetic and lives only as long as the container.
"""

from __future__ import annotations

import io
import secrets
import tarfile
import time
import urllib.request
from collections.abc import Iterator, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore.store import Credentials, S3Store

REPO = Path(__file__).resolve().parents[4]
MINIO_DIR = REPO / "deploy" / "minio"
# The last community builds still published (minio/minio and minio/mc are gone); test
# only, AGPL, never shipped. The same digests as docker-compose.models.yml.
MINIO_IMAGE = (
    "bitnamilegacy/minio@sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20"
)
MC_IMAGE = (
    "bitnamilegacy/minio-client"
    "@sha256:73bd39f7899a0cef12b8dd5df13aa93a3ed1aaa44236542442e9ac76819ac158"
)
BUCKET = "mhai-models-test"
ROLES = ("promoter", "signoff", "reader", "eval-reader", "eval-writer")
RETENTION_DAYS = 1
# The policies' cap on remaining lock days. Small here so a test can go past it; the
# stores built below don't share it (they use the default), so the store's own refusal is
# what the tests see.
MAX_RETENTION_DAYS = 30
LABEL = "mhai.test"
MC_UID = 1001  # the client image's user


@dataclass(frozen=True)
class InitRun:
    exit_code: int
    logs: str = field(repr=False)
    secrets: Mapping[str, str] = field(repr=False)
    modes: Mapping[str, int]


@dataclass(frozen=True)
class MinioStore:
    endpoint: str
    bucket: str
    root: Credentials = field(repr=False)
    users: Mapping[str, Credentials] = field(repr=False)
    init: InitRun = field(repr=False)
    _env: Mapping[str, str] = field(repr=False)

    def store(self, role: str = "root") -> S3Store:
        creds = self.root if role == "root" else self.users[role]
        return S3Store(self.endpoint, self.bucket, creds, max_attempts=1)

    def client(self, role: str = "root") -> Any:
        """A raw boto3 client, for the calls S3Store never makes (deletes, shortening)."""
        import boto3
        from botocore.config import Config

        creds = self.root if role == "root" else self.users[role]
        return boto3.session.Session().client(
            "s3",
            endpoint_url=self.endpoint,
            region_name="us-east-1",
            aws_access_key_id=creds.access_key,
            aws_secret_access_key=creds.secret_key,
            config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 1}),
        )

    def run_init(self, *, keep_secrets: bool = True) -> InitRun:
        """Run the init script again, with the session's passwords in its secrets
        directory (as a compose re-run would have them) unless ``keep_secrets`` is
        false."""
        return run_init(self._env, self.init.secrets if keep_secrets else None)


def _tar_dir(src: Path, arcname: str) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        tar.add(src, arcname=arcname)
    return buf.getvalue()


def _tar_secrets(existing: Mapping[str, str]) -> bytes:
    """A ``secrets/`` directory holding ``existing``, owned by the client image's user."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        d = tarfile.TarInfo("secrets")
        d.type, d.mode, d.uid, d.gid = tarfile.DIRTYPE, 0o700, MC_UID, 0
        tar.addfile(d)
        for name, value in existing.items():
            raw = value.encode("ascii")
            f = tarfile.TarInfo(f"secrets/{name}")
            f.size, f.mode, f.uid, f.gid = len(raw), 0o600, MC_UID, 0
            tar.addfile(f, io.BytesIO(raw))
    return buf.getvalue()


def run_init(env: Mapping[str, str], existing: Mapping[str, str] | None = None) -> InitRun:
    """Run the init script in the pinned client image; return its exit code, its logs and
    the passwords and file modes it left in the secrets directory."""
    import docker

    client = docker.from_env()
    ctr = client.containers.create(
        MC_IMAGE,
        entrypoint=["/bin/sh", "/tmp/init/init-model-store.sh"],
        environment=dict(env),
        network_mode="host",
        labels={LABEL: "model-store-init"},
    )
    try:
        ctr.put_archive("/tmp", _tar_dir(MINIO_DIR, "init"))
        if existing is not None:
            ctr.put_archive("/tmp", _tar_secrets(existing))
        ctr.start()
        code = int(ctr.wait(timeout=180)["StatusCode"])
        logs = bytes(ctr.logs(stdout=True, stderr=True)).decode("utf-8", "replace")
        found: dict[str, str] = {}
        modes: dict[str, int] = {}
        if code == 0:
            stream, _ = ctr.get_archive("/tmp/secrets")
            with tarfile.open(fileobj=io.BytesIO(b"".join(stream))) as tar:
                for member in tar.getmembers():
                    name = member.name.rsplit("/", 1)[-1]
                    if member.isfile():
                        f = tar.extractfile(member)
                        assert f is not None
                        found[name] = f.read().decode("ascii")
                    modes[name if member.isfile() else "."] = member.mode & 0o777
        return InitRun(code, logs, found, modes)
    finally:
        ctr.remove(force=True, v=True)
        client.close()


def _wait_ready(endpoint: str, deadline_s: float = 60.0) -> None:
    end = time.monotonic() + deadline_s
    while time.monotonic() < end:
        try:
            with urllib.request.urlopen(f"{endpoint}/minio/health/live", timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(0.5)
    raise RuntimeError("MinIO did not start")


@pytest.fixture(scope="session")
def minio_store() -> Iterator[MinioStore]:
    from testcontainers.core.container import DockerContainer

    root = Credentials("mhai-test-root", secrets.token_hex(20))
    c = (
        DockerContainer(MINIO_IMAGE)
        .with_kwargs(entrypoint=["minio"])
        .with_command(["server", "/bitnami/minio/data", "--address", ":9000"])
        .with_env("MINIO_ROOT_USER", root.access_key)
        .with_env("MINIO_ROOT_PASSWORD", root.secret_key)
        .with_exposed_ports(9000)
    )
    c.start()
    try:
        # The published port on the host's loopback: the remote runner and CI run the
        # tests with host networking, and the init container uses the host network too.
        # Loopback is also the only place S3Store accepts plain HTTP.
        endpoint = f"http://127.0.0.1:{int(c.get_exposed_port(9000))}"
        _wait_ready(endpoint)
        env = {
            "MINIO_URL": endpoint,
            "MINIO_ROOT_USER": root.access_key,
            "MINIO_ROOT_PASSWORD": root.secret_key,
            "MODEL_STORE_BUCKET": BUCKET,
            "MODEL_STORE_DEFAULT_RETENTION_DAYS": str(RETENTION_DAYS),
            "MODEL_STORE_MAX_RETENTION_DAYS": str(MAX_RETENTION_DAYS),
            "MODEL_STORE_SECRETS_DIR": "/tmp/secrets",
        }
        init = run_init(env)
        if init.exit_code != 0:
            raise RuntimeError(f"init-model-store.sh failed: {init.logs[-2000:]}")
        users = {role: Credentials(role, init.secrets[role]) for role in ROLES}
        yield MinioStore(endpoint, BUCKET, root, users, init, env)
    finally:
        c.stop()
