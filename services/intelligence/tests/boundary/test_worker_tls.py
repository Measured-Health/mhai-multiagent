"""The worker against NATS with TLS on (docs/deploy/remote-intelligence.md).

The server runs deploy/nats/tls.conf, the split mode's block (verify: true,
handshake_first: 300ms), included around the dev services/nats/nats.conf so
the test can sign in with the dev passwords. The production worker
(development False, so TLS first) must:

- connect with the CA and its client certificate, over TLS;
- refuse a server whose certificate another CA signed;
- refuse a server certificate for another name.

And the server must refuse a client with no certificate, while an INFO-first
client (core, the BFFs) still gets TLS through handshake_first.

Every certificate is made here, at test time, and is thrown away.
"""

from __future__ import annotations

import asyncio
import datetime
import ipaddress
import ssl
import time
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

import nats
import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import pipeline
from intelligence.boundary.verify import Verifier
from intelligence.boundary.worker import Worker, _is_tls
from tests.boundary.conftest import NATS_DIR, NATS_ENV, NATS_IMAGE, REPO
from tests.boundary.tokens import Signer

pytestmark = pytest.mark.boundary

PIPELINE_PW = NATS_ENV["NATS_PIPELINE_PASSWORD"]


@dataclass(frozen=True)
class Ca:
    key: ec.EllipticCurvePrivateKey
    cert: x509.Certificate

    def pem(self) -> bytes:
        return self.cert.public_bytes(serialization.Encoding.PEM)


def make_ca(name: str) -> Ca:
    k = ec.generate_private_key(ec.SECP256R1())
    n = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    now = datetime.datetime.now(datetime.UTC)
    c = (
        x509.CertificateBuilder()
        .subject_name(n)
        .issuer_name(n)
        .public_key(k.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
        .add_extension(
            x509.KeyUsage(
                digital_signature=True,
                content_commitment=False,
                key_encipherment=False,
                data_encipherment=False,
                key_agreement=False,
                key_cert_sign=True,
                crl_sign=True,
                encipher_only=False,
                decipher_only=False,
            ),
            critical=True,
        )
        .sign(k, hashes.SHA256())
    )
    return Ca(k, c)


def issue(ca: Ca, cn: str, *, server: bool, names: list[str]) -> tuple[bytes, bytes]:
    """(cert PEM, key PEM) for cn, signed by ca, test-only."""
    k = ec.generate_private_key(ec.SECP256R1())
    now = datetime.datetime.now(datetime.UTC)
    sans: list[x509.GeneralName] = []
    for n in names:
        try:
            sans.append(x509.IPAddress(ipaddress.ip_address(n)))
        except ValueError:
            sans.append(x509.DNSName(n))
    eku = ExtendedKeyUsageOID.SERVER_AUTH if server else ExtendedKeyUsageOID.CLIENT_AUTH
    c = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, cn)]))
        .issuer_name(ca.cert.subject)
        .public_key(k.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
        .add_extension(x509.ExtendedKeyUsage([eku]), critical=False)
        .add_extension(x509.SubjectAlternativeName(sans), critical=False)
        .sign(ca.key, hashes.SHA256())
    )
    key = k.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    return c.public_bytes(serialization.Encoding.PEM), key


@dataclass(frozen=True)
class TlsNats:
    host: str
    port: int


@contextmanager
def start_tls_nats(ca: Ca, server_names: list[str], *, add_host: bool = True) -> Iterator[TlsNats]:
    """NATS with deploy/nats/tls.conf wrapped around the dev config. The
    server certificate is issued after the container starts, so it can name
    the address the tests dial (the container host: localhost, or the Docker
    gateway when the tests themselves run in a container)."""
    import io
    import tarfile

    from testcontainers.core.container import DockerContainer
    from testcontainers.core.waiting_utils import wait_for_logs

    c = (
        DockerContainer(NATS_IMAGE)
        .with_exposed_ports(4222)
        .with_command(
            [
                "sh",
                "-c",
                "until [ -f /etc/nats/tls/ready ]; do sleep 0.1; done; "
                "exec nats-server -c /etc/nats/tls.conf",
            ]
        )
        .with_envs(**NATS_ENV)
        .with_copy_into_container(NATS_DIR / "nats.conf", "/etc/nats/nats.conf")
        .with_copy_into_container(
            NATS_DIR / "bff-clinical.test.conf", "/etc/nats/bff-clinical.test.conf"
        )
        .with_copy_into_container(NATS_DIR / "emr-off.conf", "/etc/nats/emr.conf")
        .with_copy_into_container(NATS_DIR / "documents-off.conf", "/etc/nats/documents.conf")
        .with_copy_into_container(
            NATS_DIR / "notifications-off.conf", "/etc/nats/notifications.conf"
        )
        .with_copy_into_container(REPO / "deploy" / "nats" / "tls.conf", "/etc/nats/tls.conf")
        .with_copy_into_container(ca.pem(), "/etc/nats/tls/ca.pem")
    )
    c.start()
    try:
        host = c.get_container_host_ip()
        names = [*server_names, host] if add_host else server_names
        cert, key = issue(ca, "nats", server=True, names=names)
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            for name, data in (("server.pem", cert), ("server-key.pem", key), ("ready", b"")):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o600
                tar.addfile(info, io.BytesIO(data))
        c.get_wrapped_container().put_archive("/etc/nats/tls", buf.getvalue())
        wait_for_logs(c, "Server is ready", timeout=30)
        yield TlsNats(host, int(c.get_exposed_port(4222)))
    finally:
        c.stop()


@pytest.fixture(scope="module")
def ca() -> Ca:
    return make_ca("test-only split CA")


@pytest.fixture(scope="module")
def tls_nats(ca: Ca) -> Iterator[TlsNats]:
    with start_tls_nats(ca, ["mhai-app"]) as s:
        yield s


@pytest.fixture
def client_files(tmp_path: Path, ca: Ca) -> tuple[str, str, str]:
    cert, key = issue(ca, "pipeline", server=False, names=["pipeline"])
    (tmp_path / "ca.pem").write_bytes(ca.pem())
    (tmp_path / "pipeline.pem").write_bytes(cert)
    k = tmp_path / "pipeline-key.pem"
    k.write_bytes(key)
    k.chmod(0o600)
    return str(tmp_path / "ca.pem"), str(tmp_path / "pipeline.pem"), str(k)


def _worker(url: str, files: tuple[str, str, str], keys: Path) -> Worker:
    signer = Signer()
    import json

    keys.write_text(json.dumps({"keys": signer.encoded_keys()}))
    ca, cert, key = files
    s = WorkerSettings(
        service="pipeline",
        nats_url=url,
        trusted_keys_path=str(keys),
        tls_ca_path=ca,
        tls_cert_path=cert,
        tls_key_path=key,
        development=False,  # production: TLS first
    )
    v = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()),
        "pipeline",
        Operations.load(development=False),
        JtiSet(time.time),
        time.time,
    )
    return Worker(s, pipeline.handlers(), v)


def _url(host: str, port: int) -> str:
    return f"tls://pipeline:{PIPELINE_PW}@{host}:{port}"


async def test_worker_connects_tls_first_with_the_ca_and_its_certificate(
    tls_nats: TlsNats, client_files: tuple[str, str, str], tmp_path: Path
) -> None:
    w = _worker(_url(tls_nats.host, tls_nats.port), client_files, tmp_path / "keys.json")
    try:
        await asyncio.wait_for(w.start(), 10)
        assert _is_tls(w.connection)
        assert w._sub is not None  # joined the queue group
    finally:
        await w.abort()


async def test_worker_refuses_a_server_signed_by_another_ca(
    tls_nats: TlsNats, tmp_path: Path
) -> None:
    other = make_ca("test-only other CA")
    cert, key = issue(other, "pipeline", server=False, names=["pipeline"])
    (tmp_path / "ca.pem").write_bytes(other.pem())
    (tmp_path / "c.pem").write_bytes(cert)
    (tmp_path / "k.pem").write_bytes(key)
    files = (str(tmp_path / "ca.pem"), str(tmp_path / "c.pem"), str(tmp_path / "k.pem"))
    w = _worker(_url(tls_nats.host, tls_nats.port), files, tmp_path / "keys.json")
    with pytest.raises((TimeoutError, OSError, ssl.SSLError, nats.errors.Error)):
        await asyncio.wait_for(w.start(), 4)
    assert w._sub is None
    await w.abort()


async def test_worker_refuses_a_certificate_for_another_name(
    ca: Ca, client_files: tuple[str, str, str], tmp_path: Path
) -> None:
    with start_tls_nats(ca, ["other.example.test"], add_host=False) as s:
        w = _worker(_url(s.host, s.port), client_files, tmp_path / "keys.json")
        with pytest.raises((TimeoutError, OSError, ssl.SSLError, nats.errors.Error)):
            await asyncio.wait_for(w.start(), 4)
        assert w._sub is None
        await w.abort()


async def test_server_refuses_a_client_without_a_certificate(
    tls_nats: TlsNats, client_files: tuple[str, str, str]
) -> None:
    ctx = ssl.create_default_context(cafile=client_files[0])  # trusts the server, no cert
    with pytest.raises((TimeoutError, OSError, ssl.SSLError, nats.errors.Error)):
        nc = await asyncio.wait_for(
            nats.connect(
                _url(tls_nats.host, tls_nats.port),
                tls=ctx,
                tls_handshake_first=True,
                allow_reconnect=False,
                connect_timeout=2,
            ),
            5,
        )
        await nc.close()


async def test_info_first_client_still_gets_tls(
    tls_nats: TlsNats, client_files: tuple[str, str, str]
) -> None:
    """Core and the BFFs read INFO first; handshake_first's fallback upgrades
    them to TLS (never plaintext) and verify still applies."""
    ca, cert, key = client_files
    ctx = ssl.create_default_context(cafile=ca)
    ctx.load_cert_chain(cert, key)
    gw = f"tls://gateway:{NATS_ENV['NATS_GATEWAY_PASSWORD']}@{tls_nats.host}:{tls_nats.port}"
    nc = await asyncio.wait_for(
        nats.connect(gw, tls=ctx, allow_reconnect=False, connect_timeout=3), 6
    )
    try:
        assert _is_tls(nc)
    finally:
        await nc.close()
