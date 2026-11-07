"""CloudModelClient: local retrieval and verification around a gated,
scanned, budgeted cloud draft (design sections 3 to 11; plan Task 9).

generate(question, context, patient_id, chunks, *, call), in this order:

1. call is None, or patient_id is not None: egress_blocked, nothing done.
2. The per-job orchestrator (the local builder), and local retrieval:
   retriever.retrieve(question, top_k=5, patient_id=None). Each result must
   be a sealed shared chunk (gate.shared_chunk_of); anything else refuses.
3. CloudContext.from_masked (the dropped keys counted) and CloudPrompt.
4. A dry render: the adapter builds its request over a transport that
   captures the body and raises, so nothing goes anywhere. The scan runs
   on those exact bytes; a hit refuses before any budget is reserved.
5. Attempts: reserve (one token per UTF-8 byte of the body plus the output
   cap, ruling C7), then the real send through a per-attempt httpx2 client
   whose only request hook is ScanHook, then settle in `finally`. The
   hook's verdict wins over whatever the SDK raised (Review Focus 2).
   Retries follow retry.classify and RetryPolicy.

Every outcome but ok gives Generation(available=False, failed_agents=
("CCA",)) with the outcome code and no versions, so the job escalates with
no score and the decision records no cca_route (departure 15). There is no
fallback to local or to another provider.

No per-request state lives on the client (K10): the identifiers, the
budget and the deadline arrive in `call`. Nothing here logs text: ids,
codes and numbers only.
"""

from __future__ import annotations

import logging
import math
import ssl
import threading
import time
from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass, field, replace
from typing import TYPE_CHECKING, Any, Final, Protocol

import httpx2

from intelligence.boundary import telemetry
from intelligence.boundary.cloud import retry, scan
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.budget import (
    MAX_OUTPUT_TOKENS,
    BudgetPort,
    Reservation,
    estimate_tokens,
)
from intelligence.boundary.cloud.gate import (
    MAX_CHUNKS,
    CloudContext,
    CloudPrompt,
    EgressRefused,
    MaskedText,
    SharedChunk,
    TaggingMasker,
    shared_chunk_of,
)
from intelligence.boundary.cloud.reply import Answer, validate_answer
from intelligence.boundary.cloud.retry import RETRY, RetryPolicy
from intelligence.boundary.cloud.scan import Identifiers, ScanHook, Shape, scan_body
from intelligence.boundary.cloud.settings import NO_CA_BUNDLE, openssl_ca_paths
from intelligence.boundary.config import ConfigError, is_development
from intelligence.boundary.models import (
    _SAFE_VERSION,
    Builder,
    Generation,
    Verification,
    _escalated,
    _verify_with,
)

if TYPE_CHECKING:
    from intelligence.boundary.cloud.settings import CloudSettings, ProviderSettings
    from intelligence.knowledge_base.kb_builder import KBChunk

log = logging.getLogger("intelligence.boundary.cloud.client")

TOP_K: Final = MAX_CHUNKS
# A reported use more than this far above the reservation flags the
# provider for an operator (mhai_cloud_alerts_total{kind="usage_overrun"}).
OVERRUN_ALERT_PERCENT: Final = 10
CONNECT_TIMEOUT: Final = 5.0
POOL_TIMEOUT: Final = 5.0

# Every refusal's rule code, for a metric label if one is ever wanted. A
# rule code is never written to a fact or an audit field: every one of
# them is the outcome egress_blocked.
REFUSAL_RULES: Final = (*scan.RULES, "gate", "render_failed")


# --- the per-call argument and the adapter's reply -----------------------------------


@dataclass(frozen=True, repr=False)
class CloudCall:
    """One generation's per-job state: the patient's identifiers (for the
    scan), the hospital's budget port, and the stage's deadline (a
    time.monotonic() value). The repr holds no value."""

    identifiers: Identifiers
    budget: BudgetPort
    stage_deadline: float

    def __post_init__(self) -> None:
        if type(self.identifiers) is not Identifiers:
            raise TypeError("CloudCall takes Identifiers")
        d = self.stage_deadline
        if not isinstance(d, int | float) or isinstance(d, bool) or math.isnan(d):
            raise TypeError("CloudCall takes a monotonic deadline")

    def __repr__(self) -> str:
        return "CloudCall(<per-job>)"


@dataclass(frozen=True)
class CloudReply:
    """What an adapter's call gives back. outcome is retry.RETRY or one of
    vocab.OUTCOMES; answer is set only for ok and insufficient. status is
    the provider's HTTP status when there was one (for the auth alert)."""

    answer: Answer | None = field(repr=False)
    model_id: str
    tokens_in: int | None
    tokens_out: int | None
    outcome: str
    retry_after: float | None = None
    status: int | None = None


class Adapter(Protocol):
    """One provider's request builder and reply parser (Tasks 11 to 13)."""

    name: str
    shape: Shape

    def make_sdk(self, http_client: httpx2.Client, key: ApiKey, base_url: str) -> Any: ...

    def call(self, sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply: ...


# --- transports ----------------------------------------------------------------------


class _SharedTransport(httpx2.BaseTransport):
    """The one real transport, shared by every attempt's client. Closing an
    attempt's client must not close the pool, so close() does nothing here;
    the real transport is closed once, by its owner."""

    def __init__(self, inner: httpx2.BaseTransport) -> None:
        self._inner = inner

    def handle_request(self, request: httpx2.Request) -> httpx2.Response:
        return self._inner.handle_request(request)

    def close(self) -> None:
        return None


class _DeadlineStream(httpx2.SyncByteStream):
    """A response body that stops at the attempt's deadline: the per-read
    timeout bounds one read, this bounds the whole body, so a reply that
    drips in a byte at a time still ends at the stage deadline."""

    def __init__(
        self,
        inner: httpx2.SyncByteStream,
        deadline: float,
        clock: Callable[[], float],
        request: httpx2.Request,
    ) -> None:
        self._inner = inner
        self._deadline = deadline
        self._clock = clock
        self._request = request

    def __iter__(self) -> Iterator[bytes]:
        for chunk in self._inner:
            if self._clock() >= self._deadline:
                raise httpx2.ReadTimeout("stage deadline", request=self._request)
            yield chunk

    def close(self) -> None:
        self._inner.close()


class _DeadlineTransport(httpx2.BaseTransport):
    """One attempt's overall deadline, over the whole request and the whole
    body, on top of the per-read timeouts. A single read that stalls is
    still bounded by the read timeout, which is at most the time left."""

    def __init__(
        self, inner: httpx2.BaseTransport, deadline: float, clock: Callable[[], float]
    ) -> None:
        self._inner = inner
        self._deadline = deadline
        self._clock = clock

    def handle_request(self, request: httpx2.Request) -> httpx2.Response:
        if self._clock() >= self._deadline:
            raise httpx2.PoolTimeout("stage deadline", request=request)
        response = self._inner.handle_request(request)
        if self._clock() >= self._deadline:
            response.close()
            raise httpx2.ReadTimeout("stage deadline", request=request)
        stream = response.stream
        if not isinstance(stream, httpx2.SyncByteStream):
            response.close()
            raise httpx2.ReadTimeout("not a sync stream", request=request)
        response.stream = _DeadlineStream(stream, self._deadline, self._clock, request)
        return response

    def close(self) -> None:
        self._inner.close()


class _Rendered(Exception):
    """The dry render's stop: the body was captured, nothing was sent."""


class _RenderTransport(httpx2.BaseTransport):
    """Capture the request body, then abort. Nothing goes anywhere."""

    def __init__(self) -> None:
        self.body: bytes | None = None

    def handle_request(self, request: httpx2.Request) -> httpx2.Response:
        if self.body is None:
            try:
                self.body = bytes(request.read())
            except Exception:
                self.body = None
        raise _Rendered


def tls_context() -> ssl.SSLContext:
    """TLS for the providers: hostname checked, a certificate required, TLS
    1.2 or later, trusting the CA paths OpenSSL was built with, never
    SSL_CERT_FILE or SSL_CERT_DIR (which ssl.create_default_context and
    httpx2's default context both read). No CA there refuses (ConfigError),
    as start-up does (settings.openssl_ca_paths)."""
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED
    cafile, capath = openssl_ca_paths()
    if cafile is None and capath is None:
        raise ConfigError(NO_CA_BUNDLE)
    ctx.load_verify_locations(cafile=cafile, capath=capath)
    return ctx


# --- the client ----------------------------------------------------------------------


def _failed(outcome: str, egress: str = "none") -> Generation:
    return Generation(
        failed_agents=("CCA",),
        available=False,
        egress=egress,
        cloud_outcome=retry.final_outcome(outcome),
    )


def _tokens(v: object) -> int | None:
    return v if type(v) is int and v >= 0 else None


class CloudModelClient:
    """The cloud route's ModelClient for one provider."""

    def __init__(
        self,
        provider: ProviderSettings,
        adapter: Adapter,
        build: Builder,
        local_versions: Mapping[str, str],
        transport: httpx2.BaseTransport,
        *,
        policy: RetryPolicy | None = None,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        if adapter.name != provider.name:
            raise ConfigError("the adapter is not this provider's")
        if type(adapter.shape) is not Shape:
            raise ConfigError("the adapter has no Shape")
        self._provider = provider
        self._adapter = adapter
        self._build = build
        self._versions = {k: v for k, v in local_versions.items() if k != "cca"}
        self._transport = transport
        self._policy = policy or RetryPolicy()
        self._clock = clock
        self._sleep = sleep
        self._masker = TaggingMasker()
        self._close_lock = threading.Lock()

    @property
    def provider(self) -> str:
        return self._provider.name

    @property
    def transport(self) -> httpx2.BaseTransport:
        return self._transport

    def __repr__(self) -> str:
        return f"CloudModelClient(provider={self._provider.name!r})"

    def mask(self, text: str) -> MaskedText:
        return self._masker.mask(text)

    def model_versions(self) -> dict[str, str]:
        return dict(self._versions)

    def verify(self, generation: Generation) -> Verification:
        if not generation.available:
            return _escalated(generation.failed_agents or ("CCA",))
        return _verify_with(generation.handle, generation)

    def close(self) -> None:
        with self._close_lock:
            self._transport.close()

    # --- generate -------------------------------------------------------------------

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[KBChunk],
        *,
        call: CloudCall | None = None,
    ) -> Generation:
        if type(call) is not CloudCall or patient_id is not None:
            return _failed("egress_blocked")
        if call.stage_deadline - self._clock() <= 0:
            return _failed("timeout")
        started = self._clock()
        try:
            orch = self._build(list(chunks))
            retriever = orch.rag_cca.retriever
            results = retriever.retrieve(str(question), top_k=TOP_K, patient_id=None)
        except Exception as e:
            # The local models failed before anything could be sent.
            log.warning("cloud.local_retrieval_failed error=%s", type(e).__name__)
            return _failed("unavailable")
        retrieval_ms = (self._clock() - started) * 1000
        try:
            shared = self._shared(results)
            context, dropped = CloudContext.from_masked(patient_context)
            telemetry.record_cloud_context_dropped(dropped)
            if not isinstance(question, MaskedText):
                raise EgressRefused("gate")
            prompt = CloudPrompt(question, context, shared, self._masker)
        except EgressRefused:
            return _failed("egress_blocked")
        sent_ids = tuple(c.chunk_id for c in shared)
        sent = frozenset(sent_ids)

        body = self._render(prompt, call)
        if body is None:
            return _failed("egress_blocked")  # render_failed
        if scan_body(body, call.identifiers, self._adapter.shape) is not None:
            return _failed("egress_blocked")
        try:
            estimate = estimate_tokens(len(body), max_output=MAX_OUTPUT_TOKENS)
        except ValueError:
            return _failed("budget_exhausted")

        g0 = self._clock()
        outcome, reply, egress = self._attempts(prompt, call, estimate, sent)
        if outcome != "ok" or reply is None or reply.answer is None:
            if outcome == "ok":
                outcome = "malformed"
            return _failed(outcome, egress)
        answer = reply.answer
        model = self._provider.model
        p = self._provider.name
        return Generation(
            question=str(question),
            response=answer.answer,
            cited_chunk_ids=answer.citations,
            retrieved_chunk_ids=sent_ids,
            model_used=f"{p}/{model}",
            failed_agents=(),
            available=True,
            retrieval_ms=retrieval_ms,
            generation_ms=(self._clock() - g0) * 1000,
            patient_id=None,
            handle=orch,
            versions={"cca": f"{p}/{model}@{reply.model_id}", "cca_route": "cloud"},
            egress=p,
            cloud_outcome="ok",
            tokens_in=reply.tokens_in or 0,
            tokens_out=reply.tokens_out or 0,
        )

    def _shared(self, results: Any) -> tuple[SharedChunk, ...]:
        if not isinstance(results, list | tuple):
            raise EgressRefused("gate")
        out: list[SharedChunk] = []
        for r in list(results)[:TOP_K]:
            sc = shared_chunk_of(getattr(r, "chunk", None))
            if all(sc.chunk_id != o.chunk_id for o in out):
                out.append(sc)
        return tuple(out)

    def _http(
        self, transport: httpx2.BaseTransport, left: float, hooks: list[Any]
    ) -> httpx2.Client:
        return httpx2.Client(
            transport=transport,
            event_hooks={"request": hooks, "response": []},
            follow_redirects=False,
            trust_env=False,
            timeout=httpx2.Timeout(
                connect=CONNECT_TIMEOUT, read=left, write=left, pool=POOL_TIMEOUT
            ),
        )

    def _render(self, prompt: CloudPrompt, call: CloudCall) -> bytes | None:
        """The exact body the adapter would send, captured; nothing is sent."""
        render = _RenderTransport()
        left = max(0.001, call.stage_deadline - self._clock())
        try:
            with self._http(render, left, []) as http:
                sdk = self._adapter.make_sdk(http, self._provider.key, self._provider.base_url)
                self._adapter.call(sdk, prompt, self._provider.model, left)
        except Exception:
            pass  # _Rendered, as raised or as the SDK wraps it: the body decides
        return render.body

    def _attempts(
        self, prompt: CloudPrompt, call: CloudCall, estimate: int, sent_ids: frozenset[str]
    ) -> tuple[str, CloudReply | None, str]:
        """(outcome, the ok reply or None, egress)."""
        p = self._provider.name
        sent_any = False
        attempt = 0
        while True:
            attempt += 1
            left = call.stage_deadline - self._clock()
            if left <= 0:
                return "timeout", None, p if sent_any else "none"
            try:
                reservation = call.budget.reserve(p, estimate)
            except Exception:
                reservation = None
            if type(reservation) is not Reservation:
                return "budget_exhausted", None, p if sent_any else "none"
            hook: ScanHook | None = None
            reply: CloudReply | None = None
            error: BaseException | None = None
            try:
                # Built inside the try: if the hook can't be built, the
                # reservation is still settled (as 0: nothing was sent).
                hook = ScanHook(call.identifiers, self._adapter.shape)
                transport = _DeadlineTransport(
                    _SharedTransport(self._transport), call.stage_deadline, self._clock
                )
                with self._http(transport, left, [hook]) as http:
                    sdk = self._adapter.make_sdk(http, self._provider.key, self._provider.base_url)
                    reply = self._adapter.call(sdk, prompt, self._provider.model, left)
            except Exception as e:
                error = e
            finally:
                self._settle(call.budget, reservation, hook, reply)
            if hook is None:
                # No scan hook, so nothing could be sent: refuse, never retry.
                return "egress_blocked", None, p if sent_any else "none"
            sent_any = sent_any or hook.sent
            egress = p if sent_any else "none"

            outcome, retry_after, status, checked = self._outcome(hook, error, reply, sent_ids)
            if outcome == "auth_failed":
                log.error("cloud.auth_failed provider=%s status=%s", p, status or "-")
                telemetry.record_cloud_alert(p, "auth_failed")
            if outcome != RETRY:
                if outcome == "ok" and reply is not None and checked is not None:
                    return outcome, replace(reply, answer=checked), egress
                return ("malformed" if outcome == "ok" else outcome), None, egress
            left = call.stage_deadline - self._clock()
            delay = self._policy.next_delay(attempt, retry_after, left)
            if delay is None:
                timed_out = call.stage_deadline - self._clock() <= 0
                return ("timeout" if timed_out else "unavailable"), None, egress
            self._sleep(delay)

    def _outcome(
        self,
        hook: ScanHook,
        error: BaseException | None,
        reply: CloudReply | None,
        sent_ids: frozenset[str],
    ) -> tuple[str, float | None, int | None, Answer | None]:
        """(RETRY or a final outcome, Retry-After, status, the re-checked
        answer when ok) for one attempt. The hook's verdict wins over
        anything the SDK raised or returned."""
        if hook.verdict is not None:
            return "egress_blocked", None, None, None
        if error is not None:
            status = getattr(error, "status_code", None)
            return retry.classify(error), None, status if type(status) is int else None, None
        if type(reply) is not CloudReply:
            return "malformed", None, None, None
        outcome = retry.classify(reply.outcome)
        status = reply.status if type(reply.status) is int else None
        if outcome != "ok":
            return outcome, reply.retry_after, status, None
        a = reply.answer
        if type(a) is not Answer or not isinstance(reply.model_id, str):
            return "malformed", None, status, None
        version = f"{self._provider.name}/{self._provider.model}@{reply.model_id}"
        if not reply.model_id or not _SAFE_VERSION.fullmatch(version):
            return "malformed", None, status, None
        # Never trust the adapter's Answer as given (it may be hand-built or
        # made with model_construct): every rule runs again here, against
        # this job's sent ids, and only the re-checked answer is used.
        try:
            checked = validate_answer(
                {
                    "answer": a.answer,
                    "citations": list(a.citations),
                    "insufficient_evidence": a.insufficient_evidence,
                },
                sent_ids,
            )
        except Exception:
            return "malformed", None, status, None
        if not set(checked.citations) <= sent_ids:
            return "malformed", None, status, None
        if checked.insufficient_evidence:
            return "insufficient", None, status, None
        return "ok", None, status, checked

    def _settle(
        self,
        budget: BudgetPort,
        reservation: Reservation,
        hook: ScanHook | None,
        reply: CloudReply | None,
    ) -> None:
        """Settle once, whatever happened: nothing sent is 0; a reply with
        both counts is their sum; anything else is all of the reservation."""
        used: int | None
        if hook is None or not hook.sent:
            used = 0
        elif reply is not None and type(reply) is CloudReply:
            ti, to = _tokens(reply.tokens_in), _tokens(reply.tokens_out)
            used = ti + to if ti is not None and to is not None else None
        else:
            used = None
        if used is not None and reply is not None and used > reservation.tokens:
            self._overrun(reservation, used, reply.outcome)
        try:
            budget.settle(reservation, used)
        except Exception as e:
            log.warning("cloud.settle_failed error=%s", type(e).__name__)
            telemetry.record_cloud_settle_failure()

    def _overrun(self, reservation: Reservation, used: int, outcome: str) -> None:
        """Review I1: core settles at most the reservation (00018), so a
        reply that reports more is logged (outcome code and counts only)
        and counted; more than OVERRUN_ALERT_PERCENT over the reservation, the provider is
        flagged for an operator, as an alert."""
        p = self._provider.name
        over = used - reservation.tokens
        alert = over * 100 > reservation.tokens * OVERRUN_ALERT_PERCENT
        log.log(
            logging.ERROR if alert else logging.WARNING,
            "cloud.usage_overrun provider=%s outcome=%s reserved=%d used=%d over=%d",
            p,
            retry.final_outcome(outcome),
            reservation.tokens,
            used,
            over,
        )
        telemetry.record_cloud_usage_overrun(p)
        if alert:
            telemetry.record_cloud_alert(p, "usage_overrun")


# --- building the clients -------------------------------------------------------------

# Adapters by provider name (plan Tasks 11 to 13), each made for the
# provider's configured model: the model is a fixed leaf of the adapter's
# Shape, so a request for any other model is refused by the scan. Imported
# when called: the adapter modules import this one.


def _anthropic(model: str) -> Adapter:
    from intelligence.boundary.cloud.anthropic_adapter import AnthropicAdapter

    return AnthropicAdapter(model)


def _openai(model: str) -> Adapter:
    from intelligence.boundary.cloud.openai_adapter import OpenAIAdapter

    return OpenAIAdapter(model)


def _xai(model: str) -> Adapter:
    # The zero-retention header is relaxed in development only (APP_ENV,
    # read here: unset is production).
    from intelligence.boundary.cloud.xai_adapter import XaiAdapter

    return XaiAdapter(model, development=is_development())


ADAPTERS: dict[str, Callable[[str], Adapter]] = {
    "anthropic": _anthropic,
    "openai": _openai,
    "xai": _xai,
}


def build_cloud_clients(
    settings: CloudSettings, build: Builder, local_versions: Mapping[str, str]
) -> dict[str, CloudModelClient]:
    """One client per enabled provider, over one shared transport: the
    egress proxy, the explicit TLS context, no environment, no retries of
    its own. An enabled provider with no adapter refuses start-up."""
    missing = [p.name for p in settings.providers if p.name not in ADAPTERS]
    if missing:
        raise ConfigError(f"no adapter for cloud provider(s): {', '.join(missing)}")
    if not settings.providers:
        return {}
    transport = httpx2.HTTPTransport(
        proxy=settings.proxy_url, verify=tls_context(), trust_env=False, retries=0
    )
    return {
        p.name: CloudModelClient(p, ADAPTERS[p.name](p.model), build, local_versions, transport)
        for p in settings.providers
    }
