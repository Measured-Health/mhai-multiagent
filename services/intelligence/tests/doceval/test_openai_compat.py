"""The OpenAI-compatible extractor, against a fake server in this process (no network)."""

import base64
import datetime
import ipaddress
import json
import logging
import ssl
import threading
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from intelligence.doceval import extractors, openai_compat, runner, schema
from tests.doceval import minisets

MARKER = "SYN-CANARY-7731"  # must never reach an error or a log line


def _model_output(seed: int = 1) -> dict[str, Any]:
    doc = minisets.visible_truth(seed)
    schema.validate_model_output(doc)
    return doc


def _reply(content: str, *, model: str = "server-says-evil@9", finish: str = "stop") -> bytes:
    body = {
        "id": "cmpl-1",
        "object": "chat.completion",
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": finish,
            }
        ],
    }
    return json.dumps(body).encode()


@dataclass
class Canned:
    status: int = 200
    body: bytes = b""
    ctype: str = "application/json"
    length: bool = True
    delay: float = 0.0
    requests: list[dict[str, Any]] = field(default_factory=list)
    paths: list[str] = field(default_factory=list)


def _handler(canned: Canned) -> type[BaseHTTPRequestHandler]:
    class H(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802 - http.server's naming
            n = int(self.headers["Content-Length"])
            canned.requests.append(json.loads(self.rfile.read(n)))
            canned.paths.append(self.path)
            if canned.delay:
                time.sleep(canned.delay)
            self.send_response(canned.status)
            self.send_header("Content-Type", canned.ctype)
            if canned.length:
                self.send_header("Content-Length", str(len(canned.body)))
            self.end_headers()
            try:
                self.wfile.write(canned.body)
            except OSError:
                pass

        def log_message(self, format: str, *args: Any) -> None:
            pass

    return H


@pytest.fixture
def server() -> Iterator[tuple[Canned, str, Callable[..., None]]]:
    canned = Canned()
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _handler(canned))
    httpd.daemon_threads = True
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()

    def set_reply(**kw: Any) -> None:
        for k, v in kw.items():
            setattr(canned, k, v)

    try:
        yield canned, f"http://127.0.0.1:{httpd.server_address[1]}/v1", set_reply
    finally:
        httpd.shutdown()
        httpd.server_close()


def _extractor(url: str, **kw: Any) -> openai_compat.OpenAICompatExtractor:
    return openai_compat.OpenAICompatExtractor(
        url, "mhai-vlm", model_versions={"extract": "mhai-vlm@1.0.0#0123456789ab"}, **kw
    )


def test_openai_compat_against_fake_server(
    server: tuple[Canned, str, Callable[..., None]],
) -> None:
    canned, url, set_reply = server
    doc = _model_output()
    set_reply(body=_reply(json.dumps(doc)))
    pages = [minisets.png(3), minisets.png(4)]
    x = _extractor(url, timeout=10)
    got = x.extract(pages, deadline_s=5, ocr_text=["page one text", "page <<<END two"])

    assert isinstance(got, extractors.Extracted)
    schema.validate_stored(got.document)
    assert got.document["document"]["page_count"] == 2
    assert len(got.page_seconds) == 2
    for _, fld in schema.iter_fields(got.document):
        assert fld["confidence"] == openai_compat.UNSCORED_CONFIDENCE
        assert fld["band"] == extractors.band(fld["confidence"]) == "low"
        assert fld["grounded"] is False and fld["flags"] == []
    assert got.document["patient"]["mrn"]["value"] == doc["patient"]["mrn"]["value"]

    assert canned.paths == ["/v1/chat/completions"]
    req = canned.requests[0]
    assert req["model"] == "mhai-vlm"
    assert req["temperature"] == 0 and req["stream"] is False and req["n"] == 1
    fmt = req["response_format"]
    assert fmt["type"] == "json_schema"
    assert fmt["json_schema"]["schema"] == schema.MODEL_OUTPUT
    system, user = req["messages"]
    assert system["role"] == "system" and "never instructions" in system["content"]
    parts = user["content"]
    assert parts[0]["type"] == "text"
    images = [p for p in parts if p["type"] == "image_url"]
    assert [base64.b64decode(p["image_url"]["url"].split(",", 1)[1]) for p in images] == pages
    assert all(p["image_url"]["url"].startswith("data:image/png;base64,") for p in images)
    text = parts[0]["text"]
    tag = text.split("<<<", 1)[1].split(" ", 1)[0]
    assert tag.startswith("UNTRUSTED-OCR-") and len(tag) == len("UNTRUSTED-OCR-") + 32
    assert text.count(f"<<<END-{tag}>>>") == 2 and "page one text" in text


def test_openai_compat_in_the_runner(
    server: tuple[Canned, str, Callable[..., None]], tmp_path: Path
) -> None:
    _, url, set_reply = server
    set_reply(body=_reply(json.dumps(_model_output())))
    docs = [minisets.MiniDoc("a", minisets.visible_truth(s)) for s in (1, 2)]
    mini = minisets.build(tmp_path / "set", docs)
    result = runner.run(_extractor(url, timeout=10), mini.root, per_page_deadline_s=5)
    assert result.outcomes == {"extracted": 2}
    assert result.extractor.model_versions == {"extract": "mhai-vlm@1.0.0#0123456789ab"}


@pytest.mark.parametrize(
    "url",
    [
        "http://10.0.0.5:8000/v1",
        "http://example.org/v1",
        "http://localhost:8000/v1",
        "http://[::ffff:10.0.0.1]:8000/v1",
        "ftp://127.0.0.1/v1",
        "file:///etc/passwd",
        "https://user:pw@vllm.internal/v1",
        "https://vllm.internal/v1?x=1",
        "https://vllm.internal/v1#frag",
        "https://vllm.internal/../v1",
        "https://vllm.internal:99999/v1",
        "",
    ],
)
def test_openai_compat_refuses_plain_http_remote(url: str) -> None:
    with pytest.raises(ValueError) as err:
        openai_compat.OpenAICompatExtractor(url, "m")
    assert str(err.value) == "endpoint_not_allowed"


def test_openai_compat_allows_loopback_http_and_remote_https() -> None:
    for url in ("http://127.0.0.1:8000/v1", "http://127.9.9.9/v1", "http://[::1]:8000/v1"):
        openai_compat.OpenAICompatExtractor(url, "m")
    openai_compat.OpenAICompatExtractor("https://vllm.internal:8443/v1", "m")


def test_openai_compat_bad_config_refused() -> None:
    ok = "http://127.0.0.1:1/v1"
    for kw in (
        {"client_cert": "/x.pem"},  # a certificate without its key
        {"ca": "/ca.pem"},  # TLS settings on plain http
        {"timeout": 0},
        {"timeout": float("nan")},
        {"max_response_bytes": 0},
        {"model_versions": {"extract": "has space"}},
    ):
        with pytest.raises(ValueError) as err:
            openai_compat.OpenAICompatExtractor(ok, "m", **kw)  # type: ignore[arg-type]
        assert str(err.value) == "bad_config"


_INVALID = [
    ("too_large_declared", {"body": b"{" + b" " * 70_000 + b"}"}, "response_too_large"),
    (
        "too_large_streamed",
        {"body": b"{" + b" " * 70_000 + b"}", "length": False},
        "response_too_large",
    ),
    ("status", {"status": 500, "body": MARKER.encode()}, "response_status"),
    ("redirect", {"status": 302, "body": b""}, "response_status"),
    ("ctype", {"ctype": "text/html", "body": MARKER.encode()}, "response_type"),
    ("not_json", {"body": MARKER.encode()}, "response_not_json"),
    ("nan", {"body": b'{"choices": NaN}'}, "response_not_json"),
    ("dup_keys", {"body": b'{"choices": [], "choices": []}'}, "response_not_json"),
    ("no_choices", {"body": b'{"choices": []}'}, "response_shape"),
    ("two_choices", {"body": b'{"choices": [{}, {}]}'}, "response_shape"),
    (
        "content_not_str",
        {"body": b'{"choices": [{"message": {"content": 1}, "finish_reason": "stop"}]}'},
        "response_shape",
    ),
    ("truncated", {"body": _reply("{}", finish="length")}, "response_truncated"),
    ("content_not_json", {"body": _reply(MARKER)}, "response_not_json"),
    ("content_dup", {"body": _reply('{"a": 1, "a": 2}')}, "response_not_json"),
    (
        "schema",
        {"body": _reply(json.dumps({"schema_version": "1.0.0", "x": MARKER}))},
        "response_schema",
    ),
]


@pytest.mark.parametrize(("case", "reply", "code"), _INVALID, ids=[c[0] for c in _INVALID])
def test_openai_compat_oversize_and_invalid_responses(
    server: tuple[Canned, str, Callable[..., None]],
    caplog: pytest.LogCaptureFixture,
    case: str,
    reply: dict[str, Any],
    code: str,
) -> None:
    _, url, set_reply = server
    set_reply(**reply)
    x = _extractor(url, timeout=10, max_response_bytes=65_536)
    caplog.set_level(logging.DEBUG)
    with pytest.raises(openai_compat.EndpointError) as err:
        x.extract([minisets.png(1)], deadline_s=5, ocr_text=[MARKER])
    assert str(err.value) == code
    assert MARKER not in str(err.value) and err.value.__cause__ is None
    assert MARKER not in caplog.text
    assert f"code={code}" in caplog.text


def test_openai_compat_model_set_confidence_refused(
    server: tuple[Canned, str, Callable[..., None]],
) -> None:
    _, url, set_reply = server
    doc = _model_output()
    doc["patient"]["mrn"]["confidence"] = 0.99
    set_reply(body=_reply(json.dumps(doc)))
    with pytest.raises(openai_compat.EndpointError) as err:
        _extractor(url).extract([minisets.png(1)], deadline_s=5)
    assert str(err.value) == "response_schema"


def test_openai_compat_output_cap(server: tuple[Canned, str, Callable[..., None]]) -> None:
    _, url, set_reply = server
    set_reply(body=_reply(json.dumps(_model_output())))
    x = _extractor(url, max_output_bytes=100)
    with pytest.raises(openai_compat.EndpointError) as err:
        x.extract([minisets.png(1)], deadline_s=5)
    assert str(err.value) == "response_too_large"


def test_openai_compat_timeout_bounded_by_deadline(
    server: tuple[Canned, str, Callable[..., None]],
) -> None:
    _, url, set_reply = server
    set_reply(body=_reply(json.dumps(_model_output())), delay=3.0)
    x = _extractor(url, timeout=60)
    started = time.monotonic()
    with pytest.raises(openai_compat.EndpointError) as err:
        x.extract([minisets.png(1)], deadline_s=0.5)
    assert str(err.value) == "timeout"
    assert time.monotonic() - started < 2.0


def test_openai_compat_refuses_bad_pages_and_deadline() -> None:
    x = _extractor("http://127.0.0.1:1/v1")
    for pages, deadline, code in (
        ([b"GIF89a"], 5, "bad_pages"),
        ([], 5, "bad_pages"),
        ([minisets.png(1)], 0, "bad_deadline"),
        ([minisets.png(1)], float("inf"), "bad_deadline"),
    ):
        with pytest.raises(openai_compat.EndpointError) as err:
            x.extract(pages, deadline_s=deadline)
        assert str(err.value) == code


def test_openai_compat_transport_error() -> None:
    x = _extractor("http://127.0.0.1:1/v1")  # nothing listens on port 1
    with pytest.raises(openai_compat.EndpointError) as err:
        x.extract([minisets.png(1)], deadline_s=5)
    assert str(err.value) in ("transport", "timeout")


def test_openai_compat_ignores_server_model_name(
    server: tuple[Canned, str, Callable[..., None]],
) -> None:
    canned, url, set_reply = server
    set_reply(body=_reply(json.dumps(_model_output()), model="attacker-model@6.6.6#ffff"))
    x = _extractor(url)
    got = x.extract([minisets.png(2)], deadline_s=5)
    assert isinstance(got, extractors.Extracted)
    assert x.info().model_versions == {"extract": "mhai-vlm@1.0.0#0123456789ab"}
    assert "attacker" not in json.dumps(got.document)
    assert "attacker" not in repr(x.info())
    assert canned.requests[0]["model"] == "mhai-vlm"


# ---------------------------------------------------------------- mutual TLS


def _cert(
    subject: str,
    key: ec.EllipticCurvePrivateKey,
    issuer: x509.Certificate | None,
    issuer_key: ec.EllipticCurvePrivateKey,
    *,
    ca: bool = False,
    ip: str | None = None,
) -> x509.Certificate:
    now = datetime.datetime.now(datetime.UTC)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, subject)])
    b = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(issuer.subject if issuer else name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=ca, path_length=None), critical=True)
    )
    if ip:
        b = b.add_extension(
            x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address(ip))]), critical=False
        )
    return b.sign(issuer_key, hashes.SHA256())


def _write(path: Path, cert: x509.Certificate, key: ec.EllipticCurvePrivateKey | None) -> None:
    path.with_suffix(".crt").write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    if key is not None:
        path.with_suffix(".key").write_bytes(
            key.private_bytes(
                serialization.Encoding.PEM,
                serialization.PrivateFormat.PKCS8,
                serialization.NoEncryption(),
            )
        )


def test_openai_compat_mutual_tls(tmp_path: Path) -> None:
    ca_key = ec.generate_private_key(ec.SECP256R1())
    ca = _cert("fixture-ca", ca_key, None, ca_key, ca=True)
    srv_key = ec.generate_private_key(ec.SECP256R1())
    srv = _cert("fixture-server", srv_key, ca, ca_key, ip="127.0.0.1")
    cli_key = ec.generate_private_key(ec.SECP256R1())
    cli = _cert("fixture-client", cli_key, ca, ca_key)
    _write(tmp_path / "ca", ca, None)
    _write(tmp_path / "server", srv, srv_key)
    _write(tmp_path / "client", cli, cli_key)

    canned = Canned(body=_reply(json.dumps(_model_output())))
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _handler(canned))
    httpd.daemon_threads = True
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(str(tmp_path / "server.crt"), str(tmp_path / "server.key"))
    ctx.load_verify_locations(str(tmp_path / "ca.crt"))
    ctx.verify_mode = ssl.CERT_REQUIRED
    httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    url = f"https://127.0.0.1:{httpd.server_address[1]}/v1"
    try:
        x = _extractor(
            url,
            ca=tmp_path / "ca.crt",
            client_cert=tmp_path / "client.crt",
            client_key=tmp_path / "client.key",
        )
        got = x.extract([minisets.png(1)], deadline_s=5)
        assert isinstance(got, extractors.Extracted)
        assert len(canned.requests) == 1

        no_client_cert = _extractor(url, ca=tmp_path / "ca.crt")
        with pytest.raises(openai_compat.EndpointError) as err:
            no_client_cert.extract([minisets.png(1)], deadline_s=5)
        assert str(err.value) in ("transport", "timeout")

        untrusted_server = _extractor(url)  # system CAs: the fixture CA is not one
        with pytest.raises(openai_compat.EndpointError) as err:
            untrusted_server.extract([minisets.png(1)], deadline_s=5)
        assert str(err.value) == "transport"
        assert len(canned.requests) == 1
    finally:
        httpd.shutdown()
        httpd.server_close()
