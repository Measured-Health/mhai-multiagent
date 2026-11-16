package scan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

type fakeMsg struct {
	subject    string
	data       []byte
	deliveries uint64
	acked      bool
	termed     bool
	nak        time.Duration
}

func (m *fakeMsg) Subject() string    { return m.subject }
func (m *fakeMsg) Data() []byte       { return m.data }
func (m *fakeMsg) Ack() error         { m.acked = true; return nil }
func (m *fakeMsg) Term() error        { m.termed = true; return nil }
func (m *fakeMsg) InProgress() error  { return nil }
func (m *fakeMsg) Deliveries() uint64 { return m.deliveries }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.nak = d
	return nil
}

func uploaded(t *testing.T, doc string) []byte {
	t.Helper()
	b, err := proto.Marshal(&docspb.Uploaded{DocumentId: doc})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConsumerSettlesMessages(t *testing.T) {
	subj := "documents.v1." + hospital.String() + ".uploaded"
	r := newRig()
	c := &Consumer{Scanner: r.s}
	m := &fakeMsg{subject: subj, data: uploaded(t, document.String()), deliveries: 1}
	c.HandleMsg(context.Background(), m)
	if !m.acked || r.rows.status != "ready" {
		t.Fatalf("clean: %+v %q", m, r.rows.status)
	}
	// Blocked: retried with a back-off, then ended for the sweeper.
	r = newRig()
	r.virus.readyErr = errors.New("down")
	c = &Consumer{Scanner: r.s}
	m = &fakeMsg{subject: subj, data: uploaded(t, document.String()), deliveries: 3}
	c.HandleMsg(context.Background(), m)
	if m.acked || m.nak != 2*time.Minute {
		t.Fatalf("blocked: %+v", m)
	}
	m = &fakeMsg{subject: subj, data: uploaded(t, document.String()), deliveries: MaxDeliver}
	c.HandleMsg(context.Background(), m)
	if !m.termed || m.acked {
		t.Fatalf("past max deliver: %+v", m)
	}
	// Malformed: ended, nothing claimed.
	for _, bad := range []*fakeMsg{
		{subject: "documents.v1.not-a-uuid.uploaded", data: uploaded(t, document.String())},
		{subject: "documents.v1." + uuid.Nil.String() + ".uploaded", data: uploaded(t, document.String())},
		{subject: "documents.v1." + hospital.String() + ".scanned", data: uploaded(t, document.String())},
		{subject: "documents.v1." + hospital.String() + ".uploaded.x", data: uploaded(t, document.String())},
		{subject: subj, data: uploaded(t, "not-a-uuid")},
		{subject: subj, data: uploaded(t, document.String()[:35]+"A")},
		{subject: subj, data: []byte{0xff, 0xff}},
		{subject: subj, data: make([]byte, 300)},
	} {
		r = newRig()
		c = &Consumer{Scanner: r.s}
		c.HandleMsg(context.Background(), bad)
		if !bad.termed || r.rows.lease != uuid.Nil {
			t.Errorf("%s: %+v", bad.subject, bad)
		}
	}
	if c.metrics().Refused.Load() != 1 {
		t.Fatalf("refused %d", c.metrics().Refused.Load())
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[uint64]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute,
		3: 2 * time.Minute, 6: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := Backoff(n); got != want {
			t.Errorf("%d: %v", n, got)
		}
	}
}

func TestSweeperScansDueRows(t *testing.T) {
	r := newRig()
	calls := 0
	s := &Sweeper{Scanner: r.s, Due: func(_ context.Context, limit int) ([]Due, error) {
		calls++
		if limit != 50 {
			t.Fatalf("limit %d", limit)
		}
		return []Due{{Hospital: hospital, Document: document}}, nil
	}}
	n, err := s.RunOnce(context.Background())
	if err != nil || n != 1 || r.rows.status != "ready" {
		t.Fatalf("%d %v %q", n, err, r.rows.status)
	}
	s.Due = func(context.Context, int) ([]Due, error) { return nil, errors.New("database down") }
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("error swallowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx) // returns at once
}
