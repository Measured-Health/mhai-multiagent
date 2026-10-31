package intel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

const (
	// DrainEvery is how often the drainer looks at the outbox.
	DrainEvery = 2 * time.Second
	// drainBatch is the most facts one transaction moves.
	drainBatch = 100
)

var errDrain = errors.New("intel: audit drain failed")

// identityKeys are payload keys a fact may not use to name who or where:
// the drainer drops them, so the only identity on the row is the job's.
var identityKeys = []string{"actor_id", "hospital_id", "request_id", "sub"}

// RunDrainer runs DrainOnce every interval (DrainEvery when not positive)
// until ctx ends, draining again at once while a batch came back full.
// Every core copy runs it; DrainOnce's row locks append each fact once.
func RunDrainer(ctx context.Context, pool *pgxpool.Pool, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		every = DrainEvery
	}
	if log == nil {
		log = logx.New(io.Discard)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		started := time.Now()
		n, err := DrainOnce(ctx, pool, log)
		if n > 0 || (err != nil && ctx.Err() == nil) { // an idle tick leaves no span
			code := "ok"
			if err != nil {
				code = "internal_error"
			}
			telemetry.Record(ctx, "intelligence.drain", started, code)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("intel: audit drain failed")
		case n > 0:
			log.Info("intel: audit drain", "appended", n)
		}
		if n == drainBatch && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// outboxRow is one fact with the identity of its job.
type outboxRow struct {
	id                int64
	action            string
	entity, entityID  *string
	payload           []byte
	queryID, hospital uuid.UUID
	createdBy         *uuid.UUID
	requestID         *string
	status            string // the job's status now
}

// DrainOnce moves up to 100 facts from intelligence_audit_outbox onto the
// audit chain, oldest first, in one transaction as mhai_audit_drain (spec
// §10.2): it deletes them (DELETE ... WHERE id IN (oldest 100) RETURNING,
// joined to their clinical_queries row), appends each with audit.Write,
// and commits. A second drainer running at once waits on the deleted rows'
// locks and then finds them gone, so each fact is appended once (F2).
//
// The actor, hospital and request id come from the job row core wrote
// (created_by, hospital_id, request_id or JobRID), and the entity is always
// the job (query, its id), never from the fact: the fact gives the action
// (one of the three the outbox allows) and the payload (its own entity and
// entity id, if other than the job, go in as fact_entity and
// fact_entity_id), and payload keys naming an identity (actor_id,
// hospital_id, request_id, sub) are dropped before audit.Write's
// sensitive-key check. The payload must then fit its action's closed
// schema (factschema.go: closed-set codes, ids of a fixed shape, bounded
// integers; final review I-1). A payload that fails either check is
// replaced by {query_id, payload_refused: true}, so one bad fact can't hold
// the outbox up and no free text reaches the chain. A fact whose action
// doesn't match the job's status now (query.completed for a job that isn't
// completed, and so on) is deleted and not appended. Anything else rolls
// the batch back for the next run.
func DrainOnce(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int, error) {
	if log == nil {
		log = logx.New(io.Discard)
	}
	n := 0
	err := db.WithAuditDrain(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			DELETE FROM intelligence_audit_outbox o
			USING clinical_queries q
			WHERE o.id IN (SELECT id FROM intelligence_audit_outbox ORDER BY id LIMIT $1)
			  AND q.id = o.query_id AND q.hospital_id = o.hospital_id
			RETURNING o.id, o.action, o.entity, o.entity_id, o.payload, q.id, q.hospital_id, q.created_by, q.request_id, q.status`, drainBatch)
		if err != nil {
			return errDrain
		}
		var rows []outboxRow
		for r.Next() {
			var o outboxRow
			if err := r.Scan(&o.id, &o.action, &o.entity, &o.entityID, &o.payload, &o.queryID, &o.hospital, &o.createdBy, &o.requestID, &o.status); err != nil {
				r.Close()
				return errDrain
			}
			rows = append(rows, o)
		}
		r.Close()
		if r.Err() != nil {
			return errDrain
		}
		slices.SortFunc(rows, func(a, b outboxRow) int {
			switch {
			case a.id < b.id:
				return -1
			case a.id > b.id:
				return 1
			}
			return 0
		})
		for _, o := range rows {
			if job, ok := jobFactSchemas[o.action]; !ok || job.status != o.status {
				// A false statement (query.completed for a job that isn't
				// completed): taken off the outbox, never appended. Ids only.
				log.Error("intel: outbox fact refused: action doesn't match the job's status",
					"action", o.action, "query_id", o.queryID.String())
				continue
			}
			e := drainEvent(o, log)
			if err := audit.Write(ctx, tx, e); err != nil {
				return errDrain
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, errDrain
	}
	return n, nil
}

// drainEvent builds the audit event for o: identity from the job row, the
// rest from the fact.
func drainEvent(o outboxRow, log *slog.Logger) audit.Event {
	e := audit.Event{
		HospitalID: o.hospital.String(),
		Action:     o.action,
		Entity:     "query",
		EntityID:   o.queryID.String(),
	}
	if o.createdBy != nil {
		e.ActorID = o.createdBy.String()
	}
	rid := ""
	if o.requestID != nil {
		rid = *o.requestID
	}
	e.RequestID = JobRID(o.queryID, rid)
	payload := map[string]any{}
	if len(o.payload) > 0 {
		if err := json.Unmarshal(o.payload, &payload); err != nil || payload == nil {
			payload = map[string]any{}
		}
	}
	for _, k := range identityKeys {
		delete(payload, k)
	}
	// The closed schema of the action (factschema.go), before the fact's
	// own entity is added: ids and codes only, never text.
	fits := jobFactSchemas[o.action].schema.check(payload, o.queryID)
	// The row is always about the job (entity query, entity id the job's):
	// a fact's own entity, if another, is detail in the payload only.
	if o.entity != nil && *o.entity != "" && *o.entity != e.Entity {
		payload["fact_entity"] = *o.entity
	}
	if o.entityID != nil && *o.entityID != "" && *o.entityID != e.EntityID {
		fits = fits && factEntityIDPattern.MatchString(*o.entityID)
		payload["fact_entity_id"] = *o.entityID
	}
	raw, err := json.Marshal(payload)
	if _, bad := logx.SensitiveKeyInJSON(raw); err != nil || bad || !fits {
		// Ids only in the log: the action and the job, never the key's value.
		log.Error("intel: outbox fact payload refused", "action", o.action, "query_id", o.queryID.String())
		payload = map[string]any{"query_id": o.queryID.String(), "payload_refused": true}
	}
	e.Payload = payload
	return e
}
