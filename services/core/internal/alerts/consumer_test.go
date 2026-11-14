package alerts_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// fakeMsg records how HandleMsg settled it.
type fakeMsg struct {
	subject    string
	data       []byte
	deliveries uint64
	delErr     error

	mu                 sync.Mutex
	acked, termed, nak int
	nakDelay           time.Duration
}

func (m *fakeMsg) Subject() string { return m.subject }
func (m *fakeMsg) Data() []byte    { return m.data }
func (m *fakeMsg) Ack() error      { m.mu.Lock(); m.acked++; m.mu.Unlock(); return nil }
func (m *fakeMsg) Term() error     { m.mu.Lock(); m.termed++; m.mu.Unlock(); return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.mu.Lock()
	m.nak++
	m.nakDelay = d
	m.mu.Unlock()
	return nil
}
func (m *fakeMsg) Deliveries() (uint64, error) { return m.deliveries, m.delErr }

func (m *fakeMsg) settled() (acked, termed, nak int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.termed, m.nak
}

// fakePub records what was published, or fails.
type fakePub struct {
	mu       sync.Mutex
	fail     bool
	subjects []string
	ids      []string
	bodies   [][]byte
}

func (p *fakePub) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("publish refused")
	}
	p.subjects = append(p.subjects, subject)
	p.ids = append(p.ids, msgID)
	p.bodies = append(p.bodies, data)
	return nil
}

func findingMsg(t *testing.T, hid, fid uuid.UUID, deliveries uint64) *fakeMsg {
	t.Helper()
	b, err := proto.Marshal(&emrpb.Finding{FindingId: fid.String()})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeMsg{subject: "emr.v1." + hid.String() + ".finding", data: b, deliveries: deliveries}
}

// A finding that fails is naked on its first four deliveries and
// dead-lettered on the fifth (never skipped); a dead letter that can't be
// sent keeps the message (naked, neither acked nor terminated); a delivery
// count that can't be read naks (never a dead letter on a guess).
func TestConsumerDeadLettersNeverSkips(t *testing.T) {
	hid, fid := uuid.New(), uuid.New()
	failing := func(context.Context, uuid.UUID, uuid.UUID) error { return errors.New("database unavailable") }
	for n := uint64(1); n < alerts.ConsumerMaxDeliver; n++ {
		pub := &fakePub{}
		c := alerts.WithFindingHandler(&alerts.Consumer{Pub: pub, Metrics: &alerts.ConsumerMetrics{}}, failing)
		m := findingMsg(t, hid, fid, n)
		c.HandleMsg(context.Background(), m)
		if a, tm, nk := m.settled(); a != 0 || tm != 0 || nk != 1 || len(pub.subjects) != 0 {
			t.Fatalf("delivery %d: acked %d termed %d naked %d published %d", n, a, tm, nk, len(pub.subjects))
		}
	}
	pub := &fakePub{}
	met := &alerts.ConsumerMetrics{}
	c := alerts.WithFindingHandler(&alerts.Consumer{Pub: pub, Metrics: met}, failing)
	m := findingMsg(t, hid, fid, alerts.ConsumerMaxDeliver)
	c.HandleMsg(context.Background(), m)
	if a, tm, nk := m.settled(); a != 0 || tm != 1 || nk != 0 {
		t.Fatalf("fifth delivery: acked %d termed %d naked %d", a, tm, nk)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "dead.v1."+hid.String()+".core_emr" ||
		pub.ids[0] != "core_emr:finding:"+fid.String() || met.DeadLettered.Load() != 1 {
		t.Fatalf("dead letter: %v %v %d", pub.subjects, pub.ids, met.DeadLettered.Load())
	}
	var dl emrpb.DeadLetter
	if err := proto.Unmarshal(pub.bodies[0], &dl); err != nil || dl.BatchId != fid.String() || dl.Stage != "core" ||
		dl.ErrorCode != "core_finding_failed" {
		t.Fatalf("dead letter body: %+v %v", &dl, err)
	}

	failPub := &fakePub{fail: true}
	met = &alerts.ConsumerMetrics{}
	c = alerts.WithFindingHandler(&alerts.Consumer{Pub: failPub, Metrics: met}, failing)
	m = findingMsg(t, hid, fid, alerts.ConsumerMaxDeliver+3)
	c.HandleMsg(context.Background(), m)
	if a, tm, nk := m.settled(); a != 0 || tm != 0 || nk != 1 || met.DeadFailed.Load() != 1 {
		t.Fatalf("dead letter not sent: acked %d termed %d naked %d failed %d", a, tm, nk, met.DeadFailed.Load())
	}

	m = findingMsg(t, hid, fid, 0)
	m.delErr = errors.New("no metadata")
	c = alerts.WithFindingHandler(&alerts.Consumer{Pub: pub, Metrics: &alerts.ConsumerMetrics{}}, failing)
	c.HandleMsg(context.Background(), m)
	if a, tm, nk := m.settled(); a != 0 || tm != 0 || nk != 1 {
		t.Fatalf("unreadable deliveries: acked %d termed %d naked %d", a, tm, nk)
	}
}

// A finding handled is acked; a message that names no finding (a bad
// subject, another stream's subject, a non-canonical id, a body that isn't
// a Finding) is refused and acked without reaching the handler.
func TestConsumerSettlesAndRefuses(t *testing.T) {
	hid, fid := uuid.New(), uuid.New()
	var calls int
	var mu sync.Mutex
	ok := func(_ context.Context, h, f uuid.UUID) error {
		mu.Lock()
		defer mu.Unlock()
		if h != hid || f != fid {
			t.Errorf("handler got %s %s", h, f)
		}
		calls++
		return nil
	}
	c := alerts.WithFindingHandler(&alerts.Consumer{Metrics: &alerts.ConsumerMetrics{}}, ok)
	m := findingMsg(t, hid, fid, 1)
	c.HandleMsg(context.Background(), m)
	if a, _, _ := m.settled(); a != 1 || calls != 1 {
		t.Fatalf("handled: acked %d calls %d", a, calls)
	}
	good, _ := proto.Marshal(&emrpb.Finding{FindingId: fid.String()})
	upper, _ := proto.Marshal(&emrpb.Finding{FindingId: "A" + fid.String()[1:]})
	for _, bad := range []*fakeMsg{
		{subject: "emr.v1." + hid.String() + ".fetched", data: good},
		{subject: "records.v1." + hid.String() + ".finding", data: good},
		{subject: "emr.v1.not-a-uuid.finding", data: good},
		{subject: "emr.v1." + hid.String() + ".finding.x", data: good},
		{subject: "emr.v1." + hid.String() + ".finding", data: []byte{0xff, 0xff}},
		{subject: "emr.v1." + hid.String() + ".finding", data: upper},
	} {
		met := &alerts.ConsumerMetrics{}
		c := alerts.WithFindingHandler(&alerts.Consumer{Metrics: met}, ok)
		c.HandleMsg(context.Background(), bad)
		if a, tm, nk := bad.settled(); a != 1 || tm != 0 || nk != 0 || met.Refused.Load() != 1 {
			t.Errorf("%s: acked %d termed %d naked %d refused %d", bad.subject, a, tm, nk, met.Refused.Load())
		}
	}
	if calls != 1 {
		t.Fatalf("the handler ran for a refused message: %d", calls)
	}
}
