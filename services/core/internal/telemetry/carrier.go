package telemetry

import (
	"context"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// Propagator carries W3C trace context only (traceparent only).
// Baggage is never propagated: it is caller-controlled free text.
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// The one trace header. It is not a security header: no check reads it, and
// the worker's duplicate-header refusal does not list it. tracestate is
// deliberately absent: it is caller-controlled free text (up to 8 KB) that
// would otherwise be exported and re-injected.
const HeaderTraceparent = "traceparent"

// Header adapts a NATS header to propagation.TextMapCarrier. Only
// traceparent is read or written, by any spelling of the name; every other
// key (tracestate, baggage, Authorization, Mhai-*) is invisible through it.
// A traceparent given more than once reads as absent, so an ambiguous
// message starts a new trace.
type Header nats.Header

func traceKey(k string) (string, bool) {
	if strings.EqualFold(k, HeaderTraceparent) {
		return HeaderTraceparent, true
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
	if h.Get(HeaderTraceparent) != "" {
		out = append(out, HeaderTraceparent)
	}
	return out
}

// Inject writes ctx's traceparent into h.
func Inject(ctx context.Context, h nats.Header) {
	if h != nil {
		Propagator.Inject(ctx, Header(h))
	}
}

// CarrierOf returns a carrier holding only the traceparent of an HTTP
// header, for ingress (tracestate and baggage are never read).
func CarrierOf(h http.Header) propagation.TextMapCarrier {
	m := propagation.MapCarrier{}
	if vs := h.Values(HeaderTraceparent); len(vs) == 1 {
		m.Set(HeaderTraceparent, vs[0])
	}
	return m
}

// Extract returns ctx with the remote span context in h, if it is valid;
// a missing or malformed traceparent leaves ctx as it was.
func Extract(ctx context.Context, h nats.Header) context.Context {
	if h == nil {
		return ctx
	}
	return Propagator.Extract(ctx, Header(h))
}
