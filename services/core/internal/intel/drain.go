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
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
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

// outboxRow is one fact with the identity of its job: a clinical_queries
// row (query_id facts) or a jobs row (job_id facts).
type outboxRow struct {
	id               int64
	action           string
	entity, entityID *string
	payload          []byte
	jobID, hospital  uuid.UUID  // the parent row's id and hospital
	jobType          *string    // the jobs row's type; nil for a question
	subject          *uuid.UUID // the jobs row's subject; nil for a question
	createdBy        *uuid.UUID // the parent row's creator
	requestID        *string
	status           *string // the parent's status now; nil: no parent row
}

// DrainOnce moves up to 100 facts from intelligence_audit_outbox onto the
// audit chain, oldest first, in one transaction as mhai_audit_drain (spec
// §10.2, Plan 4 §6.5): it deletes the oldest 100 whatever their parent
// (DELETE ... WHERE id IN (oldest 100) RETURNING), reads each one's parent
// (its clinical_queries row for a query_id fact, its jobs row for a job_id
// fact, of the fact's own hospital), appends each with audit.Write, and
// commits. A second drainer running at once waits on the deleted rows'
// locks and then finds them gone, so each fact is appended once (F2). Facts
// of one kind at the head never hold up the other kind (Plan 3's drainer
// deleted only facts joined to clinical_queries, so 100 job facts at the
// head stalled it).
//
// The actor, hospital and request id come from the parent row core wrote
// (created_by, hospital_id, request_id or JobRID/JobsRowRID), and so does
// the entity, never the fact: the question (query and its id) for a query
// fact; for a correction fact the jobs row's subject (correction and
// subject_id, spec §9), with the job's id as job_id in the payload. The
// fact gives the action (one the outbox allows) and the payload (a query
// fact's own entity and entity id, if other than the question, go in as
// fact_entity and fact_entity_id; a job fact's correction_id, and its own
// entity id if any, must be the jobs row's subject_id), and payload keys
// naming an identity
// (actor_id, hospital_id, request_id, sub) are dropped before audit.Write's
// sensitive-key check. The payload must then fit its action's closed
// schema (factschema.go: closed-set codes, ids of a fixed shape, bounded
// integers; final review I-1). A payload that fails either check is
// replaced by {query_id, payload_refused: true} ({job_id,
// payload_refused: true} for a job fact), so one bad fact can't hold the
// outbox up and no free text reaches the chain. A fact whose action doesn't belong to its
// parent's kind (a correction.* fact on a question, a query.* fact on a
// job, a fact for another job type) or doesn't match the parent's status
// now (query.completed for a job that isn't completed, and so on), or
// whose parent can't be found, is deleted and not appended. Anything else
// rolls the batch back for the next run.
func DrainOnce(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int, error) {
	if log == nil {
		log = logx.New(io.Discard)
	}
	n := 0
	err := db.WithAuditDrain(ctx, pool, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			WITH gone AS (
				DELETE FROM intelligence_audit_outbox o
				WHERE o.id IN (SELECT id FROM intelligence_audit_outbox ORDER BY id LIMIT $1)
				RETURNING o.id, o.action, o.entity, o.entity_id, o.payload, o.hospital_id, o.query_id, o.job_id
			)
			SELECT g.id, g.action, g.entity, g.entity_id, g.payload, g.hospital_id,
			       COALESCE(g.query_id, g.job_id), j.type, j.subject_id,
			       CASE WHEN g.query_id IS NOT NULL THEN q.created_by ELSE j.created_by END,
			       CASE WHEN g.query_id IS NOT NULL THEN q.request_id ELSE j.request_id END,
			       CASE WHEN g.query_id IS NOT NULL THEN q.status ELSE j.status END
			FROM gone g
			LEFT JOIN clinical_queries q ON g.query_id IS NOT NULL AND q.id = g.query_id AND q.hospital_id = g.hospital_id
			LEFT JOIN jobs j ON g.job_id IS NOT NULL AND j.id = g.job_id AND j.hospital_id = g.hospital_id
			ORDER BY g.id`, drainBatch)
		if err != nil {
			return errDrain
		}
		var rows []outboxRow
		for r.Next() {
			var o outboxRow
			var parent *uuid.UUID
			if err := r.Scan(&o.id, &o.action, &o.entity, &o.entityID, &o.payload, &o.hospital, &parent, &o.jobType,
				&o.subject, &o.createdBy, &o.requestID, &o.status); err != nil {
				r.Close()
				return errDrain
			}
			if parent != nil {
				o.jobID = *parent
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
			if !factMatchesParent(o) {
				// A false statement (query.completed for a job that isn't
				// completed, a correction fact on a question), or a parent
				// that isn't there: taken off the outbox, never appended.
				// Ids only.
				log.Error("intel: outbox fact refused: action doesn't match the job's kind or status",
					"action", o.action, "job_id", o.jobID.String())
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

// factMatchesParent reports whether o's action is one of its parent's kind
// (a query fact on a question, a job fact on a jobs row of the action's
// type) and states the parent's status now.
func factMatchesParent(o outboxRow) bool {
	f, ok := jobFactSchemas[o.action]
	if !ok || o.status == nil || o.jobID == uuid.Nil || *o.status != f.status {
		return false
	}
	if f.jobType == "" {
		return o.jobType == nil
	}
	return o.jobType != nil && *o.jobType == f.jobType
}

// drainEvent builds the audit event for o: identity from the parent row,
// the rest from the fact.
//
// A question's fact is recorded against the question (entity query). A job
// fact whose action names a subject (correction.applied and
// correction.apply_failed, spec §9) is recorded against the jobs row's
// subject (entity correction, the jobs row's subject_id: never an id from
// the fact), with job_id, the job's own id, in the payload.
func drainEvent(o outboxRow, log *slog.Logger) audit.Event {
	isJob := o.jobType != nil
	f := jobFactSchemas[o.action]
	e := audit.Event{
		HospitalID: o.hospital.String(),
		Action:     o.action,
		Entity:     "query",
		EntityID:   o.jobID.String(),
	}
	subject := ""
	if isJob {
		e.Entity = "job"
		if f.subjectEntity != "" && o.subject != nil {
			subject = o.subject.String()
			e.Entity, e.EntityID = f.subjectEntity, subject
		}
	}
	if o.createdBy != nil {
		e.ActorID = o.createdBy.String()
	}
	rid := ""
	if o.requestID != nil {
		rid = *o.requestID
	}
	if isJob {
		e.RequestID = JobsRowRID(o.jobID, rid)
	} else {
		e.RequestID = JobRID(o.jobID, rid)
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
	fits := true
	if isJob {
		// job_id is core's to write: one the fact gives must be the job's.
		if v, ok := payload["job_id"]; ok {
			s, isStr := v.(string)
			fits = isStr && s == o.jobID.String()
			delete(payload, "job_id")
		}
	}
	// The closed schema of the action (factschema.go), before the fact's
	// own entity is added: ids and codes only, never text. query_id, if
	// present, must be the question's; a job fact may carry none.
	queryID := o.jobID
	if isJob {
		queryID = uuid.Nil
	}
	fits = fits && f.schema.check(payload, queryID)
	ent, entID := "", ""
	if o.entity != nil {
		ent = *o.entity
	}
	if o.entityID != nil {
		entID = *o.entityID
	}
	if isJob {
		// A job fact names the job's own subject (correction_id is the jobs
		// row's subject_id), never another: a worker can't put another
		// correction on the chain under this job. Its own entity, if it
		// names one, is that subject or the job itself.
		if f.subjectKey != "" {
			s, ok := payload[f.subjectKey].(string)
			fits = fits && ok && subject != "" && s == subject
		}
		switch {
		case ent == "" && entID == "":
		case f.subjectEntity != "" && ent == f.subjectEntity && (entID == "" || entID == subject):
		case ent == "job" && (entID == "" || entID == o.jobID.String()):
		default:
			fits = false
		}
	} else {
		// The row is always about the question: a fact's own entity, if
		// another, is detail in the payload only.
		if ent != "" && ent != e.Entity {
			fits = fits && factEntity.MatchString(ent)
			payload["fact_entity"] = ent
		}
		if entID != "" && entID != e.EntityID {
			fits = fits && factEntityIDPattern.MatchString(entID)
			payload["fact_entity_id"] = entID
		}
	}
	raw, err := json.Marshal(payload)
	if _, bad := logx.SensitiveKeyInJSON(raw); err != nil || bad || !fits {
		// Ids only in the log: the action and the job, never the key's value.
		log.Error("intel: outbox fact payload refused", "action", o.action, "job_id", o.jobID.String())
		payload = map[string]any{"payload_refused": true}
		if !isJob {
			payload["query_id"] = o.jobID.String()
		}
	}
	if isJob {
		payload["job_id"] = o.jobID.String()
	}
	e.Payload = payload
	return e
}
