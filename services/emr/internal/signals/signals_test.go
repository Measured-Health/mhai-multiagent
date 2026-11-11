package signals

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

type capture struct {
	subject, msgID string
	data           []byte
}

func (c *capture) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	c.subject, c.data, c.msgID = subject, data, msgID
	return nil
}

// A22 part: a signal is ids and a closed kind; nothing else fits.
func TestSignalCarriesIdsOnly(t *testing.T) {
	h, c, ref := uuid.New(), uuid.New(), uuid.New()
	var p capture
	if err := Send(context.Background(), &p, h, Signal{Kind: "possible_match", ConnectionID: c, RefID: ref}); err != nil {
		t.Fatal(err)
	}
	if p.subject != "emr.v1."+h.String()+".signal" || p.msgID != "possible_match:"+ref.String() {
		t.Fatalf("subject %s id %s", p.subject, p.msgID)
	}
	var s emrpb.Signal
	if err := proto.Unmarshal(p.data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "possible_match" || s.ConnectionId != c.String() || s.RefId != ref.String() || s.Code != "" {
		t.Fatalf("signal %v", &s)
	}
	// The message has exactly the four fields of ids and codes.
	if n := s.ProtoReflect().Descriptor().Fields().Len(); n != 4 {
		t.Errorf("Signal has %d fields", n)
	}
	for _, bad := range []Signal{{Kind: "patient_name", RefID: ref}, {Kind: "possible_match"}, {Kind: "", RefID: ref}} {
		if err := Send(context.Background(), &p, h, bad); !errors.Is(err, ErrSignal) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if err := Send(context.Background(), &p, uuid.Nil, Signal{Kind: "possible_match", RefID: ref}); !errors.Is(err, ErrSignal) {
		t.Errorf("no hospital: %v", err)
	}
}
