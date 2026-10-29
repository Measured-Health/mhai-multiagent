"""OpenTelemetry for the workers (spec §10.3, plan 3 task 14): traces and
metrics, off unless an exporter endpoint is configured.

Nothing here touches the OpenTelemetry globals. ``setup`` installs a
provider in this module's own holder, so the code that records a span or a
metric works the same with or without a collector (no-op tracer, no
instruments), and a test can install an in-memory exporter and take it
down again.

What may leave the process is decided by one filtering ``SpanExporter``
(``FilterExporter``), not a span processor: a processor's ``on_end`` gets a
read-only span and can't remove an attribute. The filter keeps

  * attributes whose key is on ``ALLOWED`` and whose value has the shape
    that key promises (an id, a code, a number); anything else is dropped,
    not redacted, so free text can't ride in under an allowed key;
  * the span name only if it starts with one of the fixed prefixes this
    code uses (names are never built from input);
  * events named "exception", reduced to ``exception.type``;
  * links as span contexts only; the status code with no description.

``Authorization`` and ``Mhai-Request-Token`` are never attributes. Trace
headers (traceparent, tracestate) are untrusted: a malformed one starts a
new trace, and baggage is neither read nor written.
"""

from __future__ import annotations

import os
import re
import time
from collections import Counter
from collections.abc import Callable, Iterator, Mapping, MutableMapping, Sequence
from contextlib import contextmanager
from typing import Any
from urllib.parse import urlparse

from opentelemetry import trace
from opentelemetry.context import Context
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import MetricReader, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import Event, ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import (
    BatchSpanProcessor,
    SimpleSpanProcessor,
    SpanExporter,
    SpanExportResult,
)
from opentelemetry.trace import Link, Span, SpanKind, Status, StatusCode
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

SCOPE = "mhai/intelligence"

# --- the allow-list ------------------------------------------------------------

_ID = re.compile(r"[A-Za-z0-9_.:\-]{1,128}")
_UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
_WORD = re.compile(r"[a-z][a-z0-9_.]{0,63}")
_MODEL = re.compile(r"[A-Za-z0-9_.:/\-]{1,64}")
_EXC_TYPE = re.compile(r"[A-Za-z_][A-Za-z0-9_.]{0,79}")
_NAME = re.compile(r"[A-Za-z0-9_.:/{}<> \-]{1,160}")

# key -> the shape its value must have.
ALLOWED: dict[str, str] = {
    "request_id": "id",
    "hospital_id": "uuid",
    "query_id": "uuid",
    "job_id": "uuid",
    "operation": "word",
    "service": "word",
    "job_type": "word",
    "stage": "word",
    "error_code": "word",
    "status": "status",
    "attempt": "count",
    "model": "model",
    "model_version": "model",
    "timing_ms": "ms",
}

NAME_PREFIXES = (
    "intelligence.request ",
    "intelligence.publish ",
    "api ",
    "grant",
    "boundary.",
    "handle ",
    "stage ",
)
UNNAMED = "unnamed"
_RESOURCE_KEYS = ("service.name", "service.version")


def _value_ok(shape: str, v: Any) -> bool:
    if isinstance(v, bool):
        return False
    if isinstance(v, str):
        if shape == "id":
            return _ID.fullmatch(v) is not None
        if shape == "uuid":
            return _UUID.fullmatch(v) is not None
        if shape in ("word", "status"):
            return _WORD.fullmatch(v) is not None
        if shape == "model":
            return _MODEL.fullmatch(v) is not None
        return False
    if isinstance(v, int):
        if shape == "count":
            return 0 <= v <= 1_000_000
        if shape == "ms":
            return v >= 0
        if shape == "status":
            return 100 <= v <= 999
        return False
    if isinstance(v, float):
        return shape == "ms" and 0 <= v < 1e12
    return False


def filter_attributes(attrs: Mapping[str, Any] | None) -> dict[str, Any]:
    """The attributes of attrs that pass the allow-list."""
    out: dict[str, Any] = {}
    for k, v in (attrs or {}).items():
        shape = ALLOWED.get(k)
        if shape is not None and _value_ok(shape, v):
            out[k] = v
    return out


def safe_name(name: str) -> str:
    """name if it is built on a fixed prefix and uses only route-pattern
    characters, else "unnamed"."""
    if _NAME.fullmatch(name) is None or not name.startswith(NAME_PREFIXES):
        return UNNAMED
    return name


def _filter_events(events: Sequence[Event]) -> list[Event]:
    out: list[Event] = []
    for e in events:
        if e.name != "exception":
            continue
        typ = (e.attributes or {}).get("exception.type")
        attrs = {"exception.type": typ} if isinstance(typ, str) and _EXC_TYPE.fullmatch(typ) else {}
        out.append(Event("exception", attrs, e.timestamp))
    return out


def filter_span(span: ReadableSpan) -> ReadableSpan:
    """A copy of span with only what the allow-list lets out."""
    res = span.resource
    resource = Resource({k: v for k, v in res.attributes.items() if k in _RESOURCE_KEYS})
    return ReadableSpan(
        name=safe_name(span.name),
        context=span.context,
        parent=span.parent,
        resource=resource,
        attributes=filter_attributes(span.attributes),
        events=_filter_events(span.events),
        links=[Link(link.context) for link in span.links],
        kind=span.kind,
        status=Status(span.status.status_code),
        start_time=span.start_time,
        end_time=span.end_time,
        instrumentation_scope=span.instrumentation_scope,
    )


class FilterExporter(SpanExporter):
    """Passes every span through the allow-list, then to ``next_exporter``."""

    def __init__(self, next_exporter: SpanExporter) -> None:
        self._next = next_exporter

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        return self._next.export([filter_span(s) for s in spans])

    def shutdown(self) -> None:
        self._next.shutdown()

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return self._next.force_flush(timeout_millis)


# --- trace context ---------------------------------------------------------------

_PROPAGATOR = TraceContextTextMapPropagator()
_TRACE_HEADERS = ("traceparent", "tracestate")


def _trace_carrier(headers: Mapping[str, str] | None) -> dict[str, str]:
    """Only traceparent and tracestate, by any spelling of the name."""
    out: dict[str, str] = {}
    for k, v in (headers or {}).items():
        lk = k.lower()
        if lk in _TRACE_HEADERS and isinstance(v, str):
            out[lk] = v
    return out


def inject(headers: MutableMapping[str, str]) -> None:
    """Write the current span context into headers (traceparent and
    tracestate only; never baggage)."""
    carrier: dict[str, str] = {}
    _PROPAGATOR.inject(carrier)
    headers.update(carrier)


def extract(headers: Mapping[str, str] | None) -> Context:
    """The context in headers, if its traceparent is valid; else an empty
    context, so the next span starts a new trace. Never raises."""
    try:
        return _PROPAGATOR.extract(_trace_carrier(headers), context=Context())
    except Exception:
        return Context()


def links_from(headers: Mapping[str, str] | None) -> list[Link]:
    """A link to the span context in headers, for a job span that follows a
    request without being its child. Empty when there is no valid one."""
    sc = trace.get_current_span(extract(headers)).get_span_context()
    return [Link(sc)] if sc.is_valid else []


# --- providers -------------------------------------------------------------------


class _State:
    def __init__(self, tracer: trace.Tracer, refusals: Any, verify: Any) -> None:
        self.tracer = tracer
        self.refusals = refusals
        self.verify = verify


_NOOP = _State(trace.NoOpTracer(), None, None)
_state: _State = _NOOP


def enabled() -> bool:
    return _state is not _NOOP


def tracer() -> trace.Tracer:
    return _state.tracer


def setup(
    endpoint: str | None = None,
    *,
    service: str = "intelligence",
    span_exporter: SpanExporter | None = None,
    metric_reader: MetricReader | None = None,
) -> Callable[[], None]:
    """Turn telemetry on and return its shutdown.

    endpoint (default: OTEL_EXPORTER_OTLP_ENDPOINT) is an OTLP/gRPC URL or
    host:port; empty means off: nothing is installed, nothing connects. A
    collector that is down never fails a request: exporting is in the
    background and drops on error. A test passes ``span_exporter`` and
    ``metric_reader`` instead. Spans go through the allow-list first.
    """
    global _state
    if endpoint is None:
        endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
    endpoint = endpoint.strip()
    if span_exporter is None and metric_reader is None:
        if not endpoint:
            return lambda: None
        span_exporter, metric_reader = _otlp(endpoint)
        batch = True
    else:
        batch = False
    resource = Resource({"service.name": f"mhai-{service}"})
    tp = TracerProvider(resource=resource)
    if span_exporter is not None:
        flt = FilterExporter(span_exporter)
        tp.add_span_processor(BatchSpanProcessor(flt) if batch else SimpleSpanProcessor(flt))
    readers = [metric_reader] if metric_reader is not None else []
    mp = MeterProvider(resource=resource, metric_readers=readers)
    meter = mp.get_meter(SCOPE)
    refusals = meter.create_counter(
        "boundary_refusals_total", description="Messages the worker refused or dropped."
    )
    verify = meter.create_histogram(
        "boundary_verify_seconds", unit="s", description="Time spent verifying a message."
    )
    state = _State(tp.get_tracer(SCOPE), refusals, verify)
    _state = state

    def shutdown() -> None:
        global _state
        if _state is state:
            _state = _NOOP
        tp.shutdown()
        mp.shutdown()

    return shutdown


def _otlp(endpoint: str) -> tuple[SpanExporter, MetricReader]:
    if "://" not in endpoint:
        endpoint = "http://" + endpoint
    u = urlparse(endpoint)
    if u.scheme not in ("http", "https") or not u.netloc:
        raise ValueError("OTEL_EXPORTER_OTLP_ENDPOINT is not a valid URL or host:port")
    insecure = u.scheme == "http"
    from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

    return (
        OTLPSpanExporter(endpoint=endpoint, insecure=insecure),
        PeriodicExportingMetricReader(
            OTLPMetricExporter(endpoint=endpoint, insecure=insecure), export_interval_millis=15000
        ),
    )


# --- spans -----------------------------------------------------------------------


@contextmanager
def span(
    name: str,
    *,
    kind: SpanKind = SpanKind.INTERNAL,
    context: Context | None = None,
    links: Sequence[Link] | None = None,
    attributes: Mapping[str, Any] | None = None,
) -> Iterator[Span]:
    """A span made current for the block. An exception marks it an error
    with the exception *type* only (``error_code`` and a type event, never
    the message) and propagates."""
    with _state.tracer.start_as_current_span(
        name,
        context=context,
        kind=kind,
        attributes=dict(attributes or {}),
        links=list(links or ()),
        record_exception=False,
        set_status_on_exception=False,
    ) as sp:
        try:
            yield sp
        except Exception as e:  # not CancelledError: a stop is not a failure
            sp.add_event("exception", {"exception.type": type(e).__name__})
            if not failed(sp):  # a code set by the caller stays
                fail(sp, "internal")
            raise


def fail(sp: Span, code: str) -> None:
    """Mark sp an error with a closed-set code (no description)."""
    sp.set_attribute("status", "error")
    sp.set_attribute("error_code", code)
    sp.set_status(Status(StatusCode.ERROR))


def current_fail(code: str) -> None:
    """fail() on the current span (a no-op when there is none). A code such
    as "grant_refused:mfa_required" is recorded with the colon as an
    underscore."""
    fail(trace.get_current_span(), code.replace(":", "_"))


def annotate(**attributes: Any) -> None:
    """Set attributes on the current span; the allow-list decides which
    leave the process."""
    trace.get_current_span().set_attributes(attributes)


def failed(sp: Span) -> bool:
    """Whether sp was marked an error (fail())."""
    return (getattr(sp, "attributes", None) or {}).get("status") == "error"


def ok(sp: Span) -> None:
    sp.set_attribute("status", "ok")
    sp.set_status(Status(StatusCode.OK))


@contextmanager
def verifying(service: str, context: Context | None = None) -> Iterator[Span]:
    """The span "boundary.verify" and the boundary_verify_seconds timer."""
    start = time.perf_counter()
    with span("boundary.verify", context=context, attributes={"service": service}) as sp:
        try:
            yield sp
        finally:
            h = _state.verify
            if h is not None:
                h.record(time.perf_counter() - start, {"service": _label(service)})


# --- metrics ---------------------------------------------------------------------

_LABEL = re.compile(r"[a-z][a-z0-9_]{0,39}")


def _label(v: str) -> str:
    """A metric label value: a short lowercase word, else "other", so a
    label can never hold free text or grow without bound."""
    return v if _LABEL.fullmatch(v) else "other"


def record_refusal(service: str, reason: str, n: int = 1) -> None:
    c = _state.refusals
    if c is not None:
        c.add(n, {"service": _label(service), "reason": _label(reason)})


class RefusalCounter(Counter[Any]):
    """A Counter whose increments also feed boundary_refusals_total.

    Keys are a reason (the worker's own, with this counter's ``service``) or
    a ``(service, reason)`` pair (the verifier's). Everything else about it
    is a Counter, so existing readers of ``refusals`` keep working.
    """

    def __init__(self, service: str = "") -> None:
        self._service = service
        super().__init__()

    def __setitem__(self, key: Any, value: int) -> None:
        old = self.get(key, 0)
        super().__setitem__(key, value)
        if value > old:
            if isinstance(key, tuple):
                svc, reason = key
            else:
                svc, reason = self._service, key
            record_refusal(str(svc), str(reason), value - old)
