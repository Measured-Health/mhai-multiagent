// Package telemetry is core's OpenTelemetry: traces and metrics, off unless
// an exporter endpoint is configured (spec §10.3, plan 3 task 14).
//
// Nothing here uses the OpenTelemetry globals: Setup installs providers in
// this package's own holder, so the code that records a span or a metric
// works the same with or without a collector (the no-op providers) and a
// test can install an in-memory exporter, then put the no-ops back.
//
// What may leave the process is decided in allowlist.go; this file only
// wires exporters. Spans carry ids, codes and numbers, never prompt text,
// answers, patient fields or tokens.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	serviceName = "mhai-core"
	scopeName   = "mhai/core"
)

var errEndpoint = errors.New("telemetry: the exporter endpoint is not a valid URL or host:port")

type providers struct {
	tp trace.TracerProvider
	mp metric.MeterProvider
	in *instruments
}

type instruments struct {
	request    metric.Float64Histogram
	reconnects metric.Int64Counter
}

var cur atomic.Pointer[providers]

func get() *providers {
	if p := cur.Load(); p != nil {
		return p
	}
	return &providers{tp: tracenoop.NewTracerProvider(), mp: metricnoop.NewMeterProvider()}
}

// Tracer returns core's tracer: a no-op until Setup.
func Tracer() trace.Tracer { return get().tp.Tracer(scopeName) }

// Setup installs OTLP/gRPC exporters for traces and metrics at endpoint
// (OTEL_EXPORTER_OTLP_ENDPOINT: a URL, or host:port). An empty endpoint
// turns telemetry off: nothing is installed, nothing connects, and the
// returned shutdown does nothing. A collector that is down never fails a
// request: exporting is in the background and drops on error.
//
// Spans go through the attribute allow-list before the exporter.
func Setup(ctx context.Context, endpoint string) (shutdown func(context.Context) error, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	u, perr := url.Parse(endpoint)
	if perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errEndpoint
	}
	texp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, errors.New("telemetry: trace exporter failed")
	}
	mexp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(endpoint))
	if err != nil {
		_ = texp.Shutdown(ctx)
		return nil, errors.New("telemetry: metric exporter failed")
	}
	return install(sdktrace.WithBatcher(FilterExporter{Next: texp}),
		sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(15*time.Second)))
}

// SetupWith installs exp (behind the allow-list, exported synchronously)
// and reader, for tests.
func SetupWith(exp sdktrace.SpanExporter, reader sdkmetric.Reader) (shutdown func(context.Context) error, err error) {
	return install(sdktrace.WithSyncer(FilterExporter{Next: exp}), reader)
}

func install(traceOpt sdktrace.TracerProviderOption, reader sdkmetric.Reader) (func(context.Context) error, error) {
	res := resource.NewSchemaless(attribute.String("service.name", serviceName))
	tp := sdktrace.NewTracerProvider(traceOpt, sdktrace.WithResource(res))
	mopts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if reader != nil {
		mopts = append(mopts, sdkmetric.WithReader(reader))
	}
	mp := sdkmetric.NewMeterProvider(mopts...)
	m := mp.Meter(scopeName)
	in := &instruments{}
	var err error
	if in.request, err = m.Float64Histogram("intelligence_request_seconds",
		metric.WithDescription("Latency of request/reply calls to the intelligence workers."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60)); err != nil {
		return nil, err
	}
	if in.reconnects, err = m.Int64Counter("nats_reconnects_total",
		metric.WithDescription("Times core's NATS connection reconnected.")); err != nil {
		return nil, err
	}
	p := &providers{tp: tp, mp: mp, in: in}
	cur.Store(p)
	return func(ctx context.Context) error {
		cur.CompareAndSwap(p, nil)
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Sample is one observed value of an observable metric.
type Sample struct {
	Value float64
	Attrs []attribute.KeyValue
}

// Observe registers an observable metric read at each collection. A
// monotonic metric is a counter (name it ..._total), else a gauge. Labels
// must be low-cardinality: never a user, query or hospital id. Call it
// after Setup; with telemetry off it does nothing.
func Observe(name, description string, monotonic bool, read func(context.Context) []Sample) error {
	p := cur.Load()
	if p == nil {
		return nil
	}
	m := p.mp.Meter(scopeName)
	cb := metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
		for _, s := range read(ctx) {
			o.Observe(s.Value, metric.WithAttributes(s.Attrs...))
		}
		return nil
	})
	var err error
	if monotonic {
		_, err = m.Float64ObservableCounter(name, metric.WithDescription(description), cb)
	} else {
		_, err = m.Float64ObservableGauge(name, metric.WithDescription(description), cb)
	}
	return err
}

// ObserveCount registers a monotonic counter read from an in-process
// counter, with fixed labels.
func ObserveCount(name, description string, read func() int64, attrs ...attribute.KeyValue) error {
	return Observe(name, description, true, func(context.Context) []Sample {
		return []Sample{{Value: float64(read()), Attrs: attrs}}
	})
}

// ObserveRequest records one request/reply call's latency, by operation
// name (a closed set from the operation table).
func ObserveRequest(operation string, d time.Duration) {
	if in := get().in; in != nil {
		in.request.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("operation", operation)))
	}
}

// NATSReconnected counts one reconnect.
func NATSReconnected() {
	if in := get().in; in != nil {
		in.reconnects.Add(context.Background(), 1)
	}
}

// Start begins a span named name (a fixed string: see SafeName) of kind,
// with attrs. Only allow-listed attributes ever leave the process.
func Start(ctx context.Context, kind trace.SpanKind, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
}

// End finishes span with a status: code "ok" or "" is success; any other
// code marks the span an error and is recorded as error_code. The status
// description is always empty.
func End(span trace.Span, code string) {
	if code == "" || code == "ok" {
		span.SetAttributes(attribute.String("status", "ok"))
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetAttributes(attribute.String("status", "error"), attribute.String("error_code", code))
		span.SetStatus(codes.Error, "")
	}
	span.End()
}

// Record emits a span for work that already happened, from start until
// now: for a background loop that should leave no span on an idle tick.
func Record(ctx context.Context, name string, start time.Time, code string, attrs ...attribute.KeyValue) {
	_, span := Tracer().Start(ctx, name, trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	End(span, code)
}

// HTTPServer wraps next in a server span named "api <method> <pattern>"
// (the registered route pattern, never the request path). The parent is
// the traceparent header, if valid; baggage is ignored. rid reads the
// request id from the context (it may be nil).
func HTTPServer(method, pattern string, rid func(context.Context) string, next http.Handler) http.Handler {
	name := "api " + method + " " + pattern
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Start(ctx, trace.SpanKindServer, name)
		if rid != nil {
			span.SetAttributes(attribute.String("request_id", rid(ctx)))
		}
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			st := sw.status
			if st == 0 {
				st = http.StatusOK
			}
			span.SetAttributes(attribute.Int("status", st))
			if st >= 500 {
				span.SetAttributes(attribute.String("error_code", "internal"))
				span.SetStatus(codes.Error, "")
			}
			span.End()
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
