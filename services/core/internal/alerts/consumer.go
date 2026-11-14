package alerts

// Core's finding consumer, core_emr (EMR design §11.4, §12.1, §12.4, §12.5,
// §15; plan Tasks 35-37). It pulls emr.v1.<hid>.finding and
// emr.v1.<hid>.signal and, under the subject's hospital as mhai_emr_core
// (row-level security hides every other hospital's row: another
// hospital's finding id reads as not found, A13), turns findings into
// alerts in one transaction per message:
//
//   - the message carries an id only; core re-reads the finding and acts
//     on its current state, so order and redelivery don't matter;
//   - shadow and historical findings never alert (K11, A18);
//   - once per finding and status: emr_consumed records what core did, and
//     a lock per hospital, patient and rule serialises every message about
//     one chain of findings, so a redelivery, a replay or a second core
//     copy changes nothing (A19);
//   - an open finding is raised (design §12.1 steps 2-5: the hospital
//     active with a connection on, the null-MRN gate, the hospital's
//     settings through Route), or updates the patient's open alert for the
//     same rule and analyte (§12.4), or is recorded as not raised with a
//     reason code (alert.not_raised);
//   - an amended result's finding updates its alert in place (amended,
//     re-notified when the severity rose); a withdrawn finding withdraws a
//     new alert (dismissed, by nobody, other) and marks a handled one
//     without reopening it (§11.4, §12.5);
//   - every alert written is audited (alert.raised, alert.updated,
//     alert.withdrawn), ids and codes only.
//
// Nothing is dropped silently: an error rolls the message's work back and
// naks it; the fifth delivery dead-letters dead.v1.<hid>.core_emr (a page);
// if that publish fails the message is naked again, never acked. Logs and
// metrics carry ids, codes and counts only.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

const (
	// ConsumerMaxDeliver is the durable's limit (bus.EMRMaxDeliver): the
	// fifth failed delivery dead-letters.
	ConsumerMaxDeliver = bus.EMRMaxDeliver
	// consumerFetchBatch: one message at a time, so a message never waits
	// in a batch past its ack wait (and isn't redelivered meanwhile).
	consumerFetchBatch = 1
	consumerFetchWait  = 5 * time.Second
	// consumerDeadline is inside the durable's 60 s ack wait.
	consumerDeadline = 50 * time.Second
	// consumerAttempts: a race (two consumers on one chain, an exclusion,
	// a merge, a deadlock) is retried in process this many times before
	// the message is naked.
	consumerAttempts = 3
	// deadAttempts and deadBackoff: a dead letter's publish is tried this
	// many times, waiting a growing multiple of deadBackoff between.
	deadAttempts = 3
	deadBackoff  = time.Second
	// maxChain bounds how far a chain of amended findings is followed.
	maxChain = 20
	// maxPrevious bounds an amended alert's previous value (characters).
	maxPrevious = 120
)

// The reasons a finding is recorded as not raised (alert.not_raised),
// beyond Route's.
const (
	ReasonHospitalInactive = "hospital_inactive"
	ReasonNoConnection     = "no_connection"
	ReasonNullMRN          = "null_mrn"
	ReasonAlreadyOpen      = "already_open"
	ReasonAlreadyHandled   = "already_handled"
	// ReasonPatientExcluded: the patient was excluded while its purge is
	// held under a legal hold, so its findings still exist (EMR PR 9).
	ReasonPatientExcluded = "patient_excluded"
)

// errRace: something the transaction read changed under it (a merge, an
// exclusion, another consumer); the message is retried.
var errRace = errors.New("alerts: the finding changed while it was read")

// ConsumerMetrics are counts only.
type ConsumerMetrics struct {
	Findings     atomic.Int64 // finding messages read
	Raised       atomic.Int64 // alerts created
	Updated      atomic.Int64 // open alerts updated by a new hit or an amendment
	Withdrawn    atomic.Int64 // alerts withdrawn or marked withdrawn
	NotRaised    atomic.Int64 // findings recorded as not raised
	Skipped      atomic.Int64 // shadow or historical findings (never alerts)
	Duplicates   atomic.Int64 // messages already acted on
	Foreign      atomic.Int64 // emr_finding_foreign_total: not found under the subject's hospital
	Refused      atomic.Int64 // malformed messages
	Signals      atomic.Int64 // signals read
	SourcePaused atomic.Int64 // breaker trips recorded (pages)
	Retried      atomic.Int64 // messages naked for a retry
	DeadLettered atomic.Int64 // messages dead-lettered (pages)
	DeadFailed   atomic.Int64 // dead letters that couldn't be sent (message kept)
	Unavailable  atomic.Int64 // attempts to reach the durable that failed (page)
}

// Publisher publishes one message with a de-duplication id (the dead
// letter).
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

// JSPublisher publishes through JetStream.
type JSPublisher struct{ JS jetstream.JetStream }

// Publish sends data on subject with msgID as Nats-Msg-Id.
func (p JSPublisher) Publish(ctx context.Context, subject string, data []byte, msgID string) error {
	_, err := p.JS.Publish(ctx, subject, data, jetstream.WithMsgID(msgID))
	return err
}

// ConsumerMsg is what HandleMsg needs of a JetStream message.
type ConsumerMsg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	// Deliveries is the delivery count; an error when it can't be read
	// (the message is then naked, never dead-lettered on a guess).
	Deliveries() (uint64, error)
}

// Consumer turns findings into alerts. It is safe for concurrent use.
type Consumer struct {
	Pool *pgxpool.Pool
	Pub  Publisher
	// AllowNullMRN is CORE_EMR_ALLOW_NULL_MRN: false (the default) records
	// a finding of a patient with no MRN as not raised (null_mrn).
	AllowNullMRN bool
	Log          *slog.Logger
	Metrics      *ConsumerMetrics
	// handleFinding, when set (tests only), replaces HandleFinding.
	handleFinding func(ctx context.Context, hid, fid uuid.UUID) error
}

func (c *Consumer) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

var unattachedConsumer ConsumerMetrics

func (c *Consumer) metrics() *ConsumerMetrics {
	if c.Metrics != nil {
		return c.Metrics
	}
	return &unattachedConsumer
}

// HandleMsg settles one emr.v1.<hid>.finding or emr.v1.<hid>.signal
// message.
func (c *Consumer) HandleMsg(ctx context.Context, m ConsumerMsg) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consumerDeadline)
	defer cancel()
	seg := strings.Split(m.Subject(), ".")
	var hid uuid.UUID
	var err error
	if len(seg) == 4 {
		hid, err = uuid.Parse(seg[2])
	}
	if len(seg) != 4 || seg[0] != "emr" || seg[1] != "v1" || err != nil || hid.String() != seg[2] ||
		(seg[3] != "finding" && seg[3] != "signal") {
		c.refuse(m, "subject")
		return
	}
	var ref uuid.UUID
	var work func() error
	switch seg[3] {
	case "finding":
		c.metrics().Findings.Add(1)
		var f emrpb.Finding
		if proto.Unmarshal(m.Data(), &f) != nil {
			c.refuse(m, "body")
			return
		}
		if ref, err = uuid.Parse(f.FindingId); err != nil || ref.String() != f.FindingId {
			c.refuse(m, "body")
			return
		}
		work = func() error {
			if c.handleFinding != nil {
				return c.handleFinding(ctx, hid, ref)
			}
			return c.HandleFinding(ctx, hid, ref)
		}
	default:
		c.metrics().Signals.Add(1)
		var s emrpb.Signal
		if proto.Unmarshal(m.Data(), &s) != nil {
			c.refuse(m, "body")
			return
		}
		if ref, err = uuid.Parse(s.RefId); err != nil || ref.String() != s.RefId {
			c.refuse(m, "body")
			return
		}
		work = func() error { return c.HandleSignal(ctx, hid, &s) }
	}
	if err := work(); err != nil {
		n, derr := m.Deliveries()
		if derr != nil {
			c.metrics().Retried.Add(1)
			_ = m.NakWithDelay(5 * time.Second)
			return
		}
		if n >= ConsumerMaxDeliver {
			if c.deadLetter(ctx, hid, seg[3], ref) {
				_ = m.Term()
				return
			}
			// The dead letter wasn't sent: keep the message. Past the
			// durable's MaxDeliver the server holds it unacked in the
			// stream, so it is never lost silently.
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		c.metrics().Retried.Add(1)
		c.log().Warn("alerts: message not settled; it will be retried", "hospital", hid.String(), "kind", seg[3],
			"ref", ref.String(), "deliveries", n, "code", errCode(err))
		_ = m.NakWithDelay(time.Duration(n) * 5 * time.Second)
		return
	}
	_ = m.Ack()
}

// refuse acks a message that can't be a finding or a signal (it names
// nothing core could read), counted and logged by its reason only.
func (c *Consumer) refuse(m ConsumerMsg, why string) {
	c.metrics().Refused.Add(1)
	c.log().Warn("alerts: malformed EMR message refused", "why", why)
	_ = m.Ack()
}

// errCode is an error as a closed code: a database error's SQLSTATE,
// never its message (which can quote a value).
func errCode(err error) string {
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg):
		return "sqlstate_" + pg.Code
	case errors.Is(err, errRace):
		return "race"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "internal_error"
}

func (c *Consumer) deadLetter(ctx context.Context, hid uuid.UUID, kind string, ref uuid.UUID) bool {
	b, err := proto.Marshal(&emrpb.DeadLetter{Stage: "core", BatchId: ref.String(), ErrorCode: "core_" + kind + "_failed"})
	if err == nil && c.Pub == nil {
		err = errors.New("alerts: no publisher")
	}
	if err == nil {
		// Past the durable's MaxDeliver the server won't deliver the
		// message again, so the publish is retried here. The id names this
		// attempt: a later letter about the same finding (another status)
		// is never taken for a duplicate.
		id := "core_emr:" + kind + ":" + ref.String() + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
		for i := 0; i < deadAttempts; i++ {
			if err = c.Pub.Publish(ctx, "dead.v1."+hid.String()+".core_emr", b, id); err == nil || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(i+1) * deadBackoff):
			}
		}
	}
	if err != nil {
		c.metrics().DeadFailed.Add(1)
		c.log().Error("alerts: dead letter not sent; message kept", "hospital", hid.String(), "kind", kind, "ref", ref.String())
		return false
	}
	c.metrics().DeadLettered.Add(1)
	c.log().Error("alerts: EMR message dead-lettered", "hospital", hid.String(), "kind", kind, "ref", ref.String())
	return true
}

// raceRetryable is a race worth retrying at once: errRace, a unique or check
// violation (another consumer's alert, a patient merged or excluded
// meanwhile), a serialization failure or a deadlock.
func raceRetryable(err error) bool {
	if errors.Is(err, errRace) {
		return true
	}
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "23505" || pg.Code == "23514" || pg.Code == "40001" || pg.Code == "40P01")
}

func (c *Consumer) inHospital(ctx context.Context, hid uuid.UUID, fn func(pgx.Tx) error) error {
	var err error
	for i := 0; i < consumerAttempts; i++ {
		err = dbx.WithHospital(ctx, c.Pool, dbx.EMRCore, hid, fn)
		if err == nil || !raceRetryable(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// findingRow is a finding as the consumer reads it.
type findingRow struct {
	id, patient, primary      uuid.UUID
	conn                      *uuid.UUID
	version, rule, category   string
	severity, status          string
	withdrawnReason           *string
	shadow, preliminary, late bool
	supersedes, supersededBy  *uuid.UUID
	effectiveAt               *time.Time
}

func readFinding(ctx context.Context, tx pgx.Tx, hid, id uuid.UUID) (*findingRow, error) {
	var f findingRow
	err := tx.QueryRow(ctx, `SELECT id, patient_id, primary_observation_id, connection_id, rule_set_version, rule_id,
		category, severity, status, withdrawn_reason, shadow, preliminary, arrived_late, supersedes, superseded_by, effective_at
		FROM emr.findings WHERE hospital_id = $1 AND id = $2`, hid, id).Scan(&f.id, &f.patient, &f.primary, &f.conn,
		&f.version, &f.rule, &f.category, &f.severity, &f.status, &f.withdrawnReason, &f.shadow, &f.preliminary, &f.late,
		&f.supersedes, &f.supersededBy, &f.effectiveAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// rank orders severities.
func rank(severity string) int {
	switch severity {
	case "critical":
		return 3
	case "urgent":
		return 2
	case "info":
		return 1
	}
	return 0
}

// HandleFinding acts on finding fid of hospital hid (see the file comment).
func (c *Consumer) HandleFinding(ctx context.Context, hid, fid uuid.UUID) error {
	return c.inHospital(ctx, hid, func(tx pgx.Tx) error { return c.finding(ctx, tx, hid, fid) })
}

func (c *Consumer) finding(ctx context.Context, tx pgx.Tx, hid, fid uuid.UUID) error {
	met := c.metrics()
	f, err := readFinding(ctx, tx, hid, fid)
	if err != nil {
		return err
	}
	if f == nil {
		// Another hospital's finding (invisible under RLS), or one an
		// exclusion purged: nothing to raise, nothing to say about it.
		met.Foreign.Add(1)
		c.log().Warn("alerts: finding not found under its subject's hospital", "hospital", hid.String(), "finding", fid.String())
		return nil
	}
	// Every message about one patient's rule waits here for the one before.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('alert:' || $1::text || ':' || $2::text || ':' || $3, 0))`,
		hid, f.patient, f.rule); err != nil {
		return err
	}
	again, err := readFinding(ctx, tx, hid, fid)
	if err != nil {
		return err
	}
	if again == nil {
		met.Foreign.Add(1)
		return nil
	}
	if again.patient != f.patient {
		return errRace // merged while it waited: lock the kept patient
	}
	f = again
	if f.shadow || f.status == "historical" {
		met.Skipped.Add(1)
		return nil
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM emr_consumed WHERE hospital_id = $1 AND kind = 'finding'
		AND ref_id = $2 AND status = $3)`, hid, f.id, f.status).Scan(&done); err != nil {
		return err
	}
	if done {
		met.Duplicates.Add(1)
		return nil
	}

	chain, head, err := readChain(ctx, tx, hid, f)
	if err != nil {
		return err
	}
	linked, err := linkedAlerts(ctx, tx, hid, chain)
	if err != nil {
		return err
	}
	outcome, reason := "no_change", ""
	for _, a := range linked {
		if a.withdrawn != nil || (head.status != "withdrawn" && a.finding == head.id) {
			continue
		}
		// A shadow head (the hospital went back to shadow) never changes
		// a live alert: it keeps the value it was raised on (K11).
		if head.shadow {
			continue
		}
		// A new alert may stand for other open findings of the same
		// patient, rule and analyte (a second hit moved it): it is never
		// withdrawn while one of them is open, nor amended below one.
		var other *findingRow
		if a.status == "new" {
			if other, err = groupBest(ctx, tx, hid, a.id, chain); err != nil {
				return err
			}
		}
		switch {
		case other != nil && (head.status == "withdrawn" || rank(other.severity) > rank(head.severity)):
			if err := c.amend(ctx, tx, hid, a, other, "regrouped"); err != nil {
				return err
			}
			outcome = "amended"
		case head.status == "withdrawn":
			if err := c.withdraw(ctx, tx, hid, a, *head.withdrawnReason); err != nil {
				return err
			}
			outcome = "withdrawn"
		default:
			if err := c.amend(ctx, tx, hid, a, head, "amended"); err != nil {
				return err
			}
			outcome = "amended"
		}
	}
	// An open finding (always its chain's head) raises unless an alert of
	// its chain is still new, or was handled at the same or a higher
	// severity (design §12.4: a handled alert re-alerts only when the
	// severity is higher).
	if f.status == "open" && head.id == f.id && !head.shadow {
		raise, handled := true, 0
		for _, a := range linked {
			if a.status == "new" && a.withdrawn == nil {
				raise = false
			}
			if a.status != "new" && a.withdrawn == nil {
				handled = max(handled, rank(a.handledSeverity))
			}
		}
		if raise && handled >= rank(head.severity) {
			raise = false
			if outcome == "no_change" {
				outcome, reason = "not_raised", ReasonAlreadyHandled
				if err := c.notRaised(ctx, tx, hid, head.id, reason); err != nil {
					return err
				}
			}
		}
		if raise {
			if outcome, reason, err = c.raise(ctx, tx, hid, head); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome, reason)
		VALUES ($1, 'finding', $2, $3, $4, $5)`, hid, f.id, f.status, outcome, nullable(reason))
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// readChain is the ids of f's chain of amendments (the findings it
// supersedes, and those that supersede it, at most maxChain each way) and
// its head, the newest.
func readChain(ctx context.Context, tx pgx.Tx, hid uuid.UUID, f *findingRow) ([]uuid.UUID, *findingRow, error) {
	ids := []uuid.UUID{f.id}
	seen := map[uuid.UUID]bool{f.id: true}
	for at, i := f.supersedes, 0; at != nil && !seen[*at] && i < maxChain; i++ {
		seen[*at] = true
		ids = append(ids, *at)
		var next *uuid.UUID
		err := tx.QueryRow(ctx, `SELECT supersedes FROM emr.findings WHERE hospital_id = $1 AND id = $2`, hid, *at).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		at = next
	}
	head := f
	for i := 0; head.supersededBy != nil && i < maxChain; i++ {
		if seen[*head.supersededBy] {
			break
		}
		n, err := readFinding(ctx, tx, hid, *head.supersededBy)
		if err != nil {
			return nil, nil, err
		}
		if n == nil {
			break
		}
		if n.patient != f.patient {
			return nil, nil, errRace
		}
		seen[n.id] = true
		ids = append(ids, n.id)
		head = n
	}
	return ids, head, nil
}

// groupBest is the best other open finding alert aid stands for: an open,
// fresh (not historical), non-shadow finding of the alert's patient, rule
// and analyte outside chain, the highest severity first, then the newest;
// nil when there is none.
func groupBest(ctx context.Context, tx pgx.Tx, hid, aid uuid.UUID, chain []uuid.UUID) (*findingRow, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT f.id FROM alerts a
		JOIN emr.findings f ON f.hospital_id = a.hospital_id AND f.patient_id = a.patient_id AND f.rule_id = a.rule_id
		JOIN emr.observations o ON o.hospital_id = f.hospital_id AND o.id = f.primary_observation_id
		WHERE a.hospital_id = $1 AND a.id = $2 AND coalesce(o.loinc, '') = coalesce(a.loinc, '')
		  AND f.status = 'open' AND NOT f.shadow AND f.id <> ALL ($3)
		ORDER BY `+rankSQL("f.severity")+` DESC, f.effective_at DESC NULLS LAST, f.id
		LIMIT 1`, hid, aid, chain).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readFinding(ctx, tx, hid, id)
}

// alertRef is an alert linked to a chain of findings.
type alertRef struct {
	id, finding     uuid.UUID
	status          string
	handledSeverity string
	withdrawn       *string
}

func linkedAlerts(ctx context.Context, tx pgx.Tx, hid uuid.UUID, chain []uuid.UUID) ([]alertRef, error) {
	rows, err := tx.Query(ctx, `SELECT id, finding_id, status, severity, withdrawn FROM alerts
		WHERE hospital_id = $1 AND finding_id = ANY ($2) ORDER BY created_at, id`, hid, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (alertRef, error) {
		var a alertRef
		return a, r.Scan(&a.id, &a.finding, &a.status, &a.handledSeverity, &a.withdrawn)
	})
}

// valueOf is a finding's primary value as recorded (the comparator and
// text, the number, or the text), at most maxPrevious characters; "" when
// it can't be read.
func valueOf(ctx context.Context, tx pgx.Tx, hid, finding uuid.UUID) (string, error) {
	var comparator, text, num *string
	err := tx.QueryRow(ctx, `SELECT o.comparator, o.value_text, o.value_num::text FROM emr.findings f
		JOIN emr.observations o ON o.hospital_id = f.hospital_id AND o.id = f.primary_observation_id
		WHERE f.hospital_id = $1 AND f.id = $2`, hid, finding).Scan(&comparator, &text, &num)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v := ""
	switch {
	case comparator != nil && text != nil:
		v = *comparator + *text
	case num != nil:
		v = *num
	case text != nil:
		v = *text
	}
	if utf8.RuneCountInString(v) > maxPrevious {
		v = string([]rune(v)[:maxPrevious])
	}
	return v, nil
}

// amend points alert a at head, the newest version of its result (design
// §11.4): amended {previous_value, amended_at}. A new alert takes the new
// severity, and is notified again when it rose; a handled alert keeps its
// status and the severity it was handled at (it is never reopened). One
// statement, so an acknowledgement racing it is never lost (Review Focus
// 5): either it lands first and the alert is marked, or it lands on the
// updated alert.
func (c *Consumer) amend(ctx context.Context, tx pgx.Tx, hid uuid.UUID, a alertRef, head *findingRow, change string) error {
	prev, err := valueOf(ctx, tx, hid, a.finding)
	if err != nil {
		return err
	}
	mark := map[string]any{"amended_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if prev != "" {
		mark["previous_value"] = prev
	}
	raw, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	var severity string
	err = tx.QueryRow(ctx, `UPDATE alerts SET finding_id = $3, amended = $4::jsonb,
		severity = CASE WHEN status = 'new' THEN $5 ELSE severity END,
		preliminary = CASE WHEN status = 'new' THEN $6 ELSE preliminary END,
		arrived_late = CASE WHEN status = 'new' THEN $7 ELSE arrived_late END,
		last_notified_at = CASE WHEN status = 'new' AND `+rankSQL("$5")+` > `+rankSQL("severity")+` THEN now()
		                        ELSE last_notified_at END,
		updated_at = now()
		WHERE hospital_id = $1 AND id = $2 AND finding_id = $8 AND withdrawn IS NULL
		RETURNING severity`, hid, a.id, head.id, string(raw), head.severity, head.preliminary, head.late, a.finding).Scan(&severity)
	if errors.Is(err, pgx.ErrNoRows) {
		return errRace
	}
	if err != nil {
		return err
	}
	c.metrics().Updated.Add(1)
	return c.audit(ctx, tx, hid, "alert.updated", "alert", a.id, map[string]any{
		"alert_id": a.id.String(), "finding_id": head.id.String(), "severity": severity, "change": change})
}

func rankSQL(expr string) string {
	return `(CASE ` + expr + ` WHEN 'critical' THEN 3 WHEN 'urgent' THEN 2 WHEN 'info' THEN 1 ELSE 0 END)`
}

// withdraw withdraws alert a for reason (design §12.5): a new alert reads
// dismissed, by nobody, other; a handled or dismissed one keeps its
// status and who handled it, and gets the mark.
func (c *Consumer) withdraw(ctx context.Context, tx pgx.Tx, hid uuid.UUID, a alertRef, reason string) error {
	if reason != "source_amended" && reason != "source_retracted" {
		return errors.New("alerts: unknown withdrawn reason")
	}
	tag, err := tx.Exec(ctx, `UPDATE alerts SET withdrawn = $3,
		dismiss_reason = CASE WHEN status = 'new' THEN 'other' ELSE dismiss_reason END,
		handled_at = CASE WHEN status = 'new' THEN now() ELSE handled_at END,
		status = CASE WHEN status = 'new' THEN 'dismissed' ELSE status END,
		updated_at = now()
		WHERE hospital_id = $1 AND id = $2 AND withdrawn IS NULL`, hid, a.id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errRace
	}
	c.metrics().Withdrawn.Add(1)
	return c.audit(ctx, tx, hid, "alert.withdrawn", "alert", a.id, map[string]any{"alert_id": a.id.String(), "reason": reason})
}

func (c *Consumer) notRaised(ctx context.Context, tx pgx.Tx, hid, finding uuid.UUID, reason string) error {
	c.metrics().NotRaised.Add(1)
	return c.audit(ctx, tx, hid, "alert.not_raised", "finding", finding,
		map[string]any{"finding_id": finding.String(), "reason": reason})
}

func (c *Consumer) audit(ctx context.Context, tx pgx.Tx, hid uuid.UUID, action, entity string, id uuid.UUID, payload map[string]any) error {
	return audit.Write(ctx, tx, audit.Event{HospitalID: hid.String(), Action: action, Entity: entity, EntityID: id.String(),
		Payload: payload})
}

// raise is design §12.1 steps 2-5 for open finding f: it returns the
// outcome and reason recorded for f.
func (c *Consumer) raise(ctx context.Context, tx pgx.Tx, hid uuid.UUID, f *findingRow) (string, string, error) {
	var hospital string
	var unit *uuid.UUID
	var recipients []uuid.UUID
	err := tx.QueryRow(ctx, `SELECT hospital, unit_id, recipients FROM alert_route_facts($1)`, f.patient).
		Scan(&hospital, &unit, &recipients)
	if errors.Is(err, pgx.ErrNoRows) {
		hospital = ReasonHospitalInactive
	} else if err != nil {
		return "", "", err
	}
	if hospital != "ok" {
		return "not_raised", hospital, c.notRaised(ctx, tx, hid, f.id, hospital)
	}
	var status string
	var noMRN bool
	if err := tx.QueryRow(ctx, `SELECT status, mrn IS NULL FROM emr.patients WHERE hospital_id = $1 AND id = $2`,
		hid, f.patient).Scan(&status, &noMRN); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", errRace
		}
		return "", "", err
	}
	if status == "excluded" {
		// Excluded under a legal hold: the purge is deferred, so the
		// finding stands, but no alert is ever raised for the patient.
		return "not_raised", ReasonPatientExcluded, c.notRaised(ctx, tx, hid, f.id, ReasonPatientExcluded)
	}
	if status != "active" {
		return "", "", errRace // merged meanwhile: read the finding again
	}
	if noMRN && !c.AllowNullMRN {
		return "not_raised", ReasonNullMRN, c.notRaised(ctx, tx, hid, f.id, ReasonNullMRN)
	}
	var loinc *string
	if err := tx.QueryRow(ctx, `SELECT loinc FROM emr.observations WHERE hospital_id = $1 AND id = $2`,
		hid, f.primary).Scan(&loinc); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", errRace
		}
		return "", "", err
	}

	// Design §12.4: one open alert per patient, rule and analyte. A new
	// hit updates it to the latest value; a higher severity re-notifies;
	// a lower one never downgrades an open alert.
	var open uuid.UUID
	var openSeverity string
	var openAt *time.Time
	err = tx.QueryRow(ctx, `SELECT a.id, a.severity, of.effective_at FROM alerts a
		LEFT JOIN emr.findings of ON of.hospital_id = a.hospital_id AND of.id = a.finding_id
		WHERE a.hospital_id = $1 AND a.patient_id = $2 AND a.rule_id = $3 AND coalesce(a.loinc, '') = coalesce($4, '')
		  AND a.status = 'new'
		FOR UPDATE OF a`, hid, f.patient, f.rule, loinc).Scan(&open, &openSeverity, &openAt)
	switch {
	case err == nil:
		newer := openAt == nil || (f.effectiveAt != nil && !f.effectiveAt.Before(*openAt))
		if rank(f.severity) > rank(openSeverity) || (rank(f.severity) == rank(openSeverity) && newer) {
			tag, err := tx.Exec(ctx, `UPDATE alerts SET finding_id = $3, severity = $4, preliminary = $5, arrived_late = $6,
				last_notified_at = CASE WHEN `+rankSQL("$4")+` > `+rankSQL("severity")+` THEN now() ELSE last_notified_at END,
				updated_at = now()
				WHERE hospital_id = $1 AND id = $2 AND status = 'new'`, hid, open, f.id, f.severity, f.preliminary, f.late)
			if err != nil {
				return "", "", err
			}
			if tag.RowsAffected() != 1 {
				return "", "", errRace
			}
			c.metrics().Updated.Add(1)
			return "updated", "", c.audit(ctx, tx, hid, "alert.updated", "alert", open, map[string]any{
				"alert_id": open.String(), "finding_id": f.id.String(), "severity": f.severity, "change": "new_hit"})
		}
		return "not_raised", ReasonAlreadyOpen, c.notRaised(ctx, tx, hid, f.id, ReasonAlreadyOpen)
	case !errors.Is(err, pgx.ErrNoRows):
		return "", "", err
	}

	s, err := routeSettings(ctx, tx, hid, recipients)
	if err != nil {
		return "", "", err
	}
	queue, users, reason := Route(s, unit, f.severity, f.category)
	if reason != "" {
		return "not_raised", reason, c.notRaised(ctx, tx, hid, f.id, reason)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO alerts (hospital_id, finding_id, patient_id, connection_id, rule_id, loinc,
		category, severity, routed_queue, preliminary, arrived_late, last_notified_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now()) RETURNING id`,
		hid, f.id, f.patient, f.conn, f.rule, loinc, f.category, f.severity, queue, f.preliminary, f.late).Scan(&id); err != nil {
		return "", "", err
	}
	for _, u := range users {
		if _, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, hospital_id, user_id, notified_at)
			VALUES ($1, $2, $3, now())`, id, hid, u); err != nil {
			return "", "", err
		}
	}
	c.metrics().Raised.Add(1)
	return "raised", "", c.audit(ctx, tx, hid, "alert.raised", "alert", id, map[string]any{
		"alert_id": id.String(), "finding_id": f.id.String(), "rule_set_version": f.version, "rule_id": f.rule,
		"category": f.category, "severity": f.severity, "routed_queue": queue, "routed_named": len(users), "grouped": false})
}

// routeSettings reads the hospital's alert settings as routing needs them
// (design §12.2; departure 27: no row reads as the defaults).
func routeSettings(ctx context.Context, tx pgx.Tx, hid uuid.UUID, recipients []uuid.UUID) (Settings, error) {
	s := Settings{Queue: true, Recipients: recipients, Covered: map[uuid.UUID]bool{}, Levels: map[string]string{}}
	err := tx.QueryRow(ctx, `SELECT queue FROM alert_settings WHERE hospital_id = $1`, hid).Scan(&s.Queue)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}
	rows, err := tx.Query(ctx, `SELECT unit_id, covered FROM alert_units WHERE hospital_id = $1`, hid)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var u uuid.UUID
		var covered bool
		if err := rows.Scan(&u, &covered); err != nil {
			rows.Close()
			return s, err
		}
		s.Covered[u] = covered
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	rows, err = tx.Query(ctx, `SELECT category, level FROM alert_levels WHERE hospital_id = $1`, hid)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var cat, level string
		if err := rows.Scan(&cat, &level); err != nil {
			rows.Close()
			return s, err
		}
		s.Levels[cat] = level
	}
	return s, rows.Err()
}

// The signal kinds (EMR plan, Global Constraints).
var signalKinds = []string{"possible_match", "source_merge", "breaker_tripped", "document_rejected", "rule_set_activated"}

// HandleSignal records a signal of hospital hid (design §12.4, §12.6): a
// breaker trip is audited once (emr.source_paused) and pages; the others
// are counted (the lists read their tables).
func (c *Consumer) HandleSignal(ctx context.Context, hid uuid.UUID, s *emrpb.Signal) error {
	known := false
	for _, k := range signalKinds {
		known = known || s.Kind == k
	}
	if !known {
		c.metrics().Refused.Add(1)
		c.log().Warn("alerts: signal of an unknown kind refused")
		return nil
	}
	if s.Kind != "breaker_tripped" {
		return nil
	}
	ref, err := uuid.Parse(s.RefId)
	if err != nil {
		c.metrics().Refused.Add(1)
		return nil
	}
	conn, err := uuid.Parse(s.ConnectionId)
	if err != nil || conn.String() != s.ConnectionId {
		c.metrics().Refused.Add(1)
		c.log().Warn("alerts: breaker signal without a connection refused", "hospital", hid.String())
		return nil
	}
	return c.inHospital(ctx, hid, func(tx pgx.Tx) error {
		var mine, done bool
		if err := tx.QueryRow(ctx, `SELECT alert_source_known($1), EXISTS (SELECT 1 FROM emr_consumed
			WHERE hospital_id = $2 AND kind = 'signal' AND ref_id = $3 AND status = 'breaker_tripped')`, conn, hid, ref).
			Scan(&mine, &done); err != nil {
			return err
		}
		if !mine {
			c.metrics().Foreign.Add(1)
			c.log().Warn("alerts: breaker signal names no connection of its hospital", "hospital", hid.String())
			return nil
		}
		if done {
			c.metrics().Duplicates.Add(1)
			return nil
		}
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: hid.String(), Action: "emr.source_paused",
			Entity: "emr_connection", EntityID: conn.String(), Payload: map[string]any{
				"hospital_id": hid.String(), "connection_id": conn.String(), "reason": "breaker_tripped"}}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO emr_consumed (hospital_id, kind, ref_id, status, outcome)
			VALUES ($1, 'signal', $2, 'breaker_tripped', 'recorded')`, hid, ref); err != nil {
			return err
		}
		c.metrics().SourcePaused.Add(1)
		c.log().Error("alerts: an EMR source was paused by its breaker", "hospital", hid.String(), "connection", conn.String())
		return nil
	})
}

// jsConsumerMsg adapts a jetstream.Msg.
type jsConsumerMsg struct{ jetstream.Msg }

func (m jsConsumerMsg) Deliveries() (uint64, error) {
	md, err := m.Metadata()
	if err != nil {
		return 0, err
	}
	return md.NumDelivered, nil
}

// Run pulls core_emr until ctx ends (every core copy shares the durable;
// each message goes to one). It returns at once with an error when the
// durable can't be found (CORE_EMR_ENABLED creates it at start-up).
func (c *Consumer) Run(ctx context.Context, js jetstream.JetStream) error {
	if js == nil || c.Pool == nil {
		return errors.New("alerts: consumer unavailable")
	}
	// The durable is retried until it is found (a NATS hiccup at start-up
	// must not leave core running with no consumer); each failure is
	// logged and counted (a page).
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		x, err := js.Consumer(ctx, bus.EMRStream, bus.CoreEMRConsumer)
		if err == nil {
			cons = x
			break
		}
		c.metrics().Unavailable.Add(1)
		c.log().Error("alerts: the core_emr durable isn't available; retrying")
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(consumerFetchBatch, jetstream.FetchMaxWait(consumerFetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			c.HandleMsg(ctx, jsConsumerMsg{m})
		}
	}
	return nil
}
