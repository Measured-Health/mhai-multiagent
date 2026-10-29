package telemetry

import (
	"context"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The attribute allow-list (spec §10.3, preflight F15). It is a filtering
// exporter, not a span processor: a processor's OnEnd gets a read-only span
// and cannot remove an attribute. Every span passes through FilterExporter
// on its way out, and the filter keeps only:
//
//   - attributes whose key is in allowed AND whose value has the shape that
//     key promises (an id, a code, a number); anything else is dropped, not
//     redacted, so a free-text value can't ride in under an allowed key;
//   - the span name, if it starts with one of the fixed prefixes core
//     itself uses (names are built from the operation table and route
//     patterns, never from input);
//   - events named "exception", reduced to their exception.type;
//   - links as span contexts only, with no attributes;
//   - the status code, with no description.
//
// Resource attributes are limited to service.name and service.version.
var (
	reID      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	reUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reWord    = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	reModel   = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
	reExcType = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,79}$`)
)

type shape int

const (
	shID shape = iota
	shUUID
	shWord
	shModel
	shCount // a small non-negative integer
	shMS    // a non-negative number
	shStatus
)

// allowed maps each attribute key that may leave the process to the shape
// of its value.
var allowed = map[attribute.Key]shape{
	"request_id":    shID,
	"hospital_id":   shUUID,
	"query_id":      shUUID,
	"job_id":        shUUID,
	"operation":     shWord,
	"service":       shWord,
	"job_type":      shWord,
	"stage":         shWord,
	"error_code":    shWord,
	"status":        shStatus,
	"attempt":       shCount,
	"model":         shModel,
	"model_version": shModel,
	"timing_ms":     shMS,
}

// AllowedKeys returns the attribute keys that survive the filter.
func AllowedKeys() []string {
	out := make([]string, 0, len(allowed))
	for k := range allowed {
		out = append(out, string(k))
	}
	return out
}

func valueOK(s shape, v attribute.Value) bool {
	switch v.Type() {
	case attribute.STRING:
		str := v.AsString()
		switch s {
		case shID:
			return reID.MatchString(str)
		case shUUID:
			return reUUID.MatchString(str)
		case shWord, shStatus:
			return reWord.MatchString(str)
		case shModel:
			return reModel.MatchString(str)
		}
	case attribute.INT64:
		n := v.AsInt64()
		switch s {
		case shCount:
			return n >= 0 && n <= 1_000_000
		case shMS:
			return n >= 0
		case shStatus:
			return n >= 100 && n <= 999
		}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return s == shMS && f >= 0 && f < 1e12
	}
	return false
}

// FilterAttributes returns the attributes of in that pass the allow-list.
func FilterAttributes(in []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, kv := range in {
		if s, ok := allowed[kv.Key]; ok && valueOK(s, kv.Value) {
			out = append(out, kv)
		}
	}
	return out
}

// namePrefixes are the fixed beginnings of every span name core creates.
var namePrefixes = []string{
	"intelligence.request ", "intelligence.publish ", "intelligence.drain", "intelligence.dead_letter",
	"api ", "grant ", "grant", "boundary.verify", "handle ", "stage ",
}

var reName = regexp.MustCompile(`^[A-Za-z0-9_.:/{}<> -]{1,160}$`)

// unnamed replaces a span name that isn't one of ours.
const unnamed = "unnamed"

// SafeName returns name if it is built on a fixed prefix and uses only
// route-pattern characters, else "unnamed".
func SafeName(name string) string {
	if !reName.MatchString(name) {
		return unnamed
	}
	for _, p := range namePrefixes {
		if strings.HasPrefix(name, p) {
			return name
		}
	}
	return unnamed
}

// FilterExporter wraps a SpanExporter so every span it exports has been
// through the allow-list.
type FilterExporter struct{ Next sdktrace.SpanExporter }

// ExportSpans filters spans and passes them on.
func (f FilterExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = filtered{ReadOnlySpan: s}
	}
	return f.Next.ExportSpans(ctx, out)
}

// Shutdown shuts the wrapped exporter down.
func (f FilterExporter) Shutdown(ctx context.Context) error { return f.Next.Shutdown(ctx) }

// filtered is a ReadOnlySpan whose free-text parts are removed. The
// embedded span supplies the SDK's unexported methods.
type filtered struct{ sdktrace.ReadOnlySpan }

func (s filtered) Name() string { return SafeName(s.ReadOnlySpan.Name()) }

func (s filtered) Attributes() []attribute.KeyValue {
	return FilterAttributes(s.ReadOnlySpan.Attributes())
}

func (s filtered) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = ""
	return st
}

func (s filtered) Events() []sdktrace.Event {
	var out []sdktrace.Event
	for _, e := range s.ReadOnlySpan.Events() {
		if e.Name != "exception" {
			continue
		}
		ne := sdktrace.Event{Name: "exception", Time: e.Time}
		for _, kv := range e.Attributes {
			if kv.Key == "exception.type" && kv.Value.Type() == attribute.STRING && reExcType.MatchString(kv.Value.AsString()) {
				ne.Attributes = []attribute.KeyValue{kv}
			}
		}
		out = append(out, ne)
	}
	return out
}

func (s filtered) DroppedAttributes() int { return 0 }
func (s filtered) DroppedEvents() int     { return 0 }

func (s filtered) Links() []sdktrace.Link {
	in := s.ReadOnlySpan.Links()
	out := make([]sdktrace.Link, len(in))
	for i, l := range in {
		out[i] = sdktrace.Link{SpanContext: l.SpanContext}
	}
	return out
}
