package rules

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// fakeMsg is a JetStream message for HandleMsg.
type fakeMsg struct {
	subject              string
	data                 []byte
	deliveries           uint64
	acked, naked, termed bool
	delay                time.Duration
	derr                 error
}

func (m *fakeMsg) Subject() string             { return m.subject }
func (m *fakeMsg) Data() []byte                { return m.data }
func (m *fakeMsg) Ack() error                  { m.acked = true; return nil }
func (m *fakeMsg) Term() error                 { m.termed = true; return nil }
func (m *fakeMsg) Deliveries() (uint64, error) { return m.deliveries, m.derr }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.naked, m.delay = true, d
	return nil
}

func createdBody(ids ...string) []byte {
	b, _ := proto.Marshal(&emrpb.Created{RecordIds: ids})
	return b
}

// A message that isn't a records.v1.<hid>.created of ids is acked with
// nothing done (and counted): never retried, never evaluated.
func TestHandleMsgRefusesBadSubjectsAndBodies(t *testing.T) {
	e := &Engine{}
	hid := uuid.NewString()
	many := make([]string, 501)
	for i := range many {
		many[i] = uuid.NewString()
	}
	for name, m := range map[string]*fakeMsg{
		"other subject": {subject: "emr.v1." + hid + ".fetched", data: createdBody(uuid.NewString())},
		"bad hospital":  {subject: "records.v1.not-a-uuid.created", data: createdBody(uuid.NewString())},
		"upper case":    {subject: "records.v1." + strings.ToUpper(hid) + ".created", data: createdBody(uuid.NewString())},
		"extra token":   {subject: "records.v1." + hid + ".created.x", data: createdBody(uuid.NewString())},
		"not protobuf":  {subject: "records.v1." + hid + ".created", data: []byte{0xff, 0xff, 0xff}},
		"no ids":        {subject: "records.v1." + hid + ".created", data: createdBody()},
		"not an id":     {subject: "records.v1." + hid + ".created", data: createdBody("x")},
		"too many ids":  {subject: "records.v1." + hid + ".created", data: createdBody(many...)},
	} {
		t.Run(name, func(t *testing.T) {
			m.deliveries = 1
			e.HandleMsg(context.Background(), m)
			if !m.acked || m.naked || m.termed {
				t.Fatalf("%+v", m)
			}
		})
	}
	if e.metrics().Refused.Load() != 8 {
		t.Fatalf("refused %d", e.metrics().Refused.Load())
	}
}
