package rules

// The rules consumer (EMR design §11.4, §15; plan Task 28). It pulls
// records.v1.<hid>.created from the emr_rules durable and, under the
// subject's hospital (RLS hides every other hospital's row), evaluates the
// rule set for every observation the event names against the patient's
// current canonical rows, and writes findings:
//
//   - once: dedup_key (rule set version, rule, primary observation) is
//     unique, so a redelivery makes no second finding (A19);
//   - fresh or historical: older than the category's window, date-only
//     (no hour is invented), or from a first sync's backfill, a finding is
//     stored historical and never announced (A18; Review Focus 1);
//   - shadow unless the deployment names the rule set (EMR_RULE_SET_ACTIVE)
//     and a platform admin activated the same version and hash for the
//     hospital (K4);
//   - arrived_late when received over an hour after the value was taken;
//   - superseded or withdrawn when an amended, corrected or retracted
//     result replaces the one a finding used (A30).
//
// Any error rolls the batch back (no partial findings, A24) and leaves the
// message unacked. An evaluation error holds that batch for StopFor (and
// pages); other batches of the hospital go on, so one bad batch never
// delays another patient's critical result. Any other error (the database,
// a merge racing the batch) retries sooner. The fifth delivery
// dead-letters dead.v1.<hid>.emr_rules; if that publish fails the message
// is naked, never dropped. Findings are
// announced on emr.v1.<hid>.finding by id after the commit; a status not
// yet announced is announced again by the next message or the sweep.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/signals"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

const (
	Stream     = "EMR"
	Durable    = "emr_rules"
	MaxDeliver = 5 // the durable's limit: the 5th failed delivery dead-letters
	MaxBatch   = 500
	// StopFor is how long an evaluation error holds its batch.
	StopFor = 5 * time.Minute
	// LateAfter: received this long after the value was taken is late.
	LateAfter   = time.Hour
	SweepEvery  = time.Minute
	fetchBatch  = 10
	fetchWait   = 5 * time.Second
	msgDeadline = 50 * time.Second // inside the durable's 60 s ack wait
	// maxRows bounds what one patient's history loads.
	maxRows = 5000
)

// ErrMerged: a patient of the batch was merged after its observations
// were read; the batch is retried (the merge moved them to the kept
// patient).
var ErrMerged = errors.New("rules: a patient was merged during the batch")

// Metrics are counts only.
type Metrics struct {
	Batches, Findings, Historical, Refused, Stopped, Announced, DeadLettered atomic.Int64
	// NotEvaluable counts mapped observations of a rule set analyte whose
	// value no numeric rule could read (a unit with no reviewed
	// conversion, an unreadable comparator): never silently normal.
	NotEvaluable atomic.Int64
}

// Msg is what HandleMsg needs of a JetStream message.
type Msg interface {
	Subject() string
	Data() []byte
	Ack() error
	NakWithDelay(time.Duration) error
	Term() error
	// Deliveries is the delivery count; an error when it can't be read
	// (the message is then naked, never dead-lettered on a guess).
	Deliveries() (uint64, error)
}

// Engine evaluates and writes findings; it is safe for concurrent use.
type Engine struct {
	Pool    *pgxpool.Pool
	RuleSet *RuleSet
	// Maps convert a comparator's bound with the normaliser's reviewed
	// conversions.
	Maps *codes.Maps
	// Active is EMR_RULE_SET_ACTIVE: the version this deployment lets
	// alert. Empty: every finding is shadow.
	Active  string
	Pub     signals.Publisher
	Breaker *Breaker
	Log     *slog.Logger
	Now     func() time.Time
	Metrics *Metrics
	// Evaluate is the evaluator (Evaluate when nil; tests replace it).
	Evaluate func(*RuleSet, History, uuid.UUID, time.Time) ([]Finding, error)

	mu sync.Mutex
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Engine) metrics() *Metrics {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Metrics == nil {
		e.Metrics = &Metrics{}
	}
	return e.Metrics
}

func (e *Engine) evaluate(h History, trig uuid.UUID) ([]Finding, error) {
	if e.Evaluate != nil {
		return e.Evaluate(e.RuleSet, h, trig, e.now())
	}
	return Evaluate(e.RuleSet, h, trig, e.now())
}

// held records an evaluation error: the batch waits StopFor; on-call is
// paged (emr_rules_stopped, design §15.2) with ids only.
func (e *Engine) held(hid, batch uuid.UUID) {
	e.metrics().Stopped.Add(1)
	e.log().Error("rules: evaluation error; batch held", "hospital", hid.String(), "batch", batch.String(),
		"for_s", int(StopFor/time.Second))
}

// HandleMsg settles one records.v1.<hid>.created message.
func (e *Engine) HandleMsg(ctx context.Context, m Msg) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), msgDeadline)
	defer cancel()
	seg := strings.Split(m.Subject(), ".")
	var hid uuid.UUID
	var err error
	if len(seg) == 4 {
		hid, err = uuid.Parse(seg[2])
	}
	if len(seg) != 4 || seg[0] != "records" || seg[1] != "v1" || seg[3] != "created" || err != nil || hid.String() != seg[2] {
		e.metrics().Refused.Add(1)
		_ = m.Ack()
		return
	}
	var c emrpb.Created
	if proto.Unmarshal(m.Data(), &c) != nil || len(c.RecordIds) == 0 || len(c.RecordIds) > MaxBatch {
		e.metrics().Refused.Add(1)
		_ = m.Ack()
		return
	}
	ids := make([]uuid.UUID, 0, len(c.RecordIds))
	for _, s := range c.RecordIds {
		id, err := uuid.Parse(s)
		if err != nil {
			e.metrics().Refused.Add(1)
			_ = m.Ack()
			return
		}
		ids = append(ids, id)
	}
	if err := e.Handle(ctx, hid, ids); err != nil {
		n, derr := m.Deliveries()
		if derr != nil {
			_ = m.NakWithDelay(5 * time.Second)
			return
		}
		if n >= MaxDeliver {
			if e.deadLetter(ctx, hid, ids[0]) {
				_ = m.Term()
				return
			}
			// The dead letter wasn't sent: keep the message. Past the
			// durable's MaxDeliver the server holds it unacked in the stream
			// (a MAX_DELIVERIES advisory), so it is never lost silently.
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		switch {
		case errors.Is(err, ErrEvaluate):
			e.held(hid, ids[0])
			_ = m.NakWithDelay(StopFor)
		case errors.Is(err, ErrMerged):
			_ = m.NakWithDelay(time.Second)
		default:
			_ = m.NakWithDelay(time.Duration(n) * 5 * time.Second)
		}
		return
	}
	_ = m.Ack()
}

func (e *Engine) deadLetter(ctx context.Context, hid, batch uuid.UUID) bool {
	b, err := proto.Marshal(&emrpb.DeadLetter{Stage: "rules", BatchId: batch.String(), ErrorCode: "rules_failed"})
	if err == nil {
		err = e.Pub.Publish(ctx, "dead.v1."+hid.String()+".emr_rules", b, "emr_rules:"+batch.String())
	}
	if err != nil {
		e.log().Error("rules: dead letter not sent", "batch", batch.String())
		return false
	}
	e.metrics().DeadLettered.Add(1)
	e.log().Warn("rules: batch dead-lettered", "batch", batch.String())
	return true
}

// obsRow is an observation as loaded, with what the consumer needs beyond
// the rules' view.
type obsRow struct {
	Observation
	patient      uuid.UUID
	conn         *uuid.UUID
	supersededBy *uuid.UUID
	received     time.Time
	backfill     bool
	unit         *string
}

const obsCols = `o.id, o.patient_id, o.loinc, o.category, o.status, o.value_num::float8, o.value_si::float8, o.comparator,
	o.value_text, o.unit_ucum, o.ref_low::float8, o.ref_high::float8, o.source_flag, o.effective_at, o.effective_date,
	o.standardised, o.superseded_by, o.connection_id, o.received_at, r.backfill`

func scanObs(r pgx.CollectableRow) (obsRow, error) {
	var x obsRow
	var loinc, cmp, text, flag *string
	err := r.Scan(&x.ID, &x.patient, &loinc, &x.Category, &x.Status, &x.ValueNum, &x.ValueSI, &cmp, &text, &x.unit,
		&x.RefLow, &x.RefHigh, &flag, &x.EffectiveAt, &x.EffectiveDate, &x.Standardised, &x.supersededBy, &x.conn,
		&x.received, &x.backfill)
	for dst, src := range map[*string]*string{&x.LOINC: loinc, &x.Comparator: cmp, &x.ValueText: text, &x.SourceFlag: flag} {
		if src != nil {
			*dst = *src
		}
	}
	x.Superseded = x.supersededBy != nil
	return x, err
}

// Handle evaluates hospital hid's created records (observation ids; other
// ids are ignored). It returns an error for anything worth retrying
// (ErrEvaluate for an evaluation error, which holds the batch).
func (e *Engine) Handle(ctx context.Context, hid uuid.UUID, ids []uuid.UUID) error {
	e.metrics().Batches.Add(1)
	var critical []uuid.UUID // connections with a new critical, non-historical finding
	inShadow := true
	err := dbx.WithHospital(ctx, e.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		critical = nil
		shadow, err := e.shadow(ctx, tx, hid)
		if err != nil {
			return err
		}
		inShadow = shadow
		seps, err := separators(ctx, tx, hid)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+obsCols+` FROM emr.observations o
			JOIN emr.raw_records r ON r.hospital_id = o.hospital_id AND r.id = o.raw_id
			WHERE o.hospital_id = $1 AND o.id = ANY ($2)`, hid, ids)
		if err != nil {
			return err
		}
		trig, err := pgx.CollectRows(rows, scanObs)
		if err != nil {
			return err
		}
		byPatient := map[uuid.UUID][]obsRow{}
		for _, o := range trig {
			byPatient[o.patient] = append(byPatient[o.patient], o)
		}
		patients := make([]uuid.UUID, 0, len(byPatient))
		for p := range byPatient {
			patients = append(patients, p)
		}
		slices.SortFunc(patients, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		for _, pid := range patients {
			c, err := e.patient(ctx, tx, hid, pid, byPatient[pid], shadow, seps)
			if err != nil {
				return err
			}
			critical = append(critical, c...)
		}
		return nil
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			e.log().Error("rules: database error", "sqlstate", pe.Code, "constraint", pe.ConstraintName)
		} else {
			e.log().Error("rules: batch failed", "ids", len(ids))
		}
		return err
	}
	if err := e.Announce(ctx, hid); err != nil {
		return err
	}
	// The breaker guards alerting: a rule set in shadow raises no alert, so
	// it never pauses a source (the findings are kept for the review).
	if e.Breaker != nil && !inShadow {
		slices.SortFunc(critical, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		for _, conn := range slices.Compact(critical) {
			if _, err := e.Breaker.Observe(ctx, hid, conn); err != nil {
				e.log().Error("rules: breaker check failed", "connection", conn.String())
			}
		}
	}
	return nil
}

// shadow reports whether the hospital's findings are shadow: unless the
// deployment names this rule set and the hospital activated the same
// version and hash.
func (e *Engine) shadow(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (bool, error) {
	if e.Active == "" || e.Active != e.RuleSet.Version() {
		return true, nil
	}
	var version string
	var sha []byte
	err := tx.QueryRow(ctx, `SELECT version, sha256 FROM public.emr_rule_set_activation WHERE hospital_id = $1`, hid).
		Scan(&version, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	want := e.RuleSet.SHA256()
	return version != e.RuleSet.Version() || !bytes.Equal(sha, want[:]), nil
}

// separators are the hospital's connections' decimal separators.
func separators(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (map[uuid.UUID]rune, error) {
	rows, err := tx.Query(ctx, `SELECT id, settings ->> 'decimal_separator' FROM public.emr_connections WHERE hospital_id = $1`, hid)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]rune{}
	for rows.Next() {
		var id uuid.UUID
		var s *string
		if err := rows.Scan(&id, &s); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = '.'
		if s != nil && *s == "," {
			out[id] = ','
		}
	}
	return out, rows.Err()
}

// patient evaluates one patient's triggers. It returns the connections
// with a new critical, non-historical finding.
func (e *Engine) patient(ctx context.Context, tx pgx.Tx, hid, pid uuid.UUID, trig []obsRow, shadow bool,
	seps map[uuid.UUID]rune) (critical []uuid.UUID, err error) {
	var sex string
	var birth *time.Time
	var status string
	if err := tx.QueryRow(ctx, `SELECT sex, birth_date, status FROM emr.patients WHERE hospital_id = $1 AND id = $2 FOR SHARE`, hid, pid).
		Scan(&sex, &birth, &status); err != nil {
		return nil, err
	}
	// FOR SHARE: an exclusion or merge (which lock the patient FOR UPDATE)
	// waits for this batch, or this batch waits and then sees the patient
	// excluded or merged.
	switch status {
	case "excluded":
		return nil, nil // excluded: nothing evaluates (its rows are purged)
	case "merged_into":
		// Merged after the batch read its observations: the merge moved
		// them to the kept patient, so evaluate them there on a retry
		// rather than acknowledge the batch with nothing raised.
		return nil, ErrMerged
	case "active":
	default:
		return nil, ErrEvaluate
	}
	rows, err := tx.Query(ctx, `SELECT `+obsCols+` FROM emr.observations o
		JOIN emr.raw_records r ON r.hospital_id = o.hospital_id AND r.id = o.raw_id
		WHERE o.hospital_id = $1 AND o.patient_id = $2
		ORDER BY coalesce(o.effective_at, o.effective_date::timestamptz) DESC, o.id LIMIT $3`, hid, pid, maxRows)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, scanObs)
	if err != nil {
		return nil, err
	}
	// The triggers are in the history even past the cap.
	for _, t := range trig {
		if !slices.ContainsFunc(all, func(o obsRow) bool { return o.ID == t.ID }) {
			all = append(all, t)
		}
	}
	byID := map[uuid.UUID]obsRow{}
	h := History{Patient: Patient{ID: pid, Sex: sex, BirthDate: birth}}
	for _, o := range all {
		if o.Comparator != "" && o.unit != nil {
			sep := '.'
			if o.conn != nil {
				if s, ok := seps[*o.conn]; ok {
					sep = s
				}
			}
			o.BoundNum, o.BoundSI = Bound(e.Maps, o.LOINC, *o.unit, o.ValueText, sep)
		}
		byID[o.ID] = o
		h.Observations = append(h.Observations, o.Observation)
	}
	mrows, err := tx.Query(ctx, `SELECT id, atc, status, start_at, end_at FROM emr.medications
		WHERE hospital_id = $1 AND patient_id = $2 AND superseded_by IS NULL ORDER BY id LIMIT $3`, hid, pid, maxRows)
	if err != nil {
		return nil, err
	}
	h.Medications, err = pgx.CollectRows(mrows, func(r pgx.CollectableRow) (Medication, error) {
		var m Medication
		var atc *string
		err := r.Scan(&m.ID, &atc, &m.Status, &m.StartAt, &m.EndAt)
		if atc != nil {
			m.ATC = *atc
		}
		return m, err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(trig, func(a, b obsRow) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	for _, t := range trig {
		if t.Status == "entered-in-error" || t.Status == "cancelled" {
			if _, err := tx.Exec(ctx, `UPDATE emr.findings SET status = 'withdrawn', withdrawn_reason = 'source_retracted',
				updated_at = now() WHERE hospital_id = $1 AND primary_observation_id = $2 AND status = 'open'`, hid, t.ID); err != nil {
				return nil, err
			}
			continue
		}
		if t.Superseded {
			continue // an older version that arrived late: nothing to raise
		}
		e.countNotEvaluable(byID[t.ID])
		fs, err := e.evaluate(h, t.ID)
		if err != nil {
			return nil, err
		}
		// The rows this one replaced, and their open findings.
		var replaced []uuid.UUID
		for _, o := range all {
			if o.supersededBy != nil && *o.supersededBy == t.ID {
				replaced = append(replaced, o.ID)
			}
		}
		type old struct {
			id   uuid.UUID
			rule string
		}
		var olds []old
		if len(replaced) > 0 {
			orows, err := tx.Query(ctx, `SELECT id, rule_id FROM emr.findings WHERE hospital_id = $1
				AND primary_observation_id = ANY ($2) AND status = 'open' ORDER BY created_at, id`, hid, replaced)
			if err != nil {
				return nil, err
			}
			olds, err = pgx.CollectRows(orows, func(r pgx.CollectableRow) (old, error) {
				var x old
				return x, r.Scan(&x.id, &x.rule)
			})
			if err != nil {
				return nil, err
			}
		}
		kept := map[uuid.UUID]uuid.UUID{} // old finding -> its successor
		for _, f := range fs {
			prim, ok := byID[f.Primary]
			if !ok {
				return nil, ErrEvaluate
			}
			var supersedes *uuid.UUID
			if f.Primary == t.ID {
				for _, o := range olds {
					if _, taken := kept[o.id]; !taken && o.rule == f.RuleID {
						supersedes = &o.id
						break
					}
				}
			}
			id, status, err := e.insert(ctx, tx, hid, pid, prim, f, shadow, supersedes)
			if err != nil {
				return nil, err
			}
			if id == uuid.Nil {
				continue // made before (a redelivery)
			}
			e.metrics().Findings.Add(1)
			if status == "historical" {
				e.metrics().Historical.Add(1)
			} else if f.Severity == "critical" && prim.conn != nil {
				critical = append(critical, *prim.conn)
			}
			if supersedes != nil {
				kept[*supersedes] = id
			}
		}
		for _, o := range olds {
			if succ, ok := kept[o.id]; ok {
				_, err = tx.Exec(ctx, `UPDATE emr.findings SET status = 'superseded', superseded_by = $3, updated_at = now()
					WHERE hospital_id = $1 AND id = $2`, hid, o.id, succ)
			} else {
				_, err = tx.Exec(ctx, `UPDATE emr.findings SET status = 'withdrawn', withdrawn_reason = 'source_amended',
					updated_at = now() WHERE hospital_id = $1 AND id = $2`, hid, o.id)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return critical, nil
}

// countNotEvaluable counts (and logs, by id) a mapped observation of a rule
// set's numeric analyte that carries a value no numeric rule can read: a
// unit with no reviewed conversion, a comparator it can't convert, text.
// Its source flag still runs; the count shows the gap instead of hiding it.
func (e *Engine) countNotEvaluable(o obsRow) {
	if o.LOINC == "" || slices.Contains([]string{"entered-in-error", "cancelled"}, o.Status) {
		return
	}
	a, ok := e.RuleSet.AnalyteOf(o.LOINC)
	if !ok || a.Text || o.ValueSI != nil || o.BoundSI != nil {
		return
	}
	if o.ValueNum == nil && o.Comparator == "" && o.ValueText == "" {
		return
	}
	e.metrics().NotEvaluable.Add(1)
	e.log().Warn("rules: mapped value not evaluable", "observation", o.ID.String(), "loinc", o.LOINC)
}

// causeJSON is a cause as stored: ids only.
type causeJSON struct {
	ID           string      `json:"id"`
	Label        string      `json:"label"`
	Confidence   string      `json:"confidence"`
	Observations []uuid.UUID `json:"observation_ids"`
	Medications  []uuid.UUID `json:"medication_ids"`
}

// insert writes one finding (or nothing, on a redelivery) and returns its
// id (uuid.Nil when it existed) and status.
func (e *Engine) insert(ctx context.Context, tx pgx.Tx, hid, pid uuid.UUID, prim obsRow, f Finding, shadow bool,
	supersedes *uuid.UUID) (uuid.UUID, string, error) {
	status := "open"
	late := false
	switch {
	case prim.EffectiveAt == nil, prim.backfill:
		status = "historical"
	default:
		hours, ok := e.RuleSet.FreshnessHours(f.Category)
		if !ok {
			return uuid.Nil, "", ErrEvaluate
		}
		if prim.EffectiveAt.Before(e.now().Add(-time.Duration(hours) * time.Hour)) {
			status = "historical"
		}
		late = prim.received.Sub(*prim.EffectiveAt) > LateAfter
	}
	causes := make([]causeJSON, 0, len(f.Causes))
	for _, c := range f.Causes {
		causes = append(causes, causeJSON{ID: c.ID, Label: c.Label, Confidence: c.Confidence,
			Observations: nonNil(c.Observations), Medications: nonNil(c.Medications)})
	}
	cj, err := json.Marshal(causes)
	if err != nil {
		return uuid.Nil, "", err
	}
	var score, scale *int
	if f.Score != nil {
		score, scale = f.Score, &f.Scale
	}
	var effDate *string
	if prim.EffectiveDate != nil {
		d := prim.EffectiveDate.Format("2006-01-02")
		effDate = &d
	}
	sha := e.RuleSet.SHA256()
	key := e.RuleSet.Version() + ":" + f.RuleID + ":" + f.Primary.String()
	var id uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, connection_id, rule_set_version,
		rule_set_sha256, rule_id, kind, category, severity, primary_observation_id, observation_ids, medication_ids, causes,
		score, scale, effective_at, effective_date, received_at, status, shadow, preliminary, arrived_late, standardised,
		supersedes, dedup_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17::date, $18, $19, $20, $21, $22,
		        $23, $24, $25)
		ON CONFLICT (hospital_id, dedup_key) DO NOTHING RETURNING id`,
		hid, pid, prim.conn, e.RuleSet.Version(), sha[:], f.RuleID, f.Kind, f.Category, f.Severity, f.Primary,
		nonNil(f.Observations), nonNil(f.Medications), cj, score, scale, prim.EffectiveAt, effDate, prim.received, status,
		shadow, f.Preliminary, late, f.Standardised, supersedes, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, status, nil
	}
	return id, status, err
}

func nonNil(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// Announce publishes every finding of hospital hid whose status hasn't
// been announced (never a historical one), new findings first, by id only,
// and records each as announced. A failed publish stops the run; the
// finding stays unannounced for the next message or the sweep.
func (e *Engine) Announce(ctx context.Context, hid uuid.UUID) error {
	for {
		type pending struct {
			id     uuid.UUID
			status string
		}
		var todo []pending
		err := dbx.WithHospital(ctx, e.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id, status FROM emr.findings WHERE hospital_id = $1 AND status <> 'historical'
				AND announced IS DISTINCT FROM status ORDER BY (status <> 'open'), created_at, id LIMIT 500`, hid)
			if err != nil {
				return err
			}
			todo, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
				var p pending
				return p, r.Scan(&p.id, &p.status)
			})
			return err
		})
		if err != nil || len(todo) == 0 {
			return err
		}
		for _, p := range todo {
			b, err := proto.Marshal(&emrpb.Finding{FindingId: p.id.String()})
			if err != nil {
				return err
			}
			if err := e.Pub.Publish(ctx, "emr.v1."+hid.String()+".finding", b, p.id.String()+":"+p.status); err != nil {
				e.log().Warn("rules: finding not announced; it will be again", "finding", p.id.String())
				return err
			}
			if err := dbx.WithHospital(ctx, e.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE emr.findings SET announced = $3 WHERE hospital_id = $1 AND id = $2 AND status = $3`,
					hid, p.id, p.status)
				return err
			}); err != nil {
				return err
			}
			e.metrics().Announced.Add(1)
		}
		if len(todo) < 500 {
			return nil
		}
	}
}

// jsMsg adapts a jetstream.Msg.
type jsMsg struct{ jetstream.Msg }

func (m jsMsg) Deliveries() (uint64, error) {
	md, err := m.Metadata()
	if err != nil {
		return 0, err
	}
	return md.NumDelivered, nil
}

// Run pulls the rules durable until ctx ends. Until core has made the
// stream (CORE_EMR_ENABLED), it waits.
func (e *Engine) Run(ctx context.Context, js jetstream.JetStream) error {
	var cons jetstream.Consumer
	for ctx.Err() == nil {
		c, err := js.Consumer(ctx, Stream, Durable)
		if err == nil {
			cons = c
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	for ctx.Err() == nil {
		batch, err := cons.Fetch(fetchBatch, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		for m := range batch.Messages() {
			e.HandleMsg(ctx, jsMsg{m})
		}
	}
	return nil
}

// Sweep announces what a failed publish left unannounced, for every
// hospital with an enabled connection.
func (e *Engine) Sweep(ctx context.Context) error {
	hospitals, err := enabledHospitals(ctx, e.Pool)
	if err != nil {
		return err
	}
	var errs []error
	for _, hid := range hospitals {
		if err := e.Announce(ctx, hid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunSweep sweeps every SweepEvery until ctx ends.
func (e *Engine) RunSweep(ctx context.Context) {
	t := time.NewTicker(SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Sweep(ctx); err != nil {
				e.log().Warn("rules: sweep incomplete")
			}
		}
	}
}

// enabledHospitals is the hospitals with an enabled connection, read as the
// scheduler's role (ids only, across hospitals).
func enabledHospitals(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr_sched`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT hospital_id FROM public.emr_connections WHERE enabled`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
