package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const marker = "SYNTH-MARKER-7f3a"

func setup(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	rd := sdkmetric.NewManualReader()
	sd, err := SetupWith(exp, rd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sd(context.Background()) })
	return exp, rd
}

// Only allow-listed keys with the right kind of value leave the process:
// the marker is in no attribute, name, event, status or link.
func TestAllowListDropsEverythingElse(t *testing.T) {
	exp, _ := setup(t)
	_, other := Start(context.Background(), trace.SpanKindInternal, "intelligence.drain")
	ctx, span := Start(context.Background(), trace.SpanKindClient, "intelligence.request pipeline.run_query",
		attribute.String("operation", "pipeline.run_query"),
		attribute.String("hospital_id", "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"),
		attribute.String("request_id", "abc123"),
		attribute.Int("attempt", 2),
		attribute.String("clinical_question", marker),
		attribute.String("Authorization", "Bearer "+marker),
		attribute.String("request_token", marker),
		attribute.String("password", marker),
		attribute.String("request_id2", marker),
		attribute.String("model", marker+" with spaces"), // allowed key, wrong shape
		attribute.String("hospital_id_bad", marker),
	)
	_ = ctx
	span.SetAttributes(attribute.String("query_id", marker)) // allowed key, not a uuid
	span.AddEvent("note "+marker, trace.WithAttributes(attribute.String("text", marker)))
	span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", "ValueError"),
		attribute.String("exception.message", marker), attribute.String("exception.stacktrace", marker)))
	span.AddLink(trace.Link{SpanContext: other.SpanContext(), Attributes: []attribute.KeyValue{attribute.String("x", marker)}})
	span.SetStatus(codes.Error, marker)
	span.End()
	other.End()
	_, bad := Start(context.Background(), trace.SpanKindInternal, "handle "+marker+"\n{\"q\":1}")
	bad.End()
	_, bad2 := Start(context.Background(), trace.SpanKindInternal, marker)
	bad2.End()

	spans := exp.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("spans: %d", len(spans))
	}
	for _, s := range spans {
		if strings.Contains(s.Name, marker) {
			t.Fatalf("name %q", s.Name)
		}
	}
	// The exporter wraps the span; re-read what the filter left.
	got := map[string]string{}
	for _, kv := range spans[0].Attributes {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	want := map[string]string{"operation": "pipeline.run_query", "hospital_id": "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b",
		"request_id": "abc123", "attempt": "2"}
	if len(got) != len(want) {
		t.Fatalf("attributes: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("attribute %s = %q", k, got[k])
		}
	}
	if spans[0].Status.Description != "" {
		t.Fatalf("status description %q", spans[0].Status.Description)
	}
	if len(spans[0].Events) != 1 || spans[0].Events[0].Name != "exception" || len(spans[0].Events[0].Attributes) != 1 ||
		spans[0].Events[0].Attributes[0].Value.AsString() != "ValueError" {
		t.Fatalf("events: %+v", spans[0].Events)
	}
	if len(spans[0].Links) != 1 || len(spans[0].Links[0].Attributes) != 0 {
		t.Fatalf("links: %+v", spans[0].Links)
	}
	if spans[2].Name != "unnamed" || spans[3].Name != "unnamed" || spans[1].Name != "intelligence.drain" {
		t.Fatalf("names: %q %q %q %q", spans[0].Name, spans[1].Name, spans[2].Name, spans[3].Name)
	}
}

// Every attribute the spec lists is on the allow-list, and nothing that
// names a person, a token or free text is.
func TestAllowedKeysAreIdsCodesAndNumbers(t *testing.T) {
	for _, k := range AllowedKeys() {
		for _, bad := range []string{"question", "answer", "patient", "token", "password", "authorization", "email", "name", "user", "text", "prompt"} {
			if strings.Contains(strings.ToLower(k), bad) {
				t.Fatalf("allowed key %q looks like free text or a secret", k)
			}
		}
	}
	if len(AllowedKeys()) == 0 {
		t.Fatal("empty")
	}
}

// Baggage is never read from a message and never written to one.
func TestBaggageDropped(t *testing.T) {
	h := nats.Header{}
	h.Set("baggage", "user="+marker)
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := Extract(context.Background(), h)
	if n := baggage.FromContext(ctx).Len(); n != 0 {
		t.Fatalf("baggage extracted: %d", n)
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("traceparent not extracted")
	}
	m, _ := baggage.NewMember("user", marker)
	b, _ := baggage.New(m)
	ctx = baggage.ContextWithBaggage(trace.ContextWithRemoteSpanContext(context.Background(), trace.SpanContextFromContext(ctx)), b)
	out := nats.Header{}
	Inject(ctx, out)
	for k, vs := range out {
		if !strings.EqualFold(k, "traceparent") && !strings.EqualFold(k, "tracestate") {
			t.Fatalf("injected header %q", k)
		}
		for _, v := range vs {
			if strings.Contains(v, marker) {
				t.Fatal("marker injected")
			}
		}
	}
	if out.Get("traceparent") == "" {
		t.Fatal("no traceparent injected")
	}
}

func TestMalformedTraceparentStartsNewTrace(t *testing.T) {
	for _, tp := range []string{"garbage", "00-zzzz-00f067aa0ba902b7-01", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""} {
		h := nats.Header{}
		h.Set("traceparent", tp)
		ctx := Extract(context.Background(), h)
		if trace.SpanContextFromContext(ctx).IsValid() {
			t.Fatalf("%q accepted", tp)
		}
	}
	// A header given twice is ambiguous: ignored.
	h := nats.Header{"traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4737-00f067aa0ba902b7-01"}}
	if trace.SpanContextFromContext(Extract(context.Background(), h)).IsValid() {
		t.Fatal("duplicate traceparent accepted")
	}
}

func TestCarrierSeesOnlyTraceHeaders(t *testing.T) {
	h := nats.Header{"Authorization": {"Bearer x"}, "Mhai-Request-Token": {"t"}}
	c := Header(h)
	c.Set("Authorization", "evil")
	c.Set("TraceParent", "v")
	if h["Authorization"][0] != "Bearer x" || h["traceparent"][0] != "v" || len(h) != 3 {
		t.Fatalf("headers: %v", h)
	}
	if c.Get("authorization") != "" || c.Get("Mhai-Request-Token") != "" {
		t.Fatal("carrier exposed a security header")
	}
	if ks := c.Keys(); len(ks) != 1 || ks[0] != "traceparent" {
		t.Fatalf("keys: %v", ks)
	}
	Header(nil).Set("traceparent", "x") // no panic
}

func TestOffWithoutEndpoint(t *testing.T) {
	sd, err := Setup(context.Background(), "")
	if err != nil || sd == nil || sd(context.Background()) != nil {
		t.Fatal("empty endpoint must be a no-op")
	}
	_, span := Start(context.Background(), trace.SpanKindInternal, "intelligence.drain")
	if span.IsRecording() {
		t.Fatal("recording without an exporter")
	}
	span.End()
	ObserveRequest("pipeline.run_query", time.Second) // no panic
	NATSReconnected()
	if err := ObserveCount("x_total", "x", func() int64 { return 1 }); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(context.Background(), "ftp://collector:4317"); err == nil {
		t.Fatal("bad scheme accepted")
	}
}

// A collector that isn't there never fails a call or hangs a request.
func TestUnreachableCollectorIsHarmless(t *testing.T) {
	sd, err := Setup(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_, span := Start(context.Background(), trace.SpanKindInternal, "intelligence.drain")
	End(span, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = sd(ctx) // may report the failed export; must return
}

func TestMetricsAreLowCardinality(t *testing.T) {
	_, rd := setup(t)
	ObserveRequest("pipeline.run_query", 120*time.Millisecond)
	NATSReconnected()
	if err := ObserveCount("natsapi_refused_replies_total", "x", func() int64 { return 7 }); err != nil {
		t.Fatal(err)
	}
	if err := Observe("jobs_queue_depth", "x", false, func(context.Context) []Sample {
		return []Sample{{Value: 3, Attrs: []attribute.KeyValue{attribute.String("type", "run_query")}}}
	}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	for _, n := range []string{"intelligence_request_seconds", "nats_reconnects_total", "natsapi_refused_replies_total", "jobs_queue_depth"} {
		if !seen[n] {
			t.Fatalf("metric %s missing: %v", n, seen)
		}
	}
}

func TestHTTPServerSpanUsesPatternAndTraceparent(t *testing.T) {
	exp, _ := setup(t)
	h := HTTPServer("GET", "/api/v1/queries/{id}", func(context.Context) string { return "rid-1" },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !trace.SpanContextFromContext(r.Context()).IsValid() {
				t.Error("no span in handler")
			}
			w.WriteHeader(404)
		}))
	req := httptest.NewRequest("GET", "/api/v1/queries/"+marker, nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("baggage", "k="+marker)
	h.ServeHTTP(httptest.NewRecorder(), req)
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "api GET /api/v1/queries/{id}" {
		t.Fatalf("spans: %+v", spans)
	}
	if spans[0].Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatal("not parented on the traceparent")
	}
	for _, kv := range spans[0].Attributes {
		if strings.Contains(kv.Value.Emit(), marker) {
			t.Fatal("marker in attributes")
		}
	}
}
