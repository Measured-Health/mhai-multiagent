package normalise

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

type fakeMsg struct {
	subject              string
	data                 []byte
	deliveries           uint64
	acked, naked, termed bool
	delay                time.Duration
}

func (m *fakeMsg) Subject() string    { return m.subject }
func (m *fakeMsg) Data() []byte       { return m.data }
func (m *fakeMsg) Ack() error         { m.acked = true; return nil }
func (m *fakeMsg) Term() error        { m.termed = true; return nil }
func (m *fakeMsg) Deliveries() uint64 { return m.deliveries }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.naked, m.delay = true, d
	return nil
}

type pubRec struct {
	subject, id string
	data        []byte
}

type pubs struct{ got []pubRec }

func (p *pubs) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	p.got = append(p.got, pubRec{subject, msgID, data})
	return nil
}

// A batch that keeps failing (here: no database) is retried with a delay
// four times; the 5th failure dead-letters it, ids and a code only, and
// ends the message.
func TestNormaliseDeadLetterAfterFive(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	p := &pubs{}
	n := &Normaliser{Pool: pool, Pub: p}
	h, raw := uuid.New(), uuid.New()
	data, _ := proto.Marshal(&emrpb.Fetched{RawIds: []string{raw.String(), uuid.NewString()}})
	for d := uint64(1); d <= 5; d++ {
		m := &fakeMsg{subject: "emr.v1." + h.String() + ".fetched", data: data, deliveries: d}
		n.HandleMsg(context.Background(), m)
		if d < 5 && (!m.naked || m.acked || m.termed || m.delay <= 0 || len(p.got) != 0) {
			t.Fatalf("delivery %d: %+v, published %d", d, m, len(p.got))
		}
		if d == 5 && (!m.termed || m.naked || m.acked) {
			t.Fatalf("delivery 5: %+v", m)
		}
	}
	if len(p.got) != 1 || p.got[0].subject != "dead.v1."+h.String()+".emr_normalise" {
		t.Fatalf("dead letters %v", p.got)
	}
	var dl emrpb.DeadLetter
	if err := proto.Unmarshal(p.got[0].data, &dl); err != nil || dl.Stage != "normalise" || dl.BatchId != raw.String() ||
		dl.ErrorCode != "normalise_failed" {
		t.Fatalf("letter %v %v", &dl, err)
	}
	if strings.Contains(string(p.got[0].data), "postgres") {
		t.Error("the letter carries the error text")
	}
}

// A malformed body or over-long batch is acked and counted, never retried.
func TestMalformedFetchedAcked(t *testing.T) {
	n := &Normaliser{Pub: &pubs{}}
	h := uuid.New()
	many := &emrpb.Fetched{}
	for i := 0; i < 501; i++ {
		many.RawIds = append(many.RawIds, uuid.NewString())
	}
	big, _ := proto.Marshal(many)
	bad, _ := proto.Marshal(&emrpb.Fetched{RawIds: []string{"not-an-id"}})
	for name, data := range map[string][]byte{"garbage": []byte("\xff\xff"), "empty": nil, "501 ids": big, "bad id": bad} {
		m := &fakeMsg{subject: "emr.v1." + h.String() + ".fetched", data: data, deliveries: 1}
		n.HandleMsg(context.Background(), m)
		if !m.acked || m.naked {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if n.metrics().Refused.Load() != 4 {
		t.Errorf("refused %d", n.metrics().Refused.Load())
	}
}
