package bus

import (
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// Link follows the state of core's NATS connection for readiness (closes
// TODO T3 M6): a disconnect or a closed connection makes /readyz answer
// not_ready until the client reconnects. The handlers log a fixed line
// (never the server's error text, which can name an address) and count
// reconnects for nats_reconnects_total.
type Link struct {
	log        *slog.Logger
	down       atomic.Bool
	reconnects atomic.Int64
}

// NewLink returns a link that is up. A nil log logs nowhere.
func NewLink(log *slog.Logger) *Link {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Link{log: log}
}

// Options are the connection options to pass to Connect.
func (l *Link) Options() []nats.Option {
	return []nats.Option{
		nats.DisconnectErrHandler(l.OnDisconnect),
		nats.ReconnectHandler(l.OnReconnect),
		nats.ClosedHandler(l.OnClosed),
	}
}

// Up reports whether the connection is believed up.
func (l *Link) Up() bool { return !l.down.Load() }

// Reconnects is how many times the connection came back.
func (l *Link) Reconnects() int64 { return l.reconnects.Load() }

// OnDisconnect is the nats.DisconnectErrHandler.
func (l *Link) OnDisconnect(_ *nats.Conn, _ error) {
	l.down.Store(true)
	l.log.Warn("nats: disconnected; not ready until it reconnects")
}

// OnReconnect is the nats.ReconnectHandler.
func (l *Link) OnReconnect(_ *nats.Conn) {
	l.down.Store(false)
	l.reconnects.Add(1)
	telemetry.NATSReconnected()
	l.log.Info("nats: reconnected")
}

// OnClosed is the nats.ClosedHandler: the client gave up for good.
func (l *Link) OnClosed(_ *nats.Conn) {
	l.down.Store(true)
	l.log.Error("nats: connection closed")
}
