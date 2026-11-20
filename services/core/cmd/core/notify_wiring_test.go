package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
)

type fakeSender struct{ err error }

func (fakeSender) Channel() string                                { return "log" }
func (f fakeSender) Send(context.Context, delivery.Message) error { return f.err }

// After each send (which runs after the action's commit) the drainer is
// kicked, whatever the send returned: the outbox row is committed either
// way. The channel is the wrapped sender's.
func TestKickSenderKicksAfterSend(t *testing.T) {
	for _, want := range []error{nil, errors.New("synthetic: not sent")} {
		kicks := 0
		k := kickSender{Sender: fakeSender{err: want}, kick: func() { kicks++ }}
		if err := k.Send(context.Background(), delivery.Message{Kind: delivery.MFAReminder}); !errors.Is(err, want) || kicks != 1 {
			t.Fatalf("send: %v, %d kicks", err, kicks)
		}
		if k.Channel() != "log" {
			t.Fatal(k.Channel())
		}
	}
}

// Off (the default), ensureNotify touches no JetStream.
func TestEnsureNotifyOff(t *testing.T) {
	if err := ensureNotify(context.Background(), config.Config{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ensureNotify(context.Background(), config.Config{NotifyEnabled: true, NATSJSReplicas: 1}, nil); err == nil {
		t.Fatal("on without JetStream")
	}
}
