//go:build integration

package ctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

func (f *credFixture) coreClaims(hid uuid.UUID) reqtoken.Claims {
	return reqtoken.Claims{Aud: "emr", Sub: "svc:core", Hid: hid.String(), Rid: rid, Kind: reqtoken.KindReq,
		Subject: "emr.v1.ctl.sync_now", ReplyPrefix: reqtoken.ReplyCore, Via: reqtoken.ViaCore}
}

// sync_now (svc:core): the connection is due now, unless it is backing off
// a failing source (a 429's wait is never cut short); another hospital's
// connection is not found and nothing is answered.
func TestSyncNowMarksDue(t *testing.T) {
	f := newCredFixture(t)
	ctx := context.Background()
	s := &SyncNow{Pool: f.pool, Now: func() time.Time { return f.now }}
	due := func() time.Time {
		var at time.Time
		if err := f.pool.QueryRow(ctx, `SELECT next_due_at FROM emr.sync_state WHERE connection_id = $1`, f.conn).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	out, err := s.Handle(ctx, f.coreClaims(f.hospital), &emrpb.SyncNowRequest{ConnectionId: f.conn.String()})
	if err != nil || !out.(*emrpb.SyncNowReply).Queued || due().After(f.now) {
		t.Fatalf("no state yet: %v %v %v", out, err, due())
	}
	if _, err := f.pool.Exec(ctx, `UPDATE emr.sync_state SET next_due_at = $2 WHERE connection_id = $1`, f.conn, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	out, err = s.Handle(ctx, f.coreClaims(f.hospital), &emrpb.SyncNowRequest{ConnectionId: f.conn.String()})
	if err != nil || !out.(*emrpb.SyncNowReply).Queued || due().After(f.now) {
		t.Fatalf("due later: %v %v %v", out, err, due())
	}
	later := f.now.Add(15 * time.Minute)
	if _, err := f.pool.Exec(ctx, `UPDATE emr.sync_state SET next_due_at = $2, failures = 2 WHERE connection_id = $1`, f.conn, later); err != nil {
		t.Fatal(err)
	}
	out, err = s.Handle(ctx, f.coreClaims(f.hospital), &emrpb.SyncNowRequest{ConnectionId: f.conn.String()})
	if err != nil || out.(*emrpb.SyncNowReply).Queued || !due().Equal(later) {
		t.Fatalf("backing off: %v %v %v", out, err, due())
	}
	other := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Other Synthetic', $2, 'active')`,
		other, "O"+other.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handle(ctx, f.coreClaims(other), &emrpb.SyncNowRequest{ConnectionId: f.conn.String()}); !errors.Is(err, errConnection) {
		t.Fatalf("another hospital's connection: %v", err)
	}
	if _, err := s.Handle(ctx, f.coreClaims(f.hospital), &emrpb.SyncNowRequest{ConnectionId: "not-a-uuid"}); err == nil {
		t.Fatal("a malformed id was answered")
	}
}

// Design §15.2: a passing test resumes a connection stopped after three
// sign-in refusals.
func TestPassingTestResumesPolling(t *testing.T) {
	f := newCredFixture(t)
	ctx := context.Background()
	if _, err := f.put(t, f.box(t, f.conn, "api_key", credCanary, stubOrigin, f.now)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO emr.sync_state (connection_id, hospital_id, sign_in_failures, failures, next_due_at)
		VALUES ($1, $2, 3, 3, now() + interval '1 hour')`, f.conn, f.hospital); err != nil {
		t.Fatal(err)
	}
	if r := f.test(t); !r.Ok {
		t.Fatalf("test %+v", r)
	}
	var signIn, failures int
	if err := f.pool.QueryRow(ctx, `SELECT sign_in_failures, failures FROM emr.sync_state WHERE connection_id = $1`, f.conn).Scan(&signIn, &failures); err != nil ||
		signIn != 0 || failures != 0 {
		t.Fatalf("after a passing test: %d %d %v", signIn, failures, err)
	}
}
