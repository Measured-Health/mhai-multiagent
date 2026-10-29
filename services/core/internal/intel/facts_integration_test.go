//go:build integration

package intel

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

func fact(action, entity, entityID string, payload map[string]any) *intelpb.AuditFact {
	p, err := structpb.NewStruct(payload)
	if err != nil {
		panic(err)
	}
	return &intelpb.AuditFact{Action: action, Entity: entity, EntityId: entityID, Payload: p}
}

// F22: facts from a reply go on the chain with the actor, hospital and
// request id core minted, never anything from the fact; a fact can't take a
// core action or carry a sensitive key, and a bad fact writes nothing.
func TestAppendFacts(t *testing.T) {
	ctx := context.Background()
	hid := seedHospital(t, "active")
	user := seedUser(t, hid, true, authz.RoleClinician)
	caller := Caller{Sub: user.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
	entity := uuid.NewString()
	meta := &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{
		// The payload names another actor and hospital: they are data, and
		// the row's actor and hospital stay the caller's.
		fact("intelligence.echo.pinged", "echo", entity, map[string]any{"actor_id": uuid.NewString(), "hospital_id": uuid.NewString(), "n": 1}),
	}}
	if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, meta) }); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows := auditFor(t, "intelligence.echo.pinged", entity)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if *r.actor != user.String() || *r.hospital != hid.String() || *r.requestID != caller.Rid || *r.entity != "echo" {
		t.Fatalf("row %+v", r)
	}
	// Nothing to append is fine.
	if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, nil) }); err != nil {
		t.Fatal(err)
	}

	tooMany := &intelpb.ResponseMeta{}
	for i := 0; i <= MaxFacts; i++ {
		tooMany.AuditFacts = append(tooMany.AuditFacts, fact("intelligence.echo.pinged", "echo", fmt.Sprint(i), nil))
	}
	for name, m := range map[string]*intelpb.ResponseMeta{
		"core's own action":   {AuditFacts: []*intelpb.AuditFact{fact("intelligence.refused", "echo", "x", nil)}},
		"a grant":             {AuditFacts: []*intelpb.AuditFact{fact("intelligence.granted", "echo", "x", nil)}},
		"outside the space":   {AuditFacts: []*intelpb.AuditFact{fact("auth.login.succeeded", "user", "x", nil)}},
		"query outcome":       {AuditFacts: []*intelpb.AuditFact{fact("query.completed", "query", "x", nil)}},
		"bad shape":           {AuditFacts: []*intelpb.AuditFact{fact("intelligence.Echo Pinged", "echo", "x", nil)}},
		"no action":           {AuditFacts: []*intelpb.AuditFact{fact("", "echo", "x", nil)}},
		"long entity id":      {AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", string(make([]byte, 200)), nil)}},
		"sensitive key":       {AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", "x", map[string]any{"email": "a@b.example"})}},
		"nested secret":       {AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", "x", map[string]any{"x": map[string]any{"token": "t"}})}},
		"too many":            tooMany,
		"one good, one wrong": {AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", "y", nil), fact("auth.x", "e", "y", nil)}},
		"nil fact":            {AuditFacts: []*intelpb.AuditFact{nil}},
	} {
		err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, m) })
		if err == nil {
			t.Errorf("%s: appended", name)
		}
		if name == "sensitive key" && !errors.Is(err, audit.ErrSensitiveKey) && !errors.Is(err, ErrBadFact) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The failed transactions rolled back: "y" never landed.
	if n := len(auditFor(t, "intelligence.echo.pinged", "y")); n != 0 {
		t.Fatalf("a refused batch wrote %d rows", n)
	}
	// svc:core facts have no actor.
	sys := System(hid.String(), "rid-"+uuid.NewString())
	e2 := uuid.NewString()
	if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error {
		return AppendFacts(ctx, tx, sys, &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", e2, nil)}})
	}); err != nil {
		t.Fatal(err)
	}
	if rows := auditFor(t, "intelligence.echo.pinged", e2); len(rows) != 1 || rows[0].actor != nil {
		t.Fatalf("svc:core rows %+v", rows)
	}
}

// F22: a refusal core receives (unauthorized, forbidden, invalid_request)
// is audited as intelligence.refused with the operation, the reason and the
// kid core signed with; nothing else is.
func TestAuditRefusal(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	hid := seedHospital(t, "active")
	user := seedUser(t, hid, true, authz.RoleClinician)
	for _, code := range []string{"unauthorized", "forbidden", "invalid_request"} {
		caller := Caller{Sub: user.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
		if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error {
			return c.AuditRefusal(ctx, tx, caller, "echo.ping", fmt.Errorf("wrapped: %w", &Error{Code: code}))
		}); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		rows := auditForRequest(t, "intelligence.refused", caller.Rid)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", code, len(rows))
		}
		r := rows[0]
		if *r.actor != user.String() || *r.hospital != hid.String() || r.payload["operation"] != "echo.ping" ||
			r.payload["reason"] != code || r.payload["kid"] != signer.KeyID || len(r.payload) != 3 {
			t.Fatalf("%s: %+v", code, r)
		}
	}
	for _, err := range []error{nil, ErrTimeout, ErrNoResponders, &Error{Code: "overloaded"}, &Error{Code: "not_found"}} {
		caller := Caller{Sub: user.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
		if e := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return c.AuditRefusal(ctx, tx, caller, "echo.ping", err) }); e != nil {
			t.Fatalf("%v: %v", err, e)
		}
		if n := len(auditForRequest(t, "intelligence.refused", caller.Rid)); n != 0 {
			t.Fatalf("%v audited as a refusal", err)
		}
	}
}

func auditForRequest(t *testing.T, action, rid string) []auditRow {
	t.Helper()
	return auditQuery(t, `request_id`, action, rid)
}

// A12 (request/reply): an echo.ping answer whose audit facts name another
// actor and hospital is appended with the caller's identity, the one core
// minted into the token.
func TestReplyFactsUseTokenIdentity(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	hid := seedHospital(t, "active")
	other := seedHospital(t, "active")
	user := seedUser(t, hid, true, authz.RoleClinician)
	intruder := seedUser(t, other, true, authz.RoleClinician)
	entity := uuid.NewString()
	meta := &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{
		fact("intelligence.echo.pinged", "echo", entity, map[string]any{"actor_id": intruder.String(), "hospital_id": other.String()}),
	}}
	startFakeWorker(t, sharedNATS.EchoURL, "echo", "ping", signer.PublicKeys(), pong(meta))
	caller := Caller{Sub: user.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
	var resp intelpb.PingResponse
	got, err := c.Request(ctx, caller, "echo.ping", &intelpb.PingRequest{Text: "synthetic ping", HospitalId: hid.String()}, &resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, got) }); err != nil {
		t.Fatal(err)
	}
	rows := auditFor(t, "intelligence.echo.pinged", entity)
	if len(rows) != 1 || *rows[0].actor != user.String() || *rows[0].hospital != hid.String() || *rows[0].requestID != caller.Rid {
		t.Fatalf("rows %+v", rows)
	}
}

// T5 note: a database failure appending a fact is core's error
// (ErrAuditWrite), not a bad fact (ErrBadFact): here the role's policy
// refuses the insert.
func TestAppendFactsInfraErrorIsNotBadFact(t *testing.T) {
	ctx := context.Background()
	hid := seedHospital(t, "active")
	user := seedUser(t, hid, true, authz.RoleClinician)
	caller := Caller{Sub: user.String(), Hid: hid.String(), Rid: "rid-" + uuid.NewString(), Roles: []string{authz.RoleClinician}}
	meta := &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", "e", nil)}}
	err := db.WithJobs(ctx, app, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, meta) })
	if !errors.Is(err, ErrAuditWrite) || errors.Is(err, ErrBadFact) {
		t.Fatalf("infra failure: %v", err)
	}
	bad := &intelpb.ResponseMeta{AuditFacts: []*intelpb.AuditFact{fact("intelligence.echo.pinged", "echo", "e", map[string]any{"password": "x"})}}
	err = db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error { return AppendFacts(ctx, tx, caller, bad) })
	if !errors.Is(err, ErrBadFact) {
		t.Fatalf("sensitive key: %v", err)
	}
}
