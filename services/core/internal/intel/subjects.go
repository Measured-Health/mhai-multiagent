package intel

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// SubjectLoader reads who submitted a job's subject and who decided it, from
// the database, inside tx (which runs under the caller's hospital, so RLS
// scopes the read). A subject that doesn't exist for the hospital is
// pgx.ErrNoRows. decidedBy is uuid.Nil while nobody has decided; the
// publisher treats that as a refusal.
type SubjectLoader func(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID) (submittedBy, decidedBy uuid.UUID, err error)

// subjectSource says how to find a second_person job's subject: the id in
// the job message, and the loader that reads its two people.
type subjectSource struct {
	subjectID func(proto.Message) (uuid.UUID, bool)
	load      SubjectLoader
}

// subjectSources is the loader registry, one per second_person operation.
var subjectSources = map[string]subjectSource{
	"pipeline.approve_correction": {
		subjectID: func(m proto.Message) (uuid.UUID, bool) {
			j, ok := m.(*intelpb.ApproveCorrectionJob)
			if !ok {
				return uuid.Nil, false
			}
			id, err := uuid.Parse(j.GetCorrectionId())
			return id, err == nil && id != uuid.Nil
		},
		load: loadCorrection,
	},
}

// loadCorrection reads a correction's submitter and decider. Only an
// approved correction has an approver: a rejected one has a decider too,
// but approving it is not what happened, so its decider is nobody here.
func loadCorrection(ctx context.Context, tx pgx.Tx, id uuid.UUID) (submittedBy, decidedBy uuid.UUID, err error) {
	var status string
	var decider *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT submitted_by, decided_by, status FROM feedback_events WHERE id = $1 AND action = 'correct'`, id).
		Scan(&submittedBy, &decider, &status)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if decider != nil && status == "approved" {
		decidedBy = *decider
	}
	return submittedBy, decidedBy, nil
}

// checkSubjectSources holds the table and the registry together: every
// second_person operation has a complete source, and nothing else has one.
func checkSubjectSources(ops []authz.Operation, sources map[string]subjectSource) error {
	need := map[string]bool{}
	for _, o := range ops {
		if !o.SecondPerson {
			continue
		}
		need[o.Name] = true
		if s, ok := sources[o.Name]; !ok || s.load == nil || s.subjectID == nil {
			return fmt.Errorf("intel: second_person operation %s has no subject loader", o.Name)
		}
	}
	for name := range sources {
		if !need[name] {
			return fmt.Errorf("intel: subject loader for %s, which is not a second_person operation", name)
		}
	}
	return nil
}

// A second_person row with no loader stops the binary at start-up.
func init() {
	if err := checkSubjectSources(authz.Operations(), subjectSources); err != nil {
		panic(err)
	}
}

// secondPersonAllowed is the rule (C28): the caller is the decider, and the
// decider is not the submitter. A null decider or submitter (uuid.Nil), a
// caller that isn't a user id, and svc:core are refused.
func secondPersonAllowed(sub string, submittedBy, decidedBy uuid.UUID) bool {
	if sub == reqtoken.SvcCore || submittedBy == uuid.Nil || decidedBy == uuid.Nil {
		return false
	}
	caller, err := uuid.Parse(sub)
	if err != nil || caller == uuid.Nil || caller.String() != sub {
		return false
	}
	return decidedBy == caller && decidedBy != submittedBy
}

// checkSecondPerson loads msg's subject under the caller's hospital and
// applies the rule. It runs its own short transaction (db.WithHospital) on
// c.Pool, so PublishJob needs no transaction from its caller. ErrMisuse: no
// pool, no hospital, or a message that names no subject. ErrSecondPerson:
// the subject isn't there for this hospital, or the rule refuses. A failed
// read is ErrPublish (unavailable); the driver's error is never passed on.
func (c *Client) checkSecondPerson(ctx context.Context, op authz.Operation, caller Caller, msg proto.Message) error {
	src, ok := subjectSources[op.Name]
	if !ok || c.Pool == nil {
		return ErrMisuse
	}
	subject, ok := src.subjectID(msg)
	if !ok {
		return ErrMisuse
	}
	hid, err := uuid.Parse(caller.Hid)
	if err != nil || hid == uuid.Nil {
		return ErrNoHospital
	}
	var submittedBy, decidedBy uuid.UUID
	err = db.WithHospital(ctx, c.Pool, hid, func(tx pgx.Tx) error {
		var err error
		submittedBy, decidedBy, err = src.load(ctx, tx, subject)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrSecondPerson
	case err != nil:
		return ErrPublish
	}
	if !secondPersonAllowed(caller.Sub, submittedBy, decidedBy) {
		return ErrSecondPerson
	}
	return nil
}
