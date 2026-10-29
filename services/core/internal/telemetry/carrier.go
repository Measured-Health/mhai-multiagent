package telemetry

import (
	"context"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// Propagator carries W3C trace context only (traceparent, tracestate).
// Baggage is never propagated: it is caller-controlled free text.
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// The two trace headers. They are not security headers: no check reads
// them, and the worker's duplicate-header refusal does not list them.
const (
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
)

// Header adapts a NATS header to propagation.TextMapCarrier. Only
// traceparent and tracestate are read or written, by any spelling of the
// name; every other key (baggage, Authorization, Mhai-*) is invisible
// through it. A trace header given more than once reads as absent, so an
// ambiguous message starts a new trace.
type Header nats.Header

func traceKey(k string) (string, bool) {
	switch {
	case strings.EqualFold(k, HeaderTraceparent):
		return HeaderTraceparent, true
	case strings.EqualFold(k, HeaderTracestate):
		return HeaderTracestate, true
	}
	return "", false
}

// Get returns the single value of key, or "".
func (h Header) Get(key string) string {
	want, ok := traceKey(key)
	if !ok {
		return ""
	}
	var vals []string
	for k, vs := range h {
		if n, ok := traceKey(k); ok && n == want {
			vals = append(vals, vs...)
		}
	}
	if len(vals) != 1 {
		return ""
	}
	return vals[0]
}

// Set writes key, replacing any spelling of it. Other keys are ignored.
func (h Header) Set(key, value string) {
	want, ok := traceKey(key)
	if !ok || h == nil {
		return
	}
	for k := range h {
		if n, ok := traceKey(k); ok && n == want {
			delete(h, k)
		}
	}
	h[want] = []string{value}
}

// Keys lists the trace headers present.
func (h Header) Keys() []string {
	var out []string
	for _, k := range []string{HeaderTraceparent, HeaderTracestate} {
		if h.Get(k) != "" {
			out = append(out, k)
		}
	}
	return out
}

// Inject writes ctx's span context into h.
func Inject(ctx context.Context, h nats.Header) {
	if h != nil {
		Propagator.Inject(ctx, Header(h))
	}
}

// Extract returns ctx with the remote span context in h, if it is valid;
// a missing or malformed traceparent leaves ctx as it was.
func Extract(ctx context.Context, h nats.Header) context.Context {
	if h == nil {
		return ctx
	}
	return Propagator.Extract(ctx, Header(h))
}
