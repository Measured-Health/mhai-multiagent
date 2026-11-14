//go:build integration

package rules

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// synthConn is the connection id of maps/testdata/synthetic/fhir.yaml.
var synthConn = uuid.MustParse("00000000-0000-4000-8000-00000000f41a")

const mrnNS = "urn:synthetic:mrn"

type sent struct {
	subject, msgID string
	data           []byte
}

type capture struct {
	mu   sync.Mutex
	msgs []sent
	fail bool
}

func (c *capture) Publish(_ context.Context, subject string, data []byte, msgID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return context.DeadlineExceeded
	}
	c.msgs = append(c.msgs, sent{subject, msgID, data})
	return nil
}

func (c *capture) on(suffix string) []sent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sent
	for _, m := range c.msgs {
		if strings.HasSuffix(m.subject, suffix) {
			out = append(out, m)
		}
	}
	return out
}

func (c *capture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = nil
}

type fixture struct {
	pool     *pgxpool.Pool
	norm     *normalise.Normaliser
	eng      *Engine
	npub     *capture
	pub      *capture
	hospital uuid.UUID
	conn     uuid.UUID
	clock    atomic.Int64 // added to time.Now, in seconds
}

func repoDir(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, pool := testpg.Migrated(t)
	tables, err := codes.Load(repoDir("codes", "testdata"), true)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := codes.LoadMaps(repoDir("maps"), tables, true)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := Load(repoDir("rules", "testdata", "ruleset-0-synthetic.yaml"), true)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, npub: &capture{}, pub: &capture{}, hospital: uuid.New(), conn: synthConn}
	f.norm = &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: f.npub}
	f.eng = &Engine{Pool: pool, RuleSet: rs, Maps: maps, Pub: f.pub,
		Now: func() time.Time { return time.Now().Add(time.Duration(f.clock.Load()) * time.Second) }}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		f.hospital, "R"+f.hospital.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, enabled)
		VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.org', $3, true)`, f.conn, f.hospital,
		map[string]any{"mrn_namespace": mrnNS}); err != nil {
		t.Fatal(err)
	}
	// Live by default: the deployment names the rule set and the hospital
	// activated it, so findings are announced. The shadow tests switch it
	// off (shadowOnly).
	sha := rs.SHA256()
	f.eng.Active = rs.Version()
	f.activate(t, rs.Version(), sha[:])
	return f
}

// shadowOnly puts the hospital back in shadow: no activation, no active
// rule set.
func (f *fixture) shadowOnly(t *testing.T) {
	t.Helper()
	f.eng.Active = ""
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM emr_rule_set_activation WHERE hospital_id = $1`, f.hospital); err != nil {
		t.Fatal(err)
	}
}

func ts(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func patientRec(id string) connector.RawRecord {
	return connector.RawRecord{Kind: "patient", SourceID: id, SourceVersion: "1", Fields: map[string]any{
		"given": "Synthetic", "family": "Patient " + id, "birth_date": "1970-01-01", "sex": "female",
		"identifier": []connector.Coding{{System: mrnNS, Code: "SYN-" + id}}}}
}

// kRec is a potassium result (the synthetic map's local code K).
func kRec(id, version, patient, value, status string, at time.Duration) connector.RawRecord {
	return connector.RawRecord{Kind: "observation", SourceID: id, SourceVersion: version,
		SourceUpdatedAt: time.Now().Add(-at).Add(time.Duration(len(version)) * time.Second), Fields: map[string]any{
			"patient": patient, "category": "laboratory", "status": status,
			"codings":        []connector.Coding{{System: "urn:synthetic:lab", Code: "K", Display: "Potassium"}},
			"value_quantity": value, "unit": "mmol/L", "effective": ts(at)}}
}

// hrRec is a heart rate by LOINC (the synthetic table's 99906-0).
func hrRec(id, patient, value, effective string) connector.RawRecord {
	return connector.RawRecord{Kind: "observation", SourceID: id, SourceVersion: "1", Fields: map[string]any{
		"patient": patient, "category": "vital-signs", "status": "final",
		"codings":        []connector.Coding{{System: codes.LOINCSystem, Code: "99906-0"}},
		"value_quantity": value, "unit": "/min", "effective": effective}}
}

// ingest stores records through the sink and normalises them, returning
// the canonical ids the normaliser announced.
func (f *fixture) ingest(t *testing.T, backfill bool, recs ...connector.RawRecord) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var raw []uuid.UUID
	s := sink.New(f.pool, f.hospital, f.conn, "stub", 1, mrnNS, func(_ context.Context, _ uuid.UUID, b []uuid.UUID) error {
		raw = append(raw, b...)
		return nil
	})
	s.Backfill = backfill
	for _, r := range recs {
		if err := s.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.npub.reset()
	if err := f.norm.Handle(ctx, f.hospital, raw); err != nil {
		t.Fatalf("normalise: %v", err)
	}
	var ids []uuid.UUID
	for _, m := range f.npub.on(".created") {
		var c emrpb.Created
		if err := proto.Unmarshal(m.data, &c); err != nil {
			t.Fatal(err)
		}
		for _, s := range c.RecordIds {
			ids = append(ids, uuid.MustParse(s))
		}
	}
	return ids
}

type findingRow struct {
	id                                 uuid.UUID
	rule, severity, status             string
	reason                             *string
	shadow, prelim, late, standardised bool
	supersedes, supersededBy           *uuid.UUID
	announced                          *string
	obs                                uuid.UUID
}

func (f *fixture) findings(t *testing.T) []findingRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id, rule_id, severity, status, withdrawn_reason, shadow, preliminary,
		arrived_late, standardised, supersedes, superseded_by, announced, primary_observation_id
		FROM emr.findings WHERE hospital_id = $1 ORDER BY created_at, rule_id`, f.hospital)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (findingRow, error) {
		var x findingRow
		return x, r.Scan(&x.id, &x.rule, &x.severity, &x.status, &x.reason, &x.shadow, &x.prelim, &x.late, &x.standardised,
			&x.supersedes, &x.supersededBy, &x.announced, &x.obs)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) activate(t *testing.T, version string, sha []byte) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO emr_rule_set_activation (hospital_id, version, sha256)
		VALUES ($1, $2, $3) ON CONFLICT (hospital_id) DO UPDATE SET version = EXCLUDED.version, sha256 = EXCLUDED.sha256`,
		f.hospital, version, sha); err != nil {
		t.Fatal(err)
	}
}

// A24: an evaluation error rolls the whole batch back (no partial
// findings) and holds that batch for five minutes (paged); the message is
// not acked and, before its fifth delivery, never dead-lettered. Another
// batch of the same hospital (here a critical potassium of another
// patient) is evaluated at once: one bad batch never delays another.
func TestRuleErrorHoldsBatchNoPartialFindings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour),
		kRec("k2", "1", "p1", "7.1", "final", time.Hour))
	good := f.ingest(t, false, patientRec("p2"), kRec("k3", "1", "p2", "7.2", "final", time.Hour))
	if len(bad) != 2 || len(good) != 1 {
		t.Fatalf("created %v %v", bad, good)
	}
	f.eng.Evaluate = func(rs *RuleSet, h History, trig uuid.UUID, now time.Time) ([]Finding, error) {
		if trig == bad[1] {
			return nil, ErrEvaluate
		}
		return Evaluate(rs, h, trig, now)
	}
	subject := "records.v1." + f.hospital.String() + ".created"
	for _, n := range []uint64{1, MaxDeliver - 1} {
		msg := &fakeMsg{subject: subject, data: created(bad), deliveries: n}
		f.eng.HandleMsg(ctx, msg)
		if !msg.naked || msg.acked || msg.termed || msg.delay != StopFor {
			t.Fatalf("delivery %d of the bad batch: %+v", n, msg)
		}
	}
	if got := f.findings(t); len(got) != 0 {
		t.Fatalf("partial findings: %+v", got)
	}
	if f.eng.metrics().Stopped.Load() != 2 || len(f.pub.on(".emr_rules")) != 0 {
		t.Fatalf("held %d, dead letters %d", f.eng.metrics().Stopped.Load(), len(f.pub.on(".emr_rules")))
	}
	msg := &fakeMsg{subject: subject, data: created(good), deliveries: 1}
	f.eng.HandleMsg(ctx, msg)
	if !msg.acked {
		t.Fatalf("the good batch: %+v", msg)
	}
	if got := f.findings(t); len(got) != 1 || got[0].severity != "critical" {
		t.Fatalf("the good batch's finding: %+v", got)
	}
	// Fixed (the next delivery after the hold): the bad batch evaluates.
	f.eng.Evaluate = nil
	msg = &fakeMsg{subject: subject, data: created(bad), deliveries: 3}
	f.eng.HandleMsg(ctx, msg)
	if !msg.acked || len(f.findings(t)) != 3 {
		t.Fatalf("after the fix: %+v, %d findings", msg, len(f.findings(t)))
	}
}

// A delivery count that can't be read is never taken as the last one: the
// message is naked, not dead-lettered.
func TestUnknownDeliveriesNaks(t *testing.T) {
	f := newFixture(t)
	ids := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))
	f.eng.Evaluate = func(*RuleSet, History, uuid.UUID, time.Time) ([]Finding, error) { return nil, ErrEvaluate }
	msg := &fakeMsg{subject: "records.v1." + f.hospital.String() + ".created", data: created(ids), derr: errors.New("no metadata")}
	f.eng.HandleMsg(context.Background(), msg)
	if !msg.naked || msg.termed || len(f.pub.on(".emr_rules")) != 0 {
		t.Fatalf("%+v", msg)
	}
}

// The fifth failed delivery dead-letters dead.v1.<hid>.emr_rules with ids
// and a code only.
func TestRulesDeadLetterAfterFive(t *testing.T) {
	f := newFixture(t)
	ids := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))
	f.eng.Evaluate = func(*RuleSet, History, uuid.UUID, time.Time) ([]Finding, error) { return nil, ErrEvaluate }
	msg := &fakeMsg{subject: "records.v1." + f.hospital.String() + ".created", data: created(ids), deliveries: MaxDeliver}
	f.eng.HandleMsg(context.Background(), msg)
	dl := f.pub.on(".emr_rules")
	if !msg.termed || len(dl) != 1 || dl[0].subject != "dead.v1."+f.hospital.String()+".emr_rules" {
		t.Fatalf("dead letter: %+v %+v", msg, dl)
	}
	var d emrpb.DeadLetter
	if err := proto.Unmarshal(dl[0].data, &d); err != nil || d.Stage != "rules" || d.BatchId != ids[0].String() ||
		d.ErrorCode != "rules_failed" {
		t.Fatalf("dead letter body %v %v", &d, err)
	}
	// The dead letter can't be published: the message is kept (naked),
	// never terminated.
	f.pub.fail = true
	msg = &fakeMsg{subject: msg.subject, data: msg.data, deliveries: MaxDeliver}
	f.eng.HandleMsg(context.Background(), msg)
	if msg.termed || !msg.naked {
		t.Fatalf("dead letter not sent: %+v", msg)
	}
}

// A merge that commits after the batch read its observations: the patient
// is merged_into when locked. The batch is retried (ErrMerged), never
// acked with nothing raised, and the retry raises the finding on the kept
// patient.
func TestMergedDuringBatchRetries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := f.ingest(t, false, patientRec("p-gone"), patientRec("p-keep"), kRec("k1", "1", "p-gone", "6.8", "final", time.Hour))
	var gone, keep uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT o.patient_id FROM emr.observations o WHERE o.id = $1`, ids[0]).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT patient_id FROM emr.patient_links WHERE source_patient_id = 'p-keep'`).Scan(&keep); err != nil {
		t.Fatal(err)
	}
	// The batch's read, before the merge.
	var trig []obsRow
	if err := dbx.WithHospital(ctx, f.pool, dbx.EMR, f.hospital, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+obsCols+` FROM emr.observations o
			JOIN emr.raw_records r ON r.hospital_id = o.hospital_id AND r.id = o.raw_id WHERE o.id = ANY ($1)`, ids)
		if err != nil {
			return err
		}
		trig, err = pgx.CollectRows(rows, scanObs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The merge commits (as core does, through the definer).
	var match uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason)
		VALUES ($1, $2, $3, 'mrn_birth_sex') RETURNING id`, f.hospital, keep, gone).Scan(&match); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.hospital_id', '` + f.hospital.String() + `', true)`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT emr_merge_patients($1, 'a')`, match); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// The batch locks the patient it read: merged, so it retries.
	err = dbx.WithHospital(ctx, f.pool, dbx.EMR, f.hospital, func(tx pgx.Tx) error {
		_, err := f.eng.patient(ctx, tx, f.hospital, gone, trig, true, map[uuid.UUID]rune{})
		return err
	})
	if !errors.Is(err, ErrMerged) {
		t.Fatalf("merged patient: %v, want ErrMerged", err)
	}
	msg := &fakeMsg{subject: "records.v1." + f.hospital.String() + ".created", data: created(ids), deliveries: 1}
	f.eng.Evaluate = func(rs *RuleSet, h History, trig uuid.UUID, now time.Time) ([]Finding, error) {
		if h.Patient.ID != keep {
			t.Errorf("evaluated for %v, want the kept patient", h.Patient.ID)
		}
		return Evaluate(rs, h, trig, now)
	}
	f.eng.HandleMsg(ctx, msg)
	if !msg.acked {
		t.Fatalf("retry: %+v", msg)
	}
	var owner uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT patient_id FROM emr.findings WHERE hospital_id = $1`, f.hospital).Scan(&owner); err != nil || owner != keep {
		t.Fatalf("finding on %v (%v), want the kept patient", owner, err)
	}
}

// Item 4 of the review: a mapped potassium in a unit with no reviewed
// conversion is counted (and logged by id) as not evaluable, never
// silently normal; in mEq/L it converts exactly and raises its finding.
func TestNotEvaluableCounted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	k := kRec("k-mgdl", "1", "p1", "26", "final", time.Hour)
	k.Fields["unit"] = "mg/dL"
	meq := kRec("k-meq", "1", "p1", "6.8", "final", time.Hour)
	meq.Fields["unit"] = "mEq/L"
	if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, patientRec("p1"), k, meq)); err != nil {
		t.Fatal(err)
	}
	if n := f.eng.metrics().NotEvaluable.Load(); n != 1 {
		t.Fatalf("not evaluable %d, want 1", n)
	}
	if got := f.findings(t); len(got) != 1 || got[0].rule != "potassium-critical-high" {
		t.Fatalf("findings %+v", got)
	}
}

// A18: a first sync's records and values older than their category's
// window make historical findings: stored, never announced.
func TestBackfillAndRenormaliseHistorical(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	back := f.ingest(t, true, patientRec("p1"), kRec("k-backfill", "1", "p1", "6.8", "final", time.Hour))
	old := f.ingest(t, false, kRec("k-old", "1", "p1", "6.9", "final", 80*time.Hour))
	fresh := f.ingest(t, false, kRec("k-fresh", "1", "p1", "7.0", "final", time.Hour))
	for _, ids := range [][]uuid.UUID{back, old, fresh} {
		if err := f.eng.Handle(ctx, f.hospital, ids); err != nil {
			t.Fatal(err)
		}
	}
	got := f.findings(t)
	if len(got) != 3 || got[0].status != "historical" || got[1].status != "historical" || got[2].status != "open" {
		t.Fatalf("findings %+v", got)
	}
	if got[0].announced != nil || got[1].announced != nil || got[2].announced == nil || *got[2].announced != "open" {
		t.Fatalf("announced %+v", got)
	}
	if n := len(f.pub.on(".finding")); n != 1 {
		t.Fatalf("%d finding messages, want 1", n)
	}
}

// K4: with no rule set active (EMR_RULE_SET_ACTIVE empty), or not
// activated for the hospital, or activated with another hash, every
// finding is shadow. Only both together alert.
func TestNoActiveRuleSetAllShadow(t *testing.T) {
	f := newFixture(t)
	f.shadowOnly(t)
	ctx := context.Background()
	f.ingest(t, false, patientRec("p1"))
	sha := f.eng.RuleSet.SHA256()
	wrong := sha
	wrong[0] ^= 1
	for i, c := range []struct {
		active  string
		version string
		sha     []byte
		shadow  bool
	}{
		{"", "", nil, true},
		{"0-synthetic", "", nil, true},
		{"", "0-synthetic", sha[:], true},
		{"0-synthetic", "0-synthetic", wrong[:], true},
		{"0-synthetic", "1", sha[:], true},
		{"1", "0-synthetic", sha[:], true},
		{"0-synthetic", "0-synthetic", sha[:], false},
	} {
		f.eng.Active = c.active
		if c.version != "" {
			f.activate(t, c.version, c.sha)
		}
		id := "k-" + string(rune('a'+i))
		if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, kRec(id, "1", "p1", "6.8", "final", time.Hour))); err != nil {
			t.Fatal(err)
		}
		got := f.findings(t)
		if last := got[len(got)-1]; last.shadow != c.shadow {
			t.Errorf("case %d (%+v): shadow %v", i, c, last.shadow)
		}
	}
	// A shadow finding is never announced (core's consumer would only ack
	// it); the one live finding is.
	if n := len(f.pub.on(".finding")); n != 1 {
		t.Fatalf("%d finding messages, want only the live one", n)
	}
	for _, x := range f.findings(t) {
		if x.shadow && x.announced != nil {
			t.Errorf("shadow finding %v announced %v", x.id, *x.announced)
		}
	}
	if err := f.eng.Sweep(ctx); err != nil || len(f.pub.on(".finding")) != 1 {
		t.Fatalf("the sweep announced a shadow finding: %v", err)
	}
}

// A19: a redelivered event makes no second finding and no second
// announcement of the same status.
func TestRedeliveredRecordOneFinding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))
	for range 3 {
		if err := f.eng.Handle(ctx, f.hospital, ids); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.findings(t); len(got) != 1 {
		t.Fatalf("%d findings", len(got))
	}
	msgs := f.pub.on(".finding")
	if len(msgs) != 1 || msgs[0].subject != "emr.v1."+f.hospital.String()+".finding" {
		t.Fatalf("messages %+v", msgs)
	}
	// Ids only: the body is the finding id, nothing else.
	var m emrpb.Finding
	if err := proto.Unmarshal(msgs[0].data, &m); err != nil || m.FindingId != f.findings(t)[0].id.String() ||
		msgs[0].msgID != m.FindingId+":open" {
		t.Fatalf("message %v %q %v", &m, msgs[0].msgID, err)
	}
	if len(msgs[0].data) > 40 {
		t.Fatalf("finding message carries %d bytes", len(msgs[0].data))
	}
}

// A publish that fails leaves the finding unannounced; the redelivery
// announces it (the finding itself is not made twice).
func TestFailedAnnouncementRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))
	f.pub.fail = true
	if err := f.eng.Handle(ctx, f.hospital, ids); err == nil {
		t.Fatal("a failed announcement was not reported")
	}
	if got := f.findings(t); len(got) != 1 || got[0].announced != nil {
		t.Fatalf("after a failed publish: %+v", got)
	}
	f.pub.fail = false
	if err := f.eng.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.findings(t); got[0].announced == nil || len(f.pub.on(".finding")) != 1 {
		t.Fatalf("after the sweep: %+v", got)
	}
}

// Review Focus 1: a date-only vital sign is a date; no hour is invented,
// so it never meets the 6-hour window: the finding is stored historical
// and never announced. A timed one inside the window is open; outside it,
// historical.
func TestDateOnlyVitalsNeverAlert(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	ids := f.ingest(t, false, patientRec("p1"),
		hrRec("hr-date", "p1", "150", today),
		hrRec("hr-9h", "p1", "151", ts(9*time.Hour)),
		hrRec("hr-1h", "p1", "152", ts(time.Hour)))
	if err := f.eng.Handle(ctx, f.hospital, ids); err != nil {
		t.Fatal(err)
	}
	var dateOnly, nineHours, oneHour string
	rows, err := f.pool.Query(ctx, `SELECT o.source_id, f.status FROM emr.findings f JOIN emr.observations o
		ON o.id = f.primary_observation_id AND o.hospital_id = f.hospital_id WHERE f.hospital_id = $1`, f.hospital)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var src, st string
		if err := rows.Scan(&src, &st); err != nil {
			t.Fatal(err)
		}
		switch src {
		case "hr-date":
			dateOnly = st
		case "hr-9h":
			nineHours = st
		case "hr-1h":
			oneHour = st
		}
	}
	if dateOnly != "historical" || nineHours != "historical" || oneHour != "open" {
		t.Fatalf("date-only %q, 9 h %q, 1 h %q", dateOnly, nineHours, oneHour)
	}
	if n := len(f.pub.on(".finding")); n != 1 {
		t.Fatalf("%d announced, want 1", n)
	}
}

// A30: an amended result's finding names the one it replaces, which is
// marked superseded; corrected to normal, the finding is withdrawn
// (source_amended); entered in error, withdrawn (source_retracted). Each
// change is announced by id.
func TestSupersedesNamesOld(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	run := func(recs ...connector.RawRecord) {
		t.Helper()
		if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, recs...)); err != nil {
			t.Fatal(err)
		}
	}
	run(patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))
	run(kRec("k1", "2", "p1", "7.2", "amended", time.Hour))
	got := f.findings(t)
	if len(got) != 2 || got[0].status != "superseded" || got[1].status != "open" || got[1].supersedes == nil ||
		*got[1].supersedes != got[0].id || got[0].supersededBy == nil || *got[0].supersededBy != got[1].id {
		t.Fatalf("after the amendment: %+v", got)
	}
	run(kRec("k1", "3", "p1", "4.0", "amended", time.Hour))
	got = f.findings(t)
	if len(got) != 2 || got[1].status != "withdrawn" || got[1].reason == nil || *got[1].reason != "source_amended" {
		t.Fatalf("corrected to normal: %+v", got)
	}
	run(kRec("k2", "1", "p1", "6.9", "final", time.Hour))
	run(kRec("k2", "2", "p1", "6.9", "entered-in-error", time.Hour))
	got = f.findings(t)
	last := got[len(got)-1]
	if last.status != "withdrawn" || last.reason == nil || *last.reason != "source_retracted" {
		t.Fatalf("entered in error: %+v", last)
	}
	// Announced: open, superseded, open, withdrawn, open, withdrawn.
	var ids []string
	for _, m := range f.pub.on(".finding") {
		ids = append(ids, m.msgID[strings.Index(m.msgID, ":")+1:])
	}
	if strings.Join(ids, ",") != "open,open,superseded,withdrawn,open,withdrawn" {
		t.Fatalf("announcements %v", ids)
	}
	for _, x := range got {
		if x.announced == nil || *x.announced != x.status {
			t.Errorf("finding %v announced %v, status %s", x.id, x.announced, x.status)
		}
	}
}

// A late value (received over an hour after it was taken) inside the
// window alerts and says so.
func TestArrivedLate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := f.ingest(t, false, patientRec("p1"), kRec("k-late", "1", "p1", "6.8", "final", 3*time.Hour),
		kRec("k-prompt", "1", "p1", "6.9", "final", 30*time.Minute))
	if err := f.eng.Handle(ctx, f.hospital, ids); err != nil {
		t.Fatal(err)
	}
	got := f.findings(t)
	late := map[bool]int{}
	for _, x := range got {
		late[x.late]++
		if x.status != "open" {
			t.Errorf("status %s", x.status)
		}
	}
	if late[true] != 1 || late[false] != 1 {
		t.Fatalf("late %v", late)
	}
}

// The emr role can't delete a finding or change what it says.
func TestEMRCannotRewriteFindings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "6.8", "final", time.Hour))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM emr.findings`, `UPDATE emr.findings SET severity = 'info'`,
		`UPDATE emr.findings SET shadow = false`, `UPDATE emr.findings SET rule_id = 'x'`} {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, `SET LOCAL ROLE mhai_emr`)
		_, _ = tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, f.hospital.String())
		_, err = tx.Exec(ctx, q)
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func created(ids []uuid.UUID) []byte {
	c := &emrpb.Created{}
	for _, id := range ids {
		c.RecordIds = append(c.RecordIds, id.String())
	}
	b, _ := proto.Marshal(c)
	return b
}
