"""The worker's own rules, without Docker: repeated headers, the reply
subject's form, failing closed, deadlines, the grant client's budget and
replies, the settings, and the two services. A fake connection records
every publish. Every id and token here is synthetic.
"""

from __future__ import annotations

import asyncio
import logging
import time
import uuid
from collections.abc import Callable
from typing import Any

import nats.errors
import pytest
from google.protobuf.message import Message
from nats.aio.msg import Msg

from intelligence.boundary import envelope, grant
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.grant import GrantClient, GrantRefused, Unavailable
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import authz_pb2, echo_pb2, envelope_pb2, pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import echo, pipeline
from intelligence.boundary.verify import Verified, Verifier
from intelligence.boundary.worker import Worker, WorkerClient, literal_reply
from tests.boundary.tokens import HOSPITAL, PING_SCHEMA, RID, Signer

PING = "inference.v1.echo.ping"
ACCESS = "synthetic-access-token-not-a-jwt-0002"


class Clock:
    def __init__(self) -> None:
        self.offset = 0.0

    def __call__(self) -> float:
        return time.time() + self.offset


class FakeSub:
    async def drain(self) -> None:
        return None

    async def unsubscribe(self) -> None:
        return None


class FakeNC:
    """Records publishes; request() is set per test."""

    def __init__(self) -> None:
        self.published: list[tuple[str, bytes, dict[str, str] | None]] = []
        self.requests: list[tuple[str, bytes, float]] = []
        self.answer: Callable[[str, bytes, float], Any] | None = None
        self.is_closed = False

    async def publish(
        self, subject: str, payload: bytes = b"", reply: str = "", headers: Any = None
    ) -> None:
        self.published.append((subject, payload, headers))

    async def subscribe(self, subject: str, queue: str = "", cb: Any = None, **_k: Any) -> FakeSub:
        self.subscribed = (subject, queue)
        return FakeSub()

    async def request(
        self, subject: str, payload: bytes = b"", timeout: float = 0.5, headers: Any = None
    ) -> Any:
        self.requests.append((subject, payload, timeout))
        assert self.answer is not None
        return await self.answer(subject, payload, timeout)

    async def flush(self) -> None:
        return None

    async def drain(self) -> None:
        self.is_closed = True

    async def close(self) -> None:
        self.is_closed = True


def run(coro: Any) -> Any:
    return asyncio.run(coro)


async def headers_of(pairs: list[tuple[str, str]]) -> Any:
    raw = b"NATS/1.0\r\n" + b"".join(f"{k}: {v}\r\n".encode() for k, v in pairs) + b"\r\n"
    return await WorkerClient()._process_headers(raw)


def ping_body(text: str = "synthetic") -> bytes:
    return echo_pb2.PingRequest(text=text, hospital_id=HOSPITAL).SerializeToString()


def make_worker(
    handlers: dict[str, Any] | None = None, *, clock: Clock | None = None, service: str = "echo"
) -> tuple[Worker, FakeNC, Signer]:
    signer = Signer()
    clk = clock or Clock()
    ops = Operations.load(development=True)
    v = Verifier(TrustedKeys.from_encoded(signer.encoded_keys()), service, ops, JtiSet(clk), clk)
    settings = WorkerSettings(
        service=service,
        nats_url="nats://127.0.0.1:4222",
        trusted_keys_path="/nonexistent",
        development=True,
    )
    w = Worker(settings, handlers if handlers is not None else dict(echo.HANDLERS), v)
    return w, FakeNC(), signer


async def started(w: Worker, nc: FakeNC) -> Worker:
    await w.start(nc=nc)  # type: ignore[arg-type]
    return w


def msg(subject: str, reply: str, data: bytes, headers: Any) -> Msg:
    return Msg(_client=None, subject=subject, reply=reply, data=data, headers=headers)  # type: ignore[arg-type]


def core_pairs(signer: Signer, body: bytes, **kw: Any) -> list[tuple[str, str]]:
    tok = signer.mint(body=body, jti=uuid.uuid4().hex, iat=int(time.time()), **kw)
    return [(envelope.TOKEN, tok), (envelope.SCHEMA, PING_SCHEMA), (envelope.REQUEST_ID, RID)]


# --- headers --------------------------------------------------------------------


@pytest.mark.parametrize(
    "pairs",
    [
        [(envelope.TOKEN, "a"), (envelope.TOKEN, "b")],
        [(envelope.TOKEN, "a"), ("mhai-request-token", "a")],
        [("MHAI-REQUEST-TOKEN", "a")],
        [(" Mhai-Request-Token", "a"), (envelope.TOKEN, "a")],
        [(envelope.SCHEMA, "x"), (envelope.SCHEMA, "x")],
        [(envelope.REQUEST_ID, "x"), (envelope.REQUEST_ID, "y")],
        [(envelope.AUTHORIZATION, "Bearer a"), (envelope.AUTHORIZATION, "Bearer b")],
        [("authorization", "Bearer a")],
        [(envelope.TIMEOUT, "100"), (envelope.TIMEOUT, "200")],
    ],
)
def test_repeated_or_case_folded_credential_headers_are_ambiguous(
    pairs: list[tuple[str, str]],
) -> None:
    h = run(headers_of(pairs))
    assert h is not None and h.ambiguous


def test_plain_headers_are_not_ambiguous() -> None:
    h = run(
        headers_of(
            [
                (envelope.TOKEN, "a"),
                (envelope.SCHEMA, "s"),
                ("traceparent", "1"),
                ("X", "1"),
                ("X", "2"),
            ]
        )
    )
    assert h is not None and not h.ambiguous
    assert h[envelope.TOKEN] == "a"
    assert run(WorkerClient()._process_headers(b"")) is None


@pytest.mark.parametrize(
    "reply,ok",
    [
        ("_INBOX_gateway.abc.1", True),
        ("_INBOX_gateway.*", False),
        ("_INBOX_gateway.>", False),
        ("_INBOX_gateway.a.>", False),
        ("_INBOX_gateway..x", False),
        ("_INBOX_gateway.a b", False),
        ("_INBOX_gateway.a\tb", False),
        ("_INBOX_gateway.é", False),
        ("", False),
        ("x" * 257, False),
        ("_INBOX_gateway.", False),
    ],
)
def test_literal_reply(reply: str, ok: bool) -> None:
    assert literal_reply(reply) is ok


# --- dispatch -------------------------------------------------------------------


def test_reply_headers_never_echo_the_request() -> None:
    async def go() -> None:
        w, nc, signer = make_worker()
        await started(w, nc)
        body = ping_body()
        await w.dispatch(
            msg(PING, "_INBOX_gateway.t.1", body, await headers_of(core_pairs(signer, body)))
        )
        await w.close()
        [(subject, _, h)] = nc.published
        assert subject == "_INBOX_gateway.t.1"
        assert h is not None and h[envelope.STATUS] == "ok"
        assert envelope.TOKEN not in h and envelope.AUTHORIZATION not in h

    run(go())


def test_plain_dict_headers_fail_closed() -> None:
    """Headers that didn't come through WorkerClient's parser (so repeats
    weren't looked for) are refused with no reply."""

    async def go() -> None:
        w, nc, signer = make_worker()
        await started(w, nc)
        body = ping_body()
        await w.dispatch(msg(PING, "_INBOX_gateway.t.1", body, dict(core_pairs(signer, body))))
        assert nc.published == [] and w.refusals["headers"] == 1
        await w.close()

    run(go())


@pytest.mark.parametrize(
    "reply", ["_INBOX_gateway.*", "_INBOX_gateway.>", "_INBOX_gateway.a.>", ""]
)
def test_wildcard_or_missing_reply_dropped(reply: str) -> None:
    async def go() -> None:
        calls: list[int] = []

        async def h(v: Verified, m: Message) -> Any:
            calls.append(1)
            return await echo.ping(v, m)

        w, nc, signer = make_worker({"ping": h})
        await started(w, nc)
        body = ping_body()
        await w.dispatch(msg(PING, reply, body, await headers_of(core_pairs(signer, body))))
        await w.close()
        assert nc.published == [] and calls == [] and w.refusals["reply_subject"] == 1

    run(go())


def test_work_not_started_after_exp() -> None:
    """The deadline is the token's exp: verified, then exp passes before the
    handler would start, so it never starts."""

    async def go() -> None:
        calls: list[int] = []

        async def h(v: Verified, m: Message) -> Any:
            calls.append(1)
            return await echo.ping(v, m)

        clock = Clock()
        w, nc, signer = make_worker({"ping": h}, clock=clock)
        await started(w, nc)
        body = ping_body()
        hdrs = await headers_of(core_pairs(signer, body, ttl=1))
        v = w.verifier.verify_request(PING, "_INBOX_gateway.t.1", hdrs, body, w.decoder("ping"))
        assert isinstance(v, Verified)
        clock.offset = 5
        await w.run_handler(msg(PING, "_INBOX_gateway.t.1", body, hdrs), v, bff_direct=False)
        await w.close()
        assert calls == []
        [(_, _, h2)] = nc.published
        assert h2 is not None and h2[envelope.STATUS] == "deadline_exceeded"

    run(go())


def test_handler_error_is_internal_and_not_logged(caplog: pytest.LogCaptureFixture) -> None:
    async def go() -> None:
        async def h(v: Verified, m: Message) -> Any:
            raise ValueError("synthetic secret detail")

        w, nc, signer = make_worker({"ping": h})
        await started(w, nc)
        body = ping_body()
        await w.dispatch(
            msg(PING, "_INBOX_gateway.t.1", body, await headers_of(core_pairs(signer, body)))
        )
        await w.close()
        [(_, _, hd)] = nc.published
        assert hd is not None and hd[envelope.STATUS] == "internal"

    with caplog.at_level(logging.INFO):
        run(go())
    assert "ValueError" in caplog.text and "synthetic secret detail" not in caplog.text


def test_wrong_response_type_is_internal() -> None:
    async def go() -> None:
        async def h(v: Verified, m: Message) -> Any:
            return envelope_pb2.ResponseMeta(), pipeline_pb2.HealthResponse()

        w, nc, signer = make_worker({"ping": h})
        await started(w, nc)
        body = ping_body()
        await w.dispatch(
            msg(PING, "_INBOX_gateway.t.1", body, await headers_of(core_pairs(signer, body)))
        )
        await w.close()
        [(_, _, hd)] = nc.published
        assert hd is not None and hd[envelope.STATUS] == "internal"

    run(go())


def test_bad_bearer_is_unauthorized_without_a_grant() -> None:
    async def go() -> None:
        w, nc, _ = make_worker()
        await started(w, nc)
        for auth in ["Basic abc", "Bearer", "Bearer a b", "Bearer ", ACCESS]:
            hdrs = await headers_of(
                [
                    (envelope.AUTHORIZATION, auth),
                    (envelope.SCHEMA, PING_SCHEMA),
                    (envelope.REQUEST_ID, RID),
                ]
            )
            await w.dispatch(msg(PING, "_INBOX_bff.clinical.t.1", ping_body(), hdrs))
        await w.close()
        assert nc.requests == []
        assert [h[envelope.STATUS] for _, _, h in nc.published if h] == ["unauthorized"] * 5

    run(go())


def test_worker_refuses_handlers_without_an_operation() -> None:
    async def h(v: Verified, m: Message) -> Any:
        raise AssertionError

    with pytest.raises(Exception, match="no operation entry"):
        make_worker({"nope": h})


def test_echo_worker_refuses_outside_development(monkeypatch: pytest.MonkeyPatch) -> None:
    """echo.ping is test-only: in production the table leaves it out, so an
    echo worker can't even be built."""
    signer = Signer()
    ops = Operations.load(development=False)
    v = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()), "echo", ops, JtiSet(time.time), time.time
    )
    s = WorkerSettings(service="echo", nats_url="nats://127.0.0.1:4222", trusted_keys_path="/x")
    with pytest.raises(Exception, match="no operation entry"):
        Worker(s, dict(echo.HANDLERS), v)


# --- the grant client -----------------------------------------------------------


def grant_reply(status: str, schema: str, body: bytes) -> Msg:
    return msg(
        "_INBOX_worker.echo.x.1", "", body, {envelope.STATUS: status, envelope.SCHEMA: schema}
    )


def ask(
    nc: FakeNC,
    *,
    deadline: float,
    reply: str = "_INBOX_bff.clinical.t.1",
    clock: Callable[[], float] = time.time,
) -> Any:
    g = GrantClient(nc, "echo", clock=clock)  # type: ignore[arg-type]
    return g.ask(
        ACCESS, PING, reply, ping_body(), (), "77777777-7777-4777-8777-777777777777", deadline
    )


def test_grant_budget_is_min_of_250ms_and_a_quarter() -> None:
    async def go() -> None:
        nc = FakeNC()

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            return grant_reply(
                "ok",
                grant.RESPONSE_SCHEMA,
                authz_pb2.GrantResponse(request_token="t.t.t").SerializeToString(),
            )

        nc.answer = answer
        assert await ask(nc, deadline=time.time() + 5) == "t.t.t"
        assert await ask(nc, deadline=time.time() + 0.4) == "t.t.t"
        (s1, p1, t1), (_, _, t2) = nc.requests
        assert s1 == "authz.v1.grant.echo"  # its own grant subject only
        assert t1 == pytest.approx(0.25) and t2 == pytest.approx(0.1, abs=0.01)
        req = authz_pb2.GrantRequest()
        req.ParseFromString(p1)
        assert req.operation == "echo.ping" and req.reply_prefix == "_INBOX_bff.clinical."
        assert 4900 <= req.deadline_ms <= 5000

    run(go())


def test_grant_refuses_a_foreign_reply_prefix_at_once() -> None:
    async def go() -> None:
        nc = FakeNC()
        for reply in ["_INBOX_bff.platform.t.1", "_INBOX_gateway.t.1", "_INBOX_bff.clinical."]:
            with pytest.raises(ValueError):
                await ask(nc, deadline=time.time() + 5, reply=reply)
        assert nc.requests == []

    run(go())


@pytest.mark.parametrize(
    "exc,reason",
    [
        (nats.errors.TimeoutError, "timeout"),
        (nats.errors.NoRespondersError, "no_responders"),
        (nats.errors.ConnectionClosedError, "disconnected"),
        (OSError, "disconnected"),
        (RuntimeError, "error"),
    ],
)
def test_grant_failures_are_unavailable(exc: type[BaseException], reason: str) -> None:
    async def go() -> None:
        nc = FakeNC()

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            raise exc()

        nc.answer = answer
        assert await ask(nc, deadline=time.time() + 5) == Unavailable(reason)

    run(go())


def test_grant_past_deadline_asks_nothing() -> None:
    async def go() -> None:
        nc = FakeNC()
        assert await ask(nc, deadline=time.time() - 1) == Unavailable("deadline")
        assert nc.requests == []

    run(go())


def test_grant_reply_after_budget_is_dropped() -> None:
    """Even if a reply came back after the budget, it is never used."""

    async def go() -> None:
        nc = FakeNC()
        t = [1000.0]

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            t[0] += 1.0  # the clock moves past the budget
            return grant_reply(
                "ok",
                grant.RESPONSE_SCHEMA,
                authz_pb2.GrantResponse(request_token="t.t.t").SerializeToString(),
            )

        nc.answer = answer
        assert await ask(nc, deadline=1005.0, clock=lambda: t[0]) == Unavailable("late")

    run(go())


@pytest.mark.parametrize(
    "status,schema,body,want",
    [
        (
            "mfa_required",
            grant.REFUSED_SCHEMA,
            authz_pb2.GrantRefused(code="mfa_required").SerializeToString(),
            GrantRefused("mfa_required"),
        ),
        (
            "not_ready",
            grant.REFUSED_SCHEMA,
            authz_pb2.GrantRefused(code="not_ready").SerializeToString(),
            GrantRefused("not_ready"),
        ),
        (
            "Bad Code",
            grant.REFUSED_SCHEMA,
            authz_pb2.GrantRefused(code="Bad Code").SerializeToString(),
            Unavailable("malformed"),
        ),
        (
            "forbidden",
            grant.REFUSED_SCHEMA,
            authz_pb2.GrantRefused(code="other").SerializeToString(),
            Unavailable("malformed"),
        ),
        ("ok", grant.REFUSED_SCHEMA, b"", Unavailable("malformed")),
        (
            "ok",
            grant.RESPONSE_SCHEMA,
            authz_pb2.GrantResponse().SerializeToString(),
            Unavailable("malformed"),
        ),
        ("ok", grant.RESPONSE_SCHEMA, b"\xff\xff", Unavailable("malformed")),
        (
            "ok",
            grant.RESPONSE_SCHEMA,
            authz_pb2.GrantResponse(request_token="x" * 4097).SerializeToString(),
            Unavailable("malformed"),
        ),
        ("ok", "mhai.intelligence.v1.Error", b"", Unavailable("malformed")),
    ],
)
def test_grant_reply_shapes(status: str, schema: str, body: bytes, want: Any) -> None:
    async def go() -> None:
        nc = FakeNC()

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            return grant_reply(status, schema, body)

        nc.answer = answer
        assert await ask(nc, deadline=time.time() + 5) == want

    run(go())


def test_grant_client_never_logs_the_access_token(caplog: pytest.LogCaptureFixture) -> None:
    async def go() -> None:
        nc = FakeNC()

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            raise nats.errors.TimeoutError()

        nc.answer = answer
        await ask(nc, deadline=time.time() + 5)

    with caplog.at_level(logging.DEBUG):
        run(go())
    assert ACCESS not in caplog.text


# --- settings -------------------------------------------------------------------


BASE = {
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://pipeline:test-only-pw@127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/run/trusted/keys.json",
    "APP_ENV": "development",
}


def test_settings_from_env() -> None:
    s = WorkerSettings.from_env(BASE)
    assert (s.service, s.max_concurrency, s.operations_path) == ("pipeline", 4, None)
    s = WorkerSettings.from_env({**BASE, "INTELLIGENCE_MAX_CONCURRENCY": "9"}, service="pipeline")
    assert s.max_concurrency == 9


@pytest.mark.parametrize(
    "env,name",
    [
        ({"INTELLIGENCE_SERVICE": ""}, "INTELLIGENCE_SERVICE"),
        ({"INTELLIGENCE_SERVICE": "nli"}, "INTELLIGENCE_SERVICE"),
        ({"NATS_URL": ""}, "NATS_URL"),
        ({"NATS_URL": "nats://pipeline:changeme@127.0.0.1:4222"}, "NATS_URL"),
        ({"NATS_URL": "nats://pipeline:PlaceHolder1@127.0.0.1:4222"}, "NATS_URL"),
        ({"NATS_URL": "http://[::1"}, "NATS_URL"),
        ({"INTELLIGENCE_TRUSTED_KEYS_PATH": ""}, "INTELLIGENCE_TRUSTED_KEYS_PATH"),
        ({"INTELLIGENCE_MAX_CONCURRENCY": "0"}, "INTELLIGENCE_MAX_CONCURRENCY"),
        ({"INTELLIGENCE_MAX_CONCURRENCY": "four"}, "INTELLIGENCE_MAX_CONCURRENCY"),
    ],
)
def test_settings_errors_name_the_variable_never_the_value(env: dict[str, str], name: str) -> None:
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**BASE, **env})
    assert name in str(e.value)
    for v in env.values():
        if len(v) > 3:  # a short value can't be told from the message's own text
            assert v not in str(e.value)
    assert "test-only-pw" not in str(e.value)


def test_settings_echo_only_in_development() -> None:
    env = {**BASE, "INTELLIGENCE_SERVICE": "echo"}
    assert WorkerSettings.from_env(env).service == "echo"
    for app_env in ["production", "", "local", "Production"]:
        with pytest.raises(ConfigError, match="APP_ENV"):
            WorkerSettings.from_env({**env, "APP_ENV": app_env})
    unset = {k: v for k, v in env.items() if k != "APP_ENV"}
    with pytest.raises(ConfigError, match="APP_ENV"):
        WorkerSettings.from_env(unset)  # unset is production


def test_settings_db_url_is_pipeline_only() -> None:
    env = {**BASE, "INTELLIGENCE_SERVICE": "echo", "INTELLIGENCE_DB_URL": "postgres://x@y/z"}
    with pytest.raises(ConfigError, match="INTELLIGENCE_DB_URL"):
        WorkerSettings.from_env(env)


def test_settings_outside_development_need_tls_and_nkeys() -> None:
    prod = {**BASE, "APP_ENV": "production"}
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env(prod)
    assert "NATS_TLS_CA_PATH" in str(e.value) and "NATS_NKEY_SEED_PATH" in str(e.value)
    ok = {
        **prod,
        "NATS_URL": "tls://127.0.0.1:4222",
        "NATS_TLS_CA_PATH": "/run/nats/ca.pem",
        "NATS_TLS_CERT_PATH": "/run/nats/pipeline.pem",
        "NATS_TLS_KEY_PATH": "/run/nats/pipeline-key.pem",
        "NATS_NKEY_SEED_PATH": "/run/nats/pipeline.nk",
    }
    s = WorkerSettings.from_env(ok)
    assert (s.tls_ca_path, s.tls_cert_path) == ("/run/nats/ca.pem", "/run/nats/pipeline.pem")
    with pytest.raises(ConfigError, match="NATS_URL"):
        WorkerSettings.from_env({**ok, "NATS_URL": "tls://pipeline:pw123456789@127.0.0.1:4222"})
    # A client certificate too: the production server runs TLS with verify.
    with pytest.raises(ConfigError, match="set together"):
        WorkerSettings.from_env({k: v for k, v in ok.items() if k != "NATS_TLS_KEY_PATH"})
    # The packaged permission table only, in production.
    with pytest.raises(ConfigError, match="INTELLIGENCE_OPERATIONS_PATH"):
        WorkerSettings.from_env({**ok, "INTELLIGENCE_OPERATIONS_PATH": "/tmp/ops.json"})
    dev = {**BASE, "INTELLIGENCE_OPERATIONS_PATH": "/tmp/ops.json"}
    assert WorkerSettings.from_env(dev).operations_path == "/tmp/ops.json"


def test_settings_service_flag_must_agree() -> None:
    with pytest.raises(ConfigError, match="INTELLIGENCE_SERVICE"):
        WorkerSettings.from_env(BASE, service="echo")


# --- the services ---------------------------------------------------------------


def test_echo_fact_is_one_core_accepts() -> None:
    """core's intel.AppendFacts: action intelligence.<...>, not one of core's
    own, entity and entity id from their fixed alphabets, no payload."""
    import re

    f = echo.pinged_fact()
    assert re.fullmatch(r"intelligence(\.[a-z0-9_]{1,32}){1,4}", f.action)
    assert f.action not in {
        "intelligence.refused",
        "intelligence.granted",
        "intelligence.grant_refused",
    }
    assert re.fullmatch(r"[a-z0-9_.]{0,64}", f.entity) and re.fullmatch(
        r"[A-Za-z0-9_.:-]{0,128}", f.entity_id
    )
    assert not f.HasField("payload")


def test_health_readiness_fails_closed() -> None:
    async def go() -> None:
        def boom() -> bool:
            raise RuntimeError("synthetic")

        async def ready() -> bool:
            return True

        for check, want in [(None, False), (lambda: True, True), (boom, False), (ready, True)]:
            h = pipeline.health_handler(check) if check is not None else pipeline.health_handler()
            _, out = await h(None, pipeline_pb2.HealthRequest())  # type: ignore[arg-type]
            assert isinstance(out, pipeline_pb2.HealthResponse)
            assert out.database_ready is want and out.models_ready is False

    run(go())


@pytest.mark.parametrize(
    "value,ok",
    [
        ("100", True),
        ("999999", True),
        ("0", False),
        ("-5", False),
        ("1e3", False),
        ("\u0661\u0662", False),
    ],
)
def test_bff_timeout_header_only_shortens(value: str, ok: bool) -> None:
    async def go() -> None:
        w, nc, _ = make_worker()
        await started(w, nc)
        seen: list[float] = []

        async def answer(subject: str, payload: bytes, timeout: float) -> Msg:
            req = authz_pb2.GrantRequest()
            req.ParseFromString(payload)
            seen.append(req.deadline_ms)
            raise nats.errors.TimeoutError()

        nc.answer = answer
        hdrs = await headers_of(
            [
                (envelope.AUTHORIZATION, f"Bearer {ACCESS}"),
                (envelope.SCHEMA, PING_SCHEMA),
                (envelope.REQUEST_ID, RID),
                (envelope.TIMEOUT, value),
            ]
        )
        await w.dispatch(msg(PING, "_INBOX_bff.clinical.t.1", ping_body(), hdrs))
        await w.close()
        [(_, _, h)] = nc.published
        assert h is not None
        if ok:
            assert h[envelope.STATUS] == "unavailable"
            assert 0 < seen[0] <= min(int(value), 5000)
        else:
            assert h[envelope.STATUS] == "invalid_request" and seen == []

    run(go())


# --- hardening round: TLS, the header parser, the BFF request id ----------------


def tls_files(tmp: Any) -> tuple[str, str, str]:
    """A throwaway self-signed certificate (CA and client at once), test-only."""
    import datetime

    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec
    from cryptography.x509.oid import NameOID

    k = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "test-only")])
    now = datetime.datetime.now(datetime.UTC)
    c = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(k.public_key())
        .serial_number(1)
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(hours=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(k, hashes.SHA256())
    )
    cert = tmp / "test-only.pem"
    key = tmp / "test-only-key.pem"
    cert.write_bytes(c.public_bytes(serialization.Encoding.PEM))
    key.write_bytes(
        k.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        )
    )
    return str(cert), str(cert), str(key)


def _prod_worker(url: str, tmp: Any, *, with_tls: bool = True) -> Worker:
    signer = Signer()
    ops = Operations.load(development=False)
    v = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()),
        "pipeline",
        ops,
        JtiSet(time.time),
        time.time,
    )
    ca = cert = key = None
    if with_tls:
        ca, cert, key = tls_files(tmp)
    s = WorkerSettings(
        service="pipeline",
        nats_url=url,
        trusted_keys_path="/x",
        tls_ca_path=ca,
        tls_cert_path=cert,
        tls_key_path=key,
        development=False,
    )
    return Worker(s, pipeline.handlers(), v)


def test_production_worker_never_sends_connect_to_a_plaintext_server(tmp_path: Any) -> None:
    """A fake server that never asks for TLS (tls_required false), as a
    downgrading server would: the worker's first bytes are a TLS
    ClientHello, the handshake fails, and no CONNECT (with its
    credentials) is ever written in the clear."""

    async def go() -> None:
        got: list[bytes] = []

        async def serve(r: asyncio.StreamReader, wr: asyncio.StreamWriter) -> None:
            wr.write(
                b'INFO {"server_id":"fake","version":"2.10.0","tls_required":false,"headers":true,"max_payload":1048576}\r\n'
            )
            await wr.drain()
            try:
                while data := await r.read(4096):
                    got.append(data)
            finally:
                wr.close()

        server = await asyncio.start_server(serve, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        w = _prod_worker(f"nats://pipeline:test-only-pw@127.0.0.1:{port}", tmp_path)
        try:
            with pytest.raises(TimeoutError):
                await asyncio.wait_for(w.start(), 2)
        finally:
            server.close()
        raw = b"".join(got)
        assert got and got[0][:1] == b"\x16"  # a TLS handshake record first
        assert b"CONNECT" not in raw and b"test-only-pw" not in raw

    run(go())


def test_production_worker_needs_tls_settings() -> None:
    async def go() -> None:
        w = _prod_worker("nats://127.0.0.1:1", None, with_tls=False)
        with pytest.raises(ConfigError, match="NATS_TLS_CA_PATH"):
            await w.start()

    run(go())


def test_plaintext_after_reconnect_stops_the_worker() -> None:
    """Belt and braces: if a reconnect ever lands without TLS, the worker
    stops serving (and the CLI exits non-zero)."""

    async def go() -> None:
        w, nc, _ = make_worker()
        await started(w, nc)
        w._require_tls = True
        w._stop = asyncio.Event()
        await w._on_reconnected()  # FakeNC has no TLS transport
        assert w.insecure and w._stop.is_set() and nc.is_closed

    run(go())


def test_worker_refuses_to_start_with_fast_mail_parser(monkeypatch: pytest.MonkeyPatch) -> None:
    import nats.aio.client as nats_client

    monkeypatch.setattr(nats_client, "parse_email", object())

    async def go() -> None:
        w, nc, _ = make_worker()
        with pytest.raises(RuntimeError, match="fast_mail_parser"):
            await w.start(nc=nc)  # type: ignore[arg-type]

    run(go())


def test_parse_email_is_off() -> None:
    import nats.aio.client as nats_client

    assert nats_client.parse_email is None


@pytest.mark.parametrize(
    "raw",
    [
        b"NATS/1.0\r\nMhai-Request-Token: A\nMhai-Request-Token: B\r\n\r\n",
        b"NATS/1.0\nMhai-Request-Token: A\nMhai-Request-Token: B\n\n",
        b"NATS/1.0\r\nMhai-Request-Token: A\r\n B\r\n\r\n",
        b"NATS/1.0\r\nMhai-Request-Token: A\rB\r\n\r\n",
    ],
)
def test_bare_lf_folded_or_cr_credential_headers_refused(raw: bytes) -> None:
    async def go() -> None:
        h = await WorkerClient()._process_headers(raw)
        assert h is not None and h.ambiguous
        w, nc, _ = make_worker()
        await started(w, nc)
        await w.dispatch(msg(PING, "_INBOX_gateway.t.1", ping_body(), h))
        await w.close()
        [(_, _, hd)] = nc.published
        assert hd is not None and hd[envelope.STATUS] == "unauthorized"
        assert w.refusals["ambiguous_headers"] == 1

    run(go())


@pytest.mark.parametrize(
    "rid", [None, "", "short", "has space 1234", "x" * 65, "bad/char/123", "é" * 10]
)
def test_bad_bff_request_id_refused_before_any_grant(rid: str | None) -> None:
    async def go() -> None:
        w, nc, _ = make_worker()
        await started(w, nc)
        pairs = [(envelope.AUTHORIZATION, f"Bearer {ACCESS}"), (envelope.SCHEMA, PING_SCHEMA)]
        if rid is not None:
            pairs.append((envelope.REQUEST_ID, rid))
        for reply in ["_INBOX_bff.clinical.t.1", "_INBOX_gateway.t.1"]:
            await w.dispatch(msg(PING, reply, ping_body(), await headers_of(pairs)))
        await w.close()
        assert nc.requests == []  # core never asked
        # Answered only on the checked clinical BFF inbox.
        [(subject, _, hd)] = nc.published
        assert subject == "_INBOX_bff.clinical.t.1"
        assert hd is not None and hd[envelope.STATUS] == "invalid_request"
        assert envelope.REQUEST_ID not in hd

    run(go())
