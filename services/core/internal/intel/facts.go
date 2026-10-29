package intel

import (
	"context"
	"errors"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// MaxFacts is the most audit facts one reply may carry.
const MaxFacts = 16

var (
	// A worker's facts live under intelligence.*; core's own actions there
	// (coreActions) and every other namespace are core's to write.
	factAction   = regexp.MustCompile(`^intelligence(\.[a-z0-9_]{1,32}){1,4}$`)
	factEntity   = regexp.MustCompile(`^[a-z0-9_.]{0,64}$`)
	factEntityID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,128}$`)
	coreActions  = map[string]bool{
		"intelligence.refused": true, "intelligence.granted": true,
		"intelligence.grant_refused": true, "intelligence.key_rotated": true,
	}
)

// actor is the audit actor for c: the user, or none for svc:core.
func actor(c Caller) string {
	if c.Sub == reqtoken.SvcCore {
		return ""
	}
	return c.Sub
}

// AppendFacts appends a reply's audit facts (ResponseMeta.audit_facts) to
// the chain inside tx, the caller's own request transaction (authz.Tx, as
// spec §10.2 asks: the same transaction as core's own audit). The actor,
// hospital and request id are the caller's, the ones core minted; a fact
// adds only its action, entity, entity id and payload. The action must be
// one core knows (replyFactSchemas) and the payload must fit its closed
// schema, then it goes through audit.Write's sensitive-key check. Every fact is checked before
// any is written, and an error leaves tx to roll back: a bad fact records
// nothing (F22). ErrBadFact means a fact core won't record; ErrAuditWrite
// means core couldn't write it (an infrastructure error, to retry).
func AppendFacts(ctx context.Context, tx pgx.Tx, caller Caller, meta *intelpb.ResponseMeta) error {
	facts := meta.GetAuditFacts()
	if len(facts) == 0 {
		return nil
	}
	if len(facts) > MaxFacts {
		return ErrBadFact
	}
	payloads := make([]map[string]any, len(facts))
	for i, f := range facts {
		if f == nil || !factAction.MatchString(f.Action) || len(f.Action) > 64 || coreActions[f.Action] ||
			!factEntity.MatchString(f.Entity) || !factEntityID.MatchString(f.EntityId) {
			return ErrBadFact
		}
		// A closed payload schema per action (factschema.go, final review
		// I-1): an action core doesn't know, or a payload key or value
		// outside its schema, is refused. Identity keys are dropped first,
		// as the drainer does: the identity is the caller's.
		schema, known := replyFactSchemas[f.Action]
		if !known {
			return ErrBadFact
		}
		payload := map[string]any{}
		if f.Payload != nil {
			payload = f.Payload.AsMap()
		}
		for _, k := range identityKeys {
			delete(payload, k)
		}
		if !schema.check(payload, uuid.Nil) {
			return ErrBadFact
		}
		payloads[i] = payload
	}
	for i, f := range facts {
		payload := payloads[i]
		if err := audit.Write(ctx, tx, audit.Event{
			ActorID: actor(caller), HospitalID: caller.Hid, RequestID: caller.Rid,
			Action: f.Action, Entity: f.Entity, EntityID: f.EntityId, Payload: payload,
		}); err != nil {
			// audit.Write's error can name the worker's payload key or carry
			// driver text: return a fixed value only. A sensitive key is the
			// worker's fault (ErrBadFact); anything else (the database, the
			// role's policy) is core's own failure, not a bad fact.
			if errors.Is(err, audit.ErrSensitiveKey) {
				return ErrBadFact
			}
			return ErrAuditWrite
		}
	}
	return nil
}

// AuditRefusal writes intelligence.refused (spec §10.2) inside tx when err
// is a worker refusal (unauthorized, forbidden, invalid_request), with the
// operation, the reason code and the kid core signed with; for any other
// err it writes nothing. The actor, hospital and request id are the
// caller's.
func (c *Client) AuditRefusal(ctx context.Context, tx pgx.Tx, caller Caller, opName string, err error) error {
	code, ok := Refusal(err)
	if !ok {
		return nil
	}
	if _, known := authz.OperationByName(opName); !known {
		return ErrMisuse
	}
	return audit.Write(ctx, tx, audit.Event{
		ActorID: actor(caller), HospitalID: caller.Hid, RequestID: caller.Rid,
		Action: "intelligence.refused", Entity: "operation", EntityID: opName,
		Payload: map[string]any{"operation": opName, "reason": code, "kid": c.Signer.KeyID},
	})
}
