"""Observability across the boundary (spec §10.3, plan 3 task 14).

The first group needs no Docker: the attribute allow-list (a filtering
exporter), trace-context propagation and the refusal metric. The tests
marked `boundary` run real jobs and BFF-direct calls against NATS 2.10 and
Postgres 16 with an in-memory exporter behind the allow-list, and assert
that a PHI-like marker, an access token and a request token appear in no
span (name, attribute, event, link, status), no log line and no dead
letter. Every marker, id and token here is synthetic.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Callable, Iterator, Sequence
from typing import Any

import pytest
from opentelemetry import baggage, trace
from opentelemetry.context import Context
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import ReadableSpan
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import Link, SpanKind

from intelligence.boundary import envelope, telemetry
from intelligence.boundary.models import ModelError, StubModelClient
from intelligence.boundary.pb import echo_pb2
from intelligence.boundary.services import echo
from intelligence.boundary.worker import GUARDED
from tests.boundary import test_run_query as rq
from tests.boundary import test_worker as tw
from tests.boundary.tokens import HOSPITAL, RID

MARKER = "SYNTH-MARKER-7f3a"
TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
PARENT_SPAN = "00f067aa0ba902b7"
TRACEPARENT = f"00-{TRACE_ID}-{PARENT_SPAN}-01"

# Fixtures of the other boundary test modules, by name.
start_worker = tw.start_worker
core = tw.core
bff = tw.bff
grant_stub = tw.grant_stub
keys_file = tw.keys_file
database = rq.database
make_env = rq.make_env


@pytest.fixture
def exporters() -> Iterator[tuple[InMemorySpanExporter, InMemoryMetricReader]]:
    spans = InMemorySpanExporter()
    reader = InMemoryMetricReader()
    stop = telemetry.setup(span_exporter=spans, metric_reader=reader)
    yield spans, reader
    stop()


def exported_text(span: ReadableSpan) -> list[str]:
    """Every string a collector would see in span."""
    out = [span.name, span.status.description or ""]
    out += [f"{k}={v}" for k, v in (span.attributes or {}).items()]
    for e in span.events:
        out.append(e.name)
        out += [f"{k}={v}" for k, v in (e.attributes or {}).items()]
    for link in span.links:
        out += [f"{k}={v}" for k, v in (link.attributes or {}).items()]
    out += [f"{k}={v}" for k, v in span.resource.attributes.items()]
    return out


def assert_no_secret(
    spans: Sequence[ReadableSpan], logs: str, secrets: Sequence[str], dead: Sequence[bytes] = ()
) -> None:
    assert spans, "no spans exported: the test proves nothing"
    for s in spans:
        for text in exported_text(s):
            for secret in secrets:
                assert secret not in text, f"{secret!r} in span {s.name!r}: {text!r}"
    for secret in secrets:
        assert secret not in logs
        for d in dead:
            assert secret.encode() not in d


def assert_no_tracestate(spans: Sequence[ReadableSpan]) -> None:
    for s in spans:
        assert len(s.context.trace_state) == 0, s.name
        assert s.parent is None or len(s.parent.trace_state) == 0, s.name
        assert all(len(lk.context.trace_state) == 0 for lk in s.links), s.name


def metric_points(reader: InMemoryMetricReader, name: str) -> list[Any]:
    data = reader.get_metrics_data()
    out: list[Any] = []
    for rm in data.resource_metrics if data else ():
        for sm in rm.scope_metrics:
            for m in sm.metrics:
                if m.name == name:
                    out += list(m.data.data_points)
    return out


# --- no Docker -----------------------------------------------------------------


def test_allowlist_drops_everything_else(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    with telemetry.span(
        "handle echo.ping",
        attributes={
            "operation": "echo.ping",
            "hospital_id": HOSPITAL,
            "request_id": RID,
            "attempt": 2,
            "timing_ms": 12.5,
            "clinical_question": MARKER,
            "Authorization": "Bearer " + MARKER,
            "Mhai-Request-Token": MARKER,
            "password": MARKER,
            "model": MARKER + " has spaces",  # an allowed key with the wrong kind of value
            "query_id": MARKER,  # an allowed key, not a uuid
        },
    ) as sp:
        sp.add_event("note " + MARKER, {"text": MARKER})
        sp.add_event("exception", {"exception.type": "ValueError", "exception.message": MARKER})
        sp.set_status(trace.Status(trace.StatusCode.ERROR, MARKER))
    with telemetry.span("handle " + MARKER + "\n{}"):
        pass
    with telemetry.span(MARKER):
        pass
    out = spans.get_finished_spans()
    assert len(out) == 3
    s = out[0]
    assert dict(s.attributes or {}) == {
        "operation": "echo.ping",
        "hospital_id": HOSPITAL,
        "request_id": RID,
        "attempt": 2,
        "timing_ms": 12.5,
    }
    assert s.status.description is None
    assert [(e.name, dict(e.attributes or {})) for e in s.events] == [
        ("exception", {"exception.type": "ValueError"})
    ]
    assert out[1].name == "unnamed" and out[2].name == "unnamed"
    assert_no_secret(out, "", [MARKER])


def test_exception_in_a_span_records_the_type_only(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    with pytest.raises(ValueError):
        with telemetry.span("stage generate", attributes={"stage": "generate"}):
            raise ValueError("patient " + MARKER)
    (s,) = spans.get_finished_spans()
    assert s.status.status_code == trace.StatusCode.ERROR and s.status.description is None
    assert dict(s.attributes or {}) == {
        "stage": "generate",
        "status": "error",
        "error_code": "internal",
    }
    assert [dict(e.attributes or {}) for e in s.events] == [{"exception.type": "ValueError"}]
    assert_no_secret([s], "", [MARKER])


def test_malformed_traceparent_starts_new_trace(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    for bad in (
        "garbage",
        "00-zzzz-00f067aa0ba902b7-01",
        "00-" + "0" * 32 + f"-{PARENT_SPAN}-01",
        "",
    ):
        ctx = telemetry.extract({"traceparent": bad})
        assert not trace.get_current_span(ctx).get_span_context().is_valid, bad
        assert telemetry.links_from({"traceparent": bad}) == []
        with telemetry.span("handle echo.ping", context=ctx):
            pass
    for s in spans.get_finished_spans():
        assert s.parent is None and format(s.context.trace_id, "032x") != TRACE_ID
    # A valid one is picked up, by any spelling of the name.
    ctx = telemetry.extract({"TraceParent": TRACEPARENT, "tracestate": "v=1"})
    assert format(trace.get_current_span(ctx).get_span_context().trace_id, "032x") == TRACE_ID


def test_baggage_dropped(exporters: tuple[InMemorySpanExporter, InMemoryMetricReader]) -> None:
    ctx = telemetry.extract({"traceparent": TRACEPARENT, "baggage": "user=" + MARKER})
    assert baggage.get_all(ctx) == {}
    ctx = baggage.set_baggage(
        "user",
        MARKER,
        trace.set_span_in_context(
            trace.NonRecordingSpan(
                trace.SpanContext(
                    int(TRACE_ID, 16), int(PARENT_SPAN, 16), True, trace.TraceFlags(1)
                )
            )
        ),
    )
    from opentelemetry import context as otel_context

    token = otel_context.attach(ctx)
    try:
        headers: dict[str, str] = {"Authorization": "Bearer keep-me"}
        telemetry.inject(headers)
    finally:
        otel_context.detach(token)
    assert set(headers) == {"Authorization", "traceparent"}
    assert MARKER not in " ".join(headers.values())


def test_inject_roundtrip_and_job_link(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    headers: dict[str, str] = {}
    with telemetry.span("intelligence.publish run_query", kind=SpanKind.PRODUCER) as producer:
        telemetry.inject(headers)
    pc = producer.get_span_context()
    assert headers["traceparent"].split("-")[1] == format(pc.trace_id, "032x")
    # The job: a new trace, linked (not parented) to the producer.
    with telemetry.span(
        "handle pipeline.run_query",
        kind=SpanKind.CONSUMER,
        context=Context(),
        links=telemetry.links_from(headers),
    ):
        pass
    job = spans.get_finished_spans()[-1]
    assert job.parent is None and job.context.trace_id != pc.trace_id
    assert [(lk.context.trace_id, lk.context.span_id) for lk in job.links] == [
        (pc.trace_id, pc.span_id)
    ]
    assert all(not lk.attributes for lk in job.links)


def test_tracestate_never_read_or_exported(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    carrier = {"traceparent": TRACEPARENT, "tracestate": "vendor=" + MARKER}
    ctx = telemetry.extract(carrier)
    assert len(trace.get_current_span(ctx).get_span_context().trace_state) == 0
    assert telemetry.links_from(carrier)[0].context.trace_state == trace.TraceState()
    # Defence in depth: spans and links that do carry one are stripped on export.
    ts = trace.TraceState([("vendor", "x")])
    sc = trace.SpanContext(int(TRACE_ID, 16), int(PARENT_SPAN, 16), True, trace.TraceFlags(1), ts)
    pctx = trace.set_span_in_context(trace.NonRecordingSpan(sc))
    with telemetry.span("handle x", context=pctx, links=[Link(sc)]):
        headers: dict[str, str] = {}
        telemetry.inject(headers)
    assert "tracestate" not in headers
    out = spans.get_finished_spans()
    assert len(out) == 1 and out[0].parent is not None and len(out[0].links) == 1
    assert_no_tracestate(out)


def test_unsampled_traceparent_still_traced(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, _ = exporters
    ctx = telemetry.extract({"traceparent": f"00-{TRACE_ID}-{PARENT_SPAN}-00"})
    with telemetry.span("handle echo.ping", context=ctx):
        pass
    assert len(spans.get_finished_spans()) == 1


def test_plaintext_endpoint_refused_unless_loopback_or_opted_in() -> None:
    for ep in ("http://collector.example:4317", "collector.example:4317", "10.0.0.5:4317"):
        with pytest.raises(ValueError):
            telemetry.setup(ep, allow_plaintext=False)
    for ep, opt in (
        ("127.0.0.1:4317", False),
        ("http://localhost:4317", False),
        ("http://[::1]:4317", False),
        ("https://collector.example:4317", False),
        ("http://collector.example:4317", True),
    ):
        telemetry.setup(ep, allow_plaintext=opt)()


def test_start_warns_on_insecure_outside_development_and_gives_the_reason(
    caplog: pytest.LogCaptureFixture,
) -> None:
    """Carried from T14: OTEL_EXPORTER_OTLP_INSECURE outside development is
    warned about, and a refused endpoint turns telemetry off with the reason
    (never the endpoint itself); the worker still starts."""
    caplog.set_level(logging.WARNING, logger="intelligence.boundary.telemetry")
    ep = "http://collector.example:4317"
    stop = telemetry.start(
        service="echo",
        development=False,
        environ={"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_INSECURE": "true"},
    )
    assert stop is not None
    stop()
    assert telemetry.INSECURE_OUTSIDE_DEVELOPMENT in caplog.text
    caplog.clear()

    stop = telemetry.start(
        service="echo", development=False, environ={"OTEL_EXPORTER_OTLP_ENDPOINT": ep}
    )
    assert stop is None
    assert "telemetry is off: a plaintext OTLP endpoint must be loopback" in caplog.text
    assert "collector.example" not in caplog.text
    caplog.clear()

    # Development allows plaintext and warns about nothing.
    stop = telemetry.start(
        service="echo",
        development=True,
        environ={"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_EXPORTER_OTLP_INSECURE": "true"},
    )
    assert stop is not None
    stop()
    assert caplog.text == ""
    # Off without an endpoint, quietly.
    stop = telemetry.start(service="echo", development=False, environ={})
    assert stop is not None
    stop()
    assert caplog.text == ""


def test_trace_headers_are_not_security_headers() -> None:
    for h in ("traceparent", "tracestate", "baggage"):
        assert h not in {g.lower() for g in GUARDED}
    # A repeated traceparent is not an ambiguity the worker refuses.
    from intelligence.boundary.worker import _ambiguous

    raw = b"NATS/1.0\r\ntraceparent: a\r\ntraceparent: b\r\nMhai-Schema: x\r\n\r\n"
    assert not _ambiguous(raw, {"traceparent": "b", "Mhai-Schema": "x"})
    guarded = b"NATS/1.0\r\nMhai-Schema: x\r\nMhai-Schema: y\r\n\r\n"
    assert _ambiguous(guarded, {"Mhai-Schema": "y"})  # the guarded ones still are


def test_refusal_metric_increments(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    _, reader = exporters
    c = telemetry.RefusalCounter("pipeline")
    c["signature"] += 1
    c["signature"] += 1
    c["bad reason " + MARKER] += 1  # never a label of its own
    v = telemetry.RefusalCounter()
    v[("echo", "expired")] += 1
    assert c["signature"] == 2 and v[("echo", "expired")] == 1  # still a Counter
    got = {
        (p.attributes["service"], p.attributes["reason"]): p.value
        for p in metric_points(reader, "boundary_refusals_total")
    }
    assert got == {
        ("pipeline", "signature"): 2,
        ("pipeline", "other"): 1,
        ("echo", "expired"): 1,
    }
    for p in metric_points(reader, "boundary_refusals_total"):
        assert set(p.attributes) == {"service", "reason"}


def test_verify_seconds_recorded(
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    spans, reader = exporters
    with telemetry.verifying("echo") as sp:
        telemetry.fail(sp, "unauthorized")
    (p,) = metric_points(reader, "boundary_verify_seconds")
    assert p.count == 1 and dict(p.attributes) == {"service": "echo"}
    (s,) = spans.get_finished_spans()
    assert s.name == "boundary.verify" and s.attributes["error_code"] == "unauthorized"


def test_off_without_endpoint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    stop = telemetry.setup()
    assert not telemetry.enabled()
    with telemetry.span("handle echo.ping") as sp:
        assert not sp.is_recording()
    c = telemetry.RefusalCounter("echo")
    c["x"] += 1  # counts locally, exports nowhere
    assert c["x"] == 1
    stop()
    with pytest.raises(ValueError):
        telemetry.setup("ftp://collector:4317")


def test_unreachable_collector_is_harmless() -> None:
    stop = telemetry.setup("127.0.0.1:1")
    try:
        with telemetry.span("handle echo.ping"):
            pass
    finally:
        t0 = time.monotonic()
        stop()
    assert time.monotonic() - t0 < 15


# --- boundary: jobs --------------------------------------------------------------


class SelectiveStub(StubModelClient):
    """Fails the generate stage for a question that holds FAILCASE."""

    def mask(self, text: str) -> str:
        self._enter("mask")
        return "FAIL" if "FAILCASE" in text else self.MASKED

    def generate(self, question: str, patient_context: Any, patient_id: Any, chunks: Any) -> Any:
        if question == "FAIL":
            self._enter("generate")
            raise ModelError("unavailable")
        return super().generate(question, patient_context, patient_id, chunks)


@pytest.mark.boundary
async def test_marker_never_in_logs_spans_or_dead_letters(
    make_env: rq.MakeEnv,
    postgres: rq.Postgres,
    caplog: pytest.LogCaptureFixture,
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    """A14: a question and patient data holding the marker run through a
    succeeding and a failing job. The marker is in no span attribute or
    event, no log line and no dead letter."""
    spans, _ = exporters
    caplog.set_level(logging.DEBUG)
    env = await make_env()
    ok_id = await rq.seed_query(postgres, question=f"Synthetic question {MARKER}")
    bad_id = await rq.seed_query(postgres, question=f"FAILCASE {MARKER}")
    await env.start(SelectiveStub())
    _, headers, body = env.job(ok_id)
    headers["traceparent"] = TRACEPARENT
    headers["tracestate"] = "vendor=" + MARKER
    await env.send(f"jobs.v1.{rq.HOSPITAL_A}.run_query", headers, body, msg_id=ok_id)
    assert await rq.until(lambda: rq.status_is(postgres, ok_id, "completed"))
    await env.publish(bad_id)
    dead = await env.dead_letters(1, timeout=30)
    assert len(dead) == 1
    assert await rq.until(lambda: rq.status_is(postgres, bad_id, "failed"))
    assert await env.settled()
    out = spans.get_finished_spans()
    assert_no_secret(out, caplog.text, [MARKER], [d.SerializeToString() for _, d in dead])
    await asyncio.sleep(0.2)  # the job spans end just after the ack
    out = spans.get_finished_spans()
    assert_no_secret(out, caplog.text, [MARKER], [d.SerializeToString() for _, d in dead])

    names = {s.name for s in out}
    assert {
        "handle pipeline.run_query",
        "boundary.verify",
        "stage mask_all",
        "stage generate",
        "stage verify",
    } <= names
    by_trace: dict[int, list[ReadableSpan]] = {}
    for s in out:
        by_trace.setdefault(s.context.trace_id, []).append(s)
    jobs = [s for s in out if s.name == "handle pipeline.run_query"]
    assert len(jobs) >= 2
    first = next(s for s in jobs if s.attributes.get("query_id") == ok_id)
    # The job is a new trace linked to the producer's span, never its child.
    assert first.parent is None and format(first.context.trace_id, "032x") != TRACE_ID
    assert [
        (format(lk.context.trace_id, "032x"), format(lk.context.span_id, "016x"))
        for lk in first.links
    ] == [(TRACE_ID, PARENT_SPAN)]
    # Its stages are its children.
    stages = [s for s in by_trace[first.context.trace_id] if s.name.startswith("stage ")]
    assert {s.name for s in stages} == {"stage mask_all", "stage generate", "stage verify"}
    assert all(s.parent is not None and s.parent.span_id == first.context.span_id for s in stages)
    assert first.attributes["hospital_id"] == rq.HOSPITAL_A and first.attributes["status"] == "ok"
    assert any(s.attributes.get("query_id") == bad_id for s in jobs)
    broke = [s for s in out if s.name == "stage generate" and s.status.status_code.name == "ERROR"]
    assert broke and all(
        [dict(e.attributes or {}) for e in s.events] == [{"exception.type": "ModelError"}]
        and s.attributes["error_code"] == "internal"
        for s in broke
    )


@pytest.mark.boundary
@pytest.mark.parametrize("path", ["granted", "grant_refused", "core_down"])
async def test_access_token_never_in_worker_logs_or_spans(
    path: str,
    start_worker: tw.StartWorker,
    bff: Any,
    grant_stub: Callable[..., Any],
    caplog: pytest.LogCaptureFixture,
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    """A34: a BFF-direct call whose access token holds a marker leaves it in
    no log line and no span, on the success and the refusal paths; nor does
    the token core granted."""
    spans, reader = exporters
    access = f"synthetic-access-{MARKER}"
    stub = None
    if path == "granted":
        stub = await grant_stub()
    elif path == "grant_refused":
        stub = await grant_stub(refuse="mfa_required")
    h = tw.Counting(echo.ping)
    await start_worker(handlers={"ping": h})
    hdrs = tw.bff_headers(access)
    hdrs["traceparent"] = TRACEPARENT
    hdrs["baggage"] = "user=" + MARKER
    hdrs["tracestate"] = "vendor=" + MARKER
    body = echo_pb2.PingRequest(text=MARKER, hospital_id=HOSPITAL).SerializeToString(
        deterministic=True
    )
    with caplog.at_level(logging.DEBUG):
        r = await bff.request(tw.PING, body, timeout=3, headers=hdrs)
    assert (
        tw.status(r)
        == {
            "granted": "ok",
            "grant_refused": "grant_refused:mfa_required",
            "core_down": "unavailable",
        }[path]
    )
    secrets = [access, MARKER]
    if stub is not None and stub.tokens:
        secrets.append(stub.tokens[0])
    out = spans.get_finished_spans()
    assert_no_secret(out, caplog.text, secrets)
    assert_no_tracestate(out)
    # The BFF's id is its own attribute; request_id only ever holds core-minted ids.
    bff_id = hdrs[envelope.REQUEST_ID]
    grants = [s for s in out if s.name == "boundary.grant"]
    if stub is not None:
        assert grants and all(
            g.attributes["bff_request_id"] == bff_id and "request_id" not in g.attributes
            for g in grants
        )
        for h in stub.headers:  # what the worker sent core: our context, never theirs
            assert "tracestate" not in {k.lower() for k in h} and "traceparent" in h
            assert MARKER not in " ".join(h.values())
    # The caller's trace context was honoured, and the grant request carried
    # the worker's own span context to core.
    assert any(format(s.context.trace_id, "032x") == TRACE_ID for s in out)
    if path == "granted":
        assert any(s.name == "handle echo.ping" and s.attributes["status"] == "ok" for s in out)
    else:
        assert not any(s.name == "handle echo.ping" for s in out)
    if stub is not None:
        assert stub.requests
    refusals = {
        (p.attributes["service"], p.attributes["reason"]): p.value
        for p in metric_points(reader, "boundary_refusals_total")
    }
    if path != "granted":
        assert sum(refusals.values()) == 1 and all(svc == "echo" for svc, _ in refusals)


@pytest.mark.boundary
async def test_core_request_is_a_child_and_baggage_is_dropped(
    start_worker: tw.StartWorker,
    core: Any,
    signer: Any,
    exporters: tuple[InMemorySpanExporter, InMemoryMetricReader],
) -> None:
    """A request/reply call's worker span is a child of the caller's span
    (only jobs are linked); a refusal is counted by reason."""
    spans, reader = exporters
    await start_worker()
    body, hdrs = tw.core_ping(signer, MARKER)
    hdrs["traceparent"] = TRACEPARENT
    hdrs["baggage"] = "user=" + MARKER
    hdrs["tracestate"] = "vendor=" + MARKER
    r = await core.request(tw.PING, body, timeout=2, headers=hdrs)
    assert tw.status(r) == "ok"
    body2, hdrs2 = tw.core_ping(signer, MARKER)
    hdrs2["traceparent"] = "garbage"
    assert tw.status(await core.request(tw.PING, body2, timeout=2, headers=hdrs2)) == "ok"
    await asyncio.sleep(0.05)
    out = spans.get_finished_spans()
    handles = [s for s in out if s.name == "handle echo.ping"]
    assert len(handles) == 2
    child = next(s for s in handles if s.parent is not None)
    assert format(child.context.trace_id, "032x") == TRACE_ID
    assert format(child.parent.span_id, "016x") == PARENT_SPAN and not child.links
    fresh = next(s for s in handles if s.parent is None)
    assert format(fresh.context.trace_id, "032x") != TRACE_ID  # a malformed one starts anew
    assert_no_secret(out, "", [MARKER])
    assert_no_tracestate(out)
    # A replay is refused and counted by reason, with no user in the labels.
    r2 = await core.request(tw.PING, body, timeout=2, headers=hdrs)
    assert tw.status(r2) == "unauthorized"
    pts = metric_points(reader, "boundary_refusals_total")
    assert pts and all(set(p.attributes) == {"service", "reason"} for p in pts)
    assert sum(p.value for p in pts if p.attributes["service"] == "echo") == 1
