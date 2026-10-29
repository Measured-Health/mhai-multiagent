//go:build integration

package intel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

const spanMarker = "SYNTH-MARKER-7f3a"

// A request/reply call is a client span under the caller's span, its
// traceparent goes to the worker (and nothing else new: no baggage), and
// no span, name or log line holds the question or a token. Its latency is
// in intelligence_request_seconds{operation}.
func TestRequestSpanAndHeaders(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	rd := sdkmetric.NewManualReader()
	shutdown, err := telemetry.SetupWith(exp, rd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	c, logs := newClient(t)
	w := startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), pong(&intelpb.ResponseMeta{}))
	hid := uuid.New()
	caller := clinician(hid)

	ctx, parent := telemetry.Start(context.Background(), trace.SpanKindServer, "api POST /api/v1/queries")
	m, _ := baggage.NewMember("question", spanMarker)
	bag, _ := baggage.New(m)
	ctx = baggage.ContextWithBaggage(ctx, bag)
	var resp intelpb.PingResponse
	if _, err := c.Request(ctx, caller, "echo.ping", &intelpb.PingRequest{Text: spanMarker}, &resp); err != nil {
		t.Fatal(err)
	}
	parent.End()

	rs := w.received()
	if len(rs) != 1 {
		t.Fatalf("worker got %d", len(rs))
	}
	tp := rs[0].header.Get("traceparent")
	if !strings.Contains(tp, parent.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q is not on the caller's trace", tp)
	}
	for k, vs := range rs[0].header {
		switch strings.ToLower(k) {
		case "mhai-request-token", "mhai-schema", "mhai-request-id", "traceparent", "tracestate":
		default:
			t.Errorf("unexpected header %q on the request", k)
		}
		for _, v := range vs {
			if strings.Contains(v, spanMarker) && !strings.EqualFold(k, "mhai-request-token") {
				t.Errorf("marker in header %q", k)
			}
		}
	}

	var client tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "intelligence.request echo.ping" {
			client = s
		}
	}
	if client.Name == "" || client.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("client span missing or not a child of the caller's: %+v", exp.GetSpans())
	}
	got := map[string]string{}
	for _, kv := range client.Attributes {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["operation"] != "echo.ping" || got["hospital_id"] != hid.String() || got["request_id"] != caller.Rid || got["status"] != "ok" {
		t.Fatalf("attributes: %v", got)
	}
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if strings.Contains(kv.Value.Emit(), spanMarker) {
				t.Fatalf("marker in span attribute %s", kv.Key)
			}
		}
		if len(s.Events) > 0 {
			t.Fatalf("events on %s", s.Name)
		}
	}
	if strings.Contains(logs.String(), spanMarker) {
		t.Fatal("marker in the logs")
	}

	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if h, ok := mm.Data.(metricdata.Histogram[float64]); ok && mm.Name == "intelligence_request_seconds" {
				for _, dp := range h.DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key("operation"))
					found = v.AsString() == "echo.ping" && dp.Attributes.Len() == 1 && dp.Count == 1
				}
			}
		}
	}
	if !found {
		t.Fatal("intelligence_request_seconds{operation} not recorded")
	}
}

// A worker refusal is recorded by its closed-set code, not its message.
func TestRequestSpanRecordsRefusalCode(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.SetupWith(exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	c, _ := newClient(t)
	startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), refuse("forbidden", false))
	var resp intelpb.PingResponse
	if _, err := c.Request(context.Background(), clinician(uuid.New()), "echo.ping", &intelpb.PingRequest{}, &resp); err == nil {
		t.Fatal("expected a refusal")
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans %d", len(spans))
	}
	got := map[string]string{}
	for _, kv := range spans[0].Attributes {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["status"] != "error" || got["error_code"] != "forbidden" {
		t.Fatalf("attributes: %v", got)
	}
}

// A job is published under a producer span and carries its traceparent, for
// the worker to link to; no baggage rides along.
func TestPublishJobCarriesTraceparent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.SetupWith(exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	c, _ := newClient(t)
	drainJobs(t, sharedNATS.PipelineURL)
	hid := uuid.New()
	job := uuid.New()
	ctx, parent := telemetry.Start(context.Background(), trace.SpanKindServer, "api POST /api/v1/queries")
	m, _ := baggage.NewMember("q", spanMarker)
	bag, _ := baggage.New(m)
	ctx = baggage.ContextWithBaggage(ctx, bag)
	if err := c.PublishJob(ctx, clinician(hid), "pipeline.run_query", job, &intelpb.RunQueryJob{QueryId: job.String()}, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	parent.End()
	got := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(got) != 1 {
		t.Fatalf("%d jobs", len(got))
	}
	if tp := got[0].header.Get("traceparent"); !strings.Contains(tp, parent.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q", tp)
	}
	for k := range got[0].header {
		if strings.EqualFold(k, "baggage") {
			t.Fatal("baggage on the job")
		}
	}
	var prod tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "intelligence.publish run_query" {
			prod = s
		}
	}
	if prod.Name == "" || prod.SpanKind != trace.SpanKindProducer {
		t.Fatalf("no producer span: %+v", exp.GetSpans())
	}
	vals := map[string]string{}
	for _, kv := range prod.Attributes {
		vals[string(kv.Key)] = kv.Value.Emit()
	}
	if vals["job_id"] != job.String() || vals["job_type"] != "run_query" {
		t.Fatalf("attributes: %v", vals)
	}
}
