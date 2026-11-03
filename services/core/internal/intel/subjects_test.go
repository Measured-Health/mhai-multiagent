package intel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

func noLoad(context.Context, pgx.Tx, uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}

// A second_person operation with no subject loader stops the binary at
// start-up: the table can't name a rule nothing enforces.
func TestSecondPersonNeedsLoader(t *testing.T) {
	op := authz.Operation{Name: "x.y", Service: "x", Op: "y", Kind: reqtoken.KindJob,
		AnyRole: []string{authz.RoleClinician}, SecondPerson: true, Timeout: time.Minute}
	if err := checkSubjectSources([]authz.Operation{op}, map[string]subjectSource{}); err == nil {
		t.Fatal("a second_person row with no loader was accepted")
	}
	loose := map[string]subjectSource{"x.y": {load: noLoad}} // no subject id extractor
	if err := checkSubjectSources([]authz.Operation{op}, loose); err == nil {
		t.Fatal("a loader with no subject id extractor was accepted")
	}
	// A loader for an operation that isn't second_person is a mistake too.
	plain := op
	plain.SecondPerson = false
	if err := checkSubjectSources([]authz.Operation{plain}, map[string]subjectSource{"x.y": {load: noLoad}}); err == nil {
		t.Fatal("a loader on a plain operation was accepted")
	}
	// The real table and registry agree (the init would have panicked).
	if err := checkSubjectSources(authz.Operations(), subjectSources); err != nil {
		t.Fatal(err)
	}
	if _, ok := subjectSources["pipeline.approve_correction"]; !ok {
		t.Fatal("pipeline.approve_correction has no subject loader")
	}
}

// C28, the decision: the caller must be the decider, and the decider must
// not be the submitter. A null decider (uuid.Nil) is nobody.
func TestSecondPersonDecision(t *testing.T) {
	sub, other, caller := uuid.New(), uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		sub                    string
		submittedBy, decidedBy uuid.UUID
		want                   bool
	}{
		"the decider, not the submitter":            {caller.String(), sub, caller, true},
		"the caller is the submitter":               {sub.String(), sub, sub, false},
		"the caller is the submitter, decider null": {sub.String(), sub, uuid.Nil, false},
		"decider null":                              {caller.String(), sub, uuid.Nil, false},
		"someone else decided":                      {caller.String(), sub, other, false},
		"decider is the submitter":                  {sub.String(), sub, sub, false},
		"submitter unknown":                         {caller.String(), uuid.Nil, caller, false},
		"svc:core":                                  {reqtoken.SvcCore, sub, caller, false},
		"caller not a uuid":                         {"nobody", sub, caller, false},
		"empty caller":                              {"", sub, uuid.Nil, false},
	} {
		if got := secondPersonAllowed(tc.sub, tc.submittedBy, tc.decidedBy); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// With no database to look the subject up in, the check fails closed: the
// job is refused, never published.
func TestPublishJobSecondPersonFailsClosedWithoutPool(t *testing.T) {
	c := &Client{}
	hid, id := uuid.New(), uuid.New()
	caller := Caller{Sub: uuid.NewString(), Hid: hid.String(), Rid: "rid-1", Roles: []string{authz.RoleClinician}}
	err := c.checkSecondPerson(context.Background(), mustOp(t, "pipeline.approve_correction"), caller,
		&intelpb.ApproveCorrectionJob{JobId: id.String(), CorrectionId: id.String()})
	if !errors.Is(err, ErrMisuse) {
		t.Fatalf("got %v, want ErrMisuse", err)
	}
	// A message that names no correction can't be checked either.
	err = c.checkSecondPerson(context.Background(), mustOp(t, "pipeline.approve_correction"), caller, &intelpb.ApproveCorrectionJob{})
	if !errors.Is(err, ErrMisuse) {
		t.Fatalf("empty subject: %v", err)
	}
}

func mustOp(t *testing.T, name string) authz.Operation {
	t.Helper()
	op, ok := authz.OperationByName(name)
	if !ok {
		t.Fatalf("no operation %s", name)
	}
	return op
}
