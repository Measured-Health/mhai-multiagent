package notify

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestSubjectAndKick(t *testing.T) {
	h := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	if Subject(h) != "notify.v1.22222222-2222-4222-8222-222222222222.due" {
		t.Fatal(Subject(h))
	}
	d := New(nil, nil, nil)
	d.Kick()
	d.Kick() // a pending kick is enough; never blocks
	if len(d.kick) != 1 {
		t.Fatalf("%d kicks pending", len(d.kick))
	}
	var nilD *Drainer
	nilD.Kick()
	(&Drainer{}).Kick()
	if _, err := d.DrainOnce(context.Background()); err == nil {
		t.Fatal("drained without a pool")
	}
	if _, err := d.Retention(context.Background()); err == nil {
		t.Fatal("retention without a pool")
	}
	if d.log() == nil || (&Drainer{}).metrics() == nil {
		t.Fatal("defaults")
	}
}
