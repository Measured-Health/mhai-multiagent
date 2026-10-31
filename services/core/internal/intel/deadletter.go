package intel

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

const (
	// deadMaxDeliver: a letter core couldn't handle this many times (the
	// database down for minutes) is logged and dropped; the job row keeps
	// its state and the deadline sweep ends it.
	deadMaxDeliver = 10
	// deadRetryDelay is the Nak delay after a transient failure.
	deadRetryDelay = 5 * time.Second
	deadFetchBatch = 16
	deadFetchWait  = 2 * time.Second
	// deadMaxBody: bus.DeadMaxMsgSize; anything larger isn't a letter.
	deadMaxBody = bus.DeadMaxMsgSize
	// jobType is the one job type with a clinical_queries row.
	jobType = "run_query"
)

var (
	errDeadLetters = errors.New("intel: dead-letter consumer unavailable")
	errDeadTx      = errors.New("intel: dead-letter update failed")
	errRetried     = errors.New("intel: job already re-minted for this kid")
	// safeToken is what a letter's reason or kid must look like before core
	// logs or records it (a worker wrote it: unverified).
	safeToken = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// Metrics counts what core's background intelligence loops did, for a
// later metrics endpoint (T14). DeadRefused is the alert: a boundary
// refusal (unauthorized, forbidden, invalid_request) of a real job.
type Metrics struct {
	DeadReceived atomic.Int64 // letters read
	DeadIgnored  atomic.Int64 // malformed, or naming no job of the subject's hospital
	DeadMarked   atomic.Int64 // job rows set failed
	DeadRefused  atomic.Int64 // boundary refusals (alert)
	DeadExpired  atomic.Int64 // deadline_exceeded letters (no alert, F6)
	DeadRetried  atomic.Int64 // jobs re-minted after an emergency key rotation
	DeadErrors   atomic.Int64 // transient failures (letter Nak'd)
}

// unattached counts for a Client without Metrics, so log sampling still
// works.
var unattached Metrics

func (c *Client) metrics() *Metrics {
	if c.Metrics != nil {
		return c.Metrics
	}
	return &unattached
}

// deadLogEvery samples the log line of an ignored letter: the first, then
// one in this many, so a flood of replayed or forged letters can't flood
// the log either.
const deadLogEvery = 100

// deadOutcome is what to do with a letter's message.
type deadOutcome int

const (
	deadAck deadOutcome = iota
	deadRetry
)

// RunDeadLetters is core's consumer of dead letters (spec §9.2, §10.1): the
// durable core_dead on DEAD, shared by every core copy (each letter goes to
// one), until ctx ends. It returns at once with an error if the consumer
// can't be found.
//
// A dead letter is untrusted input: a worker wrote it, and any worker may
// publish dead.v1.>. Core acts only on a letter whose own subject,
// dead.v1.<hospital>.run_query, names the same hospital as its
// subject_hospital, and whose job_id is a clinical_queries row of that
// hospital, looked up as mhai_jobs (F5); anything else is logged by ids and
// acked. Then:
//   - reason signature from a kid core no longer trusts (an emergency
//     rotation, spec §7.5), on a job still queued: Republish as the
//     creator, after re-checking the account and hospital (F23), with a new
//     message id (RetryMsgID), audited job.dead_lettered and job.retried;
//   - otherwise the row, if still queued, is set failed with the letter's
//     code, audited job.dead_lettered and query.failed; unauthorized,
//     forbidden and invalid_request also audit intelligence.refused and
//     count the DeadRefused alert; deadline_exceeded (a job that expired in
//     the queue, F6) doesn't;
//   - a letter that changes nothing (the row running, which a worker holds,
//     or finished, or already re-minted for that kid) writes nothing to the
//     chain and raises no alert: it is counted DeadIgnored, with a sampled
//     log line, so a replayed letter can't flood the chain or the pager.
//
// A transient failure Naks the letter for a retry; one that failed
// deadMaxDeliver times is dropped (the deadline sweep ends the row).
func RunDeadLetters(ctx context.Context, pool *pgxpool.Pool, js jetstream.JetStream, client *Client) error {
	if js == nil || client == nil || pool == nil {
		return errDeadLetters
	}
	cons, err := js.Consumer(ctx, bus.DeadStream, bus.CoreDeadConsumer)
	if err != nil {
		return errDeadLetters
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(deadFetchBatch, jetstream.FetchMaxWait(deadFetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			handleDeadMsg(ctx, pool, client, m)
		}
	}
	return nil
}

func handleDeadMsg(ctx context.Context, pool *pgxpool.Pool, client *Client, m jetstream.Msg) {
	// A letter already fetched is finished even while core shuts down, so
	// it isn't left to wait out its ack timer.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if md, err := m.Metadata(); err == nil && md.NumDelivered > deadMaxDeliver {
		client.log().Error("intel: dead letter dropped after repeated failures", "deliveries", md.NumDelivered)
		_ = m.Term()
		return
	}
	// The letter's subject and body are a worker's words: no attribute
	// carries them. The span says only whether the letter was settled.
	started := time.Now()
	out := HandleDeadLetter(ctx, pool, client, m.Subject(), m.Data())
	switch out {
	case deadRetry:
		telemetry.Record(ctx, "intelligence.dead_letter", started, "unavailable")
		_ = m.NakWithDelay(deadRetryDelay)
	default:
		telemetry.Record(ctx, "intelligence.dead_letter", started, "ok")
		_ = m.Ack()
	}
}

// deadRow is what the consumer reads of a job.
type deadRow struct {
	JobRow
	status string
}

// HandleDeadLetter handles one letter (see RunDeadLetters) and says whether
// to ack it or retry it later.
func HandleDeadLetter(ctx context.Context, pool *pgxpool.Pool, client *Client, subject string, data []byte) deadOutcome {
	met := client.metrics()
	met.DeadReceived.Add(1)
	log := client.log()
	ignore := func(why string) deadOutcome {
		if n := met.DeadIgnored.Add(1); n == 1 || n%deadLogEvery == 0 {
			log.Warn("intel: dead letter ignored", "why", why, "ignored_total", n)
		}
		return deadAck
	}
	seg := strings.Split(subject, ".")
	if len(seg) != 4 || seg[0] != "dead" || seg[1] != "v1" {
		return ignore("subject")
	}
	if seg[3] != jobType {
		return ignore("type") // no other job type has a row to mark yet
	}
	hid, err := uuid.Parse(seg[2])
	if err != nil || !canonicalUUID(seg[2]) {
		return ignore("subject hospital")
	}
	if len(data) > deadMaxBody {
		return ignore("size")
	}
	var dl intelpb.DeadLetter
	if err := proto.Unmarshal(data, &dl); err != nil {
		return ignore("body")
	}
	if dl.SubjectHospital != seg[2] || dl.Type != jobType {
		return ignore("letter names another hospital or type")
	}
	if _, ok := workerCodes[dl.Code]; !ok {
		return ignore("code")
	}
	if !canonicalUUID(dl.JobId) {
		return ignore("job id")
	}
	jobID := uuid.MustParse(dl.JobId)
	kid := ""
	if safeToken.MatchString(dl.Kid) {
		kid = dl.Kid
	}
	reason := ""
	if safeToken.MatchString(dl.Reason) {
		reason = dl.Reason
	}

	row, found, err := lookupDeadJob(ctx, pool, jobID, hid)
	if err != nil {
		met.DeadErrors.Add(1)
		return deadRetry
	}
	if !found {
		return ignore("no job of the subject's hospital")
	}

	code := dl.Code
	if code == "unauthorized" && reason == "signature" && kid != "" && row.status == "queued" && rotatedOut(client, kid) {
		switch err := remint(ctx, pool, client, row, kid); {
		case err == nil:
			met.DeadRetried.Add(1)
			return deadAck
		case errors.Is(err, errRetried):
			// This row was already re-minted for this kid: a replayed or
			// duplicated letter changes nothing.
			return ignore("already re-minted for this kid")
		case errors.Is(err, ErrCreatorUnusable):
			// Not re-minted: the account or hospital can't use the service.
			// Marked failed below, as any refusal.
		case errors.Is(err, ErrDeadline):
			code = "deadline_exceeded"
		default:
			met.DeadErrors.Add(1)
			return deadRetry
		}
	}

	marked, err := markDead(ctx, pool, row, code, kid)
	if err != nil {
		met.DeadErrors.Add(1)
		return deadRetry
	}
	if !marked {
		// Running (a worker holds it), or already finished: the letter
		// changes nothing, so nothing goes on the chain and no alert fires
		// (a replayed letter can't write rows or page anyone).
		return ignore("job not queued")
	}
	met.DeadMarked.Add(1)
	attrs := []any{"job_id", row.ID.String(), "hospital_id", row.HospitalID.String(), "error_code", code, "kid", kid}
	switch {
	case refusalCodes[code]:
		met.DeadRefused.Add(1)
		// A refusal of a job core signed is a bug or an attack (spec §10.1).
		log.Error("intel: worker refused a job", attrs...)
	case code == "deadline_exceeded":
		met.DeadExpired.Add(1)
		log.Info("intel: job expired before it ran", attrs...)
	default:
		log.Warn("intel: job dead-lettered", attrs...)
	}
	return deadAck
}

// rotatedOut reports whether kid was removed from the trusted set while
// core's own signing kid is in it: only then does a new token help.
func rotatedOut(client *Client, kid string) bool {
	t := client.Trusted
	return t.Known() && !t.Has(kid) && t.Has(client.Signer.KeyID) && kid != client.Signer.KeyID
}

func lookupDeadJob(ctx context.Context, pool *pgxpool.Pool, id, hid uuid.UUID) (deadRow, bool, error) {
	var r deadRow
	found := false
	op, ok := authz.OperationByName(runQuery)
	if !ok {
		return r, false, errDeadTx
	}
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		var by *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id, hospital_id, created_by, COALESCE(request_id, ''), attempts, status,
			       COALESCE(deadline_at, created_at + make_interval(secs => $3))
			FROM clinical_queries WHERE id = $1 AND hospital_id = $2`, id, hid, op.Timeout.Seconds()).
			Scan(&r.ID, &r.HospitalID, &by, &r.RequestID, &r.Attempts, &r.status, &r.Deadline)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return errDeadTx
		}
		if by != nil {
			r.CreatedBy = *by
		}
		found = true
		return nil
	})
	if err != nil {
		return r, false, errDeadTx
	}
	return r, found, nil
}

// jobEvent is an audit event about row, written by core's loops as
// mhai_jobs: no actor, the row's hospital and request id (00009 ties the
// row to a job of that hospital).
func jobEvent(row JobRow, action string, payload map[string]any) audit.Event {
	return audit.Event{HospitalID: row.HospitalID.String(), RequestID: JobRID(row.ID, row.RequestID),
		Action: action, Entity: "query", EntityID: row.ID.String(), Payload: payload}
}

// remint re-publishes a queued job refused for a removed kid (F23: the one
// re-mint helper, Republish), then marks it published and audits it.
//
// A row is re-minted at most once per kid: retried_kid is claimed first
// (00009), so a replayed or forged letter can't make core publish the job
// again; errRetried says it was already done. A failed publish gives the
// claim back for the letter's retry.
func remint(ctx context.Context, pool *pgxpool.Pool, client *Client, row deadRow, kid string) error {
	claimed := false
	if err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE clinical_queries SET retried_kid = $3
			WHERE id = $1 AND hospital_id = $2 AND status = 'queued' AND retried_kid IS DISTINCT FROM $3`,
			row.ID, row.HospitalID, kid)
		if err != nil {
			return errDeadTx
		}
		claimed = tag.RowsAffected() == 1
		return nil
	}); err != nil {
		return errDeadTx
	}
	if !claimed {
		return errRetried
	}
	msgID := RetryMsgID(row.ID, row.Attempts)
	if err := Republish(ctx, pool, client, row.JobRow, msgID); err != nil {
		_ = db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE clinical_queries SET retried_kid = NULL
				WHERE id = $1 AND hospital_id = $2 AND status = 'queued' AND retried_kid = $3`, row.ID, row.HospitalID, kid)
			return err
		})
		return err
	}
	return db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE clinical_queries SET status = 'queued', published_at = now()
			WHERE id = $1 AND hospital_id = $2 AND status = 'queued'`, row.ID, row.HospitalID); err != nil {
			return errDeadTx
		}
		if err := audit.Write(ctx, tx, jobEvent(row.JobRow, "job.dead_lettered", map[string]any{
			"job_id": row.ID.String(), "type": jobType, "error": "unauthorized", "attempts": row.Attempts, "kid": kid,
		})); err != nil {
			return errDeadTx
		}
		if err := audit.Write(ctx, tx, jobEvent(row.JobRow, "job.retried", map[string]any{
			"job_id": row.ID.String(), "msg_id": msgID,
		})); err != nil {
			return errDeadTx
		}
		return nil
	})
}

// markDead fails row with code if it is still queued and, only then,
// audits the letter (job.dead_lettered, query.failed and, for a refusal,
// intelligence.refused) in the same transaction as mhai_jobs. It reports
// whether it marked the row; a letter that changes nothing writes nothing.
func markDead(ctx context.Context, pool *pgxpool.Pool, row deadRow, code, kid string) (bool, error) {
	marked := false
	err := db.WithJobs(ctx, pool, func(tx pgx.Tx) error {
		var attempts int
		err := tx.QueryRow(ctx, `UPDATE clinical_queries SET status = 'failed', error = $3, completed_at = now()
			WHERE id = $1 AND hospital_id = $2 AND status = 'queued'
			RETURNING attempts`, row.ID, row.HospitalID, code).Scan(&attempts)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return errDeadTx
		}
		marked = true
		if err := audit.Write(ctx, tx, jobEvent(row.JobRow, "job.dead_lettered", map[string]any{
			"job_id": row.ID.String(), "type": jobType, "error": code, "attempts": attempts,
		})); err != nil {
			return errDeadTx
		}
		if err := audit.Write(ctx, tx, jobEvent(row.JobRow, "query.failed", map[string]any{
			"query_id": row.ID.String(), "error": code, "attempts": attempts,
		})); err != nil {
			return errDeadTx
		}
		if refusalCodes[code] {
			p := map[string]any{"operation": runQuery, "reason": code}
			if kid != "" {
				p["kid"] = kid
			}
			if err := audit.Write(ctx, tx, jobEvent(row.JobRow, "intelligence.refused", p)); err != nil {
				return errDeadTx
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}
