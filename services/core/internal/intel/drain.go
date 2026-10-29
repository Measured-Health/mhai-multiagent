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
		n, err := DrainOnce(ctx, pool, log)
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
}

// DrainOnce moves up to 100 facts from intelligence_audit_outbox onto the
// audit chain, oldest first, in one transaction as mhai_audit_drain (spec
// §10.2): it deletes them (DELETE ... WHERE id IN (oldest 100) RETURNING,
// joined to their clinical_queries row), appends each with audit.Write,
// and commits. A second drainer running at once waits on the deleted rows'
// locks and then finds them gone, so each fact is appended once (F2).
//
// The actor, hospital and request id come from the job row core wrote
// (created_by, hospital_id, request_id or JobRID), never from the fact: the
// fact gives the action (one of the three the outbox allows), entity,
// entity id and payload, and payload keys naming an identity (actor_id,
// hospital_id, request_id, sub) are dropped before audit.Write's
// sensitive-key check. A payload that fails that check is replaced by
// {query_id, payload_refused: true}, so one bad fact can't hold the outbox
// up; anything else rolls the batch back for the next run.
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
			RETURNING o.id, o.action, o.entity, o.entity_id, o.payload, q.id, q.hospital_id, q.created_by, q.request_id`, drainBatch)
		if err != nil {
			return errDrain
		}
		var rows []outboxRow
		for r.Next() {
			var o outboxRow
			if err := r.Scan(&o.id, &o.action, &o.entity, &o.entityID, &o.payload, &o.queryID, &o.hospital, &o.createdBy, &o.requestID); err != nil {
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
	if o.entity != nil && *o.entity != "" {
		e.Entity = *o.entity
	}
	if o.entityID != nil && *o.entityID != "" {
		e.EntityID = *o.entityID
	}
	payload := map[string]any{}
	if len(o.payload) > 0 {
		if err := json.Unmarshal(o.payload, &payload); err != nil || payload == nil {
			payload = map[string]any{}
		}
	}
	for _, k := range identityKeys {
		delete(payload, k)
	}
	raw, err := json.Marshal(payload)
	if _, bad := logx.SensitiveKeyInJSON(raw); err != nil || bad {
		// Ids only in the log: the action and the job, never the key's value.
		log.Error("intel: outbox fact payload refused", "action", o.action, "query_id", o.queryID.String())
		payload = map[string]any{"query_id": o.queryID.String(), "payload_refused": true}
	}
	e.Payload = payload
	return e
}
