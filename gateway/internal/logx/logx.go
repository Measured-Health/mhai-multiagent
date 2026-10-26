// Package logx builds the gateway's structured logger. Attributes that could
// carry credentials or personal data are replaced with "[redacted]".
package logx

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

const redacted = "[redacted]"

var sensitive = map[string]bool{
	"password":      true,
	"token":         true,
	"code":          true,
	"email":         true,
	"access_token":  true,
	"refresh_token": true,
	"mfa_token":     true,
	"backup_code":   true,
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
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		clean := make([]slog.Attr, len(group))
		for i, g := range group {
			clean[i] = redact(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	}
	if sensitive[strings.ToLower(a.Key)] {
		return slog.String(a.Key, redacted)
	}
	return a
}
