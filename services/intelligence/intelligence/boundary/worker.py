"""The NATS worker runtime (spec §4, §7.3, §9.1, §17.2-17.3).

Each copy of a service subscribes inference.v1.<service>.> in the queue
group named after the service, so NATS gives each request to one copy. Per
message, in order:

  1. the reply subject is a literal subject (no wildcard, no empty token,
     printable ASCII, at most 256 bytes); otherwise drop, no reply;
  2. the credential headers (Mhai-Request-Token, Mhai-Schema,
     Mhai-Request-Id, Authorization, Mhai-Timeout-Ms) each appear at most
     once, with their exact name, and a message never carries both a
     request token and an access token; otherwise refuse;
  3. a request token: the verifier's checks 1-9. An access token
     (Authorization) on a bff_direct operation: check 1, a grant from core,
     then checks 2-9 on the token core returned. Anything else:
     unauthorized;
  4. the concurrency cap: past it, overloaded at once;
  5. the handler, under a deadline at the token's exp.

Replies: only to the message's own reply subject, and only once the
verifier passed it (check 5); a refusal before check 5 goes only to a
literal inbox under _INBOX_gateway. (or _INBOX_bff.clinical. on the grant
path). Nothing here sizes a message: the verifier owns check 1 (F25). Logs
carry reason codes and verified ids only, never a token, a header value or
a body.

Jobs (spec §9.2, T11): a service with job handlers also pulls from its own
durable consumer on JOBS (named after the service, shared by every copy),
one message at a time and only while it has a free job slot and its
readiness check passes (no database: no job is taken). Jobs never reply.
A job that fails verification is Termed with a dead letter (ids, type,
attempts, code and reason only) and touches nothing else: without a
verified hospital there is no database access (departure 5). A verified job
goes to its handler, which owns the message's outcome.

The reply is published directly to msg.reply rather than with
Msg.respond: nats-py's respond() sends the request's own headers back,
which would echo the request token or the access token to the caller.
"""

from __future__ import annotations

import asyncio
import contextlib
import inspect
import json
import logging
import re
import signal
import ssl
import time
from collections import Counter
from collections.abc import Awaitable, Callable, Mapping
from typing import TYPE_CHECKING, Any

import nats.aio.client as nats_client
import nats.errors as nats_errors
from google.protobuf import descriptor_pool, message_factory
from google.protobuf.message import Message
from nats.aio.client import Client
from nats.aio.msg import Msg
from nats.aio.subscription import Subscription
from opentelemetry.context import Context
from opentelemetry.trace import SpanKind

from intelligence.boundary import _b64, envelope, telemetry
from intelligence.boundary.config import ConfigError, WorkerSettings
from intelligence.boundary.grant import GrantClient, GrantRefused, Unavailable
from intelligence.boundary.jobs import JobContext, JobHandler, Slot
from intelligence.boundary.keys import TrustedKeys, install_sighup_reload
from intelligence.boundary.operations import Operation, Operations

# The request and response types the operations name, registered in the
# default descriptor pool for _decoder_for.
from intelligence.boundary.pb import echo_pb2, envelope_pb2, pipeline_pb2  # noqa: F401
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.verify import (
    REPLY_CORE,
    REPLY_GRANT,
    Decoder,
    Refusal,
    Verified,
    Verifier,
    strict_decoder,
)

if TYPE_CHECKING:
    from intelligence.boundary.db import Database

log = logging.getLogger("intelligence.boundary.worker")

Handler = Callable[[Verified, Message], Awaitable[tuple[envelope_pb2.ResponseMeta, Message]]]
Readiness = Callable[[], bool | Awaitable[bool]]

JOBS_STREAM = "JOBS"  # core's bus.JobsStream; the durable is the service's name
DEAD_STREAM = "DEAD"
NATS_MSG_ID = "Nats-Msg-Id"

MAX_REPLY = 256
TIMEOUT_HEADER = envelope.TIMEOUT
# Headers whose value decides who is asking or what: each at most once,
# under its exact name (T8 Minor 5).
GUARDED = (
    envelope.TOKEN,
    envelope.SCHEMA,
    envelope.REQUEST_ID,
    envelope.AUTHORIZATION,
    envelope.TIMEOUT,
)
_GUARDED_LOWER = {g.lower().encode(): g for g in GUARDED}
_RETRYABLE = frozenset({"overloaded", "unavailable", "deadline_exceeded"})
_BFF_RID = re.compile(r"[A-Za-z0-9_-]{8,64}")
_SAFE = re.compile(r"[A-Za-z0-9_.:\-]{1,128}")
_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_KID = re.compile(r"[0-9a-f]{16}")
_TOKEN = re.compile(r"[A-Za-z0-9_-]{1,64}")


def literal_reply(reply: str) -> bool:
    """reply is one literal subject: tokens non-empty, printable ASCII with
    no space, no wildcard, at most 256 bytes (core's bus.ReplyUnder)."""
    if not reply or len(reply) > MAX_REPLY:
        return False
    for tok in reply.split("."):
        if not tok:
            return False
        for ch in tok:
            if ch <= " " or ch >= "\x7f" or ch in "*>":
                return False
    return True


def _under(prefix: str, reply: str) -> bool:
    return len(reply) > len(prefix) and reply.startswith(prefix)


def _safe(v: str) -> str:
    return v if _SAFE.fullmatch(v) else "?"


class Headers(dict[str, str]):
    """A message's headers, with ambiguous set when a guarded header was
    repeated or sent under another spelling (nats-py keeps one value)."""

    ambiguous: bool = False


def _ambiguous(raw: bytes, parsed: Mapping[str, str]) -> bool:
    """True when a guarded header could be read two ways. Lines are split on
    LF as well as CRLF (as Go's textproto does, though nats-py splits on CRLF
    only), so "A\\nMhai-Request-Token: B" is a repeat, not one odd value; a
    continuation line after a guarded header, or a stray CR in one, is
    ambiguous too."""
    nl = raw.find(b"\n")
    if nl < 0:
        return False
    seen: Counter[str] = Counter()
    after_guarded = False
    for line in raw[nl + 1 :].split(b"\n"):
        line = line.removesuffix(b"\r")
        if after_guarded and line[:1] in (b" ", b"\t"):
            return True  # a folded value: readers disagree on it
        name, sep, _ = line.partition(b":")
        g = _GUARDED_LOWER.get(name.strip().lower()) if sep else None
        after_guarded = g is not None
        if g is None:
            continue
        if name.strip() != g.encode() or b"\r" in line:
            return True  # another spelling, surrounding space or a stray CR
        seen[g] += 1
    return any(n > 1 or g not in parsed for g, n in seen.items())


def _check_header_parser() -> None:
    """The repeat check assumes nats-py's own header parser. With the
    optional fast_mail_parser installed nats-py uses that instead, whose
    repeat and folding rules differ: refuse to start (not an assert, which
    -O would strip)."""
    if nats_client.parse_email is not None:
        raise RuntimeError("nats-py is using fast_mail_parser; the worker needs its own parser")


def _is_tls(nc: Client) -> bool:
    """The connection's transport is TLS. Anything unexpected is False."""
    try:
        writer = nc._transport._io_writer  # type: ignore[union-attr]
        return writer is not None and writer.get_extra_info("ssl_object") is not None
    except Exception:
        return False


class WorkerClient(Client):
    """nats-py's client, with the header parser marking repeated or
    re-spelled credential headers. The worker refuses headers that didn't
    come through here (fail closed if nats-py stops calling this hook)."""

    async def _process_headers(self, headers: bytes) -> dict[str, str] | None:  # type: ignore[override]
        parsed = await super()._process_headers(headers)
        if not headers:
            return parsed
        out = Headers(parsed or {})
        out.ambiguous = _ambiguous(bytes(headers), out)
        if not out and not out.ambiguous:
            return None
        return out


def _decoder_for(op: Operation) -> Decoder:
    desc = descriptor_pool.Default().FindMessageTypeByName(op.request_schema)
    return strict_decoder(message_factory.GetMessageClass(desc))


def _never(_body: bytes) -> Message:
    raise ValueError("no handler for this subject")


def _bearer(value: str) -> str | None:
    """core's authz.bearer: "Bearer <token>", one space, no other blanks."""
    scheme, sep, tok = value.partition(" ")
    if not sep or scheme.lower() != "bearer" or not tok or any(c in tok for c in " \t"):
        return None
    return tok


class Worker:
    """One copy of one service."""

    def __init__(
        self,
        settings: WorkerSettings,
        handlers: Mapping[str, Handler],
        verifier: Verifier,
        *,
        jobs: Mapping[str, JobHandler] | None = None,
        jobs_ready: Readiness | None = None,
    ) -> None:
        service = settings.service
        if verifier.service != service:
            raise ValueError("the verifier is for another service")
        ops = verifier.operations
        ops.require(service, handlers)
        self.settings = settings
        self.service = service
        self.verifier = verifier
        self._ops = ops
        self._clock = verifier.clock
        self._handlers: dict[str, tuple[Operation, Handler, Decoder]] = {}
        for name, h in handlers.items():
            op = ops.by_name(f"{service}.{name}")
            assert op is not None  # require() checked
            if op.kind != "req":
                raise ValueError(f"handler {op.name} is not request/reply")
            self._handlers[op.name] = (op, h, _decoder_for(op))
        self._jobs: dict[str, tuple[Operation, JobHandler, Decoder]] = {}
        if jobs:
            ops.require(service, jobs)
            if jobs_ready is None:
                raise ValueError("job handlers need a readiness check")
            for name, jh in jobs.items():
                op = ops.by_name(f"{service}.{name}")
                assert op is not None  # require() checked
                if op.kind != "job":
                    raise ValueError(f"handler {op.name} is not a job")
                self._jobs[op.op] = (op, jh, _decoder_for(op))
        self._jobs_ready = jobs_ready
        self._sem = asyncio.Semaphore(settings.max_concurrency)
        # Job slots, apart from request/reply's, so a long job never makes
        # health answer overloaded. A slot is held until its model threads end.
        self._job_sem = asyncio.Semaphore(settings.max_concurrency)
        self._job_loop: asyncio.Task[None] | None = None
        self._psub: Any = None
        self._js: Any = None
        self.fetch_timeout = 5.0
        self._tasks: set[asyncio.Task[None]] = set()
        self._nc: Client | None = None
        self._sub: Subscription | None = None
        self._grant: GrantClient | None = None
        self._stop: asyncio.Event | None = None
        self.draining = False
        self.insecure = False
        self._require_tls = False
        self.received = 0
        # Worker-side refusals and drops by reason (the verifier counts its own).
        self.refusals: Counter[str] = telemetry.RefusalCounter(service)

    # --- lifecycle --------------------------------------------------------------

    @property
    def connection(self) -> Client:
        if self._nc is None:
            raise RuntimeError("not started")
        return self._nc

    def decoder(self, name: str) -> Decoder:
        return self._handlers[f"{self.service}.{name}"][2]

    async def _connect(self) -> Client:
        """Connect. Outside development TLS is the client's rule, not the
        server's: nats-py only upgrades when the server's INFO says
        tls_required, so a fake or downgraded server could keep the session
        (and the CONNECT credentials) in plaintext. tls_handshake_first does
        the TLS handshake before reading a single protocol byte, on every
        connect and reconnect, so a plaintext server fails the handshake and
        nothing is ever sent in the clear. The transport is checked for TLS
        after connecting and after every reconnect as well."""
        s = self.settings
        if not s.development and not (s.tls_ca_path and s.tls_cert_path and s.tls_key_path):
            raise ConfigError(
                "NATS_TLS_CA_PATH, NATS_TLS_CERT_PATH and NATS_TLS_KEY_PATH are required "
                "outside development"
            )
        nc = WorkerClient()
        tls = None
        if s.tls_ca_path:
            tls = ssl.create_default_context(cafile=s.tls_ca_path)
            if s.tls_cert_path and s.tls_key_path:
                tls.load_cert_chain(s.tls_cert_path, s.tls_key_path)
        self._require_tls = tls is not None

        async def on_error(e: Exception) -> None:
            log.warning("nats error service=%s error=%s", self.service, type(e).__name__)

        await nc.connect(
            servers=[u.strip() for u in s.nats_url.split(",")],
            name=f"intelligence-{self.service}",
            inbox_prefix=f"_INBOX_worker.{self.service}",
            tls=tls,
            nkeys_seed=s.nkey_seed_path,
            error_cb=on_error,
            max_reconnect_attempts=-1,
            tls_handshake_first=not s.development,
            reconnected_cb=self._on_reconnected,
        )
        if self._require_tls and not _is_tls(nc):
            await nc.close()
            raise ConnectionError("the NATS connection is not TLS")
        return nc

    async def _on_reconnected(self) -> None:
        if self._require_tls and self._nc is not None and not _is_tls(self._nc):
            # Never reached with handshake-first, but if it is: stop, don't serve.
            self.insecure = True
            self.refusals["plaintext"] += 1
            log.error("nats reconnected without TLS service=%s; stopping", self.service)
            await self.abort()
            if self._stop is not None:
                self._stop.set()

    async def start(self, nc: Client | None = None) -> None:
        """Connect (or use nc) and join the service's queue group."""
        _check_header_parser()
        self._nc = nc if nc is not None else await self._connect()
        self._grant = GrantClient(self._nc, self.service, clock=self._clock)
        self._sub = await self._nc.subscribe(
            f"inference.v1.{self.service}.>", queue=self.service, cb=self.dispatch
        )
        await self._nc.flush()
        if self._jobs:
            await self._start_jobs()
        log.info(
            "worker started service=%s max_concurrency=%d jobs=%s",
            self.service,
            self.settings.max_concurrency,
            ",".join(sorted(self._jobs)) or "-",
        )

    async def run(self, trusted: TrustedKeys | None = None) -> None:
        """Serve until SIGTERM (or SIGINT), then drain. With trusted, SIGHUP
        re-reads the trusted-keys file on this loop."""
        loop = asyncio.get_running_loop()
        self._stop = asyncio.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, self._stop.set)
        if trusted is not None:
            install_sighup_reload(trusted, self.settings.trusted_keys_path, loop)
        try:
            await self._stop.wait()
            log.info("worker draining service=%s", self.service)
            await self.drain()
        finally:
            for sig in (signal.SIGTERM, signal.SIGINT):
                loop.remove_signal_handler(sig)
            if trusted is not None:
                loop.remove_signal_handler(signal.SIGHUP)

    async def drain(self) -> None:
        """Take no new message, finish the ones in hand (each bounded by its
        exp), then close."""
        self.draining = True
        await self._stop_jobs()
        if self._sub is not None:
            with contextlib.suppress(Exception):
                await self._sub.drain()
            self._sub = None
        if self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)
        if self._nc is not None and not self._nc.is_closed:
            with contextlib.suppress(Exception):
                await self._nc.drain()

    async def close(self) -> None:
        """drain(), safe to call twice."""
        await self.drain()

    async def abort(self) -> None:
        """Stop at once: no drain, work in hand abandoned (and not answered)."""
        self.draining = True
        await self._stop_jobs()
        for t in list(self._tasks):
            t.cancel()
        if self._nc is not None and not self._nc.is_closed:
            with contextlib.suppress(Exception):
                await self._nc.close()
        if self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)

    # --- per message -------------------------------------------------------------

    def _drop(self, reason: str, subject: str) -> None:
        self.refusals[reason] += 1
        log.warning(
            "worker dropped service=%s subject=%s reason=%s reply=False",
            self.service,
            _safe(subject),
            reason,
        )

    async def _send(self, msg: Msg, headers: dict[str, str], body: bytes) -> None:
        """The one place a reply is published: to msg.reply, which every
        caller has checked (verified by check 5, or a literal inbox under the
        one prefix the path allows)."""
        try:
            await self.connection.publish(msg.reply, body, headers=headers)
        except Exception as e:
            log.warning("worker reply failed service=%s error=%s", self.service, type(e).__name__)

    async def _error(
        self, msg: Msg, code: str, *, request_id: str = "", bff_direct: bool = False
    ) -> None:
        h, b = envelope.reply_error(
            code, code in _RETRYABLE, request_id=request_id, bff_direct=bff_direct
        )
        telemetry.current_fail(code)  # the span in hand, if any: the code only
        await self._send(msg, h, b)

    async def _refuse_unchecked(
        self, msg: Msg, reason: str, code: str, prefix: str, request_id: str = ""
    ) -> None:
        """A refusal before check 5: answer only a literal inbox under prefix."""
        can_reply = _under(prefix, msg.reply)
        self.refusals[reason] += 1
        log.warning(
            "worker refused service=%s subject=%s reason=%s code=%s reply=%s",
            self.service,
            _safe(msg.subject),
            reason,
            code,
            can_reply,
        )
        if can_reply:
            await self._error(msg, code, request_id=request_id, bff_direct=prefix == REPLY_GRANT)

    async def dispatch(self, msg: Msg) -> None:
        """The subscription callback. Never raises: any failure is no work
        and no reply, logged by type."""
        self.received += 1
        try:
            await self._dispatch(msg)
        except Exception as e:
            self.refusals["internal"] += 1
            log.error(
                "worker dispatch failed service=%s subject=%s error=%s reply=False",
                self.service,
                _safe(msg.subject),
                type(e).__name__,
            )

    async def _dispatch(self, msg: Msg) -> None:
        subject, reply, body = msg.subject, msg.reply, msg.data
        if not literal_reply(reply):
            self._drop("reply_subject", subject)
            return
        raw = msg.headers
        if raw is not None and not isinstance(raw, Headers):
            self._drop("headers", subject)  # not parsed by WorkerClient: fail closed
            return
        headers: Mapping[str, str] = raw if raw is not None else {}
        token = headers.get(envelope.TOKEN)
        auth = headers.get(envelope.AUTHORIZATION)
        grant_path = token is None and auth is not None
        prefix = REPLY_GRANT if grant_path else REPLY_CORE
        if (raw is not None and raw.ambiguous) or (token is not None and auth is not None):
            await self._refuse_unchecked(msg, "ambiguous_headers", "unauthorized", prefix)
            return
        if token is not None:
            await self._core_path(msg, headers, body)
        elif auth is not None:
            await self._grant_path(msg, headers, auth, body)
        else:
            await self._refuse_unchecked(msg, "no_credentials", "unauthorized", REPLY_CORE)

    def _route(self, subject: str) -> tuple[Operation, Handler, Decoder] | None:
        op = self._ops.for_subject(subject)
        if op is None or op.service != self.service:
            return None
        return self._handlers.get(op.name)

    async def _core_path(self, msg: Msg, headers: Mapping[str, str], body: bytes) -> None:
        route = self._route(msg.subject)
        decoder = route[2] if route is not None else _never
        with telemetry.verifying(self.service, telemetry.extract(headers)) as sp:
            r = self.verifier.verify_request(msg.subject, msg.reply, headers, body, decoder)
            if isinstance(r, Refusal):
                telemetry.fail(sp, r.code)
        if isinstance(r, Refusal):
            if r.reply:
                await self._error(msg, r.code)
            return
        if not await self._try_acquire():
            self.refusals["overloaded"] += 1
            await self._error(msg, "overloaded", request_id=r.rid)
            return
        self._spawn(self._run_and_release(msg, r, bff_direct=False))

    async def _grant_path(
        self, msg: Msg, headers: Mapping[str, str], auth: str, body: bytes
    ) -> None:
        if not _under(REPLY_GRANT, msg.reply):
            # Spec §17.2: any other prefix is refused at once, with no reply.
            self._drop("reply_subject", msg.subject)
            return
        bff_rid = headers.get(envelope.REQUEST_ID, "")
        if not _BFF_RID.fullmatch(bff_rid):
            # Core's validRequestID: refused here, before any grant is asked.
            await self._refuse_unchecked(msg, "request_id", "invalid_request", REPLY_GRANT)
            return
        echo_rid = bff_rid
        route = self._route(msg.subject)
        if route is None or not route[0].bff_direct:
            await self._refuse_unchecked(
                msg, "not_bff_direct", "unauthorized", REPLY_GRANT, echo_rid
            )
            return
        op = route[0]
        access = _bearer(auth)
        if access is None:
            await self._refuse_unchecked(
                msg, "authorization", "unauthorized", REPLY_GRANT, echo_rid
            )
            return
        size = self.verifier.check_sizes(access, body, "req")  # check 1, the verifier's
        if size is not None:
            await self._refuse_unchecked(msg, size.reason, size.code, REPLY_GRANT, echo_rid)
            return
        timeout_s = op.timeout_ms / 1000
        raw_timeout = headers.get(TIMEOUT_HEADER)
        if raw_timeout is not None:
            # The BFF's deadline can only shorten the operation's (as core's
            # natsapi); a value that isn't a positive whole number is refused.
            if not raw_timeout.isascii() or not raw_timeout.isdigit() or len(raw_timeout) > 9:
                ms = 0
            else:
                ms = int(raw_timeout)
            if ms <= 0:
                await self._refuse_unchecked(
                    msg, "timeout", "invalid_request", REPLY_GRANT, echo_rid
                )
                return
            timeout_s = min(ms, op.timeout_ms) / 1000
        if not await self._try_acquire():
            self.refusals["overloaded"] += 1
            await self._error(msg, "overloaded", request_id=echo_rid, bff_direct=True)
            return
        deadline = self._clock() + timeout_s
        self._spawn(self._granted(msg, headers, access, body, bff_rid, echo_rid, deadline))

    async def _granted(
        self,
        msg: Msg,
        headers: Mapping[str, str],
        access: str,
        body: bytes,
        bff_rid: str,
        echo_rid: str,
        deadline: float,
    ) -> None:
        try:
            assert self._grant is not None
            parent = telemetry.extract(headers)
            with telemetry.span(
                "boundary.grant",
                kind=SpanKind.CLIENT,
                context=parent,
                attributes={"service": self.service, "bff_request_id": bff_rid},
            ) as gsp:
                got = await self._grant.ask(
                    access, msg.subject, msg.reply, body, (), bff_rid, deadline
                )
                if isinstance(got, Unavailable | GrantRefused):
                    telemetry.fail(
                        gsp, "unavailable" if isinstance(got, Unavailable) else "forbidden"
                    )
            del access  # held only for the grant call
            if isinstance(got, Unavailable):
                self.refusals["grant_unavailable"] += 1
                await self._error(msg, "unavailable", request_id=echo_rid, bff_direct=True)
                return
            if isinstance(got, GrantRefused):
                self.refusals["grant_refused"] += 1
                await self._error(
                    msg, envelope.GRANT_REFUSED + got.code, request_id=echo_rid, bff_direct=True
                )
                return
            route = self._route(msg.subject)
            decoder = route[2] if route is not None else _never
            with telemetry.verifying(self.service, parent) as vsp:
                r = self.verifier.verify_granted(
                    got, msg.subject, msg.reply, headers, body, decoder
                )
                if isinstance(r, Refusal):
                    telemetry.fail(vsp, r.code)
            if isinstance(r, Refusal):
                if r.reply:
                    await self._error(msg, r.code, request_id=echo_rid, bff_direct=True)
                return
            await self.run_handler(msg, r, bff_direct=True)
        finally:
            self._sem.release()

    async def _try_acquire(self) -> bool:
        """A slot now, or False: never queues (spec §9.1, overloaded at once).
        acquire() on an unlocked semaphore takes it without yielding, so
        nothing can take the slot between the check and the acquire."""
        if self._sem.locked():
            return False
        await self._sem.acquire()
        return True

    def _spawn(self, coro: Awaitable[None]) -> None:
        async def guarded() -> None:
            try:
                await coro
            except asyncio.CancelledError:
                raise
            except Exception as e:
                self.refusals["internal"] += 1
                log.error("worker task failed service=%s error=%s", self.service, type(e).__name__)

        t = asyncio.get_running_loop().create_task(guarded())
        self._tasks.add(t)
        t.add_done_callback(self._tasks.discard)

    async def _run_and_release(self, msg: Msg, v: Verified, *, bff_direct: bool) -> None:
        try:
            await self.run_handler(msg, v, bff_direct=bff_direct)
        finally:
            self._sem.release()

    async def run_handler(self, msg: Msg, v: Verified, *, bff_direct: bool) -> None:
        """The handler for v, bounded by the token's exp: not started after
        it, abandoned at it. Answers once, inside the span "handle <op>",
        a child of the caller's span (a request/reply call is short; only
        jobs are linked, not parented)."""
        raw = msg.headers if isinstance(msg.headers, Mapping) else None
        with telemetry.span(
            f"handle {v.operation.name}",
            kind=SpanKind.SERVER,
            context=telemetry.extract(raw),
            attributes={
                "operation": v.operation.name,
                "service": self.service,
                "request_id": v.rid,
                "hospital_id": v.hid,
            },
        ) as sp:
            await self._run_handler(msg, v, bff_direct=bff_direct)
            if not telemetry.failed(sp):
                telemetry.ok(sp)

    async def _run_handler(self, msg: Msg, v: Verified, *, bff_direct: bool) -> None:
        route = self._handlers.get(v.operation.name)
        if route is None:  # verify only passes a known operation; belt and braces
            await self._error(msg, "internal", request_id=v.rid, bff_direct=bff_direct)
            return
        op, handler, _ = route
        remaining = v.exp - self._clock()
        if remaining <= 0:
            self.refusals["deadline"] += 1
            await self._error(msg, "deadline_exceeded", request_id=v.rid, bff_direct=bff_direct)
            return
        started = time.monotonic()
        try:
            async with asyncio.timeout(remaining):
                meta, out = await handler(v, v.message)
        except TimeoutError:
            self.refusals["deadline"] += 1
            log.warning(
                "worker deadline service=%s op=%s rid=%s", self.service, op.op, _safe(v.rid)
            )
            await self._error(msg, "deadline_exceeded", request_id=v.rid, bff_direct=bff_direct)
            return
        except Exception as e:
            self.refusals["internal"] += 1
            log.error(
                "worker handler failed service=%s op=%s rid=%s error=%s",
                self.service,
                op.op,
                _safe(v.rid),
                type(e).__name__,
            )
            await self._error(msg, "internal", request_id=v.rid, bff_direct=bff_direct)
            return
        if (
            not isinstance(meta, envelope_pb2.ResponseMeta)
            or not isinstance(out, Message)
            or out.DESCRIPTOR.full_name != op.response_schema
        ):
            self.refusals["internal"] += 1
            log.error(
                "worker handler returned the wrong type service=%s op=%s", self.service, op.op
            )
            await self._error(msg, "internal", request_id=v.rid, bff_direct=bff_direct)
            return
        meta.timings_ms["handler"] = (time.monotonic() - started) * 1000
        h, b = envelope.reply_ok(meta, out, request_id=v.rid)
        await self._send(msg, h, b)

    # --- jobs (JetStream) -----------------------------------------------------------

    async def _start_jobs(self) -> None:
        """Bind the service's durable pull consumer (core made it) and start
        the loop that feeds the job handlers."""
        self._js = self.connection.jetstream()
        self._psub = await self._js.pull_subscribe_bind(durable=self.service, stream=JOBS_STREAM)
        self._job_loop = asyncio.get_running_loop().create_task(self._consume())

    async def _stop_jobs(self) -> None:
        loop, self._job_loop = self._job_loop, None
        if loop is not None:
            loop.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await loop
        psub, self._psub = self._psub, None
        if psub is not None:
            with contextlib.suppress(Exception):
                await psub.unsubscribe()

    async def _ready_for_jobs(self) -> bool:
        check = self._jobs_ready
        if check is None:
            return False
        try:
            r = check()
            if inspect.isawaitable(r):
                r = await r
        except Exception as e:
            log.warning("job readiness failed service=%s error=%s", self.service, type(e).__name__)
            return False
        return r is True

    async def _consume(self) -> None:
        """One job at a time per free slot, only while ready. A message is
        fetched only with a slot in hand, so JetStream (not this process)
        holds the queue, and max_ack_pending bounds what every copy has."""
        not_ready = False
        while not self.draining:
            if not await self._ready_for_jobs():
                if not not_ready:
                    self.refusals["jobs_not_ready"] += 1
                    log.warning("jobs paused service=%s: not ready", self.service)
                not_ready = True
                await asyncio.sleep(1.0)
                continue
            if not_ready:
                log.info("jobs resumed service=%s", self.service)
                not_ready = False
            await self._job_sem.acquire()
            try:
                msgs = await self._psub.fetch(1, timeout=self.fetch_timeout)
            except asyncio.CancelledError:
                self._job_sem.release()
                raise
            except nats_errors.TimeoutError:
                self._job_sem.release()
                continue
            except Exception as e:
                self._job_sem.release()
                log.warning("job fetch failed service=%s error=%s", self.service, type(e).__name__)
                await asyncio.sleep(1.0)
                continue
            if not msgs:
                self._job_sem.release()
                continue
            for i, m in enumerate(msgs):
                if i:  # fetch(1) gives one; any extra needs its own slot
                    await self._job_sem.acquire()
                self.received += 1
                self._spawn(self._job(m, Slot(self._job_sem)))

    async def _job(self, msg: Msg, slot: Slot) -> None:
        try:
            await self._run_job(msg, slot)
        finally:
            slot.release()

    def _job_route(self, subject: str) -> tuple[Operation, JobHandler, Decoder] | None:
        op = self._ops.for_subject(subject)
        if op is None or op.service != self.service or op.kind != "job":
            return None
        return self._jobs.get(op.op)

    async def _run_job(self, msg: Msg, slot: Slot) -> None:
        """One delivery, inside the span "handle <op>". A job is long and
        asynchronous, so the span starts a new trace and *links* to the
        producer span in the message's traceparent instead of hanging under
        it (spec §10.3)."""
        route = self._job_route(msg.subject)
        name = f"handle {route[0].name}" if route is not None else "handle unknown"
        hdrs = msg.headers if isinstance(msg.headers, Mapping) else None
        with telemetry.span(
            name,
            kind=SpanKind.CONSUMER,
            context=Context(),
            links=telemetry.links_from(hdrs),
            attributes={"service": self.service},
        ) as sp:
            await self._run_job_checked(msg, slot)
            if not telemetry.failed(sp):
                telemetry.ok(sp)

    async def _run_job_checked(self, msg: Msg, slot: Slot) -> None:
        raw = msg.headers
        if raw is not None and not isinstance(raw, Headers):
            await self._refuse_job(msg, "headers", "unauthorized")  # not parsed by WorkerClient
            return
        headers: Mapping[str, str] = raw if raw is not None else {}
        if raw is not None and raw.ambiguous:
            await self._refuse_job(msg, "ambiguous_headers", "unauthorized")
            return
        if headers.get(envelope.AUTHORIZATION) is not None:
            await self._refuse_job(msg, "authorization", "unauthorized")  # jobs come from core only
            return
        route = self._job_route(msg.subject)
        decoder = route[2] if route is not None else _never
        with telemetry.verifying(self.service) as vsp:
            r = self.verifier.verify_job(msg.subject, headers, msg.data, decoder)
            if isinstance(r, Refusal):
                telemetry.fail(vsp, r.code)
        if isinstance(r, Refusal):
            await self._refuse_job(msg, r.reason, r.code, alert=r.alert)
            return
        telemetry.annotate(hospital_id=r.hid, request_id=r.rid, operation=r.operation.name)
        if route is None or route[0].name != r.operation.name:  # verify passed a known op
            await self._refuse_job(msg, "schema", "internal")
            return
        await route[1](r, msg, JobContext(slot=slot, dead_letter=self.publish_dead_letter))

    async def _refuse_job(self, msg: Msg, reason: str, code: str, *, alert: bool = True) -> None:
        """A job that never verified: count, log (no alert for one that only
        expired in the queue, F6), a dead letter for core to mark the row,
        and Term. No database: there is no verified hospital."""
        self.refusals[reason] += 1
        telemetry.current_fail(code)
        log.log(
            logging.WARNING if alert else logging.INFO,
            "worker refused job service=%s subject=%s reason=%s code=%s",
            self.service,
            _safe(msg.subject),
            reason,
            code,
        )
        seg = msg.subject.split(".")
        hid = seg[2] if len(seg) == 4 and (seg[2] == "global" or _UUID.fullmatch(seg[2])) else ""
        kind = seg[3] if len(seg) == 4 and seg[3] in self._jobs else "unknown"
        headers = msg.headers if isinstance(msg.headers, Mapping) else {}
        job_id = headers.get(NATS_MSG_ID, "")
        try:
            attempts = int(msg.metadata.num_delivered)
        except Exception:
            attempts = 0
        await self.publish_dead_letter(
            envelope_pb2.DeadLetter(
                job_id=job_id if _UUID.fullmatch(job_id or "") else "",
                type=kind,
                subject_hospital=hid,
                attempts=max(0, attempts),
                code=code,
                reason=reason,
                kid=_token_kid(headers.get(envelope.TOKEN)),
            )
        )
        try:
            await msg.term()
        except Exception as e:
            log.warning("job term failed service=%s error=%s", self.service, type(e).__name__)

    async def publish_dead_letter(self, d: envelope_pb2.DeadLetter) -> None:
        """One dead letter on dead.v1.<hospital>.<type> (the DEAD stream).
        It holds ids, the type, attempts, a code and a reason only. Never
        raises: a lost letter leaves the row to core's deadline sweeper."""
        hid = d.subject_hospital if _TOKEN.fullmatch(d.subject_hospital) else "unknown"
        kind = d.type if _TOKEN.fullmatch(d.type) else "unknown"
        try:
            js = self._js if self._js is not None else self.connection.jetstream()
            await js.publish(
                f"dead.v1.{hid}.{kind}",
                d.SerializeToString(deterministic=True),
                timeout=2.0,
                stream=DEAD_STREAM,
            )
        except Exception as e:
            log.error("dead letter failed service=%s error=%s", self.service, type(e).__name__)
            return
        log.info(
            "dead letter service=%s type=%s job_id=%s code=%s attempts=%d",
            self.service,
            kind,
            d.job_id or "-",
            d.code,
            d.attempts,
        )


def _token_kid(token: str | None) -> str:
    """The kid in a refused token's header, for the dead letter, if it is a
    well-formed key id; else empty. Read after the refusal: it decides
    nothing."""
    if not token or len(token) > 4096:
        return ""
    try:
        h = json.loads(_b64.decode(token.split(".", 1)[0]))
    except Exception:
        return ""
    kid = h.get("kid") if isinstance(h, dict) else None
    return kid if isinstance(kid, str) and _KID.fullmatch(kid) else ""


def build_verifier(settings: WorkerSettings) -> tuple[Verifier, TrustedKeys]:
    """The verifier from the settings' files; refuses to start on a bad one."""
    trusted = TrustedKeys.load(settings.trusted_keys_path)
    ops = Operations.load(settings.operations_path)
    clock = time.time
    return Verifier(trusted, settings.service, ops, JtiSet(clock), clock), trusted


def service_handlers(
    service: str, database: Database | None = None, *, models_ready: bool = False
) -> dict[str, Handler]:
    from intelligence.boundary.services import echo, pipeline

    if service == "echo":
        return dict(echo.HANDLERS)
    if service == "pipeline":

        def models() -> bool:
            return models_ready

        if database is None:
            return pipeline.handlers(models_ready=models)
        return pipeline.handlers(database_ready=database.ready, models_ready=models)
    raise ValueError("unknown service")


async def open_database(settings: WorkerSettings) -> Database | None:
    """The pipeline's database, connected and its login checked (spec
    §8.2), or None without INTELLIGENCE_DB_URL (health then reports
    database_ready false). Refuses to start on a login RLS can't hold."""
    if settings.service != "pipeline" or settings.db_url is None:
        return None
    from intelligence.boundary.db import Database

    database = Database(
        settings.db_url,
        user=settings.db_user,
        password=settings.db_password,
        max_size=max(2, settings.max_concurrency),
    )
    await database.open()
    return database


async def run_service(settings: WorkerSettings) -> None:
    """`intelligence worker`: serve settings.service until SIGTERM. The
    pipeline also runs run_query jobs, but only with a database (T10 note):
    without one it takes no job and health says database_ready false."""
    verifier, trusted = build_verifier(settings)
    # Off unless OTEL_EXPORTER_OTLP_ENDPOINT is set; a bad endpoint turns it
    # off with a warning, never keeps the worker from starting.
    try:
        stop_telemetry = telemetry.setup(
            service=settings.service, allow_plaintext=settings.development or None
        )
    except ValueError:
        log.warning("telemetry is off: OTEL_EXPORTER_OTLP_ENDPOINT is not usable")
        stop_telemetry = None
    database = await open_database(settings)
    runner = None
    try:
        jobs: dict[str, JobHandler] | None = None
        if settings.service == "pipeline":
            from intelligence.boundary.services import pipeline

            jobs, runner = pipeline.job_handlers(settings, database, clock=verifier.clock)
            if database is None:
                log.warning("no INTELLIGENCE_DB_URL: run_query jobs are not taken")
        w = Worker(
            settings,
            service_handlers(settings.service, database, models_ready=runner is not None),
            verifier,
            jobs=jobs,
            jobs_ready=runner.ready if runner is not None else None,
        )
        await w.start()
        await w.run(trusted)
    finally:
        if runner is not None:
            runner.close()
        if database is not None:
            await database.close()
        if stop_telemetry is not None:
            stop_telemetry()
    if w.insecure:
        raise ConnectionError("the NATS connection lost TLS")
