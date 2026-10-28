"""The worker runtime against a live NATS 2.10 running the real nats.conf
(spec §4, §7.3, §9.1, §17.2-17.3). Marked `boundary`: needs Docker.

Core, the BFFs and core's grant server are played by plain nats-py clients
connected as their own NATS users; core's tokens come from the test-only
signer (tests/boundary/tokens.py). Every id, key, token and password here is
synthetic.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import subprocess
import sys
import time
import uuid
from collections.abc import AsyncIterator, Awaitable, Callable
from pathlib import Path
from typing import Any

import nats
import pytest
from google.protobuf.message import Message
from nats.aio.client import Client
from nats.aio.msg import Msg

from intelligence.boundary import envelope
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import authz_pb2, echo_pb2, envelope_pb2, pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import echo, pipeline
from intelligence.boundary.verify import Verified, Verifier
from intelligence.boundary.worker import Handler, Worker
from tests.boundary.conftest import NatsServer, Postgres, now_s
from tests.boundary.tokens import (
    HEALTH_SCHEMA,
    HOSPITAL,
    PING_SCHEMA,
    RID,
    Signer,
)

pytestmark = pytest.mark.boundary

PING = "inference.v1.echo.ping"
HEALTH = "inference.v1.pipeline.health"
CORE_INBOX = "_INBOX_gateway"
BFF_INBOX = "_INBOX_bff.clinical"
ACCESS = "synthetic-access-token-not-a-jwt-0001"


def ping_body(text: str = "synthetic ping") -> bytes:
    return echo_pb2.PingRequest(text=text, hospital_id=HOSPITAL).SerializeToString(
        deterministic=True
    )


def core_headers(token: str, schema: str = PING_SCHEMA, rid: str = RID) -> dict[str, str]:
    return {envelope.TOKEN: token, envelope.SCHEMA: schema, envelope.REQUEST_ID: rid}


def core_ping(
    signer: Signer, text: str = "synthetic ping", **kw: Any
) -> tuple[bytes, dict[str, str]]:
    body = ping_body(text)
    kw.setdefault("ttl", 5)
    tok = signer.mint(body=body, jti=uuid.uuid4().hex, iat=now_s(), **kw)
    return body, core_headers(tok)


def bff_headers(access: str = ACCESS, schema: str = PING_SCHEMA) -> dict[str, str]:
    return {
        envelope.AUTHORIZATION: f"Bearer {access}",
        envelope.SCHEMA: schema,
        envelope.REQUEST_ID: str(uuid.uuid4()),
    }


def status(m: Msg) -> str:
    assert m.headers is not None
    return m.headers[envelope.STATUS]


async def connect(server: NatsServer, user: str, inbox: str) -> Client:
    return await nats.connect(
        server.url(user), inbox_prefix=inbox, allow_reconnect=False, connect_timeout=5
    )


async def eventually(pred: Callable[[], bool], timeout: float = 3.0) -> bool:
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if pred():
            return True
        await asyncio.sleep(0.02)
    return pred()


class Counting:
    """Wraps a handler and counts the calls that reached it."""

    def __init__(self, inner: Handler) -> None:
        self.inner = inner
        self.calls = 0
        self.seen: list[Verified] = []

    async def __call__(self, v: Verified, m: Message) -> tuple[envelope_pb2.ResponseMeta, Message]:
        self.calls += 1
        self.seen.append(v)
        return await self.inner(v, m)


def blocking(release: asyncio.Event, started: list[str] | None = None) -> Handler:
    async def h(v: Verified, m: Message) -> tuple[envelope_pb2.ResponseMeta, Message]:
        if started is not None:
            started.append(v.jti)
        await release.wait()
        return await echo.ping(v, m)

    return h


def spy(w: Worker) -> list[str]:
    """Every subject the worker's connection publishes to."""
    sent: list[str] = []
    nc = w.connection
    real = nc.publish

    async def rec(subject: str, payload: bytes = b"", reply: str = "", headers: Any = None) -> None:
        sent.append(subject)
        await real(subject, payload, reply=reply, headers=headers)

    nc.publish = rec  # type: ignore[method-assign]
    return sent


@pytest.fixture
def keys_file(tmp_path: Path, signer: Signer) -> Path:
    import json

    p = tmp_path / "trusted.json"
    p.write_text(json.dumps({"keys": signer.encoded_keys()}))
    return p


StartWorker = Callable[..., Awaitable[Worker]]


@pytest.fixture
async def start_worker(
    nats_server: NatsServer, signer: Signer, keys_file: Path
) -> AsyncIterator[StartWorker]:
    started: list[Worker] = []
    ops = Operations.load(development=True)

    async def start(
        service: str = "echo",
        handlers: dict[str, Handler] | None = None,
        max_concurrency: int = 4,
    ) -> Worker:
        settings = WorkerSettings(
            service=service,
            nats_url=nats_server.url(service),
            trusted_keys_path=str(keys_file),
            max_concurrency=max_concurrency,
        )
        if handlers is None:
            handlers = dict(echo.HANDLERS if service == "echo" else pipeline.handlers())
        verifier = Verifier(
            TrustedKeys.from_encoded(signer.encoded_keys()),
            service,
            ops,
            JtiSet(time.time),
            time.time,
        )
        w = Worker(settings, handlers, verifier)
        await w.start()
        started.append(w)
        return w

    yield start
    for w in started:
        await w.close()


@pytest.fixture
async def core(nats_server: NatsServer) -> AsyncIterator[Client]:
    nc = await connect(nats_server, "gateway", CORE_INBOX)
    yield nc
    await nc.close()


@pytest.fixture
async def bff(nats_server: NatsServer) -> AsyncIterator[Client]:
    nc = await connect(nats_server, "bff_clinical", BFF_INBOX)
    yield nc
    await nc.close()


class GrantStub:
    """Core's grant server, played by a gateway connection: grants with the
    test-only signer, refuses with a code, or answers late."""

    def __init__(self, signer: Signer, *, refuse: str = "", delay: float = 0.0) -> None:
        self.signer = signer
        self.refuse = refuse
        self.delay = delay
        self.requests: list[authz_pb2.GrantRequest] = []
        self.tokens: list[str] = []
        self.jtis: list[str] = []
        self.nc: Client | None = None

    async def start(self, server: NatsServer, service: str = "echo") -> GrantStub:
        self.nc = await connect(server, "gateway", CORE_INBOX)
        await self.nc.subscribe(f"authz.v1.grant.{service}", queue="core", cb=self._on)
        await self.nc.flush()
        self.service = service
        return self

    async def _on(self, msg: Msg) -> None:
        assert self.nc is not None
        req = authz_pb2.GrantRequest()
        req.ParseFromString(msg.data)
        self.requests.append(req)
        if self.delay:
            await asyncio.sleep(self.delay)
        if self.refuse:
            h = {envelope.STATUS: self.refuse, envelope.SCHEMA: "mhai.intelligence.v1.GrantRefused"}
            body = authz_pb2.GrantRefused(code=self.refuse).SerializeToString()
        else:
            jti = uuid.uuid4().hex
            tok = self.signer.mint_grant(
                b"",
                aud=self.service,
                nsub=req.subject,
                rid=req.request_id,
                jti=jti,
                iat=now_s(),
                ttl=2,
                edit=lambda p: p.__setitem__("bh", req.body_hash),
            )
            self.tokens.append(tok)
            self.jtis.append(jti)
            h = {envelope.STATUS: "ok", envelope.SCHEMA: "mhai.intelligence.v1.GrantResponse"}
            body = authz_pb2.GrantResponse(request_token=tok).SerializeToString()
        await self.nc.publish(msg.reply, body, headers=h)

    async def close(self) -> None:
        if self.nc is not None:
            await self.nc.close()


@pytest.fixture
async def grant_stub(
    nats_server: NatsServer, signer: Signer
) -> AsyncIterator[Callable[..., Awaitable[GrantStub]]]:
    stubs: list[GrantStub] = []

    async def make(service: str = "echo", **kw: Any) -> GrantStub:
        s = await GrantStub(signer, **kw).start(nats_server, service)
        stubs.append(s)
        return s

    yield make
    for s in stubs:
        await s.close()


# --- core's path --------------------------------------------------------------


async def test_echo_ping_ok(start_worker: StartWorker, core: Client, signer: Signer) -> None:
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    body, hdrs = core_ping(signer, "synthetic hello")
    r = await core.request(PING, body, timeout=2, headers=hdrs)
    assert status(r) == "ok"
    assert r.headers is not None
    assert r.headers[envelope.SCHEMA] == "mhai.intelligence.v1.PingResponse"
    assert r.headers[envelope.REQUEST_ID] == RID
    # The reply never carries the request's credentials back.
    assert envelope.TOKEN not in r.headers and envelope.AUTHORIZATION not in r.headers
    out = echo_pb2.PingResponse()
    out.ParseFromString(r.data)
    assert out.text == "synthetic hello"
    assert [f.action for f in out.meta.audit_facts] == ["intelligence.echo.pinged"]
    assert h.calls == 1


async def test_echo_without_token_gets_unauthorized_and_handler_never_runs(
    start_worker: StartWorker, core: Client
) -> None:
    """A1."""
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    r = await core.request(
        PING,
        ping_body(),
        timeout=2,
        headers={envelope.SCHEMA: PING_SCHEMA, envelope.REQUEST_ID: RID},
    )
    assert status(r) == "unauthorized"
    assert r.headers is not None and r.headers[envelope.SCHEMA] == envelope.ERROR_SCHEMA
    assert h.calls == 0


async def test_replayed_request_refused(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    """A3: one valid message sent twice to a single echo worker."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    body, hdrs = core_ping(signer)
    assert status(await core.request(PING, body, timeout=2, headers=hdrs)) == "ok"
    assert status(await core.request(PING, body, timeout=2, headers=hdrs)) == "unauthorized"
    assert h.calls == 1
    assert w.verifier.refusals[("echo", "replay")] == 1


async def test_reply_to_foreign_inbox_gets_no_reply(
    start_worker: StartWorker, core: Client, signer: Signer, nats_server: NatsServer
) -> None:
    """A15: a valid token, published by gateway with reply _INBOX_worker.x."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    sent = spy(w)
    watcher = await connect(nats_server, "echo", "_INBOX_worker.echo")
    seen: list[Msg] = []

    async def got(m: Msg) -> None:
        seen.append(m)

    await watcher.subscribe("_INBOX_worker.echo.>", cb=got)
    await watcher.flush()
    try:
        body, hdrs = core_ping(signer)
        await core.publish(PING, body, reply="_INBOX_worker.x", headers=hdrs)
        await core.flush()
        assert await eventually(lambda: w.verifier.refusals[("echo", "reply_subject")] == 1)
        await asyncio.sleep(0.2)
        assert sent == [] and seen == [] and h.calls == 0
    finally:
        await watcher.close()


FORGED_REPLIES = [
    f"jobs.v1.{uuid.UUID(int=7)}.run_query",
    "$KV.denylist.x",
    "_INBOX_bff.platform.x",
    "_INBOX_gateway.*",
    "_INBOX_gateway.>",
    "_INBOX_gateway.a.>",
    "_INBOX_bff.clinical.*",
]


@pytest.mark.parametrize("reply", FORGED_REPLIES)
@pytest.mark.parametrize("path", ["token", "none", "authorization"])
async def test_forged_reply_subject_gets_no_reply_and_no_side_effect(
    start_worker: StartWorker,
    core: Client,
    signer: Signer,
    nats_server: NatsServer,
    grant_stub: Callable[..., Awaitable[GrantStub]],
    reply: str,
    path: str,
) -> None:
    """The responder reply-subject check (Task 4 ruling): the worker answers
    no reply subject the verifier hasn't passed, and before check 5 only a
    literal inbox under _INBOX_gateway. (or _INBOX_bff.clinical. on the grant
    path). Nothing else is published at all: no reply, no grant request."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    sent = spy(w)
    stub = await grant_stub()
    platform = await connect(nats_server, "bff_platform", "_INBOX_bff.platform")
    seen: list[Msg] = []

    async def got(m: Msg) -> None:
        seen.append(m)

    await platform.subscribe("_INBOX_bff.platform.>", cb=got)
    await platform.flush()
    try:
        body = ping_body()
        if path == "token":
            body, hdrs = core_ping(signer)
        elif path == "none":
            hdrs = {envelope.SCHEMA: PING_SCHEMA, envelope.REQUEST_ID: RID}
        else:
            hdrs = bff_headers()
        before = w.received
        await core.publish(PING, body, reply=reply, headers=hdrs)
        await core.flush()
        arrived = await eventually(lambda: w.received > before, timeout=1.5)
        if "*" not in reply and ">" not in reply:
            assert arrived  # the server delivered it; the worker dropped it
        await asyncio.sleep(0.2)
        assert sent == [], sent
        assert seen == [] and h.calls == 0 and stub.requests == []
    finally:
        await platform.close()


async def test_oversized_request_body_refused(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    """A18 (worker side): a 300 KB body is refused by the verifier's size check."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    body = echo_pb2.PingRequest(text="x" * (300 << 10), hospital_id=HOSPITAL).SerializeToString()
    tok = signer.mint(body=body, jti=uuid.uuid4().hex, iat=now_s())
    r = await core.request(PING, body, timeout=2, headers=core_headers(tok))
    assert status(r) == "unauthorized"
    assert w.verifier.refusals[("echo", "size")] == 1
    assert h.calls == 0


async def test_repeated_credential_headers_refused(
    start_worker: StartWorker, nats_server: NatsServer, signer: Signer
) -> None:
    """T8 Minor 5: two Mhai-Request-Token (or Mhai-Schema, Mhai-Request-Id,
    Authorization) headers are refused, whichever one nats-py would keep.
    Sent over a raw connection, since nats-py's publish takes a dict."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    body, hdrs = core_ping(signer)
    good = hdrs[envelope.TOKEN]
    other = signer.mint(body=body, jti=uuid.uuid4().hex, iat=now_s())
    # (headers, answered): core's inbox is answered; an access-token message
    # is on the grant path, whose only inbox is the clinical BFF's, so a
    # gateway inbox gets nothing.
    cases = [
        ([(envelope.TOKEN, good), (envelope.TOKEN, other)], True),
        ([(envelope.TOKEN, good), ("mhai-request-token", good)], True),
        ([(envelope.SCHEMA, PING_SCHEMA), (envelope.SCHEMA, PING_SCHEMA)], True),
        ([(envelope.REQUEST_ID, RID), (envelope.REQUEST_ID, RID)], True),
        ([(envelope.TOKEN, good), (envelope.AUTHORIZATION, f"Bearer {ACCESS}")], True),
        (
            [(envelope.AUTHORIZATION, f"Bearer {ACCESS}"), (envelope.AUTHORIZATION, "Bearer x")],
            False,
        ),
    ]
    for i, (extra, answered) in enumerate(cases):
        names = {k2.lower() for k2, _ in extra}
        base = [(k, v) for k, v in hdrs.items() if k.lower() not in names]
        if not answered:
            base = [(k, v) for k, v in base if k != envelope.TOKEN]
        reply = await raw_request(nats_server, PING, base + extra, body, f"_INBOX_gateway.raw.{i}")
        if answered:
            assert reply is not None, i
            assert b"Mhai-Status: unauthorized" in reply, (i, reply[:200])
        else:
            assert reply is None, i
    assert h.calls == 0
    assert w.refusals["ambiguous_headers"] == len(cases)


async def raw_request(
    server: NatsServer, subject: str, headers: list[tuple[str, str]], body: bytes, reply: str
) -> bytes | None:
    """HPUB with headers exactly as given (repeats allowed), as gateway; the
    raw reply block, or None."""
    import json

    from tests.boundary.conftest import NATS_ENV

    r, wr = await asyncio.open_connection(server.host, server.port)
    try:
        await r.readline()  # INFO
        conn = {
            "user": "gateway",
            "pass": NATS_ENV["NATS_GATEWAY_PASSWORD"],
            "headers": True,
            "verbose": False,
            "pedantic": False,
            "protocol": 1,
        }
        hdr = b"NATS/1.0\r\n" + b"".join(f"{k}: {v}\r\n".encode() for k, v in headers) + b"\r\n"
        wr.write(f"CONNECT {json.dumps(conn)}\r\nSUB {reply} 1\r\nPING\r\n".encode())
        await wr.drain()
        line = await asyncio.wait_for(r.readline(), 5)
        assert line.startswith(b"PONG"), line
        wr.write(
            f"HPUB {subject} {reply} {len(hdr)} {len(hdr) + len(body)}\r\n".encode()
            + hdr
            + body
            + b"\r\n"
        )
        await wr.drain()
        try:
            line = await asyncio.wait_for(r.readline(), 2)
        except TimeoutError:
            return None
        if not line.startswith(b"HMSG"):
            return None
        total = int(line.split()[-1])
        return await r.readexactly(total + 2)
    finally:
        wr.close()


async def test_verifier_raising_fails_closed(
    start_worker: StartWorker,
    core: Client,
    signer: Signer,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """T8 deferred: if Verifier._run itself raises, no reply and no work, logged."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    sent = spy(w)

    def boom(*_a: Any, **_k: Any) -> Any:
        raise RuntimeError("synthetic secret detail")

    monkeypatch.setattr(w.verifier, "_run", boom)
    body, hdrs = core_ping(signer)
    with caplog.at_level(logging.INFO, logger="intelligence.boundary"):
        await core.publish(PING, body, reply="_INBOX_gateway.t.1", headers=hdrs)
        await core.flush()
        assert await eventually(lambda: w.refusals["internal"] == 1)
    await asyncio.sleep(0.1)
    assert sent == [] and h.calls == 0
    text = caplog.text
    assert "RuntimeError" in text and "synthetic secret detail" not in text


# --- limits --------------------------------------------------------------------


async def test_overloaded_when_full(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    release = asyncio.Event()
    started: list[str] = []
    await start_worker(handlers={"ping": blocking(release, started)}, max_concurrency=1)
    b1, h1 = core_ping(signer)
    first = asyncio.ensure_future(core.request(PING, b1, timeout=5, headers=h1))
    assert await eventually(lambda: len(started) == 1)
    b2, h2 = core_ping(signer)
    t0 = time.monotonic()
    r2 = await core.request(PING, b2, timeout=2, headers=h2)
    assert status(r2) == "overloaded"
    assert time.monotonic() - t0 < 0.5  # at once, not queued
    release.set()
    assert status(await first) == "ok"
    assert len(started) == 1


async def test_deadline_stops_handler(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    cancelled: list[bool] = []

    async def slow(v: Verified, m: Message) -> tuple[envelope_pb2.ResponseMeta, Message]:
        try:
            await asyncio.sleep(30)
        except asyncio.CancelledError:
            cancelled.append(True)
            raise
        return await echo.ping(v, m)

    await start_worker(handlers={"ping": slow})
    body, hdrs = core_ping(signer, ttl=1)
    t0 = time.monotonic()
    r = await core.request(PING, body, timeout=5, headers=hdrs)
    assert status(r) == "deadline_exceeded"
    assert time.monotonic() - t0 < 2.5
    assert cancelled == [True]


async def test_sigterm_drains(start_worker: StartWorker, core: Client, signer: Signer) -> None:
    """F26: on SIGTERM the in-flight handler still replies, no new message is
    taken, and run() returns normally."""
    release = asyncio.Event()
    started: list[str] = []
    w = await start_worker(handlers={"ping": blocking(release, started)})
    runner = asyncio.ensure_future(w.run())
    await asyncio.sleep(0.05)
    b1, h1 = core_ping(signer)
    first = asyncio.ensure_future(core.request(PING, b1, timeout=5, headers=h1))
    assert await eventually(lambda: len(started) == 1)
    os.kill(os.getpid(), signal.SIGTERM)
    assert await eventually(lambda: w.draining)
    await asyncio.sleep(0.2)
    b2, h2 = core_ping(signer)
    with pytest.raises((nats.errors.NoRespondersError, nats.errors.TimeoutError)):
        await core.request(PING, b2, timeout=0.5, headers=h2)
    release.set()
    assert status(await first) == "ok"
    await asyncio.wait_for(runner, 5)
    assert runner.exception() is None
    assert len(started) == 1


async def test_cli_worker_exits_zero_on_sigterm(
    nats_server: NatsServer, core: Client, signer: Signer, keys_file: Path
) -> None:
    """The real entry point: `intelligence worker --service echo` answers a
    ping, then drains and exits 0 on SIGTERM."""
    env = {
        **{k: v for k, v in os.environ.items() if not k.startswith(("NATS_", "INTELLIGENCE_"))},
        "APP_ENV": "development",
        "NATS_URL": nats_server.url("echo"),
        "INTELLIGENCE_TRUSTED_KEYS_PATH": str(keys_file),
    }
    proc = subprocess.Popen(
        [sys.executable, "-m", "intelligence", "worker", "--service", "echo"],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    try:
        ok = False
        for _ in range(100):
            body, hdrs = core_ping(signer)
            try:
                r = await core.request(PING, body, timeout=0.5, headers=hdrs)
            except (nats.errors.NoRespondersError, nats.errors.TimeoutError):
                await asyncio.sleep(0.2)
                continue
            ok = status(r) == "ok"
            break
        assert ok, "the CLI worker never answered"
        proc.send_signal(signal.SIGTERM)
        code = await asyncio.get_running_loop().run_in_executor(None, proc.wait, 20)
        out = proc.stdout.read().decode() if proc.stdout else ""
        assert code == 0, out
        assert "test-only-echo-pw" not in out  # the URL's password is never logged
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()


async def test_health_ok(start_worker: StartWorker, core: Client, signer: Signer) -> None:
    await start_worker(service="pipeline")
    body = pipeline_pb2.HealthRequest().SerializeToString()
    tok = signer.mint(
        body=body,
        aud="pipeline",
        sub="svc:core",
        roles=[],
        nsub=HEALTH,
        jti=uuid.uuid4().hex,
        iat=now_s(),
    )
    r = await core.request(HEALTH, body, timeout=2, headers=core_headers(tok, HEALTH_SCHEMA))
    assert status(r) == "ok"
    out = pipeline_pb2.HealthResponse()
    out.ParseFromString(r.data)
    # F28: no database until T10, and no models here: both fail closed.
    assert (out.models_ready, out.database_ready) == (False, False)


# --- load balancing -------------------------------------------------------------


async def test_two_worker_copies_share_requests(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    a, b = Counting(echo.ping), Counting(echo.ping)
    # A cap above the burst: this test is about sharing, not overload.
    await start_worker(handlers={"ping": a}, max_concurrency=128)
    await start_worker(handlers={"ping": b}, max_concurrency=128)
    calls = [core_ping(signer, f"synthetic {i}") for i in range(100)]
    replies = await asyncio.gather(
        *(core.request(PING, body, timeout=5, headers=h) for body, h in calls)
    )
    assert all(status(r) == "ok" for r in replies)
    jtis = [v.jti for v in a.seen + b.seen]
    assert len(jtis) == 100 and len(set(jtis)) == 100  # each answered exactly once
    assert a.calls > 0 and b.calls > 0  # each copy answered some


async def test_killed_copy_loses_nothing_after_retry(
    start_worker: StartWorker, core: Client, signer: Signer
) -> None:
    """A copy killed mid-run: the calls it held time out at core, and the
    same calls retried with fresh tokens are answered by the other copy."""
    never = asyncio.Event()
    held: list[str] = []
    dying = await start_worker(handlers={"ping": blocking(never, held)}, max_concurrency=64)
    alive = Counting(echo.ping)
    await start_worker(handlers={"ping": alive}, max_concurrency=64)
    texts = [f"synthetic {i}" for i in range(40)]

    async def call(text: str, timeout: float) -> str | None:
        body, h = core_ping(signer, text)
        try:
            r = await core.request(PING, body, timeout=timeout, headers=h)
        except nats.errors.TimeoutError:
            return None
        out = echo_pb2.PingResponse()
        out.ParseFromString(r.data)
        return out.text if status(r) == "ok" else None

    pending = [asyncio.ensure_future(call(t, 1.5)) for t in texts]
    assert await eventually(lambda: len(held) > 0)
    await dying.abort()  # killed: no drain, nothing answered
    first = await asyncio.gather(*pending)
    lost = [t for t, got in zip(texts, first, strict=True) if got is None]
    assert lost, "the dying copy held some calls"
    retried = await asyncio.gather(*(call(t, 3) for t in lost))
    assert retried == lost  # every retry answered, by the other copy
    answered = [t for t in first if t is not None] + [t for t in retried if t is not None]
    assert sorted(answered) == sorted(texts)  # none lost, none twice
    assert alive.calls == len(texts)


# --- BFF-direct calls (spec §17.2) ----------------------------------------------


async def test_bff_direct_call_granted(
    start_worker: StartWorker,
    bff: Client,
    grant_stub: Callable[..., Awaitable[GrantStub]],
    caplog: pytest.LogCaptureFixture,
) -> None:
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    stub = await grant_stub()
    body = ping_body("synthetic bff hello")
    hdrs = bff_headers()
    with caplog.at_level(logging.DEBUG):
        r = await bff.request(PING, body, timeout=3, headers=hdrs)
    assert status(r) == "ok"
    assert r.headers is not None and envelope.AUTHORIZATION not in r.headers
    [req] = stub.requests
    assert req.access_token == ACCESS  # sent only here
    assert (req.operation, req.subject) == ("echo.ping", PING)
    assert req.reply_prefix == "_INBOX_bff.clinical."
    assert req.request_id == hdrs[envelope.REQUEST_ID]
    assert 0 < req.deadline_ms <= 5000 and list(req.patient_ids) == []
    [v] = h.seen
    assert (v.via, v.rid, v.jti) == ("grant", req.request_id, stub.jtis[0])
    # Neither the access token nor the granted token is ever logged.
    assert ACCESS not in caplog.text and stub.tokens[0] not in caplog.text


async def test_core_down_means_unavailable(start_worker: StartWorker, bff: Client) -> None:
    """A32: nothing on authz.v1.grant.echo."""
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    t0 = time.monotonic()
    r = await bff.request(PING, ping_body(), timeout=3, headers=bff_headers())
    assert status(r) == "unavailable"
    assert time.monotonic() - t0 < 1.0
    assert h.calls == 0


async def test_slow_grant_refused_and_late_reply_ignored(
    start_worker: StartWorker,
    bff: Client,
    grant_stub: Callable[..., Awaitable[GrantStub]],
) -> None:
    """A33: the grant arrives after the 250 ms budget."""
    h = Counting(echo.ping)
    w = await start_worker(handlers={"ping": h})
    stub = await grant_stub(delay=0.6)
    t0 = time.monotonic()
    r = await bff.request(PING, ping_body(), timeout=3, headers=bff_headers())
    assert status(r) == "unavailable"
    assert time.monotonic() - t0 < 0.55
    assert await eventually(lambda: len(stub.tokens) == 1)  # core did answer, late
    await asyncio.sleep(0.3)
    assert h.calls == 0
    assert w.verifier.refusals == {}  # the late token was never even checked


async def test_grant_refusal_passed_through(
    start_worker: StartWorker,
    bff: Client,
    grant_stub: Callable[..., Awaitable[GrantStub]],
) -> None:
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    await grant_stub(refuse="mfa_required")
    r = await bff.request(PING, ping_body(), timeout=3, headers=bff_headers())
    assert status(r) == "grant_refused:mfa_required"
    assert h.calls == 0


async def test_grant_not_ready_passed_through(
    start_worker: StartWorker,
    bff: Client,
    grant_stub: Callable[..., Awaitable[GrantStub]],
) -> None:
    """F21."""
    await start_worker()
    await grant_stub(refuse="not_ready")
    r = await bff.request(PING, ping_body(), timeout=3, headers=bff_headers())
    assert status(r) == "grant_refused:not_ready"


async def test_not_bff_direct_operation_refused(
    start_worker: StartWorker,
    core: Client,
    nats_server: NatsServer,
    grant_stub: Callable[..., Awaitable[GrantStub]],
) -> None:
    """An access token on pipeline.health: unauthorized, and no grant asked.
    The BFF user may not publish there (nats.conf), so gateway publishes it
    with a clinical-BFF reply subject that a bff_clinical client reads."""
    h = Counting(pipeline.health_handler(lambda: True))
    await start_worker(service="pipeline", handlers={"health": h})
    stub = await grant_stub(service="pipeline")
    reader = await connect(nats_server, "bff_clinical", BFF_INBOX)
    got: asyncio.Future[Msg] = asyncio.get_running_loop().create_future()

    async def on(m: Msg) -> None:
        if not got.done():
            got.set_result(m)

    await reader.subscribe("_INBOX_bff.clinical.t.1", cb=on)
    await reader.flush()
    try:
        await core.publish(
            HEALTH,
            pipeline_pb2.HealthRequest().SerializeToString(),
            reply="_INBOX_bff.clinical.t.1",
            headers=bff_headers(schema=HEALTH_SCHEMA),
        )
        r = await asyncio.wait_for(got, 3)
        assert status(r) == "unauthorized"
        assert h.calls == 0 and stub.requests == []
    finally:
        await reader.close()


async def test_bff_cannot_present_its_own_request_token(
    start_worker: StartWorker, bff: Client, signer: Signer
) -> None:
    """A token in the message is accepted only from core (rpl _INBOX_gateway.):
    one minted for the BFF's inbox is refused."""
    h = Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    body = ping_body()
    tok = signer.mint_grant(body, jti=uuid.uuid4().hex, iat=now_s())
    hdrs = {envelope.TOKEN: tok, envelope.SCHEMA: PING_SCHEMA, envelope.REQUEST_ID: RID}
    with pytest.raises(nats.errors.TimeoutError):
        # Check 5 fails: no reply at all.
        await bff.request(PING, body, timeout=0.5, headers=hdrs)
    assert h.calls == 0


# --- fixtures -------------------------------------------------------------------


def test_postgres_fixture_is_migrated_by_core(postgres: Postgres) -> None:
    """The postgres fixture runs core's own `migrate up`: the schema is the
    one production uses (T10's tests build on it)."""
    assert postgres.psql("SELECT to_regclass('public.hospitals') IS NOT NULL") == "t"
    assert postgres.psql("SELECT count(*) FROM pg_roles WHERE rolname = 'mhai_jobs'") == "1"
