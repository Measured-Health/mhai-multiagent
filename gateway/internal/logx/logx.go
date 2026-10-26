// Package logx builds the gateway's structured logger. Attributes that could
// carry credentials or personal data are replaced with "[redacted]".
package logx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
)

const redacted = "[redacted]"

// sensitive holds normalised keys: lowercase, with "-" and "_" removed.
var sensitive = map[string]bool{
	"password":      true,
	"token":         true,
	"code":          true,
	"email":         true,
	"accesstoken":   true,
	"refreshtoken":  true,
	"mfatoken":      true,
	"backupcode":    true,
	"authorization": true,
	"cookie":        true,
	"setcookie":     true,
	"secret":        true,
	"otp":           true,
	"totp":          true,
	"mfacode":       true,
	"apikey":        true,
	"recoverycodes": true,
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, "_", "")
	return sensitive[k]
}

// New returns a JSON logger that redacts sensitive attributes, including
// those inside groups.
func New(w io.Writer) *slog.Logger {
	return slog.New(&redactingHandler{next: slog.NewJSONHandler(w, nil)})
}

type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redact(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redact(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func redact(a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		clean := make([]slog.Attr, len(group))
		for i, g := range group {
			clean[i] = redact(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		a.Value = redactAny(a.Value)
	}
	return a
}

// redactAny cleans maps, structs, slices and pointers by round-tripping them
// through JSON (so struct fields match by their JSON names) and redacting
// sensitive keys at every depth. Other values, such as errors, pass through.
func redactAny(v slog.Value) slog.Value {
	x := v.Any()
	if x == nil {
		return v
	}
	switch reflect.ValueOf(x).Kind() {
	case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array, reflect.Ptr:
	default:
		return v
	}
	if _, ok := x.(error); ok {
		return v
	}
	b, err := json.Marshal(x)
	if err != nil {
		return slog.StringValue("[unloggable]")
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return slog.StringValue("[unloggable]")
	}
	return slog.AnyValue(scrub(generic))
}

func scrub(x any) any {
	switch t := x.(type) {
	case map[string]any:
		for k, v := range t {
			if isSensitive(k) {
				t[k] = redacted
			} else {
				t[k] = scrub(v)
			}
		}
		return t
	case []any:
		for i, v := range t {
			t[i] = scrub(v)
		}
		return t
	}
	return x
}
