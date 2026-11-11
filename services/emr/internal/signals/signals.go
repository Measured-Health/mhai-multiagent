// Package signals publishes the emr service's id-only signals to core,
// emr.v1.<hid>.signal (EMR design §3.2; plan Task 20). The emr service has
// no access to the audit chain: what must be recorded or shown (a possible
// match, a source merge request, a breaker trip) reaches core this way and
// core records it in its own transaction.
package signals

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// Kinds is the closed set of signal kinds (plan Global Constraints).
var Kinds = []string{"possible_match", "source_merge", "breaker_tripped", "document_rejected", "rule_set_activated"}

// ErrSignal: a kind outside Kinds, or a missing id.
var ErrSignal = errors.New("signals: not a signal")

// Publisher publishes one message with a de-duplication id.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

// Signal is one signal to send.
type Signal struct {
	Kind         string
	ConnectionID uuid.UUID
	RefID        uuid.UUID
}

// Send publishes s for hospital hid, with Nats-Msg-Id kind:ref_id so a
// repeat within the stream's window is dropped.
func Send(ctx context.Context, pub Publisher, hid uuid.UUID, s Signal) error {
	if !slices.Contains(Kinds, s.Kind) || hid == uuid.Nil || s.RefID == uuid.Nil {
		return ErrSignal
	}
	msg := &emrpb.Signal{Kind: s.Kind, RefId: s.RefID.String()}
	if s.ConnectionID != uuid.Nil {
		msg.ConnectionId = s.ConnectionID.String()
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return pub.Publish(ctx, "emr.v1."+hid.String()+".signal", b, s.Kind+":"+s.RefID.String())
}
